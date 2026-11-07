package store

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"modernc.org/sqlite"
)

// A unique violation is ErrConflict whichever key refused the write, and the
// driver's error stays reachable behind it.
func TestClassifyUniqueViolationIsConflict(t *testing.T) {
	s := testStore(t)
	insert := func(id, name, hash string) error {
		_, err := s.db.Exec(`INSERT INTO users (id, name, api_key_hash) VALUES (?, ?, ?)`, id, name, hash)
		return err
	}
	if err := insert("u1", "alice", "h1"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ what, id, name, hash string }{
		{"a taken name (UNIQUE column)", "u2", "alice", "h2"},
		{"a taken key hash (UNIQUE column)", "u3", "carol", "h1"},
		{"a taken id (PRIMARY KEY)", "u1", "dave", "h4"},
	} {
		err := classify(insert(tc.id, tc.name, tc.hash))
		if !errors.Is(err, ErrConflict) {
			t.Errorf("%s: %v, want ErrConflict", tc.what, err)
		}
		var driver *sqlite.Error
		if !errors.As(err, &driver) {
			t.Errorf("%s: the driver error is gone from the chain: %v", tc.what, err)
		}
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrInUse) || errors.Is(err, ErrVolumeAttached) {
			t.Errorf("%s: %v matches more than one kind", tc.what, err)
		}
	}
}

// A partial unique index refuses a write the same way a column constraint
// does: sandbox names are unique among live sandboxes only.
func TestClassifyUniqueIndexViolationIsConflict(t *testing.T) {
	s := testStore(t)
	createTestSandbox(t, s, "usr_a", "sb1", "dev")
	err := s.CreateSandbox(Sandbox{ID: "sb2", Name: "dev", Status: "running", CreatedBy: "usr_a", CreatedAt: time.Now()})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("a second live sandbox named dev: %v, want ErrConflict", err)
	}
}

// Only a taken key is a conflict. Other constraints, a failing query and
// errors from outside the driver come back as they were, so a caller never
// reads a real failure as "already exists" or "not found".
func TestClassifyLeavesOtherErrorsAlone(t *testing.T) {
	s := testStore(t)
	_, notNull := s.db.Exec(`INSERT INTO users (id, name, api_key_hash) VALUES ('n', NULL, 'h')`)
	_, noTable := s.db.Exec(`SELECT * FROM no_such_table`)
	other := errors.New("not from the driver")
	for what, err := range map[string]error{
		"NOT NULL violation": notNull,
		"missing table":      noTable,
		"foreign error":      other,
	} {
		if err == nil {
			t.Fatalf("%s: the setup did not fail", what)
		}
		got := classify(err)
		if got != err {
			t.Errorf("%s: classify changed %v into %v", what, err, got)
		}
		if errors.Is(got, ErrConflict) || errors.Is(got, ErrNotFound) {
			t.Errorf("%s: %v was given a kind", what, got)
		}
	}
	if classify(nil) != nil {
		t.Error("classify(nil) is an error")
	}
}

func TestClassifyEmptyResultIsNotFound(t *testing.T) {
	s := testStore(t)
	var id string
	err := classify(s.db.QueryRow(`SELECT id FROM users WHERE id = 'nobody'`).Scan(&id))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("a query with no row: %v, want ErrNotFound", err)
	}
}

// A driver error that something wrapped on the way up is still recognised.
func TestClassifyThroughWrapping(t *testing.T) {
	s := testStore(t)
	createTestUser(t, s, "usr_a", "alice")
	_, err := s.db.Exec(`INSERT INTO users (id, name, api_key_hash) VALUES ('x', 'alice', 'other')`)
	err = fmt.Errorf("create user: %w", err)
	if !errors.Is(classify(err), ErrConflict) {
		t.Fatalf("a wrapped unique violation: %v, want ErrConflict", classify(err))
	}
}

