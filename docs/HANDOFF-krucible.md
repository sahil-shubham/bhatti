# krucible — engineer hand-off

A practical entry point for picking up the krucible engine (bhatti's libkrun-fork
VMM). Read this first, then the design docs it links. **The actionable backlog is
§5.**

Companion docs (design rationale, deeper context):
`PLAN-krucible-v3.md` (plan of record), `PLAN-krucible-productionization.md`
(Linux/topology/capabilities/parity + the **verified feature matrix** in §6f),
`PLAN-krucible-cold-tier.md`, `PLAN-krucible-init-model.md`.

---

## 1. What krucible is (1 minute)

bhatti runs agent sandboxes in microVMs. Two engines implement `engine.Engine`:
- **`firecracker`** — production on Hetzner, **do not touch** (untouched on this work).
- **`krucible`** — a fork of libkrun (`../libkrucible`, an in-process VMM library)
  that we extended with pause/resume, snapshot/restore, a control socket, and
  block-root boot. The daemon never links libkrun; it spawns one cgo helper
  (`bhatti-vmm`, from `cmd/vmm`) per sandbox and talks to it over UDS.

Repos: **`bhatti`** (Go daemon, branch `krucible`) + **`libkrucible`** (Rust fork,
branch `main`, builds `--no-default-features --features blk`).

## 2. Current state (verified)

Feature matrix — every cell run as a test on that platform (see
`PLAN-krucible-productionization.md` §6f):

| Feature | darwin/arm64 (HVF) | linux/amd64 (KVM) | linux/arm64 (KVM) |
|---|:---:|:---:|:---:|
| Agent (exec/shell/files/sessions) | ✓ | ✓ | ✓ |
| Warm pause/resume | ✓ | ✓ | ✓ |
| Warm clock continuity (freeze) | ✓ | ✓ | ✓ (KVM_REG_ARM_TIMER_CNT) |
| Cold snapshot/restore | ✓ | ✓ | ✗ (see 5.2) |
| Config drive (env/secrets/token) | ✓ | ✓ | ✓ |
| Host↔guest forward | ✓ | ✓ | ✓ |
| Recovery (restart-safe) | ✓ | ✓ | ✓ |

**One gap left, linux/arm64 (Tier-3): the cold tier.** Everything else is green on
all three. The arm64 warm-clock freeze landed (§5.1, 2026-06-18): the EL2
CNTVOFF_EL2 one-reg ENOENTs, so we rewind the guest-visible virtual counter
(`KVM_REG_ARM_TIMER_CNT`, applied once on vCPU 0) instead — `TestKrucibleClockFreeze`
is green on raspi-5a (delta 0.01s over a 3s pause). Recovery (restart-safety) and
the cold tier on x86 are done.

## 3. Build & test

### macOS (dev box, HVF)
```bash
make krucible   # builds libkrucible (cargo --features blk) + the install prefix
make vmm        # builds + codesigns bhatti-vmm (HVF entitlement)
make build      # the pure-Go daemon/CLI
go test -tags krucible ./pkg/engine/krucible/ -count=1   # full krucible suite
```
- `timeout(1)` is NOT available on the Mac.
- Toolchain: rustc 1.96, go 1.25.7, Homebrew libkrun/libkrunfw.

### Linux cluster (KVM)
`scripts/krucible-linux-bringup.sh` builds everything on a node (apt deps + rustup
+ Go 1.25 + libkrunfw + libkrucible + bhatti-vmm). Run tests with:
```bash
export PATH=/usr/local/go/bin:$HOME/.cargo/bin:$PATH
export PKG_CONFIG_PATH=$HOME/kr/libkrucible/_install/lib/pkgconfig
export TMPDIR=$HOME/krtmp          # IMPORTANT — see gotchas
go test -tags krucible ./pkg/engine/krucible/ -count=1
```
Iterate after a code change: `rsync` the changed sources to the node, re-run the
bring-up (libkrunfw is cached; only libkrucible relinks), `go test`.

## 4. The cluster (Tailscale)

| Node | Tailscale IP | arch | notes |
|---|---|---|---|
| asus-i5 | 100.108.101.22 | amd64 | primary x86 test box |
| raspi-5a | 100.119.145.44 | arm64 | primary arm64 test box |
| raspi-4b / raspi-5b | 100.66.66.124 / 100.79.148.43 | arm64 | spare |

SSH: `ssh -i ~/.ssh/id_ed25519 user@<ip>` (Tailscale SSH may prompt a one-time
browser auth). Sources live under `~/kr/{bhatti,libkrucible}` on each node.

**Operational gotchas (these cost real debugging time):**
- **`/tmp` is a small tmpfs (~3.6 G) on the Pis.** A 1 GiB `memory.img` snapshot +
  the guest RAM fill it → `EDQUOT`. **Always set `TMPDIR` to a disk path**
  (`~/krtmp`). This is why a server cold-wake test once hung.
