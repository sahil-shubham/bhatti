# Shell & Session Hardening

Status: `bhatti shell` silently drops under production conditions.
A champion user reported their shell disconnecting while running a
long-running command through `api.bhatti.sh` (Cloudflare Tunnel).
Screenshot confirms: no error, no "detached" message, no bash prompt —
just silent return to the Mac shell.

The investigation uncovered 12 bugs and a major architectural gap
(session reattach plumbing exists in the guest agent but nothing above
it uses it). This plan fixes all of them.

---

## Root Cause Analysis

The user runs `bhatti shell rory`, starts `hermes gateway` (a daemon
that prints a banner then waits for events). The CLI drops silently
back to the Mac prompt.

**What happened:** `hermes gateway` printed its startup output, then
went idle (no more terminal output = no more WebSocket frames). Cloudflare
Tunnel has a WebSocket idle timeout (~100 seconds). With no ping/pong
keepalives, Cloudflare sees an idle connection and closes it. The CLI's
`conn.ReadMessage()` returns an error, `done` closes, function returns,
`defer term.Restore()` restores the terminal. Nothing is printed because
the error path is silent.

**Why there's no recovery:** `bhatti shell rory` always creates a new
session (`s2`). The old session (`s1`) keeps running inside the VM with
`hermes gateway` still alive and scrollback accumulating, but the user
has no way to get back to it. The session leaks until the VM is destroyed.

---

## Bug Inventory

| # | Severity | Bug | Location |
|---|----------|-----|----------|
| 1 | **Critical** | No WebSocket ping/pong — proxies kill idle connections | CLI + server WS handler |
| 2 | **Critical** | Concurrent WebSocket writes — data race corrupts frames | CLI shellCmd |
| 3 | **Critical** | No session reattach — `bhatti shell` always creates new | Engine, server, CLI |
| 4 | **High** | CLI prints nothing on shell disconnect | CLI shellCmd |
| 5 | **High** | No cleanup coordination between server goroutines | Server WS handler |
| 6 | **Medium** | Guest ignores WriteFrame errors on attached conn | Guest tty.go |
| 7 | **Medium** | Detached sessions with MaxIdle=0 leak forever | Guest session.go + engine Shell() |
| 8 | **Medium** | No WebSocket read deadline — half-open connections block forever | CLI + server |
| 9 | **Low** | SIGWINCH WriteJSON error not handled | CLI shellCmd |
| 10 | **Low** | `r.Context()` used for long-lived Shell dial | Server WS handler |
| 11 | **Low** | Guest `readHostInput` doesn't log disconnect reason | Guest tty.go |
| 12 | **Medium** | Scrollback ring buffer accessed concurrently without synchronization | Guest session.go + tty.go |

---

## Dependency Graph

```
Phase 1 (stop the bleeding — fixes the reported crash)
  Part 1 (ping/pong)              — no deps
  Part 2 (concurrent write fix)   — no deps
  Part 3 (CLI disconnect message) — no deps

Phase 2 (session reattach — fixes the "no recovery" gap)
  Part 4 (engine ShellAttach)     — no deps
  Part 5 (server WS reattach)     — depends on Part 4
  Part 6 (CLI auto-reattach)      — depends on Part 5
  Part 7 (idle timeout for detached sessions) — no deps

Phase 3 (correctness — prevents silent state corruption)
  Part 8 (server goroutine coordination)    — no deps
  Part 9 (guest WriteFrame error handling)  — no deps
  Part 10 (WebSocket read deadlines)        — depends on Part 1 (pong resets deadline)
  Part 11 (r.Context fix)                   — no deps
  Part 12 (guest disconnect logging)        — no deps
  Part 13 (scrollback thread safety)          — no deps
```

Phase 1 must ship first — it fixes the user-facing crash. Phase 2 is
the highest-value work — it transforms "shell dropped, everything lost"
into "shell dropped, reconnect with scrollback." Phase 3 prevents
resource leaks and silent corruption.

---

## Phase 1 — Stop the Bleeding

### Part 1 — WebSocket Ping/Pong Keepalives

Fixes Bug 1. Without keepalives, any proxy between CLI and server
(Cloudflare, nginx, ALB, NAT gateway) kills idle WebSocket connections.
Cloudflare's default is ~100 seconds. A shell running a command that
produces no output for 100 seconds (compilation, network wait, sleep)
triggers this.

#### 1.1 Server Side

**File:** `pkg/server/routes.go` — `handleSandboxWS`

Add a ping ticker and pong handler. The server sends pings every 30
seconds. The CLI responds with pongs automatically (gorilla default
behavior). The server resets its read deadline on each pong.

