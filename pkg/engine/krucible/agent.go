package krucible

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

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

func queryAgentInfo(ctx context.Context, ag *agent.AgentClient) (proto.AgentInfo, error) {
	probeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return ag.Info(probeCtx)
}

// RequireGuestAgentFeature implements engine.GuestAgentCapabilities. Cold
// sandboxes must boot before checking: their lohar may differ from the
// daemon's current image, and every boot refreshes the in-memory report.
func (e *Engine) RequireGuestAgentFeature(ctx context.Context, id string, feature proto.AgentFeature) error {
	if err := e.EnsureHot(ctx, id); err != nil {
		return err
	}
	vm, err := e.getVM(id)
	if err != nil {
		return err
	}
	return vm.requireFeature(feature)
}

func (vm *VM) requireFeature(feature proto.AgentFeature) error {
	vm.mu.Lock()
	info, infoErr := vm.AgentInfo, vm.AgentInfoErr
	vm.mu.Unlock()
	if infoErr != nil {
		return fmt.Errorf("guest agent capabilities unavailable: %w", infoErr)
	}
	if !info.Legacy && info.Version == "" {
		return fmt.Errorf("guest agent capabilities unavailable: no response cached")
	}
	if !info.Has(feature) {
		return engine.GuestAgentOutdated(string(feature))
	}
	return nil
}

// A restored memory image inherits its source's kernel CRNG state. Unlike
// capabilities that only affect an optional operation, an unknown capability
// cannot safely permit this restore.
func reseedRestoredGuest(ctx context.Context, vmID string, ag *agent.AgentClient, info proto.AgentInfo, infoErr error) (unsupported bool, err error) {
	if infoErr != nil {
		return false, fmt.Errorf("guest agent capabilities unavailable: %w", infoErr)
	}
	if !info.Legacy && info.Version == "" {
		return false, fmt.Errorf("guest agent capabilities unavailable: no response cached")
	}
	if !info.Has(proto.FeatureReseedCRNG) {
		slog.Warn("krucible.guest_reseed_unsupported", "id", vmID, "agent_version", info.Version)
		return true, nil
	}
	var seed [proto.ReseedBytes]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return false, fmt.Errorf("generate host entropy: %w", err)
	}
	defer clear(seed[:])
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := ag.Reseed(rctx, seed); err != nil {
		return false, fmt.Errorf("agent reseed: %w", err)
	}
	return false, nil
}

// PipedSession implements engine.PipedSessionEngine.
func (e *Engine) PipedSession(ctx context.Context, id string, spec engine.PipedSpec) (*proto.SessionInfo, engine.PipedConn, error) {
	ag, err := e.agentFor(id)
	if err != nil {
		return nil, nil, err
	}
	if spec.Stderr {
		vm, err := e.getVM(id)
		if err != nil {
			return nil, nil, err
		}
		vm.mu.Lock()
		legacy := vm.AgentInfo.Legacy
		vm.mu.Unlock()
		if legacy {
			// Old lohar ignores this flag and always merges stderr into stdout.
			spec.Stderr = false
		}
	}
	return ag.PipedSession(ctx, spec)
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
