//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sahilshubham/bhatti/pkg/agent/proto"
)

// startTestAgent starts the agent as a subprocess in test mode over Unix sockets.
// Returns the socket paths and a cleanup function.
func startTestAgent(t *testing.T) (controlSock, forwardSock string, cleanup func()) {
	t.Helper()
	dir := t.TempDir()
	controlSock = filepath.Join(dir, "control.sock")
	forwardSock = filepath.Join(dir, "forward.sock")

	cmd := exec.Command(os.Args[0], "-test.run=TestHelperAgent")
	cmd.Env = append(os.Environ(),
		"LOHAR_TEST=1",
		"LOHAR_SOCK="+controlSock,
		"LOHAR_FWD_SOCK="+forwardSock,
		"GO_WANT_HELPER_PROCESS=1",
	)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start agent: %v", err)
	}

	// Wait for socket to exist.
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

// TestHelperAgent is the subprocess entry point — not a real test.
func TestHelperAgent(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	runTestMode()
}

// dialControl opens a new connection to the control socket.
func dialControl(t *testing.T, sock string) net.Conn {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial control: %v", err)
	}
	return conn
}

// sendExec sends an EXEC_REQ and returns the connection for reading frames.
func sendExec(t *testing.T, conn net.Conn, req proto.ExecRequest) {
	t.Helper()
	if err := proto.SendJSON(conn, proto.EXEC_REQ, req); err != nil {
		t.Fatalf("send exec: %v", err)
	}
}

// readAllExec reads all frames until EXIT, collecting stdout/stderr.
func readAllExec(t *testing.T, conn net.Conn) (stdout, stderr string, exitCode int32) {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	for {
		msgType, payload, err := proto.ReadFrame(conn)
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		switch msgType {
		case proto.STDOUT:
			outBuf.Write(payload)
		case proto.STDERR:
			errBuf.Write(payload)
		case proto.EXIT:
			code, ok := proto.ParseExitCode(payload)
			if !ok {
				t.Fatal("bad exit payload")
			}
			return outBuf.String(), errBuf.String(), code
		case proto.ERROR:
			t.Fatalf("agent error: %s", payload)
		}
	}
}

// --- Non-TTY exec tests ---

func TestAgentExec(t *testing.T) {
	ctrl, _, cleanup := startTestAgent(t)
	defer cleanup()

	conn := dialControl(t, ctrl)
	defer conn.Close()

	sendExec(t, conn, proto.ExecRequest{Argv: []string{"echo", "hello"}})
	stdout, stderr, code := readAllExec(t, conn)

	if code != 0 {
		t.Errorf("exit code: %d", code)
	}
	if stdout != "hello\n" {
		t.Errorf("stdout: %q, want %q", stdout, "hello\n")
	}
	if stderr != "" {
		t.Errorf("stderr: %q, want empty", stderr)
	}
}

func TestAgentExecFailure(t *testing.T) {
	ctrl, _, cleanup := startTestAgent(t)
	defer cleanup()

	conn := dialControl(t, ctrl)
	defer conn.Close()

	sendExec(t, conn, proto.ExecRequest{Argv: []string{"false"}})
	_, _, code := readAllExec(t, conn)

	if code != 1 {
		t.Errorf("exit code: %d, want 1", code)
	}
}

func TestAgentExecNotFound(t *testing.T) {
	ctrl, _, cleanup := startTestAgent(t)
	defer cleanup()

	conn := dialControl(t, ctrl)
	defer conn.Close()

	sendExec(t, conn, proto.ExecRequest{Argv: []string{"/nonexistent"}})

	// Agent should send ERROR frame (can't spawn process).
	msgType, payload, err := proto.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if msgType != proto.ERROR {
		t.Fatalf("expected ERROR frame, got 0x%02x: %s", msgType, payload)
	}
}

func TestAgentExecStderr(t *testing.T) {
	ctrl, _, cleanup := startTestAgent(t)
	defer cleanup()

	conn := dialControl(t, ctrl)
	defer conn.Close()

	sendExec(t, conn, proto.ExecRequest{Argv: []string{"sh", "-c", "echo err >&2"}})
	stdout, stderr, code := readAllExec(t, conn)

	if code != 0 {
		t.Errorf("exit code: %d", code)
	}
	if stdout != "" {
		t.Errorf("stdout: %q, want empty", stdout)
	}
	if stderr != "err\n" {
		t.Errorf("stderr: %q, want %q", stderr, "err\n")
	}
}

func TestAgentExecEnv(t *testing.T) {
	ctrl, _, cleanup := startTestAgent(t)
	defer cleanup()

	conn := dialControl(t, ctrl)
	defer conn.Close()

	sendExec(t, conn, proto.ExecRequest{
		Argv: []string{"sh", "-c", "echo $FOO"},
		Env:  map[string]string{"FOO": "bar"},
	})
	stdout, _, code := readAllExec(t, conn)

	if code != 0 {
		t.Errorf("exit code: %d", code)
	}
	if stdout != "bar\n" {
		t.Errorf("stdout: %q, want %q", stdout, "bar\n")
	}
}

func TestAgentExecCwd(t *testing.T) {
	ctrl, _, cleanup := startTestAgent(t)
	defer cleanup()

	conn := dialControl(t, ctrl)
	defer conn.Close()

	cwd := "/tmp"
	sendExec(t, conn, proto.ExecRequest{Argv: []string{"pwd"}, Cwd: &cwd})
	stdout, _, code := readAllExec(t, conn)

	if code != 0 {
		t.Errorf("exit code: %d", code)
	}
	if strings.TrimSpace(stdout) != "/tmp" {
		t.Errorf("stdout: %q, want /tmp", stdout)
	}
}

