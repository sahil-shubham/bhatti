package krucible

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent"
	"github.com/sahil-shubham/bhatti/pkg/configdrive"
	"github.com/sahil-shubham/bhatti/pkg/engine"
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
	// BlockRoot boots sandboxes from an ext4 block image (CoW-cloned per
	// sandbox from a base built once from BaseRootfs) instead of a virtio-fs
	// host dir. Required for the cold tier (Stop/Start): the block image is the
	// self-contained, snapshot-surviving rootfs (see docs/PLAN-krucible-cold-tier.md).
	BlockRoot bool
	// KernelImage, if set, boots an external kernel (e.g. a lean one) instead of
	// libkrunfw's bundled kernel. Block-root only (the cmdline roots on
	// /dev/vda). arm64 = raw `Image`, x86 = ELF vmlinux.
	KernelImage string
}

// maxUnixPath is the conservative AF_UNIX sun_path cap (macOS = 104).
const maxUnixPath = 104

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
	launchMu   sync.Mutex
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
	HelperPID  int // bhatti-vmm pid, persisted so recovery can adopt/kill it after a daemon restart
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
	eng := &Engine{vms: make(map[string]*VM), cfg: cfg}
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

// createOpts carries restore hints for create(): cold-restore from a snapshot
// bundle (snapshotDir), reuse a memory snapshot's in-guest token (forcedToken),
// and use a prebuilt config drive (configDrive) so the restored VM's /dev/vdb
// matches the captured device state. All empty = a fresh boot.
type createOpts struct {
	snapshotDir string
	forcedToken string
	configDrive string
}

// Create boots a new sandbox: prepare the rootfs (block image clone or
// virtio-fs dir), spawn bhatti-vmm, wait for lohar's agent over the bridged vsock.
func (e *Engine) Create(ctx context.Context, spec engine.SandboxSpec) (engine.SandboxInfo, error) {
	return e.create(ctx, spec, createOpts{})
}

