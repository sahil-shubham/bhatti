package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// AgentClient communicates with the guest agent running inside a microVM.
//
// Three transport modes:
//   - Vsock: connects through Firecracker's vsock UDS with CONNECT handshake
//   - TCP: connects directly to the guest's IP over the TAP network
//   - Test: connects to plain Unix sockets (no handshake)
type AgentClient struct {
	controlSock string // vsock UDS path or Unix socket path
	forwardSock string
	isVsock     bool
	tcpAddr     string // guest IP for TCP mode (e.g. "192.168.137.2")
	token       string // auth token, empty = no auth
}

// NewVsockClient creates a client that connects through a Firecracker vsock UDS.
func NewVsockClient(vsockPath string) *AgentClient {
	return &AgentClient{
		controlSock: vsockPath,
		forwardSock: vsockPath,
		isVsock:     true,
	}
}

// NewTCPClient creates a client that connects to the agent via TCP over the
// TAP network. Used after snapshot/resume since virtio-net survives but
// vsock does not.
func NewTCPClient(guestIP string) *AgentClient {
	return &AgentClient{
		tcpAddr: guestIP,
	}
}

// NewTCPClientWithAuth creates a TCP client with an auth token.
func NewTCPClientWithAuth(guestIP, token string) *AgentClient {
	return &AgentClient{
		tcpAddr: guestIP,
		token:   token,
	}
}

// NewTestClient creates a client that connects to the agent's test-mode
// Unix sockets directly (no vsock handshake).
func NewTestClient(controlSock, forwardSock string) *AgentClient {
	return &AgentClient{
		controlSock: controlSock,
		forwardSock: forwardSock,
	}
}

// NewKrucibleClient connects to lohar through the libkrun-bridged vsock UDS
// paths (one per guest port: control=1024, forward=1025). libkrun listens on
// these host UDS and bridges to the guest vsock port, so we dial them directly
// as plain Unix sockets — no Firecracker CONNECT handshake. Carries the auth
// token (empty = no auth, e.g. P1 before config-drive injection lands).
func NewKrucibleClient(controlSock, forwardSock, token string) *AgentClient {
	return &AgentClient{
		controlSock: controlSock,
		forwardSock: forwardSock,
		token:       token,
	}
}

// DialControl opens a connection to the control channel (port 1024).
// The context controls connection timeout and cancellation.
func (c *AgentClient) DialControl(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	var conn net.Conn
	var err error
	if c.tcpAddr != "" {
		conn, err = d.DialContext(ctx, "tcp", net.JoinHostPort(c.tcpAddr, fmt.Sprint(proto.VsockPortControl)))
	} else if c.isVsock {
		conn, err = c.dialVsockPort(ctx, c.controlSock, proto.VsockPortControl)
	} else {
		conn, err = d.DialContext(ctx, "unix", c.controlSock)
	}
	if err != nil {
		return nil, err
	}
	if err := c.sendAuth(conn); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// dialForward opens a connection to the forward channel (port 1025).
func (c *AgentClient) dialForward(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	var conn net.Conn
	var err error
	if c.tcpAddr != "" {
		conn, err = d.DialContext(ctx, "tcp", net.JoinHostPort(c.tcpAddr, fmt.Sprint(proto.VsockPortForward)))
	} else if c.isVsock {
		conn, err = c.dialVsockPort(ctx, c.forwardSock, proto.VsockPortForward)
	} else {
		conn, err = d.DialContext(ctx, "unix", c.forwardSock)
	}
	if err != nil {
		return nil, err
	}
	if err := c.sendAuth(conn); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// sendAuth sends the AUTH frame if a token is set.
func (c *AgentClient) sendAuth(conn net.Conn) error {
	if c.token == "" {
		return nil
	}
	return proto.WriteFrame(conn, proto.AUTH, []byte(c.token))
}

// dialVsockPort performs the Firecracker vsock CONNECT handshake.
// See: https://github.com/firecracker-microvm/firecracker/blob/main/docs/vsock.md
func (c *AgentClient) dialVsockPort(ctx context.Context, udsPath string, port uint32) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", udsPath)
	if err != nil {
		return nil, fmt.Errorf("vsock dial %s: %w", udsPath, err)
	}

	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		conn.Close()
		return nil, fmt.Errorf("vsock CONNECT write: %w", err)
	}

	// Use a small reader to avoid buffering beyond the handshake line.
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

	return conn, nil
}

