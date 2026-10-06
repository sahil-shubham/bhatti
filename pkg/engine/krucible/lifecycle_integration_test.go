//go:build krucible

package krucible

import (
	"context"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/gateway"
)

func awaitHardwareEvent(t *testing.T, ch <-chan engine.LifecycleEvent) engine.LifecycleEvent {
	t.Helper()
	select {
	case event := <-ch:
		return event
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("process death was not reflected within 1.5 seconds")
		return engine.LifecycleEvent{}
	}
}

func TestKrucibleVMMDeathOwnedAndAdopted(t *testing.T) {
	dataDir, sockDir := vmmDir(t), shortSockDir(t)
	base := buildBaseRootfs(t, repoRoot(t))
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	first := recoveryEngine(t, dataDir, sockDir, base)
	info, err := first.Create(ctx, engine.SandboxSpec{Name: "exit-observed", MemoryMB: 512})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = first.Destroy(context.Background(), info.ID) })
	events := make(chan engine.LifecycleEvent, 4)
	first.SetLifecycleHandler(func(event engine.LifecycleEvent) { events <- event })
	if err := syscall.Kill(helperPID(t, first, info.ID), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if event := awaitHardwareEvent(t, events); event.Kind != engine.VMExited || event.Signal != "killed" || event.ExitCode != -1 {
		t.Fatalf("owned helper exit: %+v", event)
	}
	if status, err := first.Status(ctx, info.ID); err != nil || status.Status != "stopped" || first.ThermalState(info.ID) != "cold" {
		t.Fatalf("owned helper status: %+v (%v)", status, err)
	}
	if err := first.Start(ctx, info.ID); err != nil {
		t.Fatalf("cold Start after crash: %v", err)
	}
	if result, err := first.Exec(ctx, info.ID, []string{"echo", "restarted"}); err != nil || !strings.Contains(result.Stdout, "restarted") {
		t.Fatalf("guest did not reboot from disk: %+v (%v)", result, err)
	}

	first.Shutdown() // leave the helper alive, as a daemon restart does
	second := recoveryEngine(t, dataDir, sockDir, base)
	t.Cleanup(func() { _ = second.Destroy(context.Background(), info.ID) })
	if status, err := second.Status(ctx, info.ID); err != nil || status.Status != "running" {
		t.Fatalf("did not adopt live helper: %+v (%v)", status, err)
	}
	adoptedEvents := make(chan engine.LifecycleEvent, 4)
	second.SetLifecycleHandler(func(event engine.LifecycleEvent) { adoptedEvents <- event })
	if err := syscall.Kill(helperPID(t, second, info.ID), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if event := awaitHardwareEvent(t, adoptedEvents); event.Kind != engine.VMExited || event.ExitCode != -1 || event.Signal != "" {
		t.Fatalf("adopted helper exit: %+v", event)
	}
	if status, err := second.Status(ctx, info.ID); err != nil || status.Status != "stopped" {
		t.Fatalf("adopted helper status: %+v (%v)", status, err)
	}
	if err := second.Start(ctx, info.ID); err != nil {
		t.Fatalf("cold Start after adopted exit: %v", err)
	}
}

func TestKrucibleNetdDeathNewGuestHasEgress(t *testing.T) {
	e := newNetEngine(t).(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	spec := engine.SandboxSpec{Name: "netd-victim", UserID: "lost-owner", MemoryMB: 512,
		NetPolicy: &gateway.NetPolicyWire{Default: "public"}}
	first, err := e.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Destroy(context.Background(), first.ID) })
	owner := netdKeyFor(spec, first.ID)
	e.netdMu.Lock()
	inst := e.netds[owner]
	e.netdMu.Unlock()
	inst.mu.Lock()
	oldPID := inst.pid
	inst.mu.Unlock()
	events := make(chan engine.LifecycleEvent, 4)
	e.SetLifecycleHandler(func(event engine.LifecycleEvent) { events <- event })
	if err := syscall.Kill(oldPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if event := awaitHardwareEvent(t, events); event.Kind != engine.NetworkLost || event.EngineID != first.ID ||
		!strings.Contains(event.Reason, "cannot reattach") {
		t.Fatalf("missing disconnection of original guest: %+v", event)
	}
	if e.netdRunning(owner) {
		t.Fatal("gateway still reported running after SIGKILL")
	}
	spec.Name = "netd-after"
	second, err := e.Create(ctx, spec)
	if err != nil {
		t.Fatalf("new guest could not spawn a fresh gateway: %v", err)
	}
	t.Cleanup(func() { _ = e.Destroy(context.Background(), second.ID) })
	inst.mu.Lock()
	newPID := inst.pid
	inst.mu.Unlock()
	if newPID == oldPID || !e.netdRunning(owner) {
		t.Fatalf("gateway not replaced: old=%d new=%d", oldPID, newPID)
	}
	if result, err := e.Exec(ctx, second.ID, []string{"netcheck", "tcp"}); err != nil || result.ExitCode != 0 {
		t.Fatalf("new guest egress after gateway loss: %+v (%v)", result, err)
	}
}
