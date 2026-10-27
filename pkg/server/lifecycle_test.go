package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pkg "github.com/sahil-shubham/bhatti/pkg"
	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

func nextLifecycleEvent(t *testing.T, sub *Subscription, kind string) store.Event {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case e, ok := <-sub.C:
			if !ok {
				t.Fatal("event subscription closed before", kind)
			}
			if e.Type == kind {
				return e
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for %s", kind)
		}
	}
}

func TestVMExitStopsSandboxAndColdExecRestarts(t *testing.T) {
	srv, ts := setup(t)
	eng := srv.engine.(*mockEngine)
	sb := createSandbox(t, ts, uniqueName(t, "crashed"))
	srv.StartEventRecorder()
	srv.StartLifecycleEvents()
	sub := srv.events.Subscribe(SubscriptionFilter{SandboxID: sb.ID})
	defer sub.Cancel()

	srv.lastActivity.Store(sb.EngineID, time.Now())
	srv.snapshotFailures.Store(sb.EngineID, &eng.nextID) // non-empty failure bookkeeping
	srv.thermalFails.Store(sb.EngineID, &eng.nextID)
	eng.emitLifecycle(engine.LifecycleEvent{
		Kind: engine.VMExited, EngineID: sb.EngineID,
		ExitCode: -1, Signal: "killed", Reason: "VMM was OOM-killed",
	})
	exit := nextLifecycleEvent(t, sub, "sandbox.exited")
	if exit.SandboxID != sb.ID || exit.UserID != sb.CreatedBy || exit.Meta["exit_code"] != -1 ||
		exit.Meta["signal"] != "killed" || exit.Meta["reason"] != "VMM was OOM-killed" {
		t.Fatalf("incorrect exit audit event: %+v", exit)
	}
	stopped, err := srv.store.GetSandboxByID(sb.ID)
	if err != nil || stopped.Status != "stopped" || stopped.StoppedAt == nil {
		t.Fatalf("unexpected persisted status after exit: %+v (%v)", stopped, err)
	}
	for name, cache := range map[string]*sync.Map{"activity": &srv.lastActivity, "snapshot failures": &srv.snapshotFailures, "thermal failures": &srv.thermalFails} {
		if _, ok := cache.Load(sb.EngineID); ok {
			t.Errorf("%s not cleared after exit", name)
		}
	}
	eng.mu.Lock()
	starts := eng.StartCalls
	eng.mu.Unlock()
	if starts != 0 {
		t.Fatalf("non-keep_hot VM restarted without a request: %d starts", starts)
	}

	resp := doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/exec", map[string]any{"cmd": []string{"true"}})
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("exec after exit returned HTTP %d", resp.StatusCode)
	}
	eng.mu.Lock()
	starts = eng.StartCalls
	eng.mu.Unlock()
	if starts != 1 {
		t.Fatalf("exec must cold-start stopped VM once, got %d starts", starts)
	}
	running, err := srv.store.GetSandboxByID(sb.ID)
	if err != nil || running.Status != "running" {
		t.Fatalf("wake did not update store: %+v (%v)", running, err)
	}
	srv.Close()
	events, err := srv.store.QueryEvents(store.EventFilter{Type: "sandbox.exited", SandboxID: sb.ID})
	if err != nil || len(events) != 1 || events[0].Meta["reason"] != "VMM was OOM-killed" {
		t.Fatalf("exit event not persisted on shutdown: %+v (%v)", events, err)
	}
}

func TestVMExitColdStartsViaPublicProxy(t *testing.T) {
	srv, eng, ts := setupPublicProxy(t)
	sb, id := publishSandbox(t, srv, eng, "proxy-exit", "proxy-exit", 8080)
	srv.StartEventRecorder()
	srv.StartLifecycleEvents()
	sub := srv.events.Subscribe(SubscriptionFilter{SandboxID: sb.ID})
	defer sub.Cancel()
	eng.emitLifecycle(engine.LifecycleEvent{Kind: engine.VMExited, EngineID: id, ExitCode: -1, Reason: "helper died"})
	nextLifecycleEvent(t, sub, "sandbox.exited")
	hitPublicProxy(t, ts, "proxy-exit")
	eng.mu.Lock()
	starts := eng.StartCalls
	eng.mu.Unlock()
	got, err := srv.store.GetSandboxByID(sb.ID)
	if err != nil || got.Status != "running" || starts != 1 {
		t.Fatalf("proxy wake after VM exit: store=%+v, starts=%d, err=%v", got, starts, err)
	}
}