func (e *Engine) create(ctx context.Context, spec engine.SandboxSpec, opts createOpts) (info engine.SandboxInfo, err error) {
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

	name := spec.Name
	if name == "" {
		name = id
	}

	// Per-sandbox auth token, carried into the guest via the config drive; the
	// agent enforces it. Empty on the config-less virtio-fs dev path (no auth).
	// On a memory-snapshot restore, reuse the snapshot's token (the restored guest
	// enforces it from RAM).
	token := opts.forcedToken

	// Rootfs + config drive. Block-root pairs root=/dev/vda with the config drive
	// at /dev/vdb; the virtio-fs path stays the minimal config-less dev profile.
	if e.cfg.BlockRoot {
		// Per-create image (image pull / image save / snapshot), falling back to
		// the engine's default base. The image is the root's CoW backing.
		base, berr := e.resolveBase(spec)
		if berr != nil {
			return info, berr
		}
		var rootImg string
		if rootQcow2() {
			// Default: a qcow2 CoW root — instant + host-FS-independent (no
			// reflink/btrfs). Raw is the opt-out (KRUCIBLE_ROOT_RAW=1).
			rootImg = filepath.Join(sandboxDir, "root.qcow2")
			if isQcow2(base) {
				// A saved qcow2 image is already a CoW node over the raw base;
				// copy it as this sandbox's root (it keeps backing that base).
				if err = cloneFile(base, rootImg); err != nil {
					return info, fmt.Errorf("clone qcow2 image: %w", err)
				}
			} else if err = e.createRootOverlayQcow2(rootImg, base); err != nil {
				return info, fmt.Errorf("create qcow2 root overlay: %w", err)
			}
			baseSpec.RootDiskFormat = "qcow2"
		} else {
			rootImg = filepath.Join(sandboxDir, "root.img")
			if err = cloneFile(base, rootImg); err != nil {
				return info, fmt.Errorf("clone base image: %w", err)
			}
		}
		baseSpec.RootDisk = rootImg
		baseSpec.KernelImage = e.cfg.KernelImage // external (lean) kernel, if configured

		if token == "" {
			token = genToken()
		}
		confPath := filepath.Join(sandboxDir, "config.ext4")
		if opts.configDrive != "" {
			// Restore: reuse the snapshot's config drive so the restored VM's
			// /dev/vdb matches the captured device state (the guest resumes from
			// RAM and never re-reads it).
			if err = cloneFile(opts.configDrive, confPath); err != nil {
				return info, fmt.Errorf("copy snapshot config drive: %w", err)
			}
		} else if err = buildConfigDrive(confPath, id, name, token, spec); err != nil {
			return info, fmt.Errorf("build config drive: %w", err)
		}
		baseSpec.ConfigDrive = confPath
	} else {
		if err = cloneTree(e.cfg.BaseRootfs, rootfsDir); err != nil {
			return info, fmt.Errorf("clone rootfs: %w", err)
		}
		baseSpec.RootfsDir = rootfsDir
	}

	vm = &VM{
		ID: id, Name: name, UserID: spec.UserID,
		SandboxDir: sandboxDir, RootfsDir: rootfsDir, SockDir: sockDir,
		ControlUDS: controlUDS, ForwardUDS: forwardUDS, CtlSockUDS: ctlSockUDS,
		MemMiB: memMiB, Thermal: "hot", Status: "stopped", Token: token,
		BundleDir: filepath.Join(sandboxDir, "bundle"),
		baseSpec:  baseSpec,
		logPath:   filepath.Join(sandboxDir, "vmm.log"),
	}

	if err = e.launch(ctx, vm, opts.snapshotDir); err != nil {
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
	// Detach the helper into its own process group so it survives a daemon
	// restart/crash — recovery can then re-adopt the live VM. (darwin + linux.)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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
	vm.HelperPID = cmd.Process.Pid
	vm.Agent = ag
	vm.Status = "running"
	vm.Thermal = "hot"
	vm.mu.Unlock()
	vm.persist()
	return nil
}

// kill terminates the helper (best effort). Uses the Cmd handle when we own the
// process, else the persisted pid (a helper adopted across a daemon restart).
func (vm *VM) kill() {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if vm.cmd != nil && vm.cmd.Process != nil {
		_ = vm.cmd.Process.Kill()
		_, _ = vm.cmd.Process.Wait()
	} else if vm.HelperPID > 0 {
		// Adopted helper (not our child) — signal by pid; init reaps it.
		_ = syscall.Kill(vm.HelperPID, syscall.SIGKILL)
	}
	if vm.cancel != nil {
		vm.cancel()
	}
	vm.cmd = nil
	vm.cancel = nil
	vm.HelperPID = 0
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
	vm.mu.Lock()
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
	vm.launchMu.Lock()
	defer vm.launchMu.Unlock()
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
		// Snapshot failed (e.g. out of disk for memory.img). The guest is still
		// PAUSED from above — un-pause it so it isn't left frozen (a frozen guest
		// hangs the next exec). Then surface the error; the sandbox stays usable.
		_, _ = controlCmd(sctx, ctlUDS, "RESUME")
		return fmt.Errorf("stop: snapshot: %w", err)
	}
	vm.kill()
	vm.mu.Lock()
	vm.Status = "stopped"
	vm.Thermal = "cold"
	vm.Agent = nil
	vm.mu.Unlock()
	vm.persist()
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
	vm.launchMu.Lock()
	defer vm.launchMu.Unlock()
	vm.mu.Lock()
	if vm.Status == "running" {
		vm.mu.Unlock()
		return nil
	}
	bundleDir := vm.BundleDir
	vm.mu.Unlock()
	// Restore from the cold bundle if present; otherwise cold-boot fresh — a
	// crashed or never-snapshotted sandbox whose RAM is gone but whose rootfs
	// image persists. Recovery relies on this for restart-safety.
	snapshot, mode := "", "fresh boot"
	if validateBundle(bundleDir) == nil {
		snapshot, mode = bundleDir, "cold restore"
	}
	if err := e.launch(ctx, vm, snapshot); err != nil {
		return fmt.Errorf("start (%s): %w", mode, err)
	}
	slog.Info("krucible sandbox started", "id", id, "mode", mode)
	return nil
}

// krucibleProtoVer is the snapshot bundle protocol version this build can
// restore. Mirrors libkrun's CHECKPOINT_VERSION / the manifest proto_ver.
const krucibleProtoVer = 1

type bundleManifest struct {
	ProtoVer  int    `json:"proto_ver"`
	Arch      string `json:"arch"`
	VcpuCount int    `json:"vcpu_count"`
}

// hostSnapshotArch maps Go's GOARCH to the arch string libkrun writes into a
// bundle manifest.
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

