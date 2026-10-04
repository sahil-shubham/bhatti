package krucible

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent"
	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
	"github.com/sahil-shubham/bhatti/pkg/configdrive"
	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/gateway"
)

// Config holds paths and defaults for the krucible engine. All pure Go — the
// engine spawns the cgo `bhatti-vmm` helper and talks to lohar over sockets.
type Config struct {
	DataDir    string // sandboxes live under DataDir/sandboxes/<id>
	BaseRootfs string // host dir tree (virtiofs root): /init.krun=lohar + mountpoints
	// BaseImage is a prebuilt ext4 root image (e.g. from oci.PullAndConvert: a
	// real userland with /init.krun -> lohar). When set with BlockRoot, sandboxes
	// CoW-clone it directly instead of building one from BaseRootfs via mke2fs.
	// This is the production rootfs path.
	BaseImage string
	VMMBinary string // path to the bhatti-vmm helper (built with `make vmm`)
	LibDir    string // dir with libkrun/libkrunfw (DYLD_FALLBACK_LIBRARY_PATH / LD_LIBRARY_PATH)
	// SocketDir holds the per-VM vsock UDS. It must be SHORT: AF_UNIX paths cap
	// at ~104 bytes (macOS) / 108 (Linux), and macOS $TMPDIR/DataDir can be deep.
	// Empty defaults to /tmp/bhatti-kr.
	SocketDir     string
	DefaultVcpus  uint8
	DefaultMemMiB uint32
	// BlockRoot is required for power-off cold tier: the root image persists
	// when Stop kills the VM, and Start boots it again without preserving RAM.
	BlockRoot bool
	// KernelImage, if set, boots an external kernel (e.g. a lean one) instead of
	// libkrunfw's bundled kernel. Block-root only (the cmdline roots on
	// /dev/vda). arm64 = raw `Image`, x86 = ELF vmlinux.
	KernelImage string
	// NetdBinary is the bhatti-netd gateway helper (cmd/bhatti-netd). Required:
	// the guest's only network is a virtio-net device wired to its owner's
	// gateway, which polices egress and isolates the host. (libkrun's TSI needs
	// a patched guest kernel; the lean kernel isn't one.)
	NetdBinary string
}

// maxUnixPath is the conservative AF_UNIX sun_path cap (macOS = 104).
const maxUnixPath = 104

// Per-owner virtio-net gateway addressing. One bhatti-netd serves all of an
// owner's sandboxes on 100.64.<subnetIdx>.0/24 as an L2 switch: gw=.1, guests=
// .2, .3, ... Siblings on the same netd reach each other; different owners get
// separate netds (isolation). Sandboxes with no owner (UserID unset) get their
// own isolated netd, so single-sandbox behavior is unchanged.
const (
	netGatewayMAC = "52:54:00:00:00:01"
	netPrefixLen  = 24
)

func netGatewayIPFor(subnetIdx int) string { return fmt.Sprintf("100.64.%d.1", subnetIdx) }
func netGuestCIDRFor(subnetIdx, guestIdx int) string {
	return fmt.Sprintf("100.64.%d.%d/%d", subnetIdx, 2+guestIdx, netPrefixLen)
}
func netGuestMACFor(guestIdx int) string { return fmt.Sprintf("52:54:00:00:00:%02x", 2+guestIdx) }
func netGuestIPFor(subnetIdx, guestIdx int) string {
	return fmt.Sprintf("100.64.%d.%d", subnetIdx, 2+guestIdx)
}

// netdInstance is one owner's shared bhatti-netd gateway process. It is spawned
// detached (survives a daemon restart) and identified by pid so recovery can
// re-adopt it instead of respawning onto a socket it still holds.
type netdInstance struct {
	owner     string
	sock      string
	ctlSock   string
	dir       string
	subnetIdx int
	mu        sync.Mutex // guards cmd/pid (spawn-once)
	cmd       *exec.Cmd  // set when WE spawned it (nil when adopted across a restart)
	pid       int        // the running netd's pid (source of truth for alive/kill)
	nextGuest int
	refs      int
	brokerLn  net.Listener // credential broker socket served to this netd (broker.go); guarded by mu
	vmmGID    uint32       // group of the owner's confined helpers, the only one sock opens to (vmmuser.go); 0 = none
}

// netdDir is the deterministic per-owner directory (so recovery finds the same
// socket + state file the create path used).
func (e *Engine) netdDir(ownerKey string) string {
	h := sha256.Sum256([]byte(ownerKey))
	return filepath.Join(e.cfg.SocketDir, "netd-"+hex.EncodeToString(h[:6]))
}

// waitForSocket blocks until path exists (a listening UDS) or the deadline.
func waitForSocket(path string, d time.Duration) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("socket %s not present after %s", path, d)
}

// netdKeyFor is the key that groups sandboxes onto a shared bhatti-netd. Same
// owner (UserID) ⇒ same netd ⇒ siblings reach each other. No owner ⇒ keyed by
// sandbox id, so each unowned sandbox gets its own isolated netd (prior
// single-sandbox behavior is preserved).
func netdKeyFor(spec engine.SandboxSpec, id string) string {
	if spec.UserID != "" {
		return "u:" + spec.UserID
	}
	return "s:" + id
}

// acquireNetd returns (creating if needed) the owner's shared netd instance and
// reserves a guest slot on it, returning the instance and this guest's index.
// Release with releaseNetd on Destroy.
func (e *Engine) acquireNetd(ownerKey string, subnetIdx int) (*netdInstance, int) {
	e.netdMu.Lock()
	inst := e.netds[ownerKey]
	if inst == nil {
		dir := e.netdDir(ownerKey)
		inst = &netdInstance{owner: ownerKey, sock: filepath.Join(dir, "n.sock"), ctlSock: filepath.Join(dir, "ctl.sock"), dir: dir, subnetIdx: subnetIdx,
			vmmGID: e.netdGIDLocked(ownerKey)}
		e.netds[ownerKey] = inst
	}
	inst.refs++
	e.netdMu.Unlock()

	inst.mu.Lock()
	idx := inst.nextGuest
	inst.nextGuest++
	writeNetdRecord(inst) // persist address counter so recovery doesn't reissue a taken IP
	inst.mu.Unlock()
	return inst, idx
}

