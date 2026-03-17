# Bhatti — Architecture

## Naming

**Bhatti** (भट्टी) means furnace — the system that manages fire, provides
the environment where work happens.

**Lohar** (लोहार) means blacksmith — the one who works inside the bhatti.
The guest agent that runs as PID 1 inside every microVM.

```
bhatti    — the daemon/CLI. Orchestrates sandboxes, exposes the API.
lohar     — the guest agent. Runs inside each sandbox as PID 1.
sandbox   — a Firecracker microVM (or Docker container on macOS).
```

---

## System Diagram

```
┌───────────────────────────────────────────────────────────────────┐
│  Host  (Pi 5 / arm64 Linux / any KVM-capable host)                │
│                                                                   │
│  ┌─────────────────────────────────────────────────────────────┐  │
│  │  bhatti daemon                                              │  │
│  │                                                             │  │
│  │  ┌──────────┐  ┌───────────────┐  ┌──────────────────────┐  │  │
│  │  │ REST/WS  │  │ Engine        │  │ Store (SQLite)       │  │  │
│  │  │ API      │  │               │  │ sandboxes, secrets,  │  │  │
│  │  │ :8080    │──│ Create/Destroy│  │ templates, images,   │  │  │
│  │  │          │  │ Exec/Tunnel   │  │ users, FC state      │  │  │
│  │  │ Proxy    │  │ Pause/Resume  │  └──────────────────────┘  │  │
│  │  │ Thermal  │  │ Snapshot/     │                            │  │
│  │  │ Manager  │  │   Restore     │                            │  │
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
                    (always works)
```

### Thermal states (internal)

Bhatti manages three thermal states invisibly:

```
Hot ◄──~1ms──► Warm ◄──~500ms──► Cold
 ▲                                  │
 └──────── any operation ───────────┘
```

| State | FC process | vCPUs | Host RAM | Resume | When |
|---|---|---|---|---|---|
| Hot | alive | running | allocated | — | active use |
| Warm | alive | paused | allocated | ~1ms | idle <30min |
| Cold | dead | — | freed | ~500ms | idle >30min |

Transitions:
- **Hot → Warm**: no activity for N seconds. `PATCH /vm {"state":"Paused"}`
- **Warm → Hot**: any operation. `PATCH /vm {"state":"Resumed"}`
- **Warm → Cold**: paused too long. Snapshot to disk, kill FC process.
- **Cold → Hot**: any operation. New FC process, load snapshot (mmap).

`ensureHot()` is called before every operation that needs the VM.
Metadata queries (status, list) don't wake the VM.

---

## Engine Interface

```go
type Engine interface {
    // Consumer-facing
    Create(ctx, spec)          → SandboxInfo, error
    Destroy(ctx, id)           → error

    // Exec (session-aware — every exec is a session)
    Exec(ctx, id, opts)        → ExecResult, error       // non-TTY one-shot
    ExecStream(ctx, id, opts)  → TerminalConn, error     // TTY/streaming
    ExecList(ctx, id)          → []SessionInfo, error
    ExecKill(ctx, id, sid)     → error

    // I/O
    Tunnel(ctx, id, port)      → ReadWriteCloser, error
    ListeningPorts(ctx, id)    → []int, error
    FileRead(ctx, id, path)    → io.ReadCloser, FileInfo, error
    FileWrite(ctx, id, path, mode, r) → error
    FileStat(ctx, id, path)    → FileInfo, error
    FileList(ctx, id, path)    → []FileInfo, error

    // Thermal management (called by bhatti internally)
    Pause(ctx, id)             → error
    Resume(ctx, id)            → error
    Snapshot(ctx, id)          → error
    Restore(ctx, id)           → error
    Activity(ctx, id)          → ActivityInfo, error

    // Metadata (don't require VM to be hot)
    Status(ctx, id)            → SandboxInfo, error
    List(ctx)                  → []SandboxInfo, error
}
```

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
Auth:     AUTH (0x11)
Exec:     EXEC_REQ (0x10)
Forward:  FWD_REQ (0x20)  FWD_RESP (0x21)
Sessions: EXEC_LIST_REQ (0x30)  EXEC_LIST_RESP (0x31)  EXEC_KILL (0x32)
          SESSION_INFO (0x33)
Activity: ACTIVITY_REQ (0x40)  ACTIVITY_RESP (0x41)
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
  Exception: attached sessions keep the connection open.
