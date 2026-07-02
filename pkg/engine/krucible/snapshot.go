package krucible

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// krucibleSnapManifest is the krucible snapshot manifest the server stores
// (SnapshotRecord.ManifestJSON) and hands back to ResumeFromManifestJSON. It
// records the captured disk + config drive (within the snapshot dir) and the
// in-guest token to reuse on a memory restore.
type krucibleSnapManifest struct {
	ProtoVer    int    `json:"proto_ver"`
	Arch        string `json:"arch"`
	Type        string `json:"type"` // "memory" (RAM+disk) | "filesystem" (disk-only)
	Vcpus       uint8  `json:"vcpus"`
	MemMiB      uint32 `json:"mem_mib"`
	DiskFile    string `json:"disk_file"`
	ConfigFile  string `json:"config_file"`
	Token       string `json:"token"`
	KernelImage string `json:"kernel_image,omitempty"`

	// Device set (so a restore/fork reproduces it — a memory restore's RAM view of
	// its disks/mounts must stay valid). Mounts are re-bound to the same host dirs;
	// volumes are frozen into the snapshot dir and cloned into the restored sandbox.
	Mounts  []snapMount  `json:"mounts,omitempty"`
	Volumes []snapVolume `json:"volumes,omitempty"`
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
	File     string `json:"file"`
	ReadOnly bool   `json:"read_only"`
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
	finalDir := filepath.Join(snapDir, snapName)
	if _, err := os.Stat(finalDir); err == nil {
		return nil, fmt.Errorf("snapshot %q already exists", snapName)
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

	if err := os.MkdirAll(finalDir, 0o700); err != nil {
		return nil, fmt.Errorf("checkpoint: snapshot dir: %w", err)
	}
	done := false
	defer func() {
		if !done {
			os.RemoveAll(finalDir)
		}
	}()

	// Flush the guest page cache to the device.
	syncCtx, syncCancel := context.WithTimeout(ctx, 10*time.Second)
	_, _ = e.Exec(syncCtx, sandboxID, []string{"sync"})
	syncCancel()

	cctx, ccancel := context.WithTimeout(ctx, 60*time.Second)
	defer ccancel()
	if _, err := controlCmd(cctx, vm.CtlSockUDS, "PAUSE"); err != nil {
		return nil, fmt.Errorf("checkpoint: pause: %w", err)
	}
	defer func() {
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer rcancel()
		_, _ = controlCmd(rctx, vm.CtlSockUDS, "RESUME")
	}()

	// A memory snapshot captures RAM + device + vCPU state (libkrun writes
	// manifest.json/checkpoint.bin/memory.img). A filesystem snapshot is disk-only.
	if snapType == "memory" {
		if _, err := controlCmd(cctx, vm.CtlSockUDS, "SNAPSHOT "+finalDir); err != nil {
			return nil, fmt.Errorf("checkpoint: snapshot: %w", err)
		}
	}
	// Freeze the disk + config drive (the VM is paused; a host sync makes the
	// overlay file on disk complete).
	_ = exec.CommandContext(cctx, "sync").Run()
	if err := cloneFile(spec.RootDisk, filepath.Join(finalDir, "rootfs.qcow2")); err != nil {
		return nil, fmt.Errorf("checkpoint: copy disk: %w", err)
	}
	if spec.ConfigDrive != "" {
		if err := cloneFile(spec.ConfigDrive, filepath.Join(finalDir, "config.ext4")); err != nil {
			return nil, fmt.Errorf("checkpoint: copy config drive: %w", err)
		}
	}

	// Capture the device set (memory snapshots only — the restored RAM's view of
	// its disks + mounts must stay valid; a filesystem snapshot is disk-only).
	// Volumes are frozen as independent copies, consistent with the paused VM +
	// the RAM snapshot; virtio-fs mounts are re-bound to the same host dirs.
	var snapVols []snapVolume
	var snapMounts []snapMount
	if snapType == "memory" {
		for i, v := range spec.Volumes {
			file := fmt.Sprintf("vol%d.img", i)
			if err := cloneFile(v.Path, filepath.Join(finalDir, file)); err != nil {
				return nil, fmt.Errorf("checkpoint: copy volume %d: %w", i, err)
			}
			snapVols = append(snapVols, snapVolume{File: file, ReadOnly: v.ReadOnly})
		}
		for _, mnt := range spec.Mounts {
			snapMounts = append(snapMounts, snapMount{HostPath: mnt.HostPath, ReadOnly: mnt.ReadOnly})
		}
	}

	done = true
	slog.Info("krucible snapshot created", "id", sandboxID, "name", snapName, "type", snapType, "dir", finalDir, "volumes", len(snapVols), "mounts", len(snapMounts))
	return krucibleSnapManifest{
		ProtoVer: krucibleProtoVer, Arch: hostSnapshotArch(), Type: snapType,
		Vcpus: spec.Vcpus, MemMiB: spec.MemMiB,
		DiskFile: "rootfs.qcow2", ConfigFile: "config.ext4",
		Token: token, KernelImage: spec.KernelImage,
		Mounts: snapMounts, Volumes: snapVols,
	}, nil
}

// ResumeFromManifestJSON creates a NEW sandbox restored from a memory snapshot
// (the server's `snapshotResumer` capability): copy the snapshot's disk + config
// drive, cold-restore its RAM/device/vCPU state, reusing the in-guest token (the
// restored guest enforces it from RAM).
func (e *Engine) ResumeFromManifestJSON(ctx context.Context, snapDir string, manifestJSON []byte, newName string) (engine.SandboxInfo, error) {
	var m krucibleSnapManifest
	if err := json.Unmarshal(manifestJSON, &m); err != nil {
		return engine.SandboxInfo{}, fmt.Errorf("parse snapshot manifest: %w", err)
	}
	if m.Arch != "" && m.Arch != hostSnapshotArch() {
		return engine.SandboxInfo{}, fmt.Errorf("snapshot arch %q != host %q (cross-arch restore not supported)", m.Arch, hostSnapshotArch())
	}
	diskFile, configFile := m.DiskFile, m.ConfigFile
	if diskFile == "" {
		diskFile = "rootfs.qcow2"
	}
	if configFile == "" {
		configFile = "config.ext4"
	}
	spec := engine.SandboxSpec{
		Name:      newName,
		CPUs:      float64(m.Vcpus),
		MemoryMB:  int(m.MemMiB),
		BaseImage: filepath.Join(snapDir, diskFile), // the frozen disk; create() copies it as the root
	}
	if m.Type == "filesystem" {
		// Disk-only snapshot: cold-boot a fresh sandbox from the frozen disk
		// (new identity/config, no RAM restore) — the same path as `create
		// --image` on a captured node.
		return e.create(ctx, spec, createOpts{})
	}
	// Memory snapshot: cold-restore RAM/device/vCPU, reusing the in-guest token +
	// the captured config drive so the restored devices match.
	if err := validateBundle(snapDir); err != nil {
		return engine.SandboxInfo{}, err
	}
	// Reproduce the captured device set. Block volumes restore cleanly: clone each
	// frozen volume into the new sandbox (independent copy — fork/restore diverges
	// from the source). Memory snapshots of mounted sandboxes are refused at
	// checkpoint (virtio-fs can't be restored), so m.Mounts is empty here.
	var restoreVols []restoreVol
	for _, v := range m.Volumes {
		restoreVols = append(restoreVols, restoreVol{path: filepath.Join(snapDir, v.File), readOnly: v.ReadOnly})
	}
	return e.create(ctx, spec, createOpts{
		snapshotDir:    snapDir,
		forcedToken:    m.Token,
		configDrive:    filepath.Join(snapDir, configFile),
		restoreVolumes: restoreVols,
	})
}

// Fork creates a new sandbox that is an instant copy of a running sandbox,
// including its in-memory process state (the "clone" of `create --from`):
// memory-checkpoint the source to a throwaway bundle and immediately restore it
// into a new sandbox, then discard the bundle. The fork's disk + config are
// copies, so it diverges from the source; the source is undisturbed.
func (e *Engine) Fork(ctx context.Context, sandboxID, newName string) (engine.SandboxInfo, error) {
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
	vm.mu.Unlock()
	if hasMount {
		return engine.SandboxInfo{}, fmt.Errorf("cannot fork a sandbox with a virtio-fs --mount (the device cannot be memory-restored); use a filesystem snapshot (snapshot create --type filesystem) then create --snapshot")
	}

	tmp, err := os.MkdirTemp(e.cfg.DataDir, "fork-")
	if err != nil {
		return engine.SandboxInfo{}, fmt.Errorf("fork: temp dir: %w", err)
	}
	// Safe to remove once Resume returns: the new sandbox has its own root +
	// config copies and the helper has already loaded memory.img.
	defer os.RemoveAll(tmp)

	manifest, err := e.Checkpoint(ctx, sandboxID, "", 0, "snap", tmp)
	if err != nil {
		return engine.SandboxInfo{}, fmt.Errorf("fork: checkpoint: %w", err)
	}
	manifestJSON, _ := json.Marshal(manifest)
	return e.ResumeFromManifestJSON(ctx, filepath.Join(tmp, "snap"), manifestJSON, newName)
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
		_, _ = controlCmd(rctx, vm.CtlSockUDS, "RESUME")
	}()

	// 3. Flush host page cache so the on-disk overlay reflects all writes.
	_ = exec.CommandContext(pauseCtx, "sync").Run()

	// 4. Copy the overlay (reflink where the host FS supports it; else a copy —
	// the overlay is a small delta).
	if err := cloneFile(vm.baseSpec.RootDisk, dst); err != nil {
		return fmt.Errorf("freeze: copy overlay: %w", err)
	}
	return nil
}
