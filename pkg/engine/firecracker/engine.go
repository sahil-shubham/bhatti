//go:build linux

// Package firecracker implements engine.Engine using Firecracker microVMs.
// It talks directly to Firecracker's HTTP API over a Unix socket — no SDK needed.
package firecracker

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent"
	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// Config holds paths and settings for a Firecracker engine.
type Config struct {
	DataDir    string // e.g. /var/lib/bhatti — sandboxes created under DataDir/sandboxes/
	KernelPath string // path to vmlinux binary
	BaseRootfs string // path to base rootfs.ext4
	FCBinary   string // path to firecracker binary
}

// Engine manages Firecracker microVMs.
type Engine struct {
	mu           sync.RWMutex
	vms          map[string]*VM
	cfg          Config
	nextCID      uint32
	userNetworks map[string]*UserNetwork // userID → network
}

// VolumeAttachmentInfo records a volume attached to a running VM.
// Populated during Create(), persisted in engine_meta, used by checkpoint.
type VolumeAttachmentInfo struct {
	DriveID  string `json:"drive_id"`   // Firecracker drive ID ("vol0")
	Name     string `json:"name"`       // volume name ("workspace")
	FilePath string `json:"file_path"`  // host path to ext4 file
	Mount    string `json:"mount"`      // guest mount point
	ReadOnly bool   `json:"read_only"`
}

// VM holds per-sandbox state.
type VM struct {
	// stateMu protects all mutable fields below. The engine-level
	// sync.RWMutex (e.mu) protects the vms map — not individual VM state.
	//
	// Lock discipline:
	//   - Short operations (Exec, Pause, Resume, Status, FileRead, etc.):
	//     hold stateMu for the entire operation.
	//   - Long-lived operations (Shell, Tunnel):
	//     hold stateMu only to validate state and capture the Agent reference,
	//     then release before the blocking call. The Agent pointer is safe to
	//     use after release because it's only replaced during Start() which
	//     holds stateMu.
	stateMu     sync.Mutex

	ID          string
	Name        string
	UserID      string // owner's user ID (for bridge cleanup on destroy)
	SocketPath  string // Firecracker API socket
	VsockPath   string // vsock UDS for host↔guest
	RootfsPath  string
	SnapMemPath string
	SnapVMPath  string
	CID         uint32
	VcpuCount   int64
	MemSizeMib  int64
	TapDevice   string
	GuestIP     string
	GuestMAC    string
	Token       string // agent auth token
	Volumes     []VolumeAttachmentInfo // populated in Create, used by checkpoint
	Agent           *agent.AgentClient
	Status          string // "running", "stopped"
	Thermal         string // "hot", "warm", "cold"
	hasBaseSnapshot bool   // true after first Full snapshot (enables Diff)
	cancel          context.CancelFunc
	cmd             *exec.Cmd
}

// New validates config and returns a Firecracker engine.
func New(cfg Config) (*Engine, error) {
	for name, path := range map[string]string{
		"kernel":      cfg.KernelPath,
		"base rootfs": cfg.BaseRootfs,
		"firecracker": cfg.FCBinary,
	} {
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("%s not found at %s: %w", name, path, err)
		}
	}

	for _, sub := range []string{"sandboxes", "images", "volumes", "snapshots"} {
		if err := os.MkdirAll(filepath.Join(cfg.DataDir, sub), 0700); err != nil {
			return nil, fmt.Errorf("create %s dir: %w", sub, err)
		}
	}

	eng := &Engine{
		vms:          make(map[string]*VM),
		cfg:          cfg,
		nextCID:      3, // 0=hypervisor, 1=loopback, 2=host
		userNetworks: make(map[string]*UserNetwork),
	}

	// Clean up legacy single-bridge from pre-multi-tenant setup
	cleanupOldBridge()

	// Set up global firewall rules (6 rules, independent of user/VM count)
	if err := setupGlobalFirewall(); err != nil {
		return nil, fmt.Errorf("setup firewall: %w", err)
	}

	// Clean up orphaned TAP devices from previous crashes.
	// At this point no VMs are loaded yet, so all TAPs are orphans.
	cleanupOrphanedTapDevices(nil)

	return eng, nil
}

// Shutdown stops all running VMs and cleans up all TAP devices.
// Called on server shutdown (SIGTERM).
func (e *Engine) Shutdown() {
	e.mu.RLock()
	ids := make([]string, 0, len(e.vms))
	for id := range e.vms {
		ids = append(ids, id)
	}
	e.mu.RUnlock()

	for _, id := range ids {
		vm, err := e.getVM(id)
		if err != nil {
			continue
		}
		if vm.Status == "running" && vm.cmd != nil {
			vm.cmd.Process.Kill()
			vm.cmd.Wait()
		}
	}

	cleanupAllTapDevices()
	cleanupAllUserBridges()
}

func (e *Engine) getVM(id string) (*VM, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	vm, ok := e.vms[id]
	if !ok {
		return nil, fmt.Errorf("sandbox %q not found", id)
	}
	return vm, nil
}

