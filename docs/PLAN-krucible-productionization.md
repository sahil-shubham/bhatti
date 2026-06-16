# krucible productionization — Linux, topology, the backlog, and the path to real use

Status: **Plan (2026-06-16).** With warm + cold tiers validated on Mac/HVF, this scopes the four things needed to make
krucible a production engine: (1) **Linux support**, (2) a **rethink of the CLI/daemon/HTTPS topology** now that the VMM
is an in-process library, (3) the **leftover actionables** (prioritized), and (4) **moving the integration suite to
krucible + production-testing real use cases.** Companion: `PLAN-krucible-v3.md` (plan of record), `PLAN-krucible-cold-tier.md`,
`PLAN-krucible-init-model.md`.

---

## 1. Linux support — when, and what's gated on hardware

Grounded in what's actually OS-gated in libkrucible today:

| Capability | Linux state today | Work to land | Gated on |
|---|---|---|---|
| **Warm tier** (pause/resume) | **code is shared** (`Vmm::pause/resume/pause_vcpus` are not OS-gated; linux vstate has the Pause/Resume StateMachine) — *should already work on KVM* | build libkrucible on Linux + validate pause/resume on KVM (x86 + arm); add the **Linux warm-resume clock fix** (KVM `KVM_SET_CLOCK`/kvmclock — the analogue of the macOS `CNTVOFF` freeze; the linux `resume_vcpus` currently ignores `paused_duration`) | a Linux/KVM box (cluster) |
| **Block root** | `blk`-gated, **not** OS-gated → the kernel-direct `root=/dev/vda` path compiles on Linux | confirm the Linux libkrunfw kernel has virtio-blk + ext4 built in (very likely) | a Linux box |
| **Cold tier (x86-Linux)** | **not implemented** — the 13 checkpoint/restore blocks in `vmm/lib.rs` are `cfg(macos, aarch64)`; linux vstate has **no** `SaveState`/`RestoreState` | port the linux side: `SaveState`/`RestoreState` vCPU events + x86 register save/restore (`KVM_GET/SET_REGS/SREGS/MSRS/...`), `Vm::save/restore_state` (PIC/PIT/clock), and widen the `cfg(...)` gates to `(linux,x86_64)`. The **device persist is arch-neutral and already done**; only vCPU+VM state is missing | a Linux/KVM x86 box |
| **Cold/fork (arm64-Linux / Pi)** | not implemented (Tier 3) | KVM-arm64 vCPU + **GICv2/v3** + arch-timer save/restore — the gnarly one | a Pi (cluster) |

**Sequencing (all gated on home-cluster access):**
1. **Warm-Linux bring-up** — build + run the `enginetest` warm/agent suites on `asus-i5` (x86 KVM) and a Pi (arm KVM). Low effort; mostly validation + the KVM clock fix. *This is the first Linux milestone.*
2. **Cold-x86-Linux** — port the linux checkpoint/restore (bounded; the reference has it, device persist is shared). `RunSnapshotSuite` green on x86 KVM.
3. **Tier-3 arm64-Linux cold/fork** — GIC save/restore. Deferred; warm works on Pi meanwhile.

The honest blocker: **none of this is testable on the Mac.** Linux work proceeds only with a KVM box in the loop. The *code* for warm-Linux is largely written (shared); cold-x86-Linux is a real but bounded port.

---

## 2. Rethinking the CLI / daemon / HTTPS topology for a library VMM

Today: `bhatti` CLI → HTTP (`localhost:8080`) → **daemon** (`pkg/server`) which owns the engine, the store (registry),
the thermal manager, and the public proxy; the daemon spawns one `bhatti-vmm` helper per sandbox (only the helper links
libkrun). This client/server shape is inherited from Firecracker (out-of-process VMM + HTTP API + jailer + TAP network).

libkrun being an **in-process library** doesn't remove the daemon — but it changes *what the daemon is for* and shrinks it.

### What genuinely needs the server (platform concerns, not FC-isms — KEEP)
- **Registry / lifecycle persistence** — a sandbox (a long-running helper process) outlives any single CLI invocation;
  something persistent must own + track helpers and survive a control-plane restart (recovery/re-adopt).
- **Thermal wake-on-request** — idle→warm→cold + transparently waking a sandbox on an incoming request needs a resident
  watcher.
- **Public proxy (HTTPS)** — `publish`/`share` route external HTTP to a guest port, wake-then-serve. A persistent
  HTTP(S) server is the product surface here.
