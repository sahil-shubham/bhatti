package store

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// What a daemon from before clones shared placeholders left on disk:
// secret_grants with UNIQUE on placeholder alone, and snapshots without the
// source-sandbox columns.
const (
	legacySecretGrants = `CREATE TABLE secret_grants (
		id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL,
		secret_name TEXT NOT NULL,
		sandbox_id TEXT NOT NULL,
		hosts_json TEXT NOT NULL DEFAULT '[]',
		placeholder TEXT NOT NULL UNIQUE,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		expires_at DATETIME,
		revoked_at DATETIME
	)`
	legacySnapshots = `CREATE TABLE snapshots (
		id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL,
		name TEXT NOT NULL,
		source_sandbox TEXT NOT NULL,
		mem_path TEXT NOT NULL,
		vm_path TEXT NOT NULL,
		rootfs_path TEXT NOT NULL,
		config_path TEXT NOT NULL,
		manifest_json TEXT NOT NULL,
		size_mb INTEGER NOT NULL DEFAULT 0,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(user_id, name)
	)`
)

func legacyGrantRows(t *testing.T, db *sql.DB) {
	t.Helper()
	expires := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	revoked := time.Date(2026, 10, 4, 13, 0, 0, 0, time.UTC)
	for _, g := range []SecretGrant{
		{ID: "g1", UserID: "alice", SecretName: "GH_TOKEN", SandboxID: "sb1", Hosts: []string{"api.github.com"}, Placeholder: "ph1", ExpiresAt: &expires},
		{ID: "g2", UserID: "alice", SecretName: "API", SandboxID: "sb2", Hosts: []string{"a.example", "*.b.example"}, Placeholder: "ph2", RevokedAt: &revoked},
	} {
		hosts, err := json.Marshal(g.Hosts)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO secret_grants (id, user_id, secret_name, sandbox_id, hosts_json, placeholder, created_at, expires_at, revoked_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			g.ID, g.UserID, g.SecretName, g.SandboxID, string(hosts), g.Placeholder,
			time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC), utcGrantTime(g.ExpiresAt), utcGrantTime(g.RevokedAt)); err != nil {
			t.Fatal(err)
		}
	}
}