func TestKeepHotExitRestartAndBackoff(t *testing.T) {
	for _, tc := range []struct {
		name       string
		startError error
		wantEvent  string
		wantStatus string
		wantStarts int
		wantDelays []time.Duration
	}{
		{"restarted", nil, "sandbox.restarted", "running", 1, []time.Duration{0}},
		{"three failed starts", errors.New("boot failed"), "sandbox.restart_failed", "stopped", 3, []time.Duration{0, 30 * time.Second, 2 * time.Minute}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := setup(t)
			eng := srv.engine.(*mockEngine)
			id := createRunningBox(t, srv, eng, uniqueName(t, "keep-hot"))
			sb, err := srv.store.GetSandboxByEngineID(id)
			if err != nil {
				t.Fatal(err)
			}
			if err := srv.store.UpdateSandboxKeepHot(sb.ID, true); err != nil {
				t.Fatal(err)
			}
			eng.StartErr = tc.startError
			var mu sync.Mutex
			var delays []time.Duration
			srv.lifecycle.now = func() time.Time { return time.Unix(42, 0) }
			srv.lifecycle.wait = func(ctx context.Context, delay time.Duration) bool {
				mu.Lock()
				delays = append(delays, delay)
				mu.Unlock()
				return ctx.Err() == nil
			}
			srv.StartEventRecorder()
			srv.StartLifecycleEvents()
			sub := srv.events.Subscribe(SubscriptionFilter{SandboxID: sb.ID})
			defer sub.Cancel()

			eng.emitLifecycle(engine.LifecycleEvent{Kind: engine.VMExited, EngineID: id, ExitCode: 137, Reason: "helper exited"})
			result := nextLifecycleEvent(t, sub, tc.wantEvent)
			if result.UserID != sb.CreatedBy || result.SandboxID != sb.ID {
				t.Fatalf("wrong restart event owner: %+v", result)
			}
			got, err := srv.store.GetSandboxByID(sb.ID)
			if err != nil || got.Status != tc.wantStatus {
				t.Fatalf("restart status: %+v (%v), want %q", got, err, tc.wantStatus)
			}
			eng.mu.Lock()
			starts := eng.StartCalls
			eng.mu.Unlock()
			mu.Lock()
			observed := append([]time.Duration(nil), delays...)
			mu.Unlock()
			if starts != tc.wantStarts || !reflect.DeepEqual(observed, tc.wantDelays) {
				t.Fatalf("backoff: %d starts, delays %v; want %d, %v", starts, observed, tc.wantStarts, tc.wantDelays)
			}
		})
	}
}

