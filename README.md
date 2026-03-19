# ⚒ Bhatti

Microvm sandbox infrastructure. Firecracker VMs with sub-millisecond pause/resume, persistent sessions, and transparent thermal management.

Built for running AI coding agents in isolated environments — each agent gets its own Linux VM with full filesystem, networking, and process isolation.

## Quick start

```bash
# On a Linux host with KVM (Raspberry Pi 5, AWS Graviton, x86_64 bare metal)
git clone https://github.com/sahil-shubham/bhatti.git
cd bhatti
sudo ./scripts/install.sh
```

The install script builds everything from source (~10 min on first run):

```
==> Installing bhatti on myhost (aarch64)
==> Installing Go 1.24.1...
==> Installing Firecracker 1.6.0...
==> Building bhatti and lohar from source...
==> Downloading kernel...
==> Building rootfs (this takes ~10 minutes on first install)...
==> Generating config...
==> Installing systemd service...
  waiting for daemon... ready

============================================
  bhatti is running on :8080
  auth token: a1b2c3d4e5f6...

  Quick start:
    export BHATTI_TOKEN=a1b2c3d4e5f6...
    bhatti create --name hello
    bhatti exec hello -- echo 'it works'
    bhatti shell hello
    bhatti destroy hello
============================================
```

## CLI

Single binary — `bhatti` is both the daemon (`bhatti serve`) and the CLI.

```bash
# Create a sandbox (no template needed)
bhatti create --name dev --cpus 2 --memory 1024
# → a1b2c3d4  dev  192.168.137.2

# Execute commands
bhatti exec dev -- uname -a
# → Linux dev 6.1.90 ... aarch64 GNU/Linux

bhatti exec dev -- node --version
# → v22.16.0

# Interactive shell (Ctrl+\ to detach)
bhatti shell dev

# File operations
echo 'console.log("hello")' | bhatti file write dev /workspace/app.js
bhatti file read dev /workspace/app.js
bhatti file ls dev /workspace/

# List sessions (init scripts, detached shells)
bhatti ps dev

# List all sandboxes
bhatti list

# Destroy
bhatti destroy dev

# Secrets
bhatti secret set API_KEY sk-...
bhatti secret list
bhatti secret delete API_KEY
```

Environment variables:
- `BHATTI_URL` — API endpoint (default: `http://localhost:8080`)
- `BHATTI_TOKEN` — Auth token (default: from `~/.bhatti/config.yaml`)

## REST API

```
GET    /health                                 Health check (no auth required)

POST   /sandboxes                              Create sandbox
GET    /sandboxes                              List sandboxes
GET    /sandboxes/:id                          Get sandbox
DELETE /sandboxes/:id                          Destroy sandbox
POST   /sandboxes/:id/exec                     Execute command (buffered or streaming)
GET    /sandboxes/:id/ws                       WebSocket shell
POST   /sandboxes/:id/stop                     Snapshot to disk
POST   /sandboxes/:id/start                    Resume from snapshot
GET    /sandboxes/:id/ports                    List listening ports
GET    /sandboxes/:id/sessions                 List sessions
GET    /sandboxes/:id/files?path=...           Read file (supports offset/limit/max_bytes)
GET    /sandboxes/:id/files?path=...&ls=true   List directory
PUT    /sandboxes/:id/files?path=...           Write file
HEAD   /sandboxes/:id/files?path=...           Stat file
GET    /sandboxes/:id/proxy/:port/...          HTTP/WebSocket reverse proxy
```

Create sandbox (no template required):
```json
POST /sandboxes
{
  "name": "my-sandbox",
  "cpus": 1,
  "memory_mb": 512,
  "env": {"API_KEY": "sk-..."},
  "init": "npm install",
  "new_volumes": [{"name": "work", "size_mb": 256, "mount": "/workspace"}]
}
```

### Streaming exec (NDJSON)

Request with `Accept: application/x-ndjson` to stream output in real time:

```bash
curl -N -H "Accept: application/x-ndjson" \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"cmd":["npm","install"]}' \
  http://localhost:8080/sandboxes/$ID/exec
```

```json
{"type":"stdout","data":"Installing dependencies...\n"}
{"type":"stderr","data":"npm warn deprecated ...\n"}
{"type":"stdout","data":"added 847 packages in 12s\n"}
{"type":"exit","exit_code":0}
```

Without the `Accept` header, the existing buffered JSON response is returned (backward compatible).

### Server-side file truncation