// Exec runs a command non-interactively and returns after it exits.
func (c *AgentClient) Exec(ctx context.Context, argv []string, env map[string]string, cwd string) (engine.ExecResult, error) {
	conn, err := c.DialControl(ctx)
	if err != nil {
		return engine.ExecResult{}, fmt.Errorf("agent connect: %w", err)
	}
	defer conn.Close()

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

	const maxBufferedOutput = 10 << 20 // 10 MB

	var stdout, stderr bytes.Buffer
	var totalBytes int
	for {
		msgType, payload, err := proto.ReadFrame(conn)
		if err != nil {
			return engine.ExecResult{}, fmt.Errorf("agent read: %w", err)
		}
		switch msgType {
		case proto.STDOUT:
			if totalBytes+len(payload) <= maxBufferedOutput {
				stdout.Write(payload)
				totalBytes += len(payload)
			}
		case proto.STDERR:
			if totalBytes+len(payload) <= maxBufferedOutput {
				stderr.Write(payload)
				totalBytes += len(payload)
			}
		case proto.EXIT:
			exitCode, _ := proto.ParseExitCode(payload)
			result := engine.ExecResult{
				ExitCode: int(exitCode),
				Stdout:   stdout.String(),
				Stderr:   stderr.String(),
			}
			if totalBytes >= maxBufferedOutput {
				result.Stderr += "\n[output truncated at 10MB]"
			}
			return result, nil
		case proto.ERROR:
			return engine.ExecResult{}, fmt.Errorf("agent error: %s", payload)
		default:
			// Unknown frame type — skip (forward compatibility).
		}
	}
}

// ExecDetached starts a command in a detached session (setsid) and returns
// immediately with the child PID and output file path. The command survives
// vsock connection close. Output is redirected to outputFile (or a generated
// path if empty).
func (c *AgentClient) ExecDetached(ctx context.Context, argv []string, env map[string]string, cwd, outputFile string) (pid int, outputPath string, err error) {
	conn, err := c.DialControl(ctx)
	if err != nil {
		return 0, "", fmt.Errorf("agent connect: %w", err)
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}

	detach := true
	req := proto.ExecRequest{Argv: argv, Env: env, Detach: &detach}
	if outputFile != "" {
		req.OutputFile = &outputFile
	}
	if cwd != "" {
		req.Cwd = &cwd
	}
	if err := proto.SendJSON(conn, proto.EXEC_REQ, req); err != nil {
		return 0, "", fmt.Errorf("agent send exec: %w", err)
	}

	// Read STDOUT frame (JSON metadata) + EXIT frame.
	// handleDetachedExec sends: STDOUT({"pid":N,"output_file":"..."}) then EXIT(0)
	var stdout bytes.Buffer
	for {
		msgType, payload, err := proto.ReadFrame(conn)
		if err != nil {
			return 0, "", fmt.Errorf("agent read: %w", err)
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
				return 0, "", fmt.Errorf("parse detach response: %w (raw: %s)", err, stdout.String())
			}
			return meta.PID, meta.OutputFile, nil
		case proto.ERROR:
			return 0, "", fmt.Errorf("agent: %s", payload)
		default:
			// Unknown frame — skip (forward compat)
		}
	}
}