// getOrCreateUserNetwork returns the UserNetwork for a user, creating the
// bridge and IP pool if this is the first sandbox for that user.
func (e *Engine) getOrCreateUserNetwork(userID string, subnetIndex int) *UserNetwork {
	e.mu.Lock()
	defer e.mu.Unlock()

	if net, ok := e.userNetworks[userID]; ok {
		return net
	}

	gateway, subnet, bridge := subnetFromIndex(subnetIndex)
	net := &UserNetwork{
		BridgeName: bridge,
		GatewayIP:  gateway,
		Subnet:     subnet,
		Pool:       newIPPool(gateway),
	}
	e.userNetworks[userID] = net
	return net
}

// removeUserNetworkIfEmpty removes the user's bridge if they have no more VMs.
func (e *Engine) removeUserNetworkIfEmpty(userID string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	net, ok := e.userNetworks[userID]
	if !ok {
		return
	}

	// Check if any VMs still belong to this user
	for _, vm := range e.vms {
		if vm.UserID == userID {
			return // still has VMs, keep the bridge
		}
	}

	destroyUserBridge(net.BridgeName)
	delete(e.userNetworks, userID)
	slog.Info("destroyed user bridge", "user", userID, "bridge", net.BridgeName)
}

// --- Create ---

func (e *Engine) Create(ctx context.Context, spec engine.SandboxSpec) (info engine.SandboxInfo, err error) {
	id := generateID()
	sandboxDir := filepath.Join(e.cfg.DataDir, "sandboxes", id)
	os.MkdirAll(sandboxDir, 0700)

	// Deferred cleanup: on any error, release all resources acquired so far.
	// Uses named return `err` so the defer sees the final error value.
	var (
		guestIP  string
		tapName  string
		vmCancel context.CancelFunc
		fcCmd    *exec.Cmd
	)
	defer func() {
		if err != nil {
			if fcCmd != nil && fcCmd.Process != nil {
				fcCmd.Process.Kill()
				fcCmd.Wait()
			}
			if vmCancel != nil {
				vmCancel()
			}
			if tapName != "" {
				destroyTapDevice(tapName)
			}
			if guestIP != "" {
				e.mu.RLock()
				if net, ok := e.userNetworks[spec.UserID]; ok {
					net.Pool.Release(guestIP)
				}
				e.mu.RUnlock()
			}
			os.RemoveAll(sandboxDir)
		}
	}()

	// 1. Copy rootfs (from resolved image path or default base)
	baseImage := e.cfg.BaseRootfs
	if spec.BaseImage != "" {
		baseImage = spec.BaseImage
	}
	rootfsPath := filepath.Join(sandboxDir, "rootfs.ext4")
	if err = copyRootfs(baseImage, rootfsPath); err != nil {
		return info, fmt.Errorf("copy rootfs: %w", err)
	}

	// 1b. Re-inject lohar into rootfs to prevent protocol drift.
	// Saved images / OCI images may have an older lohar that doesn't
	// understand new config drive fields (e.g. ReadOnly). Without this,
	// the read_only JSON key is silently ignored → data corruption.
	if err = injectLoharIntoRootfs(rootfsPath, e.cfg.DataDir); err != nil {
		slog.Warn("lohar injection failed", "error", err)
		// Non-fatal — image's lohar may work, but warn loudly
	}

	// 1c. Resize rootfs if requested
	if spec.DiskSizeMB > 0 {
		exec.Command("e2fsck", "-f", "-y", rootfsPath).Run() // best effort
		if err = exec.Command("truncate", "-s", fmt.Sprintf("%dM", spec.DiskSizeMB), rootfsPath).Run(); err != nil {
			return info, fmt.Errorf("resize rootfs: %w", err)
		}
		if err = exec.Command("resize2fs", rootfsPath).Run(); err != nil {
			return info, fmt.Errorf("resize2fs: %w", err)
		}
	}

	// 2. Allocate CID and paths
	cid := atomic.AddUint32(&e.nextCID, 1)
	socketPath := filepath.Join(sandboxDir, "firecracker.sock")
	vsockPath := filepath.Join(sandboxDir, "vsock.sock")

	// 3. Get or create user's network, allocate IP, create TAP
	userNet := e.getOrCreateUserNetwork(spec.UserID, spec.SubnetIndex)
	if err = ensureUserBridge(userNet); err != nil {
		return info, fmt.Errorf("setup user bridge: %w", err)
	}
	guestIP, err = userNet.Pool.Allocate()
	if err != nil {
		return info, fmt.Errorf("allocate IP: %w", err)
	}
	tapName, err = createTapDevice(id, userNet.BridgeName)
	if err != nil {
		return info, fmt.Errorf("create tap: %w", err)
	}

	// 4. Compute VM config
	vcpuCount := int64(spec.CPUs)
	if vcpuCount < 1 {
		vcpuCount = 1
	}
	memMB := int64(spec.MemoryMB)
	if memMB < 128 {
		memMB = 512
	}
	mac := generateMAC()

	// 4b. Generate auth token
	tokenBytes := make([]byte, 16)
	rand.Read(tokenBytes)
	token := fmt.Sprintf("%x", tokenBytes)

	// 4c. Build config drive
	envMap := make(map[string]string)
	for k, v := range spec.Env {
		envMap[k] = v
	}
	filesMap := make(map[string]ConfigFile)
	for path, f := range spec.Files {
		filesMap[path] = ConfigFile{
			Content: base64.StdEncoding.EncodeToString(f.Content),
			Mode:    f.Mode,
		}
	}

	configDrivePath := filepath.Join(sandboxDir, "config.ext4")
	var volumeMounts []VolumeMountConfig
	var volAttachments []VolumeAttachmentInfo
	driveIndex := byte('c') // vdb=config, vdc=first vol, vdd=second, ...

	// Maximum 24 volumes per sandbox (vdc through vdz)
	const maxVolumesPerSandbox = 24
	totalVols := len(spec.ResolvedVolumes) + len(spec.NewVolumes)
	if totalVols > maxVolumesPerSandbox {
		return info, fmt.Errorf("too many volumes: %d (max %d)", totalVols, maxVolumesPerSandbox)
	}

	// Persistent volumes (resolved by server layer)
	for _, vol := range spec.ResolvedVolumes {
		device := fmt.Sprintf("/dev/vd%c", driveIndex)
		volumeMounts = append(volumeMounts, VolumeMountConfig{
			Device: device, Mount: vol.Mount, FS: "ext4", ReadOnly: vol.ReadOnly,
		})
		volAttachments = append(volAttachments, VolumeAttachmentInfo{
			DriveID: vol.DriveID, Name: vol.Name, FilePath: vol.FilePath,
			Mount: vol.Mount, ReadOnly: vol.ReadOnly,
		})
		driveIndex++
	}

	// Legacy ephemeral volumes (created in sandbox dir, destroyed with sandbox)
	for _, vs := range spec.NewVolumes {
		volPath := filepath.Join(sandboxDir, fmt.Sprintf("vol-%s.ext4", vs.Name))
		if err = createVolume(volPath, vs.SizeMB); err != nil {
			return info, fmt.Errorf("create volume %s: %w", vs.Name, err)
		}
		device := fmt.Sprintf("/dev/vd%c", driveIndex)
		volumeMounts = append(volumeMounts, VolumeMountConfig{
			Device: device, Mount: vs.Mount, FS: "ext4",
		})
		driveIndex++
	}

	name := spec.Name
	if name == "" {
		name = id
	}

	if err = createConfigDrive(configDrivePath, SandboxConfig{
		SandboxID: id,
		Hostname:  name,
		Token:     token,
		Env:       envMap,
		Files:     filesMap,
		Volumes:   volumeMounts,
		Init:      spec.Init,
		DNS:       []string{"1.1.1.1", "8.8.8.8"},
		User:      "lohar",
	}); err != nil {
		return info, fmt.Errorf("create config drive: %w", err)
	}

	// 5. Start Firecracker process
	vmCtx, cancel := context.WithCancel(context.Background())
	vmCancel = cancel // assign to outer var so defer can clean up
	os.Remove(socketPath)
	fcCmd = exec.CommandContext(vmCtx, e.cfg.FCBinary, "--api-sock", socketPath)
	fcCmd.Stderr = os.Stderr
	if err = fcCmd.Start(); err != nil {
		return info, fmt.Errorf("start firecracker: %w", err)
	}

	// Wait for API socket to appear
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 6. Configure via HTTP API
	client := fcAPIClient(socketPath)

	// Boot args include ip= for kernel-level network configuration.
	// Uses the user's bridge gateway instead of a hardcoded IP.
	bootArgs := fmt.Sprintf(
		"console=ttyS0 reboot=k panic=1 pci=off init=/usr/local/bin/lohar quiet loglevel=0 ip=%s::%s:255.255.255.0::eth0:off:1.1.1.1:8.8.8.8:",
		guestIP, userNet.GatewayIP)

	if err = fcPut(client, "/boot-source", fmt.Sprintf(
		`{"kernel_image_path":%q,"boot_args":%q}`,
		e.cfg.KernelPath, bootArgs)); err != nil {
		return info, fmt.Errorf("set boot-source: %w", err)
	}

	if err = fcPut(client, "/drives/rootfs", fmt.Sprintf(
		`{"drive_id":"rootfs","path_on_host":%q,"is_root_device":true,"is_read_only":false}`,
		rootfsPath)); err != nil {
		return info, fmt.Errorf("set drive: %w", err)
	}

	// track_dirty_pages enables diff snapshots (only dirty pages are written).
	if err = fcPut(client, "/machine-config", fmt.Sprintf(
		`{"vcpu_count":%d,"mem_size_mib":%d,"track_dirty_pages":true}`, vcpuCount, memMB)); err != nil {
		return info, fmt.Errorf("set machine-config: %w", err)
	}

	if err = fcPut(client, "/vsock", fmt.Sprintf(
		`{"guest_cid":%d,"uds_path":%q}`, cid, vsockPath)); err != nil {
		return info, fmt.Errorf("set vsock: %w", err)
	}

	if err = fcPut(client, "/network-interfaces/eth0", fmt.Sprintf(
		`{"iface_id":"eth0","guest_mac":%q,"host_dev_name":%q}`,
		mac, tapName)); err != nil {
		return info, fmt.Errorf("set network: %w", err)
	}

	// 6b. Attach config drive as /dev/vdb
	if err = fcPut(client, "/drives/config", fmt.Sprintf(
		`{"drive_id":"config","path_on_host":%q,"is_root_device":false,"is_read_only":true}`,
		configDrivePath)); err != nil {
		return info, fmt.Errorf("set config drive: %w", err)
	}

	// 6c. Attach persistent volume drives
	for _, vol := range spec.ResolvedVolumes {
		if err = fcPut(client, fmt.Sprintf("/drives/%s", vol.DriveID), fmt.Sprintf(
			`{"drive_id":%q,"path_on_host":%q,"is_root_device":false,"is_read_only":%v}`,
			vol.DriveID, vol.FilePath, vol.ReadOnly)); err != nil {
			return info, fmt.Errorf("set persistent volume drive %s: %w", vol.DriveID, err)
		}
	}

	// 6d. Attach legacy ephemeral volume drives
	for i, vs := range spec.NewVolumes {
		volPath := filepath.Join(sandboxDir, fmt.Sprintf("vol-%s.ext4", vs.Name))
		driveID := fmt.Sprintf("ephvol%d", i)
		if err = fcPut(client, fmt.Sprintf("/drives/%s", driveID), fmt.Sprintf(
			`{"drive_id":%q,"path_on_host":%q,"is_root_device":false,"is_read_only":false}`,
			driveID, volPath)); err != nil {
			return info, fmt.Errorf("set volume drive %d: %w", i, err)
		}
	}

	// 7. Boot
	if err = fcPut(client, "/actions", `{"action_type":"InstanceStart"}`); err != nil {
		return info, fmt.Errorf("start instance: %w", err)
	}

	// 8. Wait for agent via TCP (kernel ip= already configured eth0).
	agentClient := agent.NewTCPClientWithAuth(guestIP, token)
	if err = agentClient.WaitReady(ctx, 30*time.Second); err != nil {
		return info, fmt.Errorf("agent not ready: %w", err)
	}

	vm := &VM{
		ID: id, Name: name, UserID: spec.UserID,
		SocketPath: socketPath,
		VsockPath: vsockPath, RootfsPath: rootfsPath,
		CID: cid, VcpuCount: vcpuCount, MemSizeMib: memMB,
		TapDevice: tapName, GuestIP: guestIP, GuestMAC: mac,
		Token: token, Volumes: volAttachments,
		Agent: agentClient, Status: "running",
		Thermal: "hot", cancel: vmCancel, cmd: fcCmd,
	}

	e.mu.Lock()
	e.vms[id] = vm
	e.mu.Unlock()

	return engine.SandboxInfo{
		ID: id, Name: name, Status: "running",
		IP: guestIP, EngineID: id,
	}, nil
}

