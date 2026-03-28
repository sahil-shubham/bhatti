# Bhatti v0.4 — Kernel, Rootfs Tiers, Installation

v0.3 shipped images, persistent volumes, named snapshots, and OCI support.
The platform primitives are complete. v0.4 makes bhatti installable in
30 seconds instead of 10 minutes, ships three rootfs application profiles,
and builds a custom kernel that enables Docker inside VMs.

---

## Current State

The install script (`scripts/install.sh`) runs `build-rootfs.sh` which:
1. Creates a 2GB ext4 image
2. Runs debootstrap (~200MB of .debs, 3-5 min)
3. Installs 20+ packages in chroot (zsh, git, node, claude-code, etc.)
4. Clones 7 git repos for shell plugins
5. Downloads starship, node.js tarballs

Total: **10+ minutes**, requires root + debootstrap + internet, and is
non-reproducible (packages update between runs). The rootfs is a monolith
that includes everything whether the user needs it or not.

The kernel is the stock Firecracker CI kernel (v1.6 era, 6.1.58). It
lacks `CONFIG_NETFILTER` entirely — no iptables inside VMs. Docker,
WireGuard, TUN/TAP, and FUSE are all broken inside guests.

---

## Target Machines

| Machine | Arch | Role |
|---------|------|------|
| agni-01 | x86_64 / amd64 | Primary dev + test server. All Phase 1-2 work happens here. |
| Raspberry Pis | aarch64 / arm64 | Secondary. arm64 builds run in CI or cross-compile from agni-01. |

Architecture naming conventions used throughout this plan:
- **Kernel / Firecracker:** `x86_64`, `aarch64`
- **Debian / Ubuntu / Go:** `amd64`, `arm64`

---

## Design Principle: Rootfs = Application Profile

Bhatti is programmable Linux. The rootfs defines what kind of Linux.
Three profiles ship, each serving a different use case:

```
minimal   — bare Ubuntu + lohar deps. The base layer.
browser   — minimal + headless Chromium + Playwright. Browser automation.
docker    — minimal + Docker Engine. Containers inside VMs.
```

There is no "default" tier with dev tools pre-installed. Users who want
zsh/git/node/claude-code boot from `minimal`, install what they want,
then `bhatti image save` to snapshot their custom environment. This is
the primary workflow — ship minimal, users build their own.

### Why not Playwright inside Docker?

The browser tier runs Chromium directly in the VM, not inside a Docker
container. This is deliberate:

1. **Snapshot semantics.** Firecracker snapshots capture VM process memory.
   Standalone Chromium is one process — clean snapshot/resume. Chromium
   inside Docker means snapshotting dockerd + containerd + shim + chromium.
   Docker's internal state machines (health checks, restart policies,
   event streams) may not tolerate the time jump on resume.

2. **No kernel dependency.** Browser tier needs only `/dev/shm` for
   Chromium shared memory. No iptables, no bridge networking, no overlay
   filesystem. It works with the stock Firecracker CI kernel — shipping
   before the custom kernel is built.

3. **Lighter and faster.** ~600MB vs ~800MB+. No dockerd startup (2-3s),
   no container image pull. Chromium launches directly, CDP ready in <1s.

### Why no default tier

The current rootfs has: zsh, zinit, 3 zinit plugins, starship, tmux,
3 tmux plugins, vim-tiny, htop, jq, ripgrep, fd-find, git, curl, wget,
socat, unzip, xz-utils, node 22, claude-code, custom .zshrc, custom
.tmux.conf. None of this is needed by bhatti. It's one developer's
opinionated setup baked into every installation.

Users who want this exact setup can build it once and save it as an
image. Users who want Python instead of Node, or fish instead of zsh,
or no shell customization at all, aren't forced to carry 500MB of
someone else's preferences.

---

## Part 1 — Custom Kernel

### 1.1 Why

The stock Firecracker CI kernel (v1.6 era, 6.1.58) lacks
`CONFIG_NETFILTER` entirely — no iptables inside VMs. Docker networking
is completely broken.

The v1.15 CI kernel (6.1.155) has `NETFILTER` and iptables but is
missing `IP_NF_RAW` — a single flag that Docker 28+ requires for
bridge networking.

**Verified on agni-01 (x86_64, 2026-03-26):** Booted the v1.15 kernel
with Firecracker v1.14.0. Docker 29.3.1 starts, pulls images, runs
`hello-world` with `--network host`. Bridge networking fails with
`iptables: can't initialize table 'raw'`. `/proc/config.gz` confirms
`CONFIG_IP_NF_RAW is not set`.

### 1.2 What we change

Start from the Firecracker CI v1.15 config (6.1.155). Enable **13 flags**
— strictly what Docker bridge networking requires:

```bash
# ── Docker bridge networking (hard blockers) ──
# Without these, `docker run` fails with "can't initialize table 'raw'".
scripts/config --enable CONFIG_IP_NF_RAW
scripts/config --enable CONFIG_IP6_NF_RAW

# ── Docker container plumbing ──
# BRIDGE: docker0 is a kernel bridge. Required for bridge networking.
# VETH: every container gets a veth pair into docker0. No veth = no containers.
# OVERLAY_FS: Docker's default storage driver. Without it, Docker falls back
#   to `vfs` which copies the entire image per container — unusable in a 2GB rootfs.
# NF_CONNTRACK + XT_CONNTRACK: required by iptables `-m state --state
#   RELATED,ESTABLISHED` which Docker's FORWARD rules use.
scripts/config --enable CONFIG_BRIDGE
scripts/config --enable CONFIG_VETH
scripts/config --enable CONFIG_OVERLAY_FS
scripts/config --enable CONFIG_NF_CONNTRACK
scripts/config --enable CONFIG_NETFILTER_XT_CONNTRACK

# ── Docker security tables ──
# Required by Docker's default iptables rules for container isolation.
scripts/config --enable CONFIG_IP_NF_SECURITY
scripts/config --enable CONFIG_IP6_NF_SECURITY

# ── Docker traffic shaping ──
# NET_CLS_CGROUP: Docker uses cgroup net_cls for traffic classification.
# XT_MARK: Docker uses MARK target for routing decisions.
scripts/config --enable CONFIG_NET_CLS_CGROUP
scripts/config --enable CONFIG_NETFILTER_XT_MARK
```

All flags are `=y` (built-in). Firecracker doesn't support loadable
modules — any flag set to `=m` in the CI config is effectively missing.

**Pre-build verification step:** Before building, check which of these
flags the v1.15 CI config already has as `=y`. Any that are already
`=y` are harmless to re-enable (idempotent). Any that are `=m` must be
flipped to `=y`. The `scripts/config --enable` call handles both cases.

```bash
# Run on agni-01 to audit the base config before building:
curl -fsSL "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.15/x86_64/vmlinux-6.1.155.config" \
  | grep -E 'BRIDGE|VETH|OVERLAY_FS|NF_CONNTRACK|IP_NF_RAW|IP6_NF_RAW|IP_NF_SECURITY|IP6_NF_SECURITY|NETFILTER_XT_CONNTRACK|NET_CLS_CGROUP|NETFILTER_XT_MARK'
