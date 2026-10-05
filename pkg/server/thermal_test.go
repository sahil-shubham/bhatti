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

type engineWithoutThermal struct{ engine.Engine }

func TestThermalManagerRejectsEngineWithoutThermal(t *testing.T) {
	srv, _ := setup(t)
	srv.engine = engineWithoutThermal{Engine: srv.engine}
	if err := srv.StartThermalManager(ThermalConfig{}); err == nil {
		t.Fatal("engine without thermal methods must fail startup")
	}
	if srv.stopThermal != nil || srv.thermalDone != nil {
		t.Fatal("thermal manager started despite missing engine capability")
	}
}

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

func TestThermalAttachedInteractivePinsHot(t *testing.T) {
	srv, _ := setup(t)
	eng := srv.engine.(*mockEngine)
	cfg := ThermalConfig{WarmTimeout: 50 * time.Millisecond, ColdTimeout: time.Hour}

	eid := createRunningBox(t, srv, eng, "shell-box")

	// Idle long enough that it WOULD be paused, and the agent also reports idle.
	eng.mu.Lock()
	eng.ActivityResult = &proto.ActivityInfo{
		LastActivityUnix: time.Now().Add(-time.Minute).Unix(),
		AttachedSessions: 0,
	}
	eng.mu.Unlock()

	// A client is attached — host-authoritative pin, independent of the agent.
	srv.attachInteractive(eid)
	srv.lastActivity.Store(eid, time.Now().Add(-time.Minute))

	te := srv.engine.(ThermalEngine)
	srv.runThermalCycle(te, cfg)

	eng.mu.Lock()
	state := eng.thermal[eid]
	eng.mu.Unlock()
	if state != "hot" {
		t.Fatalf("attached interactive session must pin hot, got thermal=%q", state)
	}

	// Non-vacuous: after detach the SAME idle sandbox pauses — proving the pin
	// was the only thing holding it hot.
	srv.detachInteractive(eid)
	srv.lastActivity.Store(eid, time.Now().Add(-time.Minute))
	srv.runThermalCycle(te, cfg)

	eng.mu.Lock()
	state = eng.thermal[eid]
	eng.mu.Unlock()
	if state != "warm" {
		t.Fatalf("after detach an idle sandbox must pause, got thermal=%q", state)
	}
}

// TestThermalExecInFlightPinsHot: a buffered exec still running when the
// thermal manager runs keeps its sandbox hot. Before, only the start of the
// call counted as activity, so a command running past the 30 s idle window was
// frozen mid-run by the pause.
func TestThermalExecInFlightPinsHot(t *testing.T) {
	srv, ts := setup(t)
	eng := srv.engine.(*mockEngine)
	cfg := ThermalConfig{WarmTimeout: 50 * time.Millisecond, ColdTimeout: time.Hour}
	sb := createSandbox(t, ts, uniqueName(t, "long-exec"))
	eng.mu.Lock()
	eng.thermal[sb.EngineID] = "hot"
	eng.ActivityResult = &proto.ActivityInfo{LastActivityUnix: time.Now().Add(-time.Minute).Unix()}
	eng.ExecStarted = make(chan struct{}, 1)
	eng.ExecRelease = make(chan struct{})
	eng.mu.Unlock()

	done := make(chan int, 1)
	go func() {
		resp := doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/exec", map[string]any{"cmd": []string{"sleep", "60"}})
		resp.Body.Close()
		done <- resp.StatusCode
	}()
	<-eng.ExecStarted
	srv.lastActivity.Store(sb.EngineID, time.Now().Add(-time.Minute)) // idle long past WarmTimeout
	srv.runThermalCycle(srv.engine.(ThermalEngine), cfg)

	eng.mu.Lock()
	state := eng.thermal[sb.EngineID]
	eng.mu.Unlock()
	close(eng.ExecRelease)
	if code := <-done; code != 200 {
		t.Fatalf("exec: status %d", code)
	}
	if state != "hot" {
		t.Fatalf("sandbox paused under a running exec: thermal=%q", state)
	}
}