- **`/dev/kvm` needs the `kvm` group.** `sudo usermod -aG kvm user` then reconnect
  (new SSH session picks up the group).
- **TSI shares the host's port namespace.** A guest can't `listen` on a port the
  host already uses (e.g. 8080 on a k8s node), and a guest connect to such a port
  before a guest-local listener exists falls through to the host process. Tests use
  high guest ports (18080). Real impact: published/forwarded guest ports must avoid
  host-occupied ports.
- libkrun/libkrunfw install to **`lib64`** on Linux (not `lib`); the lib autodetect
  handles both.

## 5. Actionable backlog

Ordered: the two arm64 Tier-3 gaps first (what was asked), then the smaller wins,
then the larger capability tracks. Each item: **goal · status · files · next ·
validate · gotchas.**

### 5.1 arm64 warm-clock freeze  — **DONE (2026-06-18)**
- **Goal:** a warm pause must not advance the guest's `CLOCK_MONOTONIC` by the
  pause duration on linux/arm64.
- **Outcome:** **green on raspi-5a** — `TestKrucibleClockFreeze` reports delta
  0.01 s across a 3 s pause (threshold 1.5 s). No regression in the warm
  pause/resume suites.
- **What worked (and why the original attempt didn't):** `CNTVOFF_EL2` is an EL2
  register KVM does not surface to an EL1 guest vCPU via `KVM_GET_ONE_REG`
  (ENOENT), so the original CNTVOFF approach was a graceful no-op. The fix rewinds
  the **guest-visible virtual counter** `KVM_REG_ARM_TIMER_CNT` instead
  (read at resume, subtract `paused_ns` worth of ticks, write back). NB: the kernel
  ABI accidentally swapped the CVAL/CNT encodings — the counter is the fixed
  `ARM64_SYS_REG(3,3,14,3,2)` slot, used as-is (see the uapi WARNING). The offset
  is VM-wide on modern KVM, so the rewind is applied **once, on vCPU 0**
  (`self.id == 0`) to avoid N× compounding across vCPUs.
- **Files (changed):** `libkrucible/src/arch/src/aarch64/linux/regs.rs`
  (`adjust_virtual_timer_offset` now uses `KVM_REG_ARM_TIMER_CNT`),
  `libkrucible/src/vmm/src/linux/vstate.rs` (`Vcpu::adjust_guest_clock_after_pause`
  aarch64 branch, gated `self.id == 0`), `pkg/engine/krucible/clock_test.go`
  (un-skipped for linux/arm64). Also fixed `scripts/krucible-build-lib.sh` (it
  hardcoded `lib`, breaking the Linux link — `cannot find -lkrun`; now derives
  libdir from the .pc like the bringup script).
- **Follow-up (not blocking):** the test uses 1 vCPU, so the `self.id == 0`
  compounding gate isn't exercised by CI — a multi-vCPU clock-freeze case would
  lock it in.

### 5.2 arm64 cold tier (snapshot/restore)  (large; the gnarly arm64 gap)
- **Goal:** `Stop`/`Start` (snapshot → free RAM → restore) on linux/arm64. Today
  the SNAPSHOT verb returns `not supported on this platform` (cold-tier `Vmm` gates
  are `any(macos+aarch64, linux+x86_64)`; arm64-linux is excluded).
- **Status:** not started.
- **Scope (three parts):**
  1. **arm64 vcpu state save/restore** via KVM — core regs + the sysreg set
     enumerated with `KVM_GET_REG_LIST`, each via `KVM_GET/SET_ONE_REG`. The x86
     `VcpuState` (`libkrucible/src/vmm/src/linux/vstate.rs`) is the structural
     template; the arm64 register set differs.
  2. **GICv3 save/restore** — distributor + redistributor (and ITS if used) via the
     KVM vGIC device ioctls (`KVM_DEV_ARM_VGIC_GRP_*`). This is the hard part.
  3. **arch-timer state** (`CNTV_CVAL`, `CNTVOFF`).
  Then widen the cold-tier `cfg` gates (the ~15 in `vmm/src/lib.rs` + builder +
  `libkrun/src/lib.rs`) to include `(linux, aarch64)`.
- **Reference (important — it is NOT unreferenced):** **Firecracker supports arm64
  snapshot** (GICv3 + vcpu), and libkrucible's `vmm` crate is FC-derived. Study
  Firecracker's (and cloud-hypervisor's) arm64 snapshot code for the
  `KVM_GET_REG_LIST` sysreg enumeration and the GICv3 device-attr save/restore. The
  macОS/HVF GIC capture (`libkrucible/src/hvf/src/lib.rs`, GIC distributor/
  redistributor save/restore) is the conceptual analogue but a different API.
- **Validate:** `TestKrucibleSnapshotSuite` green on raspi-5a (Stop/Start/
  exec-after-restore + RAM-survived). The x86 cold tier is the proven sibling.

### 5.3 publish / public-proxy verification on krucible  (small)
- **Goal:** confirm `publish` + the wake-then-serve public proxy work on krucible
  (engine-agnostic — uses `Tunnel` + `ensureHot`, both implemented). Untested on
  krucible.
