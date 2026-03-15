//go:build linux

// Package firecracker implements engine.Engine using Firecracker microVMs.
// It talks directly to Firecracker's HTTP API over a Unix socket — no SDK needed.
package firecracker

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sahilshubham/bhatti/pkg/agent"
	"github.com/sahilshubham/bhatti/pkg/engine"
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
	mu      sync.RWMutex
	vms     map[string]*VM
	cfg     Config
	nextCID uint32
}

// VM holds per-sandbox state.
type VM struct {
	ID         string
	Name       string
	SocketPath string // Firecracker API socket
	VsockPath  string // vsock UDS for host↔guest
	RootfsPath string
	SnapMemPath string
	SnapVMPath  string
	CID        uint32
	VcpuCount  int64
	MemSizeMib int64
	TapDevice  string
	GuestIP    string
	GuestMAC   string
	Agent      *agent.AgentClient
	Status     string // "running", "stopped"
	cancel     context.CancelFunc
	cmd        *exec.Cmd
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

	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "sandboxes"), 0700); err != nil {
		return nil, fmt.Errorf("create sandbox dir: %w", err)
	}

	eng := &Engine{
		vms:     make(map[string]*VM),
		cfg:     cfg,
		nextCID: 3, // 0=hypervisor, 1=loopback, 2=host
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

// --- Create ---

func (e *Engine) Create(ctx context.Context, spec engine.SandboxSpec) (engine.SandboxInfo, error) {
	id := generateID()
	sandboxDir := filepath.Join(e.cfg.DataDir, "sandboxes", id)
	os.MkdirAll(sandboxDir, 0700)

	// 1. Copy rootfs
	rootfsPath := filepath.Join(sandboxDir, "rootfs.ext4")
	if err := copyRootfs(e.cfg.BaseRootfs, rootfsPath); err != nil {
		return engine.SandboxInfo{}, fmt.Errorf("copy rootfs: %w", err)
	}

	// 2. Allocate CID and paths
	cid := atomic.AddUint32(&e.nextCID, 1)
	socketPath := filepath.Join(sandboxDir, "firecracker.sock")
	vsockPath := filepath.Join(sandboxDir, "vsock.sock")

	// 3. Create TAP device
	tapName, guestIP, err := createTapDevice(id, cid)
	if err != nil {
		os.RemoveAll(sandboxDir)
		return engine.SandboxInfo{}, fmt.Errorf("create tap: %w", err)
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
	s := subnetForCID(cid)

	// 5. Start Firecracker process
	vmCtx, vmCancel := context.WithCancel(context.Background())
	os.Remove(socketPath)
	fcCmd := exec.CommandContext(vmCtx, e.cfg.FCBinary, "--api-sock", socketPath)
	fcCmd.Stderr = os.Stderr
	if err := fcCmd.Start(); err != nil {
		vmCancel()
		destroyTapDevice(tapName, cid)
		os.RemoveAll(sandboxDir)
		return engine.SandboxInfo{}, fmt.Errorf("start firecracker: %w", err)
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
	// Format: ip=<client-ip>::<gateway>:<netmask>::<device>:off:<dns1>:<dns2>:
	// This configures eth0 before init runs, so the agent's TCP listener works immediately.
	bootArgs := fmt.Sprintf(
		"console=ttyS0 reboot=k panic=1 pci=off init=/usr/local/bin/bhatti-agent quiet loglevel=0 ip=%s::%s:255.255.255.252::eth0:off:1.1.1.1:8.8.8.8:",
		guestIP, s.HostIP)

	if err := fcPut(client, "/boot-source", fmt.Sprintf(
		`{"kernel_image_path":%q,"boot_args":%q}`,
		e.cfg.KernelPath, bootArgs)); err != nil {
		fcCmd.Process.Kill()
		vmCancel()
		destroyTapDevice(tapName, cid)
		os.RemoveAll(sandboxDir)
		return engine.SandboxInfo{}, fmt.Errorf("set boot-source: %w", err)
	}

	if err := fcPut(client, "/drives/rootfs", fmt.Sprintf(
		`{"drive_id":"rootfs","path_on_host":%q,"is_root_device":true,"is_read_only":false}`,
		rootfsPath)); err != nil {
		fcCmd.Process.Kill()
		vmCancel()
		destroyTapDevice(tapName, cid)
		os.RemoveAll(sandboxDir)
		return engine.SandboxInfo{}, fmt.Errorf("set drive: %w", err)
	}

	if err := fcPut(client, "/machine-config", fmt.Sprintf(
		`{"vcpu_count":%d,"mem_size_mib":%d}`, vcpuCount, memMB)); err != nil {
		fcCmd.Process.Kill()
		vmCancel()
		destroyTapDevice(tapName, cid)
		os.RemoveAll(sandboxDir)
		return engine.SandboxInfo{}, fmt.Errorf("set machine-config: %w", err)
	}

	if err := fcPut(client, "/vsock", fmt.Sprintf(
		`{"guest_cid":%d,"uds_path":%q}`, cid, vsockPath)); err != nil {
		fcCmd.Process.Kill()
		vmCancel()
		destroyTapDevice(tapName, cid)
		os.RemoveAll(sandboxDir)
		return engine.SandboxInfo{}, fmt.Errorf("set vsock: %w", err)
	}

	if err := fcPut(client, "/network-interfaces/eth0", fmt.Sprintf(
		`{"iface_id":"eth0","guest_mac":%q,"host_dev_name":%q}`,
		mac, tapName)); err != nil {
		fcCmd.Process.Kill()
		vmCancel()
		destroyTapDevice(tapName, cid)
		os.RemoveAll(sandboxDir)
		return engine.SandboxInfo{}, fmt.Errorf("set network: %w", err)
	}

	// 7. Boot
	if err := fcPut(client, "/actions", `{"action_type":"InstanceStart"}`); err != nil {
		fcCmd.Process.Kill()
		vmCancel()
		destroyTapDevice(tapName, cid)
		os.RemoveAll(sandboxDir)
		return engine.SandboxInfo{}, fmt.Errorf("start instance: %w", err)
	}

	// 8. Wait for agent via TCP (kernel ip= already configured eth0).
	// TCP is the primary channel — it survives snapshot/resume.
	agentClient := agent.NewTCPClient(guestIP)
	if err := agentClient.WaitReady(ctx, 30*time.Second); err != nil {
		fcCmd.Process.Kill()
		vmCancel()
		destroyTapDevice(tapName, cid)
		os.RemoveAll(sandboxDir)
		return engine.SandboxInfo{}, fmt.Errorf("agent not ready: %w", err)
	}

	name := spec.Name
	if name == "" {
		name = id
	}

	vm := &VM{
		ID: id, Name: name, SocketPath: socketPath,
		VsockPath: vsockPath, RootfsPath: rootfsPath,
		CID: cid, VcpuCount: vcpuCount, MemSizeMib: memMB,
		TapDevice: tapName, GuestIP: guestIP, GuestMAC: mac,
		Agent: agentClient, Status: "running",
		cancel: vmCancel, cmd: fcCmd,
	}

	e.mu.Lock()
	e.vms[id] = vm
	e.mu.Unlock()

	return engine.SandboxInfo{
		ID: id, Name: name, Status: "running",
		IP: guestIP, EngineID: id,
	}, nil
}

// --- Stop (Snapshot) ---

func (e *Engine) Stop(ctx context.Context, id string) error {
	vm, err := e.getVM(id)
	if err != nil {
		return err
	}
	if vm.Status != "running" {
		return fmt.Errorf("sandbox %q is not running", id)
	}

	client := fcAPIClient(vm.SocketPath)

	// Pause VM
	if err := fcPatch(client, "/vm", `{"state":"Paused"}`); err != nil {
		return fmt.Errorf("pause: %w", err)
	}

	// Create snapshot
	vm.SnapMemPath = filepath.Join(filepath.Dir(vm.RootfsPath), "mem.snap")
	vm.SnapVMPath = filepath.Join(filepath.Dir(vm.RootfsPath), "vm.snap")
	if err := fcPut(client, "/snapshot/create", fmt.Sprintf(
		`{"snapshot_type":"Full","snapshot_path":%q,"mem_file_path":%q}`,
		vm.SnapVMPath, vm.SnapMemPath)); err != nil {
		return fmt.Errorf("create snapshot: %w", err)
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
	return nil
}

// --- Start (Resume) ---

func (e *Engine) Start(ctx context.Context, id string) error {
	vm, err := e.getVM(id)
	if err != nil {
		return err
	}
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

	// Load snapshot and resume. The snapshot contains all device config.
	// The backing resources (rootfs file, TAP device, vsock UDS path) must
	// exist at the same paths as when the snapshot was created.
	if err := fcPut(client, "/snapshot/load", fmt.Sprintf(
		`{"snapshot_path":%q,"mem_backend":{"backend_path":%q,"backend_type":"File"},"resume_vm":true}`,
		vm.SnapVMPath, vm.SnapMemPath)); err != nil {
		fcCmd.Process.Kill()
		vmCancel()
		return fmt.Errorf("load snapshot: %w", err)
	}

	vm.SocketPath = newSocketPath
	vm.cmd = fcCmd
	vm.cancel = vmCancel
	vm.Status = "running"

	// Use TCP client for post-resume — vsock is broken after snapshot/restore
	// but virtio-net (TCP over TAP) survives.
	vm.Agent = agent.NewTCPClient(vm.GuestIP)

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

	if vm.Status == "running" && vm.cmd != nil {
		vm.cmd.Process.Kill()
		vm.cmd.Wait()
		vm.cancel()
	}

	// Always clean up TAP — whether running or stopped.
	if vm.TapDevice != "" {
		destroyTapDevice(vm.TapDevice, vm.CID)
	}

	os.RemoveAll(filepath.Dir(vm.RootfsPath))

	e.mu.Lock()
	delete(e.vms, id)
	e.mu.Unlock()
	return nil
}

// --- Status, List ---

func (e *Engine) Status(ctx context.Context, id string) (engine.SandboxInfo, error) {
	vm, err := e.getVM(id)
	if err != nil {
		return engine.SandboxInfo{}, err
	}
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
	return map[string]interface{}{
		"rootfs_path":   vm.RootfsPath,
		"snap_mem_path": vm.SnapMemPath,
		"snap_vm_path":  vm.SnapVMPath,
		"vsock_cid":     vm.CID,
		"tap_device":    vm.TapDevice,
		"guest_ip":      vm.GuestIP,
		"guest_mac":     vm.GuestMAC,
		"vcpu_count":    vm.VcpuCount,
		"mem_size_mib":  vm.MemSizeMib,
		"socket_path":   vm.SocketPath,
		"vsock_path":    vm.VsockPath,
	}
}

// RestoreVM adds a VM to the engine's in-memory map from persisted state.
// Used during startup recovery.
func (e *Engine) RestoreVM(id, name, status string, state map[string]interface{}) {
	vm := &VM{
		ID:         id,
		Name:       name,
		Status:     status,
		RootfsPath: state["rootfs_path"].(string),
		SocketPath: state["socket_path"].(string),
		VsockPath:  state["vsock_path"].(string),
		CID:        uint32(state["vsock_cid"].(int)),
		TapDevice:  state["tap_device"].(string),
		GuestIP:    state["guest_ip"].(string),
		GuestMAC:   state["guest_mac"].(string),
		VcpuCount:  int64(state["vcpu_count"].(float64)),
		MemSizeMib: int64(state["mem_size_mib"].(int)),
	}
	if v, ok := state["snap_mem_path"].(string); ok {
		vm.SnapMemPath = v
	}
	if v, ok := state["snap_vm_path"].(string); ok {
		vm.SnapVMPath = v
	}

	if status == "running" {
		vm.Agent = agent.NewTCPClient(vm.GuestIP)
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
	return vm.Agent.Exec(ctx, cmd, nil, "")
}

func (e *Engine) Shell(ctx context.Context, id string) (engine.TerminalConn, error) {
	vm, err := e.getVM(id)
	if err != nil {
		return nil, err
	}
	return vm.Agent.Shell(ctx, []string{"/bin/zsh", "-li"}, map[string]string{
		"TERM": "xterm-256color",
	}, 24, 80)
}

func (e *Engine) ListeningPorts(ctx context.Context, id string) ([]int, error) {
	vm, err := e.getVM(id)
	if err != nil {
		return nil, err
	}
	result, err := vm.Agent.Exec(ctx, []string{"ss", "-tln", "--no-header"}, nil, "")
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
	return vm.Agent.Forward(ctx, uint16(port))
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

func copyRootfs(src, dst string) error {
	// Try CoW clone first
	if err := exec.Command("cp", "--reflink=always", src, dst).Run(); err == nil {
		return nil
	}
	return exec.Command("cp", src, dst).Run()
}

// fcAPIClient returns an HTTP client that talks to Firecracker's API over a Unix socket.
func fcAPIClient(socketPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
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
