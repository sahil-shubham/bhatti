package store

import (
	"testing"
	"time"
)

// Tests for sandbox labels (G1.6 of PLAN-bhatti-v2.md). The store layer
// is the lowest tier — JSON marshal/unmarshal, transactional merge for
// PATCH, and the filtered list helper.

// labelTestSandbox returns a baseline Sandbox with the given labels
// installed in the store. Reuses the testStore fixture from
// store_test.go.
func labelTestSandbox(t *testing.T, s *Store, id, name string, labels map[string]string) Sandbox {
	t.Helper()
	sb := Sandbox{
		ID:        id,
		Name:      name,
		Status:    "running",
		CreatedBy: "usr_test",
		CreatedAt: time.Now(),
		CPUs:      1,
		MemoryMB:  256,
		Image:     "minimal",
		Labels:    labels,
	}
	if err := s.CreateSandbox(sb); err != nil {
		t.Fatal(err)
	}
	return sb
}

// TestSandboxLabels_CreateAndRead verifies the basic round-trip:
// labels set at Create are returned from Get and List.
func TestSandboxLabels_CreateAndRead(t *testing.T) {
	s := testStore(t)
	s.CreateUser(User{
		ID: "usr_test", Name: "test", APIKeyHash: "h",
		MaxSandboxes: 10, CreatedAt: time.Now(),
	})

	labels := map[string]string{"pool": "workers", "env": "prod"}
	labelTestSandbox(t, s, "sb1", "w1", labels)

	got, err := s.GetSandbox("usr_test", "sb1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Labels["pool"] != "workers" || got.Labels["env"] != "prod" {
		t.Fatalf("labels not round-tripped: %v", got.Labels)
	}
}

// TestSandboxLabels_EmptyMapIsBenign verifies that nil/empty labels
// don't break Create (the SQL column is NOT NULL DEFAULT '{}'). On
// read, an empty map serialises back as a nil map (omitempty on JSON).
func TestSandboxLabels_EmptyMapIsBenign(t *testing.T) {
	s := testStore(t)
	s.CreateUser(User{
		ID: "usr_test", Name: "test", APIKeyHash: "h",
		MaxSandboxes: 10, CreatedAt: time.Now(),
	})

	// nil map
	labelTestSandbox(t, s, "sb_nil", "n", nil)
	// explicit empty
	labelTestSandbox(t, s, "sb_empty", "e", map[string]string{})

	for _, id := range []string{"sb_nil", "sb_empty"} {
		got, err := s.GetSandbox("usr_test", id)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Labels) != 0 {
			t.Fatalf("%s: expected no labels, got %v", id, got.Labels)
		}
	}
}

// TestSandboxLabels_FilterAND covers the selector contract used by
// `bhatti ls --label k1=v1 --label k2=v2`. All filter pairs must match
// (AND), and extra labels on a sandbox don't break the match.
func TestSandboxLabels_FilterAND(t *testing.T) {
	s := testStore(t)
	s.CreateUser(User{
		ID: "usr_test", Name: "test", APIKeyHash: "h",
		MaxSandboxes: 10, CreatedAt: time.Now(),
	})

	labelTestSandbox(t, s, "sb_a", "a", map[string]string{"pool": "workers", "env": "prod"})
	labelTestSandbox(t, s, "sb_b", "b", map[string]string{"pool": "workers", "env": "staging"})
	labelTestSandbox(t, s, "sb_c", "c", map[string]string{"pool": "database", "env": "prod"})
	labelTestSandbox(t, s, "sb_d", "d", map[string]string{"pool": "workers", "env": "prod", "extra": "yes"})

	tests := []struct {
		name   string
		filter map[string]string
		wantID map[string]bool
	}{
		{"single match", map[string]string{"pool": "workers"}, map[string]bool{"sb_a": true, "sb_b": true, "sb_d": true}},
		{"both labels AND", map[string]string{"pool": "workers", "env": "prod"}, map[string]bool{"sb_a": true, "sb_d": true}},
		{"no match", map[string]string{"pool": "workers", "env": "qa"}, map[string]bool{}},
		{"single env", map[string]string{"env": "prod"}, map[string]bool{"sb_a": true, "sb_c": true, "sb_d": true}},
		{"no filter", nil, map[string]bool{"sb_a": true, "sb_b": true, "sb_c": true, "sb_d": true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.ListSandboxesWithFilter("usr_test", tt.filter)
			if err != nil {
				t.Fatal(err)
			}
			ids := map[string]bool{}
			for _, sb := range got {
				ids[sb.ID] = true
			}
			if len(ids) != len(tt.wantID) {
				t.Fatalf("count: got %v (%d), want %v (%d)", ids, len(ids), tt.wantID, len(tt.wantID))
			}
			for id := range tt.wantID {
				if !ids[id] {
					t.Errorf("expected %s in result, got %v", id, ids)
				}
			}
		})
	}
}

