# War Stories: Your Production Incidents as a Debugging Course

These are real. Every one happened to bhatti, was debugged in a pi session, and
(mostly) left a scar in the code or the `docs/archive/` investigations. They are
the best teaching material you have, because the stakes were real and you
remember them.

Each story has the same shape, which is also **the debugging procedure** you're
trying to internalize:

1. **Symptom** — what was observed (start here, always).
2. **Surface** — how it came to your attention.
3. **Investigation** — the measurements, in order. The narrowing.
4. **Root cause** — the actual gap between model and reality.
5. **The fundamental concept** — what OS/systems idea this teaches (→ curriculum module).
6. **The fix** — what changed, and what was *given up*.
7. **Reproduce it** — a lab to feel the failure yourself.
8. **Source** — where to read the original (session file / archive doc).

> Read these in this order the first time. W1 and W2 are intertwined and are the
> spine of the whole project's reliability story. Don't skip the "reproduce it" —
> a war story you only read is trivia; one you reproduce is a skill.

---

## W1 — "rory" is unreachable: the diff-snapshot corruption ★★★

*The flagship. If you learn one of these cold, this is it.*

**Symptom.** User kowshik could not `bhatti shell` into his hot sandbox "rory".
The command hung, then failed. From the host, `bhatti ls` didn't even show it,
yet the DB said it was `running`.

**Surface.** "help me debug on agni-01 why a user is unable to shell into their
hot sandbox named rory."

**Investigation (watch the narrowing).**
- `bhatti ls` (host) → rory missing from the in-memory map but present in SQLite
  (`status=running`). *First fact: DB and engine disagree.*
- `GET /sandboxes/rory` → 404. Dug into store: `GetSandbox` queries `WHERE id = ?`
  — **no name resolution**. So `rory` (a name) never matched (the id was
  `eb82cee6...`). A *second, separate* bug, found incidentally.
- `systemctl`/kernel logs on agni-01 showed the smoking gun, a repeating pattern
  on rory's TAP device:
  ```
  brbhatti-2: port 1(tap70d13b24) entered forwarding state
  brbhatti-2: port 1(tap70d13b24) entered disabled state   ← FC died within ~1s
  ```
  Every restore spawned a Firecracker that **died within ~1 second**, leaving
  zombie processes. bhatti logs: `agent not ready after resume ... context
  deadline exceeded`, then `dial tcp 10.0.2.2:1024: no route to host`.
- Network oddity: rory lived on `brbhatti-2` (`10.0.2.0/24`) while everyone else
  was on `brbhatti-1`. The bridge was `NO-CARRIER` because its only member kept
  going `disabled` as FC died.