// --- SaveImage (save rootfs as image) ---

// SaveImage pauses the VM, flushes the page cache, copies the rootfs to
// destPath, and resumes. The copy is a complete flat ext4 file capturing
// the filesystem at save time (no memory state).
func (e *Engine) SaveImage(ctx context.Context, sandboxID, destPath string) error {
	vm, err := e.getVM(sandboxID)
	if err != nil {
		return err
	}

	vm.stateMu.Lock()
	defer vm.stateMu.Unlock()

	// Flush guest page cache before pausing — pausing vCPUs does NOT
	// flush dirty pages from guest RAM to the virtio-blk device.
	if vm.Thermal == "hot" && vm.Agent != nil {
		syncCtx, syncCancel := context.WithTimeout(context.Background(), 10*time.Second)
		vm.Agent.Exec(syncCtx, []string{"sync"}, nil, "")
		syncCancel()
	}

	wasPaused := vm.Thermal == "warm"
	if vm.Thermal == "hot" {
		client := fcAPIClient(vm.SocketPath)
		if err := fcPatch(client, "/vm", `{"state":"Paused"}`); err != nil {
			return fmt.Errorf("pause for save: %w", err)
		}
		vm.Thermal = "warm"
	}

	// Copy rootfs while VM is paused — no concurrent mutations possible
	if err := copyRootfs(vm.RootfsPath, destPath); err != nil {
		if !wasPaused {
			client := fcAPIClient(vm.SocketPath)
			fcPatch(client, "/vm", `{"state":"Resumed"}`)
			vm.Thermal = "hot"
		}
		return fmt.Errorf("copy rootfs: %w", err)
	}

	if !wasPaused {
		client := fcAPIClient(vm.SocketPath)
		if err := fcPatch(client, "/vm", `{"state":"Resumed"}`); err != nil {
			return fmt.Errorf("resume after save: %w", err)
		}
		vm.Thermal = "hot"
	}

	return nil
}

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

	client := fcAPIClient(vm.SocketPath)

	// Pause VM
	if err := fcPatch(client, "/vm", `{"state":"Paused"}`); err != nil {
		return fmt.Errorf("pause: %w", err)
	}

	// Create snapshot — use Diff if we have a base, Full otherwise.
	// Diff snapshots write only dirty pages since the last snapshot,
	// reducing write time from ~4s (512MB full) to ~0.5s (10-50MB diff).
	vm.SnapMemPath = filepath.Join(filepath.Dir(vm.RootfsPath), "mem.snap")
	vm.SnapVMPath = filepath.Join(filepath.Dir(vm.RootfsPath), "vm.snap")

	snapshotType := "Full"
	if vm.hasBaseSnapshot {
		// Verify base snapshot files still exist
		if _, err := os.Stat(vm.SnapMemPath); err != nil {
			slog.Warn("base snapshot missing, falling back to full",
				"id", id, "path", vm.SnapMemPath, "error", err)
			vm.hasBaseSnapshot = false
		} else {
			snapshotType = "Diff"
		}
	}

	if err := fcPut(client, "/snapshot/create", fmt.Sprintf(
		`{"snapshot_type":%q,"snapshot_path":%q,"mem_file_path":%q}`,
		snapshotType, vm.SnapVMPath, vm.SnapMemPath)); err != nil {
		return fmt.Errorf("create %s snapshot: %w", snapshotType, err)
	}

	if !vm.hasBaseSnapshot {
		vm.hasBaseSnapshot = true
	}

	// Stop VMM process
	vm.cmd.Process.Kill()
	vm.cmd.Wait()
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
	client := fcAPIClient(vm.SocketPath)
	if err := fcPatch(client, "/vm", `{"state":"Paused"}`); err != nil {
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
	client := fcAPIClient(vm.SocketPath)
	if err := fcPatch(client, "/vm", `{"state":"Resumed"}`); err != nil {
		return fmt.Errorf("resume: %w", err)
	}
	vm.Thermal = "hot"
	return nil
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
	vm, err := e.getVM(id)
	if err != nil {
		return err
	}

	vm.stateMu.Lock()
	defer vm.stateMu.Unlock()

	if vm.SnapMemPath == "" {
		return fmt.Errorf("sandbox %q has no snapshot to resume from", id)
	}

	// TAP device is kept alive across stop/start (not destroyed in Stop).
	// No need to recreate it.

	// New Firecracker process
	newSocketPath := vm.SocketPath + ".resume"
	os.Remove(newSocketPath)
	os.Remove(vm.VsockPath)

	vmCtx, vmCancel := context.WithCancel(context.Background())
	fcCmd := exec.CommandContext(vmCtx, e.cfg.FCBinary, "--api-sock", newSocketPath)
	fcCmd.Stderr = os.Stderr
	if err := fcCmd.Start(); err != nil {
		vmCancel()
		return fmt.Errorf("start firecracker: %w", err)
	}

	for i := 0; i < 50; i++ {
		if _, err := os.Stat(newSocketPath); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	client := fcAPIClient(newSocketPath)

	// enable_diff_snapshots re-enables dirty page tracking after restore,
	// allowing subsequent Diff snapshots on the restored VM.
	if err := fcPut(client, "/snapshot/load", fmt.Sprintf(
		`{"snapshot_path":%q,"mem_backend":{"backend_path":%q,"backend_type":"File"},"resume_vm":true,"enable_diff_snapshots":true}`,
		vm.SnapVMPath, vm.SnapMemPath)); err != nil {
		fcCmd.Process.Kill()
		vmCancel()
		return fmt.Errorf("load snapshot: %w", err)
	}

	vm.SocketPath = newSocketPath
	vm.cmd = fcCmd
	vm.cancel = vmCancel
	vm.Status = "running"
	vm.Thermal = "hot"

	// Use TCP client for post-resume — vsock is broken after snapshot/restore
	// but virtio-net (TCP over TAP) survives.
	vm.Agent = agent.NewTCPClientWithAuth(vm.GuestIP, vm.Token)

	// Wait for agent to be responsive after resume.
	if err := vm.Agent.WaitReady(ctx, 30*time.Second); err != nil {
		fcCmd.Process.Kill()
		vmCancel()
		return fmt.Errorf("agent not ready after resume: %w", err)
	}

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
		vm.cmd.Process.Kill()
		vm.cmd.Wait()
		vm.cancel()
	}

	// Always clean up TAP and release IP — whether running or stopped.
	if vm.TapDevice != "" {
		destroyTapDevice(vm.TapDevice)
	}

	// Release IP back to the user's pool
	userID := vm.UserID
	if vm.GuestIP != "" {
		e.mu.RLock()
		if net, ok := e.userNetworks[userID]; ok {
			net.Pool.Release(vm.GuestIP)
		}
		e.mu.RUnlock()
	}

	rootfsDir := filepath.Dir(vm.RootfsPath)
	vm.stateMu.Unlock()

	os.RemoveAll(rootfsDir)

	e.mu.Lock()
	delete(e.vms, id)
	e.mu.Unlock()

	// If this was the user's last VM, destroy their bridge
	if userID != "" {
		e.removeUserNetworkIfEmpty(userID)
	}

	return nil
}