- **Next:** a server-level integration test (mirror `TestKrucibleServerForward` in
  `pkg/engine/krucible/server_integration_test.go`) that publishes a guest port and
  fetches it through the public proxy. Use a high guest port (TSI gotcha, §4).

### 5.4 behavior-suite migration (FC↔krucible parity)  (medium, mechanical)
- **Goal:** run FC's behavior tests on both engines via `pkg/engine/enginetest`.
- **Status:** `RunAgentSuite`/`RunThermalSuite`/`RunSnapshotSuite` exist + a
  server-level krucible integration test. Remaining: port sessions/piped/files/
  ringbuffer assertions into `enginetest` (`pkg/engine/enginetest/`). FC-only
  network tests are NOT ported (TSI obsoletes them).

### 5.5 Volumes  (medium-large) — `PLAN-krucible-productionization.md` §6e
- **Goal:** persistent + ephemeral volumes on krucible. The v0.3 `PersistentVolume`
  model (`pkg/store/volume.go`) is engine-agnostic and ports as-is.
- **Mechanics decided:** libkrun disks are boot-time only (no hotplug; `write_config`
  is a no-op) → attach = record + `krun_add_disk` on next launch; resize = grow file
  + `resize2fs` on boot. Cheap because cold wake is sub-second.
- **Versioning (the agent-first investment):** the cold tier already versions the
  whole VM; adopt the checkpoint = "Change" / bookmark / timeline model (§6e). Local
  CoW (clonefile/reflink/qcow2-overlay) + durable chunked-CDC.

### 5.6 Inter-sandbox networking + gateway  (medium) — §6a.2 / §6a.3
- Host↔guest forward (§6a.1) is **done** (`pkg/forward`, the `bhatti forward` CLI).
  Next: server-brokered per-sandbox host endpoints + name resolution (`<name>.sb`),
  and a sandbox→host gateway address.

### 5.7 Agent-first capability tokens  (medium) — §6b / `PLAN-krucible-v3.md` §12
- Per-sandbox token is **done** (config drive, enforced by lohar). Next: scoped
  caps `{exec, files:*, publish, net:egress, snapshot, fork}`, route middleware,
  audit to `events`, offline-mint, scoped share URLs. Track-J jail for hostile
  multi-tenant on Linux is separate (§11 of the v3 plan).

### 5.8 Unified event stream  (small-medium) — §6d
- The `EventRecorder` pub/sub bus exists. Next: a live fleet SSE/WS endpoint +
  richer event types (output/log/thermal/network).

## 6. Constraints & conventions (don't relitigate)

- **Never name third parties** (the comparable agent-sandbox runtimes, or the
  reference libkrun fork) in commits, files, comments, or docs. Refer generically
  ("the reference fork"). The reference fork is Apache-2.0; porting *code* is fine,
  unnamed. Ask the operator for its local clone path.
- **Hetzner stays on Firecracker, untouched.** krucible is a parallel engine; lohar
  is shared, so guest changes must not break FC (e.g. `setupNetworking` self-skips
  on krucible and is load-bearing on FC).
- **Single-writer server is the spine** — we deliberately did NOT build a daemonless
  CLI mode (multi-writer hazard). See `PLAN-krucible-productionization.md` §2.
- **Cold/fork rootfs = block device**, not virtio-fs (self-contained snapshot,
  faster, isolated). virtio-fs stays as the warm/dev profile.
- **lohar is PID-1 by design** under the kernel-direct block-root boot (M1′); the
  envisioned "slim" is moot (see `PLAN-krucible-init-model.md` DECISION).
- Commit per closed unit with descriptive, third-party-free messages; keep both repo
  trees clean.

## 7. Map of the code

- **bhatti** `pkg/engine/krucible/` — engine (`engine.go`), thermal (`thermal.go`),
  control socket (`control.go`), agent (`agent.go`), recovery (`recovery.go`),
  config drive build (in `engine.go` + `pkg/configdrive/`), tests (`*_test.go`).
  `cmd/vmm/main.go` is the cgo helper. `pkg/forward/` is the host↔guest bridge.
- **libkrucible** `src/vmm/src/lib.rs` (`Vmm`: pause/resume, checkpoint/restore,
  `VmCheckpoint`), `src/vmm/src/linux/vstate.rs` (KVM vCPU/VM state + events),
  `src/vmm/src/macos/vstate.rs` + `src/hvf/src/lib.rs` (HVF), `src/vmm/src/snapshot.rs`
  (guest-memory serialize), `src/vmm/src/device_manager/{kvm,hvf}/mmio.rs` (device
  persist), `src/devices/src/virtio/persist.rs` (device state), `src/libkrun/src/lib.rs`
  (C API: `krun_set_root_disk`/`set_data_disk`/`set_control_socket`/`set_snapshot`,
  the SNAPSHOT verb, the restore boot path, the kernel cmdline).
