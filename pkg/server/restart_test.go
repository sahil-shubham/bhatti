package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

// TestShutdownLeavesSandboxesRunning: the daemon's shutdown stops no sandbox,
// so a restart reboots no guest. It used to stop — power off — every running
// one (SnapshotAll) before letting the engine go.
func TestShutdownLeavesSandboxesRunning(t *testing.T) {
	srv, _ := setup(t)
	eng := srv.engine.(*mockEngine)
	hot := createRunningBox(t, srv, eng, "hot-at-shutdown")
	warm := createRunningBox(t, srv, eng, "warm-at-shutdown")
	eng.mu.Lock()
	eng.thermal[warm] = "warm"
	eng.mu.Unlock()

	srv.Shutdown("terminated")

	eng.mu.Lock()
	stops, shutdowns := eng.Stops, eng.Shutdowns
	thermal := map[string]string{hot: eng.thermal[hot], warm: eng.thermal[warm]}
	eng.mu.Unlock()
	if stops != 0 {
		t.Fatalf("shutdown stopped %d sandbox(es)", stops)
	}
	if shutdowns != 1 {
		t.Fatalf("engine shut down %d times, want once", shutdowns)
	}
	if thermal[hot] != "hot" || thermal[warm] != "warm" {
		t.Fatalf("thermal after shutdown %v, want hot and warm as they were", thermal)
	}
	for _, eid := range []string{hot, warm} {
		if sb, err := srv.store.GetSandboxByID(t.Context(), eid); err != nil || sb.Status != "running" {
			t.Fatalf("store after shutdown: %+v (%v), want running", sb, err)
		}
	}
}

// TestRecoverSandboxesAfterRestart: at startup the store follows what the
// engine recovered. A sandbox whose VM died while no daemon ran is stopped
// rather than left "running"; one the engine adopted is running, whatever the
// store said; and the adopted ones' idle clocks start now — the warm→cold
// check skips a warm sandbox it has no activity time for, which would leave it
// warm for good.
func TestRecoverSandboxesAfterRestart(t *testing.T) {
	srv, _ := setup(t)
	eng := srv.engine.(*mockEngine)
	hot := createRunningBox(t, srv, eng, "adopted-hot")
	warm := createRunningBox(t, srv, eng, "adopted-warm")
	lost := createRunningBox(t, srv, eng, "lost")
	unrecorded := createRunningBox(t, srv, eng, "started-unrecorded")
	eng.mu.Lock()
	eng.thermal[warm] = "warm"
	eng.sandboxes[lost].Status = "stopped"
	eng.thermal[lost] = "cold"
	eng.mu.Unlock()
	if err := srv.store.StopSandbox(t.Context(), unrecorded); err != nil { // a start the store missed
		t.Fatal(err)
	}
	for _, eid := range []string{hot, warm, lost, unrecorded} {
		srv.lastActivity.Delete(eid) // a fresh daemon knows no activity
	}

	srv.RecoverSandboxes(context.Background())

	for eid, want := range map[string]string{hot: "running", warm: "running", lost: "stopped", unrecorded: "running"} {
		if sb, err := srv.store.GetSandboxByID(t.Context(), eid); err != nil || sb.Status != want {
			t.Errorf("%s: store status %+v (%v), want %s", eid, sb, err, want)
		}
	}
	for _, eid := range []string{hot, warm, unrecorded} {
		if _, ok := srv.lastActivity.Load(eid); !ok {
			t.Errorf("%s: no activity time after recovery", eid)
		}
	}

	time.Sleep(20 * time.Millisecond)
	srv.runThermalCycle(eng, ThermalConfig{WarmTimeout: time.Hour, ColdTimeout: 10 * time.Millisecond})
	if sb, err := srv.store.GetSandboxByID(t.Context(), warm); err != nil || sb.Status != "stopped" {
		t.Fatalf("adopted warm sandbox didn't go cold once idle: %+v (%v)", sb, err)
	}
}

// Sandboxes from before has_init existed read 0 even when they boot an --init
// app; recovery takes the engine's boot config as the truth, so the proxy
// waits for their app after an idle stop instead of answering 502.
func TestRecoverSandboxesBackfillsHasInit(t *testing.T) {
	srv, _ := setup(t)
	eng := srv.engine.(*mockEngine)
	legacy := createRunningBox(t, srv, eng, "legacy-init")
	plain := createRunningBox(t, srv, eng, "no-init")
	unknown := createRunningBox(t, srv, eng, "unknown")
	if err := srv.store.UpdateSandboxHasInit(t.Context(), unknown, true); err != nil {
		t.Fatal(err)
	}
	eng.mu.Lock()
	eng.Inits = map[string]bool{legacy: true, plain: false}
	eng.mu.Unlock()

	srv.RecoverSandboxes(context.Background())

	for id, want := range map[string]bool{legacy: true, plain: false, unknown: true} {
		sb, err := srv.store.GetSandboxByID(t.Context(), id)
		if err != nil || sb.HasInit != want {
			t.Errorf("%s: has_init %v (%v), want %v", id, sb.HasInit, err, want)
		}
	}
}

type ownerRecoveryEngine struct {
	*mockEngine
	ownerCalls       []struct{ id, userID string }
	statusOwnerCalls []int
	recoverErr       error
}

