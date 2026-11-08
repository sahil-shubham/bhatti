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
| qcow2 CoW root + overlay (default substrate) | ✓ | ✓ | ✓ |
| Image save + `create --image` | ✓ | ✓ | ✓ |
| Named snapshot create/resume (memory) | ✓ | ✓ | ✓ |
| Filesystem snapshot (`--type`, disk-only) | ✓ | ✓ | ✓ |
| Fork (`create --from`, instant memory clone) | ✓ | ✓ | ✓ |
| Live `--mount` (virtio-fs host-dir bind) | ✓ | ✓ | ✓ |
| Block `volume` (persistent data disk, `/dev/vdc+`) | ✓ | ✓ | ✓ |
| Fork/snapshot restores the device set (volumes) | ✓ | ✓ | ✓ |

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

**New this cycle (2026-06-30) — storage rework + Phase 2:**
- **qcow2 CoW substrate, default** — images/snapshots/volumes unify on one
  copy-on-write format (host-FS-independent; no btrfs needed). `krun_set_root_disk2`
  (qcow2-capable root) + `krun_create_disk_overlay` (imago-backed overlay) in the
  fork; `KRUCIBLE_ROOT_RAW=1` opts out. ~0.5% overhead on agent workloads.
- **Phase 2 capture/instantiate** — per-create image selection + `SaveImage`;
  named memory snapshots (`Checkpoint`) + `ResumeFromManifestJSON`; **fork**
  (`create --from`, instant memory clone — checkpoint+restore into a fresh
  identity); `snapshot create --type filesystem|memory` (filesystem = disk-only,
  cold-boot restore). All wired end-to-end (CLI→server→engine; FC unaffected via
  optional capabilities) with integration tests.
- **HVF multi-vCPU cold-restore deadlock fixed** — secondary vCPU no longer
  blocks on a PSCI bring-up that never comes on restore (skip `boot_receiver`
  when `start_paused`). Cold tier now full multi-vCPU on all three platforms.
- **Validated cross-arch (2026-06-30)** — full krucible suite green on darwin/arm64
  (HVF), linux/arm64 (KVM, raspi-5a: 25 PASS/4 SKIP), linux/amd64 (KVM, asus-i5:
  25 PASS/4 SKIP). libkrucible `4e82af6`, bhatti `origin/krucible`.
- **CI** — `krucible-build` (GitHub-hosted, VM suites self-skip without a
  hypervisor) green; `krucible-integration` (self-hosted arc-runner-set) is the
  KVM gate. NOTE: the Firecracker homelab integration workflow was **retired**
  from the cluster (its `10/8` host-iptables collided with k3s/flannel and broke
  cross-node DNS); arc-runner-set now serves krucible only.

**New this cycle (2026-07-01) — storage Phase 3 (mounts):**
- **Live `--mount host:guest[:ro]`** — a virtio-fs host-directory bind
  (`krun_add_virtiofs3` host-side + a guest mount by lohar from the config
  drive). Shared, bidirectional, N-writer — edit on the host, run in the sandbox
  (and vice-versa) live. Wired end-to-end (engine.FsMount → CLI/server → vmm +
  lohar); FC ignores it. `TestKrucibleMount` green on all three platforms.
- **Codesign safety-net** — a plain `go build -o bhatti-vmm` strips the HVF
  hypervisor entitlement that `make vmm` applies, so `hv_vm_create` fails for
  every VM on darwin (this masqueraded as a bogus “--mount HVF limitation” for a
  while). The test harness now re-signs on darwin (idempotent) so tests pass
  regardless of how the binary was built. **Always `make vmm` on macOS.**
- **Block `volume` wiring** — krucible now honors `spec.ResolvedVolumes`: each
  attaches as a block disk after root (vda) + config (vdb) → `/dev/vdc+` via
  `krun_add_disk2`, mounted by lohar from the config drive. `create --volume
  name:mount[:ro]` works end-to-end (server volume resolution was already
  engine-agnostic). **Substrate fix (libkrun `175f28c`):** `get_block_cfg()`
  treated `set_root_disk*`/`set_data_disk` and `add_disk*` as mutually exclusive
  (any add_disk silently dropped root/data); now it composes `[root?, data?,
  ...adds]` so a block-root sandbox keeps `root=/dev/vda` (is_block_root keys on
  root_block_cfg) *and* attaches volumes. `TestKrucibleVolume` (attach + mount +
  persist across sandboxes) green on all three platforms.