// releaseNetd drops one reference to the owner's netd, tearing the process down
// when the last sandbox of the owner is gone.
func (e *Engine) releaseNetd(ownerKey string) {
	e.netdMu.Lock()
	inst := e.netds[ownerKey]
	if inst == nil {
		e.netdMu.Unlock()
		return
	}
	inst.refs--
	done := inst.refs <= 0
	if done {
		delete(e.netds, ownerKey)
	}
	e.netdMu.Unlock()
	if !done {
		return
	}
	inst.mu.Lock()
	switch {
	case inst.cmd != nil && inst.cmd.Process != nil:
		_ = inst.cmd.Process.Kill()
		_, _ = inst.cmd.Process.Wait() // reap our child
	case inst.pid > 0 && isNetd(inst.pid, inst.sock):
		// Adopted across a restart, and still that netd (by now its pid could be
		// anyone's). Reap it if it's our child (same-process recovery / tests); a
		// no-op (ECHILD) in production where init re-parented it.
		_ = syscall.Kill(inst.pid, syscall.SIGKILL)
		var ws syscall.WaitStatus
		_, _ = syscall.Wait4(inst.pid, &ws, 0, nil)
	}
	inst.pid = 0
	if inst.brokerLn != nil {
		inst.brokerLn.Close()
		inst.brokerLn = nil
	}
	inst.mu.Unlock()
	os.RemoveAll(inst.dir)
}

// ensureNetd spawns the owner's bhatti-netd (LISTENING on inst.sock) if it is
// not already running. Idempotent: siblings and cold Start reuse a live gateway.
func (e *Engine) ensureNetd(ownerKey string) error {
	e.netdMu.Lock()
	inst := e.netds[ownerKey]
	broker := e.broker
	e.netdMu.Unlock()
	if inst == nil {
		return fmt.Errorf("netd instance %q not found", ownerKey)
	}
	inst.mu.Lock()
	defer inst.mu.Unlock()
	// Already running — we spawned it, or adopted it across a daemon restart.
	// A live netd that serves traffic (n.sock) but has no control socket
	// (ctl.sock) is an older binary that survived a daemon upgrade: it can never
	// receive per-sandbox egress policy, so replace it instead of silently
	// running guests on the open default.
	if inst.pid > 0 && isNetd(inst.pid, inst.sock) {
		_, sockErr := os.Stat(inst.sock)
		_, ctlErr := os.Stat(inst.ctlSock)
		if sockErr == nil && ctlErr == nil {
			serveBrokerLocked(inst, broker)
			return e.shareNetdSocket(inst)
		}
		if sockErr == nil {
			slog.Warn("krucible: adopted netd lacks control socket; respawning", "owner", ownerKey, "pid", inst.pid)
			_ = syscall.Kill(-inst.pid, syscall.SIGKILL)
			inst.pid = 0
		}
	}
	if err := os.MkdirAll(inst.dir, 0700); err != nil {
		return fmt.Errorf("netd dir: %w", err)
	}
	_ = os.Remove(inst.sock)
	_ = os.Remove(inst.ctlSock)
	lf, err := os.OpenFile(filepath.Join(inst.dir, "netd.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("netd log: %w", err)
	}
	defer lf.Close()
	// Everyone may search its directory (shareNetdSocket), so the log stays
	// the daemon's alone — also one an older daemon left readable.
	if err := lf.Chmod(0o600); err != nil {
		return fmt.Errorf("netd log: %w", err)
	}
	// netd's identity once confined is ours to name: the broker socket is
	// shared with exactly that uid/gid (broker.go).
	cmd := exec.Command(e.cfg.NetdBinary,
		"--net-uds", inst.sock, "--ctl-uds", inst.ctlSock,
		"--broker-uds", brokerSockPath(inst.dir),
		"--uid", fmt.Sprint(netdUID), "--gid", fmt.Sprint(netdGID),
		"--gw-ip", netGatewayIPFor(inst.subnetIdx),
		"--prefix", fmt.Sprintf("%d", netPrefixLen), "--mac", netGatewayMAC)
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start bhatti-netd: %w", err)
	}
	if werr := waitForSocket(inst.sock, 5*time.Second); werr != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		return fmt.Errorf("bhatti-netd not listening: %w", werr)
	}
	if werr := waitForSocket(inst.ctlSock, 5*time.Second); werr != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		return fmt.Errorf("bhatti-netd control socket not listening: %w", werr)
	}
	inst.cmd = cmd
	inst.pid = cmd.Process.Pid
	writeNetdRecord(inst) // persist pid+sock so recovery can re-adopt this netd
	serveBrokerLocked(inst, broker)
	return e.shareNetdSocket(inst)
}

