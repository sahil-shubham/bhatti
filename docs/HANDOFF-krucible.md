# krucible — engineer hand-off

A practical entry point for picking up the krucible engine (bhatti's libkrun-fork
VMM). Read this first, then the design docs it links. **The actionable backlog is
§5.**

Companion docs (design rationale, deeper context):
`internal/PLAN-krucible-v3.md` (plan of record), `PLAN-krucible-productionization.md`
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

Repos: **`bhatti`** (Go daemon, branch `krucible`) + **`libkrucible`** — now a
**git submodule** at `./libkrucible` (branch `krucible`, the gitlink SHA is the
version-of-record; `make krucible` builds it `--no-default-features --features
blk`). The fork delta lives on `origin/krucible`; bump the gitlink to advance,
rebase onto upstream libkrun on your own cadence.

## 2. Current state (verified)

Feature matrix — every cell run as a test on that platform (see
`PLAN-krucible-productionization.md` §6f):

| Feature | darwin/arm64 (HVF) | linux/amd64 (KVM) | linux/arm64 (KVM) |
|---|:---:|:---:|:---:|
| Agent (exec/shell/files/sessions) | ✓ | ✓ | ✓ |
| Warm pause/resume | ✓ | ✓ | ✓ |
| Warm clock continuity (freeze) | ✓ | ✓ | ✓ (KVM_REG_ARM_TIMER_CNT) |
| Cold snapshot/restore | ✓ | ✓ | ✓ |
| Config drive (env/secrets/token) | ✓ | ✓ | ✓ |
| Host↔guest forward | ✓ | ✓ | ✓ |
| Recovery (restart-safe) | ✓ | ✓ | ✓ |
| Lean external kernel (~2x cold-start) | ✓ | ✓ | ✓ |

**Every cell is green on all three platforms — full cross-arch parity.** The last
gap, the linux/arm64 cold tier, closed with the GICv2 save/restore (§5.2). The
arm64 warm-clock freeze (§5.1): the EL2 CNTVOFF_EL2 one-reg ENOENTs, so we rewind
the guest-visible virtual counter (`KVM_REG_ARM_TIMER_CNT`, once on vCPU 0) —
`TestKrucibleClockFreeze` green on raspi-5a (delta 0.01s/3s).

**New this cycle (2026-06-27):**
- **Lean external kernel** (§5.9) — `krun_set_kernel` boots our own kernel,
  bypassing libkrunfw: **~2x faster cold-start** (boot→agent 312ms vs 610ms on
  HVF), validated cross-arch (mac/HVF + linux/arm64,x86 under KVM). Owned,
  reproducible config + build (`scripts/lean-kernel/`, `build-lean-kernel.sh`).
- **libkrucible vendored as a submodule** (§5.10) — gitlink = version-of-record.
- **Concurrency hardening** (§5.11) — fixed the concurrent wake-on-request
  double-launch race (per-VM `launchMu`) + a `Destroy` adopted-helper leak;
  regression test added. Surfaced under benchmarking.
- **Packaging/release/testing eval** (§5.12) — the pipeline is 100% FC; krucible
  has zero CI/release coverage. Expand testing next; design release later.

## 3. Build & test

### macOS (dev box, HVF)
```bash
git submodule update --init libkrucible   # fork is a submodule now (branch krucible)
make krucible   # builds libkrucible (cargo --features blk) + the install prefix
make vmm        # builds + codesigns bhatti-vmm (HVF entitlement)
make build      # the pure-Go daemon/CLI
./scripts/build-lean-kernel.sh aarch64    # lean kernel -> dist/kernel/ (Docker; daemon autodetects it)
go test -tags krucible ./pkg/engine/krucible/ -count=1   # full krucible suite
```
- `timeout(1)` is NOT available on the Mac.
- Toolchain: rustc 1.96, go 1.25.7, Docker (lean-kernel build), Homebrew libkrun/libkrunfw.
- The lean kernel is opt-in-by-presence: build it and the daemon autodetects
  `dist/kernel/*-lean-*`; absent, it falls back to the libkrunfw bundle.

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

## 4. The cluster

**On the home network use the LAN IPs directly (no Tailscale needed)** — from
`../another-attempt-at-local-infra/ansible/inventory.yml`:

| Node | LAN IP | Tailscale IP | arch | notes |
|---|---|---|---|---|
| asus-i5 | 192.168.1.4 (DHCP, floats) | 100.108.101.22 | x86_64 | primary x86 test box |
| raspi-5a | 192.168.1.201 | 100.119.145.44 | arm64 | primary arm64 test box |
| raspi-4b / raspi-5b | 192.168.1.200 / .202 | 100.66.66.124 / 100.79.148.43 | arm64 | spare (4b = k3s master) |

