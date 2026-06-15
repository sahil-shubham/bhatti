package krucible

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent"
	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// Config holds paths and defaults for the krucible engine. All pure Go — the
// engine spawns the cgo `bhatti-vmm` helper and talks to lohar over sockets.
type Config struct {
	DataDir       string // sandboxes live under DataDir/sandboxes/<id>
	BaseRootfs    string // host dir tree (virtiofs root): /init.krun=lohar + mountpoints
	VMMBinary     string // path to the bhatti-vmm helper (built with `make vmm`)
	LibDir        string // dir with libkrun/libkrunfw (DYLD_FALLBACK_LIBRARY_PATH / LD_LIBRARY_PATH)
	// SocketDir holds the per-VM vsock UDS. It must be SHORT: AF_UNIX paths cap
	// at ~104 bytes (macOS) / 108 (Linux), and macOS $TMPDIR/DataDir can be deep.
	// Empty defaults to /tmp/bhatti-kr.
	SocketDir     string
	DefaultVcpus  uint8
	DefaultMemMiB uint32
}

// maxUnixPath is the conservative AF_UNIX sun_path cap (macOS = 104).
const maxUnixPath = 104

// VM is per-sandbox state. The helper process IS the VM; we hold its cmd to
// stop it and the agent client to drive lohar.
type VM struct {
	mu         sync.Mutex
	ID         string
	Name       string
	UserID     string
	SandboxDir string
	RootfsDir  string
	SockDir    string
	ControlUDS string // guest vsock 1024 (agent control)
	ForwardUDS string // guest vsock 1025 (port forward)
	CtlSockUDS string // VMM control socket (PAUSE/RESUME/STATUS)
	MemMiB     uint32 // configured at boot (for ThermalEngine.MemSizeMib)
	Thermal    string // "hot" | "warm" | (cold lands in P3)
	Token      string
	Agent      *agent.AgentClient
	Status     string // "running" | "stopped"
	cmd        *exec.Cmd
	cancel     context.CancelFunc
}

