# Performance & DX review — roundup and priorities

Distilled from a full-codebase review + live benchmarking session (2026-07-09,
darwin/arm64, HVF, v2.1.1, minimal tier). Companion to the krucible plan docs.

---

## 1. Measured state (the numbers everything below is ranked by)

| Operation | Measured | Verdict |
|---|---|---|
| exec `true` — SDK floor (keep-alive over api.sock) | **p50 1.2ms · min 0.8ms** | Done. Defend, don't optimize. |
| exec `true` — CLI | p50 ~32ms | Process start + 1 wasted RTT. One fix, then stop. |
| create (cold boot, qcow2 CoW root) | 320–400ms | Boot-bound (~312ms kernel→agent). Step change available. |
| stop (snapshot 512MB VM to disk) | 538–937ms | Slowest op, worst variance (1.7× spread). Unprofiled. |
| cold wake (exec on stopped sandbox) | **77–142ms** | Proof that restore ≈ 3–4× cheaper than boot. |
| warm wake | unmeasured | No pause API; needs 30s idle per sample. Bench gap. |
| destroy | 10–20ms | Fine. |
| file transfer (marginal) | ~260MB/s | Fine; per-op floor dominates. |
| egress throughput (netd, 3 TCP stacks) | **unmeasured** | Likely where real agent wall-clock hides. |
| density (N warm sandboxes/host) | **unmeasured** | Decides the self-host story. |

Measurement lessons encoded in the bench going forward:
- curl-per-request inflated exec 8× (26.9ms vs 3.3ms) — client process spawn
  is not server latency. SDK floor and CLI floor are different products;
  bench them separately, never conflate.
- Stale result files ≠ current numbers. The README perf table is still v1/FC;
  fresh v2 numbers above are better than the v1 table — publish them.

## 2. Found & fixed during the session

- **Local install was broken**: bundled `rootfs-minimal-arm64.ext4` (v2.1.0)
  predates the `/init.krun` fix (205d92e) — every create kernel-panicked.
  Updated in place to v2.1.1 (checksums verified, config repointed, daemon
  restarted, default tier boot re-verified). Rollback: `~/.bhatti/local.yaml.v2.1.0.bak`.
- Daemon config is selected by `BHATTI_CONFIG`; without it the daemon read the
  CLI config and exited near-silently (see P0-5).
- Artifacts left for future benches: user `bench-local` (`usr_1a969bc8`),
  image `bench-alpine`. An API token was printed into the session — rotate it.

---

## 3. Priorities

### P0 — this week (hours each) — **DONE 2026-07-09**

1. ~~Rotate the leaked API token~~ — local bench-local key rotated & discarded.
   **Remote (api.bhatti.sh) token rotation is on the operator.**
2. ~~Remove `resolveID`'s extra round trip~~ — now a pass-through; server 404
   message upgraded to `sandbox "x" not found` (pkg/server/routes.go).
3. ~~Makefile `.PHONY` lohar/netd~~.
4. ~~`registerTierImages` → `runtime.GOARCH`~~.
5. ~~`serve` fails loudly~~ — engine-create failure now logs the loaded config
   path + BHATTI_CONFIG hint.
6. ~~Publish fresh v2 numbers~~ — README Performance section now leads with
   measured v2.1.1 macOS numbers; v1/FC table kept as the Linux baseline.
7. ~~store.go orphaned comments~~; ~~flag precedence~~ — **flipped to
   flag → env → config** (12-factor). NOTE for release notes: behavior change
   when both env and config are set.

**Bonus fixes surfaced by P0 verification:**
- `confirmAction` abort paths returned exit 0 — an aborted `destroy` read as
  success to scripts/agents. All 7 destructive commands now return `errAborted`
  (non-zero).
- CLI integration tests: were skipping in CI and silently running against the
  production v1 server via config-file precedence (the exact precedence bug);
  fixed `-y` flags, `createdID` output parsing, minimal-tier-compatible test
  listener (perl; no python3/nc in minimal, /bin/sh is dash), hugepages test
  made engine-aware (skip on krucible).

**New backlog item (P1): CLI suite v1→v2 drift inventory.** Still failing
against a local v2 daemon for *semantic* reasons needing product decisions,
not test fixes: IP line in create/inspect output (krucible reports no guest
IP), NDJSON streaming shape, exec-on-stopped error text, keep-hot edit,
publish/share flows, snapshot resume, disk/volume resize. Decide the v2
contract per feature, then fix test or server. Goal: whole suite green against
a local v2 daemon, wired into CI with a hermetic daemon fixture.

### P1 — measurement infrastructure (~1 week; gates all perf work)

*Rule: no optimization lands without a before/after from this tooling.*

1. **Server-Timing header** with phases (auth / db / wake / agent_dial / guest),
   surfaced by the CLI's existing `--timing`.
2. **pprof** on the admin unix socket (never public).
3. **Bench split + CI gate**: persistent-client SDK-floor harness and CLI
   harness as separate metrics; regression gates on exec floor and create p50.
4. **Close the bench gaps**: debug `pause` verb (makes warm wake benchable);
   iperf3 through netd; wall-clock of a real ~200MB `npm install`; density
   bench (50 sandboxes → warm: host RSS, wake-all time, thermal cycle cost).