SSH: `ssh -i ~/.ssh/id_ed25519 user@<ip>` (LAN or Tailscale). Sources live under
`~/kr/{bhatti,libkrucible}` on each node. With the submodule layout, symlink the
in-repo path to the sibling on each node: `ln -sfn ~/kr/libkrucible ~/kr/bhatti/libkrucible`.
The k3s cluster also hosts the GitHub Actions `arc-runner-set` (self-hosted, has
`/dev/kvm`) that `integration.yml` uses — the natural home for a krucible CI job (§5.12).

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

### 5.2 arm64 cold tier (snapshot/restore)  — **DONE (2026-06-27)**
- **Goal:** `Stop`/`Start` (snapshot → free RAM → restore) on linux/arm64.
- **Outcome:** **green on raspi-5a** — `TestKrucibleSnapshotSuite` +
  `TestKrucibleColdTierMultiVcpu` (2 vCPUs, vtimer fires after restore, both CPUs
  online, two cold cycles). Full krucible suite green, no regression; x86 cold
  tier unaffected. The cold tier is now green on all three platforms.
- **What landed:** `KvmGicV2::{save_state,restore_state}` (libkrucible) capture/
  replay the in-kernel vGIC — GICD (group/priority/SPI-targets/config → enable →
  pending → active, GICD_CTLR last) + per-vCPU GICC (CTLR/PMR/BPR/ABPR/APR),
  honoring the per-vCPU banking of the 32 private IRQs (SGI/PPI) via the attr
  CPUID field. Read path uses a raw `KVM_GET_DEVICE_ATTR` ioctl (kvm-ioctls 0.24
  wraps only SET); blob tagged `GV2\x01`, restore refuses any other tag.
- **Key finding (reshapes the original plan): it's GICv2, not GICv3.** raspi-5a's
  host is a GIC-400 (`/proc/interrupts`: `GICv2 … vgic`), so KVM gives the guest a
  **vGICv2** — `KvmGicV3::new` fails and the builder falls back to `KvmGicV2`.
  GICv2 save/restore is *simpler* than v3 (distributor + CPU-interface regs; no
  redistributor/LPI/ITS). No GICv3 hardware exists in the cluster.
- **Done:** single `cold_tier` build cfg (`build.rs`, replacing ~21 copied gates);
  aarch64 `VcpuState` via `KVM_GET_REG_LIST` → `GET/SET_ONE_REG` + `mp_state`
  (`vstate.rs`); device persist widened to `cold_tier` (`device_manager/kvm/mmio.rs`);
  `Vmm` now owns the `intc`, and checkpoint/restore route VM-level state through
  `GICDevice::save_state/restore_state` on linux/aarch64 (`lib.rs`, `gic.rs`,
  `irqchip.rs`, `kvmgicv2.rs`).
