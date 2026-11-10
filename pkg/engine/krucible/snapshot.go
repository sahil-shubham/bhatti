package krucible

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
	"github.com/sahil-shubham/bhatti/pkg/configdrive"
	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/gateway"
)

// krucibleSnapManifest is the krucible snapshot manifest the server stores
// (SnapshotRecord.ManifestJSON) and hands back to ResumeFromManifestJSON. It
// records the captured disk + config drive (within the snapshot dir) and the
// in-guest token to reuse on a memory restore.
type krucibleSnapManifest struct {
	Arch        string `json:"arch"`
	Type        string `json:"type"` // "memory" (RAM+disk) | "filesystem" (disk-only)
	Vcpus       uint8  `json:"vcpus"`
	MemMiB      uint32 `json:"mem_mib"`
	DiskFile    string `json:"disk_file"`
	DiskFormat  string `json:"disk_format,omitempty"`
	ConfigFile  string `json:"config_file"`
	Token       string `json:"token"`
	KernelImage string `json:"kernel_image,omitempty"`
	// Backing records the strictly validated captured graph, normalizing private
	// legacy copies first. The local manifest is authoritative, not an archive.
	Backing []diskLayer `json:"backing"`

	// Device set (so a restore/fork reproduces it — a memory restore's RAM view of
	// its disks/mounts must stay valid). Mounts are re-bound to the same host dirs;
	// volumes are frozen into the snapshot dir and cloned into the restored sandbox.
	Mounts  []snapMount  `json:"mounts,omitempty"`
	Volumes []snapVolume `json:"volumes,omitempty"`
	// The source's network posture: whether it has a NIC is part of the device
	// set, and a restore or fork keeps the source's egress rules rather than
	// opening up a sandbox that had none.
	NetPolicy *gateway.NetPolicyWire `json:"net_policy,omitempty"`
}

// snapMount records a virtio-fs bind for restore. Only HostPath + ReadOnly are
// needed: the tag is positional (reassigned by order) and the guest mount path
// lives in the captured config drive (and, for a memory restore, in guest RAM).
type snapMount struct {
	HostPath string `json:"host_path"`
	ReadOnly bool   `json:"read_only"`
}

// snapVolume records a frozen data volume: its file within the snapshot dir +
// read-only flag. The guest mount path lives in the captured config drive.
type snapVolume struct {
	File     string      `json:"file"`
	Format   string      `json:"format,omitempty"`
	Backing  []diskLayer `json:"backing"`
	ReadOnly bool        `json:"read_only"`
}

// Checkpoint captures a named, sandbox-independent snapshot (the server's
// `checkpointer` capability): a copy of the running sandbox's disk + config
// drive, plus — for a "memory" snapshot — its RAM + device + vCPU state, into
// snapDir/snapName/, WITHOUT stopping the sandbox. Restore via
// ResumeFromManifestJSON. Defaults to "memory".
func (e *Engine) Checkpoint(ctx context.Context, sandboxID, userID string, subnetIndex int, snapName, snapDir string) (any, error) {
	return e.checkpoint(ctx, sandboxID, snapName, snapDir, "memory")
}

// CheckpointTyped is Checkpoint with an explicit type ("memory" | "filesystem")
// — the server's optional typedCheckpointer capability for `snapshot create
// --type`. "filesystem" is disk-only (restores with a cold boot).
func (e *Engine) CheckpointTyped(ctx context.Context, sandboxID, userID string, subnetIndex int, snapName, snapDir, snapType string) (any, error) {
	if snapType == "" {
		snapType = "memory"
	}
	if snapType != "memory" && snapType != "filesystem" {
		return nil, fmt.Errorf("unknown snapshot type %q (want memory|filesystem)", snapType)
	}
	return e.checkpoint(ctx, sandboxID, snapName, snapDir, snapType)
}