- **Snapshot/fork restores the device set (volumes)** — a memory snapshot/fork now
  reproduces attached volumes, not just RAM+root: Checkpoint freezes each volume
  (independent copy, consistent with the paused VM) + records it in the manifest;
  Resume re-attaches each as `/dev/vdc+`, so the restored RAM's disk view stays
  valid. **Fork-your-whole-environment:** the fork's volumes are its own (since
  2026-11-08 an overlay over the frozen copy; before, a copy of it — libkrun block
  devices are boot-fixed, so a live source can't be rebased, and the frozen copy is
  what is shared). `TestKrucibleForkWithVolume` green on all three platforms.
- **Honest limit — virtio-fs `--mount` + memory snapshot:** libkrun can't restore a
  virtio-fs device from a memory snapshot (the resume hangs), so memory-snapshot/
  fork of a *mounted* sandbox is **refused cleanly** (fast error → use `--type
  filesystem`). `TestKrucibleForkMountedRefused`.
- **Next (storage, deferred):** (1) volume-management commands — offline `volume
  clone` (instant qcow2 overlay of a stopped volume) + `volume snapshot`/version;
  requires migrating fresh volumes to qcow2 (the raw-ext4 store today gives instant
  clone only on CoW host FS via cloneFile). (2) restore-with-mounts (needs a libkrun
  fix for virtio-fs snapshot restore). (3) the unified-node DAG/listing UX.

**Snapshot nodes and disk authority (2026-11-08):**
- **Disk preparation is independent of the saved delta.** A normal resume makes a
  qcow2 overlay over each immutable snapshot disk, rather than copying that disk.
  The node store names those disks by `<DataDir>/nodes/<dev>-<ino>.img` hard links;
  clones share the saved blocks but write only their own overlays. An incompatible
  filesystem, a relative backing name, a raw disk whose size is not sector-aligned,
  or the eight-qcow2-image depth cap takes the existing move/copy fallback instead.
  This is an O(1) disk-preparation claim, not a promise that guest boot time or
  parallel scheduling is constant.
- **Formats and dependencies are daemon metadata.** Checkpoint and import publish
  a daemon-owned `manifest.json` using a temporary file, file fsync, rename and
  directory fsync. It records the root's format and typed backing chain and each
  captured volume's original `VMVolume.Format` and typed chain. Persistent volumes
  are created and attached as raw unconditionally: valid qcow2-looking guest bytes
  cannot select a driver. A raw disk or raw ancestor is never parsed for a header.
  Sandbox records, saved-image sidecars and hidden node metadata preserve the same
  per-disk information for GC and after a fork's staging directory disappears.
  Image deletion and failed capture remove the disk and sidecar together. OCI
  pull/import publish from private stages with explicit raw metadata, replacing
  stale qcow2 authority even when the raw bytes happen to look like a header.
- **Capture cannot launder rewritten headers.** Snapshot and image capture compare
  every disk's chain with the source VM's recorded chain before copying anything,
  and repeat the check while paused. Captured copies are checked too. A pointer
  must match both its recorded path and its recorded format before the daemon
  opens its target. Capture errors leave the source running.
  Checkpoint holds the shared bases lease from before capture through manifest
  publication, source resume and failure cleanup.
- **Legacy snapshots are compatibility reads, not new authority.** Without a local
  manifest, format inference is allowed only with an owner-authorized walk: shared
  bases, the owner's images, and nodes anchored by that owner's snapshots/records.
  Each target is authorized before it is opened. An outside-owner or arbitrary
  host path is refused, and the inferred snapshot is not given a trusted manifest.
  Old qcow2 headers missing a backing-format extension are normalized into new
  private copies only after authorization; their original disks are unchanged.
  That legacy-only compatibility fallback can copy data and is not the O(1) path.
  A SQLite/API copy cannot expand a local manifest's disk authority.
  For old VM records, a known top-level qcow2 driver is separate from permission
  to infer missing ancestor formats. Only an absent typed chain enables that
  compatibility; nonnil chains remain strict, and a known raw top stays opaque.
