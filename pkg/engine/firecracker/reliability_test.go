//go:build linux

package firecracker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- Phase 1.3: Circuit breaker blocks retries ---

func TestCircuitBreakerBlocksRetry(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("cb-test"))
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Stop to get a snapshot
	if err := eng.Stop(ctx, info.ID); err != nil {
		t.Fatal(err)
	}

	// Corrupt the snapshot
	vmSnapPath := filepath.Join(eng.cfg.DataDir, "sandboxes", info.ID, "vm.snap")
	os.WriteFile(vmSnapPath, []byte("corrupt"), 0644)

	// First Start should fail and set circuit breaker
	err = eng.Start(ctx, info.ID)
	if err == nil {
		t.Fatal("expected Start to fail on corrupt snapshot")
	}

	// Second Start should fail immediately (circuit breaker) without spawning FC
	start := time.Now()
	err = eng.Start(ctx, info.ID)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected circuit breaker to block retry")
	}
	if !strings.Contains(err.Error(), "corrupt") {
		t.Errorf("expected 'corrupt' in error, got: %v", err)
	}
	// Circuit breaker should return instantly, not wait 30s for agent
	if elapsed > 1*time.Second {
		t.Errorf("circuit breaker took %v, expected instant", elapsed)
	}
}

// --- Phase 1.3: StartForce clears circuit breaker ---

func TestStartForceClearsCircuitBreaker(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("force-test"))
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Destroy(ctx, info.ID)

	if err := eng.Stop(ctx, info.ID); err != nil {
		t.Fatal(err)
	}

	// Corrupt, trigger circuit breaker
	vmSnapPath := filepath.Join(eng.cfg.DataDir, "sandboxes", info.ID, "vm.snap")
	origData, _ := os.ReadFile(vmSnapPath)
	os.WriteFile(vmSnapPath, []byte("corrupt"), 0644)

	eng.Start(ctx, info.ID) // fails, sets circuit breaker

	// Restore valid snapshot
	os.WriteFile(vmSnapPath, origData, 0644)

	// StartForce should clear the breaker and succeed
	if err := eng.StartForce(ctx, info.ID); err != nil {
		t.Fatalf("StartForce should succeed after restoring snapshot: %v", err)
	}

	// Exec should work
	r, err := execWithTimeout(t, eng, info.ID, []string{"echo", "force-ok"})
	if err != nil || r.ExitCode != 0 {
		t.Fatalf("exec after force start: err=%v exit=%d", err, r.ExitCode)
	}
	if !strings.Contains(r.Stdout, "force-ok") {
		t.Errorf("expected 'force-ok', got %q", r.Stdout)
	}
}

// --- Phase 1.4: FC stderr captured on failure ---

func TestFCStderrCapturedOnFailure(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("stderr-test"))
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Destroy(ctx, info.ID)

	if err := eng.Stop(ctx, info.ID); err != nil {
		t.Fatal(err)
	}

	// Corrupt snapshot to make FC fail
	vmSnapPath := filepath.Join(eng.cfg.DataDir, "sandboxes", info.ID, "vm.snap")
	os.WriteFile(vmSnapPath, []byte("corrupt"), 0644)

	err = eng.Start(ctx, info.ID)
	if err == nil {
		t.Fatal("expected failure")
	}

	// FC may return a 400 with the error in the response body (for parse
	// errors) or crash with stderr (for panics). Either way, the error
	// message should contain diagnostic information.
	errMsg := err.Error()
	hasInfo := strings.Contains(errMsg, "FC stderr:") ||
		strings.Contains(errMsg, "snapshot") ||
		strings.Contains(errMsg, "load snapshot")
	if !hasInfo {
		t.Errorf("expected diagnostic info in error, got: %v", err)
	}
}

// --- Phase 2.2: Sync before snapshot ---

func TestSyncBeforeSnapshot(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("sync-test"))
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Write a file
	execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "echo sync-data > /tmp/sync-test"})

	// Stop (which runs sync + snapshot)
	if err := eng.Stop(ctx, info.ID); err != nil {
		t.Fatal(err)
	}

	// Resume and verify data persisted
	if err := eng.Start(ctx, info.ID); err != nil {
		t.Fatal(err)
	}

	r, err := execWithTimeout(t, eng, info.ID, []string{"cat", "/tmp/sync-test"})
	if err != nil || r.ExitCode != 0 {
		t.Fatalf("read after resume: err=%v exit=%d", err, r.ExitCode)
	}
	if !strings.Contains(r.Stdout, "sync-data") {
		t.Errorf("expected 'sync-data', got %q", r.Stdout)
	}
}