// Engine implements engine.Engine on libkrun via the per-VM bhatti-vmm helper.
type Engine struct {
	mu  sync.RWMutex
	vms map[string]*VM
	cfg Config
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
	if cfg.BaseRootfs == "" {
		return nil, fmt.Errorf("krucible: BaseRootfs not set")
	}
	if _, err := os.Stat(cfg.BaseRootfs); err != nil {
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
	return &Engine{vms: make(map[string]*VM), cfg: cfg}, nil
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

// Create boots a new sandbox: clone the base rootfs, spawn bhatti-vmm, wait
// for lohar's agent to answer over the bridged vsock.
func (e *Engine) Create(ctx context.Context, spec engine.SandboxSpec) (info engine.SandboxInfo, err error) {
	id := generateID()
	sandboxDir := filepath.Join(e.cfg.DataDir, "sandboxes", id)
	rootfsDir := filepath.Join(sandboxDir, "rootfs")
	sockDir := filepath.Join(e.cfg.SocketDir, id)

	// Cleanup on any failure.
	var cancel context.CancelFunc
	var cmd *exec.Cmd
	defer func() {
		if err != nil {
			if cmd != nil && cmd.Process != nil {
				_ = cmd.Process.Kill()
				_, _ = cmd.Process.Wait()
			}
			if cancel != nil {
				cancel()
			}
			os.RemoveAll(sandboxDir)
			os.RemoveAll(sockDir)
		}
	}()

	if err = os.MkdirAll(sandboxDir, 0700); err != nil {
		return info, fmt.Errorf("create sandbox dir: %w", err)
	}
	if err = cloneTree(e.cfg.BaseRootfs, rootfsDir); err != nil {
		return info, fmt.Errorf("clone rootfs: %w", err)
	}

	vcpus := e.cfg.DefaultVcpus
	if spec.CPUs >= 1 {
		vcpus = uint8(spec.CPUs)
	}
	memMiB := e.cfg.DefaultMemMiB
	if spec.MemoryMB > 0 {
		memMiB = uint32(spec.MemoryMB)
	}

	// Short UDS paths in a dedicated dir — sockaddr_un caps at ~104 bytes, and a
	// deep DataDir/$TMPDIR would overflow it (ENAMETOOLONG on the vsock proxy).
	controlUDS := filepath.Join(sockDir, "c.sock")
	forwardUDS := filepath.Join(sockDir, "f.sock")
	ctlSockUDS := filepath.Join(sockDir, "k.sock") // VMM control (PAUSE/RESUME/STATUS)
	if err = os.MkdirAll(sockDir, 0700); err != nil {
		return info, fmt.Errorf("create socket dir: %w", err)
	}
	for _, p := range []string{controlUDS, forwardUDS, ctlSockUDS} {
		if len(p) >= maxUnixPath {
			return info, fmt.Errorf("vsock path too long (%d >= %d): %s — set a shorter SocketDir", len(p), maxUnixPath, p)
		}
	}

	// P1: no config-drive injection yet, so no auth token. (Config + token
	// land with the overlay-file config in a follow-up.)
	token := ""

	vmSpec := VMSpec{
		RootfsDir:       rootfsDir,
		Vcpus:           vcpus,
		MemMiB:          memMiB,
		Pid1:            true,
		ExecPath:        "/init.krun",
		VsockControlUDS:  controlUDS,
		VsockForwardUDS:  forwardUDS,
		ControlSocketUDS: ctlSockUDS,
		LogLevel:         2,
	}
	specPath := filepath.Join(sandboxDir, "vmspec.json")
	specBytes, _ := json.MarshalIndent(vmSpec, "", "  ")
	if err = os.WriteFile(specPath, specBytes, 0600); err != nil {
		return info, fmt.Errorf("write vmspec: %w", err)
	}

	// Spawn the helper. Its stdout/stderr (guest console + libkrun logs) go to
	// vmm.log so we can surface the tail on boot failure.
	logPath := filepath.Join(sandboxDir, "vmm.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return info, fmt.Errorf("create vmm log: %w", err)
	}
	defer logFile.Close()

	vmCtx, vmCancel := context.WithCancel(context.Background())
	cancel = vmCancel
	cmd = exec.CommandContext(vmCtx, e.cfg.VMMBinary, specPath)
	// Guest console + libkrun logs go to vmm.log; the tail is surfaced on boot
	// failure (see WaitReady below).
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if e.cfg.LibDir != "" {
		cmd.Env = append(os.Environ(),
			"DYLD_FALLBACK_LIBRARY_PATH="+e.cfg.LibDir,
			"LD_LIBRARY_PATH="+e.cfg.LibDir,
		)
	}
	if err = cmd.Start(); err != nil {
		return info, fmt.Errorf("start vmm helper: %w", err)
	}

	// Wait for lohar's agent. If the helper died early, report its log tail.
	ag := agent.NewKrucibleClient(controlUDS, forwardUDS, token)
	if werr := ag.WaitReady(ctx, 30*time.Second); werr != nil {
		err = fmt.Errorf("agent not ready: %w\nvmm log:\n%s", werr, tailFile(logPath, 4096))
		return info, err
	}

	name := spec.Name
	if name == "" {
		name = id
	}
	vm := &VM{
		ID: id, Name: name, UserID: spec.UserID,
		SandboxDir: sandboxDir, RootfsDir: rootfsDir, SockDir: sockDir,
		ControlUDS: controlUDS, ForwardUDS: forwardUDS, CtlSockUDS: ctlSockUDS,
		MemMiB: memMiB, Thermal: "hot",
		Token: token, Agent: ag, Status: "running",
		cmd: cmd, cancel: vmCancel,
	}
	e.mu.Lock()
	e.vms[id] = vm
	e.mu.Unlock()

	slog.Info("krucible sandbox created", "id", id, "name", name, "vcpus", vcpus, "mem_mib", memMiB)
	return engine.SandboxInfo{ID: id, Name: name, Status: "running", EngineID: id}, nil
}

// Destroy kills the helper and removes the sandbox dir.
func (e *Engine) Destroy(ctx context.Context, id string) error {
	vm, err := e.getVM(id)
	if err != nil {
		return err
	}
	vm.mu.Lock()
	if vm.cmd != nil && vm.cmd.Process != nil {
		_ = vm.cmd.Process.Kill()
		_, _ = vm.cmd.Process.Wait()
	}
	if vm.cancel != nil {
		vm.cancel()
	}
	dir := vm.SandboxDir
	sockDir := vm.SockDir
	vm.Status = "stopped"
	vm.mu.Unlock()

	e.mu.Lock()
	delete(e.vms, id)
	e.mu.Unlock()

	os.RemoveAll(dir)
	os.RemoveAll(sockDir)
	slog.Info("krucible sandbox destroyed", "id", id)
	return nil
}

// Stop/Start are the cold tier (snapshot-to-disk + free RAM / restore), which
// lands in P3. The warm tier (hot<->warm) is the ThermalEngine surface in
// thermal.go (Pause/Resume/EnsureHot via the control socket).
func (e *Engine) Stop(ctx context.Context, id string) error {
	return fmt.Errorf("krucible: stop/snapshot not implemented yet (P3)")
}

func (e *Engine) Start(ctx context.Context, id string) error {
	return fmt.Errorf("krucible: start/resume not implemented yet (P3)")
}

func (e *Engine) Status(ctx context.Context, id string) (engine.SandboxInfo, error) {
	vm, err := e.getVM(id)
	if err != nil {
		return engine.SandboxInfo{}, err
	}
	vm.mu.Lock()
	defer vm.mu.Unlock()
	return engine.SandboxInfo{ID: vm.ID, Name: vm.Name, Status: vm.Status, EngineID: vm.ID}, nil
}

func (e *Engine) List(ctx context.Context) ([]engine.SandboxInfo, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]engine.SandboxInfo, 0, len(e.vms))
	for _, vm := range e.vms {
		vm.mu.Lock()
		out = append(out, engine.SandboxInfo{ID: vm.ID, Name: vm.Name, Status: vm.Status, EngineID: vm.ID})
		vm.mu.Unlock()
	}
	return out, nil
}