func TestKeepHotExitDuringStartIsNotLost(t *testing.T) {
	srv, _ := setup(t)
	eng := srv.engine.(*mockEngine)
	id := createRunningBox(t, srv, eng, "exit-during-start")
	sb, err := srv.store.GetSandboxByEngineID(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.UpdateSandboxKeepHot(sb.ID, true); err != nil {
		t.Fatal(err)
	}
	srv.lifecycle.now = func() time.Time { return time.Unix(42, 0) }
	srv.lifecycle.wait = func(ctx context.Context, _ time.Duration) bool { return ctx.Err() == nil }
	srv.StartEventRecorder()
	srv.StartLifecycleEvents()
	exits := srv.events.Subscribe(SubscriptionFilter{SandboxID: sb.ID, TypePrefix: "sandbox.exited"})
	defer exits.Cancel()
	restarts := srv.events.Subscribe(SubscriptionFilter{SandboxID: sb.ID, TypePrefix: "sandbox.restarted"})
	defer restarts.Cancel()
	secondExitHandled := make(chan struct{})
	go func() {
		for range 2 {
			if _, ok := <-exits.C; !ok {
				return
			}
		}
		close(secondExitHandled)
	}()
	var once atomic.Bool
	eng.StartHook = func(engineID string) {
		if !once.CompareAndSwap(false, true) {
			return
		}
		eng.emitLifecycle(engine.LifecycleEvent{Kind: engine.VMExited, EngineID: engineID, ExitCode: -1})
		select {
		case <-secondExitHandled:
		case <-time.After(3 * time.Second):
		}
	}
	eng.emitLifecycle(engine.LifecycleEvent{Kind: engine.VMExited, EngineID: id, ExitCode: -1})
	nextLifecycleEvent(t, restarts, "sandbox.restarted")
	eng.mu.Lock()
	starts := eng.StartCalls
	eng.mu.Unlock()
	got, err := srv.store.GetSandboxByID(sb.ID)
	if err != nil || got.Status != "running" || starts != 2 {
		t.Fatalf("second exit must trigger another restart: store=%+v, starts=%d, err=%v", got, starts, err)
	}
}

func TestKeepHotCrashLoopStopsAfterThreeInTenMinutes(t *testing.T) {
	srv, _ := setup(t)
	eng := srv.engine.(*mockEngine)
	id := createRunningBox(t, srv, eng, "crash-loop")
	sb, err := srv.store.GetSandboxByEngineID(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.UpdateSandboxKeepHot(sb.ID, true); err != nil {
		t.Fatal(err)
	}
	var now atomic.Int64
	now.Store(42)
	srv.lifecycle.now = func() time.Time { return time.Unix(now.Load(), 0) }
	delays := make(chan time.Duration, 4)
	srv.lifecycle.wait = func(ctx context.Context, delay time.Duration) bool {
		delays <- delay
		return ctx.Err() == nil
	}
	srv.StartEventRecorder()
	srv.StartLifecycleEvents()
	sub := srv.events.Subscribe(SubscriptionFilter{SandboxID: sb.ID})
	defer sub.Cancel()
	for range 3 {
		eng.emitLifecycle(engine.LifecycleEvent{Kind: engine.VMExited, EngineID: id, ExitCode: -1, Reason: "repeated crash"})
		nextLifecycleEvent(t, sub, "sandbox.restarted")
	}
	eng.emitLifecycle(engine.LifecycleEvent{Kind: engine.VMExited, EngineID: id, ExitCode: -1, Reason: "fourth crash"})
	nextLifecycleEvent(t, sub, "sandbox.restart_failed")
	eng.mu.Lock()
	starts := eng.StartCalls
	eng.mu.Unlock()
	if starts != 3 {
		t.Fatalf("expected only three starts, got %d", starts)
	}
	for i, want := range restartBackoff {
		if got := <-delays; got != want {
			t.Errorf("restart %d delay %v, want %v", i, got, want)
		}
	}
	// Give-up is windowed, not permanent: a later owner-initiated start can
	// crash again and receive a fresh immediate automatic restart.
	now.Store(42 + int64((11 * time.Minute).Seconds()))
	if err := eng.Start(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.UpdateSandboxStatus(sb.ID, "running"); err != nil {
		t.Fatal(err)
	}
	eng.emitLifecycle(engine.LifecycleEvent{Kind: engine.VMExited, EngineID: id, ExitCode: -1, Reason: "later crash"})
	nextLifecycleEvent(t, sub, "sandbox.restarted")
	if delay := <-delays; delay != 0 {
		t.Fatalf("restart after window delay = %v, want immediate", delay)
	}
}

func TestKeepHotStartupRecoveryIsJoinedByClose(t *testing.T) {
	srv, _ := setup(t)
	eng := srv.engine.(*mockEngine)
	id := createRunningBox(t, srv, eng, "startup-keep-hot")
	sb, err := srv.store.GetSandboxByEngineID(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.UpdateSandboxKeepHot(sb.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := eng.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	// The VM went down with the daemon: the store still says running.
	srv.RecoverSandboxes(context.Background())
	if pending, err := srv.store.ListRestartPending(); err != nil || len(pending) != 1 || pending[0].ID != sb.ID {
		t.Fatalf("restart-pending set = %+v (%v), want just %s", pending, err, sb.ID)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	eng.StartHook = func(string) {
		close(started)
		<-release
	}
	srv.StartKeepHotRecovery()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("startup did not cold-boot keep_hot sandbox")
	}
	closed := make(chan struct{})
	go func() {
		srv.Close()
		close(closed)
	}()
	select {
	case <-srv.ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not begin cancellation")
	}
	select {
	case <-closed:
		t.Fatal("Close returned while startup auto-wake was still running")
	default:
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not join startup auto-wake")
	}
	eng.mu.Lock()
	starts := eng.StartCalls
	eng.mu.Unlock()
	if starts != 1 {
		t.Fatalf("startup auto-wake started sandbox %d times, want once", starts)
	}
}

// runStartupRecovery runs the startup sequence — recover, then wake — against
// whatever state the engine and store were left in, joins it with Close, and
// returns the sandboxes it was due to wake and what it logged.
func runStartupRecovery(t *testing.T, srv *Server) (due []store.Sandbox, logs string) {
	t.Helper()
	w := &syncLogWriter{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(w, nil)))
	defer slog.SetDefault(old)
	srv.RecoverSandboxes(context.Background())
	due, err := srv.store.ListRestartPending()
	if err != nil {
		t.Fatal(err)
	}
	srv.StartKeepHotRecovery()
	// Close cancels the wake: let it finish its boots before joining.
	deadline := time.Now().Add(3 * time.Second)
	for strings.Count(w.String(), "msg=sandbox.restarted") < len(due) {
		if time.Now().After(deadline) {
			t.Fatalf("%d keep_hot sandbox(es) due, not all woken: %s", len(due), w.String())
		}
		time.Sleep(time.Millisecond)
	}
	srv.Close()
	return due, w.String()
}

// A restart boots the keep_hot guests that went down with the daemon or host,
// and only those. One its owner stopped stays stopped (a stopped 8 GB golden
// VM was booted on every restart), and one the engine adopted still running
// is not announced as a fresh boot.
func TestKeepHotStartupWakesOnlyWhatWentDown(t *testing.T) {
	for _, tc := range []struct {
		name       string
		keepHot    bool
		storedDown bool // the store says stopped: stopped on purpose
		vmGone     bool // the engine has no running VM
		wantStarts int
		wantStatus string
	}{
		{"stopped by owner", true, true, true, 0, "stopped"},
		{"went down with the daemon", true, false, true, 1, "running"},
		{"adopted running", true, false, false, 0, "running"},
		{"went down but not keep_hot", false, false, true, 0, "stopped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := setup(t)
			eng := srv.engine.(*mockEngine)
			id := createRunningBox(t, srv, eng, "golden")
			sb, err := srv.store.GetSandboxByEngineID(id)
			if err != nil {
				t.Fatal(err)
			}
			if err := srv.store.UpdateSandboxKeepHot(sb.ID, tc.keepHot); err != nil {
				t.Fatal(err)
			}
			if tc.vmGone {
				eng.mu.Lock()
				eng.sandboxes[id].Status = "stopped"
				eng.thermal[id] = "cold"
				eng.mu.Unlock()
			}
			if tc.storedDown {
				if err := srv.store.StopSandbox(sb.ID); err != nil {
					t.Fatal(err)
				}
			}

			_, logs := runStartupRecovery(t, srv)

			eng.mu.Lock()
			starts := eng.StartCalls
			eng.mu.Unlock()
			if starts != tc.wantStarts {
				t.Errorf("engine started %d times, want %d", starts, tc.wantStarts)
			}
			if got, _ := srv.store.GetSandboxByID(sb.ID); got == nil || got.Status != tc.wantStatus {
				t.Errorf("store status %+v, want %s", got, tc.wantStatus)
			}
			if n := strings.Count(logs, "msg=sandbox.restarted"); n != tc.wantStarts {
				t.Errorf("logged %d boots, want %d:\n%s", n, tc.wantStarts, logs)
			}
		})
	}
}

// An explicit stop of a keep_hot sandbox survives a daemon restart.
func TestExplicitStopOfKeepHotSurvivesRestart(t *testing.T) {
	srv, ts := setup(t)
	eng := srv.engine.(*mockEngine)
	sb := createSandbox(t, ts, uniqueName(t, "golden"))
	if err := srv.store.UpdateSandboxKeepHot(sb.ID, true); err != nil {
		t.Fatal(err)
	}
	resp := doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/stop", nil)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("stop: %d", resp.StatusCode)
	}
	eng.mu.Lock()
	eng.StartCalls = 0
	eng.mu.Unlock()

	lost, logs := runStartupRecovery(t, srv)

	eng.mu.Lock()
	starts := eng.StartCalls
	eng.mu.Unlock()
	got, err := srv.store.GetSandboxByID(sb.ID)
	if err != nil || got.Status != "stopped" || starts != 0 || len(lost) != 0 {
		t.Fatalf("restart after explicit stop: status=%v (%v) starts=%d lost=%d logs=%s", got, err, starts, len(lost), logs)
	}
}

// A crashed keep_hot sandbox is stored stopped while its restart backs off, so
// the owner's stop changes nothing in the store. It must still cancel the
// pending restart, or the VM the owner just stopped boots anyway.
func TestExplicitStopCancelsBackingOffKeepHotRestart(t *testing.T) {
	srv, ts := setup(t)
	eng := srv.engine.(*mockEngine)
	sb := createSandbox(t, ts, uniqueName(t, "crashy"))
	if err := srv.store.UpdateSandboxKeepHot(sb.ID, true); err != nil {
		t.Fatal(err)
	}
	waiting := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv.lifecycle.wait = func(ctx context.Context, _ time.Duration) bool {
		once.Do(func() { close(waiting) })
		select {
		case <-release:
			return true
		case <-ctx.Done():
			return false
		}
	}
	srv.StartEventRecorder()
	srv.StartLifecycleEvents()
	eng.mu.Lock()
	eng.StartCalls = 0
	eng.mu.Unlock()
	eng.emitLifecycle(engine.LifecycleEvent{Kind: engine.VMExited, EngineID: sb.EngineID, ExitCode: -1})
	select {
	case <-waiting:
	case <-time.After(3 * time.Second):
		t.Fatal("restart did not back off")
	}

	resp := doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/stop", nil)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("stop: %d", resp.StatusCode)
	}
	close(release)
	deadline := time.Now().Add(3 * time.Second)
	for srv.lifecycle.isRestarting(sb.EngineID) {
		if time.Now().After(deadline) {
			t.Fatal("restart worker never finished")
		}
		time.Sleep(time.Millisecond)
	}
	eng.mu.Lock()
	starts := eng.StartCalls
	eng.mu.Unlock()
	got, err := srv.store.GetSandboxByID(sb.ID)
	if err != nil || got.Status != "stopped" || starts != 0 {
		t.Fatalf("backing-off restart ran after explicit stop: status=%v (%v) starts=%d", got, err, starts)
	}
}

// restarted builds the daemon that comes up after srv exits: same store, same
// engine (its VMs outlive the daemon), none of the old in-process state.
func restarted(t *testing.T, srv *Server) *Server {
	t.Helper()
	srv2 := New(srv.engine, srv.store, t.TempDir())
	t.Cleanup(srv2.Close)
	return srv2
}

// crashedIntoBackoff crashes a keep_hot sandbox and leaves its restart waiting
// out the backoff, as when a daemon restart lands inside the 30s/2m delay.
func crashedIntoBackoff(t *testing.T, srv *Server, ts *httptest.Server) store.Sandbox {
	t.Helper()
	eng := srv.engine.(*mockEngine)
	sb := createSandbox(t, ts, uniqueName(t, "crashy"))
	if err := srv.store.UpdateSandboxKeepHot(sb.ID, true); err != nil {
		t.Fatal(err)
	}
	waiting := make(chan struct{})
	var once sync.Once
	srv.lifecycle.wait = func(ctx context.Context, _ time.Duration) bool {
		once.Do(func() { close(waiting) })
		<-ctx.Done()
		return false
	}
	srv.StartEventRecorder()
	srv.StartLifecycleEvents()
	eng.mu.Lock()
	eng.StartCalls = 0
	eng.mu.Unlock()
	eng.emitLifecycle(engine.LifecycleEvent{Kind: engine.VMExited, EngineID: sb.EngineID, ExitCode: -1})
	select {
	case <-waiting:
	case <-time.After(3 * time.Second):
		t.Fatal("restart did not back off")
	}
	if pending, err := srv.store.ListRestartPending(); err != nil || len(pending) != 1 || pending[0].ID != sb.ID {
		t.Fatalf("crashed keep_hot sandbox not marked for restart: %+v (%v)", pending, err)
	}
	return sb
}

// A daemon restart inside a crashed keep_hot sandbox's backoff must not lose
// the restart: the sandbox is stored stopped, and only the stored intent tells
// the next daemon it went down unasked.
func TestKeepHotCrashRestartSurvivesDaemonRestart(t *testing.T) {
	srv, ts := setup(t)
	eng := srv.engine.(*mockEngine)
	sb := crashedIntoBackoff(t, srv, ts)
	srv.Close()

	due, logs := runStartupRecovery(t, restarted(t, srv))

	eng.mu.Lock()
	starts := eng.StartCalls
	eng.mu.Unlock()
	got, err := srv.store.GetSandboxByID(sb.ID)
	if len(due) != 1 || starts != 1 || err != nil || got.Status != "running" {
		t.Fatalf("crashed sandbox not revived: due=%d starts=%d status=%v (%v) logs=%s", len(due), starts, got, err, logs)
	}
	if pending, _ := srv.store.ListRestartPending(); len(pending) != 0 {
		t.Fatalf("marker left after the boot: %+v", pending)
	}
}

// loseKeepHot leaves a keep_hot sandbox as a host reboot does: stored running,
// its VM gone.
func loseKeepHot(t *testing.T, srv *Server, eng *mockEngine, name string) string {
	t.Helper()
	id := createRunningBox(t, srv, eng, name)
	if err := srv.store.UpdateSandboxKeepHot(id, true); err != nil {
		t.Fatal(err)
	}
	eng.mu.Lock()
	eng.sandboxes[id].Status = "stopped"
	eng.thermal[id] = "cold"
	eng.mu.Unlock()
	return id
}

// failStartFor fails the boot of one sandbox, counting it as a start.
type failStartFor struct {
	*mockEngine
	id  string
	err error
}

func (e *failStartFor) Start(ctx context.Context, id string) error {
	if id != e.id {
		return e.mockEngine.Start(ctx, id)
	}
	e.mockEngine.mu.Lock()
	e.mockEngine.StartCalls++
	e.mockEngine.mu.Unlock()
	return e.err
}

// After a host reboot the first boot can fail for a moment (gateway not up
// yet, disk busy). Startup retries on the crash-restart schedule instead of
// giving the guest up after one attempt.
func TestKeepHotStartupBootRetriesAfterTransientFailure(t *testing.T) {
	srv, _ := setup(t)
	eng := srv.engine.(*mockEngine)
	id := loseKeepHot(t, srv, eng, "reboot-victim")
	eng.StartErr = errors.New("gateway not up yet")
	delays := make(chan time.Duration, 4)
	srv.lifecycle.wait = func(ctx context.Context, d time.Duration) bool {
		delays <- d
		if d > 0 { // the transient condition clears while it backs off
			eng.mu.Lock()
			eng.StartErr = nil
			eng.mu.Unlock()
		}
		return ctx.Err() == nil
	}

	due, logs := runStartupRecovery(t, srv)

	eng.mu.Lock()
	starts := eng.StartCalls
	eng.mu.Unlock()
	got, err := srv.store.GetSandboxByID(id)
	if len(due) != 1 || starts != 2 || err != nil || got.Status != "running" {
		t.Fatalf("boot did not recover from a transient failure: due=%d starts=%d status=%v (%v)\n%s", len(due), starts, got, err, logs)
	}
	if first, second := <-delays, <-delays; first != 0 || second != restartBackoff[1] {
		t.Fatalf("retry delays %v, %v; want 0, %v", first, second, restartBackoff[1])
	}
	if pending, _ := srv.store.ListRestartPending(); len(pending) != 0 {
		t.Fatalf("marker left after the boot: %+v", pending)
	}
}

// One guest boots, another fails and is backing off when the daemon exits
// again. The marker stays set while it backs off, so the next start retries
// that one and only that one.
func TestKeepHotStartupBackoffSurvivesDaemonRestart(t *testing.T) {
	srv, _ := setup(t)
	eng := srv.engine.(*mockEngine)
	good := loseKeepHot(t, srv, eng, "boots")
	bad := loseKeepHot(t, srv, eng, "disk-busy")
	srv.engine = &failStartFor{mockEngine: eng, id: bad, err: errors.New("disk busy")}
	backingOff := make(chan struct{})
	var once sync.Once
	srv.lifecycle.wait = func(ctx context.Context, d time.Duration) bool {
		if d == 0 {
			return ctx.Err() == nil
		}
		once.Do(func() { close(backingOff) })
		<-ctx.Done()
		return false
	}
	srv.RecoverSandboxes(context.Background())
	srv.StartKeepHotRecovery()
	select {
	case <-backingOff:
	case <-time.After(3 * time.Second):
		t.Fatal("failed boot did not back off")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if sb, err := srv.store.GetSandboxByID(good); err == nil && sb.Status == "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("healthy sandbox did not boot")
		}
		time.Sleep(time.Millisecond)
	}
	srv.Close()
	eng.mu.Lock()
	before := eng.StartCalls
	eng.mu.Unlock()

	due, logs := runStartupRecovery(t, New(eng, srv.store, t.TempDir())) // the disk is free now: the plain engine

	eng.mu.Lock()
	starts := eng.StartCalls - before
	eng.mu.Unlock()
	got, err := srv.store.GetSandboxByID(bad)
	if len(due) != 1 || due[0].ID != bad || starts != 1 || err != nil || got.Status != "running" {
		t.Fatalf("next start due=%+v starts=%d status=%v (%v), want only %s booted:\n%s", due, starts, got, err, bad, logs)
	}
}