// --- Phase 3.1: Serial console disabled ---

func TestSerialConsoleDisabled(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("serial-test"))
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Destroy(ctx, info.ID)

	r, err := execWithTimeout(t, eng, info.ID, []string{"cat", "/proc/cmdline"})
	if err != nil || r.ExitCode != 0 {
		t.Fatalf("cat cmdline: err=%v exit=%d", err, r.ExitCode)
	}
	if !strings.Contains(r.Stdout, "8250.nr_uarts=0") {
		t.Errorf("expected 8250.nr_uarts=0 in cmdline, got: %s", r.Stdout)
	}
	if strings.Contains(r.Stdout, "console=ttyS0") {
		t.Errorf("console=ttyS0 should be removed, got: %s", r.Stdout)
	}
}

// --- Phase 3.4: Entropy device ---

func TestEntropyDevicePresent(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("entropy-test"))
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Destroy(ctx, info.ID)

	r, err := execWithTimeout(t, eng, info.ID, []string{"ls", "/dev/hwrng"})
	if err != nil || r.ExitCode != 0 {
		t.Errorf("expected /dev/hwrng to exist: err=%v exit=%d stderr=%s", err, r.ExitCode, r.Stderr)
	}
}

// --- Phase 3.5: network_overrides survive stop/start ---

func TestNetworkOverridesStopStart(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("netoverride-test"))
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Verify network works
	r, err := execWithTimeout(t, eng, info.ID, []string{"echo", "before-stop"})
	if err != nil || !strings.Contains(r.Stdout, "before-stop") {
		t.Fatalf("pre-stop exec failed: err=%v out=%q", err, r.Stdout)
	}

	// Stop and start (uses network_overrides on resume)
	if err := eng.Stop(ctx, info.ID); err != nil {
		t.Fatal(err)
	}
	if err := eng.Start(ctx, info.ID); err != nil {
		t.Fatal(err)
	}

	// Verify network still works after resume
	r, err = execWithTimeout(t, eng, info.ID, []string{"echo", "after-resume"})
	if err != nil || !strings.Contains(r.Stdout, "after-resume") {
		t.Fatalf("post-resume exec failed: err=%v out=%q", err, r.Stdout)
	}
}

// --- Phase 4.3/4.4: FC logger and metrics files created ---

func TestFCLoggerAndMetrics(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("logger-test"))
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Destroy(ctx, info.ID)

	sandboxDir := filepath.Join(eng.cfg.DataDir, "sandboxes", info.ID)
	logPath := filepath.Join(sandboxDir, "firecracker.log")
	metricsPath := filepath.Join(sandboxDir, "firecracker.metrics")

	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("firecracker.log not created: %v", err)
	}
	if _, err := os.Stat(metricsPath); err != nil {
		t.Errorf("firecracker.metrics not created: %v", err)
	}
}

// --- Phase 5.2: Socket path validation ---

func TestSocketPathValidation(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	// Normal create should work
	info, err := eng.Create(ctx, testSpec("socket-ok"))
	if err != nil {
		t.Fatal(err)
	}
	eng.Destroy(ctx, info.ID)

	// Socket path validation is checked inside startFC, which is called
	// by Create. With a normal DataDir the path is well under 108 bytes.
	// We can't easily test the failure path without changing DataDir to
	// something very long, which would break the whole engine. So just
	// verify the function directly.
	if err := validateSocketPath("/short/path.sock"); err != nil {
		t.Errorf("short path should be valid: %v", err)
	}
	longPath := "/" + strings.Repeat("a", 110) + ".sock"
	if err := validateSocketPath(longPath); err == nil {
		t.Error("long path should fail validation")
	}
}

// --- Diff snapshots disabled: all snapshots are Full ---