```

### 1.3 What we deliberately skip

**TUN, FUSE, WireGuard, kTLS, AppArmor, Landlock:** Useful but not
needed by any v0.4 tier. Can be added later with zero risk — same
process, append flags, rebuild. Keeping the flag set minimal means fewer
unknowns to debug if the kernel misbehaves.

**DUMMY, MACVLAN, IPVLAN, VXLAN:** Docker Swarm overlay networks and
macvlan drivers. Nobody runs Swarm inside a single Firecracker VM.
Standard bridge networking covers all v0.4 use cases.

**NF_TABLES + NFT_\*:** Docker works with `iptables-legacy`. nftables
adds ~20 config flags for zero benefit in our use case.

**IP_VS\*:** IPVS is Docker Swarm load balancing.

**DRM / framebuffer:** No GPU in Firecracker. Xvfb works without DRM.

**FTRACE:** Kernel function tracing adds overhead to every function call
even when disabled (nop sled in function prologues). Not worth it for
application workloads.

### 1.4 One kernel for all tiers

Every flag we enable is inert when unused. Empty iptables tables, no
bridge devices created. Zero runtime overhead for minimal/browser tiers.
Ship one kernel artifact for all tiers.

### 1.5 Build script

**File:** `scripts/build-kernel.sh`

```bash
#!/bin/bash
# Build the bhatti kernel from Firecracker CI config + additional flags.
# Usage: ./scripts/build-kernel.sh [arch]
#   arch: x86_64 (default) or aarch64
#
# Requirements: build-essential flex bison libelf-dev libssl-dev bc
# For aarch64 cross-compile: gcc-aarch64-linux-gnu
set -euo pipefail

ARCH="${1:-x86_64}"
KERNEL_VERSION="6.1.155"
FC_CI_VERSION="v1.15"

case "$ARCH" in
    x86_64)  KARCH="x86_64"; CROSS="" ;;
    aarch64) KARCH="arm64";  CROSS="ARCH=arm64 CROSS_COMPILE=aarch64-linux-gnu-" ;;
    *) echo "unsupported arch: $ARCH" >&2; exit 1 ;;
esac

# Download kernel source if not cached
if [ ! -d "linux-${KERNEL_VERSION}" ]; then
    echo "==> Downloading kernel ${KERNEL_VERSION} source..."
    curl -fsSL "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-${KERNEL_VERSION}.tar.xz" | tar xJ
fi
cd "linux-${KERNEL_VERSION}"

# Start from Firecracker CI config
echo "==> Downloading Firecracker CI config (${FC_CI_VERSION}/${ARCH})..."
curl -fsSL "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/${FC_CI_VERSION}/${ARCH}/vmlinux-${KERNEL_VERSION}.config" -o .config

# Audit: show current state of our flags before modification
echo "==> Current state of bhatti flags in CI config:"
for flag in IP_NF_RAW IP6_NF_RAW BRIDGE VETH OVERLAY_FS NF_CONNTRACK \
    NETFILTER_XT_CONNTRACK IP_NF_SECURITY IP6_NF_SECURITY \
    NET_CLS_CGROUP NETFILTER_XT_MARK; do
    grep "CONFIG_${flag}[= ]" .config 2>/dev/null || echo "# CONFIG_${flag} is not set"
done

# Apply bhatti additions (idempotent — safe if already =y)
echo "==> Applying bhatti kernel config (13 flags)..."

# Docker bridge networking (hard blockers)
scripts/config --enable CONFIG_IP_NF_RAW
scripts/config --enable CONFIG_IP6_NF_RAW

# Docker container plumbing
scripts/config --enable CONFIG_BRIDGE
scripts/config --enable CONFIG_VETH
scripts/config --enable CONFIG_OVERLAY_FS
scripts/config --enable CONFIG_NF_CONNTRACK
scripts/config --enable CONFIG_NETFILTER_XT_CONNTRACK

# Docker security tables
scripts/config --enable CONFIG_IP_NF_SECURITY
scripts/config --enable CONFIG_IP6_NF_SECURITY

# Docker traffic shaping
scripts/config --enable CONFIG_NET_CLS_CGROUP
scripts/config --enable CONFIG_NETFILTER_XT_MARK

# Resolve dependencies (turns on transitive deps, answers new prompts with defaults)
make $CROSS olddefconfig

# Post-build verification: ensure critical flags survived olddefconfig
echo "==> Verifying critical flags in final config..."
MISSING=0
for flag in IP_NF_RAW IP6_NF_RAW BRIDGE VETH OVERLAY_FS NF_CONNTRACK NETFILTER_XT_CONNTRACK; do
    if ! grep -q "CONFIG_${flag}=y" .config; then
        echo "FATAL: CONFIG_${flag} not set to =y after olddefconfig" >&2
        MISSING=1
    fi
done
if [ "$MISSING" -eq 1 ]; then
    echo "Aborting: critical kernel flags missing. Check dependency conflicts." >&2
    exit 1
fi

# Build
echo "==> Building vmlinux ($(nproc) cores)..."
make $CROSS -j$(nproc) vmlinux

mkdir -p ../dist
cp vmlinux "../dist/vmlinux-${KERNEL_VERSION}-${ARCH}"
echo "==> Built: dist/vmlinux-${KERNEL_VERSION}-${ARCH} ($(du -h "../dist/vmlinux-${KERNEL_VERSION}-${ARCH}" | cut -f1))"
```

### 1.6 Verification

After building, boot a VM on agni-01 and check:

```bash
# Kernel flags
bhatti exec test -- sh -c 'zcat /proc/config.gz | grep -E "IP_NF_RAW|BRIDGE|VETH|OVERLAY_FS|NF_CONNTRACK"'
# CONFIG_IP_NF_RAW=y
# CONFIG_BRIDGE=y
# CONFIG_VETH=y
# CONFIG_OVERLAY_FS=y
# CONFIG_NF_CONNTRACK=y

# iptables raw table works
bhatti exec test -- sudo iptables -t raw -L
# Chain PREROUTING (policy ACCEPT) ...