```go
func (s *Server) handleSandboxWS(w http.ResponseWriter, r *http.Request, id string) {
    sb := s.getUserSandbox(w, r, id)
    if sb == nil {
        return
    }

    conn, err := upgrader.Upgrade(w, r, nil)
    if err != nil {
        slog.Error("websocket upgrade failed", "error", err)
        return
    }
    defer conn.Close()

    const (
        pingInterval = 30 * time.Second
        pongTimeout  = 90 * time.Second  // 3 missed pings = dead
    )

    // Pong resets the read deadline. If no pong arrives within
    // pongTimeout, ReadMessage returns a deadline error and the
    // handler cleans up.
    conn.SetReadDeadline(time.Now().Add(pongTimeout))
    conn.SetPongHandler(func(string) error {
        conn.SetReadDeadline(time.Now().Add(pongTimeout))
        return nil
    })

    if err := s.ensureHot(r.Context(), sb.EngineID); err != nil {
        conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
        conn.WriteMessage(websocket.TextMessage, []byte("wake sandbox: "+err.Error()))
        return
    }
    term, err := s.engine.Shell(context.Background(), sb.EngineID)
    if err != nil {
        conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
        conn.WriteMessage(websocket.TextMessage, []byte("shell error: "+err.Error()))
        return
    }
    // N.B. defer order matters: conn.Close() (from earlier defer) runs
    // after term.Close(). term.Close() unblocks the term→WS goroutine's
    // Read(); conn.Close() unblocks the WS→term goroutine's ReadMessage().
    // Both goroutines must exit before the function returns.
    defer term.Close()

    // Serialize all WebSocket writes through a mutex. gorilla allows
    // one concurrent reader + one concurrent writer, but we have
    // three write sources: terminal data, ping ticker, and close frame.
    var wsMu sync.Mutex
    wsWrite := func(msgType int, data []byte) error {
        wsMu.Lock()
        defer wsMu.Unlock()
        conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
        return conn.WriteMessage(msgType, data)
    }

    // done signals both goroutines to exit.
    done := make(chan struct{})
    closeOnce := sync.Once{}
    closeDone := func() { closeOnce.Do(func() { close(done) }) }

    // Ping ticker — keeps the connection alive through proxies.
    go func() {
        ticker := time.NewTicker(pingInterval)
        defer ticker.Stop()
        for {
            select {
            case <-ticker.C:
                if err := wsWrite(websocket.PingMessage, nil); err != nil {
                    closeDone()
                    return
                }
            case <-done:
                return
            }
        }
    }()

    // Terminal → WebSocket
    go func() {
        defer closeDone()
        buf := make([]byte, 4096)
        for {
            n, err := term.Read(buf)
            if err != nil {
                wsWrite(websocket.CloseMessage,
                    websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
                return
            }
            if err := wsWrite(websocket.BinaryMessage, buf[:n]); err != nil {
                return
            }
        }
    }()

    // WebSocket → Terminal (runs in handler goroutine)
    go func() {
        defer closeDone()
        for {
            msgType, msg, err := conn.ReadMessage()
            if err != nil {
                return
            }

            // Pong received → read deadline already reset by PongHandler.
            // Text messages are resize commands.
            if msgType == websocket.TextMessage {
                var resize struct {
                    Type string `json:"type"`
                    Rows int    `json:"rows"`
                    Cols int    `json:"cols"`
                }
                if json.Unmarshal(msg, &resize) == nil && resize.Type == "resize" {
                    term.Resize(resize.Rows, resize.Cols)
                    continue
                }
            }

            if _, err := term.Write(msg); err != nil {
                return
            }
        }
    }()

    <-done
}
```

**Key changes from current code:**

1. `wsMu` serializes all writes (fixes the server side of Bug 2's
   pattern — though the server currently only has one writer goroutine,
   adding pings introduces a second).
2. `done` channel coordinates shutdown — when either direction dies,
   both goroutines exit promptly. Fixes Bug 5.
3. `pongTimeout` of 90 seconds — 3 missed pings = dead connection.
   Fixes Bug 8 for the server side.
4. `context.Background()` instead of `r.Context()` for `Shell()`.
   Fixes Bug 10.
5. `closeDone()` with `sync.Once` — safe to call from any goroutine.
6. Both Terminal→WS and WS→Terminal run as goroutines, handler blocks
   on `<-done`. This is cleaner than the current "one goroutine + main
   loop" pattern because shutdown is symmetric.
7. Pre-upgrade error messages (`"wake sandbox"`, `"shell error"`) now
   set a 10-second write deadline. Without it, a half-open client
   blocks the handler goroutine forever.
8. Defer ordering is load-bearing: `defer conn.Close()` is registered
   before `defer term.Close()`, so `term.Close()` runs first (LIFO),
   unblocking the term→WS goroutine. Then `conn.Close()` unblocks the
   WS→term goroutine. The `agentTermConn` (which wraps a `net.Conn`
   vsock) supports concurrent close+read safely.
9. The `wsWrite` helper is used for **all** write paths including the
   ping ticker — no manual lock/unlock outside the helper.

#### 1.2 CLI Side

**File:** `cmd/bhatti/cli.go` — `shellCmd`

The CLI needs to respond to pings (gorilla does this automatically) and
set its own read deadline.

```go
conn, _, err := websocket.DefaultDialer.Dial(
    wsURL+"/sandboxes/"+id+"/ws", header)
if err != nil {
    return err
}
defer conn.Close()

const pongTimeout = 90 * time.Second

conn.SetReadDeadline(time.Now().Add(pongTimeout))
conn.SetPongHandler(func(string) error {
    conn.SetReadDeadline(time.Now().Add(pongTimeout))
    return nil
})

// gorilla's default PingHandler sends a pong automatically.
// We additionally reset the read deadline on receiving a ping
// (which means the server is alive).
conn.SetPingHandler(func(appData string) error {
    conn.SetReadDeadline(time.Now().Add(pongTimeout))
    // Must write pong under lock (see Part 2).
    wsMu.Lock()
    defer wsMu.Unlock()
    conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
    err := conn.WriteMessage(websocket.PongMessage, []byte(appData))
    if err != nil {
        // Pong write failed — connection is dead. Close the conn so
        // ReadMessage returns immediately instead of waiting for the
        // full 90-second read deadline to expire.
        conn.Close()
    }
    return err
})
```