func TestAgentExecLargeOutput(t *testing.T) {
	ctrl, _, cleanup := startTestAgent(t)
	defer cleanup()

	conn := dialControl(t, ctrl)
	defer conn.Close()

	// Generate 1MB of output.
	sendExec(t, conn, proto.ExecRequest{
		Argv: []string{"dd", "if=/dev/zero", "bs=1024", "count=1024", "status=none"},
	})
	stdout, _, code := readAllExec(t, conn)

	if code != 0 {
		t.Errorf("exit code: %d", code)
	}
	if len(stdout) != 1024*1024 {
		t.Errorf("stdout len: %d, want %d", len(stdout), 1024*1024)
	}
}

func TestAgentKill(t *testing.T) {
	ctrl, _, cleanup := startTestAgent(t)
	defer cleanup()

	conn := dialControl(t, ctrl)
	defer conn.Close()

	sendExec(t, conn, proto.ExecRequest{Argv: []string{"sleep", "60"}})
	time.Sleep(100 * time.Millisecond)

	// Send KILL frame.
	if err := proto.WriteFrame(conn, proto.KILL, nil); err != nil {
		t.Fatalf("write KILL: %v", err)
	}

	_, _, code := readAllExec(t, conn)
	// SIGTERM = signal 15, exit code = 128 + 15 = 143
	if code != 143 {
		t.Errorf("exit code: %d, want 143 (128+SIGTERM)", code)
	}
}

// --- Stdin piping ---

func TestAgentExecStdin(t *testing.T) {
	ctrl, _, cleanup := startTestAgent(t)
	defer cleanup()

	conn := dialControl(t, ctrl)
	defer conn.Close()

	// Use head -n2 instead of cat, so it exits after reading 2 lines
	// without needing to close the connection.
	sendExec(t, conn, proto.ExecRequest{Argv: []string{"head", "-n2"}})

	proto.WriteFrame(conn, proto.STDIN, []byte("line1\n"))
	proto.WriteFrame(conn, proto.STDIN, []byte("line2\n"))

	stdout, _, code := readAllExec(t, conn)
	if code != 0 {
		t.Errorf("exit code: %d", code)
	}
	if stdout != "line1\nline2\n" {
		t.Errorf("stdout: %q, want %q", stdout, "line1\nline2\n")
	}
}

// --- Mixed stdout + stderr ---

func TestAgentExecMixedOutput(t *testing.T) {
	ctrl, _, cleanup := startTestAgent(t)
	defer cleanup()

	conn := dialControl(t, ctrl)
	defer conn.Close()

	sendExec(t, conn, proto.ExecRequest{
		Argv: []string{"sh", "-c", "echo out1; echo err1 >&2; echo out2; echo err2 >&2"},
	})
	stdout, stderr, code := readAllExec(t, conn)

	if code != 0 {
		t.Errorf("exit code: %d", code)
	}
	if !strings.Contains(stdout, "out1") || !strings.Contains(stdout, "out2") {
		t.Errorf("stdout: %q, want out1 and out2", stdout)
	}
	if !strings.Contains(stderr, "err1") || !strings.Contains(stderr, "err2") {
		t.Errorf("stderr: %q, want err1 and err2", stderr)
	}
}

// --- Default environment ---

func TestAgentExecDefaultEnv(t *testing.T) {
	ctrl, _, cleanup := startTestAgent(t)
	defer cleanup()

	conn := dialControl(t, ctrl)
	defer conn.Close()

	sendExec(t, conn, proto.ExecRequest{
		Argv: []string{"sh", "-c", "echo PATH=$PATH; echo TERM=$TERM; echo HOME=$HOME; echo LANG=$LANG"},
	})
	stdout, _, code := readAllExec(t, conn)

	if code != 0 {
		t.Errorf("exit code: %d", code)
	}
	if !strings.Contains(stdout, "PATH=/usr/local/sbin") {
		t.Errorf("missing default PATH in: %q", stdout)
	}
	if !strings.Contains(stdout, "TERM=xterm-256color") {
		t.Errorf("missing default TERM in: %q", stdout)
	}
	if !strings.Contains(stdout, "HOME=/root") {
		t.Errorf("missing default HOME in: %q", stdout)
	}
	if !strings.Contains(stdout, "LANG=en_US.UTF-8") {
		t.Errorf("missing default LANG in: %q", stdout)
	}
}

// --- Multiple commands on same agent (concurrency) ---

func TestAgentMultipleCommands(t *testing.T) {
	ctrl, _, cleanup := startTestAgent(t)
	defer cleanup()

	// Run 5 commands concurrently on the same agent, each on its own connection.
	const n = 5
	type result struct {
		stdout string
		code   int32
	}
	results := make(chan result, n)

	for i := 0; i < n; i++ {
		go func(i int) {
			conn := dialControl(t, ctrl)
			defer conn.Close()

			sendExec(t, conn, proto.ExecRequest{
				Argv: []string{"sh", "-c", "echo " + strings.Repeat("x", 100)},
			})
			stdout, _, code := readAllExec(t, conn)
			results <- result{stdout, code}
		}(i)
	}

	for i := 0; i < n; i++ {
		r := <-results
		if r.code != 0 {
			t.Errorf("command %d: exit code %d", i, r.code)
		}
		expected := strings.Repeat("x", 100) + "\n"
		if r.stdout != expected {
			t.Errorf("command %d: stdout length %d, want %d", i, len(r.stdout), len(expected))
		}
	}
}

