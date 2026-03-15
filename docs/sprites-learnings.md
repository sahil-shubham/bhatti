# Sprites (fly.io) Architecture Learnings

Research from [Sprites docs](https://docs.sprites.dev/) and
[API reference](https://sprites.dev/api). Sprites is fly.io's pre-release
product for persistent, hardware-isolated Linux environments.

Researched: 2026-03-15

---

## Core philosophy: "persistent computers, not ephemeral functions"

Sprites are microVMs that feel like persistent dev environments. Unlike
serverless, they keep full filesystem state between runs. When idle, they
hibernate automatically and wake on any request. The user never thinks about
start/stop — they just use the Sprite.

## Lifecycle — the defining feature

- **Active**: running, executing code, billed for compute
- **Warm**: idle 30 seconds → hibernated, filesystem on NVMe, fast resume
- **Cold**: prolonged idle → filesystem backed to object storage, slower resume

**Automatic**: 30-second inactivity timeout (not configurable yet). Any CLI
command, HTTP request, or API call wakes the Sprite. No manual pause/resume.

Activity signals: exec running, stdin data, active TCP connection, detachable
session running. If none → idle timer starts.

**What persists across hibernation:**
- All files and directories ✅
- Installed packages ✅
- Environment configs ✅
- Git repos, databases ✅

**What doesn't persist:**
- Running processes ❌
- Network connections ❌
- In-memory state ❌
- /tmp files ❌
- PIDs ❌

This is filesystem-only persistence, NOT memory snapshots. Different from
bhatti's full memory snapshots where processes survive.

## Fixed resources (no config)

- 8 vCPUs, 8 GB RAM, 100 GB storage
- Not resizable. One size fits all. Simplicity over flexibility.

## Sessions — detachable TTY (built-in tmux)

All TTY sessions are automatically detachable:
```
sprite exec -tty npm run dev   # start
Ctrl+\                          # detach
sprite sessions list            # list running
sprite sessions attach <id>     # reattach
sprite sessions kill <id>       # kill
```

Sessions survive disconnects. Process keeps running. No tmux needed.
`max_run_after_disconnect` controls how long a disconnected session stays alive.

## Services — persistent background processes

Services are managed processes that auto-restart on Sprite boot:
```json
{
  "cmd": "npm",
  "args": ["run", "dev"],
  "needs": ["database"]
}
```

Configured via internal API at `/.sprite/api.sock`. Have dependency ordering
(`needs` field). Survive full Sprite restarts. Like systemd but simpler.

## Checkpoints — filesystem snapshots (not memory)

- ~300ms to create, non-disruptive (Sprite keeps running)
- Copy-on-write, incremental (only stores what changed)
- Can restore to any checkpoint
- Filesystem only — no process state

Use cases: before risky operations, reproducible environments, undo.

## Networking

**Public URL per Sprite**: `https://my-sprite-abc123.sprites.dev`
- Port-specific routing: traffic to the URL hits whichever port is listening
- Auth options: `sprite` (token required, default) or `public`
- No proxy setup needed — start a server, it's accessible

**Port forwarding** (local proxy):
```
sprite proxy 3000 8080 5432
# localhost:3000 → sprite:3000, localhost:8080 → sprite:8080, etc.
```

**DNS-based network policy**:
- Allow/deny outbound by domain, not IP
- Preset bundles (e.g., "npm + github only")
- Changes apply immediately, existing connections terminated

## Exec — WebSocket binary protocol

- `WSS /v1/sprites/{name}/exec` — WebSocket, not HTTP
- Binary multiplexing of stdin/stdout/stderr
- Session IDs for reconnection
- Real-time port notifications (`port_opened`/`port_closed` messages)
- `max_run_after_disconnect` timeout

## Filesystem API

Direct file operations without exec:
- `GET /fs/read?path=...` — read file
- `POST /fs/write` — write file
- `DELETE /fs/...` — delete
- `WSS /fs/watch` — watch for changes
- Copy, rename, chmod, chown

## Proxy — WebSocket TCP tunnel

`WSS /v1/sprites/{name}/proxy` → send `{host: "localhost", port: 5432}`
→ transparent TCP relay. Like our port forwarding but over WebSocket.

## Control channel — multiplexed WebSocket

Single persistent WebSocket for sequential operations. Reduces connection
overhead for agents issuing many rapid commands.

## Base image — kitchen sink approach

Ubuntu 25.04 with everything pre-installed:
- Node 22, Python 3.13, Go 1.25, Rust, Ruby, Elixir, Java, Bun, Deno
- Claude Code, Gemini CLI, OpenAI Codex, Cursor — all pre-installed
- Build tools, git, vim, tmux, etc.

No custom images (yet). One image, everything included.

## SDKs: Go, JavaScript, Python, Elixir

Full SDKs from day one. The API is the product surface, not the CLI.

---

## Key differences: Sprites vs bhatti

| | Sprites | Bhatti |
|---|---|---|
| **Persistence** | Filesystem only (processes die) | Full memory (processes survive) |
| **Idle behavior** | Automatic 30s timeout | Manual stop/start |
| **Resume** | Filesystem restore, no processes | Full state restore with processes |
| **Hosting** | Cloud (fly.io) | Self-hosted (Pi, any arm64) |
| **Resources** | Fixed 8 vCPU/8GB | Configurable per sandbox |
| **Images** | One base image | Configurable rootfs |
| **Services** | Built-in service manager | None (use systemd or manual) |
| **Networking** | Public URL per sprite | TAP + NAT, local only |
| **Checkpoints** | Filesystem snapshots (fast, non-disruptive) | Full VM snapshots (slower, process-preserving) |

## What bhatti should learn from Sprites

1. **Auto-idle/resume** — the #1 UX improvement. Users shouldn't manage lifecycle.
2. **Sessions** — detachable TTY without tmux is excellent UX
3. **Services** — managed background processes that survive reboots
4. **Public URLs** — zero-config HTTP access to sandbox services
5. **Filesystem API** — read/write without exec
6. **Port notifications** — real-time, not polling
7. **DNS-based network policy** — user-friendly security
8. **Control channel** — single WebSocket for multiplexed operations

## What bhatti has that Sprites doesn't

1. **Full memory snapshots** — processes, fds, memory all survive
2. **Self-hosted** — no cloud dependency, no data leaving your network
3. **Web terminal UI** — browser-based access
4. **No vendor lock-in** — own everything
5. **Configurable resources** — right-size per workload
