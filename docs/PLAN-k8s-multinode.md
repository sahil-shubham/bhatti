# Multi-node k3s on bhatti — HA cluster spin-up and its performance

Single-node k3s already runs on bhatti end-to-end (proven 2026-08: node
Ready, kube-proxy clean, CoreDNS 1/1, ClusterIP + cluster-DNS routing all
verified after adding five netfilter flags). This plan takes the next step:
**an HA cluster of sandboxes — 3 servers forming etcd quorum plus agents —
and a harness to measure how fast it spins up.**

The plan is deliberately staged so the *performance question you actually
asked* — how fast does an HA cluster form — is answerable in Phase 1 with
almost no new networking code, because HA formation is control-plane TCP
and sibling TCP already works. Cross-node pod networking (the real code) is
Phase 2. The bhatti-native speed win (a baked k3s tier) is Phase 3.

---

## Why

Formation performance is a bhatti story, not a k3s story. bhatti creates a
sandbox in ~2s cold; k3s formation is dominated by TLS + etcd quorum +
~150 MB of image pulls per node. The interesting measurement is the split:
how much of "time to a working 3-node HA cluster" is bhatti overhead
(near-zero) versus k3s bootstrap (most of it) — and how far a baked tier
collapses the pull cost. You cannot answer that without a harness that
times each phase, so the harness is the deliverable, not a side effect.

## First principles

An HA k3s cluster needs exactly three things from the substrate:

1. **Servers reach each other on the control-plane ports** — etcd peer
   2380/tcp, apiserver 6443/tcp, kubelet 10250/tcp. All TCP, all
   sibling-to-sibling.
2. **A stable join endpoint** — the address joiners pass to
   `--server https://<addr>:6443`.
3. **Cross-node pod networking** — pod-to-pod across nodes, and kube-proxy
   routing a ClusterIP to a pod on another node. This is the only piece
   that needs UDP (flannel VXLAN 8472) or an alternative.

Requirement 1 works today (`forward.go:33-37`, the TCP sibling branch).
Requirement 2 is server-1's sandbox IP (`100.64.1.x`, known at create
time). Requirement 3 is the blocker — and it is only needed once you
schedule cross-node workloads, which is why it is Phase 2, not Phase 1.

---

## Current state (verified this session, 2026-08)

| fact | evidence |
|---|---|
| Single-node k3s works | node Ready; nginx ClusterIP `10.43.89.52` curled by IP and by `web.default.svc.cluster.local` from separate client pods |
| The kernel gap was 5 flags | `COMMENT`, `STATISTIC`, `RECENT`, `MULTIPORT`, `MARK` (gated by `NETFILTER_ADVANCED=y`, already present); found by reading k3s's `Extension comment revision 0 not supported` |
| Flags are in `config-trace` only | not yet in `config-lean` — the default kernel still can't run kube-proxy |
| Sibling **TCP** works | `cmd/bhatti-netd/forward.go:33-37` dials siblings via the stack |
| Sibling **UDP** is dropped | `forward.go:98-121` has no sibling branch; comment at `:93-94` explicitly denies siblings — so flannel VXLAN to a sibling is dropped |
| `--disk-size` is broken | flag accepted and reported ("8192 MB disk") but `vda` stayed 1 GB; ext4 not grown |
| Cold-restore wedges vsock | `unexpected dgram pkt: 3`, agent-not-ready after wake — use fresh-create + keep-hot, never wake, for cluster nodes |
| Create is fast | ~2.1 s cold, keep-hot holds the node up |

---

## Scope

### In

- Fold the 5 netfilter flags into `config-lean` so the **default** kernel
  runs k8s (Phase 0).
- A cluster harness that creates N sandboxes, bootstraps k3s HA, and times
  every phase (Phase 1) — the performance instrument.
- The netd sibling-UDP branch + VXLAN kernel flag so cross-node pods work
  (Phase 2).
- A baked `k3s` rootfs tier (binary + airgap images) and a cold-vs-baked
  spin-up measurement (Phase 3).

### Out (deliberately)

