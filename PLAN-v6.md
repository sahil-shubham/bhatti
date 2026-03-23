# Bhatti v0.3 — Images, Volumes, and Snapshots

v0.1 shipped multi-tenant security: per-user auth, network isolation,
guest hardening, encrypted secrets, rate limiting, and observability.
v0.2 shipped CLI improvements (cobra migration, --timing, --json, --timeout).

v0.3 adds the storage and state primitives that make bhatti useful for
real workloads: persistent data, custom environments, and checkpoint/resume.

---

## Core Design Principle: Everything Is a Block Device

Firecracker VMs are composed of block devices. Each is an ext4 file on
the host, attached as a virtio-blk drive:

```
/dev/vda  →  rootfs.ext4        (OS, packages, tools)
/dev/vdb  →  config.ext4        (env vars, mount instructions, auth token)
/dev/vdc  →  workspace.ext4     (user data)
/dev/vdd  →  datasets.ext4      (shared read-only data)
```

Firecracker doesn't know what's in these files. It gets `path_on_host`
for each drive. The guest agent reads the config drive and mounts them.

**The v0.1 model:** all block devices are created fresh inside the sandbox
directory and destroyed with the sandbox. The rootfs is always a copy of
one hardcoded base image. Volumes are ephemeral.

**The v0.3 model:** block devices have their own lifecycle, independent
of sandboxes. A sandbox is a transient compute context that *references*
durable block devices, not *owns* them. This is the same relationship
as EC2 instances to EBS volumes, or Kubernetes pods to PersistentVolumes.

This principle — every piece of persistent state is a block device with
its own lifecycle — is the foundation. Images, volumes, and snapshots are
all governance layers on top of block devices.

---

## Three Primitive Entities

### Images

An image is an immutable ext4 file used as a rootfs source. Sandboxes
get copy-on-write clones. The image itself is never modified.

```
/var/lib/bhatti/images/
  base-amd64.ext4                   admin-built, ships with bhatti
  python-3.12.ext4                  pulled from docker.io/library/python:3.12
  usr_alice/
    ml-ready.ext4                   saved by alice from a running sandbox
```

Sources:
- **Admin-built**: `build-rootfs.sh` with different package lists
- **OCI pull**: `bhatti image pull python:3.12` converts a Docker image
- **Save-as-image**: `bhatti image save <sandbox> --name ml-ready`
- **Import**: `bhatti image import --file custom.ext4 --name my-env`

Scoping: admin images are global (no user prefix). User-saved images
are private (stored under `usr_{id}/`). Users see both admin and their
own images. They cannot see other users' images.

**Name validation**: image, volume, and snapshot names are used to
construct file paths. All names must match `^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`
(same regex as sandbox names). Names containing `/`, `..`, or null bytes
are rejected. This prevents path traversal attacks like
`name: "../../sandboxes/victim/rootfs"`.

### Volumes

A volume is a mutable ext4 file owned by a user. Stored outside the
sandbox directory. Survives sandbox destroy. Attachable to sandboxes
by name.

```
/var/lib/bhatti/volumes/
  usr_alice/
    workspace.ext4                  5GB, her project files
    shared-data.ext4                20GB, team dataset
  usr_bob/
    workspace.ext4                  2GB, his project
```

Lifecycle:
1. **Create**: explicit API call or auto-create on first use
2. **Attach**: referenced by name in sandbox creation
3. **Use**: mounted at specified path inside VM
4. **Detach**: sandbox destroyed, volume remains
5. **Reattach**: new sandbox references same volume, data intact
6. **Delete**: explicit API call, must be detached

Concurrency: a volume can be attached to **one sandbox for writing** at
a time. ext4 does not support concurrent writers — attaching the same
volume to two running sandboxes would corrupt the filesystem. Read-only
attachment to multiple sandboxes simultaneously is safe and supported.

Read-only mounts require changes at three layers:
1. **Firecracker**: `/drives/{id}` with `is_read_only: true`
2. **Config drive**: `VolumeMountConfig` gains a `ReadOnly bool` field
3. **Guest agent**: lohar's `mountVolumes` uses `syscall.MS_RDONLY`

Without all three, the volume is mounted read-write inside the VM
regardless of what the API says, allowing corruption of shared data.

The updated config drive struct:
```go
type VolumeMountConfig struct {
    Device   string `json:"device"`    // e.g. "/dev/vdc"
    Mount    string `json:"mount"`     // e.g. "/workspace"
    FS       string `json:"fs"`        // e.g. "ext4"
    ReadOnly bool   `json:"read_only"` // mount with MS_RDONLY
}
```

**MUST: update `cmd/lohar/main.go`**: the existing `SandboxConfig.Volumes`
uses an anonymous struct `[]struct{Device, Mount, FS string}` that does
NOT have a `ReadOnly` field. It must be changed to use `VolumeMountConfig`
(the named type above). Without this, Go's JSON unmarshaler silently
ignores the `read_only` key and every "read-only" volume is mounted
read-write — a data corruption path, not a cosmetic bug. The type name
`VolumeMountConfig` is used on both engine and guest sides; the JSON
serialization on the config drive is the contract between them.

And the updated lohar mount code:
```go
func mountVolumes(volumes []VolumeMountConfig) {
    for _, v := range volumes {
        os.MkdirAll(v.Mount, 0755)
        var flags uintptr
        if v.ReadOnly {
            flags |= syscall.MS_RDONLY
        }
        if err := syscall.Mount(v.Device, v.Mount, v.FS, flags, ""); err != nil {
            fmt.Fprintf(os.Stderr, "lohar: mount %s → %s: %v\n", v.Device, v.Mount, err)
            continue
        }
        if !v.ReadOnly {
            os.Chown(v.Mount, 1000, 1000)
        }
        fmt.Fprintf(os.Stderr, "lohar: mounted %s → %s (ro=%v)\n", v.Device, v.Mount, v.ReadOnly)
    }
}
```

**Dirty journal hazard for read-only multi-attach**: ext4 journal replay
occurs on mount — even read-only mounts — if the journal is dirty. If a
volume was previously attached read-write and the VM crashed (unclean
unmount), the journal is dirty. Two Firecracker instances trying to
mount the same dirty volume simultaneously both attempt journal replay,
which writes to the block device, causing corruption.

Mitigation: before allowing read-only attachment of a volume that has
any existing attachments (i.e., second+ RO attach), verify the volume
is clean on the host:

```go
func volumeIsClean(path string) bool {
    // tune2fs -l reports "Filesystem state: clean" for cleanly unmounted ext4
    out, err := exec.Command("tune2fs", "-l", path).Output()
    if err != nil {
        return false
    }
    return strings.Contains(string(out), "Filesystem state:            clean")
}
```

If the volume is dirty, reject the multi-attach with:
`"volume %q has a dirty journal — attach read-write to a single sandbox first to replay the journal, then retry read-only"`

For the FIRST read-only attachment (no other attachments), Firecracker
opens the file with `O_RDONLY` and the kernel replays the journal
safely (single writer). The problem is only concurrent journal replays.
After the first mount replays the journal, the volume is clean and
subsequent RO attaches are safe.

This check goes in `AttachVolume` when `readOnly && roCount > 0`
(second+ RO attachment).

### Snapshots

A snapshot is a frozen point-in-time of an entire VM: memory state, CPU
registers, device state, plus references to the block devices that were
attached. Firecracker's existing snapshot/restore mechanism.

```
/var/lib/bhatti/snapshots/
  usr_alice/
    dev-ready/
      mem.snap                      memory snapshot
      vm.snap                       VM state (CPU, devices)
      manifest.json                 VM config + attached block devices
```

Today snapshots are ephemeral — tied to a sandbox ID, stored in the
sandbox directory, used only for thermal management (warm → cold → resume).
Named snapshots promote this to a first-class entity that users control.

**Firecracker constraint**: a snapshot is tied to the exact VM
configuration — same vCPU count, same memory size, same number and order
of block devices. You cannot resume a 2-vCPU snapshot as a 4-vCPU VM.
The manifest records the configuration so resume uses the same parameters.

---

## OCI Image Support

### What an OCI/Docker image actually is

An OCI image is NOT a filesystem. It's a stack of compressed tar layers
plus a JSON config:

```
python:3.12
  ├── layer 0: debian:bookworm base           (28MB compressed, 75MB expanded)
  ├── layer 1: apt packages (libc, libssl...) (45MB compressed, 180MB expanded)
  ├── layer 2: python3.12 build               (60MB compressed, 150MB expanded)
  ├── layer 3: pip + setuptools               (12MB compressed, 35MB expanded)
  └── config.json:
        Env: ["PATH=/usr/local/bin:/usr/local/sbin:..."]
        Cmd: ["python3"]
        WorkingDir: "/"
        User: ""
```

Docker's runtime extracts these layers using overlayfs — a union mount
that stacks read-only layers with a writable top. The container sees a
flat filesystem. The host stores each layer independently and shares
common layers across images.

### Why this doesn't map directly to Firecracker

Firecracker uses raw block devices, not overlay filesystems.

| Docker | Firecracker |
|--------|-------------|
| Shares host kernel | Own guest kernel |
| overlayfs layers | Single ext4 block device |
| Container entrypoint is PID 1 | lohar is always PID 1 |
| Namespaces for isolation | Full VM for isolation |
| /dev, /proc from host | Guest mounts its own |
| Layers shared across containers | Each VM has independent rootfs |

A Docker image cannot be directly booted by Firecracker. It must be
converted to a flat ext4 filesystem image with bhatti's guest agent
injected.

### The conversion pipeline

```
Docker Registry                        Bhatti
─────────────                          ──────
manifest.json ───┐
layer-0.tar.gz ──┤    bhatti image     /var/lib/bhatti/images/
layer-1.tar.gz ──┼──► pull python:3.12 ──► python-3.12.ext4
layer-2.tar.gz ──┤      (one-time)         (flat ext4, cached)
layer-3.tar.gz ──┘
config.json ─────┘
```

Step by step:

**1. Pull from registry.**
Use `go-containerregistry` (crane) to download the manifest and layers.
Pure Go, no Docker daemon needed, works with any OCI-compliant registry
(Docker Hub, GHCR, ECR, GCR, private registries). Handles auth via
`~/.docker/config.json` or explicit credentials.

```go
import "github.com/google/go-containerregistry/pkg/crane"

img, err := crane.Pull("python:3.12")
```

This downloads compressed layers to a temp directory. For `python:3.12`,
roughly 350MB of network transfer.

**2. Flatten layers.**
Extract each layer's tar in order into a single directory. Later layers
overwrite earlier layers' files. OCI whiteout markers (`.wh.` prefix
files) represent deletions — a file `.wh.config.json` in layer 3 means
"delete config.json from the merged view."

```go
for _, layer := range layers {
    reader, _ := layer.Uncompressed()
    // extract tar to temp directory, handling whiteouts
}
```

The result is a single directory tree representing the container's
filesystem as the user would see it. For `python:3.12`, roughly 1.1GB
on disk.

**Things that can go wrong here:**
- **Whiteout handling**: OCI has two whiteout types — per-file (`.wh.NAME`)
  and opaque (`.wh..wh..opq`, meaning "ignore everything from lower layers
  in this directory"). Both must be handled correctly or the merged
  filesystem will have ghost files from lower layers.
- **Permissions and ownership**: tar entries have uid/gid. During extraction,
  these must be preserved. Some images create files owned by specific users
  (uid 1000, uid 999) that must exist in the target filesystem.
- **Symlinks**: layers can contain absolute symlinks that point to other
  layers' files. These resolve correctly in the flattened tree but must be
  extracted in layer order.
- **Hard links**: OCI layers can contain hard links. These must be preserved
  during extraction (same inode, not a copy).
- **Special files**: device nodes in layers. Most registries strip these, but
  some images include them. They should be skipped during extraction since
  lohar creates /dev at boot.

**3. Create ext4 image.**
Create an empty ext4 file of the right size, mount it, copy the flattened
tree into it.

```bash
# Size: flattened tree size + 20% headroom + 256MB minimum free
truncate -s ${SIZE}M image.ext4
mkfs.ext4 -F image.ext4
mount image.ext4 /mnt
cp -a flattened/ /mnt/
umount /mnt
```

**Things that can go wrong here:**
- **Size estimation**: you need to know the total size of the flattened tree
  before creating the ext4 file. A dry-run pass through the layers to sum
  file sizes, or a two-pass approach (flatten to temp dir, measure, create
  ext4 at measured size + headroom).
- **Inode count**: ext4 has a fixed inode count set at mkfs time. Images with
  many small files (node_modules: tens of thousands of files) can exhaust
  inodes before filling disk. Use `mkfs.ext4 -N` to allocate more inodes
  based on the file count from the flattened tree.
- **Mount requires root**: mounting an ext4 loopback image requires root.
  The conversion pipeline must run as root (same as the daemon). Alternative:
  use `e2fsprogs`' `mke2fs` + `e2cp` to populate without mounting, but
  this is slower and doesn't handle symlinks/permissions as cleanly.

**4. Inject bhatti components.**
The image needs lohar and a few directory stubs for the boot process:

```go
// Copy lohar binary
copyFile(loharPath, mountpoint+"/usr/local/bin/lohar")
chmod(mountpoint+"/usr/local/bin/lohar", 0755)

// Ensure boot directories exist (lohar mounts these)
for _, dir := range []string{"/proc", "/sys", "/dev", "/dev/pts",
                              "/tmp", "/run", "/workspace"} {
    os.MkdirAll(mountpoint+dir, 0755)
}

// Ensure DNS is writable (lohar overwrites this)
os.Remove(mountpoint + "/etc/resolv.conf")  // may be a symlink
os.WriteFile(mountpoint+"/etc/resolv.conf", []byte(""), 0644)

// Ensure lohar user exists (for exec as uid 1000)
// If the image doesn't have a uid-1000 user, create one
ensureUser(mountpoint, "lohar", 1000)
```

**Things that can go wrong here:**

- **Conflicting lohar path**: if the image has its own `/usr/local/bin/lohar`,
  we overwrite it. This is correct (our lohar must be PID 1) but should log
  a warning.

- **Missing shell**: lohar spawns exec commands via the shell. If the image
  doesn't have `/bin/sh` or `/bin/bash` (some minimal images like `scratch`
  or `distroless` don't), exec won't work. We should detect this during
  conversion and warn.

- **Missing libc**: lohar is statically compiled (CGO_ENABLED=0), so it
  doesn't need libc. But user commands (python, node, etc.) do. If the
  image is based on musl (Alpine) vs glibc (Debian/Ubuntu), commands in
  the image work fine because they were compiled against the image's libc.
  But if the user tries to run binaries copied from the host, they'll fail.
  This is expected container behavior, not a bhatti-specific issue.

- **resolv.conf symlink**: many images have `/etc/resolv.conf` as a symlink
  to `/run/systemd/resolve/stub-resolv.conf` (Ubuntu's systemd-resolved).
  Since there's no systemd in the VM, the symlink is broken. We must remove
  it and create a regular file. Lohar already handles this at boot, but
  having a broken symlink during conversion can cause issues with any
  post-extraction validation.

- **The lohar user**: Docker images often have their own users (uid 1000 might
  already be taken by `node` in Node images, or `appuser` in Python images).
  We need uid 1000 to exist for exec-as-lohar. Options:
  a) Reuse whatever user has uid 1000 (may have different name, different
     home dir, different shell)
  b) Always create the `lohar` user, overwriting any existing uid 1000
  c) Use whatever uid the image specifies in its `User` config field

  For v0.3: option (a) — reuse the existing uid 1000 user. If no uid 1000
  exists, create `lohar`. This handles Node images (uid 1000 = `node`) and
  Python images (uid 1000 = `appuser`) without conflict.

  **Why this works**: lohar's exec uses `Credential{Uid: 1000}` which is
  a kernel-level operation — it doesn't consult /etc/passwd. The passwd
  entry only affects `whoami`, `~` expansion, and programs that call
  `getpwuid(1000)`. By reusing the image's existing uid 1000 user, we
  preserve the image's expected home directory, shell, and npm/pip configs.

