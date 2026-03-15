//go:build linux

package agent

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startTestAgent starts the bhatti-agent binary in test mode.
// The BHATTI_AGENT_BIN env var must point to the compiled agent binary.
func startTestAgent(t *testing.T) (controlSock, forwardSock string, cleanup func()) {
	t.Helper()

	agentBin := os.Getenv("BHATTI_AGENT_BIN")
	if agentBin == "" {
		t.Skip("BHATTI_AGENT_BIN not set — skipping agent client test")
	}

	dir := t.TempDir()
	controlSock = filepath.Join(dir, "control.sock")
	forwardSock = filepath.Join(dir, "forward.sock")

	cmd := exec.Command(agentBin)
	cmd.Env = append(os.Environ(),
		"BHATTI_AGENT_TEST=1",
		"BHATTI_AGENT_SOCK="+controlSock,
		"BHATTI_AGENT_FWD_SOCK="+forwardSock,
	)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start agent: %v", err)
	}

	for i := 0; i < 100; i++ {
		if _, err := os.Stat(controlSock); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	return controlSock, forwardSock, func() {
		cmd.Process.Kill()
		cmd.Wait()
	}
}

// --- Exec tests ---

func TestClientExec(t *testing.T) {
	ctrl, fwd, cleanup := startTestAgent(t)
	defer cleanup()

	client := NewTestClient(ctrl, fwd)
	result, err := client.Exec(context.Background(), []string{"echo", "hello"}, nil, "")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit code: %d", result.ExitCode)
	}
	if result.Stdout != "hello\n" {
		t.Errorf("stdout: %q, want %q", result.Stdout, "hello\n")
	}
	if result.Stderr != "" {
		t.Errorf("stderr: %q, want empty", result.Stderr)
	}
}

func TestClientExecWithEnv(t *testing.T) {
	ctrl, fwd, cleanup := startTestAgent(t)
	defer cleanup()

	client := NewTestClient(ctrl, fwd)
	result, err := client.Exec(context.Background(),
		[]string{"sh", "-c", "echo $MY_VAR"},
		map[string]string{"MY_VAR": "works"},
		"",
	)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit code: %d", result.ExitCode)
	}
	if strings.TrimSpace(result.Stdout) != "works" {
		t.Errorf("stdout: %q, want %q", result.Stdout, "works\n")
	}
}

func TestClientExecStderr(t *testing.T) {
	ctrl, fwd, cleanup := startTestAgent(t)
	defer cleanup()

	client := NewTestClient(ctrl, fwd)
	result, err := client.Exec(context.Background(),
		[]string{"sh", "-c", "echo out; echo err >&2"},
		nil, "",
	)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit code: %d", result.ExitCode)
	}
	if result.Stdout != "out\n" {
		t.Errorf("stdout: %q", result.Stdout)
	}
	if result.Stderr != "err\n" {
		t.Errorf("stderr: %q", result.Stderr)
	}
}

func TestClientExecCwd(t *testing.T) {
	ctrl, fwd, cleanup := startTestAgent(t)
	defer cleanup()

	client := NewTestClient(ctrl, fwd)
	result, err := client.Exec(context.Background(), []string{"pwd"}, nil, "/tmp")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.TrimSpace(result.Stdout) != "/tmp" {
		t.Errorf("stdout: %q, want /tmp", result.Stdout)
	}
}

func TestClientExecFailure(t *testing.T) {
	ctrl, fwd, cleanup := startTestAgent(t)
	defer cleanup()

	client := NewTestClient(ctrl, fwd)
	result, err := client.Exec(context.Background(), []string{"false"}, nil, "")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 1 {
		t.Errorf("exit code: %d, want 1", result.ExitCode)
	}
}