**Root cause.** Two compounding issues. (1) The snapshot rory was restoring from
was **corrupt** — produced by *diff snapshotting* with dirty-page tracking that
missed some host-side virtio ring writes, so the restored guest kernel was
internally inconsistent and panicked/died within a second. (2) A network-state
mismatch (the bridge/subnet rory was created under no longer matched the running
daemon's layout after a restart) meant even a healthy restore had no route.

**The fundamental concept.** Snapshots = device state + full RAM. *Diff*
snapshots rely on the dirty bitmap (`KVM_GET_DIRTY_LOG`) being complete. When a
write to guest RAM bypasses dirty tracking, the diff is missing pages and the
restore is silently corrupt. → **Curriculum Module 3.2** (and 2.3 virtio, 4.1
bridges, 8.3 recovery).

**The fix.** Disable diff snapshots entirely: `track_dirty_pages: false`, **all
snapshots Full**. Add snapshot sanity checks, a circuit breaker on repeated
restore failures, and force-pause after N activity failures. **Given up:** speed
and disk space (Full snapshots are bigger and slower). **Bought:** you never
restore garbage. Reliability over cleverness. Also planned: `GetSandbox` should
match `(id = ? OR name = ?)`.

**Reproduce it (disposable VM only).** Build bhatti with `track_dirty_pages:
true` and diff snapshots on. Run a workload that does heavy host-mediated I/O
(network + disk), snapshot mid-flight, restore. Observe instability/death.
Compare with Full snapshots. *Feel why "always Full" was the call.*

**Source.** Session `2026-04-01T14-28-40-350Z_...ff3fa539...jsonl` (the live
debug) and `2026-04-02T03-34-57-357Z_...f5fab530...jsonl` (the reliability
design). Archive: `docs/archive/PLAN-reliability.md`,
`SNAPSHOT-RELIABILITY-TRACE.md`, `RELIABILITY-AUDIT.md`.

---

## W2 — kowshik can't edit certain files: corruption on a resumed rootfs ★★★

*The sibling of W1. Storage corruption meets snapshot corruption.*

**Symptom.** A user (kowshik) intermittently couldn't write to "certain parts of
his disk." Some files couldn't be edited; writes silently failed or reverted.

**Surface.** "help me debug why one of my users kowshik is facing issues
sometimes writing to certain parts of his disk... WITHOUT doing anything
destructive."

**Investigation.** Checked rory's filesystems read-only first (the user
explicitly: "what's the least amount of damage we can do here"). The corrupt
files traced back to a rootfs that had been **resumed from a dirty/diff
snapshot** (the W1 mechanism) — the on-disk filesystem image carried
inconsistencies. Then a *separate* operator mistake compounded it: a **test
bhatti instance on agni-01 deleted all TAP devices globally** (see W5), severing
connectivity right as the snapshot issue was active. The user's own framing is
the lesson: *"were things just my mistakes or murphy's law? or something getting
revealed right when other things were going wrong?"*

**Root cause.** Layered failure: a dirty-snapshot-corrupted rootfs (W1) + a
global TAP wipe (W5) hitting simultaneously. The corruption was *revealed*, not
caused, by the network event — they were independent faults that overlapped.

**The fundamental concept.** Crash/restore consistency for filesystems: a
filesystem image captured from a VM whose RAM snapshot was inconsistent can have
a page cache that never matched its disk. Combine with *atomic writes* (fsync +
rename) being the only thing standing between you and torn files. → **Curriculum
Module 6.1, 6.2** (and 3.2).

**The fix / recovery.** Non-destructively recover: create a *new* sandbox,
preserve both the volume *and* the rootfs ("ensure nothing is lost, including the
volume and the rootfs"), restart the services that had been running (hermes
gateway, the WebDAV server), re-publish so `rory-files` was reachable again.
Policy lesson reinforced: only remove the old snapshot/VM *after* the new one
works.

**Reproduce it.** In a disposable VM, write a file without fsync, then crash the
VM (`echo b > /proc/sysrq-trigger`). Observe the zero-length or stale file. Then
do temp+fsync+rename and crash again — observe the file is whole or absent, never
torn.

**Source.** Sessions `2026-04-29T08-51-03-815Z_...019dd86f...jsonl` and the short
intro `2026-04-29...kowshik...`.

---

## W3 — Exec hangs only after snapshot/restore: vsock is dead ★★★

**Symptom.** On a fresh VM, `exec` worked perfectly. After snapshot/restore, exec
silently *hung* — the connection established but the agent never received it.

**Surface.** Found during the first Firecracker engine implementation (Part 5),
not from a user — from your own tests passing on fresh VMs and hanging on restored
ones.

