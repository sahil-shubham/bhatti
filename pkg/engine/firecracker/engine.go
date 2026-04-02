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
	"syscall"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent"
	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// RateLimitConfig controls per-VM resource limits.
// Zero values mean "use defaults". Negative values disable the limiter.
type RateLimitConfig struct {
	NetBandwidthBytes int64 // bytes/s per direction (default: 12_500_000 = 100 Mbps)
	NetBurstBytes     int64 // one-time burst bytes (default: 10_000_000 = 10 MB)
	DiskBandwidthBytes int64 // bytes/s (default: 104_857_600 = 100 MB/s)
	DiskIOPS          int64 // ops/s (default: 10_000)
}

// Defaults are disabled (0) — rate limiters are opt-in. Configure in
// config.yaml when running multi-tenant or at scale.
func (r RateLimitConfig) netBandwidth() int64  { return r.NetBandwidthBytes }
func (r RateLimitConfig) netBurst() int64      { return r.NetBurstBytes }
func (r RateLimitConfig) diskBandwidth() int64  { return r.DiskBandwidthBytes }
func (r RateLimitConfig) diskIOPS() int64       { return r.DiskIOPS }

// Config holds paths and settings for a Firecracker engine.
type Config struct {
	DataDir    string // e.g. /var/lib/bhatti — sandboxes created under DataDir/sandboxes/
	KernelPath string // path to vmlinux binary
	BaseRootfs string // path to base rootfs.ext4
	FCBinary   string // path to firecracker binary
	RateLimits RateLimitConfig

	// Jailer (empty JailerBinary = bare mode, no isolation)
	JailerBinary string // path to jailer binary
	JailUID      int    // uid for jailed FC processes (e.g. 10000)
	JailGID      int    // gid for jailed FC processes (e.g. 10000)
}

// jailed returns true if the jailer is configured.
func (c Config) jailed() bool { return c.JailerBinary != "" }

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
	FCPathOrigin string // sandbox ID whose paths Firecracker has recorded internally
	Volumes     []VolumeAttachmentInfo // populated in Create, used by checkpoint
	Agent           *agent.AgentClient
	Status          string // "running", "stopped"
	Thermal         string // "hot", "warm", "cold"
	hasBaseSnapshot bool   // unused — Diff snapshots disabled. Kept for DB compat.
	restoreFailed   bool   // set on restore failure, blocks retries until --force
	restoreError    string // the error message for user display
	stderrBuf       *ringBuffer // last 64KB of FC stderr
	jailRoot        string      // chroot root dir (empty if bare mode)
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

	// NOTE: Do NOT clean up TAP devices here. recoverVMs hasn't run yet,
	// so we can't distinguish orphaned TAPs from ones needed by snapshotted
	// VMs. Call CleanupOrphanedTaps() after recovery instead.

	return eng, nil
}

