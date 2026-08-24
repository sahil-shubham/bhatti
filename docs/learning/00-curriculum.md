# The Curriculum: Covering the Gap

This is the map. Ten domains, sequenced bottom-up. Each domain has a short "why
this matters in bhatti" framing, then modules. Each **module** is one study unit
with the same shape:

- **Concept** — the first-principles idea, in one or two sentences.
- **Resource** — one finishable thing to study. (R) = read, (V) = video/visual,
  (M) = man page / spec.
- **Your code** — where bhatti does this. Open these files.
- **War story** — the real incident that teaches it (→ [`war-stories.md`](./war-stories.md)).
- **Lab** — what you build / break / instrument.
- **Mastery check** — answer without looking.

> Difficulty: ★ foundational · ★★ intermediate · ★★★ deep / the stuff that
> separates "wrote it" from "can debug it in prod."

A note on resources: I've named specific, *finishable* things. OSTEP (Operating
Systems: Three Easy Pieces) is free at `pages.cs.wisc.edu/~remzi/OSTEP/` and is
your spine for OS theory — chapters are ~15 pages each. `man` pages on your Linux
box are primary sources; read them. Julia Evans (`jvns.ca`) is the best on-ramp
for networking and debugging tooling.

---

## Domain 0 — The debugging mindset (do this first, it's short)

