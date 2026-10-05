package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path"
	"sync"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// AgentClient communicates with lohar over the libkrun-bridged Unix sockets.
type AgentClient struct {
	controlSock string
	forwardSock string
	token       string // auth token, empty = no auth
	infoMu      sync.RWMutex
	info        proto.AgentInfo // a successful INFO response from this guest boot
	infoKnown   bool
	leaseMu     sync.Mutex
	leases      map[string]net.Conn // successful FREEZE_ACK held open until THAW
}

// NewTestClient connects to the agent's test-mode Unix sockets.
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
	conn, err := d.DialContext(ctx, "unix", c.controlSock)
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
	conn, err := d.DialContext(ctx, "unix", c.forwardSock)
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

// Info asks the baked-in guest agent which behaviors it implements. Only the
// exact unexpected-frame reply identifies a legacy lohar; connection/auth,
// timeout, protocol and other guest errors leave capabilities unknown.
func (c *AgentClient) Info(ctx context.Context) (proto.AgentInfo, error) {
	conn, err := c.DialControl(ctx)
	if err != nil {
		return proto.AgentInfo{}, fmt.Errorf("agent connect for info: %w", err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	if err := proto.WriteFrame(conn, proto.INFO_REQ, nil); err != nil {
		return proto.AgentInfo{}, fmt.Errorf("agent send info: %w", err)
	}
	// A failed probe cannot establish that the guest is old or supports a
	// feature. Invalidate any previous answer rather than using stale info.
	c.infoMu.Lock()
	c.infoKnown = false
	c.infoMu.Unlock()
	msgType, payload, err := proto.ReadFrame(conn)
	if err != nil {
		return proto.AgentInfo{}, fmt.Errorf("agent read info: %w", err)
	}
	if msgType == proto.ERROR {
		if unexpectedFrame(payload, proto.INFO_REQ) {
			info := proto.AgentInfo{Legacy: true}
			c.cacheInfo(info)
			return info, nil
		}
		return proto.AgentInfo{}, fmt.Errorf("guest agent info failed: %s", payload)
	}
	if msgType != proto.INFO_RESP {
		return proto.AgentInfo{}, fmt.Errorf("expected INFO_RESP, got 0x%02x", msgType)
	}
	var info proto.AgentInfo
	if err := json.Unmarshal(payload, &info); err != nil {
		return proto.AgentInfo{}, fmt.Errorf("unmarshal agent info: %w", err)
	}
	if info.Version == "" || info.Features == nil {
		return proto.AgentInfo{}, fmt.Errorf("incomplete guest agent info")
	}
	c.cacheInfo(info)
	return info, nil
}

func unexpectedFrame(payload []byte, msgType byte) bool {
	return string(payload) == fmt.Sprintf("unexpected frame type 0x%02x", msgType)
}

func (c *AgentClient) cacheInfo(info proto.AgentInfo) {
	c.infoMu.Lock()
	c.info, c.infoKnown = info, true
	c.infoMu.Unlock()
}

func (c *AgentClient) requireFeature(ctx context.Context, feature proto.AgentFeature) error {
	c.infoMu.RLock()
	info, known := c.info, c.infoKnown
	c.infoMu.RUnlock()
	if !known {
		var err error
		info, err = c.Info(ctx)
		if err != nil {
			return fmt.Errorf("guest agent capabilities unavailable: %w", err)
		}
	}
	if !info.Has(feature) {
		return engine.GuestAgentOutdated(string(feature))
	}
	return nil
}

// Exec runs a command non-interactively without an implicit guest-wide sync.
func (c *AgentClient) Exec(ctx context.Context, argv []string, env map[string]string, cwd string) (engine.ExecResult, error) {
	return c.ExecWithSync(ctx, argv, env, cwd, false)
}

// ExecWithSync requests a guest-wide sync before EXIT only when explicitly
// enabled and the baked-in guest advertised the new request field.
func (c *AgentClient) ExecWithSync(ctx context.Context, argv []string, env map[string]string, cwd string, sync bool) (engine.ExecResult, error) {
	if sync {
		if err := c.requireFeature(ctx, proto.FeatureExecSync); err != nil {
			return engine.ExecResult{}, err
		}
	}
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
	if sync {
		req.Sync = &sync
	}
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

// ErrPortRefused means the guest agent couldn't connect to the requested port:
// nothing is listening there (yet). Nothing was sent to the guest app.
var ErrPortRefused = errors.New("refused")

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
		return nil, fmt.Errorf("forward to port %d: %w: %s", port, ErrPortRefused, msg)
	}

	// After handshake, conn is a raw bidirectional TCP tunnel.
	return conn, nil
}

// WaitReady polls the agent until it responds or the context expires.
// Used during VM boot to wait for the agent to start listening.
//
// Short connection timeouts on early attempts avoid waiting through a full
// boot timeout when the guest's control socket is not accepting yet.
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
				"addr", c.controlSock)
			return nil
		}

		slog.Debug("wait_ready.attempt_failed",
			"attempt", attempt,
			"attempt_ms", time.Since(attemptStart).Milliseconds(),
			"total_ms", time.Since(start).Milliseconds(),
			"error", err,
			"addr", c.controlSock)

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

