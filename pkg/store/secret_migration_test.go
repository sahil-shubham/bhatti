package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// Regression tests for the v1 → v2 secrets schema migration.
// Tranche 0a item #6 of PLAN-bhatti-v2.md.
//
// The bug: the migration sequence (CREATE secrets_v2, INSERT from
// secrets, DROP secrets, RENAME secrets_v2 → secrets) ran on every
// startup. After the first boot the table was already v2, but the
// rewrite happened anyway: re-copying every secret, dropping the
// real table, renaming the just-populated v2 back to it. Functionally
// idempotent in the happy case, wasteful in the steady case, and
// risky in the crash case (a crash between DROP and RENAME leaves
// the database with no `secrets` table until the next boot reruns
// the dance).
//
// Fix: migrateSecretsToV2 checks pragma_table_info for user_id being
// in the primary key (the v2 marker) and short-circuits when it is.

// openRaw opens a fresh SQLite database without going through store.New,
// so the migration helper can be exercised against a controlled schema.
func openRaw(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "raw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// createV1Secrets installs the post-ALTER v1 secrets schema:
// PRIMARY KEY(name), plus the columns added by the additive
// migrations in store.go's `migrations` const (value_encrypted,
// updated_at, user_id). This is the state migrateSecretsToV2
// actually sees in production — the additive migrations always run
// before the v1→v2 rewrite, so the rewrite assumes those columns
// exist.
func createV1Secrets(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`CREATE TABLE secrets (
		name TEXT PRIMARY KEY,
		path TEXT NOT NULL,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		value_encrypted BLOB DEFAULT NULL,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		user_id TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		t.Fatal(err)
	}
}

// TestMigrateSecretsToV2_FromV1 verifies a freshly-created v1 secrets
// table gets migrated on the first call.
func TestMigrateSecretsToV2_FromV1(t *testing.T) {
	db := openRaw(t)
	createV1Secrets(t, db)

	didMigrate, err := migrateSecretsToV2(db)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !didMigrate {
		t.Fatal("expected didMigrate=true on first call against v1 schema")
	}

	// Post-migration, user_id should be in the primary key.
	var pkCount int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('secrets') WHERE name='user_id' AND pk > 0`,
	).Scan(&pkCount); err != nil {
		t.Fatal(err)
	}
	if pkCount != 1 {
		t.Fatalf("user_id not in primary key after migration; pkCount=%d", pkCount)
	}
}

// TestMigrateSecretsToV2_Idempotent is the regression test for the
// bug: a second call must be a no-op (didMigrate=false). Pre-fix this
// returned true on every call because the migration ran unconditionally.
func TestMigrateSecretsToV2_Idempotent(t *testing.T) {
	db := openRaw(t)
	createV1Secrets(t, db)

	// First call: v1 → v2.
	if didMigrate, err := migrateSecretsToV2(db); err != nil || !didMigrate {
		t.Fatalf("first call: didMigrate=%v err=%v", didMigrate, err)
	}

	// Second call: must be a no-op.
	didMigrate, err := migrateSecretsToV2(db)
	if err != nil {
		t.Fatalf("second call err: %v", err)
	}
	if didMigrate {
		t.Fatal("expected didMigrate=false on second call (already v2); the rewrite is running unconditionally")
	}

	// Third call for good measure.
	if didMigrate, err := migrateSecretsToV2(db); err != nil || didMigrate {
		t.Fatalf("third call: didMigrate=%v err=%v (expected false, nil)", didMigrate, err)
	}
}

// TestMigrateSecretsToV2_PreservesData inserts v1 data, migrates,
// then verifies the data survives intact (including timestamps).
// Catches a regression where the migration loses or rewrites data.
func TestMigrateSecretsToV2_PreservesData(t *testing.T) {
	db := openRaw(t)
	createV1Secrets(t, db)

	if _, err := db.Exec(
		`INSERT INTO secrets (name, path, created_at) VALUES (?, ?, ?)`,
		"API_KEY", "/run/secrets/api_key", "2024-01-15T10:30:00Z",
	); err != nil {
		t.Fatal(err)
	}

	if _, err := migrateSecretsToV2(db); err != nil {
		t.Fatal(err)
	}

	var (
		userID, name, path, createdAt string
	)
	if err := db.QueryRow(
		`SELECT user_id, name, path, created_at FROM secrets WHERE name=?`, "API_KEY",
	).Scan(&userID, &name, &path, &createdAt); err != nil {
		t.Fatalf("read post-migration: %v", err)
	}
	if userID != "" || name != "API_KEY" || path != "/run/secrets/api_key" {
		t.Fatalf("data corrupted: user_id=%q name=%q path=%q", userID, name, path)
	}
	if createdAt != "2024-01-15T10:30:00Z" {
		t.Fatalf("created_at not preserved: %q", createdAt)
	}
}

// TestMigrateSecretsToV2_FullStorePath exercises the production path:
// open a Store (which runs the migration via New), close, reopen.
// Asserts that the second New invocation does not rewrite the table.
// We detect this indirectly by adding a marker (an extra index on
// the v2 secrets table) after the first boot and verifying it
// survives the second boot — a CREATE-INSERT-DROP-RENAME rewrite
// would lose the marker along with the dropped table.
func TestMigrateSecretsToV2_FullStorePath(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "store.db")

	// First boot — migration runs.
	s1, err := New(dbPath)
	if err != nil {
		t.Fatalf("first New: %v", err)
	}

	// Add a marker that would not survive a CREATE-INSERT-DROP-RENAME
	// rewrite (a non-essential index on the v2 table).
	if _, err := s1.db.Exec(
		`CREATE INDEX idx_marker_test ON secrets(path)`,
	); err != nil {
		t.Fatalf("create marker index: %v", err)
	}
	s1.Close()

	// Second boot — if the migration re-runs, it drops `secrets` and
	// renames `secrets_v2`. The marker index would be lost.
	s2, err := New(dbPath)
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	defer s2.Close()

	var markerCount int
	if err := s2.db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_marker_test'`,
	).Scan(&markerCount); err != nil {
		t.Fatal(err)
	}
	if markerCount != 1 {
		t.Fatal("marker index lost across boot — secrets migration ran a second time and rewrote the table")
	}
}