Before any topic: the meta-skill. Production debugging is not knowledge, it's a
*procedure* under uncertainty. Your sessions show you already have good
instincts ("we should never be making an assumption, everything can be tested and
fact backed" — your words, boot-time session). Let's make them explicit.

### Module 0.1 — Observe, hypothesize, test, narrow ★

- **Concept**: A bug is a difference between your mental model and reality. You
  close that gap by *measuring*, not guessing. Every step either confirms or
  kills a hypothesis. You never change two things at once.
- **Resource**: (R) Julia Evans, "How to be a wizard programmer" + her debugging
  zine notes, `jvns.ca/debugging-zine/`. (R) Brendan Gregg, "Linux Performance"
  USE method intro, `brendangregg.com/usemethod.html`.
- **Your code / sources**: read three of your own investigations end to end:
  `docs/archive/INVESTIGATION-create-performance.md`,
  `docs/archive/INVESTIGATION-cold-wake-cache.md`,
  `docs/archive/SNAPSHOT-RELIABILITY-TRACE.md`. Notice the structure: trigger →
  method → instrument → measure → conclude. That *is* the procedure.
- **War story**: the rory incident (W1) — watch how the diagnosis narrows from
  "user can't shell in" to "FC process dies in <1s after restore" to "dirty-page
  tracking corrupted the snapshot." Each step is a measurement.
- **Lab**: Take any past bug from `war-stories.md`. Before reading the
  resolution, write your own hypothesis tree: "if X, I'd expect to see Y; I'd
  test it with Z." Then compare to what actually happened.
- **Mastery check**: Name the four tools you reach for *in order* when a process
  is hung. (Suggested: `ps`/`top` → `strace -p` / `cat /proc/PID/stack` →
  `lsof -p` → `dmesg`.) Why that order?

### Module 0.2 — The tools that see everything ★★

- **Concept**: You can't debug what you can't observe. A handful of tools expose
  the kernel/userspace boundary: `strace` (syscalls), `ltrace` (libcalls), `/proc`
  (kernel's view of every process), `dmesg` (kernel ring buffer), `ss`/`ip`
  (network), `perf`/`bpftrace` (sampling/tracing).
- **Resource**: (M) `man strace`, `man proc` (yes, the whole thing — it's a map
  of the kernel's exposed state). (R) Brendan Gregg's `bpftrace` one-liners.
- **Your code**: `docs/archive/INVESTIGATION-create-performance.md` shows you
  instrumenting six phases with `slog.Debug` behind `BHATTI_LOG_LEVEL=debug`.
  That's app-level tracing; now learn the kernel-level equivalents.
- **Lab**: On your Pi or a local VM: `strace -f -e trace=clone,execve,wait4 ls`.
  Watch fork+exec happen. Then `strace -f bhatti exec dev -- echo hi` on the
  *host* side and find the network syscalls. Then inside a sandbox,
  `cat /proc/1/status` — that's lohar.
- **Mastery check**: A process is stuck. `cat /proc/PID/stack` shows it in
  `__refrigerator` or a `D` state in `ps`. What does uninterruptible sleep (`D`)
  mean and why can't you `kill -9` it?

---

## Domain 1 — Processes, signals, and the init system (lohar)

**Why it matters here:** lohar is PID 1 in every VM. It forks every command,
reaps every child, owns every PTY, and — in `cmd/lohar/systemctl.go` (56KB!) —
*reimplements a usable slice of systemd*. This is the most "Unix" part of the
codebase and the foundation for everything. **Fully built out in
[`module-01-processes-and-init.md`](./module-01-processes-and-init.md).**

### Module 1.1 — fork, exec, wait, exit codes ★
- **Concept**: `fork()` clones a process; `execve()` replaces its image; `wait()`
  collects its death. Go's `exec.Command{}.Start()` is fork+exec; `.Wait()` is
  wait4.
- **Resource**: (R) OSTEP Ch.5 "Process API" (15 pp).
- **Your code**: `cmd/lohar/exec.go` (`handlePipedExec`, `exitCodeFromErr`).
- **Lab**: write `fork`/`execve`/`waitpid` in C *and* in Go; decode a
  signal-terminated exit code (128 + signum — exactly what `exitCodeFromErr` does).
- **Mastery check**: what is exit code 137? 139? Why `128 + signal`?

### Module 1.2 — Signals and process groups ★★
- **Concept**: signals are async notifications. `SIGKILL`/`SIGSTOP` can't be
  caught. A process *group* lets you signal a whole tree. `Kill(-pid)` signals
  the group.
- **Resource**: (M) `man 7 signal`, `man 2 setpgid`.
- **Your code**: `cmd/lohar/exec.go` — `Setpgid: true` then
  `syscall.Kill(-cmd.Process.Pid, SIGKILL)`. **Teaching bug**: `proto/constants.go`
  says `KILL` "sends SIGTERM"; the code sends `SIGKILL`. Doc/code drift — find it.
- **War story**: W4 (the two bricked Pis) — a `SIGTERM` handler that calls
  `reboot(POWER_OFF)` is fine in a VM and catastrophic on a host.
- **Lab**: spawn `sleep 1000 & sleep 1000` under a shell, kill the group vs the
  leader, watch orphans with `ps -o pid,ppid,pgid`.
- **Mastery check**: you cancel `bhatti exec dev -- npm install`. Why does the
  `node` child die too? What flag makes that work?

### Module 1.3 — PID 1: mounts, the reboot handler, zombie reaping ★★★
- **Concept**: PID 1 is special. It never gets reparented, it's the default
  parent of orphans, and the kernel panics if it dies. It must mount `/proc`,
  `/sys`, `/dev`, handle `SIGTERM`, and (classically) reap zombies.
- **Resource**: (R) "What is PID 1" + Rich Felker's `minit`/`dumb-init` rationale.
  (M) `man 2 reboot`, `man 2 mount`.
- **Your code**: `cmd/lohar/main.go` `runAgent()` — the `mustMount` sequence,
  the `os.Getpid() != 1` guard, `select{}` at the end. Note the *deliberate*
  decision in `decisions.md §3` to **skip zombie reaping** (Go's runtime races
  with a manual `Wait4(-1)`).
- **War story**: W4.
- **Lab**: in a throwaway VM, write a 30-line Go PID-1 that mounts `/proc` and
  `select{}`s; boot a kernel into it with `init=/your/bin`. Watch it be PID 1.
- **Mastery check**: why does `runAgent` end in `select{}`? What happens to a VM
  if lohar exits? Why is skipping zombie reaping acceptable *here* specifically?

### Module 1.4 — PTYs, sessions, and SIGHUP ★★★
- **Concept**: a pseudo-terminal is a master/slave pair; the kernel's tty line
  discipline sits between them doing echo, line editing, `^C`→SIGINT. Closing the
  master sends SIGHUP to the foreground group. Detaching *without* closing keeps
  the job alive.
- **Resource**: (R) "The TTY demystified" (Linusakesson). (R) yakout.io
  "Terminal under the hood". (M) `man 4 pts`, `man 3 openpty`.
- **Your code**: `cmd/lohar/tty.go`, `cmd/lohar/session.go`, the 64KB ring buffer
  (`pkg/engine/firecracker/ringbuffer.go`). `decisions.md §4` ("Exec is sessions")
  is the rationale — born from your SSH-to-the-Pi dropping mid-`npm install`.
- **Lab**: open `/dev/ptmx`, `grantpt`/`unlockpt`, run a shell on the slave, proxy
  bytes. Then drop the master and watch SIGHUP kill the shell; then *don't* and
  watch it survive.
- **Mastery check**: how does a bhatti session survive a host disconnect when an
  SSH session wouldn't? Where does output go while nobody's attached?

### Module 1.5 — Reimplementing systemd: units, dependency graphs, conditions ★★★
- **Concept**: an init system is a dependency-ordered service supervisor. systemd
  units declare `Wants`/`Requires`/`After`; the manager topologically sorts and
  starts them, honoring `Condition*` gates and `cgroup` placement.
- **Resource**: (R) `man systemd.unit`, `man systemd.service`; Lennart
  Poettering's "systemd for Administrators" parts 1–3.
- **Your code**: the shim you wrote — `cmd/lohar/systemctl.go` (56KB),
  `unit.go` (parser), `depgraph.go` (topo-sort), `conditions.go`, `cgroup.go`,
  `tmpfiles.go`, `spawn.go` (the cgroup-placement race fix for forking daemons),
  `notify.go` (sd_notify). This is a *huge* learning surface you built mostly
  blind.
- **War story**: W8 (fastidious's perf comment → "are we doing short-term fixes
  or going about it architecturally?" → the systemd-rc plan).
- **Lab**: write a 100-line init that parses three unit files with `After=` and
  starts them in order. Then read `depgraph.go` and find where it handles cycles.
- **Mastery check**: why does a forking daemon need `lohar spawn` (the helper)
  instead of a plain `exec.Command`? (Hint: cgroup placement race — who's in the
  cgroup when the parent forks and exits?)

---

## Domain 2 — Virtualization and the VMM

**Why it matters here:** this is the literal core. Firecracker is a VMM on KVM;
you talk to it over an HTTP-on-Unix-socket API; you've since built a VMM
*abstraction* (`pkg/engine/krucible`) and even a toy VMM on macOS's
Hypervisor.framework (`cmd/vmm`). Understanding what a VMM *is* makes snapshots,
the boot args, and the device model obvious instead of magic.

### Module 2.1 — Hardware virtualization: VT-x, EPT, /dev/kvm ★★★
- **Concept**: the CPU runs guest code *natively*. Privileged instructions and
  faults *trap* (VM-exit) to the VMM. Two-level page tables (EPT/NPT) virtualize
  physical memory. `/dev/kvm` is the kernel interface a VMM uses to set this up.
- **Resource**: (R) LWN "KVM" intro; (R) the KVM API doc
  `Documentation/virt/kvm/api.rst` (skim the ioctls: `KVM_CREATE_VM`,
  `KVM_CREATE_VCPU`, `KVM_RUN`). (V) "Build a tiny VMM" talks.
- **Your code**: `cmd/vmm/main.go` (your HVF experiment — the macOS analog of
  KVM), `pkg/engine/krucible/` (the abstraction over FC/libkrun).
- **War story**: W9 (VMM-agnostic / libkrun fork investigation — why you'd even
  want to swap the VMM).
- **Lab**: read/run a ~200-line KVM "hello world" (there are several; e.g. the
  classic `kvm-hello-world`). Make a vCPU execute a few instructions and handle
  one VM-exit for `outb`. *This is the single best demystifier in the whole
  curriculum.*
- **Mastery check**: when a guest reads from a virtio-net device register, what
  sequence of events crosses the guest/host boundary? Where does Firecracker get
  control?

### Module 2.2 — Firecracker: the minimal VMM and its API ★★
- **Concept**: Firecracker is ~50k lines of Rust: KVM + a handful of virtio
  devices (block, net, vsock, balloon) + an HTTP control API on a Unix socket.
  No BIOS, no PCI, no legacy. That minimalism is the speed.
- **Resource**: (R) Firecracker `docs/design.md` (15 min). (R) the NSDI'20 paper
  "Firecracker: Lightweight Virtualization for Serverless" (the *why*).
- **Your code**: `pkg/engine/firecracker/fc.go` (process spawn), `create.go`
  (the `fcPut` sequence — trace the ~8 API calls), `decisions.md §2` (why you
  refused the 15k-line Go SDK and write JSON by hand).
- **Lab**: by hand, `curl --unix-socket` a boot-source + drive + network +
  `InstanceStart` to a raw Firecracker. Boot a kernel with no orchestrator. Feel
  how little there is.
- **Mastery check**: list the API endpoints `create.go` hits, in order. What's
  the last one and why must it be last?

### Module 2.3 — virtio: how a guest gets a disk and a NIC ★★★
- **Concept**: virtio is a paravirtualized device standard — guest and host share
  ring buffers (virtqueues) in guest memory; the guest "kicks", the host
  processes, signals back via interrupt. `virtio-blk` = your `rootfs.ext4`,
  `virtio-net` = your TAP, `virtio-vsock` = host↔guest socket, `virtio-balloon` =
  memory reclaim.
- **Resource**: (R) "Virtio: An I/O virtualization framework" (Rusty Russell's
  paper) or the OASIS virtio spec intro; (R) Redhat "Introduction to virtio".
- **Your code**: the device JSON in `create.go`; balloon in the thermal path
  (Domain 3); `decisions.md §1` (vsock) and §11 (net).
- **War story**: W1 (the rory snapshot corruption is *literally* about host-side
  virtio ring writes being missed by dirty-page tracking).
- **Mastery check**: why does `virtio-net` survive snapshot/restore but
  `virtio-vsock` doesn't? (This is the crux of W3.)

---

## Domain 3 — Snapshots, memory, and thermal states

**Why it matters here:** the "warm wake in 3.7ms" headline feature *is* snapshots.
And the worst bug in the project's history (rory) *was* snapshots. This domain is
where systems theory has the highest stakes for you.

### Module 3.1 — What a snapshot actually is ★★
- **Concept**: a Firecracker snapshot = a small device-state file (vCPU regs,
  virtio device state) + a full guest-RAM dump (`mem.snap`). Restore = spawn a
  new FC, mmap the memory file, reload device state, resume vCPUs.
- **Resource**: (R) Firecracker `docs/snapshotting/snapshot-support.md`.
- **Your code**: `pkg/engine/firecracker/snapshot.go` (`Checkpoint`,
  `ResumeSnapshot`), `lifecycle.go` (`Stop`/`Start`).
- **Lab**: snapshot a VM, `ls -la` the snapshot dir, note `mem.snap` ≈ RAM size.
  Restore it, `cat` a file you wrote pre-snapshot. Then `hexdump` the device-state
  file.
- **Mastery check**: why does `Stop()` pause vCPUs *before* snapshotting? What
  would a snapshot taken of a running vCPU look like?

### Module 3.2 — Diff vs Full snapshots & dirty-page tracking ★★★ (THE big one)
- **Concept**: a diff snapshot writes only pages dirtied since the last one,
  tracked by the CPU's dirty bit via `KVM_GET_DIRTY_LOG`. It's faster and
  smaller — *if* every memory write is tracked. Host-side virtio writes into
  guest RAM can be missed, producing a snapshot that restores into a corrupt
  kernel that dies in ~1 second.
- **Resource**: (R) re-read snapshot-support.md's diff section; (R) `KVM_GET_DIRTY_LOG`
  in the KVM API doc; (R) your own `docs/archive/PLAN-reliability.md` and
  `SNAPSHOT-RELIABILITY-TRACE.md`.
- **Your code**: `engine.go` — `track_dirty_pages: false`, all snapshots Full.
  `decisions.md` predecessor note + the reliability plan explain *why*.
- **War story**: **W1 — read it twice.** This is the flagship. A user (kowshik's
  sandbox "rory") couldn't shell in; FC died <1s after every restore; root cause
  was diff-snapshot corruption compounded by a network change.
- **Lab**: reproduce conceptually — enable dirty tracking on a test build, snapshot
  a VM doing heavy I/O, restore, observe instability. (Disposable VM only.)
- **Mastery check**: explain to a colleague why "Full snapshots, always" was the
  fix and what you gave up (speed/space) to get reliability.

### Module 3.3 — Memory ballooning and density ★★★
- **Concept**: each VM reserves its full `mem_size_mib` from host RAM forever — a
  4GB idle VM wastes 4GB. The balloon device lets the host *reclaim* unused guest
  pages; `deflate_on_oom` gives them back under pressure. This is the difference
  between 30 and 200 sandboxes on one box.
- **Resource**: (R) Firecracker `docs/ballooning.md`; (R) the original VMware
  "ballooning" idea in the ESX memory-management paper (the canonical source).
- **Your code**: balloon install in `create.go`, inflate/deflate in the thermal
  path; `CONFIG_VIRTIO_BALLOON` in the kernel (Domain 7).
- **War story**: surfaced *inside* W1 — a user raised ballooning concerns; you
  reasoned through "does balloon state survive snapshot/restore?" (it interacts).
- **Lab**: boot a VM with a balloon, inflate it via the FC API, watch host RSS
  drop with `cat /proc/<fc-pid>/status` (VmRSS). Deflate, watch it return.
- **Mastery check**: ballooning is *cooperative*. What does that mean for a
  malicious guest, and why is it still safe for *your* threat model?

### Module 3.4 — Thermal state machine: hot/warm/cold & the cold-wake cost ★★
- **Concept**: hot (running) → warm (vCPUs paused, RAM resident, ~4ms wake) →
  cold (snapshotted to disk, RAM freed, ~360ms wake including page-in). Any API
  request transparently wakes it.
- **Resource**: your `docs/thermal-management.md` + the bhatti.sh thermal page;
  (R) OSTEP Ch.21–22 (paging/swapping) for *why* cold-wake page-in costs what it
  does.
- **Your code**: `pkg/server/server.go` `runThermalCycle`, `engine.go`
  `EnsureHot`/`Pause`/`Resume`, `decisions.md §9` (host-side activity cache).
- **War story**: W7 — the cold-wake page-cache investigation. "42ms p50" vs
  "360ms p50" were both real but measured different things; the docs weren't
  honest about it.
- **Lab**: drop caches (`echo 3 > /proc/sys/vm/drop_caches`), cold-wake a VM,
  time it; then warm-wake, time it. Reproduce the 100x gap and explain it via
  page faults.
- **Mastery check**: a warm VM holds full RAM; a cold one doesn't. So what is the
  warm→cold transition actually *buying* and *costing*?

---

## Domain 4 — Networking

**Why it matters here:** every sandbox needs internet + isolation, the reverse
proxy gives preview URLs, and a single careless `iptables`/TAP command once took
down a user (W5). Networking bugs are the scariest because they're invisible
until you `tcpdump`.

### Module 4.1 — TAP devices and Linux bridges (L2) ★★
- **Concept**: a TAP device is a virtual Ethernet cable — one end in the VM, one a
  file descriptor on the host. A bridge is a virtual switch. Put each user's TAPs
  on their own bridge and users are isolated at layer 2 — they can't even see each
  other's MACs.
- **Resource**: (R) Redhat "Introduction to Linux interfaces for virtual
  networking"; (V) Julia Evans networking posts.
- **Your code**: `pkg/engine/firecracker/network.go` — `createTapDevice`,
  `ensureUserBridge`, `subnetFromIndex`, `UserNetwork`. `decisions.md §11`.
- **Lab**: `ip tuntap add`, `ip link add br0 type bridge`, attach, ping across.
  Then create two bridges and confirm a VM on one can't ARP a VM on the other.
- **Mastery check**: how does `subnetFromIndex` give user 1 a different /24 than
  user 2, and why per-user bridges instead of one shared bridge?

### Module 4.2 — iptables, NAT, and the FORWARD chain ★★★
- **Concept**: packets *routed through* the host (not *to* it) traverse the
  FORWARD chain. MASQUERADE rewrites source IPs so private VM IPs can reach the
  internet. Rule *order* matters — `-I FORWARD 1` inserts at the top.
- **Resource**: (R) Arch Wiki iptables (the packet-flow diagram is essential);
  (M) `man iptables-extensions` (MASQUERADE, conntrack).
- **Your code**: `network.go` `setupGlobalFirewall` (the 6 global rules — know
  what each does); the per-bridge masquerade.
- **War story**: **W5** — you ran a *test* bhatti instance that deleted *all* TAP
  devices globally, severing every user's connectivity, which then *compounded*
  with the rory snapshot bug. A one-command blast radius.
- **Lab**: set up MASQUERADE for a VM, `tcpdump -i br0` while it pings 8.8.8.8,
  watch the SNAT. Then delete the rule and watch it break. (Throwaway box.)
- **Mastery check**: why `-I FORWARD 1` and not `-A FORWARD`? What's in the 6
  global rules and which one provides the cross-user block?

### Module 4.3 — The kernel `ip=` trick (chicken-and-egg) ★★
- **Concept**: the host detects "VM ready" by polling lohar over TCP — but lohar
  needs an IP first, and the host can't tell it the IP because it can't reach it
  yet. Solution: the kernel `ip=` boot parameter configures the NIC during early
  boot, *before* init runs.
- **Resource**: (M) `Documentation/admin-guide/kernel-parameters.txt` (search
  `ip=`); (R) `decisions.md §11`.
- **Your code**: boot-args construction in `create.go`; `cmd/lohar/net.go`.
- **Mastery check**: why not have lohar do DHCP or configure its own IP? Walk the
  dependency cycle.

### Module 4.4 — The reverse proxy, preview URLs, and the WebSocket gotcha ★★★
- **Concept**: `publish` maps `dev-k3m9x2.bhatti.sh` → a port in a (possibly cold)
  VM. The proxy must auth, wake the VM, stream HTTP *and* upgrade WebSockets, and
  not buffer. Intermediaries (Cloudflare) can silently break long-lived upgrades.
- **Resource**: (R) MDN "Protocol upgrade mechanism" (WebSocket `Upgrade`/`101`);
  (R) the relevant Cloudflare proxy/WebSocket docs.
- **Your code**: `pkg/server/` public proxy (`public_proxy.go` and friends), the
  share/WS path.
- **War story**: **W6** — `bhatti share` stuck on "connecting"; the WebSocket
  upgrade didn't survive the path through Cloudflare. You researched whether
  others hit it before patching.
- **Lab**: write a 60-line Go reverse proxy that proxies HTTP and upgrades a WS
  connection; break it by buffering the response and watch the upgrade hang.
- **Mastery check**: what's different about proxying a WebSocket vs a normal
  request? Where in the path did W6 actually break?

### Module 4.5 — The ARP trick after restore & DNS forwarding ★★
- **Concept**: after restore, the network around the VM may have stale ARP
  caches (the VM "moved"). A gratuitous ARP re-announces the MAC↔IP binding.
  DNS inside the VM is forwarded to a host resolver.
- **Resource**: (R) "Gratuitous ARP" explainer; (M) `man 7 arp`.
- **Your code**: ARP handling in `network.go`; `pkg/dns/`, `cmd/lohar/net.go`
  resolv handling; `docs/internal/PLAN-dns-forwarding.md`.
- **Mastery check**: why might a restored VM's first outbound packet get dropped
  without a gratuitous ARP?

---

## Domain 5 — The wire protocol and host↔guest comms

**Why it matters here:** every exec, file op, and shell rides a tiny binary
protocol you designed. It's small enough to understand *completely* — a rare
chance to fully own a protocol end to end.

### Module 5.1 — Binary framing and the interleaving trap ★★
- **Concept**: `[4-byte length][1-byte type][payload]`. The whole frame must be
  written in *one* `Write()` — otherwise two goroutines (stdout + stderr) interleave
  bytes and corrupt the stream.
- **Resource**: (R) HTTP/2 framing, RFC 7540 §4.1 (same idea, more features —
  validates your design); (R) "Length-prefixed message framing" basics.
- **Your code**: `pkg/agent/proto/frame.go` (`WriteFrame` assembles one buffer),
  `constants.go` (the type table), `exec.go` (the `tx chan frameMsg` that
  serializes writes).
- **Lab**: write a framer; then deliberately split a frame into 3 `Write()`s
  across 2 goroutines and watch it corrupt. Then fix it with a channel like yours.
- **Mastery check**: what exactly does the `tx` channel in `handlePipedExec`
  prevent? Why is `WriteFrame`'s single-buffer assembly not enough on its own when
  multiple goroutines call it?

### Module 5.2 — Connection lifecycle, auth, and content-negotiated streaming ★★
- **Concept**: first frame after connect is `AUTH` (a token). Exec can be
  buffered or streamed (NDJSON) via `Accept` negotiation. Port-forwarding
  *abandons* framing after the handshake and goes raw.
- **Resource**: (R) `decisions.md §6` (server-side truncation) and §7 (NDJSON vs
  SSE/WebSocket).
- **Your code**: `pkg/agent/client.go` (`WaitReady`, AUTH), `forward.go`,
  `pkg/server` exec handler.
- **Mastery check**: why does the forward protocol drop framing after handshake?
  What would framing cost you on a high-throughput tunnel?

---

## Domain 6 — Filesystems, storage, and durability

**Why it matters here:** kowshik's files got corrupted (W2); atomic writes and
`fsync` are why that isn't routine; SQLite-WAL is why the thermal manager can
write while the API reads. Durability bugs are silent until a crash.

### Module 6.1 — Crash consistency: write, page cache, fsync, rename ★★★
- **Concept**: `write()` lands in the page cache (RAM), not disk. `fsync()` forces
  it down. `rename()` is atomic on POSIX. Temp-file + fsync + rename = a reader
  never sees a half-written file, even across a crash.
- **Resource**: (R) OSTEP Ch.42 "Crash Consistency / journaling" (free PDF). (R)
  "Files are hard" (Dan Luu).
- **Your code**: `cmd/lohar/files.go` (atomic write), `decisions.md §5`. Note the
  ordering subtlety: fsync the *data* before rename, or the renamed file can exist
  with zero bytes after a crash.
- **War story**: **W2** — kowshik couldn't edit certain files; they were corrupt
  on a rootfs that had been resumed from a dirty (diff) snapshot. Storage
  corruption + snapshot corruption, intertwined.
- **Lab**: write a file *without* fsync, pull power on a VM (or `echo b >
  /proc/sysrq-trigger` in a disposable VM), observe loss/zeros. Then with fsync.
- **Mastery check**: you skip fsync and just write+rename. Construct the exact
  crash window where a reader sees an empty file.

### Module 6.2 — ext4 rootfs images, overlays, and btrfs ★★
- **Concept**: a rootfs is a file containing an ext4 filesystem, attached as
  virtio-blk. btrfs on the host gives you `cp --reflink` (CoW clones — instant
  copies sharing blocks), compression, and snapshots — which is why create-time
  and storage scale the way they do.
- **Resource**: (R) ext4 wiki overview; (R) btrfs `man 5 btrfs` + the reflink/CoW
  docs; your `docs/archive/MIGRATION-v0.5.14-btrfs.md`.
- **Your code**: rootfs build in `scripts/build-rootfs.sh`, image handling in
  `pkg/oci/`, create path's reflink usage.
- **War story**: W10 (create/boot performance) — much of the speedup depends on
  btrfs reflink; you checked "does this hurt people who bring their own image?"
- **Lab**: `mkfs.ext4` a file, loop-mount it, put files in, detach, boot a VM off
  it. Then `cp --reflink=always` it and compare `du` vs `df`.
- **Mastery check**: why is creating from a btrfs reflink near-instant while a
  plain `cp` of a 600MB rootfs is not?

### Module 6.3 — SQLite, WAL, and pure-Go ★★
- **Concept**: WAL (write-ahead logging) lets readers see a consistent snapshot
  without blocking the writer. The thermal manager writes state while the API
  reads — WAL is what keeps that from deadlocking or blocking.
- **Resource**: (R) `sqlite.org/wal.html` (15 min). (R) `decisions.md §10` (why
  pure-Go `modernc.org/sqlite` for CGO-free cross-compilation).
- **Your code**: `pkg/store/store.go`.
- **Mastery check**: with the default rollback journal (not WAL), what happens
  when the thermal manager writes while an API request reads?

### Module 6.4 — Config drive: bootstrapping before the agent exists ★★
- **Concept**: the VM needs hostname, token, env, secrets, and an init script
  *before* lohar can accept connections. A tiny read-only ext4 "config drive" is
  attached and read during early boot, then unmounted (hardening).
- **Resource**: your `docs/wire-protocol.md` + config-drive schema discussion
  (session idx 114 asked exactly this — "what's the schema for the config drive?").
- **Your code**: `pkg/engine/firecracker/configdrive.go`.
- **Mastery check**: name three things on the config drive the agent *can't* get
  via exec, and explain why (the agent doesn't exist yet at read time).

### Module 6.5 — Secrets at rest: age encryption ★★
- **Concept**: secrets are encrypted on disk with `age` (modern, simple,
  X25519). Decrypted only into the config drive at create/restore time.
- **Resource**: (R) the `age` spec/README; (R) intro to authenticated encryption.
- **Your code**: `pkg/secrets/age.go`.
- **Mastery check**: where does the plaintext secret exist, for how long, and who
  can read it?

---

## Domain 7 — The kernel itself

**Why it matters here:** you compile your own guest kernel. Boot time, FUSE,
ballooning, multi-arch (`binfmt_misc`), and serial-console-off are all
*kernel-config* decisions. This is the closest you get to "debug a kernel issue."

### Module 7.1 — Kernel config, building, and boot args ★★★
- **Concept**: the kernel `.config` decides which drivers/features compile in.
  Boot args (`init=`, `ip=`, `console=`, `8250.nr_uarts=0`, `quiet`) shape early
  boot. A minimal config boots faster and is smaller.
- **Resource**: (R) the kernel docs `admin-guide/kernel-parameters.txt`; (R) a
  "build a minimal kernel for Firecracker" guide; your `docs/kernel.md`.
- **Your code**: `scripts/build-kernel.sh`, the kernel config it uses, boot-args
  in `create.go`.
- **War story**: W10 — boot/create perf; serial console disabled via
  `8250.nr_uarts=0` (the old `console=ttyS0` slowed boot and the doc was stale).
- **Lab**: build a Firecracker guest kernel from a config; flip one option (e.g.
  enable `CONFIG_FUSE_FS`), rebuild, boot, confirm `/dev/fuse` works.
- **Mastery check**: which kernel options does bhatti *need* on (FUSE, balloon,
  binfmt_misc, cgroup2, virtio-*) and what's the cost of each for boot/size?

### Module 7.2 — FUSE: filesystems in userspace ★★★
- **Concept**: FUSE lets a userspace program implement a filesystem; the kernel's
  `fuse` module forwards VFS calls to it over `/dev/fuse`. Needs the kernel option
  *and* the device node (lohar `chmod 0666 /dev/fuse` in `main.go`).
- **Resource**: (R) the libfuse "hello world" example; (R) kernel `filesystems/fuse.rst`.
- **Your code**: `/dev/fuse` setup in `cmd/lohar/main.go`; your
  `docs/archive/PLAN-fuse-support.md`.
- **Lab**: write a 100-line FUSE "hello" fs (Python `fusepy` or Go `bazil/fuse`),
  mount it inside a sandbox.
- **Mastery check**: trace an `ls` on a FUSE mount from the syscall to your
  userspace handler and back.

### Module 7.3 — binfmt_misc and multi-arch ★★
- **Concept**: `binfmt_misc` lets the kernel hand a foreign-arch ELF to a
  userspace interpreter (qemu-user). That's how `docker buildx` builds arm64 on
  x86 *inside* a sandbox.
- **Resource**: (R) kernel `admin-guide/binfmt-misc.rst`; (R) `tonistiigi/binfmt`
  README.
- **Your code**: the `binfmt_misc` mount in `cmd/lohar/main.go`.
- **War story**: session idx 125 — "can bhatti do multi-arch? can I `docker
  buildx` arm images inside?" Yes, *because* of this mount.
- **Mastery check**: when you run an arm64 binary on your x86 sandbox, what does
  the kernel do at `execve` time?

---

## Domain 8 — Concurrency, state machines, and reliability

**Why it matters here:** bhatti is a concurrent daemon managing dozens of VMs.
Get a lock wrong and a hung shell stalls every snapshot. This is where Go's
memory model meets distributed-systems thinking.

### Module 8.1 — The Go memory model & capture-and-release locking ★★★
- **Concept**: a mutex unlock *happens-before* the next lock — that's the only
  guarantee. bhatti uses "capture the pointer under lock, release, then do the
  slow call" so an hours-long shell doesn't hold a VM's lock.
- **Resource**: (R) `go.dev/ref/mem` (one page); (R) "The Go Memory Model" talk.
- **Your code**: `decisions.md §8`; per-VM `stateMu` usage in `engine.go`;
  `cmd/lohar/tty_race_test.go` (a real race you wrote a test for).
- **Lab**: run `go test -race ./...` on bhatti; introduce a deliberate race and
  watch the detector catch it.
- **Mastery check**: `Destroy()` is called while `Exec()` holds a captured `Agent`
  pointer. Why is that safe? What invariant makes it safe?

### Module 8.2 — State machines, circuit breakers, and graceful degradation ★★★
- **Concept**: a VM moves through states (hot/warm/cold/unknown); transitions can
  fail. A circuit breaker (`restoreFailed`) stops hammering a broken VM; a
  force-pause after N activity failures prevents a stuck VM from blocking the
  cycle; `SnapshotAll` retries once then *leaves the VM running* (live > dead).
- **Resource**: (R) "Release It!" circuit-breaker pattern summary; (R) your
  `docs/archive/PLAN-reliability.md` and `RELIABILITY-AUDIT.md`.
- **Your code**: `pkg/server/server.go` (`runThermalCycle`, `SnapshotAll`),
  `engine.go` thermal failure counters.
- **War story**: W1 again — the failure that *had no circuit breaker* (FC dying in
  a loop, zombies piling up) is what motivated these.
- **Lab**: draw the complete state machine *including* error states. Where does a
  VM go after a failed restore? After a double SnapshotAll failure?
- **Mastery check**: why does `SnapshotAll` leave a VM running on double failure
  instead of killing it? What's the failure mode that policy accepts?

### Module 8.3 — Crash recovery on daemon restart ★★
- **Concept**: SQLite is the source of truth. On boot, the daemon reloads all
  sandboxes, verifies snapshot files exist, and marks unrecoverable ones
  "unknown" rather than crashing.
- **Resource**: your `docs/archive/PLAN-snapshot-recovery.md`.
- **Your code**: `recoverVMs` in the engine; startup path in `cmd/bhatti/main.go`.
- **War story**: W1 — rory was "recovered" into a state where it existed in the DB
  but its FC kept dying; recovery surfaced the corruption rather than hiding it.
- **Mastery check**: after a hard `kill -9` of the daemon (no graceful
  `SnapshotAll`), what state are hot VMs in on restart, and what does a `keep_hot`
  VM lose?

---

## Domain 9 — Security and multi-tenancy

**Why it matters here:** you run *other people's* code (kowshik, your manager,
issue reporters) on your hardware. The isolation story is the product.

### Module 9.1 — The jailer: chroot, UID drop, cgroups, namespaces ★★★
- **Concept**: Firecracker's jailer runs each FC as non-root, in a chroot, in
  cgroups, optionally in namespaces — so a guest escape lands in a sandboxed FC,
  not on your host. You deliberately *dropped* the PID namespace (it broke host
  socket access).
- **Resource**: (R) Firecracker `docs/jailer.md`; (R) "Namespaces in operation"
  (LWN series); (M) `man 7 namespaces`, `man 7 cgroups`.
- **Your code**: `pkg/engine/firecracker/jail.go`, `fc.go` `startFCJailed`; your
  `docs/archive/PLAN-jailer.md`.
- **War story**: W5 (browser tier) surfaced *leaked jails* — "that begs the
  question of proper cleanup not being there." Jail lifecycle = a real bug class.
- **Lab**: `unshare -Urm` a shell; explore what you can and can't see. Then read
  how `startFCJailed` builds the chroot and what it hardlinks in.
- **Mastery check**: name what the jailer provides (chroot/UID/cgroup) and the
  one namespace you *don't* use and exactly why.

### Module 9.2 — Multi-tenant isolation: API scoping, rate limits, resource caps ★★
- **Concept**: per-user API keys (SHA-256), per-user network (L2), per-user
  resource caps, and token-bucket rate limits (30 creates/min, etc.) — defense in
  depth across layers.
- **Resource**: (R) token-bucket algorithm explainer; your `docs/architecture.md`
  multi-tenancy section.
- **Your code**: auth + rate limiting in `pkg/server/`; `subnetFromIndex` (L2).
- **War story**: W10 — "10 creates/min is absurdly low" — you tuned limits, then
  asked "is exec 2/s per-sandbox or per-server?" Know the scope of every limit.
- **Mastery check**: a user hits their create rate limit. Which layer rejects
  them, and what does the token bucket look like at that instant?

### Module 9.3 — Guest hardening & unscrapable URLs ★
- **Concept**: exec runs as uid 1000 (not root); the config drive is unmounted
  after boot; preview URLs use unguessable subdomains so bots can't enumerate
  someone's work.
- **Resource**: your `docs/guest-agent.md` hardening section.
- **Your code**: uid 1000 in `exec.go`; config-drive unmount in `main.go`; URL
  generation in the proxy.
- **Mastery check**: why uid 1000 and not root, given users can `sudo` anyway?
  (Hint: defaults, blast radius, and what `sudo` audit-trails.)

---

## Domain 10 — Operations, performance, and production craft

**Why it matters here:** you operate this for real users. The skill of *running*
a system — measuring it honestly, deploying without downtime, knowing when a
number is a lie — is what "production engineer" means.

### Module 10.1 — Benchmarking honestly ★★★
- **Concept**: a number without a method is marketing. Control the variables
  (page cache, warm vs cold, loopback vs network), report p50 *and* p99, and be
  honest about what you're measuring.
- **Resource**: (R) Gil Tene "How NOT to Measure Latency" (coordinated omission);
  your `bench/README.md` and the README perf table.
- **Your code**: `bench/run.sh`, `bench/computer.sh`,
  `docs/archive/INVESTIGATION-*.md`.
- **War story**: W7 — "42ms vs 360ms cold wake" both real, measuring different
  things; the doc wasn't honest until you fixed it.
- **Lab**: run `bench/run.sh` on your Pi; reproduce the README's create/wake
  numbers; deliberately *not* drop caches and watch the cold number lie.
- **Mastery check**: what is coordinated omission and how would it have flattered
  your warm-wake p99?

### Module 10.2 — Zero-downtime deploys & graceful shutdown ★★
- **Concept**: on `SIGTERM`, `SnapshotAll` checkpoints every VM (parallel,
  bounded, retry-once) so a deploy/restart doesn't lose state. Crash (SIGKILL)
  bypasses this — hence the durability discussion in W1.
- **Resource**: (R) systemd `KillMode`/`TimeoutStopSec` docs; your session idx 29
  ("zero downtime deployments on agni-01").
- **Your code**: signal handling + `SnapshotAll` in `cmd/bhatti/main.go` /
  `pkg/server/server.go`.
- **Mastery check**: what's the difference in VM state after a graceful restart
  vs an OOM-kill of the daemon? What protects you in each case?

### Module 10.3 — Observability: events, metrics, structured logs ★★
- **Concept**: you can't operate what you can't see. Structured logs
  (`slog`, debug behind a flag), an event log, and metrics snapshots
  (`admin events`/`admin metrics`) are your eyes in prod.
- **Resource**: (R) "Observability" 3-pillars overview; your
  `docs/archive/PLAN-observability.md`.
- **Your code**: event/metrics code in `pkg/server/` and `pkg/store/`; the
  `BHATTI_LOG_LEVEL=debug` instrumentation pattern.
- **Mastery check**: a user reports intermittent slowness. Which of
  logs/events/metrics do you reach for first, and what query?

### Module 10.4 — VMM portability: krucible, libkrun, Apple container ★★★
- **Concept**: Firecracker is Linux/KVM-only. To run on macOS (Hypervisor.framework)
  or get different tradeoffs (libkrun), you abstract the VMM behind an interface.
  This is the frontier of the project (June 2026 sessions).
- **Resource**: (R) libkrun README + design; (R) Apple `container`/`containerization`
  docs; your `docs/internal/PLAN-krucible-*.md`, `docs/PLAN-krucible-v3.md`,
  `docs/research/machinen-vmm-learnings.md`.
- **Your code**: `pkg/engine/krucible/` (engine, spec, agent, capability),
  `cmd/vmm/main.go` (HVF), `cmd/krucible-probe/`.
- **War story**: W9 — the VMM-agnostic investigation and the libkrun fork
  decision (sessions idx 120, 136, 137, 138).
- **Mastery check**: what's the *minimum* interface a VMM must satisfy for bhatti
  (create, exec-transport, snapshot, network)? Which of those does HVF make
  hard?

---

## Suggested sequence (if you want a schedule)

| Phase | Domains | Theme | Weeks (relaxed) |
|-------|---------|-------|-----------------|
| 1 | 0, 1 | Debugging mindset + processes/init | 2–3 |
| 2 | 5, 6 | Wire protocol + storage/durability | 2 |
| 3 | 2, 3 | Virtualization + snapshots (the core) | 3–4 |
| 4 | 4 | Networking | 2 |
| 5 | 7 | The kernel | 2 |
| 6 | 8, 9 | Concurrency/reliability + security | 2 |
| 7 | 10 | Operations + VMM portability | 2 |

Foundations first (0,1) and the wire/storage layer (5,6) before the core
virtualization (2,3) — because by the time you snapshot a VM, you want to already
understand processes, files, and the protocol that talks to it. Networking,
kernel, and the operational layer build on top.

Don't rush phase 3. Snapshots are where your hardest bug lived; that's where the
deepest understanding pays off.

---

*Next: start with [`module-01-processes-and-init.md`](./module-01-processes-and-init.md),
which is built out completely as the template for every other module. Log your
progress in [`progress.md`](./progress.md). When you're ready, we build the next
module together.*
