# Bhatti — Architecture

## Naming

**Bhatti** (भट्टी) means furnace — the system that manages fire, provides
the environment where work happens.

**Lohar** (लोहार) means blacksmith — the one who works inside the bhatti.
The guest agent that runs as PID 1 inside every microVM.

```
bhatti    — the daemon + CLI. Orchestrates sandboxes, exposes the API.
lohar     — the guest agent. Runs inside each sandbox as PID 1.
sandbox   — a Firecracker microVM (or Docker container on macOS).
```

---

## System Diagram

```
┌───────────────────────────────────────────────────────────────────┐
│  Host  (Pi 5 / arm64 or x86_64 Linux / any KVM-capable host)     │
│                                                                   │
│  ┌─────────────────────────────────────────────────────────────┐  │
│  │  bhatti daemon  (bhatti serve)                              │  │
│  │                                                             │  │
│  │  ┌──────────┐  ┌───────────────┐  ┌──────────────────────┐  │  │
│  │  │ REST/WS  │  │ Engine        │  │ Store (SQLite)       │  │  │
│  │  │ API      │  │               │  │ sandboxes, secrets,  │  │  │
│  │  │ :8080    │──│ Create/Destroy│  │ templates, volumes,  │  │  │
│  │  │          │  │ Exec/Shell    │  │ FC state             │  │  │
│  │  │ Proxy    │  │ File ops      │  └──────────────────────┘  │  │
│  │  │ Thermal  │  │ Pause/Resume  │                            │  │
│  │  │ Manager  │  │ Snapshot/     │                            │  │
│  │  │          │  │   Restore     │                            │  │
│  │  └──────────┘  └──────┬────────┘                            │  │
│  │                        │ implements                         │  │
│  │             ┌──────────┴──────────┐                         │  │
│  │    ┌────────▼──────┐  ┌───────────▼──────────┐              │  │
│  │    │ Docker Engine │  │ Firecracker Engine   │              │  │
│  │    │ (macOS dev)   │  │ (Linux production)   │              │  │
│  │    └───────────────┘  └───────────┬──────────┘              │  │
│  └────────────────────────────────────┼────────────────────────┘  │
│                                       │ TCP over TAP              │
│  ┌────────────────────────────────────┼────────────────────────┐  │
│  │  Sandbox (Firecracker microVM)     │                        │  │
│  │  ┌──────────────────────────────┐  │                        │  │
│  │  │  vmlinux kernel              │  │                        │  │
│  │  │  rootfs.ext4    config.ext4  │  │                        │  │
│  │  │  vol-*.ext4 (volumes)        │  │                        │  │
│  │  │                              │  │                        │  │
│  │  │  ┌────────────────────────┐  │  │                        │  │
│  │  │  │  lohar (PID 1)         │◄─┤──┘                        │  │
│  │  │  │  TCP :1024 (control)   │  │                           │  │
│  │  │  │  TCP :1025 (forward)   │  │                           │  │
│  │  │  │  session registry      │  │                           │  │
│  │  │  │  file handlers         │  │                           │  │
│  │  │  │  scrollback buffers    │  │                           │  │
│  │  │  └────────────────────────┘  │                           │  │
│  │  │  user: lohar  /workspace     │                           │  │
│  │  └──────────────────────────────┘                           │  │
│  │  tapXXXXXXXX ─── brbhatti0 (bridge) ─── iptables NAT        │  │
│  └─────────────────────────────────────────────────────────────┘  │
└───────────────────────────────────────────────────────────────────┘
```

---

## Sandbox Lifecycle

### Consumer's view

Consumers see two operations: create and destroy. Everything between is
bhatti's job. A sandbox is always `"running"` from the API's perspective.

```
Create ──► sandbox exists (always "running") ──► Destroy
                    │         ▲
                    exec, file, tunnel, session
                    (always works — ensureHot is transparent)
```

### Thermal states (internal)

Bhatti manages three thermal states invisibly:

```
Hot ◄──~400µs──► Warm ◄──~50ms──► Cold
 ▲                                   │
 └──────── any operation ────────────┘
```

| State | FC process | vCPUs | Host RAM | Resume | When |
|---|---|---|---|---|---|
| Hot | alive | running | allocated | — | active use |
| Warm | alive | paused | allocated | ~400µs | idle <30s |
| Cold | dead | — | freed | ~50ms | idle >30min |

Transitions:
- **Hot → Warm**: no attached sessions + idle N seconds. `PATCH /vm {"state":"Paused"}`
- **Warm → Hot**: any operation. `PATCH /vm {"state":"Resumed"}`
- **Warm → Cold**: paused too long. Snapshot to disk (diff if base exists), kill FC process.
- **Cold → Hot**: any operation. New FC process, load snapshot.

