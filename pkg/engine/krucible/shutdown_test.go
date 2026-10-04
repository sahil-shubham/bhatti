// These tests intentionally carry NO `//go:build krucible` tag: they exercise
// pure lifecycle logic with stand-in processes (no libkrun, no hypervisor), so
// they run in the default `make test` on every OS/arch — the portable safety
// net that catches Shutdown/Fork regressions in the plain CI build job, not
// just the KVM/HVF integration lanes.

package krucible

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		if cmd.ProcessState == nil {
			_, _ = cmd.Process.Wait() // only fixtures without their own Cmd.Wait
		}
	})
	execd(t, pid, "sleep", "60")
	return cmd, pid
}

// TestShutdownLeavesHelpersRunning pins what a daemon restart relies on: the
// engine's Shutdown stops no sandbox. A helper the engine spawned and one it
// adopted from the previous daemon both keep running, still recorded, for the
// next daemon to adopt; what the daemon served them — boot config, credential
// broker — is closed. Shutdown used to kill both: every restart rebooted every
// guest.
func TestShutdownLeavesHelpersRunning(t *testing.T) {
	owned, ownedPID := spawnStandin(t)
	done := make(chan error, 1)
	go func() { done <- owned.Wait() }()
	t.Cleanup(func() {
		_ = owned.Process.Kill()
		select { // Shutdown must not have reaped it; if it did, don't wait forever
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	_, adoptedPID := spawnStandin(t)

	socks := shortSockDir(t)
	cfgSock := filepath.Join(socks, "cfg.sock")
	cfgSrv, err := newConfigServer(cfgSock, []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	brokerLn, err := net.Listen("unix", filepath.Join(socks, "b.sock"))
	if err != nil {
		t.Fatal(err)
	}
	inst := &netdInstance{owner: "u:owner", brokerLn: brokerLn}
	e := &Engine{
		vms: map[string]*VM{
			"owned":   {ID: "owned", Status: "running", Thermal: "hot", cmd: owned, waitDone: done, HelperPID: ownedPID, configSrv: cfgSrv},
			"adopted": {ID: "adopted", Status: "running", Thermal: "warm", HelperPID: adoptedPID},
		},
		netds: map[string]*netdInstance{"u:owner": inst},
	}

	e.Shutdown()

	for id, pid := range map[string]int{"owned": ownedPID, "adopted": adoptedPID} {
		vm := e.vms[id]
		if !running(pid, "sleep", "60") {
			t.Errorf("%s helper pid %d killed by Shutdown", id, pid)
		}
		if vm.HelperPID != pid || vm.Status != "running" {
			t.Errorf("%s: Shutdown changed the record: pid %d status %q, want %d running", id, vm.HelperPID, vm.Status, pid)
		}
	}
	select {
	case err := <-done:
		t.Errorf("owned helper exited after Shutdown: %v", err)
		done <- err
	default:
	}
	if e.vms["owned"].configSrv != nil {
		t.Error("boot config server still served after Shutdown")
	}
	if _, err := os.Stat(cfgSock); !os.IsNotExist(err) {
		t.Errorf("boot config socket still there after Shutdown: %v", err)
	}
	if inst.brokerLn != nil {
		t.Error("credential broker socket still served after Shutdown")
	}
}

// TestShutdownStoppedVMIsNoop: a cold/stopped VM (no process) must not panic.
func TestShutdownStoppedVMIsNoop(t *testing.T) {
	e := &Engine{vms: map[string]*VM{}}
	e.vms["cold"] = &VM{ID: "cold", Status: "stopped"}
	e.Shutdown() // must not panic
}

// TestShutdownWaitsForLaunchMu: Shutdown waits out an in-flight lifecycle
// transition (Start/Stop/Pause/Resume hold launchMu), which persists what it
// did, before the daemon exits — and leaves the helper running either way.
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
		t.Fatal("Shutdown returned while a transition held launchMu")
	case <-time.After(200 * time.Millisecond):
		// good: Shutdown is blocked on launchMu
	}

	vm.launchMu.Unlock()
	select {
	case <-done: // Shutdown proceeded once launchMu was free
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not complete after launchMu released")
	}
	if !running(pid, "sleep", "60") {
		t.Fatal("Shutdown killed the helper")
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