**Investigation.** The host-side `CONNECT/OK` vsock handshake *succeeded*
(Firecracker's proxy answered it), but lohar never saw the connection. Tested
across kernel 5.10 and 6.1, FC 1.6.0. Cross-referenced the Firecracker issue
tracker — a known limitation: vsock state is stale after restore.

**Root cause.** Firecracker's vsock implementation doesn't cleanly survive
snapshot/restore — the guest kernel's vsock state and FC's proxy state are
inconsistent after reload. The TCP stack over virtio-net, by contrast, comes back
clean because the NIC's device state restores correctly.

**The fundamental concept.** Different virtio devices have different
restore-fidelity. virtio-net survives; virtio-vsock doesn't. → **Curriculum
Module 2.3, 3.1, 5.2.**

**The fix.** lohar listens on *both* vsock and TCP on ports 1024/1025. After
restore, the host creates a fresh `AgentClient` that uses **TCP over the TAP
device**. (You eventually went TCP-always, even on cold boot — see the staleness
note in `PLAN-learning.md`.) **Cost:** ~0.1ms extra latency vs vsock. Negligible.

**Reproduce it.** Hard, because it's FC-version-dependent — but conceptually:
stand up a VM with a vsock listener, snapshot, restore, and try to connect over
vsock; then try the same over a TCP listener on the virtio-net interface. The TCP
one works.

**Source.** `docs/decisions.md §1` ("TCP over TAP instead of vsock after
snapshot/restore"). Sessions tagged with `vsock` (111 of them mention it).

---

## W4 — Two Pi5s powered off: lohar's PID-1 reboot handler ran on the host ★★

*Short, visceral, unforgettable.*

**Symptom.** A Raspberry Pi 5 development machine just... powered off. Then it
happened again on another.

**Root cause.** lohar's `runAgent()` installs a `SIGTERM` handler that calls
`reboot(LINUX_REBOOT_CMD_POWER_OFF)` — completely correct *inside a Firecracker
VM where lohar is PID 1*. But lohar got invoked on the *host* (a test, a wrong
path) where it is **not** PID 1, and the PID-1 behavior (mounts, bring-up,
`reboot`) executed against the real machine.

**The fundamental concept.** PID 1 is special and its powers are host-level. A
binary that does PID-1 things must *refuse* to run unless it really is PID 1. →
**Curriculum Module 1.3.**

**The fix.** The guard now at the top of `runAgent()` in `cmd/lohar/main.go`:
```go
if os.Getpid() != 1 {
    fmt.Fprintf(os.Stderr, "lohar: refusing to runAgent: not PID 1 ...")
    os.Exit(2)
}
```
The comment is the tombstone: *"Running it on a real host as root has powered off
two Pi5 machines in this project's history."*

**Reproduce it.** Don't. Instead, read the guard and write a tiny program that
checks `os.Getpid()` and explain what `reboot(2)` does and why only PID 1 should
ever call it. Read `man 2 reboot`.

**Source.** `cmd/lohar/main.go` `runAgent()`.

---

## W5 — `bhatti share` stuck on "connecting": the WebSocket upgrade dies in Cloudflare ★★

**Symptom.** The freshly shipped `bhatti share` (web shell) feature, deployed on
agni-01, sat forever on "connecting." The WebSocket never opened.

**Surface.** "when I just tried doing it it is stuck on connecting."

**Investigation.** Your instinct was good — *"First run me through the issue and
what are you going to do to fix it, not get to it directly."* Then *"look through
the web — are people using Cloudflare aware of this and have they discussed it?"*
The HTTP `Upgrade: websocket` / `101 Switching Protocols` handshake was being
disrupted on the path from the browser through Cloudflare to the bhatti reverse
proxy.

**Root cause.** A proxying/upgrade-handling issue in the path: the long-lived,
bidirectional WebSocket needs the proxy to *not* buffer and to correctly forward
the `Upgrade`/`Connection` headers and the `101`. Intermediaries (Cloudflare
settings, your own proxy code) can silently break that.

**The fundamental concept.** WebSockets are HTTP that gets *upgraded* to a raw
bidirectional stream. Reverse proxies must special-case the upgrade and never
buffer it. → **Curriculum Module 4.4.**

**The fix.** Patch the proxy/upgrade path, then a patch release. (You also wrote
the finding to a file for posterity.)

**Reproduce it.** Write a 60-line Go reverse proxy. Proxy a normal GET — works.
Now proxy a WebSocket endpoint *with response buffering on* — watch the upgrade
hang. Turn buffering off and forward the upgrade headers — watch it connect.

**Source.** Session `2026-04-10T10-22-16-227Z_...04882470...jsonl`. See also the
follow-up `...review the current changes regarding unable to make the share
websocket connection work...`.

---

## W6 — Browser-tier sandboxes fail; leaked jails reveal missing cleanup ★★

**Symptom.** `--image browser` sandboxes failed to come up on agni-01.

**Investigation.** Suspected snapshot resume first ("is it because of resuming
from the snapshot?"). Tried fresh creates. Found **leaked jailer chroot
directories** from prior failed runs — and your sharp follow-up: *"Why are there
leaked jails? That begs the question of proper cleanup not being there in the
codebase."* Also discovered an unrealistic timeout ("not a realistic timeout,
should be less than 5 seconds") and, alarmingly, *"How does the rory vm have
missing volume?"* — cross-contaminating with W1/W2.

**Root cause.** Jail lifecycle wasn't cleaning up chroots on failure paths, so
retries collided with stale state; compounded by timeouts that were too long to
fail fast and by the ongoing rory/volume issues.

**The fundamental concept.** Every resource you create (TAP, jail chroot, FC
process, snapshot file) needs an owner and a cleanup path on *every* exit,
including the error paths. The jailer's chroot is just a directory tree — orphan
it and the next run trips over it. → **Curriculum Module 9.1, 8.2.**

**The fix.** Add proper jail cleanup, tighten timeouts (<5s), then commit and cut
an RC. Your meta-move: *"it's working, let's take a step back and see what issues
we ran into and why"* — postmortem culture.

**Reproduce it.** Create + force-kill bhatti mid-create so cleanup doesn't run.
Inspect `/srv/jailer/...` (or wherever your jail root is) for orphaned chroots.
Then read `startFCJailed` and trace where cleanup should happen on each error
return.

**Source.** Session `2026-04-03T14-25-28-716Z_...c84d7fee...jsonl`.

---

## W7 — "Cold wake is 42ms" vs "Cold wake is 360ms": both true, doc was dishonest ★★

**Symptom.** Not a crash — a *credibility* bug. An HN commenter on the launch post
asked pointed questions about snapshot scaling and secret-reconstruction cost.
Drafting a reply, you noticed `thermal-states.mdx` claimed cold wake `~42ms p50`
while the homepage benchmark said `360ms p50`.

**Investigation.** Enabled `BHATTI_LOG_LEVEL=debug` on agni-01 via a systemd
override. Used existing instrumentation in `startFCJailed` (FC spawn + socket
ready), `startVM` (`/snapshot/load` timing), and `WaitReady` (TCP connect + AUTH
+ exec split). Controlled the page cache with `echo 3 > /proc/sys/vm/drop_caches`
between runs. Timed end-to-end against the public proxy.

**Root cause.** The two numbers measured *different things*. 42ms was the
orchestration call returning; 360ms was end-to-end **including page-in of the
memory snapshot from disk on first access**. With a warm page cache the snapshot
is already in RAM (fast); cold, every page faults in from NVMe. Both real; the doc
just wasn't honest about *which*.

**The fundamental concept.** Demand paging and the page cache. A `mmap`'d snapshot
file isn't "loaded" — pages fault in lazily on first touch. Your benchmark must
state cache state. → **Curriculum Module 3.4, 10.1** (coordinated omission &
honest measurement).

**The fix.** Make the docs honest: distinguish cold (page-in included) from warm,
state the methodology, report p50 *and* p99. The README perf table now does this
explicitly ("Cold-wake reads the memory snapshot from disk on first use — page-in
cost is included").

**Reproduce it.** `drop_caches`, cold-wake a VM, time it (~hundreds of ms).
Immediately warm-wake (~ms). The ratio *is* the page-in cost. Now write the doc
sentence that wouldn't mislead anyone.

**Source.** `docs/archive/INVESTIGATION-cold-wake-cache.md`.

---

## W8 — Boot/create is too slow: fact-backed perf work (and a rate limit that lied) ★★

**Symptom.** Sandbox create/boot felt slow; you wanted it faster than the
already-good numbers.

**Surface.** "help me figure out how to improve the bhatti boot time. I did a
thorough testing and noted my methodology in INVESTIGATION-create-performance.md."

**Investigation.** Pure method, and your principles shine: *"we should never be
making an assumption, everything can be tested and fact-backed"* and *"let's do
these one by one and keep testing on every step on my pi5a. Agni-01 testing
later, as it serves production."* You instrumented six create phases behind a
debug flag, found a `10 creates/min` rate limit was *"absurdly low"* and silently
shaping results, checked whether a btrfs-dependent optimization would hurt
bring-your-own-image users, and benchmarked all stock images.

**Root cause(s).** A mix: an over-tight rate limit distorting tests; backoff/poll
timing in `WaitReady`; reliance on btrfs reflink for fast image copies (great on
agni-01's btrfs, neutral elsewhere). An earlier "reduced backoff" change (1.8.0)
had to be reverted because the *rest* of that commit didn't deliver — taught you
to isolate changes.

**The fundamental concept.** Honest benchmarking (control variables, isolate one
change at a time, know your rate limits), plus btrfs CoW/reflink semantics. →
**Curriculum Module 10.1, 6.2, 5.2.**

**The fix.** Tightened changes to the *actual* improvements, kept debug
instrumentation behind a flag, raised rate limits to sane values, committed
atomically for easy patch releases.

**Reproduce it.** Run `bench/run.sh` on your Pi. Now set the create rate limit to
something tiny and re-run — watch the benchmark "slow down" for a reason that has
nothing to do with boot. That's how a limit lies in a benchmark.

**Source.** Session `2026-04-26T09-08-49-253Z_...019dc90c...jsonl`,
`docs/archive/INVESTIGATION-create-performance.md`,
`docs/archive/PLAN-create-performance.md`.

---

## W9 — "fastidious" files a perf issue → the systemd-shim architectural reckoning ★★★

**Symptom.** A user (GitHub handle *fastidious*) commented on issue #12 that
something was off. Validating it exposed that your homegrown `systemctl`/`init`
shim in lohar was patching symptoms, not built on a model.

**Surface.** "debug if this comment by fastidious on his issue is valid and if
there's anything wrong deeper."

**Investigation.** You debugged confidently against the live server, then made the
key move: *"can you look at some popular systemd/systemctl implementations on
GitHub and see if we are not just doing short-term fixes and are going about it in
a well-defined architectural way?"* and *"take a look at the rest of the shims in
lohar too and compare them to their original parent."* This reframed a bug into an
architecture decision.

**Root cause.** The shim had grown feature-by-feature without matching systemd's
actual model (unit dependency ordering, conditions, cgroup placement, notify
protocol). Real services exposed the gaps.

**The fundamental concept.** An init system is a *dependency-ordered, condition-
gated service supervisor with cgroup placement* — not a pile of "start this
process" calls. → **Curriculum Module 1.5** (and 1.3, 9.1).

**The fix.** The `PLAN-systemd-rc.md` line of work: `unit.go` (real unit parsing),
`depgraph.go` (topological ordering), `conditions.go` (`Condition*` gates),
`cgroup.go` (placement), `notify.go` (`sd_notify`), `spawn.go` (the `lohar spawn`
helper that fixes the cgroup-placement race for forking daemons). The 56KB
`systemctl.go` is the result. Your release philosophy: *"work on things atomically
so we can keep getting patch versions out."*

**Reproduce it.** Read `depgraph.go` and `conditions.go`. Write three unit files
with an `After=` chain and a `ConditionPathExists=`; trace how the shim would
order and gate them. Compare to `man systemd.unit`.

**Source.** Session `2026-04-30T11-01-23-326Z_...019dde0c...jsonl`,
`docs/archive/PLAN-systemd-rc.md`, `docs/internal/PLAN-systemd-shims-tail.md`.

---

## W10 — Going VMM-agnostic: when Firecracker isn't enough ★★★

*The current frontier (May–June 2026). Less "bug," more "deep architectural
spelunking" — but it's where you're learning the most right now.*

**Symptom / driver.** Firecracker is Linux/KVM-only. You want bhatti on macOS
(Hypervisor.framework), and you want the option of libkrun (different tradeoffs,
e.g. running without a separate kernel image). So: how much work to make bhatti
*VMM-agnostic*?

**Surface.** "help me understand the amount of work required to make bhatti VMM
agnostic. Particularly I am on the lookout for [libkrun / Apple container]."

**Investigation.** A sequence of design spikes: the `krucible` engine abstraction
(`pkg/engine/krucible/`), a probe (`cmd/krucible-probe/`), a toy VMM on macOS HVF
(`cmd/vmm/main.go` with `hvf-entitlements.plist`), and deep reads of libkrun and
Apple's `container`/`containerization`. The hard questions: what's the *minimum*
VMM interface (create, exec transport, snapshot, network)? Which VMMs support
snapshots at all? How do you keep lohar/the wire protocol unchanged across VMMs?

**The fundamental concept.** Hardware virtualization is an *interface* (`/dev/kvm`
on Linux, Hypervisor.framework on macOS), and a VMM is the userspace program that
drives it. Abstract the VMM and the rest of bhatti (agent, protocol, store,
networking) can be reused. → **Curriculum Module 2.1, 2.2, 10.4.**

**Status / lessons.** Ongoing. Snapshot support is the dividing line: not every
VMM gives you the warm/cold trick. HVF on macOS makes some things (jailer-style
isolation, the exact device model) different. `docs/PLAN-krucible-v3.md`,
`docs/internal/PLAN-krucible-migration.md`, and
`docs/research/machinen-vmm-learnings.md` are the live thinking.

**Reproduce it.** Read `pkg/engine/krucible/engine.go` and list the methods the
interface requires. Then open `pkg/engine/firecracker/engine.go` and check each is
implemented. That diff *is* the "what a VMM must do" spec.

**Source.** Sessions idx 120 (`2026-05-11`), 136 (`2026-06-10`, Apple container),
137 (`2026-06-11`, portable sandbox), 138 (`2026-06-13`, libkrun fork). Archive:
`docs/PLAN-krucible-v3.md`, `docs/internal/PLAN-krucible-*.md`,
`docs/SPIKE-kubelet-pause-resume-*.md`.

---

## How to use this catalog for skill-building

1. **Cold-read drill.** Pick a story, read only the *Symptom* and *Surface*. Write
   your own hypothesis tree before reading on. Then compare. Your gap *is* your
   curriculum.
2. **Tool drill.** For each story, list which observability tool would have shown
   it fastest (`dmesg`/kernel logs for W1, `tcpdump` for W5, `drop_caches`+timing
   for W7, `/proc/PID/status` for ballooning). Build the reflex.
3. **Reproduce.** Do at least the W2, W5, and W7 labs — they're safe, cheap, and
   teach fsync, WebSocket upgrades, and demand paging viscerally.
4. **Teach-back.** Explain W1 to a rubber duck (or a person) from memory. If you
   can't say *why diff snapshots corrupt and why Full fixed it*, loop back to
   Module 3.2.

When you've internalized these ten, you won't be "the person who built bhatti
with an AI." You'll be the person who can be handed a hung VM and a `dmesg` and
*find the truth*.
