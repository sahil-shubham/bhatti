package krucible

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
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
	// DaemonListen lists API/proxy TCP listeners; netd protects their bind
	// addresses in addition to all host interface addresses.
	DaemonListen []string
	// Cgroups is how helpers are placed in cgroups: "auto" (also ""), "systemd"
	// or "none" (cgroup.go). The daemon passes the config's cgroups: value.
	Cgroups string
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

// netGuestSlots is how many guests share one owner's /24: .2 through .254
// (.0 is the network, .1 the gateway, .255 broadcast). A slot also fixes the
// guest's MAC, so slot 252 (MAC :fe) is the last one either can express.
const netGuestSlots = 253

// netGuestIndexFor recovers the slot a guest address was handed out from; false
// for no address, or one outside the slots.
func netGuestIndexFor(ip string) (int, bool) {
	addr, err := netip.ParseAddr(ip)
	if err != nil || !addr.Is4() {
		return 0, false
	}
	idx := int(addr.As4()[3]) - 2
	return idx, idx >= 0 && idx < netGuestSlots
}

// netdInstance is one owner's shared bhatti-netd gateway process. It is spawned
// detached (survives a daemon restart) and identified by pid so recovery can
// re-adopt it instead of respawning onto a socket it still holds.
type netdInstance struct {
	owner          string
	sock           string
	ctlSock        string
	dir            string
	subnetIdx      int
	mu             sync.Mutex // guards process handles, pid, broker, and guest counter
	cmd            *exec.Cmd
	waitDone       <-chan error  // one cmd.Wait goroutine, drained by watchNetd
	exitDone       chan struct{} // closed as soon as cmd.Wait returns, before cleanup
	stopping       bool          // deliberate replacement / last-sandbox removal
	pid            int
	watchedPID     int          // only one adopted death monitor per incarnation
	lostAtRecovery bool         // persisted pid was gone before startup adoption
	nextGuest      int          // where the next slot search starts (persisted)
	guests         map[int]bool // slots held by the owner's sandboxes, running or stopped
	refs           int
	brokerLn       net.Listener
	vmmGID         uint32
	scope          placedScope // the cgroup scope it runs in (cgroup.go); inst.mu
	scopePID       int         // the pid whose scope is settled: spawned placed, or adopted and checked
}