```
GET /sandboxes/:id/files?path=/app.log&offset=1&limit=2000&max_bytes=51200
```

- `offset` — 1-indexed line number to start from
- `limit` — max lines to return
- `max_bytes` — max bytes to return

Whichever limit hits first stops the read. Avoids transferring 100MB log files when the consumer only needs the first 2000 lines. The `X-File-Size` response header reports the total file size so the client knows if content was truncated.

## Performance

Measured on Raspberry Pi 5 (ARM64, NVMe) with percentiles:

```
                                p50       p95       p99
Exec `true`:                    1.0ms     1.2ms     1.3ms
1KB file write:                 809µs     1.1ms     1.4ms
1KB file read:                  472µs     826µs     881µs
5 parallel file reads:          1.9ms     2.3ms     2.3ms
Warm→exec (resume + exec):     2.5ms     2.6ms     2.6ms
10 concurrent execs:            18ms      19ms      19ms
Stream exec TTFB:               963µs     5.6ms     5.6ms
```

```
VM boot (create → ready):      ~3.5s
Full snapshot (512MB):          ~4.4s
Diff snapshot:                  ~52ms
Restore (cold → hot):           ~50ms
Pause/Resume:                   ~400µs
Exec throughput:                ~1000 exec/sec
```

A paused sandbox resumes and executes a command in **under 3ms**.

## Architecture

```
bhatti (host daemon)
  ├─ CLI (create, exec, shell, file, secret, ...)
  ├─ REST/WS API (:8080) with structured logging (log/slog)
  ├─ Firecracker engine (create, exec, snapshot/restore, diff snapshots)
  ├─ Filesystem API (read, write, stat, ls — atomic writes, server-side truncation)
  ├─ Streaming exec (NDJSON, content-negotiated)
  ├─ Thermal manager (hot → warm → cold, host-side activity cache)
  ├─ Bridge networking (192.168.137.0/24, up to 253 VMs)
  ├─ SQLite store (sandboxes, secrets, templates, FC state)
  ├─ Reverse proxy (HTTP + WebSocket through VM tunnels)
  └─ Graceful shutdown (drain connections, clean TAPs)

lohar (guest agent, PID 1 inside each VM)
  ├─ TCP :1024 (exec, sessions, activity, file ops)
  ├─ TCP :1025 (port forwarding)
  ├─ Config drive (env vars, files, volumes, auth token)
  ├─ Session registry (TTY sessions survive disconnects)
  ├─ File handlers (atomic write, server-side truncation)
  ├─ Process group kill (SIGKILL to pgid for reliable abort)
  └─ Scrollback buffers (64KB ring buffer per session)
```

**Thermal states** — managed automatically:

| State | FC process | vCPUs | Host RAM | Resume time |
|-------|-----------|-------|----------|-------------|
| Hot   | alive     | running | allocated | —         |
| Warm  | alive     | paused  | allocated | ~400µs    |
| Cold  | dead      | —       | freed     | ~50ms     |

Idle sandbox → warm after 30s → cold after 30min. Any API request transparently wakes it.

## Key features

- **No templates required** — create sandboxes directly with CPUs, memory, env vars, init scripts. Templates still supported for repeated configurations.
- **Streaming exec** — real-time NDJSON output via `Accept: application/x-ndjson`. Backward compatible — without the header, existing buffered JSON works unchanged.
- **Server-side file truncation** — `offset`, `limit`, `max_bytes` parameters on file read. Agents truncate to 2000 lines / 50KB; doing it guest-side avoids transferring megabytes that get thrown away.
- **Diff snapshots** — after the first full snapshot, subsequent snapshots write only dirty pages. Reduces snapshot time from ~4.4s to ~52ms for idle VMs.
- **Filesystem API** — read, write, stat, list files inside VMs. Writes are atomic (temp+rename) so concurrent readers never see partial content. Binary-safe, supports all 256 byte values.
- **Session-aware exec** — every TTY exec is a session. Disconnect and reconnect later; scrollback is replayed. Processes survive host disconnects.
- **Process group kill** — piped exec abort kills the entire process tree (SIGKILL to pgid), not just the shell. Child processes like `npm install → node` are reliably terminated.
- **Agent tooling** — `ripgrep` (rg) and `fd-find` (fd) pre-installed in the rootfs. 10-100x faster than system grep/find for coding agents.
- **Health check** — `GET /health` returns status, sandbox count, and uptime without auth. Use for deployment probes.
- **Graceful shutdown** — `SIGTERM` drains HTTP connections, stops background goroutines, then cleans up VMs and TAP devices.
- **Config drive** — hostname, env vars, secret files, DNS, volumes, init scripts — all injected at boot.
- **Auth tokens** — each VM gets a unique token. Both control and forward channels require auth.
- **Volumes** — ext4 images attached as additional drives. Persist across snapshot/resume.
- **Init scripts** — run setup commands at boot as session ID `"init"`. Attachable for monitoring.
- **Bridge networking** — shared bridge, single masquerade rule. VMs can reach the internet and each other.