// NetConfig reconciles the guest's eth0 to a fresh IP/MAC identity after a
// memory-restore fork. The restored guest holds both source identities in RAM.
// Ack is a NET_CONFIG frame with nil payload; ERROR surfaces guest failure.
func (c *AgentClient) NetConfig(ctx context.Context, ipCIDR, gateway, mac string) error {
	conn, err := c.DialControl(ctx)
	if err != nil {
		return fmt.Errorf("agent connect: %w", err)
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}

	req := struct {
		IPCIDR  string `json:"ip_cidr"`
		Gateway string `json:"gateway"`
		MAC     string `json:"mac"`
	}{IPCIDR: ipCIDR, Gateway: gateway, MAC: mac}
	if err := proto.SendJSON(conn, proto.NET_CONFIG, req); err != nil {
		return fmt.Errorf("agent send net config: %w", err)
	}

	msgType, payload, err := proto.ReadFrame(conn)
	if err != nil {
		return fmt.Errorf("agent read net config ack: %w", err)
	}
	if msgType == proto.ERROR {
		if unexpectedFrame(payload, proto.NET_CONFIG) {
			return engine.GuestAgentOutdated("network reconciliation (net_config)")
		}
		return fmt.Errorf("guest net config failed: %s", string(payload))
	}
	if msgType != proto.NET_CONFIG {
		return fmt.Errorf("expected NET_CONFIG ack, got 0x%02x", msgType)
	}
	return nil
}

