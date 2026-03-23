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

**Resolution**: copy everything. Rootfs, config drive, and all attached
volumes are copied into the snapshot directory. Snapshots are fully
self-contained and always safe to resume regardless of what happened
to the original volumes.

The cost: for a sandbox with 2GB rootfs, 1GB mem, and 5GB volume, a
checkpoint writes 8GB. At NVMe speeds (2GB/s), that's 4 seconds of
VM pause. This is acceptable for explicit user-initiated checkpoints.
It is NOT acceptable for automatic thermal snapshots — those continue
to use the existing ephemeral snapshot path which doesn't copy volumes.

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
→ start new Firecracker process
→ PUT /drives/rootfs   {"path_on_host": NEW rootfs path}     ← REQUIRED
→ PUT /drives/config   {"path_on_host": NEW config path}     ← REQUIRED
→ for each volume:
    PUT /drives/volN    {"path_on_host": volume file path}   ← REQUIRED
→ PUT /machine-config  {same vcpu/memory as manifest}
→ PUT /network-interfaces/eth0 {same MAC, new TAP}
→ PUT /vsock           {same CID, new UDS path}
→ PUT /snapshot/load   {snapshot_path, mem_backend, resume_vm: true}
→ reconnect agent with token from manifest
```

All drive PUTs must happen BEFORE `/snapshot/load`. Firecracker
supports this pre-load drive patching — it's the official way to
relocate snapshot files. Without it, Firecracker opens the old paths
and the resume fails or reads stale/deleted data.

The manifest must record:
```json
{
    "vm_config": {"vcpu_count": 2, "mem_size_mib": 1024},
    "drives": [
        {"drive_id": "rootfs", "role": "rootfs"},
        {"drive_id": "config", "role": "config"},
        {"drive_id": "vol0", "role": "volume", "name": "workspace", "user_id": "usr_alice"}
    ],
    "network": {"guest_mac": "02:ab:cd:...", "guest_ip": "10.0.1.2"},
    "agent_token": "abc123...",
    "vsock_cid": 42
}
```

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

func (s *Store) CreateVolume(v Volume) error
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
func (s *Store) DetachOrphanedVolumes() error {
    _, err := s.db.Exec(`DELETE FROM volume_attachments
        WHERE sandbox_id NOT IN (SELECT id FROM sandboxes WHERE status != 'destroyed')`)
    return err
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

**Crash recovery tests (integration, agni-01):**
- `TestCrashBetweenDestroyAndDetach` — create sandbox with volume, kill
  daemon between engine.Destroy and store.DetachAllForSandbox, restart,
  verify volume is auto-detached by DetachOrphanedVolumes on startup
- `TestCrashDuringVolumeCreate` — kill daemon during mkfs.ext4, restart,
  verify partial ext4 file is cleaned up or ignored
- `TestOrphanedVolumeFile` — delete store record but leave ext4 file,
  verify no crash and file is reportable via admin tooling

**Concurrency tests:**
- `TestConcurrentAttachSameVolume` — two goroutines attach same volume
  read-write simultaneously → exactly one succeeds, one gets error
- `TestAutoCreateRace` — two sandbox creates with same auto_create volume
  → only one volume file created, no data loss from mkfs overwrite
- `TestConcurrentAttachReadOnly` — three goroutines attach same volume
  read-only simultaneously → all three succeed

**Schema migration test:**
- `TestMigrateV02ToV03` — create a v0.2 database (old volumes table),
  run New() which applies migrations, verify data preserved in volumes_v2
  and volume_attachments tables

**Guest-side edge cases (integration, agni-01):**
- `TestMountCorruptVolume` — attach a volume with invalid ext4 (zeroed
  file), verify lohar logs error and continues (doesn't crash)
- `TestMountMissingDevice` — config drive references /dev/vde but only
  3 drives attached, verify lohar logs error and continues
- `TestMountPointConflict` — volume mount at /workspace which already
  has files in rootfs, verify mount overlays correctly (existing files
  hidden, volume content visible)


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
// Uses a two-pass approach because the OCI spec does NOT guarantee ordering
// within a layer tar. An opaque whiteout (.wh..wh..opq) can appear AFTER
// regular files in the same directory within the same tar. A single-pass
// approach would extract files then delete them when the whiteout is hit.
//
// Pass 1: scan the tar for all whiteout entries, record them.
// Pass 2: apply whiteouts (delete from previous layers), then extract files.
func extractLayer(layer v1.Layer, targetDir string) error {
    // Pass 1: collect whiteouts
    reader1, _ := layer.Uncompressed()
    tr1 := tar.NewReader(reader1)

    type whiteout struct {
        path   string
        opaque bool // true = .wh..wh..opq (clear entire directory)
    }
    var whiteouts []whiteout

    for {
        header, err := tr1.Next()
        if err == io.EOF { break }
        if err != nil { return err }

        base := filepath.Base(header.Name)
        if base == ".wh..wh..opq" {
            dir := filepath.Dir(filepath.Join(targetDir, header.Name))
            whiteouts = append(whiteouts, whiteout{path: dir, opaque: true})
        } else if strings.HasPrefix(base, ".wh.") {
            target := filepath.Join(targetDir, filepath.Dir(header.Name),
                strings.TrimPrefix(base, ".wh."))
            whiteouts = append(whiteouts, whiteout{path: target, opaque: false})
        }
    }

    // Apply whiteouts (delete from previous layers)
    for _, wo := range whiteouts {
        if wo.opaque {
            removeDirectoryContents(wo.path)
        } else {
            os.RemoveAll(wo.path)
        }
    }

    // Pass 2: extract files (re-read layer since tar is streaming)
    reader2, _ := layer.Uncompressed()
    tr2 := tar.NewReader(reader2)

    for {
        header, err := tr2.Next()
        if err == io.EOF { break }
        if err != nil { return err }

        // Skip whiteout entries (already processed)
        base := filepath.Base(header.Name)
        if base == ".wh..wh..opq" || strings.HasPrefix(base, ".wh.") {
            continue
        }

        path := filepath.Join(targetDir, header.Name)

        // Path traversal protection
        if !strings.HasPrefix(filepath.Clean(path), filepath.Clean(targetDir)) {
            continue // skip entries that escape the target dir
        }

        switch header.Typeflag {
        case tar.TypeDir:
            os.MkdirAll(path, os.FileMode(header.Mode))
        case tar.TypeReg:
            writeFile(path, tr2, os.FileMode(header.Mode))
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
        fields := strings.Split(line, ":")
        if len(fields) >= 3 && fields[2] == "1000" {
            return nil // uid 1000 exists (field index 2 is uid)
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
// Uses mke2fs -d to populate the filesystem without mounting, avoiding
// leaked loop devices on crash and reducing syscall overhead for
// directories with many small files (node_modules).
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
        return fmt.Errorf("mke2fs: %s: %w", out, err)
    }

    return nil
}
```