- **GC and dependents use the metadata graph.** Raw bytes never name or pin a node.
  Nodes remain while a recorded sandbox, snapshot, image or external hard link
  holds them. Multiple physical store names for one inode are internal aliases,
  not external holders; an unreachable inode's aliases are reclaimed together.
  Publication syncs a private inode pin and daemon pending/format metadata before
  exposing the final store name. Startup/sweep reconciles interrupted publication;
  copied adoption publishes by rename, so hard-link-free filesystems work too.
  Startup reclaims unrecorded sandbox directories only after ruling out surviving
  helpers, and refuses recovery rather than deleting an unrecorded live helper's
  disks. A complete first VM record is durable before launching any new helper.
  Snapshot deletion goes through `engine.SnapshotRemover` and returns 409 while
  live overlays depend on its disks. Destroying those overlays permits deletion
  without breaking siblings or requiring a second dependency table in SQLite.
- **Imports rebuild the graph.** Archive layer formats are explicit, and imported
  header backing paths and host-local backing lists are discarded. Only the
  validated `layer-N.img` graph and a content-identified shared base determine the
  paths written into imported disks and their local manifests. New exports use
  archive v3 with explicit root, volume and layer formats. v1/v2 archives retain
  compatibility when parent formats disambiguate their layers; a standalone
  untyped legacy volume defaults to raw, and ambiguous layers are rejected.
  Missing backing-format extensions are rebuilt from graph metadata. Raw layers
  remain opaque even if their bytes resemble qcow2.
  Engine-owned import publication retains the shared lease across final rename;
  the completed staging manifest is a GC root before that step. Export retains
  the lease from planning through streaming and the final compression flush.
- **Confinement and the remaining admission limit.** Linux node disks use durable,
  owner-scoped synthetic groups, disjoint from helper and netd identity ranges.
  A node is daemon-only before publication; only its owner's group receives read
  access. Private nodes are never opened to the universal helper group, even
  without Landlock. Startup migrates pre-change permissions. Adopted helpers keep
  their already-open chain descriptors; their next start receives the new groups.
  Landlock further limits each helper to exact chain files, excluding snapshot
  directories, `manifest.json` and hidden node metadata. Restore checkpoint/RAM
  files are linked into a separate helper-readable restore directory. Linux
  enforcement is not exercised by the historical macOS HVF runs below.
  `vmmBaseAllowed` still admits this host's
  shared bases and any image of the same owner when authorizing a legacy or new
  image chain; it does not admit another owner's images. To tighten that admission,
  persist an immutable daemon-owned `(device, inode, format, typed ancestors)`
  record for each shared base and owner image, resolve the selected image once,
  and authorize only those exact recorded identities at create/legacy restore.
  Then remove the directory-wide `vmmBaseAllowed` exception instead of allowing
  a rewritten header to choose another otherwise-authorized same-owner image.
- **Pause window.** Checkpoint flushes only the disks it copies while paused and
  resumes before the snapshot's durability sync. `krucible.checkpoint.paused`
  records source pause, SAVE, flush and copy durations. The resume benchmark also
  reports a live fork's source pause on helpers that support memory checkpointing;
  HVF helpers without that capability cannot supply a real fork measurement.

### Upgrade and rollback across v2.5.10

The storage upgrade adds filesystem/JSON authority records, not a storage-table
migration. Existing v2.5.10 VM records remain compatibility reads, including a
known qcow2 root whose header omits the optional backing-format extension.
Captured copies are normalized before receiving new typed metadata; the running
source is not rewritten.

**Replacing only the binary with v2.5.10 is not a transparent rollback after
creating layered state:**

- **Layered sandboxes:** an already-running, adopted helper may keep running, but
  Linux cold starts fail because v2.5.10 does not authorize `/nodes/...`
  ancestors. Stopping a working layered sandbox can therefore make it
  unstartable under the old daemon.
- **Layered snapshots:** the files are not flattened or converted on downgrade.
  Their node-backed root/volume chains have the same Linux cold-start limitation;
  the old daemon has no node-store retention/reclamation protocol. Do not use it
  to manage or clean up newly layered snapshots and their nodes.
- **Archives:** new exports are archive v3; v2.5.10 accepts archive v1 only and
  rejects them. Keeping a new archive is not an old-version restore plan.
