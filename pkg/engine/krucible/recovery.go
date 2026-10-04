package krucible

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent"
	"github.com/sahil-shubham/bhatti/pkg/gateway"
)

// Recovery makes krucible restart-safe. A daemon's shutdown lets go of its
// sandboxes without stopping them (Shutdown): helpers and netds are detached
// processes that outlive it. The next daemon reads each sandbox's state.json,
// adopts the helpers still running it — as they are, running or paused — and
// marks the rest stopped, to boot fresh from their root disk. Pure Go on macOS
// and Linux.

// vmRecord is the durable, JSON-serialized state of a VM — everything needed to
// reconnect to a live helper or to relaunch a dead one. The runtime-only fields
// (cmd, cancel, Agent, mu) are intentionally excluded.
type vmRecord struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	UserID     string `json:"user_id"`
	SandboxDir string `json:"sandbox_dir"`
	SockDir    string `json:"sock_dir"`
	ControlUDS string `json:"control_uds"`
	ForwardUDS string `json:"forward_uds"`
	CtlSockUDS string `json:"ctl_sock_uds"`
	MemMiB     uint32 `json:"mem_mib"`
	Thermal    string `json:"thermal"`
	Status     string `json:"status"`
	Token      string `json:"token"`
	LogPath    string `json:"log_path"`
	BaseSpec   VMSpec `json:"base_spec"`
	HelperPID  int    `json:"helper_pid"`
	NetdKey    string `json:"netd_key,omitempty"`    // owner key of the shared bhatti-netd (net backend)
	SubnetIdx  int    `json:"subnet_idx,omitempty"`  // owner's vnet subnet index
	NetIP      string `json:"net_ip,omitempty"`      // guest IP on the netd gateway subnet
	SandboxRef string `json:"sandbox_ref,omitempty"` // the server's sandbox ID, named to the credential broker
	VMMUID     uint32 `json:"vmm_uid,omitempty"`     // the helper's own uid (vmmuser.go), kept for the sandbox's life
	// The egress rules netd enforces for the guest; nil = public, and all a
	// record from before this field can say.
	NetPolicy *gateway.NetPolicyWire `json:"net_policy,omitempty"`
}

// netdRecord is the durable state of one owner's shared bhatti-netd, so recovery
// can re-adopt the running gateway (survives daemon restarts, spawned detached)
// rather than respawn onto a socket it still holds. Lives beside the socket.
type netdRecord struct {
	Owner     string `json:"owner"`
	Sock      string `json:"sock"`
	Dir       string `json:"dir"`
	SubnetIdx int    `json:"subnet_idx"`
	Pid       int    `json:"pid"`
	NextGuest int    `json:"next_guest"`
	VMMGID    uint32 `json:"vmm_gid,omitempty"` // the group of the owner's helpers, which its socket opens to
}

func netdStatePath(dir string) string { return filepath.Join(dir, "netd.json") }

// writeNetdRecord persists the instance. Caller must hold inst.mu (it reads
// nextGuest + pid). Best-effort + atomic (temp + rename).
func writeNetdRecord(inst *netdInstance) {
	if inst.dir == "" {
		return
	}
	rec := netdRecord{Owner: inst.owner, Sock: inst.sock, Dir: inst.dir,
		SubnetIdx: inst.subnetIdx, Pid: inst.pid, NextGuest: inst.nextGuest, VMMGID: inst.vmmGID}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(inst.dir, 0700)
	tmp := netdStatePath(inst.dir) + ".tmp"
	if os.WriteFile(tmp, data, 0600) == nil {
		_ = os.Rename(tmp, netdStatePath(inst.dir))
	}
}

func stateFilePath(sandboxDir string) string { return filepath.Join(sandboxDir, "state.json") }