- **Images that expect root**: some images have `User: ""` (default) or
  `User: "root"` in their OCI config, meaning the container was designed
  to run everything as root. In bhatti, exec runs as uid 1000. If the
  image writes to `/usr/local/lib`, `/etc`, or other root-owned paths,
  it will get permission denied.

  Mitigation: sudo. Our base rootfs has sudo + NOPASSWD configured. But
  pulled Docker images may not have sudo installed. During conversion,
  we should:
  a) Check if sudo exists in the image
  b) If not, install it (copy a static sudo binary, or add to sudoers
     if sudo is present but not configured for uid 1000)
  c) If we can't install sudo, warn: "image expects root but has no
     sudo — permission errors may occur for system-level operations"

  This is the biggest compatibility gap between Docker (runs as root by
  default) and bhatti (runs as uid 1000 by default). It's solvable but
  must be handled during conversion, not at runtime.

**5. Extract and store OCI config metadata.**
The image's config contains runtime hints that should be preserved:

```json
{
    "image": "python-3.12",
    "source": "docker.io/library/python:3.12",
    "env": {"PATH": "/usr/local/bin:...", "PYTHON_VERSION": "3.12.x"},
    "working_dir": "/",
    "user": "",
    "cmd": ["python3"],
    "exposed_ports": [8000],
    "size_mb": 1200,
    "created_at": "2026-03-22T..."
}
```

This metadata is stored alongside the ext4 file (or in the SQLite images
table). When a sandbox is created from this image:
- `env` is merged into the config drive (sandbox env overrides image env)
- `working_dir` is used as default cwd for exec
- `cmd` can be used as default init if the user doesn't specify one
- `exposed_ports` is informational

### What we lose compared to Docker

**Layer sharing.** Docker stores each layer once and shares it across
images. `python:3.12` and `python:3.12-slim` might share 80% of their
layers. In bhatti, each is an independent ext4 file. No sharing.

Impact: ~1-3GB per cached image. With 20 images, 30-60GB of image
storage. On 1.8TB NVMe, this is 2-3% of capacity. Not a problem for
single-node.

If layer sharing becomes critical later (hundreds of images, frequent
updates), the path is: mount an overlayfs inside the guest VM with
layers as separate read-only block devices. But this adds complexity to
lohar and limits layers per image. Not worth it for v0.3.

**Incremental updates.** Pulling `python:3.12.4` after having `3.12.3`
in Docker only downloads the changed layers. In bhatti, it re-downloads
everything and re-converts.

Mitigation: the conversion result is cached. `bhatti image pull python:3.12`
checks if the image digest has changed since last pull. If not, no-op. If
yes, re-pulls and re-converts. This is a full re-download but it's a
background operation, not in the sandbox creation path.

**Build workflow.** Docker has `Dockerfile` for declarative image building.
bhatti doesn't replicate this. Users either pull existing images or
customize by running commands in a sandbox and saving.

This is a feature, not a limitation. Dockerfiles are a build tool.
Bhatti's model — boot a sandbox, run commands, save the result — is
more interactive and more natural for dev environments. If users want
Dockerfile-style builds, they run `docker build` externally and
`bhatti image pull` the result.

### Kernel compatibility

Docker containers share the host kernel. The host kernel has thousands
of options compiled in — filesystems, network modules, security modules,
device drivers. Containers get all of this for free.

Firecracker VMs run a minimal guest kernel. The kernel shipped with
bhatti has a stripped-down config optimized for fast boot:

**What's in the kernel:**
- ext4 (rootfs, volumes)
- virtio-blk (block devices)
- virtio-net (networking)
- devtmpfs, procfs, sysfs, tmpfs
- PTY support
- Basic networking (TCP, UDP, ICMP)

**What's NOT in the kernel:**
- Overlay filesystem (no overlayfs)
- FUSE (no sshfs, s3fs, etc.)
- NFS / CIFS
- Device mapper
- iptables/nftables (inside guest)
- cgroups v2 (inside guest)
- GPU / NVIDIA drivers
- USB, sound, display

Most application containers work fine because they only need ext4,
networking, and PTYs. But some images won't work:

- Images that use FUSE mounts (gcsfuse, s3fs) — **won't work**
- Images that run Docker-in-Docker — **won't work** (no cgroups, no
  device mapper)
- Images that need GPU access — **won't work** (no device passthrough)
- Images that use iptables internally — **won't work** (no netfilter
  in guest kernel)
- Images that use inotify extensively — **works** (supported in kernel)
- Images that use systemd — **won't work** (lohar is PID 1, not systemd)

We should detect known-incompatible patterns during conversion (presence
of `/usr/bin/dockerd`, NVIDIA libraries, systemd units) and warn the
user.

### Auth for private registries

crane supports Docker credential helpers and `~/.docker/config.json`.
For private registries:

```bash
# Docker Hub (private repos)
bhatti image pull myorg/private-image:latest --auth user:token

# GitHub Container Registry
bhatti image pull ghcr.io/myorg/my-image:latest --auth user:ghp_...

# AWS ECR
bhatti image pull 123456.dkr.ecr.us-east-1.amazonaws.com/my-image:latest
# (uses AWS credential chain automatically if aws-cli is configured)
```

For v0.3: support `--auth user:token` flag and `~/.docker/config.json`.
Don't implement the full Docker credential helper ecosystem — it's a
rabbit hole. Users who need ECR/GCR auth can `docker pull` + `docker save`
+ `bhatti image import` as a workaround.

### Async Pull: Task System

OCI image pull + conversion takes 30-120 seconds for typical images
(350MB download + 1.1GB flatten + ext4 creation). This exceeds HTTP
timeout defaults. The pull endpoint is asynchronous:

```
POST /images/pull {"ref": "python:3.12", "name": "python-3.12"}
→ 202 Accepted
→ {"task_id": "task-abc123", "status": "running"}

GET /tasks/task-abc123
→ {"task_id": "task-abc123", "status": "running", "progress": "downloading layer 3/4"}

GET /tasks/task-abc123  (later)
→ {"task_id": "task-abc123", "status": "completed", "result": {"image": "python-3.12", "size_mb": 1200}}

GET /tasks/task-abc123  (on failure)
→ {"task_id": "task-abc123", "status": "failed", "error": "registry auth failed: 401"}
```

**Task store schema:**
```sql
CREATE TABLE tasks (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,
    type TEXT NOT NULL,                  -- 'image_pull'
    status TEXT NOT NULL DEFAULT 'running', -- 'running', 'completed', 'failed'
    progress TEXT NOT NULL DEFAULT '',   -- human-readable progress string
    result_json TEXT NOT NULL DEFAULT '{}',
    error TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    completed_at DATETIME
);
CREATE INDEX idx_tasks_created_at ON tasks(created_at);  -- cleanup query scans by created_at
```

**Task lifecycle:**
```go
func (s *Server) handleImagePull(w http.ResponseWriter, r *http.Request) {
    // Validate request
    var req struct {
        Ref  string `json:"ref"`
        Name string `json:"name"`
        Auth string `json:"auth,omitempty"` // "user:token"
    }
    // ... parse, validate name regex ...

    // Create task record
    taskID := generateID()
    store.CreateTask(Task{ID: taskID, UserID: userID, Type: "image_pull", Status: "running"})

    // Run in background goroutine
    go func() {
        ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
        defer cancel()

        outputPath := filepath.Join(dataDir, "images", userID, req.Name+".ext4")
        config, err := oci.PullAndConvert(ctx, req.Ref, outputPath, loharPath,
            oci.WithProgress(func(msg string) {
                store.UpdateTaskProgress(taskID, msg)
            }),
        )
        if err != nil {
            os.Remove(outputPath) // clean up partial file
            store.FailTask(taskID, err.Error())
            return
        }

        // Create image store record
        store.CreateImage(Image{...})
        store.CompleteTask(taskID, resultJSON)
    }()

    // Return 202 immediately
    w.WriteHeader(http.StatusAccepted)
    json.NewEncoder(w).Encode(map[string]string{"task_id": taskID, "status": "running"})
}
```

**Task cleanup:** tasks older than 24 hours are deleted by the
cleanup goroutine (same one that handles thermal management). This
prevents the tasks table from growing unbounded.

**Cancellation:** `DELETE /tasks/{id}` cancels a running task (via
context cancellation). The background goroutine checks `ctx.Err()`
between stages (download, flatten, create ext4) and aborts, cleaning
up temp files.

**Duplicate prevention:** before starting a pull, check if another
task is already pulling the same `ref` for the same user. If so,
return the existing task ID instead of starting a duplicate.

---

## Save-as-Image: Checkpoint the Filesystem

When a user runs `bhatti image save <sandbox> --name my-env`:

1. **Pause the VM** — ensure filesystem consistency (no in-flight writes)
2. **Copy the rootfs** — `cp` the sandbox's rootfs.ext4 to the images
   directory. This captures everything: the base image's files plus
   everything the user installed/modified.
3. **Resume the VM** — the sandbox continues running

The saved image is a complete, flat ext4 file. No layers, no diffs. It
includes the base image contents plus all modifications. This is simple
and correct — the saved image is exactly what the filesystem looked like
at save time.

**Size implications**: if the base image is 1GB and the user installed
500MB of packages, the saved image is ~1.5GB (whatever the rootfs file
size is). There's no deduplication against the base image.

**Consistency**: pausing the VM ensures no writes are in flight. The ext4
journal is clean. This is the same mechanism used for snapshots.

**What's NOT saved**: memory state, running processes, open connections.
Only the filesystem. When a new sandbox boots from this image, it starts
fresh (lohar init, config drive, etc.) but with all the files in place.
This is distinct from a snapshot, which captures the entire VM state.

---

## Persistent Volumes: Detailed Design

### Storage layout

```
/var/lib/bhatti/volumes/
  {user_id}/
    {name}.ext4             the volume data
```

Volume metadata lives in SQLite (`volumes_v2` table), not in sidecar
JSON files. Attachment state is tracked in the `volume_attachments`
junction table (see §1.1 Store Schema). The ext4 file is the only
artifact on the filesystem.

### Attachment lifecycle

**Create:**
```
POST /volumes {"name": "workspace", "size_mb": 5120}
→ validate name regex, check quota
→ create ext4 file with O_EXCL (fail if exists)
→ mkfs.ext4 on new file
→ insert into volumes_v2
```

**Attach (during sandbox creation):**
```
POST /sandboxes {"name": "dev", "volumes": [{"name": "workspace", "mount": "/workspace"}]}
→ check volume exists and belongs to user (volumes_v2)
→ check attachment rules in volume_attachments:
    - RW: must have zero existing attachments
    - RO: must have no RW attachments; if second+ RO, check journal is clean
→ insert into volume_attachments (sandbox_id, volume_id, mount, read_only)
→ pass volume file path to Firecracker as additional drive
→ config drive tells lohar to mount /dev/vdc at /workspace
```

**Detach (during sandbox destroy):**
```
DELETE /sandboxes/abc123
→ destroy VM, remove sandbox directory
→ delete from volume_attachments where sandbox_id = abc123
→ volume ext4 file untouched
```

**The volume file is never inside the sandbox directory.** This is the
key difference from v0.1. The sandbox directory contains only the rootfs
(ephemeral copy), config drive, and snapshot files. Volumes live in
their own directory hierarchy.

### Concurrent access protection

The `volume_attachments` junction table supports multiple read-only
attachments to the same volume. The attachment check happens inside a
SQLite transaction (see §1.1 for the full `AttachVolume` implementation):

- **RW attach**: requires zero rows in volume_attachments for this volume
- **RO attach**: requires zero RW rows; if other RO rows exist, also
  requires the volume's ext4 journal to be clean (see dirty journal
  hazard in §Volumes above)

SQLite's WAL mode with `busy_timeout(5000)` handles concurrent
transactions. Two concurrent RW attach attempts: one commits first,
the second sees the row and fails. No application-level locking needed.

### Volume expansion