// Shell opens an interactive TTY session and returns a TerminalConn.
func (c *AgentClient) Shell(ctx context.Context, argv []string, env map[string]string, rows, cols uint16) (engine.TerminalConn, error) {
	conn, err := c.DialControl(ctx)
	if err != nil {
		return nil, fmt.Errorf("agent connect: %w", err)
	}

	tty := true
	req := proto.ExecRequest{
		Argv: argv,
		Env:  env,
		TTY:  &tty,
		Rows: &rows,
		Cols: &cols,
	}
	if err := proto.SendJSON(conn, proto.EXEC_REQ, req); err != nil {
		conn.Close()
		return nil, fmt.Errorf("agent send shell: %w", err)
	}

	// Consume the SESSION_INFO frame that the agent sends before STDOUT
	msgType, payload, err := proto.ReadFrame(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("agent read session info: %w", err)
	}
	if msgType == proto.ERROR {
		conn.Close()
		return nil, fmt.Errorf("agent: %s", payload)
	}
	// SESSION_INFO consumed, ready for STDOUT/STDIN

	return &agentTermConn{conn: conn}, nil
}

// Forward opens a raw TCP tunnel to a port inside the guest.
func (c *AgentClient) Forward(ctx context.Context, port uint16) (io.ReadWriteCloser, error) {
	conn, err := c.dialForward(ctx)
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
		if resp.Message != nil {
			msg = *resp.Message
		}
		return nil, fmt.Errorf("forward to port %d refused: %s", port, msg)
	}

	// After handshake, conn is a raw bidirectional TCP tunnel.
	return conn, nil
}

