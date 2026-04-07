package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

// setupPublicProxy creates a server with a properly wired PublicProxyHandler
// (path-based mode), matching production wiring in main.go.
// Returns the server, engine, and a test HTTP server that routes through
// the public proxy (unauthenticated, like the real listener).
func setupPublicProxy(t *testing.T) (*Server, *mockEngine, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	keyHash := sha256Hex(testAPIKey)
	st.CreateUser(store.User{
		ID: "usr_test", Name: "test-user", APIKeyHash: keyHash,
		MaxSandboxes: 50, MaxCPUsPerSandbox: 4, MaxMemoryMBPerSandbox: 4096,
		SubnetIndex: 1, CreatedAt: time.Now(),
	})

	eng := newMockEngine()
	srv := New(eng, st, dir)

	// Wire exactly like main.go: both callbacks delegate to Server
	pub := NewPublicProxyHandler(eng, st, srv.ResumeSem(),
		func(engineID string) { srv.TouchActivity(engineID) },
		func(ctx context.Context, engineID string) error { return srv.EnsureHot(ctx, engineID) },
	)
	srv.SetPublicProxy(pub)

	ts := httptest.NewServer(http.HandlerFunc(pub.ServeHTTPPathBased))
	t.Cleanup(func() { srv.Close(); ts.Close(); st.Close() })
	return srv, eng, ts
}

// publishSandbox creates a running sandbox with a published port.
// Returns the store sandbox and engine ID.
func publishSandbox(t *testing.T, srv *Server, eng *mockEngine, name, alias string, port int) (store.Sandbox, string) {
	t.Helper()
	info, err := eng.Create(nil, engine.SandboxSpec{Name: name})
	if err != nil {
		t.Fatal(err)
	}
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

	rule := store.PublishRule{
		ID: "pub_" + genID(), SandboxID: sb.ID, UserID: "usr_test",
		Port: port, Alias: alias,
	}
	if err := srv.store.CreatePublishRule(rule); err != nil {
		t.Fatal(err)
	}
	return sb, info.EngineID
}

// hitPublicProxy fires an HTTP GET through the public proxy and returns
// immediately without waiting for the response body. The side effects we
// test (ensureHot, touchActivity) fire before the tunnel proxy, so a short
// timeout is fine — the mock tunnel will block but we don't care about
// the response.
func hitPublicProxy(t *testing.T, ts *httptest.Server, alias string) {
	t.Helper()
	client := &http.Client{Timeout: 100 * time.Millisecond}
	resp, err := client.Get(ts.URL + "/" + alias + "/")
	if err != nil {
		// Expected: mock tunnel blocks → client timeout. Side effects
		// already fired before the tunnel proxy started.
		return
	}
	resp.Body.Close()
}

// ==========================================================================
// Tests
// ==========================================================================

// TestPublicProxyColdWakeUpdatesStore: a cold sandbox woken via public
// proxy must have its store status updated to "running". Without this,
// the thermal manager never sees it (it only processes status=="running")
// and the VM leaks resources forever.
func TestPublicProxyColdWakeUpdatesStore(t *testing.T) {
	srv, eng, ts := setupPublicProxy(t)
	sb, eid := publishSandbox(t, srv, eng, "cold-wake-store", "cold-wake", 8080)

	// Simulate cold sandbox: engine says "cold", store says "stopped"
	eng.mu.Lock()
	eng.thermal[eid] = "cold"
	eng.mu.Unlock()
	srv.store.StopSandbox(sb.ID)

	// Precondition
	got, _ := srv.store.GetSandboxByID(sb.ID)
	if got.Status != "stopped" {
		t.Fatalf("precondition: expected stopped, got %q", got.Status)
	}

	hitPublicProxy(t, ts, "cold-wake")

	// Store must be "running" now
	got, _ = srv.store.GetSandboxByID(sb.ID)
	if got.Status != "running" {
		t.Fatalf("expected store status=running after cold wake via public proxy, got %q", got.Status)
	}
}

// TestPublicProxyHTTPTouchesActivity: every HTTP request through the
// public proxy must update lastActivity so the thermal manager knows
// the sandbox has traffic. Without this, the thermal manager sees stale
// timestamps and cools the sandbox despite active public traffic.
func TestPublicProxyHTTPTouchesActivity(t *testing.T) {
	srv, eng, ts := setupPublicProxy(t)
	_, eid := publishSandbox(t, srv, eng, "activity-http", "act-http", 8080)

	// Clear any existing activity
	srv.lastActivity.Delete(eid)

	hitPublicProxy(t, ts, "act-http")

	val, ok := srv.lastActivity.Load(eid)
	if !ok {
		t.Fatal("lastActivity not set after public proxy HTTP request")
	}
	if elapsed := time.Since(val.(time.Time)); elapsed > 5*time.Second {
		t.Fatalf("lastActivity should be recent, was %v ago", elapsed)
	}
}