// pushSandboxPolicy registers this VM's per-sandbox egress state with its
// owner's netd over the control UDS, retrying briefly since netd may have only
// just started listening. Returns an error when the policy could not be
// delivered, so the caller can fail closed rather than boot the guest on the
// open default. No-op on TSI (no netd IP).
func (e *Engine) pushSandboxPolicy(vm *VM) error {
	if vm.netIP == "" || vm.netdKey == "" {
		return nil
	}
	e.netdMu.Lock()
	inst := e.netds[vm.netdKey]
	e.netdMu.Unlock()
	if inst == nil || inst.ctlSock == "" {
		return fmt.Errorf("netd control socket unavailable for %s", vm.ID)
	}
	msg := gateway.ControlMsg{Op: gateway.ControlSet, GuestIP: vm.netIP, Sandbox: vm.brokerRef(), Policy: vm.netPolicy}
	c := gateway.NewControlClient(inst.ctlSock)
	defer c.Close()
	var lastErr error
	for range 20 {
		if lastErr = c.Send(msg); lastErr == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	slog.Warn("krucible: netd policy push failed", "sandbox", vm.ID, "ip", vm.netIP, "err", lastErr)
	return fmt.Errorf("netd policy push failed for %s: %w", vm.ID, lastErr)
}

// delSandboxPolicy removes this VM's state from netd (best-effort; the owner's
// netd may already be gone when this was the last sandbox).
func (e *Engine) delSandboxPolicy(vm *VM) {
	if vm.netIP == "" || vm.netdKey == "" {
		return
	}
	e.netdMu.Lock()
	inst := e.netds[vm.netdKey]
	e.netdMu.Unlock()
	if inst == nil || inst.ctlSock == "" {
		return
	}
	c := gateway.NewControlClient(inst.ctlSock)
	defer c.Close()
	_ = c.Send(gateway.ControlMsg{Op: gateway.ControlDel, GuestIP: vm.netIP})
}

// VM is per-sandbox state. The helper process IS the VM; we hold its cmd to
// stop it and the agent client to drive lohar.
type VM struct {
	mu sync.Mutex
	// launchMu serializes lifecycle transitions (Create's launch / Start / Stop /
	// Pause / Resume / Destroy) for this VM. Held for the whole transition so a
	// burst of concurrent wake-on-request calls (the public proxy + exec handlers
	// all call ensureHot uncoalesced) can't double-spawn the helper, racing on the
	// same vsock UDS paths and orphaning processes. Ordering rule: acquire
	// launchMu BEFORE mu, never the reverse (mu guards field reads/writes and is
	// also taken by read-only Status/List, which must not block on a transition).
	launchMu     sync.Mutex
	ID           string
	Name         string
	UserID       string
	SandboxDir   string
	SockDir      string
	ControlUDS   string // guest vsock 1024 (agent control)
	ForwardUDS   string // guest vsock 1025 (port forward)
	CtlSockUDS   string // VMM control socket (PAUSE/RESUME/SAVE/STATUS)
	MemMiB       uint32 // configured at boot (for ThermalEngine.MemSizeMib)
	Thermal      string // "hot" | "warm" | "cold"
	Token        string
	Agent        *agent.AgentClient
	AgentInfo    proto.AgentInfo // refreshed at every boot; never persisted across launches
	AgentInfoErr error           // a failed query is unknown, not a legacy guest
	Status       string          // "running" | "stopped"
	baseSpec     VMSpec          // the spec to (re-)launch with
	logPath      string
	HelperPID    int // bhatti-vmm pid, persisted so recovery can adopt/kill it after a daemon restart
	cmd          *exec.Cmd
	waitDone     <-chan error // cmd.Wait is owned by one goroutine, including during kill
	cancel       context.CancelFunc
	configSrv    *configServer          // host-side boot config server (§3.4); launchMu-guarded
	netdKey      string                 // owner key of the shared bhatti-netd (net backend); "" on TSI
	subnetIdx    int                    // owner's vnet subnet index (net backend); persisted for recovery
	netIP        string                 // guest IP on the netd gateway subnet (net backend); "" on TSI; reported in SandboxInfo + persisted for restart
	netPolicy    *gateway.NetPolicyWire // per-sandbox egress rules pushed to netd; nil = public; persisted, so a relaunch after a restart pushes it again
	sandboxRef   string                 // the server's ID for the sandbox (spec.SandboxID); "" for forks/restores
	vmmUID       uint32                 // the helper's own uid and primary gid (vmmuser.go); 0 = runs as the daemon; persisted
}

// brokerRef is how netd names this sandbox to the credential broker: the
// server's ID when the server created it, else ours (the broker maps a fork's
// or restore's engine ID back to the server's record).
func (vm *VM) brokerRef() string {
	if vm.sandboxRef != "" {
		return vm.sandboxRef
	}
	return vm.ID
}

// specPath is the spec a sandbox's helper is started on — its only argument,
// and so what tells that process apart from any other (isHelper).
func (vm *VM) specPath() string { return filepath.Join(vm.SandboxDir, "vmspec.json") }

// Engine implements engine.Engine on libkrun via the per-VM bhatti-vmm helper.
type Engine struct {
	mu        sync.RWMutex
	vms       map[string]*VM
	cfg       Config
	baseImgMu sync.Mutex               // guards the one-time base-image build
	netdMu    sync.Mutex               // guards netds and broker
	netds     map[string]*netdInstance // owner key → shared bhatti-netd gateway
	broker    engine.CredentialBroker  // served to every netd (broker.go); nil = no credential substitution
	caps      VMMCapabilities          // what the bhatti-vmm build supports (probed in New)
	// Confined helpers (vmmuser.go): confineVMM on Linux; dropVMM when the
	// daemon is root, which gives each helper its own uid from vmmIDs (mu).
	confineVMM, dropVMM bool
	vmmIDs              map[uint32]string // helper uid → sandbox id
	kvmGID              uint32            // group that may open /dev/kvm; 0 = anyone may
}

// errNoCheckpoint is returned by every operation that needs saved VM state
// when the VMM build has no checkpoint support.
var errNoCheckpoint = fmt.Errorf("%w: this bhatti-vmm build has no checkpoint support (snapshot, restore and fork are unavailable)", engine.ErrNotSupported)

// ThermalSupported reports whether the warm tier (live pause) is available.
// The server consults it before using the engine's thermal methods.
func (e *Engine) ThermalSupported() bool { return e.caps.Pause }

// errNoPause is returned by Pause when the VMM build can't pause a VM.
var errNoPause = fmt.Errorf("%w: this bhatti-vmm build can't pause a VM", engine.ErrNotSupported)

// probeCapabilities asks the VMM helper what it supports. A helper that can't
// answer is rejected: daemon and helper ship in one bundle, so a mismatch is
// a broken install, not something to guess around.
func probeCapabilities(cfg Config) (VMMCapabilities, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, cfg.VMMBinary, "capabilities")
	if cfg.LibDir != "" {
		cmd.Env = append(os.Environ(),
			"DYLD_FALLBACK_LIBRARY_PATH="+cfg.LibDir,
			"LD_LIBRARY_PATH="+cfg.LibDir,
		)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return VMMCapabilities{}, fmt.Errorf("krucible: %s capabilities: %w: %s", cfg.VMMBinary, err, strings.TrimSpace(stderr.String()))
	}
	var caps VMMCapabilities
	if err := json.Unmarshal(out, &caps); err != nil {
		return VMMCapabilities{}, fmt.Errorf("krucible: %s capabilities: bad output %q: %w", cfg.VMMBinary, out, err)
	}
	return caps, nil
}

var _ engine.Engine = (*Engine)(nil)

// New validates config and returns a krucible engine.
func New(cfg Config) (*Engine, error) {
	if cfg.VMMBinary == "" {
		return nil, fmt.Errorf("krucible: VMMBinary not set")
	}
	if _, err := os.Stat(cfg.VMMBinary); err != nil {
		return nil, fmt.Errorf("krucible: vmm helper not found at %s (run `make vmm`): %w", cfg.VMMBinary, err)
	}
	// bhatti-vmm boots an external kernel from a block root. libkrunfw's bundled
	// kernel, and the virtio-fs root it could boot, aren't used any more.
	if cfg.KernelImage == "" {
		return nil, fmt.Errorf("krucible: no kernel image (set krucible_kernel_image)")
	}
	if !cfg.BlockRoot {
		return nil, fmt.Errorf("krucible: block root required (set krucible_base_image or krucible_block_root)")
	}
	if cfg.NetdBinary == "" {
		return nil, fmt.Errorf("krucible: no bhatti-netd (set krucible_netd)")
	}
	if _, err := os.Stat(cfg.NetdBinary); err != nil {
		return nil, fmt.Errorf("krucible: bhatti-netd not found at %s: %w", cfg.NetdBinary, err)
	}
	// Need a rootfs source: a prebuilt block image (production) or a dir tree.
	if cfg.BaseImage != "" {
		if _, err := os.Stat(cfg.BaseImage); err != nil {
			return nil, fmt.Errorf("krucible: base image not found at %s: %w", cfg.BaseImage, err)
		}
	} else if cfg.BaseRootfs == "" {
		return nil, fmt.Errorf("krucible: set BaseImage or BaseRootfs")
	} else if _, err := os.Stat(cfg.BaseRootfs); err != nil {
		return nil, fmt.Errorf("krucible: base rootfs not found at %s: %w", cfg.BaseRootfs, err)
	}
	if cfg.DefaultVcpus == 0 {
		cfg.DefaultVcpus = 1
	}
	if cfg.DefaultMemMiB == 0 {
		cfg.DefaultMemMiB = 1024
	}
	if cfg.SocketDir == "" {
		cfg.SocketDir = "/tmp/bhatti-kr"
	}
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "sandboxes"), 0700); err != nil {
		return nil, fmt.Errorf("krucible: create data dir: %w", err)
	}
	if err := os.MkdirAll(cfg.SocketDir, 0700); err != nil {
		return nil, fmt.Errorf("krucible: create socket dir: %w", err)
	}
	shareSocketDir(cfg.SocketDir)
	caps, err := probeCapabilities(cfg)
	if err != nil {
		return nil, err
	}
	eng := &Engine{vms: make(map[string]*VM), netds: make(map[string]*netdInstance), cfg: cfg, caps: caps}
	if err := eng.initConfinement(); err != nil {
		return nil, err
	}
	eng.recover() // rehydrate live/dead sandboxes from <sandboxDir>/state.json
	return eng, nil
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