// WaitReady polls the agent until it responds or the context expires.
// Used during VM boot to wait for the agent to start listening.
//
// The per-attempt timeout starts short (100ms) and escalates. This avoids
// the 1-second ARP retransmit penalty: the first probe triggers an ARP
// request for the guest IP, but the guest can't reply yet (kernel still
// booting). Linux's default retrans_time_ms is 1000ms, so the host waits
// a full second before re-probing. By timing out at 100ms, closing the
// socket, and opening a fresh one, we send a new SYN that lands after the
// guest is ready — typically within 2-3 attempts (~150ms total) instead
// of one long ARP wait (~1005ms).
func (c *AgentClient) WaitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	start := time.Now()
	attempt := 0
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("agent not ready: %w", err)
		}

		// Try a lightweight exec. If the agent responds, it's ready.
		attempt++
		attemptStart := time.Now()

		// Escalating timeout: 100ms (×3) → 250ms (×3) → 500ms (×4) → 2s.
		dialTimeout := 100 * time.Millisecond
		switch {
		case attempt > 10:
			dialTimeout = 2 * time.Second
		case attempt > 6:
			dialTimeout = 500 * time.Millisecond
		case attempt > 3:
			dialTimeout = 250 * time.Millisecond
		}

		execCtx, execCancel := context.WithTimeout(ctx, dialTimeout)
		result, err := c.Exec(execCtx, []string{"true"}, nil, "")
		execCancel()

		if err == nil && result.ExitCode == 0 {
			slog.Debug("wait_ready.success",
				"attempt", attempt,
				"attempt_ms", time.Since(attemptStart).Milliseconds(),
				"total_ms", time.Since(start).Milliseconds(),
				"addr", c.tcpAddr)
			return nil
		}

		slog.Debug("wait_ready.attempt_failed",
			"attempt", attempt,
			"attempt_ms", time.Since(attemptStart).Milliseconds(),
			"total_ms", time.Since(start).Milliseconds(),
			"error", err,
			"addr", c.tcpAddr)

		// Brief pause before retry. The dial timeout provides the main
		// pacing — this just prevents a tight spin when the guest sends
		// RST ("connection refused", returns instantly).
		select {
		case <-ctx.Done():
			return fmt.Errorf("agent not ready after %d attempts (%dms): %w",
				attempt, time.Since(start).Milliseconds(), ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// SessionList returns all active sessions inside the VM.
func (c *AgentClient) SessionList(ctx context.Context) ([]proto.SessionInfo, error) {
	conn, err := c.DialControl(ctx)
	if err != nil {
		return nil, fmt.Errorf("agent connect: %w", err)
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}

	if err := proto.WriteFrame(conn, proto.EXEC_LIST_REQ, nil); err != nil {
		return nil, fmt.Errorf("agent send list: %w", err)
	}

	msgType, payload, err := proto.ReadFrame(conn)
	if err != nil {
		return nil, fmt.Errorf("agent read list: %w", err)
	}
	if msgType != proto.EXEC_LIST_RESP {
		return nil, fmt.Errorf("expected EXEC_LIST_RESP, got 0x%02x", msgType)
	}

	var sessions []proto.SessionInfo
	if err := json.Unmarshal(payload, &sessions); err != nil {
		return nil, fmt.Errorf("unmarshal sessions: %w", err)
	}
	return sessions, nil
}

// Activity queries the agent for activity info (last interaction, session counts).
func (c *AgentClient) Activity(ctx context.Context) (*proto.ActivityInfo, error) {
	conn, err := c.DialControl(ctx)
	if err != nil {
		return nil, fmt.Errorf("agent connect: %w", err)
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}

	if err := proto.WriteFrame(conn, proto.ACTIVITY_REQ, nil); err != nil {
		return nil, fmt.Errorf("agent send activity: %w", err)
	}

	msgType, payload, err := proto.ReadFrame(conn)
	if err != nil {
		return nil, fmt.Errorf("agent read activity: %w", err)
	}
	if msgType != proto.ACTIVITY_RESP {
		return nil, fmt.Errorf("expected ACTIVITY_RESP, got 0x%02x", msgType)
	}

	var info proto.ActivityInfo
	if err := json.Unmarshal(payload, &info); err != nil {
		return nil, fmt.Errorf("unmarshal activity: %w", err)
	}
	return &info, nil
}

// SessionKill sends SIGTERM to a session's process.
func (c *AgentClient) SessionKill(ctx context.Context, sessionID string) error {
	conn, err := c.DialControl(ctx)
	if err != nil {
		return fmt.Errorf("agent connect: %w", err)
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}

	req := struct {
		SessionID string `json:"session_id"`
	}{SessionID: sessionID}
	if err := proto.SendJSON(conn, proto.EXEC_KILL, req); err != nil {
		return fmt.Errorf("agent send kill: %w", err)
	}

	msgType, payload, err := proto.ReadFrame(conn)
	if err != nil {
		return fmt.Errorf("agent read kill resp: %w", err)
	}
	if msgType == proto.ERROR {
		return fmt.Errorf("agent: %s", payload)
	}
	return nil
}

// ShellSession opens a TTY session and returns both the session info and the terminal connection.
func (c *AgentClient) ShellSession(ctx context.Context, argv []string, env map[string]string, rows, cols uint16, maxIdleSec int, cwd string) (*proto.SessionInfo, engine.TerminalConn, error) {
	conn, err := c.DialControl(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("agent connect: %w", err)
	}

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
	if cwd != "" {
		req.Cwd = &cwd
	}
	if err := proto.SendJSON(conn, proto.EXEC_REQ, req); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("agent send shell: %w", err)
	}

	// Read SESSION_INFO frame
	msgType, payload, err := proto.ReadFrame(conn)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("agent read session info: %w", err)
	}
	if msgType == proto.ERROR {
		conn.Close()
		return nil, nil, fmt.Errorf("agent: %s", payload)
	}
	if msgType != proto.SESSION_INFO {
		conn.Close()
		return nil, nil, fmt.Errorf("expected SESSION_INFO, got 0x%02x", msgType)
	}
	var info proto.SessionInfo
	if err := json.Unmarshal(payload, &info); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("unmarshal session info: %w", err)
	}

	return &info, &agentTermConn{conn: conn}, nil
}

