> [!WARNING]
> **DEPRECATED — do not edit.**
> The canonical, maintained version of this page is at
> <https://bhatti.sh/docs/under-the-hood/wire-protocol/>.
> This file is kept only for git history and may be removed in a future
> cleanup. See [`docs/README.md`](./README.md) for the redirect index.

---

# Wire Protocol

All communication between the bhatti host and a guest VM happens over a binary framing protocol. The same protocol runs over vsock (cold boot), TCP over TAP (post-snapshot), or Unix sockets (testing). The protocol is engine-independent — the entire agent test suite runs on macOS over `net.Pipe()` without any VM.

## Frame Format

```
┌────────────────┬───────────┬──────────────────────┐
│ Length (4B BE)  │ Type (1B) │ Payload (N bytes)    │
└────────────────┴───────────┴──────────────────────┘
```

**Length** is a 4-byte big-endian unsigned integer. It equals `1 + len(Payload)` — the type byte plus the payload. It does *not* include the 4-byte length prefix itself.

**Type** is a single byte identifying the frame kind.

**Payload** is variable-length, up to 1MB minus 1 byte.

**Maximum frame size**: 1MB (1,048,576 bytes). Both `WriteFrame` and `ReadFrame` enforce this — oversized frames are rejected, not truncated.

## Atomic Writes

`WriteFrame` assembles the entire frame (length + type + payload) into a single buffer and writes it in one `Write()` call. This prevents interleaved partial frames when multiple goroutines write concurrently — the agent's piped exec has stdout and stderr goroutines writing to the same connection simultaneously.

This is a necessity, not an optimization. Without it, two goroutines writing concurrent 8KB stdout/stderr chunks could interleave at any byte boundary, producing corrupt frames on the wire. The single-buffer approach pushes the atomicity guarantee down to the kernel's `write()` syscall, which is atomic for sizes under the pipe buffer limit on all supported platforms.

## Frame Types

### I/O Streams

| Type | Byte | Direction | Payload |
|------|------|-----------|---------|
| `STDIN` | `0x01` | host → guest | raw bytes for child's stdin |
| `STDOUT` | `0x02` | guest → host | child's stdout bytes |
| `STDERR` | `0x03` | guest → host | child's stderr bytes |

### Control

| Type | Byte | Direction | Payload |
|------|------|-----------|---------|
| `RESIZE` | `0x04` | host → guest | `[u16 rows BE][u16 cols BE]` — exactly 4 bytes |
| `EXIT` | `0x05` | guest → host | `[i32 exit_code BE]` — exactly 4 bytes |
| `ERROR` | `0x06` | either | UTF-8 error message (variable length) |
| `KILL` | `0x07` | host → guest | empty payload |

### Exec

| Type | Byte | Direction | Payload |
|------|------|-----------|---------|
| `EXEC_REQ` | `0x10` | host → guest | JSON-encoded `ExecRequest` |

### Auth

| Type | Byte | Direction | Payload |
|------|------|-----------|---------|
| `AUTH` | `0x11` | host → guest | raw token bytes |

### Port Forwarding

| Type | Byte | Direction | Payload |
|------|------|-----------|---------|
| `FWD_REQ` | `0x20` | host → guest | JSON `{"port": 8080}` |
| `FWD_RESP` | `0x21` | guest → host | JSON `{"status": "ok"}` or `{"status": "error", "message": "..."}` |

### Sessions

| Type | Byte | Direction | Payload |
|------|------|-----------|---------|
| `EXEC_LIST_REQ` | `0x30` | host → guest | empty |
| `EXEC_LIST_RESP` | `0x31` | guest → host | JSON `[]SessionInfo` |
| `EXEC_KILL` | `0x32` | host → guest | JSON `{"session_id": "s1"}` |
| `SESSION_INFO` | `0x33` | guest → host | JSON `SessionInfo` |

### Activity

| Type | Byte | Direction | Payload |
|------|------|-----------|---------|
| `ACTIVITY_REQ` | `0x40` | host → guest | empty |
| `ACTIVITY_RESP` | `0x41` | guest → host | JSON `ActivityInfo` |

### File Operations

| Type | Byte | Direction | Payload |
|------|------|-----------|---------|
| `FILE_READ_REQ` | `0x50` | host → guest | JSON `{"path": "/workspace/app.js", "offset": 1, "limit": 2000, "max_bytes": 51200}` |
| `FILE_READ_RESP` | `0x51` | guest → host | JSON `{"size": 1234, "mode": "0644"}` |
| `FILE_WRITE_REQ` | `0x52` | host → guest | JSON `{"path": "/workspace/app.js", "mode": "0644", "size": 1234}` |
| `FILE_WRITE_RESP` | `0x53` | guest → host | JSON `{"status": "ok"}` |
| `FILE_STAT_REQ` | `0x54` | host → guest | JSON `{"path": "..."}` |
| `FILE_STAT_RESP` | `0x55` | guest → host | JSON `FileInfo` |
| `FILE_LS_REQ` | `0x56` | host → guest | JSON `{"path": "..."}` |
| `FILE_LS_RESP` | `0x57` | guest → host | JSON `[]FileInfo` |

