# Sandbox Guardrails — Defaults, Docs, and Don'ts

Issue [#12](https://github.com/sahil-shubham/bhatti/issues/12) (Fastidious):
user couldn't get sandboxes working with defaults, had to dig through docs
to find `--cpus` and `--memory`, and installing `openssh` completely broke
a VM. Three problems with a common root: **the system doesn't surface its
constraints early enough**.

---

## Current State

### 1. Memory defaults are documented wrong

The system has **three** different memory defaults depending on where you look:

| Source | Default Memory |
|--------|---------------|
| CLI `--memory` flag help | `0 = server default: 2048` |
| Server code (`sandbox_handlers.go:229`) | `2048` MB |
| CLI reference doc (`cli-reference.md`) | `512` MB |
| DB schema `DEFAULT` (`store.go:28`) | `512` MB |
| DB migration `DEFAULT` (`store.go:92`) | `512` MB |

The actual behavior: CLI sends `0` → server sees `0` → applies `2048` MB.
This is correct. But `cli-reference.md` says the default is `512`, which
is **wrong** and is almost certainly what the user read. 512 MB is below the
threshold where Ubuntu 24.04 + Node.js + Claude Code can function
comfortably — apt operations fail, npm OOMs, basic tools misbehave.

The DB schema `DEFAULT 512` is a red herring — it's the column default for
SQLite, used when recovering state from old rows or templates. It does NOT
control the create path. But it adds to the confusion if anyone reads the
source.

### 2. No concept of "things that will break your VM"

Bhatti VMs are not standard Linux boxes. They have a fundamental
architectural constraint that users don't know about:

> **Lohar is PID 1, not systemd.**

The kernel boots with `init=/usr/local/bin/lohar`. There is no systemd,
no init scripts, no service manager. This is a deliberate design decision
documented in `decisions.md` (Decision #3) but **never communicated to
users**. The implications are severe for anyone who treats the VM like a
normal Ubuntu installation:

- `systemctl` commands do nothing (no systemd running)
- Package postinst scripts that call `systemctl start <service>` fail
- Packages that depend on systemd subsystems (journald, resolved,
  networkd, logind) install but don't work
- There is no service supervision — daemons started manually don't
  restart on crash

### 3. Why `openssh` completely breaks the VM

Installing `openssh-server` (or the `openssh` metapackage) on Ubuntu 24.04
triggers a cascade of failures:

**Stage 1 — Dependency pull.**
`openssh-server` depends on `libpam-systemd` → `systemd` → `systemd-resolved`
→ `systemd-sysv`. This pulls ~40MB of systemd packages into the rootfs.

**Stage 2 — resolv.conf destruction.**
`systemd-resolved`'s postinst script replaces `/etc/resolv.conf` with a
symlink to `/run/systemd/resolve/stub-resolv.conf`. But `systemd-resolved`
is not running (lohar is PID 1, not systemd), so that target file does not
exist. DNS resolution immediately breaks.

This is fatal: lohar writes a static `/etc/resolv.conf` at boot
(`main.go:210-213`), and the `ensureResolvConf()` function even explicitly
removes broken symlinks — but this only runs at boot time. A mid-session
`apt-get install openssh-server` replaces the working file with a dead
symlink *after* boot, and nothing repairs it.

**Stage 3 — Disk full (minimal tier).**
The minimal tier rootfs is 512 MB. After debootstrap + base packages, ~200 MB
is used (~300 MB free). `openssh-server` + its systemd dependency tree adds
~50-80 MB of packages. This alone doesn't fill the disk, but if the user ran
other installs first, or if apt's package cache isn't cleaned, the rootfs can
fill up. When ext4 hits 100% usage, writes fail, and the VM becomes
unrecoverable (agent can't write to `/tmp`, commands fail with ENOSPC).

**Stage 4 — sshd startup failure.**
The `openssh-server` postinst script runs `systemctl start ssh`. This fails
(no systemd), but it's a non-fatal error in the dpkg script. The real damage
is already done by Stage 2.

**Net result:** After `apt-get install openssh-server`, DNS is broken. The
user can't install anything else, can't `curl`, can't `wget`, can't do any
network operation. The VM appears "completely broken."

**Why this is not a bug but an architecture constraint:** lohar-as-PID-1 is
the reason bhatti boots in 3.5 seconds instead of 6-8 seconds, has
deterministic startup, and uses minimal memory. It's a fundamental design
choice. But users need to know about it.

---

## Analysis: What UX Improvements Would Prevent This

### A. Fix the documentation lie (urgent, zero-effort)

`cli-reference.md` says `--memory` defaults to `512`. The actual default
is `2048`. This is the most likely cause of the user's "nothing I was
creating was working" complaint — they read the docs, assumed 512 MB was
the default, and either used it explicitly or were confused by the
inconsistency.

**Fix:** Update cli-reference.md to say `2048`, matching the server code
and the CLI flag help text.

### B. Show effective defaults in `create` output

When a user runs `bhatti create --name dev`, they get back:

```
sb_a1b2c3    dev    192.168.137.2
```

No indication of how much CPU or memory was allocated. The user has to
`bhatti inspect dev` or read docs to know. Compare to Docker:

```
$ docker run -d ubuntu
# → docker stats shows CPU/MEM immediately
```

**Improvement:** Print the effective resource allocation:

```
sb_a1b2c3    dev    192.168.137.2    1 vCPU    2048 MB
```

Or at minimum, when no `--cpus` or `--memory` flags are given, print a
hint: `(1 vCPU, 2048 MB — use --cpus/--memory to change)`.

### C. Add a "Known Limitations" or "Don'ts" doc

The project needs a page that surfaces constraints early. Something
users can find before they brick a VM. This would cover:

1. **No systemd** — lohar is PID 1. Packages that depend on systemd
   services (openssh-server, nginx via apt, postgresql, docker — the
   docker *tier* works because it starts dockerd in the boot profile,
   but `apt-get install docker.io` does not) will install but their
   services won't start.

2. **DNS fragility** — `/etc/resolv.conf` is a plain file, not managed
   by resolved. Anything that replaces it with a symlink (systemd,
   resolvconf, NetworkManager) breaks DNS.

3. **Fixed disk size** — The rootfs is the size of the tier image (512 MB
   minimal, 2 GB browser/docker, 4 GB computer) unless `--disk-size` is
   passed. There is no auto-grow. Heavy package installs on `minimal`
   will hit ENOSPC.

4. **No GPU** — Firecracker has no GPU passthrough. CUDA packages install
   but don't work.

5. **No systemd service supervision** — If you start a daemon, it won't
   restart on crash. Use the `--init` flag or the init session instead.

6. **No X11/Wayland** (except computer tier) — graphical packages need
   the computer tier's Xvfb/KasmVNC setup.

### D. Warn at package-install time (aspirational, harder)

The nuclear option: have lohar intercept or wrap `apt-get install` and
warn about known-dangerous packages before they install. This is complex
and fragile, but even a simpler version could help:

**Simple version:** Ship a `/etc/apt/apt.conf.d/99bhatti-warn` that uses
apt's `DPkg::Pre-Install-Pkgs` hook to check if `systemd-resolved` or
`systemd-sysv` is in the install set, and prints a warning.

**Simpler version:** Add a `~/.bashrc` / `~/.zshrc` wrapper that catches
`apt install openssh*` patterns and prints a warning. Ugly but effective
for the common case.

**Simplest version:** Just document it (option C above) and trust users
to read docs. This is the realistic first step.

### E. Make resolv.conf resilient

Lohar already handles broken resolv.conf at boot (`ensureResolvConf()`
removes symlinks and writes a static file). But this only runs once.

**Improvement:** Use `chattr +i /etc/resolv.conf` (immutable attribute)
after writing it at boot. This prevents any package postinst script from
replacing or symlinking it. The user can still override with
`sudo chattr -i /etc/resolv.conf && sudo nano /etc/resolv.conf`, but
accidental destruction via package installation is prevented.

This is a one-line addition to `ensureResolvConf()` or the boot sequence
and would have prevented the openssh breakage entirely.

Alternatively, a watchdog goroutine in lohar that periodically checks if
`/etc/resolv.conf` is still a regular file with valid nameservers, and
repairs it if not. More complex, but handles edge cases where `chattr`
isn't available or the immutable flag is cleared.

### F. Default disk size should be larger for non-minimal tiers

The `--disk-size` flag exists but defaults to 0 (use image size). For the
minimal tier at 512 MB, this is extremely tight. Users who don't know
about `--disk-size` will hit ENOSPC after a few `apt-get install` commands.

**Options:**
- Auto-resize the rootfs to a comfortable default (e.g., 2 GB for all
  tiers) unless the user explicitly passes `--disk-size`
- Print the available disk space in `bhatti inspect` output
- Warn in the `create` output when the rootfs is small:
  `⚠ disk: 512 MB (use --disk-size to increase)`

### G. `bhatti create` should show what happened

Currently `create` returns a one-line table. Many CLI tools (Docker,
Kubernetes, Terraform) show a summary of what was provisioned:

```
Created sandbox "dev"
  ID:       sb_a1b2c3
  IP:       192.168.137.2
  vCPUs:    1
  Memory:   2048 MB
  Disk:     512 MB (minimal tier)
  Image:    rootfs-minimal-arm64

  shell:    bhatti shell dev
  exec:     bhatti exec dev -- <command>
```

This tells the user exactly what they got, whether it's enough for their
use case, and what to do next. First-time users especially benefit from
the `shell:` / `exec:` hints.

---

## Recommendations (prioritized)

| Priority | Item | Effort | Impact |
|----------|------|--------|--------|
| **P0** | Fix cli-reference.md memory default (512→2048) | 5 min | Eliminates the #1 confusion from issue #12 |
| **P0** | Make resolv.conf immutable at boot (`chattr +i`) | 10 min | Prevents openssh and all systemd packages from breaking DNS |
| **P1** | Add `docs/limitations.md` (known don'ts) | 1 hr | Sets expectations before users hit walls |
| **P1** | Show vCPU/memory/disk in `create` output | 30 min | Users know what they got without inspecting |
| **P2** | Richer `create` output with next-step hints | 1 hr | Better first-run experience |
| **P2** | Auto-resize rootfs to 2 GB when `--disk-size` not set | 30 min | Prevents ENOSPC on minimal tier |
| **P3** | `apt.conf.d` hook to warn about systemd packages | 2 hr | Proactive guard, fragile to maintain |

---

## Deep Dive: Custom PID 1 vs systemd vs Alternatives

The question isn't just "should we add systemd" — it's "what init
model gives us the right balance of control, user experience, and
maintainability." This section walks through concrete user scenarios
under each model, examines what control lohar-as-PID-1 actually
exercises, surveys alternatives, and asks whether an init system is
even necessary.

### What lohar actually does as PID 1 (all 28ms of it)

Lohar's entire PID 1 init path is 9 functions, ~120 lines of Go:

```
main():
  1. Mount proc, sysfs, devtmpfs, devpts, tmpfs ×3, /dev/shm, cgroup2
  2. chmod /dev/fuse 0666
  3. Enable cgroup controllers (+cpu +memory +io +pids)
  4. Bring up loopback (lo) via raw ioctl
  5. Load config drive (/dev/vdb) → hostname, DNS, env, files, volumes
  6. Set up eth0 from kernel ip= parameter (if kernel didn't already)
  7. Install signal handlers (SIGTERM → sync → poweroff)
  8. Listen on vsock + TCP ports 1024/1025
  9. Run /etc/bhatti/init.sh boot profile (if present)
  10. Run --init script as TTY session (if configured)
  11. Block forever (select{})

installSignalHandlers():
  SIGTERM/SIGINT → syscall.Sync() → syscall.Reboot(POWER_OFF)
  SIGCHLD: NOT handled. Orphan zombies accumulate.
```

Steps 1-3 are filesystem mounts that every init system does.
Step 4 is one ioctl call.
Step 5 is config injection (bhatti-specific, ~60 lines).
Step 6 is network setup that the kernel already handles via `ip=`.
Steps 7-11 are agent duties, not init duties.

**What lohar does NOT do as PID 1:**
- Reap orphan zombies (explicitly documented as skipped — Go's runtime
  races with `Wait4(-1)`)
- Manage services (no restart, no health check, no dependency ordering)
- Handle device hotplug (no udev equivalent)
- Manage cgroup hierarchies (Docker tier does this manually)
- Rotate logs (nothing manages /var/log)
- Clean temporary files (no tmpfiles.d equivalent)

The zombie issue is worth flagging. From `main.go:334`:
```go
// Note: we do NOT install a SIGCHLD handler. Go's runtime manages
// SIGCHLD for processes started via exec.Command. A manual Wait4(-1)
// reaper would race with cmd.Wait() and corrupt exit codes.
// Orphan zombies (from grandchild processes) are acceptable for now.
```

If a user runs `npm install` (which spawns dozens of child processes)
and some of those grandchildren get orphaned, they become zombies that
persist until the VM is destroyed. This doesn't cause functional
problems (zombie processes consume only a PID table entry), but `ps`
shows growing zombie counts over time. systemd, runit, s6 — every
real init system — reaps zombies automatically.

### What "control" do we actually exercise?

The argument for lohar-as-PID-1 is control. Let's audit what control
we exercise and whether it matters:

| Control | How lohar uses it | Would we lose it with systemd? |
|---------|-------------------|-------------------------------|
| Mount ordering | Mount in hardcoded order | No — systemd does the same mounts, in a tested order |
| Network config | Parse kernel ip= and configure if needed | No — kernel ip= works before ANY init |
| Config drive | Read /dev/vdb, apply, unmount | No — ExecStartPre in lohar.service, identical code |
| DNS | Write static /etc/resolv.conf | Partially — resolved would manage it (but more robustly) |
| Agent token | Unmount config drive after reading | Same — ExecStartPre unmounts it |
| Agent uptime | Can't be killed (IS PID 1) | `RefuseManualStop=yes` in unit file. But if lohar crashes, systemd RESTARTS it — actually more resilient |
| Boot profile | Run /etc/bhatti/init.sh as root | Same — `ExecStartPre` or a separate oneshot unit |
| Shutdown | SIGTERM → sync → poweroff | `ExecStop=sync; poweroff` or let systemd handle it natively |

**The DNS point is the only one where we have more control with lohar.**
We write a static resolv.conf and nothing interferes. With systemd,
resolved takes over DNS management. But resolved is actually better —
it handles DNS fallback, caching, and DNSSEC. The issue from #12
(openssh breaking DNS) exists precisely because we DON'T use resolved.

**The agent resilience point actually favors systemd.** If lohar
crashes as PID 1, the kernel panics — the VM is dead. If lohar crashes
as a systemd service with `Restart=always`, systemd restarts it in 1
second and the VM keeps running. We've never had a lohar crash in
production, but the failure mode is strictly worse with lohar-as-PID-1.

### User experience comparison: concrete scenarios

#### Scenario 1: Fresh sandbox, install packages, run code

**With lohar-as-PID-1 (current):**
```bash
bhatti exec dev -- sudo apt-get install -y python3 python3-pip
# ✅ Works (no systemd deps)

bhatti exec dev -- sudo apt-get install -y openssh-server
# ❌ Installs systemd-resolved → DNS breaks → VM bricked

bhatti exec dev -- sudo apt-get install -y postgresql
# ⚠️ Installs but postgresql doesn't start (postinst calls systemctl)
# User must manually: sudo -u postgres pg_ctlcluster 16 main start

bhatti exec dev -- sudo apt-get install -y nginx
# ⚠️ Installs but nginx doesn't start
# User must manually: sudo nginx

bhatti exec dev -- sudo apt-get install -y redis-server
# ⚠️ Installs but redis doesn't start
# User must manually: sudo redis-server --daemonize yes
```

**With systemd:**
```bash
bhatti exec dev -- sudo apt-get install -y openssh-server
# ✅ Installs, resolved manages DNS, sshd starts automatically

bhatti exec dev -- sudo apt-get install -y postgresql
# ✅ Installs, systemctl starts postgresql, pg_isready works

bhatti exec dev -- sudo apt-get install -y nginx
# ✅ Installs, systemctl starts nginx, curl localhost works

bhatti exec dev -- sudo apt-get install -y redis-server
# ✅ Installs, systemctl starts redis, redis-cli ping works
```

**Every Ubuntu package that ships a service assumes systemd.** The
postinst scripts call `systemctl start`, `systemctl enable`,
`systemctl daemon-reload`. Without systemd, they silently fail. The
package installs but the service doesn't run, leaving users confused.

#### Scenario 2: Running a web server (the `--init` pattern)

**With lohar-as-PID-1:**
```bash
bhatti create --name api --keep-hot --init 'cd /workspace && node server.js'
# ✅ Works for the happy path

# But:
# - If server.js crashes, it stays dead. No restart.
# - If you want to add redis alongside it, destroy and recreate:
bhatti create --name api --keep-hot --init '
  redis-server --daemonize yes
  until redis-cli ping 2>/dev/null; do sleep 0.1; done
  cd /workspace && node server.js
'
# - No way to check if redis crashed later
# - No log rotation for redis or node
# - Can't add a service after creation without destroying
```

**With systemd:**
```bash
bhatti create --name api --keep-hot
bhatti exec api -- sudo apt-get install -y redis-server
# Redis starts automatically, restarts on crash

# Create a systemd service for the app:
bhatti exec api -- sudo tee /etc/systemd/system/myapp.service <<'EOF'
[Unit]
Description=My App
After=redis.service

[Service]
Type=simple
User=lohar
WorkingDirectory=/workspace
ExecStart=/usr/local/bin/node server.js
Restart=always
RestartSec=1

[Install]
WantedBy=multi-user.target
EOF

bhatti exec api -- sudo systemctl enable --now myapp
# Node starts, restarts on crash, starts after redis
# journalctl -u myapp shows logs
# systemctl status myapp shows health
```

The systemd version is more commands upfront but gives: crash
recovery, dependency ordering, log management, health visibility, and
the ability to add services without recreating the sandbox.

#### Scenario 3: Docker tier (our own use case)

**Current (lohar-as-PID-1):**
```bash
# /etc/bhatti/init.sh in docker tier:
dockerd > /var/log/dockerd.log 2>&1 &
for i in $(seq 1 100); do
    [ -S /var/run/docker.sock ] && break
    sleep 0.1
done
chmod 666 /var/run/docker.sock  # hack: group membership doesn't work
```

Problems:
- dockerd crashes → stays dead, no restart
- `chmod 666` on docker.sock is a security workaround
- Logs go to a file nobody rotates
- No readiness notification — just polling in a loop

**With systemd:**
```ini
# Docker's own systemd unit (ships with docker-ce package):
[Service]
Type=notify                    # Docker signals readiness via sd_notify
ExecStart=/usr/bin/dockerd
Restart=always
RestartSec=2
```

Docker's unit file already handles everything: readiness notification
(no polling loop), automatic restart, proper logging via journald,
group-based socket permissions. We'd delete 15 lines of shell script
and get better behavior.

#### Scenario 4: Computer tier (most complex)

**Current init.sh (33 lines of fragile shell):**
```bash
Xkasmvnc :99 -geometry 1280x720 -websocketPort 6080 ... &
for i in $(seq 1 30); do xdpyinfo && break; sleep 0.1; done
dbus-daemon --system --fork
pulseaudio --start
startxfce4 &
echo "DISPLAY=:99" > /run/bhatti/env
```

Problems:
- KasmVNC crashes → no desktop until recreate
- No dependency ordering (XFCE before X is ready = race)
- dbus-daemon can fail silently
- No health monitoring for any of the 5 daemons

**With systemd:** Each component becomes a unit with proper
dependencies, restart policies, and readiness checks. The boot profile
disappears entirely.

#### Scenario 5: Snapshot/restore with running services

**With lohar-as-PID-1:**
```
VM running: node server.js on port 3000, redis on 6379
  → thermal manager snapshots to disk (cold)
  → user runs: bhatti exec dev -- curl localhost:3000
  → VM restored from snapshot
  → node and redis resume exactly where they were (memory snapshot)
  → curl works immediately
```

This is bhatti's killer feature and it works beautifully with
lohar-as-PID-1. Processes survive.

**With systemd:**
```
VM running: same setup, systemd managing both services
  → thermal manager snapshots to disk (cold)
  → user runs: bhatti exec dev -- curl localhost:3000
  → VM restored from snapshot
  → node and redis resume (same as above — memory snapshot)
  → systemd also resumes, sees a clock jump
  → systemd timers fire (logrotate, tmpfiles cleanup)
  → possible: watchdog timers trigger service restart
  → services restart cleanly (Restart=always)
  → curl works after brief restart (~1-2s)
```

The systemd case has a wrinkle: service watchdogs might trigger
unnecessary restarts after a time jump. This is fixable
(`RuntimeWatchdogSec=0` for our services) but needs testing.

**However:** The current lohar-as-PID-1 snapshot/restore is also not
perfect. If a process was mid-write when snapshotted, it continues
mid-write on restore. If a TCP connection was open, the remote end
may have closed it during the cold period — the restored process
gets a broken pipe. These are fundamental to memory snapshots, not
to the init system.

### The daemon problem (restated)

This is the deeper concern beneath issue #12. Bhatti VMs deliberately
have no systemd, but real users need to run daemons — web servers,
databases, background workers. The current story has gaps.

### What Exists Today

**Tier boot profiles** (`/etc/bhatti/init.sh`) — baked into the rootfs
at image build time, run by lohar at boot. This is how our own tiers
solve it:

```sh
# Docker tier: start dockerd, poll for socket, chmod
dockerd > /var/log/dockerd.log 2>&1 &
for i in $(seq 1 100); do
    [ -S /var/run/docker.sock ] && break; sleep 0.1
done

# Browser tier: start headless Chrome, poll for CDP
"$HEADLESS_SHELL" --remote-debugging-port=9222 &
for i in $(seq 1 50); do
    curl -sf http://127.0.0.1:9222/json/version && break; sleep 0.1
done

# Computer tier: start KasmVNC + XFCE + dbus + pulseaudio
Xkasmvnc :99 -geometry 1280x720 &
dbus-daemon --system --fork
pulseaudio --start
startxfce4 &
```

Pattern: background with `&`, readiness poll, move on. It works because
we write it and test it.

**`--init` flag** — user-specified init script. Runs as an attachable TTY
session with ID `"init"`. Users can `bhatti shell dev` → attach to the
init session to see output.

```bash
bhatti create --name api --init "cd /workspace && node server.js"
bhatti create --name agent --init "hermes gateway" --keep-hot
```

**`--keep-hot`** — prevents thermal transitions (pause/snapshot) for
sandboxes running long-lived processes. Without this, the thermal manager
would pause the VM after 30s of no API activity, killing the daemon's
connections.

### What's Missing

**No restart-on-crash.** If `node server.js` segfaults, it's dead. The
boot profiles have the same problem — if `dockerd` crashes in the docker
tier, it stays dead until the VM is destroyed and recreated.

**No multi-service orchestration.** If a user needs postgres + redis +
their app server, they have to write a shell script that backgrounds all
three, polls all three, and hopes nothing crashes. No dependency ordering.

**No daemon health visibility.** `bhatti ps dev` shows TTY sessions, not
daemon processes. There's no way to ask "is my postgres still running?"
without `bhatti exec dev -- pgrep postgres`.

**No documented pattern.** The `--init` flag docs in cli-reference.md say:

```
| `--init` | — | Init script (runs as attachable TTY session "init") |
```

That's the entire guidance. No examples of running daemons, no pattern
for multi-service, no mention that there's no supervision. A user who
wants to run a web server has to figure out the `&` + readiness poll
pattern by reading our tier scripts.

**No way to add daemons after create.** The boot profile runs once at
VM boot. If a user installs postgres after creation (via `bhatti exec`),
there's no way to register it as a managed service. They'd have to
destroy the sandbox, create a custom image, and start over.

### Options (lightest → heaviest)

#### Option 1: Document the pattern (zero code)

Explain the `--init` + `&` + readiness poll pattern in a "Running
Services" guide. Show concrete examples:

```bash
# Single daemon
bhatti create --name api --keep-hot --init '
  cd /workspace && node server.js
'

# Multiple daemons
bhatti create --name stack --keep-hot --init '
  postgres -D /var/lib/postgresql/data &
  redis-server --daemonize yes
  # Wait for deps
  until pg_isready; do sleep 0.5; done
  cd /workspace && node server.js
'
```

Acknowledge the limitation: no restart-on-crash. Suggest `while true; do
$cmd; sleep 1; done` as a manual workaround.

**Pros:** Zero effort, sets correct expectations.
**Cons:** Doesn't actually fix anything. Users still have no supervision.

#### Option 2: Ship a `supervise` wrapper in the rootfs (30 min)

Add a tiny shell script to all tiers:

```sh
#!/bin/sh
# /usr/local/bin/supervise — restart a command on crash
while true; do
    "$@"
    echo "supervise: $1 exited ($?), restarting in 1s..." >&2
    sleep 1
done
```

Usage:
```bash
bhatti create --name api --keep-hot --init '
  supervise node server.js
'

# Multiple supervised daemons
bhatti create --name stack --keep-hot --init '
  supervise postgres -D /var/lib/postgresql/data &
  supervise redis-server &
  until pg_isready; do sleep 0.5; done
  supervise node server.js
'
```

**Pros:** Tiny, obvious, no dependencies, covers the 80% case.
**Cons:** No health checks, no backoff, no log routing. `supervise` is
just a restart loop — a process that starts and immediately crashes will
spin forever.

#### Option 3: Ship a real lightweight supervisor (2–4 hrs)

Include a proper process supervisor in the rootfs. Candidates:

| Tool | Size | Language | Restart | Health | Deps |
|------|------|----------|---------|--------|------|
| **s6** | ~200KB | C | ✓ | ✓ | musl-static |
| **runit** | ~120KB | C | ✓ | ✗ | libc |
| **supervisord** | ~15MB | Python | ✓ | ✓ | Python |
| **dinit** | ~300KB | C++ | ✓ | ✓ | libstdc++ |

**s6** is the best fit — it's what container-native init systems (s6-overlay
in Docker) use. Static binary, tiny footprint, dependency ordering,
automatic restart with configurable rate limiting, readiness notification
protocol. It's designed for exactly the "not systemd but need supervision"
niche.

But it adds complexity: users need to understand s6's service directory
structure (`/run/service/<name>/run`), and our boot profile would need to
integrate with it.

**Pros:** Real supervision with restart, backoff, dependency ordering.
**Cons:** Adds a tool users need to learn. Binary size in rootfs.
Minimal tier (512 MB) gets tighter.

#### Option 4: Build supervision into lohar (1–2 weeks)

Extend the agent protocol with a `SERVICE_START` / `SERVICE_STOP` /
`SERVICE_STATUS` command. Lohar manages named services with restart
policies, health checks, and log capture.

```bash
bhatti service add dev --name postgres --cmd "postgres -D /data" --restart always
bhatti service add dev --name redis --cmd "redis-server" --restart on-failure
bhatti service add dev --name app --cmd "node server.js" \
    --restart always --depends-on postgres,redis
bhatti service list dev
bhatti service logs dev postgres
```

Lohar would maintain a service table in memory, restart crashed processes
with exponential backoff, and expose status via the control protocol.

**Pros:** Fully integrated, first-class CLI experience, survives
snapshot/restore (service state is lohar's responsibility). Users never
think about supervision — it's part of the platform.
**Cons:** Significant scope. Adds protocol complexity. Service state
needs to survive snapshot/restore (serialize to disk before snapshot,
restore on resume). Overlap with the session model creates conceptual
ambiguity (is a service a session? a different thing?).

### Recommendation

**Now: Option 1 + Option 2.** Document the pattern properly and ship the
trivial `supervise` wrapper. This unblocks users immediately and gives us
a clear story:

> *"Bhatti sandboxes don't have systemd. Use `--init` with `supervise` to
> run daemons. For complex multi-service setups, write a shell init
> script that backgrounds each daemon — the same pattern our Docker and
> computer tiers use internally."*

**Later (if demand): Option 4.** The lohar-native service model is the
right long-term answer, but only if users are actually hitting the
limitations of the `supervise` wrapper. Building it prematurely risks
designing the wrong abstraction.

**Skip: Option 3.** Shipping s6/runit adds a third-party tool that we
have to maintain, document, and support. If we're going to invest in
real supervision, it should be integrated into lohar (Option 4), not
bolted on as an external binary.

### Alternatives survey: what exists between lohar and systemd

The init system landscape, ordered from lightest to heaviest:

#### tini / dumb-init (~20-50 KB)
Minimal PID 1 for containers. Reaps zombies, forwards signals. Nothing
else. Used by Docker's `--init` flag.

**What it solves for bhatti:** Zombie reaping (lohar's acknowledged gap).
**What it doesn't solve:** Service management, package compatibility,
restart-on-crash, logging. Packages that call `systemctl` still fail.

**Verdict:** Solves one minor problem. Not worth the complexity.

#### BusyBox init (~1 MB, part of busybox)
`/etc/inittab`-based. Spawns/respawns processes based on a config file.
No dependency ordering, no readiness notification, no logging.

```
::sysinit:/etc/init.d/rcS
::respawn:/usr/local/bin/lohar
::respawn:/usr/bin/dockerd
::shutdown:/bin/sync
```

**What it solves:** Restart-on-crash for declared services. Zombie
reaping.
**What it doesn't solve:** Package compatibility (`apt-get install
postgresql` still doesn't start). No `systemctl`. No dependency
ordering. No logging.

**Verdict:** Marginal improvement. Doesn't solve the user-facing problem.

#### runit (~120 KB, used by Void Linux)
Directory-based supervision. Each service is a directory in
`/etc/sv/<name>/` with a `run` script. `sv start/stop/restart`
commands. Automatic restart on crash.

```bash
# /etc/sv/dockerd/run
#!/bin/sh
exec dockerd 2>&1

# /etc/sv/lohar/run
#!/bin/sh
exec /usr/local/bin/lohar --agent
```

**What it solves:** Restart-on-crash, clean process supervision, zombie
reaping, per-service logging (via svlogd).
**What it doesn't solve:** Package compatibility. `apt-get install
postgresql` creates systemd units, not runit service directories.
User must manually create `/etc/sv/postgresql/run`. No `systemctl`.

**Verdict:** Good supervision, but packages still don't work out of
the box. Users trade "figure out `--init`" for "figure out runit."

#### s6 (~200 KB, used by s6-overlay in Docker)
Laurent Bercot's supervision suite. Conceptually similar to runit but
with readiness notification, dependency ordering, and a proper init
stage. s6-overlay is the standard for "PID 1 in containers that need
services."

```
/etc/s6-overlay/s6-rc.d/
  lohar/
    type: longrun
    run: exec /usr/local/bin/lohar --agent
    dependencies.d/
      base
  dockerd/
    type: longrun
    run: exec dockerd
    dependencies.d/
      lohar
```

**What it solves:** Everything runit does, plus dependency ordering,
readiness notification, proper init sequencing.
**What it doesn't solve:** Package compatibility. Same problem as
runit — Ubuntu packages ship systemd units, not s6 service
directories.

**Verdict:** The best non-systemd supervisor. But the package
compatibility problem remains. And it's another tool users must learn.

#### dinit (~300 KB, C++)
Modern, dependency-based init system. Closer to systemd's service
model (unit-file-like syntax) but minimal. Supports readiness
notification, dependency ordering, socket activation.

**What it solves:** Similar to s6.
**What it doesn't solve:** Package compatibility.

**Verdict:** Less proven than s6, similar trade-offs.

#### OpenRC (~2 MB, used by Alpine/Gentoo)
Shell-script-based init system. Each service is a shell script in
`/etc/init.d/`. Dependency ordering via `depend()` functions.
`rc-service start/stop` commands.

**What it solves:** Service management with a familiar model.
**What it doesn't solve:** Package compatibility. Ubuntu packages
don't ship OpenRC scripts. Alpine packages do, but we use Ubuntu.

**Verdict:** Wrong ecosystem. Only works if we switch to Alpine rootfs.

#### systemd (~15 MB resident, used by Ubuntu/Debian/Fedora/RHEL/SUSE)
Full init system + service manager + logging + DNS + device management
+ timer scheduling + temporary file management + ...

**What it solves:** Everything. Packages just work. Service management
just works. Logging just works. DNS just works.
**What it costs:** Boot time (+50-100ms stripped, +150-250ms with
journald/resolved). Memory (+12-45 MB). Snapshot/restore needs testing.

**Verdict:** The only option that solves the package compatibility
problem, which is the actual user-facing issue from #12.

### The package compatibility problem is the crux

Every alternative except systemd shares the same fatal flaw:
**Ubuntu packages ship systemd units, and their postinst scripts call
`systemctl`.** This isn't fixable with a shim. postinst scripts do:

```bash
systemctl daemon-reload
systemctl enable postgresql
systemctl start postgresql
systemctl is-active postgresql
systemctl show -p LoadState postgresql
```

A `systemctl` shim that translates these to runit/s6/dinit commands
would need to:
1. Parse systemd unit files (which have a complex syntax with
   conditionals, dependencies, resource limits, namespaces)
2. Translate them to the target init system's format
3. Handle `daemon-reload`, `is-active`, `show`, `cat`, `edit`
4. Handle socket activation, notify readiness, watchdog timers

This is a multi-month project that would be perpetually incomplete.
`systemd-shim` and `elogind` exist for parts of this (logind
compatibility) but nothing covers the full `systemctl` surface.

**If we want packages to work, we need systemd. There is no shortcut.**

If we DON'T care about packages working (only AI agent workloads that
never `apt-get install`), then lohar-as-PID-1 is perfect. The question
is which user base matters more.

### Does one even need an init system?

Let's ask the inverse question. What if we kept lohar as PID 1 but
made it a better PID 1?

**Minimum viable improvements to lohar-as-PID-1:**
1. Add zombie reaping (fixable — reap in a goroutine with careful
   Wait4 handling that doesn't race with exec.Command)
2. Add the `supervise` wrapper for crash recovery
3. Document the `--init` pattern thoroughly
4. Make resolv.conf immutable to prevent openssh breakage
5. Accept that packages requiring systemd won't work, document it

This path is defensible IF bhatti's primary users are AI agents and
CI pipelines that never install packages interactively. But issue #12
is from a human user who expected `apt-get install openssh-server` to
work. The question is: is that user representative?

**Agents DO apt-get install.** Claude Code, Codex, Devin, and every
coding agent routinely installs packages inside sandboxes:
`apt-get install -y ripgrep fd-find python3-pip postgresql-client`.
Some install services too: `apt-get install -y redis-server` to get a
local cache, `apt-get install -y openssh-server` to set up key-based
access between sandboxes. The lack of systemd creates gotchas that
we haven't hit in volume only because we haven't had volume yet. As
more users onboard, these failures will become support tickets.

**Judgment call:** bhatti is marketed as "isolated Linux environments"
that feel like real VMs. Real VMs have init systems. Real VMs let you
install packages. The lohar-as-PID-1 model creates a constant stream
of "why doesn't X work" moments for any user who treats the sandbox
like the Ubuntu VM it appears to be.

### Revisiting decisions.md with current data

The original decision (#3) was made with these assumptions:

> "Boot to agent-ready: ~3.5 seconds"

This was before the ARP pre-populate optimization. Current measured
boot: **365ms** (p50, Pi 5). The 3.5s figure is 10x stale.

> "Systemd adds 1-2 seconds to boot time"

This was never measured in a Firecracker VM. Firecracker's own CI
shows **231ms systemd userspace** with a full Ubuntu rootfs. A stripped
config (only lohar.service) would be **75-130ms**. The delta over
lohar's 28ms init is **~50-100ms**, not 1-2 seconds.

> "Zero services to manage or debug"

True, but the flip side is: zero services supervised, zero crash
recovery, zero logging. Our own Docker tier manages 1 daemon with
a shell script. The computer tier manages 5 daemons with a shell
script. "Zero services" is aspirational, not actual.

> "Zombie reaping intentionally omitted"

Still true and still a known gap. Every alternative init system
(systemd, runit, s6, busybox init, tini) handles this. We are the
only PID 1 implementation in production that accumulates zombies.

The decision made sense when boot was 3.5s and systemd would add
1-2s (a 30-60% regression). With boot at 365ms and systemd adding
~75ms (a 20% regression), the calculus has changed.

---

## Revisiting the Premise: Should We Just Add systemd?

The supervision gap exists because we chose lohar-as-PID-1 over systemd
(Decision #3 in `decisions.md`). The rationale was boot speed and
determinism. But every solution to the supervision gap — `supervise`
wrapper, s6, lohar-native services — is a worse reimplementation of
what systemd already does. And systemd is *already on the rootfs*
(debootstrap includes it). We pay the disk cost and get zero benefit.

### What lohar does as PID 1 (the init duties)

```
1. Mount proc, sysfs, devtmpfs, devpts, tmpfs, /dev/shm, cgroup2
2. Bring up loopback interface
3. Read config drive → apply hostname, DNS, env, files, volumes
4. Set up eth0 from kernel ip= parameter
5. Listen on vsock + TCP (agent protocol)
6. Run /etc/bhatti/init.sh boot profile
7. Run --init script as attachable session
8. Block forever (PID 1 must not exit)
```

Steps 1, 2, and 8 are things systemd does natively. Step 4 is handled
by the kernel's `ip=` parameter before init even runs. Steps 3, 6, 7
are bhatti-specific — but they can be systemd services.

The only thing that REQUIRES lohar to be PID 1 is... nothing. The agent
duties (step 5: listen, exec, sessions, files, port forwarding) are
completely independent of being PID 1.

### What systemd would actually cost (researched, not guessed)

The claim in `decisions.md` — "Systemd adds 1-2 seconds to boot time" —
was an estimate, not a measurement. It assumed a full Ubuntu boot with
all services. Here's what the data actually shows.

#### Boot time: the real numbers

**Firecracker's own CI benchmarks use systemd rootfs images.** Their
`test_boottime.py` runs `systemd-analyze` inside the VM and reports the
results. The example in their test code comment:

```
Startup finished in 79ms (kernel) + 231ms (userspace) = 310ms
```

That's **310ms total** — kernel boot through systemd reaching
default.target — on their CI x86_64 hardware with a stock Ubuntu 24.04
rootfs.

**Current bhatti boot breakdown (measured, Pi 5 ARM64):**

```
  Host-side (rootfs copy, FC start, API config):  ~130ms
  Kernel boot:                                    ~130ms (est)
  Lohar init (mounts → TCP listen):               ~28ms
  WaitReady (ARP + TCP probing):                  ~75ms
  Total end-to-end create:                        ~365ms (p50)
```

Lohar's PID 1 init contributes **28ms** of the 365ms. The rest is
host-side and kernel.

**What systemd would replace those 28ms with:**

If we strip systemd to only lohar.service (disable journald, resolved,
networkd, udevd, logind, timedatectl — none of these are needed in a
Firecracker VM with kernel `ip=` networking):

```
  systemd PID 1 init + mount essential FS:        ~30-50ms
  Process generators (scan, none present):         ~5-10ms
  Start basic.target → lohar.service:              ~20-40ms
  Lohar agent mode (read config, TCP listen):      ~20-30ms
  Total systemd userspace:                         ~75-130ms
```

**Realistic delta: +50-100ms over lohar-as-PID-1's 28ms.**
End-to-end create would go from ~365ms to ~415-465ms.
Not 1-3 seconds. Not even 500ms.

If we enable a larger service set (journald for logging, resolved for
DNS management):

```
  systemd userspace with journald + resolved:     ~150-250ms
  Delta over lohar-as-PID-1:                      ~120-220ms
  End-to-end create:                              ~485-585ms
```

Still under 600ms. The Firecracker CI number (231ms userspace) is with
a fuller service set than we'd need.

#### How every comparable system handles PID 1

| System | PID 1 | Agent runs as | Boot time | Snapshot support |
|--------|-------|---------------|-----------|------------------|
| **Firecracker CI** | systemd | N/A (test rootfs) | ~310ms total (79ms kernel + 231ms userspace) | Yes (FC native) |
| **Kata Containers** | Dual-mode (`getpid()==1` check) | PID 1 or systemd service | ~500ms (published) | No |
| **OpenFaaS/faasd** | systemd | Agent as systemd service | Not published | Pause only (no snapshot) |
| **the reference runtime** | Agent IS PID 1 | PID 1 (via libkrun init.c) | <200ms (published) | No |
| **Sprites** (fly.io) | Custom init | Built-in service manager | Not published | Filesystem only (no memory) |
| **AWS Lambda** | Custom init | Runtime Interface | ~100-200ms (kernel to handler) | Yes (SnapStart) |
| **Bhatti** | lohar | PID 1 | 365ms e2e (28ms init) | Yes (full memory) |

**the reference runtime deep dive (from source review):**

the reference runtime uses libkrun (not Firecracker) as its VMM. libkrun's `init.c`
mounts proc/sys/dev *before* exec'ing the agent — the VMM itself does
the init duties, then the agent runs as a normal process. the reference runtime's
agent checks if mounts already exist and skips them:

```rust
// the reference runtime-agent/src/main.rs
fn mount_essential_filesystems() {
    // libkrun's init.c mounts /proc, /sys, /dev, /dev/pts before
    // exec'ing the agent. Skip redundant mounts if already present.
    if std::path::Path::new("/proc/uptime").exists() {
        return;
    }
    // ... fallback mount code if running as actual PID 1 ...
}
```

the reference runtime does NOT use systemd. It also does NOT support snapshot/restore,
does NOT run Ubuntu packages (it uses Alpine/OCI images via crun), and
does NOT have thermal management. It's a fundamentally different
architecture: ephemeral containers in microVMs, not persistent Linux
environments. Packages are installed via `apk add` (Alpine) or are
baked into OCI images, and services are managed by crun's container
lifecycle, not by an init system.

**Why the reference runtime's approach doesn't apply to bhatti:**
- the reference runtime runs OCI containers inside VMs — services are container
  lifecycle, not init system
- the reference runtime uses Alpine, not Ubuntu — no systemd dependency chain
- the reference runtime has no snapshot/restore — no time-jump concerns
- the reference runtime is primarily for ephemeral workloads and single-file VM packing
- the reference runtime users don't `apt-get install postgresql` inside a running VM

**Kata Containers deep dive (from source review):**

Kata's agent (Rust, ~10K lines) is the closest architectural analog to
lohar. It runs in both modes:

```rust
// kata-containers/src/agent/src/main.rs
fn main() {
    let init_mode = unistd::getpid() == Pid::from_raw(1);
    let result = rt.block_on(real_main(init_mode));
    if init_mode {
        sync();
        reboot(RB_POWER_OFF);
    }
}

async fn real_main(init_mode: bool) {
    if init_mode {
        general_mount(&logger)?;          // mount proc/sys/dev/cgroup
        init_agent_as_init(&logger)?;     // bring up lo, hostname, etc
    }
    // ... start gRPC server, handle containers ...
}
```

When running as PID 1: mounts filesystems, sets up cgroups, brings up
loopback. When NOT PID 1 (systemd started it): skips all init duties.
This is exactly the dual-mode pattern. Kata's been shipping this for
5+ years across Azure, GCP, and bare metal Kubernetes clusters.

Kata does NOT have zombie reaping issues because Rust's process
handling is different from Go's — they use explicit `waitpid` calls
that don't race with their async runtime.

Kata Containers is the most relevant comparison. Their agent (Rust,
~4000 lines) does **exactly** the dual-mode pattern:

```rust
// kata-containers/src/agent/src/main.rs
let init_mode = unistd::getpid() == Pid::from_raw(1);
let result = rt.block_on(real_main(init_mode));

// In real_main:
if init_mode {
    general_mount(&logger)?;     // mount proc/sys/dev
    init_agent_as_init(&logger, cgroup_v2)?;
}
// Then start agent regardless of mode
```

If PID 1: do init duties (mounts, cgroups). If not: skip them, just
run the agent. This has been in production for 5+ years across major
cloud providers.

Some Firecracker-based products use systemd as PID 1 with their
agent as a systemd service. This is a deliberate choice for
package compatibility at the cost of boot time and snapshot
complexity.

#### Memory: +30-80 MB resident

With only lohar.service enabled (no journald, no resolved):
- systemd PID 1: ~8-12 MB
- dbus-daemon (required by systemd): ~4-6 MB
- Total: ~12-18 MB

With journald + resolved:
- systemd PID 1: ~8-12 MB
- systemd-journald: ~8-15 MB
- systemd-resolved: ~8-12 MB
- dbus-daemon: ~4-6 MB
- Total: ~28-45 MB

On a 2048 MB VM (the default): 1-2%. On 512 MB minimal: 4-9%.

#### Snapshot/restore: the real risk

This is the one area that genuinely needs empirical testing, not
estimates. When a VM is restored from snapshot, the clock jumps.
systemd responds to time jumps:

- **Timers fire.** logrotate, tmpfiles cleanup. Mostly harmless.
- **Watchdogs trigger.** Services with `WatchdogSec=` get killed and
  restarted. Actually *good* — ensures daemons are healthy after wake.
  But our lohar.service must NOT have WatchdogSec.
- **resolved updates DNS.** Fine.
- **journald writes catch-up entries.** Small I/O spike.

Mitigations (configure once in rootfs build):
```ini
# /etc/systemd/system.conf
RuntimeWatchdogSec=0
ShutdownWatchdogSec=0
```

The warm→hot transition (vCPU unpause, typically <30s gap) is the
common case and creates small time jumps that systemd handles routinely.
The cold→hot transition (snapshot restore, minutes to hours gap) is
less common and needs the test matrix described below.

#### Rootfs size: 0 MB extra

systemd is already in the debootstrap output. We're not adding it —
we're just not bypassing it with `init=`.

### What systemd would give us

- **`apt-get install openssh-server` just works.** Issue #12 goes away
  completely. resolved manages DNS. sshd starts. No VM breakage.
- **`apt-get install postgresql` just works.** `systemctl start postgresql`
  does what the user expects.
- **Service supervision for free.** `Restart=always`, `RestartSec=`,
  `WatchdogSec=`, `Type=notify` — the entire restart/health/dependency
  system that would take us weeks to reimplement.
- **Every Ubuntu tutorial works.** Users don't have to learn "bhatti is
  different." It's just Ubuntu in a VM.
- **journald for logging.** `journalctl -u myapp` instead of grepping
  random log files.
- **Our own tiers get simpler.** Docker tier becomes a `dockerd.service`
  with `Restart=always` instead of a shell script with `&` and a
  readiness poll loop.

### The dual-mode approach

We don't have to choose one path for all use cases. The kernel cmdline
controls which init runs:

```
Fast mode:   init=/usr/local/bin/lohar   (current behavior, AI agent workloads)
Compat mode: init=/sbin/init             (systemd, dev sandboxes)
```

**Implementation:**

1. **Lohar gets a non-PID-1 mode.** If `os.Getpid() != 1`, skip all
   init duties (mounts, networking, config drive). Just start the agent
   listeners and block. ~20 lines of change in `main.go`:

   ```go
   if os.Getpid() == 1 {
       // current PID 1 init path (mounts, config drive, networking...)
       runAsPID1()
   } else {
       // systemd started us as a service
       runAsAgent()
   }
   ```

2. **Ship a `lohar.service` systemd unit in the rootfs.** It reads the
   config drive, applies bhatti config, and starts the agent:

   ```ini
   [Unit]
   Description=Bhatti Guest Agent
   After=network-online.target
   Wants=network-online.target

   [Service]
   Type=simple
   ExecStartPre=/usr/local/bin/lohar --apply-config
   ExecStart=/usr/local/bin/lohar --agent
   Restart=always
   RestartSec=1

   [Install]
   WantedBy=multi-user.target
   ```

3. **Engine picks boot args based on a flag.** In `create.go`:

   ```go
   initBin := "/usr/local/bin/lohar"
   if spec.SystemdMode {
       initBin = "/sbin/init"
   }
   bootArgs := fmt.Sprintf(
       "reboot=k panic=1 pci=off init=%s quiet ip=%s::%s:...",
       initBin, guestIP, gatewayIP)
   ```

4. **CLI flag: `--compat` or `--systemd` on create.** Or make it a
   per-tier default: minimal stays fast, a new "standard" tier uses
   systemd.

5. **Config drive handling in systemd mode.** A `lohar --apply-config`
   step reads the config drive and writes:
   - `/etc/hostname`
   - `/etc/hosts`
   - env vars to `/etc/bhatti/env` (sourced by lohar.service and init scripts)
   - files from config drive
   - volume mounts via systemd mount units or direct mount calls

   The config drive mount/read/unmount is the same code — just called
   from ExecStartPre instead of the PID 1 init path.

### What about snapshot/restore in systemd mode?

This is the question that needs empirical answers. The concern:

```
  t=0s    VM running, systemd happy, services up
  t=30s   Thermal manager pauses vCPUs (warm)
  t=5min  Thermal manager snapshots to disk (cold)
  ...
  t=2hrs  User runs bhatti exec dev -- echo hi
          → restore snapshot
          → systemd sees clock jump of 2 hours
          → what happens?
```

Likely outcomes:
- **Timers fire.** logrotate, tmpfiles cleanup, etc. Mostly harmless.
- **Watchdogs trigger.** Services with `WatchdogSec=` get killed and
  restarted. Actually *good* — ensures daemons are healthy after wake.
- **resolved updates DNS.** Also fine.
- **journald writes catch-up entries.** Small I/O spike, harmless.

The dangerous case would be if systemd tries to power off the VM
(shutdown timer), but that requires explicit configuration (`ScheduledShutdown=`).

This needs a test matrix, not speculation. The engineering work is:
1. Boot a systemd VM, start some services
2. Pause vCPUs for 1 min, resume, verify services
3. Full snapshot, restore after 1 hr, verify services
4. Repeat with WatchdogSec, timer units, resolved
5. Measure boot time delta

### What about snapshot/restore — the test matrix

Before committing to systemd mode, run this matrix on Pi 5 and one
x86_64 machine:

```
Test 1: Boot → systemd-analyze → verify lohar.service is active
Test 2: Boot → start user services → pause 30s → resume → verify services
Test 3: Boot → start user services → full snapshot → restore after 5m → verify
Test 4: Boot → start user services → full snapshot → restore after 2h → verify
Test 5: Boot → apt-get install openssh-server → systemctl start ssh → verify
Test 6: Boot → apt-get install postgresql → pg_isready → snapshot → restore → pg_isready
Test 7: Boot 20 VMs sequentially, measure p50/p95 create time vs lohar-as-PID-1
```

"Verify services" means: lohar agent responds, user services are
running (`systemctl is-active`), DNS works, network works.

If tests 1-6 pass and test 7 shows <150ms delta, systemd mode is ready.
If the delta is >150ms or any snapshot/restore test fails, we need to
investigate which systemd units are responsible and disable them.

### Recommendation (revised)

**The pragmatic path is dual-mode, phased:**

Phase 1 (now): Fix the immediate issue #12 problems — docs, resolv.conf
immutability, create output. These help regardless of the systemd decision.

Phase 2 (next, ~3-5 days): Build and test systemd mode.
- Refactor lohar: `if os.Getpid() == 1 { runAsPID1() } else { runAsAgent() }`
- Ship `lohar.service` in rootfs
- Engine: choose boot args based on sandbox/tier flag
- Run the test matrix above
- Gate behind a `--compat` flag or make it per-tier

Phase 3 (if test matrix passes): Make systemd the default for all
non-minimal tiers. Keep the fast path (`init=/usr/local/bin/lohar`)
for the minimal tier and AI agent workloads where every millisecond
of boot time matters.

The key insight from the research: **the boot time delta is ~50-100ms,
not 1-3 seconds.** The claim in decisions.md was an unverified estimate
that assumed full Ubuntu services. Firecracker's own CI measures
231ms systemd userspace. A stripped config would be less. This is
well within acceptable range for all use cases except Lambda-style
sub-100ms cold starts, which is not our use case.

---

## Updated Recommendations (prioritized)

| Priority | Item | Effort | Impact |
|----------|------|--------|--------|
| **P0** | Fix cli-reference.md memory default (512→2048) | 5 min | Eliminates the #1 confusion from issue #12 |
| **P0** | Make resolv.conf immutable at boot (`chattr +i`) | 10 min | Prevents openssh and all systemd packages from breaking DNS |
| **P1** | Add `docs/limitations.md` (known don'ts) | 1 hr | Sets expectations before users hit walls |
| **P1** | Show vCPU/memory/disk in `create` output | 30 min | Users know what they got without inspecting |
| **P1** | Document the `--init` daemon pattern (interim until systemd mode) | 1 hr | Gives users a story while we build the real fix |
| **P2** | Lohar dual-mode + `lohar.service` + snapshot/restore test matrix | 3–5 days | Systemd mode behind `--compat` flag |
| **P2** | Auto-resize rootfs to 2 GB when `--disk-size` not set | 30 min | Prevents ENOSPC on minimal tier |
| **P3** | Make systemd the default for non-minimal tiers | 1 day | Full compatibility, issue #12 class of problems eliminated |

---

## What This Plan Does NOT Cover

**Automatic package compatibility testing.** We can't test every apt
package. The systemd mode eliminates the need — if systemd is running,
packages that depend on systemd just work.

**SSH access to VMs.** With systemd mode, `apt-get install openssh-server`
would work and sshd would start. But the intended access path is still
`bhatti shell` / `bhatti exec` — SSH is a bonus, not the primary interface.

**Full container orchestration.** Users who need 5+ services with complex
dependency graphs should use the Docker tier and `docker compose`. That's
the right tool for that job — bhatti provides the VM, Docker provides the
orchestration.

**Removing the fast path.** `init=/usr/local/bin/lohar` stays forever.
It's the right choice for AI agent workloads where boot time matters and
no one is apt-installing packages. Dual-mode means we don't have to
choose.