// --- Status, List ---

func (e *Engine) Status(ctx context.Context, id string) (engine.SandboxInfo, error) {
	vm, err := e.getVM(id)
	if err != nil {
		return engine.SandboxInfo{}, err
	}
	vm.stateMu.Lock()
	defer vm.stateMu.Unlock()
	return engine.SandboxInfo{
		ID: vm.ID, Name: vm.Name, Status: vm.Status,
		IP: vm.GuestIP, EngineID: vm.ID,
	}, nil
}

// VMState returns the internal state of a VM for persistence.
// Returns nil if the VM doesn't exist.
func (e *Engine) VMState(id string) map[string]interface{} {
	vm, err := e.getVM(id)
	if err != nil {
		return nil
	}
	vm.stateMu.Lock()
	defer vm.stateMu.Unlock()
	return map[string]interface{}{
		"rootfs_path":       vm.RootfsPath,
		"snap_mem_path":     vm.SnapMemPath,
		"snap_vm_path":      vm.SnapVMPath,
		"vsock_cid":         vm.CID,
		"tap_device":        vm.TapDevice,
		"guest_ip":          vm.GuestIP,
		"guest_mac":         vm.GuestMAC,
		"vcpu_count":        vm.VcpuCount,
		"mem_size_mib":      vm.MemSizeMib,
		"socket_path":       vm.SocketPath,
		"vsock_path":        vm.VsockPath,
		"has_base_snapshot": vm.hasBaseSnapshot,
		"agent_token":       vm.Token,
		"volumes":           vm.Volumes,
	}
}