- **Next:** implement `KvmGicV2::{save_state,restore_state}` — distributor
  (`KVM_DEV_ARM_VGIC_GRP_DIST_REGS`) + per-vCPU CPU interface (`…_CPU_REGS`),
  honoring the per-vCPU banking of the first 32 IRQs (SGI/PPI). Version-tagged
  opaque blob. (GICv3 left as the default-unsupported stub until there's hardware.)
- **Validate:** `TestKrucibleSnapshotSuite` green on raspi-5a. The x86 cold tier
  + the macOS HVF GIC capture are the proven siblings.

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

### 5.7 Agent-first capability tokens  (medium) — §6b / `internal/PLAN-krucible-v3.md` §12
- Per-sandbox token is **done** (config drive, enforced by lohar). Next: scoped
  caps `{exec, files:*, publish, net:egress, snapshot, fork}`, route middleware,
  audit to `events`, offline-mint, scoped share URLs. Track-J jail for hostile
  multi-tenant on Linux is separate (§11 of the v3 plan).

### 5.8 Unified event stream  (small-medium) — §6d
- The `EventRecorder` pub/sub bus exists. Next: a live fleet SSE/WS endpoint +
  richer event types (output/log/thermal/network).

### 5.9 Lean external kernel  — **DONE (2026-06-27); two follow-ups**
- **Outcome:** `krun_set_kernel` boots our own lean kernel, bypassing libkrunfw —
  **~2x faster cold-start** (boot→agent 312ms vs 610ms on HVF), validated
  cross-arch: mac/HVF + linux/arm64,x86 under KVM (`TestKrucibleLeanKernel*`,
  `TestKrucibleBlockRootAgentSuite`). The kernel is *ours* now — pinned,
  reproducible, 2x faster; libkrunfw bypassed on the block-root path (build+runtime).
- **What/why:** same 6.12.91 kernel as libkrunfw but a lean config (998 vs 1433
  `=y`) — less driver/subsystem init = faster boot + smaller footprint. Setting an
  external kernel makes `krun_start_enter` skip libkrunfw entirely.
- **Files:** `cmd/vmm/main.go` (`krun_set_kernel`, arch-aware cmdline),
  `pkg/engine/krucible/{engine,spec}.go` (`Config.KernelImage`),
  `cmd/bhatti/engine_krucible.go` + `pkg/config.go` (`krucible_kernel_image` +
  autodetect of `dist/kernel/*-lean-*`, fallback to bundle),
  `scripts/lean-kernel/config-lean_{aarch64,x86_64}` + `scripts/build-lean-kernel.sh`.
- **Follow-ups:** (1) **ship it** — build the lean kernel in `release.yml` + place
  it in `install.sh` (until then the 2x win is local-build-only). (2) drop the
  libkrunfw fallback once the lean kernel ships on all arches (the migration
  plan's "no libkrunfw" end-state). Per-tier fat kernel (docker/k8s) is future.

### 5.10 libkrucible as a submodule  — **DONE (2026-06-27)**
- libkrucible is a git submodule at `./libkrucible` (branch `krucible`); the
  gitlink SHA is the version-of-record, `make krucible` builds it from source
  (local == CI by construction). Matches the migration plan's packaging model.

### 5.11 Concurrency hardening  — **DONE (2026-06-27)**
- Fixed the concurrent wake-on-request **double-launch race** (the public proxy +
  every exec/file handler call `ensureHot` uncoalesced — a request burst on a
  non-hot sandbox spawned N helpers racing the same vsock UDS): per-VM `launchMu`
  serializes Start/Stop/Pause/Resume/Destroy. Also fixed a `Destroy`
  adopted-helper leak. Regression test `TestKrucibleConcurrentWakeNoDoubleLaunch`.
  Pure-Go — fixes all arches. Surfaced under benchmarking (`bench/krucible-mac.sh`).
- **Still open** (lower severity): exec-vs-Stop race (§2.3 of the review), a guard
  refusing cold `Stop` on a non-block-root VM, and cold-bundle integrity
  (fsync+atomic+hash).

### 5.12 Packaging / release / testing expansion  — **CI safety net DONE; release deferred**
- **CI safety net landed (2026-06-27):** (1) `krucible-build` job in `ci.yml`
  — builds the fork (libkrun) + cgo helper (bhatti-vmm) + krucible-tagged Go +
  pure-unit tests on GitHub-hosted runners (no KVM; VM suites self-skip via
  `hasHypervisor()`). (2) `krucible-integration.yml` — the full `-tags krucible`
  VM suite on the self-hosted `arc-runner-set` (real KVM), reusing
  `krucible-linux-bringup.sh`. Both gated to the `krucible` branch; **nothing is
  pushed yet** (the branch carries the migration history, scrubbed but unpublished).
  Steps were validated by hand on the cluster: raspi-5a (arm64) + asus-i5 (x86)
  agent/block-root/cold(x86)/clock all green.
- **First-run follow-ups (need a push to tune):** cache the libkrunfw kernel
  build in the integration job; split it into an arm64+x86 runner-arch matrix.
- **Release/install still deferred** (design locked, build later): per-platform
  `libkrun` + `bhatti-vmm` + lean kernel in `release.yml`/`install.sh`; the macOS
  full-stack install. **macOS distribution is now unblocked** — the operator has
  an Apple Developer membership, so notarization (not just ad-hoc + quarantine
  strip) is on the table. Ship the lean kernel here too (§5.9 follow-up).

### 5.12b (historical) Packaging / release / testing evaluation
- **Finding:** the pipeline is 100% Firecracker. `release.yml` builds CLI + FC
  kernel + tiers; `install.sh` installs an FC server (or macOS CLI-only);
  `ci.yml`/`integration.yml` have **zero krucible coverage** (krucible VM tests
  skip without libkrun; the cgo helper isn't even build-checked).
- **Do next (low-cost, infra exists):** (1) a **krucible CI build job** —
  `submodule update` + `make krucible`/`vmm` + cross-`cargo check` + no-VM units;
  (2) a **krucible integration job** on the `arc-runner-set` cluster runners
  (both arm64 + x86 have `/dev/kvm`), mirroring `integration.yml`; (3) the
  **lean-kernel build** in CI (reproducible via `build-lean-kernel.sh`).
- **Design now, build later (gated on parity + a macOS-distribution decision):**
  the release/install expansion — per-platform `libkrun` + `bhatti-vmm` + lean
  kernel, the macOS full-stack install, a krucible `config.yaml`. **The gating
  decision is macOS codesigning/notarization** (ad-hoc `-s -` + `xattr`
  quarantine-strip like the CLI, vs an Apple Developer cert).

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