func (e *Engine) checkpoint(ctx context.Context, sandboxID, snapName, snapDir, snapType string) (any, error) {
	unlock, err := lockBasesCtx(ctx, e.cfg.DataDir, false)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: lock nodes: %w", err)
	}
	defer unlock()
	return e.checkpointLocked(ctx, sandboxID, snapName, snapDir, snapType)
}

// checkpointLocked requires the shared bases lease before launchMu. Keep it
// until capture, source resume, manifest publication and failed-directory
// cleanup have all finished; a new checkpoint is not a GC root until then.
func (e *Engine) checkpointLocked(ctx context.Context, sandboxID, snapName, snapDir, snapType string) (manifest any, err error) {
	if snapType == "memory" && !e.caps.Checkpoint {
		return nil, errNoCheckpoint
	}
	if snapType == "filesystem" && !e.caps.Pause {
		return nil, errNoPause
	}
	finalDir, err := filepath.Abs(filepath.Join(snapDir, snapName))
	if err != nil {
		return nil, fmt.Errorf("checkpoint: absolute snapshot dir: %w", err)
	}
	if _, err := os.Stat(finalDir); err == nil {
		return nil, fmt.Errorf("snapshot %q %w", snapName, engine.ErrSnapshotExists)
	}
	if err := e.EnsureHot(ctx, sandboxID); err != nil {
		return nil, fmt.Errorf("checkpoint: ensure hot: %w", err)
	}
	vm, err := e.getVM(sandboxID)
	if err != nil {
		return nil, err
	}
	vm.mu.Lock()
	spec := vm.baseSpec
	token := vm.Token
	netPolicy := vm.netPolicy
	vm.mu.Unlock()
	if spec.RootDiskFormat != "qcow2" || spec.RootDisk == "" {
		return nil, fmt.Errorf("snapshot requires a qcow2 block-root sandbox")
	}
	// A memory snapshot captures device + vCPU state; libkrun cannot restore a
	// virtio-fs device from that state (the resume hangs/fails), so refuse rather
	// than produce an unrestorable snapshot. Block volumes are fine. A filesystem
	// snapshot (disk-only, cold-boot restore) has no captured device state and
	// works for mounted sandboxes.
	if snapType == "memory" && len(spec.Mounts) > 0 {
		return nil, fmt.Errorf("cannot memory-snapshot/fork a sandbox with a virtio-fs --mount (the device cannot be restored); use a filesystem snapshot (--type filesystem)")
	}

	vm.launchMu.Lock()
	defer vm.launchMu.Unlock()
	baseline, err := e.captureDiskBacking(vm, &spec)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: source disk backing: %w", err)
	}

	if err := os.MkdirAll(snapDir, 0o700); err != nil {
		return nil, fmt.Errorf("checkpoint: snapshot parent: %w", err)
	}
	ownDir := false
	defer func() {
		if err != nil && ownDir {
			_ = os.RemoveAll(finalDir)
			if baseline.legacy() {
				e.scheduleNodeSweep()
			}
		}
	}()
	if snapType == "filesystem" {
		if err := os.Mkdir(finalDir, 0o700); err != nil {
			return nil, fmt.Errorf("checkpoint: snapshot dir: %w", err)
		}
		ownDir = true
	}

	// Flush the guest page cache to the device.
	syncCtx, syncCancel := context.WithTimeout(ctx, 10*time.Second)
	_, _ = e.Exec(syncCtx, sandboxID, []string{"sync"})
	syncCancel()

	limit := 60 * time.Second
	if snapType == "memory" {
		limit = 120 * time.Second // SAVE may stream a large guest's RAM.
	}
	cctx, ccancel := context.WithTimeout(ctx, limit)
	defer ccancel()
	// What the source is held for: from the command that parks it to its
	// resume. Logged when it ends, so a snapshot or fork that stalls a busy
	// sandbox shows what the stall was made of.
	pausedAt := time.Now()
	var saveDur, flushDur, copyDur time.Duration
	var stage string
	if snapType == "filesystem" {
		if _, err := controlCmd(cctx, vm.CtlSockUDS, "PAUSE"); err != nil {
			return nil, fmt.Errorf("checkpoint: pause: %w", err)
		}
	} else {
		// SAVE creates a directory, freezes RAM/devices into it and leaves the
		// VM paused. No separate PAUSE: SAVE itself owns the quiesce and
		// failure cleanup. The helper may only create files in its save dir,
		// so it saves there and the result is moved to finalDir below, with
		// the resume already deferred.
		s, serr := saveStage(vm)
		if serr != nil {
			return nil, fmt.Errorf("checkpoint: %w", serr)
		}
		if _, err := controlCmd(cctx, vm.CtlSockUDS, "SAVE "+s); err != nil {
			_ = os.RemoveAll(s)
			return nil, fmt.Errorf("checkpoint: save: %w", err)
		}
		stage = s
		ownDir = true
		saveDur = time.Since(pausedAt)
	}
	// The source runs again as soon as its disks are copied, not when the
	// snapshot is on stable storage: resume is explicit below, and deferred
	// for every way out before it.
	resumed := false
	resume := func() error {
		if resumed {
			return nil
		}
		resumed = true
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer rcancel()
		if _, resumeErr := controlCmd(rctx, vm.CtlSockUDS, "RESUME"); resumeErr != nil {
			return fmt.Errorf("checkpoint: resume: %w", resumeErr)
		}
		vm.syncGuestClock(rctx)
		slog.Info("krucible.checkpoint.paused", "id", sandboxID, "type", snapType,
			"paused_ms", time.Since(pausedAt).Milliseconds(), "save_ms", saveDur.Milliseconds(),
			"flush_ms", flushDur.Milliseconds(), "copy_ms", copyDur.Milliseconds())
		return nil
	}
	defer func() {
		if rerr := resume(); rerr != nil {
			err = errors.Join(err, rerr)
			manifest = nil
		}
	}()
	if err := baseline.validate(spec); err != nil {
		if stage != "" {
			_ = os.RemoveAll(stage)
		}
		return nil, fmt.Errorf("checkpoint: paused source disk backing: %w", err)
	}
	if stage != "" {
		if err := e.takeSave(stage, finalDir); err != nil {
			_ = os.RemoveAll(stage)
			return nil, fmt.Errorf("checkpoint: save: %w", err)
		}
	}
	// Copy disks at the saved VM's quiescent point. Guest writes cannot
	// resume until after all copies finish. What is copied is flushed first
	// (a copy that shares blocks, as on a reflinking file system, shares
	// what is on disk), and only that: a host-wide sync would wait for every
	// other writer on the host and for the memory image just saved, none of
	// which a copy needs. The snapshot as a whole is made durable below, with
	// the source running again.
	t := time.Now()
	toCopy := []string{spec.RootDisk}
	if snapType == "memory" {
		for _, v := range spec.Volumes {
			toCopy = append(toCopy, v.Path)
		}
	}
	if err := flushFiles(toCopy...); err != nil {
		return nil, fmt.Errorf("checkpoint: flush disk: %w", err)
	}
	flushDur = time.Since(t)
	t = time.Now()
	if err := cloneFile(cctx, spec.RootDisk, filepath.Join(finalDir, "rootfs.qcow2")); err != nil {
		return nil, fmt.Errorf("checkpoint: copy disk: %w", err)
	}
	copyDur = time.Since(t)
	// Compare against the source baseline, not a header the helper can rewrite.
	// Validate the daemon-owned copy so a change during the copy cannot turn
	// a captured disk into authority to read another sandbox's node.
	rootBacking, err := e.normalizeCapturedDisk(cctx, filepath.Join(finalDir, "rootfs.qcow2"),
		spec.RootDisk, spec.RootDiskFormat, spec.Backing, baseline.rootLegacy)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: captured root disk: %w", err)
	}

	// Capture the device set (memory snapshots only — the restored RAM's view of
	// its disks + mounts must stay valid; a filesystem snapshot is disk-only).
	// Volumes are frozen as independent copies, consistent with the paused VM +
	// the RAM snapshot; virtio-fs mounts are re-bound to the same host dirs.
	var snapVols []snapVolume
	var snapMounts []snapMount
	var snapNet *gateway.NetPolicyWire
	if snapType == "memory" {
		t = time.Now()
		for i, v := range spec.Volumes {
			file := fmt.Sprintf("vol%d.img", i)
			if err := cloneFile(cctx, v.Path, filepath.Join(finalDir, file)); err != nil {
				return nil, fmt.Errorf("checkpoint: copy volume %d: %w", i, err)
			}
			backing, err := e.normalizeCapturedDisk(cctx, filepath.Join(finalDir, file),
				v.Path, v.Format, v.Backing, baseline.legacyVolume(i))
			if err != nil {
				return nil, fmt.Errorf("checkpoint: captured volume %d: %w", i, err)
			}
			format := v.Format
			if format == "" {
				format = "raw"
			}
			snapVols = append(snapVols, snapVolume{File: file, Format: format, Backing: backing, ReadOnly: v.ReadOnly})
		}
		copyDur += time.Since(t)
		for _, mnt := range spec.Mounts {
			snapMounts = append(snapMounts, snapMount{HostPath: mnt.HostPath, ReadOnly: mnt.ReadOnly})
		}
		snapNet = netPolicy
	}
	if rerr := resume(); rerr != nil {
		return nil, rerr
	}
	m := krucibleSnapManifest{
		Arch: hostSnapshotArch(), Type: snapType,
		Vcpus: spec.Vcpus, MemMiB: spec.MemMiB,
		DiskFile: "rootfs.qcow2", DiskFormat: spec.RootDiskFormat, ConfigFile: "config.ext4",
		Token: token, KernelImage: spec.KernelImage, Backing: rootBacking,
		Mounts: snapMounts, Volumes: snapVols, NetPolicy: snapNet,
	}
	if snapType == "memory" {
		if err := recordGuestBoot(vm.SandboxDir, finalDir); err != nil {
			return nil, fmt.Errorf("checkpoint: boot settings: %w", err)
		}
	}
	if err := writeSnapshotManifest(finalDir, m); err != nil {
		return nil, fmt.Errorf("checkpoint: manifest: %w", err)
	}
	if err := syncTree(finalDir); err != nil {
		return nil, fmt.Errorf("checkpoint: sync snapshot: %w", err)
	}
	slog.Info("krucible snapshot created", "id", sandboxID, "name", snapName, "type", snapType, "dir", finalDir, "volumes", len(snapVols), "mounts", len(snapMounts))
	return m, nil
}