// agentFor returns the agent client for a running VM, or an error.
func (e *Engine) agentFor(id string) (*agent.AgentClient, error) {
	vm, err := e.getVM(id)
	if err != nil {
		return nil, err
	}
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if vm.Status != "running" {
		return nil, fmt.Errorf("sandbox %q is not running (status=%s)", id, vm.Status)
	}
	return vm.Agent, nil
}

// createOpts carries restore hints for create(): restore from a named checkpoint
// (snapshotDir), reuse its in-guest token (forcedToken), and clone its volumes.
type createOpts struct {
	snapshotDir    string
	forcedToken    string
	restoreVolumes []restoreVol // frozen volume images to clone in + re-attach (restore/fork)
}

// restoreVol is a frozen volume image (from a snapshot dir) to clone into a
// restored/forked sandbox and re-attach as /dev/vdc+, reproducing the snapshot's
// device set. An independent copy — libkrun block devices are boot-fixed, so the
// running source can't be CoW-rebased; cloneFile is instant on CoW FS (APFS/
// btrfs), a full copy elsewhere.
type restoreVol struct {
	path     string
	readOnly bool
}

// Create boots a new sandbox: prepare the rootfs (block image clone or
// virtio-fs dir), spawn bhatti-vmm, wait for lohar's agent over the bridged vsock.
func (e *Engine) Create(ctx context.Context, spec engine.SandboxSpec) (engine.SandboxInfo, error) {
	return e.create(ctx, spec, createOpts{})
}

