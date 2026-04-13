//go:build linux

package firecracker

import (
	"context"
	"fmt"
	"strings"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// --- Exec, Shell, ListeningPorts, Tunnel ---

func (e *Engine) Exec(ctx context.Context, id string, cmd []string) (engine.ExecResult, error) {
	vm, err := e.getVM(id)
	if err != nil {
		return engine.ExecResult{}, err
	}

	vm.stateMu.Lock()
	if vm.Thermal != "hot" {
		vm.stateMu.Unlock()
		return engine.ExecResult{}, fmt.Errorf("sandbox %q is not hot (thermal=%s)", id, vm.Thermal)
	}
	ag := vm.Agent
	vm.stateMu.Unlock()

	return ag.Exec(ctx, cmd, nil, "")
}

// ExecStream implements engine.StreamExecEngine. It sends STDOUT/STDERR/EXIT
// frames as StreamEvents via the callback as they arrive from the agent.
func (e *Engine) ExecStream(ctx context.Context, id string, cmd []string, onEvent func(engine.StreamEvent)) error {
	vm, err := e.getVM(id)
	if err != nil {
		return err
	}

	vm.stateMu.Lock()
	if vm.Thermal != "hot" {
		vm.stateMu.Unlock()
		return fmt.Errorf("sandbox %q is not hot (thermal=%s)", id, vm.Thermal)
	}
	ag := vm.Agent
	vm.stateMu.Unlock()

	conn, err := ag.DialControl(ctx)
	if err != nil {
		return fmt.Errorf("agent connect: %w", err)
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}

	req := proto.ExecRequest{Argv: cmd}
	if err := proto.SendJSON(conn, proto.EXEC_REQ, req); err != nil {
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

func (e *Engine) Shell(ctx context.Context, id string) (engine.TerminalConn, error) {
	_, term, err := e.ShellSession(ctx, id)
	return term, err
}

// ShellSession opens a new TTY session and returns the session ID + terminal.
// Implements engine.ShellSessioner.
func (e *Engine) ShellSession(ctx context.Context, id string) (string, engine.TerminalConn, error) {
	vm, err := e.getVM(id)
	if err != nil {
		return "", nil, err
	}

	// Capture agent ref under lock, release before long-lived Shell call.
	vm.stateMu.Lock()
	if vm.Thermal != "hot" {
		vm.stateMu.Unlock()
		return "", nil, fmt.Errorf("sandbox %q is not hot (thermal=%s)", id, vm.Thermal)
	}
	ag := vm.Agent
	vm.stateMu.Unlock()

	info, term, err := ag.ShellSession(ctx, []string{"/bin/bash", "-li"}, map[string]string{
		"TERM": "xterm-256color",
	}, 24, 80, 3600, "/workspace") // 1 hour idle timeout for detached sessions
	if err != nil {
		return "", nil, err
	}
	return info.SessionID, term, nil
}

// ShellAttach reconnects to an existing TTY session by ID.
// Implements engine.SessionAttacher.
func (e *Engine) ShellAttach(ctx context.Context, id, sessionID string, ifDetached bool) (*proto.SessionInfo, engine.TerminalConn, error) {
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

	return ag.SessionAttach(ctx, sessionID, ifDetached)
}

func (e *Engine) ListeningPorts(ctx context.Context, id string) ([]int, error) {
	vm, err := e.getVM(id)
	if err != nil {
		return nil, err
	}

	vm.stateMu.Lock()
	if vm.Thermal != "hot" {
		vm.stateMu.Unlock()
		return nil, fmt.Errorf("sandbox %q is not hot (thermal=%s)", id, vm.Thermal)
	}
	ag := vm.Agent
	vm.stateMu.Unlock()

	result, err := ag.Exec(ctx, []string{"ss", "-tln", "--no-header"}, nil, "")
	if err != nil {
		return nil, err
	}
	return parseSSOutput(result.Stdout), nil
}

// --- Session Operations ---

func (e *Engine) SessionList(ctx context.Context, id string) ([]proto.SessionInfo, error) {
	vm, err := e.getVM(id)
	if err != nil {
		return nil, err
	}

	vm.stateMu.Lock()
	if vm.Thermal != "hot" {
		vm.stateMu.Unlock()
		return nil, fmt.Errorf("sandbox %q is not hot (thermal=%s)", id, vm.Thermal)
	}
	ag := vm.Agent
	vm.stateMu.Unlock()

	return ag.SessionList(ctx)
}

// parseSSOutput is duplicated from docker/docker.go — extracts listening ports from `ss -tln` output.
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
