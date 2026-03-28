# ⚒ Bhatti

Open-source Firecracker microVM orchestrator. Each sandbox is a real Linux VM with its own kernel, filesystem, and process isolation — created in seconds, paused for free, resumed in microseconds.

Built for running AI coding agents in isolated environments. A paused sandbox resumes and executes a command in **under 3ms**.

```
bhatti create --name dev --cpus 2 --memory 1024
bhatti exec dev -- npm install
bhatti shell dev                          # Ctrl+\ to detach
bhatti destroy dev
```

## Install the CLI

No KVM, root, or Go needed — just a ~11MB binary. Works on macOS and Linux.

```bash
curl -fsSL https://raw.githubusercontent.com/sahil-shubham/bhatti/main/scripts/install-cli.sh | bash
bhatti setup     # enter API endpoint + key
```

Self-hosting? See [server installation](docs/quickstart.md#option-b-run-the-server-self-hosted).

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
  ├─ Per-user auth (API keys, SHA-256)        ├─ TCP :1025 (port forwarding)
  ├─ Firecracker engine                       ├─ PTY sessions + 64KB scrollback
  │  (create, exec, snapshot, diff snap)      ├─ Atomic file writes
  ├─ Thermal manager                          ├─ Process group kill
  │  (hot → warm → cold, auto)               ├─ Exec as uid 1000 (not root)
  ├─ Per-user bridge networks (isolated)      └─ Config drive (env, secrets)
  ├─ SQLite store + age encryption
  ├─ Rate limiting + exec timeouts
  └─ Reverse proxy (HTTP + WebSocket)
```

Idle sandbox → **warm** after 30s (vCPUs paused, ~400µs resume) → **cold** after 30min (snapshotted to disk, memory freed, ~50ms resume). Any API request transparently wakes it.

## Multi-Tenant Isolation

Each user gets their own API key, sandbox limits, and network:

```bash
sudo bhatti user create --name alice --max-sandboxes 5
# → API key: bht_...  (shown once)
```

- **API scoping** — users see only their own sandboxes and secrets
- **Network isolation** — per-user bridge + /24 subnet, cross-user traffic blocked at L2
- **Resource caps** — per-user limits on sandbox count, CPUs, and memory
- **Rate limiting** — per-user token buckets (10 creates/min, 120 execs/min)
- **Secrets** — encrypted at rest (age), scoped per user

## Documentation

| | |
|---|---|
| **[Quickstart](docs/quickstart.md)** | CLI install + server install, user management |
| **[Architecture](docs/architecture.md)** | System design, data flow, concurrency model |
| **[Wire Protocol](docs/wire-protocol.md)** | Binary framing, connection lifecycle, auth |
| **[Guest Agent](docs/guest-agent.md)** | PID 1 init, PTY, sessions, process management |
| **[Thermal Management](docs/thermal-management.md)** | Hot/warm/cold, diff snapshots, activity caching |
| **[Networking](docs/networking.md)** | Per-user bridges, iptables isolation, kernel ip= |
| **[API Reference](docs/api-reference.md)** | REST/WebSocket endpoints |
| **[CLI Reference](docs/cli-reference.md)** | All commands — create, exec, shell, user, setup |
| **[Testing](docs/testing.md)** | 11K lines of tests, zero mocks for VM tests |
| **[Design Decisions](docs/decisions.md)** | Why TCP over vsock, why no FC SDK, why PID 1, ... |

## Key Features

- **Multi-tenant** — per-user API keys, sandbox scoping, network isolation, rate limiting
- **Streaming exec** — real-time NDJSON output via `Accept: application/x-ndjson`
- **Server-side file truncation** — `offset`/`limit`/`max_bytes` on file reads
- **Diff snapshots** — only dirty pages after the first snapshot (~52ms vs ~4.4s)
- **Session-aware exec** — TTY sessions survive disconnects, scrollback replayed on reattach
- **Atomic file writes** — temp + fsync + rename, concurrent readers never see partial content
- **Process group kill** — `SIGKILL` to pgid for piped exec, `SIGTERM` for TTY sessions
- **Guest hardening** — exec as uid 1000, config drive unmounted after boot, connection/session limits
- **Single binary** — `bhatti serve` = daemon, `bhatti create` = CLI, `bhatti user` = admin

## Requirements

**Server:** Linux (aarch64 or x86_64) with KVM (`/dev/kvm`) and root access.

**CLI:** macOS or Linux. No special requirements.

## License

[Apache 2.0](LICENSE).