The rest of the read deadline handling is automatic: `ReadMessage()`
returns an error when the deadline expires, `done` closes, the shell
exits (now with a proper message — see Part 3).

#### 1.3 Tests

The ping interval and pong timeout should be settable via functional
options or a test-only override so tests don't need real 90-second
timeouts. Tests use a 1-second ping interval / 3-second pong timeout.

- `TestWSPingPong` — connect to `/ws`, don't send any data, verify
  connection stays alive beyond the pong timeout (would have died
  without keepalives). With 1s/3s test config, this runs in <5s.
- `TestWSPongTimeout` — connect to `/ws`, install a custom PingHandler
  that does NOT send pongs. Verify server closes connection within
  ~3 seconds (test pong timeout).
- `TestWSPingInterval` — connect, capture all frames, verify PingMessage
  arrives within 2 seconds (test ping interval).

---

### Part 2 — Fix Concurrent WebSocket Writes in CLI

Fixes Bug 2 and Bug 9. Three goroutines write to `conn` concurrently:
stdin→WS, SIGWINCH resize, and now the PingHandler (pong reply). Any
concurrent write can corrupt an in-flight WebSocket frame, causing the
server to close the connection.

**File:** `cmd/bhatti/cli.go` — `shellCmd`

Add a write mutex. All writes go through it:

```go
var wsMu sync.Mutex
wsWrite := func(msgType int, data []byte) error {
    wsMu.Lock()
    defer wsMu.Unlock()
    conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
    return conn.WriteMessage(msgType, data)
}

// Initial size
w, h, _ := term.GetSize(int(os.Stdin.Fd()))
wsWriteJSON := func(v any) error {
    wsMu.Lock()
    defer wsMu.Unlock()
    conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
    return conn.WriteJSON(v)
}
wsWriteJSON(map[string]any{"type": "resize", "rows": h, "cols": w})

// SIGWINCH → resize (uses mutex)
go func() {
    for range sigwinch {
        w, h, _ := term.GetSize(int(os.Stdin.Fd()))
        wsWriteJSON(map[string]any{"type": "resize", "rows": h, "cols": w})
    }
}()

// stdin → WebSocket (uses mutex)
go func() {
    buf := make([]byte, 4096)
    for {
        n, err := os.Stdin.Read(buf)
        if err != nil {
            conn.Close()
            return
        }
        for i := 0; i < n; i++ {
            if buf[i] == 0x1c {
                // Send bytes before the escape character (don't
                // leak 0x1c or anything after it to the remote).
                if i > 0 {
                    wsWrite(websocket.BinaryMessage, buf[:i])
                }
                term.Restore(int(os.Stdin.Fd()), oldState)
                fmt.Fprintf(os.Stderr, "\r\ndetached\r\n")
                conn.Close()
                return
            }
        }
        wsWrite(websocket.BinaryMessage, buf[:n])
    }
}()
```

The PingHandler's pong reply also uses `wsMu` (shown in Part 1.2).

**This is a one-line-concept fix** — add a mutex, use it everywhere.
The reason it wasn't caught earlier is that gorilla tolerates one
reader + one writer, and the race between the two pre-existing writers
(stdin + SIGWINCH) is narrow. Adding pong as a third writer makes the
race wide enough to hit in practice.

#### 2.1 Tests

- `TestConcurrentShellResize` — open a shell, send rapid SIGWINCH
  signals while simultaneously typing. Run for 5 seconds. Verify no
  panics, no dropped connections, no corrupted frames.
  (This test would have been flaky before the fix.)

---

### Part 3 — CLI Disconnect Message

Fixes Bug 4. The CLI currently prints nothing when the shell drops.
The user has no idea what happened.

**File:** `cmd/bhatti/cli.go` — `shellCmd`

After `<-done`, before returning, print a message:

```go
<-done

// Restore terminal before printing.
term.Restore(int(os.Stdin.Fd()), oldState)

// Determine what happened.
// If the user pressed Ctrl+\, the stdin goroutine already printed
// "detached" and closed conn. The reader got an error and closed
// done. We detect this by checking if conn is already closed.
//
// For all other cases (server closed, network error, process exited),
// print a reconnect hint.
if !userDetached.Load() {
    fmt.Fprintf(os.Stderr, "\r\nconnection lost\r\n")
    fmt.Fprintf(os.Stderr, "session may still be running — reconnect with: bhatti shell %s\r\n", args[0])
}
return nil
```

The `userDetached` flag is set by the Ctrl+\ handler before closing
the connection. It **must** be an `atomic.Bool` — one goroutine writes
it (stdin), another reads it (main after `<-done`). A bare `bool`
is a data race the Go race detector will catch.

```go
var userDetached atomic.Bool

// stdin → WebSocket
go func() {
    // ...
    for i := 0; i < n; i++ {
        if buf[i] == 0x1c {
            userDetached.Store(true)
            // ...
        }
    }
    // ...
}()
```

**Why "session may still be running":** after Phase 2 ships, this
becomes accurate — the session IS still running and `bhatti shell`
WILL reconnect to it. Before Phase 2, it's aspirational but still
better than silence. The user at least knows the connection dropped
(not that their VM died).

#### 3.1 Tests

- `TestShellDisconnectMessage` — mock a server that upgrades to WS
  then closes after 1 second. Verify CLI prints "connection lost" to
  stderr.
- `TestShellDetachMessage` — send Ctrl+\ byte to CLI stdin. Verify
  "detached" is printed, not "connection lost".

---

## Phase 2 — Session Reattach

