# Exec API Hardening — Detached Exec, Idempotent Create, Timeout Fix

Three related pain points discovered while building karkhana (the agent
orchestrator that runs Claude turns inside bhatti sandboxes). All three
are API-level issues — the VM engine, guest agent protocol, and thermal
cycle are uninvolved.

1. **Detached exec**: `handlePipedExec` blocks until all pipe FDs close.
   Backgrounded children inherit the FDs → EOF never arrives → exec hangs.
   Can't fire-and-forget long-running commands.

2. **Sandbox create conflict**: Creating a sandbox with a duplicate name
   returns 409 (or 500 on race). Karkhana has to list→filter→create,
   which is a TOCTOU race. The API should be idempotent — return the
   existing sandbox on name conflict.

3. **Exec timeout cap**: Hard-coded max of 3600s, default of 300s.
   Agent turns can run longer. If the caller forgets `timeout_sec`, the
   agent gets killed after 5 minutes.

None of these are breaking changes. All are backwards-compatible
additions to the existing API surface.

---

## Current State

### Detached Exec

The exec flow: HTTP request → server `handleSandboxExec` → engine
`Exec`/`ExecStream` → agent client `DialControl` → lohar
`handlePipedExec` → `cmd.StdoutPipe()`/`cmd.StderrPipe()` → read
until EOF → `cmd.Wait()` → return exit code.

The problem is in `handlePipedExec` (cmd/lohar/exec.go):

```go
stdoutPipe, _ := cmd.StdoutPipe()
stderrPipe, _ := cmd.StderrPipe()
cmd.Start()

// These block until EOF on the pipe.
// If the command backgrounds a child with &, the child inherits
// the pipe FDs. EOF never arrives until the child exits too.
go func() { stdoutPipe.Read(buf) }()
go func() { stderrPipe.Read(buf) }()

ioWg.Wait()       // ← blocks forever
cmd.Wait()
```

The workaround in karkhana today:

```go
// Client side: wrap in setsid, redirect to file, background
exec("bash", "-c", "setsid bash -c 'claude -p ... > /tmp/out.jsonl 2>&1' &")
// Returns immediately because setsid creates a new session,
// and & backgrounds the setsid process, so the shell exits.

// Then poll the output file:
for {
    exec("bash", "-c", "cat /tmp/out.jsonl")
    time.Sleep(3 * time.Second)
}
```

This works but: 3-second latency per poll, loses real-time streaming,
fragile (file might not exist yet, partial lines, race on read).

### Sandbox Create Conflict

The current code in `sandbox_handlers.go` (line 229):

```go
// Check for duplicate name before booting a VM.
if spec.Name != "" {
    existing, _ := s.store.ListSandboxes(user.ID)
    for _, sb := range existing {
        if sb.Name == spec.Name && sb.Status != "destroyed" {
            errResp(w, 409, fmt.Sprintf("sandbox %q already exists", spec.Name))
            return
        }
    }
}
```

This is an application-level uniqueness check with a TOCTOU window.
Two concurrent requests both pass the check, both call `engine.Create()`
(boots a VM, ~3.5s), then one fails at `s.store.CreateSandbox()` with
the UNIQUE constraint from `idx_sandboxes_user_name`. The losing request
has already booted a VM that gets destroyed wastefully.

The SQLite index catches the race:

```sql
CREATE UNIQUE INDEX IF NOT EXISTS idx_sandboxes_user_name
    ON sandboxes(created_by, name) WHERE status != 'destroyed'
```

But the error bubbles up as a 500 with a raw SQLite error message, not
a clean 409. And even the 409 path isn't useful — karkhana wants the
sandbox, not an error. It has to list→filter→create, which is the same
TOCTOU race that causes the 500.

### Exec Timeout

In `exec_handlers.go`:

```go
timeout := 300 * time.Second
if req.TimeoutSec > 0 && req.TimeoutSec <= 3600 {
    timeout = time.Duration(req.TimeoutSec) * time.Second
}
```