# Docker full path
bhatti exec test -- sudo docker run --rm hello-world
# Hello from Docker!
```

### 1.7 Firecracker version upgrade (v1.6 → v1.14)

The kernel and Firecracker must be from the same generation. The v1.15 CI
kernel doesn't boot with Firecracker v1.6 (kernel panic: `VFS: Cannot open
root device "vda"`). **Upgrade to Firecracker v1.14.0** (latest stable in
the v1.15 CI artifacts).

Verified on agni-01 (x86_64): Firecracker v1.14.0 + kernel 6.1.155 boots
and runs correctly.

**Migration on agni-01:**

1. **Destroy all existing sandboxes and snapshots.** FC v1.6 snapshot
   formats (mem.snap, vm.snap) are incompatible with FC v1.14 — they
   cannot be resumed. This is a one-time cost.
   ```bash
   bhatti list --json | jq -r '.[].id' | xargs -I{} bhatti destroy {}
   # Also delete any saved snapshots:
   rm -rf /var/lib/bhatti/snapshots/*
   ```

2. **Verify API compatibility.** The Go code in `engine.go` talks to
   Firecracker's HTTP API directly (PUT `/boot-source`, `/drives/*`,
   `/machine-config`, `/vsock`, `/network-interfaces/*`, PATCH `/vm`,
   PUT `/snapshot/create`, PUT `/snapshot/load`). All of these endpoints
   exist in FC v1.14 with the same request schema. Verified against
   the [v1.14 API spec](https://github.com/firecracker-microvm/firecracker/blob/main/src/api_server/swagger/firecracker.yaml).

3. **Replace binary:**
   ```bash
   curl -fsSL "https://github.com/firecracker-microvm/firecracker/releases/download/v1.14.0/firecracker-v1.14.0-x86_64.tgz" | tar xz
   sudo mv release-v1.14.0-x86_64/firecracker-v1.14.0-x86_64 /usr/local/bin/firecracker
   sudo chmod +x /usr/local/bin/firecracker
   ```

4. **Update `scripts/install.sh`:** Change `FC_VERSION="1.6.0"` →
   `FC_VERSION="1.14.0"` and `FC_MAJOR_MINOR="1.6"` → remove (no longer
   used for kernel URL — kernel is now a build artifact, not downloaded
   from Firecracker CI).

### 1.8 Engineering overhead

We are NOT patching kernel source or maintaining a fork. We take the exact
`.config` that Firecracker's CI team maintains (tested against every FC
commit), flip 13 flags from `n`/`m` to `=y`, and run `make vmlinux`.

When Firecracker updates their CI kernel (e.g., 6.1.155 → 6.1.170 for a
CVE), we download their new config, apply the same 13 flags, rebuild.
A 15-minute task that runs in CI.

### 1.9 Tests

- `TestKernelHasNetfilterRaw` — boot VM, `zcat /proc/config.gz | grep IP_NF_RAW` → `=y`
- `TestKernelHasBridge` — boot VM, `zcat /proc/config.gz | grep CONFIG_BRIDGE` → `=y`
- `TestKernelHasVeth` — boot VM, `zcat /proc/config.gz | grep CONFIG_VETH` → `=y`
- `TestKernelHasOverlayFS` — boot VM, `zcat /proc/config.gz | grep OVERLAY_FS` → `=y`
- `TestKernelHasConntrack` — boot VM, `zcat /proc/config.gz | grep NF_CONNTRACK` → `=y`
- `TestDockerBridgeNetworking` — boot docker tier, start dockerd,
  `docker run --rm alpine ping -c1 8.8.8.8` → succeeds (bridge, not host network)

---

## Part 2 — Rootfs Tiers

### 2.1 Minimal (~100MB uncompressed)

The thinnest image that lohar can boot and users can work in.

**Contents:**
- Ubuntu 24.04 minbase (debootstrap `--variant=minbase`)
- `iproute2` — lohar calls `ip addr add`, `ip route add` for network setup;
  engine calls `ss -tln` via exec for port discovery
- `ca-certificates` — TLS from inside the VM
- `sudo` — lohar runs exec as uid 1000, users need root escalation
- `curl` — basic HTTP client for bootstrapping (installing tools, etc.)
- `bash` — comes with minbase, needed as fallback shell
- `lohar` binary at `/usr/local/bin/lohar`
- `lohar` user (uid 1000, gid 1000) with NOPASSWD sudo
- `/workspace` directory owned by lohar
- Static `/etc/resolv.conf` (1.1.1.1, 8.8.8.8)
- `en_US.UTF-8` locale

**Not included:** zsh, git, node, tmux, vim, htop, jq, ripgrep, fd-find,
socat, starship, any shell plugins, any shell configs. Users install what
they need.

**Use case:** Base layer for OCI images. Users who want full control.
CI runners that install their own deps. The "start from nothing" path.

### 2.2 Browser (~600MB uncompressed)

Headless Chromium with Playwright for browser automation.

**Contents (everything in minimal, plus):**
- Chromium via Playwright's bundled build (see §2.2.1)
- Chromium system dependencies (libatk, libcups, libnss3, etc.)
- Node.js 22.x LTS (Playwright needs it)
- Python 3.12 (Playwright Python bindings)
- Playwright pinned to a specific version (see §2.2.2)
- Boot profile: `/etc/bhatti/init.sh` starts Chromium with CDP on port 9222

**Kernel dependency: none.** Browser tier needs only `/dev/shm` for
Chromium shared memory, which lohar mounts unconditionally (§3.2.1).
No iptables, no bridge, no overlay. Works with the stock Firecracker
CI kernel. This is why browser ships before docker.

#### 2.2.1 Chromium source: Playwright bundle, not Ubuntu repos

On Ubuntu 24.04 (noble), `apt-get install chromium-browser` installs a
transitional package that redirects to snap. In a minbase chroot without
snapd, this gives you a broken shim, not an actual browser.

Instead, use Playwright's bundled Chromium:
```bash
npx playwright install chromium        # downloads a known-good binary
npx playwright install-deps chromium   # installs system lib dependencies
```

This gives a binary at a well-known path
(`~/.cache/ms-playwright/chromium-*/chrome-linux/chrome`) that is tested
against the exact Playwright version we pin.

#### 2.2.2 Playwright version pinning

Playwright's Chromium coupling is notoriously fragile. We pin the exact
Playwright version so the bundled Chromium and the Python/Node API are
tested together:

```bash
pip3 install --break-system-packages playwright==1.50.0
npm install -g playwright@1.50.0
npx playwright install chromium
npx playwright install-deps chromium
```

Update the pinned version per release, not per rootfs build. The CI
workflow validates the combination.

**Boot profile (`/etc/bhatti/init.sh`):**
```bash
#!/bin/sh
# Resolve Playwright's bundled Chromium path
CHROMIUM=$(find /root/.cache/ms-playwright -name chrome -type f 2>/dev/null | head -1)
if [ -z "$CHROMIUM" ]; then
    echo "bhatti: chromium not found" >&2
    exit 1
