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

## 2. Topology decision: keep the single-writer server; build capabilities on top

**DECISION (2026-06-16):** we evaluated a daemonless / two-mode CLI and an auto-start-the-server-from-stored-state
model (the analysis below is kept for the record). **We are not doing it.** Reasons: (a) a sandbox is a stateful,
long-running helper, and the real hazard is *two writers* mutating the same helper + store row (a CLI `stop` racing the
resident proxy's wake) — the single-writer server avoids that class of bug; (b) the daemonless win was local UX, which
isn't where the value is; (c) fragmenting into two control paths roughly doubles the lifecycle/concurrency surface for
marginal gain. **The server stays the spine.** The leverage now is *capabilities on top of it* — networking, agent-first
tenancy, env/secrets, richer streaming (§ new section below). The analysis in this section is retained as the rationale
for *why the server earns its keep*, not as a plan to split it.

---

## 2b. (rationale, retained) The CLI / daemon / HTTPS topology for a library VMM

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
   new hardware needed. **— DONE (2026-06-16):** checkpoint magic+version (libkrucible) + the runtime portability gate
   (`validateBundle`: refuse incomplete/cross-arch/proto-mismatch). **lohar slim resolved as moot** — the kernel-direct
   block-root (M1′) keeps lohar PID-1 by design; the one FC-only fn (`setupNetworking`) self-skips on krucible and is
   load-bearing on FC (see `PLAN-krucible-init-model.md` DECISION).
2. **Production rootfs (4b)** + **CLI-direct mode (D)** — unblocks real use cases locally. **— rootfs DONE (2026-06-16):**
   `oci.PullAndConvert` + `/init.krun` symlink + `Config.BaseImage` boot real OCI images on the block-root path;
   `TestKrucibleProductionImage` boots Alpine linux/arm64 (real `/bin/sh`, `uname`, os-release) + cold round-trip, green
   on HVF. **CLI-direct mode still TODO.**
3. **Test migration (4a)** — behavior suites on both engines; server-level integration on a krucible daemon. **— STARTED
   (2026-06-16):** `RunSnapshotSuite` (cold tier, both-engine-ready); `TestKrucibleServerIntegration` drives the full
   daemon (HTTP API + store + thermal) over krucible incl. **cold wake-on-request** (fixed `EnsureHot` to be tier-aware:
   cold→Start). Remaining: port the FC behavior suites (sessions/files/piped) into `enginetest`.
4. **Production use-case matrix (4c)** on Mac, then the **Linux warm-cluster bring-up (E1)** — first multi-platform proof.
5. **Cold-x86-Linux (E2)**, then the P4+ tracks (F) and **Tier-3 Pi (E3)** as their own gates.

**Capabilities (§6) now take priority over the rest of the P4+ backlog** — the order is: config drive + per-sandbox token
(6c.1/6b.1) → host↔guest forward (6a.1) → inter-sandbox + capability tokens (6a.2/6b.2) → unified event stream (6d).
These run alongside the Mac-doable cleanup; the Linux cluster work proceeds when hardware is in the loop.

Steps 1–4 are Mac-doable now; 4c/E need the cluster. The through-line: every step is gated by the `enginetest`/server
integration suites going green on krucible with the *same* assertions as FC.

---

## 6. Platform capabilities on top of the server (the new investment)

With the topology settled (§2), this is where the value is: making the server *do more* for an agent driving fleets of
sandboxes. Three tracks, plus streaming. Grounded in what the substrate actually offers today.

### 6a. Networking (the real gap under TSI)

**Where we are.** Firecracker's L2 model (per-user bridges, subnets, a sibling-name DNS responder) is **gone** under
krucible/TSI. What krucible has: guest **outbound** via TSI (transparent, host stack) ✓; **host→guest port** via the vsock
`Forward`/`Tunnel` primitive (lohar bridges vsock→`localhost:port`; the public proxy already rides this) ✓; `ListeningPorts`
via `ss` ✓. **The gap:** no sandbox↔sandbox path (no shared L2), and no clean sandbox→host-services path (guest `127.0.0.1`
is the guest's loopback). krucible sandboxes are currently **outbound-only islands.**

**Forward path** (build on `Forward`, keep TSI — do *not* switch to a passt/gvproxy L3 backend, which would re-introduce
the FC-style plumbing we shed):
1. **Host↔guest forward** — expose `Tunnel` as a stable host-side listener: `bhatti forward <id> <guestPort> [hostPort]`.
   Small (primitive exists), useful for dev, de-risks the transport. *The building block.*
2. **Inter-sandbox connectivity** — the server assigns each sandbox a stable host endpoint (vsock-forwarded to a guest
   port) and brokers **name resolution** (inject `<name>.sb → gateway` — the krucible-native replacement for the FC DNS
   responder), so A reaches B by name, routed A→host→(vsock forward)→B. The multi-agent unlock.
3. **Sandbox→host gateway** — a documented gateway address for guest→host services (validate what TSI already allows to
   the host's routable IP; add a magic host alias).

### 6b. Agent-first multi-tenancy (capability tokens)

**Where we are.** *Designed (§12 of the plan of record), not built.* Today is operator-first (`store.User` + per-user API
keys + quotas + `SubnetIndex`). The network half of the old tenancy (`SubnetIndex`/bridges) is **deleted for free** by
TSI. krucible currently injects **no token** (`vm.Token == ""`).

**Forward path** (each its own commits; not tangled with the engine):
1. **Per-sandbox token = the first brick** — falls out of wiring the config drive (§6c): each sandbox boots with its own
   token; the agent enforces it (empty token ⇒ no auth, as today). This alone gives per-sandbox isolation at the agent.
2. **Capability tokens** — `{id, sandbox_id, caps[], expires_at, revoked}`, minted on Create, enforced by route
   middleware, revoked on Destroy, per-token exec/egress **audit to `events`**.
3. **Agent-context refinements** — **offline-mintable** (operator signs `{sandbox_id, caps, exp}` with their key; the
   daemon verifies, no mint state) + **scoped share-a-port URLs** (the wake-then-serve proxy already exists, §6a/§4).
4. **Track J** (§11 of the plan of record) — jail the helper for *hostile* multi-tenant on Linux. Later; Mac/dev is
   single-user, FC keeps Hetzner multi-tenant meanwhile.

### 6c. Env & secrets handling (the unblocking move)

**Where we are.** The interface (`SandboxSpec`) carries `Env`/`Secrets`/`Files`; the server resolves them (req.Env +
store secrets, secrets override env). FC delivers them via the **config drive** (`configdrive.go` → `/dev/vdb` ext4, read
by lohar's `loadConfigDrive`). **krucible delivers none of it.** This is the most concrete, MVP-relevant gap — and it's
the move that unblocks §6b's first brick.

**Forward path:**
1. **Wire the config drive on krucible** — reuse `configdrive.go` to build a RAW ext4 from `spec.{Env,Secrets,Files,
   Token,Hostname}`; attach it as a **second block device** (`/dev/vdb`, `krun_add_disk2(..., RAW, read_only=true)`);
   lohar already reads `/dev/vdb`. One mechanism, both engines. Under block-root, root stays `/dev/vda`, config is
   `/dev/vdb`. (VMSpec gains a `ConfigDrive` path; the engine builds + passes it.)
2. **Env precedence + dynamic env** — keep the resolved precedence (env < secrets); per-exec merge already exists
   (`configEnv`); units read `/run/bhatti/config-env`. Add a path to set env *after* boot via the agent (agents mutate
   sandbox env without a reboot).
3. **Secret hygiene under the cold tier (important)** — the cold bundle's `memory.img` contains guest RAM, which
   contains secrets (lohar copies env into RAM + `/run/bhatti/config-env` on tmpfs). So **the bundle is as sensitive as
   the live VM** — it must be protected/encrypted-at-rest and shredded on Destroy, same as the config-drive file. Decide:
   keep secrets out of the snapshotted RAM where possible (e.g. fetch-on-demand from the agent vs. bake into env), and
   never leave the config-drive image world-readable. This is a design constraint to honor, not an afterthought.

### 6d. Streaming (parallel, low-risk polish)

**Where we are.** Already solid: an `EventRecorder` pub/sub bus (`Subscribe(filter)` + fan-out + a persistent `events`
table with `/events?since=<id>` replay) and exec streaming over NDJSON + WebSocket. **Forward path:** a unified live
*fleet* stream (SSE/WS over `EventRecorder.Subscribe`), richer event types on the bus (output/log/thermal/network), and
backpressure/replay polish — pairs naturally with §6a (observe the mesh).

### 6e. Agent-state timeline (volumes & versioning)

**Value first.** The experience we want is *"my agent's working context is a timeline I can rewind to before the mistake,
branch to try three things, promote the one that worked, and carry to another machine."* Not "a disk I occasionally back
up." The unit isn't a volume — it's a **timeline you checkpoint, branch, and promote.**

**Where we are.** v0.3 `PersistentVolume` (named ext4, RW-xor-RO attach, quota, resize, S3 + chunked-CDC backup, btrfs
CoW host backend) is solid and **engine-agnostic** — it ports to krucible unchanged (guest still sees `/dev/vdN`, mounted
from config-drive metadata, so it's **blocked on the config drive (6c)** like env/secrets). But versioning today is
*backup-shaped* (restore-from-S3), **not** first-class checkpoint/rollback.

**What the code dictates (mechanics).** libkrun disks are **boot-time only** — `krun_add_disk` is pre-start, the block
device's `write_config()` is a no-op (no online resize), no hotplug. So on krucible: **attach** = record + `krun_add_disk`
on next launch (to a *running* sandbox → a cold stop/start, ~500 ms); **resize** = grow the file + `resize2fs` on next
boot. Not a regression — a **simplification the cold tier earns**: cheap wake makes "reconfigure = quick restart" replace
all the hotplug/online-resize machinery. Drop any hot-plug ambition.

**The versioning model (the agent-first investment).** Inspired by a comparable agent-state versioned-FS runtime whose
model is built on **Jujutsu (jj)** — chosen for agent-friendly properties worth adopting: working-copy-is-a-change
(auto-snapshot, no explicit commit), **fork-on-first-write** (a fresh sandbox boots in "observe" off a golden base; its
writes land in a *new* change; the base stays clean until you promote), **non-blocking conflicts** (a change can be
conflicted; agents don't deadlock), lightweight **bookmarks** (movable pointers), and **repo-per-X** (their own example:
*"each user volume is a separate repo"*). The workflow patterns are the gold: **checkpoint-per-prompt** (undo-last-prompt =
move the bookmark back), **timeline-per-session** (one bookmark per run, merge or discard), **proposal + diff + approve**
(human-in-the-loop with an audit trail), `main` = promoted state.

**How it maps onto bhatti (granularity is the key call):** that runtime versions at the *file* level; bhatti's volumes are
*block* devices (which is what buys cold/fork + portability). So we do **not** replace block volumes with a file-FS — we
**adopt the model + workflows at the granularity each tier supports:**
- **Now (VM-native, cheap): the whole-sandbox checkpoint *is* a "Change."** It rides the cold/fork tier already built and
  is *more* powerful than file-level (it captures RAM + processes + disk + clock, not just files). `checkpoint` after a
  prompt; **bookmarks** (`main`, `session/<id>`, `proposal/<feat>`) point at checkpoints; `restore <bookmark>` = undo;
  `fork <checkpoint>` = branch; promote = move the bookmark. Fork-on-write isolation = the CoW-rootfs-from-base we already
  do, with a bookmark on top.
- **Later (file-level): a "workspace repo" tier** for the agent's code/output dir — diffs + approval gates + merge, on
  bhatti's file API + a content-addressed store (the chunked-CDC dedup is half of it). This is the full file-level model
  (and the eventual home of the deferred sync/Mutagen idea).

**Two version stores, matching the ladder:** local **CoW** (clonefile/reflink/btrfs-subvol/qcow2-overlay — the primitive
we already use for the rootfs) for instant on-host undo/branch; durable **chunked-CDC** (content-addressed, dedup) for
retention + cross-machine carry. **Coherence rule:** a volume version must be cut at the *same paused/fsync'd instant* as
the VM snapshot, or disk and RAM disagree on restore — tie it to the cold-tier pause+snapshot.

### Sequencing within §6

**Config drive (6c.1) first** — it unblocks env/secrets *and* the per-sandbox token (6b.1) *and* volume mount-metadata
(6e), and real use cases need secrets. Then **host↔guest forward (6a.1)** (self-contained dev win + the mesh building
block). Then they fan out: inter-sandbox (6a.2) and capability tokens (6b.2) in parallel, with the unified event stream
(6d) as a low-risk track. The **agent-state timeline (6e)** rides the cold/fork tier — the checkpoint/bookmark workflow
lands once `fork` does; the volume attach/resize cleanup is small and rides the config drive. Track J (6b.4) and the
cold-tier secret-hygiene hardening (6c.3) follow.

---

## 7. Open questions
1. **Daemonless registry** — lockfile + per-VM state dir, or a tiny always-on supervisor? (Lean: state dir + adopt-by-pid.)
2. **Production rootfs base** — build from an OCI image (like the FC path) or a from-scratch minimal userland? Tier split.
3. **Is `publish`/share ever daemonless?** (Lean: no — it needs the resident proxy; CLI-direct is exec/attach only.)
4. **Linux warm clock fix** — `KVM_SET_CLOCK` vs kvmclock PV; confirm against the arm64 Pi arch-timer behavior.
5. **One-binary release** — when to collapse `cmd/vmm` into the hidden `bhatti vmm` `dlopen` subcommand (§9b) vs keep the
   separate dev helper.