// SessionAttach reconnects to an existing session and returns the session info and terminal.
// If ifDetached is true, the attach fails if the session is currently attached by another client.
func (c *AgentClient) SessionAttach(ctx context.Context, sessionID string, ifDetached bool) (*proto.SessionInfo, engine.TerminalConn, error) {
	conn, err := c.DialControl(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("agent connect: %w", err)
	}

	req := proto.ExecRequest{SessionID: &sessionID}
	if ifDetached {
		req.IfDetached = &ifDetached
	}
	if err := proto.SendJSON(conn, proto.EXEC_REQ, req); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("agent send attach: %w", err)
	}

	// Read SESSION_INFO frame
	msgType, payload, err := proto.ReadFrame(conn)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("agent read session info: %w", err)
	}
	if msgType == proto.ERROR {
		conn.Close()
		return nil, nil, fmt.Errorf("agent: %s", payload)
	}
	if msgType != proto.SESSION_INFO {
		conn.Close()
		return nil, nil, fmt.Errorf("expected SESSION_INFO, got 0x%02x", msgType)
	}
	var info proto.SessionInfo
	if err := json.Unmarshal(payload, &info); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("unmarshal session info: %w", err)
	}

	return &info, &agentTermConn{conn: conn}, nil
}

// --- Piped Sessions (non-TTY, with scrollback+reattach) ---

// PipedSessionConn wraps a vsock connection for piped session I/O.
// Read returns STDOUT/EXIT frame payloads. Write sends STDIN frames.
type PipedSessionConn struct {
	conn net.Conn
}

// ReadFrame reads the next frame. Returns the frame type and payload.
// Callers check for STDOUT (data), EXIT (process ended), ERROR.
func (p *PipedSessionConn) ReadFrame() (byte, []byte, error) {
	return proto.ReadFrame(p.conn)
}

// WriteStdin sends bytes as a STDIN frame.
func (p *PipedSessionConn) WriteStdin(data []byte) error {
	return proto.WriteFrame(p.conn, proto.STDIN, data)
}

// Kill sends a KILL frame.
func (p *PipedSessionConn) Kill() error {
	return proto.WriteFrame(p.conn, proto.KILL, nil)
}

// Close closes the underlying connection. The session detaches
// but the process keeps running.
func (p *PipedSessionConn) Close() error {
	return p.conn.Close()
}

// PipedSession creates a non-TTY session for a long-running process.
// Returns the session info and a bidirectional connection that relays
// STDIN/STDOUT frames. The session survives host disconnect and is
// reattachable via PipedSessionAttach.
func (c *AgentClient) PipedSession(ctx context.Context, argv []string,
	env map[string]string, maxIdleSec int) (*proto.SessionInfo, *PipedSessionConn, error) {

	conn, err := c.DialControl(ctx)
	if err != nil {
		return nil, nil, err
	}

	session := true
	req := proto.ExecRequest{
		Argv:    argv,
		Env:     env,
		Session: &session,
	}
	if maxIdleSec > 0 {
		req.MaxIdleSec = &maxIdleSec
	}
	if err := proto.SendJSON(conn, proto.EXEC_REQ, req); err != nil {
		conn.Close()
		return nil, nil, err
	}

	// Read SESSION_INFO
	msgType, payload, err := proto.ReadFrame(conn)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("read session info: %w", err)
	}
	if msgType == proto.ERROR {
		conn.Close()
		return nil, nil, fmt.Errorf("agent: %s", payload)
	}
	if msgType != proto.SESSION_INFO {
		conn.Close()
		return nil, nil, fmt.Errorf("expected SESSION_INFO, got 0x%02x", msgType)
	}
	var info proto.SessionInfo
	if err := json.Unmarshal(payload, &info); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("parse session info: %w", err)
	}

	return &info, &PipedSessionConn{conn: conn}, nil
}

