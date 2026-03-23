# Bhatti v0.2 — Images, Volumes, and Snapshots

v0.1 shipped multi-tenant security: per-user auth, network isolation,
guest hardening, encrypted secrets, rate limiting, and observability.

v0.2 adds the storage and state primitives that make bhatti useful for
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

**The v0.2 model:** block devices have their own lifecycle, independent
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

  For v0.2: option (a) — reuse the existing uid 1000 user. If no uid 1000
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
lohar and limits layers per image. Not worth it for v0.2.

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

For v0.2: support `--auth user:token` flag and `~/.docker/config.json`.
Don't implement the full Docker credential helper ecosystem — it's a
rabbit hole. Users who need ECR/GCR auth can `docker pull` + `docker save`
+ `bhatti image import` as a workaround.

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
    {name}.meta.json        metadata (size, created_at, attached_to)
```

The `.meta.json` tracks attachment state:

```json
{
    "name": "workspace",
    "size_mb": 5120,
    "created_at": "2026-03-22T...",
    "attached_to": "sandbox-abc123",
    "attached_at": "2026-03-22T...",
    "mount": "/workspace",
    "read_only": false
}
```

When `attached_to` is empty, the volume is detached and available.

### Attachment lifecycle

**Create:**
```
POST /volumes {"name": "workspace", "size_mb": 5120}
→ mkfs.ext4 on new file
→ chown to user's uid inside ext4
→ store metadata
```

**Attach (during sandbox creation):**
```
POST /sandboxes {"name": "dev", "volumes": [{"name": "workspace", "mount": "/workspace"}]}
→ check volume exists and belongs to user
→ check volume not attached to another sandbox (or read_only)
→ update metadata: attached_to = sandbox_id
→ pass volume file path to Firecracker as additional drive
→ config drive tells lohar to mount /dev/vdc at /workspace
```

**Detach (during sandbox destroy):**
```
DELETE /sandboxes/abc123
→ destroy VM, remove sandbox directory
→ update volume metadata: attached_to = ""
→ volume file untouched
```

**The volume file is never inside the sandbox directory.** This is the
key difference from v0.1. The sandbox directory contains only the rootfs
(ephemeral copy), config drive, and snapshot files. Volumes live in
their own directory hierarchy.

### Concurrent access protection

The store tracks `attached_to` for each volume. The attachment check
happens inside a transaction:

```go
func (s *Store) AttachVolume(userID, volumeName, sandboxID, mount string, readOnly bool) error {
    tx, _ := s.db.Begin()
    defer tx.Rollback()

    var currentAttach string
    tx.QueryRow("SELECT attached_to FROM volumes WHERE user_id=? AND name=?",
        userID, volumeName).Scan(&currentAttach)

    if currentAttach != "" && !readOnly {
        return fmt.Errorf("volume %q already attached to %s", volumeName, currentAttach)
    }

    tx.Exec("UPDATE volumes SET attached_to=?, mount=? WHERE user_id=? AND name=?",
        sandboxID, mount, userID, volumeName)
    tx.Commit()
    return nil
}
```

Read-only volumes skip the `currentAttach` check — multiple sandboxes
can mount the same volume read-only simultaneously. Firecracker attaches
the same file as a read-only block device. ext4 supports this.

### Volume expansion

```
POST /volumes/workspace/resize {"size_mb": 10240}
→ volume must be detached (no running sandbox using it)
→ truncate -s 10240M workspace.ext4
→ resize2fs workspace.ext4
→ update metadata
```

Cannot resize while attached — the guest kernel has the filesystem
mounted and would be confused by the underlying block device changing
size. (Live resize IS possible with virtio-blk resize events, but adds
significant complexity. Defer to v0.3.)

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

**Creating a named snapshot:**
```
POST /sandboxes/abc123/checkpoint {"name": "dev-ready"}
→ pause VM
→ create Firecracker snapshot (mem.snap + vm.snap)
→ copy rootfs.ext4 to snapshots/usr_alice/dev-ready/rootfs.ext4
→ copy config.ext4 to snapshots/usr_alice/dev-ready/config.ext4
→ (volumes are NOT copied — they're referenced by path)
→ write manifest.json
→ resume VM (or leave paused, user's choice)
```

Wait — should volumes be copied or referenced?

**If referenced**: resuming the snapshot after the volume has been modified
by another sandbox would be inconsistent (memory state expects old
filesystem state). This is safe ONLY if the volume hasn't been attached
to anything else since the snapshot was taken.

**If copied**: the snapshot is fully self-contained. Resuming always
produces the exact state that was checkpointed. But copying a 20GB
volume on every checkpoint is expensive.

**Resolution**: copy the rootfs (it's ephemeral and specific to this
snapshot). Reference volumes by name but record a "generation" counter.
The volume tracks how many times it's been attached/detached. On resume,
if the generation has changed (volume was used by another sandbox since
checkpoint), warn or fail. If the generation is the same, the volume
content is unchanged and safe to resume with.

For v0.2: just copy everything. Snapshots are complete, self-contained,
and always safe. Optimize with generation tracking in v0.3.

### Resuming from a named snapshot

```
POST /snapshots/dev-ready/resume {"name": "new-sandbox"}
→ read manifest
→ create new sandbox directory
→ copy rootfs and config from snapshot dir
→ attach volume files (from volumes dir, not snapshot dir)
→ start Firecracker with snapshot load (mem.snap + vm.snap)
→ VM resumes exactly where it was checkpointed
```

**Firecracker constraint**: the resume must use the exact same VM
configuration (vCPUs, memory, drive count and order). The manifest
records this. If the user requests different resources, we reject
with a clear error explaining why.

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
    attached_to TEXT NOT NULL DEFAULT '', -- sandbox ID or empty
    attached_mount TEXT NOT NULL DEFAULT '', -- mount point when attached
    read_only INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(user_id, name)
);
-- migrate old volumes table data if any, then drop + rename
```

New store methods:

```go
// Volume entity
type Volume struct {
    ID           string    `json:"id"`
    UserID       string    `json:"user_id"`
    Name         string    `json:"name"`
    SizeMB       int       `json:"size_mb"`
    FilePath     string    `json:"-"`             // not exposed via API
    AttachedTo   string    `json:"attached_to"`   // sandbox ID or ""
    AttachedMount string   `json:"attached_mount"`
    ReadOnly     bool      `json:"read_only"`
    CreatedAt    time.Time `json:"created_at"`
}

func (s *Store) CreateVolume(v Volume) error
func (s *Store) GetVolume(userID, name string) (*Volume, error)
func (s *Store) ListUserVolumes(userID string) ([]Volume, error)
func (s *Store) DeleteVolume(userID, name string) error           // fails if attached
func (s *Store) AttachVolume(userID, name, sandboxID, mount string, readOnly bool) error  // tx: check not attached
func (s *Store) DetachVolume(userID, name string) error
func (s *Store) DetachAllVolumes(sandboxID string) error          // called on sandbox destroy
func (s *Store) UpdateVolumeSize(userID, name string, sizeMB int) error
```

`AttachVolume` runs inside a transaction:
```go
func (s *Store) AttachVolume(userID, name, sandboxID, mount string, readOnly bool) error {
    tx, _ := s.db.Begin()
    defer tx.Rollback()

    var currentAttach string
    var currentRO int
    tx.QueryRow("SELECT attached_to, read_only FROM volumes_v2 WHERE user_id=? AND name=?",
        userID, name).Scan(&currentAttach, &currentRO)

    if currentAttach != "" && !readOnly {
        return fmt.Errorf("volume %q already attached to sandbox %s", name, currentAttach)
    }
    // read-only attachment to already-attached volume is OK only if both are read-only
    if currentAttach != "" && readOnly && currentRO == 0 {
        return fmt.Errorf("volume %q attached read-write to %s, cannot attach read-only simultaneously", name, currentAttach)
    }

    tx.Exec("UPDATE volumes_v2 SET attached_to=?, attached_mount=?, read_only=? WHERE user_id=? AND name=?",
        sandboxID, mount, boolToInt(readOnly), userID, name)
    return tx.Commit()
}
```

#### 1.2 Engine Changes

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
}
```

**File:** `pkg/engine/firecracker/engine.go`

In `Create()`, after rootfs copy and before Firecracker configuration:

```go
// 4d. Resolve and attach persistent volumes
var volumeMounts []VolumeMountConfig
driveIndex := byte('c') // vdb=config, vdc=first vol, ...

for _, vol := range spec.Volumes {
    volDir := filepath.Join(e.cfg.DataDir, "volumes", spec.UserID)
    volPath := filepath.Join(volDir, vol.Name+".ext4")

    if _, statErr := os.Stat(volPath); os.IsNotExist(statErr) {
        if !vol.AutoCreate || vol.SizeMB <= 0 {
            return info, fmt.Errorf("volume %q not found", vol.Name)
        }
        os.MkdirAll(volDir, 0700)
        if err = createVolume(volPath, vol.SizeMB); err != nil {
            return info, fmt.Errorf("create volume %q: %w", vol.Name, err)
        }
    }

    device := fmt.Sprintf("/dev/vd%c", driveIndex)
    volumeMounts = append(volumeMounts, VolumeMountConfig{
        Device: device, Mount: vol.Mount, FS: "ext4",
    })
    driveIndex++
}

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

#### 1.3 Server / Routes Changes

**File:** `pkg/server/routes.go`

Update `handleSandboxes POST` to handle persistent volumes:
- Before `engine.Create()`: for each volume in request, call
  `store.AttachVolume()` to check/reserve the attachment
- If any attachment fails: return 409 ("volume already attached")
- After sandbox destroy: `store.DetachAllVolumes(sandboxID)` to release
  all volume attachments

New volume endpoints:
```go
POST   /volumes          → handleVolumeCreate (user_id, name, size_mb)
GET    /volumes          → handleVolumeList (user-scoped)
GET    /volumes/:name    → handleVolumeGet
DELETE /volumes/:name    → handleVolumeDelete (must be detached)
POST   /volumes/:name/resize → handleVolumeResize (must be detached)
```

**Important:** `handleVolumeDelete` must check `attached_to == ""` before
deleting both the store record and the ext4 file on disk. If the file
is deleted while attached to a running VM, the VM's block device becomes
invalid and the VM crashes.

#### 1.4 CLI Changes

```
bhatti volume create --name workspace --size 5120
bhatti volume list
bhatti volume delete workspace
bhatti volume resize workspace --size 10240

bhatti create --name dev --volume workspace:/workspace
bhatti create --name dev --volume datasets:/data:ro   # read-only
```

The `--volume` flag format: `name:mount[:ro]`

#### 1.5 Tests

**Store tests (pkg/store/):**
- `TestVolumeCreateAndGet` — create volume, verify fields
- `TestVolumeUserScoped` — user A can't see/delete user B's volumes
- `TestVolumeAttachDetach` — attach to sandbox, verify attached_to, detach
- `TestVolumeDoubleAttachRejected` — attach to sb1, try attach to sb2 → error
- `TestVolumeReadOnlyMultiAttach` — attach RO to sb1 and sb2 → both succeed
- `TestVolumeDeleteWhileAttached` — fails with error
- `TestVolumeDeleteAfterDetach` — succeeds
- `TestDetachAllVolumes` — multiple volumes detached on sandbox destroy
- `TestVolumeResize` — update size_mb, verify

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

**Integration tests (pkg/engine/firecracker/, agni-01):**
- `TestPersistentVolumeData` — create volume, write data in sb1, destroy sb1,
  create sb2 with same volume, read data → data persists
- `TestVolumeOwnership` — files in volume owned by uid 1000
- `TestVolumeReadOnlyMount` — write to RO volume → fails inside VM
- `TestVolumeMultiplePerSandbox` — sandbox with 2 volumes, verify both mounted
- `TestVolumeAutoCreate` — sandbox with auto_create volume, volume created on first use
- `TestVolumeSurvivesSnapshot` — stop + start sandbox with volume, data intact
- `TestEphemeralVolumesStillWork` — legacy NewVolumes path still functions


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
func WithPlatform(os, arch string) Option
```

**File:** `pkg/oci/pull.go`

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
// Handles whiteouts (per-file and opaque), permissions, symlinks, hard links.
func extractLayer(layer v1.Layer, targetDir string) error {
    reader, _ := layer.Uncompressed()
    tr := tar.NewReader(reader)

    for {
        header, err := tr.Next()
        if err == io.EOF { break }

        path := filepath.Join(targetDir, header.Name)

        // Handle whiteouts
        base := filepath.Base(header.Name)
        dir := filepath.Dir(path)
        if base == ".wh..wh..opq" {
            // Opaque whiteout: remove all existing entries in this directory
            removeDirectoryContents(dir)
            continue
        }
        if strings.HasPrefix(base, ".wh.") {
            // Per-file whiteout: delete the named file
            target := filepath.Join(dir, strings.TrimPrefix(base, ".wh."))
            os.RemoveAll(target)
            continue
        }

        // Extract based on type
        switch header.Typeflag {
        case tar.TypeDir:
            os.MkdirAll(path, os.FileMode(header.Mode))
        case tar.TypeReg:
            writeFile(path, tr, os.FileMode(header.Mode))
        case tar.TypeSymlink:
            os.Symlink(header.Linkname, path)
        case tar.TypeLink:
            os.Link(filepath.Join(targetDir, header.Linkname), path)
        case tar.TypeBlock, tar.TypeChar:
            // Skip device nodes — lohar creates /dev at boot
            continue
        }

        // Preserve ownership
        os.Lchown(path, header.Uid, header.Gid)
    }
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
        return nil // no passwd file, skip
    }
    for _, line := range strings.Split(string(data), "\n") {
        if strings.Contains(line, ":1000:") {
            return nil // uid 1000 exists
        }
    }
    // Append lohar user
    f, _ := os.OpenFile(passwdPath, os.O_APPEND|os.O_WRONLY, 0644)
    defer f.Close()
    f.WriteString("lohar:x:1000:1000::/home/lohar:/bin/sh\n")

    groupPath := filepath.Join(rootDir, "etc/group")
    g, _ := os.OpenFile(groupPath, os.O_APPEND|os.O_WRONLY, 0644)
    defer g.Close()
    g.WriteString("lohar:x:1000:\n")

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
func createExt4FromDir(srcDir, outputPath string) error {
    // 1. Calculate required size
    totalSize, fileCount, err := dirStats(srcDir)
    if err != nil {
        return err
    }

    // Add 20% headroom + 256MB minimum free
    sizeMB := int(totalSize/1024/1024) * 120 / 100
    if sizeMB < 512 {
        sizeMB = 512
    }

    // 2. Create sparse file
    f, _ := os.Create(outputPath)
    f.Truncate(int64(sizeMB) << 20)
    f.Close()

    // 3. Format with enough inodes
    //    Default ext4 creates 1 inode per 16KB. Images with many small
    //    files (node_modules) can exhaust inodes. Allocate 1 inode per
    //    4KB or at least fileCount * 1.5.
    inodes := max(fileCount * 3 / 2, totalSize / 4096)
    exec.Command("mkfs.ext4", "-F", "-q", "-N", fmt.Sprint(inodes), outputPath).Run()

    // 4. Mount and copy
    mountDir, _ := os.MkdirTemp("", "bhatti-ext4-*")
    defer os.RemoveAll(mountDir)

    exec.Command("mount", "-o", "loop", outputPath, mountDir).Run()
    defer exec.Command("umount", mountDir).Run()

    // cp -a preserves permissions, symlinks, hard links, timestamps
    exec.Command("cp", "-a", srcDir+"/.", mountDir+"/").Run()

    return nil
}
```

#### 2.3 Engine Changes

In `Create()`, resolve image name to file path:

```go
// 1. Resolve image
baseImage := e.cfg.BaseRootfs // default
if spec.Image != "" {
    // Check user images first, then admin images
    imgPath := filepath.Join(e.cfg.DataDir, "images", spec.UserID, spec.Image+".ext4")
    if _, err := os.Stat(imgPath); os.IsNotExist(err) {
        // Try admin image
        imgPath = filepath.Join(e.cfg.DataDir, "images", spec.Image+".ext4")
        if _, err := os.Stat(imgPath); os.IsNotExist(err) {
            return info, fmt.Errorf("image %q not found", spec.Image)
        }
    }
    baseImage = imgPath
}

rootfsPath := filepath.Join(sandboxDir, "rootfs.ext4")
if err = copyRootfs(baseImage, rootfsPath); err != nil {
    return info, fmt.Errorf("copy rootfs: %w", err)
}

// Resize if requested
if spec.DiskSizeMB > 0 {
    if err = exec.Command("truncate", "-s", fmt.Sprintf("%dM", spec.DiskSizeMB), rootfsPath).Run(); err != nil {
        return info, fmt.Errorf("resize rootfs: %w", err)
    }
    if err = exec.Command("resize2fs", rootfsPath).Run(); err != nil {
        return info, fmt.Errorf("resize2fs: %w", err)
    }
}
```

Save-as-image in the engine:

```go
func (e *Engine) SaveImage(ctx context.Context, sandboxID, destPath string) error {
    vm, err := e.getVM(sandboxID)
    if err != nil {
        return err
    }
    vm.stateMu.Lock()
    defer vm.stateMu.Unlock()

    // Pause VM for filesystem consistency
    wasPaused := vm.Thermal == "warm"
    if vm.Thermal == "hot" {
        client := fcAPIClient(vm.SocketPath)
        fcPatch(client, "/vm", `{"state":"Paused"}`)
    }

    // Copy rootfs
    if err := copyRootfs(vm.RootfsPath, destPath); err != nil {
        return fmt.Errorf("copy rootfs: %w", err)
    }

    // Resume if it was hot
    if !wasPaused {
        client := fcAPIClient(vm.SocketPath)
        fcPatch(client, "/vm", `{"state":"Resumed"}`)
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
- `TestImageResizeDisk` — create sandbox with `disk_size_mb: 4096`,
  verify `df` shows ~4GB filesystem
- `TestImageNotFound` — create sandbox with nonexistent image → error
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

**Store tests:**
- `TestSnapshotCreateAndGet`
- `TestSnapshotUserScoped`
- `TestSnapshotDelete`

**Engine integration tests (agni-01):**
- `TestCheckpointAndResume` — create sandbox, run background process,
  checkpoint as "dev-ready", resume into new sandbox, verify process
  is running
- `TestCheckpointWithVolume` — sandbox has volume, checkpoint, resume,
  verify volume data accessible
- `TestCheckpointVMConfig` — resume must use same vcpu/memory as
  checkpoint
- `TestCheckpointDifferentConfig` — try resume with different config →
  clear error explaining constraint
- `TestCheckpointResumeTiming` — resume from checkpoint in <100ms
  (existing stop/start perf test, extended for named snapshots)

---

## API Surface (v0.2 complete)

```
# Images
GET    /images                         list (admin + user's own)
POST   /images/pull                    pull from OCI registry
POST   /sandboxes/:id/save-image       save sandbox rootfs as image
DELETE /images/:name                   delete image

# Volumes
POST   /volumes                        create volume (with size_mb)
GET    /volumes                        list (user-scoped)
GET    /volumes/:name                  get volume details
DELETE /volumes/:name                  delete (must be detached)
POST   /volumes/:name/resize           resize (must be detached)
POST   /volumes/:name/snapshot         copy volume

# Snapshots
POST   /sandboxes/:id/checkpoint       save full VM state
GET    /snapshots                      list
POST   /snapshots/:name/resume         create sandbox from checkpoint
DELETE /snapshots/:name                delete

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
      {"name": "workspace", "mount": "/workspace"}
    ],
    "env": {"KEY": "value"},
    "init": "npm install"
  }
```

Everything in v0.1 continues to work unchanged. The new fields are
additive — omitting them gives v0.1 behavior (base image, no persistent
volumes, no template).