fi

"$CHROMIUM" \
    --headless \
    --no-sandbox \
    --disable-gpu \
    --remote-debugging-port=9222 \
    --remote-debugging-address=0.0.0.0 &

# Wait for CDP to accept connections (up to 5 seconds)
for i in $(seq 1 50); do
    curl -sf http://localhost:9222/json/version >/dev/null 2>&1 && break
    sleep 0.1
done

if ! curl -sf http://localhost:9222/json/version >/dev/null 2>&1; then
    echo "bhatti: chromium CDP not ready after 5s" >&2
fi
```

The readiness wait ensures that by the time the user's `--init` script
runs (step 6 in boot order), CDP is accepting connections. Without this,
a user init that immediately connects to `localhost:9222` would race
against Chromium startup.

**How it's used:**
```bash
bhatti create --name scraper --image browser
# Chromium starts automatically on port 9222, ready by the time create returns

# AI agent connects via CDP through bhatti's tunnel:
bhatti exec scraper -- python3 -c "
from playwright.sync_api import sync_playwright
with sync_playwright() as p:
    browser = p.chromium.connect_over_cdp('http://localhost:9222')
    page = browser.new_page()
    page.goto('https://example.com')
    print(page.title())
"
```

**The snapshot story:** Navigate to a page, log in, get to a specific
state — `bhatti snapshot create`. Resume that snapshot 100 times, each
starting from the logged-in state with Chromium's full process memory
restored. No re-login, no cookie management, no re-navigation.

**Use case:** AI web agents, scraping, browser testing, screenshot
capture. No new bhatti API needed — CDP over port tunnel works today.

### 2.3 Docker (~800MB uncompressed)

Docker Engine running inside the VM.

**Contents (everything in minimal, plus):**
- `docker-ce`, `containerd`, `runc` (from Docker's apt repo)
- `iptables` (legacy mode configured via `update-alternatives`)
- Boot profile: `/etc/bhatti/init.sh` starts dockerd with readiness check

**Boot profile (`/etc/bhatti/init.sh`):**
```bash
#!/bin/sh
# iptables-legacy (kernel has legacy iptables, not nftables)
update-alternatives --set iptables /usr/sbin/iptables-legacy 2>/dev/null
update-alternatives --set ip6tables /usr/sbin/ip6tables-legacy 2>/dev/null

# Start Docker daemon
dockerd > /var/log/dockerd.log 2>&1 &

# Wait for socket (up to 10 seconds, then give up)
for i in $(seq 1 100); do
    [ -S /var/run/docker.sock ] && break
    sleep 0.1
done

if [ ! -S /var/run/docker.sock ]; then
    echo "bhatti: dockerd failed to start within 10s, check /var/log/dockerd.log" >&2
fi
```

Note: cgroups v2 and `/dev/shm` are mounted by lohar unconditionally
(see §3.2), so the boot profile doesn't need to handle them.

**How it's used:**
```bash
bhatti create --name ci --image docker --memory 2048
bhatti exec ci -- docker run --rm postgres:16 postgres --version
bhatti exec ci -- docker compose up -d
```

**The snapshot story:** Boot a sandbox, `docker compose up` your full
stack (Postgres + Redis + your app), wait for everything to be healthy,
then `bhatti snapshot create`. Resume later — all containers are running,
databases have data, app is connected. No cold start, no seeding.

**Kernel requirement:** Custom kernel with `CONFIG_IP_NF_RAW=y`,
`CONFIG_BRIDGE=y`, `CONFIG_VETH=y`, `CONFIG_OVERLAY_FS=y`,
`CONFIG_NF_CONNTRACK=y`. Without these, Docker's bridge networking
refuses to create containers. Verified on agni-01: with the stock v1.15
kernel, `docker run hello-world` fails with
`can't initialize iptables table 'raw'`. With the bhatti kernel, bridge
networking works.

**Use case:** Docker-based CI pipelines, testcontainers, running
databases, any workload that needs Docker's ecosystem.

---

## Part 3 — Boot Profiles + lohar changes

### 3.1 Mechanism

A boot profile is a script at `/etc/bhatti/init.sh` inside the rootfs.
Lohar runs it after system setup and **after the agent listeners are
accepting connections**. This ordering is critical — if a boot profile
hangs, the VM must still be reachable via `bhatti exec` for debugging.

**Execution order:**
```
1. lohar PID 1 init (mount /proc, /sys, /dev, /dev/pts, /dev/shm, /tmp, /run)
2. lohar mounts cgroups v2 unconditionally
3. loadConfigDrive() — env vars, files, volumes
4. setupNetworking()
5. installSignalHandlers()
6. start vsock + TCP listeners ← agent is now reachable
7. "lohar: ready"
8. /etc/bhatti/init.sh (boot profile — if exists, 30s timeout)
9. user's --init script (from create request, runs as uid 1000)
```

The boot profile starts tier-specific services (Chromium, dockerd) and
**waits for them to be ready** before returning. The user's init runs
in an environment where those services are already accepting connections.

### 3.2 lohar changes

**File:** `cmd/lohar/main.go`

Two changes to PID 1 init, one change to post-listener startup.

#### 3.2.1 Mount /dev/shm and cgroups v2 unconditionally

Add to PID 1 init, right after the existing `/dev/pts` and `/tmp` mounts:

```go
// /dev/shm — required by Chromium (shared memory), Docker containers,
// and any process using shm_open(3). Harmless when unused.
mustMount("tmpfs", "/dev/shm", "tmpfs", 0, "")

// cgroups v2 — required by Docker for resource isolation.
// Mount unconditionally: zero overhead when unused, avoids needing
// tier-specific logic in lohar.
os.MkdirAll("/sys/fs/cgroup", 0755)
if err := syscall.Mount("cgroup2", "/sys/fs/cgroup", "cgroup2", 0, ""); err != nil {
    // Non-fatal: may already be mounted, or kernel may not support it
    fmt.Fprintf(os.Stderr, "lohar: mount cgroup2: %v\n", err)
}
// Enable cgroup controllers for Docker. Without this, dockerd can't
// create cgroups for containers (errors like "cgroup: no such file").
os.WriteFile("/sys/fs/cgroup/cgroup.subtree_control",
    []byte("+cpu +memory +io +pids"), 0644)
```

#### 3.2.2 Run boot profile after listeners, with timeout

**After** the vsock + TCP listeners are started and "lohar: ready" is
printed, but **before** the user init session:

```go
fmt.Fprintln(os.Stderr, "lohar: ready")

// Run boot profile if present. Runs AFTER listeners so the VM is
// reachable via bhatti exec even if the boot profile hangs.
// 30-second hard timeout — if dockerd or chromium can't start in 30s,
// something is broken. Don't block forever.
if _, err := os.Stat("/etc/bhatti/init.sh"); err == nil {
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()
    cmd := exec.CommandContext(ctx, "/bin/sh", "/etc/bhatti/init.sh")
    cmd.Stdout = os.Stderr
    cmd.Stderr = os.Stderr
    cmd.Env = buildEnv(nil)
    if err := cmd.Run(); err != nil {
        if ctx.Err() == context.DeadlineExceeded {
            fmt.Fprintf(os.Stderr, "lohar: boot profile timed out after 30s\n")
        } else {
            fmt.Fprintf(os.Stderr, "lohar: boot profile failed: %v\n", err)
        }
        // Non-fatal — sandbox is still reachable, just without tier services
    }
}

// Init script runs as a TTY session (can be attached to via session ID "init")
if cfg != nil && cfg.Init != "" {
    go runInitSession(cfg.Init, cfg.User)
}
```

The boot profile runs as root (PID 1 context). It needs root to start
dockerd, etc. The user's `--init` runs as uid 1000 (via the existing
`runInitSession` which applies `Credential{Uid: 1000}`).

### 3.3 Shell selection fix

**Problem:** `engine.go` hardcodes `/bin/zsh` for shell sessions:

```go
return ag.Shell(ctx, []string{"/bin/zsh", "-li"}, ...)
```

The minimal tier doesn't have zsh. `bhatti shell` would fail.

**Fix:** Change to `/bin/bash` with `-li` flags. Every tier has bash
(comes with minbase). One-line change, no caching, no probing, no new
struct fields.

**File:** `pkg/engine/firecracker/engine.go`, line 1033:

```go
// Before:
return ag.Shell(ctx, []string{"/bin/zsh", "-li"}, map[string]string{
    "TERM": "xterm-256color",
}, 24, 80)

// After:
return ag.Shell(ctx, []string{"/bin/bash", "-li"}, map[string]string{
    "TERM": "xterm-256color",
}, 24, 80)
```

Users who install zsh and want it as their shell can `chsh` inside the
VM and save the image. Zsh detection can be added in a future version
if there's demand.

### 3.4 Tests

- `TestBootProfileRuns` — create sandbox from image with `/etc/bhatti/init.sh`
  that writes a marker file, exec `cat /tmp/boot-profile-ran` → exists
- `TestBootProfileBeforeUserInit` — boot profile writes timestamp to
  `/tmp/profile-ts`, user init writes to `/tmp/init-ts`, verify profile
  timestamp is earlier
- `TestBootProfileFailureNonFatal` — boot profile that exits 1, sandbox
  still boots, exec works
- `TestBootProfileRunsAsRoot` — boot profile runs `whoami > /tmp/who`,
  verify contents is `root`
- `TestBootProfileTimeout` — boot profile that `sleep 60`, verify it
  gets killed after 30s, sandbox is still reachable via exec
- `TestDevShmMounted` — boot minimal, exec `mount | grep /dev/shm` → tmpfs
- `TestCgroupsMounted` — boot minimal, exec `mount | grep cgroup2` → mounted
- `TestShellUsesBash` — boot from minimal, `bhatti shell` opens bash

---

## Part 4 — Tier Build Scripts

### 4.1 Structure

```
scripts/
  build-kernel.sh           # Part 1.5
  tiers/
    minimal.sh              # tier script — runs inside chroot
    browser.sh              # sources minimal.sh, adds Chromium
    docker.sh               # sources minimal.sh, adds Docker
  build-tier.sh             # orchestrator: create ext4, debootstrap, run tier script
```

### 4.2 Orchestrator

**File:** `scripts/build-tier.sh`

```bash
#!/bin/bash
# Build a rootfs tier image.
# Usage: sudo ./scripts/build-tier.sh <tier> <arch> <lohar-binary>
#   tier: minimal, browser, docker
#   arch: amd64, arm64
#
# Output: dist/rootfs-<tier>-<arch>.ext4
# Environment:
#   SIZE_MB — image size (default: auto per tier)
#
# For cross-arch builds (e.g., arm64 on amd64 host):
#   sudo apt-get install qemu-user-static  # registers binfmt_misc handlers
#   sudo ./scripts/build-tier.sh minimal arm64 ./lohar-arm64
set -euo pipefail

TIER="${1:?usage: build-tier.sh <tier> <arch> <lohar-binary>}"
ARCH="${2:?}"
AGENT="${3:?}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Tier-specific defaults
case "$TIER" in
    minimal) SIZE_MB="${SIZE_MB:-512}" ;;
    browser) SIZE_MB="${SIZE_MB:-2048}" ;;
    docker)  SIZE_MB="${SIZE_MB:-2048}" ;;
    *) echo "unknown tier: $TIER" >&2; exit 1 ;;
esac

case "$ARCH" in
    amd64) DEB_ARCH="amd64"; MIRROR="http://archive.ubuntu.com/ubuntu" ;;
    arm64) DEB_ARCH="arm64"; MIRROR="http://ports.ubuntu.com/ubuntu-ports" ;;
    *) echo "unsupported arch: $ARCH" >&2; exit 1 ;;
esac

IMG="dist/rootfs-${TIER}-${ARCH}.ext4"
MOUNT="/mnt/bhatti-${TIER}-$$"

mkdir -p dist

# Robust cleanup: kill leaked chroot processes, lazy-unmount everything.
# Lazy unmount (-l) is essential for CI runners where stale mounts from
# a failed previous build would block the next job.
cleanup() {
    set +e
    echo "==> Cleaning up..."
    fuser -km "$MOUNT" 2>/dev/null; sleep 1
    umount -l "$MOUNT/dev/pts" 2>/dev/null
    umount -l "$MOUNT/dev"     2>/dev/null
    umount -l "$MOUNT/sys"     2>/dev/null
    umount -l "$MOUNT/proc"    2>/dev/null
    umount -l "$MOUNT"         2>/dev/null
    rmdir "$MOUNT"             2>/dev/null
}
trap cleanup EXIT

# Create ext4 image
dd if=/dev/zero of="$IMG" bs=1M count="$SIZE_MB" status=progress
mkfs.ext4 -F -q "$IMG"
mkdir -p "$MOUNT"
mount "$IMG" "$MOUNT"

# Bootstrap minimal Ubuntu
debootstrap --variant=minbase --arch="$DEB_ARCH" noble "$MOUNT" "$MIRROR"

# Set up chroot
cp /etc/resolv.conf "$MOUNT/etc/resolv.conf"
mount --bind /proc    "$MOUNT/proc"
mount --bind /sys     "$MOUNT/sys"
mount --bind /dev     "$MOUNT/dev"
mount --bind /dev/pts "$MOUNT/dev/pts"

# Run tier script
export MOUNT ARCH DEB_ARCH AGENT SCRIPT_DIR
"$SCRIPT_DIR/tiers/${TIER}.sh"

echo "==> Built: $IMG ($(du -h "$IMG" | cut -f1))"
```

