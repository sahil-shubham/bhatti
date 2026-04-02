//go:build linux

package firecracker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent"
	"github.com/sahil-shubham/bhatti/pkg/engine"
	"golang.org/x/sync/errgroup"
)

// SnapshotManifest records everything needed to resume a VM from a named snapshot.
type SnapshotManifest struct {
	Name            string                `json:"name"`
	CreatedFrom     string                `json:"created_from"`
	CreatedAt       string                `json:"created_at"`
	UserID          string                `json:"user_id"`
	SubnetIndex     int                   `json:"subnet_index"`
	VMConfig        ManifestVMConfig      `json:"vm_config"`
	Drives          []ManifestDrive       `json:"drives"`
	Network         ManifestNetwork       `json:"network"`
	AgentToken      string                `json:"agent_token"`
	FCPathOrigin    string                `json:"fc_path_origin,omitempty"`
}

type ManifestVMConfig struct {
	VcpuCount  int64 `json:"vcpu_count"`
	MemSizeMib int64 `json:"mem_size_mib"`
}

type ManifestDrive struct {
	DriveID      string `json:"drive_id"`
	Role         string `json:"role"`          // "rootfs", "config", "volume"
	SnapshotFile string `json:"snapshot_file"` // filename relative to snapshot dir
	Name         string `json:"name,omitempty"`
	ReadOnly     bool   `json:"read_only"`
}

type ManifestNetwork struct {
	GuestMAC string `json:"guest_mac"`
	GuestIP  string `json:"guest_ip"`
}