// --- Host disconnect during exec ---

func TestAgentHostDisconnect(t *testing.T) {
	ctrl, _, cleanup := startTestAgent(t)
	defer cleanup()

	conn := dialControl(t, ctrl)

	// Start a long-running command.
	sendExec(t, conn, proto.ExecRequest{Argv: []string{"sleep", "60"}})
	time.Sleep(100 * time.Millisecond)

	// Abruptly close the connection. The agent should clean up.
	conn.Close()

	// Verify the agent is still healthy by running another command.
	time.Sleep(200 * time.Millisecond)
	conn2 := dialControl(t, ctrl)
	defer conn2.Close()

	sendExec(t, conn2, proto.ExecRequest{Argv: []string{"echo", "still alive"}})
	stdout, _, code := readAllExec(t, conn2)

	if code != 0 {
		t.Errorf("exit code: %d", code)
	}
	if strings.TrimSpace(stdout) != "still alive" {
		t.Errorf("stdout: %q", stdout)
	}
}

// --- TTY tests ---

// ttyFrame is a frame received from a TTY connection.
type ttyFrame struct {
	msgType byte
	payload []byte
	err     error
}

// startTTYReader spawns a goroutine that reads frames from conn
// and sends them to the returned channel. Avoids SetReadDeadline which
// corrupts frame boundaries when io.ReadFull gets a timeout mid-read.
func startTTYReader(conn net.Conn) <-chan ttyFrame {
	ch := make(chan ttyFrame, 64)
	go func() {
		defer close(ch)
		for {
			msgType, payload, err := proto.ReadFrame(conn)
			if err != nil {
				ch <- ttyFrame{err: err}
				return
			}
			ch <- ttyFrame{msgType: msgType, payload: payload}
		}
	}()
	return ch
}

// waitForOutput reads frames from ch until the accumulated STDOUT contains substr,
// or the timeout expires. Also returns early on ERROR or EXIT frames.
func waitForOutput(t *testing.T, ch <-chan ttyFrame, substr string, timeout time.Duration) (output string, exitCode *int32) {
	t.Helper()
	var buf bytes.Buffer
	timer := time.After(timeout)
	for {
		select {
		case f, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed, output so far: %q", buf.String())
			}
			if f.err != nil {
				t.Fatalf("read error: %v, output so far: %q", f.err, buf.String())
			}
			switch f.msgType {
			case proto.STDOUT:
				buf.Write(f.payload)
				if substr != "" && strings.Contains(buf.String(), substr) {
					return buf.String(), nil
				}
			case proto.EXIT:
				code, _ := proto.ParseExitCode(f.payload)
				return buf.String(), &code
			case proto.ERROR:
				t.Fatalf("agent error: %s", f.payload)
			}
		case <-timer:
			t.Fatalf("timeout waiting for %q, output so far: %q", substr, buf.String())
		}
	}
}

func TestAgentTTY(t *testing.T) {
	ctrl, _, cleanup := startTestAgent(t)
	defer cleanup()

	conn := dialControl(t, ctrl)
	defer conn.Close()

	ttyTrue := true
	sendExec(t, conn, proto.ExecRequest{
		Argv: []string{"/bin/sh"},
		TTY:  &ttyTrue,
	})

	ch := startTTYReader(conn)

	// Give the shell a moment to start, then send command.
	time.Sleep(200 * time.Millisecond)
	proto.WriteFrame(conn, proto.STDIN, []byte("echo hello\n"))

	// Wait for "hello" in output.
	waitForOutput(t, ch, "hello", 5*time.Second)

	// Exit the shell.
	proto.WriteFrame(conn, proto.STDIN, []byte("exit\n"))

	// Wait for EXIT frame.
	timer := time.After(5 * time.Second)
	for {
		select {
		case f := <-ch:
			if f.err != nil {
				return // connection closed, that's fine
			}
			if f.msgType == proto.EXIT {
				code, _ := proto.ParseExitCode(f.payload)
				if code != 0 {
					t.Errorf("exit code: %d, want 0", code)
				}
				return
			}
		case <-timer:
			t.Fatal("timeout waiting for EXIT")
		}
	}
}

func TestAgentTTYResize(t *testing.T) {
	ctrl, _, cleanup := startTestAgent(t)
	defer cleanup()

	conn := dialControl(t, ctrl)
	defer conn.Close()

	ttyTrue := true
	rows := uint16(24)
	cols := uint16(80)
	sendExec(t, conn, proto.ExecRequest{
		Argv: []string{"/bin/sh"},
		TTY:  &ttyTrue,
		Rows: &rows,
		Cols: &cols,
	})

	ch := startTTYReader(conn)
	time.Sleep(200 * time.Millisecond)

	// Send RESIZE.
	resize := proto.ResizePayload(40, 120)
	proto.WriteFrame(conn, proto.RESIZE, resize[:])
	time.Sleep(100 * time.Millisecond)

	// Ask for terminal size.
	proto.WriteFrame(conn, proto.STDIN, []byte("stty size\n"))

	// Wait for "40 120" in output.
	waitForOutput(t, ch, "40 120", 5*time.Second)

	// Clean exit.
	proto.WriteFrame(conn, proto.STDIN, []byte("exit\n"))
	timer := time.After(5 * time.Second)
	for {
		select {
		case f := <-ch:
			if f.err != nil || f.msgType == proto.EXIT {
				return
			}
		case <-timer:
			return // timeout is ok, we verified resize worked
		}
	}
}

// --- Port forwarding tests ---

