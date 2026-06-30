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
	Vcpus       uint8  `json:"vcpus"`
	MemMiB      uint32 `json:"mem_mib"`
	DiskFile    string `json:"disk_file"`
	ConfigFile  string `json:"config_file"`
	Token       string `json:"token"`
	KernelImage string `json:"kernel_image,omitempty"`
}

// Checkpoint captures a named, sandbox-independent memory snapshot (the server's
// `checkpointer` capability): the running guest's RAM + device + vCPU state plus
// a copy of its disk and config drive, into snapDir/snapName/, WITHOUT stopping
// the sandbox. Restore into a new sandbox via ResumeFromManifestJSON.
func (e *Engine) Checkpoint(ctx context.Context, sandboxID, userID string, subnetIndex int, snapName, snapDir string) (any, error) {
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

	// RAM + device + vCPU state (libkrun writes manifest.json/checkpoint.bin/memory.img).
	if _, err := controlCmd(cctx, vm.CtlSockUDS, "SNAPSHOT "+finalDir); err != nil {
		return nil, fmt.Errorf("checkpoint: snapshot: %w", err)
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

	done = true
	slog.Info("krucible snapshot created", "id", sandboxID, "name", snapName, "dir", finalDir)
	return krucibleSnapManifest{
		ProtoVer: krucibleProtoVer, Arch: hostSnapshotArch(),
		Vcpus: spec.Vcpus, MemMiB: spec.MemMiB,
		DiskFile: "rootfs.qcow2", ConfigFile: "config.ext4",
		Token: token, KernelImage: spec.KernelImage,
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
	if err := validateBundle(snapDir); err != nil {
		return engine.SandboxInfo{}, err
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
	return e.create(ctx, spec, createOpts{
		snapshotDir: snapDir,
		forcedToken: m.Token,
		configDrive: filepath.Join(snapDir, configFile),
	})
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