// PipedSessionAttach reconnects to an existing piped session.
// Returns the session info and a PipedSessionConn for I/O.
func (c *AgentClient) PipedSessionAttach(ctx context.Context,
	sessionID string, ifDetached bool) (*proto.SessionInfo, *PipedSessionConn, error) {

	conn, err := c.DialControl(ctx)
	if err != nil {
		return nil, nil, err
	}

	req := proto.ExecRequest{SessionID: &sessionID}
	if ifDetached {
		req.IfDetached = &ifDetached
	}
	if err := proto.SendJSON(conn, proto.EXEC_REQ, req); err != nil {
		conn.Close()
		return nil, nil, err
	}

	// Read SESSION_INFO (or ERROR)
	msgType, payload, err := proto.ReadFrame(conn)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("read session info: %w", err)
	}
	if msgType == proto.ERROR {
		conn.Close()
		return nil, nil, fmt.Errorf("agent: %s", payload)
	}
	if msgType != proto.SESSION_INFO {
		conn.Close()
		return nil, nil, fmt.Errorf("expected SESSION_INFO, got 0x%02x", msgType)
	}
	var info proto.SessionInfo
	if err := json.Unmarshal(payload, &info); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("parse session info: %w", err)
	}

	// Scrollback follows as STDOUT frames before the live stream.
	// The caller handles these identically to live STDOUT frames.
	return &info, &PipedSessionConn{conn: conn}, nil
}

// --- File Operations ---

// FileRead reads a file from the guest and writes its contents to w.
// Returns the file size and mode.
// FileReadOpts controls server-side truncation for file reads.
type FileReadOpts struct {
	Offset   int // 1-indexed line number to start from (0 = beginning)
	Limit    int // max lines to return (0 = unlimited)
	MaxBytes int // max bytes to return (0 = unlimited)
}

func (c *AgentClient) FileRead(ctx context.Context, path string, w io.Writer, opts ...FileReadOpts) (size int64, mode string, err error) {
	conn, err := c.DialControl(ctx)
	if err != nil {
		return 0, "", err
	}
	defer conn.Close()

	// Close connection on context cancellation — this makes lohar's
	// WriteFrame fail with broken pipe, stopping the transfer immediately.
	// Without this, a cancelled FileRead of a 100MB file would run to
	// completion on the guest side.
	if ctx.Done() != nil {
		done := make(chan struct{})
		defer close(done)
		go func() {
			select {
			case <-ctx.Done():
				conn.Close()
			case <-done:
			}
		}()
	}

	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}

	reqPayload := map[string]any{"path": path}
	if len(opts) > 0 {
		o := opts[0]
		if o.Offset > 0 {
			reqPayload["offset"] = o.Offset
		}
		if o.Limit > 0 {
			reqPayload["limit"] = o.Limit
		}
		if o.MaxBytes > 0 {
			reqPayload["max_bytes"] = o.MaxBytes
		}
	}
	if err := proto.SendJSON(conn, proto.FILE_READ_REQ, reqPayload); err != nil {
		return 0, "", fmt.Errorf("send file read: %w", err)
	}

	// Read FILE_READ_RESP
	msgType, payload, err := proto.ReadFrame(conn)
	if err != nil {
		return 0, "", fmt.Errorf("read file resp: %w", err)
	}
	if msgType == proto.ERROR {
		return 0, "", fmt.Errorf("agent: %s", payload)
	}
	if msgType != proto.FILE_READ_RESP {
		return 0, "", fmt.Errorf("expected FILE_READ_RESP, got 0x%02x", msgType)
	}
	var resp struct {
		Size int64  `json:"size"`
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(payload, &resp); err != nil {
		return 0, "", fmt.Errorf("unmarshal file read resp: %w", err)
	}

	// Read STDOUT frames until EXIT
	var written int64
	for {
		msgType, payload, err = proto.ReadFrame(conn)
		if err != nil {
			return written, resp.Mode, err
		}
		switch msgType {
		case proto.STDOUT:
			n, _ := w.Write(payload)
			written += int64(n)
		case proto.EXIT:
			return written, resp.Mode, nil
		case proto.ERROR:
			return written, resp.Mode, fmt.Errorf("agent: %s", payload)
		}
	}
}

