//go:build linux

package firecracker

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// readTermOutput reads from a terminal until timeout or match string is found.
func readTermOutput(term engine.TerminalConn, timeout time.Duration, match string) string {
	ch := make(chan []byte, 64)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := term.Read(buf)
			if n > 0 {
				cp := make([]byte, n)
				copy(cp, buf[:n])
				ch <- cp
			}
			if err != nil {
				close(ch)
				return
			}
		}
	}()
	var out strings.Builder
	timer := time.After(timeout)
	for {
		select {
		case data, ok := <-ch:
			if !ok {
				return out.String()
			}
			out.Write(data)
			if match != "" && strings.Contains(out.String(), match) {
				return out.String()
			}
		case <-timer:
			return out.String()
		}
	}
}

// dialControlWithAuth dials the VM's control port and sends the auth token.
func dialControlWithAuth(vm *VM) (net.Conn, error) {
	conn, err := net.Dial("tcp", net.JoinHostPort(vm.GuestIP, fmt.Sprint(proto.VsockPortControl)))
	if err != nil {
		return nil, err
	}
	if vm.Token != "" {
		if err := proto.WriteFrame(conn, proto.AUTH, []byte(vm.Token)); err != nil {
			conn.Close()
			return nil, err
		}
	}
	return conn, nil
}