func TestAgentForward(t *testing.T) {
	_, fwdSock, cleanup := startTestAgent(t)
	defer cleanup()

	// Start a TCP echo server on a random port.
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
		io.Copy(conn, conn) // echo
	}()

	// Connect to forward socket.
	conn, err := net.Dial("unix", fwdSock)
	if err != nil {
		t.Fatalf("dial forward: %v", err)
	}
	defer conn.Close()

	// Send FWD_REQ.
	proto.SendJSON(conn, proto.FWD_REQ, proto.ForwardRequest{Port: uint16(port)})

	// Read FWD_RESP.
	msgType, payload, err := proto.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read fwd resp: %v", err)
	}
	if msgType != proto.FWD_RESP {
		t.Fatalf("expected FWD_RESP, got 0x%02x", msgType)
	}
	var resp proto.ForwardResponse
	json.Unmarshal(payload, &resp)
	if resp.Status != "ok" {
		t.Fatalf("forward status: %q", resp.Status)
	}

	// After handshake, raw bytes. Write "ping", read it back.
	conn.Write([]byte("ping"))
	buf := make([]byte, 4)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != "ping" {
		t.Errorf("echo: %q, want %q", buf, "ping")
	}
}

func TestAgentForwardLargeData(t *testing.T) {
	_, fwdSock, cleanup := startTestAgent(t)
	defer cleanup()

	// TCP server that echoes everything back.
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

	conn, err := net.Dial("unix", fwdSock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	proto.SendJSON(conn, proto.FWD_REQ, proto.ForwardRequest{Port: uint16(port)})
	msgType, payload, err := proto.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read resp: %v", err)
	}
	if msgType != proto.FWD_RESP {
		t.Fatalf("expected FWD_RESP, got 0x%02x", msgType)
	}
	var resp proto.ForwardResponse
	json.Unmarshal(payload, &resp)
	if resp.Status != "ok" {
		t.Fatalf("status: %q", resp.Status)
	}

	// Send 64KB through the tunnel and verify echo.
	data := make([]byte, 64*1024)
	for i := range data {
		data[i] = byte(i % 256)
	}

	go func() {
		conn.Write(data)
	}()

	received := make([]byte, len(data))
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, received); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if !bytes.Equal(data, received) {
		t.Error("echoed data does not match sent data")
	}
}

// --- File operation tests ---

func TestAgentFileWriteRead(t *testing.T) {
	ctrl, _, cleanup := startTestAgent(t)
	defer cleanup()

	filePath := filepath.Join(t.TempDir(), "test.txt")
	content := []byte("hello world")

	// Write file
	conn := dialControl(t, ctrl)
	proto.SendJSON(conn, proto.FILE_WRITE_REQ, map[string]any{
		"path": filePath, "mode": "0644", "size": len(content),
	})
	proto.WriteFrame(conn, proto.STDIN, content)

	msgType, payload, err := proto.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read write resp: %v", err)
	}
	if msgType == proto.ERROR {
		t.Fatalf("write error: %s", payload)
	}
	if msgType != proto.FILE_WRITE_RESP {
		t.Fatalf("expected FILE_WRITE_RESP, got 0x%02x", msgType)
	}
	conn.Close()

	// Read file back
	conn2 := dialControl(t, ctrl)
	defer conn2.Close()
	proto.SendJSON(conn2, proto.FILE_READ_REQ, map[string]string{"path": filePath})

	msgType, payload, err = proto.ReadFrame(conn2)
	if err != nil {
		t.Fatalf("read resp: %v", err)
	}
	if msgType == proto.ERROR {
		t.Fatalf("read error: %s", payload)
	}
	if msgType != proto.FILE_READ_RESP {
		t.Fatalf("expected FILE_READ_RESP, got 0x%02x", msgType)
	}

	// Collect STDOUT frames until EXIT
	var buf bytes.Buffer
	for {
		msgType, payload, err = proto.ReadFrame(conn2)
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		if msgType == proto.STDOUT {
			buf.Write(payload)
		}
		if msgType == proto.EXIT {
			break
		}
	}
	if buf.String() != "hello world" {
		t.Errorf("content: %q, want %q", buf.String(), "hello world")
	}
}

func TestAgentFileReadNotFound(t *testing.T) {
	ctrl, _, cleanup := startTestAgent(t)
	defer cleanup()

	conn := dialControl(t, ctrl)
	defer conn.Close()

	proto.SendJSON(conn, proto.FILE_READ_REQ, map[string]string{"path": "/nonexistent/file.txt"})

	msgType, payload, err := proto.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if msgType != proto.ERROR {
		t.Fatalf("expected ERROR, got 0x%02x: %s", msgType, payload)
	}
	if !strings.Contains(string(payload), "no such file") {
		t.Errorf("error: %q, want 'no such file'", payload)
	}
}

func TestAgentFileStat(t *testing.T) {
	ctrl, _, cleanup := startTestAgent(t)
	defer cleanup()

	filePath := filepath.Join(t.TempDir(), "stat-test.txt")
	os.WriteFile(filePath, []byte("stat me"), 0644)

	conn := dialControl(t, ctrl)
	defer conn.Close()

	proto.SendJSON(conn, proto.FILE_STAT_REQ, map[string]string{"path": filePath})

	msgType, payload, err := proto.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if msgType == proto.ERROR {
		t.Fatalf("stat error: %s", payload)
	}
	if msgType != proto.FILE_STAT_RESP {
		t.Fatalf("expected FILE_STAT_RESP, got 0x%02x", msgType)
	}
	var info proto.FileInfo
	json.Unmarshal(payload, &info)
	if info.Name != "stat-test.txt" {
		t.Errorf("name: %q", info.Name)
	}
	if info.Size != 7 {
		t.Errorf("size: %d, want 7", info.Size)
	}
	if info.Mode != "0644" {
		t.Errorf("mode: %q, want 0644", info.Mode)
	}
	if info.IsDir {
		t.Error("is_dir should be false")
	}
}