// RestoreVM adds a VM to the engine's in-memory map from persisted state.
// Used during startup recovery.
//
// The state map comes from either JSON unmarshal (all numbers are float64) or
// SQLite (numbers may be int, int64, or float64 depending on the driver).
// All extraction uses type-safe helpers to avoid panics on type mismatch.
func (e *Engine) RestoreVM(id, name, status string, state map[string]interface{}) {
	userID := stateStr(state, "user_id")
	subnetIndex := int(stateInt64(state, "subnet_index"))

	token := stateStr(state, "agent_token")

	vm := &VM{
		ID:          id,
		Name:        name,
		UserID:      userID,
		Status:      status,
		Token:       token,
		RootfsPath:  stateStr(state, "rootfs_path"),
		SocketPath:  stateStr(state, "socket_path"),
		VsockPath:   stateStr(state, "vsock_path"),
		CID:         stateUint32(state, "vsock_cid"),
		TapDevice:   stateStr(state, "tap_device"),
		GuestIP:     stateStr(state, "guest_ip"),
		GuestMAC:    stateStr(state, "guest_mac"),
		VcpuCount:   stateInt64(state, "vcpu_count"),
		MemSizeMib:  stateInt64(state, "mem_size_mib"),
		SnapMemPath:     stateStr(state, "snap_mem_path"),
		SnapVMPath:      stateStr(state, "snap_vm_path"),
		hasBaseSnapshot: stateBool(state, "has_base_snapshot"),
	}

	// Restore volume attachments (JSON round-trip through interface{})
	if raw, ok := state["volumes"]; ok && raw != nil {
		b, _ := json.Marshal(raw)
		json.Unmarshal(b, &vm.Volumes)
	}

	if status == "running" {
		if token != "" {
			vm.Agent = agent.NewTCPClientWithAuth(vm.GuestIP, token)
		} else {
			vm.Agent = agent.NewTCPClient(vm.GuestIP)
		}
	}

	// Reserve the IP in the user's pool
	if userID != "" && subnetIndex > 0 {
		userNet := e.getOrCreateUserNetwork(userID, subnetIndex)
		userNet.Pool.Mark(vm.GuestIP)
	}

	e.mu.Lock()
	e.vms[id] = vm
	if vm.CID >= e.nextCID {
		e.nextCID = vm.CID + 1
	}
	e.mu.Unlock()
}