// A keep_hot sandbox the engine powered off after a failed thaw was not
// stopped by its owner: it is marked to restart.
func TestBackupUncleanStopMarksKeepHotForRestart(t *testing.T) {
	srv, _ := setup(t)
	eng := srv.engine.(*mockEngine)
	hot := createRunningBox(t, srv, eng, "backup-hot")
	plain := createRunningBox(t, srv, eng, "backup-plain")
	if err := srv.store.UpdateSandboxKeepHot(hot, true); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{hot, plain} {
		sb, err := srv.store.GetSandboxByID(id)
		if err != nil {
			t.Fatal(err)
		}
		srv.reconcileBackupUncleanStop(sb, &fakeBackupUncleanStop{reason: "guest thaw ACK lost"})
		if got, _ := srv.store.GetSandboxByID(id); got.Status != "stopped" {
			t.Fatalf("%s: status %s after unclean stop", id, got.Status)
		}
	}
	list, err := srv.store.ListRestartPending()
	if err != nil || len(list) != 1 || list[0].ID != hot {
		t.Fatalf("restart-pending after failed thaw = %+v (%v), want just %s", list, err, hot)
	}
}

// An owner's stop while the restart backs off is stored as a plain stop, so
// the next daemon leaves the sandbox down.
func TestExplicitStopDuringBackoffSurvivesDaemonRestart(t *testing.T) {
	srv, ts := setup(t)
	eng := srv.engine.(*mockEngine)
	sb := crashedIntoBackoff(t, srv, ts)
	resp := doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/stop", nil)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("stop: %d", resp.StatusCode)
	}
	srv.Close()

	due, logs := runStartupRecovery(t, restarted(t, srv))

	eng.mu.Lock()
	starts := eng.StartCalls
	eng.mu.Unlock()
	got, err := srv.store.GetSandboxByID(sb.ID)
	if len(due) != 0 || starts != 0 || err != nil || got.Status != "stopped" {
		t.Fatalf("stopped sandbox came back: due=%d starts=%d status=%v (%v) logs=%s", len(due), starts, got, err, logs)
	}
}