### 4.3 Minimal tier script

**File:** `scripts/tiers/minimal.sh`

```bash
#!/bin/bash
# Minimal tier: bare Ubuntu + lohar dependencies.
# Called by build-tier.sh with $MOUNT, $ARCH, $AGENT set.
set -euo pipefail

chroot "$MOUNT" /bin/bash -c '
set -eu
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y --no-install-recommends \
    iproute2 ca-certificates sudo curl locales

# Locale
sed -i "/en_US.UTF-8/s/^# //g" /etc/locale.gen
locale-gen

# Create lohar user (bash is the default shell — comes with minbase)
useradd -m -s /bin/bash -G sudo lohar
echo "lohar ALL=(ALL) NOPASSWD:ALL" >> /etc/sudoers

apt-get clean
rm -rf /var/lib/apt/lists/*
'

# Workspace
mkdir -p "$MOUNT/workspace"
chown 1000:1000 "$MOUNT/workspace"

# DNS
cat > "$MOUNT/etc/resolv.conf" << 'EOF'
nameserver 1.1.1.1
nameserver 8.8.8.8
EOF

# Install lohar
cp "$AGENT" "$MOUNT/usr/local/bin/lohar"
chmod 755 "$MOUNT/usr/local/bin/lohar"
```

### 4.4 Browser tier script

**File:** `scripts/tiers/browser.sh`

```bash
#!/bin/bash
# Browser tier: minimal + headless Chromium + Playwright.
# Sources minimal.sh first.
set -euo pipefail

PLAYWRIGHT_VERSION="1.50.0"

# Build minimal base first
"$SCRIPT_DIR/tiers/minimal.sh"

chroot "$MOUNT" /bin/bash -c "
set -eu
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq

# Node.js (for Playwright CLI)
NODE_VERSION=22.16.0
case \$(dpkg --print-architecture) in
    amd64) NODE_ARCH=x64 ;;
    arm64) NODE_ARCH=arm64 ;;
esac
curl -fsSL \"https://nodejs.org/dist/v\${NODE_VERSION}/node-v\${NODE_VERSION}-linux-\${NODE_ARCH}.tar.xz\" \\
    | tar -xJ --strip-components=1 -C /usr/local

# Python 3 + Playwright (pinned version)
apt-get install -y --no-install-recommends python3 python3-pip
pip3 install --break-system-packages playwright==${PLAYWRIGHT_VERSION}
npm install -g playwright@${PLAYWRIGHT_VERSION}

# Install Playwright's bundled Chromium + its system dependencies.
# This avoids Ubuntu's chromium-browser package which is a snap redirect
# in noble and broken in minbase chroots without snapd.
npx playwright install chromium
npx playwright install-deps chromium

apt-get clean
rm -rf /var/lib/apt/lists/* /tmp/*
"

# Boot profile: start Chromium with CDP + readiness wait
mkdir -p "$MOUNT/etc/bhatti"
cat > "$MOUNT/etc/bhatti/init.sh" << 'PROFILE'
#!/bin/sh
# Resolve Playwright's bundled Chromium path.
# Playwright installs to a versioned directory; use the CLI to get the
# exact path rather than fragile find+glob.
CHROMIUM=$(python3 -c "from playwright._impl._driver import compute_driver_executable; import subprocess; print(subprocess.check_output([compute_driver_executable(), 'print-browsers-json']).decode())" 2>/dev/null \
  | python3 -c "import sys,json; browsers=json.load(sys.stdin); print(next(b['executablePath'] for b in browsers if b['name']=='chromium'))" 2>/dev/null)

# Fallback to find if the above fails
if [ -z "$CHROMIUM" ]; then
    CHROMIUM=$(find /root/.cache/ms-playwright -path '*/chrome-linux/chrome' -type f 2>/dev/null | head -1)
fi

if [ -z "$CHROMIUM" ]; then
    echo "bhatti: chromium not found" >&2
    exit 1
fi

"$CHROMIUM" \
    --headless \
    --no-sandbox \
    --disable-gpu \
    --remote-debugging-port=9222 \
    --remote-debugging-address=0.0.0.0 &

# Wait for CDP to accept connections (up to 5 seconds)
for i in $(seq 1 50); do
    curl -sf http://localhost:9222/json/version >/dev/null 2>&1 && break
    sleep 0.1
done

if ! curl -sf http://localhost:9222/json/version >/dev/null 2>&1; then
    echo "bhatti: chromium CDP not ready after 5s" >&2
fi
PROFILE
chmod 755 "$MOUNT/etc/bhatti/init.sh"
```

### 4.5 Docker tier script

**File:** `scripts/tiers/docker.sh`

```bash
#!/bin/bash
# Docker tier: minimal + Docker Engine.
# Sources minimal.sh first.
set -euo pipefail

# Build minimal base first
"$SCRIPT_DIR/tiers/minimal.sh"

chroot "$MOUNT" /bin/bash -c '
set -eu
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq

# Docker prerequisites
apt-get install -y --no-install-recommends \
    ca-certificates curl gnupg iptables

# Docker repo
curl -fsSL https://download.docker.com/linux/ubuntu/gpg \
    | gpg --dearmor -o /usr/share/keyrings/docker-archive-keyring.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/docker-archive-keyring.gpg] \
    https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo $VERSION_CODENAME) stable" \
    > /etc/apt/sources.list.d/docker.list

apt-get update -qq
apt-get install -y --no-install-recommends docker-ce docker-ce-cli containerd.io

# Configure iptables-legacy (kernel has legacy iptables, not nftables)
update-alternatives --set iptables /usr/sbin/iptables-legacy
update-alternatives --set ip6tables /usr/sbin/ip6tables-legacy

# Add lohar user to docker group (standard Docker access control)
usermod -aG docker lohar

apt-get clean
rm -rf /var/lib/apt/lists/*
'

# Boot profile: start dockerd with readiness check + timeout
mkdir -p "$MOUNT/etc/bhatti"
cat > "$MOUNT/etc/bhatti/init.sh" << 'PROFILE'
#!/bin/sh
# iptables-legacy (kernel has legacy iptables, not nftables)
update-alternatives --set iptables /usr/sbin/iptables-legacy 2>/dev/null
update-alternatives --set ip6tables /usr/sbin/ip6tables-legacy 2>/dev/null

# Start Docker daemon
dockerd > /var/log/dockerd.log 2>&1 &

# Wait for socket (up to 10 seconds, then give up — non-fatal)
for i in $(seq 1 100); do
    [ -S /var/run/docker.sock ] && break
    sleep 0.1
done

if [ ! -S /var/run/docker.sock ]; then
    echo "bhatti: dockerd failed to start within 10s, check /var/log/dockerd.log" >&2
fi
PROFILE
chmod 755 "$MOUNT/etc/bhatti/init.sh"
```

