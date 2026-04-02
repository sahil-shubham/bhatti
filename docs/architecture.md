# Architecture

Bhatti has two binaries. **bhatti** runs on the host — it's the daemon, the CLI, the HTTP server, the thermal manager, and the engine that talks to Firecracker. **lohar** runs inside every microVM as PID 1 — it handles exec, file operations, PTY sessions, and port forwarding.

They communicate over TCP using a [binary framing protocol](wire-protocol.md).

```
┌───────────────────────────────────────────────────────────────────┐
│  Host  (Pi 5 / Graviton / x86_64 bare metal)                     │
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
│  │  │  vol-*.ext4 (optional)       │  │                        │  │
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

## The Engine Interface

The Firecracker engine and Docker engine implement the same Go interface. Docker exists as a development fallback on macOS (no KVM). The Firecracker engine is the production path.

```go
type Engine interface {
    Create(ctx, spec)        → SandboxInfo, error
    Destroy(ctx, id)         → error
    Stop(ctx, id)            → error           // snapshot to disk
    Start(ctx, id)           → error           // restore from snapshot
    Status(ctx, id)          → SandboxInfo, error
    List(ctx)                → []SandboxInfo, error
    Exec(ctx, id, cmd)       → ExecResult, error
    Shell(ctx, id)           → TerminalConn, error
    ListeningPorts(ctx, id)  → []int, error
    Tunnel(ctx, id, port)    → ReadWriteCloser, error
}
```

The Firecracker engine extends this with thermal management (`Pause`, `Resume`, `EnsureHot`), file operations (`FileRead`, `FileWrite`, `FileStat`, `FileList`), streaming exec (`ExecStream`), sessions (`SessionList`), and state persistence (`VMState`, `RestoreVM`). These are discovered at runtime via interface assertions — the server checks `if fe, ok := s.engine.(FileEngine)` before calling file methods.

## Sandbox Lifecycle

### What the consumer sees

Two operations: create and destroy. Everything between is bhatti's job.

```
Create ──► sandbox exists (always "running" from API perspective) ──► Destroy
                    │         ▲
                    exec, file, shell, tunnel
                    (always works — ensureHot is transparent)
```

### What actually happens

Behind the API, each VM moves through three thermal states:

```
Hot ◄──~400µs──► Warm ◄──~50ms──► Cold
 ▲                                   │
 └──────── any API request ──────────┘
```

| State | Firecracker process | vCPUs | Host RAM | Resume latency |
|-------|-------------------|-------|----------|----------------|
| **Hot** | alive | running | allocated | — |
| **Warm** | alive | paused | allocated | ~400µs |
| **Cold** | dead | — | freed | ~50ms |

Transitions happen automatically. Idle 30 seconds → warm. Idle 30 minutes → cold. Any API request calls `ensureHot()` which transparently restores the VM before executing the operation. Metadata queries (list, status, health) don't wake VMs.

See [Thermal Management](thermal-management.md) for the full design.

## Concurrency Model

Each VM has a `stateMu sync.Mutex` that protects its mutable fields. The engine-level `sync.RWMutex` protects only the VM map — not individual VM state.

The lock discipline has two patterns:

**Short operations** (Exec, FileRead, Pause, Resume): hold `stateMu`, validate state, capture the `Agent` reference, release the lock, then call the agent. This prevents one slow exec from blocking the thermal manager from pausing other VMs.

**Long-lived operations** (Shell, Tunnel): same capture-and-release pattern. The `Agent` pointer is safe to use after release because it's only replaced during `Start()`, which holds `stateMu`.

`EnsureHot` reads the thermal state under lock, releases, then delegates to `Resume` or `Start` — which acquire their own lock. No nested locking.

## Data Flow: Exec

Here's the complete path of `bhatti exec dev -- echo hello`:

```
CLI                     HTTP Server              Engine                  Agent Client            Lohar (guest)
 │                          │                       │                       │                       │
 ├─POST /sandboxes/         │                       │                       │                       │
 │  {id}/exec ──────────────►                       │                       │                       │
 │  {cmd:["echo","hello"]}  │                       │                       │                       │
 │                          ├─ensureHot(id)────────►│                       │                       │
 │                          │                       ├─check thermal state   │                       │
 │                          │                       │ (warm? resume first)  │                       │
 │                          │                       │                       │                       │
 │                          ├─Exec(id,cmd)─────────►│                       │                       │
 │                          │                       ├─getVM(id)             │                       │
 │                          │                       ├─capture Agent ref     │                       │
 │                          │                       ├─release lock          │                       │
 │                          │                       ├─agent.Exec()─────────►│                       │
 │                          │                       │                       ├─TCP dial :1024────────►│
 │                          │                       │                       ├─AUTH frame────────────►│
 │                          │                       │                       ├─EXEC_REQ frame────────►│
 │                          │                       │                       │                       ├─fork/exec
 │                          │                       │                       │                       ├─"echo hello"
 │                          │                       │                       │◄──STDOUT "hello\n"─────┤
 │                          │                       │                       │◄──EXIT code=0──────────┤
 │                          │                       │◄──ExecResult──────────┤                       │
 │                          │◄──ExecResult──────────┤                       │                       │
 │◄──JSON {exit_code:0,     │                       │                       │                       │
 │    stdout:"hello\n"} ────┤                       │                       │                       │