This is the most important phase. It transforms the user experience
from "shell dropped, everything is gone" to "shell dropped, pick up
where you left off."

### Design Decision: Auto-Reattach vs. Explicit

**Decision: auto-reattach by default, `--new` to force fresh.**

When the user runs `bhatti shell dev`:
1. Query sessions in the sandbox (`GET /sandboxes/:id/sessions`)
2. If there's a detached, running TTY session → reattach to it
3. If there are multiple detached sessions → reattach to the most
   recently created one
4. If there are no detached sessions → create a new one
5. `bhatti shell dev --new` always creates a new session

This matches what the README already promises ("reconnect with
`bhatti shell dev` again") and follows the tmux mental model. It
requires no new API endpoints — session listing already exists, and
the WS handler just needs a query parameter.

Why not explicit `--session s1`? The user doesn't know the session ID.
They'd have to `bhatti ps dev` first. Auto-reattach is what they want
99% of the time: "give me back my shell."

### Part 4 — Engine `ShellAttach` Method

**File:** `pkg/engine/firecracker/engine.go`

Add a method that reattaches to an existing session instead of creating
a new one:

```go
// ShellAttach reconnects to an existing TTY session by ID.
// Returns the session info and a bidirectional terminal connection.
// The previous client (if any) is detached, and the session's
// scrollback is replayed.
func (e *Engine) ShellAttach(ctx context.Context, id, sessionID string) (*proto.SessionInfo, engine.TerminalConn, error) {
    vm, err := e.getVM(id)
    if err != nil {
        return nil, nil, err
    }

    vm.stateMu.Lock()
    if vm.Thermal != "hot" {
        vm.stateMu.Unlock()
        return nil, nil, fmt.Errorf("sandbox %q is not hot (thermal=%s)", id, vm.Thermal)
    }
    ag := vm.Agent
    vm.stateMu.Unlock()

    return ag.SessionAttach(ctx, sessionID)
}
```

Also add a type assertion interface so the server can check capability:

```go
// SessionAttacher is optionally implemented by engines that support
// reconnecting to existing TTY sessions.
//
// ifDetached: if true, attach only if the session is currently detached.
// Returns an error if the session is attached by another client. This
// prevents the auto-reattach TOCTOU race (SessionList says "detached",
// but another client attached between list and attach). Explicit
// ?session=X requests pass ifDetached=false to forcibly take over.
type SessionAttacher interface {
    ShellAttach(ctx context.Context, id, sessionID string, ifDetached bool) (*proto.SessionInfo, engine.TerminalConn, error)
}
```

**File:** `pkg/engine/engine.go`

```go
type SessionAttacher interface {
    ShellAttach(ctx context.Context, id, sessionID string, ifDetached bool) (*proto.SessionInfo, engine.TerminalConn, error)
}
```

#### 4.1 Tests

- `TestShellAttach` — create session, disconnect, attach by session ID,
  verify scrollback is replayed. (This test pattern already exists in
  `session_test.go`; this adds the engine-layer wrapper.)
- `TestShellAttachDetachsPrevious` — create session, attach from client
  A, attach from client B. Verify A gets an EXIT frame (detached),
  B gets scrollback.

---

### Part 5 — Server WS Handler: Reattach Support

**File:** `pkg/server/routes.go` — `handleSandboxWS`

Accept a `?session=<id>` query parameter. If present, reattach. If
absent, use the auto-reattach logic.

```go
func (s *Server) handleSandboxWS(w http.ResponseWriter, r *http.Request, id string) {
    sb := s.getUserSandbox(w, r, id)
    if sb == nil {
        return
    }

    conn, err := upgrader.Upgrade(w, r, nil)
    if err != nil {
        slog.Error("websocket upgrade failed", "error", err)
        return
    }
    defer conn.Close()

    // ... ping/pong setup from Part 1 ...

    if err := s.ensureHot(r.Context(), sb.EngineID); err != nil {
        wsWrite(websocket.TextMessage, []byte("wake sandbox: "+err.Error()))
        return
    }

    sessionParam := r.URL.Query().Get("session")
    forceNew := r.URL.Query().Get("new") == "true"

    var term engine.TerminalConn
    var sessionID string

    sa, canAttach := s.engine.(engine.SessionAttacher)
    sl, canList := s.engine.(interface {
        SessionList(ctx context.Context, id string) ([]proto.SessionInfo, error)
    })

    if sessionParam != "" && canAttach {
        // Explicit session reattach — forcibly detaches any existing
        // client (the user explicitly chose this session).
        info, t, err := sa.ShellAttach(context.Background(), sb.EngineID, sessionParam, false)
        if err != nil {
            wsWrite(websocket.TextMessage, []byte("attach error: "+err.Error()))
            return
        }
        term = t
        sessionID = info.SessionID
    } else if !forceNew && canAttach && canList {
        // Auto-reattach: find a detached, running TTY session.
        // Uses ifDetached=true to avoid stealing a session that
        // was attached between the list call and the attach call
        // (TOCTOU: the Attached field from SessionList may be stale).
        sessions, err := sl.SessionList(context.Background(), sb.EngineID)
        if err == nil {
            var candidate *proto.SessionInfo
            for i := range sessions {
                s := &sessions[i]
                if s.TTY && s.Running && !s.Attached {
                    if candidate == nil || s.CreatedAt > candidate.CreatedAt {
                        candidate = s
                    }
                }
            }
            if candidate != nil {
                info, t, err := sa.ShellAttach(context.Background(), sb.EngineID, candidate.SessionID, true)
                if err == nil {
                    term = t
                    sessionID = info.SessionID
                }
                // If attach fails (race: session exited or was attached
                // between list and attach), fall through to create new.
            }
        }
    }

    if term == nil {
        // No session to reattach — create new
        t, err := s.engine.Shell(context.Background(), sb.EngineID)
        if err != nil {
            wsWrite(websocket.TextMessage, []byte("shell error: "+err.Error()))
            return
        }
        term = t
        sessionID = "" // unknown — we could extract from SESSION_INFO
                       // but Shell() consumes it internally
    }
    defer term.Close()

    // Send session ID to CLI so it can display it and use for reconnect.
    // Use json.Marshal to avoid injection if session IDs ever contain
    // special characters.
    if meta, err := json.Marshal(map[string]string{
        "type": "session", "session_id": sessionID,
    }); err == nil {
        wsWrite(websocket.TextMessage, meta)
    }

    // ... rest of handler (ping ticker, Terminal→WS, WS→Terminal, <-done) ...
}
```

**Session ID propagation:** The server sends a JSON text message
`{"type":"session","session_id":"s1"}` immediately after the terminal
is connected. The CLI can display this and use it for the reconnect
hint. This message is distinct from resize messages (which also use
TextMessage) because it has `"type":"session"`.

**For new sessions:** `engine.Shell()` currently consumes the
SESSION_INFO frame internally and doesn't return the session ID.

**Do NOT change the `Engine.Shell` signature.** It is an exported
interface implemented by multiple engines (Firecracker, potentially
Docker via build tags). Breaking it for one return value is too heavy.

Instead, add a separate optional interface:

```go
// ShellSessioner is optionally implemented by engines that return
// session metadata alongside the terminal connection.
type ShellSessioner interface {
    ShellSession(ctx context.Context, id string) (string, engine.TerminalConn, error)
}
```

The Firecracker engine implements `ShellSession` using
`agent.ShellSession()` internally:

```go
func (e *Engine) ShellSession(ctx context.Context, id string) (string, engine.TerminalConn, error) {
    vm, err := e.getVM(id)
    if err != nil {
        return "", nil, err
    }
    vm.stateMu.Lock()
    if vm.Thermal != "hot" {
        vm.stateMu.Unlock()
        return "", nil, fmt.Errorf("sandbox %q is not hot (thermal=%s)", id, vm.Thermal)
    }
    ag := vm.Agent
    vm.stateMu.Unlock()

    info, term, err := ag.ShellSession(ctx, []string{"/bin/bash", "-li"},
        map[string]string{"TERM": "xterm-256color"}, 24, 80, 3600)
    if err != nil {
        return "", nil, err
    }
    return info.SessionID, term, nil
}
```

The server uses a type assertion to get the session ID when available,
falling back to `Shell()` for engines that don't support it:

```go
if ss, ok := s.engine.(engine.ShellSessioner); ok {
    sid, t, err := ss.ShellSession(context.Background(), sb.EngineID)
    if err != nil {
        wsWrite(websocket.TextMessage, []byte("shell error: "+err.Error()))
        return
    }
    term = t
    sessionID = sid
} else {
    t, err := s.engine.Shell(context.Background(), sb.EngineID)
    if err != nil {
        wsWrite(websocket.TextMessage, []byte("shell error: "+err.Error()))
        return
    }
    term = t
}
```

This preserves backwards compatibility. `Engine.Shell` is untouched.

#### 5.1 Tests

- `TestWSAutoReattach` — open WS shell, get session ID, close WS,
  open WS again (no `?new=true`). Verify it reattaches to same
  session (same session ID), scrollback is replayed.
- `TestWSForceNew` — open WS shell, close, open with `?new=true`.
  Verify different session ID.
- `TestWSExplicitSession` — open WS shell (session s1), close, open
  with `?session=s1`. Verify reattach.
- `TestWSAutoReattachPicksMostRecent` — create two sessions (s1, s2),
  detach both. Open WS. Verify it picks s2 (most recent).
- `TestWSAutoReattachSkipsAttached` — create session s1 (attached by
  another client), create s2 (detached). Open WS. Verify it picks s2
  (s1 is still attached).
- `TestWSAutoReattachFallsThrough` — no detached sessions exist. Open
  WS. Verify new session created.

---

### Part 6 — CLI Auto-Reattach

**File:** `cmd/bhatti/cli.go` — `shellCmd`

The CLI changes:

1. Send `?new=true` if `--new` flag is set
2. Parse the `{"type":"session","session_id":"s1"}` message from the
   server to display session info
3. Display session ID on connect and in the disconnect message

```go
var shellCmd = &cobra.Command{
    Use:               "shell <id|name>",
    Aliases:           []string{"sh"},
    Short:             "Open an interactive shell",
    Args:              cobra.ExactArgs(1),
    ValidArgsFunction: completeSandboxNames,
    RunE: func(cmd *cobra.Command, args []string) error {
        id, err := resolveID(args[0])
        if err != nil {
            return err
        }

        forceNew, _ := cmd.Flags().GetBool("new")

        wsURL := strings.Replace(apiURL, "http://", "ws://", 1)
        wsURL = strings.Replace(wsURL, "https://", "wss://", 1)
        endpoint := wsURL + "/sandboxes/" + id + "/ws"
        if forceNew {
            endpoint += "?new=true"
        }

        header := http.Header{}
        if apiToken != "" {
            header.Set("Authorization", "Bearer "+apiToken)
        }
        conn, _, err := websocket.DefaultDialer.Dial(endpoint, header)
        if err != nil {
            return err
        }
        defer conn.Close()

        // ... ping/pong setup, raw terminal mode, write mutex ...

        var sessionID string
        var userDetached atomic.Bool

        // WebSocket → stdout
        done := make(chan struct{})
        go func() {
            defer close(done)
            for {
                msgType, msg, err := conn.ReadMessage()
                if err != nil {
                    return
                }
                // Parse session info message (sent once on connect)
                if msgType == websocket.TextMessage {
                    var meta struct {
                        Type      string `json:"type"`
                        SessionID string `json:"session_id"`
                    }
                    if json.Unmarshal(msg, &meta) == nil && meta.Type == "session" {
                        sessionID = meta.SessionID
                        continue
                    }
                }
                os.Stdout.Write(msg)
            }
        }()

        // ... stdin goroutine (sets userDetached.Store(true) on Ctrl+\) ...

        <-done
        term.Restore(int(os.Stdin.Fd()), oldState)
        if !userDetached.Load() {
            fmt.Fprintf(os.Stderr, "\r\nconnection lost")
            if sessionID != "" {
                fmt.Fprintf(os.Stderr, " (session %s still running)", sessionID)
            }
            fmt.Fprintf(os.Stderr, "\r\nreconnect: bhatti shell %s\r\n", args[0])
        }
        return nil
    },
}

func init() {
    shellCmd.Flags().Bool("new", false, "Force a new session (don't reattach)")
}
```

#### 6.1 Tests

- `TestCLIShellReattach` — start shell, get session ID from output,
  disconnect, reconnect, verify same session ID.
- `TestCLIShellNew` — `--new` flag, verify different session ID.
- `TestCLIShellDisconnectMessage` — force a disconnect, capture stderr,
  verify "connection lost (session s1 still running)" is printed.
- `TestCLIShellDetachMessage` — send Ctrl+\, verify "detached" is
  printed (not "connection lost").

---

### Part 7 — Idle Timeout for Detached Sessions

Fixes Bug 7. Currently `engine.Shell()` sends `MaxIdleSec: nil`, which
the guest defaults to 0 (forever). Detached sessions leak.

**File:** `pkg/engine/firecracker/engine.go` — `Shell`

Set a default idle timeout when creating new sessions:

```go
func (e *Engine) Shell(ctx context.Context, id string) (string, engine.TerminalConn, error) {
    // ...
    maxIdle := uint32(3600) // 1 hour idle timeout for detached sessions
    info, term, err := ag.ShellSession(ctx, []string{"/bin/bash", "-li"},
        map[string]string{"TERM": "xterm-256color"}, 24, 80)
    // ...
}
```

Wait — `ShellSession` doesn't pass `MaxIdleSec` to the request. Let me
check:

The `proto.ExecRequest` struct has `MaxIdleSec *uint32`. The agent client's
`ShellSession` builds a request with `TTY: &tty, Rows: &rows, Cols: &cols`
but does NOT set `MaxIdleSec`. Need to add it.

**File:** `pkg/agent/client.go` — `ShellSession`

Add `MaxIdleSec` parameter:

```go
func (c *AgentClient) ShellSession(ctx context.Context, argv []string,
    env map[string]string, rows, cols uint16,
    maxIdleSec uint32) (*proto.SessionInfo, engine.TerminalConn, error) {

    // ...
    tty := true
    req := proto.ExecRequest{
        Argv: argv,
        Env:  env,
        TTY:  &tty,
        Rows: &rows,
        Cols: &cols,
    }
    if maxIdleSec > 0 {
        req.MaxIdleSec = &maxIdleSec
    }
    // ...
}
```

**Default:** 3600 seconds (1 hour). A detached session that hasn't been
reattached in 1 hour is killed. This is long enough for any reasonable
"I'll come back to this" workflow, short enough to prevent session
accumulation from abandoned shells. This value should be a server-level
config (e.g. `DefaultShellIdleSec`) rather than a magic number buried
in the engine, so operators can tune it without recompiling.

When a session is reattached, `handleSessionAttach` calls
`sess.cancelIdleTimer()`, which stops the countdown. When detached
again, the timer restarts. So the 1-hour clock only ticks while
detached.

**Guard against duplicate timers.** `startIdleTimer` must check if a
timer already exists before creating a new one. Without this guard,
two concurrent detach paths (e.g. PTY reader WriteFrame error + host
disconnect) can each call `startIdleTimer()`, leaking a timer and
potentially killing a reattached session:

```go
func (s *Session) startIdleTimer() {
    if s.MaxIdle <= 0 {
        return
    }
    if s.idleTimer != nil {
        return // already ticking
    }
    s.idleTimer = time.AfterFunc(s.MaxIdle, func() {
        s.mu.Lock()
        defer s.mu.Unlock()
        if s.Cmd != nil && s.Cmd.Process != nil && s.ExitCode == nil {
            syscall.Kill(-s.Cmd.Process.Pid, syscall.SIGKILL)
        }
    })
}
```

**What about the init session?** Init sessions (`runInitSession`) have
a well-known ID "init" and `MaxIdle: 0`. They should NOT have an idle
timeout — they represent the sandbox's init script and should run for
the sandbox's lifetime. Keep `MaxIdle: 0` for init sessions.

#### 7.1 Tests

- `TestDetachedSessionIdleTimeout` — create session with MaxIdleSec=2,
  disconnect, wait 3 seconds, verify session is gone (process killed,
  session removed).
- `TestReattachResetsIdleTimer` — create session with MaxIdleSec=5,
  disconnect, wait 3 seconds, reattach, disconnect, wait 3 seconds.
  Verify session is still alive (timer reset on reattach).
- `TestInitSessionNoTimeout` — create init session, disconnect, wait
  for longer than the default timeout. Verify init session is still
  alive.

---

## Phase 3 — Correctness

### Part 8 — Server Goroutine Coordination

Fixes Bug 5. Already addressed in Part 1's rewrite of `handleSandboxWS`.
The `done` channel + `sync.Once` pattern ensures both goroutines exit
promptly when either direction fails. The `defer term.Close()` runs
after `<-done` returns, so the TCP connection to the guest is closed
only after both goroutines have exited.

No additional work needed beyond Part 1. This part exists as a
documentation entry to track that Bug 5 is resolved.

---

### Part 9 — Guest WriteFrame Error Handling

Fixes Bug 6. The guest's PTY reader goroutine ignores `WriteFrame`
errors on the attached connection. A dead connection stays "attached"
forever, preventing thermal transitions and wasting write syscalls.

**Design: single owner of detach state.** `readHostInput` is the
canonical owner of the detach transition — it sets `sess.Attached=nil`
and starts the idle timer. The PTY reader must NOT unilaterally set
`Attached=nil`, because `readHostInput` is running concurrently on the
same connection and would then also call `startIdleTimer()`, creating
duplicate timers.

Instead, when the PTY reader detects a `WriteFrame` error, it **closes
the connection**. This causes `ReadFrame` in `readHostInput` to return
an error, which triggers the canonical detach cleanup in one place.

**File:** `cmd/lohar/tty.go` — background goroutine in `handleTTYSession`

```go
go func() {
    buf := make([]byte, 4096)
    for {
        n, err := master.Read(buf)
        if n > 0 {
            sess.mu.Lock()
            sess.Scrollback.Write(buf[:n])
            if sess.Attached != nil {
                if werr := proto.WriteFrame(sess.Attached, proto.STDOUT, buf[:n]); werr != nil {
                    // Connection is dead. Close it so readHostInput's
                    // ReadFrame returns an error and performs the
                    // canonical detach (Attached=nil + startIdleTimer).
                    // Do NOT set Attached=nil here — readHostInput
                    // owns that transition.
                    sess.Attached.Close()
                }
            }
            sess.mu.Unlock()
        }
        if err != nil {
            // PTY closed — process exited
            exitCode := exitCodeFromErr(cmd.Wait())
            sess.mu.Lock()
            sess.ExitCode = &exitCode
            if sess.Attached != nil {
                exit := proto.ExitPayload(int32(exitCode))
                proto.WriteFrame(sess.Attached, proto.EXIT, exit[:])
            }
            sess.mu.Unlock()
            master.Close()
            return
        }
    }
}()
```

Note: `Scrollback.Write` is now called under `sess.mu` — see Part 13
for the scrollback thread safety fix.

Apply the same fix to `runInitSession`'s background goroutine, which
has the identical pattern.

#### 9.1 Tests

- `TestGuestDetachOnWriteError` — create session, close the host TCP
  connection (simulating network death), verify session transitions to
  detached state (Attached=nil) and thermal manager can query
  AttachedSessions=0.

---

### Part 10 — WebSocket Read Deadlines

Fixes Bug 8. Already addressed in Part 1 for both CLI and server —
the pong handler resets the read deadline. If no pong arrives within
90 seconds (3 missed pings), the read deadline fires, `ReadMessage`
returns a timeout error, and cleanup happens.

No additional work beyond Part 1. This part exists as a documentation
entry.

---

### Part 11 — `r.Context()` Fix

Fixes Bug 10. Already addressed in Part 1 — `context.Background()` is
used for `engine.Shell()` instead of `r.Context()`.

No additional work beyond Part 1. This part exists as a documentation
entry.

---

### Part 12 — Guest Disconnect Logging

Fixes Bug 11. Add minimal logging to `readHostInput` so guest-side
disconnects are observable in lohar's stderr (visible with daemon
debug logging).

