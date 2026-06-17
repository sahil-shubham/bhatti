package krucible

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent"
)

// Recovery makes krucible restart-safe: each sandbox's durable state is written
// to <sandboxDir>/state.json, the helper is detached from the daemon's process
// group (so it survives a daemon restart/crash), and on New() the engine
// rehydrates — adopting helpers that are still alive and marking dead ones cold
// so the next request cold-restores them from the bundle. Pure Go; works on
// macOS and Linux (syscall.Kill / Setpgid are cross-platform).

// vmRecord is the durable, JSON-serialized state of a VM — everything needed to
// reconnect to a live helper or to relaunch a dead one. The runtime-only fields
// (cmd, cancel, Agent, mu) are intentionally excluded.
type vmRecord struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	UserID     string `json:"user_id"`
	SandboxDir string `json:"sandbox_dir"`
	RootfsDir  string `json:"rootfs_dir"`
	SockDir    string `json:"sock_dir"`
	ControlUDS string `json:"control_uds"`
	ForwardUDS string `json:"forward_uds"`
	CtlSockUDS string `json:"ctl_sock_uds"`
	MemMiB     uint32 `json:"mem_mib"`
	Thermal    string `json:"thermal"`
	Status     string `json:"status"`
	Token      string `json:"token"`
	BundleDir  string `json:"bundle_dir"`
	LogPath    string `json:"log_path"`
	BaseSpec   VMSpec `json:"base_spec"`
	HelperPID  int    `json:"helper_pid"`
}

func stateFilePath(sandboxDir string) string { return filepath.Join(sandboxDir, "state.json") }

// toRecordLocked builds the durable record. Caller must hold vm.mu.
func (vm *VM) toRecordLocked() vmRecord {
	return vmRecord{
		ID: vm.ID, Name: vm.Name, UserID: vm.UserID,
		SandboxDir: vm.SandboxDir, RootfsDir: vm.RootfsDir, SockDir: vm.SockDir,
		ControlUDS: vm.ControlUDS, ForwardUDS: vm.ForwardUDS, CtlSockUDS: vm.CtlSockUDS,
		MemMiB: vm.MemMiB, Thermal: vm.Thermal, Status: vm.Status, Token: vm.Token,
		BundleDir: vm.BundleDir, LogPath: vm.logPath, BaseSpec: vm.baseSpec,
		HelperPID: vm.HelperPID,
	}
}

// persist writes the VM's durable state. persistLocked is the same for callers
// already holding vm.mu (e.g. thermal transitions). Atomic (temp + rename) so a
// crash mid-write never leaves a half-written record.
func (vm *VM) persist() {
	vm.mu.Lock()
	rec := vm.toRecordLocked()
	vm.mu.Unlock()
	writeRecord(rec)
}

func (vm *VM) persistLocked() { writeRecord(vm.toRecordLocked()) }

func writeRecord(rec vmRecord) {
	if rec.SandboxDir == "" {
		return
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return
	}
	dst := stateFilePath(rec.SandboxDir)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		slog.Warn("krucible: persist state", "id", rec.ID, "error", err)
		return
	}
	if err := os.Rename(tmp, dst); err != nil {
		slog.Warn("krucible: persist state rename", "id", rec.ID, "error", err)
	}
}

// vmFromRecord reconstructs the in-memory VM from a durable record (runtime
// fields left nil; the caller decides alive/dead and sets Agent/Status).
func vmFromRecord(rec vmRecord) *VM {
	return &VM{
		ID: rec.ID, Name: rec.Name, UserID: rec.UserID,
		SandboxDir: rec.SandboxDir, RootfsDir: rec.RootfsDir, SockDir: rec.SockDir,
		ControlUDS: rec.ControlUDS, ForwardUDS: rec.ForwardUDS, CtlSockUDS: rec.CtlSockUDS,
		MemMiB: rec.MemMiB, Thermal: rec.Thermal, Status: rec.Status, Token: rec.Token,
		BundleDir: rec.BundleDir, baseSpec: rec.BaseSpec, logPath: rec.LogPath,
		HelperPID: rec.HelperPID,
	}
}

// pidAlive reports whether a process exists. signal 0 probes without delivering:
// nil → alive; EPERM → alive but not ours; ESRCH → gone.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// bundleHasCheckpoint reports whether a cold-restore bundle exists.
func bundleHasCheckpoint(bundleDir string) bool {
	if bundleDir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(bundleDir, "checkpoint.bin"))
	return err == nil
}

// classifyRehydrate decides a recovered VM's status/thermal from its liveness
// and whether a cold bundle exists. Pure (no IO) so it is unit-testable on every
// OS/arch without a VM.
func classifyRehydrate(alive, hasBundle bool) (status, thermal string) {
	if alive {
		return "running", "" // thermal kept from the record by the caller
	}
	if hasBundle {
		return "stopped", "cold" // next EnsureHot/Start cold-restores from the bundle
	}
	return "stopped", "" // dead with no bundle — needs an explicit Start (relaunch)
}

// recover scans the data dir for persisted sandboxes and rehydrates them:
// reconnect to live helpers, mark dead ones cold/stopped. Best-effort; a bad
// record is skipped, not fatal.
func (e *Engine) recover() {
	matches, _ := filepath.Glob(filepath.Join(e.cfg.DataDir, "sandboxes", "*", stateFile))
	for _, p := range matches {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var rec vmRecord
		if err := json.Unmarshal(data, &rec); err != nil || rec.ID == "" {
			slog.Warn("krucible recover: bad state file", "path", p, "error", err)
			continue
		}
		vm := vmFromRecord(rec)
		alive := pidAlive(rec.HelperPID) && e.agentResponds(vm)
		status, thermal := classifyRehydrate(alive, bundleHasCheckpoint(rec.BundleDir))
		vm.Status = status
		if alive {
			vm.Agent = agent.NewKrucibleClient(vm.ControlUDS, vm.ForwardUDS, vm.Token)
		} else {
			vm.Thermal = thermal
			vm.HelperPID = 0
		}
		e.mu.Lock()
		e.vms[rec.ID] = vm
		e.mu.Unlock()
		vm.persist() // write back the reconciled status/thermal
		slog.Info("krucible recovered sandbox", "id", rec.ID, "name", rec.Name,
			"alive", alive, "status", vm.Status, "thermal", vm.Thermal)
	}
}

const stateFile = "state.json"

// agentResponds probes a recovered helper's agent with a short timeout — the
// liveness gate beyond a bare pid check (a pid can be alive while the guest is
// wedged).
func (e *Engine) agentResponds(vm *VM) bool {
	if vm.ControlUDS == "" {
		return false
	}
	ag := agent.NewKrucibleClient(vm.ControlUDS, vm.ForwardUDS, vm.Token)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return ag.WaitReady(ctx, 3*time.Second) == nil
}
