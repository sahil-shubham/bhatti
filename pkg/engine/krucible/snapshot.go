package krucible

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"time"
)

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