`ensureHot()` is called before every operation that needs the VM. It also
updates the host-side activity cache so the thermal manager knows this
sandbox was recently accessed without querying the guest agent.

Metadata queries (status, list, health) don't wake the VM.

### Thermal manager optimization

The thermal cycle runs every 10 seconds. For each sandbox, it checks
a host-side `sync.Map` of last API activity timestamps. If the sandbox
had activity within the warm timeout, the guest agent query is skipped
entirely. This avoids opening a TCP connection per sandbox per cycle.

---

## Engine Interface

The actual Go interface (`pkg/engine/engine.go`):

```go
type Engine interface {
    Create(ctx, spec)           → SandboxInfo, error
    Destroy(ctx, id)            → error
    Stop(ctx, id)               → error           // snapshot (hot → cold)
    Start(ctx, id)              → error           // restore (cold → hot)
    Status(ctx, id)             → SandboxInfo, error
    List(ctx)                   → []SandboxInfo, error
    Exec(ctx, id, cmd)          → ExecResult, error
    Shell(ctx, id)              → TerminalConn, error
    ListeningPorts(ctx, id)     → []int, error
    Tunnel(ctx, id, port)       → ReadWriteCloser, error
}

// Optional: streaming exec for NDJSON endpoint
type StreamExecEngine interface {
    ExecStream(ctx, id, cmd, onEvent) → error
}
```

The Firecracker engine additionally implements:

```go
// Thermal management
Pause(ctx, id)               → error           // hot → warm
Resume(ctx, id)              → error           // warm → hot
EnsureHot(ctx, id)           → error           // any → hot
ThermalState(id)             → string
Activity(ctx, id)            → ActivityInfo, error

// File operations (with server-side truncation)
FileRead(ctx, id, path, w, opts...)  → int64, string, error
FileWrite(ctx, id, path, mode, size, r) → error
FileStat(ctx, id, path)      → FileInfo, error
FileList(ctx, id, path)      → []FileInfo, error

// Streaming exec
ExecStream(ctx, id, cmd, onEvent) → error

// Sessions
SessionList(ctx, id)         → []SessionInfo, error

// State persistence
VMState(id)                  → map[string]interface{}
RestoreVM(id, name, status, state)
```

### Concurrency model

Each VM has a `stateMu sync.Mutex` that protects all mutable fields. The
engine-level `sync.RWMutex` protects only the VM map — not individual state.

- **Short operations** (Exec, FileRead, Pause, etc.): capture the Agent
  reference under lock, release, then call the agent.
- **Long-lived operations** (Shell, Tunnel): same capture-and-release
  pattern. The Agent pointer is safe after release because it's only
  replaced during `Start()`, which holds the lock.

---

## Wire Protocol

Binary framing over TCP (or vsock). All host↔lohar communication.

```
┌────────────────┬───────────┬──────────────────────┐
│ Length (4B BE) │ Type (1B) │ Payload (N bytes)    │
└────────────────┴───────────┴──────────────────────┘
Length = 1 + len(Payload).  Max frame: 1MB.
```

Frame types:

```
I/O:      STDIN (0x01)  STDOUT (0x02)  STDERR (0x03)
Control:  RESIZE (0x04) EXIT (0x05)    ERROR (0x06)   KILL (0x07)
Exec:     EXEC_REQ (0x10)
Auth:     AUTH (0x11)
Forward:  FWD_REQ (0x20)  FWD_RESP (0x21)
Sessions: EXEC_LIST_REQ (0x30)  EXEC_LIST_RESP (0x31)
          EXEC_KILL (0x32)      SESSION_INFO (0x33)
Activity: ACTIVITY_REQ (0x40)   ACTIVITY_RESP (0x41)
Files:    FILE_READ_REQ (0x50)  FILE_READ_RESP (0x51)
          FILE_WRITE_REQ (0x52) FILE_WRITE_RESP (0x53)
          FILE_STAT_REQ (0x54)  FILE_STAT_RESP (0x55)
          FILE_LS_REQ (0x56)    FILE_LS_RESP (0x57)
```

Ports:
- **1024** — control (exec, sessions, files, activity)
- **1025** — forward (port tunneling)

Connection model:
- Control: one connection per operation. AUTH first (if configured), then
  one request frame. Connection closes when the operation completes.
  Exception: attached TTY sessions keep the connection open.
- Forward: one connection per tunnel. FWD_REQ → FWD_RESP, then unframed
  bidirectional TCP relay.
- All dials are context-aware with timeouts (5s dial, 10s FC API).

### File operation protocol

**Read**: `FILE_READ_REQ` → `FILE_READ_RESP` (size, mode) → `STDOUT` frames → `EXIT`.
Supports server-side truncation via `offset` (1-indexed line), `limit` (max lines),
and `max_bytes` (byte budget) — whichever limit hits first stops the read.
Without these parameters, streams the full file (backward compatible).
Rejects directories and non-regular files. Cancellable via context (closes
connection, lohar gets broken pipe).