func (e *Engine) create(ctx context.Context, spec engine.SandboxSpec, opts createOpts) (info engine.SandboxInfo, err error) {
	id, err := generateID()
	if err != nil {
		return info, err
	}
	sandboxDir := filepath.Join(e.cfg.DataDir, "sandboxes", id)
	sockDir := filepath.Join(e.cfg.SocketDir, id)

	var vm *VM
	var netdKey string // owner's shared-netd key; released on error / Destroy
	defer func() {
		if err != nil {
			if vm != nil {
				vm.kill()
				e.releaseVMM(vm)
			}
			if netdKey != "" {
				e.releaseNetd(netdKey)
			}
			os.RemoveAll(sandboxDir)
			os.RemoveAll(sockDir)
		}
	}()

	if err = os.MkdirAll(sandboxDir, 0700); err != nil {
		return info, fmt.Errorf("create sandbox dir: %w", err)
	}

	vcpus := e.cfg.DefaultVcpus
	if spec.CPUs >= 1 {
		vcpus = uint8(spec.CPUs)
	}
	memMiB := e.cfg.DefaultMemMiB
	if spec.MemoryMB > 0 {
		memMiB = uint32(spec.MemoryMB)
	}

	// Short UDS paths in a dedicated dir — sockaddr_un caps at ~104 bytes.
	controlUDS := filepath.Join(sockDir, "c.sock")
	forwardUDS := filepath.Join(sockDir, "f.sock")
	ctlSockUDS := filepath.Join(sockDir, "k.sock")
	configUDS := filepath.Join(sockDir, "cfg.sock")
	// A sandbox with egress "none" gets no NIC and no netd: everything the
	// platform does with it (exec, files, shell, published ports) goes over vsock.
	var netInst *netdInstance
	var netGuestIdx int
	netUDS := ""
	if !spec.NetPolicy.NoNetwork() {
		netdKey = netdKeyFor(spec, id)
		netInst, netGuestIdx = e.acquireNetd(netdKey, spec.SubnetIndex)
		netUDS = netInst.sock
	}
	if err = os.MkdirAll(sockDir, 0700); err != nil {
		return info, fmt.Errorf("create socket dir: %w", err)
	}
	for _, p := range []string{controlUDS, forwardUDS, ctlSockUDS, netUDS, configUDS} {
		if p != "" && len(p) >= maxUnixPath {
			return info, fmt.Errorf("vsock path too long (%d >= %d): %s — set a shorter SocketDir", len(p), maxUnixPath, p)
		}
	}

	baseSpec := VMSpec{
		Vcpus:           vcpus,
		MemMiB:          memMiB,
		Pid1:            true,
		ExecPath:        "/init.krun",
		VsockControlUDS: controlUDS,
		VsockForwardUDS: forwardUDS,
		LogLevel:        2,
	}
	if e.caps.Pause || e.caps.Checkpoint {
		baseSpec.ControlSocketUDS = ctlSockUDS
	}
	// With a network, the guest's eth0 is wired to its owner's bhatti-netd;
	// lohar configures it from cdNet.
	var netIP string
	var cdNet *configdrive.NetConfig
	if netInst != nil {
		baseSpec.NetUDS = netUDS
		baseSpec.NetMAC = netGuestMACFor(netGuestIdx)
		netIP = netGuestIPFor(netInst.subnetIdx, netGuestIdx)
		cdNet = &configdrive.NetConfig{
			IP:      netGuestCIDRFor(netInst.subnetIdx, netGuestIdx),
			Gateway: netGatewayIPFor(netInst.subnetIdx),
		}
	}

	name := spec.Name
	if name == "" {
		name = id
	}

	// Per-sandbox auth token, carried into the guest via the config drive; the
	// agent enforces it. On a memory-snapshot restore, reuse the snapshot's
	// token (the restored guest enforces it from RAM).
	token := opts.forcedToken

	// virtio-fs --mount binds: assign a per-mount tag; the VMM exposes each host
	// dir (krun_add_virtiofs3) and lohar mounts the tag at its guest path (carried
	// in the config drive). Live + shared, unlike an owned/versioned volume.
	var cdMounts []configdrive.FsMountConfig
	for i, m := range spec.Mounts {
		tag := fmt.Sprintf("mnt%d", i)
		baseSpec.Mounts = append(baseSpec.Mounts, VMFsMount{Tag: tag, HostPath: m.HostPath, ReadOnly: m.ReadOnly})
		cdMounts = append(cdMounts, configdrive.FsMountConfig{Tag: tag, Mount: m.GuestPath, ReadOnly: m.ReadOnly})
	}

	// Data volumes (create --volume / persistent): attach each resolved volume as a
	// block disk AFTER root (vda) — so /dev/vdb+ in order (the config drive is gone,
	// §3.4) — and tell lohar where to mount it. The libkrun get_block_cfg fix lets
	// add_disk2 compose with the root setter.
	var cdVolumes []configdrive.VolumeMountConfig
	for i, v := range spec.ResolvedVolumes {
		format := "raw"
		if isQcow2(v.FilePath) {
			format = "qcow2"
		}
		baseSpec.Volumes = append(baseSpec.Volumes, VMVolume{BlockID: fmt.Sprintf("vol%d", i), Path: v.FilePath, Format: format, ReadOnly: v.ReadOnly})
		cdVolumes = append(cdVolumes, configdrive.VolumeMountConfig{Device: fmt.Sprintf("/dev/vd%c", 'b'+rune(i)), Mount: v.Mount, FS: "ext4", ReadOnly: v.ReadOnly})
	}

	// Restore/fork: clone each frozen volume into this sandbox and re-attach it as
	// /dev/vdc+, reproducing the snapshot's device set (so a memory restore's RAM
	// view of its disks stays valid). Independent copy; the captured config drive
	// already carries the guest mount points, so no cdVolumes entry is needed.
	for _, rv := range opts.restoreVolumes {
		idx := len(baseSpec.Volumes)
		dst := filepath.Join(sandboxDir, fmt.Sprintf("restorevol%d.img", idx))
		if err = cloneFile(rv.path, dst); err != nil {
			return info, fmt.Errorf("restore volume %d: %w", idx, err)
		}
		format := "raw"
		if isQcow2(dst) {
			format = "qcow2"
		}
		baseSpec.Volumes = append(baseSpec.Volumes, VMVolume{BlockID: fmt.Sprintf("vol%d", idx), Path: dst, Format: format, ReadOnly: rv.readOnly})
	}

	var requiresGrowth bool
	// Rootfs + config drive. Block-root pairs root=/dev/vda with the config drive
	// at /dev/vdb; the virtio-fs path stays the minimal config-less dev profile.
	if e.cfg.BlockRoot {
		rootImg, rootFormat, rootBase, perr := e.prepareRootDisk(sandboxDir, spec)
		if perr != nil {
			return info, perr
		}
		// A larger block device does not enlarge ext4 without lohar's boot-time
		// resize. Reject only actual growth; a size at/below the base is safe.
		if spec.DiskSizeMB > 0 && !isQcow2(rootBase) {
			baseStat, statErr := os.Stat(rootBase)
			if statErr != nil {
				return info, fmt.Errorf("stat root base: %w", statErr)
			}
			requiresGrowth = int64(spec.DiskSizeMB)*1024*1024 > baseStat.Size()
		}
		slog.Debug("krucible root disk", "id", id, "root", rootImg, "base", rootBase)
		baseSpec.RootDisk = rootImg
		baseSpec.RootDiskFormat = rootFormat
		baseSpec.KernelImage = e.cfg.KernelImage // external (lean) kernel, if configured

		if token == "" {
			if token, err = genToken(); err != nil {
				return info, err
			}
		}
		// Config is fetched over vsock at boot (§3.4), not read from an on-disk
		// config drive: write it host-side as config.json (no mke2fs; never on a
		// guest disk). Restore reuses the saved in-guest token.
		cfg := buildSandboxConfig(id, name, token, spec, cdMounts, cdVolumes, cdNet)
		cfgJSON, merr := json.MarshalIndent(cfg, "", "  ")
		if merr != nil {
			return info, fmt.Errorf("marshal config: %w", merr)
		}
		if err = os.WriteFile(filepath.Join(sandboxDir, "config.json"), cfgJSON, 0600); err != nil {
			return info, fmt.Errorf("write config.json: %w", err)
		}
		baseSpec.VsockConfigUDS = configUDS
	}

	vm = &VM{
		ID: id, Name: name, UserID: spec.UserID,
		SandboxDir: sandboxDir, SockDir: sockDir,
		ControlUDS: controlUDS, ForwardUDS: forwardUDS, CtlSockUDS: ctlSockUDS,
		MemMiB: memMiB, Thermal: "hot", Status: "stopped", Token: token,
		baseSpec:   baseSpec,
		logPath:    filepath.Join(sandboxDir, "vmm.log"),
		netdKey:    netdKey,
		subnetIdx:  spec.SubnetIndex,
		netIP:      netIP,
		netPolicy:  spec.NetPolicy,
		sandboxRef: spec.SandboxID,
	}

	if opts.snapshotDir != "" && !e.caps.Checkpoint {
		return info, errNoCheckpoint
	}
	if err = e.launch(ctx, vm, opts.snapshotDir); err != nil {
		return info, err
	}
	if spec.RequireGuestCA {
		if err = vm.requireFeature(proto.FeatureSandboxCA); err != nil {
			return info, fmt.Errorf("create with secret grants: %w", err)
		}
	}
	if requiresGrowth {
		if err = vm.requireFeature(proto.FeatureRootGrowth); err != nil {
			return info, fmt.Errorf("create with disk-size: %w", err)
		}
	}

	// Fork/restore reconcile: a memory restore brings the guest back with the
	// SOURCE's IP still configured in eth0 (lohar configured it once, at the
	// boot the snapshot froze). The host allocated a FRESH identity above
	// (cdNet), so re-point the restored guest's eth0 to it — otherwise every
	// fork of one source collides on the source's IP on the shared netd. Only
	// on the restore path; a normal create's guest already configured itself
	// correctly from the config drive at boot.
	if opts.snapshotDir != "" && cdNet != nil {
		nctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = vm.Agent.NetConfig(nctx, cdNet.IP, cdNet.Gateway)
		cancel()
		if err != nil {
			return info, fmt.Errorf("fork: reconcile guest network identity: %w", err)
		}
	}

	e.mu.Lock()
	e.vms[id] = vm
	e.mu.Unlock()

	slog.Info("krucible sandbox created", "id", id, "name", name, "vcpus", vcpus, "mem_mib", memMiB, "block_root", e.cfg.BlockRoot)
	return engine.SandboxInfo{ID: id, Name: name, Status: "running", EngineID: id, IP: vm.netIP}, nil
}