### P2 — the two big performance projects (weeks)

1. **Create-as-restore** (golden checkpoint per tier × shape; restore +
   re-identify on create). Measured cold wake ~100ms is the proof of the fast
   path; expect create 350ms → 100–150ms, sub-100ms with the snapshot file
   pinned in page cache. All primitives exist (`Checkpoint`,
   `ResumeFromManifestJSON`, `Fork`, token/config injection). Includes:
   pure-Go qcow2 overlay writer (drop the `bhatti-vmm create-overlay` process
   spawn + dlopen from the hot path); pre-fault base image + kernel at daemon
   start (kills the 2.2s first-create outlier); AGENT_READY event on the
   control socket to replace the WaitReady poll ladder.
2. **Egress throughput**: measure first (P1.4). Then the cheap lever — raise
   guest↔netd MTU (virtual link; 16–64KB) for fewer frames/syscalls/copies per
   byte; plausibly 2–4×. Buffer pooling / frame batching only if profiles say so.
3. **Stop**: one profiling session on the 538–937ms variance (fsync vs
   serialization), then async stop (`stopping` state, background write,
   `--wait` flag). No dirty-page diff snapshots — the v1 decision stands.

### P3 — DX platform (ordered by leverage)

1. **`bhatti doctor --json`** — the broken-install session is the spec:
   hypervisor/entitlements, **open each image and stat `/init.krun`**, kernel
   present, config candidates + which loaded, component version coherence
   (CLI / daemon / bundle / gitlink), netd liveness, socket path lengths, disk
   space, DB integrity. First line of agents.md: "run doctor, fix what it says."
2. **`bhatti ssh-proxy`** (ProxyCommand + socket-activated sshd in tiers, over
   the existing Tunnel). One adapter → VS Code/Cursor Remote, rsync, scp, git,
   Ansible, mutagen. Keys already exist. `bhatti shell` stays the flagship
   human UX (detach/reattach/scrollback are better than plain SSH).
3. **Structured error contract** `{error, code, hint, request_id}` +
   `bhatti admin trace <request-id>` (daemon already correlates; close the loop
   — the vmm.log tail should reach the client, not just the daemon log).
4. **Sandboxfile + `bhatti up`** (declarative, idempotent). Also becomes the
   cross-arch "migration" story for free.
5. **OpenAPI spec → generated TS/Python SDKs** (the 1.2ms floor is SDK-shaped).
6. **Prometheus `/metrics`** + `bhatti logs <sb> [--follow]` (syslog receiver
   and event fan-out already exist).
7. **`bhatti update`**: detect BHATTI_CONFIG/user-local server layouts (today
   it only recognizes /etc/bhatti and would CLI-only-update a mac self-host).

### P4 — strategic (quarter+)

1. **arm64↔arm64 sandbox teleport** (mac HVF ↔ linux KVM) via a normalized,
   hypervisor-neutral snapshot format. You own both VMM sides; guest-visible
   ARMv8 state + identical virtio device set makes it feasible. Nobody else
   can ship this. x86↔arm64 stays impossible (ISA physics) — offer
   declarative recreate (P3.4) + volume carry instead.
2. **Hot volumes via NBD-over-vsock** (no VMM changes, hot attach/detach/resize;
   NBD client in lean kernel + ~300 lines in lohar). virtio-pci hotplug in the
   fork later, when it unlocks more than volumes.
3. **Mutagen sync** — near-free after ssh-proxy (SSH shim transport, like its
   Docker transport). Sync beats network-FS for remote dev loops.
4. **Secrets at the gateway**: explicit-proxy pattern (guest gets
   HTTPS_PROXY=gateway with placeholder creds; netd swaps headers toward
   allowlisted hosts). No transparent TLS MITM / no per-owner CA. Header-only,
   no body rewriting in v1. Decide QUIC: block UDP 443. IPv6 policy parity or
   disable. DoH bypass stance documented.
5. **Content-addressed export/import** (qcow2 cluster dedup against shared tier
   bases) — one mechanism for backup, `bhatti move` (same-arch), and off-box
   snapshots.

### Hygiene backlog (opportunistic, with other work in the same files)

- Typed state machine: one product state (Hot/Warm/Cold/Stopped/Unknown) or a
  documented Status×Thermal transition table; kill the 80+ bare-string sites.
- Split server.go (904 lines): thermal_manager.go, backup_scheduler.go, cron.go.
- Decompose krucible `create()` (~220 lines) into phase funcs + cleanup stack.
- Dedupe client.go session plumbing (4 near-identical dial→SESSION_INFO blocks).
- Resolve engine capability interfaces once at server construction.
- `limits.go` for magic numbers (10MB exec cap, 64KB scrollback, WaitReady ladder).
- lohar test fixtures via `t.TempDir()`; loose binaries → `bin/`.

### Explicit do-not-do list

- Exec-path micro-optimizations (frame pooling, auth caching, connection
  multiplexing) — floor is 1.2ms; revisit only if the concurrency bench shows
  convoys or remote-RTT workloads demand mux.
- CLI startup beyond the resolveID fix (<100ms is instant; SDK is the hot path).
- Dirty-page incremental snapshots (v1 decision holds; async stop instead).
- Transparent TLS interception for secret injection (explicit proxy instead).