// `bhatti edit --allow-cold` (keep_hot=false) while a restart is pending
// drops it, even if keep_hot is turned on again without a wake.
func TestAllowColdDuringBackoffDropsPendingRestart(t *testing.T) {
	srv, ts := setup(t)
	eng := srv.engine.(*mockEngine)
	sb := crashedIntoBackoff(t, srv, ts)
	resp := doReq(t, ts, "PATCH", "/sandboxes/"+sb.ID, map[string]any{"keep_hot": false})
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("patch: %d", resp.StatusCode)
	}
	srv.Close()
	if err := srv.store.UpdateSandboxKeepHot(sb.ID, true); err != nil {
		t.Fatal(err)
	}

	due, logs := runStartupRecovery(t, restarted(t, srv))

	eng.mu.Lock()
	starts := eng.StartCalls
	eng.mu.Unlock()
	got, err := srv.store.GetSandboxByID(sb.ID)
	if len(due) != 0 || starts != 0 || err != nil || got.Status != "stopped" {
		t.Fatalf("--allow-cold sandbox came back: due=%d starts=%d status=%v (%v) logs=%s", len(due), starts, got, err, logs)
	}
}

// A restart that gives up must not leave the intent behind: a VM that cannot
// boot would otherwise be tried again on every daemon restart.
func TestRestartGiveUpClearsPendingRestart(t *testing.T) {
	srv, _ := setup(t)
	eng := srv.engine.(*mockEngine)
	id := createRunningBox(t, srv, eng, "broken")
	sb, err := srv.store.GetSandboxByEngineID(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.UpdateSandboxKeepHot(sb.ID, true); err != nil {
		t.Fatal(err)
	}
	eng.StartErr = errors.New("no memory")
	srv.lifecycle.wait = func(ctx context.Context, _ time.Duration) bool { return ctx.Err() == nil }
	srv.StartEventRecorder()
	srv.StartLifecycleEvents()
	sub := srv.events.Subscribe(SubscriptionFilter{SandboxID: sb.ID})
	defer sub.Cancel()
	eng.emitLifecycle(engine.LifecycleEvent{Kind: engine.VMExited, EngineID: id, ExitCode: -1})
	nextLifecycleEvent(t, sub, "sandbox.restart_failed")
	srv.Close()

	eng.StartErr = nil
	due, logs := runStartupRecovery(t, restarted(t, srv))
	eng.mu.Lock()
	starts := eng.StartCalls
	eng.mu.Unlock()
	if len(due) != 0 || starts != 3 {
		t.Fatalf("given-up sandbox retried at startup: due=%d starts=%d (want only the 3 in-process tries) logs=%s", len(due), starts, logs)
	}
}

func TestNetworkLostAuditUsesSandboxOwner(t *testing.T) {
	srv, _ := setup(t)
	eng := srv.engine.(*mockEngine)
	id := createRunningBox(t, srv, eng, "net-lost")
	sb, _ := srv.store.GetSandboxByEngineID(id)
	srv.StartEventRecorder()
	srv.StartLifecycleEvents()
	sub := srv.events.Subscribe(SubscriptionFilter{SandboxID: sb.ID})
	defer sub.Cancel()
	eng.emitLifecycle(engine.LifecycleEvent{
		Kind: engine.NetworkLost, EngineID: id, UserID: "untrusted", SandboxID: "untrusted", Reason: "bhatti-netd exited: signal: killed",
	})
	e := nextLifecycleEvent(t, sub, "sandbox.network_lost")
	if e.SandboxID != sb.ID || e.UserID != sb.CreatedBy || e.Meta["reason"] != "bhatti-netd exited: signal: killed" {
		t.Fatalf("network loss event = %+v", e)
	}
	srv.Close()
	events, err := srv.store.QueryEvents(store.EventFilter{Type: "sandbox.network_lost", SandboxID: sb.ID})
	if err != nil || len(events) != 1 || events[0].Meta["reason"] != e.Meta["reason"] {
		t.Fatalf("network event not persisted: %+v (%v)", events, err)
	}
}

func TestRecoveryEventsAreDeliveredWhenHandlerAttaches(t *testing.T) {
	srv, _ := setup(t)
	eng := srv.engine.(*mockEngine)
	id := createRunningBox(t, srv, eng, "queued-net-lost")
	sb, _ := srv.store.GetSandboxByEngineID(id)
	eng.emitLifecycle(engine.LifecycleEvent{Kind: engine.NetworkLost, EngineID: id, Reason: "gateway quarantine"})
	srv.StartEventRecorder()
	sub := srv.events.Subscribe(SubscriptionFilter{SandboxID: sb.ID})
	defer sub.Cancel()
	srv.StartLifecycleEvents()
	nextLifecycleEvent(t, sub, "sandbox.network_lost")
}

func TestCloseCancelsDelayedKeepHotRestart(t *testing.T) {
	srv, _ := setup(t)
	eng := srv.engine.(*mockEngine)
	id := createRunningBox(t, srv, eng, "cancel-restart")
	sb, err := srv.store.GetSandboxByEngineID(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.UpdateSandboxKeepHot(sb.ID, true); err != nil {
		t.Fatal(err)
	}
	waiting := make(chan struct{})
	srv.lifecycle.wait = func(ctx context.Context, _ time.Duration) bool {
		close(waiting)
		<-ctx.Done()
		return false
	}
	srv.StartEventRecorder()
	srv.StartLifecycleEvents()
	eng.emitLifecycle(engine.LifecycleEvent{Kind: engine.VMExited, EngineID: id, ExitCode: -1})
	select {
	case <-waiting:
	case <-time.After(3 * time.Second):
		t.Fatal("restart worker did not start")
	}
	done := make(chan struct{})
	go func() {
		srv.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not cancel the pending restart")
	}
	eng.mu.Lock()
	starts := eng.StartCalls
	eng.mu.Unlock()
	if starts != 0 {
		t.Fatalf("restart continued after Close: %d starts", starts)
	}
}

func TestCloseJoinsEveryServerLoopAndIsIdempotent(t *testing.T) {
	srv, _ := setup(t)
	srv.StartEventRecorder()
	srv.StartLifecycleEvents()
	srv.StartRetention()
	srv.StartMetricsSnapshots()
	if err := srv.StartThermalManager(ThermalConfig{}); err != nil {
		t.Fatal(err)
	}
	srv.backupBackend = &backupTestBackend{}
	srv.StartBackupScheduler([]pkg.BackupSchedule{{Cron: "* * * * *", Volume: "backup-volume"}})
	const loops = 6 // cleanup, lifecycle, retention, metrics, thermal, backup
	exiting := make(chan string, loops)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	srv.bgMu.Lock()
	srv.onWorkerExit = func(name string) {
		if srv.ctx.Err() == nil {
			t.Errorf("worker %q exited before cancellation", name)
		}
		exiting <- name
		<-release
	}
	srv.bgMu.Unlock()
	done := make(chan struct{})
	go func() {
		srv.Close()
		close(done)
	}()
	seen := make(map[string]bool)
	for range loops {
		select {
		case name := <-exiting:
			seen[name] = true
		case <-time.After(3 * time.Second):
			releaseOnce.Do(func() { close(release) })
			<-done
			t.Fatalf("only %v server loops stopped on cancellation", seen)
		}
	}
	select {
	case <-done:
		t.Fatal("Close returned while its loops were still in flight")
	default:
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not join the cancelled loops")
	}
	if len(seen) != loops {
		t.Fatalf("wanted %d named loops, got %v", loops, seen)
	}
	select {
	case <-srv.events.done:
	default:
		t.Fatal("Close returned before event recorder flushed and exited")
	}
	srv.Close()
	srv.RecordEvent(store.Event{Type: "after.close"})
	events, err := srv.store.QueryEvents(store.EventFilter{Type: "after.close"})
	if err != nil || len(events) != 0 {
		t.Fatalf("RecordEvent after Close persisted an event: %+v (%v)", events, err)
	}
}

func TestRecordEventCloseRace(t *testing.T) {
	root, _ := setup(t)
	for range 30 {
		srv := New(newMockEngine(), root.store, t.TempDir())
		srv.StartEventRecorder()
		var producers sync.WaitGroup
		start := make(chan struct{})
		recording := make(chan struct{}, 4)
		for range 4 {
			producers.Add(1)
			go func() {
				defer producers.Done()
				<-start
				srv.RecordEvent(store.Event{Type: "race.record_close"})
				recording <- struct{}{}
				for range 1000 {
					srv.RecordEvent(store.Event{Type: "race.record_close"})
				}
			}()
		}
		close(start)
		for range 4 {
			<-recording
		}
		srv.Close()
		producers.Wait()
		srv.RecordEvent(store.Event{Type: "race.after_close"})
		srv.Close()
	}
}

type syncLogWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncLogWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestGoSafeRecoversWorkerPanic(t *testing.T) {
	logs := &syncLogWriter{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	defer slog.SetDefault(old)
	started := make(chan struct{})
	goSafe("panic-regression", func() {
		defer close(started)
		panic("background job failed")
	})
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("panicking worker failed to execute")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		output := logs.String()
		if strings.Contains(output, "panic-regression") && strings.Contains(output, "stack=") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("recovered panic missing stack log: %q", output)
		}
		time.Sleep(time.Millisecond)
	}
	// Without the worker's recover, this panic terminates the test process.
	startedAgain := make(chan struct{})
	goSafe("still-alive", func() { close(startedAgain) })
	select {
	case <-startedAgain:
	case <-time.After(3 * time.Second):
		t.Fatal("daemon could not start another worker after a panic")
	}
}
