# krucible cold tier (P3) — architecture & plan of record

Status: **Draft (2026-06-15).** Detailed design for the cold-to-disk / snapshot tier, expanding `docs/internal/PLAN-krucible-v3.md`
§5 (tier ladder), §7.1 (owned snapshot change), and §8 (P3 gate). Written after scoping the full snapshot port against
libkrucible's *actual* code (not the reference fork's), which surfaced one load-bearing architectural decision (the rootfs
model) that this doc settles.

Companion: `docs/internal/PLAN-krucible-v3.md` (the migration plan of record), `docs/thermal-management.md` (the tier model),
`docs/archive/PLAN-snapshot-reliability-fixes.md` (FC scar tissue — the behaviors we must not re-pay for).

---

## Status update (2026-06-16): cold tier COMPLETE on HVF (exec-after-restore closed)

**P3 gate met.** The full cold-wake-survives-restart works end-to-end through the engine, with exec **and** guest RAM
intact across the round-trip. `TestKrucibleSnapshotSuite` (engine-level, FC-parity-ready `enginetest.RunSnapshotSuite`):
`Create` (block root) → write a tmpfs marker → exec → `Stop` (PAUSE + `SNAPSHOT` + kill the helper, RAM freed) → `Start`
(restore) → **exec-after-restore works** and **the tmpfs marker survived** (guest RAM round-tripped).

The exec-after-restore gap was closed via the **block root** (the decision below): a CoW ext4 image (built once from the
rootfs via `mke2fs -d`, cloned per-sandbox), booted **kernel-direct** (`root=/dev/vda rootfstype=ext4` in the cmdline —
the bundled kernel has virtio-blk+ext4 built in, so no init-blob/init-toolchain and lohar stays PID 1). The block device
persist + the block-backed rootfs survive the snapshot, so the restored guest's fs is consistent and exec works — no FUSE
persist needed. virtio-fs remains the warm/dev profile (`BlockRoot=false`).

What's left on the cold tier is hardening, not capability: bundle integrity/atomic-rename + tamper-refuse, the manifest
arch/feature gate (Tier 2), and the lohar slimming (§ init-model doc). The core mechanism is done.

---

## (historical) Status update: core cold-wake VALIDATED end-to-end on HVF

The VMM cold-wake machinery is **done and proven** by a loopback integration test
(`TestColdLoopbackRestore`): boot → agent ready → `PAUSE` + `SNAPSHOT <dir>` →
kill the helper (free RAM) → restore into a fresh helper from the bundle → **the
guest resumes and lohar answers an `Activity` request.** Memory + vCPU + GIC
(incl. the per-IRQ pending/active state — a guest paused mid-ISR EOIs cleanly) +
vsock/console/rng device state all round-trip. Layers 1→7b are committed.

**The one remaining gap is the rootfs after restore.** `exec`-after-restore hangs
because the root is **virtio-fs**, whose FUSE inode map is not persisted (it goes
stale on a fresh server). The fix is a block root (§1) — but with a new, concrete
finding: **`krun_set_root_disk` alone does not boot a block root under PID-1.**
libkrun's block-boot mounts `/dev/vda` and pivots from its *own* init (`init.c`);
we disable that init so lohar is PID 1 (`init=/init.krun`, `rootfstype=virtiofs`,
`nomodule`, no `root=`), so a block root would need lohar to `switch_root` itself.