// launch spawns the bhatti-vmm helper for vm and waits for the agent. When
// snapshotDir is non-empty the helper restores from that checkpoint directory
// instead of booting. Sets vm.cmd/cancel/Agent/Status on success.
func (e *Engine) launch(ctx context.Context, vm *VM, snapshotDir string) (err error) {
	spec := vm.baseSpec
	spec.SnapshotDir = snapshotDir
	specPath := vm.specPath()
	// On Linux the helper runs confined (vmmuser.go): it is handed the spec it
	// finds its files by, and the policy that holds it to them.
	policy := ""
	if e.confineVMM {
		var done func()
		var err error
		if spec, policy, done, err = e.confineLaunch(vm, spec, specPath); err != nil {
			return fmt.Errorf("confine vmm helper: %w", err)
		}
		defer done()
	}
	specBytes, _ := json.MarshalIndent(spec, "", "  ")
	if err := os.WriteFile(specPath, specBytes, 0600); err != nil {
		return fmt.Errorf("write vmspec: %w", err)
	}
	if err := e.handToVMM(vm, specPath, false, 0o640); err != nil {
		return fmt.Errorf("write vmspec: %w", err)
	}

	// Remove any stale UDS from a prior incarnation so libkrun can re-bind them
	// (a cold Start re-launches into the same socket dir).
	for _, p := range []string{vm.ControlUDS, vm.ForwardUDS, vm.CtlSockUDS} {
		_ = os.Remove(p)
	}

	logFile, err := os.OpenFile(vm.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("open vmm log: %w", err)
	}
	defer logFile.Close()

	// virtio-net gateway: ensure the owner's shared bhatti-netd is LISTENING on
	// the net UDS before the VMM connects to it (spawned once per owner; siblings
	// reuse it). The VMM will connect and netd will add it as a switch port.
	if vm.netdKey != "" {
		if nerr := e.ensureNetd(vm.netdKey); nerr != nil {
			return nerr
		}
		if perr := e.pushSandboxPolicy(vm); perr != nil && vm.netPolicy != nil {
			// A policy was explicitly requested but couldn't be delivered to
			// netd — fail closed rather than boot the guest on the open default.
			return fmt.Errorf("enforce egress policy: %w", perr)
		}
	}

	// Serve the boot config over the guest→host config vsock (§3.4). Must be
	// listening before the helper starts, since lohar dials it early in boot; a
	// cold re-launch replaces any prior server.
	vm.closeConfigSrv()
	if spec.VsockConfigUDS != "" {
		_ = os.Remove(spec.VsockConfigUDS)
		cfgJSON, rerr := os.ReadFile(filepath.Join(vm.SandboxDir, "config.json"))
		if rerr != nil {
			return fmt.Errorf("read config.json: %w", rerr)
		}
		srv, serr := newConfigServer(spec.VsockConfigUDS, cfgJSON)
		if serr != nil {
			return fmt.Errorf("config server: %w", serr)
		}
		vm.configSrv = srv
		// The helper connects to it for the guest.
		if err := e.handToVMM(vm, spec.VsockConfigUDS, true, 0o600); err != nil {
			vm.closeConfigSrv()
			return fmt.Errorf("config server: %w", err)
		}
	}

	vmCtx, vmCancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(vmCtx, e.cfg.VMMBinary, specPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Its own process group, out of the daemon's: the helper outlives every
	// daemon restart, and the next daemon adopts it (recover). (darwin + linux.)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if e.cfg.LibDir != "" {
		cmd.Env = append(os.Environ(),
			"DYLD_FALLBACK_LIBRARY_PATH="+e.cfg.LibDir,
			"LD_LIBRARY_PATH="+e.cfg.LibDir,
		)
	}
	if policy != "" {
		if cmd.Env == nil {
			cmd.Env = os.Environ()
		}
		cmd.Env = append(cmd.Env, VMMPolicyEnv+"="+policy)
	}
	if err := cmd.Start(); err != nil {
		vmCancel()
		vm.closeConfigSrv()
		return fmt.Errorf("start vmm helper: %w", err)
	}
	// Named in state.json before the guest is up: a daemon that goes away
	// mid-launch leaves a helper the next one has to find, to kill (recover).
	vm.mu.Lock()
	vm.HelperPID = cmd.Process.Pid
	vm.persistLocked()
	vm.mu.Unlock()
	defer func() {
		if err != nil {
			vm.mu.Lock()
			vm.HelperPID = 0
			vm.persistLocked()
			vm.mu.Unlock()
		}
	}()

	// One goroutine owns Wait for the process's entire lifetime. Racing a
	// separate Process.Wait in kill against cmd.Wait can corrupt reaping; the
	// result also lets a refused restore fail as soon as the helper exits.
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	ag := agent.NewKrucibleClient(vm.ControlUDS, vm.ForwardUDS, vm.Token)
	readyCtx, readyCancel := context.WithCancel(ctx)
	readyDone := make(chan error, 1)
	go func() { readyDone <- ag.WaitReady(readyCtx, 30*time.Second) }()
	var werr error
	select {
	case exitErr := <-waitDone:
		readyCancel()
		vmCancel()
		vm.closeConfigSrv()
		return fmt.Errorf("vmm helper exited before agent ready: %v\nvmm log:\n%s", exitErr, tailFile(vm.logPath, 4096))
	case werr = <-readyDone:
	}
	readyCancel()
	if werr != nil {
		_ = cmd.Process.Kill()
		<-waitDone
		// Leave the shared netd running; sibling sandboxes may still be using it.
		vmCancel()
		vm.closeConfigSrv()
		return fmt.Errorf("agent not ready: %w\nvmm log:\n%s", werr, tailFile(vm.logPath, 4096))
	}
	select {
	case exitErr := <-waitDone:
		vmCancel()
		vm.closeConfigSrv()
		return fmt.Errorf("vmm helper exited before agent ready: %v\nvmm log:\n%s", exitErr, tailFile(vm.logPath, 4096))
	default:
	}
	// lohar lives on the guest's disk, not in the daemon binary. A failed
	// capability probe must not prevent an ordinary boot or mislabel it old.
	agentInfo, infoErr := queryAgentInfo(ctx, ag)
	if infoErr != nil {
		slog.Warn("krucible.agent.info", "id", vm.ID, "error", infoErr)
	}
	vm.mu.Lock()
	vm.cmd = cmd
	vm.cancel = vmCancel
	vm.waitDone = waitDone
	vm.HelperPID = cmd.Process.Pid
	vm.Agent = ag
	vm.AgentInfo = agentInfo
	vm.AgentInfoErr = infoErr
	vm.Status = "running"
	vm.Thermal = "hot"
	vm.mu.Unlock()
	vm.persist()
	return nil
}

// kill terminates the helper and waits until it's gone: through the Cmd handle
// when we spawned it, else by the persisted pid of a helper adopted across a
// daemon restart — signalled only while it still runs this sandbox's spec.
func (vm *VM) kill() {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if vm.cmd != nil && vm.cmd.Process != nil {
		_ = vm.cmd.Process.Kill()
		<-vm.waitDone
	} else if vm.HelperPID > 0 {
		killHelper(vm.HelperPID, vm.specPath())
	}
	// The bhatti-netd gateway is shared per owner and outlives a single VM; it is
	// torn down by releaseNetd on Destroy of the owner's last sandbox.
	if vm.cancel != nil {
		vm.cancel()
	}
	vm.cmd = nil
	vm.cancel = nil
	vm.waitDone = nil
	vm.HelperPID = 0
	vm.closeConfigSrv()
}