func TestAgentFileList(t *testing.T) {
	ctrl, _, cleanup := startTestAgent(t)
	defer cleanup()

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("aaa"), 0644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("bbb"), 0644)
	os.Mkdir(filepath.Join(dir, "subdir"), 0755)

	conn := dialControl(t, ctrl)
	defer conn.Close()

	proto.SendJSON(conn, proto.FILE_LS_REQ, map[string]string{"path": dir})

	msgType, payload, err := proto.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if msgType == proto.ERROR {
		t.Fatalf("ls error: %s", payload)
	}
	if msgType != proto.FILE_LS_RESP {
		t.Fatalf("expected FILE_LS_RESP, got 0x%02x", msgType)
	}
	var files []proto.FileInfo
	json.Unmarshal(payload, &files)

	if len(files) != 3 {
		t.Fatalf("got %d files, want 3", len(files))
	}
	names := map[string]bool{}
	var foundDir bool
	for _, f := range files {
		names[f.Name] = true
		if f.Name == "subdir" && f.IsDir {
			foundDir = true
		}
	}
	if !names["a.txt"] || !names["b.txt"] || !names["subdir"] {
		t.Errorf("names: %v", names)
	}
	if !foundDir {
		t.Error("subdir not marked as directory")
	}
}

func TestAgentFileLargeRoundTrip(t *testing.T) {
	ctrl, _, cleanup := startTestAgent(t)
	defer cleanup()

	filePath := filepath.Join(t.TempDir(), "large.bin")

	// Generate 1MB of data with a known pattern
	size := 1024 * 1024
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 251) // prime to catch alignment bugs
	}

	// Write
	conn := dialControl(t, ctrl)
	proto.SendJSON(conn, proto.FILE_WRITE_REQ, map[string]any{
		"path": filePath, "mode": "0644", "size": size,
	})
	// Send in chunks
	for off := 0; off < size; off += 32768 {
		end := off + 32768
		if end > size {
			end = size
		}
		proto.WriteFrame(conn, proto.STDIN, data[off:end])
	}
	msgType, payload, err := proto.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read write resp: %v", err)
	}
	if msgType == proto.ERROR {
		t.Fatalf("write error: %s", payload)
	}
	conn.Close()

	// Read back
	conn2 := dialControl(t, ctrl)
	defer conn2.Close()
	proto.SendJSON(conn2, proto.FILE_READ_REQ, map[string]string{"path": filePath})

	msgType, _, err = proto.ReadFrame(conn2)
	if err != nil {
		t.Fatalf("read resp: %v", err)
	}
	if msgType != proto.FILE_READ_RESP {
		t.Fatalf("expected FILE_READ_RESP, got 0x%02x", msgType)
	}

	var buf bytes.Buffer
	for {
		msgType, payload, err = proto.ReadFrame(conn2)
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		if msgType == proto.STDOUT {
			buf.Write(payload)
		}
		if msgType == proto.EXIT {
			break
		}
	}
	if buf.Len() != size {
		t.Fatalf("read %d bytes, want %d", buf.Len(), size)
	}
	if !bytes.Equal(buf.Bytes(), data) {
		t.Error("data mismatch after round-trip")
	}
}

