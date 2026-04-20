//go:build linux

package firecracker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
)

func TestPipedSessionBasic(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("piped-basic"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	vm, _ := eng.getVM(info.ID)

	sessInfo, pc, err := vm.Agent.PipedSession(ctx, []string{"cat"}, nil, 0)
	if err != nil {
		t.Fatalf("PipedSession: %v", err)
	}

	if sessInfo.SessionID == "" {
		t.Fatal("empty session ID")
	}
	if sessInfo.TTY {
		t.Error("expected TTY=false")
	}
	t.Logf("created piped session %s", sessInfo.SessionID)

	// Write stdin, read stdout echo
	pc.WriteStdin([]byte("hello-piped\n"))

	deadline := time.After(5 * time.Second)
	var buf strings.Builder
	for {
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for echo, got: %q", buf.String())
		default:
		}
		msgType, payload, err := pc.ReadFrame()
		if err != nil {
			t.Fatalf("ReadFrame: %v", err)
		}
		if msgType == proto.STDOUT {
			buf.Write(payload)
			if strings.Contains(buf.String(), "hello-piped") {
				break
			}
		}
	}
	t.Log("✓ piped session echoes stdin → stdout")

	// Kill
	pc.Kill()
	for {
		msgType, _, err := pc.ReadFrame()
		if err != nil || msgType == proto.EXIT {
			break
		}
	}
	t.Log("✓ piped session killed cleanly")
}

func TestPipedSessionDetachReattach(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("piped-reattach"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	vm, _ := eng.getVM(info.ID)

	// Create piped session with a read-echo loop
	sessInfo, pc, err := vm.Agent.PipedSession(ctx,
		[]string{"sh", "-c", "while read line; do echo got:$line; done"}, nil, 0)
	if err != nil {
		t.Fatalf("PipedSession: %v", err)
	}

	// Send data, verify echo
	pc.WriteStdin([]byte("before\n"))
	deadline := time.After(5 * time.Second)
	var buf strings.Builder
	for {
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for echo, got: %q", buf.String())
		default:
		}
		msgType, payload, _ := pc.ReadFrame()
		if msgType == proto.STDOUT {
			buf.Write(payload)
			if strings.Contains(buf.String(), "got:before") {
				break
			}
		}
	}
	t.Log("✓ initial echo works")

	// Disconnect
	pc.Close()
	time.Sleep(500 * time.Millisecond)

	// Reattach
	sessInfo2, pc2, err := vm.Agent.PipedSessionAttach(ctx, sessInfo.SessionID, false)
	if err != nil {
		t.Fatalf("PipedSessionAttach: %v", err)
	}
	defer pc2.Close()

	if !sessInfo2.Running {
		t.Error("session should still be running")
	}

	// Scrollback should contain "got:before"
	buf.Reset()
	scrollDeadline := time.After(3 * time.Second)
	gotScrollback := false
scrollLoop:
	for {
		select {
		case <-scrollDeadline:
			break scrollLoop
		default:
		}
		msgType, payload, err := pc2.ReadFrame()
		if err != nil {
			break
		}
		if msgType == proto.STDOUT {
			buf.Write(payload)
			if strings.Contains(buf.String(), "got:before") {
				gotScrollback = true
				break
			}
		}
	}
	if !gotScrollback {
		t.Errorf("scrollback missing 'got:before': %q", buf.String())
	} else {
		t.Log("✓ scrollback replayed on reattach")
	}

	// Send new data post-reattach
	pc2.WriteStdin([]byte("after\n"))
	buf.Reset()
	afterDeadline := time.After(5 * time.Second)
	for {
		select {
		case <-afterDeadline:
			t.Fatalf("timeout waiting for post-reattach echo, got: %q", buf.String())
		default:
		}
		msgType, payload, _ := pc2.ReadFrame()
		if msgType == proto.STDOUT {
			buf.Write(payload)
			if strings.Contains(buf.String(), "got:after") {
				break
			}
		}
	}
	t.Log("✓ stdin works after reattach")
}

func TestPipedSessionStdinHeldOpen(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("piped-stdin-held"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	vm, _ := eng.getVM(info.ID)

	// Shell that reads two lines sequentially — if stdin closes
	// on disconnect, the second read fails and the shell exits.
	sessInfo, pc, err := vm.Agent.PipedSession(ctx,
		[]string{"sh", "-c", "read line && echo first:$line && read line && echo second:$line"}, nil, 0)
	if err != nil {
		t.Fatalf("PipedSession: %v", err)
	}

	// Send first line
	pc.WriteStdin([]byte("alpha\n"))
	deadline := time.After(5 * time.Second)
	var buf strings.Builder
	for {
		select {
		case <-deadline:
			t.Fatalf("timeout, got: %q", buf.String())
		default:
		}
		msgType, payload, _ := pc.ReadFrame()
		if msgType == proto.STDOUT {
			buf.Write(payload)
			if strings.Contains(buf.String(), "first:alpha") {
				break
			}
		}
	}
	t.Log("✓ first read works")

	// Disconnect
	pc.Close()
	time.Sleep(500 * time.Millisecond)

	// Reattach
	_, pc2, err := vm.Agent.PipedSessionAttach(ctx, sessInfo.SessionID, false)
	if err != nil {
		t.Fatalf("PipedSessionAttach: %v", err)
	}
	defer pc2.Close()

	// Drain scrollback first
	time.Sleep(200 * time.Millisecond)

	// Send second line — if stdin pipe closed, shell already exited
	pc2.WriteStdin([]byte("beta\n"))
	buf.Reset()
	deadline2 := time.After(5 * time.Second)
	for {
		select {
		case <-deadline2:
			t.Fatalf("stdin pipe closed on disconnect! got: %q", buf.String())
		default:
		}
		msgType, payload, err := pc2.ReadFrame()
		if err != nil {
			t.Fatalf("stdin pipe closed on disconnect (read error): %v, got: %q", err, buf.String())
		}
		if msgType == proto.STDOUT {
			buf.Write(payload)
			if strings.Contains(buf.String(), "second:beta") {
				break
			}
		}
		if msgType == proto.EXIT {
			t.Fatalf("process exited — stdin pipe was closed on disconnect! got: %q", buf.String())
		}
	}
	t.Log("✓ stdin pipe held open across disconnect — second read works")
}

func TestPipedSessionProcessExit(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("piped-exit"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	vm, _ := eng.getVM(info.ID)

	_, pc, err := vm.Agent.PipedSession(ctx,
		[]string{"sh", "-c", "echo done; exit 42"}, nil, 0)
	if err != nil {
		t.Fatalf("PipedSession: %v", err)
	}
	defer pc.Close()

	gotExit := false
	var exitCode int32
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for EXIT")
		default:
		}
		msgType, payload, err := pc.ReadFrame()
		if err != nil {
			break
		}
		if msgType == proto.EXIT {
			exitCode, _ = proto.ParseExitCode(payload)
			gotExit = true
			break
		}
	}
	if !gotExit {
		t.Fatal("no EXIT frame")
	}
	if exitCode != 42 {
		t.Errorf("exit code: %d, want 42", exitCode)
	}
	t.Log("✓ piped session delivers EXIT with correct code")
}