// netdDir is the deterministic per-owner directory (so recovery finds the same
// socket + state file the create path used).
func (e *Engine) netdDir(ownerKey string) string {
	return filepath.Join(e.cfg.SocketDir, "netd-"+ownerSlug(ownerKey))
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
func netdKeyFor(spec engine.SandboxSpec, id string) string { return ownerKeyFor(spec.UserID, id) }

// ownerKeyFor is the key an owner is known by to netd and to its cgroup slice:
// the user's, or the sandbox's own when it has none.
func ownerKeyFor(userID, sandboxID string) string {
	if userID != "" {
		return "u:" + userID
	}
	return "s:" + sandboxID
}

// acquireNetd returns (creating if needed) the owner's shared netd instance and
// reserves a free guest slot on it. A sandbox holds its slot from create until
// Destroy (a stopped one keeps its address), so an owner can have at most
// netGuestSlots sandboxes with a network at once. Release with releaseNetd.
func (e *Engine) acquireNetd(ownerKey string, subnetIdx int) (*netdInstance, int, error) {
	e.netdMu.Lock()
	inst := e.netds[ownerKey]
	if inst == nil {
		dir := e.netdDir(ownerKey)
		inst = &netdInstance{owner: ownerKey, sock: filepath.Join(dir, "n.sock"), ctlSock: filepath.Join(dir, "ctl.sock"), dir: dir, subnetIdx: subnetIdx,
			vmmGID: e.netdGIDLocked(ownerKey), guests: map[int]bool{}}
		e.netds[ownerKey] = inst
	}
	inst.refs++
	e.netdMu.Unlock()

	inst.mu.Lock()
	idx, ok := inst.claimGuestLocked()
	if ok {
		writeNetdRecord(inst) // persist the search position; held slots are rebuilt from VM records
	}
	subnet := inst.subnetIdx
	inst.mu.Unlock()
	if !ok {
		e.releaseNetd(ownerKey, -1)
		return nil, 0, fmt.Errorf("%w: this owner's sandboxes already hold all %d addresses on 100.64.%d.0/24 (stopped ones keep theirs); destroy sandboxes you no longer need",
			engine.ErrNetworkFull, netGuestSlots, subnet)
	}
	return inst, idx, nil
}

// claimGuestLocked reserves the first free slot at or after nextGuest, wrapping
// around. Starting after the last slot handed out keeps a just-freed address
// from being the next one reused. Caller holds inst.mu.
func (inst *netdInstance) claimGuestLocked() (int, bool) {
	if inst.guests == nil {
		inst.guests = map[int]bool{}
	}
	for i := range netGuestSlots {
		idx := (inst.nextGuest + i) % netGuestSlots
		if !inst.guests[idx] {
			inst.guests[idx] = true
			inst.nextGuest = idx + 1
			return idx, true
		}
	}
	return 0, false
}

// holdGuest marks a recovered sandbox's slot as taken.
func (inst *netdInstance) holdGuest(ip string) {
	idx, ok := netGuestIndexFor(ip)
	if !ok {
		return
	}
	inst.mu.Lock()
	if inst.guests == nil {
		inst.guests = map[int]bool{}
	}
	inst.guests[idx] = true
	inst.mu.Unlock()
}

// releaseNetd frees a guest slot (guestIdx < 0: none held) and drops one
// reference to the owner's netd, tearing the process down when the last
// sandbox of the owner is gone.
func (e *Engine) releaseNetd(ownerKey string, guestIdx int) {
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
		if guestIdx >= 0 {
			inst.mu.Lock()
			delete(inst.guests, guestIdx)
			inst.mu.Unlock()
		}
		return
	}
	inst.mu.Lock()
	inst.stopping = true
	switch {
	case inst.cmd != nil && inst.cmd.Process != nil:
		_ = inst.cmd.Process.Kill()
		if inst.exitDone != nil {
			<-inst.exitDone
		} else if inst.waitDone != nil {
			<-inst.waitDone
		}
	case inst.pid > 0 && isNetd(inst.pid, inst.sock):
		_ = syscall.Kill(inst.pid, syscall.SIGKILL)
		// Adopted gateways are not our children, except in same-process tests.
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
func (e *Engine) ensureNetd(ownerKey string) (retErr error) {
	e.netdMu.Lock()
	inst := e.netds[ownerKey]
	broker := e.broker
	e.netdMu.Unlock()
	if inst == nil {
		return fmt.Errorf("netd instance %q not found", ownerKey)
	}
	inst.mu.Lock()
	var lostReason string
	defer func() {
		inst.mu.Unlock()
		if lostReason != "" {
			e.emitNetworkLost(ownerKey, lostReason)
		}
	}()
	// libkrun's Unixstream holds its connected fd for the VM's lifetime
	// (no reconnect after EOF). Replacing an incompatible netd therefore
	// disconnects existing guests permanently until they are restarted. Kill it
	// FIRST so its obsolete rules cannot forward another packet; the fresh
	// gateway serves new sandboxes only. Report each affected running sandbox.
	var identity func(int) bool
	if inst.pid > 0 && inst.cmd == nil {
		sock := inst.sock
		identity = e.cg.adoptionIdentity(inst.pid, func(pid int) bool {
			return pidAlive(pid) && isNetd(pid, sock)
		})
	}
	if inst.pid > 0 && isNetd(inst.pid, inst.sock) {
		_, sockErr := os.Stat(inst.sock)
		_, ctlErr := os.Stat(inst.ctlSock)
		var versionErr error
		if sockErr == nil && ctlErr == nil {
			versionErr = gateway.ProbeVersion(inst.ctlSock)
			if versionErr == nil {
				serveBrokerLocked(inst, broker)
				if inst.cmd == nil {
					e.watchAdoptedNetdLocked(inst)
					e.adoptNetdScopeLocked(inst, identity)
				}
				return e.shareNetdSocket(inst)
			}
		} else {
			versionErr = fmt.Errorf("netd sockets unavailable (network: %v, control: %v)", sockErr, ctlErr)
		}
		if err := e.replaceNetdLocked(inst, versionErr); err != nil {
			return err // fail closed: never unlink sockets while old netd lives
		}
		lostReason = "bhatti-netd replaced after incompatible control protocol: " + versionErr.Error()
	} else if inst.pid > 0 {
		lostReason = "bhatti-netd exited or its pid was reused"
		e.netdExitedLocked(inst)
	} else {
		inst.cmd, inst.waitDone, inst.exitDone = nil, nil, nil
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
	args := []string{
		"--net-uds", inst.sock, "--ctl-uds", inst.ctlSock,
		"--broker-uds", brokerSockPath(inst.dir),
		"--uid", fmt.Sprint(netdUID), "--gid", fmt.Sprint(netdGID),
		"--gw-ip", netGatewayIPFor(inst.subnetIdx),
		"--prefix", fmt.Sprintf("%d", netPrefixLen), "--mac", netGatewayMAC,
	}
	for _, listen := range e.cfg.DaemonListen {
		if listen != "" {
			args = append(args, "--daemon-listen", listen)
		}
	}
	cmd := exec.Command(e.cfg.NetdBinary, args...)
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	ps, err := e.netdPlacementFor(inst)
	if err != nil {
		return fmt.Errorf("start bhatti-netd: %w", err)
	}
	logPath := filepath.Join(inst.dir, "netd.log")
	// netd.json names the gateway and its scope before the gateway can run, as a
	// VM's state.json does, so a daemon that dies from here leaves nothing unrecorded.
	var scope placedScope
	if err := e.cg.start(context.Background(), cmd, ps, func(pid int, unit string) {
		if pid == 0 {
			writeNetdRecord(inst) // placement failed: back to what the instance says
			return
		}
		scope = placedScope{unit: unit}
		writeNetdRecordFor(inst, pid, scope)
	}); err != nil {
		if tail := strings.TrimSpace(tailFile(logPath, 1024)); tail != "" {
			err = fmt.Errorf("%w; netd log: %s", err, tail)
		}
		return fmt.Errorf("start bhatti-netd: %w", err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	failed := func(cause error) error {
		err := netdStartFailed(cmd, waitDone, logPath, cause)
		writeNetdRecord(inst) // it is gone: the record names what the instance does
		return err
	}
	if werr := waitForSocket(inst.sock, 5*time.Second); werr != nil {
		return failed(fmt.Errorf("bhatti-netd not listening: %w", werr))
	}
	if werr := waitForSocket(inst.ctlSock, 5*time.Second); werr != nil {
		return failed(fmt.Errorf("bhatti-netd control socket not listening: %w", werr))
	}
	// The socket path appears before netd finishes confinement and starts its
	// accept loop. Give this newly spawned process time to answer, but never
	// accept an unverified policy engine if the handshake keeps failing.
	var verr error
	deadline := time.Now().Add(3 * time.Second)
	for {
		if verr = gateway.ProbeVersion(inst.ctlSock); verr == nil {
			break
		}
		if time.Now().After(deadline) {
			return failed(fmt.Errorf("bhatti-netd incompatible control/enforcement protocol: %w", verr))
		}
		time.Sleep(25 * time.Millisecond)
	}
	inst.cmd = cmd
	inst.pid = cmd.Process.Pid
	inst.scope, inst.scopePID = scope, inst.pid
	inst.waitDone = waitDone
	inst.exitDone = make(chan struct{})
	inst.stopping = false
	inst.watchedPID = inst.pid
	writeNetdRecord(inst) // persist pid+sock so recovery can re-adopt this netd
	e.watchNetd(inst, cmd, waitDone, inst.exitDone)
	serveBrokerLocked(inst, broker)
	return e.shareNetdSocket(inst)
}

// replaceNetdLocked stops a verified old netd before its sockets are reused.
// Caller holds inst.mu. Emission happens after releasing it in ensureNetd.
func (e *Engine) replaceNetdLocked(inst *netdInstance, reason error) error {
	slog.Error("krucible.netd_incompatible", "owner", inst.owner, "pid", inst.pid, "error", reason)
	inst.stopping = true
	if inst.cmd != nil && inst.cmd.Process != nil {
		if err := inst.cmd.Process.Kill(); err != nil && err != os.ErrProcessDone {
			inst.stopping = false
			return fmt.Errorf("stop incompatible netd: %w", err)
		}
		if inst.exitDone != nil {
			<-inst.exitDone
		} else if inst.waitDone != nil {
			<-inst.waitDone
		}
	} else {
		if err := syscall.Kill(inst.pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			inst.stopping = false
			return fmt.Errorf("stop incompatible netd %d: %w", inst.pid, err)
		}
		deadline := time.Now().Add(3 * time.Second)
		for isNetd(inst.pid, inst.sock) {
			var ws syscall.WaitStatus
			_, _ = syscall.Wait4(inst.pid, &ws, syscall.WNOHANG, nil)
			if !isNetd(inst.pid, inst.sock) {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("incompatible netd %d did not exit after SIGKILL", inst.pid)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	e.netdExitedLocked(inst)
	return nil
}

// pushSandboxPolicy sends this VM's policy before boot. A failed push aborts
// boot even if the policy was absent; an unregistered guest never gets egress.
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
	msg := gateway.ControlMsg{Op: gateway.ControlSet, GuestIP: vm.netIP, GuestMAC: vm.baseSpec.NetMAC,
		GuestToken: vm.baseSpec.NetToken, Sandbox: vm.brokerRef(), Policy: vm.netPolicy}
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
	// Lock order: launchMu before mu for VM transitions; never hold mu for
	// sockets, process waits or state.json I/O. e.mu/netdMu only protect maps,
	// and must not be held while taking launchMu or calling event handlers.
	// A death watcher publishes the reaped process before taking launchMu,
	// so a concurrent Stop cannot deadlock waiting on its exit.
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
	HelperPID    int         // bhatti-vmm pid, persisted so recovery can adopt/kill it after a daemon restart
	scope        placedScope // the cgroup scope HelperPID runs in (cgroup.go); persisted so recovery can check it
	cmd          *exec.Cmd
	waitDone     <-chan error  // cmd.Wait's result, owned by the death watcher
	exitDone     chan struct{} // watcher closes on reap before taking launchMu
	stopping     bool          // explicit kill owns this exit, not the watcher
	cancel       context.CancelFunc
	configSrv    *configServer          // host-side boot config server (§3.4); launchMu-guarded
	netdKey      string                 // owner key of the shared bhatti-netd (net backend); "" on TSI
	subnetIdx    int                    // owner's vnet subnet index (net backend); persisted for recovery
	netIP        string                 // guest IP on the netd gateway subnet (net backend); "" on TSI; reported in SandboxInfo + persisted for restart
	netPolicy    *gateway.NetPolicyWire // per-sandbox egress rules; nil on older recovered state means deny in netd
	sandboxRef   string                 // the server's ID for the sandbox (spec.SandboxID); "" for forks/restores
	vmmUID       uint32                 // the helper's own uid and primary gid (vmmuser.go); 0 = runs as the daemon; persisted

	createPending bool // safety record exists, but initial creation has not completed

	// Report an older restored agent without persisting this launch-only result.
	reseedUnsupported bool
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
	mu               sync.RWMutex
	vms              map[string]*VM
	cfg              Config
	baseImgMu        sync.Mutex               // guards the one-time base-image build
	netdMu           sync.Mutex               // guards netds and broker
	netds            map[string]*netdInstance // owner key → shared bhatti-netd gateway
	broker           engine.CredentialBroker  // served to every netd (broker.go); nil = no credential substitution
	eventMu          sync.Mutex
	onLifecycleEvent func(engine.LifecycleEvent)
	pendingEvents    []engine.LifecycleEvent
	monitorMu        sync.Mutex
	monitorStop      chan struct{} // closes on Shutdown; detached helpers keep running
	monitorStopped   bool
	caps             VMMCapabilities // what the bhatti-vmm build supports (probed in New)
	// Confined helpers (vmmuser.go): confineVMM on Linux; dropVMM when the
	// daemon is root, which gives each helper its own uid from vmmIDs (mu).
	confineVMM, dropVMM bool
	vmmIDs              map[uint32]string // helper uid → sandbox id
	kvmGID              uint32            // group that may open /dev/kvm; 0 = anyone may
	cg                  *cgroupManager    // places helpers in cgroups; nil places none (cgroup.go)
	// Node sweeps (nodes.go): at most one runs, and requests that arrive
	// during it fold into one more.
	sweepMu    sync.Mutex
	sweeping   bool
	sweepAgain bool
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
	unlockStorage, err := lockBases(cfg.DataDir, true)
	if err != nil {
		return nil, err
	}
	err = reconcileNodePublications(cfg.DataDir)
	if err == nil {
		err = cleanImageStages(cfg.DataDir)
	}
	unlockStorage()
	if err != nil {
		return nil, fmt.Errorf("krucible: reconcile storage publications: %w", err)
	}
	if err := eng.migrateNodeAccess(); err != nil {
		return nil, fmt.Errorf("krucible: migrate node access: %w", err)
	}
	// Before recovery, which places the helpers a previous daemon left unplaced.
	// A bhatti that asked for systemd and can't have it refuses to start here.
	cg, err := resolveCgroups(cfg.Cgroups, defaultCgroupHost())
	if err != nil {
		return nil, err
	}
	eng.cg = cg
	if st := cg.status(); st.Mode == engine.CgroupNone && st.Requested == "auto" {
		slog.Warn("krucible.cgroups", "mode", st.Mode, "requested", st.Requested, "reason", st.Reason) // helpers go unaccounted: worth noticing
	} else {
		slog.Info("krucible.cgroups", "mode", st.Mode, "requested", st.Requested, "reason", st.Reason)
	}
	if err := eng.recover(); err != nil {
		return nil, fmt.Errorf("krucible: recover sandboxes: %w", err)
	}
	// A crash can leave node links after their last sandbox or fork stage
	// disappeared, with no later Destroy to request reclamation.
	if removed, err := SweepNodes(cfg.DataDir, false); err != nil {
		slog.Warn("krucible.nodes.sweep_failed", "error", err)
	} else if len(removed) > 0 {
		slog.Info("krucible.nodes.swept", "removed", len(removed))
	}
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
// (snapshotDir), reuse its in-guest token (forcedToken), and re-attach its
// volumes.
type createOpts struct {
	snapshotDir    string
	forcedToken    string
	restoreVolumes []restoreVol // frozen volume images to layer over + re-attach (restore/fork)
	// nodeDisk is the snapshot disk the root is layered over, instead of an
	// image the spec names: a resume or fork makes the root an overlay over
	// it (nodes.go), so it costs a header whatever the snapshot holds.
	nodeDisk   string
	nodeFormat string
	// A private staged root can be moved at the depth cap instead of copied.
	nodeOwned bool
}

// restoreVol is a frozen volume image (from a snapshot dir) to re-attach to a
// restored/forked sandbox as /dev/vdc+, reproducing the snapshot's device set.
// The sandbox gets its own disk over it, so writes diverge from the snapshot
// and from its siblings; libkrun block devices are boot-fixed, so the running
// source can't be CoW-rebased, and the frozen copy it left is what is shared.
type restoreVol struct {
	path     string
	format   string
	owned    bool
	readOnly bool
}

// Create boots a new sandbox: prepare the rootfs (block image clone or
// virtio-fs dir), spawn bhatti-vmm, wait for lohar's agent over the bridged vsock.
func (e *Engine) Create(ctx context.Context, spec engine.SandboxSpec) (engine.SandboxInfo, error) {
	return e.create(ctx, spec, createOpts{})
}

func (e *Engine) create(ctx context.Context, spec engine.SandboxSpec, opts createOpts) (info engine.SandboxInfo, err error) {
	// Direct engine callers also get the server's no-NIC default; neither a
	// missing pointer nor an invalid wire policy may start an open guest.
	if spec.NetPolicy == nil {
		spec.NetPolicy = &gateway.NetPolicyWire{Default: gateway.PostureNone}
	}
	if err := gateway.ValidateWire(*spec.NetPolicy); err != nil {
		return info, fmt.Errorf("create sandbox net policy: %w", err)
	}
	// Reconciliation may remove an unrecorded directory only while no
	// creator can still be building its first durable VM record.
	unlock, err := lockBasesCtx(ctx, e.cfg.DataDir, false)
	if err != nil {
		return info, fmt.Errorf("create: lock nodes: %w", err)
	}
	defer unlock()
	id, err := generateID()
	if err != nil {
		return info, err
	}
	sandboxDir := filepath.Join(e.cfg.DataDir, "sandboxes", id)
	sockDir := filepath.Join(e.cfg.SocketDir, id)

	var vm *VM
	var netdKey string // owner's shared-netd key; released on error / Destroy
	netGuestIdx := -1  // the guest slot reserved on it
	defer func() {
		if err != nil {
			if vm != nil {
				vm.kill(e.cfg.VMMBinary)
				e.releaseVMM(vm)
			}
			if netdKey != "" {
				if vm != nil {
					e.delSandboxPolicy(vm) // also covers failed pushes with a lost ACK
				}
				e.releaseNetd(netdKey, netGuestIdx)
			}
			os.RemoveAll(sandboxDir)
			os.RemoveAll(sockDir)
			if opts.nodeDisk != "" || len(opts.restoreVolumes) > 0 {
				e.scheduleNodeSweep()
			}
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
	netUDS := ""
	if !spec.NetPolicy.NoNetwork() {
		key := netdKeyFor(spec, id)
		if netInst, netGuestIdx, err = e.acquireNetd(key, spec.SubnetIndex); err != nil {
			return info, err
		}
		netdKey = key
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
		baseSpec.NetToken, err = genNetToken()
		if err != nil {
			return info, err
		}
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
		// Persistent volumes are daemon-created raw disks; their guest-owned
		// bytes cannot select a format or authorize a backing file.
		baseSpec.Volumes = append(baseSpec.Volumes, VMVolume{BlockID: fmt.Sprintf("vol%d", i), Path: v.FilePath, Format: "raw", ReadOnly: v.ReadOnly})
		cdVolumes = append(cdVolumes, configdrive.VolumeMountConfig{Device: fmt.Sprintf("/dev/vd%c", 'b'+rune(i)), Mount: v.Mount, FS: "ext4", ReadOnly: v.ReadOnly})
	}

	// Restore/fork: re-attach each frozen volume as /dev/vdc+, reproducing the
	// snapshot's device set (so a memory restore's RAM view of its disks stays
	// valid), as a disk of this sandbox's own over it. The captured config
	// already carries the guest mount points, so no cdVolumes entry is needed.
	privateDisks := e.dropVMM && spec.UserID == ""
	if err = e.layerRestoredVolumes(ctx, sandboxDir, &baseSpec, opts.restoreVolumes, privateDisks); err != nil {
		return info, err
	}

	var requiresGrowth bool
	// Rootfs + config drive. Block-root pairs root=/dev/vda with the config drive
	// at /dev/vdb; the virtio-fs path stays the minimal config-less dev profile.
	if e.cfg.BlockRoot {
		var rootImg, rootFormat, rootBase string
		var rootBacking []diskLayer
		var perr error
		if opts.nodeDisk != "" {
			rootImg, rootFormat, rootBase, perr = e.prepareNodeRoot(ctx, sandboxDir, opts.nodeDisk, opts.nodeOwned, opts.nodeFormat, privateDisks)
		} else {
			rootImg, rootFormat, rootBase, rootBacking, perr = e.prepareRootDisk(ctx, sandboxDir, spec)
		}
		if perr != nil {
			return info, perr
		}
		// A larger block device does not enlarge ext4 without lohar's boot-time
		// resize. Reject only actual growth; a size at/below the base is safe.
		if spec.DiskSizeMB > 0 {
			baseStat, statErr := os.Stat(rootBase)
			if statErr != nil {
				return info, fmt.Errorf("stat root base: %w", statErr)
			}
			requiresGrowth = int64(spec.DiskSizeMB)*1024*1024 > baseStat.Size()
		}
		slog.Debug("krucible root disk", "id", id, "root", rootImg, "base", rootBase)
		baseSpec.RootDisk = rootImg
		baseSpec.RootDiskFormat = rootFormat
		baseSpec.Backing = rootBacking
		baseSpec.KernelImage = e.cfg.KernelImage // external (lean) kernel, if configured
		if opts.nodeDisk == "" {
			if err = e.validateDiskBacking(&VM{UserID: spec.UserID}, baseSpec); err != nil {
				return info, fmt.Errorf("authorize the disks' backing chains: %w", err)
			}
		}
		if err = e.recordDiskBacking(&baseSpec); err != nil {
			return info, fmt.Errorf("read the disks' backing chains: %w", err)
		}

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
		ID: id, Name: name, UserID: spec.UserID, createPending: true,
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

	// An exit racing Create's final checks and registration must wait until
	// the VM is visible to observers before its watcher publishes the death.
	vm.launchMu.Lock()
	defer vm.launchMu.Unlock()

	if opts.snapshotDir != "" && !e.caps.Checkpoint {
		return info, errNoCheckpoint
	}
	// A helper must never outlive a daemon without a complete disk/owner
	// safety record. Pending creates remain reclaimable until creation succeeds.
	vm.mu.Lock()
	initialRecord := vm.toRecordLocked()
	vm.mu.Unlock()
	if err = writeDaemonMetadata(stateFilePath(sandboxDir), initialRecord); err != nil {
		return info, fmt.Errorf("persist initial sandbox state: %w", err)
	}
	if err = e.launch(ctx, vm, opts.snapshotDir); err != nil {
		return info, err
	}
	reseedUnsupported := vm.reseedUnsupported
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

	// Fork/restore reconcile: a memory restore brings back the source's eth0 IP
	// and MAC in RAM. netd authenticates both against this fork's fresh identity,
	// so re-point the guest NIC before returning it. Normal boot already gets
	// the new identity from the VMM and config drive.
	if opts.snapshotDir != "" && cdNet != nil {
		if err = vm.requireFeature(proto.FeatureNetConfigMAC); err != nil {
			return info, fmt.Errorf("fork: reconcile guest network identity: %w", err)
		}
		nctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = vm.Agent.NetConfig(nctx, cdNet.IP, cdNet.Gateway, baseSpec.NetMAC)
		cancel()
		if err != nil {
			return info, fmt.Errorf("fork: reconcile guest network identity: %w", err)
		}
	}

	vm.mu.Lock()
	vm.createPending = false
	committedRecord := vm.toRecordLocked()
	vm.mu.Unlock()
	if err = writeDaemonMetadata(stateFilePath(sandboxDir), committedRecord); err != nil {
		return info, fmt.Errorf("commit sandbox state: %w", err)
	}

	e.mu.Lock()
	e.vms[id] = vm
	e.mu.Unlock()

	slog.Info("krucible sandbox created", "id", id, "name", name, "vcpus", vcpus, "mem_mib", memMiB, "block_root", e.cfg.BlockRoot)
	return engine.SandboxInfo{ID: id, Name: name, Status: "running", EngineID: id, IP: vm.netIP, GuestReseedUnsupported: reseedUnsupported}, nil
}

// launch spawns the bhatti-vmm helper for vm and waits for the agent. When
// snapshotDir is non-empty the helper restores from that checkpoint directory
// instead of booting. Sets vm.cmd/cancel/Agent/Status on success.
func (e *Engine) launch(ctx context.Context, vm *VM, snapshotDir string) (err error) {
	if vm.netdKey != "" && vm.baseSpec.NetToken == "" {
		// A stopped pre-upgrade VM has no authenticated NIC yet. The old
		// helper cannot reconnect; a fresh boot gets a fresh identity.
		vm.baseSpec.NetToken, err = genNetToken()
		if err != nil {
			return err
		}
	}
	spec := vm.baseSpec
	spec.SnapshotDir = snapshotDir
	specPath := vm.specPath()
	// On Linux the helper runs confined (vmmuser.go): it is handed the spec it
	// finds its files by, and the policy that holds it to them.
	policy := ""
	if e.confineVMM {
		var done func()
		var err error
		if spec, policy, done, err = e.confineLaunch(ctx, vm, spec, specPath); err != nil {
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
		if perr := e.pushSandboxPolicy(vm); perr != nil {
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
	cmd.WaitDelay = 5 * time.Second
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
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
	ps, err := e.placementFor(vm)
	if err != nil {
		vmCancel()
		vm.closeConfigSrv()
		return fmt.Errorf("start vmm helper: %w", err)
	}
	// The helper is named in state.json before the guest is up: a daemon that
	// goes away mid-launch leaves a helper the next one has to find, to kill
	// (recover). Placing it also records the scope it runs in, before the
	// helper can do anything.
	if err := e.cg.start(ctx, cmd, ps, func(pid int, unit string) {
		vm.mu.Lock()
		vm.HelperPID, vm.scope = pid, placedScope{unit: unit}
		vm.mu.Unlock()
		vm.persist()
	}); err != nil {
		vmCancel()
		vm.closeConfigSrv()
		if tail := vmmLogExcerpt(vm.logPath, 4096); tail != "" {
			err = fmt.Errorf("%w\nvmm log:\n%s", err, tail)
		}
		return fmt.Errorf("start vmm helper: %w", err)
	}
	defer func() {
		if err != nil {
			vm.mu.Lock()
			vm.HelperPID, vm.scope = 0, placedScope{}
			vm.mu.Unlock()
			vm.persist()
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
		return fmt.Errorf("vmm helper exited before agent ready: %v\nvmm log:\n%s", exitErr, vmmLogExcerpt(vm.logPath, 4096))
	case werr = <-readyDone:
	}
	readyCancel()
	if werr != nil {
		_ = cmd.Process.Kill()
		<-waitDone
		// Leave the shared netd running; sibling sandboxes may still be using it.
		vmCancel()
		vm.closeConfigSrv()
		return fmt.Errorf("agent not ready: %w\nvmm log:\n%s", werr, vmmLogExcerpt(vm.logPath, 4096))
	}
	select {
	case exitErr := <-waitDone:
		vmCancel()
		vm.closeConfigSrv()
		return fmt.Errorf("vmm helper exited before agent ready: %v\nvmm log:\n%s", exitErr, vmmLogExcerpt(vm.logPath, 4096))
	default:
	}
	// lohar lives on the guest's disk, not in the daemon binary. A failed
	// capability probe must not prevent an ordinary boot or mislabel it old.
	agentInfo, infoErr := queryAgentInfo(ctx, ag)
	if infoErr != nil {
		slog.Warn("krucible.agent.info", "id", vm.ID, "error", infoErr)
	}
	var reseedUnsupported bool
	if snapshotDir != "" {
		reseedUnsupported, err = reseedRestoredGuest(ctx, vm.ID, ag, agentInfo, infoErr)
		if err != nil {
			slog.Error("krucible.guest_reseed_failed", "id", vm.ID, "error", err)
			_ = cmd.Process.Kill()
			<-waitDone
			vmCancel()
			vm.closeConfigSrv()
			return fmt.Errorf("restore: %w: %w", engine.ErrGuestReseedFailed, err)
		}
		// The image keeps the wall-clock offset it was saved with.
		syncGuestClock(ctx, vm.ID, ag, agentInfo, infoErr)
	}
	reaped := make(chan struct{})
	vm.mu.Lock()
	vm.cmd = cmd
	vm.cancel = vmCancel
	vm.waitDone = waitDone
	vm.exitDone = reaped
	vm.stopping = false
	vm.HelperPID = cmd.Process.Pid
	vm.Agent = ag
	vm.AgentInfo = agentInfo
	vm.AgentInfoErr = infoErr
	vm.reseedUnsupported = reseedUnsupported
	vm.Status = "running"
	vm.Thermal = "hot"
	vm.mu.Unlock()
	vm.persist()
	e.watchVM(vm, cmd, waitDone, reaped)
	return nil
}

// kill terminates a helper that this daemon owns or adopted. The watcher owns
// cmd.Wait; signal under the short state lock, then wait without holding it.
func (vm *VM) kill(binary string) {
	vm.mu.Lock()
	cmd, waitDone, reaped := vm.cmd, vm.waitDone, vm.exitDone
	pid, cancel := vm.HelperPID, vm.cancel
	vm.stopping = true
	vm.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		if reaped != nil {
			<-reaped
		} else if waitDone != nil { // fixtures with a manually owned Cmd.Wait
			<-waitDone
		}
	} else if pid > 0 {
		killHelper(pid, vm.specPath(), binary)
	}
	if cancel != nil {
		cancel()
	}
	vm.mu.Lock()
	vm.cmd, vm.cancel, vm.waitDone, vm.exitDone = nil, nil, nil, nil
	vm.HelperPID, vm.scope = 0, placedScope{}
	vm.mu.Unlock()
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
	vm.kill(e.cfg.VMMBinary)
	e.releaseVMM(vm)
	vm.mu.Lock()
	dir := vm.SandboxDir
	sockDir := vm.SockDir
	layered := len(vm.baseSpec.Backing) > 0
	for _, v := range vm.baseSpec.Volumes {
		layered = layered || len(v.Backing) > 0
	}
	vm.Status = "stopped"
	vm.mu.Unlock()

	e.mu.Lock()
	delete(e.vms, id)
	e.mu.Unlock()

	if vm.netdKey != "" {
		e.delSandboxPolicy(vm)
		slot, ok := netGuestIndexFor(vm.netIP)
		if !ok {
			slot = -1
		}
		e.releaseNetd(vm.netdKey, slot)
	}

	os.RemoveAll(dir)
	os.RemoveAll(sockDir)
	if layered {
		// It may have been the last to hold a node.
		e.scheduleNodeSweep()
	}
	slog.Info("krucible sandbox destroyed", "id", id)
	return nil
}

// guestSyncTimeout bounds each guest-wide sync independently of the caller's
// deadline. Stopping a fleet with unresponsive agents must not take 30s/VM.
const guestSyncTimeout = 2 * time.Second

// UncleanStopError means the VM was powered off without a confirmed guest
// sync or thaw. Callers must record the stopped state and durability risk.
type UncleanStopError struct{ Reason error }

func (e *UncleanStopError) Error() string {
	return fmt.Sprintf("sandbox powered off without confirmed durability: %v", e.Reason)
}
func (e *UncleanStopError) Unwrap() error             { return e.Reason }
func (e *UncleanStopError) UncleanStopReason() string { return e.Reason.Error() }

// powerOffLocked handles an explicit, possibly unclean stop. The caller owns
// launchMu; neither killing the process nor persisting the record holds vm.mu.
func (e *Engine) powerOffLocked(vm *VM) {
	vm.kill(e.cfg.VMMBinary)
	vm.mu.Lock()
	vm.Status = "stopped"
	vm.Thermal = "cold"
	vm.Agent = nil
	vm.mu.Unlock()
	e.delSandboxPolicy(vm)
	vm.removeSockets()
	vm.persist()
}

// Stop powers off the VM after a guest sync. A warm VM must be resumed and
// synced again: writes can land after the pre-PAUSE sync but before PAUSE
// parks its vCPUs. Even if this final sync fails, the VM is powered off.
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
	warm := vm.Thermal == "warm"
	vm.mu.Unlock()

	var syncErr error
	if warm {
		// Resume directly while launchMu is held: calling e.Resume would
		// reacquire it and deadlock. Failure still powers off the VM.
		resumeCtx, resumeCancel := context.WithTimeout(ctx, 3*time.Second)
		_, resumeErr := controlCmd(resumeCtx, vm.CtlSockUDS, "RESUME")
		resumeCancel()
		if resumeErr != nil {
			syncErr = fmt.Errorf("resume for guest sync: %w", resumeErr)
		} else {
			vm.mu.Lock()
			vm.Thermal = "hot"
			ag, unasked := vm.Agent, errors.Is(vm.AgentInfoErr, errAgentInfoPaused)
			vm.mu.Unlock()
			vm.persist()
			if unasked {
				// The guest could not answer INFO while adopted paused;
				// refresh its capabilities now that its vCPUs can run.
				info, infoErr := queryAgentInfo(ctx, ag)
				vm.mu.Lock()
				vm.AgentInfo, vm.AgentInfoErr = info, infoErr
				vm.mu.Unlock()
			}
		}
	}
	if syncErr == nil {
		syncCtx, cancel := context.WithTimeout(ctx, guestSyncTimeout)
		syncErr = vm.syncGuest(syncCtx)
		cancel()
	}
	e.powerOffLocked(vm)
	slog.Info("krucible sandbox stopped (powered off)", "id", id)
	if syncErr != nil {
		slog.Warn("krucible stop: guest sync failed; VM powered off", "id", id, "error", syncErr)
		return &UncleanStopError{Reason: syncErr}
	}
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
		e.delSandboxPolicy(vm) // a failed restart must not leave an unused grant
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

func (e *Engine) rootImageMetadata(base, ownerID string) (diskMetadata, bool, error) {
	vm := &VM{UserID: ownerID}
	if !e.vmmBaseAllowed(vm, base) {
		owned, err := ownedNodeKeys(e.cfg.DataDir, ownerID)
		if err != nil {
			return diskMetadata{}, false, err
		}
		if err := e.authorizedBacking(vm, base, owned); err != nil {
			return diskMetadata{}, false, err
		}
	}
	catalog, err := readDiskCatalog(e.cfg.DataDir)
	if err != nil {
		return diskMetadata{}, false, err
	}
	fi, err := os.Stat(base)
	if err != nil {
		return diskMetadata{}, false, err
	}
	if key, ok := keyOf(fi); ok {
		if metadata, found := catalog.metadata[key]; found {
			if metadata.backing == nil {
				metadata.backing = []diskLayer{}
			}
			return metadata, true, nil
		}
	}
	// Shared bases and the configured ext4 base are daemon-created raw disks.
	// Their reserved bytes cannot choose a driver, even when they look qcow2.
	configured := ""
	if e.cfg.BaseImage != "" {
		configured, _ = resolveBasePath(e.cfg.BaseImage)
	}
	if base == configured || base == filepath.Join(e.cfg.DataDir, "base.img") ||
		within(basesDir(e.cfg.DataDir), base) || filepath.Dir(base) == filepath.Join(e.cfg.DataDir, "images") {
		return diskMetadata{format: "raw", backing: []diskLayer{}}, true, nil
	}
	// Pre-metadata owner images use the same bounded authorization as legacy
	// snapshots; inferred records are never written back to the source image.
	chain, err := e.legacyDiskChain(vm, base, "")
	if err != nil {
		return diskMetadata{}, false, err
	}
	format := "raw"
	if isQcow2(base) {
		format = "qcow2"
	}
	return diskMetadata{format: format, backing: chain}, false, nil
}

// prepareRootDisk makes a new sandbox's root disk in sandboxDir from its
// base, returning the disk, its explicit format, the base and the root's chain.
// It holds the bases lock shared from picking the base until the disk names
// it, so GC can't remove the base in between.
func (e *Engine) prepareRootDisk(ctx context.Context, sandboxDir string, spec engine.SandboxSpec) (root, format, base string, backing []diskLayer, err error) {
	unlock, err := lockBasesCtx(ctx, e.cfg.DataDir, false)
	if err != nil {
		return "", "", "", nil, fmt.Errorf("lock base images: %w", err)
	}
	defer unlock()
	if base, err = e.resolveBase(spec); err != nil {
		return "", "", "", nil, err
	}
	metadata, trusted, err := e.rootImageMetadata(base, spec.UserID)
	if err != nil {
		return "", "", "", nil, fmt.Errorf("base image metadata: %w", err)
	}
	if trusted {
		if err := validateRecordedDisk(base, metadata.format, metadata.backing); err != nil {
			return "", "", "", nil, fmt.Errorf("base image backing: %w", err)
		}
	}
	if !rootQcow2() {
		if metadata.format != "raw" {
			return "", "", "", nil, fmt.Errorf("a raw root requires a raw base image")
		}
		root = filepath.Join(sandboxDir, "root.img")
		if err := cloneFile(ctx, base, root); err != nil {
			return "", "", "", nil, fmt.Errorf("clone base image: %w", err)
		}
		if err := growFile(root, spec.DiskSizeMB); err != nil {
			return "", "", "", nil, fmt.Errorf("disk size: %w", err)
		}
		return root, "raw", base, []diskLayer{}, nil
	}
	root = filepath.Join(sandboxDir, "root.qcow2")
	if metadata.format == "qcow2" {
		if spec.DiskSizeMB > 0 {
			return "", "", "", nil, fmt.Errorf("disk size: not supported for sandboxes created from a saved image")
		}
		source, chain := base, metadata.backing
		if !trusted {
			source, chain, err = e.normalizeLegacyDisk(ctx, sandboxDir, base, metadata.format, metadata.backing)
			if err != nil {
				return "", "", "", nil, fmt.Errorf("normalize legacy image: %w", err)
			}
		}
		if e.dropVMM && spec.UserID == "" {
			_, err = e.copyNodeDisk(ctx, root, source, source != base, metadata.format)
			if err == nil {
				backing, err = readDiskChain(root, metadata.format, nil)
			}
			return root, metadata.format, base, backing, err
		}
		if source != base {
			if err := os.Rename(source, root); err != nil {
				return "", "", "", nil, fmt.Errorf("move normalized image: %w", err)
			}
		} else if err := cloneFile(ctx, base, root); err != nil {
			return "", "", "", nil, fmt.Errorf("clone qcow2 image: %w", err)
		}
		backing = chain
	} else {
		if err := e.createRootOverlayQcow2(root, base, spec.DiskSizeMB); err != nil {
			return "", "", "", nil, fmt.Errorf("create qcow2 root overlay: %w", err)
		}
		backing = []diskLayer{{Path: base, Format: "raw"}}
	}
	return root, "qcow2", base, backing, nil
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
	e.stopMonitors()
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
	// Last: it drops the references on the scopes, which the helpers outlive.
	e.cg.close()
}

// --- helpers ---

func generateID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return fmt.Sprintf("%x", b), nil
}

func genNetToken() (string, error) {
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", fmt.Errorf("generate netd attachment secret: %w", err)
	}
	return hex.EncodeToString(token[:]), nil
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
func cloneFile(ctx context.Context, src, dst string) error {
	var primary *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		primary = exec.CommandContext(ctx, "cp", "-c", src, dst) // clonefile
	default:
		primary = exec.CommandContext(ctx, "cp", "--reflink=auto", src, dst)
	}
	if out, err := primary.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fallback := exec.CommandContext(ctx, "cp", src, dst)
		if out2, err2 := fallback.CombinedOutput(); err2 != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
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

// vmmLogExcerpt is the helper's log boiled down for a failed boot: each
// distinct line once, with a count where it repeated, and the last limit bytes
// of that. A readiness probe makes libkrun log a line per vsock connection —
// thousands in 30 s — which leaves a plain tail nothing else, while the lines
// that say how far the guest got (lohar's boot trace on the console) were
// written first.
func vmmLogExcerpt(path string, limit int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	raw, _ := io.ReadAll(io.LimitReader(f, 8<<20))

	type entry struct {
		line  string
		count int
	}
	var order []*entry
	byKey := map[string]*entry{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		key := logLineKey(line)
		if e := byKey[key]; e != nil {
			e.count++
			continue
		}
		e := &entry{line: line, count: 1}
		byKey[key] = e
		order = append(order, e)
	}
	var b strings.Builder
	for _, e := range order {
		b.WriteString(e.line)
		if e.count > 1 {
			fmt.Fprintf(&b, " (x%d)", e.count)
		}
		b.WriteByte('\n')
	}
	out := b.String()
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// logLineKey makes log lines that differ only in their timestamp and numbers
// equal.
func logLineKey(line string) string {
	if strings.HasPrefix(line, "[") {
		if _, rest, ok := strings.Cut(line, " "); ok {
			line = rest // "[<timestamp>" is the first field
		}
	}
	var b strings.Builder
	inDigits := false
	for i := range len(line) {
		if c := line[i]; c >= '0' && c <= '9' {
			if !inDigits {
				b.WriteByte('#')
			}
			inDigits = true
		} else {
			inDigits = false
			b.WriteByte(c)
		}
	}
	return b.String()
}

// netdStartFailed reaps a netd that didn't come up and says why: whether it
// exited on its own (netd binds its sockets before it confines itself, so a
// confinement failure looks like an unanswered socket) and its last log lines.
func netdStartFailed(cmd *exec.Cmd, done <-chan error, logPath string, cause error) error {
	_ = cmd.Process.Kill()
	// The single cmd.Wait goroutine owns the process even on failed startup.
	// A completed wait before our SIGKILL preserves the actual startup error.
	if err := <-done; err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			if ws, ok := exit.Sys().(syscall.WaitStatus); ok && !(ws.Signaled() && ws.Signal() == syscall.SIGKILL) {
				cause = fmt.Errorf("%w (bhatti-netd exited at startup: %s)", cause, exit)
			}
		}
	}
	if tail := strings.TrimSpace(tailFile(logPath, 1024)); tail != "" {
		cause = fmt.Errorf("%w; netd log: %s", cause, tail)
	}
	return cause
}

// LoharPath is the guest agent shipped beside bhatti-vmm in the runtime
// bundle (bin/lohar); image pull/import write it into converted images.
func (e *Engine) LoharPath() string {
	return filepath.Join(filepath.Dir(e.cfg.VMMBinary), "lohar")
}

// HasInit reports whether the sandbox's boot config carries an --init command
// (known=false when the engine has no config for it).
func (e *Engine) HasInit(id string) (hasInit, known bool) {
	vm, err := e.getVM(id)
	if err != nil {
		return false, false
	}
	raw, err := os.ReadFile(filepath.Join(vm.SandboxDir, "config.json"))
	if err != nil {
		return false, false
	}
	var cfg struct {
		Init string `json:"init"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return false, false
	}
	return cfg.Init != "", true
}