**Write**: `FILE_WRITE_REQ` (path, mode, size) → `STDIN` frames → `FILE_WRITE_RESP`.
Atomic: writes to temp file, then renames. Readers never see partial content.
Rejects negative sizes (prevents silent data loss from missing Content-Length).

**Stat**: `FILE_STAT_REQ` → `FILE_STAT_RESP` (name, size, mode, is_dir, mtime).

**List**: `FILE_LS_REQ` → `FILE_LS_RESP` (JSON array of FileInfo).
Capped at 10,000 entries. Validates target is a directory.

### Kill semantics

Kill behavior depends on session type:
- **Piped exec** (non-TTY): `SIGKILL` to process group. Immediate, reliable.
  Uses `Setpgid: true` so child processes are in the same group.
- **TTY sessions**: `SIGTERM` to process group. Allows graceful shutdown,
  preserves the session model (sessions survive disconnects).
- **EXEC_KILL API**: `SIGKILL` to process group. Explicit force-kill.
- **Idle timer**: `SIGKILL` to process group. Session is abandoned.

---

## Snapshots

### Full vs Diff

The first `Stop()` creates a **Full** snapshot (all memory pages). Subsequent
`Stop()` calls create **Diff** snapshots (only dirty pages since the last
snapshot). This requires `track_dirty_pages: true` in the FC machine config
and `enable_diff_snapshots: true` on snapshot load.

On Pi 5 with NVMe: full snapshot = ~4.4s (512MB), diff = ~52ms.

If the base snapshot file is missing (deleted, corrupted), Stop() falls
back to a Full snapshot automatically with a warning log.

### Daemon recovery

On startup, `recoverVMs()` reads all sandboxes from SQLite and restores
Firecracker VMs to the engine's in-memory map:

- **Stopped + snapshot exists**: restored as "stopped" (resumable)
- **Stopped + snapshot missing**: marked "unknown"
- **Running + snapshot exists**: marked "stopped" (FC process is dead)
- **Running + no snapshot**: marked "unknown" (unrecoverable)
- **Destroyed / Docker**: skipped

State extraction uses type-safe helpers (`stateStr`, `stateInt64`,
`stateUint32`, `stateBool`) that handle both JSON `float64` and SQLite
`int` values without panicking.

---

## API Surface

```
GET    /health                                 health check (no auth)

POST   /sandboxes                              create (template or direct)
GET    /sandboxes                              list
GET    /sandboxes/:id                          get
DELETE /sandboxes/:id                          destroy
POST   /sandboxes/:id/stop                     snapshot to disk
POST   /sandboxes/:id/start                    resume from snapshot
POST   /sandboxes/:id/exec                     exec (buffered JSON or streaming NDJSON)
GET    /sandboxes/:id/ws                       WebSocket shell
GET    /sandboxes/:id/sessions                 list sessions
GET    /sandboxes/:id/ports                    listening ports
GET    /sandboxes/:id/files?path=...           read file (&offset=&limit=&max_bytes=)
GET    /sandboxes/:id/files?path=...&ls=true   list directory
PUT    /sandboxes/:id/files?path=...           write file (Content-Length required)
HEAD   /sandboxes/:id/files?path=...           stat file
ANY    /sandboxes/:id/proxy/:port/*            reverse proxy (HTTP + WebSocket)

POST   /templates                              create template
GET    /templates                              list templates
GET    /templates/:id                          get template
DELETE /templates/:id                          delete template

POST   /secrets                                create/update secret
GET    /secrets                                list (names only)
DELETE /secrets/:name                          delete

POST   /volumes                                create volume
GET    /volumes                                list volumes
GET    /volumes/:name                          get volume
DELETE /volumes/:name                          delete volume

GET    /ports                                  all listening ports across sandboxes
```

### Streaming exec

`POST /sandboxes/:id/exec` with `Accept: application/x-ndjson` streams
output as newline-delimited JSON. Each event is flushed immediately:

```json
{"type":"stdout","data":"output...\n"}
{"type":"stderr","data":"warning...\n"}
{"type":"exit","exit_code":0}
```

Without the `Accept` header, the existing buffered JSON response is returned.
The Firecracker engine implements `StreamExecEngine` natively (forwards
agent frames as events). The Docker engine falls back to buffering then
emitting as NDJSON events.

---

## CLI

Same binary as daemon. `bhatti serve` starts daemon, everything else is CLI.

