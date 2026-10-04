package server

import (
	"context"
	"testing"
	"time"
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
		if sb, err := srv.store.GetSandboxByID(eid); err != nil || sb.Status != "running" {
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
	if err := srv.store.StopSandbox(unrecorded); err != nil { // a start the store missed
		t.Fatal(err)
	}
	for _, eid := range []string{hot, warm, lost, unrecorded} {
		srv.lastActivity.Delete(eid) // a fresh daemon knows no activity
	}

	srv.RecoverSandboxes(context.Background())

	for eid, want := range map[string]string{hot: "running", warm: "running", lost: "stopped", unrecorded: "running"} {
		if sb, err := srv.store.GetSandboxByID(eid); err != nil || sb.Status != want {
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
	if sb, err := srv.store.GetSandboxByID(warm); err != nil || sb.Status != "stopped" {
		t.Fatalf("adopted warm sandbox didn't go cold once idle: %+v (%v)", sb, err)
	}
}