// toRecordLocked builds the durable record. Caller must hold vm.mu.
func (vm *VM) toRecordLocked() vmRecord {
	return vmRecord{
		ID: vm.ID, Name: vm.Name, UserID: vm.UserID,
		SandboxDir: vm.SandboxDir, SockDir: vm.SockDir,
		ControlUDS: vm.ControlUDS, ForwardUDS: vm.ForwardUDS, CtlSockUDS: vm.CtlSockUDS,
		MemMiB: vm.MemMiB, Thermal: vm.Thermal, Status: vm.Status, Token: vm.Token,
		LogPath: vm.logPath, BaseSpec: vm.baseSpec,
		HelperPID: vm.HelperPID, NetdKey: vm.netdKey, SubnetIdx: vm.subnetIdx, NetIP: vm.netIP, SandboxRef: vm.sandboxRef,
		VMMUID: vm.vmmUID, NetPolicy: vm.netPolicy,
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
		SandboxDir: rec.SandboxDir, SockDir: rec.SockDir,
		ControlUDS: rec.ControlUDS, ForwardUDS: rec.ForwardUDS, CtlSockUDS: rec.CtlSockUDS,
		MemMiB: rec.MemMiB, Thermal: rec.Thermal, Status: rec.Status, Token: rec.Token,
		baseSpec: rec.BaseSpec, logPath: rec.LogPath,
		HelperPID: rec.HelperPID, netdKey: rec.NetdKey, subnetIdx: rec.SubnetIdx, netIP: rec.NetIP, sandboxRef: rec.SandboxRef,
		vmmUID: rec.VMMUID, netPolicy: rec.NetPolicy,
	}
}

