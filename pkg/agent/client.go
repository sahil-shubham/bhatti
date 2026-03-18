package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/sahilshubham/bhatti/pkg/agent/proto"
	"github.com/sahilshubham/bhatti/pkg/engine"
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

// dialControl opens a connection to the control channel (port 1024).
func (c *AgentClient) dialControl() (net.Conn, error) {
	var conn net.Conn
	var err error
	if c.tcpAddr != "" {
		conn, err = net.Dial("tcp", net.JoinHostPort(c.tcpAddr, fmt.Sprint(proto.VsockPortControl)))
	} else if c.isVsock {
		conn, err = c.dialVsockPort(c.controlSock, proto.VsockPortControl)
	} else {
		conn, err = net.Dial("unix", c.controlSock)
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
func (c *AgentClient) dialForward() (net.Conn, error) {
	var conn net.Conn
	var err error
	if c.tcpAddr != "" {
		conn, err = net.Dial("tcp", net.JoinHostPort(c.tcpAddr, fmt.Sprint(proto.VsockPortForward)))
	} else if c.isVsock {
		conn, err = c.dialVsockPort(c.forwardSock, proto.VsockPortForward)
	} else {
		conn, err = net.Dial("unix", c.forwardSock)
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
func (c *AgentClient) dialVsockPort(udsPath string, port uint32) (net.Conn, error) {
	conn, err := net.Dial("unix", udsPath)
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
	conn, err := c.dialControl()
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
			// Unknown frame type — skip (forward compatibility).
		}
	}
}

// Shell opens an interactive TTY session and returns a TerminalConn.
func (c *AgentClient) Shell(ctx context.Context, argv []string, env map[string]string, rows, cols uint16) (engine.TerminalConn, error) {
	conn, err := c.dialControl()
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
func (c *AgentClient) WaitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("agent not ready: %w", err)
		}

		// Try a lightweight exec. If the agent responds, it's ready.
		execCtx, execCancel := context.WithTimeout(ctx, 2*time.Second)
		result, err := c.Exec(execCtx, []string{"true"}, nil, "")
		execCancel()

		if err == nil && result.ExitCode == 0 {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("agent not ready: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// SessionList returns all active sessions inside the VM.
func (c *AgentClient) SessionList(ctx context.Context) ([]proto.SessionInfo, error) {
	conn, err := c.dialControl()
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

// SessionKill sends SIGTERM to a session's process.
func (c *AgentClient) SessionKill(ctx context.Context, sessionID string) error {
	conn, err := c.dialControl()
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
func (c *AgentClient) ShellSession(ctx context.Context, argv []string, env map[string]string, rows, cols uint16) (*proto.SessionInfo, engine.TerminalConn, error) {
	conn, err := c.dialControl()
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
	json.Unmarshal(payload, &info)

	return &info, &agentTermConn{conn: conn}, nil
}

// SessionAttach reconnects to an existing session and returns the session info and terminal.
func (c *AgentClient) SessionAttach(ctx context.Context, sessionID string) (*proto.SessionInfo, engine.TerminalConn, error) {
	conn, err := c.dialControl()
	if err != nil {
		return nil, nil, fmt.Errorf("agent connect: %w", err)
	}

	req := proto.ExecRequest{SessionID: &sessionID}
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
	json.Unmarshal(payload, &info)

	return &info, &agentTermConn{conn: conn}, nil
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