// FileWrite writes content from r to a file in the guest.
func (c *AgentClient) FileWrite(ctx context.Context, path, mode string, size int64, r io.Reader) error {
	conn, err := c.DialControl(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}

	if err := proto.SendJSON(conn, proto.FILE_WRITE_REQ, map[string]any{
		"path": path, "mode": mode, "size": size,
	}); err != nil {
		return fmt.Errorf("send file write: %w", err)
	}

	// Send file content as STDIN frames
	buf := make([]byte, 32768)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if werr := proto.WriteFrame(conn, proto.STDIN, buf[:n]); werr != nil {
				return fmt.Errorf("write stdin frame: %w", werr)
			}
		}
		if err != nil {
			break
		}
	}

	// Read FILE_WRITE_RESP
	msgType, payload, err := proto.ReadFrame(conn)
	if err != nil {
		return fmt.Errorf("read write resp: %w", err)
	}
	if msgType == proto.ERROR {
		return fmt.Errorf("agent: %s", payload)
	}
	return nil
}

// FileStat returns file info for a path in the guest.
func (c *AgentClient) FileStat(ctx context.Context, path string) (*proto.FileInfo, error) {
	conn, err := c.DialControl(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}

	if err := proto.SendJSON(conn, proto.FILE_STAT_REQ, map[string]string{"path": path}); err != nil {
		return nil, fmt.Errorf("send file stat: %w", err)
	}

	msgType, payload, err := proto.ReadFrame(conn)
	if err != nil {
		return nil, fmt.Errorf("read stat resp: %w", err)
	}
	if msgType == proto.ERROR {
		return nil, fmt.Errorf("agent: %s", payload)
	}
	if msgType != proto.FILE_STAT_RESP {
		return nil, fmt.Errorf("expected FILE_STAT_RESP, got 0x%02x", msgType)
	}
	var info proto.FileInfo
	if err := json.Unmarshal(payload, &info); err != nil {
		return nil, fmt.Errorf("unmarshal file stat: %w", err)
	}
	return &info, nil
}

// FileList returns directory contents for a path in the guest.
func (c *AgentClient) FileList(ctx context.Context, path string) ([]proto.FileInfo, error) {
	conn, err := c.DialControl(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}

	if err := proto.SendJSON(conn, proto.FILE_LS_REQ, map[string]string{"path": path}); err != nil {
		return nil, fmt.Errorf("send file ls: %w", err)
	}

	msgType, payload, err := proto.ReadFrame(conn)
	if err != nil {
		return nil, fmt.Errorf("read ls resp: %w", err)
	}
	if msgType == proto.ERROR {
		return nil, fmt.Errorf("agent: %s", payload)
	}
	if msgType != proto.FILE_LS_RESP {
		return nil, fmt.Errorf("expected FILE_LS_RESP, got 0x%02x", msgType)
	}
	var files []proto.FileInfo
	if err := json.Unmarshal(payload, &files); err != nil {
		return nil, fmt.Errorf("unmarshal file list: %w", err)
	}
	return files, nil
}

// agentTermConn wraps the vsock connection as engine.TerminalConn.
type agentTermConn struct {
	conn    net.Conn
	mu      sync.Mutex   // serializes writes
	readBuf bytes.Buffer // leftover payload from previous STDOUT frame
}

func (t *agentTermConn) Read(p []byte) (int, error) {
	if t.readBuf.Len() > 0 {
		return t.readBuf.Read(p)
	}
	msgType, payload, err := proto.ReadFrame(t.conn)
	if err != nil {
		return 0, err
	}
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
	if err := proto.WriteFrame(t.conn, proto.STDIN, p); err != nil {
		return 0, err
	}
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