- **Track A declarative `apply`.** The harness is a shell script for now;
  once `bhattifile.yaml` exists (declarative-platform plan), the cluster
  becomes a manifest. Not a blocker — a shell harness measures the same
  thing today.
- **Track B internal DNS** for the join endpoint. Server-1's IP is stable
  within the user network and known at create time; DNS is a nicety that
  also fixes server-1-as-SPOF, deferred.
- **Production HA resilience** — VIP/load-balancer in front of the servers,
  server-1 failure during join, etcd backup/restore. This plan measures
  *spin-up*, not *survival*. Called out in Open Questions.
- **WireGuard backend.** VXLAN is the leaner cross-node path (one flag +
  the netd branch we already need); WireGuard adds a module and buys
  encryption we don't need between same-host siblings.

---

## Phase 0 — Make the default kernel k8s-capable

`config-lean_<arch>` gets the five flags already proven in `config-trace`
(flip in place, not append — the duplicate-line trap from this session):

```
CONFIG_NETFILTER_XT_MATCH_COMMENT=y
CONFIG_NETFILTER_XT_MATCH_STATISTIC=y
CONFIG_NETFILTER_XT_MATCH_RECENT=y
CONFIG_NETFILTER_XT_MATCH_MULTIPORT=y
CONFIG_NETFILTER_XT_MATCH_MARK=y
# CONFIG_NETFILTER_ADVANCED already =y (gates COMMENT/STATISTIC)
```

Cost: negligible (~a few KB of match modules). This is the difference
between "k8s works only on the trace kernel" and "k8s works on the default
kernel," and it is a prerequisite for every phase below. Verify: rebuild,
boot single-node k3s on the lean kernel, confirm kube-proxy clean +
CoreDNS 1/1 (the exact checks that passed on the trace kernel).

## Phase 1 — HA control-plane formation + the timing harness

This is the shippable performance slice, and it needs **no new networking
code** — etcd quorum and apiserver join are TCP over the working sibling
path.

`scripts/k3s-cluster.sh up --servers 3 --agents 0`:

1. **Create N sandboxes in parallel**, keep-hot (never wake — cold-restore
   is wedged), each with a volume mounted at `/var/lib/rancher` as the k3s
   data-dir (the disk-size bug workaround — see Cross-cutting).
2. **Server 1:** `k3s server --cluster-init --token=<fixed>
   --flannel-backend=host-gw --disable traefik,servicelb,metrics-server`.
   Fixed token avoids a fetch round-trip.
3. **Servers 2..N:** `k3s server --server https://<server1-ip>:6443
   --token=<fixed> …` — joins the etcd quorum.
4. **Agents (Phase 1b):** `k3s agent --server https://<server1-ip>:6443
   --token=<fixed>`.
5. **Time every transition** and print a table.

`--flannel-backend=host-gw` in Phase 1 means cross-node *pod* traffic won't
route (no per-node CIDR routes in netd yet), but the cluster **forms** and
system pods run locally on each node — which is exactly what "HA spin-up
performance" measures. Phase 2 makes cross-node pods work.

### The metrics the harness reports

```
t_create        parallel sandbox creation → all N running   (expect ~2-3s)
t_server1_ready cluster-init → apiserver responds            (TLS + etcd init)
t_join[i]       server i join → node Ready                   (per node)
t_quorum        3 servers → etcd reports 3 healthy members
t_all_ready     all nodes Ready AND system pods 1/1
--- breakdown ---
% bhatti (create) vs % k3s (bootstrap)                        the headline number
image-pull time (cold) — isolated, because Phase 3 kills it
```

## Phase 2 — Cross-node pod networking

Two changes, both small and both already scoped by this session's findings.

**Kernel:** add VXLAN to `config-lean`/`config-trace`:

```
CONFIG_VXLAN=y
CONFIG_BRIDGE_VLAN_FILTERING=y   # flannel.1 setup
```

**netd:** give `installUDPForwarder` the sibling branch its TCP twin
already has. Today `forward.go:98-121` sends every guest UDP flow through
the egress guard, which denies siblings (`:93-94`). Mirror the TCP branch
at `:33-37`:

```go
// in installUDPForwarder, before the guard dial:
if g.isSibling(id.LocalAddress) {      // netstack.go:191, reused
    up, err = gonet.DialUDP(g.stack,
        nil, &net.UDPAddr{IP: net.IP(id.LocalAddress.AsSlice()), Port: int(id.LocalPort)},
        ...)
} else {
    up, err = g.stateFor(id.RemoteAddress).dialer.DialContext(ctx, "udp", dest)
}
```

This forwards sibling UDP (VXLAN encap on 8472) via the stack while keeping
the fail-closed egress guard for everything else — DNS-rebinding into
host/private space stays denied, exactly as the TCP forwarder does. With
both in place, switch the harness to flannel's default VXLAN backend and
verify a pod on node-1 curls a pod on node-2, and a ClusterIP routes to a
remote endpoint.

**Note the security seam.** Opening sibling UDP widens what one sandbox can
send another. It is same-owner-only (`isSibling` checks the owner subnet),
which matches the TCP posture already shipped — but it should land with the
fail-closed egress work from `PLAN-fail-closed-and-ownership.md` (the
`Siblings Posture` field), so "siblings may exchange VXLAN" is an explicit
policy, not an implicit `if`.

## Phase 3 — The k3s tier (the performance win)

Cold spin-up pays ~150 MB of image pulls per node. bhatti's answer is a
rootfs tier with the binary and airgap images baked in, discovered
automatically (`rootfs-k3s-<arch>.ext4`, globbed at startup).

`scripts/tiers/k3s.sh` (sourcing `minimal.sh`, like the others):

- Install the k3s binary to `/usr/local/bin/k3s`.
- Pre-pull the airgap image set into
  `/var/lib/rancher/k3s/agent/images/` (k3s loads these at boot instead of
  pulling — the documented airgap path).
- Size the ext4 at build time (`SIZE_MB≈3072`) — which **also sidesteps the
  disk-resize bug** for the baked path, because the image is born large.

Then measure: `k3s-cluster.sh up --tier k3s` vs `--tier minimal` (cold).
The delta is the tier's payoff and the plan's performance headline.

---

## Cross-cutting

**Disk.** `--disk-size` is broken (verified). Two responses:
- *Now (Phase 1):* mount a volume at `/var/lib/rancher`; k3s `--data-dir`
  points there. Works today.
- *Right fix (tracked defect):* grow the block device to the requested size
  and `resize2fs` at boot in lohar. File separately; the tier (Phase 3)
  makes it non-urgent for k8s because the tier image is pre-sized.

**Never wake a node.** Cold-restore wedges the vsock muxer (`unexpected
dgram pkt: 3`). Cluster nodes are keep-hot and fresh-created; the harness
must `destroy`+`create`, never `stop`+`start`. This is a real reliability
bug (own issue) but the harness routes around it.

**Memory.** 3 servers × (etcd + apiserver + controller + scheduler +
kubelet + containerd) ≈ 1.5-2 GB each → ~6 GB for the control plane before
agents. Fine on the M4 dev box; size the Hetzner box accordingly. The
harness takes `--server-mem`/`--agent-mem`.

**Join endpoint = server-1 SPOF.** Joiners hit server-1's IP. If server-1
dies mid-join the cluster still runs (quorum) but new joins fail until the
endpoint moves. Acceptable for spin-up measurement; Track B DNS or a fixed
alias fixes it later.

---

## Risks + mitigations

| Risk | Mitigation |
|---|---|
| Phase 1 "forms" but host-gw hides a networking problem that only VXLAN exposes | Phase 1 explicitly measures *formation*, not cross-node pods; Phase 2's verify step (pod-on-n1 → pod-on-n2) is the real networking gate. Don't claim cross-node works off Phase 1. |
| Sibling UDP opens a cross-tenant hole | `isSibling` is same-owner-subnet only (verified `netstack.go:191`); land with the explicit `Siblings Posture` policy field, not a bare `if`. |
| etcd quorum flaps under slow sandbox I/O | etcd is disk-latency sensitive; the data-dir volume must be on fast storage. Measure etcd fsync latency in the harness; if it flaps, that's a finding about bhatti volume perf, not a k3s bug. |
| Image-pull dominates and masks bhatti's real spin-up cost | That's the point of Phase 3; report cold and baked separately so the bhatti number is visible. |
| 3×2 GB keep-hot nodes exhaust host RAM | Harness caps node count and prints projected RAM before creating; refuse if it exceeds a budget flag. |

