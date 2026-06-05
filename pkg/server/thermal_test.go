package server

import (
	"context"
	"encoding/json"
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

// --- List enrichment tests ---

func TestListEnrichedThermal(t *testing.T) {
	srv, ts := setup(t)
	eng := srv.engine.(*mockEngine)

	// Create a sandbox via API
	sb := createSandbox(t, ts, uniqueName(t, "thermal-list"))

	// Set thermal state in engine
	eng.mu.Lock()
	eng.thermal[sb.EngineID] = "warm"
	eng.mu.Unlock()

	// List sandboxes
	resp := doReq(t, ts, "GET", "/sandboxes", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result []struct {
		ID      string `json:"id"`
		Thermal string `json:"thermal"`
	}
	decodeJSON(t, resp, &result)

	found := false
	for _, s := range result {
		if s.ID == sb.ID {
			if s.Thermal != "warm" {
				t.Fatalf("expected thermal=warm, got %q", s.Thermal)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatal("sandbox not found in list response")
	}
}

func TestListEnrichedURLs(t *testing.T) {
	_, ts := setup(t)

	// Create a sandbox + publish a port
	sb := createSandbox(t, ts, uniqueName(t, "url-list"))
	resp := doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/publish",
		map[string]any{"port": 3000, "alias": "test-url-list"})
	if resp.StatusCode != 201 {
		body, _ := json.Marshal(resp.Body)
		t.Fatalf("publish: expected 201, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	// List sandboxes
	resp = doReq(t, ts, "GET", "/sandboxes", nil)
	var result []struct {
		ID   string   `json:"id"`
		URLs []string `json:"urls"`
	}
	decodeJSON(t, resp, &result)

	found := false
	for _, s := range result {
		if s.ID == sb.ID {
			if len(s.URLs) == 0 {
				t.Fatal("expected URLs in list response, got none")
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatal("sandbox not found in list response")
	}
}

// --- Snapshot failure retry tests (issue #4) ---

// findSandbox finds a sandbox by engine ID in the store.
func findSandbox(t *testing.T, srv *Server, engineID string) *store.Sandbox {
	t.Helper()
	sandboxes, err := srv.store.ListAllSandboxes()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sandboxes {
		if s.EngineID == engineID {
			return &s
		}
	}
	t.Fatalf("sandbox with engineID %q not found", engineID)
	return nil
}

func TestSnapshotFailureRetries(t *testing.T) {
	srv, _ := setup(t)
	eng := srv.engine.(*mockEngine)
	cfg := ThermalConfig{WarmTimeout: time.Hour, ColdTimeout: 50 * time.Millisecond}

	eid := createRunningBox(t, srv, eng, "retry-box")

	// Set to warm, past cold timeout
	eng.mu.Lock()
	eng.thermal[eid] = "warm"
	eng.StopErr = fmt.Errorf("create Full snapshot: context deadline exceeded")
	eng.mu.Unlock()
	srv.lastActivity.Store(eid, time.Now().Add(-time.Minute))

	te := srv.engine.(ThermalEngine)

	// First failure — should NOT mark unknown
	srv.runThermalCycle(te, cfg)
	sb := findSandbox(t, srv, eid)
	if sb.Status == "unknown" {
		t.Fatal("should not mark unknown on first failure")
	}
	if got := srv.snapshotFailuresCount(eid); got != 1 {
		t.Fatalf("expected failure count 1, got %d", got)
	}

	// Second failure — still not unknown
	srv.lastActivity.Store(eid, time.Now().Add(-time.Minute))
	srv.runThermalCycle(te, cfg)
	sb = findSandbox(t, srv, eid)
	if sb.Status == "unknown" {
		t.Fatal("should not mark unknown on second failure")
	}
	if got := srv.snapshotFailuresCount(eid); got != 2 {
		t.Fatalf("expected failure count 2, got %d", got)
	}

	// Third failure — NOW mark unknown
	srv.lastActivity.Store(eid, time.Now().Add(-time.Minute))
	srv.runThermalCycle(te, cfg)
	sb = findSandbox(t, srv, eid)
	if sb.Status != "unknown" {
		t.Fatalf("expected unknown after 3 failures, got %q", sb.Status)
	}
	// Counter should be cleared after escalation
	if _, ok := srv.snapshotFailures.Load(eid); ok {
		t.Fatal("failure counter should be cleared after marking unknown")
	}
}

func TestSnapshotFailureCounterResetOnActivity(t *testing.T) {
	srv, _ := setup(t)
	eng := srv.engine.(*mockEngine)
	cfg := ThermalConfig{WarmTimeout: time.Hour, ColdTimeout: 50 * time.Millisecond}

	eid := createRunningBox(t, srv, eng, "reset-box")

	eng.mu.Lock()
	eng.thermal[eid] = "warm"
	eng.StopErr = fmt.Errorf("snapshot timeout")
	eng.mu.Unlock()

	te := srv.engine.(ThermalEngine)

	// Two failures
	srv.lastActivity.Store(eid, time.Now().Add(-time.Minute))
	srv.runThermalCycle(te, cfg)
	srv.lastActivity.Store(eid, time.Now().Add(-time.Minute))
	srv.runThermalCycle(te, cfg)

	if got := srv.snapshotFailuresCount(eid); got != 2 {
		t.Fatalf("expected 2, got %d", got)
	}

	// Simulate user activity (touchActivity resets counter)
	srv.touchActivity(eid)

	if _, ok := srv.snapshotFailures.Load(eid); ok {
		t.Fatal("failure counter should be cleared after user activity")
	}
}

func TestSnapshotSuccessClearsCounter(t *testing.T) {
	srv, _ := setup(t)
	eng := srv.engine.(*mockEngine)
	cfg := ThermalConfig{WarmTimeout: time.Hour, ColdTimeout: 50 * time.Millisecond}

	eid := createRunningBox(t, srv, eng, "clear-box")

	// Fail once
	eng.mu.Lock()
	eng.thermal[eid] = "warm"
	eng.StopErr = fmt.Errorf("transient error")
	eng.mu.Unlock()
	srv.lastActivity.Store(eid, time.Now().Add(-time.Minute))

	te := srv.engine.(ThermalEngine)
	srv.runThermalCycle(te, cfg)

	if got := srv.snapshotFailuresCount(eid); got != 1 {
		t.Fatalf("expected 1, got %d", got)
	}

	// Clear error, retry succeeds
	eng.mu.Lock()
	eng.StopErr = nil
	eng.mu.Unlock()
	srv.lastActivity.Store(eid, time.Now().Add(-time.Minute))
	srv.runThermalCycle(te, cfg)

	// Counter should be cleared on success
	if _, ok := srv.snapshotFailures.Load(eid); ok {
		t.Fatal("failure counter should be cleared after successful stop")
	}
	// Sandbox should be stopped
	sb := findSandbox(t, srv, eid)
	if sb.Status != "stopped" {
		t.Fatalf("expected stopped after successful retry, got %q", sb.Status)
	}
}

func TestEnsureHotRecoverFromUnknown(t *testing.T) {
	srv, _ := setup(t)
	eng := srv.engine.(*mockEngine)

	eid := createRunningBox(t, srv, eng, "recover-box")

	// Simulate: VM is warm in engine, but store says unknown
	eng.mu.Lock()
	eng.thermal[eid] = "warm"
	eng.mu.Unlock()

	sb := findSandbox(t, srv, eid)
	srv.store.UpdateSandboxStatus(sb.ID, "unknown")

	// ensureHot should recover it
	if err := srv.ensureHot(context.Background(), eid); err != nil {
		t.Fatalf("ensureHot failed: %v", err)
	}

	// Store should be back to running
	sb = findSandbox(t, srv, eid)
	if sb.Status != "running" {
		t.Fatalf("expected running after recovery, got %q", sb.Status)
	}
}

// --- Proxy WebSocket activity tests ---

func TestProxyWSKeepsActivityAlive(t *testing.T) {
	srv, _ := setup(t)
	eng := srv.engine.(*mockEngine)
	cfg := ThermalConfig{
		WarmTimeout: 50 * time.Millisecond,
		ColdTimeout: time.Hour,
	}

	eid := createRunningBox(t, srv, eng, "ws-proxy-box")

	// Simulate what proxyWebSocket's activity goroutine does:
	// periodic touchActivity every 10ms (fast for test)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				srv.touchActivity(eid)
			}
		}
	}()

	// Agent reports idle + no sessions — without the activity goroutine,
	// the thermal cycle would pause this sandbox
	eng.mu.Lock()
	eng.ActivityResult = &proto.ActivityInfo{
		LastActivityUnix: time.Now().Add(-time.Hour).Unix(),
		AttachedSessions: 0,
	}
	eng.mu.Unlock()

	// Run several thermal cycles
	te := srv.engine.(ThermalEngine)
	for i := 0; i < 5; i++ {
		time.Sleep(20 * time.Millisecond)
		srv.runThermalCycle(te, cfg)
	}

	// Should still be hot — activity goroutine kept it alive
	eng.mu.Lock()
	state := eng.thermal[eid]
	eng.mu.Unlock()
	if state != "hot" {
		t.Fatalf("expected hot (activity goroutine running), got %q", state)
	}

	// Stop the activity goroutine (simulates WS disconnect)
	cancel()
	time.Sleep(60 * time.Millisecond) // let WarmTimeout expire

	// Now thermal cycle should pause it
	srv.runThermalCycle(te, cfg)

	eng.mu.Lock()
	state = eng.thermal[eid]
	eng.mu.Unlock()
	if state != "warm" {
		t.Fatalf("expected warm after activity stopped, got %q", state)
	}
}