func TestAgentFileWritePermissions(t *testing.T) {
	ctrl, _, cleanup := startTestAgent(t)
	defer cleanup()

	filePath := filepath.Join(t.TempDir(), "secret.txt")
	content := []byte("secret")

	conn := dialControl(t, ctrl)
	proto.SendJSON(conn, proto.FILE_WRITE_REQ, map[string]any{
		"path": filePath, "mode": "0600", "size": len(content),
	})
	proto.WriteFrame(conn, proto.STDIN, content)
	proto.ReadFrame(conn) // consume resp
	conn.Close()

	// Check mode
	info, err := os.Stat(filePath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	mode := fmt.Sprintf("%04o", info.Mode().Perm())
	if mode != "0600" {
		t.Errorf("mode: %s, want 0600", mode)
	}
}

func TestAgentFileWriteCreatesDirs(t *testing.T) {
	ctrl, _, cleanup := startTestAgent(t)
	defer cleanup()

	filePath := filepath.Join(t.TempDir(), "deep", "nested", "dir", "file.txt")
	content := []byte("nested content")

	conn := dialControl(t, ctrl)
	proto.SendJSON(conn, proto.FILE_WRITE_REQ, map[string]any{
		"path": filePath, "mode": "0644", "size": len(content),
	})
	proto.WriteFrame(conn, proto.STDIN, content)
	msgType, payload, _ := proto.ReadFrame(conn)
	conn.Close()

	if msgType == proto.ERROR {
		t.Fatalf("write error: %s", payload)
	}

	// Verify file exists
	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(data) != "nested content" {
		t.Errorf("content: %q", data)
	}
}

func TestAgentFileListEmpty(t *testing.T) {
	ctrl, _, cleanup := startTestAgent(t)
	defer cleanup()

	dir := t.TempDir()

	conn := dialControl(t, ctrl)
	defer conn.Close()

	proto.SendJSON(conn, proto.FILE_LS_REQ, map[string]string{"path": dir})

	msgType, payload, err := proto.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if msgType != proto.FILE_LS_RESP {
		t.Fatalf("expected FILE_LS_RESP, got 0x%02x: %s", msgType, payload)
	}
	var files []proto.FileInfo
	json.Unmarshal(payload, &files)
	if len(files) != 0 {
		t.Errorf("expected empty list, got %d entries", len(files))
	}
}

// --- Edge case tests ---

func TestAgentFileZeroByte(t *testing.T) {
	ctrl, _, cleanup := startTestAgent(t)
	defer cleanup()

	filePath := filepath.Join(t.TempDir(), "__init__.py")

	// Write zero-byte file
	conn := dialControl(t, ctrl)
	proto.SendJSON(conn, proto.FILE_WRITE_REQ, map[string]any{
		"path": filePath, "mode": "0644", "size": 0,
	})
	msgType, payload, err := proto.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if msgType == proto.ERROR {
		t.Fatalf("write error: %s", payload)
	}
	conn.Close()

	// Verify file exists and is empty
	info, err := os.Stat(filePath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("size: %d, want 0", info.Size())
	}

	// Read it back
	conn2 := dialControl(t, ctrl)
	defer conn2.Close()
	proto.SendJSON(conn2, proto.FILE_READ_REQ, map[string]string{"path": filePath})

	msgType, payload, err = proto.ReadFrame(conn2)
	if err != nil {
		t.Fatalf("read resp: %v", err)
	}
	if msgType != proto.FILE_READ_RESP {
		t.Fatalf("expected FILE_READ_RESP, got 0x%02x: %s", msgType, payload)
	}

	// Should get EXIT immediately (no STDOUT frames)
	msgType, _, err = proto.ReadFrame(conn2)
	if err != nil {
		t.Fatalf("read exit: %v", err)
	}
	if msgType != proto.EXIT {
		t.Fatalf("expected EXIT, got 0x%02x", msgType)
	}
}

func TestAgentFileBinaryData(t *testing.T) {
	ctrl, _, cleanup := startTestAgent(t)
	defer cleanup()

	filePath := filepath.Join(t.TempDir(), "binary.bin")

	// All 256 byte values including \x00
	data := make([]byte, 256)
	for i := range data {
		data[i] = byte(i)
	}

	// Write
	conn := dialControl(t, ctrl)
	proto.SendJSON(conn, proto.FILE_WRITE_REQ, map[string]any{
		"path": filePath, "mode": "0644", "size": len(data),
	})
	proto.WriteFrame(conn, proto.STDIN, data)
	msgType, payload, _ := proto.ReadFrame(conn)
	if msgType == proto.ERROR {
		t.Fatalf("write error: %s", payload)
	}
	conn.Close()

	// Read back
	conn2 := dialControl(t, ctrl)
	defer conn2.Close()
	proto.SendJSON(conn2, proto.FILE_READ_REQ, map[string]string{"path": filePath})
	proto.ReadFrame(conn2) // FILE_READ_RESP

	var buf bytes.Buffer
	for {
		msgType, payload, err := proto.ReadFrame(conn2)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if msgType == proto.STDOUT {
			buf.Write(payload)
		}
		if msgType == proto.EXIT {
			break
		}
	}
	if !bytes.Equal(buf.Bytes(), data) {
		t.Errorf("binary data corrupted: got %d bytes, want %d", buf.Len(), len(data))
		// Show first difference
		for i := 0; i < len(data) && i < buf.Len(); i++ {
			if data[i] != buf.Bytes()[i] {
				t.Errorf("first diff at byte %d: got 0x%02x, want 0x%02x", i, buf.Bytes()[i], data[i])
				break
			}
		}
	}
}

func TestAgentFileReadDirectory(t *testing.T) {
	ctrl, _, cleanup := startTestAgent(t)
	defer cleanup()

	conn := dialControl(t, ctrl)
	defer conn.Close()

	proto.SendJSON(conn, proto.FILE_READ_REQ, map[string]string{"path": "/tmp"})

	msgType, payload, err := proto.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if msgType != proto.ERROR {
		t.Fatalf("expected ERROR for directory read, got 0x%02x: %s", msgType, payload)
	}
	if !strings.Contains(string(payload), "directory") {
		t.Errorf("error: %q, want 'directory'", payload)
	}
}

func TestAgentFileStatNotFound(t *testing.T) {
	ctrl, _, cleanup := startTestAgent(t)
	defer cleanup()

	conn := dialControl(t, ctrl)
	defer conn.Close()

	proto.SendJSON(conn, proto.FILE_STAT_REQ, map[string]string{"path": "/nonexistent/file.txt"})

	msgType, payload, err := proto.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if msgType != proto.ERROR {
		t.Fatalf("expected ERROR, got 0x%02x: %s", msgType, payload)
	}
	if !strings.Contains(string(payload), "no such file") {
		t.Errorf("error: %q, want 'no such file'", payload)
	}
}

func TestAgentFileListNotFound(t *testing.T) {
	ctrl, _, cleanup := startTestAgent(t)
	defer cleanup()

	conn := dialControl(t, ctrl)
	defer conn.Close()

	proto.SendJSON(conn, proto.FILE_LS_REQ, map[string]string{"path": "/nonexistent/dir"})

	msgType, payload, err := proto.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if msgType != proto.ERROR {
		t.Fatalf("expected ERROR, got 0x%02x: %s", msgType, payload)
	}
	if !strings.Contains(string(payload), "no such file") {
		t.Errorf("error: %q, want 'no such file'", payload)
	}
}

func TestAgentFileListNotADirectory(t *testing.T) {
	ctrl, _, cleanup := startTestAgent(t)
	defer cleanup()

	filePath := filepath.Join(t.TempDir(), "regular.txt")
	os.WriteFile(filePath, []byte("not a dir"), 0644)

	conn := dialControl(t, ctrl)
	defer conn.Close()

	proto.SendJSON(conn, proto.FILE_LS_REQ, map[string]string{"path": filePath})

	msgType, payload, err := proto.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if msgType != proto.ERROR {
		t.Fatalf("expected ERROR for ls on file, got 0x%02x: %s", msgType, payload)
	}
	if !strings.Contains(string(payload), "not a directory") {
		t.Errorf("error: %q, want 'not a directory'", payload)
	}
}

func TestAgentFileWriteNegativeSize(t *testing.T) {
	ctrl, _, cleanup := startTestAgent(t)
	defer cleanup()

	conn := dialControl(t, ctrl)
	defer conn.Close()

	proto.SendJSON(conn, proto.FILE_WRITE_REQ, map[string]any{
		"path": "/tmp/bad.txt", "mode": "0644", "size": -1,
	})

	msgType, payload, err := proto.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if msgType != proto.ERROR {
		t.Fatalf("expected ERROR for negative size, got 0x%02x: %s", msgType, payload)
	}
	if !strings.Contains(string(payload), "size must be >= 0") {
		t.Errorf("error: %q", payload)
	}
}

func TestAgentFileWriteBadPath(t *testing.T) {
	ctrl, _, cleanup := startTestAgent(t)
	defer cleanup()

	// Create a regular file, then try to write a child under it.
	// Even root can't do this — "not a directory".
	blocker := filepath.Join(t.TempDir(), "blocker")
	os.WriteFile(blocker, []byte("x"), 0644)

	conn := dialControl(t, ctrl)
	defer conn.Close()

	proto.SendJSON(conn, proto.FILE_WRITE_REQ, map[string]any{
		"path": filepath.Join(blocker, "child.txt"), "mode": "0644", "size": 5,
	})

	msgType, payload, err := proto.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if msgType != proto.ERROR {
		t.Fatalf("expected ERROR, got 0x%02x: %s", msgType, payload)
	}
	if !strings.Contains(string(payload), "not a directory") {
		t.Errorf("error: %q, want 'not a directory'", payload)
	}
}

func TestAgentFileUnicodeFilename(t *testing.T) {
	ctrl, _, cleanup := startTestAgent(t)
	defer cleanup()

	dir := t.TempDir()
	// File with unicode name, spaces, and special chars
	filePath := filepath.Join(dir, "日本語 file (1).txt")
	content := []byte("unicode content 🎉")

	// Write
	conn := dialControl(t, ctrl)
	proto.SendJSON(conn, proto.FILE_WRITE_REQ, map[string]any{
		"path": filePath, "mode": "0644", "size": len(content),
	})
	proto.WriteFrame(conn, proto.STDIN, content)
	msgType, payload, _ := proto.ReadFrame(conn)
	if msgType == proto.ERROR {
		t.Fatalf("write error: %s", payload)
	}
	conn.Close()

	// Read back
	conn2 := dialControl(t, ctrl)
	defer conn2.Close()
	proto.SendJSON(conn2, proto.FILE_READ_REQ, map[string]string{"path": filePath})
	proto.ReadFrame(conn2) // FILE_READ_RESP

	var buf bytes.Buffer
	for {
		msgType, payload, err := proto.ReadFrame(conn2)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if msgType == proto.STDOUT {
			buf.Write(payload)
		}
		if msgType == proto.EXIT {
			break
		}
	}
	if buf.String() != string(content) {
		t.Errorf("content: %q, want %q", buf.String(), content)
	}
}

// --- Concurrency tests ---

func TestAgentFileConcurrentWritesDifferentFiles(t *testing.T) {
	ctrl, _, cleanup := startTestAgent(t)
	defer cleanup()

	dir := t.TempDir()
	const n = 10
	errs := make(chan error, n)

	for i := 0; i < n; i++ {
		go func(i int) {
			conn := dialControl(t, ctrl)
			defer conn.Close()

			path := filepath.Join(dir, fmt.Sprintf("file-%d.txt", i))
			content := fmt.Sprintf("content from goroutine %d", i)

			proto.SendJSON(conn, proto.FILE_WRITE_REQ, map[string]any{
				"path": path, "mode": "0644", "size": len(content),
			})
			proto.WriteFrame(conn, proto.STDIN, []byte(content))

			msgType, payload, err := proto.ReadFrame(conn)
			if err != nil {
				errs <- fmt.Errorf("goroutine %d: read: %v", i, err)
				return
			}
			if msgType == proto.ERROR {
				errs <- fmt.Errorf("goroutine %d: %s", i, payload)
				return
			}
			errs <- nil
		}(i)
	}

	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}

	// Verify all files
	for i := 0; i < n; i++ {
		path := filepath.Join(dir, fmt.Sprintf("file-%d.txt", i))
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read file %d: %v", i, err)
			continue
		}
		expected := fmt.Sprintf("content from goroutine %d", i)
		if string(data) != expected {
			t.Errorf("file %d: %q, want %q", i, data, expected)
		}
	}
}

