# Package Compatibility — lohar as PID 1 with systemctl shim

lohar stays as PID 1. A built-in systemctl implementation makes
`apt-get install openssh-server` work without real systemd.
Snapshot/restore keeps working. Addresses GitHub issue #12.

**Target machine:** `ssh user@192.168.1.201` (raspi-5a, aarch64, Pi 5)

---

## Why not systemd

We spent a week trying to run systemd as PID 1 with lohar as a
systemd service. Here's what happened and why we're not doing it.

### What we tried

1. Built a systemd-native rootfs (systemd + dbus + journald +
   lohar.service)
2. Fresh boot works — `systemctl is-system-running` returns
   "running", lohar.service active, exec works
3. Stop (snapshot) works
4. Start (restore) **breaks** — lohar's TCP listeners accept
   connections at the kernel level but the Go runtime never
   processes them. First exec after restore works (WaitReady),
   every subsequent exec hangs forever.

### What we ruled out

- **Not a Firecracker version issue.** Tested on FC 1.14.0 and
  1.15.1. Same failure.
- **Not our lohar code changes.** The POC systemd rootfs (built
  weeks earlier with different lohar.service config) has the same
  failure.
- **Not our service file.** Tested with both `Restart=no` and
  `Restart=always`. Same failure.
- **Not a network issue.** After restore: ping works, port 22
  returns RST (guest kernel responsive), port 1024 accepts TCP
  connections (lohar listener still registered in kernel).
- **Not a general Firecracker snapshot bug.** The same lohar
  binary, same Firecracker, same host kernel — works perfectly
  when lohar is PID 1 (no systemd). CI tests for stop/start/exec
  pass consistently.

### The root cause

When a Firecracker VM is restored from snapshot, all guest
processes resume from where they were frozen. TCP listener sockets
survive at the kernel level (accept queue works, SYN-ACK happens).
But the Go runtime's network poller — which is what wakes goroutines
blocked on Accept() and Read() — does not resume correctly when the
Go process is a child of systemd.

This is a known class of issue. Firecracker issue #4099 documented
the same pattern: processes stuck after snapshot restore, traced to
timer interrupts being lost. The x86 variant was fixed in FC's MSR
restoration order (PR #4666). The ARM64 variant with child processes
of systemd has no fix.

The fundamental problem: **systemd and its children (dbus, journald,
lohar) introduce kernel state (timers, epoll sets, inotify watches)
that doesn't survive snapshot restore cleanly.** When lohar is PID 1
and is the only userspace process, the kernel state is minimal and
resumes correctly.

Every serious Firecracker deployment — AWS Lambda, Fly.io,
CodeSandbox — runs a custom PID 1 agent, not systemd. The reason
is this exact issue.

### What users actually need

The #12 user's complaint was `apt-get install openssh-server`
breaking the VM. We tested this on the non-systemd rootfs:

- The package **installs successfully** (keys generated, configs
  written, deb-systemd-helper creates enable symlinks)
- The service **doesn't auto-start** (invoke-rc.d can't determine
  runlevel, no systemctl to call)