func TestPipedSessionListSessions(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("piped-list"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	vm, _ := eng.getVM(info.ID)

	_, pc, err := vm.Agent.PipedSession(ctx, []string{"sleep", "60"}, nil, 0)
	if err != nil {
		t.Fatalf("PipedSession: %v", err)
	}
	defer pc.Close()

	time.Sleep(500 * time.Millisecond)

	sessions, err := vm.Agent.SessionList(ctx)
	if err != nil {
		t.Fatalf("SessionList: %v", err)
	}

	found := false
	for _, s := range sessions {
		if strings.Contains(s.Argv, "sleep 60") {
			found = true
			if s.TTY {
				t.Error("piped session should have TTY=false")
			}
			if !s.Running {
				t.Error("session should be Running")
			}
		}
	}
	if !found {
		t.Error("piped session not in session list")
	}
	t.Log("✓ piped session appears in session list with TTY=false")
}

func TestPipedSessionIdleTimeout(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("piped-idle"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	vm, _ := eng.getVM(info.ID)

	// Use low-level API to set MaxIdleSec=2
	conn, err := dialControlWithAuth(vm)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	session := true
	maxIdle := 2
	req := proto.ExecRequest{
		Argv:       []string{"sleep", "3600"},
		Session:    &session,
		MaxIdleSec: &maxIdle,
	}
	proto.SendJSON(conn, proto.EXEC_REQ, req)

	msgType, payload, _ := proto.ReadFrame(conn)
	if msgType != proto.SESSION_INFO {
		t.Fatalf("expected SESSION_INFO, got 0x%02x: %s", msgType, payload)
	}
	var sessInfo proto.SessionInfo
	json.Unmarshal(payload, &sessInfo)
	t.Logf("created piped session %s with max_idle=2s", sessInfo.SessionID)

	// Disconnect — idle timer starts
	conn.Close()

	// Wait for idle timeout + buffer
	time.Sleep(4 * time.Second)

	// Session should be gone
	sessions, _ := vm.Agent.SessionList(ctx)
	for _, s := range sessions {
		if s.SessionID == sessInfo.SessionID {
			t.Error("session should have been killed by idle timeout")
		}
	}
	t.Log("✓ piped session killed after idle timeout")
}

func TestPipedSessionMergedStderr(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("piped-stderr"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	vm, _ := eng.getVM(info.ID)

	_, pc, err := vm.Agent.PipedSession(ctx,
		[]string{"sh", "-c", "echo stdout_msg; echo stderr_msg >&2"}, nil, 0)
	if err != nil {
		t.Fatalf("PipedSession: %v", err)
	}
	defer pc.Close()

	var stdout strings.Builder
	gotStderr := false
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			goto done
		default:
		}
		msgType, payload, err := pc.ReadFrame()
		if err != nil {
			break
		}
		if msgType == proto.STDOUT {
			stdout.Write(payload)
		}
		if msgType == proto.STDERR {
			gotStderr = true
		}
		if msgType == proto.EXIT {
			break
		}
	}
done:
	out := stdout.String()
	if !strings.Contains(out, "stdout_msg") {
		t.Errorf("missing stdout_msg: %q", out)
	}
	if !strings.Contains(out, "stderr_msg") {
		t.Errorf("missing stderr_msg in STDOUT: %q", out)
	}
	if gotStderr {
		t.Error("piped session should merge stderr into STDOUT, not send STDERR frames")
	}
	t.Log("✓ stderr merged into STDOUT frames")
}

func TestTTYSessionUnaffected(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("tty-unaffected"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	vm, _ := eng.getVM(info.ID)

	// Create TTY session — should still work after piped session code was added
	sessInfo, term, err := vm.Agent.ShellSession(ctx, []string{"cat"}, nil, 24, 80, 0, "")
	if err != nil {
		t.Fatalf("ShellSession: %v", err)
	}
	defer term.Close()

	if !sessInfo.TTY {
		t.Error("expected TTY=true")
	}

	term.Write([]byte("tty_ok\n"))
	output := readTermOutput(term, 3*time.Second, "tty_ok")
	if !strings.Contains(output, "tty_ok") {
		t.Fatalf("TTY echo failed: %q", output)
	}
	t.Log("✓ TTY sessions still work after piped session changes")
}