// recordGuestBoot keeps the source's guestBoot with a memory snapshot. A
// source with no boot config (from before configs moved host-side) has none.
func recordGuestBoot(sandboxDir, snapDir string) error {
	raw, err := os.ReadFile(filepath.Join(sandboxDir, "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var cfg configdrive.SandboxConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("parse config.json: %w", err)
	}
	return writeDaemonMetadata(filepath.Join(snapDir, snapshotBootFile),
		guestBoot{Env: cfg.Env, Files: cfg.Files, Init: cfg.Init})
}

// ResumeFromManifestJSON creates a NEW sandbox restored from a memory snapshot
// (the server's `snapshotResumer` capability): its root and volumes are disks
// of its own layered over the snapshot's (nodes.go); it restores RAM/device/vCPU
// state and reuses the in-guest token (the restored guest enforces it from RAM).
// ownerUserID homes the new sandbox on the owner's shared netd — a fork/restore is an
// INDEPENDENT sandbox of the same owner, so it must join that owner's fabric
// (and get a fresh, distinct IP there), not spin up an isolated netd it
// shares with nothing.
//
// The snapshot's files are never written: they are what every sandbox
// resumed from it reads through.
func (e *Engine) ResumeFromManifestJSON(ctx context.Context, snapDir string, manifestJSON []byte, newName, ownerUserID string) (engine.SandboxInfo, error) {
	return e.resume(ctx, snapDir, manifestJSON, newName, ownerUserID, false)
}

// resume is ResumeFromManifestJSON. own says snapDir is private to this call
// (a fork's staging copy), so a disk that can't be layered over is moved
// rather than copied again.
func (e *Engine) resume(ctx context.Context, snapDir string, manifestJSON []byte, newName, ownerUserID string, own bool) (engine.SandboxInfo, error) {
	unlock, err := lockBasesCtx(ctx, e.cfg.DataDir, false)
	if err != nil {
		return engine.SandboxInfo{}, fmt.Errorf("resume: lock nodes: %w", err)
	}
	defer unlock()
	m, local, err := readSnapshotManifest(snapDir)
	if err != nil {
		return engine.SandboxInfo{}, fmt.Errorf("read snapshot manifest: %w", err)
	}
	if !local {
		if err := json.Unmarshal(manifestJSON, &m); err != nil {
			return engine.SandboxInfo{}, fmt.Errorf("parse snapshot manifest: %w", err)
		}
		// Old API records never recorded verified backing provenance.
		m.Backing = nil
		for i := range m.Volumes {
			m.Volumes[i].Backing = nil
		}
	}
	m.normalize()
	if _, err := archiveSnapshotFiles(m); err != nil {
		return engine.SandboxInfo{}, err
	}
	if m.Arch != "" && m.Arch != hostSnapshotArch() {
		return engine.SandboxInfo{}, fmt.Errorf("snapshot arch %q != host %q (cross-arch restore not supported)", m.Arch, hostSnapshotArch())
	}
	source := VMSpec{RootDisk: filepath.Join(snapDir, m.DiskFile), RootDiskFormat: m.DiskFormat, Backing: m.Backing}
	owner := &VM{UserID: ownerUserID}
	if local {
		if m.DiskFormat != "raw" && m.DiskFormat != "qcow2" {
			return engine.SandboxInfo{}, fmt.Errorf("snapshot: missing or invalid recorded root format %q", m.DiskFormat)
		}
		if source.Backing == nil {
			source.Backing = []diskLayer{}
		}
	} else {
		// No local record means no format or path from the API record can
		// authorize a backing file. Infer only after checking each target.
		chain, err := e.legacyDiskChain(owner, source.RootDisk, "")
		if err != nil {
			return engine.SandboxInfo{}, fmt.Errorf("snapshot backing: %w", err)
		}
		source.RootDiskFormat = "raw"
		if isQcow2(source.RootDisk) {
			source.RootDiskFormat = "qcow2"
		}
		source.Backing = chain
	}
	for i := range m.Volumes {
		v := &m.Volumes[i]
		path := filepath.Join(snapDir, v.File)
		if local {
			if v.Format != "raw" && v.Format != "qcow2" {
				return engine.SandboxInfo{}, fmt.Errorf("snapshot volume %s: missing or invalid recorded format %q", v.File, v.Format)
			}
			if v.Backing == nil {
				v.Backing = []diskLayer{}
			}
		} else {
			chain, err := e.legacyDiskChain(owner, path, "")
			if err != nil {
				return engine.SandboxInfo{}, fmt.Errorf("snapshot volume %s backing: %w", v.File, err)
			}
			v.Format = "raw"
			if isQcow2(path) {
				v.Format = "qcow2"
			}
			v.Backing = chain
		}
		source.Volumes = append(source.Volumes, VMVolume{Path: path, Format: v.Format, Backing: v.Backing})
	}
	if !local {
		stage := ""
		defer func() {
			if stage != "" {
				_ = os.RemoveAll(stage)
				e.scheduleNodeSweep()
			}
		}()
		normalize := func(path, format string, chain []diskLayer) (string, []diskLayer, error) {
			if err := validateRecordedDisk(path, format, chain); err == nil {
				return path, chain, nil
			}
			if stage == "" {
				var err error
				stage, err = os.MkdirTemp(e.cfg.DataDir, "legacy-resume-")
				if err != nil {
					return "", nil, err
				}
			}
			return e.normalizeLegacyDisk(ctx, stage, path, format, chain)
		}
		source.RootDisk, source.Backing, err = normalize(source.RootDisk, source.RootDiskFormat, source.Backing)
		if err != nil {
			return engine.SandboxInfo{}, fmt.Errorf("normalize legacy root: %w", err)
		}
		for i := range source.Volumes {
			v := &source.Volumes[i]
			v.Path, v.Backing, err = normalize(v.Path, v.Format, v.Backing)
			if err != nil {
				return engine.SandboxInfo{}, fmt.Errorf("normalize legacy volume %d: %w", i, err)
			}
		}
	}
	if err := e.validateDiskBacking(owner, source); err != nil {
		return engine.SandboxInfo{}, fmt.Errorf("snapshot backing: %w", err)
	}
	// No CACert: the server owns the CA a copy inherits and sets it with SetCACert.
	spec := engine.SandboxSpec{
		Name:     newName,
		CPUs:     float64(m.Vcpus),
		MemoryMB: int(m.MemMiB),
		UserID:   ownerUserID, // join the owner's shared netd
	}
	opts := createOpts{nodeDisk: source.RootDisk, nodeFormat: source.RootDiskFormat,
		nodeOwned: own || source.RootDisk != filepath.Join(snapDir, m.DiskFile)}
	if m.Type == "filesystem" {
		// Disk-only snapshot: cold-boot a fresh sandbox from the frozen disk
		// (new identity/config, no RAM restore).
		return e.create(ctx, spec, opts)
	}
	// Reproduce the captured device set: libkrun refuses to restore the guest's
	// RAM into any other. Block volumes restore cleanly: each frozen volume is
	// re-attached as a disk of the new sandbox's own over it (fork/restore
	// diverges from the source). Memory snapshots of mounted sandboxes are
	// refused at checkpoint (virtio-fs can't be restored), so m.Mounts is
	// empty here.
	spec.NetPolicy = m.NetPolicy
	for i, v := range source.Volumes {
		opts.restoreVolumes = append(opts.restoreVolumes, restoreVol{path: v.Path, format: v.Format,
			owned: own || v.Path != filepath.Join(snapDir, m.Volumes[i].File), readOnly: m.Volumes[i].ReadOnly})
	}
	opts.snapshotDir = snapDir
	opts.forcedToken = m.Token
	// Snapshots taken before boot settings were recorded, and imports, have none.
	var boot guestBoot
	found, err := readDaemonMetadata(filepath.Join(snapDir, snapshotBootFile), &boot)
	if err != nil {
		return engine.SandboxInfo{}, fmt.Errorf("snapshot boot settings: %w", err)
	}
	if found {
		opts.boot = &boot
	}
	return e.create(ctx, spec, opts)
}

// Fork checkpoints a running sandbox to a temporary directory, restores a
// second VM from it, then removes the directory. The new VM privately maps
// memory.bin: unlinking it does not invalidate an already running restore. Its
// disks stay as layers retained by the node store.
func (e *Engine) Fork(ctx context.Context, sandboxID, newName string) (info engine.SandboxInfo, err error) {
	// Fast, friendly refusal: a --mount sandbox can't be memory-forked (libkrun
	// can't restore a virtio-fs device from a memory snapshot). Check up front so
	// the caller gets a clear error immediately, instead of one surfacing from deep
	// inside checkpoint after an EnsureHot + PAUSE. checkpoint() enforces the same
	// invariant as the backstop.
	vm, err := e.getVM(sandboxID)
	if err != nil {
		return engine.SandboxInfo{}, err
	}
	vm.mu.Lock()
	hasMount := len(vm.baseSpec.Mounts) > 0
	networked := vm.netIP != ""
	ownerUserID := vm.UserID
	vm.mu.Unlock()
	if hasMount {
		return engine.SandboxInfo{}, fmt.Errorf("cannot fork a sandbox with a virtio-fs --mount (the device cannot be memory-restored); use a filesystem snapshot (snapshot create --type filesystem) then create --snapshot")
	}

	// A networked fork must reconcile the cloned NIC's MAC/IP. Refuse old
	// guests before checkpointing; no-network forks do not need this feature.
	if networked {
		if err := e.RequireGuestAgentFeature(ctx, sandboxID, proto.FeatureNetConfigMAC); err != nil {
			return engine.SandboxInfo{}, fmt.Errorf("fork: %w", err)
		}
	}

	// The exclusive sweeper may remove fork stages left by a dead daemon.
	// Keep this stage live until its disks have been handed to the child and
	// its other hard links removed, even between checkpoint and resume.
	unlock, err := lockBasesCtx(ctx, e.cfg.DataDir, false)
	if err != nil {
		return engine.SandboxInfo{}, fmt.Errorf("fork: lock nodes: %w", err)
	}
	defer unlock()

	tmp, err := os.MkdirTemp(e.cfg.DataDir, "fork-")
	if err != nil {
		return engine.SandboxInfo{}, fmt.Errorf("fork: temp dir: %w", err)
	}
	// Restore layers disks and maps RAM privately; Linux keeps the unlinked
	// memory.bin backing mapped in the fork after this directory is removed.
	defer func() {
		os.RemoveAll(tmp)
		if err != nil {
			// A node the fork linked had this directory for its only other
			// name, and no disk is layered over it now.
			e.scheduleNodeSweep()
		}
	}()

	manifest, err := e.checkpointLocked(ctx, sandboxID, "snap", tmp, "memory")
	if err != nil {
		return engine.SandboxInfo{}, fmt.Errorf("fork: checkpoint: %w", err)
	}
	manifestJSON, _ := json.Marshal(manifest)
	return e.resume(ctx, filepath.Join(tmp, "snap"), manifestJSON, newName, ownerUserID, true)
}

// SaveImage captures the sandbox's current filesystem as a reusable bootable
// image at destPath, without stopping the sandbox. Implements the server's
// imageSaver capability (Phase 2). The image is a qcow2 CoW node over the
// sandbox's base — instant to create (a small overlay copy) and bootable as a
// new sandbox's root via `create --image`.
func (e *Engine) SaveImage(ctx context.Context, sandboxID, destPath string) error {
	vm, err := e.getVM(sandboxID)
	if err != nil {
		return err
	}
	vm.mu.Lock()
	status, format := vm.Status, vm.baseSpec.RootDiskFormat
	vm.mu.Unlock()
	if status != "running" {
		return fmt.Errorf("sandbox %q is stopped — start it first", sandboxID)
	}
	if format != "qcow2" {
		return fmt.Errorf("save-image requires a qcow2 block-root sandbox (got %q)", format)
	}
	if err := e.freezeDisk(ctx, sandboxID, destPath); err != nil {
		return err
	}
	slog.Info("krucible image saved", "id", sandboxID, "dest", destPath)
	return nil
}

// freezeDisk writes a consistent point-in-time copy of the sandbox's qcow2 root
// overlay to dst without stopping the sandbox, the QuiesceCopy primitive:
//
//  1. flush the guest's page cache to the virtio-blk device (an in-guest `sync`
//     — pausing vCPUs alone does NOT flush guest dirty pages);
//  2. pause the VM (no further guest writes);
//  3. flush the host page cache so the overlay file on disk is complete;
//  4. copy the overlay (a small delta that still backs the shared base);
//  5. resume.
//
// dst is itself a bootable qcow2 root (backs the same base as the source).
func (e *Engine) freezeDisk(ctx context.Context, sandboxID, dst string) error {
	if !e.caps.Pause {
		return errNoPause // the consistent copy needs the VM paused
	}
	unlock, err := lockBasesCtx(ctx, e.cfg.DataDir, false)
	if err != nil {
		return fmt.Errorf("freeze: lock nodes: %w", err)
	}
	defer unlock()
	// Legacy normalization can adopt store-only ancestors. Retain this lease
	// through sidecar publication, failed-image cleanup, and source resume.
	// Reachable agent for the guest sync; releases its own lock before we take
	// launchMu below.
	if err := e.EnsureHot(ctx, sandboxID); err != nil {
		return fmt.Errorf("freeze: ensure hot: %w", err)
	}
	vm, err := e.getVM(sandboxID)
	if err != nil {
		return err
	}

	// Serialize against Stop/Start/Destroy for the whole pause→copy→resume so the
	// helper can't be killed mid-copy.
	vm.launchMu.Lock()
	defer vm.launchMu.Unlock()
	spec := vm.baseSpec
	baseline, err := e.captureDiskBacking(vm, &spec)
	if err != nil {
		return fmt.Errorf("freeze: source disk backing: %w", err)
	}

	// 1. Flush guest page cache → device.
	syncCtx, syncCancel := context.WithTimeout(ctx, 10*time.Second)
	_, _ = e.Exec(syncCtx, sandboxID, []string{"sync"})
	syncCancel()

	// 2. Pause.
	pauseCtx, pauseCancel := context.WithTimeout(ctx, 30*time.Second)
	defer pauseCancel()
	if _, err := controlCmd(pauseCtx, vm.CtlSockUDS, "PAUSE"); err != nil {
		return fmt.Errorf("freeze: pause: %w", err)
	}
	// 5. Always resume, even if the copy fails or ctx is cancelled.
	defer func() {
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer rcancel()
		if _, err := controlCmd(rctx, vm.CtlSockUDS, "RESUME"); err == nil {
			vm.syncGuestClock(rctx)
		}
	}()
	if err := baseline.validate(spec); err != nil {
		return fmt.Errorf("freeze: paused source disk backing: %w", err)
	}

	// 3. Flush host page cache so the on-disk overlay reflects all writes.
	_ = exec.CommandContext(pauseCtx, "sync").Run()

	// 4. Copy the overlay (reflink where the host FS supports it; else a copy —
	// the overlay is a small delta).
	if err := cloneFile(pauseCtx, spec.RootDisk, dst); err != nil {
		return errors.Join(fmt.Errorf("freeze: copy overlay: %w", err), removeImageFiles(dst))
	}
	backing, err := e.normalizeCapturedDisk(pauseCtx, dst, spec.RootDisk, spec.RootDiskFormat, spec.Backing, baseline.rootLegacy)
	if err != nil {
		cleanupErr := removeImageFiles(dst)
		if baseline.rootLegacy {
			e.scheduleNodeSweep()
		}
		return errors.Join(fmt.Errorf("freeze: captured root disk: %w", err), cleanupErr)
	}
	if err := e.recordNodeMetadata(dst, spec.RootDiskFormat, backing); err != nil {
		cleanupErr := removeImageFiles(dst)
		if baseline.rootLegacy {
			e.scheduleNodeSweep()
		}
		return errors.Join(fmt.Errorf("freeze: record image backing: %w", err), cleanupErr)
	}
	return nil
}

// flushFiles pushes each file's written data to the file system, so a copy
// that shares blocks with the file shares all of it. It is a flush, not a
// durability barrier (no drive cache flush: that is syncTree's job, done where
// it stalls nothing): fsync(2) itself, which Go's File.Sync is not on darwin,
// where it is F_FULLFSYNC and costs a drive flush each.
func flushFiles(paths ...string) error {
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		err = syscall.Fsync(int(f.Fd()))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
	}
	return nil
}

// syncTree makes the files of dir durable, then dir's entries: what a
// snapshot is, once it returns, whatever happens to the host next.
func syncTree(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, de := range entries {
		if !de.Type().IsRegular() {
			continue
		}
		f, err := os.Open(filepath.Join(dir, de.Name()))
		if err != nil {
			return err
		}
		err = f.Sync()
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("%s: %w", de.Name(), err)
		}
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
