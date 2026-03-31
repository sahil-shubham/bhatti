package server

import (
	"fmt"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

// createRunningBox is a helper that creates a sandbox in the store and
// mock engine, returning the engine ID. The sandbox starts as "hot".
func createRunningBox(t *testing.T, srv *Server, eng *mockEngine, name string) string {
	t.Helper()
	// Create via engine
	info, err := eng.Create(nil, engine.SandboxSpec{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	// Register in store
	sb := store.Sandbox{
		ID: info.ID, Name: name, EngineID: info.EngineID,
		Status: "running", IP: info.IP, CreatedBy: "usr_test",
		CreatedAt: time.Now(),
	}
	if err := srv.store.CreateSandbox(sb); err != nil {
		t.Fatal(err)
	}
	eng.mu.Lock()
	eng.thermal[info.EngineID] = "hot"
	eng.mu.Unlock()
	// Seed lastActivity so the thermal cycle has a timestamp
	srv.lastActivity.Store(info.EngineID, time.Now())
	return info.EngineID
}

func TestThermalHotToWarm(t *testing.T) {
	srv, _ := setup(t)
	eng := srv.engine.(*mockEngine)
	cfg := ThermalConfig{WarmTimeout: 50 * time.Millisecond, ColdTimeout: time.Hour}

	eid := createRunningBox(t, srv, eng, "hot-box")

	// Set activity to long ago so agent query triggers
	srv.lastActivity.Store(eid, time.Now().Add(-time.Minute))

	// Agent reports idle
	eng.mu.Lock()
	eng.ActivityResult = &proto.ActivityInfo{
		LastActivityUnix: time.Now().Add(-time.Minute).Unix(),
		AttachedSessions: 0,
	}
	eng.mu.Unlock()

	// Run thermal cycle
	te := srv.engine.(ThermalEngine)
	srv.runThermalCycle(te, cfg)

	// Should be warm now
	eng.mu.Lock()
	state := eng.thermal[eid]
	eng.mu.Unlock()
	if state != "warm" {
		t.Fatalf("expected thermal=warm, got %q", state)
	}
}

func TestThermalHotStaysHotWithActivity(t *testing.T) {
	srv, _ := setup(t)
	eng := srv.engine.(*mockEngine)
	cfg := ThermalConfig{WarmTimeout: time.Hour, ColdTimeout: 2 * time.Hour}

	eid := createRunningBox(t, srv, eng, "active-box")

	// Recent activity — should skip agent query entirely
	srv.lastActivity.Store(eid, time.Now())

	te := srv.engine.(ThermalEngine)
	srv.runThermalCycle(te, cfg)

	eng.mu.Lock()
	state := eng.thermal[eid]
	eng.mu.Unlock()
	if state != "hot" {
		t.Fatalf("expected thermal=hot (recent activity), got %q", state)
	}
}

func TestThermalWarmToCold(t *testing.T) {
	srv, _ := setup(t)
	eng := srv.engine.(*mockEngine)
	cfg := ThermalConfig{WarmTimeout: time.Hour, ColdTimeout: 50 * time.Millisecond}

	eid := createRunningBox(t, srv, eng, "warm-box")

	// Manually set to warm (as if hot→warm already fired)
	eng.mu.Lock()
	eng.thermal[eid] = "warm"
	eng.mu.Unlock()

	// Set lastActivity to long ago — past the cold timeout
	srv.lastActivity.Store(eid, time.Now().Add(-time.Minute))

	te := srv.engine.(ThermalEngine)
	srv.runThermalCycle(te, cfg)

	// Sandbox should be stopped in store
	sb, err := srv.store.GetSandboxByID(eid)
	if err != nil {
		// Engine IDs and sandbox IDs differ in mock — look up by listing
		sandboxes, _ := srv.store.ListAllSandboxes()
		for _, s := range sandboxes {
			if s.EngineID == eid {
				sb = &s
				break
			}
		}
	}
	if sb == nil {
		t.Fatal("sandbox not found in store")
	}
	if sb.Status != "stopped" {
		t.Fatalf("expected store status=stopped after cold transition, got %q", sb.Status)
	}
}

func TestThermalWarmDoesNotQueryAgent(t *testing.T) {
	srv, _ := setup(t)
	eng := srv.engine.(*mockEngine)
	cfg := ThermalConfig{WarmTimeout: time.Hour, ColdTimeout: time.Hour}

	eid := createRunningBox(t, srv, eng, "warm-no-agent")

	// Set to warm
	eng.mu.Lock()
	eng.thermal[eid] = "warm"
	// Make Activity always fail — if the thermal cycle calls it,
	// it would previously skip the warm→cold check
	eng.ActivityErr = fmt.Errorf("agent unreachable (vCPUs paused)")
	eng.mu.Unlock()

	// Recent pause — cold timeout not reached
	srv.lastActivity.Store(eid, time.Now())

	te := srv.engine.(ThermalEngine)
	srv.runThermalCycle(te, cfg)

	// Should still be warm (cold timeout not reached), and importantly
	// should NOT have been woken to hot by an agent query
	eng.mu.Lock()
	state := eng.thermal[eid]
	eng.mu.Unlock()
	if state != "warm" {
		t.Fatalf("expected thermal=warm (agent should not be queried), got %q", state)
	}
}

func TestThermalPauseSetsLastActivity(t *testing.T) {
	srv, _ := setup(t)
	eng := srv.engine.(*mockEngine)
	cfg := ThermalConfig{WarmTimeout: 50 * time.Millisecond, ColdTimeout: time.Hour}

	eid := createRunningBox(t, srv, eng, "pause-time-box")

	// Set activity to long ago
	oldTime := time.Now().Add(-time.Minute)
	srv.lastActivity.Store(eid, oldTime)

	// Agent reports idle
	eng.mu.Lock()
	eng.ActivityResult = &proto.ActivityInfo{
		LastActivityUnix: oldTime.Unix(),
		AttachedSessions: 0,
	}
	eng.mu.Unlock()

	te := srv.engine.(ThermalEngine)
	srv.runThermalCycle(te, cfg)

	// Verify hot→warm fired
	eng.mu.Lock()
	state := eng.thermal[eid]
	eng.mu.Unlock()
	if state != "warm" {
		t.Fatalf("expected warm, got %q", state)
	}

	// Verify lastActivity was updated to ~now (not the old time)
	ts, ok := srv.lastActivity.Load(eid)
	if !ok {
		t.Fatal("lastActivity not set after pause")
	}
	pauseTime := ts.(time.Time)
	if time.Since(pauseTime) > 5*time.Second {
		t.Fatalf("lastActivity should be ~now after pause, got %v ago", time.Since(pauseTime))
	}
}
