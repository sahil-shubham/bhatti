> [!WARNING]
> **DEPRECATED — do not edit.**
> The canonical, maintained version of this page is at
> <https://bhatti.sh/docs/contributing/kernel/>.
> This file is kept only for git history and may be removed in a future
> cleanup. See [`docs/README.md`](./README.md) for the redirect index.

---

# Bhatti Kernel Configuration

Bhatti ships a custom Linux kernel built from the Firecracker CI configuration
with additional flags enabled for Docker, VPN, FUSE, and security features.
One kernel is used for all rootfs tiers (minimal, browser, docker).

## Base

The starting point is the Firecracker CI kernel config from their v1.15 test
artifacts. This config is maintained by the Firecracker team and tested against
every Firecracker commit. It includes everything needed to boot a microVM:

- virtio-blk, virtio-net, virtio-mmio, virtio-vsock (Firecracker I/O)
- ext4, overlayfs, tmpfs, devtmpfs (filesystems)
- All Linux namespaces (pid, net, user, mount, uts, ipc, time)
- cgroups v2 (full: memory, cpu, pids, freezer, devices, perf, BPF)
- seccomp, SELinux, audit
- iptables (filter, nat, mangle, conntrack)
- io_uring, BPF, inotify, fanotify
- Bridge, veth

Source: `s3://spec.ccfc.min/firecracker-ci/v1.15/{arch}/vmlinux-6.1.155.config`

## What we add

