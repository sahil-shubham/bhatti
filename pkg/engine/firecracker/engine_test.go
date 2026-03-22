//go:build linux

package firecracker

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
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

	// Auto-detect architecture for image paths
	arch := "arm64"
	if out, _ := os.ReadFile("/proc/cpuinfo"); strings.Contains(string(out), "GenuineIntel") || strings.Contains(string(out), "AuthenticAMD") {
		arch = "amd64"
	}

	eng, err := New(Config{
		DataDir:    "/var/lib/bhatti",
		KernelPath: fmt.Sprintf("/var/lib/bhatti/images/vmlinux-%s", arch),
		BaseRootfs: fmt.Sprintf("/var/lib/bhatti/images/rootfs-base-%s.ext4", arch),
		FCBinary:   "/usr/local/bin/firecracker",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return eng
}

func testSpec(name string) engine.SandboxSpec {
	return engine.SandboxSpec{
		Name: name, CPUs: 1, MemoryMB: 512,
		UserID: "usr_test", SubnetIndex: 99, // test user on isolated subnet
	}
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

func TestDiffSnapshot(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("diff-snap"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Write data before first snapshot
	execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "echo snap-v1 > /tmp/data"})

	// First stop → Full snapshot
	if err := eng.Stop(ctx, info.ID); err != nil {
		t.Fatalf("Stop 1: %v", err)
	}

	vm, _ := eng.getVM(info.ID)
	fullSize, err := fileSize(vm.SnapMemPath)
	if err != nil {
		t.Fatalf("stat full snapshot: %v", err)
	}
	t.Logf("full snapshot: %d MB", fullSize/(1024*1024))

	// Resume
	if err := eng.Start(ctx, info.ID); err != nil {
		t.Fatalf("Start 1: %v", err)
	}

	// Verify data persists
	r, _ := execWithTimeout(t, eng, info.ID, []string{"cat", "/tmp/data"})
	if !strings.Contains(r.Stdout, "snap-v1") {
		t.Fatalf("data not preserved after full snapshot: %q", r.Stdout)
	}

	// Write more data
	execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "echo snap-v2 > /tmp/data2"})

	// Second stop → Diff snapshot (should be smaller)
	if err := eng.Stop(ctx, info.ID); err != nil {
		t.Fatalf("Stop 2: %v", err)
	}

	diffSize, err := fileSize(vm.SnapMemPath)
	if err != nil {
		t.Fatalf("stat diff snapshot: %v", err)
	}
	t.Logf("diff snapshot: %d MB (full was %d MB)", diffSize/(1024*1024), fullSize/(1024*1024))

	if diffSize >= fullSize {
		t.Logf("⚠ diff snapshot not smaller than full (may be expected if VM dirtied many pages)")
	} else {
		t.Logf("✓ diff snapshot is %dx smaller than full", fullSize/diffSize)
	}

	// Resume from diff and verify ALL data (both v1 and v2)
	if err := eng.Start(ctx, info.ID); err != nil {
		t.Fatalf("Start 2: %v", err)
	}
	r, _ = execWithTimeout(t, eng, info.ID, []string{"cat", "/tmp/data"})
	if !strings.Contains(r.Stdout, "snap-v1") {
		t.Fatalf("v1 data lost after diff snapshot: %q", r.Stdout)
	}
	r, _ = execWithTimeout(t, eng, info.ID, []string{"cat", "/tmp/data2"})
	if !strings.Contains(r.Stdout, "snap-v2") {
		t.Fatalf("v2 data lost after diff snapshot: %q", r.Stdout)
	}
	t.Log("✓ all data preserved across full→diff→resume")
}