Two issues:
- Default is 300s (5 min). If the caller doesn't pass `timeout_sec`,
  agents get killed mid-turn.
- Max is 3600s (1 hour). Claude turns can theoretically run longer,
  especially with complex multi-step tasks.

---

## Design Principles

**Backwards compatible.** Every change adds an optional field or changes
error handling. Existing clients that don't use the new fields see
identical behavior.

**Solve at the right layer.** Detached exec is a guest agent concern
(lohar). Idempotent create is a server concern (sandbox_handlers).
Timeout cap is a server concern (exec_handlers). Don't conflate them.

**Minimal protocol changes.** The vsock framing protocol is bhatti's
most sensitive interface — it's baked into every running lohar binary
inside every VM. Adding an optional JSON field to `ExecRequest` is safe.
Adding new frame types is not necessary here.

**Idempotent APIs over TOCTOU guards.** Instead of list→check→create
(three round trips, still racy), make create itself idempotent. This is
the standard pattern (S3 CreateBucket, k8s apply, Terraform).

---

## Part 1 — Exec Timeout Cap (5 minutes)

The simplest change. Raise the cap from 3600 to 86400 (24 hours).

### 1.1 Why Not "0 = No Timeout"

An unbounded exec with no timeout is a resource leak vector. If the
client disappears (network failure, crash), the exec hangs forever,
consuming one of lohar's 50 concurrent connection slots
(`maxConcurrentConns` in handler.go). A 24h cap is effectively "no
timeout" for any real workload while still providing a safety net.

With detached exec (Part 3), the timeout question becomes less
important for long-running tasks — those bypass the HTTP request
lifecycle entirely. The timeout only matters for synchronous exec.

### 1.2 Changes

**`pkg/server/exec_handlers.go`** — raise the cap:

```go
type execReq struct {
    Cmd        []string `json:"cmd"`
    TimeoutSec int      `json:"timeout_sec,omitempty"` // default 300, max 86400
}

// ...

timeout := 300 * time.Second
if req.TimeoutSec > 0 && req.TimeoutSec <= 86400 {
    timeout = time.Duration(req.TimeoutSec) * time.Second
}
```

**`pkg/server/server_test.go`** — update `TestExecTimeoutClamped`:

The test currently sends `timeout_sec: 99999` and expects it to be
clamped. Update to use a value > 86400:

```go
// timeout_sec > 86400 should be ignored (uses default 300)
resp := doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/exec", map[string]any{
    "cmd":         []string{"echo", "ok"},
    "timeout_sec": 100000,
})
```

### 1.3 Verification

- [ ] `timeout_sec: 7200` (2h) should be accepted — previously silently
      clamped to 300s
- [ ] `timeout_sec: 100000` should be clamped to 300s (default)
- [ ] `timeout_sec: 0` should use the default 300s (existing behavior)
- [ ] Existing tests pass unchanged (mock engine exec is instant)

---

## Part 2 — Idempotent Sandbox Create (30 minutes)

### 2.1 Semantics

`POST /sandboxes` with a `name` that already exists (for the same user,
non-destroyed) returns the existing sandbox with HTTP 200, not 409 or
500. This makes the endpoint idempotent: calling it N times with the
same name produces the same result.

The response includes `X-Bhatti-Existing: true` header so callers can
distinguish "created" (201) from "already existed" (200) if they care.

### 2.2 Why 200 Not 409

409 Conflict tells the caller "you did something wrong." But requesting
a sandbox by name isn't wrong — it's "ensure this sandbox exists." The
caller (karkhana) wants the sandbox object regardless of whether it was
just created or already existed. Returning 409 forces the caller into a
retry loop:

```go
// Current karkhana pattern (bad):
sb, err := client.CreateSandbox(name, ...)
if err != nil && isConflict(err) {
    sandboxes, _ := client.ListSandboxes()
    for _, s := range sandboxes {
        if s.Name == name { sb = s; break }
    }
}
```

With idempotent create:
```go
// New karkhana pattern (good):
sb, err := client.CreateSandbox(name, ...)
// Done. sb is always valid on success.
```

