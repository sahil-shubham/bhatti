# ⚒ Bhatti

Firecracker microVM orchestrator. Each sandbox is a real Linux VM with its own kernel, filesystem, and process isolation — created in seconds, paused for free, resumed in microseconds.

Built for running AI coding agents in isolated environments. A paused sandbox resumes and executes a command in **under 3ms**.

```
bhatti create --name dev --cpus 2 --memory 1024
bhatti exec dev -- npm install
bhatti shell dev                          # Ctrl+\ to detach
bhatti destroy dev
```

## Performance

On a Raspberry Pi 5 (ARM64, NVMe):

```
                                p50       p95       p99
Exec `true`:                    1.0ms     1.2ms     1.3ms
1KB file read:                  472µs     826µs     881µs
Warm→exec (resume + exec):     2.5ms     2.6ms     2.6ms
10 concurrent execs:            18ms      19ms      19ms

VM boot:                        ~3.5s
Diff snapshot (idle VM):        ~52ms
Pause/Resume:                   ~400µs
```

## Architecture

```
bhatti (host daemon)                        lohar (guest agent, PID 1 in each VM)
  ├─ REST/WS API (:8080)                     ├─ TCP :1024 (exec, files, sessions)
  ├─ Firecracker engine                       ├─ TCP :1025 (port forwarding)
  │  (create, exec, snapshot, diff snap)      ├─ PTY allocation + session registry
  ├─ Thermal manager                          ├─ Atomic file writes
  │  (hot → warm → cold, auto)               ├─ Process group kill
  ├─ Bridge networking (253 VMs)              ├─ 64KB scrollback per session
  ├─ SQLite store + age encryption            └─ Config drive (env, secrets, volumes)
  └─ Reverse proxy (HTTP + WebSocket)
```

Idle sandbox → **warm** after 30s (vCPUs paused, ~400µs resume) → **cold** after 30min (snapshotted to disk, memory freed, ~50ms resume). Any API request transparently wakes it. The consumer never sees thermal states.

## Documentation

| | |
|---|---|
| **[Quickstart](docs/quickstart.md)** | Install, create, exec, shell, destroy |
| **[Architecture](docs/architecture.md)** | System design, data flow, concurrency model |
| **[Wire Protocol](docs/wire-protocol.md)** | Binary framing, connection lifecycle, auth |
| **[Guest Agent](docs/guest-agent.md)** | PID 1 init, PTY, sessions, process management |
| **[Thermal Management](docs/thermal-management.md)** | Hot/warm/cold, diff snapshots, activity caching |
| **[Networking](docs/networking.md)** | Bridge, TAP, IP pool, kernel ip=, post-snapshot TCP |
| **[API Reference](docs/api-reference.md)** | REST/WebSocket endpoints |
| **[CLI Reference](docs/cli-reference.md)** | All commands with examples |
| **[Testing](docs/testing.md)** | 11K lines of tests, zero mocks for VM tests |
| **[Design Decisions](docs/decisions.md)** | Why TCP over vsock post-snapshot, why no FC SDK, why PID 1, ... |

## Quick Start

```bash
# On a Linux host with KVM
git clone https://github.com/sahil-shubham/bhatti.git && cd bhatti
sudo ./scripts/install.sh
# starts daemon, creates rootfs, configures everything
```

See [docs/quickstart.md](docs/quickstart.md) for the full walkthrough.

## Key Features

- **No templates required** — create sandboxes with CPUs, memory, env vars, init scripts directly
- **Streaming exec** — real-time NDJSON output via `Accept: application/x-ndjson`
- **Server-side file truncation** — `offset`/`limit`/`max_bytes` on file reads (agents truncate to 2000 lines / 50KB — doing it guest-side avoids transferring megabytes)
- **Diff snapshots** — after the first full snapshot, subsequent snapshots write only dirty pages (~52ms vs ~4.4s)
- **Session-aware exec** — TTY sessions survive disconnects, scrollback replayed on reattach
- **Atomic file writes** — temp + fsync + rename, concurrent readers never see partial content
- **Process group kill** — `SIGKILL` to pgid for piped exec, `SIGTERM` for TTY sessions
- **Single binary** — `bhatti serve` = daemon, `bhatti create` = CLI

## Requirements

- Linux (aarch64 or x86_64) with KVM (`/dev/kvm`)
- Root access

## License

[AGPL-3.0](LICENSE). If your use case requires a commercial license, [get in touch](mailto:sahil@bhatti.sh).