```

## Data Flow: Streaming Exec

With `Accept: application/x-ndjson`, exec output streams in real time:

```
Client                 Server                   Engine                  Lohar
 │                       │                       │                       │
 ├─POST exec ───────────►│                       │                       │
 │  Accept: x-ndjson     ├─ExecStream()─────────►│                       │
 │                       │                       ├─dial + EXEC_REQ──────►│
 │                       │                       │                       ├─fork/exec
 │                       │                       │◄──STDOUT frame────────┤
 │                       │◄──onEvent(stdout)─────┤                       │
 │◄──{"type":"stdout"}───┤ (flush)               │                       │
 │                       │                       │◄──STDERR frame────────┤
 │◄──{"type":"stderr"}───┤ (flush)               │                       │
 │                       │                       │◄──EXIT frame──────────┤
 │◄──{"type":"exit"}─────┤ (flush)               │                       │
```

Each NDJSON line is flushed immediately. The Firecracker engine implements `StreamExecEngine` natively — it reads agent frames and emits events as they arrive. The Docker engine falls back to buffering then emitting.

## Disk Layout

```
/var/lib/bhatti/
├── config.yaml                   daemon config (engine, listen, auth, paths)
├── state.db                      SQLite (WAL mode, sandboxes/templates/secrets/FC state)
├── age.key                       secret encryption key (age)
├── id_ed25519 / .pub             SSH keypair
├── lohar                         guest agent binary (baked into rootfs)
├── images/
│   ├── vmlinux-arm64             kernel (or vmlinux-amd64)
│   └── rootfs-minimal-arm64.ext4 minimal rootfs (Ubuntu 24.04)
└── sandboxes/
    └── <id>/
        ├── rootfs.ext4           CoW copy of base rootfs
        ├── config.ext4           config drive (1MB ext4: env, files, volumes, auth)
        ├── vol-<name>.ext4       attached volumes (if any)
        ├── firecracker.sock      FC API Unix socket
        ├── vsock.sock            vsock UDS (unused post-snapshot, kept for cold boot)
        ├── mem.snap              memory snapshot (when cold)
        └── vm.snap               VM state snapshot (when cold)
```

## Project Structure

```
cmd/
  bhatti/
    main.go             daemon + CLI entrypoint, VM recovery, graceful shutdown
    cli.go              CLI commands (create, exec, shell, file, secret, ...)
    engine_linux.go     Firecracker engine constructor (Linux only)
    engine_other.go     stub for macOS (returns "not supported" error)
    recovery_test.go    8 daemon recovery tests (no VMs needed)
    cli_test.go         11 CLI integration tests (real Firecracker)
  lohar/
    main.go             PID 1 init: mounts, config drive, networking, listeners
    handler.go          protocol dispatch: exec, sessions, files, activity
    tty.go              PTY allocation, sessions, scrollback, detach/reattach
    session.go          session registry, ring buffer, idle timers
    exec.go             piped (non-TTY) exec with process group kill
    files.go            file read/write/stat/ls with atomic writes + truncation
    forward.go          port forwarding relay
    net.go              vsock listener, interface bring-up
    testmode.go         Unix socket listener for testing without VMs
    agent_test.go       40+ agent-level tests (protocol handlers, no VM needed)
pkg/
  agent/
    client.go           host-side client: Exec, Shell, Forward, FileRead/Write, ...
    client_test.go      client tests against agent in test mode
    proto/
      constants.go      frame type bytes, vsock ports, max frame size
      messages.go       ExecRequest, SessionInfo, FileInfo, etc.
      frame.go          WriteFrame, ReadFrame, TryParse, SendJSON
      frame_test.go     round-trip, EOF handling, max size, concurrent writes
      frame_bench_test.go  throughput benchmarks
  engine/
    engine.go           Engine interface + StreamExecEngine, VMStateProvider
    firecracker/
      engine.go         full implementation: Create through FileList
      network.go        bridge, TAP, IP pool, iptables, orphan cleanup
      configdrive.go    config drive creation, volume formatting
      *_test.go         integration tests on real Firecracker VMs
    docker/
      docker.go         Docker implementation (macOS development)
  server/
    server.go           HTTP server, thermal manager, activity cache
    routes.go           REST/WS handlers, NDJSON streaming, file ops
    proxy.go            TCP port forwarding through engine tunnels
  store/
    store.go            SQLite: sandboxes, templates, secrets, FC state, volumes
  config.go             YAML config loading, SSH keypair generation
  secrets/
    age.go              age encryption for secrets at rest
```

## Recovery

On startup, `recoverVMs()` reads all sandboxes from SQLite and restores them to the engine's in-memory map:

- **Stopped + snapshot exists** → restored as "stopped" (resumable on next API call)
- **Stopped + snapshot missing** → marked "unknown" (unrecoverable)
- **Running + snapshot exists** → marked "stopped" (FC process died, but snapshot is valid)
- **Running + no snapshot** → marked "unknown" (unrecoverable)
- **Destroyed / Docker sandboxes** → skipped

State extraction uses type-safe helpers (`stateStr`, `stateInt64`, `stateUint32`, `stateBool`) that handle both JSON `float64` and SQLite `int` values — the state map passes through JSON serialization where all numbers become `float64`, and through SQLite where they're native integers. Without these helpers, a `map[string]interface{}` type assertion on `int` vs `float64` would panic.

Orphaned TAP devices from previous crashes are cleaned up on engine startup before any VMs are loaded.