func TestAgentFileConcurrentReadWrite(t *testing.T) {
	ctrl, _, cleanup := startTestAgent(t)
	defer cleanup()

	filePath := filepath.Join(t.TempDir(), "shared.txt")
	original := "original content"
	os.WriteFile(filePath, []byte(original), 0644)

	updated := "updated content!!"
	done := make(chan struct{})

	// Writer goroutine: writes new content
	go func() {
		defer close(done)
		conn := dialControl(t, ctrl)
		defer conn.Close()

		proto.SendJSON(conn, proto.FILE_WRITE_REQ, map[string]any{
			"path": filePath, "mode": "0644", "size": len(updated),
		})
		proto.WriteFrame(conn, proto.STDIN, []byte(updated))
		proto.ReadFrame(conn) // consume resp
	}()

	// Wait for writer to finish (atomic rename guarantees consistency)
	<-done

	// Reader: should see complete content (old or new, never partial)
	conn := dialControl(t, ctrl)
	defer conn.Close()
	proto.SendJSON(conn, proto.FILE_READ_REQ, map[string]string{"path": filePath})

	msgType, _, err := proto.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read resp: %v", err)
	}
	if msgType != proto.FILE_READ_RESP {
		t.Fatalf("expected FILE_READ_RESP, got 0x%02x", msgType)
	}

	var buf bytes.Buffer
	for {
		msgType, payload, err := proto.ReadFrame(conn)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if msgType == proto.STDOUT {
			buf.Write(payload)
		}
		if msgType == proto.EXIT {
			break
		}
	}

	got := buf.String()
	if got != original && got != updated {
		t.Errorf("got %q — expected either %q or %q (partial content!)", got, original, updated)
	}
}