func TestThermalAttachedInteractiveNotColdStopped(t *testing.T) {
	srv, _ := setup(t)
	eng := srv.engine.(*mockEngine)
	cfg := ThermalConfig{WarmTimeout: time.Hour, ColdTimeout: 50 * time.Millisecond}

	eid := createRunningBox(t, srv, eng, "warm-shell-box")
	eng.mu.Lock()
	eng.thermal[eid] = "warm"
	eng.mu.Unlock()

	// Attached + idle past the cold timeout: must NOT be snapshotted/stopped.
	srv.attachInteractive(eid)
	srv.lastActivity.Store(eid, time.Now().Add(-time.Minute))

	te := srv.engine.(ThermalEngine)
	srv.runThermalCycle(te, cfg)

	sb, err := srv.store.GetSandboxByID(eid)
	if err != nil {
		t.Fatal(err)
	}
	if sb.Status == "stopped" {
		t.Fatalf("attached interactive session must not be cold-stopped")
	}

	// Non-vacuous: detach, age activity, and the same warm box is stopped.
	srv.detachInteractive(eid)
	srv.lastActivity.Store(eid, time.Now().Add(-time.Minute))
	srv.runThermalCycle(te, cfg)

	sb, err = srv.store.GetSandboxByID(eid)
	if err != nil {
		t.Fatal(err)
	}
	if sb.Status != "stopped" {
		t.Fatalf("after detach an idle warm sandbox must cold-stop, got %q", sb.Status)
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

type uncleanStopTestError struct{ reason string }

func (e uncleanStopTestError) Error() string             { return "power-off succeeded, sync failed: " + e.reason }
func (e uncleanStopTestError) UncleanStopReason() string { return e.reason }

// A real Stop implementation kills the helper and returns a durability warning.
// This wrapper models that outcome without treating the warning as a failed Stop.
type uncleanStopTestEngine struct {
	*mockEngine
	reason string
}

func (e *uncleanStopTestEngine) Stop(ctx context.Context, id string) error {
	if err := e.mockEngine.Stop(ctx, id); err != nil {
		return err
	}
	return uncleanStopTestError{reason: e.reason}
}

func TestThermalUncleanPowerOffRecordsReasonWithoutRetry(t *testing.T) {
	srv, _ := setup(t)
	eng := srv.engine.(*mockEngine)
	eid := createRunningBox(t, srv, eng, "thermal-unclean")
	srv.engine = &uncleanStopTestEngine{mockEngine: eng, reason: "guest sync exited 1"}
	srv.events = NewEventRecorder(srv.store)
	eng.mu.Lock()
	eng.thermal[eid] = "warm"
	eng.mu.Unlock()
	srv.lastActivity.Store(eid, time.Now().Add(-time.Minute))

	srv.runThermalCycle(srv.engine.(ThermalEngine), ThermalConfig{ColdTimeout: time.Second})

	if sb := findSandbox(t, srv, eid); sb.Status != "stopped" {
		t.Fatalf("powered-off VM left store at status %q", sb.Status)
	}
	if n := srv.snapshotFailuresCount(eid); n != 0 {
		t.Fatalf("sync warning wrongly scheduled thermal retries: %d", n)
	}
	srv.events.Close()
	srv.events = nil
	events, err := srv.store.QueryEvents(store.EventFilter{SandboxID: eid})
	if err != nil {
		t.Fatal(err)
	}
	var gotUnclean, gotStopped bool
	for _, event := range events {
		switch event.Type {
		case "sandbox.unclean_stop":
			gotUnclean = event.Meta["reason"] == "guest sync exited 1"
		case "sandbox.stopped":
			gotStopped = event.Meta["reason"] == "thermal"
		case "thermal.snapshot_failed":
			t.Fatal("powered-off VM recorded as failed snapshot")
		}
	}
	if !gotUnclean || !gotStopped {
		t.Fatalf("missing persisted unclean and stopped events: %+v", events)
	}
}

type forcePauseTestEngine struct {
	*mockEngine
	forced, ordinary int
}

func (e *forcePauseTestEngine) Pause(ctx context.Context, id string) error {
	e.ordinary++
	return e.mockEngine.Pause(ctx, id)
}

func (e *forcePauseTestEngine) ForcePause(ctx context.Context, id string) error {
	e.forced++
	return e.mockEngine.Pause(ctx, id)
}

func TestThermalAgentFailureUsesForcePause(t *testing.T) {
	srv, _ := setup(t)
	eng := srv.engine.(*mockEngine)
	eid := createRunningBox(t, srv, eng, "force-pause")
	wrapped := &forcePauseTestEngine{mockEngine: eng}
	srv.engine = wrapped
	eng.mu.Lock()
	eng.ActivityErr = fmt.Errorf("agent unreachable")
	eng.mu.Unlock()
	srv.lastActivity.Store(eid, time.Now().Add(-time.Minute))
	for range maxThermalFailures {
		srv.runThermalCycle(wrapped, ThermalConfig{WarmTimeout: time.Second})
	}
	if wrapped.forced != 1 || wrapped.ordinary != 0 || wrapped.ThermalState(eid) != "warm" {
		t.Fatalf("unresponsive guest: forced=%d ordinary=%d state=%s",
			wrapped.forced, wrapped.ordinary, wrapped.ThermalState(eid))
	}
}

type pinBeforePauseEngine struct {
	*mockEngine
	activityStarted chan struct{}
	activityRelease chan struct{}
}

func (e *pinBeforePauseEngine) Activity(ctx context.Context, id string) (*proto.ActivityInfo, error) {
	close(e.activityStarted)
	select {
	case <-e.activityRelease:
		return e.mockEngine.Activity(ctx, id)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestThermalPinAcquiredDuringActivityPreventsPause(t *testing.T) {
	srv, _ := setup(t)
	eng := srv.engine.(*mockEngine)
	eid := createRunningBox(t, srv, eng, "pinned-during-activity")
	eng.mu.Lock()
	eng.ActivityResult = &proto.ActivityInfo{LastActivityUnix: time.Now().Add(-time.Hour).Unix()}
	eng.mu.Unlock()
	wrapped := &pinBeforePauseEngine{
		mockEngine: eng, activityStarted: make(chan struct{}), activityRelease: make(chan struct{}),
	}
	srv.engine = wrapped
	srv.lastActivity.Store(eid, time.Now().Add(-time.Hour))
	done := make(chan struct{})
	go func() {
		srv.runThermalCycle(wrapped, ThermalConfig{WarmTimeout: time.Second})
		close(done)
	}()
	<-wrapped.activityStarted
	// A backup pins the VM after the first host-side pin check, while the
	// guest Activity request is in flight. It must not be paused at its end.
	srv.attachInteractive(eid)
	close(wrapped.activityRelease)
	<-done
	if got := eng.ThermalState(eid); got != "hot" {
		t.Fatalf("guest paused after backup acquired thermal pin: %s", got)
	}
	srv.detachInteractive(eid)
}

type blockedThermalTransition struct {
	*mockEngine
	id, operation string
	entered       chan struct{}
	release       chan struct{}
}

func (e *blockedThermalTransition) hold(id, operation string) {
	if id == e.id && operation == e.operation {
		close(e.entered)
		<-e.release
	}
}

func (e *blockedThermalTransition) Pause(ctx context.Context, id string) error {
	e.hold(id, "pause")
	return e.mockEngine.Pause(ctx, id)
}

func (e *blockedThermalTransition) ForcePause(ctx context.Context, id string) error {
	e.hold(id, "force")
	return e.mockEngine.Pause(ctx, id)
}

func (e *blockedThermalTransition) Stop(ctx context.Context, id string) error {
	e.hold(id, "stop")
	return e.mockEngine.Stop(ctx, id)
}

func TestThermalSlowTransitionDoesNotBlockAnotherSandboxPin(t *testing.T) {
	for _, operation := range []string{"pause", "force", "stop"} {
		t.Run(operation, func(t *testing.T) {
			srv, _ := setup(t)
			eng := srv.engine.(*mockEngine)
			slow := createRunningBox(t, srv, eng, "slow-"+operation)
			other := createRunningBox(t, srv, eng, "other-"+operation)
			wrapped := &blockedThermalTransition{
				mockEngine: eng, id: slow, operation: operation,
				entered: make(chan struct{}), release: make(chan struct{}),
			}
			srv.engine = wrapped
			srv.lastActivity.Store(slow, time.Now().Add(-2*time.Hour))
			srv.lastActivity.Store(other, time.Now())
			eng.mu.Lock()
			switch operation {
			case "stop":
				eng.thermal[slow] = "warm"
			case "pause":
				eng.ActivityResult = &proto.ActivityInfo{LastActivityUnix: time.Now().Add(-2 * time.Hour).Unix()}
			case "force":
				eng.ActivityErr = fmt.Errorf("unresponsive agent")
			}
			eng.mu.Unlock()
			if operation == "force" {
				for range maxThermalFailures - 1 {
					srv.incrementThermalFails(slow)
				}
			}

			cycleDone := make(chan struct{})
			go func() {
				srv.runThermalCycle(wrapped, ThermalConfig{WarmTimeout: time.Hour, ColdTimeout: time.Hour})
				close(cycleDone)
			}()
			<-wrapped.entered // guest A is held inside Pause/ForcePause/Stop
			pinned := make(chan struct{})
			go func() {
				srv.attachInteractive(other)
				close(pinned)
			}()
			free := false
			select {
			case <-pinned:
				free = true
			case <-time.After(time.Second):
			}
			close(wrapped.release)
			<-cycleDone
			<-pinned
			if !free {
				t.Fatal("guest A's slow transition blocked guest B's backup/interactive pin")
			}
			if got := wrapped.ThermalState(other); got != "hot" {
				t.Fatalf("unrelated guest B cooled during pin: %s", got)
			}
			srv.detachInteractive(other)
			srv.transitionMu.Lock()
			n := len(srv.transitionGates)
			srv.transitionMu.Unlock()
			if n != 0 {
				t.Fatalf("%d unused per-sandbox transition gates leaked", n)
			}
		})
	}
}

type parallelColdStopEngine struct {
	*mockEngine
	entered chan string
	release chan struct{}
}

func (e *parallelColdStopEngine) Stop(ctx context.Context, id string) error {
	select {
	case e.entered <- id:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-e.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return e.mockEngine.Stop(ctx, id)
}

func TestThermalColdStopsDoNotSerializeOtherSandboxes(t *testing.T) {
	srv, _ := setup(t)
	eng := srv.engine.(*mockEngine)
	hot := createRunningBox(t, srv, eng, "hot-after-cold")
	warmA := createRunningBox(t, srv, eng, "cold-first")
	warmB := createRunningBox(t, srv, eng, "cold-second")
	eng.mu.Lock()
	eng.thermal[warmA], eng.thermal[warmB] = "warm", "warm"
	eng.ActivityResult = &proto.ActivityInfo{LastActivityUnix: time.Now().Add(-2 * time.Hour).Unix()}
	eng.mu.Unlock()
	for _, id := range []string{hot, warmA, warmB} {
		srv.lastActivity.Store(id, time.Now().Add(-2*time.Hour))
	}
	wrapped := &parallelColdStopEngine{
		mockEngine: eng, entered: make(chan string, 2), release: make(chan struct{}),
	}
	srv.engine = wrapped
	cycleDone := make(chan struct{})
	go func() {
		srv.runThermalCycle(wrapped, ThermalConfig{WarmTimeout: time.Hour, ColdTimeout: time.Hour})
		close(cycleDone)
	}()

	// Both warm guests must reach Stop without either completing its guest sync.
	// The hot guest must already have been processed, even though it was
	// created before the warm guests and appears later in the store's list.
	entered := make(map[string]bool)
	for range 2 {
		select {
		case id := <-wrapped.entered:
			entered[id] = true
		case <-time.After(time.Second):
			close(wrapped.release)
			<-cycleDone
			t.Fatal("a slow warm→cold stop serialized the thermal cycle")
		}
	}
	if !entered[warmA] || !entered[warmB] {
		close(wrapped.release)
		<-cycleDone
		t.Fatalf("cold stop called for unexpected sandboxes: %v", entered)
	}
	hotState := wrapped.ThermalState(hot)
	close(wrapped.release)
	<-cycleDone // all store updates and events complete before the cycle returns
	if hotState != "warm" {
		t.Fatalf("idle hot guest was not paused before cold stops: %s", hotState)
	}
	for _, id := range []string{warmA, warmB} {
		if sb := findSandbox(t, srv, id); sb.Status != "stopped" {
			t.Fatalf("cold stop for %s finished without updating store: %s", id, sb.Status)
		}
	}
}