- Forward: one connection per tunnel. FWD_REQ → FWD_RESP, then unframed
  bidirectional TCP relay.

---

## API Surface

```
POST   /sandboxes                              create sandbox
GET    /sandboxes                              list sandboxes
GET    /sandboxes/:id                          get sandbox
DELETE /sandboxes/:id                          destroy sandbox

POST   /sandboxes/:id/exec                    non-TTY exec (HTTP, blocks)
WS     /sandboxes/:id/exec                    TTY/streaming exec (WebSocket)
         ?tty=true                              new TTY session
         ?id=SESSION_ID                         attach to session
GET    /sandboxes/:id/exec                    list sessions
DELETE /sandboxes/:id/exec/:session_id        kill session

GET    /sandboxes/:id/files?path=P            read file
PUT    /sandboxes/:id/files?path=P            write file
DELETE /sandboxes/:id/files?path=P            delete file
GET    /sandboxes/:id/files?path=P&ls=true    list directory
HEAD   /sandboxes/:id/files?path=P            stat file

GET    /sandboxes/:id/ports                   listening ports
ANY    /sandboxes/:id/proxy/:port/*           reverse proxy

POST   /secrets                                create/update secret
GET    /secrets                                list (names only)
DELETE /secrets/:name                          delete

GET    /templates                              list
POST   /templates                              create
DELETE /templates/:id                          delete

POST   /images/pull                            pull OCI image
GET    /images                                 list local images
DELETE /images/:ref                            delete

POST   /users                                  create user (admin)
GET    /users                                  list users (admin)
```

---

## CLI

```
bhatti serve                                    start daemon

bhatti sandbox create [flags] ...               create
bhatti sandbox list                             list
bhatti sandbox get ID                           get
bhatti sandbox destroy ID                       destroy
bhatti sandbox suspend ID                       force cold (operator)
bhatti sandbox resume ID                        force hot (operator)

bhatti exec ID [--tty] [--env K=V] -- CMD       run command
bhatti exec list ID                             list sessions
bhatti exec attach ID SID                       reattach
bhatti exec kill ID SID                         kill session
bhatti shell ID                                 exec --tty -- /bin/zsh -li

bhatti file read ID PATH                        read to stdout
bhatti file write ID PATH                       write from stdin
bhatti file ls ID [PATH]                        list directory

bhatti secret set NAME [--value V|--from-file F]
bhatti secret list
bhatti secret delete NAME

bhatti image pull REF
bhatti image list
bhatti image delete REF
```

---

## Disk Layout

```
/var/lib/bhatti/
├── config.yaml
├── state.db                      SQLite
├── age.key                       secret encryption key
├── id_ed25519 / .pub             SSH keypair
├── images/
│   ├── vmlinux-arm64             kernel
│   ├── rootfs-base-arm64.ext4    base rootfs
│   └── oci/                      pulled OCI images
└── sandboxes/
    └── <id>/
        ├── rootfs.ext4           sandbox rootfs
        ├── config.ext4           config drive (1MB)
        ├── vol-workspace.ext4    volume (if any)
        ├── firecracker.sock      FC API socket
        ├── vsock.sock            vsock UDS
        ├── mem.snap              memory snapshot (cold)
        └── vm.snap               VM state snapshot (cold)
```

---

## Key Design Decisions

**TCP over TAP for post-snapshot.** Vsock is broken after Firecracker
snapshot/restore (confirmed in Slicer too). TCP over virtio-net works.
Lohar listens on both.

**No FC Go SDK.** Direct HTTP to FC's Unix socket API. ~20 lines of
helpers replace thousands of SDK lines.

**No systemd in guest.** Lohar IS init. Mounts, networking, PTYs,
processes — all deterministic.

**Guest IP via kernel `ip=`.** Network up before init runs.

**Pure Go SQLite.** `modernc.org/sqlite`. Cross-compile with CGO_ENABLED=0.

**Exec IS sessions.** No separate concept. Every exec gets a session ID.
TTY execs survive disconnect. Scrollback on reattach.

**Three-tier thermals.** Hot (running), Warm (paused, ~1ms resume),
Cold (snapshotted, ~500ms resume). Consumer never sees this.

**Secrets via age + config drive.** Encrypted at rest, decrypted at
sandbox creation, injected as files or env vars.