```
POST /volumes/workspace/resize {"size_mb": 10240}
→ volume must be detached (no running sandbox using it)
→ new_size must be > current_size (shrink is rejected)
→ e2fsck -f workspace.ext4  (repair before resize)
→ truncate -s 10240M workspace.ext4
→ resize2fs workspace.ext4
→ update metadata
```

Cannot resize while attached — the guest kernel has the filesystem
mounted and would be confused by the underlying block device changing
size. (Live resize IS possible with virtio-blk resize events, but adds
significant complexity. Defer to v0.4.)

**Shrink is explicitly rejected.** `truncate` to a smaller size silently
chops the file. `resize2fs` would then fail or corrupt the filesystem.
ext4 shrink IS possible (`resize2fs <size>`) but requires the filesystem
to have enough free space, and data loss occurs if files occupy the
truncated region. Too dangerous for an API endpoint — users who need
to shrink can create a new smaller volume and copy data manually.

```go
func (s *Server) handleVolumeResize(w http.ResponseWriter, r *http.Request) {
    // ...
    vol, _ := store.GetVolume(userID, name)
    if newSizeMB <= vol.SizeMB {
        http.Error(w, fmt.Sprintf("new size (%dMB) must be larger than current size (%dMB)",
            newSizeMB, vol.SizeMB), 400)
        return
    }
    // ... proceed with e2fsck + truncate + resize2fs ...
}
```

### Volume snapshots (copies)

```
POST /volumes/workspace/snapshot {"name": "workspace-backup"}
→ volume must be detached (consistency)
→ cp workspace.ext4 workspace-backup.ext4
→ create new volume metadata
```

This creates an independent copy. Changes to the original don't affect
the snapshot, and vice versa. Useful for "let me try something risky
with a backup."

---

## Named Snapshots: Full VM Checkpoint

### What Firecracker snapshots contain

When Firecracker creates a snapshot, it writes two files:
- `mem.snap` — full memory contents (size = VM memory)
- `vm.snap` — CPU registers, device state, virtio queue state

On resume, Firecracker loads these into a new VMM process and the VM
continues exactly where it left off. Every process, every open file
descriptor, every network connection state.

The block devices (rootfs, volumes) are NOT part of the snapshot files.
They're separate ext4 files that must be at the same paths on resume.
The snapshot just references them — the VM state expects the block
devices to have the same content as when the snapshot was taken.

### Consistency model

When you take a snapshot:
1. VM is paused (vCPUs frozen)
2. Memory is written to `mem.snap`
3. VM state is written to `vm.snap`
4. Block devices are consistent (no in-flight writes since VM is paused)

The block device files on disk are whatever they were when the VM
paused. The kernel's page cache is captured in `mem.snap`. So the
snapshot represents a consistent point-in-time across memory and disk.

### Making snapshots portable

Today, snapshots are stored in the sandbox directory and can only be
resumed as the same sandbox (same ID, same engine state). To make them
portable:

**Manifest:**
```json
{
    "name": "dev-ready",
    "created_from": "sandbox-abc123",
    "created_at": "2026-03-22T...",
    "vm_config": {
        "vcpu_count": 2,
        "mem_size_mib": 1024
    },
    "block_devices": [
        {"role": "rootfs", "snapshot_path": "rootfs.ext4"},
        {"role": "config", "snapshot_path": "config.ext4"},
        {"role": "volume", "name": "workspace", "source": "usr_alice/workspace.ext4"}
    ]
}
```

**Engine must track volume attachments at runtime.** The checkpoint code
needs to know which volume files are attached and their drive IDs. But
after `Create()` boots the VM, the drive configuration lives inside the
Firecracker VMM process and isn't queryable. The VM struct must record
this information:

```go
// VolumeAttachmentInfo records a volume attached to a running VM.
// Populated during Create(), persisted in engine_meta, used by checkpoint.
type VolumeAttachmentInfo struct {
    DriveID  string `json:"drive_id"`   // Firecracker drive ID ("vol0")
    Name     string `json:"name"`       // volume name ("workspace")
    FilePath string `json:"file_path"`  // host path to ext4 file
    Mount    string `json:"mount"`      // guest mount point
    ReadOnly bool   `json:"read_only"`
}
```

Added to the VM struct:
```go
type VM struct {
    // ... existing fields ...
    Volumes []VolumeAttachmentInfo // populated in Create, used by checkpoint
}
```

These must also be persisted in `VMState()` / `RestoreVM()` so they
survive daemon restarts (stored as JSON in the sandboxes table's
`engine_meta_json` column).

**Concrete `VMState()` / `RestoreVM()` changes** — without these, a
daemon restart followed by a checkpoint produces a snapshot manifest
with zero volume entries, silently losing all volume references:

```go
// In VMState() — add to the returned map:
func (e *Engine) VMState(id string) map[string]interface{} {
    // ... existing fields ...
    m["volumes"] = vm.Volumes // []VolumeAttachmentInfo, marshaled by json.Marshal
    return m
}

// In RestoreVM() — deserialize volumes from persisted state:
func (e *Engine) RestoreVM(id, name, status string, state map[string]interface{}) {
    // ... existing field extraction ...

    // Restore volume attachments (JSON round-trip through interface{})
    if raw, ok := state["volumes"]; ok {
        b, _ := json.Marshal(raw) // re-marshal the []interface{} back to JSON
        json.Unmarshal(b, &vm.Volumes)
    }

    // ... rest of RestoreVM ...
}
```

The `engine_meta_json` column in the sandboxes table already stores
arbitrary JSON. The server layer's existing `SaveFirecrackerState` /
`LoadFirecrackerState` must be updated to round-trip this field. The
simplest path: stop using per-column storage for engine state and
switch entirely to the `engine_meta_json` blob (which already exists
but is underused). This avoids adding more ALTER TABLE migrations.

**Creating a named snapshot:**
```
POST /sandboxes/abc123/checkpoint {"name": "dev-ready"}
→ CHECK if snapshots/usr_alice/dev-ready/ already exists → 409 "delete first"
    (fail BEFORE pausing the VM — don't waste 10s of copying only to fail at rename)
→ acquire vm.stateMu (hold for entire operation — see below)
→ pause VM
→ create temp directory: snapshots/usr_alice/.dev-ready.tmp/
→ create Firecracker snapshot → .tmp/mem.snap + .tmp/vm.snap
→ copy rootfs.ext4 → .tmp/rootfs.ext4                  (parallel)
→ copy config.ext4 → .tmp/config.ext4                  (parallel)
→ for each vm.Volumes:                                  (parallel)
    copy vol file → .tmp/vol-{name}.ext4
→ write .tmp/manifest.json
→ atomic rename: .tmp/ → snapshots/usr_alice/dev-ready/
→ resume VM
→ release vm.stateMu
```

Uses `--sparse=always` for all copies (volumes may be large and sparse).

**Parallel copies**: rootfs, config, and volume files are independent.
Copy them concurrently using `errgroup` (NOT `atomic.Value` — that
only stores the last error, losing all but one if multiple copies fail):
```go
g, _ := errgroup.WithContext(ctx)
for _, src := range filesToCopy {
    g.Go(func() error {
        return copyFile(src.path, dst.path)
    })
}
if err := g.Wait(); err != nil {
    os.RemoveAll(tmpDir) // clean up partial snapshot
    return err           // first error (usually the root cause, e.g. "disk full")
}
```

**Atomic staging via temp directory**: all files are written to a `.tmp`
directory first. Only after ALL copies succeed (including manifest
write) is the directory atomically renamed to the final name. If any
copy fails (disk full, I/O error), the entire `.tmp` directory is
removed — no partial snapshots on disk.

Wait — should volumes be copied or referenced?

**If referenced**: resuming the snapshot after the volume has been modified
by another sandbox would be inconsistent (memory state expects old
filesystem state). This is safe ONLY if the volume hasn't been attached
to anything else since the snapshot was taken.

**If copied**: the snapshot is fully self-contained. Resuming always
produces the exact state that was checkpointed. But copying a 20GB
volume on every checkpoint is expensive.

**Resolution**: copy everything. Rootfs, config drive, and all attached
volumes are copied into the snapshot directory. Snapshots are fully
self-contained and always safe to resume regardless of what happened
to the original volumes.

The cost: for a sandbox with 2GB rootfs, 1GB mem, and 5GB volume, a
checkpoint writes 8GB. At NVMe speeds (2GB/s), that's 4 seconds of
VM pause (faster with parallel copies — overlapping I/O on NVMe brings
this down to ~2-3s). This is acceptable for explicit user-initiated
checkpoints. It is NOT acceptable for automatic thermal snapshots —
those continue to use the existing ephemeral snapshot path which doesn't
copy volumes.

A future optimization (volume deduplication or reference counting) is
a significant redesign of the consistency model and is deferred
indefinitely. Copy-everything is the correct starting point.

### Resuming from a named snapshot

**Critical implementation detail**: Firecracker's `vm.snap` records the
original `path_on_host` for every block device. On snapshot load,
Firecracker opens those exact paths. If the rootfs was at
`/var/lib/bhatti/sandboxes/abc/rootfs.ext4` when checkpointed, `vm.snap`
contains that path — NOT the copied snapshot path.

The resume procedure must reconfigure drives BEFORE loading the snapshot:

```
POST /snapshots/dev-ready/resume {"name": "new-sandbox"}
→ read manifest (VM config, drive map, agent token)
→ create new sandbox directory
→ copy rootfs and config from snapshot dir to new sandbox dir
→ for each volume in manifest:
    copy from snapshot dir to new sandbox dir
→ allocate network: MUST get the same guest IP as the manifest
    (guest memory contains interface config with this IP)
    if IP is taken → fail with "IP 10.0.1.3 in use, cannot resume"
→ create TAP device on user's bridge
→ start new Firecracker process
→ PUT /drives/rootfs   {"path_on_host": NEW rootfs path}     ← REQUIRED
→ PUT /drives/config   {"path_on_host": NEW config path}     ← REQUIRED
→ for each volume:
    PUT /drives/volN    {"path_on_host": NEW volume copy path} ← REQUIRED
→ PUT /machine-config  {same vcpu/memory as manifest}
→ PUT /network-interfaces/eth0 {guest_mac: SAME, host_dev_name: NEW TAP}
→ PUT /vsock           {guest_cid: NEW, uds_path: NEW}
→ PUT /snapshot/load   {snapshot_path, mem_backend, resume_vm: true}
→ flush bridge FDB for the MAC to avoid ARP staleness
→ reconnect agent with token from manifest
```

**ARP/FDB staleness**: the resumed VM uses the same MAC but a new TAP
device. The host bridge's forwarding database (FDB) still maps the MAC
to the old (deleted) TAP. Frames to the VM go to the wrong interface
and are silently dropped for 5-30 seconds until FDB expires. Fix: flush
the FDB entry after creating the new TAP:
```go
exec.Command("bridge", "fdb", "del", guestMAC, "dev", oldTAP, "master").Run()
```
Or send a gratuitous ARP from the host side after resume.

All pre-configuration PUTs must happen BEFORE `/snapshot/load`.
Firecracker supports this pre-load resource patching — it's the
official way to relocate snapshot files and rebind network/vsock.
Without it, Firecracker opens the old paths and the resume fails
or reads stale/deleted data.

**IP allocation constraint**: the guest's network stack (in restored
memory) has the original IP configured on eth0. The kernel's `ip=`
boot parameter was applied before the snapshot was taken and is NOT
re-applied on resume. Therefore the resumed VM MUST get the same
guest IP. If that IP is currently allocated to another sandbox in
the user's pool, the resume fails.

To support this, the IP pool needs a `TryAllocate(ip string)` method:
```go
// TryAllocate attempts to allocate a specific IP. Returns error if taken.
func (p *ipPool) TryAllocate(ip string) error {
    var octet int
    fmt.Sscanf(ip, p.prefix+"%d", &octet)
    if octet < 2 || octet > 254 {
        return fmt.Errorf("IP %s out of range", ip)
    }
    p.mu.Lock()
    defer p.mu.Unlock()
    if p.used[octet] {
        return fmt.Errorf("IP %s is in use by another sandbox", ip)
    }
    p.used[octet] = true
    return nil
}
```

**Cleanup on failed resume**: if ANY step in the resume procedure
fails after resources have been allocated, ALL must be released:
```go
defer func() {
    if err != nil {
        if fcCmd != nil && fcCmd.Process != nil {
            fcCmd.Process.Kill(); fcCmd.Wait()
        }
        if vmCancel != nil { vmCancel() }
        if tapName != "" { destroyTapDevice(tapName) }
        if guestIP != "" {
            if net, ok := e.userNetworks[userID]; ok {
                net.Pool.Release(guestIP)
            }
        }
        os.RemoveAll(sandboxDir)
    }
}()
```

This mirrors the existing cleanup pattern in `Create()`. Every
resource acquisition (TAP, IP, Firecracker process, sandbox dir)
has a matching release in the defer.

**vsock CID**: unlike the guest IP, the vsock CID does NOT need to
match. Firecracker's pre-load vsock configuration replaces the
snapshot's CID. The guest agent listens on TCP (not vsock) after
resume anyway. Allocate a fresh CID via `atomic.AddUint32(&e.nextCID, 1)`.

The manifest must record:
```json
{
    "vm_config": {"vcpu_count": 2, "mem_size_mib": 1024},
    "drives": [
        {"drive_id": "rootfs", "role": "rootfs", "snapshot_file": "rootfs.ext4"},
        {"drive_id": "config", "role": "config", "snapshot_file": "config.ext4"},
        {"drive_id": "vol0", "role": "volume", "name": "workspace",
         "snapshot_file": "vol-workspace.ext4"}
    ],
    "network": {"guest_mac": "02:ab:cd:...", "guest_ip": "10.0.1.2"},
    "agent_token": "abc123...",
    "original_sandbox": "sandbox-abc123"
}
```

Each drive entry has a `snapshot_file` — the filename relative to the
snapshot directory. On resume, each is copied to the new sandbox dir
and the drive PUT uses the new path.

**Firecracker constraint**: the resume must use the exact same VM
configuration (vCPUs, memory, drive count and order). The manifest
records this. If the user requests different resources, we reject
with a clear error explaining why.

