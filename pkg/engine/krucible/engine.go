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
	// BlockRoot boots sandboxes from an ext4 block image (CoW-cloned per
	// sandbox from a base built once from BaseRootfs) instead of a virtio-fs
	// host dir. Required for the cold tier (Stop/Start): the block image is the
	// self-contained, snapshot-surviving rootfs (see docs/PLAN-krucible-cold-tier.md).
	BlockRoot bool
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
	Thermal    string // "hot" | "warm" | "cold"
	Token      string
	Agent      *agent.AgentClient
	Status     string // "running" | "stopped"
	BundleDir  string // cold-snapshot bundle dir (Stop writes, Start restores from)
	baseSpec   VMSpec // the spec to (re-)launch with; Start adds SnapshotDir
	logPath    string
	cmd        *exec.Cmd
	cancel     context.CancelFunc
}

// Engine implements engine.Engine on libkrun via the per-VM bhatti-vmm helper.
type Engine struct {
	mu        sync.RWMutex
	vms       map[string]*VM
	cfg       Config
	baseImgMu sync.Mutex // guards the one-time base-image build
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

// Create boots a new sandbox: prepare the rootfs (block image clone or
// virtio-fs dir), spawn bhatti-vmm, wait for lohar's agent over the bridged vsock.
func (e *Engine) Create(ctx context.Context, spec engine.SandboxSpec) (info engine.SandboxInfo, err error) {
	id := generateID()
	sandboxDir := filepath.Join(e.cfg.DataDir, "sandboxes", id)
	rootfsDir := filepath.Join(sandboxDir, "rootfs")
	sockDir := filepath.Join(e.cfg.SocketDir, id)

	var vm *VM
	defer func() {
		if err != nil {
			if vm != nil {
				vm.kill()
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
	if err = os.MkdirAll(sockDir, 0700); err != nil {
		return info, fmt.Errorf("create socket dir: %w", err)
	}
	for _, p := range []string{controlUDS, forwardUDS, ctlSockUDS} {
		if len(p) >= maxUnixPath {
			return info, fmt.Errorf("vsock path too long (%d >= %d): %s — set a shorter SocketDir", len(p), maxUnixPath, p)
		}
	}

	baseSpec := VMSpec{
		Vcpus:            vcpus,
		MemMiB:           memMiB,
		Pid1:             true,
		ExecPath:         "/init.krun",
		VsockControlUDS:  controlUDS,
		VsockForwardUDS:  forwardUDS,
		ControlSocketUDS: ctlSockUDS,
		LogLevel:         2,
	}

	// Rootfs: a CoW-cloned ext4 block image (cold-capable) or a virtio-fs dir.
	if e.cfg.BlockRoot {
		rootImg := filepath.Join(sandboxDir, "root.img")
		if err = e.cloneBaseImage(rootImg); err != nil {
			return info, fmt.Errorf("clone base image: %w", err)
		}
		baseSpec.RootDisk = rootImg
	} else {
		if err = cloneTree(e.cfg.BaseRootfs, rootfsDir); err != nil {
			return info, fmt.Errorf("clone rootfs: %w", err)
		}
		baseSpec.RootfsDir = rootfsDir
	}

	name := spec.Name
	if name == "" {
		name = id
	}
	vm = &VM{
		ID: id, Name: name, UserID: spec.UserID,
		SandboxDir: sandboxDir, RootfsDir: rootfsDir, SockDir: sockDir,
		ControlUDS: controlUDS, ForwardUDS: forwardUDS, CtlSockUDS: ctlSockUDS,
		MemMiB: memMiB, Thermal: "hot", Status: "stopped",
		BundleDir: filepath.Join(sandboxDir, "bundle"),
		baseSpec:  baseSpec,
		logPath:   filepath.Join(sandboxDir, "vmm.log"),
	}

	if err = e.launch(ctx, vm, ""); err != nil {
		return info, err
	}

	e.mu.Lock()
	e.vms[id] = vm
	e.mu.Unlock()

	slog.Info("krucible sandbox created", "id", id, "name", name, "vcpus", vcpus, "mem_mib", memMiB, "block_root", e.cfg.BlockRoot)
	return engine.SandboxInfo{ID: id, Name: name, Status: "running", EngineID: id}, nil
}

// launch spawns the bhatti-vmm helper for vm and waits for the agent. When
// snapshotDir is non-empty the helper cold-restores from that bundle instead of
// cold booting. Sets vm.cmd/cancel/Agent/Status on success.
func (e *Engine) launch(ctx context.Context, vm *VM, snapshotDir string) error {
	spec := vm.baseSpec
	spec.SnapshotDir = snapshotDir
	specPath := filepath.Join(vm.SandboxDir, "vmspec.json")
	specBytes, _ := json.MarshalIndent(spec, "", "  ")
	if err := os.WriteFile(specPath, specBytes, 0600); err != nil {
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

	vmCtx, vmCancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(vmCtx, e.cfg.VMMBinary, specPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if e.cfg.LibDir != "" {
		cmd.Env = append(os.Environ(),
			"DYLD_FALLBACK_LIBRARY_PATH="+e.cfg.LibDir,
			"LD_LIBRARY_PATH="+e.cfg.LibDir,
		)
	}
	if err := cmd.Start(); err != nil {
		vmCancel()
		return fmt.Errorf("start vmm helper: %w", err)
	}

	ag := agent.NewKrucibleClient(vm.ControlUDS, vm.ForwardUDS, vm.Token)
	if werr := ag.WaitReady(ctx, 30*time.Second); werr != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		vmCancel()
		return fmt.Errorf("agent not ready: %w\nvmm log:\n%s", werr, tailFile(vm.logPath, 4096))
	}

	vm.mu.Lock()
	vm.cmd = cmd
	vm.cancel = vmCancel
	vm.Agent = ag
	vm.Status = "running"
	vm.Thermal = "hot"
	vm.mu.Unlock()
	return nil
}

// kill terminates the helper (best effort).
func (vm *VM) kill() {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if vm.cmd != nil && vm.cmd.Process != nil {
		_ = vm.cmd.Process.Kill()
		_, _ = vm.cmd.Process.Wait()
	}
	if vm.cancel != nil {
		vm.cancel()
	}
	vm.cmd = nil
	vm.cancel = nil
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

// Stop is the cold tier: pause at a quiesced boundary, snapshot to a
// self-contained bundle, then kill the helper to free RAM. Requires a block
// root (BlockRoot) so the rootfs survives the round-trip; a virtio-fs VM can be
// snapshotted but exec-after-restore breaks (the FUSE map isn't persisted).
func (e *Engine) Stop(ctx context.Context, id string) error {
	vm, err := e.getVM(id)
	if err != nil {
		return err
	}
	vm.mu.Lock()
	if vm.Status != "running" {
		vm.mu.Unlock()
		return nil
	}
	ctlUDS, bundleDir := vm.CtlSockUDS, vm.BundleDir
	vm.mu.Unlock()

	// Generous deadline: SNAPSHOT streams the whole guest RAM to disk.
	sctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	if _, err := controlCmd(sctx, ctlUDS, "PAUSE"); err != nil {
		return fmt.Errorf("stop: pause: %w", err)
	}
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		return fmt.Errorf("stop: bundle dir: %w", err)
	}
	if _, err := controlCmd(sctx, ctlUDS, "SNAPSHOT "+bundleDir); err != nil {
		return fmt.Errorf("stop: snapshot: %w", err)
	}
	vm.kill()
	vm.mu.Lock()
	vm.Status = "stopped"
	vm.Thermal = "cold"
	vm.Agent = nil
	vm.mu.Unlock()
	slog.Info("krucible sandbox stopped (cold)", "id", id, "bundle", bundleDir)
	return nil
}

// Start cold-restores a stopped sandbox from its snapshot bundle: re-launch the
// helper with the bundle, restoring RAM + device + vCPU state and resuming from
// the snapshot point.
func (e *Engine) Start(ctx context.Context, id string) error {
	vm, err := e.getVM(id)
	if err != nil {
		return err
	}
	vm.mu.Lock()
	if vm.Status == "running" {
		vm.mu.Unlock()
		return nil
	}
	bundleDir := vm.BundleDir
	vm.mu.Unlock()
	if _, err := os.Stat(filepath.Join(bundleDir, "checkpoint.bin")); err != nil {
		return fmt.Errorf("start: no snapshot bundle at %s: %w", bundleDir, err)
	}
	if err := e.launch(ctx, vm, bundleDir); err != nil {
		return fmt.Errorf("start (cold restore): %w", err)
	}
	slog.Info("krucible sandbox started (cold restore)", "id", id)
	return nil
}

// cloneBaseImage ensures the shared base ext4 image exists (built once from
// BaseRootfs) and CoW-clones it to dst (per-sandbox root disk).
func (e *Engine) cloneBaseImage(dst string) error {
	base := filepath.Join(e.cfg.DataDir, "base.img")
	e.baseImgMu.Lock()
	if _, err := os.Stat(base); err != nil {
		if berr := buildBaseImage(e.cfg.BaseRootfs, base); berr != nil {
			e.baseImgMu.Unlock()
			return berr
		}
	}
	e.baseImgMu.Unlock()
	return cloneFile(base, dst)
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