16 config flags, all set to `=y` (compiled into the kernel, not as loadable
modules — Firecracker doesn't support module loading).

### Docker networking (4 flags)

```
CONFIG_IP_NF_RAW=y
CONFIG_IP6_NF_RAW=y
CONFIG_IP_NF_SECURITY=y
CONFIG_IP6_NF_SECURITY=y
```

**What they do:** Enable the iptables `raw` and `security` tables.

**Why:** Docker 28+ uses the `raw` table for "direct routing on bridge
networks" — a security feature that drops packets sent directly to container
IPs without going through the Docker bridge. Without `raw`, Docker refuses to
create containers with bridge networking (the default). The `security` table
is used by AppArmor/SELinux to label network traffic per container.

**The `raw` table** is the earliest hook in the netfilter packet processing
pipeline. Packets hit `raw` → `conntrack` → `mangle` → `nat` → `filter`.
The raw table is typically used to exempt certain traffic from connection
tracking (with `-j NOTRACK`) or to drop traffic before any stateful processing.
Docker adds rules like:

```
iptables -t raw -A PREROUTING -d 172.17.0.2 ! -i docker0 -j DROP
```

This ensures container traffic always enters through the bridge interface,
preventing a class of attacks where an attacker on the host network sends
packets directly to a container's IP address.

**When not using Docker:** These tables exist but are empty. No rules are
evaluated, no performance impact. The kernel allocates a small amount of
memory for the empty table structures (~few hundred bytes each).

### Docker advanced networking (4 flags)

```
CONFIG_DUMMY=y
CONFIG_MACVLAN=y
CONFIG_IPVLAN=y
CONFIG_VXLAN=y
```

**DUMMY** creates virtual network interfaces that discard all traffic. Docker
uses dummy interfaces for internal service discovery (the `docker_gwbridge`
network). Also useful for testing network configurations.

**MACVLAN** creates virtual interfaces that each get their own MAC address on
a parent interface. Docker's `--network macvlan` mode gives each container a
unique MAC address, making them appear as separate physical devices on the
LAN. Useful when containers need to be directly reachable from the host
network without NAT.

**IPVLAN** is similar to MACVLAN but all virtual interfaces share the parent's
MAC address. Each gets its own IP. Docker's `--network ipvlan` mode is useful
in environments where MAC address limits exist (some cloud providers,
enterprise switches). Lower overhead than MACVLAN since the switch doesn't
need to learn multiple MACs.

**VXLAN** (Virtual eXtensible LAN) encapsulates Layer 2 frames in UDP packets,
creating virtual overlay networks. Docker uses VXLAN for multi-host overlay
networks (`docker network create --driver overlay`). Also used by Kubernetes
(flannel, calico in VXLAN mode) and Podman. Inside a bhatti VM, VXLAN enables
Docker Compose services that span multiple bridge networks.

**When not using Docker:** These are just kernel drivers that register virtual
interface types. If no one creates a MACVLAN/IPVLAN/VXLAN/dummy interface,
the code is never invoked. Memory overhead: a few KB of registered `net_device`
operations.

### Docker traffic accounting (2 flags)

```
CONFIG_NET_CLS_CGROUP=y
CONFIG_NETFILTER_XT_MARK=y
```

**NET_CLS_CGROUP** assigns a classid to network packets based on the sending
process's cgroup. Docker uses this to identify which container generated
network traffic, enabling per-container bandwidth accounting and traffic
shaping.

**NETFILTER_XT_MARK** enables the `MARK` target and match in iptables, which
sets/reads a per-packet mark (an integer stored in the kernel's `sk_buff`).
Docker uses packet marks to implement routing decisions — for example, marking
packets from a specific container so they get routed through a particular
gateway.

**When not using Docker:** The cgroup classid field exists in the packet
metadata regardless but is always 0. The MARK iptables module is loaded but
no rules reference it. Negligible overhead.

### TUN/TAP (1 flag)

```
CONFIG_TUN=y
```

**What it does:** Enables the `/dev/net/tun` device, which allows userspace
programs to create virtual network interfaces. When a program opens
`/dev/net/tun`, it gets a file descriptor. Bytes written to the fd appear
as network packets on the virtual interface, and packets sent to the virtual
interface can be read from the fd.

**Why:** This is how every VPN client works:

- **Tailscale** creates a `tailscale0` TUN interface for its WireGuard tunnel
- **cloudflared** (Cloudflare Tunnel) uses TUN for WARP routing
- **OpenVPN** creates a `tun0` interface for its SSL/TLS tunnel
- **WireGuard-go** (userspace WireGuard) uses TUN when the kernel WireGuard
  module isn't available

Also used by:
- Docker's `--network=host` with custom routing in some configurations
- `slirp4netns` (rootless container networking)
- Network testing tools (`socat tun`, `ip tuntap`)

**When not using VPNs:** The `/dev/net/tun` device node exists but
opening it without `CAP_NET_ADMIN` (or `sudo`) fails. No interfaces are
created, no overhead.

### FUSE (1 flag)

```
CONFIG_FUSE_FS=y
```

**What it does:** FUSE (Filesystem in USErspace) lets userspace programs
implement filesystem interfaces. The kernel handles VFS routing and forwards
read/write/readdir/etc calls to a userspace process via `/dev/fuse`.

**Why:**

- **sshfs** — mount a remote directory over SSH. Common in dev workflows:
  `sshfs user@server:/code /mnt/remote`
- **rclone mount** — mount S3, GCS, Azure Blob, Google Drive, Dropbox as a
  local filesystem. Used for accessing cloud storage from inside a sandbox
- **s3fs-fuse** — mount an S3 bucket as a POSIX filesystem
- **AppImage** — Linux portable applications use FUSE to mount their internal
  filesystem
- **bindfs** — remount a directory with different ownership/permissions
- **Docker/Podman** — some rootless container runtimes use `fuse-overlayfs`
  when the kernel's overlayfs doesn't support unprivileged mounts
- **NTFS-3G** — read/write NTFS filesystems (unlikely in a VM, but costs
  nothing to have)

**When not using FUSE:** The `/dev/fuse` device exists but no FUSE daemon is
running. The kernel module is compiled in but idle. ~0 overhead.

### WireGuard (1 flag)

```
CONFIG_WIREGUARD=y
```

**What it does:** In-kernel WireGuard VPN implementation. Creates `wg0`-style
interfaces that handle encrypted tunneling entirely in the kernel — no
userspace daemon needed for the data path.

**Why:** WireGuard is the modern standard for VPN connectivity:

- Connect a sandbox to a private network (corporate VPN, tailnet)
- Secure point-to-point tunnels between sandboxes
- Access services that are only reachable over VPN
- Tailscale can use the kernel WireGuard module (faster than wireguard-go)

With `CONFIG_TUN` (above), the userspace `wireguard-go` implementation already
works. But the kernel module is significantly faster — it handles encryption
and packet routing in kernel space, avoiding context switches for every packet.

**Performance difference:** Kernel WireGuard achieves near-line-rate on modern
CPUs (~1-3 Gbps on a single core). wireguard-go tops out around 500 Mbps due
to userspace-kernel context switching overhead.

**When not using WireGuard:** The module is compiled in but no `wg0` interface
exists. The Noise protocol handshake code and ChaCha20-Poly1305 encryption are
linked into the kernel but never executed. ~50KB of kernel text.

### Kernel TLS (1 flag)

```
CONFIG_TLS=y
```

**What it does:** Offloads TLS record encryption/decryption from userspace
(OpenSSL, GnuTLS) to the kernel. Applications `setsockopt(SOL_TLS)` to hand
the TLS session keys to the kernel, then `sendfile()` and `write()` handle
encryption transparently in kernel space.

**Why:**

- **nginx** uses kernel TLS with `ssl_conf_command Options KTLS` — eliminates
  a copy between userspace and kernel for TLS-encrypted static file serving.
  10-20% throughput improvement for HTTPS file serving.
- **HAProxy** supports kTLS for backend connections
- **curl/OpenSSL 3.x** can use kTLS automatically when available
- **Go's crypto/tls** is considering kTLS support

The main benefit is with `sendfile()` — without kTLS, serving an HTTPS
response requires: read file → kernel → userspace → encrypt → kernel → send.
With kTLS: read file → kernel → encrypt-in-kernel → send. One less copy.

**When not using kTLS:** Applications use their normal userspace TLS
implementation. The kernel module is present but `setsockopt(SOL_TLS)` is
never called. No overhead.

### AppArmor + Landlock (2 flags)

```
CONFIG_SECURITY_APPARMOR=y
CONFIG_SECURITY_LANDLOCK=y
```

**AppArmor** is a mandatory access control (MAC) system that confines programs
based on file paths, network access, and capabilities. Docker uses AppArmor by
default on Ubuntu to limit what containers can do — even if a container process
runs as root, AppArmor prevents it from accessing sensitive host paths.

Without AppArmor compiled into the kernel, Docker falls back to no MAC
enforcement. Containers still have namespace isolation but lack the additional
confinement layer. For a bhatti VM where the VM itself is the isolation
boundary, this is defense-in-depth rather than strictly necessary.

**Landlock** is a newer (Linux 5.13+), lightweight sandboxing mechanism that
doesn't require root to use. Any unprivileged process can restrict its own
filesystem and network access via `landlock_create_ruleset()`. Modern security
tools and some container runtimes use it for fine-grained sandboxing without
needing a full LSM like AppArmor or SELinux.

**When not used:** Both LSMs register themselves at boot but apply no
restrictions unless profiles are loaded (AppArmor) or rules are created
(Landlock). Minimal overhead — a few function pointer checks on syscall paths.

## Flags we deliberately skip

**NF_TABLES + NFT_\* (nftables):** Docker works with `iptables-legacy` which
uses the `IP_NF_*` flags we already have. nftables is the newer replacement
for iptables, but Docker's nftables support is still maturing and our testing
confirmed iptables-legacy works. Enabling nftables would add ~20 flags and
doesn't provide benefits for our use case. Can revisit when Docker drops
iptables-legacy support.

**IP_VS\* (IPVS):** IP Virtual Server is the Linux kernel's Layer 4 load
balancer. Docker Swarm uses it for service mesh routing. Since we're running
Docker inside single VMs (not multi-node Swarm clusters), IPVS is unnecessary.

**IP_SCTP:** Stream Control Transmission Protocol — used in telecom (SIGTRAN),
some WebRTC stacks (DTLS/SCTP data channels), and a few databases (Oracle
Cluster). Very niche. Can add if someone specifically needs it.

**DRM / framebuffer:** Firecracker has no GPU passthrough. The computer-use
tier would use Xvfb (virtual framebuffer in userspace) which doesn't need
kernel DRM. Adding DRM would pull in a large subsystem for zero benefit.

**FTRACE:** Kernel function tracing for debugging kernel behavior. Useful for
kernel developers, not for application workloads. Adds measurable overhead to
every function call (even when disabled, the `nop` sled in function prologues
affects instruction cache).

**NFS_V4, CIFS, 9P:** Network filesystems. FUSE covers the common cases
(sshfs, rclone). Native kernel NFS/CIFS adds significant code. Can add later
if someone needs NFS performance.

**BTRFS:** We use ext4 for all rootfs images and volumes. Btrfs adds ~500KB
of kernel text for zero benefit.

## Build process

```bash
#!/bin/bash
# scripts/build-kernel.sh — Build the bhatti kernel
# Usage: ./scripts/build-kernel.sh [arch]
# arch: x86_64 (default) or aarch64

set -euo pipefail
ARCH="${1:-x86_64}"
KERNEL_VERSION="6.1.155"
FC_CI_VERSION="v1.15"

# Map to kernel ARCH names
case "$ARCH" in
    x86_64)  KARCH="x86_64"; CROSS="" ;;
    aarch64) KARCH="arm64";  CROSS="ARCH=arm64 CROSS_COMPILE=aarch64-linux-gnu-" ;;
esac

# Download kernel source
if [ ! -d "linux-${KERNEL_VERSION}" ]; then
    curl -fsSL "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-${KERNEL_VERSION}.tar.xz" | tar xJ
fi
cd "linux-${KERNEL_VERSION}"

# Start from Firecracker CI config
curl -fsSL "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/${FC_CI_VERSION}/${ARCH}/vmlinux-${KERNEL_VERSION}.config" -o .config

# Docker networking
scripts/config --enable CONFIG_IP_NF_RAW
scripts/config --enable CONFIG_IP6_NF_RAW
scripts/config --enable CONFIG_IP_NF_SECURITY
scripts/config --enable CONFIG_IP6_NF_SECURITY

# Docker advanced networking
scripts/config --enable CONFIG_DUMMY
scripts/config --enable CONFIG_MACVLAN
scripts/config --enable CONFIG_IPVLAN
scripts/config --enable CONFIG_VXLAN
scripts/config --enable CONFIG_NET_CLS_CGROUP
scripts/config --enable CONFIG_NETFILTER_XT_MARK

# TUN/TAP (VPNs, userspace networking)
scripts/config --enable CONFIG_TUN

# FUSE (sshfs, rclone, AppImage)
scripts/config --enable CONFIG_FUSE_FS

# WireGuard VPN
scripts/config --enable CONFIG_WIREGUARD

# Kernel TLS offload
scripts/config --enable CONFIG_TLS

# Security: AppArmor + Landlock
scripts/config --enable CONFIG_SECURITY_APPARMOR
scripts/config --enable CONFIG_SECURITY_LANDLOCK

# Resolve dependencies non-interactively
make $CROSS olddefconfig

# Build (vmlinux only, no modules, no bzImage)
make $CROSS -j$(nproc) vmlinux

mkdir -p ../dist
cp vmlinux "../dist/vmlinux-${KERNEL_VERSION}-${ARCH}"
echo "Built: dist/vmlinux-${KERNEL_VERSION}-${ARCH} ($(du -h "../dist/vmlinux-${KERNEL_VERSION}-${ARCH}" | cut -f1))"
```

### Build requirements

- `build-essential` (gcc, make, binutils)
- `flex`, `bison` (kernel build generates parsers)
- `libelf-dev` (BPF/BTF support)
- `libssl-dev` (kernel module signing, kTLS)
- `bc` (kernel build arithmetic)
- For arm64 cross-compilation: `gcc-aarch64-linux-gnu`

### Build time

| Machine | Cores | Time |
|---------|-------|------|
| agni-01 (Ryzen 9 3900) | 24 | ~3-5 min |
| GitHub Actions (ubuntu-latest) | 4 | ~8-12 min |
| Apple M1 (cross-compile to aarch64) | 8 | ~6-8 min |

### Output

A single `vmlinux` file. ~43-45MB uncompressed. No modules, no initramfs, no
bzImage. Firecracker loads the uncompressed ELF directly.

## Versioning

The kernel version tracks the Firecracker CI config version. When Firecracker
updates their CI kernel (e.g., from 6.1.155 to 6.1.160 for a CVE fix), we
rebuild with their new config + our flag additions.

The bhatti release includes the kernel: bhatti v0.4.0 ships with kernel
6.1.155. The kernel is not independently versioned — it's an artifact of
the bhatti build, like the lohar binary.

## Verification

After building, verify all flags are present:

```bash
# Extract config from the built kernel
scripts/extract-ikconfig vmlinux > /tmp/built.config

# Check our additions
for flag in IP_NF_RAW IP6_NF_RAW TUN FUSE_FS WIREGUARD TLS \
           SECURITY_APPARMOR SECURITY_LANDLOCK DUMMY MACVLAN \
           IPVLAN VXLAN IP_NF_SECURITY IP6_NF_SECURITY \
           NET_CLS_CGROUP NETFILTER_XT_MARK; do
    grep -q "CONFIG_${flag}=y" /tmp/built.config && echo "✓ $flag" || echo "✗ $flag MISSING"
done
```

Or boot a VM and check `/proc/config.gz`:

```bash
bhatti exec test-vm -- sh -c 'zcat /proc/config.gz | grep CONFIG_IP_NF_RAW'
# CONFIG_IP_NF_RAW=y
```