func (e *Engine) List(ctx context.Context) ([]engine.SandboxInfo, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	var out []engine.SandboxInfo
	for _, vm := range e.vms {
		out = append(out, engine.SandboxInfo{
			ID: vm.ID, Name: vm.Name, Status: vm.Status,
			IP: vm.GuestIP, EngineID: vm.ID,
		})
	}
	return out, nil
}

// --- Exec, Shell, ListeningPorts, Tunnel ---

func (e *Engine) Exec(ctx context.Context, id string, cmd []string) (engine.ExecResult, error) {
	vm, err := e.getVM(id)
	if err != nil {
		return engine.ExecResult{}, err
	}

	vm.stateMu.Lock()
	if vm.Thermal != "hot" {
		vm.stateMu.Unlock()
		return engine.ExecResult{}, fmt.Errorf("sandbox %q is not hot (thermal=%s)", id, vm.Thermal)
	}
	ag := vm.Agent
	vm.stateMu.Unlock()

	return ag.Exec(ctx, cmd, nil, "")
}

// ExecStream implements engine.StreamExecEngine. It sends STDOUT/STDERR/EXIT
// frames as StreamEvents via the callback as they arrive from the agent.
func (e *Engine) ExecStream(ctx context.Context, id string, cmd []string, onEvent func(engine.StreamEvent)) error {
	vm, err := e.getVM(id)
	if err != nil {
		return err
	}

	vm.stateMu.Lock()
	if vm.Thermal != "hot" {
		vm.stateMu.Unlock()
		return fmt.Errorf("sandbox %q is not hot (thermal=%s)", id, vm.Thermal)
	}
	ag := vm.Agent
	vm.stateMu.Unlock()

	conn, err := ag.DialControl(ctx)
	if err != nil {
		return fmt.Errorf("agent connect: %w", err)
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}

	req := proto.ExecRequest{Argv: cmd}
	if err := proto.SendJSON(conn, proto.EXEC_REQ, req); err != nil {
		return fmt.Errorf("agent send exec: %w", err)
	}

	for {
		msgType, payload, err := proto.ReadFrame(conn)
		if err != nil {
			return fmt.Errorf("agent read: %w", err)
		}
		switch msgType {
		case proto.STDOUT:
			onEvent(engine.StreamEvent{Type: "stdout", Data: string(payload)})
		case proto.STDERR:
			onEvent(engine.StreamEvent{Type: "stderr", Data: string(payload)})
		case proto.EXIT:
			code, _ := proto.ParseExitCode(payload)
			c := int(code)
			onEvent(engine.StreamEvent{Type: "exit", ExitCode: &c})
			return nil
		case proto.ERROR:
			onEvent(engine.StreamEvent{Type: "error", Data: string(payload)})
			return fmt.Errorf("agent: %s", payload)
		}
	}
}