- **Re-upgrading does not undo all rollback damage:** old recovery rewrites VM
  records through its older spec and drops `disk_backing`. A fork whose staging
  snapshot is already gone can lose its only owner-provenance anchor, leaving
  store-only ancestors unauthorized even after the new binary is restored.
  Surviving named snapshot/image anchors can preserve that particular provenance,
  but are not a general rollback guarantee.

For a rollback, restore a consistent **pre-upgrade data-directory backup and its
matching binaries**. Preserve nodes, manifests, image sidecars and VM records;
deleting the new metadata to make old readers appear compatible can destroy the
information needed to recover layered disks. Preserve post-upgrade data
separately before restoring an older backup.

### Storage review fixes and verification (2026-11-08)

The release review's ten findings are addressed by:

- `31b0109` — findings 3, 5, 6 and 9: owner-scoped node DAC, recoverable inode
  publication, physical-store alias accounting and rename-only copied adoption.
  These share the publication and inode-accounting protocol.
- `1e8d8d7` — finding 4: safely reclaim interrupted, unrecorded creates before
  dependency traversal; preserve/refuse recovery for a surviving helper.
- `f46042f` — findings 1, 2 and 10: shared leases cover checkpoint publication,
  engine-owned import rename and the entire export stream/final flush.
- `d8c9e48` — finding 7: paired image authority lifecycle, private publisher stages
  and explicit raw metadata on raw replacement.
- `1028faf` — finding 8: v2.5.10-shaped VM records cold-start and capture missing
  backing-format extensions without weakening typed chains.

Each finding's protection was temporarily removed, its deterministic regression
was observed failing, and the protection was restored before the passing suites.
Crash tests exit a subprocess at publication boundaries; concurrency tests use
fake peers, channels and nonblocking flock probes rather than timing sleeps.
The root Linux DAC subprocess uses numeric credentials without Landlock and
proves own-chain access plus `EACCES` for foreign nodes, including an attacker
with no private ancestors of its own.

Observed verification:

- `go build ./... && go vet ./...`: passed.
- `GOOS=linux GOARCH=amd64 go build ./...` and
  `GOOS=linux GOARCH=amd64 go vet ./pkg/engine/krucible/`: passed.
- `go test -race ./pkg/engine/... ./pkg/server/ -count=1`: passed; krucible
  57.802 s, server 88.309 s.
- Root container, `golang:1.26.3-bookworm`,
  `go test -race ./pkg/engine/krucible/ -count=1`: passed, 47.994 s.
- `scripts/test-cgroups-systemd.sh`: passed on systemd 257; both krucible and
  launchgate suites passed. Its no-toolchain/no-KVM and optional qemu-img skips
  remain skips, not exercised VM or qemu comparisons.
- `make vmm netd`: passed and codesigned the HVF helper.
- Full tagged HVF suite with `Image-lean-6.12.94-aarch64-k8s`: **failed** after
  336.859 s, solely in `TestKrucibleNetDNS`'s `TempDir RemoveAll` cleanup:
  `directory not empty`. The remaining directory contained only
  `images/.bases.lock`; no guest DNS assertion failed. An outliving background
  sweep is the inferred teardown cause, not a confirmed new storage regression.
  This teardown failure is not suppressed or reported as green.

No remote execution, push, tag or history rewrite was performed. Real memory
checkpoint/fork VM coverage remains capability-gated on this HVF helper; the
deterministic fake-helper regressions exercise those orchestration paths.

### Storage hardening evidence (2026-11-08)

Host: Darwin/arm64 HVF, Mac16,12, 10 logical CPUs, 24 GiB RAM, Go 1.27.3.
The VM tests used `Image-lean-6.12.94-aarch64-k8s` and the existing libkrucible
install, with `make vmm netd` rebuilding/codesigning the helper. Its reported
capabilities were `{"pause":true,"checkpoint":false}`.

- `go build ./...` and `go vet ./...`: passed.
- `GOOS=linux GOARCH=amd64 go build ./...` and
  `GOOS=linux GOARCH=amd64 go vet ./pkg/engine/krucible/`: passed.
- `go test -race ./pkg/engine/... ./pkg/server/ -count=1`: passed; krucible
  41.196 s, server 81.183 s. The ordinary krucible unit suite passed in 26.004 s.