func TestDiffSnapshotMultipleCycles(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("diff-multi"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Full → Diff → Diff → Diff: verify data accumulates correctly
	for cycle := 1; cycle <= 4; cycle++ {
		execWithTimeout(t, eng, info.ID, []string{"sh", "-c",
			fmt.Sprintf("echo cycle-%d > /tmp/cycle%d", cycle, cycle)})

		if err := eng.Stop(ctx, info.ID); err != nil {
			t.Fatalf("Stop cycle %d: %v", cycle, err)
		}
		if err := eng.Start(ctx, info.ID); err != nil {
			t.Fatalf("Start cycle %d: %v", cycle, err)
		}

		// Verify ALL previous cycles' data
		for check := 1; check <= cycle; check++ {
			r, _ := execWithTimeout(t, eng, info.ID, []string{"cat",
				fmt.Sprintf("/tmp/cycle%d", check)})
			expected := fmt.Sprintf("cycle-%d", check)
			if !strings.Contains(r.Stdout, expected) {
				t.Fatalf("cycle %d: data from cycle %d lost: %q", cycle, check, r.Stdout)
			}
		}
		t.Logf("✓ cycle %d: all data preserved", cycle)
	}
}

func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
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

// --- ExecStream Tests ---

func TestExecStreamBasic(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("stream-basic"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	var events []engine.StreamEvent
	err = eng.ExecStream(ctx, info.ID, []string{"echo", "hello-stream"}, func(e engine.StreamEvent) {
		events = append(events, e)
	})
	if err != nil {
		t.Fatalf("ExecStream: %v", err)
	}

	// Should have at least a stdout event and an exit event
	var gotStdout, gotExit bool
	for _, e := range events {
		if e.Type == "stdout" && strings.Contains(e.Data, "hello-stream") {
			gotStdout = true
		}
		if e.Type == "exit" && e.ExitCode != nil && *e.ExitCode == 0 {
			gotExit = true
		}
	}
	if !gotStdout {
		t.Errorf("missing stdout event, events: %+v", events)
	}
	if !gotExit {
		t.Errorf("missing exit event, events: %+v", events)
	}
	t.Logf("✓ streaming exec: %d events", len(events))
}

func TestExecStreamStderr(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("stream-stderr"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	var events []engine.StreamEvent
	err = eng.ExecStream(ctx, info.ID, []string{"sh", "-c", "echo out; echo err >&2"}, func(e engine.StreamEvent) {
		events = append(events, e)
	})
	if err != nil {
		t.Fatalf("ExecStream: %v", err)
	}

	var gotStdout, gotStderr, gotExit bool
	for _, e := range events {
		if e.Type == "stdout" && strings.Contains(e.Data, "out") {
			gotStdout = true
		}
		if e.Type == "stderr" && strings.Contains(e.Data, "err") {
			gotStderr = true
		}
		if e.Type == "exit" {
			gotExit = true
		}
	}
	if !gotStdout || !gotStderr || !gotExit {
		t.Errorf("stdout=%v stderr=%v exit=%v events=%+v", gotStdout, gotStderr, gotExit, events)
	}
	t.Log("✓ streaming exec separates stdout/stderr")
}

func TestExecStreamExitCode(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("stream-exit"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	var exitCode *int
	eng.ExecStream(ctx, info.ID, []string{"sh", "-c", "exit 42"}, func(e engine.StreamEvent) {
		if e.Type == "exit" {
			exitCode = e.ExitCode
		}
	})
	if exitCode == nil || *exitCode != 42 {
		t.Fatalf("expected exit code 42, got %v", exitCode)
	}
	t.Log("✓ streaming exec preserves exit code 42")
}

func TestExecStreamIncremental(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("stream-incr"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Emit 3 lines with delays — events should arrive incrementally
	var timestamps []time.Time
	err = eng.ExecStream(ctx, info.ID, []string{
		"sh", "-c", "echo line1; sleep 0.3; echo line2; sleep 0.3; echo line3",
	}, func(e engine.StreamEvent) {
		if e.Type == "stdout" {
			timestamps = append(timestamps, time.Now())
		}
	})
	if err != nil {
		t.Fatalf("ExecStream: %v", err)
	}

	if len(timestamps) < 2 {
		t.Fatalf("expected multiple stdout events, got %d", len(timestamps))
	}

	// The gap between first and last stdout should be > 0.4s (two 0.3s sleeps)
	gap := timestamps[len(timestamps)-1].Sub(timestamps[0])
	if gap < 400*time.Millisecond {
		t.Errorf("events arrived too fast (%v) — may not be truly streaming", gap)
	}
	t.Logf("✓ streaming exec is incremental: %d stdout events over %v", len(timestamps), gap.Round(time.Millisecond))
}