func (e *Engine) Shell(ctx context.Context, id string) (engine.TerminalConn, error) {
	vm, err := e.getVM(id)
	if err != nil {
		return nil, err
	}

	// Capture agent ref under lock, release before long-lived Shell call.
	vm.stateMu.Lock()
	if vm.Thermal != "hot" {
		vm.stateMu.Unlock()
		return nil, fmt.Errorf("sandbox %q is not hot (thermal=%s)", id, vm.Thermal)
	}
	ag := vm.Agent
	vm.stateMu.Unlock()

	return ag.Shell(ctx, []string{"/bin/zsh", "-li"}, map[string]string{
		"TERM": "xterm-256color",
	}, 24, 80)
}

func (e *Engine) ListeningPorts(ctx context.Context, id string) ([]int, error) {
	vm, err := e.getVM(id)
	if err != nil {
		return nil, err
	}

	vm.stateMu.Lock()
	if vm.Thermal != "hot" {
		vm.stateMu.Unlock()
		return nil, fmt.Errorf("sandbox %q is not hot (thermal=%s)", id, vm.Thermal)
	}
	ag := vm.Agent
	vm.stateMu.Unlock()

	result, err := ag.Exec(ctx, []string{"ss", "-tln", "--no-header"}, nil, "")
	if err != nil {
		return nil, err
	}
	return parseSSOutput(result.Stdout), nil
}

func (e *Engine) Tunnel(ctx context.Context, id string, port int) (io.ReadWriteCloser, error) {
	vm, err := e.getVM(id)
	if err != nil {
		return nil, err
	}

	// Capture agent ref under lock, release before long-lived Tunnel call.
	vm.stateMu.Lock()
	if vm.Thermal != "hot" {
		vm.stateMu.Unlock()
		return nil, fmt.Errorf("sandbox %q is not hot (thermal=%s)", id, vm.Thermal)
	}
	ag := vm.Agent
	vm.stateMu.Unlock()

	return ag.Forward(ctx, uint16(port))
}

// --- Session Operations ---

func (e *Engine) SessionList(ctx context.Context, id string) ([]proto.SessionInfo, error) {
	vm, err := e.getVM(id)
	if err != nil {
		return nil, err
	}

	vm.stateMu.Lock()
	if vm.Thermal != "hot" {
		vm.stateMu.Unlock()
		return nil, fmt.Errorf("sandbox %q is not hot (thermal=%s)", id, vm.Thermal)
	}
	ag := vm.Agent
	vm.stateMu.Unlock()

	return ag.SessionList(ctx)
}

// --- File Operations ---

func (e *Engine) FileRead(ctx context.Context, id, path string, w io.Writer, opts ...agent.FileReadOpts) (int64, string, error) {
	vm, err := e.getVM(id)
	if err != nil {
		return 0, "", err
	}

	vm.stateMu.Lock()
	if vm.Thermal != "hot" {
		vm.stateMu.Unlock()
		return 0, "", fmt.Errorf("sandbox %q is not hot (thermal=%s)", id, vm.Thermal)
	}
	ag := vm.Agent
	vm.stateMu.Unlock()

	return ag.FileRead(ctx, path, w, opts...)
}