func TestClientExecNotFound(t *testing.T) {
	ctrl, fwd, cleanup := startTestAgent(t)
	defer cleanup()

	client := NewTestClient(ctrl, fwd)
	_, err := client.Exec(context.Background(), []string{"/nonexistent"}, nil, "")
	if err == nil {
		t.Fatal("expected error for non-existent command")
	}
	if !strings.Contains(err.Error(), "agent error") {
		t.Errorf("error: %v, want agent error", err)
	}
}

// --- Shell tests ---

func TestClientShell(t *testing.T) {
	ctrl, fwd, cleanup := startTestAgent(t)
	defer cleanup()

	client := NewTestClient(ctrl, fwd)
	term, err := client.Shell(context.Background(), []string{"/bin/sh"}, nil, 24, 80)
	if err != nil {
		t.Fatalf("Shell: %v", err)
	}
	defer term.Close()

	// Read in a goroutine to avoid SetReadDeadline corrupting frame boundaries.
	type readResult struct {
		data []byte
		err  error
	}
	readCh := make(chan readResult, 64)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := term.Read(buf)
			if n > 0 {
				cp := make([]byte, n)
				copy(cp, buf[:n])
				readCh <- readResult{data: cp}
			}
			if err != nil {
				readCh <- readResult{err: err}
				return
			}
		}
	}()

	// Give shell time to start.
	time.Sleep(200 * time.Millisecond)

	// Write a command.
	if _, err := term.Write([]byte("echo ok\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Wait for "ok" in output.
	var output bytes.Buffer
	timer := time.After(5 * time.Second)
	found := false
	for !found {
		select {
		case r := <-readCh:
			if r.err != nil {
				t.Fatalf("Read error: %v, output: %q", r.err, output.String())
			}
			output.Write(r.data)
			if strings.Contains(output.String(), "ok") {
				found = true
			}
		case <-timer:
			t.Fatalf("timeout waiting for 'ok', output: %q", output.String())
		}
	}

	// Test Resize.
	if err := term.Resize(40, 120); err != nil {
		t.Errorf("Resize: %v", err)
	}

	// Exit.
	term.Write([]byte("exit\n"))

	// Drain until EOF.
	drainTimer := time.After(5 * time.Second)
	for {
		select {
		case r := <-readCh:
			if r.err != nil {
				return // EOF — done
			}
		case <-drainTimer:
			return
		}
	}
}

// --- Forward tests ---

func TestClientForward(t *testing.T) {
	ctrl, fwd, cleanup := startTestAgent(t)
	defer cleanup()

	// Start a TCP echo server.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		io.Copy(conn, conn)
	}()

	client := NewTestClient(ctrl, fwd)
	tunnel, err := client.Forward(context.Background(), uint16(port))
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	defer tunnel.Close()

	// Write through tunnel, read echo back.
	tunnel.Write([]byte("ping"))
	buf := make([]byte, 4)
	tunnel.(net.Conn).SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(tunnel, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != "ping" {
		t.Errorf("echo: %q, want %q", buf, "ping")
	}
}

func TestClientForwardRefused(t *testing.T) {
	ctrl, fwd, cleanup := startTestAgent(t)
	defer cleanup()

	client := NewTestClient(ctrl, fwd)
	_, err := client.Forward(context.Background(), 59999)
	if err == nil {
		t.Fatal("expected error for refused port")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Errorf("error: %v, want 'refused'", err)
	}
}

// --- WaitReady tests ---

func TestClientWaitReady(t *testing.T) {
	ctrl, fwd, cleanup := startTestAgent(t)
	defer cleanup()

	client := NewTestClient(ctrl, fwd)
	err := client.WaitReady(context.Background(), 5*time.Second)
	if err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
}

func TestClientWaitReadyTimeout(t *testing.T) {
	// Point at a socket that doesn't exist — should timeout.
	client := NewTestClient("/tmp/nonexistent-bhatti-ctrl.sock", "/tmp/nonexistent-bhatti-fwd.sock")
	err := client.WaitReady(context.Background(), 200*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}