func TestAgentFileConcurrentWritesSameFile(t *testing.T) {
	ctrl, _, cleanup := startTestAgent(t)
	defer cleanup()

	filePath := filepath.Join(t.TempDir(), "race.txt")
	const n = 5
	done := make(chan int, n) // each goroutine sends its index

	for i := 0; i < n; i++ {
		go func(i int) {
			conn := dialControl(t, ctrl)
			defer conn.Close()

			// Each writer writes a unique, distinguishable content
			content := strings.Repeat(fmt.Sprintf("%d", i), 100)
			proto.SendJSON(conn, proto.FILE_WRITE_REQ, map[string]any{
				"path": filePath, "mode": "0644", "size": len(content),
			})
			proto.WriteFrame(conn, proto.STDIN, []byte(content))
			proto.ReadFrame(conn)
			done <- i
		}(i)
	}

	for i := 0; i < n; i++ {
		<-done
	}

	// File should contain ONE writer's complete content (atomic rename),
	// not a mix of multiple writers.
	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content := string(data)
	if len(content) != 100 {
		t.Fatalf("length: %d, want 100", len(content))
	}
	// All chars should be the same digit (from one writer)
	first := content[0]
	for i, c := range content {
		if byte(c) != first {
			t.Fatalf("mixed content at byte %d: got %c, expected %c (non-atomic write!)", i, c, first)
		}
	}
	t.Logf("winner: goroutine %c", first)
}

func TestAgentFileAtomicWriteVisibility(t *testing.T) {
	// Simulates the agentic pattern: write file, then immediately read it back.
	// The read must see the complete written content.
	ctrl, _, cleanup := startTestAgent(t)
	defer cleanup()

	filePath := filepath.Join(t.TempDir(), "causal.txt")

	for round := 0; round < 10; round++ {
		content := fmt.Sprintf("round-%d-data-%s", round, strings.Repeat("x", 1000))

		// Write
		wConn := dialControl(t, ctrl)
		proto.SendJSON(wConn, proto.FILE_WRITE_REQ, map[string]any{
			"path": filePath, "mode": "0644", "size": len(content),
		})
		proto.WriteFrame(wConn, proto.STDIN, []byte(content))
		msgType, payload, _ := proto.ReadFrame(wConn)
		wConn.Close()
		if msgType == proto.ERROR {
			t.Fatalf("round %d write error: %s", round, payload)
		}

		// Immediate read (same logical sequence)
		rConn := dialControl(t, ctrl)
		proto.SendJSON(rConn, proto.FILE_READ_REQ, map[string]string{"path": filePath})
		proto.ReadFrame(rConn) // FILE_READ_RESP

		var buf bytes.Buffer
		for {
			msgType, payload, err := proto.ReadFrame(rConn)
			if err != nil {
				t.Fatalf("round %d read: %v", round, err)
			}
			if msgType == proto.STDOUT {
				buf.Write(payload)
			}
			if msgType == proto.EXIT {
				break
			}
		}
		rConn.Close()

		if buf.String() != content {
			t.Fatalf("round %d: read %d bytes, want %d (causal violation!)", round, buf.Len(), len(content))
		}
	}
}

func TestAgentForwardRefused(t *testing.T) {
	_, fwdSock, cleanup := startTestAgent(t)
	defer cleanup()

	conn, err := net.Dial("unix", fwdSock)
	if err != nil {
		t.Fatalf("dial forward: %v", err)
	}
	defer conn.Close()

	// Forward to a port nobody is listening on.
	proto.SendJSON(conn, proto.FWD_REQ, proto.ForwardRequest{Port: 59999})

	msgType, payload, err := proto.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if msgType != proto.FWD_RESP {
		t.Fatalf("expected FWD_RESP, got 0x%02x", msgType)
	}
	var resp proto.ForwardResponse
	json.Unmarshal(payload, &resp)
	if resp.Status != "error" {
		t.Errorf("status: %q, want %q", resp.Status, "error")
	}
	if resp.Message == nil || !strings.Contains(*resp.Message, "refused") {
		t.Errorf("message: %v, want 'refused'", resp.Message)
	}
}