// closeConfigSrv stops the boot config server (§3.4). Serialized with launch by
// launchMu; safe when none is running.
func (vm *VM) closeConfigSrv() {
	if vm.configSrv != nil {
		vm.configSrv.Close()
		vm.configSrv = nil
	}
}

// Destroy kills the helper and removes the sandbox dir.
func (e *Engine) Destroy(ctx context.Context, id string) error {
	vm, err := e.getVM(id)
	if err != nil {
		return err
	}
	vm.launchMu.Lock()
	defer vm.launchMu.Unlock()
	// kill() handles both an owned helper (vm.cmd) and one adopted across a daemon
	// restart (only vm.HelperPID set) — the inlined cmd-only kill here used to leak
	// the latter, leaving a live VM with its backing files deleted.
	vm.kill()
	e.releaseVMM(vm)
	vm.mu.Lock()
	dir := vm.SandboxDir
	sockDir := vm.SockDir
	vm.Status = "stopped"
	vm.mu.Unlock()

	e.mu.Lock()
	delete(e.vms, id)
	e.mu.Unlock()

	if vm.netdKey != "" {
		e.delSandboxPolicy(vm)
		e.releaseNetd(vm.netdKey)
	}

	os.RemoveAll(dir)
	os.RemoveAll(sockDir)
	slog.Info("krucible sandbox destroyed", "id", id)
	return nil
}

// Stop powers off the VM after a best-effort guest sync. The root disk
// persists; Start boots it fresh, without processes or files in tmpfs.
func (e *Engine) Stop(ctx context.Context, id string) error {
	vm, err := e.getVM(id)
	if err != nil {
		return err
	}
	vm.launchMu.Lock()
	defer vm.launchMu.Unlock()
	vm.mu.Lock()
	if vm.Status != "running" {
		vm.mu.Unlock()
		return nil
	}
	ag := vm.Agent
	vm.mu.Unlock()
	if ag != nil {
		sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		// Best effort: a guest that can't sync still gets powered off; ext4's
		// journal keeps the filesystem consistent, only unflushed writes are lost.
		if _, err := ag.Exec(sctx, []string{"sync"}, nil, ""); err != nil {
			slog.Warn("krucible stop: guest sync failed", "id", id, "error", err)
		}
		cancel()
	}
	vm.kill()
	vm.mu.Lock()
	vm.Status = "stopped"
	vm.Thermal = "cold"
	vm.Agent = nil
	vm.mu.Unlock()
	vm.persist()
	slog.Info("krucible sandbox stopped (powered off)", "id", id)
	return nil
}

// Start boots a stopped sandbox from its persisted root disk.
func (e *Engine) Start(ctx context.Context, id string) error {
	vm, err := e.getVM(id)
	if err != nil {
		return err
	}
	vm.launchMu.Lock()
	defer vm.launchMu.Unlock()
	vm.mu.Lock()
	if vm.Status == "running" {
		vm.mu.Unlock()
		return nil
	}
	vm.mu.Unlock()
	if err := e.launch(ctx, vm, ""); err != nil {
		return fmt.Errorf("start (fresh boot): %w", err)
	}
	slog.Info("krucible sandbox started", "id", id, "mode", "fresh boot")
	return nil
}

// hostSnapshotArch maps Go's GOARCH to the saved manifest's arch string.
func hostSnapshotArch() string {
	switch runtime.GOARCH {
	case "arm64":
		return "aarch64"
	case "amd64":
		return "x86_64"
	default:
		return runtime.GOARCH
	}
}

// genToken returns a random 128-bit hex token (matches the FC engine's scheme).
// A failed system RNG is surfaced, never silently swallowed — the token is a
// security boundary (the guest agent enforces it).
func genToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// buildSandboxConfig assembles the per-sandbox config lohar fetches over vsock at
// boot (§3.4). Secrets are pre-resolved into spec.Env by the server layer (same
// contract as the FC engine).
func buildSandboxConfig(id, name, token string, spec engine.SandboxSpec, mounts []configdrive.FsMountConfig, volumes []configdrive.VolumeMountConfig, net *configdrive.NetConfig) configdrive.SandboxConfig {
	files := make(map[string]configdrive.ConfigFile, len(spec.Files))
	for p, f := range spec.Files {
		files[p] = configdrive.ConfigFile{
			Content: base64.StdEncoding.EncodeToString(f.Content),
			Mode:    f.Mode,
		}
	}
	return configdrive.SandboxConfig{
		SandboxID: id,
		Hostname:  name,
		Token:     token,
		Env:       spec.Env,
		Files:     files,
		Mounts:    mounts,
		Volumes:   volumes,
		Net:       net,
		// Init: the once-after-boot command (create --init); lohar runs it as a
		// TTY session named "init", as the sandbox user.
		Init:   spec.Init,
		User:   "lohar",
		CACert: spec.CACert,
	}
}

// resolveBase returns the CoW backing image for a new sandbox's root: the
// per-create image (spec.BaseImage — set by the server for `image pull`,
// `image save`, snapshot), else the engine's default BaseImage, else a dev base
// built once from BaseRootfs. May be raw (a fresh base) or qcow2 (a saved image
// / snapshot, itself a CoW node over a raw base). A root names its backing by
// path for good, so this is the file a symlink (a tier name) leads to now,
// never the symlink an update re-points (bases.go).
func (e *Engine) resolveBase(spec engine.SandboxSpec) (string, error) {
	base := spec.BaseImage
	if base == "" {
		base = e.cfg.BaseImage
	}
	if base != "" {
		p, err := resolveBasePath(base)
		if err != nil {
			return "", fmt.Errorf("base image: %w", err)
		}
		return p, nil
	}
	base = filepath.Join(e.cfg.DataDir, "base.img")
	e.baseImgMu.Lock()
	defer e.baseImgMu.Unlock()
	if _, err := os.Stat(base); err != nil {
		if berr := buildBaseImage(e.cfg.BaseRootfs, base); berr != nil {
			return "", berr
		}
	}
	return base, nil
}