// Checkpoint creates a named snapshot of a running VM.
// The snapshot is fully self-contained: mem, vm state, rootfs, config, and all
// attached volumes are copied into the snapshot directory.
// Checkpoint creates a named snapshot. Returns the manifest as *SnapshotManifest.
// The return type is `any` to allow interface-based dispatch from the server layer.
func (e *Engine) Checkpoint(ctx context.Context, sandboxID, userID string, subnetIndex int, snapName, snapDir string) (any, error) {
	vm, err := e.getVM(sandboxID)
	if err != nil {
		return nil, err
	}

	// Check if target directory already exists BEFORE pausing the VM.
	finalDir := filepath.Join(snapDir, snapName)
	if _, err := os.Stat(finalDir); err == nil {
		return nil, fmt.Errorf("snapshot %q already exists — delete first", snapName)
	}

	vm.stateMu.Lock()
	defer vm.stateMu.Unlock()

	if vm.Status != "running" {
		return nil, fmt.Errorf("sandbox %q is not running", sandboxID)
	}

	// Flush guest page cache before pausing
	if vm.Thermal == "hot" && vm.Agent != nil {
		syncCtx, syncCancel := context.WithTimeout(context.Background(), 10*time.Second)
		vm.Agent.Exec(syncCtx, []string{"sync"}, nil, "")
		syncCancel()
	}

	// Pause VM
	client := fcAPIClient(vm.SocketPath)
	wasPaused := vm.Thermal == "warm"
	if vm.Thermal == "hot" {
		if err := fcPatch(ctx, client, "/vm", `{"state":"Paused"}`); err != nil {
			return nil, fmt.Errorf("pause for checkpoint: %w", err)
		}
		vm.Thermal = "warm"
	}

	// On any error after pause, resume the VM
	resumeOnError := func() {
		if !wasPaused {
			fcPatch(ctx, client, "/vm", `{"state":"Resumed"}`)
			vm.Thermal = "hot"
		}
	}

	// Create temp directory for atomic staging
	tmpDir := finalDir + ".tmp"
	os.RemoveAll(tmpDir) // clean any stale tmp from previous crash
	if err := os.MkdirAll(tmpDir, 0700); err != nil {
		resumeOnError()
		return nil, fmt.Errorf("create snapshot temp dir: %w", err)
	}

	// Create Firecracker snapshot (mem.snap + vm.snap)
	memPath := filepath.Join(tmpDir, "mem.snap")
	vmPath := filepath.Join(tmpDir, "vm.snap")
	if err := fcPut(ctx, client, "/snapshot/create", fmt.Sprintf(
		`{"snapshot_type":"Full","snapshot_path":%q,"mem_file_path":%q}`,
		vmPath, memPath)); err != nil {
		os.RemoveAll(tmpDir)
		resumeOnError()
		return nil, fmt.Errorf("create FC snapshot: %w", err)
	}

	// Build drive list and file copy list
	var drives []ManifestDrive
	type copyJob struct {
		src, dstFile string
	}
	var copies []copyJob

	// Rootfs
	drives = append(drives, ManifestDrive{
		DriveID: "rootfs", Role: "rootfs", SnapshotFile: "rootfs.ext4",
	})
	copies = append(copies, copyJob{vm.RootfsPath, "rootfs.ext4"})

	// Config drive
	configPath := filepath.Join(filepath.Dir(vm.RootfsPath), "config.ext4")
	drives = append(drives, ManifestDrive{
		DriveID: "config", Role: "config", SnapshotFile: "config.ext4",
		ReadOnly: true,
	})
	copies = append(copies, copyJob{configPath, "config.ext4"})

	// Volumes
	for _, vol := range vm.Volumes {
		snapFile := fmt.Sprintf("vol-%s.ext4", vol.Name)
		drives = append(drives, ManifestDrive{
			DriveID: vol.DriveID, Role: "volume", SnapshotFile: snapFile,
			Name: vol.Name, ReadOnly: vol.ReadOnly,
		})
		copies = append(copies, copyJob{vol.FilePath, snapFile})
	}

	// Parallel copy all block devices (rootfs, config, volumes)
	g, _ := errgroup.WithContext(ctx)
	for _, c := range copies {
		c := c // capture
		g.Go(func() error {
			dst := filepath.Join(tmpDir, c.dstFile)
			return copyBlock(c.src, dst)
		})
	}
	if err := g.Wait(); err != nil {
		os.RemoveAll(tmpDir)
		resumeOnError()
		return nil, fmt.Errorf("copy block devices: %w", err)
	}

	// Write manifest
	// FCPathOrigin tracks which sandbox ID Firecracker has recorded in its
	// vm.snap for block device paths. For a directly-created sandbox this is
	// the sandbox's own ID. For a sandbox resumed from a snapshot, it's the
	// original seed sandbox whose paths FC inherited through snapshot/load.
	fcOrigin := vm.FCPathOrigin
	if fcOrigin == "" {
		fcOrigin = sandboxID // fallback for VMs created before this field existed
	}
	manifest := &SnapshotManifest{
		Name:         snapName,
		CreatedFrom:  sandboxID,
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
		UserID:       userID,
		SubnetIndex:  subnetIndex,
		FCPathOrigin: fcOrigin,
		VMConfig: ManifestVMConfig{
			VcpuCount:  vm.VcpuCount,
			MemSizeMib: vm.MemSizeMib,
		},
		Drives:     drives,
		Network:    ManifestNetwork{GuestMAC: vm.GuestMAC, GuestIP: vm.GuestIP},
		AgentToken: vm.Token,
	}
	manifestBytes, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(tmpDir, "manifest.json"), manifestBytes, 0644); err != nil {
		os.RemoveAll(tmpDir)
		resumeOnError()
		return nil, fmt.Errorf("write manifest: %w", err)
	}

	// Atomic rename
	if err := os.Rename(tmpDir, finalDir); err != nil {
		os.RemoveAll(tmpDir)
		resumeOnError()
		return nil, fmt.Errorf("rename snapshot dir: %w", err)
	}

	// Resume VM
	if !wasPaused {
		if err := fcPatch(ctx, client, "/vm", `{"state":"Resumed"}`); err != nil {
			return nil, fmt.Errorf("resume after checkpoint: %w", err)
		}
		vm.Thermal = "hot"
	}

	slog.Info("checkpoint created", "sandbox", sandboxID, "snapshot", snapName,
		"drives", len(drives))

	return manifest, nil
}