## Project structure

```
cmd/
  bhatti/
    main.go           daemon entrypoint (bhatti serve, graceful shutdown)
    cli.go            CLI commands (create, exec, shell, file, ...)
    recovery_test.go  daemon recovery tests (8 tests, no VMs needed)
  lohar/
    handler.go        protocol dispatch (exec, sessions, files, activity)
    files.go          file read/write/stat/ls (atomic writes, server-side truncation)
    tty.go            TTY sessions, scrollback, detach/reattach
    session.go        session registry, ring buffer, idle timers
    exec.go           non-TTY piped exec (process group kill on abort)
    forward.go        port forwarding relay
    main.go           PID 1 init: mounts, config drive, networking
pkg/
  agent/
    client.go         host-side client (context-aware dial, file read abort)
    proto/            wire protocol (frame types, messages, benchmarks)
  engine/
    engine.go         sandbox lifecycle + StreamExecEngine interface
    firecracker/
      engine.go       Firecracker implementation (diff snapshots, per-VM mutex)
      network.go      bridge, TAP, IP pool
      configdrive.go  config drive + volume creation
      perf_test.go    percentile-based performance workload tests
  server/
    server.go         HTTP server, thermal manager (host-side activity cache)
    routes.go         REST/WS handlers, NDJSON streaming, file truncation
    proxy.go          reverse proxy through VM tunnels
  store/
    store.go          SQLite (sandboxes, secrets, templates, FC state)
deploy/
  bhatti.service      systemd unit file
scripts/
  install.sh          full install from source (Go, Firecracker, rootfs, systemd)
  build-rootfs.sh     build base rootfs (Ubuntu 24.04 + Node + rg + fd)
```

## Testing

289 tests across four layers, all on real Firecracker VMs. Zero mocks for VM tests.

```bash
# Agent-level tests (protocol handlers, no VM needed)
sudo go test -v -timeout=120s ./cmd/lohar/

# Integration tests (real Firecracker VMs)
sudo go test -v -timeout=600s ./pkg/engine/firecracker/

# Recovery + CLI tests
go test -v -timeout=30s ./cmd/bhatti/

# Cross-compile and run on remote Pi:
GOOS=linux GOARCH=arm64 go test -c -o bin/fc-test ./pkg/engine/firecracker
scp bin/fc-test pi:/tmp/ && ssh pi "sudo /tmp/fc-test -test.v"
```

Test coverage includes:
- VM lifecycle: create, exec, shell, snapshot/resume, destroy
- Diff snapshots: full→diff→diff, data accumulation across cycles
- Streaming exec: NDJSON events, incremental delivery, exit codes
- File truncation: offset, limit, max_bytes, backward compat
- File operations: read, write, stat, ls, zero-byte, binary data, unicode filenames, permissions, atomic writes, concurrent access
- Process group kill: piped exec child termination, TTY graceful shutdown
- Proxy routes: HTTP GET/404/headers, invalid port, sandbox not found
- Daemon recovery: stopped/running/destroyed/non-FC sandboxes, type coercion
- CLI: create, list, exec, destroy, file ops, secrets
- Networking: bridge, cross-VM, IP reuse, TAP cleanup, post-resume
- Config drive: env vars, hostname, DNS, file injection, volumes
- Auth: token validation, WS auth (query param + bearer header + rejection)
- Sessions: create/detach/reattach, scrollback, kill, idle timer
- Thermal: pause/resume, ensureHot from warm/cold, activity tracking
- Performance: p50/p95/p99 for exec, file ops, streaming, concurrent workloads
- Store: FC state round-trip, update, defaults
- Benchmarks: frame write/read throughput, ring buffer, JSON encoding

## Requirements

- Linux (aarch64 or x86_64)
- KVM (`/dev/kvm`)
- Root access (for Firecracker, TAP devices, bridge networking)

Tested on:
- Raspberry Pi 5 (aarch64, Ubuntu, NVMe)
- AWS Graviton bare metal (aarch64)

## License

Private project.
