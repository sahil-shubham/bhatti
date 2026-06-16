package krucible

import (
	"context"
	"fmt"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
)

// This file implements pkg/server.ThermalEngine on top of the libkrun control
// socket (PAUSE/RESUME/STATUS). P2 scope = warm tier (hot↔warm). Cold lands in
// P3 (snapshot-to-disk via a separate control verb).
//
// Memory model: libkrun maps guest RAM MAP_PRIVATE|MAP_ANONYMOUS (lazy commit),
// so a paused VM's host RSS already only counts touched pages — we don't need
// the FC balloon-inflate trick for the warm tier. BalloonSet is therefore a
// no-op on krucible; it stays on the interface for FC compatibility.

// Pause: hot → warm. Idempotent on warm.
func (e *Engine) Pause(ctx context.Context, id string) error {
	vm, err := e.getVM(id)
	if err != nil {
		return err
	}
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if vm.Status != "running" {
		return fmt.Errorf("sandbox %q is not running (status=%s)", id, vm.Status)
	}
	if vm.Thermal == "warm" {
		return nil
	}
	if _, err := controlCmd(ctx, vm.CtlSockUDS, "PAUSE"); err != nil {
		return fmt.Errorf("pause: %w", err)
	}
	vm.Thermal = "warm"
	return nil
}

// Resume: warm → hot. Exposed both as Resume (krucible-native) and via
// EnsureHot (the server's wake-on-request entry point).
func (e *Engine) Resume(ctx context.Context, id string) error {
	vm, err := e.getVM(id)
	if err != nil {
		return err
	}
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if vm.Thermal == "hot" {
		return nil
	}
	if _, err := controlCmd(ctx, vm.CtlSockUDS, "RESUME"); err != nil {
		return fmt.Errorf("resume: %w", err)
	}
	vm.Thermal = "hot"
	return nil
}

// EnsureHot is the canonical wake path used by the server's thermal manager
// (the public proxy calls it on every incoming request). It is tier-aware:
//   - hot:  no-op
//   - warm: RESUME over the control socket (helper alive, vCPUs paused)
//   - cold: Start (re-launch the helper + restore the snapshot bundle) — the
//     helper was killed at Stop, so a socket RESUME would fail.
// This lets a single wake-on-request transparently revive both warm and cold
// sandboxes.
func (e *Engine) EnsureHot(ctx context.Context, id string) error {
	vm, err := e.getVM(id)
	if err != nil {
		return err
	}
	vm.mu.Lock()
	thermal, status := vm.Thermal, vm.Status
	vm.mu.Unlock()
	switch {
	case thermal == "hot" && status == "running":
		return nil
	case thermal == "cold" || status == "stopped":
		return e.Start(ctx, id)
	default:
		return e.Resume(ctx, id)
	}
}

// ThermalState returns "hot" | "warm" | "cold" mirrored from local state. (We trust the
// state machine in Pause/Resume rather than round-tripping STATUS over the UDS
// on every call — every server.ListSandboxes call would otherwise hit the UDS.)
func (e *Engine) ThermalState(id string) string {
	vm, err := e.getVM(id)
	if err != nil {
		return ""
	}
	vm.mu.Lock()
	defer vm.mu.Unlock()
	return vm.Thermal
}

// Activity delegates to the lohar agent (last-activity timestamp + session
// counts — used by the thermal manager to decide when to pause an idle VM).
func (e *Engine) Activity(ctx context.Context, id string) (*proto.ActivityInfo, error) {
	ag, err := e.agentFor(id)
	if err != nil {
		return nil, err
	}
	return ag.Activity(ctx)
}

// BalloonSet is a no-op on krucible (see file header). Returning nil keeps the
// server's thermal manager happy without a special-case for the engine kind.
func (e *Engine) BalloonSet(ctx context.Context, id string, amountMiB int64) error {
	return nil
}

// MemSizeMib returns the boot-configured RAM size, which is also the ceiling.
// Used by the thermal manager when sizing the balloon target on FC; here it's
// for parity / reporting.
func (e *Engine) MemSizeMib(id string) int64 {
	vm, err := e.getVM(id)
	if err != nil {
		return 0
	}
	vm.mu.Lock()
	defer vm.mu.Unlock()
	return int64(vm.MemMiB)
}