**File:** `cmd/lohar/tty.go` — `readHostInput`

```go
func readHostInput(conn net.Conn, sess *Session) {
    for {
        msgType, payload, err := proto.ReadFrame(conn)
        if err != nil {
            fmt.Fprintf(os.Stderr, "lohar: session %s: host disconnected: %v\n",
                sess.ID, err)
            sess.mu.Lock()
            sess.Attached = nil
            sess.mu.Unlock()
            sess.startIdleTimer()
            return
        }
        // ... rest unchanged ...
    }
}
```

This is low-cost (one fmt.Fprintf per disconnect) and invaluable for
debugging. The error message distinguishes io.EOF (clean close) from
network errors.

#### 12.1 Tests

No specific test — this is observability. Verified by running the
existing session tests and checking stderr output.

---

### Part 13 — Scrollback Ring Buffer Thread Safety

Fixes Bug 12. The `ringBuffer` in `session.go` has no internal
synchronization. The PTY reader goroutine calls `Scrollback.Write()`
while `handleSessionAttach` calls `Scrollback.Bytes()` from a
different goroutine. This is a data race on `ringBuffer.w`,
`ringBuffer.full`, and the underlying `ringBuffer.buf` slice.

The current code in `tty.go` calls `Scrollback.Write()` **outside**
`sess.mu` — the lock is only acquired to check `sess.Attached`.
`handleSessionAttach` calls `Scrollback.Bytes()` also outside the
lock. Both paths race.