// readoptNetd re-registers a recovered VM with its owner's shared bhatti-netd:
// it rebuilds the per-owner instance from the persisted netd record, re-adopts
// the still-running gateway (so ensureNetd reuses it instead of respawning onto a
// socket it holds), and reference-counts it so Destroy of the owner's last
// sandbox still tears it down.
func (e *Engine) readoptNetd(vm *VM) {
	if vm.netdKey == "" {
		return
	}
	e.netdMu.Lock()
	defer e.netdMu.Unlock()
	if inst := e.netds[vm.netdKey]; inst != nil {
		inst.refs++
		return
	}
	dir := e.netdDir(vm.netdKey)
	inst := &netdInstance{owner: vm.netdKey, sock: filepath.Join(dir, "n.sock"), ctlSock: filepath.Join(dir, "ctl.sock"), dir: dir, subnetIdx: vm.subnetIdx, refs: 1}
	if data, err := os.ReadFile(netdStatePath(dir)); err == nil {
		var rec netdRecord
		if json.Unmarshal(data, &rec) == nil {
			inst.nextGuest = rec.NextGuest
			if rec.Pid > 0 && pidAlive(rec.Pid) && isNetd(rec.Pid, inst.sock) {
				inst.pid = rec.Pid // adopt the live gateway
			}
			if e.dropVMM {
				inst.vmmGID = rec.VMMGID
			}
		}
	}
	if e.dropVMM && inst.vmmGID == 0 {
		inst.vmmGID = e.netdGIDLocked(vm.netdKey)
	}
	e.netds[vm.netdKey] = inst
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

// isHelper reports whether pid is a bhatti-vmm started on spec, its only
// argument. Once a process exits its pid can be anyone's, so a pid read back
// from state.json names a helper only together with this.
func isHelper(pid int, spec string) bool {
	args, err := procArgs(pid)
	return err == nil && len(args) == 2 && args[1] == spec
}

// isNetd reports whether pid is the bhatti-netd serving sock.
func isNetd(pid int, sock string) bool {
	args, err := procArgs(pid)
	if err != nil {
		return false
	}
	for i := 1; i+1 < len(args); i++ {
		if args[i] == "--net-uds" && args[i+1] == sock {
			return true
		}
	}
	return false
}

// killHelper SIGKILLs pid if it is still the helper running spec, and waits
// for it to go: whatever runs on the sandbox's disks next mustn't share them
// with it. An adopted helper isn't our child — init reaps it — except when a
// test runs both daemons in one process, so it's reaped here if it is.
func killHelper(pid int, spec string) {
	if !isHelper(pid, spec) {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	deadline := time.Now().Add(10 * time.Second)
	for pidAlive(pid) {
		var ws syscall.WaitStatus
		if wpid, _ := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil); wpid == pid {
			return
		}
		if time.Now().After(deadline) {
			slog.Warn("krucible: helper still running 10s after SIGKILL", "pid", pid, "spec", spec)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// errAgentInfoPaused stands in for the guest agent's report while a sandbox
// adopted paused hasn't run since: its guest can't answer until Resume.
var errAgentInfoPaused = errors.New("not asked yet: the guest was paused when this daemon started")

// recover rebuilds the engine's sandboxes from their state.json. Bad records
// are skipped.
func (e *Engine) recover() {
	matches, _ := filepath.Glob(filepath.Join(e.cfg.DataDir, "sandboxes", "*", stateFile))
	var adopted []*VM
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
		helper := e.adoptHelper(vm, rec.Status)
		switch helper {
		case "running", "paused":
			vm.Status, vm.Thermal = "running", "hot"
			vm.Agent = agent.NewKrucibleClient(vm.ControlUDS, vm.ForwardUDS, vm.Token)
			if helper == "paused" {
				vm.Thermal, vm.AgentInfoErr = "warm", errAgentInfoPaused
			} else if vm.AgentInfo, vm.AgentInfoErr = queryAgentInfo(context.Background(), vm.Agent); vm.AgentInfoErr != nil {
				slog.Warn("krucible.agent.info", "id", vm.ID, "error", vm.AgentInfoErr)
			}
			adopted = append(adopted, vm)
		default:
			vm.Status, vm.Thermal, vm.HelperPID = "stopped", "cold", 0
		}
		e.mu.Lock()
		e.vms[rec.ID] = vm
		if e.dropVMM && vm.vmmUID != 0 {
			e.vmmIDs[vm.vmmUID] = vm.ID
		}
		e.mu.Unlock()
		e.readoptNetd(vm) // re-adopt the owner's shared gateway (net backend)
		vm.persist()      // write back the reconciled status/thermal
		slog.Info("krucible recovered sandbox", "id", rec.ID, "name", rec.Name,
			"helper", helper, "status", vm.Status, "thermal", vm.Thermal)
	}
	// netd kept each guest's egress state across the restart, but the records
	// are its source of truth (gateway/control.go). A nil policy is netd's own
	// default and all a record from before net_policy can say: pushing it could
	// only loosen what netd holds.
	for _, vm := range adopted {
		if vm.netPolicy != nil && e.netdRunning(vm.netdKey) {
			_ = e.pushSandboxPolicy(vm)
		}
	}
}

// adoptHelper says what the helper a record names is doing: "running" or
// "paused" for one to adopt, "" for none. It must still run this sandbox's
// spec and answer on its control socket. One that is this sandbox's but can't
// be adopted — wedged, or launched by a daemon that went away before the guest
// came up — is killed: the sandbox's next boot would share its disks.
func (e *Engine) adoptHelper(vm *VM, recStatus string) string {
	pid, spec := vm.HelperPID, vm.specPath()
	if pid <= 0 || !pidAlive(pid) || !isHelper(pid, spec) {
		return ""
	}
	if recStatus != "running" {
		slog.Warn("krucible recover: killing a helper whose launch never finished", "id", vm.ID, "pid", pid)
		killHelper(pid, spec)
		return ""
	}
	state, err := e.helperState(vm)
	if err != nil {
		slog.Warn("krucible recover: killing a helper that doesn't answer", "id", vm.ID, "pid", pid, "error", err)
		killHelper(pid, spec)
		return ""
	}
	return state
}

// helperState asks a live helper whether its VM is running or paused. The VMM
// answers on its control socket while the guest's vCPUs are parked, when the
// guest's agent can't. A helper without one (a build that can't pause) is
// running if its agent answers.
func (e *Engine) helperState(vm *VM) (string, error) {
	if vm.baseSpec.ControlSocketUDS == "" {
		if e.agentResponds(vm) {
			return "running", nil
		}
		return "", errors.New("agent not answering")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	reply, err := controlCmd(ctx, vm.CtlSockUDS, "STATUS")
	if err != nil {
		return "", err
	}
	if reply != "running" && reply != "paused" {
		return "", fmt.Errorf("control STATUS: unexpected reply %q", reply)
	}
	return reply, nil
}

// netdRunning reports whether the owner's netd is a live process (adopted or
// spawned), so there is something to push to.
func (e *Engine) netdRunning(ownerKey string) bool {
	e.netdMu.Lock()
	inst := e.netds[ownerKey]
	e.netdMu.Unlock()
	if inst == nil {
		return false
	}
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.pid > 0
}

const stateFile = "state.json"

// agentResponds probes a recovered helper's agent with a short timeout.
func (e *Engine) agentResponds(vm *VM) bool {
	if vm.ControlUDS == "" {
		return false
	}
	ag := agent.NewKrucibleClient(vm.ControlUDS, vm.ForwardUDS, vm.Token)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return ag.WaitReady(ctx, 3*time.Second) == nil
}