---

## Phasing

| # | Phase | Ships | Effort |
|---|-------|-------|--------|
| 0 | 5 netfilter flags → `config-lean`, rebuild, single-node re-verify | k8s on the default kernel | 0.5 d |
| 1 | `k3s-cluster.sh up` (servers) + timing table + data-dir volume | **HA formation + the performance number you asked for** | 2 d |
| 1b | agents | mixed server/agent clusters | 0.5 d |
| 2 | VXLAN flag + netd sibling-UDP branch + cross-node verify | cross-node pods + service routing | 2 d |
| 3 | `k3s` tier (baked binary + airgap images) + cold-vs-baked measurement | fast spin-up + the perf headline | 2 d |

**Phase 0 + 1 answer the question in ~2.5 days** with no new networking
code. Phases 2-3 are where the real engineering (netd UDP) and the payoff
(the tier) live.

---

## Pre-flight verifications (done this session)

1. **Single-node k3s is fully working on the trace kernel** — node Ready,
   kube-proxy programs rules with no `comment`/`statistic` errors, CoreDNS
   1/1, nginx ClusterIP `10.43.89.52` reached by IP and by
   `web.default.svc.cluster.local` from two separate client pods.
2. **The 5 netfilter flags are the whole single-node gap** — before them,
   kube-proxy failed on `Extension comment revision 0`; after, clean.
   Verified they survive `olddefconfig` (they were being dropped as
   duplicate lines until flipped in place).
3. **Sibling TCP works, sibling UDP does not** — `forward.go:33-37` vs the
   branchless `forward.go:98-121`; the UDP comment at `:93-94` denies
   siblings. This is why Phase 1 (TCP) is free and Phase 2 (UDP) is code.
4. **`isSibling` is reusable and same-owner-scoped** —
   `netstack.go:191`.
5. **The tier system is glob-discovered** — `scripts/build-tier.sh` emits
   `dist/rootfs-<tier>-<arch>.ext4`; startup globs `rootfs-*-<arch>.ext4`,
   so a new `k3s` tier needs no Go change, only `scripts/tiers/k3s.sh`.
6. **`--disk-size` does not resize `vda`** (stayed 1 GB) and **cold-restore
   wedges vsock** — both verified live; both routed around by the harness
   (data-dir volume, keep-hot fresh-create) and filed as their own defects.
7. **Create is ~2.1 s cold** — so the harness's `t_create` is expected to
   be a rounding error against k3s bootstrap, which is the hypothesis the
   measurement will confirm or refute.

---

## Open questions

1. **host-gw vs VXLAN as the default once Phase 2 lands?** host-gw is
   faster (no encap) and needs no UDP, but requires netd to route per-node
   pod CIDRs between siblings — a *different* netd change than the VXLAN
   sibling-UDP branch. VXLAN is the leaner first cut (one flag + one branch
   mirroring existing TCP code). Decide after Phase 2 measures VXLAN
   overhead on same-host siblings (likely negligible — no real network
   between them). Leaning VXLAN-first, host-gw as an optimization if encap
   cost shows up.
2. **Is server-1-as-join-endpoint acceptable, or pull Track B forward?**
   For spin-up measurement, yes. For anything a human relies on, the
   endpoint must survive server-1, which means Track B DNS or a fixed-IP
   alias — sequence that decision with the declarative-platform plan.
3. **Does etcd on a bhatti volume meet its fsync-latency budget?** Unknown
   until Phase 1 measures it. If etcd flaps, that is the most valuable
   finding in this whole plan — it says something real about bhatti volume
   durability semantics under a fsync-heavy workload.
4. **Should the harness become a `bhattifile.yaml` the moment Track A
   ships?** Almost certainly — an HA cluster is the canonical multi-sandbox
   manifest, and it would validate Track A's design against a real
   workload. Keep the shell harness's phase boundaries aligned with what a
   manifest reconciler would do, so the port is mechanical.
