# PLAN — arm64-linux cold tier (§5.2, the last parity gap)

Status: **In progress (2026-06-18).** Closes the final strict-parity cell: cold
snapshot/restore (`Stop`/`Start`) on linux/arm64 (KVM). Warm tier + the arm64
warm-clock freeze (§5.1) are already green on raspi-5a. Companion:
`HANDOFF-krucible.md` §5.2, `PLAN-krucible-cold-tier.md` (the x86/macOS design
this extends).

**Progress:** steps 0–2 done + step 3 all but the GIC register table
(libkrucible `c43991b` cfg refactor, `e91b14a` vCPU + device + state plumbing).
Verified compiling on macOS/aarch64 (no regression) and linux/aarch64 (cross
check). **Remaining: the GICv2 distributor + CPU-interface register save/restore
(§4b), currently a clearly-marked runtime TODO in `KvmGicV2`.**

---

## 0. The two findings that reshape the handoff's §5.2

1. **The target hardware is GICv2, not GICv3.** raspi-5a's host interrupt
   controller is a GIC-400 (`/proc/interrupts` line 9: `GICv2 … vgic`). KVM's
   vGICv3 requires a GICv3 host, so `KvmGicV3::new` fails and `builder.rs` falls
   back to **`KvmGicV2`** — the guest runs a **vGICv2**. GICv2 save/restore is
   *materially simpler* than the handoff's GICv3 plan: distributor regs + CPU
   interface regs, **no redistributor, no LPI/ITS pending tables**. This is the
   single biggest scope reduction. (No GICv3 hardware exists in the cluster, so
   GICv3 save/restore is written-but-unvalidatable; see §5.)

2. **macOS/aarch64 cold tier already works — this is a port, not greenfield.**
   The aarch64 state shapes exist: `VcpuState` (macOS = `HvfVcpuState`),
   `VmState { gic_distributor }`, and the device-persist layer
   (`devices::virtio::persist::VmDevicesState`) is **arch-agnostic and shared**.
   The work is "capture the same aarch64 state via KVM ioctls instead of HVF
   calls," reusing the entire orchestration (`checkpoint`/`restore_and_resume`,
   the bundle writer, `validateBundle`, the restore boot path).

---

## 1. What already works (do not rebuild)

- **Orchestration** (`vmm/src/lib.rs`): `VmCheckpoint` (magic+version, serialize/
  deserialize), `checkpoint()` (pause → quiesce → save vcpu → save vm → snapshot
  devices → dump RAM → rearm), `restore_and_resume()` (load RAM → restore vm →
  restore devices → restore vcpu → resume), `start_vcpus_paused`. All gated on
  the cold-tier cfg; the body is arch-generic except the three save/restore
  primitives below.
- **Guest memory** (`vmm/src/snapshot.rs`): arch-agnostic, done.
- **Device persist** (`devices/src/virtio/persist.rs`, `device_manager/kvm/mmio.rs`):
  arch-agnostic (virtio queues/vsock/block/rng/console), done and used by x86.
- **bhatti side**: `Stop`/`Start`/`validateBundle`/`EnsureHot`/bundle manifest —
  all engine-level, arch-agnostic. The manifest already writes `arch:"aarch64"`
  on an arm64 build (the SNAPSHOT verb in `libkrun/src/lib.rs`). **No bhatti Go
  changes expected** beyond un-skipping `TestKrucibleSnapshotSuite` on arm64.

The three things that are x86-only today and must gain an aarch64-linux impl:
**(A) vCPU save/restore, (B) VM/GIC save/restore, (C) the cfg gates.**

---

## 2. The cfg-gate refactor (Phase 0 — do first, Mac-verifiable)

The string `#[cfg(any(all(target_os="macos",target_arch="aarch64"), all(target_os="linux",target_arch="x86_64")))]`
appears ~25× across `vmm/src/lib.rs`, `libkrun/src/lib.rs`, `vmm/src/.../mmio.rs`.
Widening to arm64-linux by editing 25 sites is error-prone.

**Decision:** introduce a single build-script cfg `cold_tier`, emitted per target
in each crate's `build.rs`:
```rust
// build.rs (vmm, libkrun, devices as needed)
let os = std::env::var("CARGO_CFG_TARGET_OS").unwrap_or_default();
let arch = std::env::var("CARGO_CFG_TARGET_ARCH").unwrap_or_default();
let cold = matches!((os.as_str(), arch.as_str()),
    ("macos","aarch64") | ("linux","x86_64") | ("linux","aarch64"));
if cold { println!("cargo:rustc-cfg=cold_tier"); }
println!("cargo:rustc-check-cfg=cfg(cold_tier)"); // silence unexpected-cfg lint
```
Then mechanically replace the 25 `#[cfg(any(...))]` with `#[cfg(cold_tier)]`.
**Widening to arm64 = adding one tuple to the `matches!`.** This is the only
change that touches the x86/macOS build, so it must be a no-op there — validate
by building libkrucible on the Mac (macOS/aarch64 still compiles identically) and
on asus-i5/raspi-5a (x86 stays cold-capable; arm64 *enables* the cfg, surfacing
the not-yet-written A/B as compile errors — the intended signal).

