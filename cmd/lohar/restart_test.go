//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// failedStateTestSetup returns a fresh service-dir + Registry pair with
// all paths sandboxed to tempdirs. The Registry has its own Config (paths)
// and watcher coordination state, so tests don't share package-level
// globals -- which is what made -race flag the watcher reading paths
// while another test was concurrently rewriting them.
//
// Cleanup waits for spawned watcher goroutines to return so the test's
// tempdirs can be cleaned up without an in-flight watcher reading them.
// Without this Wait, t.Cleanup's tempdir removal could race with the
// watcher's path-reading methods.
func failedStateTestSetup(t *testing.T) (svcDir string, reg *Registry) {
	t.Helper()
	svcDir = t.TempDir()
	reg = NewRegistry(Config{
		ServiceDirs: []string{svcDir},
		PidDir:      t.TempDir(),
		LogDir:      t.TempDir(),
	})
	t.Cleanup(reg.WaitForWatchers)
	return svcDir, reg
}

func TestShouldRestart(t *testing.T) {
	// Maps Restart= directive + exit code to a yes/no decision. Mirrors
	// systemd's policy table.
	cases := []struct {
		policy  string
		exit    int
		wantRun bool
	}{
		{"no", 0, false},
		{"no", 1, false},
		{"", 0, false}, // unset = explicit opt-in
		{"", 1, false},
		{"always", 0, true},
		{"always", 1, true},
		{"on-success", 0, true},
		{"on-success", 1, false},
		{"on-failure", 0, false},
		{"on-failure", 1, true},
		{"on-abnormal", 1, false},     // not signal-killed
		{"on-abnormal", 130, true},    // exit > 128 -> killed by signal
		{"on-abnormal", 137, true},    // SIGKILL = 128+9
	}
	for _, c := range cases {
		got := shouldRestart(c.policy, c.exit)
		if got != c.wantRun {
			t.Errorf("shouldRestart(%q, exit=%d) = %v, want %v", c.policy, c.exit, got, c.wantRun)
		}
	}
}