// Shutdown stops all running helpers (called on daemon SIGTERM).
func (e *Engine) Shutdown() {
	e.mu.RLock()
	vms := make([]*VM, 0, len(e.vms))
	for _, vm := range e.vms {
		vms = append(vms, vm)
	}
	e.mu.RUnlock()
	for _, vm := range vms {
		vm.mu.Lock()
		if vm.Status == "running" && vm.cmd != nil && vm.cmd.Process != nil {
			_ = vm.cmd.Process.Kill()
		}
		if vm.cancel != nil {
			vm.cancel()
		}
		vm.mu.Unlock()
	}
}

// --- helpers ---

func generateID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// cloneTree copies src/* into dst, preferring a CoW clone (APFS clonefile on
// darwin, reflink on linux) and falling back to a plain recursive copy.
func cloneTree(src, dst string) error {
	if err := os.MkdirAll(dst, 0755); err != nil {
		return err
	}
	var primary *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		primary = exec.Command("cp", "-c", "-R", src+"/.", dst) // clonefile
	default:
		primary = exec.Command("cp", "-a", "--reflink=auto", src+"/.", dst)
	}
	if out, err := primary.CombinedOutput(); err != nil {
		fallback := exec.Command("cp", "-R", src+"/.", dst)
		if out2, err2 := fallback.CombinedOutput(); err2 != nil {
			return fmt.Errorf("clone (%v: %s) and fallback (%v: %s) both failed",
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