## Connection Model

Two TCP ports, two purposes:

- **Port 1024** (control) — exec, sessions, files, activity queries
- **Port 1025** (forward) — port forwarding / TCP tunneling

### Control Connection Lifecycle

One connection per operation. The host dials port 1024, optionally sends an `AUTH` frame, sends exactly one request frame, reads responses until the operation completes, then the connection closes.

```
Host                                  Lohar
 │                                      │
 ├──TCP connect :1024──────────────────►│
 ├──AUTH frame (if token configured)───►│
 ├──EXEC_REQ frame────────────────────►│
 │                                      ├──fork/exec child
 │◄──STDOUT frame──────────────────────┤
 │◄──STDOUT frame──────────────────────┤
 │◄──STDERR frame──────────────────────┤
 │◄──EXIT frame────────────────────────┤
 └──connection closed──────────────────┘
```

Exception: TTY sessions keep the connection open for bidirectional I/O. The host sends `STDIN` and `RESIZE` frames; the guest sends `STDOUT` frames and eventually an `EXIT` frame. If the host disconnects, the session detaches (process keeps running, scrollback buffer captures output).

### Forward Connection Lifecycle

One connection per tunnel. After the `FWD_REQ`/`FWD_RESP` handshake, the framing protocol is *abandoned* — the connection becomes a raw bidirectional TCP relay.

```
Host                                  Lohar                     Target (localhost:8080)
 │                                      │                          │
 ├──TCP connect :1025──────────────────►│                          │
 ├──AUTH frame─────────────────────────►│                          │
 ├──FWD_REQ {"port": 8080}────────────►│                          │
 │                                      ├──TCP connect :8080──────►│
 │◄──FWD_RESP {"status": "ok"}─────────┤                          │
 │                                      │                          │
 │══ raw bytes (no framing) ═══════════►│══════════════════════════►│
 │◄══════════════════════════════════════│◄══════════════════════════│
```

## Auth

If a token is configured (via the config drive at boot), the first frame on every connection must be `AUTH` with the token as payload. Lohar validates it within a 5-second deadline. Invalid or missing auth gets an `ERROR` frame and the connection is closed.

The token is generated per-sandbox during `Create()` — 16 random bytes, hex-encoded. It's injected into the VM via the config drive and stored in the host's `AgentClient`.

## File Read Protocol

File reads support server-side truncation to avoid transferring large files when the consumer only needs the first N lines.

```
FILE_READ_REQ {"path": "/app.log", "offset": 1, "limit": 2000, "max_bytes": 51200}
                    ↓
FILE_READ_RESP {"size": 10485760, "mode": "0644"}    ← total file size
                    ↓
STDOUT frame (line data)
STDOUT frame (line data)
...                                                    ← stops when limit or max_bytes hit
EXIT code=0
```

- `offset` — 1-indexed line number to start from (0 or absent = beginning)
- `limit` — maximum lines to return (0 = unlimited)
- `max_bytes` — maximum bytes to return (0 = unlimited)

Whichever limit hits first stops the read. Without any truncation parameters, the full file is streamed (backward compatible). The `FILE_READ_RESP` always contains the *total* file size so the consumer knows whether content was truncated.

Directories and non-regular files are rejected with an `ERROR` frame. File reads are cancellable via context — closing the connection gives lohar a broken pipe, stopping the transfer immediately.

## File Write Protocol

Writes are atomic. Lohar writes to a temp file, fsyncs, then renames over the target. Concurrent readers see either the old content or the new content, never partial.

```
FILE_WRITE_REQ {"path": "/workspace/app.js", "mode": "0644", "size": 1234}
                    ↓
STDIN frame (content bytes)
STDIN frame (content bytes)
...                                                    ← until size bytes sent
                    ↓
FILE_WRITE_RESP {"status": "ok"}
```

Negative sizes are rejected (prevents silent data loss from missing `Content-Length`). If the connection drops mid-write, the temp file is cleaned up.

## Kill Semantics

Kill behavior depends on what's being killed:

| Context | Signal | Why |
|---------|--------|-----|
| **Piped exec** (non-TTY) | `SIGKILL` to process group | Agents need instant, reliable abort. Child processes (npm → node) must die immediately. |
| **TTY session disconnect** | No signal | Process keeps running, session detaches. Scrollback captures output. |
| **TTY session KILL frame** | `SIGTERM` to process group | Allows graceful shutdown. If the process handles SIGTERM and survives, the session remains reattachable. |
| **EXEC_KILL API** | `SIGKILL` to process group | Explicit force-kill by session ID. |
| **Idle timer** | `SIGKILL` to process group | Session is abandoned, no observer. |

All kill operations target the *process group* (negative PID), not just the session leader. This requires `Setpgid: true` on the `SysProcAttr` so child processes are in the same group. Without this, `npm install` would survive killing the shell that launched it.

## Forward Compatibility

`ReadFrame` in the client skips unknown frame types rather than erroring. This allows the protocol to be extended without breaking existing clients — a new frame type added to lohar won't crash an older bhatti host.
