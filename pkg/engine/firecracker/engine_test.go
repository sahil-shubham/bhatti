//go:build linux

package firecracker

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sahilshubham/bhatti/pkg/engine"
)

// These tests require root + Firecracker + kernel + rootfs on the Pi.
// Run: sudo go test -v -count=1 -timeout=120s ./pkg/engine/firecracker/

func skipIfNotRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("must run as root")
	}
}

func testEngine(t *testing.T) *Engine {
	t.Helper()
	skipIfNotRoot(t)
	eng, err := New(Config{
		DataDir:    "/var/lib/bhatti",
		KernelPath: "/var/lib/bhatti/images/vmlinux-arm64",
		BaseRootfs: "/var/lib/bhatti/images/rootfs-base-arm64.ext4",
		FCBinary:   "/usr/local/bin/firecracker",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return eng
}

func testSpec(name string) engine.SandboxSpec {
	return engine.SandboxSpec{Name: name, CPUs: 1, MemoryMB: 512}
}

// execWithTimeout wraps eng.Exec with a 15-second timeout to prevent tests
// from hanging if a command blocks (e.g. ping after VM resume).
func execWithTimeout(t *testing.T, eng *Engine, id string, cmd []string) (engine.ExecResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return eng.Exec(ctx, id, cmd)
}

func TestCreateExecDestroy(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("test-1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Logf("created %s (ip=%s)", info.ID, info.IP)
	defer eng.Destroy(ctx, info.ID)

	if info.Status != "running" {
		t.Errorf("status: %q", info.Status)
	}

	// uname
	result, err := execWithTimeout(t, eng, info.ID, []string{"uname", "-a"})
	if err != nil {
		t.Fatalf("Exec uname: %v", err)
	}
	if result.ExitCode != 0 || !strings.Contains(result.Stdout, "aarch64") {
		t.Errorf("uname: exit=%d out=%q", result.ExitCode, result.Stdout)
	}
	t.Logf("uname: %s", strings.TrimSpace(result.Stdout))

	// node
	result, err = execWithTimeout(t, eng, info.ID, []string{"node", "--version"})
	if err != nil {
		t.Fatalf("Exec node: %v", err)
	}
	if !strings.Contains(result.Stdout, "v22") {
		t.Errorf("node: %q", result.Stdout)
	}

	// list
	list, err := eng.List(ctx)
	if err != nil || len(list) != 1 || list[0].ID != info.ID {
		t.Errorf("List: %v err=%v", list, err)
	}

	// destroy
	if err := eng.Destroy(ctx, info.ID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := eng.Status(ctx, info.ID); err == nil {
		t.Error("expected error after destroy")
	}
}

func TestSnapshotResume(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("snap-test"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// 1. Write a file
	r, _ := execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "echo snap-data > /tmp/test && cat /tmp/test"})
	if r.ExitCode != 0 {
		t.Fatalf("write file: exit=%d err=%q", r.ExitCode, r.Stderr)
	}
	t.Logf("wrote file: %s", strings.TrimSpace(r.Stdout))

	// 2. Start a background process and record its PID
	// Redirect stdin/stdout/stderr so the background process doesn't hold the pipe open.
	r, _ = execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "sleep 3600 </dev/null >/dev/null 2>&1 & echo $!"})
	if r.ExitCode != 0 {
		t.Fatalf("start background: exit=%d", r.ExitCode)
	}
	bgPID := strings.TrimSpace(r.Stdout)
	t.Logf("background sleep PID: %s", bgPID)

	// 3. Set an env var in a file to verify later
	execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "echo MY_STATE=preserved > /tmp/env-test"})

	// 4. Stop (snapshot)
	if err := eng.Stop(ctx, info.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	s, _ := eng.Status(ctx, info.ID)
	if s.Status != "stopped" {
		t.Errorf("status after stop: %q", s.Status)
	}
	t.Log("stopped (snapshot created)")

	// 5. Verify FC process is gone
	vm, _ := eng.getVM(info.ID)
	if vm.SnapMemPath == "" || vm.SnapVMPath == "" {
		t.Fatal("snapshot paths not set")
	}

	// 6. Resume
	if err := eng.Start(ctx, info.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Log("resumed")

	// 7. Verify file persists
	r, err = execWithTimeout(t, eng, info.ID, []string{"cat", "/tmp/test"})
	if err != nil {
		t.Fatalf("read file after resume: %v", err)
	}
	if strings.TrimSpace(r.Stdout) != "snap-data" {
		t.Errorf("file: %q, want 'snap-data'", r.Stdout)
	}
	t.Log("✓ file persists")

	// 8. Verify background process is still alive
	r, err = execWithTimeout(t, eng, info.ID, []string{"kill", "-0", bgPID})
	if err != nil {
		t.Fatalf("kill -0 after resume: %v", err)
	}
	if r.ExitCode != 0 {
		t.Errorf("background PID %s not alive: exit=%d stderr=%q", bgPID, r.ExitCode, r.Stderr)
	} else {
		t.Logf("✓ background PID %s still alive", bgPID)
	}

	// 9. Verify it shows in ps
	r, _ = execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "ps aux | grep 'sleep 3600' | grep -v grep"})
	if !strings.Contains(r.Stdout, "sleep 3600") {
		t.Errorf("ps: %q, want 'sleep 3600'", r.Stdout)
	} else {
		t.Log("✓ background process visible in ps")
	}

	// 10. Verify env file persists
	r, _ = execWithTimeout(t, eng, info.ID, []string{"cat", "/tmp/env-test"})
	if !strings.Contains(r.Stdout, "MY_STATE=preserved") {
		t.Errorf("env: %q", r.Stdout)
	} else {
		t.Log("✓ env state preserved")
	}

	// 11. Second stop/start cycle — verify it works multiple times
	execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "echo cycle2 > /tmp/cycle2"})
	if err := eng.Stop(ctx, info.ID); err != nil {
		t.Fatalf("Stop (cycle 2): %v", err)
	}
	if err := eng.Start(ctx, info.ID); err != nil {
		t.Fatalf("Start (cycle 2): %v", err)
	}
	r, _ = execWithTimeout(t, eng, info.ID, []string{"cat", "/tmp/cycle2"})
	if strings.TrimSpace(r.Stdout) != "cycle2" {
		t.Errorf("cycle2 file: %q", r.Stdout)
	} else {
		t.Log("✓ second stop/start cycle works")
	}
}

