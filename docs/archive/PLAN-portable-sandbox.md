# Portable Sandboxes — bhatti owns its VMM (**krucible**, a libkrun fork): live snapshot/fork, host-owned networking, cross-platform; sunset Firecracker

Status: **Draft — the v2 re-platform decision. Not started.** Grounded in source review (2026-06-11) of the cloned repos under `/tmp/bhatti-research/`: upstream `containers/libkrun`, **`smol-machines/libkrun`** (the snapshot-capable fork we seed from), `checkpoint-restore/criu`, `superhq-ai/shuru`, `smol-machines/smolvm`, and libkrun's three README consumers — `containers/crun` (krun handler), `containers/krunkit`, `AsahiLinux/muvm`. The conversation that produced this plan corrected three load-bearing errors from earlier drafts; the corrections and their evidence are in [Pre-flight](#pre-flight--what-the-source-actually-says). **Read that first.**

Target release: `v2.0` (major — bhatti owns its VMM: a libkrun fork, **krucible**, with VM-boundary checkpoint/restore/fork on KVM **and** HVF, host-owned networking, macOS support; **Firecracker removed**).

Touches: **new repo/submodule `krucible/` (the libkrun fork)** · new `pkg/engine/krucible/` (cgo or purego bindings + control-socket client) · **delete `pkg/engine/firecracker/`** (Track F) · `pkg/engine/engine.go` (keep snapshot/pause/resume/fork; re-cut to krucible's control surface) · `pkg/engine/guestfs/` (extract VMM-agnostic rootfs/config-drive builders) · `cmd/bhatti/vmm.go` (the `start_enter` helper + control socket, jailed) · `cmd/bhatti/engine_{linux,darwin}.go` · `pkg/proxy/` (egress + MITM secret injection) · `pkg/bundle/` (portable checkpoint bundle) · `cmd/lohar/` (`guest-set-time` vsock cmd; otherwise unchanged) · `pkg/store/` (arch, feature_hash, bundle_ref, tokens; drop FC + subnet columns) · `pkg/server/` (capability tokens, resume ladder, recovery) · `sdk/python` + `sdk/ts` + `skills/bhatti` · `kernel/` (arm64 RAW `Image` + virtio + CHECKPOINT_RESTORE).

> **Filing note.** This belongs in `docs/internal/` next to `PLAN-bhatti-v2.md`, not `docs/archive/` (which is for shipped/superseded designs). Left here to avoid surprising the author; move before work starts.

---

## First Principles — the pinpointed identity

**Cross-compatible, portable sandboxes you own.** Every decision serves it:

1. **Cross-compatible** — same abstraction/SDK/CLI/API on macOS (HVF) and Linux/Pi/Graviton (KVM). Dev on your Mac, run on your box, identical behavior.
2. **Portable** — a sandbox is a thing you can carry: **checkpoint, fork, and resume the *live* VM** (same process, same memory, same open files) across your own same-arch machines. This is delivered by **owning the VMM**, not by CRIU and not by disk-only bundles.
3. **Runs where the agent runs → fast** — co-located with the agent: no network hop, local tool calls; plus a real warm tier (pause/resume) restored by owning the VMM.
4. **Secure by default** — host owns the network boundary; agent assumed hostile. Zero durable secrets in the guest, deny-by-default egress enforced *in the VMM*, capability-scoped per-agent tokens, and a jailed VMM process (Track J).
5. **One VMM, one control model, one snapshot mechanism, one network model** — and **we own it** (krucible).

The guest contract — lohar as PID 1 + ext4 rootfs + config drive + `ip=` cmdline + the agent frame protocol over vsock — is the **asset** and is preserved verbatim. libkrun boots an external kernel with `krun_disable_implicit_init()`, so lohar stays PID 1 unchanged.

---

## The decision, in one paragraph

bhatti **forks libkrun into krucible** and owns it. We do **not** use stock libkrun (it has no runtime control surface and no snapshot, which would cost us the warm tier and live fork — the core of the scale-to-zero strategy). We do **not** write a VMM from scratch (libkrun already gives KVM+HVF+virtio+boot). We seed krucible by **porting `smol-machines/libkrun`'s snapshot/fork/HVF/control-socket/egress primitives onto *current* upstream libkrun** — because (a) snapshot is permanently off upstream's roadmap (verified: zero snapshot/checkpoint/pause/resume issues or PRs in libkrun's entire history; they're building Windows/WHP, GPU, init-in-Rust, virtio-fs — never VM-state lifecycle), so we own a fork *regardless*; (b) smolmachine already wrote the hardest part (HVF vCPU register save/restore), Apache-2.0; and (c) basing on *current* upstream (not smolmachine's Feb-2026-frozen tree) keeps the init/virtio-fs/Windows improvements and one-hop tracking. The result restores everything FC gave us (snapshot, pause/resume) **plus** cross-platform, live fork, and a host-owned network/secret posture FC never had — at the cost of owning a ~3–5k-line VMM delta and a jailer-lite we build around the VMM process.

---

## Pre-flight — what the source actually says

These findings are the foundation; each is grounded in a cloned repo.

### F1 — Stock upstream libkrun has no control surface and no snapshot. krucible must add them.
Upstream's public ABI is **66 functions**; the only post-`krun_start_enter` handle is `krun_get_shutdown_eventfd` (`include/libkrun.h:1035`). No pause/resume/snapshot/balloon-command. `krun_start_enter` "never returns" (blocks the caller — `examples/external_kernel.c`). So stock libkrun cannot pause, resume, snapshot, or fork a VM at runtime. (The balloon is always attached with *free-page reporting* — `src/vmm/src/builder.rs:973`, features `VIRTIO_BALLOON_F_FREE_PAGE_HINT|_REPORTING` — so passive guest-driven RAM reclaim works, but there is no host-commanded inflate.)

### F2 — `smol-machines/libkrun` already implements the whole capability set, on **both** backends (Apache-2.0).
The fork (created 2026-02-15, ~3.5 months, ~1–2 authors — `BinSquare`/`hoshinolina`/"BinBin He"; the rest of the contributor list is inherited upstream) adds, over upstream:
- **`krun_set_control_socket(ctx, path)`** — a Unix socket the VMM serves *after build, before the event loop*; newline commands **`PAUSE`/`RESUME`/`STATUS`** (public) plus internal **`CHECKPOINT`/`RESTORE`** handlers (`src/libkrun/src/lib.rs:1007` decl, `:483` `handle_checkpoint`).
- **VM checkpoint/restore on KVM** (`linux/vstate.rs:859/902` `Vm::save_state/restore_state`, `:1334/1398` `Vcpu` save/restore, `VcpuEvent::SaveState/RestoreState`) **and on HVF/macOS** (`macos/vstate.rs:126` `Vm::save_state`, `:624` `VcpuEvent::SaveState→hvf::vcpu_save_state`; `hvf/lib.rs:521` `vcpu_save_state` reading X0–X30 + sysregs via `hv_vcpu_get_reg`/`hv_vcpu_get_sys_reg`, `:617` restore) **plus device-state snapshot** (`device_manager/hvf/mmio.rs` → `virtio::persist::{snapshot_device,restore_device}`). **The HVF register-level snapshot is the genuinely novel piece — and it's open.**
- **`krun_create_disk_overlay(overlay, base, fmt)`** — qcow2 CoW overlay (the disk layer of fork/branch).
- **Live fork** — `Vmm::checkpoint_cow` (a `MAP_PRIVATE` CoW clone of guest RAM with the parent frozen) + a `snapshot_dir` (checkpoint + manifest) cold-boot path for fork clones (`lib.rs:172`, `:441`).
- **`krun_set_egress_policy(ctx, cidrs[], egress_hosts[], dns_resolvers[])`** — deny-by-default egress *in the VMM*: CIDR allow rules, DNS-hostname interception (A/AAAA answers learned as temporary allow entries), and forced-trusted-resolver forwarding (`include/libkrun.h:974`).

Net divergence concentrated in 4 files: `src/libkrun/src/lib.rs` **+1,501**, `macos/vstate.rs` **+578**, `hvf/lib.rs` **+551**, `include/libkrun.h` **+84**, plus virtio `persist` plumbing. **~3–5k lines of focused Rust** on top of an existing VMM.

### F3 — Upstream libkrun's roadmap is breadth, never VM-state. So we own a fork either way.
Last 20 commits + 25 open PRs: **Windows/WHP** (3rd backend), a **native-Rust-API + autogenerated-C-API** rework, **init-binary-in-Rust** + `krun_set_init_binary` + init-blob gating (good for lohar PID-1), **virtio-fs macOS** hardening, **TSI** ICMP/UDP, GPU/dmabuf, TEE/TDX. Snapshot/checkpoint/pause/resume/migrate search → **`total_count: None`**, never in history. It's against their fire-and-forget design. ⇒ our snapshot patches are unupstreamable; krucible is a permanent fork no matter what, so the only question is *sourcing* (answer: port F2's code onto current upstream).

### F4 — libkrun *is* Firecracker-derived (so porting FC's arm64 snapshot / trusting the lineage is sound).
`linux/vstate.rs` header: `Copyright 2018 Amazon.com` + `Portions Copyright 2017 The Chromium OS Authors`; **83 files** under `src/` carry Amazon copyright; libkrun README: *"incorporates code from Firecracker, rust-vmm and Cloud-Hypervisor."* Not a maintained downstream fork — a divergent derivative (HVF backend added; API server/jailer/snapshot-serialization stripped). The shared FC+rust-vmm base is *why* smolmachine's and our snapshot work is tractable.

### F5 — Pickable patterns from the three consumers.
- **crun (`src/libcrun/handlers/krun.c`):** `dlopen`s libkrun and `dlsym`s every symbol — graceful degradation when libkrun is absent. Also the `.krun_config.json`/OCI-annotation resource model and the **TEE/confidential** path (`krun_set_tee_config_file`).
- **krunkit (`status.rs`/`timesync.rs`/`context.rs`):** a **control socket over a blocked `start_enter`** (thread spawned before `start_enter`, holds the shutdown eventfd, serves state/stop) — the exact shape krucible's control socket extends; **`timesync`** pushes host time into the guest over vsock on wake (`guest-set-time`) — the fix for post-resume clock jump; pidfile-written-last supervision.
- **muvm (`bin/muvm.rs` + `krun-sys`):** the minimal binding sequence via the maintained **`krun-sys`** Rust crate; confirms `krun_add_vsock_port2(..., listen=true)` for host→guest and plain `krun_add_vsock_port` for guest→host; `krun_set_passt_fd` + publish-ports integration.

### F6 — concrete integration facts.
arm64 kernel must be **RAW `Image`**, not the ELF `vmlinux` bhatti ships for FC (`examples/external_kernel.c:31-35`; `krun_set_kernel(path, format, initrd, cmdline)` takes format). vsock host-dials-guest = `krun_add_vsock_port2(listen=true)`, host dials the UDS directly (FC's `CONNECT %d\n` handshake in `pkg/agent/client.go:dialVsockPort` is FC-only — use the plain-UDS branch at `client.go:80`/`:102`). TSI is the default net backend (host owns connections by construction). macOS entitlement is `com.apple.security.hypervisor` (`hvf-entitlements.plist`).

---

## How today's Firecracker features move to krucible (scope map)

bhatti's value lives mostly *above* the VMM. Four buckets:

**Bucket A — ports unchanged (guest contract + agent surface).** lohar PID 1, ext4 rootfs, config drive, `ip=` parsing (`cmd/lohar/net.go`); the entire agent protocol — `Exec`/`Shell`/PTY/`File*`/`StreamExec`/`ExecDetached`/sessions+scrollback+reattach/`ListeningPorts`/`Tunnel` (lohar + `pkg/agent/client.go`); systemd shim (`cmd/lohar/systemctl.go`); tiers→profiles; volumes-as-mounts; wake-then-serve proxy (`pkg/server/public_proxy.go`). **One transport line changes** (vsock UDS direct-dial vs FC CONNECT).

**Bucket B — re-backed onto krucible's control surface (same feature, better):**
| FC feature | FC mechanism | krucible mechanism |
|---|---|---|
| Thermal Pause/Resume/EnsureHot | FC REST `/actions` | control socket `PAUSE`/`RESUME` — **now on macOS too** |
| Native snapshot/restore | FC full-VM snap (host-locked) | `CHECKPOINT`/`RESTORE` — **portable across machines** |
| *(none — FC has no fork)* | — | `FORK` (CoW live fan-out) — **new** |
| Tunnel / publish | vsock over FC | `krun_add_vsock_port2` pairing |
| Volumes | FC `/drives` | `krun_add_disk` + `krun_create_disk_overlay` (CoW) |

**Bucket C — deleted, replaced by something structurally better:**
| FC subsystem | Why it dies | Replacement |
|---|---|---|
| `network.go`: TAP+bridge+iptables NAT + IP pool + per-user DNS responder | macOS-impossible; **open guest egress** | **TSI/passt**, host owns every packet; **egress allowlist in the VMM** (`krun_set_egress_policy`) |
| jailer wiring | libkrun has none | `krun_setuid/setgid/set_rlimits` + a cgroup/namespace/seccomp wrapper on the helper (**Track J**) |
| FC rate limiters | FC-API-specific | cgroup v2 io/net limits on the helper |
| config-drive secret injection | secret enters guest | **zero-secret MITM** (Track C2) |
| subnet_index / per-user bridges (store) | no bridges | dropped from schema |

**Bucket D — host engine rewrite.** `pkg/engine/firecracker/` (~13k LOC) → `pkg/engine/krucible/` driving krucible via the bindings + control socket; the jailed `vmm` helper (because `start_enter` blocks); recovery keyed on helper-pid + bundle. **Net: likely *less* host code than today** — krucible absorbs networking + snapshot that bhatti currently hand-rolls.

---

## Storage & CoW — qcow2 overlays replace the btrfs/reflink dependency

Today bhatti's CoW for create-from-image/snapshot is `cp --reflink=auto --sparse=always` (`pkg/engine/firecracker/fc.go:236`), which needs a CoW-capable **filesystem** (btrfs/XFS); on ext4 it silently degrades to a full copy, and macOS APFS doesn't honor it. The quickstart, the README benchmark, and `docs/archive/MIGRATION-v0.5.14-btrfs.md` all bake in btrfs. krucible moves CoW to the **disk-image-format layer** via `krun_create_disk_overlay(overlay, base, KRUN_DISK_FORMAT_QCOW2)`: each sandbox gets a thin qcow2 overlay over a shared read-only base image; the overlay stores only deltas, reads fall through to the base.

Consequences:
- **No filesystem dependency.** Works uniformly on ext4 / XFS / btrfs and **APFS on macOS** — removes the btrfs requirement that blocks the cross-platform identity. CoW becomes a property of the image format, not the host FS.
- **Inherently CoW.** Create-from-image/snapshot is instant on *any* FS (no base copy); one RO base image is shared by many sandboxes' overlays (format-level dedup — better than today's per-sandbox reflink copies).
- **Composes with snapshot/fork.** A checkpoint bundle = vmstate + the qcow2 overlay delta; `fork` = a new overlay on the same base/checkpoint. CoW end to end (G2.2/G2.4).
- Volumes follow the same model (RO base or fresh overlay); the store gains overlay/`bundle_ref` paths; docs drop the btrfs assumption.

Tradeoff (named): qcow2's L1/L2 metadata indirection adds overhead vs raw images on a reflink FS for heavy random I/O (e.g., a database tier) — negligible for typical agent/dev workloads. **Deferred option:** keep a fast path of raw image + `cp --reflink` on btrfs/XFS where present, with qcow2 overlays as the portable default — but standardize on qcow2 overlays first and add the raw+reflink path only if a workload proves it's needed (avoids two code paths up front).

## What owning krucible unlocks (the expansion vision)

Owning the VMM moves it from a *dependency we configure* to the *bottom of our own stack we differentiate on*:

- **Cross-platform identity** — one engine on Linux/Pi/Graviton (KVM) + macOS (HVF), and a credible path to **Windows (WHP)** as upstream lands it. FC can't leave Linux.
- **Live handoff / migration** — checkpoint → ship → restore the live VM (the "close your laptop, resume on another host" capability). A product, not a feature.
- **Live fork / fan-out** — branch a warm agent into N CoW copies for speculative/tree-search execution. FC has no fork.
- **Real warm tier restored** — pause/resume + checkpoint give back the µs/ms-wake economics the scale-to-zero strategy needs.
- **Egress + zero-secret as VMM primitives** — not bolted-on host iptables; a stronger agentic posture than FC's open-egress NIC.
- **GPU/venus/dmabuf** (krunkit's path) → `computer`/`browser` tiers **on macOS**.
- **Confidential computing** (SEV/SNP/TDX, crun's `krun_set_tee_config_file`) → attestable sandboxes for regulated buyers.
- **Nested virtualization** (`krun_set_nested_virt`) → sandboxes that run VMs/k8s (the karkhana/k3s direction).
- **Shape the device set + control protocol to bhatti's exact needs** — e.g., `guest-set-time` resync, a snapshot manifest tuned to lohar.

---

## Security model — is krucible as secure as Firecracker?

Two-sided, honest. **Same hardware boundary; weaker defense-in-depth out of the box; stronger on bhatti's actual threat.**

- **Same primary boundary.** Both isolate the guest with hardware virtualization via **KVM** (VT-x/EPT, AMD-V, ARM EL2 stage-2) on Linux, HVF on macOS. A guest "break out" requires a **KVM/HVF 0-day or a memory-safety bug in virtio device emulation** — and both FC and krucible write devices in **Rust**, removing the bug class that caused historic C-VMM escapes. So a guest cannot trivially escape krucible any more than Firecracker.
- **Where FC is genuinely stronger (and what Track J rebuilds):**
  1. **The jailer.** FC wraps the VMM in seccomp-bpf + chroot + cgroups + userns, so a guest that escapes *through* a virtio bug into the VMM process lands in a jail, not on the host. libkrun/krucible has **no jailer** — a compromised VMM process = host access. **Track J is the non-negotiable answer:** run the `vmm` helper unprivileged (`krun_setuid`/`setgid`), seccomp-filter it, and confine it in a cgroup + mount/user namespace.
  2. **Maturity + a security team + CVE process** — FC is AWS-hardened at Lambda/Fargate scale; krucible's delta is audited only by us.
  3. **Larger host-facing surface** — TSI proxies guest socket syscalls to host-side code parsing guest input; virtio-fs sharing host dirs is a by-design path to host files. Mitigate: default to **no host mounts / RO+overlay**, enforce egress in the VMM.
- **Where krucible is *better* for bhatti's real threat (hostile agent reaching network/credentials):** FC gave the guest a **real NIC with open egress** via host NAT; krucible **host-mediates every connection** (TSI) and **filters egress in the VMM**, and Track C2 means the **real secret never enters the guest**. That guards the boundary the agent actually attacks.

**Verdict:** hardware isolation is a wash; FC's edge is jailer + maturity (Track J + careful defaults close most of it); in exchange we get cross-platform + live snapshot/fork + a better network/secret posture. The jailer-lite is the homework that makes accepting hostile multi-tenant guests defensible (DoD gate).

---

## Tracks

- **Track 0 — krucible (the fork).** Stand up bhatti's libkrun fork: current-upstream base + ported snapshot/fork/HVF/control-socket/egress primitives; CI builds `libkrucible.so`/`.dylib` for arm64 (KVM + HVF) and x86_64 (KVM). Keystone.
- **Track A — the krucible engine.** cgo/purego bindings + control-socket client; the jailed `vmm` helper; `engine.Engine` (incl. snapshot/pause/resume/fork) on Linux, then macOS.
- **Track C — host-owned networking + agentic security.** C1: TSI/egress-policy (in-VMM allowlist). C2: TLS-MITM zero-secret injection.
- **Track D — capability tokens / per-agent identity.**
- **Track E — cross-compatible host + DX** (build split, macOS, SDKs, skill).
- **Track J — jail the VMM** (unprivileged + seccomp + cgroup/namespace).
- **Track F — sunset Firecracker.**

```
0 (krucible fork) ──► A (engine + snapshot/fork) ──► C1 (egress) ──► C2 (MITM)
       │                  │     │     │
       │                  │     │     └► D (tokens)
       │                  │     └► J (jail VMM)
       │                  └► E (macOS + SDKs + skill)
       └──────────────────────────────────────────► F (delete FC, last)
```

Order: **0 → A(Linux, incl. checkpoint/fork) → J → C1 → E(macOS) → D → C2 → F.** CRIU is **out** (krucible's VM-boundary snapshot supersedes it).

---

## Implementation — Goals & Chunks

Each chunk: **Problem / Change / Files / Tests / Done-when.** Signatures are grounded in the cloned headers.

### G0 — krucible: bhatti's libkrun fork (keystone)

**Goal:** a buildable libkrun fork with checkpoint/restore/fork + control socket + egress policy on KVM and HVF, on a current-upstream base.

- **G0.1 — establish the fork + build.** Fork current `containers/libkrun` as `krucible`; add bhatti CI to build `libkrucible.so` (linux arm64+x86_64, KVM) and `libkrucible.dylib` (darwin arm64, HVF) + `libkrunfw`. **ABI-compatible with libkrun** — the C symbols stay `krun_*` and we don't rename them (an ABI-compat fork, not a 66-function rewrite); ship a `krucible.h` = `libkrun.h` + the fork additions; use a **distinct soname** (`libkrucible`) so it coexists with a system libkrun. Vendor as a git submodule pinned to a SHA. **Tests:** CI builds all targets; `examples/external_kernel` boots a busybox initramfs on each. **Done:** reproducible artifacts in CI.
- **G0.2 — port the snapshot/control/egress primitives.** Cherry-pick/rebase `smol-machines/libkrun`'s deltas onto the G0.1 base: `krun_set_control_socket` (PAUSE/RESUME/STATUS/CHECKPOINT/RESTORE), KVM + **HVF** `save_state`/`restore_state` + device snapshot, `Vmm::checkpoint`/`checkpoint_cow` + `snapshot_dir` fork-boot, `krun_create_disk_overlay`, `krun_set_egress_policy`. Expect conflicts in `macos/vstate.rs`/`hvf/lib.rs`/init (upstream churns these). **Tests (in the fork):** `checkpoint→restore` resumes a counter mid-state on KVM and HVF; `fork` yields two live VMs from one; control socket round-trips PAUSE/RESUME. **Done:** `cargo test` green on both backends; a manual checkpoint/restore across two machines works.
- **G0.3 — arch coverage decision.** Determine x86 checkpoint support (smolmachine is likely arm64-first like Machinen). Set policy: **arm64 = full snapshot/fork; x86 = cold-boot + disk overlay only** (documented), or invest in x86 later. **Tests:** `TestCheckpointArm64`, `TestX86RefusesCheckpointWithClearError`. **Done:** capability matrix per arch is explicit.
- **G0.4 — track-upstream discipline.** A `krucible/REBASE.md` + a CI job that periodically rebases our delta onto upstream `main` and runs G0.2 tests; watch upstream's "native Rust API + autogenerated C API" PR (it churns our binding surface). **Done:** a documented, tested rebase loop.

### G1 — Boot lohar under krucible, agent answers (Linux)

- **G1.0 — kernel.** Add a krucible kernel target in `kernel/`: arm64 **RAW `Image`** + x86 ELF, with `CONFIG_VIRTIO_BALLOON/FS/VSOCKETS` built in. **Tests:** `kernel/verify.sh` asserts format + CONFIGs. **Done:** boots under `external_kernel` both arches.
- **G1.1 — bindings (`pkg/engine/krucible/krun.go`).** Bind the ~21 calls we use. **Decision (from crun, F5):** prefer **`dlopen`** (via a small C shim or `purego`) over hard `#cgo -lkrucible`, so the daemon/CLI runs on hosts without krucible and fails only at `Create` — graceful degradation + easier cross-compile.
```go
type Ctx struct{ id uint32 }
func CreateCtx() (*Ctx, error)
func (c *Ctx) SetVMConfig(vcpus uint8, ramMiB uint32) error
func (c *Ctx) SetKernel(path string, format uint32, initrd, cmdline string) error // RAW arm64 / ELF x86 (F6)
func (c *Ctx) AddDisk(blockID, path string, readonly bool) error
func (c *Ctx) CreateDiskOverlay(overlay, base string, baseFmt uint32) error        // krucible (F2)
func (c *Ctx) DisableImplicitInit() error                                          // lohar is PID 1
func (c *Ctx) AddVsockPortListen(guestPort uint32, hostUDS string) error           // add_vsock_port2 listen=true (F6)
func (c *Ctx) SetControlSocket(path string) error                                  // krucible: PAUSE/RESUME/CHECKPOINT (F2)
func (c *Ctx) SetEgressPolicy(cidrs, hosts, resolvers []string) error              // krucible (F2)
func (c *Ctx) SetUID(uint32) error; func (c *Ctx) SetGID(uint32) error             // Track J
func (c *Ctx) SetRlimits([]string) error
func (c *Ctx) SetConsoleOutput(path string) error
func (c *Ctx) ShutdownEventFD() (int, error)
func (c *Ctx) StartEnter() error                                                   // becomes the VM; blocks
func (c *Ctx) Free() error
```
**Tests:** `TestCtxCreateConfigFree` (links/dlopens, error mapping, no boot; `t.Skip` if krucible absent). **Done:** `go test ./pkg/engine/krucible/` passes in CI (krucible installed).
- **G1.2 — the jailed `vmm` helper (`cmd/bhatti/vmm.go`).** `start_enter` blocks, so a per-VM helper becomes the VM (the `lohar spawn` pattern, `cmd/lohar/main.go:43`). Adopt **krunkit's control-socket shape (F5):** before `StartEnter`, the helper calls `SetControlSocket(<uds>)`; the daemon drives PAUSE/RESUME/CHECKPOINT/RESTORE/STATUS over it and holds the shutdown eventfd for stop. The helper sets uid/gid/rlimits + enters a cgroup/namespace (Track J) before `StartEnter`. **Files:** `cmd/bhatti/vmm.go`, `pkg/engine/krucible/spec.go` (`VMSpec`), `pkg/engine/krucible/control.go` (control-socket client). **Tests:** `TestVMMHelperBootsBusybox`, `TestControlSocketPauseResume`. **Done:** minimal rootfs boots, pauses/resumes via the socket, exits cleanly.
- **G1.3 — engine skeleton (`pkg/engine/krucible/engine.go`).** Extract VMM-agnostic builders into **`pkg/engine/guestfs/`** (`BuildConfigDrive` from `firecracker/configdrive.go`; `PrepareRootfs`/`InjectLohar` from `create.go` — already FC-decoupled). `Create` preps rootfs+config-drive (+ `CreateDiskOverlay`), spawns the helper with a `VMSpec`, waits agent-ready, records `VM{helperPID, controlUDS, vsockUDS, rootfs, bundleRef}`. `Destroy` writes shutdown eventfd, reaps, cleans. **Files:** `pkg/engine/guestfs/*`, `pkg/engine/krucible/engine.go`, `cmd/bhatti/engine_linux.go` (`case "krucible":` at `main.go:101`). **Tests:** `TestCreateDestroyKrucible`, `TestListReflectsCreateDestroy`. **Done:** a bhatti rootfs boots to agent-ready under krucible and tears down.
- **G1.4 — agent over krucible vsock.** lohar listens on guest `:1024/:1025`; `AddVsockPortListen` bridges to host UDS; host dials directly (no CONNECT). **Files:** `pkg/agent/client.go` `NewKrucibleClient(controlUDS, forwardUDS)` reusing the plain-UDS branch (`:80`/`:102`) + token; `pkg/engine/krucible/engine.go` implements the optional interfaces (`FileEngine`, `StreamExecEngine`, `ShellSessioner`, `SessionAttacher`, `PipedSessionEngine`, `DetachedExecEngine`) by delegation. **Tests:** the FC agent integration suite, extracted into a shared `enginetest` harness, re-run against krucible. **Done:** full agent surface passes.
- **G1.5 — recovery.** Persist `VM{helperPID, controlUDS, rootfs, bundleRef}` in `engine_meta_json` (`pkg/store/sandbox.go:17`); on boot re-adopt live helpers (pid + cmdline guard) else mark stopped. **Files:** `pkg/engine/krucible/recover.go`; `cmd/bhatti/main.go:147` recovery call. **Tests:** `TestRecoverAdoptsLiveHelper`, `TestRecoverRejectsReusedPID`. **Done:** restart preserves running sandboxes.

### G2 — Snapshot / restore / fork (the warm + portable tier)

**Goal:** pause/resume, checkpoint→bundle→restore (live), and fork — via krucible's control socket. **Depends on:** G0, G1.

- **G2.1 — pause/resume (thermal).** Re-back the thermal manager's Pause/Resume/EnsureHot on the control socket (`PAUSE`/`RESUME`); `ensureHot`→`ensureResumed`. Resume ladder: `hot` / `hot-but-shrunk` (passive FPR + `memory.high` squeeze on the helper cgroup) / `paused` / `cold`. **Files:** `pkg/engine/krucible/thermal.go`, `pkg/server/*` (re-point `ensureHot`). **Tests:** `TestPauseResumeRoundtrip`, `TestEnsureResumedFromPaused`.
- **G2.2 — checkpoint → bundle.** `Snapshot(id)`: `agent.Exec("sync")` → control-socket `CHECKPOINT` → write `.bhatti` bundle = `disk-overlay.qcow2` + `criu/`-free `vmstate` images + `manifest.json{arch, feature_hash, base, env, ports, proto_ver}`. **Files:** `pkg/bundle/`, `pkg/engine/krucible/snapshot.go`. **Tests:** `TestCheckpointBundleRoundTrip`, `TestBundleRejectsTampered`.
- **G2.3 — restore (live).** `ResumeFromBundle`: boot a krucible VM in restore mode from the bundle (`snapshot_dir`), re-declare egress/ports, reattach agent; re-back the existing `Checkpoint`/`ResumeFromManifestJSON` API (`pkg/server/admin_handlers.go:529-546`). **Push host time via the lohar `guest-set-time` vsock command (F5/krunkit)** so the resumed guest clock is correct. **Tests:** `TestRestoreResumesLiveProcessSameArch`, `TestRestoreAcrossMachines` (scp the bundle).
- **G2.4 — fork / fan-out.** `Fork(id)` (control-socket `FORK`, `checkpoint_cow`) → bundle or in-process clone; `RestoreCopies(bundle, n)`; CLI `bhatti fork` / `copy --count N`. **Tests:** `TestForkLeavesSourceRunning`, `TestForkFanoutNCopies`.
- **G2.5 — arch guard.** Stamp/check `arch`+`feature_hash`; refuse cross-arch precisely. **Tests:** `TestCrossArchRestoreRefused`.
**Done:** `snapshot → scp → restore` resumes the *live* process same-arch (clock correct); `fork` fans out; pause/resume drives the thermal tier.

### G3 — Jail the VMM (Track J) — security gate

**Goal:** a compromised `vmm` helper cannot reach the host. **Depends on:** G1.
- **G3.1 unprivileged** — `SetUID/SetGID` to a per-VM uid; ensure krucible works post-drop. Tests: `TestVMMRunsUnprivileged`.
- **G3.2 seccomp** — a seccomp-bpf allowlist around the helper (model on FC's profile). Tests: `TestSeccompBlocksUnexpectedSyscalls`.
- **G3.3 cgroup + namespace** — confine the helper in a cgroup (cpu/mem/io/pids) + mount/user/net namespace; minimize virtio-fs exposure (default none/RO+overlay). Tests: `TestHelperConfinedToCgroupAndNS`.
**Done:** the helper runs unprivileged, seccomped, namespaced; documented as the precondition for hostile multi-tenant.

### G4 — Host-owned networking (Track C)

**Depends on:** G1 (egress), G2 (re-declare on restore).
- **C1.1 egress allowlist (in-VMM)** — `SetEgressPolicy(cidrs, hosts, resolvers)`; deny-by-default, DNS host-learned IPs, forced resolvers. `--allow-net`/`--allow-host` map onto it. **Tests:** `TestAllowlistPermitsListedBlocksOthers`, `TestNoDNSExfil`.
- **C1.2 publish/tunnel** — re-back `Tunnel()`/`public_proxy` on vsock port pairing. **Tests:** `TestPublishServesGuestHTTP`, `TestPublicProxyWakeThenServe`.
- **C2 zero-secret MITM** — host CA installed in guest trust (`guestfs`); TLS-terminate with minted leaf, upstream with a browser-shaped fingerprint; substitute placeholder→real secret only on HTTPS to the allowed host. Spec `NAME=<secretRef>@host1,host2` (shuru-identical). Retire config-drive secret injection. **Files:** `pkg/proxy/{tls,proxy,inject}.go`. **Tests:** `TestSecretSubstitutedOnAllowedHost`, `TestPlaceholderLeaksHarmlesslyElsewhere`.

### G5 — Capability tokens (Track D)

`tokens` table `{id, sandbox_id, caps[], expires_at, revoked}`; mint on Create (TTL+caps), middleware per route (`exec`/`files:*`/`publish`/`net:egress:<list>`/`snapshot`/`fork`), revoke on Destroy; egress attribution + per-token rate limit; per-agent audit to `events`. **Files:** `pkg/store/token.go`, `pkg/server/auth_token.go`, `pkg/server/event_recorder.go`. **Tests:** `TestTokenScopedToSandbox`, `TestExpiredTokenRejected`, `TestCapEnforcedPerRoute`, `TestEgressAttributedAndLimited`.

### G6 — Cross-compatible host + DX (Track E)

- **macOS engine** — same bindings, HVF; codesign `bhatti` + `vmm` with `com.apple.security.hypervisor`; **port krunkit's `timesync`** (host-sleep → `guest-set-time`). Tests: macOS CI Create→Exec→Checkpoint→Restore→Destroy.
- **build split** — `pkg/{server,store,agent,bundle,proxy}` + most of `cmd/bhatti` build on darwin; engine behind `engine_{linux,darwin}.go`; `GOOS=darwin go build ./...` in CI.
- **SDKs** — `sdk/python` + `sdk/ts` (`Sandbox.create/run/exec/files/publish/snapshot/fork`), generated from an OpenAPI emitter; shape-match shuru's `@superhq/shuru`.
- **skill** — `skills/bhatti` (agentskills.io, mirror shuru's) + one-paste onboarding; `brew`/`curl|sh`.

### G7 — Sunset Firecracker (Track F)

- **flip default** — `pkg/config.go:194` `Engine: "krucible"`; FC behind hidden `--engine=firecracker`. 
- **re-back snapshot API** on krucible bundles; FC snapshot code unreachable.
- **delete** `pkg/engine/firecracker/` (TAP/bridge/iptables, jailer, FC manifest, FC-shaped interfaces); drop FC + subnet store columns (migration); `docs/migration-v2.md` (FC snapshots aren't portable → export-and-rebuild). **Done:** `rg firecracker pkg/` empty; one VMM.

---

### Sequencing & traceability

| Goal | Track | Depends | v2.0 gate? |
|---|---|---|---|
| G0 krucible fork | 0 | — (keystone) | yes |
| G1 engine boots lohar | A | G0 | yes |
| G2 snapshot/restore/fork | A | G0,G1 | yes (the differentiator) |
| G3 jail the VMM | J | G1 | yes (security gate) |
| G4 net (C1) / secrets (C2) | C | G1 | C1 yes; C2 yes |
| G5 capability tokens | D | G1 (G4 for egress caps) | yes |
| G6 macOS + SDKs + skill | E | G1 (+G2 for restore parity) | yes |
| G7 sunset FC | F | G0–G6 | last |

### Definition of done (v2.0)

1. One VMM — **krucible**; `rg firecracker pkg/` empty.
2. `create/exec/shell/files/publish/sessions` on **macOS and Linux** from one binary.
3. **`snapshot → scp → restore` resumes the live process same-arch (clock correct); `fork` fans out; pause/resume backs the warm tier.**
4. Host-owned egress (in-VMM allowlist), **zero real secrets in the guest** (MITM), per-sandbox capability tokens + audit.
5. **The `vmm` helper is unprivileged + seccomped + namespaced (Track J)** — the precondition for hostile multi-tenant.
6. Python + TS SDKs + a `skills/bhatti` skill.
7. A documented, tested **rebase-onto-upstream loop** for krucible (G0.4).

---

## Risk + mitigations

| Risk | Likelihood | Mitigation |
|---|---|---|
| Owning a VMM fork is a maintenance tax | Certain | Bounded: ~3–5k-line delta (F2), one-hop rebase onto upstream (G0.4); upstream's init/fs/Windows work flows in for free |
| Rebasing smolmachine's patches onto current upstream conflicts | High | They touch files upstream churns (`macos/vstate.rs`, `hvf/lib.rs`, init) — budget real rebase work in G0.2, not a clean cherry-pick |
| No jailer → VMM-process compromise = host compromise | High if skipped | **Track J is a v2.0 gate**; unprivileged + seccomp + cgroup/ns; default no/RO host mounts |
| HVF snapshot bugs (the novel part) | Medium | Inherit smolmachine's implementation + its tests; arm64-symmetric with KVM; macOS CI checkpoint/restore gate |
| Snapshot reliability is its own multi-bug journey (FC scars) | Medium | Reuse the disk/volume/recovery hardening from `PLAN-snapshot-reliability-fixes.md`; refuse-on-uncertain; tight test matrix |
| x86 checkpoint unsupported in the seed | Medium | G0.3: arm64 = full; x86 = cold-boot + overlay, documented; invest later |
| TSI host-side surface / virtio-fs exposure | Medium | egress policy in-VMM; default no host mounts; Track J |
| Upstream "native Rust API" rework churns our bindings | Medium | G0.4 watch-item; consider binding the new Rust API / a `krun-sys`-style shim |
| arm64 RAW `Image` / virtio kernel config wrong | Low (caught) | G1.0 verified artifact + `external_kernel` proof harness |

## Alternatives considered

- **Stock upstream libkrun (no fork).** Rejected: no control surface, no snapshot (F1) → loses the warm tier + live fork, the core of the strategy. Off upstream's roadmap permanently (F3).
- **Write a VMM from scratch (shuru's path).** Rejected: ~2900 lines of KVM device emulation + a VZ integration + permanent virtio tax; libkrun already gives KVM+HVF+virtio+boot. We add a ~3–5k-line *feature* delta, not a VMM.
- **Live on `smol-machines/libkrun`'s fork.** Rejected as the *base*: frozen at Feb-2026 upstream; forfeits upstream's init/virtio-fs/Windows work; two-hop tracking. We take their *patches*, base on *current upstream* (F3).
- **CRIU in-guest snapshot.** Dropped: VM-boundary checkpoint (krucible) is cleaner (no GPU/external-FD fragility, no guest cooperation) — the reason Machinen/smolmachine do it at the VM boundary.
- **Keep Firecracker (dual-engine).** Rejected: capability sets barely overlap (rich control vs none); split-brain; Linux-only. A snapshot-capable krucible dominates FC on every axis.

## Pre-flight verifications (grounded in the clones)

1. **krucible's capability set already exists, open, on both backends** — F2 (file/line evidence). The HVF register snapshot (`hvf/lib.rs:521/617`) is the hard part, and it's done.
2. **Upstream will never ship it** — F3 (`total_count: None` for snapshot/checkpoint/pause/resume/migrate across all history).
3. **libkrun is FC+rust-vmm-derived** — F4 (83 Amazon-copyright files; README statement) — so porting FC's arm64 snapshot / trusting the lineage is sound.
4. **External kernel + lohar PID-1** — `krun_set_kernel` + `krun_disable_implicit_init`; arm64 RAW `Image` (F6).
5. **vsock host-dials-guest = `krun_add_vsock_port2(listen=true)`**, direct UDS dial; confirmed in muvm (F5/F6).
6. **dlopen graceful degradation** — crun's handler (F5) — adopt for the binding layer.
7. **control-socket-over-blocked-`start_enter`** — krunkit `status.rs` (F5) — the shape krucible's socket extends.
8. **post-resume clock fix** — krunkit `timesync.rs` host→guest `guest-set-time` (F5) — add to lohar (G2.3/G6).
9. **VMM-agnostic builders are FC-decoupled** — `firecracker/configdrive.go` (`mke2fs -d`) + rootfs/lohar inject in `create.go` — clean to extract into `guestfs/` (G1.3).
10. **Same KVM/HVF isolation boundary as FC; Rust device models** — the security wash; FC's edge is the jailer (Track J rebuilds it).

## Open Questions

1. **x86 checkpoint** — does the seed support it, or arm64-only? (G0.3 decides the per-arch matrix.)
2. **Bindings: cgo hard-link vs dlopen (purego / C shim)** — dlopen wins on graceful degradation + cross-compile; confirm purego handles the control-socket + eventfd fds cleanly.
3. **Upstream "native Rust API" PR** — bind the new Rust API directly (a `krun-sys`-style shim) vs the legacy C ABI? Affects G1.1 + G0.4.
4. **Track J depth for hostile multi-tenant** — is unprivileged+seccomp+cgroup/ns enough, or do we want an extra gVisor-style layer? Resolve before accepting untrusted multi-tenant.
5. **Rebase cadence** (G0.4) — how often to rebase krucible onto upstream without destabilizing; pin policy.
6. **Migration for FC users** — export-and-rebuild only (FC snapshots aren't portable). Acceptable for a major version (almost certainly yes; document loudly).

## What's NOT in scope

- Keeping Firecracker; writing a VMM from scratch; CRIU; cross-arch restore (refused); a disk-only "bundle" as the *primary* portability mechanism (it's the disk *layer* of the VM checkpoint); out-spending the clouds on VM-start benchmarks (we win via co-location + live snapshot/fork).

## Summary

bhatti **owns its VMM**: **krucible**, a libkrun fork seeded by porting `smol-machines/libkrun`'s checkpoint/restore/fork + HVF snapshot + control socket + egress primitives (Apache-2.0, F2) onto *current* upstream libkrun. This is necessary because snapshot is permanently off upstream's roadmap (F3) — so we own a fork regardless — and tractable because it's a ~3–5k-line *feature* delta on an existing FC+rust-vmm-derived VMM (F4), not a VMM rewrite. Owning krucible **restores the warm tier and adds live fork + cross-machine live migration**, gives **cross-platform** (KVM + HVF, Windows later), and makes **egress allowlisting + zero-secret** VMM primitives — a stronger agentic posture than FC's open-egress NIC. Firecracker's only real edge — the jailer + maturity — is rebuilt as **Track J** (unprivileged + seccomp + cgroup/namespace around the `vmm` helper), a v2.0 security gate. The guest contract (lohar PID 1 + agent protocol) and most of bhatti's surface move unchanged; the ~13k-line FC engine + its TAP/bridge/iptables/jailer host glue is deleted, replaced by less code over a more capable VMM. The keystone is **Track 0: stand up krucible** and boot lohar under it with the control socket answering.
