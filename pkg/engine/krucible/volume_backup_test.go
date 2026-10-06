package krucible

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent"
	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
	"github.com/sahil-shubham/bhatti/pkg/engine"
)

type backupGuestFake struct {
	calls        []string
	freezeErr    error
	freezeCancel context.CancelFunc
	thawErr      error
	execResult   engine.ExecResult
	execErr      error
	thawCtxValid bool
}

func (f *backupGuestFake) Exec(ctx context.Context, argv []string, _ map[string]string, _ string) (engine.ExecResult, error) {
	f.calls = append(f.calls, "sync")
	if len(argv) != 1 || argv[0] != "sync" {
		return engine.ExecResult{}, errors.New("unexpected guest command")
	}
	if _, ok := ctx.Deadline(); !ok {
		return engine.ExecResult{}, errors.New("unbounded guest sync")
	}
	return f.execResult, f.execErr
}

func (f *backupGuestFake) Freeze(ctx context.Context, mount string) error {
	f.calls = append(f.calls, "freeze:"+mount)
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("unbounded guest freeze")
	}
	if f.freezeCancel != nil {
		f.freezeCancel()
	}
	return f.freezeErr
}

func (f *backupGuestFake) Thaw(ctx context.Context, mount string) error {
	f.calls = append(f.calls, "thaw:"+mount)
	_, deadline := ctx.Deadline()
	f.thawCtxValid = deadline && ctx.Err() == nil
	return f.thawErr
}

func TestBeginGuestVolumeBackupFreezesAndThawsMount(t *testing.T) {
	guest := &backupGuestFake{}
	info := proto.AgentInfo{Version: "v2.5.1", Features: []proto.AgentFeature{proto.FeatureFSFreeze}}
	thaw, mode, err := beginGuestVolumeBackup(context.Background(), guest, info, nil, "/workspace")
	if err != nil || mode != "frozen" || thaw == nil {
		t.Fatalf("begin: mode=%q thaw=%v error=%v", mode, thaw != nil, err)
	}
	if len(guest.calls) != 2 || guest.calls[0] != "sync" || guest.calls[1] != "freeze:/workspace" {
		t.Fatalf("expected guest-wide sync then FIFREEZE before copy: %v", guest.calls)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := thaw(ctx); err != nil {
		t.Fatal(err)
	}
	if !guest.thawCtxValid || len(guest.calls) != 3 || guest.calls[2] != "thaw:/workspace" {
		t.Fatalf("guest was not thawed with bounded cleanup context: %v", guest.calls)
	}
}

func TestBeginGuestVolumeBackupFreezeErrorStillThawsAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	guest := &backupGuestFake{freezeErr: context.Canceled, freezeCancel: cancel}
	info := proto.AgentInfo{Version: "v2.5.1", Features: []proto.AgentFeature{proto.FeatureFSFreeze}}
	thaw, _, err := beginGuestVolumeBackup(ctx, guest, info, nil, "/data")
	if err == nil || thaw != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("ambiguous freeze must return cancellation: thaw=%v err=%v", thaw != nil, err)
	}
	if !guest.thawCtxValid || len(guest.calls) != 3 || guest.calls[2] != "thaw:/data" {
		t.Fatalf("ambiguous freeze did not independently thaw: %v", guest.calls)
	}

	guest = &backupGuestFake{freezeErr: context.DeadlineExceeded, thawErr: errors.New("thaw failed")}
	_, _, err = beginGuestVolumeBackup(context.Background(), guest, info, nil, "/data")
	if err == nil || !strings.Contains(err.Error(), "thaw failed") {
		t.Fatalf("lost thaw failure after ambiguous freeze: %v", err)
	}
}

func TestBeginGuestVolumeBackupRejectedFreezeDoesNotThawAnotherLease(t *testing.T) {
	info := proto.AgentInfo{Version: "v2.5.1", Features: []proto.AgentFeature{proto.FeatureFSFreeze}}
	for _, rejected := range []error{
		errors.Join(agent.ErrFSFreezeRejected, errors.New("guest filesystem already frozen")),
		engine.GuestAgentOutdated(string(proto.FeatureFSFreeze)),
	} {
		guest := &backupGuestFake{freezeErr: rejected}
		thaw, mode, err := beginGuestVolumeBackup(context.Background(), guest, info, nil, "/data")
		if thaw != nil || mode != "" || !errors.Is(err, rejected) {
			t.Fatalf("definitive rejection became a freeze lease: thaw=%v mode=%q err=%v", thaw != nil, mode, err)
		}
		if len(guest.calls) != 2 || guest.calls[0] != "sync" || guest.calls[1] != "freeze:/data" {
			t.Fatalf("rejected FREEZE incorrectly issued THAW: calls=%v", guest.calls)
		}
	}
}

