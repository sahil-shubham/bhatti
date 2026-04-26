//go:build linux

// Package firecracker implements engine.Engine using Firecracker microVMs.
// It talks directly to Firecracker's HTTP API over a Unix socket — no SDK needed.
package firecracker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent"
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

	// Ensure all base images in images/ have the current lohar binary.
	// The install script ships rootfs images with whatever lohar was baked
	// in at build time, which may lag the installed lohar after upgrades.
	// Injecting here (once per image per lohar version) means every
	// reflink copy during Create already has the right agent — skipping
	// the per-create mount+cp+umount (~80ms).
	ensureImagesHaveCurrentLohar(cfg.DataDir)

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