// CleanupOrphanedTaps removes TAP devices that don't belong to any known VM.
// Must be called AFTER recoverVMs so that restored VMs' TAPs are preserved.
func (e *Engine) CleanupOrphanedTaps() {
	e.mu.RLock()
	known := make(map[string]bool, len(e.vms))
	for _, vm := range e.vms {
		if vm.TapDevice != "" {
			known[vm.TapDevice] = true
		}
	}
	e.mu.RUnlock()
	cleanupOrphanedTapDevices(known)
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

	// Kill any VMs still running (not already stopped by SnapshotAll).
	// VMs that were snapshotted are status="stopped" with cmd=nil — skip them.
	hasStoppedVMs := false
	for _, id := range ids {
		vm, err := e.getVM(id)
		if err != nil {
			continue
		}
		if vm.Status == "stopped" {
			hasStoppedVMs = true
			continue
		}
		if vm.Status == "running" && vm.cmd != nil {
			killFC(vm.cmd, 1*time.Second)
		}
	}

	// Only clean up TAP devices and bridges if no VMs were snapshotted.
	// Stopped VMs need their TAP alive for cold resume on next startup.
	// If some VMs failed to snapshot, their TAPs may linger — that's fine,
	// CleanupOrphanedTaps() on the next startup will remove them.
	if !hasStoppedVMs {
		cleanupAllTapDevices()
		cleanupAllUserBridges()
	}
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
		memMB = 2048
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

	// 5. Build path resolver for FC API calls
	jp := newJailPaths(e.cfg.jailed())

	// Resolve all paths FC will reference
	kernelPath := jp.resolve("kernel", e.cfg.KernelPath)
	rootfsRef := jp.resolve("rootfs.ext4", rootfsPath)
	configRef := jp.resolve("config.ext4", configDrivePath)
	vsockRef := jp.chrootPath("vsock.sock", vsockPath)
	logRef := jp.chrootPath("firecracker.log", filepath.Join(sandboxDir, "firecracker.log"))
	metricsRef := jp.chrootPath("firecracker.metrics", filepath.Join(sandboxDir, "firecracker.metrics"))

	// Start Firecracker process
	fcProc, err := e.startFC(socketPath, startFCOpts{
		id: id, vcpus: vcpuCount, memMB: memMB, files: jp.files,
	})
	if err != nil {
		return info, err
	}
	fcCmd = fcProc.cmd
	vmCancel = fcProc.cancel
	stderrBuf := fcProc.stderrBuf

	// In jailed mode, the API socket is inside the chroot
	apiSocket := socketPath
	if fcProc.socket != "" {
		apiSocket = fcProc.socket
	}

	// 6. Configure via HTTP API
	client := fcAPIClient(apiSocket)

	// FC logger and metrics must be set before any other configuration.
	// Warning level only — Debug is guest-influenceable. Non-fatal if setup fails.
	if err = fcPut(ctx, client, "/logger", fmt.Sprintf(
		`{"log_path":%q,"level":"Warning","show_level":true,"show_log_origin":true}`,
		logRef)); err != nil {
		slog.Warn("FC logger setup failed", "error", err)
	}
	if err = fcPut(ctx, client, "/metrics", fmt.Sprintf(
		`{"metrics_path":%q}`, metricsRef)); err != nil {
		slog.Warn("FC metrics setup failed", "error", err)
	}

	// Boot args include ip= for kernel-level network configuration.
	// Uses the user's bridge gateway instead of a hardcoded IP.
	bootArgs := fmt.Sprintf(
		"reboot=k panic=1 pci=off 8250.nr_uarts=0 init=/usr/local/bin/lohar quiet loglevel=0 ip=%s::%s:255.255.255.0::eth0:off:1.1.1.1:8.8.8.8:",
		guestIP, userNet.GatewayIP)

	if err = fcPut(ctx, client, "/boot-source", fmt.Sprintf(
		`{"kernel_image_path":%q,"boot_args":%q}`,
		kernelPath, bootArgs)); err != nil {
		return info, fmt.Errorf("set boot-source: %w", err)
	}

	rootfsDrive := fmt.Sprintf(`{"drive_id":"rootfs","path_on_host":%q,"is_root_device":true,"is_read_only":false`, rootfsRef)
	if bw := e.cfg.RateLimits.diskBandwidth(); bw > 0 {
		iops := e.cfg.RateLimits.diskIOPS()
		rootfsDrive += fmt.Sprintf(`,"rate_limiter":{"bandwidth":{"size":%d,"refill_time":1000},"ops":{"size":%d,"refill_time":1000}}`, bw, iops)
	}
	rootfsDrive += "}"
	if err = fcPut(ctx, client, "/drives/rootfs", rootfsDrive); err != nil {
		return info, fmt.Errorf("set drive: %w", err)
	}

	// track_dirty_pages is disabled — all snapshots are Full. This eliminates
	// Diff snapshot corruption (the rory incident) at the cost of ~500ms extra
	// per snapshot. With NVMe + btrfs this is negligible.
	// Disabling dirty page tracking also unlocks hugepages.
	hugePages := "None"
	if spec.Hugepages {
		hugePages = "2M"
	}
	if err = fcPut(ctx, client, "/machine-config", fmt.Sprintf(
		`{"vcpu_count":%d,"mem_size_mib":%d,"track_dirty_pages":false,"huge_pages":%q}`,
		vcpuCount, memMB, hugePages)); err != nil {
		return info, fmt.Errorf("set machine-config: %w", err)
	}

	// Entropy device — virtio-rng so guests don't block on getrandom().
	// 10 KB/s sustained, 8 KB initial burst for TLS handshakes / key generation.
	if err = fcPut(ctx, client, "/entropy",
		`{"rate_limiter":{"bandwidth":{"size":1024,"one_time_burst":8192,"refill_time":100}}}`); err != nil {
		slog.Warn("entropy device setup failed", "error", err) // non-fatal
	}

	// Balloon device — allows host to reclaim guest memory dynamically.
	// deflate_on_oom: guest reclaims memory when it needs it.
	// stats_polling_interval_s: FC collects balloon stats every 5s.
	// The thermal manager inflates the balloon on warm VMs to reclaim memory.
	if err = fcPut(ctx, client, "/balloon",
		`{"amount_mib":0,"deflate_on_oom":true,"stats_polling_interval_s":5}`); err != nil {
		slog.Warn("balloon device setup failed", "error", err) // non-fatal
	}

	if err = fcPut(ctx, client, "/vsock", fmt.Sprintf(
		`{"guest_cid":%d,"uds_path":%q}`, cid, vsockRef)); err != nil {
		return info, fmt.Errorf("set vsock: %w", err)
	}

	netPayload := fmt.Sprintf(`{"iface_id":"eth0","guest_mac":%q,"host_dev_name":%q`, mac, tapName)
	if bw := e.cfg.RateLimits.netBandwidth(); bw > 0 {
		burst := e.cfg.RateLimits.netBurst()
		netPayload += fmt.Sprintf(`,"rx_rate_limiter":{"bandwidth":{"size":%d,"one_time_burst":%d,"refill_time":1000}}`, bw, burst)
		netPayload += fmt.Sprintf(`,"tx_rate_limiter":{"bandwidth":{"size":%d,"one_time_burst":%d,"refill_time":1000}}`, bw, burst)
	}
	netPayload += "}"
	if err = fcPut(ctx, client, "/network-interfaces/eth0", netPayload); err != nil {
		return info, fmt.Errorf("set network: %w", err)
	}

	// 6b. Attach config drive as /dev/vdb
	if err = fcPut(ctx, client, "/drives/config", fmt.Sprintf(
		`{"drive_id":"config","path_on_host":%q,"is_root_device":false,"is_read_only":true}`,
		configRef)); err != nil {
		return info, fmt.Errorf("set config drive: %w", err)
	}

	// 6c. Attach persistent volume drives
	for _, vol := range spec.ResolvedVolumes {
		volRef := jp.resolve(fmt.Sprintf("vol-%s.ext4", vol.Name), vol.FilePath)
		if err = fcPut(ctx, client, fmt.Sprintf("/drives/%s", vol.DriveID), fmt.Sprintf(
			`{"drive_id":%q,"path_on_host":%q,"is_root_device":false,"is_read_only":%v}`,
			vol.DriveID, volRef, vol.ReadOnly)); err != nil {
			return info, fmt.Errorf("set persistent volume drive %s: %w", vol.DriveID, err)
		}
	}

	// 6d. Attach legacy ephemeral volume drives
	for i, vs := range spec.NewVolumes {
		volPath := filepath.Join(sandboxDir, fmt.Sprintf("vol-%s.ext4", vs.Name))
		ephVolRef := jp.resolve(fmt.Sprintf("ephvol-%s.ext4", vs.Name), volPath)
		driveID := fmt.Sprintf("ephvol%d", i)
		if err = fcPut(ctx, client, fmt.Sprintf("/drives/%s", driveID), fmt.Sprintf(
			`{"drive_id":%q,"path_on_host":%q,"is_root_device":false,"is_read_only":false}`,
			driveID, ephVolRef)); err != nil {
			return info, fmt.Errorf("set volume drive %d: %w", i, err)
		}
	}

	// 7. Boot
	if err = fcPut(ctx, client, "/actions", `{"action_type":"InstanceStart"}`); err != nil {
		return info, fmt.Errorf("start instance: %w", err)
	}

	// 8. Wait for agent via TCP (kernel ip= already configured eth0).
	agentClient := agent.NewTCPClientWithAuth(guestIP, token)
	if err = agentClient.WaitReady(ctx, 30*time.Second); err != nil {
		return info, fmt.Errorf("agent not ready: %w", err)
	}

	vm := &VM{
		ID: id, Name: name, UserID: spec.UserID,
		SocketPath: apiSocket,
		VsockPath: vsockPath, RootfsPath: rootfsPath,
		CID: cid, VcpuCount: vcpuCount, MemSizeMib: memMB,
		TapDevice: tapName, GuestIP: guestIP, GuestMAC: mac,
		Token: token, FCPathOrigin: id, Volumes: volAttachments,
		Agent: agentClient, Status: "running",
		Thermal: "hot", cancel: vmCancel, cmd: fcCmd,
		stderrBuf: stderrBuf, jailRoot: fcProc.jailRoot,
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
		pauseCtx, pauseCancel := context.WithTimeout(ctx, 5*time.Second)
		defer pauseCancel()
		if err := fcPatch(pauseCtx, client, "/vm", `{"state":"Paused"}`); err != nil {
			return fmt.Errorf("pause for save: %w", err)
		}
		vm.Thermal = "warm"
	}

	// Copy rootfs while VM is paused — no concurrent mutations possible
	if err := copyRootfs(vm.RootfsPath, destPath); err != nil {
		if !wasPaused {
			client := fcAPIClient(vm.SocketPath)
			resumeCtx, resumeCancel := context.WithTimeout(context.Background(), 5*time.Second)
			fcPatch(resumeCtx, client, "/vm", `{"state":"Resumed"}`)
			resumeCancel()
			vm.Thermal = "hot"
		}
		return fmt.Errorf("copy rootfs: %w", err)
	}

	if !wasPaused {
		client := fcAPIClient(vm.SocketPath)
		resumeCtx, resumeCancel := context.WithTimeout(ctx, 5*time.Second)
		defer resumeCancel()
		if err := fcPatch(resumeCtx, client, "/vm", `{"state":"Resumed"}`); err != nil {
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

	client := fcAPIClient(vm.SocketPath)

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
					slog.Error("move snapshot from chroot", "file", name, "error", err)
				}
			}
		}
	}

	// Lightweight sanity check on snapshot artifacts.
	if err := verifySnapshotArtifacts(vm.SnapVMPath, vm.SnapMemPath, vm.MemSizeMib, snapshotType); err != nil {
		slog.Error("snapshot sanity check failed", "sandbox", id, "error", err, "type", snapshotType)
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
	client := fcAPIClient(vm.SocketPath)
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
	client := fcAPIClient(vm.SocketPath)
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
	client := fcAPIClient(vm.SocketPath)
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

	// TAP device is kept alive across stop/start (not destroyed in Stop).
	// No need to recreate it.

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
	client := fcAPIClient(apiSocket)

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

	// Clean up jailer chroot and cgroup if jailed
	if e.cfg.jailed() {
		jailDir := filepath.Join(e.cfg.DataDir, "jails", "firecracker", id)
		os.RemoveAll(jailDir)
	}

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
		"fc_path_origin":    vm.FCPathOrigin,
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

	thermal := ""
	if status == "stopped" {
		thermal = "cold" // has snapshot on disk, can be resumed
	}

	vm := &VM{
		ID:          id,
		Name:        name,
		UserID:      userID,
		Status:      status,
		Thermal:     thermal,
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
		FCPathOrigin:    stateStr(state, "fc_path_origin"),
		hasBaseSnapshot: false, // Always reset on recovery — first post-recovery
		// snapshot will be Full, establishing a clean base. The persisted
		// has_base_snapshot may refer to a base that was overwritten.
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
	_, term, err := e.ShellSession(ctx, id)
	return term, err
}

// ShellSession opens a new TTY session and returns the session ID + terminal.
// Implements engine.ShellSessioner.
func (e *Engine) ShellSession(ctx context.Context, id string) (string, engine.TerminalConn, error) {
	vm, err := e.getVM(id)
	if err != nil {
		return "", nil, err
	}

	// Capture agent ref under lock, release before long-lived Shell call.
	vm.stateMu.Lock()
	if vm.Thermal != "hot" {
		vm.stateMu.Unlock()
		return "", nil, fmt.Errorf("sandbox %q is not hot (thermal=%s)", id, vm.Thermal)
	}
	ag := vm.Agent
	vm.stateMu.Unlock()

	info, term, err := ag.ShellSession(ctx, []string{"/bin/bash", "-li"}, map[string]string{
		"TERM": "xterm-256color",
	}, 24, 80, 3600) // 1 hour idle timeout for detached sessions
	if err != nil {
		return "", nil, err
	}
	return info.SessionID, term, nil
}

// ShellAttach reconnects to an existing TTY session by ID.
// Implements engine.SessionAttacher.
func (e *Engine) ShellAttach(ctx context.Context, id, sessionID string, ifDetached bool) (*proto.SessionInfo, engine.TerminalConn, error) {
	vm, err := e.getVM(id)
	if err != nil {
		return nil, nil, err
	}

	vm.stateMu.Lock()
	if vm.Thermal != "hot" {
		vm.stateMu.Unlock()
		return nil, nil, fmt.Errorf("sandbox %q is not hot (thermal=%s)", id, vm.Thermal)
	}
	ag := vm.Agent
	vm.stateMu.Unlock()

	return ag.SessionAttach(ctx, sessionID, ifDetached)
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

// verifySnapshotArtifacts performs lightweight sanity checks on snapshot files.
// Catches truncated files and zero-byte files without spawning a throwaway
// Firecracker process.
func verifySnapshotArtifacts(vmSnapPath, memSnapPath string, memSizeMib int64, snapshotType string) error {
	// vm.snap must exist and be non-empty.
	// Note: FC ≥1.14 uses a binary format for vm.snap, not JSON.
	vmInfo, err := os.Stat(vmSnapPath)
	if err != nil {
		return fmt.Errorf("stat vm.snap: %w", err)
	}
	if vmInfo.Size() == 0 {
		return fmt.Errorf("vm.snap is empty (0 bytes)")
	}

	// mem.snap size sanity
	memInfo, err := os.Stat(memSnapPath)
	if err != nil {
		return fmt.Errorf("stat mem.snap: %w", err)
	}
	expectedFull := memSizeMib * 1024 * 1024
	if snapshotType == "Full" && memInfo.Size() != expectedFull {
		return fmt.Errorf("Full snapshot mem.snap size %d != expected %d (VM memory)",
			memInfo.Size(), expectedFull)
	}
	if snapshotType == "Diff" && (memInfo.Size() == 0 || memInfo.Size() > expectedFull) {
		return fmt.Errorf("Diff snapshot mem.snap size %d out of range (0, %d]",
			memInfo.Size(), expectedFull)
	}

	return nil
}

// fcProcess holds the resources for a running Firecracker process.
type fcProcess struct {
	cmd       *exec.Cmd
	cancel    context.CancelFunc
	stderrBuf *ringBuffer
	socket    string    // host-visible API socket path
	jailRoot  string    // chroot root dir (empty if bare mode)
}

// startFCOpts are passed to startFC for jailed mode. Bare mode ignores them.
type startFCOpts struct {
	id       string // sandbox/VM ID
	vcpus    int64
	memMB    int64
	files    map[string]string // chroot filename → host path (hard-linked into jail)
}

// startFC launches a Firecracker process. In jailed mode, sets up the chroot,
// hard-links files, applies cgroups, and drops privileges.
func (e *Engine) startFC(socketPath string, opts startFCOpts) (*fcProcess, error) {
	if e.cfg.jailed() {
		return e.startFCJailed(socketPath, opts)
	}
	return e.startFCBare(socketPath)
}

// startFCBare launches FC directly (no isolation). Used in dev mode.
func (e *Engine) startFCBare(socketPath string) (*fcProcess, error) {
	if err := validateSocketPath(socketPath); err != nil {
		return nil, err
	}
	os.Remove(socketPath)
	vmCtx, cancel := context.WithCancel(context.Background())
	stderrBuf := newRingBuffer(64 * 1024)
	cmd := exec.CommandContext(vmCtx, e.cfg.FCBinary, "--api-sock", socketPath)
	cmd.Stderr = stderrBuf
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start firecracker: %w", err)
	}
	waitForSocket(socketPath)
	return &fcProcess{cmd: cmd, cancel: cancel, stderrBuf: stderrBuf, socket: socketPath}, nil
}

// startFCJailed launches FC inside a jailer chroot with UID drop, PID namespace,
// and cgroup limits.
func (e *Engine) startFCJailed(socketPath string, opts startFCOpts) (*fcProcess, error) {
	chrootBase := filepath.Join(e.cfg.DataDir, "jails")
	jailRoot := filepath.Join(chrootBase, "firecracker", opts.id, "root")
	if err := os.MkdirAll(jailRoot, 0700); err != nil {
		return nil, fmt.Errorf("create jail root: %w", err)
	}

	// Hard-link all files FC needs into the chroot.
	// Hard-links are instant (same filesystem) and FC writes go to the
	// same inode — no copy, no sync issue.
	for name, hostPath := range opts.files {
		dst := filepath.Join(jailRoot, name)
		os.Remove(dst) // remove stale from previous run
		if err := os.Link(hostPath, dst); err != nil {
			// Cross-device? Fall back to copy (e.g. kernel on different mount)
			if err := copyBlock(hostPath, dst); err != nil {
				os.RemoveAll(filepath.Join(chrootBase, "firecracker", opts.id))
				return nil, fmt.Errorf("link %s into jail: %w", name, err)
			}
		}
	}

	// chown the chroot tree to the jail user so FC can write
	uid, gid := e.cfg.JailUID, e.cfg.JailGID
	filepath.Walk(jailRoot, func(path string, info os.FileInfo, err error) error {
		if err == nil {
			os.Lchown(path, uid, gid)
		}
		return nil
	})

	// The API socket path as seen from inside the chroot.
	// Must be short to stay under the 108-byte Unix socket limit.
	internalSock := "api.sock"
	hostSock := filepath.Join(jailRoot, internalSock)
	os.Remove(hostSock)

	vmCtx, cancel := context.WithCancel(context.Background())
	stderrBuf := newRingBuffer(64 * 1024)

	args := []string{
		"--id", opts.id,
		"--exec-file", e.cfg.FCBinary,
		"--uid", fmt.Sprintf("%d", uid),
		"--gid", fmt.Sprintf("%d", gid),
		"--chroot-base-dir", chrootBase,
		// NOTE: --new-pid-ns is intentionally omitted. With --new-pid-ns,
		// the jailer's pivot_root makes the API socket inaccessible from
		// the host mount namespace. Chroot + UID drop + cgroups provide
		// the critical isolation. PID namespace can be revisited when we
		// add per-VM network namespaces (Phase 6b).
		"--cgroup-version", "2",
	}

	// cgroup limits — at least one --cgroup flag is REQUIRED with cgroup v2.
	// Without it, the jailer tries to move the process directly into
	// /sys/fs/cgroup/firecracker which hits "no internal process constraint".
	vcpus := opts.vcpus
	if vcpus < 1 {
		vcpus = 1
	}
	memMB := opts.memMB
	if memMB < 128 {
		memMB = 2048
	}
	args = append(args, "--cgroup", fmt.Sprintf("cpu.max=%d 100000", vcpus*100000))
	args = append(args, "--cgroup", fmt.Sprintf("memory.max=%d", (memMB+128)*1024*1024))

	// Everything after "--" is forwarded to firecracker
	args = append(args, "--", "--api-sock", internalSock)

	cmd := exec.CommandContext(vmCtx, e.cfg.JailerBinary, args...)
	cmd.Stderr = stderrBuf
	if err := cmd.Start(); err != nil {
		cancel()
		os.RemoveAll(filepath.Join(chrootBase, "firecracker", opts.id))
		return nil, fmt.Errorf("start jailer: %w", err)
	}

	// Wait for the API socket, but also detect early jailer/FC death.
	// If the process exits before the socket appears, we'd hang forever.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	socketReady := false
	for i := 0; i < 100; i++ {
		select {
		case err := <-done:
			// Process exited before socket appeared
			cancel()
			os.RemoveAll(filepath.Join(chrootBase, "firecracker", opts.id))
			return nil, fmt.Errorf("jailer exited prematurely: %v\nstderr: %s", err, stderrBuf.String())
		default:
		}
		if _, err := os.Stat(hostSock); err == nil {
			socketReady = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !socketReady {
		killFC(cmd, 1*time.Second)
		cancel()
		os.RemoveAll(filepath.Join(chrootBase, "firecracker", opts.id))
		return nil, fmt.Errorf("jailer: API socket not ready after 2s\nstderr: %s", stderrBuf.String())
	}

	return &fcProcess{
		cmd: cmd, cancel: cancel, stderrBuf: stderrBuf,
		socket: hostSock, jailRoot: jailRoot,
	}, nil
}

// waitForSocket polls for a Unix socket to appear (up to 1s).
func waitForSocket(path string) {
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// validateSocketPath checks that a Unix socket path fits within the 108-byte
// limit on Linux. A long DataDir could overflow silently.
func validateSocketPath(path string) error {
	if len(path) >= 108 {
		return fmt.Errorf("socket path too long (%d >= 108): %s "+
			"\u2014 use a shorter data_dir", len(path), path)
	}
	return nil
}

// killFC sends SIGTERM and waits up to timeout for clean exit, then SIGKILL.
// Firecracker may or may not handle SIGTERM in bare mode — the timeout
// ensures we don't block indefinitely before falling back to SIGKILL.
func killFC(cmd *exec.Cmd, timeout time.Duration) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		return
	case <-time.After(timeout):
		cmd.Process.Kill()
		<-done
	}
}

// copyBlock copies a block device file (rootfs, volume, snapshot artifact).
// Uses reflink for instant CoW clones on btrfs/xfs, falls back to sparse copy.
func copyBlock(src, dst string) error {
	return exec.Command("cp", "--reflink=auto", "--sparse=always", src, dst).Run()
}

// copyRootfs is an alias for copyBlock (kept for call-site readability).
func copyRootfs(src, dst string) error {
	return copyBlock(src, dst)
}

// fcAPIClient returns an HTTP client that talks to Firecracker's API over a Unix socket.
func fcAPIClient(socketPath string) *http.Client {
	return &http.Client{
		// No Timeout — each call site uses context.WithTimeout.
		// This avoids the old 10s global timeout silently racing with
		// per-call context deadlines (see issue #4).
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: 5 * time.Second}
				return d.DialContext(ctx, "unix", socketPath)
			},
			DisableKeepAlives: true, // one request per connection, avoids stale socket issues
		},
	}
}

func fcPut(ctx context.Context, client *http.Client, path, body string) error {
	req, _ := http.NewRequestWithContext(ctx, "PUT", "http://localhost"+path, strings.NewReader(body))
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

func fcPatch(ctx context.Context, client *http.Client, path, body string) error {
	req, _ := http.NewRequestWithContext(ctx, "PATCH", "http://localhost"+path, strings.NewReader(body))
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
