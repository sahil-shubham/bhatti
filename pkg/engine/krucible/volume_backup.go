package krucible

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent"
	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
	"github.com/sahil-shubham/bhatti/pkg/engine"
)

const volumeBackupGuestTimeout = 10 * time.Second

type volumeBackupGuest interface {
	Exec(context.Context, []string, map[string]string, string) (engine.ExecResult, error)
	Freeze(context.Context, string) error
	Thaw(context.Context, string) error
}

// BeginVolumeBackup holds the VM's lifecycle lock until its thaw function is
// called. Pause and Stop take the same lock, so neither can interrupt a frozen
// filesystem while the host clones the volume.
func (e *Engine) BeginVolumeBackup(ctx context.Context, id, mount string) (func(context.Context) error, string, error) {
	vm, err := e.getVM(id)
	if err != nil {
		return nil, "", err
	}
	for range 2 {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		vm.launchMu.Lock()
		vm.mu.Lock()
		status, thermal := vm.Status, vm.Thermal
		ag, info, infoErr := vm.Agent, vm.AgentInfo, vm.AgentInfoErr
		vm.mu.Unlock()
		if status != "running" {
			vm.launchMu.Unlock()
			return nil, "", fmt.Errorf("sandbox %q stopped before live volume backup", id)
		}
		if thermal != "hot" {
			vm.launchMu.Unlock()
			if err := e.Resume(ctx, id); err != nil {
				return nil, "", fmt.Errorf("resume sandbox for volume backup: %w", err)
			}
			continue // A pause raced the server's wake; resume without starting a stopped VM.
		}
		if ag == nil {
			vm.launchMu.Unlock()
			return nil, "", fmt.Errorf("sandbox %q has no guest agent", id)
		}
		thaw, mode, err := beginGuestVolumeBackup(ctx, ag, info, infoErr, mount)
		if err != nil {
			var uncertain *unconfirmedVolumeThaw
			if errors.As(err, &uncertain) {
				vm.powerOffLocked()
				err = &UncleanStopError{Reason: fmt.Errorf("volume backup freeze cleanup could not confirm thaw: %w", err)}
			}
			vm.launchMu.Unlock()
			return nil, mode, err
		}
		if thaw == nil && mode != "sync_only" {
			vm.launchMu.Unlock()
			return nil, mode, fmt.Errorf("sandbox %q provided no thaw for frozen volume", id)
		}
		var once sync.Once
		var thawErr error
		return func(cleanupCtx context.Context) error {
			once.Do(func() { thawErr = vm.finishVolumeBackup(thaw, cleanupCtx) })
			return thawErr
		}, mode, nil
	}
	return nil, "", fmt.Errorf("sandbox %q did not stay hot for volume backup", id)
}

// finishVolumeBackup owns launchMu. A failed THAW ACK means the filesystem's
// state is unknown, even if closing the agent connection may thaw it later.
// Power off before allowing any other lifecycle transition.
func (vm *VM) finishVolumeBackup(thaw func(context.Context) error, cleanupCtx context.Context) error {
	defer vm.launchMu.Unlock()
	if thaw == nil { // sync-only still needs the lifecycle lease until after clone
		return nil
	}
	if err := thaw(cleanupCtx); err != nil {
		vm.powerOffLocked()
		return &UncleanStopError{Reason: fmt.Errorf("volume backup thaw unconfirmed: %w", err)}
	}
	return nil
}

// BeginStoppedVolumeBackup keeps an attached but powered-off VM from starting
// against the volume image while the host clones it. The caller releases the
// lifecycle lock after the clone, before compression or upload.
func (e *Engine) BeginStoppedVolumeBackup(ctx context.Context, id string) (func(), error) {
	vm, err := e.getVM(id)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	vm.launchMu.Lock()
	if err := ctx.Err(); err != nil {
		vm.launchMu.Unlock()
		return nil, err
	}
	vm.mu.Lock()
	status := vm.Status
	vm.mu.Unlock()
	if status != "stopped" {
		vm.launchMu.Unlock()
		return nil, fmt.Errorf("sandbox %q became %s before stopped-volume clone", id, status)
	}
	var once sync.Once
	return func() { once.Do(vm.launchMu.Unlock) }, nil
}

// A failed freeze may have succeeded in the guest before its ACK was lost.
// If even the independent cleanup thaw is unconfirmed, stop the VM before
// releasing its lifecycle lock instead of leaving a running frozen guest.
type unconfirmedVolumeThaw struct{ err error }

func (e *unconfirmedVolumeThaw) Error() string { return e.err.Error() }
func (e *unconfirmedVolumeThaw) Unwrap() error { return e.err }

func beginGuestVolumeBackup(ctx context.Context, ag volumeBackupGuest, info proto.AgentInfo, infoErr error, mount string) (func(context.Context) error, string, error) {
	if infoErr != nil {
		return nil, "", fmt.Errorf("guest agent capabilities unavailable: %w", infoErr)
	}
	if !info.Legacy && info.Version == "" {
		return nil, "", fmt.Errorf("guest agent capabilities unavailable: no response cached")
	}
	// Sync all guest filesystems before freezing this mount. Old lohar cannot
	// freeze, but still provides a completed, bounded guest-wide sync.
	syncCtx, syncCancel := context.WithTimeout(ctx, volumeBackupGuestTimeout)
	result, err := ag.Exec(syncCtx, []string{"sync"}, nil, "")
	syncCancel()
	if err != nil {
		return nil, "", fmt.Errorf("sync guest before volume backup: %w", err)
	}
	if result.ExitCode != 0 {
		return nil, "", fmt.Errorf("sync guest before volume backup: exit %d: %s", result.ExitCode, result.Stderr)
	}
	if !info.Has(proto.FeatureFSFreeze) {
		return nil, "sync_only", nil
	}

	freezeCtx, cancel := context.WithTimeout(ctx, volumeBackupGuestTimeout)
	defer cancel()
	if err := ag.Freeze(freezeCtx, mount); err != nil {
		if errors.Is(err, agent.ErrFSFreezeRejected) || errors.Is(err, engine.ErrGuestAgentOutdated) {
			return nil, "", fmt.Errorf("freeze guest volume %q: %w", mount, err)
		}
		// Losing an ACK does not mean FIFREEZE failed: the filesystem may be
		// frozen. Try thaw even on an expired/cancelled freeze context.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), volumeBackupGuestTimeout)
		defer cleanupCancel()
		if thawErr := ag.Thaw(cleanupCtx, mount); thawErr != nil {
			return nil, "", errors.Join(
				fmt.Errorf("freeze guest volume %q: %w", mount, err),
				&unconfirmedVolumeThaw{err: fmt.Errorf("cleanup thaw guest volume %q: %w", mount, thawErr)},
			)
		}
		return nil, "", fmt.Errorf("freeze guest volume %q: %w", mount, err)
	}
	return func(cleanupCtx context.Context) error {
		if err := ag.Thaw(cleanupCtx, mount); err != nil {
			return fmt.Errorf("thaw guest volume %q: %w", mount, err)
		}
		return nil
	}, "frozen", nil
}