func TestParseRestartSec(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", 100 * time.Millisecond},      // default matches systemd
		{"5", 5 * time.Second},            // bare integer = seconds
		{"500ms", 500 * time.Millisecond}, // Go duration form
		{"2s", 2 * time.Second},
		{"garbage", 100 * time.Millisecond}, // fallback on unparseable
	}
	for _, c := range cases {
		if got := parseRestartSec(c.in); got != c.want {
			t.Errorf("parseRestartSec(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestFailedMarker(t *testing.T) {
	// MarkFailed writes the exit code; IsFailed reads it; ClearFailed
	// removes it. The whole machinery underlying systemctl is-failed.
	dir, reg := failedStateTestSetup(t)
	os.WriteFile(filepath.Join(dir, "test.service"),
		[]byte("[Service]\nExecStart=/bin/true\n"), 0644)

	u, _ := reg.Resolve("test")

	if u.IsFailed() {
		t.Fatal("fresh unit should not be failed")
	}
	u.MarkFailed(42)
	if !u.IsFailed() {
		t.Error("after MarkFailed, IsFailed should be true")
	}
	if got := u.LastExitCode(); got != 42 {
		t.Errorf("LastExitCode = %d, want 42", got)
	}
	u.ClearFailed()
	if u.IsFailed() {
		t.Error("after ClearFailed, IsFailed should be false")
	}
}

func TestRestartOnFailure(t *testing.T) {
	// Real lifecycle test: a service with Restart=on-failure that exits
	// non-zero should be auto-restarted by the watcher. /bin/false exits
	// 1 immediately; StartLimitBurst=2 stops the loop after 2 attempts.
	dir, reg := failedStateTestSetup(t)
	os.WriteFile(filepath.Join(dir, "crasher.service"), []byte(`
[Service]
Type=simple
ExecStart=/bin/false
Restart=on-failure
RestartSec=10ms
StartLimitBurst=2
StartLimitIntervalSec=10
`), 0644)

	u, _ := reg.Resolve("crasher")

	if err := svcStart(u); err != nil {
		t.Fatalf("svcStart: %v", err)
	}

	// Wait for burst limit to be hit. With RestartSec=10ms and burst=2,
	// we need ~20ms of attempts plus the final mark-failed.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		reg.coordMu.Lock()
		count := len(reg.restartBurst[u.Canonical])
		reg.coordMu.Unlock()
		if count >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Give the final attempt time to land.
	time.Sleep(50 * time.Millisecond)

	if !u.IsFailed() {
		t.Error("crasher should be marked failed after the restart loop gives up")
	}
	if u.LastExitCode() != 1 {
		t.Errorf("LastExitCode = %d, want 1 (/bin/false's exit code)", u.LastExitCode())
	}
}

func TestRestartNoSuppressesAutoRestart(t *testing.T) {
	// Restart=no (the default) means a crashing service stays dead.
	// The watcher writes the failed marker but doesn't loop.
	dir, reg := failedStateTestSetup(t)
	os.WriteFile(filepath.Join(dir, "diehard.service"),
		[]byte("[Service]\nType=simple\nExecStart=/bin/false\nRestart=no\n"), 0644)

	u, _ := reg.Resolve("diehard")

	if err := svcStart(u); err != nil {
		t.Fatalf("svcStart: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if u.IsFailed() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !u.IsFailed() {
		t.Fatal("diehard.service should be marked failed after exit")
	}

	// Burst history must be empty -- no restart was attempted.
	reg.coordMu.Lock()
	count := len(reg.restartBurst[u.Canonical])
	reg.coordMu.Unlock()
	if count != 0 {
		t.Errorf("burst history = %d, want 0 (Restart=no should not attempt restart)", count)
	}
}

func TestStopSuppressesRestart(t *testing.T) {
	// Even with Restart=always, an explicit svcStop must NOT trigger a
	// restart -- the admin asked for the service to stop. The watcher
	// reads stopReqs (per-Registry) and bails out.
	dir, reg := failedStateTestSetup(t)
	os.WriteFile(filepath.Join(dir, "loopy.service"),
		[]byte("[Service]\nType=simple\nExecStart=/bin/sleep 30\nRestart=always\nRestartSec=10ms\n"), 0644)

	u, _ := reg.Resolve("loopy")

	if err := svcStart(u); err != nil {
		t.Fatalf("svcStart: %v", err)
	}
	if err := svcStop(u); err != nil {
		t.Fatalf("svcStop: %v", err)
	}

	// Give the watcher time to react (it reads stopReqs after Wait).
	time.Sleep(200 * time.Millisecond)

	if _, err := os.Stat(u.PidPath()); !os.IsNotExist(err) {
		t.Errorf("pidfile reappeared after stop; restart was not suppressed")
	}
	if u.IsFailed() {
		t.Error("svcStop after a Restart=always service should not leave the failed marker")
	}
}

func TestResetFailed(t *testing.T) {
	// `systemctl reset-failed` clears the .failed marker. The marker is
	// on disk, so a fresh Registry resolving the same name sees the same
	// state -- this is what makes the marker visible across systemctl
	// invocations.
	dir, reg := failedStateTestSetup(t)
	os.WriteFile(filepath.Join(dir, "errored.service"),
		[]byte("[Service]\nExecStart=/bin/true\n"), 0644)

	u, _ := reg.Resolve("errored")
	u.MarkFailed(7)
	if !u.IsFailed() {
		t.Fatal("setup: should be failed before reset-failed")
	}

	// Build a separate Registry pointing at the same Config (same paths)
	// to simulate what the IPC server does: each request resolves
	// through whatever Registry is reachable. Since the marker lives on
	// disk under PidDir, both registries see it.
	reg2 := NewRegistry(reg.Config)
	u2, _ := reg2.Resolve("errored")
	u2.ClearFailed()

	if u.IsFailed() {
		t.Error("after ClearFailed, IsFailed should return false (marker is on disk, not in memory)")
	}
}

func TestStatusShowsFailed(t *testing.T) {
	// status should print "Active: failed" with the exit code when the
	// failed marker is present.
	dir, reg := failedStateTestSetup(t)
	os.WriteFile(filepath.Join(dir, "boom.service"),
		[]byte("[Unit]\nDescription=Boom service\n[Service]\nExecStart=/bin/true\n"), 0644)

	u, _ := reg.Resolve("boom")
	u.MarkFailed(99)

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	svcStatus(u, "boom")
	w.Close()
	os.Stdout = old
	buf := make([]byte, 1024)
	n, _ := r.Read(buf)
	got := string(buf[:n])

	if !strings.Contains(got, "Active: failed") {
		t.Errorf("status missing 'Active: failed', got: %q", got)
	}
	if !strings.Contains(got, "code=99") {
		t.Errorf("status missing 'code=99', got: %q", got)
	}
}