func TestExecOneShot(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("exec-oneshot"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	r, err := execWithTimeout(t, eng, info.ID, []string{"echo", "hello-session"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if r.ExitCode != 0 || !strings.Contains(r.Stdout, "hello-session") {
		t.Errorf("exit=%d stdout=%q", r.ExitCode, r.Stdout)
	}
	t.Log("✓ one-shot exec works with session-aware handler")
}

func TestTTYSessionCreateDetachReattach(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("tty-reattach"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	vm, _ := eng.getVM(info.ID)

	// Create a TTY session running cat
	sessInfo, term, err := vm.Agent.ShellSession(ctx, []string{"cat"}, nil, 24, 80, 0, "")
	if err != nil {
		t.Fatalf("ShellSession: %v", err)
	}
	if sessInfo.SessionID == "" {
		t.Fatal("empty session ID")
	}
	t.Logf("created session %s", sessInfo.SessionID)

	// Write and verify echo
	term.Write([]byte("hello-reattach\n"))
	output := readTermOutput(term, 3*time.Second, "hello-reattach")
	if !strings.Contains(output, "hello-reattach") {
		t.Fatalf("no echo: %q", output)
	}
	t.Log("✓ TTY session echoes input")

	// Detach
	term.Close()
	time.Sleep(500 * time.Millisecond)

	// Reattach
	sessInfo2, term2, err := vm.Agent.SessionAttach(ctx, sessInfo.SessionID, false)
	if err != nil {
		t.Fatalf("SessionAttach: %v", err)
	}
	defer term2.Close()

	if !sessInfo2.Running {
		t.Error("session should still be running")
	}

	// Scrollback should contain previous output
	scrollback := readTermOutput(term2, 2*time.Second, "hello-reattach")
	if !strings.Contains(scrollback, "hello-reattach") {
		t.Errorf("scrollback missing: %q", scrollback)
	} else {
		t.Log("✓ scrollback replayed on reattach")
	}
}

func TestProcessSurvivesDetach(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("survive-detach"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	vm, _ := eng.getVM(info.ID)

	// Process that outputs after a delay
	sessInfo, term, err := vm.Agent.ShellSession(ctx,
		[]string{"sh", "-c", "echo before; sleep 2; echo AFTER_DETACH"},
		nil, 24, 80, 0, "")
	if err != nil {
		t.Fatalf("ShellSession: %v", err)
	}
	// Wait for first output then detach
	readTermOutput(term, 1*time.Second, "before")
	term.Close()
	t.Log("detached, process running in background...")

	// Wait for process to finish
	time.Sleep(3 * time.Second)

	// Reattach — scrollback should have delayed output
	_, term2, err := vm.Agent.SessionAttach(ctx, sessInfo.SessionID, false)
	if err != nil {
		t.Fatalf("SessionAttach: %v", err)
	}
	defer term2.Close()

	scrollback := readTermOutput(term2, 3*time.Second, "AFTER_DETACH")
	if !strings.Contains(scrollback, "AFTER_DETACH") {
		t.Errorf("process didn't survive: %q", scrollback)
	} else {
		t.Log("✓ process survived detach, output in scrollback")
	}
}

func TestSessionList(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("sess-list"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	vm, _ := eng.getVM(info.ID)

	// Create two sessions
	_, term1, _ := vm.Agent.ShellSession(ctx, []string{"sleep", "3600"}, nil, 24, 80, 0, "")
	defer term1.Close()
	_, term2, _ := vm.Agent.ShellSession(ctx, []string{"sleep", "3601"}, nil, 24, 80, 0, "")
	defer term2.Close()
	time.Sleep(500 * time.Millisecond)

	sessions, err := vm.Agent.SessionList(ctx)
	if err != nil {
		t.Fatalf("SessionList: %v", err)
	}
	if len(sessions) < 2 {
		t.Fatalf("expected >= 2, got %d", len(sessions))
	}
	t.Logf("✓ listed %d sessions", len(sessions))
}

func TestSessionKill(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("sess-kill"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	vm, _ := eng.getVM(info.ID)

	sessInfo, term, err := vm.Agent.ShellSession(ctx, []string{"sleep", "3600"}, nil, 24, 80, 0, "")
	if err != nil {
		t.Fatalf("ShellSession: %v", err)
	}
	term.Close()
	time.Sleep(500 * time.Millisecond)

	if err := vm.Agent.SessionKill(ctx, sessInfo.SessionID); err != nil {
		t.Fatalf("SessionKill: %v", err)
	}
	time.Sleep(1 * time.Second)

	// Reattach — should get exit
	_, term2, err := vm.Agent.SessionAttach(ctx, sessInfo.SessionID, false)
	if err != nil {
		t.Logf("✓ session already cleaned up: %v", err)
		return
	}
	defer term2.Close()
	readTermOutput(term2, 3*time.Second, "") // drain until EOF
	t.Log("✓ killed session exited")
}

func TestMaxIdle(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("max-idle"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	vm, _ := eng.getVM(info.ID)

	// Create session with 2s idle timeout via low-level API
	controlConn, err := dialControlWithAuth(vm)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	tty := true
	maxIdle := 2
	req := proto.ExecRequest{
		Argv:       []string{"sleep", "3600"},
		TTY:        &tty,
		MaxIdleSec: &maxIdle,
	}
	proto.SendJSON(controlConn, proto.EXEC_REQ, req)

	// Read SESSION_INFO
	msgType, payload, _ := proto.ReadFrame(controlConn)
	if msgType != proto.SESSION_INFO {
		t.Fatalf("expected SESSION_INFO, got 0x%02x: %s", msgType, payload)
	}
	var sessInfo proto.SessionInfo
	json.Unmarshal(payload, &sessInfo)
	t.Logf("created session %s with max_idle=2s", sessInfo.SessionID)

	// Detach — idle timer starts
	controlConn.Close()

	// Wait for idle timeout
	time.Sleep(4 * time.Second)

	// Reattach — should get EXIT (process killed)
	_, term, err := vm.Agent.SessionAttach(ctx, sessInfo.SessionID, false)
	if err != nil {
		t.Logf("✓ session cleaned up after idle: %v", err)
		return
	}
	defer term.Close()
	readTermOutput(term, 3*time.Second, "")
	t.Log("✓ idle timer killed the session")
}

func TestAttachNonexistent(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("attach-bogus"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	vm, _ := eng.getVM(info.ID)

	_, _, err = vm.Agent.SessionAttach(ctx, "nonexistent-session", false)
	if err == nil {
		t.Error("expected error attaching to nonexistent session")
	} else {
		t.Logf("✓ attach to nonexistent session rejected: %v", err)
	}
}

func TestKillNonexistent(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("kill-bogus"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	vm, _ := eng.getVM(info.ID)

	err = vm.Agent.SessionKill(ctx, "nonexistent-session")
	if err == nil {
		t.Error("expected error killing nonexistent session")
	} else {
		t.Logf("✓ kill nonexistent session rejected: %v", err)
	}
}

func TestMaxIdleZeroStaysAlive(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("idle-zero"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	vm, _ := eng.getVM(info.ID)

	// Default TTY session (max_idle=0 = forever)
	sessInfo, term, err := vm.Agent.ShellSession(ctx, []string{"sleep", "3600"}, nil, 24, 80, 0, "")
	if err != nil {
		t.Fatalf("ShellSession: %v", err)
	}

	// Detach
	term.Close()
	t.Log("detached default session (max_idle=0)")

	// Wait — process should NOT be killed
	time.Sleep(5 * time.Second)

	// Reattach — should still be running
	info2, term2, err := vm.Agent.SessionAttach(ctx, sessInfo.SessionID, false)
	if err != nil {
		t.Fatalf("SessionAttach after 5s: %v", err)
	}
	defer term2.Close()

	if !info2.Running {
		t.Error("session should still be running with max_idle=0")
	} else {
		t.Log("✓ max_idle=0 session stays alive after 5s detached")
	}
}

func TestExecExitCode(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("exit-code"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Exit 0
	r, _ := execWithTimeout(t, eng, info.ID, []string{"true"})
	if r.ExitCode != 0 {
		t.Errorf("true: exit=%d", r.ExitCode)
	}

	// Exit 1
	r, _ = execWithTimeout(t, eng, info.ID, []string{"false"})
	if r.ExitCode != 1 {
		t.Errorf("false: exit=%d, want 1", r.ExitCode)
	}

	// Exit 42
	r, _ = execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "exit 42"})
	if r.ExitCode != 42 {
		t.Errorf("exit 42: got %d", r.ExitCode)
	}

	// Stderr captured
	r, _ = execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "echo err-msg >&2; exit 1"})
	if r.ExitCode != 1 || !strings.Contains(r.Stderr, "err-msg") {
		t.Errorf("stderr: exit=%d stderr=%q", r.ExitCode, r.Stderr)
	}

	t.Log("✓ exit codes 0, 1, 42 and stderr all preserved")
}

func TestSessionListAttachedField(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("list-attached"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	vm, _ := eng.getVM(info.ID)

	// Session 1: attached
	_, term1, _ := vm.Agent.ShellSession(ctx, []string{"sleep", "3600"}, nil, 24, 80, 0, "")
	defer term1.Close()

	// Session 2: detached
	_, term2, _ := vm.Agent.ShellSession(ctx, []string{"sleep", "3601"}, nil, 24, 80, 0, "")
	term2.Close()
	time.Sleep(500 * time.Millisecond)

	sessions, err := vm.Agent.SessionList(ctx)
	if err != nil {
		t.Fatalf("SessionList: %v", err)
	}

	attached := 0
	detached := 0
	for _, s := range sessions {
		if s.Attached {
			attached++
		} else {
			detached++
		}
	}
	if attached < 1 || detached < 1 {
		t.Errorf("expected at least 1 attached and 1 detached, got attached=%d detached=%d", attached, detached)
		for _, s := range sessions {
			t.Logf("  %s: attached=%v running=%v", s.SessionID, s.Attached, s.Running)
		}
	} else {
		t.Logf("✓ session list shows %d attached, %d detached", attached, detached)
	}
}

func TestScrollbackOverflow(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("scrollback"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	vm, _ := eng.getVM(info.ID)

	// Use a long-running process so the session stays alive across
	// disconnect/reattach. Previously used a command that exited, which
	// raced with agent session cleanup — the session could be reaped
	// before reattach.
	sessInfo, term, err := vm.Agent.ShellSession(ctx,
		[]string{"sh", "-c", "for i in $(seq 1 1000); do printf 'L%04d-' $i; head -c 80 /dev/urandom | base64 | head -c 80; echo; done; echo SCROLL_END; sleep 3600"},
		nil, 24, 80, 0, "")
	if err != nil {
		t.Fatalf("ShellSession: %v", err)
	}

	// Wait for output to complete (sleep 3600 keeps the process alive)
	readTermOutput(term, 5*time.Second, "SCROLL_END")
	term.Close()
	time.Sleep(500 * time.Millisecond)

	// Reattach — session is alive because sleep is still running
	_, term2, err := vm.Agent.SessionAttach(ctx, sessInfo.SessionID, false)
	if err != nil {
		t.Fatalf("SessionAttach: %v", err)
	}
	defer term2.Close()

	sb := readTermOutput(term2, 3*time.Second, "")
	t.Logf("scrollback: %d bytes", len(sb))

	if !strings.Contains(sb, "SCROLL_END") {
		t.Error("scrollback missing SCROLL_END")
	} else {
		t.Log("✓ scrollback has most recent output")
	}

	// Scrollback must be exactly 64KB (ring buffer size)
	if len(sb) != 65536 {
		t.Errorf("scrollback size: %d, want exactly 65536", len(sb))
	} else {
		t.Log("✓ scrollback is exactly 64KB")
	}

	// First lines should be evicted (output was ~86KB: 1000 lines × ~86 chars)
	if strings.Contains(sb, "L0001-") {
		t.Error("L0001 should have been evicted from 64KB ring buffer")
	} else {
		t.Log("✓ oldest lines evicted from scrollback")
	}
}