**Checkpoint 0:** macOS build unchanged; arm64-linux now compile-*fails* only on
the missing A/B impls. Commit the refactor as its own unit before A/B.

---

## 3. (A) aarch64-linux vCPU save/restore — `vmm/src/linux/vstate.rs`

Today `save_state`/`restore_state` + `struct VcpuState` are `#[cfg(x86_64)]`.
Add the aarch64 siblings.

**State captured (`VcpuState` aarch64):**
- **Core registers**: the `kvm_regs` set (X0–X30, SP, PC, PSTATE, SP_EL1,
  ELR_EL1, SPSR[], fp_regs) via `KVM_GET_ONE_REG` over the offsets the existing
  `arm64_core_reg!` macro already computes (`regs.rs`).
- **System registers**: enumerated **dynamically** via `KVM_GET_REG_LIST`
  (`vcpu.get_reg_list`) — the set is kernel/feature-dependent and must not be
  hardcoded. Save each via `GET_ONE_REG`, store `(reg_id, u64)` pairs.
- **MP state** (`KVM_GET_MP_STATE`) — needed so secondary vCPUs restore to the
  correct powered-on/off PSCI state.
- **arch-timer** is part of the sysreg list (`KVM_REG_ARM_TIMER_CNT/CVAL/CTL`),
  captured for free by the reg-list sweep — but see the ordering note.

**Serialize:** length-prefixed `(reg_id:u64, val:u64)` pairs + mp_state, mirroring
the macOS `HvfVcpuState` serialize. Hand-rolled (no serde), like the x86 path.

**Restore ordering (load-bearing):**
1. vCPU already created + `KVM_ARM_VCPU_INIT`'d with the **same features** as
   boot (the restore boot path re-runs `configure_aarch64`).
2. `SET_ONE_REG` for every saved sysreg, then core regs, then `SET_MP_STATE`.
   Restoring the full reg list is order-tolerant for most regs; the known
   exception is that **`KVM_REG_ARM_TIMER_CNT` is VM-global** — restore it once
   (it's also touched by §5.1's freeze; the cold path resumes with
   `paused_ns=0`, and the saved CNT is the absolute value, so restoring it on
   each vCPU is idempotent for v-counter but we still apply once for cleanliness).
3. Skip regs KVM rejects on SET (read-only IDs); log+continue rather than abort,
   matching the reference pattern.

**Wire into the StateMachine:** the `paused()` handler already routes
`VcpuEvent::SaveState`/`RestoreState` but those arms are `#[cfg(x86_64)]`. Widen
to `cold_tier` and call the aarch64 `save_state`/`restore_state`. (`Vcpu` already
exposes `self.fd`, `self.id`.)

**Risk:** medium. Well-trodden (the reference VMM + cloud-hypervisor do exactly
this). The reg-list sweep is the fiddly part (some regs need size masks honored
from the reg_id, not assumed u64 — e.g. fp_regs are 128-bit). **Pause point:** if
any reg is non-u64-sized, handle the size field properly rather than truncating.

---

## 4. (B) aarch64-linux VM/GIC save/restore

### 4a. Structural: `Vmm` must own the `intc`
`Vmm` has no `intc` field; the GIC is created in `builder.rs` and only passed to
`configure_system`/`attach_legacy_devices`. macOS reaches its GIC via the global
`hvf::gic_save_distributor()`, but KVM's vGIC is a per-VM `DeviceFd` inside
`KvmGicV2`. **Add `intc: IrqChip` to `Vmm`** (the `Arc<Mutex<IrqChipDevice>>`
already built), set it in the builder. Architecturally correct independent of
snapshots — the VMM should own its interrupt controller. (x86's irqchip is the
VM fd, so no change there.)

### 4b. GIC save/restore on the `GICDevice` trait
Add to the `GICDevice` trait (`devices/src/legacy/gic.rs`):
```rust
fn save_state(&self) -> Result<Vec<u8>, Error>;        // version-tagged, opaque
fn restore_state(&self, blob: &[u8]) -> Result<(), Error>;
```
- **`KvmGicV2`** (the validated path): un-prefix `_device_fd`. Save/restore via
  `KVM_DEV_ARM_VGIC_GRP_DIST_REGS` (distributor) + `KVM_DEV_ARM_VGIC_GRP_CPU_REGS`
  (CPU interface, per-vcpu) using `get_device_attr`/`set_device_attr` over the
  GICv2 register offset ranges. Tag the blob `gicv2/v1`.
- **`KvmGicV3`** (written, *unvalidatable* — no hardware): `DIST_REGS` +
  `REDIST_REGS` (per-vcpu) + `CPU_SYSREGS` + `LEVEL_INFO`, plus
  `KVM_DEV_ARM_VGIC_GRP_CTRL/SAVE_PENDING_TABLES` before save. Tag `gicv3/v1`.