// TestSandboxLabels_UpdateMerge covers the PATCH semantics: `set`
// inserts/overwrites, `remove` deletes, and other labels are preserved.
func TestSandboxLabels_UpdateMerge(t *testing.T) {
	s := testStore(t)
	s.CreateUser(User{
		ID: "usr_test", Name: "test", APIKeyHash: "h",
		MaxSandboxes: 10, CreatedAt: time.Now(),
	})

	labelTestSandbox(t, s, "sb1", "w1", map[string]string{
		"pool": "workers",
		"env":  "prod",
		"tier": "small",
	})

	// Overwrite env, add a new key, remove tier.
	err := s.UpdateSandboxLabels("usr_test", "sb1",
		map[string]string{"env": "staging", "team": "platform"},
		[]string{"tier"})
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.GetSandbox("usr_test", "sb1")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"pool": "workers",   // preserved
		"env":  "staging",   // overwritten
		"team": "platform",  // added
		// tier — removed
	}
	if len(got.Labels) != len(want) {
		t.Fatalf("got %v, want %v", got.Labels, want)
	}
	for k, v := range want {
		if got.Labels[k] != v {
			t.Errorf("label %q: got %q, want %q", k, got.Labels[k], v)
		}
	}
	if _, ok := got.Labels["tier"]; ok {
		t.Errorf("label 'tier' should have been removed: %v", got.Labels)
	}
}

// TestSandboxLabels_UpdateWrongUser fails with "not found" if the
// (id, userID) pair doesn't exist. Protects against cross-user label
// edits via spoofed IDs.
func TestSandboxLabels_UpdateWrongUser(t *testing.T) {
	s := testStore(t)
	s.CreateUser(User{ID: "usr_a", Name: "a", APIKeyHash: "ha", MaxSandboxes: 10, CreatedAt: time.Now()})
	s.CreateUser(User{ID: "usr_b", Name: "b", APIKeyHash: "hb", MaxSandboxes: 10, CreatedAt: time.Now()})

	// sandbox owned by usr_a
	if err := s.CreateSandbox(Sandbox{
		ID: "sb1", Name: "w1", Status: "running",
		CreatedBy: "usr_a", CreatedAt: time.Now(), CPUs: 1, MemoryMB: 256, Image: "minimal",
		Labels: map[string]string{"pool": "workers"},
	}); err != nil {
		t.Fatal(err)
	}

	// usr_b tries to update — must fail with not-found.
	err := s.UpdateSandboxLabels("usr_b", "sb1", map[string]string{"hijacked": "yes"}, nil)
	if err == nil {
		t.Fatal("expected not-found error for cross-user update, got nil")
	}

	// And the original sandbox's labels are untouched.
	got, _ := s.GetSandbox("usr_a", "sb1")
	if _, ok := got.Labels["hijacked"]; ok {
		t.Fatalf("cross-user update succeeded: %v", got.Labels)
	}
}

// TestSandboxLabels_UpdateNoOp verifies that empty set + empty remove
// is a no-op that still validates ownership.
func TestSandboxLabels_UpdateNoOp(t *testing.T) {
	s := testStore(t)
	s.CreateUser(User{ID: "usr_test", Name: "t", APIKeyHash: "h", MaxSandboxes: 10, CreatedAt: time.Now()})
	labelTestSandbox(t, s, "sb1", "w1", map[string]string{"a": "1"})

	if err := s.UpdateSandboxLabels("usr_test", "sb1", nil, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetSandbox("usr_test", "sb1")
	if got.Labels["a"] != "1" {
		t.Fatalf("no-op altered labels: %v", got.Labels)
	}

	// Non-existent sandbox still errors.
	if err := s.UpdateSandboxLabels("usr_test", "sb_nope", nil, nil); err == nil {
		t.Fatal("expected error for non-existent sandbox")
	}
}