**Decision (2026-06-16, supersedes §1's block-root call below): the fix is FUSE
state persist on the existing virtio-fs root — NOT a block root.** The reference
fork solves exec-after-restore exactly this way: capture the FUSE server's logical
state (nodeid→`(dev,ino)` via volfs, open handles, the inode counter, writeback
flags), and on restore rebuild the map on a fresh server (volfs makes inodes
addressable by `(dev,ino)` with no held fd) and reopen handles — the guest's
cached node-ids resolve and exec works. This **keeps bhatti's design intact**
(virtio-fs + lohar-as-PID-1); the block-root detour and its PID-1 boot friction
were self-inflicted. The port is tractable: libkrucible's macOS passthrough is
volfs-compatible (the hard part — inode identity — ports directly); the
`AugmentFs`/`inode_alloc` delta is small (`inode_alloc` is one `AtomicU64`, the
virtual entries are deterministic). §1 below (block-root) is retained for history
but is **not** the chosen path; block-root remains a *possible* future profile,
and if pursued needs the guest-side `switch_root` noted above.

**Separate strategic question (P4+, do not couple to the snapshot work):** our
`lohar-as-PID-1` (`disable_implicit_init`) is a Firecracker-ism. The reference /
idiomatic-libkrun model is libkrun's `init.c` → a real init → the agent as a
*service* (not PID 1). That model makes block-root *and* virtio-fs work, frees the
agent from PID-1 duties (mounts, zombie reaping, reboot/SIGHUP — the W4 bricked-Pi
risks), and stops fighting the VMM. It is a guest-model change worth a deliberate
evaluation — but on its own gate, not under cold-tier.

---

## 0. Where we are (what's committed)

Warm tier (P2) is **done and green** on Mac/HVF. Cold tier (P3) is **in progress**, built as committed, compiling,
independently-tested layers on libkrucible:

| Layer | What | State |
|---|---|---|
| 1 | guest-memory eager serialize/restore (`snapshot.rs`) | ✅ committed, unit-tested |
| 2 | HVF vCPU + GIC state capture/restore (`HvfVcpuState`, `gic_save/restore_distributor`) | ✅ committed, serialize-tested |
| 3 | macОS vstate plumbing (`SaveState`/`RestoreState` events, `VcpuState`/`VmState`, `Vm::save_state`) | ✅ committed, unit-tested |
| 5a | virtio `QueueState` + `Queue::save_state/restore` | ✅ committed |
| 5b | vsock device persist (`VsockState`) | ✅ committed (WIP — not yet wired) |
| 5b… | console / rng / **block** device persist | ⬜ |
| 5c | `persist.rs` aggregator (`VmDevicesState`, JSON) | ⬜ |
| 5d | device-manager `snapshot/restore_devices` + MMIO transport queue rebuild | ⬜ |
| 6 | `VmCheckpoint` + `Vmm::checkpoint`/`restore` + `save/restore_vcpu_states` | ⬜ |
| 7 | `SNAPSHOT <dir>` control verb + `krun_set_snapshot` eager `build_restore_ctx` | ⬜ |
| 8 | bhatti `pkg/bundle`, engine `Snapshot`/`Stop`/`Start`, `RunSnapshotSuite` | ⬜ |

Everything above 5b is a tractable port from the reference fork (Apache-2.0; device models, GIC, memory, queue all line
up). **One thing is not a port: virtio-fs.** That's the architectural fork in the road this doc resolves.

---

## 1. The load-bearing decision: the cold-tier rootfs is a block device, not virtio-fs

### The problem
libkrucible's virtio-fs stack diverged from the reference: the passthrough is wrapped in an **`AugmentFs`** layer over a
shared **`inode_alloc`**, behind a **`FsServer` enum** (read-write + read-only variants), owned by the worker thread. The
reference's FUSE persist (`PassthroughFs::snapshot/restore` + worker `quiesce_for_snapshot` + `pending_fuse`) assumes the
un-wrapped passthrough and a different worker lifecycle, so it is **net-new design on libkrucible, not a port**.

And virtio-fs *needs* that work for cold-wake: the guest kernel caches FUSE node-ids in its dcache/icache. A restored VM
gets a **fresh** FUSE server; unless the nodeid→`(dev,ino)` map (and the `inode_alloc` counter, refcounts, open handles,
and `AugmentFs` virtual entries) are persisted and rebuilt, the guest's first `execve("/bin/sh")` walks stale node-ids →
`ESTALE`. (Pure stable-nodeid addressing can't fully sidestep this: `(dev:64, ino:64)` doesn't pack into a 64-bit nodeid,
which is exactly why the reference keeps a map.)

### The decision
**Cold-tier (and fork-tier) guests root on a block device (raw/qcow2 via `krun_set_root_disk` / `krun_add_disk2`), not
virtio-fs.** Rationale, in first-principles order:

- **Block persist is trivial and robust.** A block device's entire snapshot state is `acked_features` + `activated` + one
  `QueueState`; the data lives in the backing image on disk, already consistent at a quiesced checkpoint. No inode map, no
  handle reopen, no `ESTALE` class of bugs. This is the dominant correctness argument.
- **It is what the plan already intends.** `docs/internal/PLAN-krucible-v3.md` §2/§6 maps the krucible rootfs to
  `krun_create_disk_overlay` **qcow2 CoW** (the FC reflink-`cp` replacement) and explicitly defers it to P3 "for
  CoW/snapshot." The krucible engine's own `spec.go` comment already says *"qcow2 CoW overlays arrive with snapshot in
  P3."* virtio-fs (`krun_set_root` on a host dir) was the **S0/P1 fast-boot path**, never the snapshot path.
- **The disk image is the portable artifact.** A qcow2 base + per-sandbox overlay is the natural unit for cold storage,
  cross-machine move (Tier 2), and fork fan-out (Tier 3) — the same overlay travels with the memory image. virtio-fs has
  no portable on-disk artifact to pair with a memory snapshot.
- **It removes the one bespoke blocker** so the *entire* snapshot/restore pipeline (memory + vCPU + GIC + block/vsock/
  console queues + checkpoint + control verb + eager restore + bundle) lands and proves out end-to-end.

### What this costs / changes
- The cold-tier guest rootfs becomes a **built ext4/qcow2 image** (lohar at `/init.krun` + base userland), attached via
  `krun_set_root_disk` (or `krun_add_disk2` + `root=/dev/vda` cmdline). The `mke2fs -d` tooling already exists
  (`pkg/engine/guestfs/configdrive.go`, extracted from FC). The base image build is a bounded addition to
  `scripts/krucible-rootfs.sh`.
- The engine's `Create` path gains an overlay step (`krun_create_disk_overlay(base, overlay)`), replacing the host-dir
  `cloneTree` for cold-capable sandboxes. CoW overlay on APFS/any-FS (no btrfs/XFS reflink requirement) — a strict
  improvement over the FC reflink dependency.
- **virtio-fs stays** as the default for warm-only / fast-iteration sandboxes (host-dir convenience, no image build). Only
  cold/fork-capable sandboxes require a block root. This is a per-profile choice, not a global swap.

### virtio-fs + cold-wake (deferred capability, §8)
If we later want virtio-fs **and** cold-wake (e.g. live host-dir sharing that survives a daemon restart), the fs FUSE
persist becomes a scoped follow-up: persist `inode_alloc` (counter + nodeid→`(dev,ino)` via the volfs identity libkrucible
already uses), the `AugmentFs` virtual entries, refcounts, and open handles; rebuild lazily through volfs on restore. This
is real design on libkrucible's `AugmentFs`/`inode_alloc` stack and is **not** on the P3 critical path. Tracked as a
distinct capability, gated by its own tests.

---

## 2. Bundle format (the `.bhatti` cold-storage unit)

A self-contained directory (the unit `Stop`/`Snapshot` writes and `Start`/`ResumeFromBundle` reads). Survives the VMM
helper exiting and a daemon restart.

```
<sandbox>.bhatti/
  manifest.json     # compatibility gate + layout (below)
  memory.img        # eager guest RAM, region-ordered (snapshot::write_guest_memory)
  checkpoint.bin    # VmCheckpoint: VmState (GIC distributor) + Vec<VcpuState> + VmDevicesState
  disk/             # qcow2 overlay(s) — the rootfs + any data disks (or a content-addressed ref)
```

`manifest.json` is the **refuse-or-restore gate** (Tier 2/3 safety, §4):
```json
{
  "proto_ver": 1,
  "arch": "aarch64",                 // refuse cross-arch (an arm64 image can't resume on x86)
  "feature_hash": "<sha256>",        // host CPU feature/ID-reg fingerprint; refuse on downgrade
  "mem_layout": [{"gpa":0,"len":...}],
  "vm_config": {"vcpus":1,"ram_mib":512},
  "vcpu_count": 1,
  "disks": [{"id":"root","base":"<sha>","format":"qcow2"}]
}
```

Format discipline (from `PLAN-snapshot-reliability-fixes.md`): **fsync + atomic rename** the bundle into place; a
half-written bundle must never be loadable (validate magic/length/hash before restore; refuse on tamper). These are the
`RunSnapshotSuite` assertions (`TestSnapshotBundleSelfContained`, `TestBundleRejectsTampered`), ported as the spec.

---

## 3. Restore boot path (the sequencing that makes or breaks it)

A restore is a **fresh VMM process** (the helper exited to free RAM) booted via `krun_set_snapshot(<dir>)`. The order is
load-bearing — it's where the e2e-only bugs live:

1. **Build the ctx** from `manifest.vm_config` (same vcpu count, RAM layout). Attach the disk overlay(s).
2. **Allocate guest memory** with the manifest's layout, then **eagerly load** `memory.img`
   (`read_guest_memory_into`) — before any vCPU runs.
3. **Build devices fresh**, but do **not** let them self-activate from the guest's MMIO (the guest won't re-init). Instead,
   for each device in `VmDevicesState`: set `acked_features`, rebuild each queue from its `QueueState`, and **re-activate**
   the device against restored guest memory (`restore_activate_devices`). Workers start with queues already at the saved
   ring positions.