// TestPublicProxyThermalCycleRespectsHTTPTraffic: a sandbox receiving
// continuous HTTP traffic through the public proxy must NOT be cooled
// by the thermal manager, even when the guest agent reports idle.
func TestPublicProxyThermalCycleRespectsHTTPTraffic(t *testing.T) {
	srv, eng, ts := setupPublicProxy(t)
	_, eid := publishSandbox(t, srv, eng, "thermal-http", "therm-http", 8080)

	// WarmTimeout must be longer than the mock tunnel round-trip (~100ms)
	// so that lastActivity set by onActivity is still fresh when the
	// thermal cycle runs. In production, HTTP requests complete in <10ms.
	cfg := ThermalConfig{WarmTimeout: 500 * time.Millisecond, ColdTimeout: time.Hour}

	// Agent reports idle — without activity tracking, thermal manager
	// would pause this sandbox.
	eng.mu.Lock()
	eng.ActivityResult = &proto.ActivityInfo{
		LastActivityUnix: time.Now().Add(-time.Hour).Unix(),
		AttachedSessions: 0,
	}
	eng.mu.Unlock()

	te := srv.engine.(ThermalEngine)

	// Simulate continuous HTTP traffic interleaved with thermal cycles.
	// Each hitPublicProxy sets lastActivity via onActivity before the
	// mock tunnel blocks. The thermal cycle should see fresh activity.
	for i := 0; i < 5; i++ {
		hitPublicProxy(t, ts, "therm-http")
		srv.runThermalCycle(te, cfg)
	}

	eng.mu.Lock()
	state := eng.thermal[eid]
	eng.mu.Unlock()
	if state != "hot" {
		t.Fatalf("expected hot (public proxy traffic active), got %q", state)
	}
}

// TestPublicProxyColdWakeThermalManaged: after a cold wake via public
// proxy, the sandbox must be visible to the thermal manager. If the
// store isn't updated to "running", the thermal manager skips it and
// the VM stays hot forever, leaking resources.
func TestPublicProxyColdWakeThermalManaged(t *testing.T) {
	srv, eng, ts := setupPublicProxy(t)
	sb, eid := publishSandbox(t, srv, eng, "cold-thermal", "cold-therm", 8080)

	cfg := ThermalConfig{WarmTimeout: 50 * time.Millisecond, ColdTimeout: time.Hour}

	// Cold sandbox
	eng.mu.Lock()
	eng.thermal[eid] = "cold"
	eng.mu.Unlock()
	srv.store.StopSandbox(sb.ID)

	// Wake via public proxy
	hitPublicProxy(t, ts, "cold-therm")

	// Set activity to long ago + agent reports idle
	srv.lastActivity.Store(eid, time.Now().Add(-time.Minute))
	eng.mu.Lock()
	eng.ActivityResult = &proto.ActivityInfo{
		LastActivityUnix: time.Now().Add(-time.Minute).Unix(),
		AttachedSessions: 0,
	}
	eng.mu.Unlock()

	// Run thermal cycle
	te := srv.engine.(ThermalEngine)
	srv.runThermalCycle(te, cfg)

	// Thermal manager should have transitioned it to warm.
	// If store still says "stopped", thermal manager skips it → stays hot.
	eng.mu.Lock()
	state := eng.thermal[eid]
	eng.mu.Unlock()
	if state != "warm" {
		t.Fatalf("expected warm (thermal manager should see sandbox after cold wake), got %q", state)
	}
}

// TestPublicProxySnapshotFailureResetOnTraffic: public proxy traffic
// should reset the snapshot failure counter via touchActivity, same as
// authenticated API traffic does.
func TestPublicProxySnapshotFailureResetOnTraffic(t *testing.T) {
	srv, eng, ts := setupPublicProxy(t)
	_, eid := publishSandbox(t, srv, eng, "snap-reset", "snap-rst", 8080)

	// Simulate 2 consecutive snapshot failures
	srv.snapshotFailures.Store(eid, 2)

	hitPublicProxy(t, ts, "snap-rst")

	if _, ok := srv.snapshotFailures.Load(eid); ok {
		t.Fatal("snapshot failure counter should be cleared after public proxy traffic")
	}
}
