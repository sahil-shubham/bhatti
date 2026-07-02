//go:build krucible

package krucible

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// reapTerminatedWithin waits up to d for pid to terminate, reaping it, and
// reports whether it did. It exists because these tests run BOTH engines in one
// process: the "old daemon" (eng1) that spawned the helper is still our parent,
// so a SIGKILL from the "new daemon" (eng2) leaves a zombie that a bare
// kill(pid,0) liveness probe still reports as alive until it's Wait()ed. In the
// real daemon-restart case the old daemon has exited and init reaps the adopted
// helper, so no zombie survives. We reap explicitly here to assert termination
// truthfully. A still-RUNNING helper (the leak we're guarding against) never
// becomes reapable, so this times out and the test fails — exactly the signal
// we want.
func reapTerminatedWithin(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		var ws syscall.WaitStatus
		wpid, err := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
		if err == syscall.ECHILD {
			return true // already reaped (e.g. owned path Wait()ed it) or gone
		}
		if err == nil && wpid == pid {
			return true // reaped a terminated (signalled/exited) child
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// TestKrucibleShutdownKillsAdoptedHelper is the end-to-end proof of the review
// fix on a REAL VM: a helper adopted across a daemon restart must be killed by
// Shutdown (daemon SIGTERM), not leaked. Sequence: eng1 boots a sandbox (owns
// the helper) → eng2 is built over the same data dir, adopting the LIVE helper
// (cmd == nil, only HelperPID) → eng2.Shutdown() must terminate that adopted
// helper. Before the fix Shutdown skipped adopted helpers entirely.
func TestKrucibleShutdownKillsAdoptedHelper(t *testing.T) {
	dataDir := t.TempDir()
	base := buildBaseRootfs(t, repoRoot(t))
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	eng1 := recoveryEngine(t, dataDir, base)
	info, err := eng1.Create(ctx, engine.SandboxSpec{Name: "shut", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := info.ID
	pid := helperPID(t, eng1, id)
	// Safety net: the helper is detached, so ensure it dies even if the test bails.
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL); syscall.Wait4(pid, nil, syscall.WNOHANG, nil) })

	// Simulate a daemon restart: a fresh engine over the same data dir adopts the
	// still-running helper (it did not spawn it, so vm.cmd == nil).
	eng2 := recoveryEngine(t, dataDir, base)
	if got := helperPID(t, eng2, id); got != pid {
		t.Fatalf("eng2 did not adopt the live helper: pid %d, want %d", got, pid)
	}

	// Daemon SIGTERM path — must terminate the adopted helper.
	eng2.Shutdown()

	if !reapTerminatedWithin(pid, 5*time.Second) {
		t.Fatalf("adopted helper pid %d still running after Shutdown — leaked", pid)
	}
}

// TestKrucibleShutdownKillsOwnedHelper is the same guard for the common case: an
// engine that still owns the helper (no restart) kills it on Shutdown. Here
// kill() reaps via cmd.Wait(), so termination is observable directly.
func TestKrucibleShutdownKillsOwnedHelper(t *testing.T) {
	dataDir := t.TempDir()
	base := buildBaseRootfs(t, repoRoot(t))
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	eng := recoveryEngine(t, dataDir, base)
	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "shut2", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	pid := helperPID(t, eng, info.ID)
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL); syscall.Wait4(pid, nil, syscall.WNOHANG, nil) })

	eng.Shutdown()

	if !reapTerminatedWithin(pid, 5*time.Second) {
		t.Fatalf("owned helper pid %d still running after Shutdown", pid)
	}
}
