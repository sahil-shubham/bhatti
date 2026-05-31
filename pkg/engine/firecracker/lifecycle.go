//go:build linux

package firecracker

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent"
	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
)

// --- Stop (Snapshot) ---

func (e *Engine) Stop(ctx context.Context, id string) error {
	vm, err := e.getVM(id)
	if err != nil {
		return err
	}

	vm.stateMu.Lock()
	defer vm.stateMu.Unlock()

	if vm.Status != "running" {
		return fmt.Errorf("sandbox %q is not running", id)
	}

	// Flush guest page cache before pausing. Pausing vCPUs does NOT
	// flush dirty pages from guest RAM to the virtio-blk device.
	if vm.Thermal == "hot" && vm.Agent != nil {
		syncCtx, syncCancel := context.WithTimeout(context.Background(), 10*time.Second)
		if _, err := vm.Agent.Exec(syncCtx, []string{"sync"}, nil, ""); err != nil {
			slog.Warn("guest sync failed before snapshot",
				"sandbox", id, "error", err)
			// Continue — stale snapshot > no snapshot
		}
		syncCancel()
	}

	client, fcDone := fcAPIClient(vm.SocketPath)
	defer fcDone()

	// Skip Pause if already paused (warm→cold path).
	// Firecracker may reject Pause on an already-paused VM.
	// SaveImage already uses this pattern (see wasPaused above).
	if vm.Thermal != "warm" {
		pauseCtx, pauseCancel := context.WithTimeout(ctx, 5*time.Second)
		defer pauseCancel()
		if err := fcPatch(pauseCtx, client, "/vm", `{"state":"Paused"}`); err != nil {
			return fmt.Errorf("pause: %w", err)
		}
	}

	// Always take Full snapshots.
	sandboxDir := filepath.Dir(vm.RootfsPath)
	vm.SnapMemPath = filepath.Join(sandboxDir, "mem.snap")
	vm.SnapVMPath = filepath.Join(sandboxDir, "vm.snap")

	// In jailed mode, FC can only write inside its chroot. Pass
	// chroot-relative paths to /snapshot/create, then move the files
	// to the sandbox dir after FC exits.
	var snapVMRef, snapMemRef string
	if e.cfg.jailed() && vm.jailRoot != "" {
		snapVMRef = "/vm.snap"
		snapMemRef = "/mem.snap"
	} else {
		snapVMRef = vm.SnapVMPath
		snapMemRef = vm.SnapMemPath
	}

	snapshotType := "Full"
	if err := fcPut(ctx, client, "/snapshot/create", fmt.Sprintf(
		`{"snapshot_type":%q,"snapshot_path":%q,"mem_file_path":%q}`,
		snapshotType, snapVMRef, snapMemRef)); err != nil {
		return fmt.Errorf("create %s snapshot: %w", snapshotType, err)
	}

	// In jailed mode, move snapshot files from chroot to sandbox dir.
	if e.cfg.jailed() && vm.jailRoot != "" {
		for _, name := range []string{"vm.snap", "mem.snap"} {
			src := filepath.Join(vm.jailRoot, name)
			dst := filepath.Join(sandboxDir, name)
			os.Remove(dst)
			if err := os.Rename(src, dst); err != nil {
				// Cross-device? Copy instead.
				if err := copyBlock(src, dst); err != nil {
					return fmt.Errorf("move snapshot %s from chroot: %w", name, err)
				}
			}
		}
	}

	// Lightweight sanity check on snapshot artifacts.
	if err := verifySnapshotArtifacts(vm.SnapVMPath, vm.SnapMemPath, vm.MemSizeMib, snapshotType); err != nil {
		return fmt.Errorf("snapshot sanity check failed: %w", err)
	}

	// hasBaseSnapshot no longer used — all snapshots are Full.

	// Stop VMM process — SIGTERM first, SIGKILL after 3s
	killFC(vm.cmd, 3*time.Second)
	vm.cancel()

	// NOTE: We do NOT destroy the TAP device here. The snapshot contains
	// virtio-net state that references the TAP. On resume, Firecracker
	// expects the same TAP to exist. Destroying and recreating it causes
	// the resumed guest to fail vsock communication.
	// TAP is cleaned up in Destroy().

	vm.Status = "stopped"
	vm.Thermal = "cold"
	return nil
}

// --- Pause (Hot → Warm) ---

