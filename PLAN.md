# Bhatti — Firecracker Implementation Plan

Migrating the sandbox engine from Docker to Firecracker microVMs. The goal is
true pause/resume (snapshot full VM state — processes, memory, file descriptors)
and strong isolation (separate kernel per sandbox).

The existing `engine.Engine` interface, HTTP server, WebSocket terminal,
`ProxyManager`, and SQLite store remain unchanged. The Firecracker
implementation is a new `Engine` behind the same interface. Docker stays as a
fallback engine for macOS / environments without KVM.

### Why a custom protocol?

With Docker, the Docker daemon provides an API for exec, attach, TTY resize,
and stdin/stdout multiplexing. Bhatti calls the Docker Go client and gets
structured I/O back — Docker IS the protocol layer.

With Firecracker, there is no daemon between the host and the guest. The only
communication channel is vsock — a raw bidirectional byte pipe. No HTTP, no
API, no multiplexing. Someone has to:
- Listen inside the guest for requests ("run this command", "open a shell")
- Multiplex stdout/stderr/exit codes over a single connection
- Handle TTY allocation and resize signals

That someone is the guest agent (Part 2), and the framing format (Part 1) is
how the host and guest understand each other. This replaces what Docker's exec
API gave us for free. The protocol is the same one used by
[shuru](https://github.com/superhq-ai/shuru) — a proven, minimal design.

### Dependency graph

```
Part 1 (proto)     — no dependencies, pure Go, tested on Mac
     ↓
Part 2 (agent)     — imports Part 1, compiles for Linux, tested on Mac via Unix socket
     ↓
Part 3 (client)    — imports Part 1, tested on Mac against Part 2 in test mode
     ↓
Part 4 (images)    — needs Part 2 binary baked into rootfs
     ↓
Part 5 (engine)    — imports Part 3, needs Part 4 images, runs on Pi
     ↓
Part 6 (pi tests)  — integration tests for Part 5 on real hardware
     ↓
Part 7 (persistence) — store changes for crash recovery
```

---

## Part 1 — Guest Agent Protocol

All communication between the bhatti host process and a guest VM happens over
vsock using a binary framing protocol. This protocol is engine-independent — it
can be tested over `net.Pipe()` or a Unix socket without any VM.

### 1.1 Binary Frame Format

```
┌────────────────┬───────────┬──────────────────────┐
│ Length (4 bytes)│ Type (1B) │ Payload (N bytes)    │
│ big-endian     │           │                      │
└────────────────┴───────────┴──────────────────────┘
Length = 1 + len(Payload)    (excludes the 4-byte length prefix itself)
Max frame size: 1 MB (1048576 bytes). ReadFrame must reject lengths > 1MB.
```

**Writes must be atomic.** `WriteFrame` must assemble the full frame (4-byte
header + 1-byte type + payload) into a single buffer and write it in one
`Write()` call. This prevents interleaved partial frames when multiple
goroutines write to the same connection (the agent's piped exec has stdout
and stderr goroutines writing concurrently). The caller must still hold a
mutex if multiple goroutines call `WriteFrame` on the same writer — assembling
into one buffer just avoids torn frames at the TCP/vsock level.

### 1.2 Frame Types

```
I/O streams:
  STDIN        = 0x01    host → guest: bytes for child stdin
  STDOUT       = 0x02    guest → host: child stdout bytes
  STDERR       = 0x03    guest → host: child stderr bytes

Control:
  RESIZE       = 0x04    host → guest: [u16 rows][u16 cols] big-endian (4 bytes exactly)
  EXIT         = 0x05    guest → host: [i32 exit_code] big-endian (4 bytes exactly)
  ERROR        = 0x06    either direction: UTF-8 error message (variable length)
  KILL         = 0x07    host → guest: empty payload, agent sends SIGTERM to child

Exec:
  EXEC_REQ     = 0x10    host → guest: JSON-encoded ExecRequest

Port forwarding:
  FWD_REQ      = 0x20    host → guest: JSON-encoded ForwardRequest
  FWD_RESP     = 0x21    guest → host: JSON-encoded ForwardResponse
```

**Connection lifecycle for control (port 1024):** The host opens a new vsock
connection for each operation. It sends exactly one EXEC_REQ. The guest handles
it (spawning a process, streaming I/O) and the connection closes when the
process exits or the host disconnects. One connection = one command.

**Connection lifecycle for forwarding (port 1025):** The host opens a new vsock
connection for each forwarded port. It sends FWD_REQ, reads FWD_RESP, then the
connection becomes an unframed raw byte stream (bidirectional TCP relay). The
connection closes when either side disconnects. One connection = one tunnel.

### 1.3 Message Structs

```go
// pkg/agent/proto/messages.go

type ExecRequest struct {
    Argv []string          `json:"argv"`
    Env  map[string]string `json:"env,omitempty"`
    TTY  *bool             `json:"tty,omitempty"`   // nil = false
    Rows *uint16           `json:"rows,omitempty"`  // only used when TTY=true, default 24
    Cols *uint16           `json:"cols,omitempty"`  // only used when TTY=true, default 80
    Cwd  *string           `json:"cwd,omitempty"`   // nil = agent's cwd (/)
}

type ForwardRequest struct {
    Port uint16 `json:"port"`
}

type ForwardResponse struct {
    Status  string  `json:"status"`            // "ok" or "error"
    Message *string `json:"message,omitempty"` // error detail when Status="error"
}
```

### 1.4 Files to Create

```
pkg/agent/proto/
  frame.go        — WriteFrame, ReadFrame, TryParse, SendJSON
  frame_test.go   — round-trip tests, max-size enforcement, partial-read tests
  messages.go     — ExecRequest, ForwardRequest, ForwardResponse structs
  constants.go    — frame type constants, vsock port numbers
```

**`constants.go`:**

```go
package proto

// Frame types
const (
    STDIN    byte = 0x01
    STDOUT   byte = 0x02
    STDERR   byte = 0x03
    RESIZE   byte = 0x04
    EXIT     byte = 0x05
    ERROR    byte = 0x06
    KILL     byte = 0x07
    EXEC_REQ byte = 0x10
    FWD_REQ  byte = 0x20
    FWD_RESP byte = 0x21
)

// Vsock ports
const (
    VsockPortControl = uint32(1024) // exec, shell
    VsockPortForward = uint32(1025) // port forwarding
)

// MaxFrameSize is the maximum allowed frame size (1 MB).
const MaxFrameSize = 1 << 20
```

**`frame.go` function signatures:**

```go
package proto

import "io"

// WriteFrame writes a single frame: [4-byte length BE][1-byte type][payload].
// The entire frame is assembled into one buffer before writing.
// Returns error if payload exceeds MaxFrameSize.
func WriteFrame(w io.Writer, msgType byte, payload []byte) error

// ReadFrame reads one frame. Returns io.EOF on clean end-of-stream (0 bytes
// available). Returns io.ErrUnexpectedEOF if the stream ends mid-frame.
// Returns an error if the frame length exceeds MaxFrameSize.
func ReadFrame(r io.Reader) (msgType byte, payload []byte, err error)

// TryParse checks whether buf contains a complete frame at the front.
// If yes, returns the type, the offset where payload starts (always 5),
// and the total frame length (4 + 1 + payload_len). ok=false if buf is
// too short for a complete frame or the length exceeds MaxFrameSize.
// Used by the TTY poll loop in the agent where data arrives incrementally.
func TryParse(buf []byte) (msgType byte, payloadStart int, totalLen int, ok bool)

// SendJSON JSON-encodes v and sends it as a typed frame.
func SendJSON(w io.Writer, msgType byte, v any) error

// ResizePayload encodes terminal dimensions as [u16 rows BE][u16 cols BE].
func ResizePayload(rows, cols uint16) [4]byte

// ParseResize decodes a RESIZE payload. Returns ok=false if len < 4.
func ParseResize(payload []byte) (rows, cols uint16, ok bool)

// ExitPayload encodes an exit code as [i32 BE].
func ExitPayload(code int32) [4]byte

// ParseExitCode decodes an EXIT payload. Returns ok=false if len < 4.
func ParseExitCode(payload []byte) (int32, bool)
```

### 1.5 Testing (Mac)

All proto tests run on Mac with `go test ./pkg/agent/proto/`:

- `TestWriteReadRoundTrip` — write a frame to a `bytes.Buffer`, read it back,
  verify type + payload match. Test with empty payload, 1-byte payload, and a
  large payload (64KB).
- `TestReadFrameEOF` — `ReadFrame` on an empty `bytes.Reader` returns `io.EOF`
  (not `io.ErrUnexpectedEOF`)
- `TestReadFrameUnexpectedEOF` — write only the 4-byte length header then EOF.
  `ReadFrame` must return `io.ErrUnexpectedEOF`.
- `TestMaxFrameSize` — `WriteFrame` with payload > 1MB returns an error.
  `ReadFrame` on a buffer containing a frame with length header = 2MB returns
  an error.
- `TestTryParsePartial` — buffer has only 3 bytes: returns `ok=false`. Buffer
  has a complete frame: returns correct values. Buffer has a frame plus extra
  bytes: `totalLen` doesn't include the extra bytes.
- `TestSendJSON` — `SendJSON` with an `ExecRequest`, then `ReadFrame` +
  `json.Unmarshal`, verify fields match.
- `TestResizePayload` — encode rows=24, cols=80, decode back, verify. Also
  test rows=0, cols=0 and max uint16 values.
- `TestExitPayload` — encode/decode exit codes: 0, 1, -1, 127, 137 (128+9,
  SIGKILL), 143 (128+15, SIGTERM).
- `TestConcurrentWrites` — two goroutines each write 1000 frames to the same
  `bytes.Buffer` (protected by a mutex). Read all 2000 frames back. None
  should be corrupt.

---

## Part 2 — Guest Agent Binary

A static Linux binary that runs as PID 1 (init) inside the microVM. Compiled
from Go with `CGO_ENABLED=0`. Does not depend on libc, systemd, or any distro
packages.

**File:** `cmd/bhatti-agent/main.go` (plus internal packages as needed)

**Build constraint:** The PID 1 init code (mount, vsock, PTY) uses Linux-only
syscalls. Use a build tag or `_linux.go` suffix so the package compiles on Mac
(for `go vet` / IDE support) but the Linux-specific code only compiles for
`GOOS=linux`. The test-mode entry point (section 2.9) works on all platforms.

### 2.1 Boot Sequence (PID 1 responsibilities)

```go
// cmd/bhatti-agent/main.go

func main() {
    // Test mode: skip PID 1 duties, listen on Unix socket (see 2.9)
    if os.Getenv("BHATTI_AGENT_TEST") == "1" {
        runTestMode()
        return
    }

    // --- PID 1 init (Linux only) ---

    // 1. Mount essential filesystems
    //    These don't exist yet — the kernel provides an empty rootfs.
    //    Without /proc, tools like `ps`, `ss`, `kill` don't work.
    //    Without /dev, there are no device nodes.
    //    Without /dev/pts, PTY allocation fails.
    mustMount("proc",     "/proc",    "proc",    0, "")
    mustMount("sysfs",    "/sys",     "sysfs",   0, "")
    mustMount("devtmpfs", "/dev",     "devtmpfs",0, "")
    os.MkdirAll("/dev/pts", 0755)
    mustMount("devpts",   "/dev/pts", "devpts",  0, "newinstance,ptmxmode=0666")
    mustMount("tmpfs",    "/tmp",     "tmpfs",   0, "")
    mustMount("tmpfs",    "/run",     "tmpfs",   0, "")

    // 2. Set hostname
    syscall.Sethostname([]byte("bhatti"))

    // 3. Bring up loopback interface (lo)
    //    Without this, localhost/127.0.0.1 doesn't work inside the VM.
    //    Uses raw ioctl: socket(AF_INET, SOCK_DGRAM) → SIOCGIFFLAGS →
    //    set IFF_UP → SIOCSIFFLAGS. See bringUpInterface() below.
    bringUpInterface("lo")

    // 4. Set up networking
    //    The Firecracker SDK's StaticNetworkConfiguration passes IP config
    //    via kernel cmdline args that the guest must parse, OR the IP is
    //    configured by an initramfs script before switch_root.
    //
    //    Simpler approach: the Firecracker SDK sets ip= on the kernel
    //    cmdline (ip=<guest>::<gateway>:<mask>::eth0:off). The kernel's
    //    built-in IP autoconfiguration handles it before init runs.
    //    The agent just needs to verify eth0 is up.
    //
    //    If eth0 doesn't exist (no network device attached), that's fine —
    //    the sandbox runs without internet. Vsock still works regardless.
    setupNetworking()

    // 5. Register signal handlers
    installSignalHandlers()

    // 6. Start vsock listeners
    go acceptLoop(listenVsock(proto.VsockPortControl), handleControlConnection)
    go acceptLoop(listenVsock(proto.VsockPortForward), handleForwardConnection)

    // 7. Block forever, reaping zombies.
    //    PID 1 must never exit. If it does, the kernel panics.
    //    The blocking Wait4 call also reaps orphaned child processes.
    for {
        var status syscall.WaitStatus
        syscall.Wait4(-1, &status, 0, nil)
    }
}

// mustMount wraps syscall.Mount and logs on failure (but doesn't crash —
// some mounts may fail in test/degraded environments and that's ok).
func mustMount(source, target, fstype string, flags uintptr, data string) {
    os.MkdirAll(target, 0755)
    if err := syscall.Mount(source, target, fstype, flags, data); err != nil {
        fmt.Fprintf(os.Stderr, "bhatti-agent: mount %s on %s: %v\n", source, target, err)
    }
}
```

### 2.2 Control Connection Handler (port 1024)

Each connection from the host reads frames in a loop. One connection handles
one operation (exec, shell, etc.) then closes.

```
handleControlConnection(conn):
  loop:
    frame = ReadFrame(conn)
    switch frame.Type:

      EXEC_REQ:
        req = JSON decode ExecRequest
        if req.TTY:
          handleTTYExec(conn, req)    // allocate PTY, fork, relay
          return                       // connection is consumed
        else:
          handlePipedExec(conn, req)  // pipe stdin/stdout/stderr
          return

      // Future: MOUNT_REQ, READ_FILE_REQ, WRITE_FILE_REQ, WATCH_REQ
```

### 2.3 Piped Exec (non-TTY)

For `engine.Exec()` — run a command, capture stdout/stderr, return exit code.

```
handlePipedExec(conn, req):
  cmd = exec.Command(req.Argv[0], req.Argv[1:]...)
  cmd.Env = buildEnv(req.Env)    // merge req.Env with defaults (PATH, TERM, HOME)
  cmd.Dir = derefOr(req.Cwd, "/")
  cmd.Stdin = pipe
  cmd.Stdout = pipe
  cmd.Stderr = pipe
  cmd.Start()

  // Serialize all frame writes through a channel to prevent interleaving.
  // stdout and stderr goroutines send (type, payload) to the channel.
  // A single writer goroutine drains the channel and calls WriteFrame.
  tx = make(chan (byte, []byte))

  // Writer goroutine: serializes all frames to conn
  go func():
    for (msgType, payload) in tx:
      WriteFrame(conn, msgType, payload)

  // Thread: child stdout → STDOUT frames
  go func():
    buf = [8192]byte
    loop:
      n = cmd.Stdout.Read(buf)
      if n == 0 or err: break
      tx <- (STDOUT, buf[:n])

  // Thread: child stderr → STDERR frames
  go func():
    buf = [8192]byte
    loop:
      n = cmd.Stderr.Read(buf)
      if n == 0 or err: break
      tx <- (STDERR, buf[:n])

  // Thread: STDIN/KILL frames from conn → child stdin
  //   For non-interactive exec, the host usually doesn't send STDIN.
  //   But the KILL frame is how the host cancels a long-running command.
  go func():
    loop:
      msgType, payload = ReadFrame(conn)
      if msgType == STDIN: cmd.Stdin.Write(payload)
      if msgType == KILL:  cmd.Process.Signal(SIGTERM); break
      if err (conn closed): cmd.Stdin.Close(); break

  // Wait for stdout+stderr goroutines to finish, THEN wait for child.
  // This ensures we don't send EXIT before all output is flushed.
  stdout_thread.Join()
  stderr_thread.Join()
  exitCode = cmd.Wait()
  sync()   // flush filesystem writes before host might snapshot
  tx <- (EXIT, ExitPayload(exitCode))
  close(tx)
```

**Important: the `sync()` call.** `syscall.Sync()` flushes all pending
filesystem writes to the ext4 disk image. Without this, if the host snapshots
the VM immediately after receiving the EXIT frame, data written by the command
may be lost (still in the kernel page cache, not yet on the virtual disk).
Shuru does the same thing.

### 2.4 TTY Exec (interactive shell)

For `engine.Shell()` — allocate a PTY, relay I/O bidirectionally.

**Go implementation note:** Go's `os/exec` doesn't support PTY allocation
natively. Use the `github.com/creack/pty` package which provides
`pty.StartWithSize(cmd, &pty.Winsize{Rows: rows, Cols: cols})`. This returns
the master fd. However, `creack/pty` uses cgo on some platforms. Since the
agent is `CGO_ENABLED=0`, you may need to use raw syscalls:
`syscall.Openpty()` doesn't exist in Go stdlib, so use `open("/dev/ptmx")` +
`grantpt` + `unlockpt` + `ptsname`, or use the simpler approach: call
`os/exec.Command` with `cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true,
Setctty: true, Ctty: slaveFd}` after opening a PTY pair via
`posix_openpt`/ioctl. See Shuru's `handle_tty_exec` in `shuru-guest/main.rs`
for the exact syscall sequence — the Go equivalent is identical.

Alternatively, the simplest approach that works with `CGO_ENABLED=0`:

```go
// Open PTY master
master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
// grantpt + unlockpt via ioctl
syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCGPTN, ...)
syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), TIOCSPTLCK, ...)
// Get slave path: /dev/pts/N
slavePath := fmt.Sprintf("/dev/pts/%d", ptsNum)
slave, _ := os.OpenFile(slavePath, os.O_RDWR, 0)
```

```
handleTTYExec(conn, req):
  // Open PTY pair
  master, slave = openPTY()
  setWinsize(master, rows=derefOr(req.Rows, 24), cols=derefOr(req.Cols, 80))

  pid = fork()   // or: exec.Command with SysProcAttr
  if child:
    close(master)
    setsid()                        // new session
    ioctl(slave, TIOCSCTTY, 0)      // make slave the controlling terminal
    dup2(slave, 0)                  // stdin
    dup2(slave, 1)                  // stdout
    dup2(slave, 2)                  // stderr
    if slave > 2: close(slave)
    close all fds > 2 (prevent leaking vsock fds to child)
    chdir(req.Cwd)
    setenv(req.Env)
    setenv("TERM", "xterm-256color")  // if not in req.Env
    setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
    execvp(req.Argv[0], req.Argv)
    // if exec fails: write error to stderr (fd 2 = slave = PTY), exit 127

  // Parent: close slave end, run poll loop
  close(slave)

  // Use a single-threaded poll loop (not goroutines) because we need to
  // interleave reads from two fds (vsock conn + PTY master) without races.
  // This matches Shuru's pty_poll_loop design.
  //
  // vsock_buf accumulates partial frames from the host. We call TryParse
  // to extract complete frames as they arrive.

  vsock_buf = []byte{}
  loop:
    fds = poll([conn_fd, master_fd], timeout=200ms)

    if conn_fd readable:
      data = read(conn_fd, 4096)
      if data == 0: kill(pid, SIGHUP); break   // host disconnected
      vsock_buf = append(vsock_buf, data)

      // Process complete frames from buffer
      while TryParse(vsock_buf) → (msgType, payloadStart, totalLen, ok):
        payload = vsock_buf[payloadStart:totalLen]
        if msgType == STDIN:  write(master_fd, payload)
        if msgType == RESIZE:
          rows, cols = ParseResize(payload)
          ioctl(master_fd, TIOCSWINSZ, {rows, cols})
        vsock_buf = vsock_buf[totalLen:]

    if conn_fd HUP/ERR:
      kill(pid, SIGHUP)    // host gone, terminate child
      break

    if master_fd readable:
      data = read(master_fd, 4096)
      if data > 0:
        WriteFrame(conn, STDOUT, data)

    if master_fd HUP (child closed PTY):
      // Drain remaining output
      while read(master_fd) > 0:
        WriteFrame(conn, STDOUT, data)
      break

  // Collect exit status
  waitpid(pid) → status
  exit_code = WEXITSTATUS(status) or 128+WTERMSIG(status)
  sync()
  WriteFrame(conn, EXIT, ExitPayload(exit_code))
  close(master_fd)
  close(conn)
```

PTY management happens entirely in the guest. The host sees only STDIN/STDOUT/
RESIZE/EXIT frames — same as the piped case but with TTY semantics.

**Why poll loop instead of goroutines for TTY?** The vsock connection and PTY
master are raw file descriptors. We need to detect HUP (hangup) on both sides,
which Go's goroutine-per-fd model doesn't handle cleanly. A `poll()` syscall
with two fds + 200ms timeout gives us: low-latency I/O, HUP detection, and
the ability to break out when the child exits. This is exactly what Shuru does.

### 2.5 Port Forward Handler (port 1025)

Each connection handles one forwarded port:

```
handleForwardConnection(conn):
  frame = ReadFrame(conn)   // expect FWD_REQ
  req = JSON decode ForwardRequest

  tcp = net.Dial("tcp", "127.0.0.1:" + req.Port)
  if error:
    SendJSON(conn, FWD_RESP, {Status: "error", Message: err})
    return

  SendJSON(conn, FWD_RESP, {Status: "ok"})

  // Bidirectional relay — raw bytes, no framing after handshake
  go io.Copy(tcp, conn)
  io.Copy(conn, tcp)
```

After the FWD_RESP handshake, the vsock connection becomes a raw TCP tunnel.
No further framing. This is what `engine.Tunnel()` returns to the caller.

### 2.6 vsock Listener Setup (Linux AF_VSOCK)

```go
// AF_VSOCK and sockaddr_vm are not in Go's syscall package.
// Define them manually.
const (
    AF_VSOCK       = 40
    VMADDR_CID_ANY = 0xFFFFFFFF
)

// SockaddrVM is the C struct sockaddr_vm, packed for bind().
type SockaddrVM struct {
    Family    uint16   // AF_VSOCK
    Reserved1 uint16
    Port      uint32
    CID       uint32
    Flags     uint8
    Zero      [3]uint8
}

func listenVsock(port uint32) (net.Listener, error) {
    fd, err := syscall.Socket(AF_VSOCK, syscall.SOCK_STREAM, 0)
    if err != nil {
        return nil, fmt.Errorf("vsock socket: %w", err)
    }

    addr := SockaddrVM{
        Family: AF_VSOCK,
        Port:   port,
        CID:    VMADDR_CID_ANY,
    }
    // Use RawSockaddrAny and unsafe.Pointer to call bind, since Go's
    // syscall.Bind doesn't know about AF_VSOCK.
    _, _, errno := syscall.Syscall(syscall.SYS_BIND, uintptr(fd),
        uintptr(unsafe.Pointer(&addr)), unsafe.Sizeof(addr))
    if errno != 0 {
        syscall.Close(fd)
        return nil, fmt.Errorf("vsock bind port %d: %w", port, errno)
    }

    if err := syscall.Listen(fd, 128); err != nil {
        syscall.Close(fd)
        return nil, fmt.Errorf("vsock listen: %w", err)
    }

    // Wrap the raw fd as a net.Listener using os.NewFile + net.FileListener
    f := os.NewFile(uintptr(fd), fmt.Sprintf("vsock:%d", port))
    ln, err := net.FileListener(f)
    f.Close() // FileListener dups the fd
    if err != nil {
        return nil, fmt.Errorf("vsock file listener: %w", err)
    }
    return ln, nil
}
```

**Alternative:** The `github.com/mdlayher/vsock` package provides
`vsock.Listen(port, nil)` which does this cleanly. It works with
`CGO_ENABLED=0`. Either the manual approach above or `mdlayher/vsock` works —
the manual approach has zero dependencies but is more code.

### 2.7 Signal Handling

```go
func installSignalHandlers() {
    // SIGCHLD: PID 1 must reap adopted orphans
    sigchld := make(chan os.Signal, 32)
    signal.Notify(sigchld, syscall.SIGCHLD)
    go func() {
        for range sigchld {
            for {
                pid, _ := syscall.Wait4(-1, nil, syscall.WNOHANG, nil)
                if pid <= 0 { break }
            }
        }
    }()

    // SIGTERM/SIGINT: clean shutdown
    sigterm := make(chan os.Signal, 1)
    signal.Notify(sigterm, syscall.SIGTERM, syscall.SIGINT)
    go func() {
        <-sigterm
        syscall.Sync()
        syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF)
    }()
}
```

### 2.8 Build

```makefile
# Cross-compile from Mac. Static binary, no libc dependency.
# -s strips symbol table, -w strips DWARF — reduces binary from ~15MB to ~8MB.
# CGO_ENABLED=0 ensures pure Go — no dynamic linking, runs on any Linux.
# -extldflags "-static" is redundant with CGO_ENABLED=0 but harmless.
build-agent:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build \
		-ldflags='-s -w' \
		-o bin/bhatti-agent-linux-arm64 \
		./cmd/bhatti-agent

# Verify it's static:
#   file bin/bhatti-agent-linux-arm64
#   → ELF 64-bit LSB executable, ARM aarch64, statically linked
```

**Note:** If you use `mdlayher/vsock` for the vsock listener (section 2.6
alternative), verify it compiles with `CGO_ENABLED=0`. As of v0.4.x it does.
If you use `creack/pty` for PTY allocation (section 2.4), it requires cgo on
Linux — in that case, use the raw syscall approach instead.

### 2.9 Testing the Agent Without a VM

Before any Firecracker involvement, test the agent over a Unix socket.

**Test mode entry point:**

```go
// cmd/bhatti-agent/testmode.go (no build tags — runs on Mac and Linux)

func runTestMode() {
    controlSock := os.Getenv("BHATTI_AGENT_SOCK")
    forwardSock := os.Getenv("BHATTI_AGENT_FWD_SOCK")
    if controlSock == "" || forwardSock == "" {
        fmt.Fprintln(os.Stderr, "BHATTI_AGENT_SOCK and BHATTI_AGENT_FWD_SOCK required")
        os.Exit(1)
    }

    os.Remove(controlSock)
    os.Remove(forwardSock)

    lnControl, _ := net.Listen("unix", controlSock)
    lnForward, _ := net.Listen("unix", forwardSock)

    go acceptLoop(lnControl, handleControlConnection)
    go acceptLoop(lnForward, handleForwardConnection)

    // Block forever. Tests kill the process when done.
    select {}
}
```

**Test harness (`cmd/bhatti-agent/agent_test.go`):**

The tests start the agent as a subprocess in test mode, connect to its Unix
socket, and exercise the protocol. Each test gets a fresh agent process.

```go
func startTestAgent(t *testing.T) (controlConn, forwardConn net.Conn, cleanup func()) {
    t.Helper()
    dir := t.TempDir()
    controlSock := filepath.Join(dir, "control.sock")
    forwardSock := filepath.Join(dir, "forward.sock")

    // Start agent subprocess
    cmd := exec.Command(os.Args[0], "-test.run=TestHelperAgent")
    cmd.Env = append(os.Environ(),
        "BHATTI_AGENT_TEST=1",
        "BHATTI_AGENT_SOCK="+controlSock,
        "BHATTI_AGENT_FWD_SOCK="+forwardSock,
        "GO_WANT_HELPER_PROCESS=1",
    )
    cmd.Start()

    // Wait for socket to exist
    for i := 0; i < 50; i++ {
        if _, err := os.Stat(controlSock); err == nil { break }
        time.Sleep(20 * time.Millisecond)
    }

    controlConn, _ = net.Dial("unix", controlSock)
    forwardConn, _ = net.Dial("unix", forwardSock)

    return controlConn, forwardConn, func() {
        controlConn.Close()
        forwardConn.Close()
        cmd.Process.Kill()
        cmd.Wait()
    }
}

// TestHelperAgent is the subprocess entry point (not a real test).
func TestHelperAgent(t *testing.T) {
    if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" { return }
    runTestMode()
}
```

**Test cases** (all run on Mac via `go test ./cmd/bhatti-agent/`):

Non-TTY exec tests (these work identically on Mac and Linux):

- `TestAgentExec` — send EXEC_REQ `["echo", "hello"]`, read STDOUT frame
  `"hello\n"`, read EXIT frame with code 0
- `TestAgentExecFailure` — send EXEC_REQ `["false"]`, read EXIT with code 1
- `TestAgentExecNotFound` — send EXEC_REQ `["/nonexistent"]`, read ERROR frame
  (agent can't spawn the process)
- `TestAgentExecStderr` — send EXEC_REQ `["sh", "-c", "echo err >&2"]`, read
  STDERR frame containing `"err\n"`, then EXIT code 0
- `TestAgentExecEnv` — send EXEC_REQ with env `{"FOO": "bar"}` and argv
  `["sh", "-c", "echo $FOO"]`, verify STDOUT is `"bar\n"`
- `TestAgentExecCwd` — send EXEC_REQ with cwd `/tmp`, argv `["pwd"]`, verify
  STDOUT is `"/tmp\n"`
- `TestAgentExecLargeOutput` — send EXEC_REQ `["dd", "if=/dev/zero",
  "bs=1024", "count=1024"]` (1MB output), read all STDOUT frames, verify total
  bytes = 1MB. This tests that large outputs are correctly streamed across
  multiple frames.
- `TestAgentKill` — send EXEC_REQ `["sleep", "60"]`, wait 100ms, send KILL
  frame, read EXIT frame, verify exit code is 128+15 (SIGTERM)

TTY tests (these work on Mac since `/bin/sh` and PTYs exist on macOS too,
but the PTY code path in the agent uses Linux syscalls — so TTY tests may
need to run the agent in a Linux Docker container, or skip on Mac and run
only on Pi. Mark with `//go:build linux`):

- `TestAgentTTY` — send EXEC_REQ with tty=true, argv `["/bin/sh"]`, write
  STDIN frame `"echo hello\n"`, read STDOUT frames until `"hello"` appears,
  write STDIN `"exit\n"`, read EXIT frame
- `TestAgentTTYResize` — send EXEC_REQ with tty=true, write STDIN
  `"stty size\n"` → read output → send RESIZE(40, 120) → write STDIN
  `"stty size\n"` again → verify output contains `"40 120"`

Port forwarding test:

- `TestAgentForward` — start a TCP listener on localhost:9999 in the test
  process, connect to the agent's forward socket, send FWD_REQ port=9999, read
  FWD_RESP "ok", write `"ping"` through the tunnel, read `"pong"` back (the
  test's TCP handler echoes with transformation). Then close and verify cleanup.
- `TestAgentForwardRefused` — send FWD_REQ for a port nobody is listening on,
  verify FWD_RESP has status="error"

---

## Part 3 — Host-Side Agent Client

A Go package that connects to the guest agent over vsock and translates the
framed protocol into the `engine.Engine` interface methods.

### 3.1 Files

```
pkg/agent/
  client.go       — AgentClient: Exec, Shell, Forward methods
  client_test.go  — tests using the same Unix-socket agent from Part 2
```

### 3.2 AgentClient

```go
// pkg/agent/client.go

// AgentClient communicates with the guest agent running inside a microVM.
//
// In production, vsockPath is the Firecracker vsock UDS and the client
// performs the CONNECT/OK handshake (see 3.3). In tests, controlSock and
// forwardSock are plain Unix sockets and no handshake is needed.
type AgentClient struct {
    controlSock string // for exec/shell (vsock port 1024, or test UDS)
    forwardSock string // for port forwarding (vsock port 1025, or test UDS)
    isVsock     bool   // true = Firecracker vsock (needs CONNECT handshake)
}

// NewVsockClient creates a client that connects through a Firecracker vsock UDS.
// The vsockPath is the UDS file created by Firecracker (e.g. /path/to/vsock.sock).
// Each call to Exec/Shell/Forward dials a new connection through this UDS.
func NewVsockClient(vsockPath string) *AgentClient {
    return &AgentClient{
        controlSock: vsockPath,
        forwardSock: vsockPath,
        isVsock:     true,
    }
}

// NewTestClient creates a client that connects to the agent's test-mode
// Unix sockets directly (no vsock handshake).
func NewTestClient(controlSock, forwardSock string) *AgentClient {
    return &AgentClient{
        controlSock: controlSock,
        forwardSock: forwardSock,
        isVsock:     false,
    }
}

// Exec runs a command non-interactively. Returns after the command exits.
// Maps to engine.Exec().
func (c *AgentClient) Exec(ctx context.Context, argv []string, env map[string]string, cwd string) (engine.ExecResult, error)

// Shell opens an interactive TTY session. Returns a TerminalConn.
// Maps to engine.Shell().
func (c *AgentClient) Shell(ctx context.Context, argv []string, env map[string]string, rows, cols uint16) (engine.TerminalConn, error)

// Forward opens a raw TCP tunnel to a port inside the guest.
// Maps to engine.Tunnel().
func (c *AgentClient) Forward(ctx context.Context, port uint16) (io.ReadWriteCloser, error)

// WaitReady polls the agent until it responds, or the timeout expires.
// Used during VM boot to wait for the agent to start listening.
func (c *AgentClient) WaitReady(ctx context.Context, timeout time.Duration) error
```

### 3.3 Connecting to vsock from the host

Firecracker exposes vsock as a Unix domain socket on the host. To connect to a
guest-side listener on port N:

```go
// dialControl opens a connection to the control channel (port 1024).
func (c *AgentClient) dialControl() (net.Conn, error) {
    if c.isVsock {
        return c.dialVsockPort(c.controlSock, proto.VsockPortControl)
    }
    return net.Dial("unix", c.controlSock)
}

// dialForward opens a connection to the forward channel (port 1025).
func (c *AgentClient) dialForward() (net.Conn, error) {
    if c.isVsock {
        return c.dialVsockPort(c.forwardSock, proto.VsockPortForward)
    }
    return net.Dial("unix", c.forwardSock)
}

// dialVsockPort performs the Firecracker vsock handshake.
// Firecracker vsock docs: https://github.com/firecracker-microvm/firecracker/blob/main/docs/vsock.md
func (c *AgentClient) dialVsockPort(udsPath string, port uint32) (net.Conn, error) {
    conn, err := net.Dial("unix", udsPath)
    if err != nil {
        return nil, fmt.Errorf("vsock dial %s: %w", udsPath, err)
    }

    // Firecracker vsock protocol:
    // 1. Write "CONNECT <port>\n"
    if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
        conn.Close()
        return nil, fmt.Errorf("vsock CONNECT write: %w", err)
    }

    // 2. Read "OK <assigned_hostside_port>\n"
    reader := bufio.NewReaderSize(conn, 64)
    line, err := reader.ReadString('\n')
    if err != nil {
        conn.Close()
        return nil, fmt.Errorf("vsock CONNECT read: %w", err)
    }
    if !strings.HasPrefix(line, "OK ") {
        conn.Close()
        return nil, fmt.Errorf("vsock handshake failed: %q", strings.TrimSpace(line))
    }

    // 3. Connection is now a raw bidirectional byte stream to guest port.
    //    IMPORTANT: the bufio.Reader may have buffered extra bytes beyond
    //    the "OK" line. In practice Firecracker sends exactly "OK <port>\n"
    //    and nothing more, so the reader's buffer is empty. But to be safe,
    //    if reader.Buffered() > 0, those bytes belong to the first protocol
    //    frame and must be prepended. For simplicity, use a small reader
    //    (64 bytes) that won't over-read.
    return conn, nil
}
```

**Alternative:** The `firecracker-go-sdk/vsock` package provides
`vsock.Dial(path, port)` which does this handshake with configurable retries
and timeouts. It handles edge cases (temporary errors, partial reads). Use it
instead of hand-rolling if you prefer — add to go.mod:
`github.com/firecracker-microvm/firecracker-go-sdk`.

### 3.4 Exec Implementation

```go
func (c *AgentClient) Exec(ctx context.Context, argv []string, env map[string]string, cwd string) (engine.ExecResult, error) {
    conn, err := c.dialControl()
    if err != nil {
        return engine.ExecResult{}, fmt.Errorf("agent connect: %w", err)
    }
    defer conn.Close()

    // Apply context deadline to the connection so reads/writes cancel.
    if deadline, ok := ctx.Deadline(); ok {
        conn.SetDeadline(deadline)
    }

    var cwdPtr *string
    if cwd != "" {
        cwdPtr = &cwd
    }
    req := proto.ExecRequest{Argv: argv, Env: env, Cwd: cwdPtr}
    if err := proto.SendJSON(conn, proto.EXEC_REQ, req); err != nil {
        return engine.ExecResult{}, fmt.Errorf("agent send exec: %w", err)
    }

    var stdout, stderr bytes.Buffer

    for {
        msgType, payload, err := proto.ReadFrame(conn)
        if err != nil {
            return engine.ExecResult{}, fmt.Errorf("agent read: %w", err)
        }

        switch msgType {
        case proto.STDOUT:
            stdout.Write(payload)
        case proto.STDERR:
            stderr.Write(payload)
        case proto.EXIT:
            exitCode, _ := proto.ParseExitCode(payload)
            return engine.ExecResult{
                ExitCode: int(exitCode),
                Stdout:   stdout.String(),
                Stderr:   stderr.String(),
            }, nil
        case proto.ERROR:
            return engine.ExecResult{}, fmt.Errorf("agent error: %s", payload)
        default:
            // Unknown frame type — skip (forward compatibility)
        }
    }
}
```

### 3.5 Shell Implementation

```go
func (c *AgentClient) Shell(ctx context.Context, argv []string, env map[string]string, rows, cols uint16) (engine.TerminalConn, error) {
    conn, err := c.dialControl()
    if err != nil {
        return nil, fmt.Errorf("agent connect: %w", err)
    }

    req := proto.ExecRequest{
        Argv: argv, Env: env,
        TTY: boolPtr(true), Rows: &rows, Cols: &cols,
    }
    if err := proto.SendJSON(conn, proto.EXEC_REQ, req); err != nil {
        conn.Close()
        return nil, fmt.Errorf("agent send shell: %w", err)
    }

    return &agentTermConn{conn: conn}, nil
}

// agentTermConn wraps the vsock connection as engine.TerminalConn.
// Read  → reads STDOUT frames, strips framing, returns raw bytes
// Write → wraps raw bytes in STDIN frames, writes to conn
// Resize → sends a RESIZE frame
// Close → closes the connection
type agentTermConn struct {
    conn   net.Conn
    mu     sync.Mutex     // serializes writes
    readBuf bytes.Buffer  // leftover payload from previous STDOUT frame
}

func (t *agentTermConn) Read(p []byte) (int, error) {
    // If we have leftover from a previous frame, return that first
    if t.readBuf.Len() > 0 {
        return t.readBuf.Read(p)
    }
    // Read next frame — expecting STDOUT, EXIT, or ERROR
    msgType, payload, err := proto.ReadFrame(t.conn)
    if err != nil { return 0, err }
    switch msgType {
    case proto.STDOUT:
        n := copy(p, payload)
        if n < len(payload) {
            t.readBuf.Write(payload[n:])
        }
        return n, nil
    case proto.EXIT:
        return 0, io.EOF
    case proto.ERROR:
        return 0, fmt.Errorf("agent: %s", payload)
    default:
        return 0, fmt.Errorf("unexpected frame type: 0x%02x", msgType)
    }
}

func (t *agentTermConn) Write(p []byte) (int, error) {
    t.mu.Lock()
    defer t.mu.Unlock()
    err := proto.WriteFrame(t.conn, proto.STDIN, p)
    if err != nil { return 0, err }
    return len(p), nil
}

func (t *agentTermConn) Resize(rows, cols int) error {
    t.mu.Lock()
    defer t.mu.Unlock()
    payload := proto.ResizePayload(uint16(rows), uint16(cols))
    return proto.WriteFrame(t.conn, proto.RESIZE, payload[:])
}

func (t *agentTermConn) Close() error {
    return t.conn.Close()
}
```

### 3.6 Forward (Tunnel) Implementation

```go
func (c *AgentClient) Forward(ctx context.Context, port uint16) (io.ReadWriteCloser, error) {
    conn, err := c.dialForward()
    if err != nil {
        return nil, fmt.Errorf("agent forward connect: %w", err)
    }

    req := proto.ForwardRequest{Port: port}
    if err := proto.SendJSON(conn, proto.FWD_REQ, req); err != nil {
        conn.Close()
        return nil, fmt.Errorf("agent send forward: %w", err)
    }

    msgType, payload, err := proto.ReadFrame(conn)
    if err != nil {
        conn.Close()
        return nil, fmt.Errorf("agent forward read: %w", err)
    }
    if msgType != proto.FWD_RESP {
        conn.Close()
        return nil, fmt.Errorf("expected FWD_RESP, got 0x%02x", msgType)
    }
    var resp proto.ForwardResponse
    if err := json.Unmarshal(payload, &resp); err != nil {
        conn.Close()
        return nil, fmt.Errorf("agent forward unmarshal: %w", err)
    }
    if resp.Status != "ok" {
        conn.Close()
        msg := ""
        if resp.Message != nil { msg = *resp.Message }
        return nil, fmt.Errorf("forward to port %d refused: %s", port, msg)
    }

    // After handshake, conn is a raw bidirectional TCP tunnel.
    // No more framing — the caller reads/writes raw bytes.
    // This conn is returned as the io.ReadWriteCloser from engine.Tunnel().
    return conn, nil
}
```

### 3.7 Testing (Mac)

Same pattern as Part 2 — start the agent in test mode via `startTestAgent(t)`,
then create a `NewTestClient(controlSock, forwardSock)`. The client tests
exercise the full stack: client → Unix socket → agent → child process → frames
back to client. All on Mac with `go test ./pkg/agent/`.

```go
func TestClientExec(t *testing.T) {
    controlSock, forwardSock, cleanup := startTestAgent(t) // from Part 2
    defer cleanup()

    client := agent.NewTestClient(controlSock, forwardSock)
    result, err := client.Exec(context.Background(), []string{"echo", "hello"}, nil, "")
    require.NoError(t, err)
    assert.Equal(t, 0, result.ExitCode)
    assert.Equal(t, "hello\n", result.Stdout)
    assert.Empty(t, result.Stderr)
}
```

- `TestClientExec` — Exec `["echo", "hello"]`, verify ExecResult fields
- `TestClientExecWithEnv` — Exec with env vars, verify they're visible
- `TestClientExecStderr` — verify Stderr field is populated separately
- `TestClientShell` — `Shell(["/bin/sh"], ...)`, write `"echo ok\n"` via
  `TerminalConn.Write()`, read via `TerminalConn.Read()` until `"ok"` appears,
  call `TerminalConn.Resize(40, 120)`, write `"exit\n"`, read until `io.EOF`
- `TestClientForward` — start a TCP echo server on a random port, call
  `client.Forward(port)`, write `"ping"`, read `"ping"` back, close
- `TestClientForwardRefused` — Forward to a port with no listener, verify error
- `TestClientWaitReady` — start agent with a 500ms delay, call WaitReady with
  2s timeout, verify it succeeds. Call WaitReady with 10ms timeout, verify it
  fails.

**Note:** `startTestAgent` is a helper shared between Part 2 and Part 3 tests.
Put it in an internal test helper package, or in `cmd/bhatti-agent/testutil_test.go`
and have Part 3 tests import the agent binary's test package (or just duplicate
the helper — it's ~30 lines).

---

## Part 4 — Rootfs and Kernel Images

### 4.1 Kernel

Download a pre-built uncompressed Linux kernel for aarch64. Amazon publishes
these for Firecracker:

```bash
# On Mac or Pi — download once
ARCH=aarch64
curl -fsSL \
  "https://s3.amazonaws.com/spec.ccfc.min/img/quickstart_guide/${ARCH}/kernels/vmlinux.bin" \
  -o images/vmlinux-arm64
```

Alternatively, compile a minimal kernel from source with only the options
Firecracker needs (virtio-blk, virtio-net, virtio-vsock, ext4, devtmpfs,
procfs, sysfs). A custom kernel is smaller (~5MB vs ~25MB) and boots faster.
Not required to start — the pre-built one works.

### 4.2 Base Rootfs

An ext4 disk image containing the userland. Equivalent to the current
`Dockerfile.sandbox` contents. Built in a Docker container for reproducibility,
or directly on the Pi.

```bash
# scripts/build-rootfs.sh
# Run on a Linux host (Pi or Docker container with --privileged)

set -eu

SIZE_MB=4096
IMG=images/rootfs-base-arm64.ext4
MOUNT=/mnt/bhatti-rootfs
AGENT=bin/bhatti-agent-linux-arm64

# Create empty ext4 image
dd if=/dev/zero of="$IMG" bs=1M count="$SIZE_MB"
mkfs.ext4 -F "$IMG"

mkdir -p "$MOUNT"
mount "$IMG" "$MOUNT"

# Bootstrap minimal Ubuntu (or Debian)
debootstrap --arch=arm64 noble "$MOUNT" http://ports.ubuntu.com/ubuntu-ports

# Set up DNS resolution inside the chroot (debootstrap doesn't copy resolv.conf)
cp /etc/resolv.conf "$MOUNT/etc/resolv.conf"

# Mount proc/sys/dev so chrooted processes can function (e.g. locale-gen)
mount --bind /proc "$MOUNT/proc"
mount --bind /sys "$MOUNT/sys"
mount --bind /dev "$MOUNT/dev"
mount --bind /dev/pts "$MOUNT/dev/pts"

# Install packages (mirrors Dockerfile.sandbox)
chroot "$MOUNT" /bin/bash -c '
  export DEBIAN_FRONTEND=noninteractive
  apt-get update
  apt-get install -y --no-install-recommends \
    zsh git curl wget ca-certificates gnupg \
    tmux vim-tiny htop jq unzip xz-utils \
    locales sudo socat iproute2

  sed -i "/en_US.UTF-8/s/^# //g" /etc/locale.gen
  locale-gen

  # Starship prompt
  curl -fsSL https://starship.rs/install.sh | sh -s -- -y

  # Create user
  useradd -m -s /bin/zsh -G sudo lohar
  echo "lohar ALL=(ALL) NOPASSWD:ALL" >> /etc/sudoers

  # Node.js
  ARCH=$(dpkg --print-architecture)
  NODE_VERSION=22.16.0
  curl -fsSL "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${ARCH}.tar.xz" \
    | tar -xJ --strip-components=1 -C /usr/local

  # Claude Code
  npm install -g @anthropic-ai/claude-code

  apt-get clean
  rm -rf /var/lib/apt/lists/*
'

# Copy zsh/tmux configs
cp sandbox/zshrc "$MOUNT/home/lohar/.zshrc"
cp sandbox/tmux.conf "$MOUNT/home/lohar/.tmux.conf"
chown 1000:1000 "$MOUNT/home/lohar/.zshrc" "$MOUNT/home/lohar/.tmux.conf"

# Install tmux plugins (same as Dockerfile.sandbox)
chroot "$MOUNT" su - lohar -c '
  mkdir -p ~/.tmux/plugins
  git clone --depth 1 https://github.com/tmux-plugins/tmux-sensible ~/.tmux/plugins/tmux-sensible
  git clone --depth 1 https://github.com/dracula/tmux ~/.tmux/plugins/tmux
  git clone --depth 1 https://github.com/tmux-plugins/tmux-cpu ~/.tmux/plugins/tmux-cpu
'

# Install zsh plugins (same as Dockerfile.sandbox)
chroot "$MOUNT" su - lohar -c '
  git clone --depth 1 https://github.com/zdharma-continuum/zinit.git ~/.local/share/zinit/zinit.git
  mkdir -p ~/.local/share/zinit/plugins
  git clone --depth 1 https://github.com/zsh-users/zsh-syntax-highlighting ~/.local/share/zinit/plugins/zsh-users---zsh-syntax-highlighting
  git clone --depth 1 https://github.com/zsh-users/zsh-autosuggestions ~/.local/share/zinit/plugins/zsh-users---zsh-autosuggestions
  git clone --depth 1 https://github.com/agkozak/zsh-z ~/.local/share/zinit/plugins/agkozak---zsh-z
'

# Create workspace directory
mkdir -p "$MOUNT/workspace"
chown 1000:1000 "$MOUNT/workspace"

# Install guest agent as /usr/local/bin/bhatti-agent
cp "$AGENT" "$MOUNT/usr/local/bin/bhatti-agent"
chmod 755 "$MOUNT/usr/local/bin/bhatti-agent"

# Set up DNS for the guest (static, since there's no NetworkManager)
cat > "$MOUNT/etc/resolv.conf" << 'EOF'
nameserver 1.1.1.1
nameserver 8.8.8.8
EOF

# Configure init: kernel boots into bhatti-agent as PID 1
# The kernel cmdline (set by Firecracker config) will include:
#   init=/usr/local/bin/bhatti-agent
# No initramfs needed — the agent handles everything.

# Clean up bind mounts from build
umount "$MOUNT/dev/pts" 2>/dev/null || true
umount "$MOUNT/dev" 2>/dev/null || true
umount "$MOUNT/sys" 2>/dev/null || true
umount "$MOUNT/proc" 2>/dev/null || true

umount "$MOUNT"
rmdir "$MOUNT"
echo "Built rootfs: $IMG ($(du -h "$IMG" | cut -f1))"
```

Building from Mac via Docker:

```makefile
build-rootfs:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build \
		-ldflags='-s -w' -o bin/bhatti-agent-linux-arm64 ./cmd/bhatti-agent
	docker run --rm --privileged \
		-v $(PWD):/work -w /work \
		--platform linux/arm64 \
		ubuntu:24.04 \
		bash scripts/build-rootfs.sh
```

### 4.3 Per-Sandbox Rootfs

When creating a sandbox, copy the base rootfs. On filesystems with CoW (btrfs,
xfs with reflink) this is instant and space-efficient. On ext4 it's a full
copy.

```go
func copyRootfs(base, dest string) error {
    // Try CoW clone first (fast, space-efficient)
    err := exec.Command("cp", "--reflink=always", base, dest).Run()
    if err == nil { return nil }
    // Fall back to full copy
    return exec.Command("cp", base, dest).Run()
}
```

Rootfs size per sandbox: 0 bytes initially with reflink, up to the full image
size without it. Snapshots add the memory file (= RAM allocated to VM).

---

## Part 5 — Firecracker Engine

Implements `engine.Engine` using the `firecracker-go-sdk`.

### 5.1 Dependencies

```bash
go get github.com/firecracker-microvm/firecracker-go-sdk
# The SDK pulls in its own dependencies (client models, swagger, etc.)
# Also needed for TAP device management (optional, can use exec("ip") instead):
# go get github.com/vishvananda/netlink
```

### 5.2 Files

```
pkg/engine/firecracker/
  engine.go     — Engine struct, Create/Destroy/List/Status
  vm.go         — VM struct, per-VM state management
  network.go    — TAP device creation/cleanup
  snapshot.go   — Pause/CreateSnapshot, LoadSnapshot/Resume
```

### 5.3 Engine Struct and Constructor

```go
// pkg/engine/firecracker/engine.go

// Config holds the paths and settings needed to create a Firecracker engine.
type Config struct {
    DataDir    string // ~/.bhatti — sandbox dirs created under DataDir/sandboxes/
    KernelPath string // path to vmlinux binary
    BaseRootfs string // path to base rootfs.ext4
    FCBinary   string // path to firecracker binary (e.g. /usr/local/bin/firecracker)
}

type Engine struct {
    mu          sync.RWMutex
    vms         map[string]*VM          // engineID → VM
    cfg         Config
    nextCID     uint32                  // next vsock CID to assign
}

// New validates config and returns a Firecracker engine.
// CID numbering starts at 3 (0=hypervisor, 1=loopback, 2=host).
func New(cfg Config) (*Engine, error) {
    // Validate required files exist
    for name, path := range map[string]string{
        "kernel":      cfg.KernelPath,
        "base rootfs": cfg.BaseRootfs,
        "firecracker": cfg.FCBinary,
    } {
        if _, err := os.Stat(path); err != nil {
            return nil, fmt.Errorf("%s not found at %s: %w", name, path, err)
        }
    }

    sandboxDir := filepath.Join(cfg.DataDir, "sandboxes")
    if err := os.MkdirAll(sandboxDir, 0700); err != nil {
        return nil, fmt.Errorf("create sandbox dir: %w", err)
    }

    return &Engine{
        vms:     make(map[string]*VM),
        cfg:     cfg,
        nextCID: 3, // first available CID
    }, nil
}

type VM struct {
    ID            string
    Machine       *firecracker.Machine
    SocketPath    string                 // Firecracker API socket UDS
    VsockPath     string                 // vsock UDS (host→guest communication)
    RootfsPath    string                 // this sandbox's rootfs.ext4
    SnapMemPath   string                 // memory snapshot file (set when stopped)
    SnapVMPath    string                 // VM state snapshot file (set when stopped)
    CID           uint32                 // vsock context ID (unique per VM, ≥ 3)
    VcpuCount     int64                  // vCPUs (preserved for resume config)
    MemSizeMib    int64                  // RAM in MiB (preserved for resume config)
    TapDevice     string                 // TAP interface name (e.g. "tap-abc12345")
    GuestIP       string                 // guest IP address (e.g. "172.16.0.2")
    Agent         *agent.AgentClient     // host-side agent client
    Status        string                 // "running", "stopped"
    cancel        context.CancelFunc     // cancels the VM context (kills firecracker)
}

func (e *Engine) getVM(id string) (*VM, error) {
    e.mu.RLock()
    defer e.mu.RUnlock()
    vm, ok := e.vms[id]
    if !ok {
        return nil, fmt.Errorf("sandbox %q not found", id)
    }
    return vm, nil
}
```

### 5.4 Create

```go
func (e *Engine) Create(ctx context.Context, spec engine.SandboxSpec) (engine.SandboxInfo, error) {
    id := generateID()
    sandboxDir := filepath.Join(e.cfg.DataDir, "sandboxes", id)
    os.MkdirAll(sandboxDir, 0700)

    // 1. Copy rootfs
    rootfsPath := filepath.Join(sandboxDir, "rootfs.ext4")
    copyRootfs(e.cfg.BaseRootfs, rootfsPath)

    // 2. Allocate CID and paths
    cid := atomic.AddUint32(&e.nextCID, 1)
    socketPath := filepath.Join(sandboxDir, "firecracker.sock")
    vsockPath := filepath.Join(sandboxDir, "vsock.sock")

    // 3. Create TAP device
    tapName, guestIP := createTapDevice(id)

    // 4. Configure Firecracker
    vcpuCount := int64(spec.CPUs)
    if vcpuCount < 1 { vcpuCount = 1 }
    memMB := int64(spec.MemoryMB)
    if memMB < 128 { memMB = 128 }

    cfg := firecracker.Config{
        SocketPath:      socketPath,
        KernelImagePath: e.cfg.KernelPath,
        KernelArgs:      "console=ttyS0 reboot=k panic=1 pci=off init=/usr/local/bin/bhatti-agent quiet",
        Drives:          firecracker.NewDrivesBuilder(rootfsPath).Build(),
        MachineCfg: models.MachineConfiguration{
            VcpuCount:  firecracker.Int64(vcpuCount),
            MemSizeMib: firecracker.Int64(memMB),
        },
        VsockDevices: []firecracker.VsockDevice{{
            Path: vsockPath,
            CID:  cid,
        }},
        NetworkInterfaces: firecracker.NetworkInterfaces{{
            StaticConfiguration: &firecracker.StaticNetworkConfiguration{
                MacAddress:  generateMAC(),
                HostDevName: tapName,
                IPConfiguration: &firecracker.IPConfiguration{
                    IPAddr:      guestIPNet,
                    Gateway:     gatewayIP,
                    Nameservers: []string{"1.1.1.1", "8.8.8.8"},
                },
            },
        }},
    }

    // 5. Start Firecracker process
    vmCtx, vmCancel := context.WithCancel(context.Background())
    cmd := firecracker.VMCommandBuilder{}.
        WithBin(e.cfg.FCBinary).
        WithSocketPath(socketPath).
        Build(vmCtx)

    machine, err := firecracker.NewMachine(vmCtx, cfg, firecracker.WithProcessRunner(cmd))
    if err != nil {
        vmCancel()
        return engine.SandboxInfo{}, fmt.Errorf("create machine: %w", err)
    }
    if err := machine.Start(vmCtx); err != nil {
        vmCancel()
        return engine.SandboxInfo{}, fmt.Errorf("start machine: %w", err)
    }

    // 6. Wait for guest agent to be ready.
    //    The VM boots the kernel + runs the agent as PID 1. The agent opens
    //    vsock listeners on ports 1024/1025. We retry connecting until it
    //    responds or we time out. Typical cold boot: ~1-2 seconds on Pi 5.
    agentClient := agent.NewVsockClient(vsockPath)
    waitCtx, waitCancel := context.WithTimeout(ctx, 30*time.Second)
    defer waitCancel()
    if err := agentClient.WaitReady(waitCtx, 30*time.Second); err != nil {
        machine.StopVMM()
        vmCancel()
        destroyTapDevice(tapName)
        return engine.SandboxInfo{}, fmt.Errorf("agent not ready: %w", err)
    }

    // 7. Inject environment variables via exec
    //    (or bake them into the rootfs, or pass via MMDS)
    if len(spec.Env) > 0 {
        envScript := buildEnvScript(spec.Env)
        agentClient.Exec(ctx, []string{"sh", "-c", envScript}, nil, "")
    }

    vm := &VM{
        ID: id, Machine: machine, SocketPath: socketPath,
        VsockPath: vsockPath, RootfsPath: rootfsPath,
        CID: cid, TapDevice: tapName, GuestIP: guestIP,
        Agent: agentClient, Status: "running", cancel: vmCancel,
    }

    e.mu.Lock()
    e.vms[id] = vm
    e.mu.Unlock()

    return engine.SandboxInfo{
        ID: id, Name: spec.Name, Status: "running",
        IP: guestIP, EngineID: id,
    }, nil
}
```

### 5.5 Stop (Snapshot)

```go
func (e *Engine) Stop(ctx context.Context, id string) error {
    vm, err := e.getVM(id)
    if err != nil { return err }

    // 1. Pause the VM (freezes all vCPUs)
    vm.Machine.PauseVM(ctx)

    // 2. Create snapshot (writes memory + VM state to files)
    vm.SnapMemPath = filepath.Join(filepath.Dir(vm.RootfsPath), "mem.snap")
    vm.SnapVMPath = filepath.Join(filepath.Dir(vm.RootfsPath), "vm.snap")
    vm.Machine.CreateSnapshot(ctx, vm.SnapMemPath, vm.SnapVMPath)

    // 3. Stop the VMM process (frees CPU/RAM on host)
    vm.Machine.StopVMM()

    // 4. Clean up TAP device (and its iptables rule)
    destroyTapDevice(vm.TapDevice, vm.CID)

    vm.Status = "stopped"
    return nil
}
```

### 5.6 Start (Resume from Snapshot)

```go
func (e *Engine) Start(ctx context.Context, id string) error {
    vm, err := e.getVM(id)
    if err != nil { return err }

    if vm.SnapMemPath == "" {
        return fmt.Errorf("no snapshot to resume from")
    }

    // 1. Recreate TAP device (previous one was cleaned up)
    tapName, guestIP := createTapDevice(id)
    vm.TapDevice = tapName
    vm.GuestIP = guestIP

    // 2. New Firecracker process with snapshot config.
    //    IMPORTANT: When loading a snapshot, Firecracker requires the SAME
    //    machine config (vcpus, memory, drives, vsock, network) as the
    //    original VM. The snapshot only stores CPU/memory/device state —
    //    Firecracker still needs to know the device topology to set up the
    //    VMM before loading state into it.
    newSocketPath := vm.SocketPath + ".resume"
    os.Remove(newSocketPath)

    // Vsock UDS must also be fresh — remove old one so Firecracker recreates it
    os.Remove(vm.VsockPath)

    cfg := firecracker.Config{
        SocketPath:      newSocketPath,
        KernelImagePath: e.cfg.KernelPath,    // required even for snapshot resume
        Drives:          firecracker.NewDrivesBuilder(vm.RootfsPath).Build(),
        MachineCfg: models.MachineConfiguration{
            VcpuCount:  firecracker.Int64(vm.VcpuCount),
            MemSizeMib: firecracker.Int64(vm.MemSizeMib),
        },
        VsockDevices: []firecracker.VsockDevice{{
            Path: vm.VsockPath,
            CID:  vm.CID,
        }},
        NetworkInterfaces: firecracker.NetworkInterfaces{{
            StaticConfiguration: &firecracker.StaticNetworkConfiguration{
                MacAddress:  generateMAC(), // TODO: persist and reuse original MAC
                HostDevName: tapName,
                IPConfiguration: &firecracker.IPConfiguration{
                    IPAddr:      mustParseCIDR(guestIP + "/30"),
                    Gateway:     gatewayForGuest(guestIP),
                    Nameservers: []string{"1.1.1.1", "8.8.8.8"},
                },
            },
        }},
        Snapshot: firecracker.SnapshotConfig{
            MemFilePath:  vm.SnapMemPath,
            SnapshotPath: vm.SnapVMPath,
            ResumeVM:     true,   // resume vCPUs immediately after loading
        },
    }

    vmCtx, vmCancel := context.WithCancel(context.Background())
    cmd := firecracker.VMCommandBuilder{}.
        WithBin(e.cfg.FCBinary).
        WithSocketPath(newSocketPath).
        Build(vmCtx)

    machine, err := firecracker.NewMachine(vmCtx, cfg, firecracker.WithProcessRunner(cmd))
    if err != nil {
        vmCancel()
        return fmt.Errorf("create machine for resume: %w", err)
    }
    if err := machine.Start(vmCtx); err != nil {
        vmCancel()
        return fmt.Errorf("resume from snapshot: %w", err)
    }

    // 3. Update VM state
    vm.Machine = machine
    vm.SocketPath = newSocketPath
    vm.cancel = vmCancel
    vm.Status = "running"

    // 4. Agent is already listening (it was running when we snapshotted).
    //    Reconnect the client to the new vsock UDS.
    vm.Agent = agent.NewVsockClient(vm.VsockPath)

    return nil
}
```

### 5.7 Other Engine Methods

```go
func (e *Engine) Destroy(ctx context.Context, id string) error {
    vm, err := e.getVM(id)
    if err != nil { return err }

    if vm.Status == "running" {
        vm.Machine.StopVMM()
        vm.cancel()
    }
    destroyTapDevice(vm.TapDevice, vm.CID)
    os.RemoveAll(filepath.Dir(vm.RootfsPath))  // delete entire sandbox dir

    e.mu.Lock()
    delete(e.vms, id)
    e.mu.Unlock()
    return nil
}

func (e *Engine) Status(ctx context.Context, id string) (engine.SandboxInfo, error) {
    vm, err := e.getVM(id)
    if err != nil { return engine.SandboxInfo{}, err }

    return engine.SandboxInfo{
        ID: vm.ID, Name: vm.ID, Status: vm.Status,
        IP: vm.GuestIP, EngineID: vm.ID,
    }, nil
}

func (e *Engine) Exec(ctx context.Context, id string, cmd []string) (engine.ExecResult, error) {
    vm, err := e.getVM(id)
    if err != nil { return engine.ExecResult{}, err }

    return vm.Agent.Exec(ctx, cmd, nil, "")
}

func (e *Engine) Shell(ctx context.Context, id string) (engine.TerminalConn, error) {
    vm, err := e.getVM(id)
    if err != nil { return nil, err }

    // Start with default 24x80 — the WebSocket handler in routes.go sends a
    // RESIZE message immediately after connect with the real terminal size.
    // The agent's TTY handler applies it via ioctl(TIOCSWINSZ).
    return vm.Agent.Shell(ctx, []string{"/bin/zsh", "-li"}, map[string]string{
        "TERM": "xterm-256color",
    }, 24, 80)
}

func (e *Engine) ListeningPorts(ctx context.Context, id string) ([]int, error) {
    vm, err := e.getVM(id)
    if err != nil { return nil, err }

    // Same approach as Docker engine: run `ss` inside the sandbox.
    // Reuse the existing parseSSOutput() function from docker/docker.go.
    // Move it to a shared package (e.g. pkg/engine/portparse/) or duplicate it.
    result, err := vm.Agent.Exec(ctx, []string{"ss", "-tln", "--no-header"}, nil, "")
    if err != nil { return nil, err }
    return parseSSOutput(result.Stdout), nil
}

func (e *Engine) Tunnel(ctx context.Context, id string, port int) (io.ReadWriteCloser, error) {
    vm, err := e.getVM(id)
    if err != nil { return nil, err }

    return vm.Agent.Forward(ctx, uint16(port))
}

func (e *Engine) List(ctx context.Context) ([]engine.SandboxInfo, error) {
    e.mu.RLock()
    defer e.mu.RUnlock()
    var out []engine.SandboxInfo
    for _, vm := range e.vms {
        out = append(out, engine.SandboxInfo{
            ID: vm.ID, Name: vm.ID, Status: vm.Status,
            IP: vm.GuestIP, EngineID: vm.ID,
        })
    }
    return out, nil
}
```

### 5.8 Network (TAP device management)

```go
// pkg/engine/firecracker/network.go

// IP allocation: each sandbox gets a /30 subnet from 172.16.0.0/16.
// A /30 has 4 addresses: network, host (.1), guest (.2), broadcast.
//   CID 3 → 172.16.0.0/30:  host=172.16.0.1,  guest=172.16.0.2
//   CID 4 → 172.16.0.4/30:  host=172.16.0.5,  guest=172.16.0.6
//   CID 5 → 172.16.0.8/30:  host=172.16.0.9,  guest=172.16.0.10
//   ...
// Formula: base = (cid - 3) * 4, hostIP = 172.16.x.y+1, guestIP = 172.16.x.y+2

type subnet struct {
    HostIP  string  // e.g. "172.16.0.1"
    GuestIP string  // e.g. "172.16.0.2"
}

func subnetForCID(cid uint32) subnet {
    offset := (cid - 3) * 4
    b3 := byte(offset >> 8)
    b4 := byte(offset & 0xFF)
    return subnet{
        HostIP:  fmt.Sprintf("172.16.%d.%d", b3, b4+1),
        GuestIP: fmt.Sprintf("172.16.%d.%d", b3, b4+2),
    }
}

func createTapDevice(sandboxID string, cid uint32) (tapName, guestIP string, err error) {
    tapName = "tap" + sandboxID[:8]  // max 15 chars for Linux interface name
    s := subnetForCID(cid)

    // Create TAP device
    if err := run("ip", "tuntap", "add", tapName, "mode", "tap"); err != nil {
        return "", "", fmt.Errorf("create tap: %w", err)
    }
    if err := run("ip", "addr", "add", s.HostIP+"/30", "dev", tapName); err != nil {
        run("ip", "link", "del", tapName) // cleanup on failure
        return "", "", fmt.Errorf("set tap ip: %w", err)
    }
    if err := run("ip", "link", "set", tapName, "up"); err != nil {
        run("ip", "link", "del", tapName)
        return "", "", fmt.Errorf("bring up tap: %w", err)
    }

    // NAT: masquerade guest traffic through the host's default interface.
    // Detect the default route interface (e.g. "eth0", "wlan0", "end0").
    defaultIface := detectDefaultInterface()
    if err := run("iptables", "-t", "nat", "-A", "POSTROUTING",
        "-s", s.GuestIP+"/32", "-o", defaultIface,
        "-j", "MASQUERADE"); err != nil {
        // Non-fatal: sandbox works for vsock but not internet
        fmt.Fprintf(os.Stderr, "warning: iptables NAT failed: %v\n", err)
    }

    return tapName, s.GuestIP, nil
}

func destroyTapDevice(tapName string, cid uint32) {
    // Delete the TAP (also removes routes)
    run("ip", "link", "del", tapName)

    // Remove the iptables rule by exact match
    s := subnetForCID(cid)
    defaultIface := detectDefaultInterface()
    run("iptables", "-t", "nat", "-D", "POSTROUTING",
        "-s", s.GuestIP+"/32", "-o", defaultIface,
        "-j", "MASQUERADE")
}

// detectDefaultInterface returns the interface name used for the default route.
func detectDefaultInterface() string {
    out, err := exec.Command("ip", "route", "show", "default").Output()
    if err != nil { return "eth0" }
    // Output: "default via 192.168.1.1 dev eth0 ..."
    fields := strings.Fields(string(out))
    for i, f := range fields {
        if f == "dev" && i+1 < len(fields) {
            return fields[i+1]
        }
    }
    return "eth0"
}

func run(name string, args ...string) error {
    cmd := exec.Command(name, args...)
    cmd.Stderr = os.Stderr
    return cmd.Run()
}
```

**Note on Pi networking:** Your Pis use `end0` or `eth0` depending on the OS.
`detectDefaultInterface()` handles this dynamically. If you're on WiFi (`wlan0`)
on some machines, it adapts automatically.

### 5.9 Wire Into main.go

```go
// cmd/bhatti/main.go — engine selection

var eng engine.Engine
switch cfg.Engine {
case "firecracker":
    eng, err = fc.New(fc.Config{
        DataDir:    cfg.DataDir,
        KernelPath: cfg.FirecrackerKernel,
        BaseRootfs: cfg.FirecrackerRootfs,
        FCBinary:   cfg.FirecrackerBin,
    })
case "docker":
    eng, err = docker.New()
default:
    log.Fatalf("unknown engine: %s", cfg.Engine)
}
```

Config additions to `pkg/config.go`:

```go
type Config struct {
    Engine    string `yaml:"engine"`     // "docker" or "firecracker"
    Listen    string `yaml:"listen"`     // e.g. ":8080"
    AuthToken string `yaml:"auth_token"` // bearer token
    DataDir   string `yaml:"data_dir"`   // defaults to ~/.bhatti

    // Firecracker-specific (ignored when engine=docker)
    FirecrackerBin    string `yaml:"firecracker_bin"`    // path to firecracker binary
    FirecrackerKernel string `yaml:"firecracker_kernel"` // path to vmlinux
    FirecrackerRootfs string `yaml:"firecracker_rootfs"` // path to base rootfs.ext4
}
```

Example `~/.bhatti/config.yaml` on Pi:

```yaml
engine: firecracker
listen: :8080
data_dir: /var/lib/bhatti
firecracker_bin: /usr/local/bin/firecracker
firecracker_kernel: /var/lib/bhatti/images/vmlinux-arm64
firecracker_rootfs: /var/lib/bhatti/images/rootfs-base-arm64.ext4
```

---

## Part 6 — Pi Setup and Testing

All integration testing happens on a bare Pi 5 (not inside k3s).

### 6.1 Pi Prerequisites

```bash
# Verify KVM support
ls -la /dev/kvm
# If missing:
sudo modprobe kvm
# Persist:
echo "kvm" | sudo tee /etc/modules-load.d/kvm.conf

# Install Firecracker
ARCH=aarch64
VERSION=1.11.0
curl -fsSL \
  "https://github.com/firecracker-microvm/firecracker/releases/download/v${VERSION}/firecracker-v${VERSION}-${ARCH}.tgz" \
  | tar xz
sudo mv release-v${VERSION}-${ARCH}/firecracker-v${VERSION}-${ARCH} /usr/local/bin/firecracker
sudo mv release-v${VERSION}-${ARCH}/jailer-v${VERSION}-${ARCH} /usr/local/bin/jailer
chmod +x /usr/local/bin/firecracker /usr/local/bin/jailer

# Verify
firecracker --version

# Enable IP forwarding (for guest networking)
echo 'net.ipv4.ip_forward = 1' | sudo tee /etc/sysctl.d/99-bhatti.conf
sudo sysctl -p /etc/sysctl.d/99-bhatti.conf

# Create data directories
sudo mkdir -p /var/lib/bhatti/{images,sandboxes}
sudo chown $(whoami):$(whoami) /var/lib/bhatti -R
```

### 6.2 Deploy From Mac

```makefile
PI_HOST ?= user@192.168.1.201
PI_DIR  ?= /var/lib/bhatti

# The bhatti server uses go-sqlite3 which requires CGO. Cross-compiling with
# CGO needs a cross-compiler toolchain (aarch64-linux-gnu-gcc). Options:
#   a) Install the cross-compiler: brew install aarch64-elf-gcc (or similar)
#      and set CC=aarch64-linux-gnu-gcc
#   b) Use a pure-Go SQLite like modernc.org/sqlite (drop-in replacement for
#      go-sqlite3 that works with CGO_ENABLED=0)
#   c) Build the server binary ON the Pi instead of cross-compiling
#
# Option (b) is recommended — change the import in pkg/store/store.go from
#   _ "github.com/mattn/go-sqlite3"
# to
#   _ "modernc.org/sqlite"
# (the database/sql interface is identical, no other code changes needed)
#
# With modernc.org/sqlite:
build-pi:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build \
		-ldflags='-s -w' -o bin/bhatti-linux-arm64 ./cmd/bhatti
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build \
		-ldflags='-s -w' -o bin/bhatti-agent-linux-arm64 ./cmd/bhatti-agent

deploy: build-pi
	rsync -avz bin/bhatti-linux-arm64 $(PI_HOST):$(PI_DIR)/bhatti
	rsync -avz bin/bhatti-agent-linux-arm64 $(PI_HOST):$(PI_DIR)/bhatti-agent
	rsync -avz web/ $(PI_HOST):$(PI_DIR)/web/
```

### 6.3 Test Sequence on Pi

Each test is run manually via SSH. Ordered by dependency — each step validates
something the next step depends on.

**Test A: Firecracker boots a VM**

Verify that Firecracker, the kernel, and the rootfs work at all. No bhatti
involved — just raw Firecracker.

**Run as root** (or ensure your user has rw access to `/dev/kvm` and
`CAP_NET_ADMIN` for TAP devices):

```bash
# On Pi — SSH in, then:
sudo su -
cd /var/lib/bhatti

# Copy base rootfs for this test
cp images/rootfs-base-arm64.ext4 /tmp/test-rootfs.ext4

# Start Firecracker manually
rm -f /tmp/fc-test.sock
firecracker --api-sock /tmp/fc-test.sock &
FC_PID=$!

# Configure VM via API
curl --unix-socket /tmp/fc-test.sock -X PUT \
  http://localhost/boot-source \
  -d '{
    "kernel_image_path": "/var/lib/bhatti/images/vmlinux-arm64",
    "boot_args": "console=ttyS0 reboot=k panic=1 pci=off init=/usr/local/bin/bhatti-agent"
  }'

curl --unix-socket /tmp/fc-test.sock -X PUT \
  http://localhost/drives/rootfs \
  -d '{
    "drive_id": "rootfs",
    "path_on_host": "/tmp/test-rootfs.ext4",
    "is_root_device": true,
    "is_read_only": false
  }'

curl --unix-socket /tmp/fc-test.sock -X PUT \
  http://localhost/machine-config \
  -d '{"vcpu_count": 1, "mem_size_mib": 512}'

curl --unix-socket /tmp/fc-test.sock -X PUT \
  http://localhost/vsock \
  -d '{"guest_cid": 3, "uds_path": "/tmp/fc-test-vsock.sock"}'

# Start the VM
curl --unix-socket /tmp/fc-test.sock -X PUT \
  http://localhost/actions \
  -d '{"action_type": "InstanceStart"}'

# Wait a couple seconds for boot, then try vsock
sleep 2

# Connect to guest agent via vsock
# Write "CONNECT 1024\n", should get "OK <port>\n" back
echo -e "CONNECT 1024\n" | socat - UNIX-CONNECT:/tmp/fc-test-vsock.sock

# Clean up
kill $FC_PID
rm /tmp/fc-test.sock /tmp/fc-test-vsock.sock /tmp/test-rootfs.ext4
```

Expected: the socat command prints `OK <number>`. This confirms the kernel
booted, the agent started, and vsock is working.

If the VM doesn't boot, check `dmesg` and Firecracker's stderr for kernel
panic messages. Common issues:
- Kernel doesn't match architecture (need aarch64 vmlinux for Pi)
- rootfs doesn't have the agent at `/usr/local/bin/bhatti-agent`
- `/dev/kvm` permissions — run Firecracker as root or add user to kvm group

**Test B: Agent exec over vsock**

Write a small Go test program that:
1. Starts Firecracker (using the Go SDK)
2. Waits for vsock
3. Connects `AgentClient` to the vsock UDS
4. Calls `AgentClient.Exec(["uname", "-a"])`
5. Asserts stdout contains `aarch64`
6. Calls `AgentClient.Exec(["cat", "/etc/os-release"])`
7. Asserts stdout contains `Ubuntu`
8. Stops Firecracker

```bash
# On Pi
cd /var/lib/bhatti
sudo go test -tags=integration -run TestFirecrackerExec -v ./pkg/engine/firecracker/
```

This test must run as root (or with `/dev/kvm` access + CAP_NET_ADMIN for TAP
devices).

**Test C: Agent shell (TTY) over vsock**

Same setup as Test B, but:
1. Calls `AgentClient.Shell(["/bin/sh"], ..., 24, 80)`
2. Writes STDIN: `echo hello\n`
3. Reads STDOUT frames until `hello` appears
4. Writes STDIN: `exit\n`
5. Reads until EXIT frame

**Test D: Port forwarding over vsock**

1. Start a VM
2. Exec `python3 -m http.server 8000 &` inside the VM
3. Call `AgentClient.Forward(8000)`
4. Write an HTTP request through the tunnel
5. Read the HTTP response
6. Verify you get a valid HTML directory listing

**Test E: Snapshot and resume**

The core test. Validates that process state survives stop/start.

1. Create a sandbox via `engine.Create()`
2. Exec: `echo $$ > /tmp/test-pid && sleep 3600 &`
3. Exec: `cat /tmp/test-pid` → save this PID
4. Stop the sandbox (`engine.Stop()`)
5. Verify Firecracker process is gone (`ps aux | grep firecracker`)
6. Verify snapshot files exist on disk
7. Start the sandbox (`engine.Start()`)
8. Exec: `cat /tmp/test-pid` → same PID as before
9. Exec: `kill -0 $(cat /tmp/test-pid)` → exit code 0 (process still alive)
10. Exec: `ps aux | grep sleep` → the `sleep 3600` process is still running
11. Destroy the sandbox

This proves that a background process launched before snapshot survives the
full stop→start cycle.

**Test F: Full bhatti server end-to-end**

1. Start `./bhatti` on the Pi with `engine: firecracker`
2. From Mac, open browser to `http://192.168.1.201:8080`
3. Create a template via API:
   ```
   POST /templates
   {"name": "dev", "image": "base", "cpus": 1, "memory_mb": 512, "engine": "firecracker"}
   ```
4. Create a sandbox:
   ```
   POST /sandboxes
   {"name": "test-1", "template_id": "<id>"}
   ```
5. Open the web terminal — click through the UI or connect directly:
   ```
   wscat -c ws://192.168.1.201:8080/sandboxes/<id>/ws
   ```
6. Type commands in the terminal, verify they work
7. Start a web server inside the sandbox:
   ```
   python3 -m http.server 9000
   ```
8. Verify port appears in `GET /sandboxes/<id>/ports`
9. Access the server through the proxy:
   ```
   curl http://192.168.1.201:8080/sandboxes/<id>/proxy/9000/
   ```
10. Stop the sandbox: `POST /sandboxes/<id>/stop`
11. Verify the terminal disconnects
12. Start it again: `POST /sandboxes/<id>/start`
13. Open a new terminal, verify the python server is still running
14. Destroy: `DELETE /sandboxes/<id>`

**Test G: Ephemeral sandbox lifecycle**

Simulates the LLM code review use case:

```bash
# Create
ID=$(curl -s -X POST http://pi:8080/sandboxes \
  -d '{"name":"review-pr","template_id":"dev"}' | jq -r .id)

# Clone + test
curl -s -X POST http://pi:8080/sandboxes/$ID/exec \
  -d '{"cmd":["git","clone","--depth=1","https://github.com/some/repo","/workspace/repo"]}'

curl -s -X POST http://pi:8080/sandboxes/$ID/exec \
  -d '{"cmd":["sh","-c","cd /workspace/repo && npm install && npm test"]}'

# Read results
RESULT=$(curl -s -X POST http://pi:8080/sandboxes/$ID/exec \
  -d '{"cmd":["cat","/workspace/repo/test-results.json"]}')

# Destroy — everything gone
curl -s -X DELETE http://pi:8080/sandboxes/$ID
```

Measure: time from POST /sandboxes to first exec returning. Target: under 3
seconds for a cold boot, under 500ms when resuming from a pre-warmed snapshot.

---

## Part 7 — State Persistence Across Restarts

The current Docker engine doesn't need to track VM-level state because Docker
manages container lifecycle independently. With Firecracker, bhatti IS the
lifecycle manager — if bhatti restarts, it needs to know which VMs exist and
their snapshot paths.

### 7.1 Store Changes

Add columns to the `sandboxes` table. Follow the existing migration pattern in
`pkg/store/store.go` — the `migrations` const runs `ALTER TABLE` statements
that are silently ignored if the column already exists:

```go
// Add to the migrations const in pkg/store/store.go:
const migrations = `
-- existing
ALTER TABLE templates ADD COLUMN mounts_json TEXT NOT NULL DEFAULT '[]';

-- Firecracker engine state (added for Part 7)
ALTER TABLE sandboxes ADD COLUMN rootfs_path TEXT DEFAULT '';
ALTER TABLE sandboxes ADD COLUMN snap_mem_path TEXT DEFAULT '';
ALTER TABLE sandboxes ADD COLUMN snap_vm_path TEXT DEFAULT '';
ALTER TABLE sandboxes ADD COLUMN vsock_cid INTEGER DEFAULT 0;
ALTER TABLE sandboxes ADD COLUMN tap_device TEXT DEFAULT '';
ALTER TABLE sandboxes ADD COLUMN guest_ip TEXT DEFAULT '';
ALTER TABLE sandboxes ADD COLUMN fc_pid INTEGER DEFAULT 0;
ALTER TABLE sandboxes ADD COLUMN vcpu_count REAL DEFAULT 1;
ALTER TABLE sandboxes ADD COLUMN mem_size_mib INTEGER DEFAULT 512;
`
```

Add corresponding store methods:

```go
// UpdateSandboxFirecracker persists Firecracker-specific VM state.
// Called after Create (to save paths/CID) and after Stop (to save snapshot paths).
func (s *Store) UpdateSandboxFirecracker(id string, rootfs, snapMem, snapVM, tapDev, guestIP string, cid, pid int, vcpu float64, memMB int) error

// GetSandboxFirecracker loads the Firecracker-specific fields.
// Returns zero values for Docker sandboxes (the columns default to empty/0).
func (s *Store) GetSandboxFirecracker(id string) (rootfs, snapMem, snapVM, tapDev, guestIP string, cid, pid int, vcpu float64, memMB int, err error)
```

**Why explicit columns instead of `engine_meta_json`:** The existing
`engine_meta_json` column works, but explicit columns let you query directly
(e.g. `SELECT id FROM sandboxes WHERE vsock_cid = ?` to detect CID conflicts
on startup). The migration pattern is already in place — just add more `ALTER
TABLE` lines.

### 7.2 Startup Recovery

When the bhatti server starts with `engine: firecracker`, it must reconcile
the SQLite state with reality. This handles crashes, reboots, and manual
Firecracker process kills.

```go
func (e *Engine) RecoverFromStore(st *store.Store) error {
    sandboxes, _ := st.ListSandboxes()

    for _, sb := range sandboxes {
        if sb.Status == "destroyed" { continue }

        rootfs, snapMem, snapVM, tapDev, guestIP, cid, pid, vcpu, memMB, _ :=
            st.GetSandboxFirecracker(sb.ID)

        if rootfs == "" {
            // Not a Firecracker sandbox (might be Docker), skip
            continue
        }

        if sb.Status == "running" {
            // Was running when bhatti last saved state. Is the process alive?
            alive := pid > 0 && syscall.Kill(pid, 0) == nil

            if alive {
                // Firecracker process survived bhatti restart.
                // Reconnect the agent client.
                vsockPath := filepath.Join(filepath.Dir(rootfs), "vsock.sock")
                vm := &VM{
                    ID: sb.ID, RootfsPath: rootfs, VsockPath: vsockPath,
                    CID: uint32(cid), TapDevice: tapDev, GuestIP: guestIP,
                    VcpuCount: int64(vcpu), MemSizeMib: int64(memMB),
                    Agent: agent.NewVsockClient(vsockPath),
                    Status: "running",
                }
                e.mu.Lock()
                e.vms[sb.ID] = vm
                e.mu.Unlock()

                // Update nextCID to avoid conflicts
                if uint32(cid) >= e.nextCID {
                    e.nextCID = uint32(cid) + 1
                }
            } else {
                // Firecracker process died (host rebooted, OOM killed, etc.)
                if snapMem != "" && snapVM != "" {
                    // Has snapshot files → mark stopped (resumable)
                    st.UpdateSandboxStatus(sb.ID, "stopped")
                } else {
                    // No snapshot → data is lost, mark unknown
                    st.UpdateSandboxStatus(sb.ID, "unknown")
                }
            }
        }

        if sb.Status == "stopped" {
            // Already stopped. Verify snapshot files still exist on disk.
            if snapMem != "" && snapVM != "" {
                if _, err := os.Stat(snapMem); err != nil {
                    st.UpdateSandboxStatus(sb.ID, "unknown") // snapshot lost
                    continue
                }
            }
            // Sandbox is resumable — register in memory map so Start() can find it
            vm := &VM{
                ID: sb.ID, RootfsPath: rootfs, SnapMemPath: snapMem,
                SnapVMPath: snapVM, CID: uint32(cid), GuestIP: guestIP,
                VcpuCount: int64(vcpu), MemSizeMib: int64(memMB),
                VsockPath: filepath.Join(filepath.Dir(rootfs), "vsock.sock"),
                Status: "stopped",
            }
            e.mu.Lock()
            e.vms[sb.ID] = vm
            e.mu.Unlock()

            if uint32(cid) >= e.nextCID {
                e.nextCID = uint32(cid) + 1
            }
        }
    }
    return nil
}
```

**Called from `main.go`** after creating the engine and before starting the
HTTP server:

```go
if fcEngine, ok := eng.(*fc.Engine); ok {
    if err := fcEngine.RecoverFromStore(st); err != nil {
        log.Printf("warning: recovery: %v", err)
    }
}
```

**State summary:**

| DB Status | Process alive? | Snapshot exists? | Action |
|---|---|---|---|
| running | yes | — | Reconnect agent, add to VM map |
| running | no | yes | Mark stopped (resumable) |
| running | no | no | Mark unknown (data lost) |
| stopped | — | yes | Add to VM map (resumable) |
| stopped | — | no | Mark unknown |
