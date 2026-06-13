package krucible

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/sahil-shubham/bhatti/pkg/agent"
	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// The agent surface delegates to the lohar client over the bridged vsock UDS.
// Identical behavior to the firecracker engine; only the transport differs.

func (e *Engine) Exec(ctx context.Context, id string, cmd []string) (engine.ExecResult, error) {
	ag, err := e.agentFor(id)
	if err != nil {
		return engine.ExecResult{}, err
	}
	return ag.Exec(ctx, cmd, nil, "")
}

func (e *Engine) Shell(ctx context.Context, id string) (engine.TerminalConn, error) {
	_, term, err := e.ShellSession(ctx, id)
	return term, err
}

// ShellSession implements engine.ShellSessioner.
func (e *Engine) ShellSession(ctx context.Context, id string) (string, engine.TerminalConn, error) {
	ag, err := e.agentFor(id)
	if err != nil {
		return "", nil, err
	}
	info, term, err := ag.ShellSession(ctx, []string{"/bin/bash", "-li"},
		map[string]string{"TERM": "xterm-256color"}, 24, 80, 3600, "/workspace")
	if err != nil {
		return "", nil, err
	}
	return info.SessionID, term, nil
}

// ShellAttach implements engine.SessionAttacher.
func (e *Engine) ShellAttach(ctx context.Context, id, sessionID string, ifDetached bool) (*proto.SessionInfo, engine.TerminalConn, error) {
	ag, err := e.agentFor(id)
	if err != nil {
		return nil, nil, err
	}
	return ag.SessionAttach(ctx, sessionID, ifDetached)
}

// ExecDetached implements engine.DetachedExecEngine.
func (e *Engine) ExecDetached(ctx context.Context, id string, cmd []string, outputFile string) (int, string, error) {
	ag, err := e.agentFor(id)
	if err != nil {
		return 0, "", err
	}
	return ag.ExecDetached(ctx, cmd, nil, "", outputFile)
}

// ExecStream implements engine.StreamExecEngine.
func (e *Engine) ExecStream(ctx context.Context, id string, cmd []string, onEvent func(engine.StreamEvent)) error {
	ag, err := e.agentFor(id)
	if err != nil {
		return err
	}
	conn, err := ag.DialControl(ctx)
	if err != nil {
		return fmt.Errorf("agent connect: %w", err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	if err := proto.SendJSON(conn, proto.EXEC_REQ, proto.ExecRequest{Argv: cmd}); err != nil {
		return fmt.Errorf("agent send exec: %w", err)
	}
	for {
		msgType, payload, err := proto.ReadFrame(conn)
		if err != nil {
			return fmt.Errorf("agent read: %w", err)
		}
		switch msgType {
		case proto.STDOUT:
			onEvent(engine.StreamEvent{Type: "stdout", Data: string(payload)})
		case proto.STDERR:
			onEvent(engine.StreamEvent{Type: "stderr", Data: string(payload)})
		case proto.EXIT:
			code, _ := proto.ParseExitCode(payload)
			c := int(code)
			onEvent(engine.StreamEvent{Type: "exit", ExitCode: &c})
			return nil
		case proto.ERROR:
			onEvent(engine.StreamEvent{Type: "error", Data: string(payload)})
			return fmt.Errorf("agent: %s", payload)
		}
	}
}

// PipedSession implements engine.PipedSessionEngine.
func (e *Engine) PipedSession(ctx context.Context, id string, cmd []string,
	env map[string]string, maxIdleSec int) (*proto.SessionInfo, engine.PipedConn, error) {
	ag, err := e.agentFor(id)
	if err != nil {
		return nil, nil, err
	}
	return ag.PipedSession(ctx, cmd, env, maxIdleSec)
}

func (e *Engine) PipedSessionAttach(ctx context.Context, id, sessionID string,
	ifDetached bool) (*proto.SessionInfo, engine.PipedConn, error) {
	ag, err := e.agentFor(id)
	if err != nil {
		return nil, nil, err
	}
	return ag.PipedSessionAttach(ctx, sessionID, ifDetached)
}

func (e *Engine) SessionList(ctx context.Context, id string) ([]proto.SessionInfo, error) {
	ag, err := e.agentFor(id)
	if err != nil {
		return nil, err
	}
	return ag.SessionList(ctx)
}

func (e *Engine) ListeningPorts(ctx context.Context, id string) ([]int, error) {
	ag, err := e.agentFor(id)
	if err != nil {
		return nil, err
	}
	result, err := ag.Exec(ctx, []string{"ss", "-tln", "--no-header"}, nil, "")
	if err != nil {
		return nil, err
	}
	return parseSSOutput(result.Stdout), nil
}

func (e *Engine) Tunnel(ctx context.Context, id string, port int) (io.ReadWriteCloser, error) {
	ag, err := e.agentFor(id)
	if err != nil {
		return nil, err
	}
	return ag.Forward(ctx, uint16(port))
}

// --- File operations ---

func (e *Engine) FileRead(ctx context.Context, id, path string, w io.Writer, opts ...agent.FileReadOpts) (int64, string, error) {
	ag, err := e.agentFor(id)
	if err != nil {
		return 0, "", err
	}
	return ag.FileRead(ctx, path, w, opts...)
}

func (e *Engine) FileWrite(ctx context.Context, id, path, mode string, size int64, r io.Reader) error {
	ag, err := e.agentFor(id)
	if err != nil {
		return err
	}
	return ag.FileWrite(ctx, path, mode, size, r)
}

func (e *Engine) FileStat(ctx context.Context, id, path string) (*proto.FileInfo, error) {
	ag, err := e.agentFor(id)
	if err != nil {
		return nil, err
	}
	return ag.FileStat(ctx, path)
}

func (e *Engine) FileList(ctx context.Context, id, path string) ([]proto.FileInfo, error) {
	ag, err := e.agentFor(id)
	if err != nil {
		return nil, err
	}
	return ag.FileList(ctx, path)
}

// parseSSOutput extracts listening ports from `ss -tln` output.
func parseSSOutput(output string) []int {
	seen := map[int]bool{}
	var ports []int
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		addr := fields[3]
		idx := strings.LastIndex(addr, ":")
		if idx < 0 {
			continue
		}
		var p int
		fmt.Sscanf(addr[idx+1:], "%d", &p)
		if p > 0 && p < 65536 && !seen[p] {
			seen[p] = true
			ports = append(ports, p)
		}
	}
	return ports
}