- `systemctl start ssh` fails (binary doesn't exist)

Users don't need systemd. They need `systemctl` to work — the
binary that reads `.service` files and starts/stops processes.
These are different things.

### The design: busybox-pattern systemctl built into lohar

lohar (already in every rootfs) gains a systemctl personality.
One binary, three symlinks:

```
/usr/local/bin/lohar         — the actual binary
/sbin/init → lohar           — kernel boots this as PID 1
/usr/bin/systemctl → lohar   — packages call this
```

When invoked as `init` or `lohar`: run the agent (existing code).
When invoked as `systemctl`: handle service management commands.

This is the busybox pattern — a single binary that checks
`os.Args[0]` to determine its behavior. busybox provides 300+
Unix utilities in one binary this way. lohar provides the sandbox
agent + systemctl in one binary.

No Python. No external dependencies. No separate daemon. The
systemctl implementation reads `.service` files directly, manages
processes via PID files, and exits. lohar (PID 1) handles zombie
reaping for everything.

Reference implementation: `narwhal-cloud/systemctl` (Go, ~400
lines, built for Docker containers without systemd). We'll build
our own, tailored to bhatti's needs.

---

# Release A — systemctl shim + package compatibility

Tag: `v1.9.0-rc.1` → `v1.9.0`

lohar gets the systemctl personality. The rootfs gets the symlinks.
Packages that call systemctl during install work. Services start.
Snapshot/restore keeps working because lohar stays as PID 1.

---

## A1 — Restore lohar PID 1 init duties

We deleted the PID 1 path in the previous commit. We need to bring
back the init duties (mounts, loopback, signal handlers) but keep
the code clean — no dual-mode, no `runAsAgent()` copy-paste.

The current `runAgent()` assumes systemd handles mounts and
loopback. Since lohar is PID 1 again, it needs to do these itself.

Restore from the git history:
- `mustMount()` calls for proc, sys, dev, devpts, tmpfs, run, shm
- cgroup v2 mount + subtree_control
- `bringUpInterface("lo")`
- `installSignalHandlers()` (SIGTERM → sync → poweroff)

Keep the fixes from the systemd attempt:
- DNS fallback (`ensureResolvConf()` when config drive has no DNS)
- Boot timing to `/run/bhatti/boot-timing.txt`
- Clean single-function structure (`runAgent()`)

Add zombie reaping (lohar is PID 1, must reap orphans):
```go
// Reap orphaned zombie processes. Go's runtime handles SIGCHLD for
// processes started via exec.Command, but grandchild processes
// (e.g. services started by systemctl shim) need explicit reaping.
go func() {
    var status syscall.WaitStatus
    for {
        pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
        if err != nil || pid <= 0 {
            time.Sleep(1 * time.Second)
            continue
        }
    }
}()
```

### Changes

- `cmd/lohar/main.go`: Restore PID 1 init in `runAgent()`, add
  zombie reaper. Keep the DNS fix and boot-timing fix.

---

## A2 — Revert engine init= to /usr/local/bin/lohar

We changed `init=/sbin/init` in the previous commit. Revert to
`init=/usr/local/bin/lohar` since lohar is PID 1 again.

The `/sbin/init → lohar` symlink in the rootfs means both paths
work, but being explicit about lohar avoids ambiguity.

### Changes

- `pkg/engine/firecracker/create.go`: Change `init=/sbin/init`
  back to `init=/usr/local/bin/lohar`.

---

## A3 — Build the systemctl shim into lohar

New file: `cmd/lohar/systemctl.go`

### Dispatch (busybox pattern)

In `main()`:
```go
func main() {
    name := filepath.Base(os.Args[0])
    switch {
    case os.Getenv("LOHAR_TEST") == "1":
        runTestMode()
    case name == "systemctl":
        runSystemctl(os.Args[1:])
    default:
        runAgent()
    }
}
```

### Commands to implement

Based on what Debian/Ubuntu package scripts actually call:

**`systemctl start <service>`**
1. Find `.service` file in `/usr/lib/systemd/system/` or
   `/etc/systemd/system/`
2. Check if masked (symlink to `/dev/null`) → error
3. Parse `[Service]` section: `ExecStartPre`, `ExecStart`,
   `RuntimeDirectory`, `User`, `WorkingDirectory`,
   `Environment`, `EnvironmentFile`
4. Create RuntimeDirectory (e.g. `/run/sshd`)
5. Run ExecStartPre commands (sequential, fail-fast)
6. Fork/exec ExecStart
7. For `Type=simple`: write PID to
   `/run/bhatti/services/<name>.pid`, exit
8. For `Type=oneshot`: wait for completion, exit
9. For `Type=forking`: wait for main process to exit, find
   child PID (from PIDFile or by scanning), write to PID file

**`systemctl stop <service>`**
1. Read PID from `/run/bhatti/services/<name>.pid`
2. Parse ExecStop from service file (if present, run it)
3. If no ExecStop: send SIGTERM, wait TimeoutStopSec (default
   5s), SIGKILL
4. Remove PID file

**`systemctl restart <service>`** — stop then start.

**`systemctl enable <service>`** — create symlink in
`/etc/systemd/system/multi-user.target.wants/`. Parse
`[Install] WantedBy` for the target directory.

**`systemctl disable <service>`** — remove the symlink.

**`systemctl is-active <service>`** — read PID file, check
process alive (`kill -0`), print `active` or `inactive`.

**`systemctl is-enabled <service>`** — check symlink exists,
print `enabled` or `disabled`.

**`systemctl status <service>`** — print active/inactive +
PID + service description. Minimal, not full systemd output.

**`systemctl is-system-running`** — print `running`. Always.

**`systemctl daemon-reload`** — no-op, exit 0.

**`systemctl list-units [--state=X]`** — scan service files,
check PID files, print table. Support `--state=failed` (always
empty — we don't track failure state) and `--state=running`.

**`systemctl mask <service>`** — symlink service file to
`/dev/null`.

**`systemctl unmask <service>`** — remove the `/dev/null`
symlink.

**`systemctl show <service> -p <prop>`** — parse service file,
return requested property. Package scripts use this.

**`systemctl cat <service>`** — print the service file. Trivial.

### .service file parser

Minimal INI parser. No dependency on `coreos/go-systemd`. The
format is simple enough:

```
[Unit]
Description=OpenBSD Secure Shell server

[Service]
Type=simple
ExecStartPre=/usr/sbin/sshd -t
ExecStart=/usr/sbin/sshd -D
RuntimeDirectory=sshd
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

Parse into `map[string]map[string]string` (section → key → value).
Handle multi-line values (backslash continuation). Handle multiple
`ExecStartPre` lines. ~100 lines.

### PID file location

All PID files go to `/run/bhatti/services/<name>.pid`. This is
a tmpfs directory, cleared on reboot. lohar creates it at boot.
The shim reads/writes PID files. lohar (PID 1) reaps the zombies.

### What we explicitly DON'T implement

- Socket activation (`ListenStream`) — no services in our target
  set need it
- `Type=notify` (sd_notify) — complex, openssh/nginx/postgres/
  redis don't use it in their default configs
- Timer units — cron exists
- Slice/scope/cgroup management — Firecracker already constrains
  the VM
- `journalctl` — services write to stdout/stderr, lohar can
  capture if needed later
- `systemd-tmpfiles`, `systemd-sysusers` — handle manually in
  rootfs build

### Changes

- `cmd/lohar/main.go`: Add busybox dispatch in `main()`
- `cmd/lohar/systemctl.go`: New file, ~400-500 lines
- `cmd/lohar/service_parser.go`: New file, .service file parser,
  ~100 lines

---

## A4 — Rootfs: add symlinks and enable auto-start

Modify `scripts/tiers/minimal.sh` to set up the shim:

```bash
# systemctl shim — lohar handles systemctl commands via busybox pattern
ln -sf /usr/local/bin/lohar "$MOUNT/usr/bin/systemctl"

# Create the services PID directory
mkdir -p "$MOUNT/run/bhatti/services"

# Mark system as "systemd-like" so deb-systemd-helper uses
# the enable/disable path instead of the no-op path.
# deb-systemd-helper checks for /run/systemd/system to decide.
mkdir -p "$MOUNT/run/systemd/system"
```

Also in `minimal.sh` — lohar needs to create `/run/systemd/system`
at boot (it's tmpfs, gone on reboot). Add to `runAgent()`:
```go
os.MkdirAll("/run/systemd/system", 0755)
os.MkdirAll("/run/bhatti/services", 0755)
```

**Auto-start enabled services at boot.** After listeners are up
and before the boot profile runs, lohar scans
`/etc/systemd/system/multi-user.target.wants/` and starts each
enabled service:

```go
// Start enabled services (reads the wants directory, starts each)
startEnabledServices()
```

This replaces what systemd's multi-user.target does. Services
installed with `apt-get install` and enabled via
`deb-systemd-helper` will auto-start on next boot.

### Changes

- `scripts/tiers/minimal.sh`: Add symlink, keep rootfs at 512 MB
  (no systemd packages needed)
- `scripts/build-tier.sh`: Revert minimal size to 512 MB
- `cmd/lohar/main.go`: Create /run/systemd/system and
  /run/bhatti/services at boot, call `startEnabledServices()`

---

## A5 — Lower default memory

Same as before — 2048 → 1024 MB default. Already committed but
needs the server handler update.

```go
// pkg/server/sandbox_handlers.go:229
spec.MemoryMB = 1024
```

Already done in the previous commit. No additional changes.

---

## A6 — Integration tests (Pi cluster)

### Automated (extend integration.yml)

New test file: `pkg/engine/firecracker/systemctl_test.go`

```go
func TestPackageInstallOpenssh(t *testing.T) {
    eng, ctx := setupEngine(t)
    info := createSandbox(t, eng, ctx, 1024, 2048) // 1GB RAM, 2GB disk
    defer eng.Destroy(ctx, info.ID)

    // apt-get install openssh-server
    execOrFail(t, eng, ctx, info.ID,
        "apt-get update -qq")
    execOrFail(t, eng, ctx, info.ID,
        "apt-get install -y --no-install-recommends openssh-server")

    // Service should be running
    assertExec(t, eng, ctx, info.ID,
        "systemctl is-active ssh", "active")

    // sshd should be listening
    assertExecContains(t, eng, ctx, info.ID,
        "ss -tln", ":22")
}

func TestPackageInstallNginx(t *testing.T) {
    eng, ctx := setupEngine(t)
    info := createSandbox(t, eng, ctx, 1024, 2048)
    defer eng.Destroy(ctx, info.ID)

    execOrFail(t, eng, ctx, info.ID,
        "apt-get update -qq")
    execOrFail(t, eng, ctx, info.ID,
        "apt-get install -y --no-install-recommends nginx")

    assertExec(t, eng, ctx, info.ID,
        "systemctl is-active nginx", "active")
    assertExecContains(t, eng, ctx, info.ID,
        "curl -sf localhost", "nginx")
}

func TestServiceSurvivesSnapshot(t *testing.T) {
    eng, ctx := setupEngine(t)
    info := createSandbox(t, eng, ctx, 1024, 2048)
    defer eng.Destroy(ctx, info.ID)

    // Install and start a service
    execOrFail(t, eng, ctx, info.ID,
        "apt-get update -qq && apt-get install -y --no-install-recommends nginx")
    assertExec(t, eng, ctx, info.ID,
        "systemctl is-active nginx", "active")

    // Stop (snapshot)
    eng.Stop(ctx, info.ID)

    // Start (restore) — lohar resumes as PID 1, restarts services
    eng.Start(ctx, info.ID)

    // nginx should be running again
    assertExec(t, eng, ctx, info.ID,
        "systemctl is-active nginx", "active")
    assertExecContains(t, eng, ctx, info.ID,
        "curl -sf localhost", "nginx")
}

func TestSystemctlBasicCommands(t *testing.T) {
    eng, ctx := setupEngine(t)
    info := createSandbox(t, eng, ctx, 1024, 2048)
    defer eng.Destroy(ctx, info.ID)

    execOrFail(t, eng, ctx, info.ID,
        "apt-get update -qq && apt-get install -y --no-install-recommends nginx")

    // is-system-running
    assertExec(t, eng, ctx, info.ID,
        "systemctl is-system-running", "running")

    // stop
    execOrFail(t, eng, ctx, info.ID, "systemctl stop nginx")
    assertExec(t, eng, ctx, info.ID,
        "systemctl is-active nginx", "inactive")

    // start
    execOrFail(t, eng, ctx, info.ID, "systemctl start nginx")
    assertExec(t, eng, ctx, info.ID,
        "systemctl is-active nginx", "active")

    // restart
    execOrFail(t, eng, ctx, info.ID, "systemctl restart nginx")
    assertExec(t, eng, ctx, info.ID,
        "systemctl is-active nginx", "active")

    // daemon-reload (should be no-op)
    execOrFail(t, eng, ctx, info.ID, "systemctl daemon-reload")
}

func TestThermalCyclesWithServices(t *testing.T) {
    eng, ctx := setupEngine(t)
    info := createSandbox(t, eng, ctx, 1024, 2048)
    defer eng.Destroy(ctx, info.ID)

    execOrFail(t, eng, ctx, info.ID,
        "apt-get update -qq && apt-get install -y --no-install-recommends nginx")

    for i := 0; i < 5; i++ {
        eng.Stop(ctx, info.ID)
        eng.Start(ctx, info.ID)
        assertExec(t, eng, ctx, info.ID,
            "systemctl is-active nginx", "active")
        assertExecContains(t, eng, ctx, info.ID,
            "curl -sf localhost", "nginx")
    }
}
```

### Manual tests (after CI passes)

```bash
# Issue #12 scenario end-to-end
bhatti create --name test --cpus 1 --memory 1024 --disk-size 2048
bhatti exec test -- sudo apt-get update -qq
bhatti exec test -- sudo apt-get install -y --no-install-recommends openssh-server
bhatti exec test -- systemctl is-active ssh
bhatti exec test -- sudo systemctl stop ssh
bhatti exec test -- systemctl is-active ssh    # inactive
bhatti exec test -- sudo systemctl start ssh
bhatti exec test -- systemctl is-active ssh    # active

# Survives snapshot
bhatti stop test
bhatti start test
bhatti exec test -- systemctl is-active ssh    # active

# Other packages
bhatti exec test -- sudo apt-get install -y --no-install-recommends nginx
bhatti exec test -- curl -sf localhost | head -1
bhatti exec test -- sudo apt-get install -y --no-install-recommends redis-server
bhatti exec test -- redis-cli ping

# Boot timing (target < 500ms — no systemd overhead)
for i in $(seq 1 5); do
    START=$(date +%s%N)
    bhatti create --name bt --cpus 1 --memory 1024 >/dev/null 2>&1
    END=$(date +%s%N)
    echo "$i: $(( (END - START) / 1000000 ))ms"
    bhatti destroy bt -y >/dev/null 2>&1
    sleep 1
done

# Tier boot profiles
bhatti create --name docker-test --image docker --cpus 2 --memory 2048
bhatti exec docker-test -- docker info
bhatti destroy docker-test -y

bhatti destroy test -y
```

### Decision gate

If package installs work + services start + snapshot/restore works
+ boot timing < 500ms → tag v1.9.0.

If a package's postinst calls a systemctl subcommand we don't
support → add it (the shim is extensible, add cases as needed).

---

# Release B — CLI/UX overhaul + cleanup

**Moved to [PLAN-cli-ux.md](PLAN-cli-ux.md).** Release A shipped
as v1.9.0. Release B is its own plan now.

The remainder of this file is kept for reference (the original
inline B items). See `PLAN-cli-ux.md` for the current plan with
`--secret`, `--file`, and the full integration test spec.

---

*Original inline B items (superseded by PLAN-cli-ux.md):*

Tag: `v1.10.0`

All CLI/host-side changes. No guest changes, no rootfs rebuilds.
Ordered by impact for the HN launch — first impression items first.

---

## B1 — Verbose create output + disk visibility

The first thing every user sees. Currently:
```
a1b2c3d4e5f6    dev    10.0.1.2
```

New:
```
sandbox/dev created (1 vCPU, 1024 MB, 1024 MB disk)
  IP:    10.0.1.2
  Shell: bhatti shell dev
```

Shows resources so the user knows what was allocated (the #12
complaint: "had to read docs to find the right flags"). Shows
disk size so they know how much space they have. Hints the next
command.

Idempotent create: `sandbox/dev unchanged (already exists)`.

## B2 — Streaming exec

Second thing every user hits. `bhatti exec dev -- sudo apt-get
install openssh-server` shows nothing for 30+ seconds.

When stdout is a terminal, send `Accept: application/x-ndjson`.
Server already supports it. Stream output line by line. When
piped, keep existing buffered behavior.

## B3 — Actionable error messages

Third thing every user hits — when something goes wrong.
Currently: `500 Internal Server Error: internal error`.

Pattern-match known errors, append recovery hints:
```
Error: sandbox "dev" is not running

  Resume it first:
    bhatti start dev
```

Also: confirm verbs on stop/start/destroy:
```
sandbox/dev stopped
sandbox/dev started
sandbox/dev destroyed
```

## B4 — Richer inspect + disk usage

kubectl `describe` style. The page you look at when something
isn't working.

```
Name:       dev
ID:         a1b2c3d4e5f6
Status:     running
Thermal:    hot
Image:      minimal
Created:    2026-04-30 10:00:00 (3 hours ago)

Resources:
  CPUs:     1
  Memory:   1024 MB
  Disk:     1024 MB (407 MB used, 518 MB free)

Network:
  IP:       10.0.1.2

Ports:
  22/tcp
  8080/tcp

Volumes:
  workspace → /workspace (rw)
```

Server: add cpus, memory_mb, disk_size_mb, image columns to
sandboxes table. Disk usage via live `df` exec (running VMs only).

## B5 — `bhatti ports`

CLI for existing `GET /ports` and `GET /sandboxes/:id/ports`.

```
$ bhatti ports dev
PORT    PROXY
22      /sandboxes/a1b2c3d4/proxy/22/
8080    /sandboxes/a1b2c3d4/proxy/8080/
```

~40 lines. Server endpoints exist.

## B6 — Cleaner list + wide mode

Drop ID from default columns (names are the primary key).
Add `-o wide` with resources and image.

```
$ bhatti ls
NAME         STATUS   THERMAL  IP
dev          running  hot      10.0.1.2

$ bhatti ls -o wide
NAME         STATUS   THERMAL  IP            CPUS  MEMORY  DISK   IMAGE
dev          running  hot      10.0.1.2      1     1024    1024   minimal
```

Needs the store columns from B4.

## B7 — Wire up `--force` on start

Error says `"use 'bhatti start --force' to retry"` but the flag
doesn't exist. Engine has `StartForce()`. Wire server + CLI.
~10 lines.

## B8 — Fix image pull Ctrl+C

Trap SIGINT, print "pull continues on server, check: bhatti
image list", exit cleanly. ~15 lines.

## B9 — `--detach` flag on exec

CLI for existing server `detach: true`. Fire-and-forget for
long-running commands.

```
$ bhatti exec dev --detach -- make build-all
pid: 4821
output: /tmp/bhatti-exec-4821.log
```

## B10 — `--hugepages` flag on create

CLI for existing server/engine support. 3 lines.

## B11 — `bhatti volume clone`

CLI for existing `POST /volumes/:name/snapshot`. ~20 lines.

## B12 — Delete dead code

Remove `UserData` and `Labels` from SandboxSpec and Template.
Leave DB columns (harmless, risky to migrate).

## B13 — Update `scripts/build-rootfs.sh`

Standalone dev rootfs gets the systemctl/journalctl symlinks,
policy-rc.d, runlevel shim, universe repo, systemd pin.

## B14 — Rewrite cli-reference.md

Cover all 35+ commands. Group by category:
```
Core:       create, list, inspect, destroy, stop, start, edit
Exec:       exec, shell, ps, ports
Files:      file read, file write, file ls
Resources:  image, volume, snapshot, secret
Publish:    publish, unpublish, share
Admin:      admin status, admin events, admin metrics, user, setup, update
```

Document volume backup (fully built, zero docs). Document the
systemctl shim behavior and limitations.

## B15 — Integration tests

New file: `cmd/bhatti/cli_ux_test.go`

This is the HN launch gate. Every B item gets verified by at least
one test. Tests are organized in three tiers: must-have for launch,
important polish, and edge cases for long-term safety.

All tests use the existing `cliTest` harness (builds the binary
from source, talks to a real daemon). Tests that modify output
formats use exact string matching (golden-file style) — not
`strings.Contains` — so formatting regressions are caught.

### Tier 1 — Must-have for launch (first-impression path)

The exact path every HN user will walk: create → see output →
exec something → hit an error → check inspect → list → stop/start
→ check ports. 11 tests.

```go
// B1: Verbose create output
func TestCLICreateVerboseOutput(t *testing.T)
    // Create sandbox, verify multi-line format:
    //   sandbox/<name> created (1 vCPU, 1024 MB, 1024 MB disk)
    //     IP:    10.x.x.x
    //     Shell: bhatti shell <name>
    // Match exact format — not strings.Contains.

func TestCLICreateIdempotent(t *testing.T)
    // Create same name twice → second prints:
    //   sandbox/<name> unchanged (already exists)
    // Exit code 0 (not an error).

// B2: Streaming exec
func TestCLIStreamingExecNDJSON(t *testing.T)
    // Run slow command (echo line; sleep 0.1; echo line).
    // Verify Accept: application/x-ndjson was sent.
    // Verify stdout lines arrive incrementally.
    // Use BHATTI_FORCE_STREAM=1 env to bypass TTY check in tests.
    //
    // Cut: TestCLIExecBufferedWhenPiped — existing TestCLIExec
    // already runs piped (harness always pipes stdout). That test
    // already proves piped mode returns plain text.

// B3: Actionable error messages
func TestCLIErrorExecOnStopped(t *testing.T)
    // Create → stop → exec. Verify stderr contains:
    //   sandbox "<name>" is not running
    //   Resume it first:
    //     bhatti start <name>
    //
    // Cut: TestCLIErrorNotFound — existing TestCLIExecNonexistentSandbox
    // and TestCLIDestroyNonexistentSandbox already cover that path.

func TestCLIStopStartConfirmVerbs(t *testing.T)
    // Stop → verify output: "sandbox/<name> stopped"
    // Stop again → should not error (idempotency)
    // Start → verify output: "sandbox/<name> started"
    // Start again → should succeed or print "already running" (idempotency)
    // Destroy → verify output: "sandbox/<name> destroyed"
    // Exact format, not substring.
    //
    // Merged: TestCLIStopStopIdempotent and TestCLIStartRunningIdempotent
    // fold into this test — same sandbox, same lifecycle, zero extra VMs.

// B3 + lifecycle: Stop/start round-trip
func TestCLIStopStartRoundTrip(t *testing.T)
    // Create → exec (write marker) → stop → start → exec (read marker).
    // Verifies the snapshot/restore path through the CLI.

// B4: Richer inspect
func TestCLIInspectRichOutput(t *testing.T)
    // Create with --cpus 2 --memory 2048 --disk-size 4096.
    // Write a 10MB file to show disk usage.
    // Inspect → verify kubectl-describe-style fields present:
    //   Name, ID, Status, Thermal, Image, Created,
    //   Resources: (CPUs, Memory, Disk with used/free), Network: (IP)
    // Inspect --json → parse → verify cpus, memory_mb,
    //   disk_size_mb, image fields present and correct types.
    //
    // Merged: TestCLIInspectDiskUsage and TestCLIInspectJSON — same
    // sandbox, same inspect call. One VM, text + JSON assertions.

// B5: Ports
func TestCLIPorts(t *testing.T)
    // Create → exec (python3 -m http.server 9090 &) → ports.
    // Verify output table includes 9090.
    // Verify --json returns parseable array with port + proxy path.

// B6: Cleaner list + wide mode
func TestCLIListCleanDefault(t *testing.T)
    // Create → list. Verify default columns are:
    //   NAME  STATUS  THERMAL  IP
    // Verify ID column is NOT present.

func TestCLIListWideMode(t *testing.T)
    // Create with --cpus 2 --memory 2048 → list -o wide.
    // Verify columns include:
    //   NAME  STATUS  THERMAL  IP  CPUS  MEMORY  DISK  IMAGE
    // Verify CPUS=2, MEMORY=2048 in the output row.

// B7: Force start
func TestCLIForceStart(t *testing.T)
    // Create → stop → start --force → exec succeeds.
    // (If no restore failure to trigger, at minimum verify
    // the flag is accepted and start succeeds.)
```

### Tier 2 — Important polish (10 tests)

Commands that exist but have zero test coverage, plus the
remaining B items that wire CLI flags to existing server features.

```go
// B9: Detached exec
func TestCLIDetachedExec(t *testing.T)
    // exec --detach -- sleep 300
    // Verify output contains "pid:" and "output:".
    // Verify exit code 0 (command launched, not waited on).
    // Verify the process is running: exec -- kill -0 <pid>

// B10: Hugepages flag
func TestCLIHugepagesFlag(t *testing.T)
    // create --hugepages --name hp-test
    // Verify sandbox created successfully.
    // Inspect → verify hugepages field is true.

// B11: Volume clone
func TestCLIVolumeClone(t *testing.T)
    // volume create → create sandbox → write data → destroy.
    // volume clone src → dst.
    // create sandbox with dst → verify data present.
    // Cleanup both volumes.

// Exit code contract (kubectl/docker standard)
func TestCLIExitCodeContract(t *testing.T)
    // exec -- true → exit 0
    // exec -- false → exit 1
    // exec -- sh -c "exit 42" → exit 42
    // exec nonexistent-sandbox → exit non-zero (not 0)
    // destroy nonexistent → exit non-zero

// --json on every major command
func TestCLIJSONCreateInspectListPorts(t *testing.T)
    // Create --json → valid JSON with id, name, ip, cpus, memory_mb
    // Inspect --json → valid JSON with all B4 fields
    // List --json → array, sandbox present
    // Ports --json → array
    // Each parse must succeed (json.Unmarshal, not strings.Contains).

// Commands with zero test coverage today
func TestCLIEditKeepHot(t *testing.T)
    // Create → edit --keep-hot → inspect → keep_hot: true.
    // edit --allow-cold → inspect → keep_hot: false.
    // edit --keep-hot --allow-cold → error.

func TestCLIPublishUnpublish(t *testing.T)
    // Create → exec (start http server on 9090).
    // Publish -p 9090 → verify URL in output.
    // Unpublish -p 9090 → verify success message.

func TestCLIShareRevoke(t *testing.T)
    // Create → share → verify URL in output.
    // share --revoke → verify revoked message.

func TestCLIAdminStatus(t *testing.T)
    // admin status → verify output contains version/uptime/sandboxes.
    // admin status --json → parse valid JSON.
    // No VM needed.

func TestCLITimingFlag(t *testing.T)
    // exec --timing -- echo hi
    // Verify stderr contains "server:" and "total:" lines.
    // Verify stdout still has "hi".
```

### Tier 3 — Edge cases for long-term safety (7 tests)

```go
// Full lifecycle
func TestCLILifecycleFullCycle(t *testing.T)
    // create → exec (write) → stop → start → exec (read) →
    // stop → start → exec (read again) → destroy.
    // Data survives two thermal cycles.
    //
    // Cut: TestCLIThermalCycleWithExec — this test already does
    // 2 thermal cycles. A6 engine tests already do 5 cycles with
    // nginx + apt-get. Repeating through CLI adds 30s+ of apt-get
    // wall time for no new signal.

func TestCLIConcurrentExec(t *testing.T)
    // Launch 5 concurrent execs (different commands) on same sandbox.
    // All return correct results (no cross-contamination).
    // Use sync.WaitGroup + goroutines.

func TestCLILargeOutput(t *testing.T)
    // exec -- seq 1 100000 (outputs ~600KB).
    // Verify first line is "1" and last line is "100000".
    // Verify no truncation.

func TestCLIExecTimeout(t *testing.T)
    // exec --timeout 2 -- sleep 30
    // Verify exit code non-zero.
    // Verify completes in < 10s (not 300s default).

func TestCLICreateAllFlags(t *testing.T)
    // create --cpus 2 --memory 2048 --disk-size 4096
    //        --volume vol:/data --init "echo ok > /tmp/init"
    //        --env FOO=bar --keep-hot
    // Verify: inspect shows all resources.
    // Verify: exec -- cat /tmp/init → "ok".
    // Verify: exec -- printenv FOO → "bar".
    // Verify: exec -- ls /data → empty dir (volume mounted).

func TestCLIStopDestroyShortcut(t *testing.T)
    // Create → stop → destroy.
    // Verify stopped sandbox can be destroyed without start first.

func TestCLIInspectStoppedSandbox(t *testing.T)
    // Create → stop → inspect.
    // Verify status: stopped, stopped_at present.
    // Verify disk fields show "—" or "stopped" (no live df).
```

### What was cut and why

| Cut | Reason |
|-----|--------|
| `TestCLIExecBufferedWhenPiped` | Existing `TestCLIExec` already runs piped. Tests a negative. |
| `TestCLIErrorNotFound` | Existing `TestCLIExecNonexistentSandbox` + `TestCLIDestroyNonexistentSandbox` already cover. |
| `TestCLIHelpGroupHeaders` | Tests cobra's group rendering, not our code. |
| `TestCLICompletionScripts` | Tests cobra's `GenBashCompletion`. No bhatti logic. |
| `TestCLIVersionCheck` | Existing `TestCLIVersion` already covers; `--json` proven by `TestCLIJSONOutput`. |
| `TestCLIThermalCycleWithExec` | `TestCLILifecycleFullCycle` does 2 cycles; A6 engine tests do 5 with nginx. 30s+ of apt-get for no new signal. |
| `TestCLICreateDuplicateName` | Identical to `TestCLICreateIdempotent`. |
| `TestCLIInspectDiskUsage` | Merged into `TestCLIInspectRichOutput` — same VM, same inspect call. |
| `TestCLIInspectJSON` | Merged into `TestCLIInspectRichOutput` — text + JSON in one test. |
| `TestCLIStopStopIdempotent` | Merged into `TestCLIStopStartConfirmVerbs` — same lifecycle. |
| `TestCLIStartRunningIdempotent` | Merged into `TestCLIStopStartConfirmVerbs` — same lifecycle. |
| `TestCLIExecWithEnv` | Subsumed by `TestCLICreateAllFlags` which tests --env + --init + --volume + --cpus + --memory + --disk-size together. |

### Server-side prerequisites for B tests

Some tests require B implementation changes to pass. Tests should
be written first (TDD-style) and will fail until the corresponding
B item lands:

| Test | Blocks on |
|------|-----------|
| `TestCLICreateVerboseOutput` | B1 (new output format) |
| `TestCLICreateIdempotent` | B1 (idempotent create) |
| `TestCLIStreamingExecNDJSON` | B2 (CLI sends Accept header) |
| `TestCLIErrorExecOnStopped` | B3 (pattern-matched errors) |
| `TestCLIStopStartConfirmVerbs` | B3 (confirm verb format) |
| `TestCLIInspectRichOutput` | B4 (store columns + Sandbox struct + live df) |
| `TestCLIPorts` | B5 (ports CLI command) |
| `TestCLIListCleanDefault` | B6 (drop ID column) |
| `TestCLIListWideMode` | B6 (-o wide) + B4 (store columns) |
| `TestCLIForceStart` | B7 (server reads force param + CLI flag) |
| `TestCLIDetachedExec` | B9 (--detach flag) |
| `TestCLIHugepagesFlag` | B10 (--hugepages flag) |
| `TestCLIVolumeClone` | B11 (volume clone command) |

Tests not in this table can pass against the existing codebase
and should be written and merged first.

### Implementation order

1. Write all Tier 1 + Tier 2 + Tier 3 tests in `cli_ux_test.go`.
   Tests that depend on unshipped B items use `t.Skip("requires B<N>")`.
2. Ship each B item. Remove the corresponding `t.Skip`.
3. All tests green → tag v1.10.0.

### Coverage summary

| Category | Before B15 | After B15 |
|----------|------------|----------|
| Total CLI test functions | 54 | 82 |
| Lifecycle (stop/start) | 0 | 5 |
| Output format verification | 0 | 5 |
| Error message contracts | 0 | 2 |
| Streaming exec | 0 | 1 |
| New B commands (ports, volume clone, detach, hugepages, force) | 0 | 5 |
| Untested existing commands (inspect, edit, publish, share, admin) | 0 | 5 |
| Exit code contracts | 1 (partial) | 2 |
| Flag composition (--json, --timing, --force, -o wide) | 1 (partial) | 4 |
| Concurrent/stress | 0 | 2 |
| Full lifecycle round-trip | 0 | 2 |

---

## Dependency graph

```
Release A (v1.9.0 + v1.9.1) — SHIPPED

Release B (v1.10.0) — CLI/UX for HN launch

Server-side prerequisites (must land before dependent B items):
  Sandbox struct: add CPUs, MemoryMB, DiskSizeMB, Image, Thermal
  fields to store.Sandbox + sandboxCols query + JSON response.
  Needed by: B1, B4, B6.

  handleSandboxStart: read "force" param from request body,
  call engine.StartForce() when true. Needed by: B7.

Within B, dependencies:
  B4 (store columns) ← B6 (list -o wide needs resource columns)
  B4 (store columns) ← B1 (create output shows allocated resources)
  B4 (store columns) ← B15 tests that verify resource fields
  B1-B14 all land   ← B14 (docs rewrite)
  All B items        ← B15 (integration tests remove t.Skip gates)
  Everything else is independent.

Implementation order for HN:
  Phase 1 — Write all B15 tests (with t.Skip gates)       [day 1]
  Phase 2 — Server prereqs (Sandbox struct, force param)   [day 1]
  Phase 3 — B1 (create output)                             [day 2]
  Phase 4 — B3 (error hints + confirm verbs)               [day 2]
  Phase 5 — B2 (streaming exec)                            [day 2]
  Phase 6 — B4 (richer inspect)                            [day 3]
  Phase 7 — B6 (list -o wide)                              [day 3]
  Phase 8 — B5 (ports), B7 (--force), B9 (--detach)       [day 3]
  Phase 9 — B10 (hugepages), B11 (vol clone), B8 (Ctrl+C) [day 4]
  Phase 10 — B12 (dead code), B13 (build-rootfs.sh)        [day 4]
  Phase 11 — B14 (docs rewrite)                            [day 5]
  Phase 12 — Remove all t.Skip gates, all green → tag      [day 5]
```

---

## What's NOT in this plan

**Real systemd.** We tried it. Snapshot/restore breaks. Every
Firecracker deployment at scale uses a custom PID 1, not systemd.
The shim covers the package compatibility use case. If we ever
need full systemd (socket activation, complex dependencies, dbus
services), we'd revisit with VMGenID-based restore detection —
but that's a different project.

**`Type=notify` services.** Requires implementing the sd_notify
protocol (lohar listens on a socket, service sends READY=1).
openssh, nginx, postgres, redis all use `Type=simple` or
`Type=forking`. notify can be added later if needed.

**Socket activation.** No target packages need it.

**journalctl replacement.** Services write to stdout/stderr.
Users can check service output via `bhatti exec dev -- cat
/var/log/<service>.log` or we add log capture later.

**Timer units.** cron exists and works.

**Converting tier boot profiles to systemctl.** Docker tier's
`init.sh` starts dockerd via shell script. This continues to
work as-is. Converting to a `.service` file managed by the shim
is a follow-up.

**`--secret` / `--file` on create.** Needs server-side
`createSandboxReq` changes. Follow-up.

---

## Audit reference

| # | Gap | Fix | Release | Test |
|---|-----|-----|---------|------|
| 1 | `--force` flag referenced, doesn't exist | Wire server + CLI | B7 | `TestCLIForceStart` |
| 2 | Port discovery: server exists, no CLI | `bhatti ports` | B5 | `TestCLIPorts` |
| 3 | Detached exec: server supports, no CLI | `--detach` flag | B9 | `TestCLIDetachedExec` |
| 4 | Streaming exec: server supports NDJSON, CLI never requests | Stream when TTY | B2 | `TestCLIStreamingExecNDJSON` |
| 5 | Hugepages: API field, no CLI flag | `--hugepages` flag | B10 | `TestCLIHugepagesFlag` |
| 6 | Labels: dead field | Delete | B12 | — |
| 7 | UserData: dead field | Delete | B12 | — |
| 8 | Files in create: works, no CLI flag | Deferred | — | — |
| 9 | Secrets in create: works, no CLI flag | Deferred | — | — |
| 10 | Volume clone: server works, no CLI | `bhatti volume clone` | B11 | `TestCLIVolumeClone` |
| 11 | Template CRUD: server works, CLI only consumes | Deferred | — | — |
| 12 | Task status: server works, no CLI | Deferred | — | — |
| 13 | Health endpoint: no CLI | Not needed | — | — |
| 14 | cli-reference.md: 16/55+ commands documented | Rewrite | B14 | — |
| 15 | Image pull: no Ctrl+C handling | Signal trap | B8 | — (signal behavior, manual) |
| 16 | build-rootfs.sh: needs systemctl symlink | Update | B13 | — |
| 17 | Disk space not visible in create/inspect/list | Show in output | B1/B4/B6 | `TestCLIInspectRichOutput`, `TestCLIListWideMode` |
| 18 | `Sandbox` struct missing CPUs/Memory/Disk/Image/Thermal | Add to struct + query | B4 prereq | `TestCLIInspectRichOutput` |
| 19 | `handleSandboxStart` ignores force param | Read param, call `StartForce()` | B7 prereq | `TestCLIForceStart` |
| 20 | stop/start lifecycle: zero CLI test coverage | Add tests | B15 | `TestCLIStopStartRoundTrip`, `TestCLILifecycleFullCycle` |
| 21 | inspect command: zero test coverage | Add tests | B15 | `TestCLIInspectRichOutput`, `TestCLIInspectStoppedSandbox` |
| 22 | edit command: zero test coverage | Add tests | B15 | `TestCLIEditKeepHot` |
| 23 | publish/unpublish: zero test coverage | Add tests | B15 | `TestCLIPublishUnpublish` |
| 24 | share: zero test coverage | Add tests | B15 | `TestCLIShareRevoke` |
| 25 | admin commands: zero test coverage | Add tests | B15 | `TestCLIAdminStatus` |
| 26 | Exit code contract not verified | Add tests | B15 | `TestCLIExitCodeContract` |
| 27 | Create with all flags: untested combo | Add test | B15 | `TestCLICreateAllFlags` |