func TestBeginGuestVolumeBackupSyncFallbackAndProbeErrors(t *testing.T) {
	guest := &backupGuestFake{}
	thaw, mode, err := beginGuestVolumeBackup(context.Background(), guest, proto.AgentInfo{Legacy: true}, nil, "/workspace")
	if err != nil || mode != "sync_only" || thaw != nil || len(guest.calls) != 1 || guest.calls[0] != "sync" {
		t.Fatalf("old agent must complete bounded sync: mode=%q thaw=%v calls=%v err=%v", mode, thaw != nil, guest.calls, err)
	}
	guest.calls = nil
	guest.execResult.ExitCode = 7
	_, _, err = beginGuestVolumeBackup(context.Background(), guest, proto.AgentInfo{Legacy: true}, nil, "/workspace")
	if err == nil || !strings.Contains(err.Error(), "exit 7") {
		t.Fatalf("nonzero sync exit must fail: %v", err)
	}
	guest.calls = nil
	_, _, err = beginGuestVolumeBackup(context.Background(), guest, proto.AgentInfo{}, errors.New("INFO failed"), "/workspace")
	if err == nil || !strings.Contains(err.Error(), "INFO failed") || len(guest.calls) != 0 {
		t.Fatalf("probe failure must not become fallback: calls=%v err=%v", guest.calls, err)
	}
	_, _, err = beginGuestVolumeBackup(context.Background(), guest, proto.AgentInfo{}, nil, "/workspace")
	if err == nil || len(guest.calls) != 0 {
		t.Fatalf("missing cached INFO must not become fallback: calls=%v err=%v", guest.calls, err)
	}
}

func TestStoppedVolumeBackupLeaseBlocksConcurrentStart(t *testing.T) {
	vm := &VM{ID: "stopped", Status: "stopped", Thermal: "cold"}
	e := &Engine{vms: map[string]*VM{vm.ID: vm}}
	release, err := e.BeginStoppedVolumeBackup(context.Background(), vm.ID)
	if err != nil || release == nil {
		t.Fatalf("acquire stopped backup lease: release=%v err=%v", release != nil, err)
	}
	defer release()

	if vm.launchMu.TryLock() {
		vm.launchMu.Unlock()
		t.Fatal("sandbox could start against an attached volume during clone")
	}
	release()
	if !vm.launchMu.TryLock() {
		t.Fatal("stopped backup lease was not released after clone")
	}
	vm.mu.Lock()
	vm.Status = "running"
	vm.mu.Unlock()
	vm.launchMu.Unlock()
	if _, err := e.BeginStoppedVolumeBackup(context.Background(), vm.ID); err == nil {
		t.Fatal("running sandbox received a stopped-volume backup lease")
	}
}

func TestLiveBackupDoesNotStartGuestStoppedAfterServerStatusCheck(t *testing.T) {
	vm := &VM{ID: "stopped", Status: "stopped", Thermal: "cold"}
	e := &Engine{vms: map[string]*VM{vm.ID: vm}}
	thaw, mode, err := e.BeginVolumeBackup(context.Background(), vm.ID, "/data")
	if err == nil || thaw != nil || mode != "" || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("live backup must refuse a concurrently stopped guest: thaw=%v mode=%q err=%v", thaw != nil, mode, err)
	}
	if vm.Status != "stopped" {
		t.Fatalf("live backup unexpectedly started stopped guest: %s", vm.Status)
	}
}

func TestVolumeBackupLifecycleLeaseAndUnconfirmedThawContainment(t *testing.T) {
	vm := &VM{ID: "vm", Status: "running", Thermal: "hot"}
	e := &Engine{}
	vm.launchMu.Lock()
	if err := e.finishVolumeBackup(vm, nil, context.Background()); err != nil {
		t.Fatalf("sync-only backup must release its lifecycle lease: %v", err)
	}
	if !vm.launchMu.TryLock() {
		t.Fatal("sync-only backup left the VM launch lock held")
	}
	vm.launchMu.Unlock()
	if vm.Status != "running" {
		t.Fatalf("successful sync-only backup stopped the VM: %s", vm.Status)
	}

	vm.launchMu.Lock()
	err := e.finishVolumeBackup(vm, func(ctx context.Context) error {
		if ctx.Err() != nil {
			t.Fatal("thaw ran under canceled context")
		}
		return errors.New("lost thaw ACK")
	}, context.Background())
	var unclean *UncleanStopError
	if !errors.As(err, &unclean) || !strings.Contains(err.Error(), "lost thaw ACK") {
		t.Fatalf("unconfirmed thaw did not surface typed unclean stop: %v", err)
	}
	if vm.Status != "stopped" || vm.Thermal != "cold" || vm.Agent != nil {
		t.Fatalf("unconfirmed thaw left a running/frozen guest: status=%s thermal=%s agent=%v", vm.Status, vm.Thermal, vm.Agent)
	}
	if !vm.launchMu.TryLock() {
		t.Fatal("thaw containment leaked lifecycle lock")
	}
	vm.launchMu.Unlock()
}