- Regression tests were demonstrated red by temporarily removing the relevant
  protection, then restored green: raw-volume metadata, raw-file open suppression,
  archive terminal-path removal, authorization before legacy header probes, and
  exact capture checks before and after quiesce. Raw-root format, legacy missing
  extensions and saved-image owner provenance have the same red/green proof.
- The complete tagged HVF suite ran in 287.900 s and was **not fully green**.
  Its raw-volume test initially used shell syntax unsupported by the test rootfs;
  its saved-image fixture used a path outside the owner's image directory. Those
  fixtures were corrected, and both targeted VM tests passed (2.552 s combined).
  The separately recorded `TestKrucibleCredentialSubstitution` failure was
  `no secret.denied event for the request target`; credential code/tests were
  deliberately left unchanged.
- Actual VM storage gates passed: 160 MiB-delta snapshot resumes, independent
  clone writes, deletion refusal with live clones and success after destruction,
  boot after manual snapshot-directory removal, and generations beyond the depth
  cap. The raw-attach VM regression observes a 96 MiB raw device rather than its
  forged header's 64 MiB device and cannot read the backing filesystem's marker.
  Unit checkpoint/resume/GC tests preserve the forged raw volume byte for byte
  and prove that its forged path creates neither helper grants nor node pins.
- **Not exercised:** memory-snapshot/fork VMs, a real fork's source pause, and a
  memory-restored raw volume, because this HVF helper cannot checkpoint memory.
  The real fork tests remain capability-gated; fake-helper tests exercise fork
  orchestration and the still-serving source. Linux root/Landlock execution needs
  a Linux/KVM host. The optional qemu-img comparisons skipped because qemu-img
  was not installed. No remote execution was used.

`KRUCIBLE_BENCH_RESUME=1` / `TestKrucibleResumeScaling` wrote 2,048 MiB of nonzero
guest data and captured a filesystem snapshot occupying 2,050 MiB. Every batch
verified the full file's SHA-256 through a resumed guest. Per-resume times below
are individual concurrent-call durations, not batch wall time divided by N.
Root allocation is the sum of `st_blocks * 512` for clone disks, reported in
whole KiB and excluding the shared snapshot/base; filesystem delta is the
coarser `statfs` free-space change, not an I/O write counter.

| Parallel resumes | Batch wall (ms) | Per-resume mean / p50 / max (ms) | Clone root allocation, total (KiB) | Filesystem delta (MiB) | Fresh-create batch (ms) |
|---:|---:|---:|---:|---:|---:|
| 1 | 115 | 115 / 115 / 115 | 1,616 | 1 | 102 |
| 5 | 186 | 174 / 174 / 186 | 8,080 | 7 | 161 |
| 10 | 457 | 378 / 402 / 457 | 16,160 | 15 | 303 |
| 20 | 759 | 600 / 627 / 759 | 32,704 | 31 | 633 |

The initial snapshot took 28 ms; a snapshot of the first thin clone took 20 ms
and held only 2 MiB of its own disk. A real fork pause is **not measured** here:
the benchmark explicitly reports the absent memory-checkpoint capability.
These results support delta-independent disk preparation, not constant time as
parallelism grows and not the legacy/depth/filesystem copy fallbacks.

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
- Verification below used Go 1.27.3 and the existing libkrucible install; Docker
  is needed only when rebuilding the lean kernel.
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

### 5.13 macOS/HVF multi-vCPU cold restore  — **FIXED (2026-06-30)**
- **Was:** a 2-vCPU cold restore on macOS/HVF failed with `vCPU restore not
  acknowledged` (`TestKrucibleColdTierMultiVcpu`); 1-vCPU + KVM were fine.
- **Root cause:** on restore all vCPUs start paused, but a secondary vCPU blocked
  on `boot_receiver.recv()` (its PSCI-CPU_ON boot address) *before* reaching the
  paused loop — and on restore there's no PSCI bring-up (it was already online in
  the snapshot) and vCPU 0 is itself parked and never signals. So the secondary
  deadlocked, never acked RestoreState → 5s timeout.
- **Fix (libkrucible `4e82af6`, `vmm/macos/vstate.rs`):** when `start_paused`,
  skip the `boot_receiver` wait and use a placeholder entry (RestoreState
  overwrites PC). HVF-only. `TestKrucibleColdTierMultiVcpu` + full suite green on
  HVF; **cold tier is now full multi-vCPU on all three platforms.**

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