**Snapshot rootfs copies are NOT standalone images.** The copied rootfs
may have dirty pages that only exist in `mem.snap` (kernel page cache).
On resume, the kernel flushes these from restored memory to the rootfs —
this is correct. But booting the rootfs independently (without mem.snap)
would give an inconsistent filesystem. Do not use snapshot rootfs copies
as images.

**Resumed sandbox volumes are ephemeral copies.** On resume, volume
files are copied from the snapshot directory into the new sandbox
directory. These copies are destroyed when the resumed sandbox is
destroyed — they are NOT attached to the original persistent volume.
The `volume_attachments` table has NO rows for the resumed sandbox's
volumes. This means:
- `GET /volumes/:name` shows 0 attachments even though the resumed
  sandbox has a copy of that volume's data at snapshot time.
- Writes to `/workspace` in the resumed sandbox do NOT affect the
  original persistent volume.
- The original volume can be independently attached to other sandboxes.
This is correct and intentional — the snapshot is a frozen point-in-time.
Modifying the original volume after checkpoint would violate snapshot
isolation. Document this clearly in the API docs so users understand
"resume gives you a copy, not a reference."

**What the user experiences**: a sandbox that boots in ~50ms with all
their processes running, files open, dev server responding. Not a fresh
boot — a continuation.

---

## Templates Redesign

Templates are now a server-layer convenience that maps a name to a
fully-specified sandbox configuration. The engine doesn't know about
templates.

### Schema

```sql
CREATE TABLE templates_v2 (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,               -- owner (or 'admin' for global)
    name TEXT NOT NULL,
    image TEXT NOT NULL DEFAULT 'base',   -- rootfs image name
    cpus REAL NOT NULL DEFAULT 1,
    memory_mb INTEGER NOT NULL DEFAULT 512,
    disk_size_mb INTEGER NOT NULL DEFAULT 0,  -- 0 = use image size
    env_json TEXT NOT NULL DEFAULT '{}',
    volumes_json TEXT NOT NULL DEFAULT '[]',
    init TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(user_id, name)
);
```

Volume spec in templates:
```json
{
    "volumes": [
        {
            "name": "workspace",
            "mount": "/workspace",
            "size_mb": 5120,
            "auto_create": true,
            "read_only": false
        }
    ]
}
```

### Template resolution

```
POST /sandboxes {"template": "ml-env", "name": "experiment-1"}

Server:
  1. Look up template "ml-env" (user-scoped, then admin)
  2. Build SandboxSpec:
       Image:      template.Image
       CPUs:       template.CPUs
       MemoryMB:   template.MemoryMB
       DiskSizeMB: template.DiskSizeMB
       Env:        merge(template.Env, request.Env)  // request overrides
       Init:       template.Init (if request doesn't specify one)
       Volumes:    template.Volumes
  3. For each volume: resolve name, auto-create if needed
  4. Pass SandboxSpec to engine.Create()
```

Request fields override template fields. This lets users tweak a
template without creating a new one — `{"template": "ml-env", "cpus": 8}`
uses the template but with more CPUs.

---

## Implementation Phases

### Phase 1: Persistent Volumes

#### 1.1 Store Schema

Replace the existing `volumes` table (which only tracked Docker named
volumes by name):

```sql
CREATE TABLE volumes_v2 (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,
    name TEXT NOT NULL,
    size_mb INTEGER NOT NULL,
    file_path TEXT NOT NULL,              -- /var/lib/bhatti/volumes/{user_id}/{name}.ext4
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(user_id, name)
);

-- Storage quotas added to users table (v0.1 already has max_sandboxes, max_cpus, max_memory)
-- ALTER TABLE users ADD COLUMN max_volume_storage_mb INTEGER NOT NULL DEFAULT 20480;   -- 20GB default
-- ALTER TABLE users ADD COLUMN max_images INTEGER NOT NULL DEFAULT 10;
-- ALTER TABLE users ADD COLUMN max_snapshots INTEGER NOT NULL DEFAULT 5;

-- Junction table for volume attachments. Supports multiple read-only
-- attachments to the same volume simultaneously. A volume can have
-- at most one read-write attachment OR multiple read-only attachments.
CREATE TABLE volume_attachments (
    volume_id TEXT NOT NULL,
    sandbox_id TEXT NOT NULL,
    mount TEXT NOT NULL,
    read_only INTEGER NOT NULL DEFAULT 0,
    attached_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (volume_id, sandbox_id)
);
-- migrate old volumes table data if any, then drop + rename
```

New store methods:

```go
// Volume entity
type Volume struct {
    ID          string             `json:"id"`
    UserID      string             `json:"user_id"`
    Name        string             `json:"name"`
    SizeMB      int                `json:"size_mb"`
    FilePath    string             `json:"-"`
    Attachments []VolumeAttachment `json:"attachments"`
    CreatedAt   time.Time          `json:"created_at"`
}

type VolumeAttachment struct {
    SandboxID string `json:"sandbox_id"`
    Mount     string `json:"mount"`
    ReadOnly  bool   `json:"read_only"`
}

func (s *Store) CreateVolume(v Volume) error                       // plain INSERT (not OR IGNORE) — must return error on UNIQUE violation for race coordination
func (s *Store) GetVolume(userID, name string) (*Volume, error)    // includes attachments
func (s *Store) ListUserVolumes(userID string) ([]Volume, error)
func (s *Store) DeleteVolume(userID, name string) error            // fails if any attachments
func (s *Store) AttachVolume(userID, name, sandboxID, mount string, readOnly bool) error
func (s *Store) DetachVolume(userID, name, sandboxID string) error
func (s *Store) DetachAllForSandbox(sandboxID string) error        // called on sandbox destroy
func (s *Store) DetachOrphanedVolumes() error                      // startup recovery
func (s *Store) UpdateVolumeSize(userID, name string, sizeMB int) error
```

`AttachVolume` runs inside a transaction with the junction table:
```go
func (s *Store) AttachVolume(userID, name, sandboxID, mount string, readOnly bool) error {
    tx, _ := s.db.Begin()
    defer tx.Rollback()

    // Get volume ID
    var volID string
    tx.QueryRow("SELECT id FROM volumes_v2 WHERE user_id=? AND name=?",
        userID, name).Scan(&volID)
    if volID == "" {
        return fmt.Errorf("volume %q not found", name)
    }

    // Check existing attachments
    var rwCount, roCount int
    tx.QueryRow("SELECT COUNT(*) FROM volume_attachments WHERE volume_id=? AND read_only=0",
        volID).Scan(&rwCount)
    tx.QueryRow("SELECT COUNT(*) FROM volume_attachments WHERE volume_id=? AND read_only=1",
        volID).Scan(&roCount)

    if !readOnly {
        // Requesting read-write: must have zero existing attachments
        if rwCount > 0 || roCount > 0 {
            return fmt.Errorf("volume %q already attached (rw=%d, ro=%d)", name, rwCount, roCount)
        }
    } else {
        // Requesting read-only: must have no read-write attachments
        if rwCount > 0 {
            return fmt.Errorf("volume %q has a read-write attachment, cannot attach read-only", name)
        }
    }

    tx.Exec("INSERT INTO volume_attachments (volume_id, sandbox_id, mount, read_only) VALUES (?,?,?,?)",
        volID, sandboxID, mount, boolToInt(readOnly))
    return tx.Commit()
}
```

Orphan cleanup on startup (fixes crash between Destroy and DetachAll):
```go
func (s *Store) DetachOrphanedVolumes() (int64, error) {
    // Use LEFT JOIN instead of NOT IN for better query planning.
    // NOT IN does a full table scan of sandboxes for every attachment row.
    // LEFT JOIN + IS NULL lets SQLite use the index on sandboxes.id.
    res, err := s.db.Exec(`DELETE FROM volume_attachments
        WHERE sandbox_id IN (
            SELECT va.sandbox_id FROM volume_attachments va
            LEFT JOIN sandboxes s ON va.sandbox_id = s.id AND s.status != 'destroyed'
            WHERE s.id IS NULL
        )`)
    if err != nil {
        return 0, err
    }
    n, _ := res.RowsAffected()
    return n, nil
}
```

#### 1.2 Startup Initialization

**File:** `pkg/engine/firecracker/engine.go` — `New()`

The existing `New()` creates only `DataDir/sandboxes/`. The new
entity directories must also be created at startup, otherwise the
first image pull / volume create / checkpoint fails with ENOENT:

```go
for _, sub := range []string{"sandboxes", "images", "volumes", "snapshots"} {
    if err := os.MkdirAll(filepath.Join(cfg.DataDir, sub), 0700); err != nil {
        return nil, fmt.Errorf("create %s dir: %w", sub, err)
    }
}
```

**Stale snapshot temp directory cleanup**: if the daemon was killed
during a checkpoint, a `.tmp` directory with gigabytes of partial
data may survive in the snapshots tree. Clean these on startup:

```go
// Clean stale checkpoint temp dirs left by crashed checkpoints
filepath.WalkDir(filepath.Join(cfg.DataDir, "snapshots"), func(path string, d fs.DirEntry, err error) error {
    if err != nil {
        return nil
    }
    if d.IsDir() && strings.HasSuffix(d.Name(), ".tmp") {
        slog.Info("removing stale snapshot temp dir", "path", path)
        os.RemoveAll(path)
        return filepath.SkipDir
    }
    return nil
})
```

This runs BEFORE any user requests are accepted, alongside the
existing orphaned TAP device cleanup and volume orphan reconciliation.

#### 1.3 Engine Changes

**File:** `pkg/engine/engine.go`

Replace the existing `VolumeMount` and `NewVolumes` fields on
`SandboxSpec` with a unified volume attachment:

```go
// PersistentVolume describes a named volume to attach to a sandbox.
type PersistentVolume struct {
    Name       string `json:"name"`        // volume name (scoped to user)
    Mount      string `json:"mount"`       // mount point inside VM
    SizeMB     int    `json:"size_mb"`     // used only if AutoCreate
    AutoCreate bool   `json:"auto_create"` // create if doesn't exist
    ReadOnly   bool   `json:"read_only"`
}
```

`SandboxSpec` changes:
```go
type SandboxSpec struct {
    // ... existing fields ...
    Volumes    []PersistentVolume `json:"volumes,omitempty"`  // replaces both VolumeMount and NewVolumes

    // Deprecated — kept for backward compat with v0.1 API, ignored if Volumes is set
    NewVolumes []VolumeSpec       `json:"new_volumes,omitempty"`

    // Set by server layer (not by API clients):
    BaseImage       string           `json:"-"` // resolved image file path (from store.GetImage)
    ResolvedVolumes []ResolvedVolume `json:"-"` // resolved volume file paths (from store + attach)
}
```

**File:** `pkg/engine/firecracker/engine.go`

In `Create()`, after rootfs copy and before Firecracker configuration:

Volume resolution and auto-creation happen in the **server layer**,
NOT in the engine. The engine only receives resolved file paths. This
ensures all volumes go through the store (quota checks, attachment
tracking) and avoids the engine creating files that the store doesn't
know about.

**Server layer** (in handleSandboxes POST, before engine.Create):
```go
// Resolve volumes: auto-create if needed, attach all, build spec
var engineVolumes []engine.ResolvedVolume
for _, vol := range req.Volumes {
    existing, err := store.GetVolume(userID, vol.Name)
    if err != nil && vol.AutoCreate && vol.SizeMB > 0 {
        // Auto-create: use the store's UNIQUE(user_id, name) constraint
        // as the coordination primitive, NOT filesystem O_EXCL.
        //
        // Why: O_EXCL wins the file race, but the loser then calls
        // store.GetVolume() which may return "not found" because the
        // winner is still running mkfs.ext4 (~200ms) and hasn't
        // committed store.CreateVolume yet. The loser gets a 500.
        //
        // Correct approach: insert the store record FIRST (name reservation),
        // then create the file. The store's unique constraint serializes
        // concurrent creates. Losers see "duplicate" and re-fetch.
        volDir := filepath.Join(dataDir, "volumes", userID)
        os.MkdirAll(volDir, 0700)
        volPath := filepath.Join(volDir, vol.Name+".ext4")

        storeVol := store.Volume{ID: generateID(), UserID: userID,
            Name: vol.Name, SizeMB: vol.SizeMB, FilePath: volPath}
        createErr := store.CreateVolume(storeVol)
        if createErr != nil {
            // UNIQUE constraint violation — another request won the race.
            // The winner has committed the store record, so GetVolume
            // will always succeed (no timing window).
            existing, err = store.GetVolume(userID, vol.Name)
            if err != nil {
                return fmt.Errorf("volume %q: race recovery failed: %w", vol.Name, err)
            }
        } else {
            // We won the race. Create the file.
            if err := createVolume(volPath, vol.SizeMB); err != nil {
                store.DeleteVolume(userID, vol.Name) // remove store record
                return fmt.Errorf("create volume %q: %w", vol.Name, err)
            }
            existing = &storeVol
        }
    } else if err != nil {
        return fmt.Errorf("volume %q not found", vol.Name)
    }

    // Attach (store transaction handles concurrency)
    if err := store.AttachVolume(userID, vol.Name, sandboxID, vol.Mount, vol.ReadOnly); err != nil {
        // Rollback previously attached volumes
        store.DetachAllForSandbox(sandboxID)
        return err // 409 conflict
    }

    engineVolumes = append(engineVolumes, engine.ResolvedVolume{
        FilePath: existing.FilePath,
        DriveID:  fmt.Sprintf("vol%d", len(engineVolumes)),
        Mount:    vol.Mount,
        ReadOnly: vol.ReadOnly,
    })
}
spec.ResolvedVolumes = engineVolumes
```

