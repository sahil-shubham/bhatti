# Hermes Agent — issue engagement plan for bhatti

> Goal: earn attention/credibility for bhatti as a sandbox option **before**
> writing the backend, by engaging the issues where bhatti's microVM isolation
> is the actual answer. Ranked by: bhatti fit × demand × maintainer-alignment ×
> strategic leverage. (Implementation plan: `hermes-agent-backend.md`.)

## Why engage-first (recap of the evidence)

- 12 backend PRs are stalled; **11/12 reference no issue** → orphan drive-by
  contributions, no demand signal, no maintainer attention.
- The only sandbox work getting deep review is the **maintainer's own**
  iron-proxy egress-firewall PR (#30179, teknium1, 65 reviews). Maintainer
  priority in this area = **the isolation/security boundary**, not breadth of
  backends.
- A 13th "add bhatti backend" PR would join the dead zone. Instead, land bhatti
  in the conversations where its differentiator (real per-VM kernel isolation,
  microsecond resume) is the recommended fix.

## Demand snapshot (open issues, reactions = demand)

| # | Title | comments | +1 | bhatti fit |
|---|-------|:--:|:--:|------------|
| 30101 | code-exec runs same UID, no seccomp/network isolation (P2 sec) | 1 | 0 | ★★★★★ names **Firecracker** as the fix |
| 10561 | Custom runtime for per-execution sandbox isolation (gVisor user) | 0 | 0 | ★★★★★ per-exec microVM |
| 1855 | Multi-backend terminal — local + N remotes, persistent | 5 | 5 | ★★★★ reference remote/persistent backend |
| 4271 | Per-subagent terminal backend isolation | 0 | 2 | ★★★★ microVM-per-subagent |
| 30179 (PR) | iron-proxy egress firewall for sandboxes (teknium1) | 12 | 4 | ★★★ network-ns complements it |
| 15790 | Docker sandbox isolation scoped to user.id (messaging) | 0 | 2 | ★★★ microVM-per-user |
| 4281 | Enforce sandboxed execution for messaging sessions | 1 | 0 | ★★★ |
| 20744 (RFC) | Multi-user gateway access control (admin/user + Docker) | 0 | 1 | ★★ |
| 26675 (RFC) | Managed Agent Runtime contracts | 2 | 0 | ★★ bhatti as primitive |
| 32141 | Named terminal backend pool, per-call selection | 1 | 0 | ★★ |
| 8943 | Docker sandbox for non-main terminal sessions | 0 | 1 | ★★ |
| 10561… | (custom runtime, see above) | | | |

---

## Tier 1 — LEAD HERE (security/isolation; bhatti is the literal answer)

### #30101 — "no seccomp/network isolation, same UID" (P2, type/security)
**The single best entry point.** The issue body says:
> "Consider using gVisor, **Firecracker**, or nsjail for stronger isolation."

bhatti is a Firecracker microVM orchestrator — separate kernel, own network
namespace, true UID boundary, by construction. This is on-roadmap for the
maintainers (iron-proxy proves they're investing in exactly this boundary).
- **Angle:** "Firecracker/microVM is the strongest option on this list — each
  `execute_code` / terminal session in its own VM means the listed exfil
  vectors (`/proc/<ppid>/mem`, `~/.ssh`, raw network) are physically
  unavailable, not just policy-blocked. bhatti (OSS) does this with
  microsecond resume so per-call isolation is actually affordable. Happy to
  prototype a backend if there's interest." Link bhatti, keep it technical.
- **Why first:** security-labeled, maintainer-aligned, bhatti is uniquely
  positioned vs Docker/nsjail.

### #10561 — Custom runtime for per-execution sandbox isolation
Author built a gVisor-isolated multi-agent Hermes (gvisor.dev blog), wants a
**unique sandbox per code execution**, tagged @teknium1.
- **Angle:** bhatti gives one microVM per execution cheaply (pause-is-free,
  resume <4ms), which is precisely the "unique sandbox per request" they want —
  with VM-level isolation, not container-level. Offer bhatti as a peer option
  alongside the proposed `--runtime=runsc`.
- **Why:** technical, isolation-minded author; aligns bhatti with a credible
  existing isolation story rather than competing with it.

## Tier 2 — PLANT THE FLAG (top community demand)

### #1855 — Multi-backend terminal (local + N remotes, persistent)
Highest demand (+5, 5 comments) and **no maintainer reply yet**. Users want the
*architecture* (persistent remote sessions across machines), which is exactly
bhatti's create/stop(snapshot)/start(resume) model.
- **Angle:** Offer bhatti as the reference implementation of a clean
  remote+persistent backend; note its stop=snapshot/start=resume maps 1:1 to
  the persistence the thread is asking for. Reference the backend plan.
- **Caveat:** no maintainer has engaged → treat as flag-planting/visibility, not
  a green light. Pair with Tier 1 to actually get noticed.

### #4271 — Per-subagent terminal backend isolation (+2)
Parallel subagents each needing an isolated workspace.
- **Angle:** microVM-per-subagent is cheap with bhatti (free pause, fast
  resume) — isolation without the per-container overhead. Short, supportive
  comment tying to #1855.

## Tier 3 — ALIGN, DON'T HIJACK (maintainer's active work)

### #30179 (PR) — iron-proxy egress credential-injection firewall (teknium1)
The hot, maintainer-driven sandbox-security PR (65 reviews). **Do not pitch
bhatti here.** Instead add genuine operator/runtime-boundary value (the kind of
review teknium1 thanked others for), e.g. how a per-VM network namespace
interacts with the proxy assumption, or failure modes. Goal: get on the
maintainer's radar as a competent sandbox-security contributor. Credibility now
→ receptiveness later.

## Tier 4 — SUPPORT / WATCH (comment only if Tier 1–2 gains traction)

- **#15790 / #20744** multi-user isolation → microVM-per-user.id story.
- **#4281** enforce sandboxed execution for messaging → microVM default.
- **#26675 (RFC)** managed agent runtime → bhatti as a durable-sandbox primitive.
- **#32141 / #8943** named backend pool / non-main session sandbox → fits the
  multi-backend direction; defer to #1855.

## Explicitly DO NOT (yet)

- Open a "add bhatti backend" PR cold — that's the stalled path (11/12 orphans).
- Comment on the backend **bug** issues (the ~20 docker/ssh cwd/cleanup bugs) —
  noise for our goal.
- Spam the same pitch across many issues — pick Tier 1 + one Tier 2, go deep.

## Sequencing

1. **Comment on #30101** (Tier 1) — the technical isolation pitch + offer to prototype.
2. **Comment on #10561** (Tier 1) — peer option to gVisor, per-exec microVM.
3. **Plant in #1855** (Tier 2) — reference remote/persistent backend.
4. **Add real review value on #30179** (Tier 3) — build maintainer rapport.
5. **Gate on response:** if a maintainer engages on #30101/#10561/#1855 →
   propose either (a) a scoped backend PR *referencing that issue*, or (b) the
   terminal-backend plugin-discovery RFC (so backends live out-of-tree, the
   memory-provider precedent). Only then start the implementation in
   `hermes-agent-backend.md`.
</content>