// prepareRootDisk makes a new sandbox's root disk in sandboxDir from its
// base, returning the disk, its format ("qcow2", or "" for raw) and the base.
// It holds the bases lock shared from picking the base until the disk names
// it, so GC can't remove the base in between.
func (e *Engine) prepareRootDisk(sandboxDir string, spec engine.SandboxSpec) (root, format, base string, err error) {
	unlock, err := lockBases(e.cfg.DataDir, false)
	if err != nil {
		return "", "", "", fmt.Errorf("lock base images: %w", err)
	}
	defer unlock()
	if base, err = e.resolveBase(spec); err != nil {
		return "", "", "", err
	}
	if !rootQcow2() {
		root = filepath.Join(sandboxDir, "root.img")
		if err := cloneFile(base, root); err != nil {
			return "", "", "", fmt.Errorf("clone base image: %w", err)
		}
		if err := growFile(root, spec.DiskSizeMB); err != nil {
			return "", "", "", fmt.Errorf("disk size: %w", err)
		}
		return root, "", base, nil
	}
	// Default: a qcow2 CoW root — instant + host-FS-independent (no
	// reflink/btrfs). Raw is the opt-out (KRUCIBLE_ROOT_RAW=1).
	root = filepath.Join(sandboxDir, "root.qcow2")
	if isQcow2(base) {
		// A saved qcow2 image is already a CoW node over the raw base;
		// copy it as this sandbox's root (it keeps backing that base).
		if spec.DiskSizeMB > 0 {
			return "", "", "", fmt.Errorf("disk size: not supported for sandboxes created from a saved image")
		}
		if err := cloneFile(base, root); err != nil {
			return "", "", "", fmt.Errorf("clone qcow2 image: %w", err)
		}
	} else if err := e.createRootOverlayQcow2(root, base, spec.DiskSizeMB); err != nil {
		return "", "", "", fmt.Errorf("create qcow2 root overlay: %w", err)
	}
	return root, "qcow2", base, nil
}

// isQcow2 reports whether path is a qcow2 image (magic "QFI\xfb"), so a saved
// image/snapshot (a CoW node) is copied as a root rather than overlaid as a raw
// backing.
func isQcow2(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return false
	}
	return magic == [4]byte{'Q', 'F', 'I', 0xfb}
}

// rootQcow2 reports whether to boot the block root from a qcow2 CoW overlay
// (the default — host-FS-independent CoW, no reflink/btrfs requirement) instead
// of a reflink-cloned raw ext4. Set KRUCIBLE_ROOT_RAW=1 to force the raw path
// (the native-perf / mountable escape hatch, until `flatten` lands).
func rootQcow2() bool { return os.Getenv("KRUCIBLE_ROOT_RAW") != "1" }

// createRootOverlayQcow2 creates a qcow2 CoW overlay over the shared base ext4
// at dst (the per-sandbox root): instant and host-FS-independent. Its virtual
// size is the base's, or diskMB if that's larger; the guest grows its
// filesystem into the extra space at boot (lohar growRoot). The overlay header
// is written directly (createQcow2Overlay); the VMM's qcow2 driver (imago)
// opens it at boot.
func (e *Engine) createRootOverlayQcow2(dst, base string, diskMB int) error {
	fi, err := os.Stat(base)
	if err != nil {
		return fmt.Errorf("stat base image %s: %w", base, err)
	}
	size := max(uint64(fi.Size()), uint64(diskMB)<<20)
	return createQcow2Overlay(dst, base, size)
}

// growFile extends a raw root image to diskMB (sparse) if that's larger than
// it already is. It never shrinks: a filesystem can't be cut under the guest.
func growFile(path string, diskMB int) error {
	if diskMB <= 0 {
		return nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if want := int64(diskMB) << 20; want > fi.Size() {
		return os.Truncate(path, want)
	}
	return nil
}

func (e *Engine) Status(ctx context.Context, id string) (engine.SandboxInfo, error) {
	vm, err := e.getVM(id)
	if err != nil {
		return engine.SandboxInfo{}, err
	}
	vm.mu.Lock()
	defer vm.mu.Unlock()
	return engine.SandboxInfo{ID: vm.ID, Name: vm.Name, Status: vm.Status, EngineID: vm.ID, IP: vm.netIP}, nil
}

func (e *Engine) List(ctx context.Context) ([]engine.SandboxInfo, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]engine.SandboxInfo, 0, len(e.vms))
	for _, vm := range e.vms {
		vm.mu.Lock()
		out = append(out, engine.SandboxInfo{ID: vm.ID, Name: vm.Name, Status: vm.Status, EngineID: vm.ID, IP: vm.netIP})
		vm.mu.Unlock()
	}
	return out, nil
}

// Shutdown is the engine's part of a daemon shutdown, and it stops no sandbox:
// helpers and netds are detached processes that outlive the daemon, and the
// next one adopts them (recover), so a restart — an upgrade, `systemctl
// restart`, a cert-renewal hook — never reboots a guest. It waits out in-flight
// lifecycle transitions, which persist what they did, and closes what the
// daemon serves the sandboxes: boot config and credential broker sockets.
func (e *Engine) Shutdown() {
	e.mu.RLock()
	vms := make([]*VM, 0, len(e.vms))
	for _, vm := range e.vms {
		vms = append(vms, vm)
	}
	e.mu.RUnlock()
	for _, vm := range vms {
		vm.launchMu.Lock()
		vm.closeConfigSrv()
		vm.launchMu.Unlock()
	}
	e.netdMu.Lock()
	insts := make([]*netdInstance, 0, len(e.netds))
	for _, inst := range e.netds {
		insts = append(insts, inst)
	}
	e.netdMu.Unlock()
	for _, inst := range insts {
		inst.mu.Lock()
		if inst.brokerLn != nil {
			inst.brokerLn.Close()
			inst.brokerLn = nil
		}
		inst.mu.Unlock()
	}
}

// --- helpers ---

func generateID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return fmt.Sprintf("%x", b), nil
}

// buildBaseImage builds an ext4 image populated from srcDir (the rootfs tree)
// via `mke2fs -d` — no mount, no root. The image is the shared base that each
// sandbox CoW-clones for its root disk.
func buildBaseImage(srcDir, dst string) error {
	// 1 GiB ceiling: the file is CoW-cloned per sandbox (cheap on APFS/reflink),
	// and ext4 only writes metadata for the populated tree.
	cmd := exec.Command("mke2fs", "-t", "ext4", "-d", srcDir, "-F", "-q", dst, "1024M")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mke2fs base image: %s: %w", out, err)
	}
	return nil
}

// cloneFile CoW-clones a file (APFS clonefile on darwin, reflink on linux),
// falling back to a plain copy.
func cloneFile(src, dst string) error {
	var primary *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		primary = exec.Command("cp", "-c", src, dst) // clonefile
	default:
		primary = exec.Command("cp", "--reflink=auto", src, dst)
	}
	if out, err := primary.CombinedOutput(); err != nil {
		fallback := exec.Command("cp", src, dst)
		if out2, err2 := fallback.CombinedOutput(); err2 != nil {
			return fmt.Errorf("clone image (%v: %s) and fallback (%v: %s) both failed",
				err, out, err2, out2)
		}
	}
	return nil
}

// tailFile returns up to the last n bytes of a file (best effort).
func tailFile(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	off := int64(0)
	if st.Size() > n {
		off = st.Size() - n
	}
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && len(buf) == 0 {
		return ""
	}
	return string(buf)
}

// LoharPath is the guest agent shipped beside bhatti-vmm in the runtime
// bundle (bin/lohar); image pull/import write it into converted images.
func (e *Engine) LoharPath() string {
	return filepath.Join(filepath.Dir(e.cfg.VMMBinary), "lohar")
}