4. **Restore VM-level state** — replay the GIC **distributor** (`Vm::restore_state`, `GICD_CTLR` last) so SPIs (vsock/blk)
   aren't masked.
5. **Restore per-vCPU state on the vCPU thread, while paused** — `RestoreState` event → `vcpu_restore_state` (GP/PC/PSTATE,
   EL1 sysregs incl. pointer-auth keys + vtimer arm, per-vCPU GIC redist+ICC, **`CNTVOFF` set absolutely**). HVF requires
   register access on the owning thread, so this must ride the paused event loop, not the control thread.
6. **Resume.** The guest continues from the exact instruction, clock continued (no jump), devices at their saved indices.

Hazards to encode as tests/asserts: memory loaded before vCPU run; queues restored before re-activation; GIC distributor
before vCPU ICC; `CNTVOFF` absolute (not the warm-tier delta nudge); vsock host side reconnects (no live connections in the
image).

---

## 4. Tier ladder & portability gates (manifest-driven)

Maps `internal/PLAN-krucible-v3.md` §5 onto the bundle:

| Tier | Capability | Gate | Status |
|---|---|---|---|
| 1 | cold-to-disk, restore on the **same/identical** box | `arch` match + exact `feature_hash` | **this work (P3)** |
| 2 | restore across **same-arch, different CPU** | `feature_hash` classify: portable/mask/**translate `CNTVOFF`**/**skip+refuse-on-downgrade** | deferred (§7.2 of v3) |
| — | cross-**arch** | **refuse** (`arch` mismatch) — physically impossible | enforced from day 1 |