// ResumeSnapshot creates a new sandbox from a named snapshot.
// Returns sandbox info for the new VM.
func (e *Engine) ResumeSnapshot(ctx context.Context, snapDir string, manifest *SnapshotManifest, newName string) (info engine.SandboxInfo, err error) {
	id := generateID()
	sandboxDir := filepath.Join(e.cfg.DataDir, "sandboxes", id)
	os.MkdirAll(sandboxDir, 0700)

	// Resource tracking for cleanup on failure
	var (
		guestIP  string
		tapName  string
		vmCancel context.CancelFunc
		fcCmd    *exec.Cmd
	)
	defer func() {
		if err != nil {
			if fcCmd != nil && fcCmd.Process != nil {
				killFC(fcCmd, 1*time.Second)
			}
			if vmCancel != nil {
				vmCancel()
			}
			if tapName != "" {
				destroyTapDevice(tapName)
			}
			if guestIP != "" {
				e.mu.RLock()
				if net, ok := e.userNetworks[manifest.UserID]; ok {
					net.Pool.Release(guestIP)
				}
				e.mu.RUnlock()
			}
			os.RemoveAll(sandboxDir)
		}
	}()

	// 1. Copy block devices from snapshot dir to new sandbox dir.
	// copyBlock uses --reflink=auto for instant CoW clones on btrfs/xfs.
	for _, drive := range manifest.Drives {
		src := filepath.Join(snapDir, drive.SnapshotFile)
		dst := filepath.Join(sandboxDir, drive.SnapshotFile)
		if err = copyBlock(src, dst); err != nil {
			return info, fmt.Errorf("copy %s: %w", drive.SnapshotFile, err)
		}
	}

	// Copy mem.snap and vm.snap
	for _, f := range []string{"mem.snap", "vm.snap"} {
		src := filepath.Join(snapDir, f)
		dst := filepath.Join(sandboxDir, f)
		if err = copyBlock(src, dst); err != nil {
			return info, fmt.Errorf("copy %s: %w", f, err)
		}
	}

	// 2. Network: allocate the EXACT same IP (guest memory has it hardcoded)
	userNet := e.getOrCreateUserNetwork(manifest.UserID, manifest.SubnetIndex)
	if err = ensureUserBridge(userNet); err != nil {
		return info, fmt.Errorf("setup user bridge: %w", err)
	}
	guestIP = manifest.Network.GuestIP
	if err = userNet.Pool.TryAllocate(guestIP); err != nil {
		return info, fmt.Errorf("IP %s required by snapshot is in use: %w", guestIP, err)
	}

	// Create a fresh TAP with a name based on the NEW sandbox ID.
	// network_overrides in /snapshot/load remaps the TAP name, so we
	// no longer need to recreate the original TAP name. This eliminates
	// races when two snapshots from the same origin are resumed concurrently.
	fcOrigin := manifest.FCPathOrigin
	if fcOrigin == "" {
		fcOrigin = manifest.CreatedFrom
	}
	tapName, err = createTapDevice(id, userNet.BridgeName)
	if err != nil {
		return info, fmt.Errorf("create tap: %w", err)
	}

	// 3. Start new Firecracker process
	cid := atomic.AddUint32(&e.nextCID, 1)
	socketPath := filepath.Join(sandboxDir, "firecracker.sock")
	vsockPath := filepath.Join(sandboxDir, "vsock.sock")

	fcProc, startErr := e.startFC(socketPath)
	if startErr != nil {
		err = startErr
		return info, err
	}
	fcCmd = fcProc.cmd
	vmCancel = fcProc.cancel
	stderrBuf := fcProc.stderrBuf

	client := fcAPIClient(socketPath)

	// 4. Pre-load resource configuration — ALL PUTs BEFORE /snapshot/load

	// 5. Load snapshot.
	// Firecracker v1.6 requires /snapshot/load as the ONLY API call on a
	// fresh VMM — no pre-configuration of drives/network/vsock allowed.
	// The vm.snap file records the original block device paths from when
	// the checkpoint was taken. Firecracker opens those exact paths during
	// snapshot load.
	//
	// Solution: the new sandbox dir IS the sandbox dir. We already copied
	// all block devices there with the same filenames used during Create().
	// But the vm.snap records the ORIGINAL sandbox dir path (e.g.
	// /var/lib/bhatti/sandboxes/<old-id>/rootfs.ext4).
	//
	// We must place our files at those original paths. Since the original
	// sandbox was destroyed, those paths are free. We use the new sandbox
	// dir but create symlinks at the old paths pointing to our copies.
	// Use FCPathOrigin (the sandbox whose paths FC recorded in vm.snap),
	// not CreatedFrom (which may be a different sandbox for nested snapshots).
	origSandboxDir := filepath.Join(e.cfg.DataDir, "sandboxes", fcOrigin)
	needCleanup := false
	if _, err := os.Stat(origSandboxDir); os.IsNotExist(err) {
		os.MkdirAll(origSandboxDir, 0700)
		needCleanup = true
	}

	// Place files at original paths (symlinks for efficiency)
	for _, drive := range manifest.Drives {
		src := filepath.Join(sandboxDir, drive.SnapshotFile)
		dst := filepath.Join(origSandboxDir, drive.SnapshotFile)
		os.Remove(dst)
		os.Symlink(src, dst)
	}
	// mem.snap and vm.snap
	for _, f := range []string{"mem.snap", "vm.snap"} {
		src := filepath.Join(sandboxDir, f)
		dst := filepath.Join(origSandboxDir, f)
		os.Remove(dst)
		os.Symlink(src, dst)
	}

	// The vm.snap also references the original vsock.sock path.
	// Firecracker will try to bind this UDS. It must NOT already exist.
	origVsockPath := filepath.Join(origSandboxDir, "vsock.sock")
	os.Remove(origVsockPath)

	memSnapPath := filepath.Join(origSandboxDir, "mem.snap")
	vmSnapPath := filepath.Join(origSandboxDir, "vm.snap")
	// network_overrides remaps the TAP device to the fresh name, eliminating
	// the need to recreate the original TAP name from FCPathOrigin.
	if err = fcPut(ctx, client, "/snapshot/load", fmt.Sprintf(
		`{"snapshot_path":%q,"mem_backend":{"backend_path":%q,"backend_type":"File"},"resume_vm":true,"network_overrides":[{"iface_id":"eth0","host_dev_name":%q}]}`,
		vmSnapPath, memSnapPath, tapName)); err != nil {
		if needCleanup {
			os.RemoveAll(origSandboxDir)
		}
		if fcStderr := stderrBuf.String(); fcStderr != "" {
			return info, fmt.Errorf("load snapshot: %w\nFC stderr: %s", err, fcStderr)
		}
		return info, fmt.Errorf("load snapshot: %w", err)
	}

	// Cleanup symlinks — FC has opened the file descriptors, symlinks no longer needed
	if needCleanup {
		os.RemoveAll(origSandboxDir)
	}

	// 6. Flush bridge FDB for the MAC to avoid ARP staleness
	exec.Command("bridge", "fdb", "del", manifest.Network.GuestMAC,
		"dev", userNet.BridgeName, "master").Run() // best effort

	// 7. Connect agent
	agentClient := agent.NewTCPClientWithAuth(guestIP, manifest.AgentToken)
	if err = agentClient.WaitReady(ctx, 30*time.Second); err != nil {
		return info, fmt.Errorf("agent not ready after resume: %w", err)
	}

	name := newName
	if name == "" {
		name = id
	}

	vm := &VM{
		ID: id, Name: name, UserID: manifest.UserID,
		SocketPath: socketPath, VsockPath: vsockPath,
		RootfsPath: filepath.Join(sandboxDir, "rootfs.ext4"),
		CID: cid, VcpuCount: manifest.VMConfig.VcpuCount,
		MemSizeMib: manifest.VMConfig.MemSizeMib,
		TapDevice: tapName, GuestIP: guestIP,
		GuestMAC: manifest.Network.GuestMAC,
		Token:   manifest.AgentToken,
		FCPathOrigin: fcOrigin, // inherit: FC still has same internal paths
		Agent:   agentClient,
		Status:  "running",
		Thermal: "hot",
		cancel:  vmCancel,
		cmd:     fcCmd,
		stderrBuf: stderrBuf,
		hasBaseSnapshot: true, // resumed VMs can do diff snapshots
	}

	e.mu.Lock()
	e.vms[id] = vm
	e.mu.Unlock()

	slog.Info("snapshot resumed", "snapshot", manifest.Name, "sandbox", id,
		"ip", guestIP)

	return engine.SandboxInfo{
		ID: id, Name: name, Status: "running",
		IP: guestIP, EngineID: id,
	}, nil
}

// ResumeFromManifestJSON is the server-callable entry point that unmarshals
// the manifest JSON and delegates to ResumeSnapshot.
func (e *Engine) ResumeFromManifestJSON(ctx context.Context, snapDir string, manifestJSON []byte, newName string) (engine.SandboxInfo, error) {
	var m SnapshotManifest
	if err := json.Unmarshal(manifestJSON, &m); err != nil {
		return engine.SandboxInfo{}, fmt.Errorf("parse manifest: %w", err)
	}
	return e.ResumeSnapshot(ctx, snapDir, &m, newName)
}