// validateBundle is bhatti's portability gate (Tier-2 of the cold/move design):
// refuse a snapshot bundle that can't be restored on this host — incomplete,
// wrong proto version, or cross-arch (a bundle moved from a different machine)
// — before spawning the helper, so the failure is a clear error, not a guest
// crash mid-restore.
func validateBundle(bundleDir string) error {
	for _, f := range []string{"manifest.json", "checkpoint.bin", "memory.img"} {
		if _, err := os.Stat(filepath.Join(bundleDir, f)); err != nil {
			return fmt.Errorf("incomplete snapshot bundle (missing %s): %w", f, err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(bundleDir, "manifest.json"))
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	var m bundleManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	if m.ProtoVer != krucibleProtoVer {
		return fmt.Errorf("bundle proto_ver %d != %d (incompatible snapshot, re-snapshot)", m.ProtoVer, krucibleProtoVer)
	}
	if want := hostSnapshotArch(); m.Arch != want {
		return fmt.Errorf("bundle arch %q != host %q (cross-arch restore not supported)", m.Arch, want)
	}
	return nil
}

// genToken returns a random 128-bit hex token (matches the FC engine's scheme).
func genToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// buildConfigDrive writes the per-sandbox config drive (hostname, token, env,
// files) that lohar reads at /dev/vdb. Secrets are expected to be pre-resolved
// into spec.Env by the server layer (same contract as the FC engine).
func buildConfigDrive(path, id, name, token string, spec engine.SandboxSpec) error {
	files := make(map[string]configdrive.ConfigFile, len(spec.Files))
	for p, f := range spec.Files {
		files[p] = configdrive.ConfigFile{
			Content: base64.StdEncoding.EncodeToString(f.Content),
			Mode:    f.Mode,
		}
	}
	return configdrive.Build(path, configdrive.SandboxConfig{
		SandboxID: id,
		Hostname:  name,
		Token:     token,
		Env:       spec.Env,
		Files:     files,
		User:      "lohar",
	})
}

// cloneBaseImage CoW-clones the shared base ext4 image to dst (per-sandbox root
// disk). The base is either a prebuilt image (BaseImage, the production path) or
// one built once from BaseRootfs via mke2fs (the dev path).
// resolveBase returns the CoW backing image for a new sandbox's root: the
// per-create image (spec.BaseImage — set by the server for `image pull`,
// `image save`, snapshot), else the engine's default BaseImage, else a dev base
// built once from BaseRootfs. May be raw (a fresh base) or qcow2 (a saved image
// / snapshot, itself a CoW node over a raw base).
func (e *Engine) resolveBase(spec engine.SandboxSpec) (string, error) {
	if spec.BaseImage != "" {
		return spec.BaseImage, nil
	}
	if e.cfg.BaseImage != "" {
		return e.cfg.BaseImage, nil
	}
	base := filepath.Join(e.cfg.DataDir, "base.img")
	e.baseImgMu.Lock()
	defer e.baseImgMu.Unlock()
	if _, err := os.Stat(base); err != nil {
		if berr := buildBaseImage(e.cfg.BaseRootfs, base); berr != nil {
			return "", berr
		}
	}
	return base, nil
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
// at dst (the per-sandbox root) — instant + host-FS-independent. The daemon is
// pure Go (never links libkrun), so it shells to the cgo helper, which creates
// the overlay via libkrun/imago (krun_create_disk_overlay) — reusing the same
// library that opens these images, with no external tool (no qemu-img).
func (e *Engine) createRootOverlayQcow2(dst, base string) error {
	fi, err := os.Stat(base)
	if err != nil {
		return fmt.Errorf("stat base image %s: %w", base, err)
	}
	cmd := exec.Command(e.cfg.VMMBinary, "create-overlay", dst, base, strconv.FormatInt(fi.Size(), 10))
	if e.cfg.LibDir != "" {
		cmd.Env = append(os.Environ(),
			"DYLD_FALLBACK_LIBRARY_PATH="+e.cfg.LibDir,
			"LD_LIBRARY_PATH="+e.cfg.LibDir,
		)
	}
	if out, cerr := cmd.CombinedOutput(); cerr != nil {
		return fmt.Errorf("create qcow2 overlay (%s -> %s): %w: %s", base, dst, cerr, out)
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