// Pause freezes vCPUs. FC process stays alive. Memory stays allocated. ~1ms.
func (e *Engine) Pause(ctx context.Context, id string) error {
	vm, err := e.getVM(id)
	if err != nil {
		return err
	}

	vm.stateMu.Lock()
	defer vm.stateMu.Unlock()

	if vm.Thermal != "hot" {
		return fmt.Errorf("sandbox %q is not hot (thermal=%s)", id, vm.Thermal)
	}
	client, fcDone := fcAPIClient(vm.SocketPath)
	defer fcDone()
	if err := fcPatch(ctx, client, "/vm", `{"state":"Paused"}`); err != nil {
		return fmt.Errorf("pause: %w", err)
	}
	vm.Thermal = "warm"
	return nil
}

// Resume unfreezes vCPUs. Warm → Hot. ~1ms.
func (e *Engine) Resume(ctx context.Context, id string) error {
	vm, err := e.getVM(id)
	if err != nil {
		return err
	}

	vm.stateMu.Lock()
	defer vm.stateMu.Unlock()

	if vm.Thermal != "warm" {
		return fmt.Errorf("sandbox %q is not warm (thermal=%s)", id, vm.Thermal)
	}
	client, fcDone := fcAPIClient(vm.SocketPath)
	defer fcDone()
	if err := fcPatch(ctx, client, "/vm", `{"state":"Resumed"}`); err != nil {
		return fmt.Errorf("resume: %w", err)
	}
	vm.Thermal = "hot"
	return nil
}

// BalloonSet adjusts the virtio balloon to reclaim or release guest memory.
// amountMiB is how much memory the balloon should occupy (0 = fully deflated).
// Only works on warm VMs (vCPUs paused, FC process alive).
func (e *Engine) BalloonSet(ctx context.Context, id string, amountMiB int64) error {
	vm, err := e.getVM(id)
	if err != nil {
		return err
	}
	vm.stateMu.Lock()
	defer vm.stateMu.Unlock()

	if vm.Thermal != "warm" {
		return fmt.Errorf("balloon: sandbox %q is not warm (thermal=%s)", id, vm.Thermal)
	}
	client, fcDone := fcAPIClient(vm.SocketPath)
	defer fcDone()
	return fcPatch(ctx, client, "/balloon",
		fmt.Sprintf(`{"amount_mib":%d}`, amountMiB))
}

// MemSizeMib returns the configured memory size for a VM.
func (e *Engine) MemSizeMib(id string) int64 {
	vm, err := e.getVM(id)
	if err != nil {
		return 0
	}
	return vm.MemSizeMib
}

// ThermalState returns the current thermal state of a VM.
func (e *Engine) ThermalState(id string) string {
	vm, err := e.getVM(id)
	if err != nil {
		return ""
	}
	vm.stateMu.Lock()
	defer vm.stateMu.Unlock()
	return vm.Thermal
}

// Activity queries the guest agent for activity information.
func (e *Engine) Activity(ctx context.Context, id string) (*proto.ActivityInfo, error) {
	vm, err := e.getVM(id)
	if err != nil {
		return nil, err
	}

	vm.stateMu.Lock()
	ag := vm.Agent
	vm.stateMu.Unlock()

	return ag.Activity(ctx)
}

// EnsureHot brings a VM to "hot" state from any thermal state.
// Note: reads Thermal under lock but delegates to Resume/Start which
// acquire the lock themselves — no nested locking.
func (e *Engine) EnsureHot(ctx context.Context, id string) error {
	vm, err := e.getVM(id)
	if err != nil {
		return err
	}

	vm.stateMu.Lock()
	if vm.restoreFailed {
		err := fmt.Errorf("sandbox %q snapshot is corrupt: %s \u2014 "+
			"use 'bhatti start --force' to retry or "+
			"destroy and recreate (volume data is safe)", id, vm.restoreError)
		vm.stateMu.Unlock()
		return err
	}
	thermal := vm.Thermal
	vm.stateMu.Unlock()

	switch thermal {
	case "hot":
		return nil
	case "warm":
		return e.Resume(ctx, id)
	case "cold":
		return e.Start(ctx, id)
	}
	return nil
}

// --- Start (Resume from snapshot, Cold → Hot) ---

func (e *Engine) Start(ctx context.Context, id string) error {
	return e.startVM(ctx, id, false)
}

// StartForce clears a restore-failed circuit breaker and retries.
func (e *Engine) StartForce(ctx context.Context, id string) error {
	return e.startVM(ctx, id, true)
}