// Reseed credits fresh host entropy to the restored guest's kernel CRNG.
// It must be acknowledged before the caller can use a cloned memory image.
func (c *AgentClient) Reseed(ctx context.Context, seed [proto.ReseedBytes]byte) error {
	defer clear(seed[:])
	conn, err := c.DialControl(ctx)
	if err != nil {
		return fmt.Errorf("agent connect for reseed: %w", err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	if err := proto.WriteFrame(conn, proto.RESEED, seed[:]); err != nil {
		return fmt.Errorf("agent send reseed: %w", err)
	}
	msgType, payload, err := proto.ReadFrame(conn)
	if err != nil {
		return fmt.Errorf("agent read reseed ack: %w", err)
	}
	if msgType == proto.ERROR {
		return fmt.Errorf("guest reseed failed: %s", payload)
	}
	if msgType != proto.RESEED || len(payload) != 0 {
		return fmt.Errorf("expected empty RESEED ack, got 0x%02x (%d bytes)", msgType, len(payload))
	}
	return nil
}

// ErrFSFreezeRejected means the guest explicitly refused FREEZE, or the
// host rejected it before sending the request. No new lease exists; callers
// must not send an independent THAW that could release another owner's freeze.
var ErrFSFreezeRejected = errors.New("guest filesystem freeze rejected")

// ErrFSFreezeReleaseUnconfirmed means no THAW_ACK was received. The control
// connection has been closed, which asks the guest to auto-thaw on EOF, but
// the caller must not assume the filesystem is released without confirmation.
var ErrFSFreezeReleaseUnconfirmed = errors.New("guest filesystem thaw not confirmed")

// Freeze flushes and suspends writes to an exact guest mountpoint. A successful
// ACK retains this control connection as a lease until Thaw; a failed or timed
// out request closes it so even a delayed guest freeze is undone on EOF.
func (c *AgentClient) Freeze(ctx context.Context, mount string) error {
	if err := c.requireFeature(ctx, proto.FeatureFSFreeze); err != nil {
		return fmt.Errorf("%w: %w", ErrFSFreezeRejected, err)
	}
	key := path.Clean(mount)
	c.leaseMu.Lock()
	_, held := c.leases[key]
	c.leaseMu.Unlock()
	if held {
		return fmt.Errorf("%w: guest filesystem %q already has a freeze lease", ErrFSFreezeRejected, mount)
	}
	conn, err := c.DialControl(ctx)
	if err != nil {
		return fmt.Errorf("%w: agent connect for filesystem quiescence: %w", ErrFSFreezeRejected, err)
	}
	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()
	if err := sendFSFreeze(ctx, conn, mount, proto.FREEZE_REQ, proto.FREEZE_ACK); err != nil {
		return err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return fmt.Errorf("clear freeze lease deadline: %w", err)
	}
	c.leaseMu.Lock()
	if c.leases == nil {
		c.leases = make(map[string]net.Conn)
	}
	if _, held := c.leases[key]; held {
		c.leaseMu.Unlock()
		return fmt.Errorf("guest filesystem %q already has a freeze lease", mount)
	}
	c.leases[key] = conn
	c.leaseMu.Unlock()
	conn = nil // ownership moved to Thaw, not to the request context
	return nil
}

// Thaw sends THAW_REQ on the retained freeze connection and always closes
// that connection after the response. Without a lease (ambiguous failed
// Freeze), it sends a separate request without re-probing INFO so an already
// frozen filesystem can still be released.
func (c *AgentClient) Thaw(ctx context.Context, mount string) error {
	key := path.Clean(mount)
	c.leaseMu.Lock()
	conn := c.leases[key]
	delete(c.leases, key)
	c.leaseMu.Unlock()
	if conn == nil {
		var err error
		conn, err = c.DialControl(ctx)
		if err != nil {
			return fmt.Errorf("%w: agent connect: %w", ErrFSFreezeReleaseUnconfirmed, err)
		}
	}
	defer conn.Close() // EOF asks the guest to thaw even if the ACK is lost
	if err := sendFSFreeze(ctx, conn, mount, proto.THAW_REQ, proto.THAW_ACK); err != nil {
		return fmt.Errorf("%w: %w", ErrFSFreezeReleaseUnconfirmed, err)
	}
	return nil
}

func sendFSFreeze(ctx context.Context, conn net.Conn, mount string, request, ack byte) error {
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			if request == proto.FREEZE_REQ {
				return fmt.Errorf("%w: set filesystem quiescence deadline: %w", ErrFSFreezeRejected, err)
			}
			return fmt.Errorf("set filesystem quiescence deadline: %w", err)
		}
	}
	if err := proto.SendJSON(conn, request, proto.FSFreezeRequest{Mount: mount}); err != nil {
		return fmt.Errorf("agent send filesystem quiescence: %w", err)
	}
	msgType, payload, err := proto.ReadFrame(conn)
	if err != nil {
		return fmt.Errorf("agent read filesystem quiescence ack: %w", err)
	}
	if msgType == proto.ERROR {
		if unexpectedFrame(payload, request) {
			if request == proto.FREEZE_REQ {
				return fmt.Errorf("%w: %w", ErrFSFreezeRejected, engine.GuestAgentOutdated(string(proto.FeatureFSFreeze)))
			}
			return engine.GuestAgentOutdated(string(proto.FeatureFSFreeze))
		}
		if request == proto.FREEZE_REQ {
			return fmt.Errorf("%w: guest filesystem quiescence failed: %s", ErrFSFreezeRejected, payload)
		}
		return fmt.Errorf("guest filesystem quiescence failed: %s", payload)
	}
	if msgType != ack || len(payload) != 0 {
		return fmt.Errorf("expected empty filesystem quiescence ack 0x%02x, got 0x%02x (%d bytes)", ack, msgType, len(payload))
	}
	return nil
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

// PipedSession creates a non-TTY session for a long-running process. Returns
// the session info and a bidirectional connection that relays STDIN/STDOUT
// (and, with spec.Stderr, STDERR) frames. The session survives host
// disconnect and is reattachable via PipedSessionAttach.
func (c *AgentClient) PipedSession(ctx context.Context, spec engine.PipedSpec) (*proto.SessionInfo, *PipedSessionConn, error) {

	conn, err := c.DialControl(ctx)
	if err != nil {
		return nil, nil, err
	}

	session := true
	req := proto.ExecRequest{
		Argv:    spec.Cmd,
		Env:     spec.Env,
		Session: &session,
	}
	if spec.MaxIdleSec > 0 {
		req.MaxIdleSec = &spec.MaxIdleSec
	}
	if spec.Cwd != "" {
		req.Cwd = &spec.Cwd
	}
	if spec.Stderr {
		req.Stderr = &spec.Stderr
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