**Engine layer** receives only resolved paths:
```go
// engine.ResolvedVolume is a fully resolved volume reference.
// The server layer handles name resolution, auto-create, and attachment.
type ResolvedVolume struct {
    FilePath string `json:"file_path"` // host path to ext4 file
    DriveID  string `json:"drive_id"`  // Firecracker drive ID ("vol0")
    Mount    string `json:"mount"`     // guest mount point
    ReadOnly bool   `json:"read_only"`
}

// In Create():
// Maximum 24 volumes per sandbox (vdc through vdz).
// Firecracker supports more drive IDs but single-letter /dev/vdX
// names run out at 'z'. Exceeding this produces bogus device names.
const maxVolumesPerSandbox = 24

if len(spec.Volumes) + len(spec.NewVolumes) > maxVolumesPerSandbox {
    return info, fmt.Errorf("too many volumes: %d (max %d)",
        len(spec.Volumes)+len(spec.NewVolumes), maxVolumesPerSandbox)
}

var volumeMounts []VolumeMountConfig
driveIndex := byte('c') // vdb=config, vdc=first vol, ...
var volAttachments []VolumeAttachmentInfo

for _, vol := range spec.ResolvedVolumes {
    device := fmt.Sprintf("/dev/vd%c", driveIndex)
    volumeMounts = append(volumeMounts, VolumeMountConfig{
        Device: device, Mount: vol.Mount, FS: "ext4", ReadOnly: vol.ReadOnly,
    })
    volAttachments = append(volAttachments, VolumeAttachmentInfo{
        DriveID: vol.DriveID, Name: vol.Mount, FilePath: vol.FilePath,
        Mount: vol.Mount, ReadOnly: vol.ReadOnly,
    })
    driveIndex++
}

// Store in VM struct for later use by checkpoint
vm.Volumes = volAttachments
```

```go
// Also handle legacy NewVolumes for backward compat
for _, vs := range spec.NewVolumes {
    volPath := filepath.Join(sandboxDir, fmt.Sprintf("vol-%s.ext4", vs.Name))
    // ... existing code (creates ephemeral volume in sandbox dir) ...
}
```

Key difference: persistent volumes use `e.cfg.DataDir/volumes/{user_id}/`
while legacy NewVolumes use `sandboxDir/`. The sandbox dir is deleted on
destroy; the volumes dir is not.

In `Destroy()`:
```go
// Release volume attachments (but don't delete volume files)
// The server layer calls store.DetachAllVolumes(sandboxID)
// Volume files in /var/lib/bhatti/volumes/ are untouched
```

No change to `Destroy()` itself — `os.RemoveAll(sandboxDir)` only
removes the sandbox directory. Persistent volumes are outside it.

#### 1.4 Server / Routes Changes

**File:** `pkg/server/routes.go`

**Regression warning**: the store volume API changes signature entirely
(e.g., `CreateVolume(name string)` → `CreateVolume(v Volume)`,
`AttachVolume(sandboxID, volumeName, target, readonly)` →
`AttachVolume(userID, name, sandboxID, mount, readOnly)`). There are
9 call sites in `routes.go` plus 1 in `server_test.go`. All must be
updated atomically — a partial migration that compiles (e.g., wrong
arg order with matching types) produces silent wrong behavior. Compile
won't catch `AttachVolume(sbID, vol.Name, ...)` vs
`AttachVolume(userID, vol.Name, ...)` since both are strings.