- **Capability auth + events + rate limit + observability** — server-side middleware.

### What shrinks or disappears (FC-isms removed by libkrun + TSI)
- **All host network plumbing** — TAP/bridge/iptables/IP-pool/per-user-DNS (`network.go`, `dns.go`, `subnet`, `ippool`,
  the global firewall) → **deleted** on the krucible path (TSI: no L2). This is the single biggest daemon simplification.
- **FC-process + jailer management** → spawn a `bhatti-vmm` helper (plus Track-J later). Simpler.
- **Per-user bridge/subnet multi-tenancy** → capability tokens (no network half).

### What the library nature *newly enables* (NEW)
- **A daemonless CLI-direct mode.** Because the VMM is in-process in the helper, a `bhatti run`/`bhatti sbx` can spawn a
  helper directly, exec/attach, and tear down — **no daemon, no HTTP** — for local one-shot / ephemeral / CI sandboxes
  (the "put your agent in a VM and let it be" shape). The daemon stays for the persistent, multi-sandbox, proxy,
  multi-tenant *platform*. Same helper binary, two front-ends.
- **One binary, two roles** (release §9b of the plan): `bhatti` is the CLI *and* the daemon *and* (via a hidden `vmm`
  subcommand that `dlopen`s libkrun) the helper. Pure-Go control plane; cgo only when it's the helper.

### The reframed topology
```
            ┌─ bhatti CLI ──HTTP──► bhatti daemon (server) ──► registry, thermal, public proxy, auth
            │                              │ spawns
  one binary ┤                              └─► bhatti vmm (helper, dlopen libkrun)  ── per sandbox
            │
            └─ bhatti run (CLI-direct) ───────► bhatti vmm (helper)   ── local, daemonless, ephemeral
```
Net: the daemon is **leaner** (loses the network/jailer/FC bulk), still essential for the *platform*; and a *daemonless*
path becomes a first-class local/CI mode. Decisions to make: where the daemonless registry lives (a lockfile + per-VM
state dir), and whether `share`/publish is daemon-only (yes — it needs the resident proxy).

---

## 3. Leftover actionables (prioritized backlog)

**A. Finish/​harden the cold tier (P3 closeout)**
- Bundle integrity: fsync + atomic rename of the `.bhatti` bundle; refuse a half-written/tampered bundle (magic + length
  + hash). Add `RunSnapshotSuite` cases `BundleSelfContained` + `RejectsTampered`.
- Manifest gate: enforce `arch` match (refuse cross-arch) + an exact `feature_hash` for Tier 1; the classify/refuse model
  for Tier 2.
- Port the surviving behaviors from `PLAN-snapshot-reliability-fixes.md` (volume persistence through resume, error-on-bad-
  artifact, recovery ordering, destroy race) — the spec; never weaken them.

**B. lohar slimming (cash in the block-root + VMM-clock paydown — `PLAN-krucible-init-model.md` §6)**
- Delete the dead FC networking (`net.go`, `ip=` parse) on the krucible path.
- Idempotent mounts (the libkrun-init path already mounts some); audit for now-redundant clock-jump defensiveness (the VMM
  owns clock continuity).
- Keep the agent + the systemd shim (the shim becomes a *tier* capability — code stays, not default).

**C. Daemon slimming** — delete/neutralize the FC network plumbing on the krucible path (`network.go`, `dns.go`,
`subnet`, `ippool`) — unused by TSI; keep them FC-only or remove from the krucible build.

**D. CLI-direct mode** — `bhatti run --engine=krucible <cmd>`: spawn a helper, exec, tear down, daemonless.

**E. Linux** — §1: warm-cluster bring-up → cold-x86 port → Tier-3 Pi.

**F. P4+ tracks (each its own gate):** `FORK` fan-out verbs; egress allowlist (`krun_set_egress_policy`) + TSI;
Track-J jail (Linux multi-tenant); capability tokens (per-sandbox, offline-mint by signing, scoped share URLs); release
packaging (single-binary `dlopen` + macOS notarization).

---

## 4. Move the integration suite to krucible + production-test real use cases

### 4a. Test migration (the parity strategy, made concrete)
The FC suite is ~28 files. Sort them:
- **Behavior (VMM-agnostic) → `enginetest`, run on FC *and* krucible:** exec/exit-codes/stdout, files, sessions, piped
  sessions, shell, tunnel, ringbuffer, keepalive. Today: `RunAgentSuite` + `RunThermalSuite` + `RunSnapshotSuite` exist.
  Extend with `RunSessionSuite`, `RunFileSuite`, `RunTunnelSuite`, `RunPipedSuite` (move assertions, keep them identical).
