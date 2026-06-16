# krucible — the guest init model (lohar-as-PID-1 vs init-mediated)

Status: **Design doc (2026-06-16).** Scopes what the "lohar → service" restructure concretely takes, after the cold-tier
work surfaced friction between bhatti's `lohar-as-PID-1` model and libkrun's init-centric design. Companion:
`docs/PLAN-krucible-cold-tier.md` (the cold tier + the block-root-vs-FUSE decision), `docs/PLAN-krucible-v3.md` (P1's
"lohar is the asset" decision), `docs/guest-agent.md`.

> Reference points: the libkrun upstream README (its security model + intended init usage) and the reference fork /
> reference product (their init→real-init→agent-as-service shape). Studied for technique only; named generically here.

---

## DECISION (2026-06-16): we shipped M1′ (kernel-direct block root), and the lohar slim is moot

The cold tier shipped a **variant of M1 — "M1′ kernel-direct block root"** — not the init-mediated M1 described below.
The difference matters for the slimming question:

- **M1 (as planned):** libkrun's bundled **init** (PID 1) mounts `/dev/vda`, pivots, then `execvp`s lohar → lohar could
  shed its *early-boot* mounts/pivot to that init. Requires the **init-blob toolchain** (cross-compiled `init.c`).
- **M1′ (what we shipped):** no init-blob. The kernel cmdline carries `root=/dev/vda rootfstype=ext4 ... init=/init.krun`,
  so the **kernel** mounts the ext4 root and execs lohar directly as PID 1. The bundled kernel has virtio-blk+ext4 built
  in (`nomodule`). lohar keeps **every** PID-1 duty — `/proc`,`/sys`,`devtmpfs`,`devpts`,`tmpfs`×3,`cgroup2`,`binfmt_misc`,
  loopback, signals/reboot, reaping, vsock listeners — because the kernel only mounts the *root*, nothing else.

**Consequence — the envisioned lohar slim does not apply:** there is no libkrun init to hand early boot to, so lohar's
mount/pivot/PID-1 block stays as-is. We chose this deliberately: **avoiding the init-blob cross-compile toolchain is worth
more than the marginal lohar reduction.** The cold tier is green with lohar unchanged.

**The one piece of FC-era code in lohar (`setupNetworking`, the `ip=`/TAP path in `net.go`) is *not* deletable and needs
no krucible change:** the FC engine still injects `ip=...::eth0:off:...` on its kernel cmdline (`create.go`), so it is
load-bearing on Hetzner production; and on krucible there is no `ip=`, so it **self-skips** ("no ip= in cmdline, skipping")
on every boot. The rest of `net.go` is the vsock-listener machinery (the snapshot-resume heartbeat included), shared and
essential on both engines.

**Net: lohar is already appropriately lean for the krucible path.** No slimming action is taken. The real lohar
restructure (shedding PID-1 duties) only becomes relevant under **M2** (agent-as-service + a supervisor PID 1), which is
still gated on the systemd-shim product question (below). The M0/M1/M2 analysis that follows is retained as the rationale.

---

## TL;DR (the reframing)

The framing "demote lohar to a non-PID-1 *service*" **does not fit what lohar is.** lohar is not just an agent — it is
bhatti's **PID 1 + a systemd-compatible service manager + the agent**, fused into one binary:

- **PID-1 init**: mounts (`/proc`,`/sys`,`devtmpfs`,`devpts`,`tmpfs`×3,`cgroup2`,`binfmt_misc`), loopback, hostname/DNS,
  config-drive + volume mounts, signal handlers, **zombie reaping**, syslog, `reboot()` on shutdown.
- **service manager**: a ~100 KB systemd shim (`systemctl.go`, `unit.go`, `tmpfiles.go`, `depgraph.go`, `conditions.go`,
  `notify.go`, `statedirs.go`) that runs Docker/systemd-dependent workloads — bhatti's *replacement* for systemd.
- **agent**: vsock listeners (control :1024, forward :1025) serving Exec/Shell/Files/Sessions/Tunnel.

The other design (libkrun's init → a *real* init → a thin agent-as-service) works *because the agent is thin and a real
distro init does service management.* bhatti deliberately went the other way: **lohar *is* the service manager**, so it is
inherently PID-1-shaped. There's even a hard `os.Getpid() != 1` refuse-guard (two Pi5s were powered off in this project's
history — the W4 story). So:

- **The realistic, bounded restructure is B1: keep lohar as PID 1, but let libkrun's init do the *early* boot** (mount the
  block root + pivot + base mounts), then `exec` lohar. This unlocks the **block root** (→ cold-tier exec-after-restore,
  via the block persist we already built) and the **stronger guest confinement** libkrun's docs call for — without gutting
  lohar's role.
- **True agent-as-service (B2)** — least-privilege agent, no-panic-on-crash — **requires first answering whether krucible
  sandboxes still need the systemd shim.** It's a real product/architecture decision, separable from this work.

---

## 1. The three boot models

### M0 — today: `disable_implicit_init`, lohar = PID 1, virtio-fs root
Kernel `init=/init.krun`, `/init.krun` = lohar. lohar does *everything* (mounts → service-manager → agent). libkrun's own
init is suppressed. Root is a host dir over virtio-fs.
- **Pain (surfaced by cold tier):** virtio-fs needs FUSE-state persist to survive restore (host-side, fragile, live-host-dir
  dependency); and libkrun's docs flag virtio-fs as a host-escape risk (no path isolation). A block root would be cleaner
  + safer, but a block root won't *boot* under M0 (no init to mount `/dev/vda` + pivot; `rootfstype=virtiofs`, no `root=`).

### M1 (B1) — init-mediated boot, lohar still PID 1, block root
Do **not** `disable_implicit_init`. `krun_set_root_disk(<ext4 image>)` + `krun_set_exec(<lohar>)`. libkrun's init (PID 1)
mounts `/dev/vda`, pivots to it, then `execvp`s lohar — which **becomes PID 1** (exec replaces the init) and continues as
the service-manager + agent.
- **Wins:** block root boots → cold-tier exec-after-restore works (block persist already done); guest confined to the image
  (no host-dir passthrough → satisfies libkrun's isolation guidance); faster fs (guest page cache vs FUSE round-trips);
  self-contained/CoW-overlay snapshots. lohar sheds *early-boot* duty (mount/pivot) but keeps service-manager + agent.
- **Cost:** lohar's remaining mounts must become **idempotent** (libkrun's init already mounted `/proc`,`/sys`,`/dev` and
  moved them across the pivot); the engine must build/maintain an ext4 image + CoW overlay instead of a dir; a small
  re-validation of the boot sequence.

### M2 (B2) — real init/supervisor = PID 1, lohar = service
libkrun init → a minimal supervisor (PID 1: reap + spawn + restart + reboot) → lohar as a **child service**, possibly
non-root, with the agent split from the service-manager.
- **Wins:** least-privilege agent; agent crash ≠ kernel panic; smallest, idiomatic agent; cleanest libkrun fit.
- **Blocker:** *who runs the systemd shim?* It is the service manager and is inherently PID 1. M2 only makes sense if we
  either (a) **drop the systemd shim** on the krucible path (viable *iff* krucible sandboxes are agent-first and don't need
  Docker/systemd-unit workloads — a product call), or (b) keep a PID-1 service-manager and run the *agent* (only) as a
  child — which is a real lohar split (agent ↔ service-manager) and a larger refactor.

---

## 2. B1 concretely — what changes, what stays

This is the recommended near-term path; it's bounded and unlocks the cold tier.

### libkrun / cmd/vmm
- **Stop calling `krun_disable_implicit_init`** on the cold/fork-capable profile. Set `krun_set_root_disk(<image>)` +
  `krun_set_exec(<lohar path on the image>)`. libkrun's init mounts + pivots + execs lohar. (Already wired:
  `spec.RootDisk` → `krun_set_root_disk`; what's missing is *not disabling* the implicit init for this profile.)
- Keep `KRUCIBLE`-themed config; the libkrun init reads exec/workdir/rlimits/env/block-root from the cmdline as usual.

### lohar (the bounded guest changes)
- **Idempotent early boot.** `mustMount(...)` must become "skip if already a mountpoint" (check `/proc/mounts`), because
  libkrun's init already mounted `/proc`,`/sys`,`devtmpfs` and carried them across the pivot. Today these are fatal
  re-mounts. (lohar already has mountpoint-detection logic to reuse.)