Update `handleSandboxes POST` to handle persistent volumes:
- **Move volume resolution BEFORE `engine.Create()`** (currently it's after).
  Volumes must be reserved in the store before the VM boots, so a second
  concurrent request for the same volume gets a 409 instead of two VMs
  fighting over the same ext4 file.
- If any attachment fails: return 409 ("volume already attached")
- On `engine.Create()` failure: `store.DetachAllForSandbox(sandboxID)` to
  undo the pre-reservations (new rollback path — v0.1 had no equivalent)
- After sandbox destroy: `store.DetachAllForSandbox(sandboxID)` to release
  all volume attachments (replaces v0.1's `DetachVolumes`)

New volume endpoints:
```go
POST   /volumes          → handleVolumeCreate (user_id, name, size_mb)
GET    /volumes          → handleVolumeList (user-scoped)
GET    /volumes/:name    → handleVolumeGet
DELETE /volumes/:name    → handleVolumeDelete (must be detached)
POST   /volumes/:name/resize → handleVolumeResize (must be detached)
```

**Important: `handleVolumeDelete` deletion ordering.**

The volume_attachments junction table must have zero rows for this
volume. The store's `DeleteVolume` checks this:
```go
func (s *Store) DeleteVolume(userID, name string) error {
    tx, _ := s.db.Begin()
    defer tx.Rollback()

    var volID string
    tx.QueryRow("SELECT id FROM volumes_v2 WHERE user_id=? AND name=?",
        userID, name).Scan(&volID)
    if volID == "" {
        return fmt.Errorf("volume %q not found", name)
    }

    var count int
    tx.QueryRow("SELECT COUNT(*) FROM volume_attachments WHERE volume_id=?",
        volID).Scan(&count)
    if count > 0 {
        return fmt.Errorf("volume %q has %d active attachment(s)", name, count)
    }

    // Delete store record FIRST — makes volume invisible to API immediately.
    // If daemon crashes after this but before file removal, we get an
    // orphaned ext4 file on disk with no store record. Risks:
    //   - A new volume with the same name before startup reconciliation
    //     runs could pick up the old file (data leak). Mitigated by
    //     reconcileOrphanedVolumeFiles running BEFORE any user requests.
    //   - The file uses disk until cleanup.
    // Alternative (file-first): orphaned store record without file gives
    // clean "not found" errors but more confusing for the user.
    tx.Exec("DELETE FROM volumes_v2 WHERE id=?", volID)
    if err := tx.Commit(); err != nil {
        return err
    }

    // Delete file SECOND — if this fails, orphaned file is cleaned up on startup.
    filePath := filepath.Join(dataDir, "volumes", userID, name+".ext4")
    if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
        slog.Warn("failed to remove volume file", "path", filePath, "error", err)
    }
    return nil
}
```

**Startup reconciliation for orphaned files** (called from engine init):
```go
func reconcileOrphanedVolumeFiles(dataDir string, store *Store) {
    // Walk /var/lib/bhatti/volumes/*/  and check each .ext4 file
    // against the store. Files with no store record are orphans.
    filepath.WalkDir(filepath.Join(dataDir, "volumes"), func(path string, d fs.DirEntry, err error) error {
        if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".ext4") {
            return nil
        }
        name := strings.TrimSuffix(d.Name(), ".ext4")
        userID := filepath.Base(filepath.Dir(path))
        if _, err := store.GetVolume(userID, name); err != nil {
            slog.Info("removing orphaned volume file", "path", path)
            os.Remove(path)
        }
        return nil
    })
}
```

#### 1.5 CLI Changes

```
bhatti volume create --name workspace --size 5120
bhatti volume list
bhatti volume delete workspace
bhatti volume resize workspace --size 10240

bhatti create --name dev --volume workspace:/workspace
bhatti create --name dev --volume datasets:/data:ro   # read-only
```

The `--volume` flag format: `name:mount[:ro]`

#### 1.6 Tests

**Store tests (pkg/store/):**
- `TestVolumeCreateAndGet` — create volume, verify fields
- `TestVolumeUserScoped` — user A can't see/delete user B's volumes
- `TestVolumeAttachDetach` — attach to sandbox, verify attachment row created, detach, verify row deleted
- `TestVolumeDoubleAttachRejected` — attach RW to sb1, try RW attach to sb2 → error
- `TestVolumeReadOnlyMultiAttach` — attach RO to sb1, sb2, sb3 → all succeed, verify 3 attachment rows
- `TestVolumeRWBlocksRO` — attach RW to sb1, try RO attach to sb2 → error
- `TestVolumeROBlocksRW` — attach RO to sb1, try RW attach to sb2 → error
- `TestVolumeDeleteWhileAttached` — fails with error, verify attachment count in message
- `TestVolumeDeleteAfterDetach` — succeeds, verify attachment rows gone
- `TestDetachAllForSandbox` — sandbox has 3 volumes, detach all, verify all 3 released
- `TestDetachAllPreservesOtherSandbox` — sb1 and sb2 each have volumes, detach sb1, sb2's attachments intact
- `TestVolumeResize` — update size_mb, verify new value
- `TestVolumeResizeShrinkRejected` — resize to smaller size → error
- `TestDetachOrphanedVolumes` — create attachments to nonexistent sandbox IDs, run DetachOrphanedVolumes, verify cleaned up
- `TestDetachOrphanedVolumesPreservesValid` — mix of valid and orphaned attachments, only orphans removed
- `TestVolumeNameValidation` — names with `/`, `..`, null bytes, empty string → rejected
- `TestVolumeQuotaEnforcement` — user at max_volume_storage_mb, create → rejected

**Server tests (pkg/server/, mock engine):**
- `TestVolumeCreateHTTP` — POST /volumes with size, verify 201
- `TestVolumeListHTTP` — user-scoped listing
- `TestVolumeScopingHTTP` — user A can't access user B's volumes
- `TestSandboxWithVolume` — create sandbox with --volume, verify sandbox created
- `TestSandboxVolumeConflict` — two sandboxes same volume → 409 on second
- `TestSandboxDestroyReleasesVolume` — destroy sandbox, volume is detached
- `TestVolumeDeleteWhileAttachedHTTP` — 409
- `TestVolumeResizeWhileAttachedHTTP` — 409
- `TestVolumeResizeHTTP` — resize detached volume, verify new size
- `TestVolumeResizeShrinkHTTP` — resize to smaller size → 400
- `TestSandboxCreateFailRollbacksVolumes` — engine.Create fails, verify
  all volumes that were attached during the request are detached
- `TestSandboxCreateNoVolumesRegression` — create sandbox with zero
  volumes (the v0.1 path), verify it still works end-to-end. This is
  the #1 regression test — the volume resolution loop now runs before
  engine.Create(), and an off-by-one or nil-slice bug here breaks ALL
  sandbox creation, not just the volume path.
- `TestVolumeDeleteOrdering` — delete volume, verify store record gone
  before file (mock filesystem to verify call order)

**Integration tests (pkg/engine/firecracker/, agni-01):**
- `TestPersistentVolumeData` — create volume, write data in sb1, destroy sb1,
  create sb2 with same volume, read data → data persists
- `TestVolumeOwnership` — files in volume owned by uid 1000
- `TestVolumeReadOnlyMount` — write to RO volume → fails inside VM
- `TestVolumeReadOnlyFirecrackerDrive` — verify Firecracker drive was
  configured with `is_read_only: true` (check via FC API)
- `TestVolumeMultiplePerSandbox` — sandbox with 2 volumes, verify both mounted
- `TestVolumeAutoCreate` — sandbox with auto_create volume, volume created on first use
- `TestVolumeAutoCreateStoreRecord` — auto-created volume has correct store record (name, size, user_id)
- `TestVolumeSurvivesSnapshot` — stop + start sandbox with volume, data intact
- `TestEphemeralVolumesStillWork` — legacy NewVolumes path still functions
- `TestVolumeAttachmentInfoInVMState` — after create, VMState() includes
  volume attachment info (drive_id, file_path, mount); after RestoreVM,
  the info is recovered

**Crash recovery tests (integration, agni-01):**
- `TestCrashBetweenDestroyAndDetach` — create sandbox with volume, kill
  daemon between engine.Destroy and store.DetachAllForSandbox, restart,
  verify volume is auto-detached by DetachOrphanedVolumes on startup
- `TestCrashDuringVolumeCreate` — kill daemon during mkfs.ext4, restart,
  verify partial ext4 file is cleaned up by reconcileOrphanedVolumeFiles
- `TestOrphanedVolumeFile` — delete store record but leave ext4 file,
  restart, verify reconciliation removes the orphan
- `TestOrphanedVolumeFileNoFalsePositive` — volume file with valid store
  record, verify reconciliation does NOT delete it
- `TestCrashDuringVolumeDelete` — delete store record (simulated crash
  before file deletion), restart, verify orphaned file cleaned up

**Concurrency tests (pkg/store/ + pkg/server/):**
- `TestConcurrentAttachSameVolume` — 10 goroutines attach same volume
  read-write simultaneously → exactly one succeeds, 9 get error
- `TestAutoCreateRace` — 5 goroutines create sandbox with same auto_create
  volume → only one volume file + store record created, all sandboxes
  get the same volume, no mkfs overwrite
- `TestConcurrentAttachReadOnly` — 10 goroutines attach same volume
  read-only simultaneously → all 10 succeed, 10 attachment rows exist
- `TestConcurrentAttachROMixedWithRW` — 5 RO goroutines + 5 RW goroutines
  simultaneously → either all RO succeed (and all RW fail) or exactly
  one RW succeeds (and all RO fail), never a mix
- `TestConcurrentSaveImageAndExec` — save-as-image while exec is
  running → exec blocks until save completes, then returns normally
- `TestConcurrentCheckpointAndExec` — checkpoint while exec is running →
  exec blocks until checkpoint completes, then returns normally

**Schema migration test:**
- `TestMigrateV02ToV03` — create a v0.2 database (old volumes table with
  just name + created_at), run New() which applies migrations, verify:
  - old volume records accessible (or gracefully absent — v0.1 volumes
    were Docker-only, Firecracker volumes are new)
  - new volumes_v2 and volume_attachments tables exist
  - existing sandboxes, users, templates, secrets are preserved

**Guest-side edge cases (integration, agni-01):**
- `TestMountCorruptVolume` — attach a volume with invalid ext4 (zeroed
  file), verify lohar logs error and continues (doesn't crash)
- `TestMountMissingDevice` — config drive references /dev/vde but only
  3 drives attached, verify lohar logs error and continues
- `TestMountPointConflict` — volume mount at /workspace which already
  has files in rootfs, verify mount overlays correctly (existing files
  hidden, volume content visible)
- `TestVolumeReadOnlyMountGuest` — exec `touch /vol/test` on a RO-mounted
  volume → "Read-only file system" error (verifies MS_RDONLY propagation)
- `TestDirtyVolumeROMultiAttach` — crash a VM with a RW-attached volume
  (dirty journal), try to RO-attach to two sandboxes → second attach
  rejected with "dirty journal" error


### Phase 2: Custom Images + OCI Pull

#### 2.1 Image Store Schema

```sql
CREATE TABLE images (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL DEFAULT '',     -- '' = admin/global image
    name TEXT NOT NULL,
    source TEXT NOT NULL DEFAULT '',      -- 'admin', 'oci:docker.io/library/python:3.12', 'saved:sandbox-abc'
    file_path TEXT NOT NULL,             -- /var/lib/bhatti/images/{name}.ext4 or usr_{id}/{name}.ext4
    size_mb INTEGER NOT NULL DEFAULT 0,
    oci_config_json TEXT NOT NULL DEFAULT '{}',  -- extracted OCI config (env, workdir, cmd, etc.)
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(user_id, name)
);
```

Store methods:
```go
type Image struct {
    ID        string            `json:"id"`
    UserID    string            `json:"user_id"`
    Name      string            `json:"name"`
    Source    string             `json:"source"`
    FilePath  string            `json:"-"`
    SizeMB    int               `json:"size_mb"`
    OCIConfig ImageConfig       `json:"oci_config"`
    CreatedAt time.Time         `json:"created_at"`
}

type ImageConfig struct {
    Env        map[string]string `json:"env,omitempty"`
    WorkingDir string            `json:"working_dir,omitempty"`
    Cmd        []string          `json:"cmd,omitempty"`
    User       string            `json:"user,omitempty"`
}

func (s *Store) CreateImage(img Image) error
func (s *Store) GetImage(userID, name string) (*Image, error)       // checks user + admin
func (s *Store) GetAdminImage(name string) (*Image, error)          // admin only
func (s *Store) ListImages(userID string) ([]Image, error)          // user's + admin
func (s *Store) DeleteImage(userID, name string) error
```

`GetImage` first checks `user_id = ?`, then falls back to `user_id = ''`
(admin images). This gives user images priority — a user can shadow an
admin image with their own version.

#### 2.2 OCI Conversion Package

**New package:** `pkg/oci/`

```go
package oci

// PullAndConvert pulls an OCI image from a registry, flattens it to
// an ext4 rootfs image, injects the lohar agent, and returns the
// path to the created ext4 file.
//
// The loharPath is the path to the lohar binary on the host.
// The outputPath is where the ext4 file will be written.
// Returns the extracted OCI config for storage.
func PullAndConvert(ctx context.Context, ref, outputPath, loharPath string, opts ...Option) (*Config, error)

type Config struct {
    Env          map[string]string
    WorkingDir   string
    Cmd          []string
    User         string
    ExposedPorts []int
    TotalSize    int64    // flattened size in bytes
}

type Option func(*pullOptions)
func WithAuth(user, password string) Option
func WithPlatform(os, arch string) Option  // MUST default to runtime.GOARCH
```

**File:** `pkg/oci/pull.go`

**Platform resolution**: Docker Hub serves multi-platform manifests.
`crane.Pull("python:3.12")` defaults to the build host's architecture.
If bhatti is compiled for amd64, it pulls amd64 images. But if someone
runs the CLI on an arm64 Mac (for `bhatti image pull`), crane pulls
arm64 — which won't run on an amd64 Firecracker host. The default
platform must be the TARGET host architecture (the Firecracker VM arch),
not the build/CLI architecture. For `bhatti image pull` run remotely
via API, the server knows its own `runtime.GOARCH`. For CLI-initiated
pulls, the pull should happen server-side, not client-side.

```go
func PullAndConvert(ctx context.Context, ref, outputPath, loharPath string, opts ...Option) (*Config, error) {
    // 1. Pull image
    img, err := crane.Pull(ref, craneOpts...)

    // 2. Read config
    cfgFile, _ := img.ConfigFile()
    config := extractConfig(cfgFile)

    // 3. Create temp dir for flattening
    tmpDir, _ := os.MkdirTemp("", "bhatti-oci-*")
    defer os.RemoveAll(tmpDir)

    // 4. Flatten layers
    layers, _ := img.Layers()
    for _, layer := range layers {
        if err := extractLayer(layer, tmpDir); err != nil {
            return nil, fmt.Errorf("extract layer: %w", err)
        }
    }

    // 5. Inject bhatti components
    if err := injectLohar(tmpDir, loharPath); err != nil {
        return nil, fmt.Errorf("inject lohar: %w", err)
    }

    // 6. Validate compatibility
    if warnings := validateImage(tmpDir); len(warnings) > 0 {
        for _, w := range warnings {
            slog.Warn("oci image warning", "ref", ref, "issue", w)
        }
    }

    // 7. Create ext4
    if err := createExt4FromDir(tmpDir, outputPath); err != nil {
        return nil, fmt.Errorf("create ext4: %w", err)
    }

    return config, nil
}
```

**File:** `pkg/oci/flatten.go`

```go
// extractLayer extracts a single OCI layer tar into the target directory.
//
// Uses single-pass extraction with post-extraction whiteout application.
// The OCI spec does not guarantee ordering within a layer tar — an opaque
// whiteout (.wh..wh..opq) can appear after regular files in the same
// directory. We extract everything (including whiteout markers as-is),
// then apply whiteouts after extraction. This avoids decompressing the
// layer twice (which doubles I/O cost for large images).
//
// The only edge case: an opaque whiteout in the SAME layer as new files
// in that directory. After extraction, the opaque marker deletes files
// from LOWER layers, then we remove the marker. Files from THIS layer
// that were extracted after the marker survive because they were written
// after removeDirectoryContents ran... wait, no — we apply whiteouts
// AFTER extracting ALL files. So opaque whiteout + same-layer files
// requires special handling:
//   1. Extract all files and whiteout markers
//   2. For opaque whiteouts: delete files NOT from this layer
// This requires tracking which files came from this layer vs previous.
// Simpler: extract to a staging dir, apply whiteouts to target, then
// merge staging into target.
//
// In practice, Docker's layer builder emits opaque whiteouts BEFORE
// content in the same directory. But we handle the general case.
func extractLayer(layer v1.Layer, targetDir string) error {
    // Stage this layer's files in a temp dir
    stageDir, _ := os.MkdirTemp("", "bhatti-layer-*")
    defer os.RemoveAll(stageDir)

    reader, _ := layer.Uncompressed()
    tr := tar.NewReader(reader)

    type whiteout struct {
        path   string // relative to targetDir
        opaque bool
    }
    var whiteouts []whiteout

    for {
        header, err := tr.Next()
        if err == io.EOF { break }
        if err != nil { return err }

        base := filepath.Base(header.Name)

        // Collect whiteout entries (don't extract them as files)
        if base == ".wh..wh..opq" {
            whiteouts = append(whiteouts, whiteout{
                path: filepath.Dir(header.Name), opaque: true})
            continue
        }
        if strings.HasPrefix(base, ".wh.") {
            whiteouts = append(whiteouts, whiteout{
                path: filepath.Join(filepath.Dir(header.Name),
                    strings.TrimPrefix(base, ".wh.")), opaque: false})
            continue
        }

        // Path traversal protection
        path := filepath.Join(stageDir, header.Name)
        if !strings.HasPrefix(filepath.Clean(path), filepath.Clean(stageDir)) {
            continue
        }

        // Extract to staging directory
        switch header.Typeflag {
        case tar.TypeDir:
            os.MkdirAll(path, os.FileMode(header.Mode))
        case tar.TypeReg:
            writeFile(path, tr, os.FileMode(header.Mode))
        case tar.TypeSymlink:
            os.MkdirAll(filepath.Dir(path), 0755)
            os.Symlink(header.Linkname, path)
        case tar.TypeLink:
            os.MkdirAll(filepath.Dir(path), 0755)
            os.Link(filepath.Join(stageDir, header.Linkname), path)
        case tar.TypeBlock, tar.TypeChar:
            continue // skip device nodes
        }
        os.Lchown(path, header.Uid, header.Gid)
    }

    // Apply whiteouts to target (deletes from previous layers)
    for _, wo := range whiteouts {
        target := filepath.Join(targetDir, wo.path)
        if wo.opaque {
            removeDirectoryContents(target)
        } else {
            os.RemoveAll(target)
        }
    }

    // Merge staged files into target (this layer's files overwrite previous).
    // Use filepath.Walk + os.Rename instead of `cp -a` subprocess.
    // Rename is O(1) on the same filesystem (stageDir and targetDir are
    // both under the same MkdirTemp parent). For a 12-layer image this
    // eliminates 12 cp processes and avoids re-reading every file.
    mergeDir(stageDir, targetDir) // Walk stageDir, Rename files into targetDir

    return nil
}
```

**File:** `pkg/oci/inject.go`

```go
// injectLohar copies the lohar binary and ensures boot directories exist.
func injectLohar(rootDir, loharPath string) error {
    // Copy lohar
    dst := filepath.Join(rootDir, "usr/local/bin/lohar")
    os.MkdirAll(filepath.Dir(dst), 0755)
    copyFile(loharPath, dst)
    os.Chmod(dst, 0755)

    // Ensure boot directories
    for _, dir := range []string{
        "proc", "sys", "dev", "dev/pts", "tmp", "run", "workspace",
    } {
        os.MkdirAll(filepath.Join(rootDir, dir), 0755)
    }

    // Fix resolv.conf (may be a broken symlink from systemd-resolved)
    resolvPath := filepath.Join(rootDir, "etc/resolv.conf")
    os.Remove(resolvPath) // remove symlink if exists
    os.WriteFile(resolvPath, []byte("nameserver 1.1.1.1\nnameserver 8.8.8.8\n"), 0644)

    // Ensure uid 1000 user exists
    if err := ensureUser1000(rootDir); err != nil {
        return fmt.Errorf("ensure user: %w", err)
    }

    return nil
}

// ensureUser1000 checks if uid 1000 exists in /etc/passwd.
// If not, creates a 'lohar' user with uid 1000.
// If uid 1000 exists (e.g., 'node' in node images), leaves it as-is.
func ensureUser1000(rootDir string) error {
    passwdPath := filepath.Join(rootDir, "etc/passwd")
    data, err := os.ReadFile(passwdPath)
    if err != nil {
        return nil // no passwd file (scratch/distroless), skip
    }

    // Check if uid 1000 already exists
    for _, line := range strings.Split(string(data), "\n") {
        fields := strings.Split(line, ":")
        if len(fields) >= 4 && fields[2] == "1000" {
            // uid 1000 exists — ensure home directory exists
            homeDir := "/home/lohar"
            if len(fields) >= 6 && fields[5] != "" {
                homeDir = fields[5] // use the image's home dir
            }
            os.MkdirAll(filepath.Join(rootDir, homeDir), 0755)
            os.Chown(filepath.Join(rootDir, homeDir), 1000, 1000)
            return nil
        }
    }

    // uid 1000 doesn't exist — create via chroot + useradd if available,
    // fall back to direct passwd editing if useradd isn't in the image.
    //
    // useradd handles: /etc/passwd, /etc/shadow, /etc/group, /etc/gshadow,
    // home directory creation, and proper field formatting.
    // Direct passwd editing misses /etc/shadow (breaks sudo on Debian/Ubuntu).
    useraddPath := filepath.Join(rootDir, "usr/sbin/useradd")
    if _, err := os.Stat(useraddPath); err == nil {
        cmd := exec.Command("chroot", rootDir,
            "useradd", "-m", "-u", "1000", "-s", "/bin/sh", "lohar")
        if err := cmd.Run(); err != nil {
            // May fail for cross-arch images (arm64 image on amd64 host
            // gives "exec format error") or if chroot is unavailable.
            // Fall through to manual passwd editing below.
            slog.Debug("chroot useradd failed, falling back to manual",
                "rootDir", rootDir, "error", err)
        }
        // Verify it worked
        data2, _ := os.ReadFile(passwdPath)
        if strings.Contains(string(data2), ":1000:") {
            return nil
        }
    }

    // Fallback: manual passwd editing (for minimal images without useradd)
    // Also create shadow entry so sudo doesn't fail
    f, _ := os.OpenFile(passwdPath, os.O_APPEND|os.O_WRONLY, 0644)
    f.WriteString("lohar:x:1000:1000::/home/lohar:/bin/sh\n")
    f.Close()

    groupPath := filepath.Join(rootDir, "etc/group")
    // Check if gid 1000 already exists before adding
    groupData, _ := os.ReadFile(groupPath)
    gid1000Exists := false
    for _, line := range strings.Split(string(groupData), "\n") {
        fields := strings.Split(line, ":")
        if len(fields) >= 3 && fields[2] == "1000" {
            gid1000Exists = true
            break
        }
    }
    if !gid1000Exists {
        g, _ := os.OpenFile(groupPath, os.O_APPEND|os.O_WRONLY, 0644)
        g.WriteString("lohar:x:1000:\n")
        g.Close()
    }

    // Create shadow entry (required for sudo on Debian/Ubuntu)
    shadowPath := filepath.Join(rootDir, "etc/shadow")
    if _, err := os.Stat(shadowPath); err == nil {
        s, _ := os.OpenFile(shadowPath, os.O_APPEND|os.O_WRONLY, 0640)
        s.WriteString("lohar:!:19000:0:99999:7:::\n")
        s.Close()
    }

    os.MkdirAll(filepath.Join(rootDir, "home/lohar"), 0755)
    os.Chown(filepath.Join(rootDir, "home/lohar"), 1000, 1000)
    return nil
}
```

**File:** `pkg/oci/validate.go`

```go
// validateImage checks for known incompatibilities and returns warnings.
func validateImage(rootDir string) []string {
    var warnings []string

    // Check for systemd (won't work — lohar is PID 1)
    if exists(rootDir, "lib/systemd/systemd") || exists(rootDir, "usr/lib/systemd/systemd") {
        warnings = append(warnings, "image contains systemd — it will NOT run as PID 1, lohar replaces it")
    }

    // Check for Docker-in-Docker
    if exists(rootDir, "usr/bin/dockerd") {
        warnings = append(warnings, "image contains dockerd — Docker-in-Docker is not supported in Firecracker VMs")
    }

    // Check for NVIDIA/GPU libraries
    if globExists(rootDir, "usr/lib/*/libcuda.*") || exists(rootDir, "usr/local/cuda") {
        warnings = append(warnings, "image contains CUDA libraries — GPU passthrough is not supported in Firecracker")
    }

    // Check for missing shell
    hasShell := exists(rootDir, "bin/sh") || exists(rootDir, "usr/bin/sh") ||
                exists(rootDir, "bin/bash") || exists(rootDir, "usr/bin/bash")
    if !hasShell {
        warnings = append(warnings, "image has no /bin/sh — exec commands will fail. "+
            "This image may be a 'scratch' or 'distroless' image which is not compatible with bhatti")
    }

    // Check for FUSE
    if exists(rootDir, "usr/bin/fusermount") || exists(rootDir, "usr/bin/fusermount3") {
        warnings = append(warnings, "image contains FUSE tools — FUSE is not supported in the Firecracker guest kernel")
    }

    // Check sudo availability (bhatti runs exec as uid 1000, not root)
    hasSudo := exists(rootDir, "usr/bin/sudo") || exists(rootDir, "bin/sudo")
    if !hasSudo {
        warnings = append(warnings, "image does not have sudo — commands that need root will fail. "+
            "Install sudo in the image or use 'bhatti image save' from a sandbox with sudo configured")
    }

    return warnings
}
```

**File:** `pkg/oci/ext4.go`

```go
// createExt4FromDir creates an ext4 image from a directory tree.
// Uses mke2fs -d to populate the filesystem without mounting, avoiding
// leaked loop devices on crash and reducing syscall overhead for
// directories with many small files (node_modules).
func createExt4FromDir(srcDir, outputPath string) error {
    // 1. Calculate required size
    totalSize, fileCount, err := dirStats(srcDir)
    if err != nil {
        return err
    }

    // Add 30% headroom for ext4 metadata (journal, inode table, block group
    // descriptors, superblock copies). 20% is too tight for images with many
    // small files where inode overhead alone exceeds 5%. mke2fs does NOT
    // auto-expand — if this estimate is even one block group short, mke2fs
    // fails with "Not enough space" and leaves a partial image.
    // Minimum 512MB to avoid micro-images with no free space.
    sizeMB := int(totalSize/1024/1024) * 130 / 100
    if sizeMB < 512 {
        sizeMB = 512
    }

    // 2. Create sparse file
    f, _ := os.Create(outputPath)
    f.Truncate(int64(sizeMB) << 20)
    f.Close()

    // 3. Format and populate in one step using mke2fs -d.
    //    This avoids the mount/umount dance entirely — no loop device,
    //    no risk of leaked mounts on crash, faster for many small files.
    //
    //    -N: inode count (1 per 4KB or fileCount*1.5, whichever is more)
    //    -d: populate from directory
    inodes := max(fileCount * 3 / 2, totalSize / 4096)
    cmd := exec.Command("mke2fs",
        "-t", "ext4",
        "-d", srcDir,           // populate from directory
        "-N", fmt.Sprint(inodes),
        "-F", "-q",
        outputPath)
    if out, err := cmd.CombinedOutput(); err != nil {
        os.Remove(outputPath) // clean up truncated/partial file on failure
        return fmt.Errorf("mke2fs: %s: %w", out, err)
    }

    return nil
}
```

Note: `mke2fs -d` requires e2fsprogs >= 1.43 (Ubuntu 18.04+). It
handles permissions, symlinks, and timestamps. It does NOT require
root — no mount/umount. This also fixes the existing bug in
`createConfigDrive` which uses mount/umount and can leak loop devices
on crash. Migrate `createConfigDrive` to use `mke2fs -d` as well.

**Config drive sizing**: the existing `createConfigDrive` hardcodes a
1MB ext4 image. With ext4 overhead on 1MB, only ~500KB is usable. A
sandbox with 20 volume mounts, 50 env vars, 10 injected files, and a
long init script can exceed this. Bump to 4MB (still trivial for disk
usage, eliminates the silent failure where `write config.json` returns
ENOSPC and the sandbox boots with no config). When migrating to
`mke2fs -d`, calculate size from the serialized JSON + 50% headroom
with a 1MB floor.

**Known `mke2fs -d` limitations:**
- **Hard links**: preserved only in e2fsprogs >= 1.45. Ubuntu 18.04
  ships 1.44, which copies hard links as independent files. Ubuntu 20.04+
  (1.45.5) is fine. Since bhatti targets Ubuntu 22.04+, this is OK.
  Add a version check during build/startup:
  ```go
  func checkE2fsprogsVersion() error {
      out, _ := exec.Command("mke2fs", "-V").CombinedOutput()
      // parse "mke2fs 1.46.5 (30-Dec-2021)" and check >= 1.45
  }
  ```
- **Extended attributes (xattr)**: NOT copied by `mke2fs -d`. Some
  Docker images use `security.capability` xattrs on binaries like
  `ping` (to grant `CAP_NET_RAW` without setuid). In bhatti VMs, ping
  uses the guest kernel's network stack and lohar runs exec as uid 1000
  — ping won't work regardless (needs CAP_NET_RAW). This is acceptable.
  Document: "images using file capabilities (xattr) for privilege
  escalation will lose those capabilities during conversion."
- **Large files**: `mke2fs -d` streams files into the image. No known
  size limits beyond the ext4 file size limit (16TB).

#### 2.3 Engine Changes

In `Create()`, resolve image name to file path:

```go
// 1. Resolve image
// Image resolution happens in the SERVER LAYER (like volumes), not the
// engine. The engine receives a resolved file path in spec.BaseImage.
// This ensures all image access goes through store.GetImage() — so a
// deleted image whose orphaned file wasn't cleaned up yet can't be
// used, and a missing file gives a clear "image not found" error
// instead of a confusing "copy rootfs" failure.
//
// Server layer (before engine.Create):
//   if req.Image != "" {
//       img, err := store.GetImage(userID, req.Image) // checks user, then admin
//       if err != nil { return 404 }
//       spec.BaseImage = img.FilePath
//   }
//
// Engine layer:
baseImage := e.cfg.BaseRootfs // default
if spec.BaseImage != "" {
    baseImage = spec.BaseImage
}

rootfsPath := filepath.Join(sandboxDir, "rootfs.ext4")
if err = copyRootfs(baseImage, rootfsPath); err != nil {
    return info, fmt.Errorf("copy rootfs: %w", err)
}

// Note: copyRootfs should use `cp --reflink=auto --sparse=always` on the
// fallback path (when reflink fails). Without --sparse=always, a 10GB
// rootfs that's 90% empty materializes as 10GB on disk instead of ~1GB.
// This matters especially for named snapshots that copy rootfs + mem.snap.
//
// Updated copyRootfs:
//   func copyRootfs(src, dst string) error {
//       if err := exec.Command("cp", "--reflink=always", src, dst).Run(); err == nil {
//           return nil
//       }
//       return exec.Command("cp", "--sparse=always", src, dst).Run()
//   }

// Lohar re-injection: done during image creation (OCI pull, save-as-image,
// import), NOT on every boot. Injecting on every boot would add 300-800ms
// (mount rootfs, copy binary, unmount) and negate Firecracker's fast-boot
// advantage. Instead:
//   - OCI pull: injectLohar() during conversion (already in pipeline)
//   - save-as-image: SaveImage() copies the rootfs which already has the
//     current lohar (it's a running sandbox from this bhatti version)
//   - import: injectLohar() during import
//   - upgrade path: `bhatti image rebuild` re-injects lohar into all
//     cached images. Run after upgrading bhatti.
// The tradeoff: saved images pin lohar to the version at save time.
// This is acceptable — `bhatti image rebuild` is the explicit upgrade
// path. Doing it implicitly on every boot is too expensive.

// Resize if requested
if spec.DiskSizeMB > 0 {
    // e2fsck before resize2fs — resize on a dirty filesystem amplifies corruption.
    // Only run when actually resizing, not on every boot.
    exec.Command("e2fsck", "-f", "-y", rootfsPath).Run() // best effort
    if err = exec.Command("truncate", "-s", fmt.Sprintf("%dM", spec.DiskSizeMB), rootfsPath).Run(); err != nil {
        return info, fmt.Errorf("resize rootfs: %w", err)
    }
    if err = exec.Command("resize2fs", rootfsPath).Run(); err != nil {
        return info, fmt.Errorf("resize2fs: %w", err)
    }
}
```

Save-as-image in the engine:

New methods `SaveImage` and `Checkpoint`/`ResumeSnapshot` are NOT added
to the `engine.Engine` interface — they're Firecracker-specific. The
server layer uses a type assertion:
```go
if imgEngine, ok := s.engine.(interface {
    SaveImage(ctx context.Context, sandboxID, destPath string) error
}); ok {
    imgEngine.SaveImage(ctx, sandboxID, destPath)
}
```
This avoids breaking the Docker engine (if it still exists) or any
future engine implementations that don't support snapshots.

```go
func (e *Engine) SaveImage(ctx context.Context, sandboxID, destPath string) error {
    vm, err := e.getVM(sandboxID)
    if err != nil {
        return err
    }

    // Hold stateMu for the ENTIRE operation (pause + copy + resume).
    //
    // Why not release the lock during the copy? Because any concurrent
    // caller (Resume, Stop, Destroy) could mutate VM state while the
    // lock is released:
    //   - Resume() sees Thermal=="warm" and unpauses vCPUs → rootfs
    //     is written to mid-copy → corrupted image
    //   - Destroy() deletes the rootfs file → copy reads from deleted fd
    //   - Stop() snapshots and kills the VMM → resume below fails
    //
    // The cost: all Exec/Shell/FileRead operations on this sandbox block
    // for the duration of the copy (1-2s on NVMe for a 2GB rootfs).
    // This is acceptable — the user explicitly asked to save, and
    // the VM is paused anyway (no processes running to interact with).
    vm.stateMu.Lock()
    defer vm.stateMu.Unlock()

    // CRITICAL: flush the guest kernel's dirty page cache BEFORE pausing.
    // Pausing vCPUs does NOT flush dirty pages from guest RAM to the
    // virtio-blk device. Without sync, recent writes (pip install,
    // file saves, etc.) exist only in guest page cache and will be
    // MISSING from the saved image. This is different from snapshots
    // where mem.snap captures the page cache contents.
    if vm.Thermal == "hot" && vm.Agent != nil {
        vm.Agent.Exec(context.Background(), []string{"sync"}, nil, "")
    }

    wasPaused := vm.Thermal == "warm"
    if vm.Thermal == "hot" {
        client := fcAPIClient(vm.SocketPath)
        if err := fcPatch(client, "/vm", `{"state":"Paused"}`); err != nil {
            return fmt.Errorf("pause for save: %w", err)
        }
        vm.Thermal = "warm"
    }

    // Copy rootfs (VM is paused, lock is held, no concurrent mutation possible)
    if err := copyRootfs(vm.RootfsPath, destPath); err != nil {
        // Resume even on copy failure — don't leave VM paused
        if !wasPaused {
            client := fcAPIClient(vm.SocketPath)
            fcPatch(client, "/vm", `{"state":"Resumed"}`)
            vm.Thermal = "hot"
        }
        return fmt.Errorf("copy rootfs: %w", err)
    }

    // Resume
    if !wasPaused {
        client := fcAPIClient(vm.SocketPath)
        if err := fcPatch(client, "/vm", `{"state":"Resumed"}`); err != nil {
            return fmt.Errorf("resume after save: %w", err)
        }
        vm.Thermal = "hot"
    }

    return nil
}
```

#### 2.4 Tests

**OCI package unit tests (pkg/oci/, run anywhere):**
- `TestExtractLayerBasic` — single layer with files, verify extracted
- `TestExtractLayerWhiteoutFile` — `.wh.config.json` deletes file from lower layer
- `TestExtractLayerWhiteoutOpaque` — `.wh..wh..opq` clears directory
- `TestExtractLayerSymlinks` — symlinks preserved
- `TestExtractLayerHardLinks` — hard links preserved (same inode)
- `TestExtractLayerPermissions` — file modes preserved
- `TestExtractLayerDeviceNodesSkipped` — block/char devices ignored
- `TestInjectLohar` — lohar binary present, boot dirs exist
- `TestInjectLoharResolvConf` — broken symlink replaced with file
- `TestEnsureUser1000Exists` — uid 1000 already in passwd → no change
- `TestEnsureUser1000Missing` — uid 1000 not in passwd → lohar user added
- `TestEnsureUser1000NoPasswd` — no /etc/passwd file → skip gracefully
- `TestValidateImageClean` — normal image, no warnings
- `TestValidateImageSystemd` — detects systemd
- `TestValidateImageDockerInDocker` — detects dockerd
- `TestValidateImageCuda` — detects CUDA
- `TestValidateImageNoShell` — detects missing /bin/sh
- `TestValidateImageFuse` — detects FUSE tools
- `TestValidateImageNoSudo` — detects missing sudo
- `TestValidateImageWithSudo` — sudo present, no warning
- `TestCreateExt4FromDir` — verify ext4 image is mountable
- `TestCreateExt4InodeCount` — directory with 50000 small files, verify no inode exhaustion
- `TestCreateExt4Size` — verify image size has headroom

These tests use crafted tar layers and temp directories, not real
Docker images. They run on any Linux system without network access.

**OCI whiteout edge case tests (pkg/oci/, unit):**
- `TestWhiteoutAfterFileInSameLayer` — layer tar has `dir/file-a` THEN
  `dir/.wh..wh..opq` (opaque whiteout after files). Single-pass would
  delete file-a. Two-pass must preserve it. This is the critical
  ordering test.
- `TestWhiteoutEmptyLayer` — layer with only whiteout entries, no files
- `TestWhiteoutDeletedInUpperLayer` — layer 1 creates `/etc/config`,
  layer 2 creates `/etc/.wh.config`, verify file absent in result
- `TestWhiteoutOpaquePartialOverlay` — layer 1 creates `dir/{a,b,c}`,
  layer 2 has `.wh..wh..opq` + `dir/{a,d}`, verify result has {a,d} only
- `TestPathTraversalInTar` — tar entry with `../../etc/passwd`, verify
  skipped (path traversal protection in extractLayer)
- `TestEmptyTarLayer` — zero entries → no error, no changes
- `TestLayerWithVeryLongFilename` — 300-char path component → extracted
  correctly (ext4 supports up to 255 per component, but the flattened
  dir is on the host which may be different)

**mke2fs compatibility test (pkg/oci/, unit):**
- `TestE2fsprogsVersion` — call `checkE2fsprogsVersion()`, verify it
  returns nil on supported versions and error on unsupported. Run as
  part of `TestMain` setup — skip entire OCI test suite if e2fsprogs
  is too old.

**OCI integration tests (pkg/oci/, needs network + root):**
- `TestPullAndConvertAlpine` — pull alpine:latest (5MB), convert, verify
  ext4 mountable, /bin/sh exists, lohar exists
- `TestPullAndConvertPython` — pull python:3.12-slim (~150MB), convert,
  verify python3 binary exists
- `TestPullAndConvertNode` — pull node:22-slim, convert, verify node
  binary exists, verify uid 1000 = `node` (not overwritten)
- `TestPullAndConvertDistroless` — pull gcr.io/distroless/static,
  convert, verify warning about missing shell
- `TestPullAndConvertUbuntu` — pull ubuntu:24.04, verify resolv.conf
  is a regular file (not systemd symlink)
- `TestPullPrivateImageAuth` — pull from private registry with auth
  (needs test registry or skip)
- `TestPullContextCancellation` — start a pull, cancel context after
  2 seconds, verify temp files are cleaned up and no partial ext4 on disk
- `TestPullDiskFull` — set up a small tmpfs, pull a large image →
  verify error, no partial files left, no leaked temp dirs
- `TestPullDuplicatePrevention` — start two concurrent pulls of same
  ref for same user → second returns first's task ID, only one
  download happens

**Async task tests (pkg/server/):**
- `TestImagePullReturns202` — POST /images/pull → 202 with task_id
- `TestTaskPolling` — poll GET /tasks/{id} → status transitions from
  "running" → "completed" (or "failed")
- `TestTaskCleanup` — create task, advance clock 25 hours, verify
  task is deleted by cleanup goroutine
- `TestTaskCancellation` — DELETE /tasks/{id} on running task → task
  cancelled, partial files cleaned up
- `TestTaskUserScoped` — user A can't see user B's tasks

**Engine integration tests (agni-01, real Firecracker):**
- `TestBootFromOCIImage` — pull alpine, convert, boot sandbox with
  `image: alpine`, exec `cat /etc/alpine-release` → success
- `TestBootFromPythonImage` — pull python:3.12-slim, boot, exec
  `python3 -c "print('hello')"` → "hello"
- `TestBootFromNodeImage` — pull node:22-slim, boot, exec
  `node -e "console.log('ok')"` → "ok"
- `TestImageEnvMerge` — OCI image has `PYTHON_VERSION=3.12`, sandbox
  adds `MY_VAR=test`, exec `env` shows both
- `TestSaveAndBootImage` — create sandbox, install package, save as
  image, boot new sandbox from saved image, verify package exists
- `TestSaveImageDuringExec` — start a long-running exec, call save-as-image
  concurrently → exec blocks during save, resumes after, both complete
- `TestImageResizeDisk` — create sandbox with `disk_size_mb: 4096`,
  verify `df` shows ~4GB filesystem
- `TestImageNotFound` — create sandbox with nonexistent image → error
- `TestImageNameValidation` — image names with path traversal chars → rejected
- `TestUserImageShadowsAdmin` — admin image "base" exists, user saves
  their own "base", user's sandbox uses user's version
- `TestOCIImageExecAsUid1000` — pull node:22-slim, boot, exec `whoami`
  → returns `node` (the image's uid 1000 user, not `lohar`)
- `TestOCIImageSudoWorks` — boot from image with sudo, exec
  `sudo whoami` → `root`


### Phase 3: Named Snapshots + Templates

#### 3.1 Snapshot Store Schema

```sql
CREATE TABLE snapshots (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,
    name TEXT NOT NULL,
    source_sandbox TEXT NOT NULL,       -- sandbox ID it was created from
    mem_path TEXT NOT NULL,             -- /var/lib/bhatti/snapshots/{user_id}/{name}/mem.snap
    vm_path TEXT NOT NULL,
    rootfs_path TEXT NOT NULL,          -- copied rootfs at snapshot time
    config_path TEXT NOT NULL,          -- copied config drive
    manifest_json TEXT NOT NULL,        -- VM config + volume refs
    size_mb INTEGER NOT NULL DEFAULT 0, -- total snapshot size
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(user_id, name)
);
```

#### 3.2 Tests

**Store tests (pkg/store/):**
- `TestSnapshotCreateAndGet` — create, verify all fields including manifest_json
- `TestSnapshotUserScoped` — user A can't see/delete user B's snapshots
- `TestSnapshotDelete` — delete removes store record
- `TestSnapshotNameValidation` — names with path traversal chars → rejected
- `TestSnapshotQuotaEnforcement` — user at max_snapshots, checkpoint → rejected
- `TestSnapshotDuplicateName` — checkpoint with existing name → error
  (user must delete old snapshot first)

**Engine integration tests — happy path (agni-01):**
- `TestCheckpointAndResume` — create sandbox, run background process,
  checkpoint as "dev-ready", resume into new sandbox, verify process
  is still running, verify PID matches (exact memory restore)
- `TestCheckpointWithVolume` — sandbox has volume, write file to volume,
  checkpoint, resume, `cat` the file → data intact
- `TestCheckpointWithMultipleVolumes` — sandbox has 3 volumes, checkpoint,
  resume, verify all 3 mounted and data present
- `TestCheckpointVMConfig` — resume uses same vcpu/memory as checkpoint,
  exec `nproc` and `free -m` match original
- `TestCheckpointDifferentConfig` — try resume with different config →
  clear error: "snapshot requires 2 vCPUs and 1024 MiB"
- `TestCheckpointResumeTiming` — resume from checkpoint in <100ms
  (existing stop/start perf test, extended for named snapshots)
- `TestCheckpointResumeAgentReconnects` — after resume, exec works,
  file read works, shell works (agent token from manifest is correct)

**Engine integration tests — drive patching (agni-01):**

These test the most mechanically complex part of the plan: Firecracker
pre-load drive configuration.

- `TestResumeWithRelocatedRootfs` — checkpoint, delete original sandbox
  directory, resume from snapshot → VM works (drives were patched to
  new paths). This is THE critical test.
- `TestResumeWithRelocatedVolumes` — checkpoint sandbox with volume,
  resume → volume is at a new path (snapshot dir copy), VM can read
  data from it
- `TestResumeDriveOrder` — checkpoint sandbox with 2 volumes, resume,
  verify /dev/vdc and /dev/vdd map to the correct volumes (not swapped)
- `TestResumeManifestDriveIDs` — verify manifest records drive_id for
  every block device and they match what Firecracker expects

**Engine integration tests — IP reservation (agni-01):**
- `TestResumeIPAvailable` — checkpoint, destroy original sandbox (frees
  IP), resume → gets the same IP, VM networking works
- `TestResumeIPConflict` — checkpoint sandbox A (IP 10.0.1.2), keep A
  running, try resume → error "IP 10.0.1.2 is in use by another sandbox"
- `TestResumeIPConflictAfterNewSandbox` — checkpoint sandbox A (IP
  10.0.1.2), destroy A, create sandbox B (gets 10.0.1.2), try resume →
  error about IP conflict with B
- `TestResumeMACPreserved` — resume from checkpoint, verify guest MAC
  matches manifest (ARP cache in restored memory depends on this)

**Engine integration tests — failure cleanup (agni-01):**
- `TestCheckpointDiskFull` — fill disk to near capacity, attempt
  checkpoint → error, verify: VM is resumed (not left paused), partial
  snapshot temp dir is cleaned up, no `.tmp` directories left
- `TestCheckpointPartialCopyCleanup` — simulate I/O error mid-copy
  (e.g., remove source file during copy), verify temp dir cleaned up
- `TestResumeFailsCleanup` — corrupt vm.snap in snapshot dir, attempt
  resume → error, verify: TAP device cleaned up, IP released back to
  pool, sandbox dir removed, no VM in engine's map
- `TestResumeTAPFailureCleanup` — resume but TAP creation fails (e.g.,
  bridge doesn't exist) → error, verify IP released

**Concurrency tests — snapshots (agni-01):**
- `TestConcurrentCheckpointAndDestroy` — call checkpoint and destroy
  simultaneously on same sandbox → one succeeds, one fails, no crash,
  no leaked resources (TAP, IP, files)
- `TestConcurrentCheckpointAndExec` — checkpoint while exec is running →
  exec blocks until checkpoint completes (stateMu held), then returns
- `TestConcurrentCheckpointAndStop` — checkpoint and thermal-stop
  simultaneously → one succeeds, one fails, VM ends in consistent state
- `TestResumeWhileCheckpointing` — resume from snapshot A while
  checkpoint of a different sandbox is in progress → both succeed
  (independent VMs, independent locks)

**Additional tests from second review:**

- `TestSaveImageFlushesPageCache` — write a file in sandbox, immediately
  save-as-image without waiting, boot from saved image, verify file
  exists and is complete (catches missing `sync` before pause)
- `TestSaveImageDuringHeavyIO` — run `dd if=/dev/urandom of=/tmp/big bs=1M
  count=100` in sandbox, save-as-image while dd is running, verify saved
  image has consistent ext4 (e2fsck clean)
- `TestVolumeMaxPerSandbox` — try creating sandbox with 25 volumes → error
- `TestCheckpointOverwriteExisting` — checkpoint "dev-ready", checkpoint
  "dev-ready" again → must delete old one first or return clear error
- `TestResumeAfterBridgeDestroyed` — checkpoint, destroy all sandboxes
  (bridge gets cleaned up), resume → bridge recreated, VM works
- `TestConfigDriveBackwardCompat` — v0.1 config drive (no ReadOnly field)
  loaded by v0.3 lohar → mounts work (Go JSON defaults missing bool to false)
- `TestVolumeAttachedToCrashedSandbox` — FC process killed (not daemon),
  sandbox status still "running" in DB, volume still "attached" → verify
  startup recovery updates sandbox status AND detaches volumes
- `TestDuplicateImagePullAcrossUsers` — user A pulls python:3.12, user B
  pulls python:3.12 → both succeed, get independent copies (correct
  but documents the known cost)
- `TestSnapshotRootfsNotUsableAsImage` — copy rootfs.ext4 from snapshot
  dir, try to boot from it without mem.snap → either fails or produces
  inconsistent state (documents the constraint)

**Resumed sandbox volume behavior (agni-01):**
- `TestResumedSandboxVolumeIsEphemeral` — checkpoint sandbox A with
  volume "ws", resume as sandbox B, write file in B's /workspace,
  destroy B, attach "ws" to sandbox C → file from B is NOT in C
  (B had a snapshot copy, not the live volume)
- `TestResumedSandboxVolumeNoAttachmentRecord` — resume from snapshot,
  GET /volumes/ws → 0 attachments (resumed sandbox uses ephemeral copy)
- `TestResumedSandboxVolumeReadable` — resume, exec `cat /workspace/file`
  → returns data from snapshot time (copy is functional)

**Save-as-image page cache flush verification (agni-01):**
- `TestSaveImageLargeWriteFlush` — write 10MB file via `dd` (large
  enough to stay in page cache), immediately save-as-image, boot from
  saved image, verify full 10MB file is present and intact (catches
  missing `sync` — a 1-byte file would pass even without sync)

**Startup reconciliation (integration):**
- `TestStartupCleansStaleSnapshotTmpDirs` — create a
  `snapshots/usr_alice/.test.tmp/` dir with files, restart daemon,
  verify the .tmp dir was removed
- `TestStartupCreatesEntityDirectories` — delete images/ dir, restart
  daemon, verify images/ dir recreated

**Task table index (store):**
- `TestTaskCleanupPerformance` — insert 1000 tasks, run cleanup,
  verify it completes in <100ms (catches missing index on created_at)

**Crashed sandbox volume detachment (integration):**
- `TestCrashedFCProcessReleasesVolumes` — create sandbox with volume,
  kill Firecracker process (not daemon), restart daemon, verify startup
  recovery detects dead sandbox, updates status to 'destroyed', AND
  detaches the volume so it can be reused

---

## API Surface (v0.3 complete)

```
# Images
GET    /images                         list (admin + user's own)
POST   /images/pull                    pull from OCI registry (async — returns 202 + task ID)
GET    /images/pull/:task_id           poll pull status
POST   /sandboxes/:id/save-image       save sandbox rootfs as image
DELETE /images/:name                   delete image

# Volumes
POST   /volumes                        create volume (with size_mb)
GET    /volumes                        list (user-scoped)
GET    /volumes/:name                  get volume details + attachments
DELETE /volumes/:name                  delete (must be detached)
POST   /volumes/:name/resize           resize (must be detached, grow only)
POST   /volumes/:name/snapshot         copy volume

# Snapshots
POST   /sandboxes/:id/checkpoint       save full VM state (sync, blocks until complete)
GET    /snapshots                      list
POST   /snapshots/:name/resume         create sandbox from checkpoint
DELETE /snapshots/:name                delete

# Tasks (async operations)
GET    /tasks/:id                      poll task status (running/completed/failed)
DELETE /tasks/:id                      cancel running task

# Templates
POST   /templates                      create
GET    /templates                      list (user + admin)
GET    /templates/:name                get
DELETE /templates/:name                delete

# Sandboxes (extended)
POST   /sandboxes
  {
    "name": "dev",
    "image": "python-3.12",           ← new
    "template": "ml-env",             ← new (alternative to specifying fields)
    "cpus": 2,
    "memory_mb": 4096,
    "disk_size_mb": 8192,             ← new (resize rootfs)
    "volumes": [                      ← new (persistent volumes)
      {"name": "workspace", "mount": "/workspace"},
      {"name": "datasets", "mount": "/data", "read_only": true}
    ],
    "env": {"KEY": "value"},
    "init": "npm install"
  }
```

Everything in v0.1 continues to work unchanged. The new fields are
additive — omitting them gives v0.1 behavior (base image, no persistent
volumes, no template).