Note: the old boot profile had `chmod 666 /var/run/docker.sock` which is
overly permissive. Removed — the `docker` group membership from
`usermod -aG docker lohar` in the tier script is the standard Docker
access control mechanism.

Note: cgroups v2 mount and `/dev/shm` are handled by lohar
unconditionally (§3.2.1), so the Docker boot profile doesn't duplicate them.

### 4.6 Tests

**Tier build tests (CI, ubuntu-latest):**
- `TestMinimalTierBoots` — build minimal, boot VM, exec `whoami` → `lohar`
- `TestMinimalTierHasIproute` — exec `ip addr` → works
- `TestMinimalTierHasCurl` — exec `curl -V` → works
- `TestMinimalTierHasSudo` — exec `sudo whoami` → `root`
- `TestMinimalTierNoZsh` — exec `which zsh` → not found
- `TestMinimalTierSize` — rootfs < 200MB uncompressed

**Browser tier tests (agni-01, x86_64):**
- `TestBrowserTierChromiumStarts` — boot, exec
  `curl -s http://localhost:9222/json/version` → returns Chromium version
- `TestBrowserTierPlaywright` — boot, exec playwright script that
  navigates to example.com and returns title
- `TestBrowserTierSnapshotResume` — navigate to a page, snapshot, resume,
  verify Chromium is still running with the page loaded (CDP connection alive)

**Docker tier tests (agni-01, x86_64):**
- `TestDockerTierDaemonStarts` — boot, exec `docker version` → succeeds
- `TestDockerTierHelloWorld` — exec `docker run --rm hello-world` → works
  with bridge networking (not `--network host`)
- `TestDockerTierOverlayFS` — exec `docker info --format '{{.Driver}}'` → `overlay2`
- `TestDockerTierBuildAndRun` — write a Dockerfile, `docker build`, `docker run`
- `TestDockerTierComposeUp` — write docker-compose.yml, `docker compose up -d`,
  verify services running
- `TestDockerTierSnapshotResume` — start containers, snapshot, resume,
  verify containers still running

---

## Part 5 — Distribution

### 5.1 Artifacts per release

```
dist/
  # Binaries (existing)
  bhatti-darwin-arm64
  bhatti-darwin-amd64
  bhatti-linux-amd64
  bhatti-linux-arm64

  # Kernel (new)
  vmlinux-6.1.155-x86_64
  vmlinux-6.1.155-aarch64

  # Rootfs images (new, zstd-compressed)
  rootfs-minimal-amd64.ext4.zst
  rootfs-minimal-arm64.ext4.zst
  rootfs-browser-amd64.ext4.zst
  rootfs-browser-arm64.ext4.zst
  rootfs-docker-amd64.ext4.zst
  rootfs-docker-arm64.ext4.zst

  # Firecracker binary (new)
  firecracker-v1.14.0-x86_64
  firecracker-v1.14.0-aarch64
```

Total: 4 binaries + 2 kernels + 6 rootfs + 2 firecracker = 14 artifacts.
GitHub Releases limit is 2GB per file. Largest artifact is the docker
rootfs at ~800MB uncompressed, ~300MB with zstd. Well within budget.

### 5.2 Install script changes

**File:** `scripts/install.sh`

The install script changes from "build everything" to "download pre-built":

```bash
# Old (10+ min):
debootstrap ... && chroot ... && apt-get install ...

# New (30 seconds):
curl -fsSL "$RELEASE_URL/vmlinux-6.1.155-${ARCH}" -o "$KERNEL_PATH"
curl -fsSL "$RELEASE_URL/rootfs-${TIER}-${ARCH}.ext4.zst" | zstd -d > "$ROOTFS_PATH"
curl -fsSL "$RELEASE_URL/firecracker-v1.14.0-${ARCH}" -o /usr/local/bin/firecracker
```

Where `ARCH` is `x86_64` / `aarch64` for kernel/firecracker and `amd64` /
`arm64` for rootfs (matching the artifact naming conventions).

**Tier selection:**
```bash
sudo ./scripts/install.sh --systemd                    # minimal (default)
sudo ./scripts/install.sh --systemd --image browser    # browser tier
sudo ./scripts/install.sh --systemd --image docker     # docker tier
```

**Key changes to install.sh:**
- `FC_VERSION="1.6.0"` → `FC_VERSION="1.14.0"`
- Remove `FC_MAJOR_MINOR` (kernel is no longer downloaded from FC CI)
- Kernel download URL → GitHub release artifact
- Remove debootstrap / build-rootfs.sh path entirely
- Add `--image` flag for tier selection
- Add `zstd` to dependencies (for decompression)

**Fallback:** `build-tier.sh` remains for users who want to build from
source or customize tier scripts. The install script checks if rootfs
already exists before downloading.

### 5.3 CI workflow

**File:** `.github/workflows/build-images.yml`