- **Relax / re-scope the PID-1 refuse-guard** carefully: lohar is *still* PID 1 under M1 (exec'd by the init), so the
  `getpid()==1` guard still holds — good. No change to the guard's intent.
- **Boot-source detection:** lohar should not assume virtio-fs (`no ip= … skipping` etc. stays); with a block root the
  rootfs is already `/`. Volumes/config-drive paths unchanged (config drive is still a separate disk).
- **What stays untouched:** the entire systemd shim, the agent protocol (Exec/Shell/Files/Sessions/Tunnel), zombie
  reaping, signals, syslog. lohar remains the init + service-manager + agent.

### bhatti engine (`pkg/engine/krucible`)
- **Build a root *image*, not a dir.** `mke2fs -t ext4 -d <rootfs tree> <image>` (the `configdrive.go` tooling already
  does this) producing a base; per-sandbox **`krun_create_disk_overlay`** qcow2 CoW over the base (the FC reflink-`cp`
  replacement, on any FS incl. APFS).
- `Create` attaches the overlay as the root disk; `Stop`/`Start` (cold) snapshot the overlay + memory bundle; the overlay
  is the portable cold artifact (and the fork base later).
- A `warm/dev` profile can keep virtio-fs (host-dir convenience, no image build) for fast iteration; **cold/fork-capable
  sandboxes use the block root.** Per-profile, not a global swap.

### Cold-tier payoff (why B1 finishes P3)
With a block root: the block device persist (already implemented + the worker-reclaim machinery) captures the queue state;
restore re-activates it; the ext4 image (frozen overlay) is self-consistent with the restored guest page cache. **exec-
after-restore works** — closing the one gap left after the validated memory+vCPU+GIC+device cold-wake. No FUSE-persist
needed.

### B1 risk register
| Risk | Mitigation |
|---|---|
| Double-mount / EBUSY when both libkrun-init and lohar mount | Make lohar mounts idempotent (check `/proc/mounts`); test the exact post-pivot mount set |
| PID-1 boot regression (the W4 bricked-Pi class) | Keep the `getpid()==1` guard; gate behind a profile; heavy `LOHAR_TEST=1` + VM smoke before default |
| libkrun init's exec/env contract differs from our config-drive | lohar keeps reading the config drive (separate disk); the libkrun-init cmdline env is parallel, not a replacement |
| Kernel must mount ext4 block root | libkrun's block-boot path is exactly this (init mounts `/dev/vda`); validated by upstream's `chroot_vm` block mode |
| Boot latency (image vs dir) | qcow2 CoW overlay + page cache; measure vs the virtio-fs path (expect parity-or-better for fs-heavy) |

---

## 3. B2 concretely — what it would take (and why it's separable)

B2 is the security/robustness end-state (least-privilege agent, agent-crash-≠-panic), but it is gated on the systemd shim:

1. **Decide the systemd-shim future on krucible.** If krucible sandboxes are agent-first (exec/shell/files, no in-guest
   Docker/systemd units), the ~100 KB shim can be *omitted* from the krucible guest → then a thin supervisor can be PID 1
   and the agent a child. If sandboxes must run Docker/systemd workloads, the shim stays PID-1-shaped and B2 needs a real
   agent↔service-manager split.
2. **Introduce a supervisor as PID 1** (libkrun init execs it): reap orphans, spawn + monitor + restart the agent, handle
   `reboot`/signals, apply the hardened base mounts (`MS_NOSUID|MS_NODEV|MS_NOEXEC`).
3. **Run the agent as a child service**, ideally as a dropped-privilege uid with a capability set (the agent does need
   root for some ops — exec-as-root, mounts, cgroup writes — so this needs a capability audit; some ops may move to the
   supervisor via a tiny privileged IPC, mirroring lohar's existing `systemctl_ipc` split).
4. **Re-home the PID-1 duties** lohar does today (mounts, reaping, reboot) into the supervisor.

This is a genuine guest re-architecture with its own test surface (the agent's ~40-func `LOHAR_TEST=1` suite assumes the
fused model). **Recommendation: do not couple it to the cold tier.** Capture it as its own track, decided by the
systemd-shim question.

---

## 4. Security & performance (why the block root in B1 is worth it) — recap

Grounded in libkrun's own docs + code (see the cold-tier doc for detail):
- **Security:** libkrun states virtio-fs has *no* protection against the guest reaching other host dirs/filesystems
  ("treat guest + VMM as one entity"). A **block root confines the guest to one image** — no host-path traversal/symlink
  class, no host inode/disk exhaustion, smaller VMM attack surface (block queue vs a FUSE protocol parser doing host
  syscalls). Strictly better confinement; M1 gets this even while lohar stays PID 1.
- **Performance:** block root = in-guest ext4 + page cache (fs ops stay in-guest; batched block I/O on miss) vs virtio-fs's
  per-op FUSE round-trip — block is faster for metadata-heavy / repeat-access (compiles, `node_modules`) and on exec-heavy
  cold start; virtio-fs only wins on zero-copy large reads (DAX) + live host-dir sharing.
- **Snapshot correctness:** the block image / CoW overlay is the *self-contained, content-addressable* fs artifact (frozen
  with the snapshot, overlay-able for fork). virtio-fs snapshot depends on the *live host dir being unchanged* + can't
  capture DAX-mapped host pages in the RAM image. Block is fundamentally cleaner for cold/move/fork.

The agent-as-service *robustness/least-privilege* wins (no-panic, dropped privileges) are **B2-only** — M1 keeps lohar PID
1, so those remain open until the systemd-shim question is settled.

---

## 5. Recommendation & sequencing

1. **Now (finish P3): M1.** Init-mediated **block root**, lohar stays PID 1. Bounded lohar change (idempotent mounts) +
   engine image/overlay build. Delivers exec-after-restore on the already-built block persist, plus the security/perf/
   snapshot wins of a block root. Lowest risk to the validated cold-wake and to lohar's contract.
2. **Decision gate (P4): the systemd shim on krucible.** Agent-first only, or Docker/systemd workloads too? This answer
   determines whether **M2** (thin agent-as-service + supervisor, least-privilege) is reachable cheaply or needs an
   agent↔service-manager split.
3. **Then (if M2 chosen): the lohar split** on its own gate, with the agent's test suite reworked for the unfused model.

Net: M1 is the right next move — it converts the friction we hit into a concrete win without a guest re-architecture, and
it keeps the door open to M2 once the product question is answered.

---

## 6. The real question: lohar's *delta* in libkrun (what's better NOT done in lohar)

Much of lohar exists because **Firecracker gave us nothing** — FC boots a kernel and runs `init=`, full stop. So lohar had
to *be* the init, the network setup, the clock-workaround, the service manager, and the agent. libkrun is the opposite: it
ships an init and a device/boot model, and our own VMM work (the warm/cold tier) added the missing pieces. A lot of lohar
is therefore **FC-era workaround that libkrun now subsumes.** The discussion isn't "shrink lohar for its own sake" — it's:
*where libkrun (or the VMM) does a job natively and better, lohar carrying its own version is liability, not value.*

### Why lohar did things its own way (documented) — and what changed
- **No systemd (decisions §3):** determinism + boot speed (367ms vs 708ms) **and snapshot-correctness** — real systemd
  reacts to the `CLOCK_MONOTONIC` jump on resume (arm64 `arch_timer` advances while vCPUs are paused) with timer storms /
  `degraded`/`maintenance` mode (`EXPERIMENT-systemd-predictions.md`). lohar-as-PID-1 has no timers/watchdogs to misbehave.
  **What changed:** the VMM now *freezes the guest clock* across pause (the `CNTVOFF` vtimer-offset adjust on warm resume;
  absolute restore on cold). The clock-jump that broke systemd is **handled natively** — so the snapshot-correctness
  argument for avoiding a real init is materially weaker on krucible than it was on FC.
- **Own mounts / network / reaping:** FC had no init to do them. **What changed:** libkrun's init does idempotent
  (`EBUSY`-tolerant), hardened (`MS_NOSUID|NODEV|NOEXEC`) mounts of `/proc`,`/sys`,`/dev`, the block-root pivot,
  loopback/dummy-net (TSI-aware), zombie reaping + `reboot` (under `KRUN_INIT_PID1`), and virtio-port I/O redirects.
- **Own networking (`net.go`, `ip=`):** FC used TAP + kernel `ip=`. **What changed:** TSI — no `eth0`; lohar's network
  setup already no-ops on krucible. Dead code on this path.

### Responsibility inventory: lohar today → libkrun-native → verdict
| lohar does today | libkrun / VMM native? | Verdict on krucible |
|---|---|---|
| mount `/proc`,`/sys`,`/dev`,`devpts` | **yes** (init, idempotent + hardened) | **shed** — let the init do it |
| block-root mount + pivot | **yes** (init) | **shed** (this is exactly the M1 unlock) |
| loopback up / dummy net | **yes** (init, TSI-aware) | **shed** |
| `ip=` parse / TAP networking | n/a under TSI | **delete** (dead) |
| zombie reaping / `reboot` on exit | **yes** (init, `KRUN_INIT_PID1`) | **shed** (init owns PID-1 duties) |
| stdio / console redirects | **yes** (init virtio-ports) | **shed** |
| clock continuity across pause/restore | **yes** (VMM `CNTVOFF` freeze — our work) | **shed the workaround**; rely on VMM |
| `cgroup2` + `binfmt_misc` mounts | no (init doesn't) | **keep** — but only the workload/docker tier needs them |
| **agent protocol** (exec/shell/files/sessions/tunnel/vsock) | **no** — nothing like it | **KEEP — the irreducible product** |
| **service supervision** (the systemd shim) | **no** | **keep, but decouple** (separable concern; see below) |
| bhatti config drive (token/files/secrets/volumes/DNS) | partial (init reads OCI `KRUN_CONFIG`, different contract) | **keep** (bhatti's contract) |
| syslog | partial | minor — keep |

### Where the value is
Strip the FC-era plumbing and lohar's *unique, defensible* value is two things, of very different natures:
1. **The agent** — exec/shell/files/sessions/tunnel over vsock, with sessions-as-the-model (decisions §4), scrollback,
   detach-survival. This is the bhatti guest contract and the product. libkrun has nothing like it. **This is the core;
   make it thin and sharp.**
2. **Service supervision (the shim)** — a real, hard-won dependency-ordered/condition-gated/cgroup-placing supervisor
   (W9), so `apt install postgres|nginx|docker` works without booting systemd. Its rationale is documented and valid —
   **but it is a different concern from the agent**, and it is only needed by the *workload* tiers, not the agent base.

### The shape this points to
- **init plumbing → libkrun's init** (M1): mounts, pivot, net, reaping, reboot, redirects. Delete lohar's copies.
- **clock workarounds → gone**: the VMM owns clock continuity. (Re-audit lohar/the shim for any monotonic-jump
  defensiveness that's now redundant.)
- **agent → the thin core of lohar**, the workload the init execs (M1) — or a service (M2) once supervision is settled.
- **service supervision → a tier concern, decoupled from the agent**: either keep the shim but as its own unit invoked by
  the workload tiers, **or** — now that the VMM handles the clock jump — reconsider *real systemd for the heavy tiers*
  (the M2 shape bhatti already prototyped at +340ms, whose main objection — snapshot fragility — the VMM just defused).
  The agent base tier needs neither.

Net: libkrun doesn't make lohar *less* valuable — it lets lohar **stop pretending to be an OS** and be the thing only
bhatti can provide (the agent), while the VMM does the OS plumbing it does better. The systemd shim stays justified, but as
a *tier capability*, not a tax every sandbox's PID 1 carries.

---

## 7. Open questions

1. **Does the krucible guest need the systemd shim at all?** (The hinge for M2; likely "no" for pure agent sandboxes,
   "yes" for Docker-in-sandbox.)
2. **Root image lifecycle:** rebuild-on-version vs a stable base + overlay; where the base lives (content-addressed?);
   `/workspace` as a separate persistent data disk so it survives a base re-bump.
3. **Idempotent-mount surface in lohar:** exact post-libkrun-init mount set to skip; any ordering assumptions (cgroup2,
   binfmt_misc) that the libkrun init doesn't establish and lohar must still do.
4. **Config drive vs libkrun-init cmdline env:** keep the config drive as the source of truth (token/files/secrets/DNS),
   treat the libkrun-init env as boot-only.
5. **Warm/dev virtio-fs profile:** keep it for fast iteration, or standardize on block everywhere for one code path?