```
bhatti serve                        start daemon

bhatti create [--name N] [--cpus C] [--memory M] [--env K=V,K=V] [--init CMD]
bhatti list | ls                    list sandboxes
bhatti destroy | rm <id|name>       destroy sandbox

bhatti exec <id|name> -- CMD...     run command (streaming output)
bhatti shell | sh <id|name>         interactive shell (Ctrl+\ to detach)
bhatti ps <id|name>                 list sessions

bhatti file read <id|name> PATH     read file to stdout
bhatti file write <id|name> PATH    write file from stdin
bhatti file ls <id|name> PATH       list directory

bhatti secret set NAME VALUE
bhatti secret list
bhatti secret delete NAME
```

Name-to-ID resolution: all commands accept sandbox name or ID.
Config: `BHATTI_URL`, `BHATTI_TOKEN` env vars, or `~/.bhatti/config.yaml`.

---

## Disk Layout

```
/var/lib/bhatti/
├── config.yaml                   daemon config
├── state.db                      SQLite (sandboxes, templates, secrets, FC state)
├── age.key                       secret encryption key
├── id_ed25519 / .pub             SSH keypair
├── lohar                         guest agent binary
├── images/
│   ├── vmlinux-arm64             kernel (or vmlinux-amd64)
│   └── rootfs-base-arm64.ext4   base rootfs (Ubuntu 24.04 + Node + rg + fd)
└── sandboxes/
    └── <id>/
        ├── rootfs.ext4           CoW copy of base rootfs
        ├── config.ext4           config drive (env, files, volumes, auth)
        ├── vol-<name>.ext4       volumes (if any)
        ├── firecracker.sock      FC API socket
        ├── vsock.sock            vsock UDS
        ├── mem.snap              memory snapshot (when cold)
        └── vm.snap               VM state snapshot (when cold)
```

---

## Key Design Decisions

**TCP over TAP for post-snapshot.** Vsock is broken after Firecracker
snapshot/restore. TCP over virtio-net works. Lohar listens on both;
after resume, the TCP client is used.

**No FC Go SDK.** Direct HTTP to FC's Unix socket API. ~20 lines of
helpers replace thousands of SDK lines. `DisableKeepAlives: true` prevents
connection pile-up on the Unix socket under rapid pause/resume cycles.

**No systemd in guest.** Lohar IS init. Mounts, networking, PTYs,
processes — all deterministic. Boot to ready in ~3.5s.

**Guest IP via kernel `ip=`.** Network up before init runs. No DHCP.

**Pure Go SQLite.** `modernc.org/sqlite`. Cross-compile with CGO_ENABLED=0.

**Exec IS sessions.** No separate concept. Every exec gets a session ID.
TTY execs survive disconnect. Scrollback on reattach.

**Three-tier thermals.** Hot (running), Warm (paused, ~400µs resume),
Cold (snapshotted, ~50ms resume). Consumer never sees this. Host-side
activity cache avoids per-sandbox TCP queries on every thermal cycle.

**Diff snapshots.** First snapshot is Full, subsequent are Diff (dirty
pages only). Reduces snapshot time from ~4.4s to ~52ms. Falls back to
Full if base snapshot is missing.

**Atomic file writes.** Write to temp file, fsync, rename. Concurrent
readers always see complete content (old or new, never partial).

**Server-side file truncation.** Agents always truncate (2000 lines / 50KB).
Doing it guest-side via `offset`/`limit`/`max_bytes` avoids transferring
megabytes through the wire protocol. 4.5x speedup on 10K-line files.

**Content-negotiated streaming.** `Accept: application/x-ndjson` on the
exec endpoint streams output as it arrives. No WebSocket required for the
95% case (fire a command, stream output, get exit code). Falls back to
buffered JSON without the header.

**Process group kill.** Piped exec uses `Setpgid: true` + `SIGKILL` to
the process group. TTY sessions use `SIGTERM` (graceful). This matches
how coding agents abort commands (`kill(-pid, SIGKILL)`) while preserving
the session model for interactive use.

**Per-VM mutex.** Each VM has a `stateMu` that protects mutable fields.
The engine-level lock protects only the VM map. Short ops capture the
Agent reference under lock, release, then call. Long ops (Shell, Tunnel)
use the same capture-and-release pattern.

**Context-aware connections.** All agent dials accept `context.Context`.
Firecracker API client has 10s timeout with `DisableKeepAlives`. Thermal
manager wraps each agent query in a 5s per-sandbox timeout.

**Structured logging.** `log/slog` (stdlib). Text for development, JSON
for production. Zero external dependencies.

**Secrets via age + config drive.** Encrypted at rest, decrypted at
sandbox creation, injected as files or env vars.

**Single binary.** `bhatti serve` = daemon, `bhatti create` = CLI.
No separate CLI tool to install or version.

**Graceful shutdown.** `http.Server.Shutdown()` drains connections on
SIGTERM/SIGINT, then stops background goroutines and cleans up VMs/TAPs.
