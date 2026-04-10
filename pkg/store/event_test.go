package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestInsertAndQueryEvents(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Insert a batch of events
	events := []Event{
		{Type: "sandbox.created", UserID: "usr_alice", SandboxID: "sb_1",
			Meta: map[string]any{"name": "dev", "cpus": 2}},
		{Type: "sandbox.destroyed", UserID: "usr_alice", SandboxID: "sb_1",
			Meta: map[string]any{"name": "dev", "lifetime_s": 3600}},
		{Type: "thermal.pause", SandboxID: "sb_2",
			Meta: map[string]any{"sandbox": "api-server", "idle_s": 30}},
		{Type: "thermal.wake", SandboxID: "sb_2",
			Meta: map[string]any{"sandbox": "api-server", "from_state": "warm", "wake_ms": 52}},
		{Type: "auth.failed",
			Meta: map[string]any{"ip": "1.2.3.4"}},
	}
	if err := st.InsertEvents(events); err != nil {
		t.Fatal(err)
	}

	// Query all
	all, err := st.QueryEvents(EventFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 5 {
		t.Fatalf("expected 5 events, got %d", len(all))
	}

	// Query by exact type
	sandboxCreated, err := st.QueryEvents(EventFilter{Type: "sandbox.created"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sandboxCreated) != 1 {
		t.Fatalf("expected 1 sandbox.created, got %d", len(sandboxCreated))
	}
	if sandboxCreated[0].Meta["name"] != "dev" {
		t.Errorf("expected name=dev, got %v", sandboxCreated[0].Meta["name"])
	}

	// Query by prefix type (thermal matches thermal.*)
	thermal, err := st.QueryEvents(EventFilter{Type: "thermal"})
	if err != nil {
		t.Fatal(err)
	}
	if len(thermal) != 2 {
		t.Fatalf("expected 2 thermal events, got %d", len(thermal))
	}

	// Query by user
	alice, err := st.QueryEvents(EventFilter{UserID: "usr_alice"})
	if err != nil {
		t.Fatal(err)
	}
	if len(alice) != 2 {
		t.Fatalf("expected 2 alice events, got %d", len(alice))
	}

	// Query by sandbox_id
	sb2, err := st.QueryEvents(EventFilter{SandboxID: "sb_2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sb2) != 2 {
		t.Fatalf("expected 2 sb_2 events, got %d", len(sb2))
	}

	// Count
	count, err := st.CountEvents(EventFilter{Type: "sandbox"})
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("expected count=2, got %d", count)
	}
}

func TestMetricsSnapshots(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Insert snapshots
	for i := 0; i < 3; i++ {
		snap := MetricsSnapshot{
			APIRequests:    int64(10 + i),
			APIErrors:      int64(i),
			SandboxesTotal: 5,
			SandboxesHot:   2,
			SandboxesWarm:  1,
			SandboxesCold:  2,
			HostLoad1m:     0.5,
		}
		if err := st.InsertMetricsSnapshot(snap); err != nil {
			t.Fatal(err)
		}
	}

	// Query all
	snaps, err := st.QueryMetricsSnapshots(time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 3 {
		t.Fatalf("expected 3 snapshots, got %d", len(snaps))
	}

	// Sum
	sums, err := st.SumMetricsSnapshots(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	// 10 + 11 + 12 = 33
	if sums.APIRequests != 33 {
		t.Errorf("expected sum api_requests=33, got %d", sums.APIRequests)
	}

	// Latest
	latest, err := st.LatestMetricsSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if latest.APIRequests != 12 {
		t.Errorf("expected latest api_requests=12, got %d", latest.APIRequests)
	}
}

func TestRetention(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Insert events
	if err := st.InsertEvents([]Event{
		{Type: "sandbox.created", Meta: map[string]any{"name": "test"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertMetricsSnapshot(MetricsSnapshot{APIRequests: 1}); err != nil {
		t.Fatal(err)
	}

	// Small sleep so the cutoff timestamp is strictly after the inserted rows
	time.Sleep(10 * time.Millisecond)

	// Purge with 0 retention (deletes everything)
	n, err := st.PurgeOldEvents(0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected 1 event purged, got %d", n)
	}

	n, err = st.PurgeOldMetricsSnapshots(0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected 1 snapshot purged, got %d", n)
	}

	// Verify empty
	events, _ := st.QueryEvents(EventFilter{Limit: 100})
	if len(events) != 0 {
		t.Errorf("expected 0 events after purge, got %d", len(events))
	}
}
