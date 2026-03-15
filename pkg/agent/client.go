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
// In production, vsockPath is the Firecracker vsock UDS and the client
// performs the CONNECT/OK handshake. In tests, controlSock and forwardSock
// are plain Unix sockets and no handshake is needed.
type AgentClient struct {
	controlSock string
	forwardSock string
	isVsock     bool
}

// NewVsockClient creates a client that connects through a Firecracker vsock UDS.
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