### 2.3 Changes

**`pkg/server/sandbox_handlers.go`** — return existing before booting a VM:

Replace the existing duplicate check (lines 229-237):

```go
// Current:
if spec.Name != "" {
    existing, _ := s.store.ListSandboxes(user.ID)
    for _, sb := range existing {
        if sb.Name == spec.Name && sb.Status != "destroyed" {
            errResp(w, 409, fmt.Sprintf("sandbox %q already exists", spec.Name))
            return
        }
    }
}
```

With:

```go
// Idempotent create: return existing sandbox if name matches.
// This eliminates the TOCTOU race where two concurrent creates
// both pass the check, both boot VMs, one wastes ~3.5s.
if spec.Name != "" {
    existing, err := s.store.GetSandbox(user.ID, spec.Name)
    if err == nil && existing.Status != "destroyed" {
        w.Header().Set("X-Bhatti-Existing", "true")
        writeJSON(w, 200, existing)
        return
    }
}
```

This uses `GetSandbox` (single row lookup by name) instead of
`ListSandboxes` (full table scan). More efficient and doesn't need a
loop.

**Also** — add a safety net at the `CreateSandbox` error path for the
race where two requests pass the check simultaneously:

```go
if err := s.store.CreateSandbox(sb); err != nil {
    // UNIQUE constraint → name race. Another request won.
    // Destroy the VM we just booted and return the winner.
    if strings.Contains(err.Error(), "UNIQUE") {
        s.engine.Destroy(r.Context(), info.EngineID)
        if len(resolvedVolumes) > 0 {
            s.store.DetachAllPersistentVolumesForSandbox(sbID)
        }
        existing, lookupErr := s.store.GetSandbox(user.ID, spec.Name)
        if lookupErr == nil {
            w.Header().Set("X-Bhatti-Existing", "true")
            writeJSON(w, 200, existing)
            return
        }
    }
    s.engine.Destroy(r.Context(), info.EngineID)
    errRespInternal(w, r, "store sandbox failed", err)
    return
}
```

### 2.4 Test Changes

**`pkg/server/server_test.go`** — update `TestDuplicateSandboxNameHTTP`:

The test currently expects 409. Update to expect 200 with the existing
sandbox data:

```go
func TestDuplicateSandboxNameHTTP(t *testing.T) {
    _, alice, _ := setupTwoUsers(t)
    name := uniqueName(t, "dup-name")

    resp := alice(t, "POST", "/sandboxes", map[string]any{"name": name})
    if resp.StatusCode != 201 {
        body, _ := io.ReadAll(resp.Body)
        t.Fatalf("first: expected 201, got %d: %s", resp.StatusCode, body)
    }
    var sb store.Sandbox
    decodeJSON(t, resp, &sb)
    t.Cleanup(func() { alice(t, "DELETE", "/sandboxes/"+sb.ID, nil) })

    // Duplicate — should return existing sandbox with 200
    resp = alice(t, "POST", "/sandboxes", map[string]any{"name": name})
    if resp.StatusCode != 200 {
        body, _ := io.ReadAll(resp.Body)
        t.Fatalf("duplicate: expected 200, got %d: %s", resp.StatusCode, body)
    }
    if resp.Header.Get("X-Bhatti-Existing") != "true" {
        t.Error("missing X-Bhatti-Existing header")
    }
    var sb2 store.Sandbox
    decodeJSON(t, resp, &sb2)
    if sb2.ID != sb.ID {
        t.Errorf("expected same sandbox ID %q, got %q", sb.ID, sb2.ID)
    }
}
```

### 2.5 Verification

- [ ] First `POST /sandboxes {"name":"foo"}` returns 201
- [ ] Second `POST /sandboxes {"name":"foo"}` returns 200 with same ID
- [ ] Response has `X-Bhatti-Existing: true` header on the second call
- [ ] Concurrent creates with same name: one wins with 201, other gets
      200 (no 500, no leaked VM)