func (e *Engine) startVM(ctx context.Context, id string, force bool) error {
	vm, err := e.getVM(id)
	if err != nil {
		return err
	}

	vm.stateMu.Lock()
	defer vm.stateMu.Unlock()

	if force {
		vm.restoreFailed = false
		vm.restoreError = ""
	} else if vm.restoreFailed {
		return fmt.Errorf("sandbox %q snapshot is corrupt: %s \u2014 "+
			"use 'bhatti start --force' to retry or "+
			"destroy and recreate (volume data is safe)", id, vm.restoreError)
	}

	if vm.SnapMemPath == "" {
		return fmt.Errorf("sandbox %q has no snapshot to resume from", id)
	}

	// TAP device is normally kept alive across stop/start. But after a
	// daemon restart (which cleans orphaned TAPs) or external deletion,
	// the TAP may be gone. Recreate it if missing.
	if vm.TapDevice != "" {
		if _, err := net.InterfaceByName(vm.TapDevice); err != nil {
			// TAP is gone — recreate it with the user's bridge.
			e.mu.RLock()
			userNet := e.userNetworks[vm.UserID]
			e.mu.RUnlock()
			if userNet != nil {
				if err := ensureUserBridge(userNet); err != nil {
					return fmt.Errorf("recreate bridge for resume: %w", err)
				}
				if _, err := createTapDevice(id, userNet.BridgeName); err != nil {
					return fmt.Errorf("recreate tap for resume: %w", err)
				}
				slog.Info("recreated TAP for resume", "sandbox", vm.Name, "tap", vm.TapDevice)
			} else {
				slog.Warn("cannot recreate TAP: no user network", "sandbox", vm.Name, "user", vm.UserID)
			}
		}
	}

	// New Firecracker process — always use base path + ".resume" to avoid
	// path growing on repeated stop/start cycles (SUN_LEN overflow).
	baseSockPath := strings.TrimSuffix(vm.SocketPath, ".resume")
	newSocketPath := baseSockPath + ".resume"
	os.Remove(vm.VsockPath)

	// Build jail files for resume — need snapshot files + drives in chroot
	jp := newJailPaths(e.cfg.jailed())
	if e.cfg.jailed() {
		jp.resolve("mem.snap", vm.SnapMemPath)
		jp.resolve("vm.snap", vm.SnapVMPath)
		jp.resolve("rootfs.ext4", vm.RootfsPath)
		configPath := filepath.Join(filepath.Dir(vm.RootfsPath), "config.ext4")
		jp.resolve("config.ext4", configPath)
		for _, vol := range vm.Volumes {
			jp.resolve(fmt.Sprintf("vol-%s.ext4", vol.Name), vol.FilePath)
		}
	}

	fcProc, err := e.startFC(newSocketPath, startFCOpts{
		id: id, vcpus: vm.VcpuCount, memMB: vm.MemSizeMib, files: jp.files,
	})
	if err != nil {
		return err
	}

	// Cleanup on any failure after FC is started
	restoreFailed := func(fmtStr string, args ...interface{}) error {
		killFC(fcProc.cmd, 1*time.Second)
		fcProc.cancel()
		// Remove vsock left by the failed FC instance so the next
		// attempt (via --force) doesn't hit "Address in use".
		os.Remove(vm.VsockPath)
		errMsg := fmt.Sprintf(fmtStr, args...)
		vm.restoreFailed = true
		vm.restoreError = errMsg
		if stderr := fcProc.stderrBuf.String(); stderr != "" {
			return fmt.Errorf("%s\nFC stderr: %s", errMsg, stderr)
		}
		return fmt.Errorf("%s", errMsg)
	}

	apiSocket := newSocketPath
	if fcProc.socket != "" {
		apiSocket = fcProc.socket
	}
	client, fcDone := fcAPIClient(apiSocket)
	defer fcDone()

	// In bare mode, if this VM was resumed from a snapshot, vm.snap has the
	// original sandbox's paths baked in (FCPathOrigin). Create symlinks so
	// they resolve to our files. In jailed mode, the chroot handles this
	// — all paths are chroot-relative.
	var symlinkDir string
	if !e.cfg.jailed() && vm.FCPathOrigin != "" && vm.FCPathOrigin != vm.ID {
		origDir := filepath.Join(e.cfg.DataDir, "sandboxes", vm.FCPathOrigin)
		curDir := filepath.Dir(vm.RootfsPath)
		if origDir != curDir {
			os.MkdirAll(origDir, 0700)
			symlinkDir = origDir
			// Symlink all files FC might reference
			for _, name := range []string{"rootfs.ext4", "config.ext4", "mem.snap", "vm.snap"} {
				src := filepath.Join(curDir, name)
				dst := filepath.Join(origDir, name)
				os.Remove(dst)
				os.Symlink(src, dst)
			}
			// Volume files
			for _, vol := range vm.Volumes {
				snapFile := fmt.Sprintf("vol-%s.ext4", vol.Name)
				src := filepath.Join(curDir, snapFile)
				dst := filepath.Join(origDir, snapFile)
				os.Remove(dst)
				if _, err := os.Stat(src); err == nil {
					os.Symlink(src, dst)
				}
			}
			// Vsock must NOT exist — FC needs to create it fresh
			os.Remove(filepath.Join(origDir, "vsock.sock"))
		}
	}

	// In jailed mode, paths are chroot-relative. In bare mode, use host paths.
	snapVMRef := vm.SnapVMPath
	snapMemRef := vm.SnapMemPath
	if e.cfg.jailed() {
		snapVMRef = "/vm.snap"
		snapMemRef = "/mem.snap"
	}
	// network_overrides remaps the TAP device name so FC doesn't need the
	// exact TAP name from when the snapshot was taken.
	if err := fcPut(ctx, client, "/snapshot/load", fmt.Sprintf(
		`{"snapshot_path":%q,"mem_backend":{"backend_path":%q,"backend_type":"File"},"resume_vm":true,"network_overrides":[{"iface_id":"eth0","host_dev_name":%q}]}`,
		snapVMRef, snapMemRef, vm.TapDevice)); err != nil {
		if symlinkDir != "" {
			os.RemoveAll(symlinkDir)
		}
		return restoreFailed("load snapshot: %v", err)
	}

	// Clean up symlinks — FC has opened the file descriptors
	if symlinkDir != "" {
		os.RemoveAll(symlinkDir)
	}

	vm.SocketPath = apiSocket
	vm.cmd = fcProc.cmd
	vm.cancel = fcProc.cancel
	vm.stderrBuf = fcProc.stderrBuf
	vm.jailRoot = fcProc.jailRoot
	vm.Status = "running"
	vm.Thermal = "hot"

	// Use TCP client for post-resume — vsock is broken after snapshot/restore
	// but virtio-net (TCP over TAP) survives.
	vm.Agent = agent.NewTCPClientWithAuth(vm.GuestIP, vm.Token)

	// Wait for agent to be responsive after resume.
	if err := vm.Agent.WaitReady(ctx, 30*time.Second); err != nil {
		return restoreFailed("agent not ready after resume: %v", err)
	}

	// Restore succeeded — clear any previous failure state
	vm.restoreFailed = false
	vm.restoreError = ""

	return nil
}