func TestPortForwarding(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("fwd-test"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Start python HTTP server in background (redirect fds to avoid pipe hold)
	execWithTimeout(t, eng, info.ID, []string{"sh", "-c",
		"cd /tmp && echo hello-from-vm > index.html && python3 -m http.server 9000 </dev/null >/dev/null 2>&1 &"})
	time.Sleep(1 * time.Second)

	// Verify it's listening
	r, _ := execWithTimeout(t, eng, info.ID, []string{"ss", "-tln"})
	if !strings.Contains(r.Stdout, "9000") {
		t.Fatalf("port 9000 not listening: %s", r.Stdout)
	}

	// ListeningPorts should find it
	ports, err := eng.ListeningPorts(ctx, info.ID)
	if err != nil {
		t.Fatalf("ListeningPorts: %v", err)
	}
	found := false
	for _, p := range ports {
		if p == 9000 {
			found = true
		}
	}
	if !found {
		t.Errorf("port 9000 not in ListeningPorts: %v", ports)
	}

	// Tunnel from host and make an HTTP request
	tunnel, err := eng.Tunnel(ctx, info.ID, 9000)
	if err != nil {
		t.Fatalf("Tunnel: %v", err)
	}
	defer tunnel.Close()

	// Send HTTP GET and read full response
	fmt.Fprintf(tunnel, "GET /index.html HTTP/1.0\r\nHost: localhost\r\n\r\n")
	tunnel.(interface{ SetReadDeadline(time.Time) error }).SetReadDeadline(time.Now().Add(3 * time.Second))
	var resp strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := tunnel.Read(buf)
		if n > 0 {
			resp.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	if !strings.Contains(resp.String(), "hello-from-vm") {
		t.Errorf("tunnel response missing body: %q", resp.String())
	} else {
		t.Log("✓ port forwarding + HTTP through VM tunnel works")
	}
}

func TestShell(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("shell-test"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	term, err := eng.Shell(ctx, info.ID)
	if err != nil {
		t.Fatalf("Shell: %v", err)
	}
	defer term.Close()

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
				return
			}
		}
	}()

	time.Sleep(500 * time.Millisecond)
	term.Write([]byte("echo shell-works\n"))

	var out strings.Builder
	timer := time.After(5 * time.Second)
	for {
		select {
		case data := <-ch:
			out.Write(data)
			if strings.Contains(out.String(), "shell-works") {
				t.Log("shell inside VM works ✓")
				term.Write([]byte("exit\n"))
				return
			}
		case <-timer:
			t.Fatalf("timeout, output: %q", out.String())
		}
	}
}