func (e *Engine) FileWrite(ctx context.Context, id, path, mode string, size int64, r io.Reader) error {
	vm, err := e.getVM(id)
	if err != nil {
		return err
	}

	vm.stateMu.Lock()
	if vm.Thermal != "hot" {
		vm.stateMu.Unlock()
		return fmt.Errorf("sandbox %q is not hot (thermal=%s)", id, vm.Thermal)
	}
	ag := vm.Agent
	vm.stateMu.Unlock()

	return ag.FileWrite(ctx, path, mode, size, r)
}

func (e *Engine) FileStat(ctx context.Context, id, path string) (*proto.FileInfo, error) {
	vm, err := e.getVM(id)
	if err != nil {
		return nil, err
	}

	vm.stateMu.Lock()
	if vm.Thermal != "hot" {
		vm.stateMu.Unlock()
		return nil, fmt.Errorf("sandbox %q is not hot (thermal=%s)", id, vm.Thermal)
	}
	ag := vm.Agent
	vm.stateMu.Unlock()

	return ag.FileStat(ctx, path)
}

func (e *Engine) FileList(ctx context.Context, id, path string) ([]proto.FileInfo, error) {
	vm, err := e.getVM(id)
	if err != nil {
		return nil, err
	}

	vm.stateMu.Lock()
	if vm.Thermal != "hot" {
		vm.stateMu.Unlock()
		return nil, fmt.Errorf("sandbox %q is not hot (thermal=%s)", id, vm.Thermal)
	}
	ag := vm.Agent
	vm.stateMu.Unlock()

	return ag.FileList(ctx, path)
}

// --- State extraction helpers (type-safe for JSON float64 / SQLite int) ---

func stateStr(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func stateInt64(m map[string]interface{}, key string) int64 {
	switch v := m[key].(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	case uint32:
		return int64(v)
	}
	return 0
}

func stateUint32(m map[string]interface{}, key string) uint32 {
	switch v := m[key].(type) {
	case int:
		return uint32(v)
	case int64:
		return uint32(v)
	case float64:
		return uint32(v)
	case uint32:
		return v
	}
	return 0
}

func stateBool(m map[string]interface{}, key string) bool {
	switch v := m[key].(type) {
	case bool:
		return v
	case int:
		return v != 0
	case float64:
		return v != 0
	}
	return false
}

// --- Helpers ---

func generateID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func generateMAC() string {
	b := make([]byte, 6)
	rand.Read(b)
	b[0] = (b[0] & 0xfe) | 0x02 // locally administered, unicast
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3], b[4], b[5])
}

// injectLoharIntoRootfs mounts the rootfs and overwrites /usr/local/bin/lohar
// with the current binary from DataDir. This ensures every sandbox uses the
// latest guest agent, preventing protocol drift after daemon upgrades.
// Adds ~50ms to sandbox creation (mount + cp + umount).
func injectLoharIntoRootfs(rootfsPath, dataDir string) error {
	loharSrc := filepath.Join(dataDir, "lohar")
	if _, err := os.Stat(loharSrc); err != nil {
		return nil // no lohar binary to inject (dev mode)
	}
	mnt, err := os.MkdirTemp("", "bhatti-inject-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(mnt)
	if err := exec.Command("mount", "-o", "loop", rootfsPath, mnt).Run(); err != nil {
		return fmt.Errorf("mount rootfs for lohar injection: %w", err)
	}
	defer exec.Command("umount", mnt).Run()
	dst := filepath.Join(mnt, "usr/local/bin/lohar")
	if err := exec.Command("cp", loharSrc, dst).Run(); err != nil {
		return fmt.Errorf("copy lohar: %w", err)
	}
	return os.Chmod(dst, 0755)
}

func copyRootfs(src, dst string) error {
	// Try CoW clone first (instant on btrfs/xfs)
	if err := exec.Command("cp", "--reflink=always", src, dst).Run(); err == nil {
		return nil
	}
	// Fallback: preserve sparsity to avoid materializing empty blocks
	return exec.Command("cp", "--sparse=always", src, dst).Run()
}

// fcAPIClient returns an HTTP client that talks to Firecracker's API over a Unix socket.
func fcAPIClient(socketPath string) *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.DialTimeout("unix", socketPath, 5*time.Second)
			},
			DisableKeepAlives: true, // one request per connection, avoids stale socket issues
		},
	}
}

func fcPut(client *http.Client, path, body string) error {
	req, _ := http.NewRequest("PUT", "http://localhost"+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("firecracker %s: %s %s", path, resp.Status, string(b))
	}
	return nil
}

func fcPatch(client *http.Client, path, body string) error {
	req, _ := http.NewRequest("PATCH", "http://localhost"+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("firecracker %s: %s %s", path, resp.Status, string(b))
	}
	return nil
}

// parseSSOutput is duplicated from docker/docker.go — extracts listening ports from `ss -tln` output.
func parseSSOutput(output string) []int {
	seen := map[int]bool{}
	var ports []int
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		addr := fields[3]
		idx := strings.LastIndex(addr, ":")
		if idx < 0 {
			continue
		}
		var p int
		fmt.Sscanf(addr[idx+1:], "%d", &p)
		if p > 0 && p < 65536 && !seen[p] {
			seen[p] = true
			ports = append(ports, p)
		}
	}
	return ports
}