```yaml
name: Build Images

on:
  workflow_dispatch:
  push:
    tags: ['v*']
    paths:
      - 'scripts/tiers/**'
      - 'scripts/build-tier.sh'
      - 'scripts/build-kernel.sh'

jobs:
  kernel:
    strategy:
      matrix:
        arch: [x86_64, aarch64]
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - name: Install build deps
        run: |
          sudo apt-get update
          sudo apt-get install -y build-essential flex bison libelf-dev libssl-dev bc
          ${{ matrix.arch == 'aarch64' && 'sudo apt-get install -y gcc-aarch64-linux-gnu' || '' }}
      - name: Build kernel
        run: ./scripts/build-kernel.sh ${{ matrix.arch }}
      - uses: actions/upload-artifact@v4
        with:
          name: kernel-${{ matrix.arch }}
          path: dist/vmlinux-*

  rootfs:
    strategy:
      matrix:
        tier: [minimal, browser, docker]
        arch: [amd64, arm64]
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version-file: go.mod }
      - name: Build lohar
        run: |
          GOARCH=${{ matrix.arch }} GOOS=linux CGO_ENABLED=0 \
            go build -ldflags="-s -w" -o lohar ./cmd/lohar/
      - name: Install debootstrap
        run: |
          sudo apt-get update
          sudo apt-get install -y debootstrap
          ${{ matrix.arch == 'arm64' && 'sudo apt-get install -y qemu-user-static' || '' }}
      - name: Build rootfs
        run: sudo ./scripts/build-tier.sh ${{ matrix.tier }} ${{ matrix.arch }} ./lohar
      - name: Compress
        run: zstd -19 dist/rootfs-${{ matrix.tier }}-${{ matrix.arch }}.ext4
      - uses: actions/upload-artifact@v4
        with:
          name: rootfs-${{ matrix.tier }}-${{ matrix.arch }}
          path: dist/rootfs-*.ext4.zst

  release:
    needs: [kernel, rootfs]
    if: startsWith(github.ref, 'refs/tags/')
    runs-on: ubuntu-latest
    steps:
      - uses: actions/download-artifact@v4
      - uses: softprops/action-gh-release@v2
        with:
          files: |
            kernel-*/*
            rootfs-*/*
```

**Cross-arch build notes:**
- **Kernel aarch64:** Built natively on x86_64 CI runner using
  `gcc-aarch64-linux-gnu` cross-compiler. Fast (~5 min).
- **Rootfs arm64:** Built on x86_64 CI runner using `qemu-user-static` +
  binfmt_misc. QEMU userspace emulation intercepts arm64 syscalls from
  the chroot. Slow (~20-30 min) but only runs on release tags.
- **On agni-01 (x86_64):** Native amd64 builds only. To build arm64
  rootfs locally, install `qemu-user-static` or use CI.
- **On Raspberry Pis (aarch64):** Can build arm64 rootfs natively. Cannot
  build amd64 rootfs (no x86 emulation path).

---

## Part 6 — Implementation Phases

### Phase 1: Firecracker upgrade + lohar changes

No rootfs rebuilds. No kernel build. No CI changes. Just upgrade
Firecracker, update lohar for boot profiles + shell fix + mounts.

1. **Migrate agni-01:** Destroy all sandboxes and snapshots (FC v1.6
   snapshot format is incompatible with v1.14).
2. **Upgrade Firecracker** on agni-01 from v1.6.0 to v1.14.0.
3. Add `/dev/shm` mount to lohar (§3.2.1).
4. Add cgroups v2 mount + subtree_control to lohar (§3.2.1).
5. Add boot profile support to lohar — after listeners, with 30s
   timeout (§3.2.2).
6. Fix `/bin/zsh` → `/bin/bash` in `engine.go` (§3.3).
7. Update `FC_VERSION` in `scripts/install.sh` to `1.14.0`.
8. Deploy updated lohar + bhatti to agni-01.
9. Verify: boot existing rootfs with new FC v1.14 + v1.15 CI kernel,
   all existing functionality works (exec, shell, files, volumes,
   snapshots).

### Phase 2: Minimal + browser tiers

Browser tier has no kernel dependency — it works with the stock v1.15
CI kernel. Ship it first.

1. Write `scripts/build-tier.sh` orchestrator.
2. Write `scripts/tiers/minimal.sh`.
3. Build minimal on agni-01, boot, test.
4. Write `scripts/tiers/browser.sh`.
5. Build browser on agni-01, boot, test Chromium + CDP + Playwright.
6. Test snapshot/resume for browser tier (the money feature — snapshot
   Chromium process memory, resume at exact page state).

### Phase 3: Custom kernel + docker tier

Now build the kernel and ship Docker.

1. Write `scripts/build-kernel.sh`.
2. **Audit CI config:** Run the grep command from §1.2 to check which of
   the 13 flags are already `=y`, `=m`, or unset. Record results in a
   comment in `build-kernel.sh`.
3. Build kernel on agni-01 (`./scripts/build-kernel.sh x86_64`, ~5 min).
4. Deploy custom kernel to agni-01, verify boot.
5. Write `scripts/tiers/docker.sh`.
6. Build docker tier on agni-01, boot, test `docker run --rm hello-world`
   with bridge networking.
7. Test `docker info --format '{{.Driver}}'` → `overlay2`.
8. Test snapshot/resume for docker tier.

### Phase 4: CI + distribution

Add CI workflows, update install script, cut release.

1. Add `.github/workflows/build-images.yml`.
2. Update `scripts/install.sh` to download pre-built artifacts.
3. Update `.github/workflows/release.yml` to include kernel + rootfs.
4. Test clean install from scratch on a fresh x86_64 machine.
5. Test clean install on a Raspberry Pi (arm64).
6. Tag v0.4.0.

### Dependency graph

```
Phase 1 (FC upgrade + lohar)
     ↓
Phase 2 (minimal + browser)  — needs Phase 1 lohar (boot profiles, /dev/shm)
     ↓                         does NOT need custom kernel
Phase 3 (kernel + docker)    — needs Phase 1 lohar + custom kernel
     ↓
Phase 4 (CI + distribution)  — needs Phase 2+3 tier scripts
```

Phase 2 and Phase 3 are independent after Phase 1. Browser tier can
ship to users while Docker kernel is still being built/tested.

---

## Resolved Questions

1. **Rootfs default size.** Minimal at 512MB, browser/docker at 2GB.
   Users can resize with `--disk-size`. 512MB for minimal is fine — it's
   the download that matters (zstd-compressed ~40MB), not on-disk size.

2. **Chromium source.** Use Playwright's bundled Chromium, not Ubuntu
   repos. Ubuntu's `chromium-browser` is a snap redirect in noble and
   broken in minbase chroots. Playwright bundles a known-good binary
   tested against its own API. (§2.2.1)

3. **Docker version pinning.** Don't pin. Use `docker-ce` (latest from
   Docker's apt repo). Docker is stable across minor versions. Pin only
   if a specific version causes a regression.

4. **Playwright version pinning.** Yes, pin. Playwright + Chromium
   coupling is fragile. Pin `playwright==1.50.0` (or current at time of
   implementation). Update per release, test combination in CI. (§2.2.2)

5. **arm64 rootfs build time.** QEMU userspace emulation on x86_64 CI
   runners takes 20-30 min. Acceptable for release builds (tagged only).
   Future optimization: self-hosted arm64 runner (Raspberry Pi) or
   `actions/cache` for the debootstrap tarball.

6. **Browser tier vs Playwright inside Docker.** Standalone browser tier.
   Cleaner snapshot semantics (one process vs dockerd+containerd+shim),
   no kernel dependency, ~200MB lighter, faster boot. See "Why not
   Playwright inside Docker?" section above.
