// These tests intentionally carry NO `//go:build krucible` tag: they exercise
// pure lifecycle/kill logic with a stand-in `sleep` process (no libkrun, no
// hypervisor), so they run in the default `make test` on every OS/arch — the
// portable safety net that catches Shutdown/Fork regressions in the plain CI
// build job, not just the KVM/HVF integration lanes.

package krucible

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// spawnStandin starts a long-lived child process that stands in for a live
// bhatti-vmm helper, returning its *exec.Cmd + pid. Cleanup kills it (best
// effort) so a failed assertion never leaks a `sleep`.
func spawnStandin(t *testing.T) (*exec.Cmd, int) {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn stand-in process (%v); skipping", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	return cmd, pid
}

// TestShutdownKillsAdoptedHelper is the regression test for the leak found in
// review: Shutdown must kill a helper ADOPTED across a prior daemon restart
// (only HelperPID set, cmd == nil), not just ones this engine owns. The old
// cmd-only inline kill left adopted helpers running — orphaned live VMs whose
// backing files the daemon would later delete.
func TestShutdownKillsAdoptedHelper(t *testing.T) {
	cmd, pid := spawnStandin(t)
	e := &Engine{vms: map[string]*VM{}}
	// Adopted shape: running, but no owned Cmd handle — only the persisted pid.
	e.vms["adopted"] = &VM{ID: "adopted", Status: "running", HelperPID: pid}

	e.Shutdown()

	if e.vms["adopted"].HelperPID != 0 {
		t.Errorf("HelperPID not cleared after Shutdown: %d", e.vms["adopted"].HelperPID)
	}
	// Reap with a bounded wait so the OLD (leaking) behavior fails fast rather
	// than blocking for the full sleep.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		if code := cmd.ProcessState.ExitCode(); code != -1 {
			t.Errorf("adopted helper exited normally (code %d) — Shutdown leaked it", code)
		}
	case <-time.After(3 * time.Second):
		t.Errorf("adopted helper still alive 3s after Shutdown — leaked (regression)")
	}
}

// TestShutdownKillsOwnedHelper guards the owned path (vm.cmd set): it must keep
// working after the adopted-path fix.
func TestShutdownKillsOwnedHelper(t *testing.T) {
	cmd, pid := spawnStandin(t)
	e := &Engine{vms: map[string]*VM{}}
	e.vms["owned"] = &VM{ID: "owned", Status: "running", cmd: cmd}

	e.Shutdown() // vm.kill() reaps the owned process synchronously

	if pidAlive(pid) {
		t.Errorf("owned helper pid %d still alive after Shutdown", pid)
	}
	if e.vms["owned"].cmd != nil {
		t.Errorf("vm.cmd not cleared after Shutdown")
	}
}

// TestShutdownStoppedVMIsNoop: a cold/stopped VM (no process) must not panic.
func TestShutdownStoppedVMIsNoop(t *testing.T) {
	e := &Engine{vms: map[string]*VM{}}
	e.vms["cold"] = &VM{ID: "cold", Status: "stopped"}
	e.Shutdown() // must not panic
}

// TestShutdownWaitsForLaunchMu proves the race fix: Shutdown must serialize
// against an in-flight Start/Stop/Pause/Resume (which hold launchMu, not mu), so
// it can't SIGKILL a helper mid-transition. We hold launchMu and assert Shutdown
// blocks until it's released. The OLD Shutdown never touched launchMu, so it
// would kill immediately — this test fails on that behavior.
func TestShutdownWaitsForLaunchMu(t *testing.T) {
	_, pid := spawnStandin(t)
	e := &Engine{vms: map[string]*VM{}}
	vm := &VM{ID: "x", Status: "running", HelperPID: pid}
	e.vms["x"] = vm

	vm.launchMu.Lock() // simulate an in-flight transition
	done := make(chan struct{})
	go func() { e.Shutdown(); close(done) }()

	select {
	case <-done:
		vm.launchMu.Unlock()
		t.Fatal("Shutdown killed the VM without acquiring launchMu (raced an in-flight transition)")
	case <-time.After(200 * time.Millisecond):
		// good: Shutdown is blocked on launchMu
	}
	if !pidAlive(pid) {
		vm.launchMu.Unlock()
		t.Fatal("helper killed while launchMu was held")
	}

	vm.launchMu.Unlock()
	select {
	case <-done: // Shutdown proceeded once launchMu was free
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not complete after launchMu released")
	}
}

// TestForkMountRefusedFast pins the friendlier pre-check: forking a --mount
// sandbox is refused up front (virtio-fs can't be memory-restored), immediately
// and without spawning/pausing anything — so it needs no hypervisor and the
// error names the supported path. Complements the end-to-end integration test
// TestKrucibleForkMountedRefused.
func TestForkMountRefusedFast(t *testing.T) {
	e := &Engine{vms: map[string]*VM{}, cfg: Config{DataDir: t.TempDir()}}
	e.vms["m"] = &VM{
		ID: "m", Status: "running",
		baseSpec: VMSpec{Mounts: []VMFsMount{{Tag: "mnt0", HostPath: "/tmp"}}},
	}

	start := time.Now()
	_, err := e.Fork(context.Background(), "m", "fork")
	if err == nil {
		t.Fatal("Fork of a --mount sandbox should be refused, got success")
	}
	if !strings.Contains(err.Error(), "--mount") {
		t.Errorf("Fork error should name --mount as the cause; got: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Fork refusal should be immediate, took %s", d)
	}
}

// TestGenerateIDAndTokenUnique guards the RNG helpers: correct length + unique
// across calls (the ignored rand.Read error is now handled).
func TestGenerateIDAndTokenUnique(t *testing.T) {
	seenIDs, seenToks := map[string]bool{}, map[string]bool{}
	for i := 0; i < 100; i++ {
		id, err := generateID()
		if err != nil {
			t.Fatalf("generateID: %v", err)
		}
		if len(id) != 16 { // 8 random bytes -> 16 hex chars
			t.Fatalf("id %q len %d, want 16", id, len(id))
		}
		if seenIDs[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seenIDs[id] = true

		tok, err := genToken()
		if err != nil {
			t.Fatalf("genToken: %v", err)
		}
		if len(tok) != 32 { // 16 random bytes -> 32 hex chars
			t.Fatalf("token %q len %d, want 32", tok, len(tok))
		}
		if seenToks[tok] {
			t.Fatalf("duplicate token %q", tok)
		}
		seenToks[tok] = true
	}
}