// ErrNotFound means the row is absent. A lookup that could not run is a
// different failure and must not read as one.
func TestFailedLookupIsNotNotFound(t *testing.T) {
	s := testStore(t)
	createTestUser(t, s, "usr_a", "alice")
	createTestVolume(t, s, "usr_a", "ws", 64)
	if _, err := s.db.Exec(`DROP TABLE volumes_v2`); err != nil {
		t.Fatal(err)
	}
	lookups := map[string]func() error{
		"GetPersistentVolume":    func() error { _, err := s.GetPersistentVolume("usr_a", "ws"); return err },
		"DeletePersistentVolume": func() error { return s.DeletePersistentVolume("usr_a", "ws") },
		"AttachPersistentVolume": func() error { return s.AttachPersistentVolume("usr_a", "ws", "sb1", "/ws", false) },
		"DetachPersistentVolume": func() error { return s.DetachPersistentVolume("usr_a", "ws", "sb1") },
	}
	for name, call := range lookups {
		err := call()
		if err == nil {
			t.Errorf("%s: no error although the table is gone", name)
		} else if errors.Is(err, ErrNotFound) {
			t.Errorf("%s: a failed query was reported as ErrNotFound: %v", name, err)
		}
	}
	if _, err := s.db.Exec(`DROP TABLE images`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetImage("usr_a", "base"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("GetImage with its table gone: %v, want a failure that is not ErrNotFound", err)
	}
}

// An owner's lookups never find another owner's rows, and say the same thing
// they say about a row that does not exist.
func TestLookupsAreOwnerScopedNotFound(t *testing.T) {
	s := testStore(t)
	createTestUser(t, s, "usr_a", "alice")
	createTestUser(t, s, "usr_b", "bob")
	createTestSandbox(t, s, "usr_a", "sb1", "dev")
	createTestVolume(t, s, "usr_a", "ws", 64)
	if err := s.SetSecret("usr_a", "TOKEN", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSnapshot(snapshotFixture("s1", "usr_a", "snap")); err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func() error{
		"GetSandbox":             func() error { _, err := s.GetSandbox("usr_b", "dev"); return err },
		"GetPersistentVolume":    func() error { _, err := s.GetPersistentVolume("usr_b", "ws"); return err },
		"DeletePersistentVolume": func() error { return s.DeletePersistentVolume("usr_b", "ws") },
		"GetSecretValue":         func() error { _, err := s.GetSecretValue("usr_b", "TOKEN"); return err },
		"GetSecret":              func() error { _, err := s.GetSecret("usr_b", "TOKEN"); return err },
		"DeleteSecret":           func() error { return s.DeleteSecret("usr_b", "TOKEN") },
		"GetSnapshot":            func() error { _, err := s.GetSnapshot("usr_b", "snap"); return err },
		"DeleteSnapshot":         func() error { return s.DeleteSnapshot("usr_b", "snap") },
		"RenameSandbox":          func() error { return s.RenameSandbox("usr_b", "sb1", "mine") },
		"DeleteSandbox":          func() error { return s.DeleteSandbox("usr_b", "sb1") },
		"UpdateSandboxLabels":    func() error { return s.UpdateSandboxLabels("usr_b", "sb1", map[string]string{"k": "v"}, nil) },
		"GetUser (absent)":       func() error { _, err := s.GetUser("nobody"); return err },
		"GetUserByName (absent)": func() error { _, err := s.GetUserByName("nobody"); return err },
	} {
		if err := call(); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v, want ErrNotFound", name, err)
		}
	}
}

// ErrInUse: a legacy volume a sandbox still mounts cannot be deleted; one that
// is not there is ErrNotFound, not in use.
func TestDeleteLegacyVolumeInUse(t *testing.T) {
	s := testStore(t)
	if err := s.CreateVolume("data"); err != nil {
		t.Fatal(err)
	}
	if err := s.AttachVolume("sb1", "data", "/data", false); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteVolume("data"); !errors.Is(err, ErrInUse) {
		t.Fatalf("delete while a sandbox mounts it: %v, want ErrInUse", err)
	}
	if err := s.DeleteVolume("nothing"); !errors.Is(err, ErrNotFound) || errors.Is(err, ErrInUse) {
		t.Fatalf("delete of a volume that is not there: %v, want ErrNotFound only", err)
	}
	if err := s.DetachVolumes("sb1"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteVolume("data"); err != nil {
		t.Fatalf("delete once nothing mounts it: %v", err)
	}
}

// Any key can be a volume's name, including the words an error message about
// volumes happens to contain. The server used to tell "not found" from
// "attached" by looking for the word in the message, so a missing volume named
// "attachment" came back as a conflict whose body said "not found". The kind
// of the error, never its text, says which it is.
func TestVolumeNameIsNeverMistakenForAKind(t *testing.T) {
	for _, name := range []string{"attachment", "not-found", "already", "UNIQUE", "in-use"} {
		t.Run(name, func(t *testing.T) {
			s := testStore(t)
			createTestUser(t, s, "usr_a", "alice")
			createTestSandbox(t, s, "usr_a", "sb1", "dev")

			err := s.DeletePersistentVolume("usr_a", name)
			if !errors.Is(err, ErrNotFound) || errors.Is(err, ErrVolumeAttached) || errors.Is(err, ErrConflict) {
				t.Fatalf("delete of a volume that is not there: %v, want ErrNotFound only", err)
			}

			createTestVolume(t, s, "usr_a", name, 64)
			if err := s.AttachPersistentVolume("usr_a", name, "sb1", "/data", false); err != nil {
				t.Fatal(err)
			}
			err = s.DeletePersistentVolume("usr_a", name)
			if !errors.Is(err, ErrVolumeAttached) || errors.Is(err, ErrNotFound) {
				t.Fatalf("delete while attached: %v, want ErrVolumeAttached only", err)
			}

			if err := s.DetachPersistentVolume("usr_a", name, "sb1"); err != nil {
				t.Fatal(err)
			}
			if err := s.DeletePersistentVolume("usr_a", name); err != nil {
				t.Fatalf("delete once detached: %v", err)
			}
			if _, err := s.GetPersistentVolume("usr_a", name); !errors.Is(err, ErrNotFound) {
				t.Fatalf("the volume outlived its delete: %v", err)
			}
		})
	}
}

// What the driver reports when no row matched is not part of the store's
// contract: the store's own kind is.
func TestNotFoundDoesNotLeakTheDriversSentinel(t *testing.T) {
	s := testStore(t)
	_, err := s.GetSandboxByID("nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetSandboxByID: %v, want ErrNotFound", err)
	}
	if errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetSandboxByID leaks database/sql's sentinel: %v", err)
	}
}