// --- Destroy ---

func (e *Engine) Destroy(ctx context.Context, id string) error {
	vm, err := e.getVM(id)
	if err != nil {
		return err
	}

	vm.stateMu.Lock()

	if vm.Status == "running" && vm.cmd != nil {
		killFC(vm.cmd, 1*time.Second)
		vm.cancel()
	}

	// Always clean up TAP and release IP — whether running or stopped.
	if vm.TapDevice != "" {
		destroyTapDevice(vm.TapDevice)
	}

	// Release IP back to the user's pool and clean up the permanent ARP
	// entry that Create set for fast boot (nud permanent). Without this,
	// entries accumulate on the bridge until it's destroyed.
	userID := vm.UserID
	if vm.GuestIP != "" {
		e.mu.RLock()
		if net, ok := e.userNetworks[userID]; ok {
			net.Pool.Release(vm.GuestIP)
			exec.Command("ip", "neigh", "del", vm.GuestIP,
				"dev", net.BridgeName).Run() // best-effort
		}
		e.mu.RUnlock()
	}

	rootfsDir := filepath.Dir(vm.RootfsPath)

	// Remove from map FIRST — prevents getVM() from finding a VM
	// whose files are being deleted. Must happen while stateMu is
	// held so no concurrent operation is mid-flight on this VM.
	e.mu.Lock()
	delete(e.vms, id)
	e.mu.Unlock()

	vm.stateMu.Unlock()

	// Now safe to delete files — no other goroutine can reach this VM.
	os.RemoveAll(rootfsDir)

	// Clean up jailer chroot and cgroup if jailed
	if e.cfg.jailed() {
		jailDir := filepath.Join(e.cfg.DataDir, "jails", "firecracker", id)
		os.RemoveAll(jailDir)
	}

	// If this was the user's last VM, destroy their bridge
	if userID != "" {
		e.removeUserNetworkIfEmpty(userID)
	}

	return nil
}
