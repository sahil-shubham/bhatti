package krucible

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
)

// This file implements pkg/server.ThermalEngine on top of bhatti-vmm's control
// socket (PAUSE/RESUME/STATUS), the warm tier. Stop powers off the cold tier.
//
// Memory model: libkrun maps guest RAM MAP_PRIVATE|MAP_ANONYMOUS (lazy commit),
// so a paused VM's host RSS only counts touched pages. No balloon is needed.

// Pause syncs the running guest before parking its vCPUs. Stop always resumes
// and syncs again: a writer can dirty the disk after this pre-pause sync.
func (e *Engine) Pause(ctx context.Context, id string) error {
	return e.pause(ctx, id, true)
}

// ForcePause is used when the guest agent has stopped answering Activity. It
// never attempts a guest sync and leaves the next Stop responsible for one.
func (e *Engine) ForcePause(ctx context.Context, id string) error {
	return e.pause(ctx, id, false)
}

func (e *Engine) pause(ctx context.Context, id string, syncGuest bool) error {
	if !e.caps.Pause {
		return errNoPause
	}
	vm, err := e.getVM(id)
	if err != nil {
		return err
	}
	vm.launchMu.Lock()
	defer vm.launchMu.Unlock()
	vm.mu.Lock()
	if vm.Status != "running" {
		status := vm.Status
		vm.mu.Unlock()
		return fmt.Errorf("sandbox %q is not running (status=%s)", id, status)
	}
	if vm.Thermal == "warm" {
		vm.mu.Unlock()
		return nil
	}
	vm.mu.Unlock()

	if syncGuest {
		syncCtx, cancel := context.WithTimeout(ctx, guestSyncTimeout)
		err := vm.syncGuest(syncCtx)
		cancel()
		if err != nil {
			slog.Warn("krucible pause: guest sync failed; final stop will retry", "id", id, "error", err)
		}
	}
	// launchMu keeps the transition serialized while vm.mu stays available
	// to Status/List throughout a slow or wedged control-socket round-trip.
	if _, err := controlCmd(ctx, vm.CtlSockUDS, "PAUSE"); err != nil {
		return fmt.Errorf("pause: %w", err)
	}
	vm.mu.Lock()
	vm.Thermal = "warm"
	vm.mu.Unlock()
	vm.persist()
	return nil
}

// Resume: warm → hot. Exposed both as Resume (krucible-native) and via
// EnsureHot (the server's wake-on-request entry point).
func (e *Engine) Resume(ctx context.Context, id string) error {
	vm, err := e.getVM(id)
	if err != nil {
		return err
	}
	vm.launchMu.Lock()
	defer vm.launchMu.Unlock()
	vm.mu.Lock()
	if vm.Thermal == "hot" {
		vm.mu.Unlock()
		return nil
	}
	vm.mu.Unlock()
	if _, err := controlCmd(ctx, vm.CtlSockUDS, "RESUME"); err != nil {
		return fmt.Errorf("resume: %w", err)
	}
	vm.mu.Lock()
	vm.Thermal = "hot"
	ag, unasked := vm.Agent, errors.Is(vm.AgentInfoErr, errAgentInfoPaused)
	vm.mu.Unlock()
	vm.persist()
	if unasked { // adopted paused: its guest couldn't answer until now
		info, err := queryAgentInfo(ctx, ag)
		if err != nil {
			slog.Warn("krucible.agent.info", "id", id, "error", err)
		}
		vm.mu.Lock()
		vm.AgentInfo, vm.AgentInfoErr = info, err
		vm.mu.Unlock()
	}
	return nil
}

// EnsureHot is the canonical wake path used by the server's thermal manager
// (the public proxy calls it on every incoming request). It is tier-aware:
//   - hot:  no-op
//   - warm: RESUME over the control socket (helper alive, vCPUs paused)
//   - cold: Start (boot from the persisted root disk); the helper was killed
//     at Stop, so a socket RESUME would fail.
//
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