- [ ] Different users can have sandboxes with the same name (existing
      behavior, unchanged)
- [ ] Creating with a name of a destroyed sandbox creates a new one (201)

---

## Part 3 — Detached Exec (1-2 hours)

### 3.1 Why Option A (Not Option B)

Option A (detach flag) adds a `detach` boolean to ExecRequest. When
true, lohar wraps the command in `setsid`, redirects stdio to a file,
and returns immediately with the child PID. The caller reads output via
a second exec call (`tail -f /tmp/output.jsonl`).

Option B (Task API) adds new REST routes (`/tasks`), new proto messages,
new store tables, and lohar-side state management for background
processes. It's 3-5x the work.

The session system we already have (`session.go`, `EXEC_LIST_REQ`,
`SESSION_INFO`, `SessionAttach`) is essentially a Task API for TTY
processes. Option B would generalize it to non-TTY processes. That's
the right long-term direction, but it's not needed now.

**Build Option B when:** (a) real-time streaming of long-running tasks
from a dashboard is needed, or (b) managing 10+ concurrent background
tasks per sandbox. Until then, Option A + `tail -f` is sufficient.

### 3.2 Protocol Change

**`pkg/agent/proto/messages.go`** — add optional fields to ExecRequest:

```go
type ExecRequest struct {
    Argv       []string          `json:"argv"`
    Env        map[string]string `json:"env,omitempty"`
    TTY        *bool             `json:"tty,omitempty"`
    Rows       *uint16           `json:"rows,omitempty"`
    Cols       *uint16           `json:"cols,omitempty"`
    Cwd        *string           `json:"cwd,omitempty"`
    SessionID  *string           `json:"session_id,omitempty"`
    MaxIdleSec *int              `json:"max_idle_sec,omitempty"`
    IfDetached *bool             `json:"if_detached,omitempty"`
    Detach     *bool             `json:"detach,omitempty"`      // new: fire-and-forget
    OutputFile *string           `json:"output_file,omitempty"` // new: where to write stdout/stderr
}
```

These are optional JSON fields. Old lohar binaries ignore them (Go's
`json.Unmarshal` skips unknown fields). New lohar binaries with old
hosts never see them set. Fully backwards compatible.

### 3.3 Guest Agent (lohar) Changes

**`cmd/lohar/handler.go`** — dispatch detached exec before TTY/piped:

```go
case proto.EXEC_REQ:
    updateActivity()
    var req proto.ExecRequest
    if err := json.Unmarshal(payload, &req); err != nil {
        proto.WriteFrame(conn, proto.ERROR, []byte(fmt.Sprintf("bad exec request: %v", err)))
        return
    }
    if req.SessionID != nil {
        handleSessionAttach(conn, *req.SessionID, ifDetached)
    } else if len(req.Argv) == 0 {
        proto.WriteFrame(conn, proto.ERROR, []byte("empty argv"))
    } else if req.Detach != nil && *req.Detach {
        handleDetachedExec(conn, req)      // ← new
    } else if req.TTY != nil && *req.TTY {
        handleTTYSession(conn, req)
    } else {
        handlePipedExec(conn, req)
    }
```

**`cmd/lohar/exec.go`** — new handler:

```go
func handleDetachedExec(conn net.Conn, req proto.ExecRequest) {
    // Determine output file
    outputFile := fmt.Sprintf("/tmp/bhatti-detach-%d.log", time.Now().UnixNano())
    if req.OutputFile != nil && *req.OutputFile != "" {
        outputFile = *req.OutputFile
    }

    cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
    cmd.Env = buildEnv(req.Env)
    if req.Cwd != nil {
        cmd.Dir = *req.Cwd
    }
    // New session — fully detached from lohar's process group.
    // Child survives even if the vsock connection closes.
    cmd.SysProcAttr = &syscall.SysProcAttr{
        Setsid:     true,
        Credential: &syscall.Credential{Uid: 1000, Gid: 1000},
    }

    f, err := os.OpenFile(outputFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
    if err != nil {
        proto.WriteFrame(conn, proto.ERROR,
            []byte(fmt.Sprintf("open output file: %v", err)))
        return
    }
    cmd.Stdout = f
    cmd.Stderr = f

    if err := cmd.Start(); err != nil {
        f.Close()
        proto.WriteFrame(conn, proto.ERROR,
            []byte(fmt.Sprintf("start: %v", err)))
        return
    }

    pid := cmd.Process.Pid
    logf("detached exec: pid=%d cmd=%v output=%s", pid, req.Argv, outputFile)

    // Reap in background — don't leak zombies.
    go func() {
        cmd.Wait()
        f.Close()
        logf("detached exec done: pid=%d", pid)
    }()

    // Return PID and output file path to the caller.
    // Use EXIT frame with exit code 0 — this tells the host-side
    // ExecResult parser that the "launch" succeeded. The actual
    // command is still running.
    //
    // We send the metadata as stdout so it appears in ExecResult.Stdout.
    meta := fmt.Sprintf(`{"pid":%d,"output_file":%q}`, pid, outputFile)
    proto.WriteFrame(conn, proto.STDOUT, []byte(meta))
    exit := proto.ExitPayload(0)
    proto.WriteFrame(conn, proto.EXIT, exit[:])
}
```

**Why use STDOUT+EXIT instead of a new frame type:**

The host-side `AgentClient.Exec()` already handles STDOUT→buffer and
EXIT→return. By sending metadata as STDOUT and EXIT with code 0, the
existing `Exec()` function returns `ExecResult{ExitCode: 0, Stdout:
'{"pid":1234,"output_file":"/tmp/..."}'}` without any changes to the
client or engine layer. The server parses the JSON from stdout.

This avoids adding new frame types (which would break old agent clients)
and avoids changing the `Engine.Exec` interface (which would require
changes to every engine implementation).

### 3.4 Server Changes

**`pkg/server/exec_handlers.go`** — add `Detach` and `OutputFile` fields:

```go
type execReq struct {
    Cmd        []string `json:"cmd"`
    TimeoutSec int      `json:"timeout_sec,omitempty"` // default 300, max 86400
    Detach     bool     `json:"detach,omitempty"`       // fire-and-forget
    OutputFile string   `json:"output_file,omitempty"`  // detach: where to write output
    Cwd        string   `json:"cwd,omitempty"`          // working directory
}
```

In `handleSandboxExec`, when `Detach` is true, pass the fields through
to the agent. The existing `Exec()` call works unchanged — the agent
returns immediately with the PID in stdout:

```go
// Before calling engine.Exec, build the cmd to include env/cwd if needed.
// For detach mode, we need to pass extra fields through to the agent.
// Since engine.Exec only takes []string, we encode detach params
// into the command wrapper.

if req.Detach {
    // For detached exec, use a short timeout (launch should be fast)
    // and pass through to ExecStream with detach semantics.
    timeout = 30 * time.Second
    execCtx, cancel = context.WithTimeout(r.Context(), timeout)
    defer cancel()
}
```

However, the current `Engine.Exec(ctx, id, cmd)` interface only takes
`[]string` — it doesn't pass through `Detach` or `OutputFile` to the
agent. Two options:

**Option 1: Encode in command.** Wrap the command so the shell handles
detachment:

```go
if req.Detach {
    // The lohar agent sees a simple command — setsid + redirect
    // happens at the shell level
    wrapped := fmt.Sprintf("setsid bash -c %s > %s 2>&1 &",
        shellescape(strings.Join(req.Cmd, " ")),
        shellescape(outputFile))
    req.Cmd = []string{"bash", "-c", wrapped}
}
```

This works without any engine/agent changes but loses the PID tracking
(the shell exits 0 before we know the child PID) and is fragile
(shell escaping).

**Option 2: Pass detach through the agent protocol.** Add a
`DetachedExec` method to the engine interface or extend `Exec` to accept
options.

Better: add a `StreamExecEngine`-style optional interface:

```go
// In pkg/engine/engine.go:
type DetachedExecEngine interface {
    ExecDetached(ctx context.Context, id string, cmd []string, outputFile string) (pid int, err error)
}
```

```go
// In pkg/engine/firecracker/exec.go:
func (e *Engine) ExecDetached(ctx context.Context, id string, cmd []string, outputFile string) (int, error) {
    vm, err := e.getVM(id)
    if err != nil {
        return 0, err
    }
    // ... thermal check ...
    return ag.ExecDetached(ctx, cmd, nil, "", outputFile)
}
```

```go
// In pkg/agent/client.go:
func (c *AgentClient) ExecDetached(ctx context.Context, argv []string, env map[string]string, cwd, outputFile string) (int, error) {
    conn, err := c.DialControl(ctx)
    if err != nil {
        return 0, err
    }
    defer conn.Close()

    detach := true
    req := proto.ExecRequest{Argv: argv, Env: env, Detach: &detach}
    if outputFile != "" {
        req.OutputFile = &outputFile
    }
    if cwd != "" {
        req.Cwd = &cwd
    }
    if err := proto.SendJSON(conn, proto.EXEC_REQ, req); err != nil {
        return 0, err
    }

    // Read the STDOUT frame (JSON metadata) + EXIT frame
    var stdout bytes.Buffer
    for {
        msgType, payload, err := proto.ReadFrame(conn)
        if err != nil {
            return 0, err
        }
        switch msgType {
        case proto.STDOUT:
            stdout.Write(payload)
        case proto.EXIT:
            var meta struct {
                PID        int    `json:"pid"`
                OutputFile string `json:"output_file"`
            }
            if err := json.Unmarshal(stdout.Bytes(), &meta); err != nil {
                return 0, fmt.Errorf("parse detach response: %w", err)
            }
            return meta.PID, nil
        case proto.ERROR:
            return 0, fmt.Errorf("agent: %s", payload)
        }
    }
}
```

**Recommendation: Option 2.** It's cleaner, returns the PID, and the
protocol change (optional JSON fields) is safe. Option 1 is a hack that
doesn't survive shell escaping edge cases.

### 3.5 Server Handler

```go
func (s *Server) handleSandboxExec(w http.ResponseWriter, r *http.Request, id string) {
    // ... existing validation ...

    if req.Detach {
        de, ok := s.engine.(engine.DetachedExecEngine)
        if !ok {
            errResp(w, 501, "engine does not support detached exec")
            return
        }
        outputFile := req.OutputFile
        if outputFile == "" {
            outputFile = fmt.Sprintf("/tmp/bhatti-exec-%s.log", genID()[:8])
        }
        pid, err := de.ExecDetached(r.Context(), sb.EngineID, req.Cmd, outputFile)
        if err != nil {
            errRespInternal(w, r, "detached exec failed", err)
            return
        }
        writeJSON(w, 200, map[string]any{
            "pid":         pid,
            "output_file": outputFile,
            "detached":    true,
        })
        return
    }

    // ... existing synchronous exec path unchanged ...
}
```

### 3.6 Usage Pattern

```bash
# Launch a long-running agent (returns immediately)
POST /sandboxes/:id/exec
{"cmd": ["bash", "-c", "claude -p 'fix all bugs'"],
 "detach": true,
 "output_file": "/tmp/claude-run.jsonl"}
→ {"pid": 1234, "output_file": "/tmp/claude-run.jsonl", "detached": true}

# Stream the output (use existing synchronous exec with streaming)
POST /sandboxes/:id/exec
Accept: application/x-ndjson
{"cmd": ["tail", "-f", "/tmp/claude-run.jsonl"],
 "timeout_sec": 3600}
→ NDJSON stream of output lines

# Check if still running
POST /sandboxes/:id/exec
{"cmd": ["kill", "-0", "1234"]}
→ {"exit_code": 0}  (running)  or  {"exit_code": 1}  (finished)

# Kill it
POST /sandboxes/:id/exec
{"cmd": ["kill", "1234"]}
```