Note: `mke2fs -d` requires e2fsprogs >= 1.43 (Ubuntu 18.04+). It
handles permissions, symlinks, hard links, and timestamps. It does NOT
require root — no mount/umount. This also fixes the existing bug in
`createConfigDrive` which uses mount/umount and can leak loop devices
on crash. Migrate `createConfigDrive` to use `mke2fs -d` as well.

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

// Note: copyRootfs should use `cp --reflink=auto --sparse=always` on the
// fallback path (when reflink fails). Without --sparse=always, a 10GB
// rootfs that's 90% empty materializes as 10GB on disk instead of ~1GB.
// This matters especially for named snapshots that copy rootfs + mem.snap.

// Re-inject lohar into the rootfs to ensure the current version is used.
// Saved images and OCI images may contain an older lohar binary. Without
// this, a bhatti upgrade would leave VMs running old agent code, causing
// protocol mismatches between host and guest.
if err = injectLoharIntoRootfs(rootfsPath, e.cfg.DataDir); err != nil {
    slog.Warn("lohar injection failed, using image's lohar", "error", err)
}

// Resize if requested
if spec.DiskSizeMB > 0 {
    // e2fsck before resize2fs — resize on a dirty filesystem amplifies corruption
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

```go
func (e *Engine) SaveImage(ctx context.Context, sandboxID, destPath string) error {
    vm, err := e.getVM(sandboxID)
    if err != nil {
        return err
    }

    // Hold stateMu only for the pause — not for the entire copy.
    // The copy can take 1-10 seconds for a 1-4GB rootfs. Holding
    // stateMu for that duration blocks ALL operations on this sandbox
    // (exec, file read, etc.) and makes it unresponsive to API calls.
    //
    // Instead: pause under lock, release lock, copy (VM is paused so
    // rootfs is consistent), re-acquire lock, resume.
    vm.stateMu.Lock()
    wasPaused := vm.Thermal == "warm"
    if vm.Thermal == "hot" {
        client := fcAPIClient(vm.SocketPath)
        fcPatch(client, "/vm", `{"state":"Paused"}`)
        vm.Thermal = "warm"
    }
    rootfsPath := vm.RootfsPath
    socketPath := vm.SocketPath
    vm.stateMu.Unlock()

    // Copy rootfs (VM is paused, filesystem is consistent, lock is released)
    if err := copyRootfs(rootfsPath, destPath); err != nil {
        return fmt.Errorf("copy rootfs: %w", err)
    }

    // Resume
    if !wasPaused {
        vm.stateMu.Lock()
        client := fcAPIClient(socketPath)
        fcPatch(client, "/vm", `{"state":"Resumed"}`)
        vm.Thermal = "hot"
        vm.stateMu.Unlock()
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
POST   /images/pull                    pull from OCI registry (async — returns task ID)
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