**Fix: protect all scrollback access under `sess.mu`.**

This is the simplest correct approach. The scrollback is always
accessed in a context where `sess.mu` is nearby, so extending the
critical section is low-friction.

**File:** `cmd/lohar/tty.go` — PTY reader goroutine (both
`handleTTYSession` and `runInitSession`)

Move `Scrollback.Write` inside the existing `sess.mu.Lock()` block
(already done in Part 9's revised code):

```go
// Before (racy):
if n > 0 {
    sess.Scrollback.Write(buf[:n])   // no lock!
    sess.mu.Lock()
    if sess.Attached != nil {
        proto.WriteFrame(sess.Attached, proto.STDOUT, buf[:n])
    }
    sess.mu.Unlock()
}

// After (safe):
if n > 0 {
    sess.mu.Lock()
    sess.Scrollback.Write(buf[:n])   // under lock
    if sess.Attached != nil {
        if werr := proto.WriteFrame(sess.Attached, proto.STDOUT, buf[:n]); werr != nil {
            sess.Attached.Close()
        }
    }
    sess.mu.Unlock()
}
```

**File:** `cmd/lohar/tty.go` — `handleSessionAttach`

Move `Scrollback.Bytes()` inside `sess.mu`:

```go
// Before (racy):
if sess.Scrollback != nil {
    scrollback := sess.Scrollback.Bytes()   // no lock!
    if len(scrollback) > 0 {
        proto.WriteFrame(conn, proto.STDOUT, scrollback)
    }
}

// After (safe):
sess.mu.Lock()
var scrollback []byte
if sess.Scrollback != nil {
    scrollback = sess.Scrollback.Bytes()
}
sess.mu.Unlock()
if len(scrollback) > 0 {
    proto.WriteFrame(conn, proto.STDOUT, scrollback)
}
```

The `Bytes()` call returns a copy (the ring buffer already allocates a
new slice), so the write to the WebSocket happens outside the lock.

**Why not add a mutex to `ringBuffer` itself?** Adding a `sync.Mutex`
to `ringBuffer` would be over-synchronizing — every single byte
written would acquire/release a lock. Since all callers already have
`sess.mu` available, using the existing lock is both simpler and
faster. The ring buffer stays a plain data structure with no
concurrency awareness.

#### 13.1 Tests

- `TestScrollbackConcurrency` — spawn 10 goroutines: 5 writing random
  data via `Scrollback.Write()` under `sess.mu`, 5 reading via
  `Scrollback.Bytes()` under `sess.mu`. Run for 2 seconds with
  `-race`. Verify no races and `Bytes()` always returns valid data.
- `TestReattachScrollbackConsistency` — create session, write known
  data to PTY, detach, reattach. Verify scrollback replay contains
  the expected bytes (not corrupted by concurrent access).

---

## Verification: Full User Story

After all phases ship, the user's experience:

```bash
$ bhatti shell rory
lohar@rory:/$ cd /opt/hermes
lohar@rory:/opt/hermes$ hermes gateway
┌──────────────────────────────────────┐
│  ✦ Hermes Gateway Starting...        │
│                                      │
│  Messaging platforms + cron scheduler│
│  Press Ctrl+C to stop               │
└──────────────────────────────────────┘

# ... Cloudflare timeout would have killed this before.
# Now ping/pong keeps it alive. User goes to lunch.
# Eventually their laptop sleeps, WiFi reconnects to
# a different IP. The WebSocket dies.

connection lost (session s3 still running)
reconnect: bhatti shell rory

$ bhatti shell rory
# ← auto-reattaches to session s3
# ← 64KB of scrollback replayed (hermes logs since disconnect)
# ← hermes gateway is still running, user picks up where they left off
lohar@rory:/opt/hermes$
```

---

## Documented Non-Decisions

**Auto-reconnect loop in CLI.** The CLI could automatically retry the
WebSocket connection on disconnect instead of exiting. Tools like
`mosh` and `tmux` do this. Deferred — it adds complexity (exponential
backoff, max retries, display handling during reconnect) and the
reattach-on-next-invocation model is sufficient. The user runs
`bhatti shell dev` again and is back in <1 second with scrollback.

**Server-side session creation via REST API.** Creating sessions via
`POST /sandboxes/:id/sessions` and then attaching via WS would be
cleaner than the current "WS upgrade creates the session" model. But
it requires a two-step dance from the CLI (create, then connect) and
adds API surface. Defer — the current model of "WS auto-detects" is
sufficient.

**Session naming.** Sessions could have user-chosen names instead of
auto-generated IDs (`s1`, `s2`). Not needed — the auto-reattach logic
doesn't require the user to know session IDs. `bhatti ps` shows them
for debugging.

**Multiple simultaneous viewers.** Two clients attached to the same
session, both seeing output. Currently attach detaches the previous
client. tmux supports this. Significant complexity (fan-out writes,
conflict resolution for input). Defer indefinitely.

**Scrollback size configuration.** The 64KB ring buffer is hardcoded.
Configurable per-session scrollback would require protocol changes
(passing size in EXEC_REQ). 64KB is a reasonable default — it holds
~1000 lines of 64-character text. Defer.

**Session persistence across VM restarts.** Sessions die when the VM
is snapshot/restored (cold thermal transition). The PTY file descriptors
and process state are captured in the Firecracker snapshot, so sessions
technically survive warm→cold→hot transitions. But the TCP connection
from host→guest is broken. The session is "alive" inside the restored
VM but unreachable because the agent doesn't know about the old TCP
connection. This requires reconnecting the agent client to the restored
session — possible but complex. Defer.