### 3.7 Verification

- [ ] `{"cmd":["sleep","999"],"detach":true}` returns immediately with
      PID and exit_code 0 in the HTTP response
- [ ] The sleep process is actually running inside the VM (`ps aux`
      shows it)
- [ ] Output file is created and written to
- [ ] `kill -0 <pid>` returns exit_code 0 while running, 1 after done
- [ ] `kill <pid>` terminates the process
- [ ] Process survives vsock connection close (the whole point)
- [ ] Process runs as uid 1000 (lohar user)
- [ ] Zombie is reaped (the `go cmd.Wait()` goroutine)
- [ ] Non-detached exec is completely unchanged
- [ ] Old lohar binaries ignore the `detach` field (JSON backwards compat)

---

## Dependency Graph

```
Part 1 (timeout cap)     — standalone, no deps
Part 2 (idempotent create) — standalone, no deps
Part 3 (detached exec)   — standalone, no deps
```

All three are independent. Ship in any order.

**Recommended order:**

1. **Part 1** first — 5 minutes, one-line change, unblocks agent runs
   > 5 minutes immediately
2. **Part 2** second — 30 minutes, eliminates TOCTOU race and simplifies
   karkhana's create flow
3. **Part 3** third — 1-2 hours, eliminates the setsid+polling
   workaround, requires lohar rebuild + image update

---

## Files Changed

### Part 1 (timeout)
- `pkg/server/exec_handlers.go` — change `3600` to `86400` in cap check, update comment
- `pkg/server/server_test.go` — update `TestExecTimeoutClamped` threshold

### Part 2 (idempotent create)
- `pkg/server/sandbox_handlers.go` — replace 409 with 200+existing, add UNIQUE race fallback
- `pkg/server/server_test.go` — update `TestDuplicateSandboxNameHTTP` expectations

### Part 3 (detached exec)
- `pkg/agent/proto/messages.go` — add `Detach`, `OutputFile` fields to `ExecRequest`
- `cmd/lohar/handler.go` — add dispatch for detached exec
- `cmd/lohar/exec.go` — add `handleDetachedExec`
- `pkg/agent/client.go` — add `ExecDetached` method
- `pkg/engine/engine.go` — add `DetachedExecEngine` interface
- `pkg/engine/firecracker/exec.go` — implement `ExecDetached`
- `pkg/server/exec_handlers.go` — add `Detach`/`OutputFile` to `execReq`, handle detach path

---

## What's Explicitly Not in This Plan

**Task API (Option B for detached exec).** Full process lifecycle
management with SSE streaming, task status, output offset reads. Build
this when you need a real-time dashboard or are managing 10+ concurrent
background tasks per sandbox. The session system (`session.go`,
`EXEC_LIST_REQ`, `SessionAttach`) is the natural foundation — generalize
it to non-TTY processes.

**Exec env/cwd passthrough.** The `execReq` struct doesn't currently
expose `env` or `cwd`. The agent protocol supports both
(`ExecRequest.Env`, `ExecRequest.Cwd`). Adding these to the HTTP API
would be useful but is orthogonal to this plan. Note: `cwd` is added
to `execReq` for detached exec, but not wired through for normal exec.

**Process signal forwarding.** The current KILL frame sends SIGKILL to
the process group. For detached processes, there's no connection to send
KILL on — the caller uses `exec kill <pid>` instead. A proper signal
API (`POST /sandboxes/:id/signal {"pid": 1234, "signal": "SIGTERM"}`)
would be cleaner but is more machinery than needed now.

**Output streaming for detached exec.** The caller uses `tail -f` via
a second exec call. A dedicated output endpoint
(`GET /sandboxes/:id/exec/:pid/output?follow=true`) would be cleaner
but requires tracking PIDs server-side. That's Option B territory.

**Detached exec in non-Firecracker engines.** The `DetachedExecEngine`
interface is only implemented by the Firecracker engine. Docker engine
would need a similar implementation if/when it's used for long-running
agents.