- **FC-only (network plumbing) → NOT ported, replaced by krucible-specific:** `network*`, `dns`, `jailer`, `subnet`,
  `ippool` → replaced by control-protocol round-trips, cold-wake (`RunSnapshotSuite` ✓), egress, Track-J.
- **Server-level integration → run the daemon with a krucible engine:** `proxy_integration`, `v03_integration`,
  `reliability` — stand up `pkg/server` backed by krucible and run these against it (HTTP API, public proxy wake-then-serve,
  multi-sandbox, recovery). This is the real "whole suite on the engine" step.
- **CI:** keep `ci.yml` (no-VM, fast) as the required gate; add a krucible integration job (Mac/HVF smoke) + the cluster
  matrix (asus-i5 x86 KVM, Pi arm KVM warm). FC integration keeps running on Hetzner-like runners.

### 4b. Production rootfs (prerequisite for real use cases)
The current krucible test rootfs is a tiny multi-call util (no shell, no apt). Real use cases need a **production rootfs**:
a real Ubuntu (or similar) userland built into the block image (`mke2fs -d`) with lohar at `/init.krun`. This is the
`scripts/krucible-rootfs.sh` → a full image pipeline (the plan §6 deferred the tier-rich userland; productionization needs
it). Tiers: a **base** (agent only) and a **workload** tier (systemd-shim or real-systemd for Docker/packages).

### 4c. Production use-case test matrix (real workloads, on the production rootfs)
| # | Use case | Exercises |
|---|---|---|
| 1 | **Dev env**: `create` → `shell` → edit files → run a build → `stop` (cold) → `start` → continue | agent surface + cold-wake on a real workload |
| 2 | **Agent sandbox**: run a coding agent in-VM; exec/files; idle→warm→wake; cold stop/start | thermal + agent + the product story |
| 3 | **Web server + publish**: run a dev server on a guest port → `publish`/`share` → hit via the public proxy (wake-then-serve) | public proxy + TSI port-forward + thermal wake |
| 4 | **`apt install`** postgres / nginx / redis → service starts, survives | the workload tier (systemd-shim or real-systemd) |
| 5 | **Stateful snapshot/restore**: a running process + open files + in-RAM state survive `stop`/`start` | cold tier on a real stateful workload |
| 6 | **Multi-sandbox + capability tokens**: N sandboxes, scoped tokens, per-token exec/egress audit | the daemon platform + auth |
| 7 | **Daemonless `bhatti run`**: one-shot ephemeral sandbox, no daemon | the CLI-direct mode |

Each becomes an integration test (scripted, self-verifying) on the home cluster + Mac. The bar: the same use cases that
work on FC today work on krucible, plus the krucible-only wins (faster fs, sub-second cold-wake).

---

## 5. Recommended execution order

1. **Cold-tier closeout (A)** + **lohar/daemon slimming (B, C)** — finish + clean up what's already validated on Mac. No
   new hardware needed.
2. **Production rootfs (4b)** + **CLI-direct mode (D)** — unblocks real use cases locally.
3. **Test migration (4a)** — behavior suites on both engines; server-level integration on a krucible daemon.
4. **Production use-case matrix (4c)** on Mac, then the **Linux warm-cluster bring-up (E1)** — first multi-platform proof.
5. **Cold-x86-Linux (E2)**, then the P4+ tracks (F) and **Tier-3 Pi (E3)** as their own gates.

Steps 1–4 are Mac-doable now; 4c/E need the cluster. The through-line: every step is gated by the `enginetest`/server
integration suites going green on krucible with the *same* assertions as FC.

---

## 6. Open questions
1. **Daemonless registry** — lockfile + per-VM state dir, or a tiny always-on supervisor? (Lean: state dir + adopt-by-pid.)
2. **Production rootfs base** — build from an OCI image (like the FC path) or a from-scratch minimal userland? Tier split.
3. **Is `publish`/share ever daemonless?** (Lean: no — it needs the resident proxy; CLI-direct is exec/attach only.)
4. **Linux warm clock fix** — `KVM_SET_CLOCK` vs kvmclock PV; confirm against the arm64 Pi arch-timer behavior.
5. **One-binary release** — when to collapse `cmd/vmm` into the hidden `bhatti vmm` `dlopen` subcommand (§9b) vs keep the
   separate dev helper.