func TestAllSnapshotsAreFull(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("full-snap"))
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Destroy(ctx, info.ID)

	// First stop — Full snapshot
	if err := eng.Stop(ctx, info.ID); err != nil {
		t.Fatal(err)
	}

	memPath := filepath.Join(eng.cfg.DataDir, "sandboxes", info.ID, "mem.snap")
	fi, err := os.Stat(memPath)
	if err != nil {
		t.Fatal(err)
	}

	// For a 512MB VM, Full snapshot mem.snap should be exactly 512MB
	// (the testSpec default). Allow some tolerance for VM overhead.
	expectedMin := int64(500 * 1024 * 1024)
	if fi.Size() < expectedMin {
		t.Errorf("mem.snap is %d bytes, expected >= %d (Full snapshot)", fi.Size(), expectedMin)
	}

	// Start, stop again — should still be Full (not Diff)
	if err := eng.Start(ctx, info.ID); err != nil {
		t.Fatal(err)
	}
	if err := eng.Stop(ctx, info.ID); err != nil {
		t.Fatal(err)
	}

	fi2, _ := os.Stat(memPath)
	if fi2.Size() < expectedMin {
		t.Errorf("second snapshot mem.snap is %d bytes, expected >= %d (should be Full, not Diff)",
			fi2.Size(), expectedMin)
	}
}

// --- Vsock cleaned up on restore failure ---

func TestVsockCleanedUpOnRestoreFailure(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("vsock-cleanup"))
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Destroy(ctx, info.ID)

	if err := eng.Stop(ctx, info.ID); err != nil {
		t.Fatal(err)
	}

	// Corrupt snapshot
	vmSnapPath := filepath.Join(eng.cfg.DataDir, "sandboxes", info.ID, "vm.snap")
	os.WriteFile(vmSnapPath, []byte("corrupt"), 0644)

	// Start fails
	eng.Start(ctx, info.ID)

	// Vsock should be cleaned up
	vsockPath := filepath.Join(eng.cfg.DataDir, "sandboxes", info.ID, "vsock.sock")
	if _, err := os.Stat(vsockPath); err == nil {
		t.Error("vsock.sock should be removed after failed restore")
	}
}

// --- FCPathOrigin symlinks in Start() ---

func TestFCPathOriginSymlinksOnStart(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	// Create a sandbox and checkpoint it
	info, err := eng.Create(ctx, testSpec("origin-test"))
	if err != nil {
		t.Fatal(err)
	}

	execWithTimeout(t, eng, info.ID, []string{"echo", "checkpoint-data"})

	// Checkpoint creates a named snapshot
	snapDir := filepath.Join(eng.cfg.DataDir, "snapshots", "test")
	os.MkdirAll(snapDir, 0700)
	manifest, err := eng.Checkpoint(ctx, info.ID, "usr_test", 99, "origin-snap", snapDir)
	if err != nil {
		eng.Destroy(ctx, info.ID)
		t.Fatal(err)
	}
	eng.Destroy(ctx, info.ID)

	// Resume from snapshot — creates new sandbox with different ID
	m := manifest.(*SnapshotManifest)
	snapPath := filepath.Join(snapDir, "origin-snap")
	resumed, err := eng.ResumeSnapshot(ctx, snapPath, m, "origin-resumed")
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Destroy(ctx, resumed.ID)

	// Verify it works
	r, err := execWithTimeout(t, eng, resumed.ID, []string{"echo", "resumed-ok"})
	if err != nil || !strings.Contains(r.Stdout, "resumed-ok") {
		t.Fatalf("exec after resume: err=%v out=%q", err, r.Stdout)
	}

	// Stop and start — this is where symlinks are needed
	if err := eng.Stop(ctx, resumed.ID); err != nil {
		t.Fatal(err)
	}
	if err := eng.Start(ctx, resumed.ID); err != nil {
		t.Fatalf("Start after stop should recreate symlinks: %v", err)
	}

	// Verify exec works after stop/start cycle
	r, err = execWithTimeout(t, eng, resumed.ID, []string{"echo", "restart-ok"})
	if err != nil || !strings.Contains(r.Stdout, "restart-ok") {
		t.Fatalf("exec after restart: err=%v out=%q", err, r.Stdout)
	}

	// Clean up snapshot
	os.RemoveAll(snapDir)
}

// execWithTimeout is defined in perf_test.go
