package store

import (
	"path/filepath"
	"testing"
	"time"
)

func pendingIDs(t *testing.T, s *Store) []string {
	t.Helper()
	list, err := s.ListRestartPending()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, sb := range list {
		ids = append(ids, sb.ID)
	}
	return ids
}

// restart_pending is the stored intent that a keep_hot sandbox went down
// unasked. It is set by an unexpected stop, and dropped by anything that means
// the sandbox should stay down or is up again.
func TestRestartPendingMarker(t *testing.T) {
	s := testStore(t)
	mk := func(id string, keepHot bool) {
		t.Helper()
		if err := s.CreateSandbox(Sandbox{ID: id, Name: id, EngineID: id, Status: "running", CreatedBy: "u", CreatedAt: time.Now(), KeepHot: keepHot}); err != nil {
			t.Fatal(err)
		}
	}
	mk("hot", true)
	mk("cold", false)

	if err := s.StopSandboxUnexpected("hot"); err != nil {
		t.Fatal(err)
	}
	if err := s.StopSandboxUnexpected("cold"); err != nil {
		t.Fatal(err)
	}
	if got := pendingIDs(t, s); len(got) != 1 || got[0] != "hot" {
		t.Fatalf("pending after unexpected stops = %v, want only the keep_hot one", got)
	}

	// Reopening the database runs the migrations again and keeps the marker.
	path := filepath.Join(t.TempDir(), "reopen.db")
	s2, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	s2.Close()
	if s2, err = New(path); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	s2.Close()

	// A status write that is not "stopped" (running again, destroyed) clears it;
	// writing "stopped" keeps it (the GET handler mirrors engine status).
	if err := s.UpdateSandboxStatus("hot", "stopped"); err != nil {
		t.Fatal(err)
	}
	if got := pendingIDs(t, s); len(got) != 1 {
		t.Fatalf("stopped status write dropped the marker: %v", got)
	}
	if err := s.UpdateSandboxStatus("hot", "running"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateSandboxStatus("hot", "stopped"); err != nil {
		t.Fatal(err)
	}
	if got := pendingIDs(t, s); len(got) != 0 {
		t.Fatalf("marker survived the sandbox running again: %v", got)
	}

	// An owner's stop drops it.
	s.StopSandboxUnexpected("hot")
	if err := s.StopSandbox("hot"); err != nil {
		t.Fatal(err)
	}
	if got := pendingIDs(t, s); len(got) != 0 {
		t.Fatalf("marker survived an explicit stop: %v", got)
	}

	// keep_hot turning off drops it, even if it turns back on.
	s.StopSandboxUnexpected("hot")
	if err := s.UpdateSandboxKeepHot("hot", false); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateSandboxKeepHot("hot", true); err != nil {
		t.Fatal(err)
	}
	if got := pendingIDs(t, s); len(got) != 0 {
		t.Fatalf("marker survived keep_hot=false: %v", got)
	}

	// Giving up drops it; a sandbox that is not stopped is never listed.
	s.StopSandboxUnexpected("hot")
	if err := s.ClearRestartPending("hot"); err != nil {
		t.Fatal(err)
	}
	if got := pendingIDs(t, s); len(got) != 0 {
		t.Fatalf("marker survived ClearRestartPending: %v", got)
	}
}