// TestMigrateGrantPlaceholderScope: a table with the table-wide UNIQUE is
// rebuilt with the per-sandbox one, every row intact; afterwards the same
// placeholder can be granted to another sandbox (a clone) but not twice to
// one, and running it again changes nothing.
func TestMigrateGrantPlaceholderScope(t *testing.T) {
	db := openRaw(t)
	if _, err := db.Exec(legacySecretGrants); err != nil {
		t.Fatal(err)
	}
	legacyGrantRows(t, db)

	if ran, err := migrateGrantPlaceholderScope(db); err != nil || !ran {
		t.Fatalf("first run: ran=%v err=%v, want the rebuild", ran, err)
	}
	if ran, err := migrateGrantPlaceholderScope(db); err != nil || ran {
		t.Fatalf("second run: ran=%v err=%v, want a no-op", ran, err)
	}

	rows, err := db.Query(`SELECT ` + grantCols + ` FROM secret_grants ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got, err := scanSecretGrants(rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "g1" || got[0].Placeholder != "ph1" || got[0].ExpiresAt == nil ||
		!got[0].ExpiresAt.Equal(time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)) ||
		got[1].ID != "g2" || got[1].RevokedAt == nil || !reflect.DeepEqual(got[1].Hosts, []string{"a.example", "*.b.example"}) {
		t.Fatalf("rows after the rebuild: %+v", got)
	}

	insert := func(id, sandbox, placeholder string) error {
		_, err := db.Exec(`INSERT INTO secret_grants (id, user_id, secret_name, sandbox_id, placeholder) VALUES (?, 'alice', 'GH_TOKEN', ?, ?)`, id, sandbox, placeholder)
		return err
	}
	if err := insert("g3", "sb-clone", "ph1"); err != nil {
		t.Fatalf("the same placeholder for another sandbox: %v", err)
	}
	if err := insert("g4", "sb1", "ph1"); err == nil {
		t.Fatal("a sandbox was granted the same placeholder twice")
	}
}

// TestMigrateGrantPlaceholderScopeFreshDatabase: before the table exists
// there is nothing to rebuild.
func TestMigrateGrantPlaceholderScopeFreshDatabase(t *testing.T) {
	db := openRaw(t)
	if ran, err := migrateGrantPlaceholderScope(db); err != nil || ran {
		t.Fatalf("no table: ran=%v err=%v, want a no-op", ran, err)
	}
}

// TestOpenUpgradesAnOlderDatabase: New on a database from before clones
// shared placeholders keeps its grants and snapshots, lets a clone be granted
// its source's placeholder, keeps the indexes the rebuild dropped, and opens
// again without touching anything.
func TestOpenUpgradesAnOlderDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		legacySecretGrants,
		`CREATE INDEX idx_secret_grants_sandbox ON secret_grants(sandbox_id)`,
		legacySnapshots,
		`INSERT INTO snapshots (id, user_id, name, source_sandbox, mem_path, vm_path, rootfs_path, config_path, manifest_json, size_mb)
		 VALUES ('s1', 'alice', 'base', 'sb1', '/x/base/mem.snap', '', '', '', '{"vcpus":2}', 7)`,
	} {
		if _, err := old.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	legacyGrantRows(t, old)
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	for pass := range 2 {
		s, err := New(path)
		if err != nil {
			t.Fatalf("open %d: %v", pass, err)
		}
		if g, err := s.GetSandboxSecretGrantByPlaceholder("sb1", "ph1"); err != nil || g.ID != "g1" {
			t.Fatalf("open %d: grant lost: %+v, %v", pass, g, err)
		}
		if pass == 0 {
			putGrant(t, s, grantFixture("g3", "alice", "GH_TOKEN", "sb-clone", "ph1", time.Now()))
		}
		if g, err := s.GetSandboxSecretGrantByPlaceholder("sb-clone", "ph1"); err != nil || g.ID != "g3" {
			t.Fatalf("open %d: the clone's grant: %+v, %v", pass, g, err)
		}
		for _, idx := range []string{"idx_secret_grants_sandbox", "idx_secret_grants_user_created", "idx_secret_grants_placeholder"} {
			var n int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, idx).Scan(&n); err != nil || n != 1 {
				t.Fatalf("open %d: index %s: n=%d err=%v", pass, idx, n, err)
			}
		}
		// All its indexes are in place now, the placeholder one included: none
		// of them may read as the old table-wide constraint and rebuild it on
		// every start.
		if ran, err := migrateGrantPlaceholderScope(s.db); err != nil || ran {
			t.Fatalf("open %d: the rebuilt table was rebuilt again: ran=%v err=%v", pass, ran, err)
		}
		// An older snapshot recorded nothing — and, unlike one taken since that
		// kept nothing, says so: reopening never gives it a record.
		snap, err := s.GetSnapshot("alice", "base")
		if err != nil || snap.SizeMB != 7 || snap.SourceRecordedAt != nil || snap.Source != (SnapshotSource{}) ||
			snap.Substitution.CA != nil || len(snap.Substitution.Grants) != 0 {
			t.Fatalf("open %d: an older snapshot: %+v, %v", pass, snap, err)
		}
		if pass == 0 {
			if err := s.CreateSnapshot(snapshotFixture("s2", "alice", "newer")); err != nil {
				t.Fatal(err)
			}
		}
		if newer, err := s.GetSnapshot("alice", "newer"); err != nil || newer.SourceRecordedAt == nil {
			t.Fatalf("open %d: a snapshot taken after the upgrade: %+v, %v", pass, newer, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