- Default trait impls (`GicV3` userspace fallback, the macOS HVF types) return an
  "unsupported" error or keep their existing HVF capture — **only the KVM types
  gain real impls here**, macOS is untouched.

### 4c. aarch64-linux `VmState`
Add `#[cfg(all(cold_tier, target_arch="aarch64", target_os="linux"))]`:
```rust
pub struct VmState { gic: Vec<u8> }   // opaque, version-tagged by the GIC impl
```
Built by `checkpoint()` from `self.intc.lock().save_state()`, applied by
`restore_and_resume()` via `self.intc.lock().restore_state(&vm_state.gic)`. This
diverges slightly from the x86 pattern (where `Vm::save_state()` produces it) —
cleaner than forcing the GIC fd into `Vm`. Keep `Vm::save_state`/`restore_state`
as x86-only; add a thin aarch64 path in `checkpoint`/`restore_and_resume`
(`#[cfg]`-split the two lines that build/apply `vm_state`).

**GIC restore ordering:** vCPUs must be created + `VCPU_INIT`'d and the vGIC
`CTRL_INIT`'d (done at boot) before setting DIST/CPU regs. For GICv2 there's no
redistributor base to place per-vcpu (that's a GICv3 concern), so ordering is
just: vGIC created → vCPU regs restored → GIC regs restored → resume.

**Risk:** GICv2 — low/medium (small, well-defined register set). GICv3 — can't
validate, so treat as best-effort and clearly marked.

---

## 5. Multi-GIC-version handling (the architecturally-right call)

The cluster is GICv2-only, but the engine must not silently corrupt on a GICv3
host. The version-tagged opaque blob + the trait method make this clean:
- Save tags the blob with the live GIC version; restore checks the tag against
  the GIC the restored VM built and **refuses a mismatch** (a clear error, the
  same classify-or-refuse posture as `validateBundle`). A bundle is already
  arch-gated host-side; GIC-version is the intra-arch refinement.
- GICv2 is implemented + validated on raspi-5a. GICv3 is implemented from the
  standard KVM device-attr sequence but **flagged untested** in code + the matrix
  (no GICv3 hardware). We do not claim GICv3-arm64 green until there's hardware.

---

## 6. Execution order & checkpoints

Each step ends at a green gate; **pause on any deviation** (per the operator's
instruction) and take the structural fix, not a workaround.

| # | Step | Where | Validate | Risk |
|---|---|---|---|---|
| 0 | cfg-gate → `cold_tier` build cfg | Mac + Pi | macOS build unchanged; arm64 now compile-fails only on missing A/B | low |
| 1 | local `aarch64-unknown-linux-gnu` `cargo check -p krun-vmm` loop | Mac | typechecks A/B without Pi round-trips | low |
| 2 | (A) vCPU save/restore | `linux/vstate.rs`, `regs.rs` | unit: reg-list non-empty, round-trip a vCPU's regs on the Pi | med |
| 3 | (B) `Vmm.intc` + GICv2 trait save/restore + aarch64 `VmState` | `lib.rs`, `builder.rs`, `gic.rs`, `kvmgicv2.rs`, `vstate.rs` | `TestKrucibleSnapshotSuite` Stop/Start + exec-after-restore + RAM-survived on raspi-5a | med |
| 4 | widen the `cold_tier` matches! to `(linux,aarch64)` end-to-end | `build.rs` ×N | `SNAPSHOT` verb no longer returns "not supported"; full suite green on Pi | low |
| 5 | GICv3 impl (best-effort, unvalidated) + version-tag refuse | `kvmgicv3.rs`, `gic.rs` | compiles; refuses cross-version; **not** marked green | low (no hw) |
| 6 | docs + matrix flip (arm64 cold = ✓), un-skip Go suite | bhatti | suite green; HANDOFF/§6f updated | — |

**Local typecheck loop (step 1)** is worth the one-time setup: `rustup target add
aarch64-unknown-linux-gnu`; `cargo check --target aarch64-unknown-linux-gnu -p
krun-vmm --no-default-features --features blk`. `check` doesn't link, so the
absence of an aarch64 cross-linker is fine; it surfaces type errors in the
arch-gated code in seconds instead of a Pi rebuild.

## 7. Known pause-points (take the right fix, don't paper over)

- **Non-u64 sysregs** (fp_regs are 128-bit): honor the size encoded in the
  reg_id; don't truncate.
- **Reg restore rejections**: some reg IDs are read-only on SET — log + skip, but
  *verify* the skip set matches the reference, don't blanket-ignore errors.
- **GIC `nr_irqs`/addresses** must match boot exactly on restore (they do — the
  restore boot path rebuilds the same VM config), but assert it rather than
  assume.
- **`Vmm.intc` ownership** may ripple into the builder's move/borrow of `intc`
  (it's currently consumed by `configure_system`). If so, store the `Arc` clone
  in `Vmm` and pass clones — the right fix, not a `RefCell` hack.
- If GICv2 `CPU_REGS` capture proves per-vcpu-ordering sensitive, capture in vcpu
  index order and restore likewise; don't race it.