The `feature_hash` is the CPU-compatibility fingerprint (ID_AA64*/MIDR/cache-topo/SME-ID on arm64; CPUID/MSR leaves on
x86). Tier 1 demands an exact match (same box). Tier 2 relaxes it to the classify-and-refuse-on-feature-loss rule. The
manifest carries it so a wrong-host restore **refuses before** corrupting a guest, not after.

---

## 5. Device persist framework (the portable core)

Per-device `*State` (serde) + `save_state`/`restore_state`, aggregated by `persist.rs::VmDevicesState` (JSON), captured/
restored by the device manager over its activated virtio devices (downcast via the existing `AsAny` supertrait). Scope for
the cold-tier MVP (the krucible guest's active set, minus fs):

- **block** — `acked_features` + `activated` + `QueueState` (+ `disk_image_id`). Trivial. **Primary rootfs.**
- **vsock** — done (5b): cid + features + rx/tx queues; connections/listeners not captured (agent re-dials).
- **console** — features + per-queue states.
- **rng** — features + queue.
- **balloon** — skipped (no guest-liveness dependency; matches reference).
- **fs (virtio-fs)** — **deferred** (§1); cold-capable guests use a block root.

`QueueState` (5a) is the shared substrate. The MMIO transport gains a "rebuild queues from `QueueState` + re-activate" path
(5d) — the one piece that touches libkrucible's transport, kept small.

---

## 6. Owning the delta & syncing upstream (the maintenance posture)

We diverged from the reference fork (expected — it was a temporary reference, and we'd sync upstream periodically anyway).
The snapshot work is **owned libkrucible code** now. To keep the merge tax bounded (v3 §7.4 risk):

- **Additive-first.** New capability lives in new files where possible (`snapshot.rs`, `persist.rs`, the hvf state block),
  minimizing conflicts with upstream churn. Touch existing files (vstate, queue, device_manager) surgically.
- **`REBASE.md` + green-at-SHA CI.** libkrucible gets a `cargo test` gate (the serialize roundtrips + a loopback
  snapshot/restore on HVF). Bumping the bhatti submodule SHA is gated on green-at-SHA.
- **The bhatti suite is the oracle.** `RunSnapshotSuite` passing on libkrucible is the port-correctness proof; never weaken
  an assertion to make a port pass.

---

## 7. Revised sequencing (block-root decision folded in)

1. Finish device persist: **block** (priority — it's the rootfs), console, rng, wire vsock. → `persist.rs`.
2. Device-manager `snapshot/restore_devices` + transport queue rebuild (5d).
3. `VmCheckpoint` + `Vmm::checkpoint`/`restore` + `save/restore_vcpu_states` (6).
4. `SNAPSHOT <dir>` verb + `krun_set_snapshot` eager `build_restore_ctx` (7).
5. **Engine block root:** build an ext4/qcow2 base (lohar + userland), `krun_create_disk_overlay` in `Create`,
   `krun_set_root_disk`. A cold-capable profile alongside the virtio-fs warm profile.
6. bhatti `pkg/bundle` + engine `Snapshot`/`Stop`/`Start` + `RunSnapshotSuite` (8). Drive to a green
   **cold-wake-survives-daemon-restart** on Mac/HVF.
7. (Later, optional) virtio-fs FUSE persist as its own capability + gate.

P3 gate (unchanged from v3 §8): cold-wake works and survives a daemon restart on Mac (+ x86-Linux when the cluster is in
the loop); the bhatti suite is green on libkrucible.

---

## 8. Open questions

1. **Block-root base image pipeline** — reuse the `mke2fs -d` configdrive tooling for the rootfs image, or build a qcow2
   directly? (Lean: ext4 raw base + qcow2 overlay via `krun_create_disk_overlay`.)
2. **Single vs split disks** — rootfs overlay only, or a separate persistent data disk (so `/workspace` survives a base
   rebump)? (Defer; rootfs overlay first.)
3. **`feature_hash` contents** — minimal arm64 set for Tier 1 (exact-match) now; the full classify model is Tier 2.
4. **Bundle disk storage** — copy the overlay into the `.bhatti` dir, or content-address the base + store only the overlay
   delta (fork fan-out dedup)? (Defer past Tier 1; copy/ref the overlay for now.)
5. **Warm→cold transition** — `Stop` pauses then `SNAPSHOT`s then exits the helper; confirm the overlay is fsync'd into the
   bundle atomically with the memory image (crash-consistency).
