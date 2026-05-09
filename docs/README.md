# bhatti documentation

> [!IMPORTANT]
> **Documentation has moved to <https://bhatti.sh/docs>.**
>
> The website is the canonical reference and is updated with every release.
> The Markdown files alongside this README are **deprecated stubs** kept only
> for git history; they will not be updated and may be removed in a future
> cleanup.

## Quick links

| | |
| --- | --- |
| **[Quickstart](https://bhatti.sh/docs/quickstart/)** | Install and create your first sandbox |
| **[Concepts](https://bhatti.sh/docs/concepts/)** | Sandboxes, thermal states, the two binaries |
| **[Self-Hosting](https://bhatti.sh/docs/self-hosting/)** | Run bhatti on your own hardware, requirements, backups |
| **[CLI Reference](https://bhatti.sh/docs/reference/cli/)** | Every command with synopsis, flags, exit codes |
| **[API Reference](https://bhatti.sh/docs/reference/api/)** | HTTP API with curl examples and response shapes |
| **[Architecture](https://bhatti.sh/docs/under-the-hood/architecture/)** | System design, data flow, concurrency model |
| **[Decisions & Learnings](https://bhatti.sh/docs/under-the-hood/decisions/)** | Why bhatti is built the way it is |

For agents driving bhatti, start at **<https://bhatti.sh/agents.md>**. The full
machine-readable doc index is at **<https://bhatti.sh/llms.txt>**.

## Where each old doc went

If you landed here from an old link or a stale checkout, the canonical
version of each page now lives at:

| Old path | New URL |
| --- | --- |
| `docs/index.md`             | <https://bhatti.sh/docs/> |
| `docs/quickstart.md`        | <https://bhatti.sh/docs/quickstart/> |
| `docs/architecture.md`      | <https://bhatti.sh/docs/under-the-hood/architecture/> |
| `docs/guest-agent.md`       | <https://bhatti.sh/docs/under-the-hood/lohar-the-blacksmith/> |
| `docs/networking.md`        | <https://bhatti.sh/docs/under-the-hood/networking/> |
| `docs/wire-protocol.md`     | <https://bhatti.sh/docs/under-the-hood/wire-protocol/> |
| `docs/thermal-management.md`| <https://bhatti.sh/docs/under-the-hood/thermal-states/> |
| `docs/decisions.md`         | <https://bhatti.sh/docs/under-the-hood/decisions/> |
| `docs/api-reference.md`     | <https://bhatti.sh/docs/reference/api/> |
| `docs/cli-reference.md`     | <https://bhatti.sh/docs/reference/cli/> |
| `docs/kernel.md`            | <https://bhatti.sh/docs/contributing/kernel/> |
| `docs/testing.md`           | <https://bhatti.sh/docs/contributing/testing/> |
| `docs/tiers.md`             | <https://bhatti.sh/docs/contributing/adding-a-tier/> |

## What's still in this folder

- **`archive/`** — historical PLAN docs and investigations preserved for git
  archaeology. Out of date by design. Do not edit.
- **`internal/`** — gitignored local planning notes (launch posts, drafts,
  internal-only plans). Will not appear in clones.
- **Top-level `*.md` files** — deprecated stubs (see table above). Each one
  carries a deprecation banner pointing at its canonical URL on bhatti.sh.

## Editing documentation

To change documentation, edit the corresponding file in
**[`bhatti.sh/src/content/docs/`](https://github.com/sahil-shubham/bhatti.sh)**
and submit a PR there. Every Starlight page also has an "Edit this page" link
at the bottom that links straight to the right file.