func (e *ownerRecoveryEngine) RecoverOwner(id, userID string) error {
	e.ownerCalls = append(e.ownerCalls, struct{ id, userID string }{id, userID})
	return e.recoverErr
}

func (e *ownerRecoveryEngine) Status(ctx context.Context, id string) (engine.SandboxInfo, error) {
	e.statusOwnerCalls = append(e.statusOwnerCalls, len(e.ownerCalls))
	return e.mockEngine.Status(ctx, id)
}

func TestRecoverSandboxesBackfillsOwnersBeforeStatus(t *testing.T) {
	for _, tc := range []struct {
		name, storedStatus, engineStatus string
		anonymous, missing               bool
	}{
		{name: "running", storedStatus: "running", engineStatus: "running"},
		{name: "lost", storedStatus: "running", engineStatus: "stopped"},
		{name: "stopped", storedStatus: "stopped", engineStatus: "stopped"},
		{name: "adopted", storedStatus: "stopped", engineStatus: "running"},
		{name: "pending", storedStatus: "restart_pending", engineStatus: "stopped"},
		{name: "creating", storedStatus: "creating", engineStatus: "stopped"},
		{name: "missing", storedStatus: "running", missing: true},
		{name: "destroyed", storedStatus: "destroyed", engineStatus: "running"},
		{name: "anonymous", storedStatus: "running", engineStatus: "running", anonymous: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := setup(t)
			eng := &ownerRecoveryEngine{mockEngine: srv.engine.(*mockEngine)}
			srv.engine = eng
			info, err := eng.Create(t.Context(), engine.SandboxSpec{Name: tc.name})
			if err != nil {
				t.Fatal(err)
			}
			eng.mu.Lock()
			eng.sandboxes[info.ID].Status = tc.engineStatus
			if tc.missing {
				delete(eng.sandboxes, info.ID)
			}
			eng.mu.Unlock()
			sb := store.Sandbox{
				ID: "store-" + tc.name, EngineID: info.ID, Name: tc.name,
				Status: tc.storedStatus, CreatedBy: "usr_test", CreatedAt: time.Now(),
			}
			if tc.anonymous {
				sb.CreatedBy = ""
			}
			if err := srv.store.CreateSandbox(t.Context(), sb); err != nil {
				t.Fatal(err)
			}

			srv.RecoverSandboxes(t.Context())

			wantCalls := 1
			if tc.storedStatus == "destroyed" || tc.anonymous {
				wantCalls = 0
			}
			if len(eng.ownerCalls) != wantCalls {
				t.Fatalf("owner recovery calls = %+v, want %d", eng.ownerCalls, wantCalls)
			}
			if wantCalls == 1 && (eng.ownerCalls[0].id != sb.EngineID || eng.ownerCalls[0].userID != sb.CreatedBy) {
				t.Errorf("owner recovery call = %+v, want engine ID %q and owner %q", eng.ownerCalls[0], sb.EngineID, sb.CreatedBy)
			}
			if tc.storedStatus == "destroyed" {
				if len(eng.statusOwnerCalls) != 0 {
					t.Fatalf("destroyed sandbox reached status lookup")
				}
			} else if len(eng.statusOwnerCalls) != 1 || eng.statusOwnerCalls[0] != wantCalls {
				t.Errorf("owner call counts at status lookup = %v, want [%d]", eng.statusOwnerCalls, wantCalls)
			}
		})
	}
}

func TestRecoverSandboxesOwnerFailureLogsAndContinues(t *testing.T) {
	srv, _ := setup(t)
	eng := &ownerRecoveryEngine{
		mockEngine: srv.engine.(*mockEngine),
		recoverErr: errors.New("owner conflict"),
	}
	srv.engine = eng
	adopted := createRunningBox(t, srv, eng.mockEngine, "owner-failure-adopted")
	lost := createRunningBox(t, srv, eng.mockEngine, "owner-failure-lost")
	if err := srv.store.StopSandbox(t.Context(), adopted); err != nil {
		t.Fatal(err)
	}
	eng.mu.Lock()
	eng.Inits = map[string]bool{adopted: true}
	eng.sandboxes[lost].Status = "stopped"
	eng.mu.Unlock()
	logs := captureLogs(t)

	srv.RecoverSandboxes(t.Context())

	if len(eng.ownerCalls) != 2 {
		t.Fatalf("owner recovery calls = %+v, want both sandboxes despite failures", eng.ownerCalls)
	}
	for id, want := range map[string]string{adopted: "running", lost: "stopped"} {
		sb, err := srv.store.GetSandboxByID(t.Context(), id)
		if err != nil || sb.Status != want {
			t.Fatalf("%s: recovered sandbox = %+v (%v), want status %s", id, sb, err, want)
		}
		if id == adopted && !sb.HasInit {
			t.Error("owner recovery failure prevented has_init backfill")
		}
	}
	records := logs.records("sandbox.owner_recovery_failed")
	if len(records) != 2 {
		t.Fatalf("owner recovery warnings = %+v, want two", records)
	}
	seen := make(map[string]bool)
	for _, rec := range records {
		id, _ := rec["sandbox_id"].(string)
		if (id != adopted && id != lost) || seen[id] || rec["engine_id"] != id || rec["level"] != "WARN" || rec["error"] != "owner conflict" {
			t.Errorf("unexpected owner recovery warning: %+v", rec)
		}
		seen[id] = true
	}
}
