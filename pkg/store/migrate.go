package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// migration takes the schema from version i to i+1, where i is its index in
// the ladder it belongs to.
type migration struct {
	name string // in the error when the step fails
	run  func(ctx context.Context, tx *sql.Tx) error
}

// migrations is the schema's history, oldest first. PRAGMA user_version is how
// many of its steps a database has run, and New runs the ones it lacks, in
// order, each in a transaction of its own that also records the new version.
// A database is therefore always at a version some step left it at, and a step
// that fails stops the boot with its number in the error rather than leaving a
// daemon running on a schema nobody can name.
//
// Append only: a step that has shipped has already run on databases in the
// field, so editing it changes none of them. A step may assume the schema the
// one before it left, and nothing older. The first three may not. Until the
// ladder nothing recorded a version, so a database from any earlier release
// reads as version 0 whatever shape that release left it in, and those steps
// bring every such shape to the same one. testdata/olddb holds a dump of a
// database of each distinct shape, and the tests replay them.
var migrations = []migration{
	{"tables and columns", migrateTablesAndColumns},
	{"secrets keyed by user", migrateSecretsKey},
	{"secret grants unique per sandbox", migrateSecretGrants},
	{"events cursor index", migrateEventsCursorIndex},
	{"sandbox events owned", migrateSandboxEventOwners},
}

// errSchemaNewer marks a database that a newer build has migrated past the
// last step this one has.
var errSchemaNewer = errors.New("database schema is newer than this build")

// rowQuerier is what *sql.DB and *sql.Tx share that reading the version needs.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// schemaVersion is the version db is at, or an error if this build, whose
// ladder ends at known, can't take it from there: a version past known was
// written by a newer build, whose changes this one would run blind on.
func schemaVersion(ctx context.Context, db rowQuerier, known int) (int, error) {
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return 0, fmt.Errorf("store: read schema version: %w", err)
	}
	switch {
	case version < 0:
		return 0, fmt.Errorf("store: database schema version %d is not valid", version)
	case version > known:
		return 0, wrap(errSchemaNewer, "store: database is at schema version %d but this build only goes up to %d: "+
			"run the newer bhatti that migrated it, or restore a backup taken before the upgrade", version, known)
	}
	return version, nil
}

// migrate runs the steps of ladder that db has not.
func migrate(ctx context.Context, db *sql.DB, ladder []migration) error {
	// Read outside a transaction first: BEGIN IMMEDIATE takes the write lock,
	// and a database that is up to date (every boot after the first, and every
	// admin command run beside a live daemon) must not queue for it.
	version, err := schemaVersion(ctx, db, len(ladder))
	if err != nil {
		return err
	}
	for version < len(ladder) {
		if version, err = applyNext(ctx, db, ladder); err != nil {
			return err
		}
	}
	return nil
}

// applyNext runs the step after the version it finds db at and returns the
// version db is at afterwards.
//
// It reads that version inside its own transaction. The DSN's
// _txlock=immediate takes the write lock at BEGIN, so a second opener (the
// daemon booting while a `bhatti user` command opens the same file) waits
// there, and the version it reads on getting in is the one the first left. It
// skips the steps already run where the version it read before waiting would
// run them twice, and a later step is free to assume it runs once.
func applyNext(ctx context.Context, db *sql.DB, ladder []migration) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin migration: %w", err)
	}
	defer tx.Rollback()
	version, err := schemaVersion(ctx, tx, len(ladder))
	if err != nil {
		return 0, err
	}
	if version == len(ladder) {
		return version, nil
	}
	step := ladder[version]
	label := fmt.Sprintf("store: migration %d (%s)", version+1, step.name)
	if err := step.run(ctx, tx); err != nil {
		return 0, fmt.Errorf("%s: %w", label, err)
	}
	// A PRAGMA takes no bound parameters; version is an int computed here.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, version+1)); err != nil {
		return 0, fmt.Errorf("%s: record version: %w", label, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("%s: commit: %w", label, err)
	}
	return version + 1, nil
}

// execAll runs stmts in order and names the one that fails.
func execAll(ctx context.Context, tx *sql.Tx, stmts []string) error {
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("%s: %w", statementName(stmt), err)
		}
	}
	return nil
}

// statementName is the first line of stmt ("CREATE TABLE IF NOT EXISTS
// events"): enough to say which statement of a list failed.
func statementName(stmt string) string {
	head, _, _ := strings.Cut(strings.TrimSpace(stmt), "\n")
	return strings.TrimSpace(strings.TrimSuffix(head, "("))
}

// addedColumn is a column its table gained after the release that created the
// table: a database from before has the table without it.
type addedColumn struct {
	table, column, definition string
}

// addMissingColumns adds the columns that cols names and the database lacks.
// SQLite has no ADD COLUMN IF NOT EXISTS, and the error for a column that is
// already there is only driver text, so the step asks pragma_table_info which
// are missing instead of trying them all and forgiving one message.
func addMissingColumns(ctx context.Context, tx *sql.Tx, cols []addedColumn) error {
	for _, c := range cols {
		var present int
		// Column names are case-insensitive, and ADD COLUMN refuses a
		// duplicate under any casing.
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ? COLLATE NOCASE`, c.table, c.column,
		).Scan(&present); err != nil {
			return fmt.Errorf("inspect %s.%s: %w", c.table, c.column, err)
		}
		if present > 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `ALTER TABLE `+c.table+` ADD COLUMN `+c.column+` `+c.definition); err != nil {
			return fmt.Errorf("add %s.%s: %w", c.table, c.column, err)
		}
	}
	return nil
}

// originalTables is what the first release created. The secrets table here is
// the v1 shape, which migrateSecretsToV2 replaces.
const originalTables = `
CREATE TABLE IF NOT EXISTS templates (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	engine TEXT NOT NULL DEFAULT 'docker',
	image TEXT NOT NULL,
	cpus REAL NOT NULL DEFAULT 1,
	memory_mb INTEGER NOT NULL DEFAULT 512,
	disk_size_mb INTEGER NOT NULL DEFAULT 0,
	userdata TEXT NOT NULL DEFAULT '',
	secrets_json TEXT NOT NULL DEFAULT '[]',
	labels_json TEXT NOT NULL DEFAULT '{}',
	mounts_json TEXT NOT NULL DEFAULT '[]',
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS volumes (
	name TEXT PRIMARY KEY,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS sandbox_volumes (
	sandbox_id TEXT NOT NULL,
	volume_name TEXT NOT NULL,
	target TEXT NOT NULL,
	readonly INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (sandbox_id, volume_name)
);

CREATE TABLE IF NOT EXISTS sandboxes (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	template_id TEXT NOT NULL DEFAULT '',
	engine_id TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'unknown',
	ip TEXT NOT NULL DEFAULT '',
	engine_meta_json TEXT NOT NULL DEFAULT '{}',
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	stopped_at DATETIME
);

CREATE TABLE IF NOT EXISTS secrets (
	name TEXT PRIMARY KEY,
	path TEXT NOT NULL,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS users (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	api_key_hash TEXT NOT NULL UNIQUE,
	max_sandboxes INTEGER NOT NULL DEFAULT 5,
	max_cpus_per_sandbox INTEGER NOT NULL DEFAULT 4,
	max_memory_mb_per_sandbox INTEGER NOT NULL DEFAULT 4096,
	subnet_index INTEGER NOT NULL DEFAULT 0,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
`

// originalColumns are the columns the original tables gained, in the order
// they were added, which is the order a new database's columns come in. The
// secrets ones are what migrateSecretsToV2 copies from.
var originalColumns = []addedColumn{
	{"templates", "mounts_json", `TEXT NOT NULL DEFAULT '[]'`},
	{"sandboxes", "rootfs_path", `TEXT DEFAULT ''`},
	{"sandboxes", "snap_mem_path", `TEXT DEFAULT ''`},
	{"sandboxes", "snap_vm_path", `TEXT DEFAULT ''`},
	{"sandboxes", "vsock_cid", `INTEGER DEFAULT 0`},
	{"sandboxes", "tap_device", `TEXT DEFAULT ''`},
	{"sandboxes", "guest_ip", `TEXT DEFAULT ''`},
	{"sandboxes", "guest_mac", `TEXT DEFAULT ''`},
	{"sandboxes", "vcpu_count", `REAL DEFAULT 1`},
	{"sandboxes", "mem_size_mib", `INTEGER DEFAULT 512`},
	{"sandboxes", "socket_path", `TEXT DEFAULT ''`},
	{"sandboxes", "vsock_path", `TEXT DEFAULT ''`},
	{"secrets", "value_encrypted", `BLOB DEFAULT NULL`},
	{"secrets", "updated_at", `DATETIME DEFAULT NULL`},
	{"sandboxes", "created_by", `TEXT NOT NULL DEFAULT ''`},
	{"secrets", "user_id", `TEXT NOT NULL DEFAULT ''`},
	{"sandboxes", "agent_token", `TEXT DEFAULT ''`},
	{"sandboxes", "has_base_snapshot", `INTEGER DEFAULT 0`},
	{"users", "max_volume_storage_mb", `INTEGER NOT NULL DEFAULT 20480`},
	{"users", "max_images", `INTEGER NOT NULL DEFAULT 10`},
	{"users", "max_snapshots", `INTEGER NOT NULL DEFAULT 5`},
	{"sandboxes", "keep_hot", `INTEGER NOT NULL DEFAULT 0`},
	{"sandboxes", "fc_path_origin", `TEXT DEFAULT ''`},
	{"sandboxes", "shell_token_hash", `TEXT DEFAULT ''`},
	{"sandboxes", "cpus", `REAL NOT NULL DEFAULT 1`},
	{"sandboxes", "memory_mb", `INTEGER NOT NULL DEFAULT 1024`},
	{"sandboxes", "disk_size_mb", `INTEGER NOT NULL DEFAULT 0`},
	{"sandboxes", "image", `TEXT NOT NULL DEFAULT 'minimal'`},
	{"sandboxes", "labels", `TEXT NOT NULL DEFAULT '{}'`},
	{"sandboxes", "net_policy", `TEXT NOT NULL DEFAULT ''`},
	{"sandboxes", "has_init", `INTEGER NOT NULL DEFAULT 0`},
	{"sandboxes", "restart_pending", `INTEGER NOT NULL DEFAULT 0`},
}

// laterTables are the tables, and their indexes, that later releases created
// (all but secret_grants, whose step converts an older shape first). A
// database from before has some of them, which part depending on its release.
var laterTables = []string{
	`CREATE TABLE IF NOT EXISTS volumes_v2 (
		id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL,
		name TEXT NOT NULL,
		size_mb INTEGER NOT NULL,
		file_path TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'ready',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(user_id, name)
	)`,
	`CREATE TABLE IF NOT EXISTS volume_attachments (
		volume_id TEXT NOT NULL,
		sandbox_id TEXT NOT NULL,
		mount TEXT NOT NULL,
		read_only INTEGER NOT NULL DEFAULT 0,
		attached_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (volume_id, sandbox_id)
	)`,
	`CREATE TABLE IF NOT EXISTS images (
		id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL DEFAULT '',
		name TEXT NOT NULL,
		source TEXT NOT NULL DEFAULT '',
		file_path TEXT NOT NULL,
		size_mb INTEGER NOT NULL DEFAULT 0,
		oci_digest TEXT NOT NULL DEFAULT '',
		oci_config_json TEXT NOT NULL DEFAULT '{}',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(user_id, name)
	)`,
	`CREATE TABLE IF NOT EXISTS snapshots (
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
		source_image TEXT NOT NULL DEFAULT '',
		source_disk_size_mb INTEGER NOT NULL DEFAULT 0,
		source_has_init INTEGER NOT NULL DEFAULT 0,
		source_recorded_at DATETIME,
		UNIQUE(user_id, name)
	)`,
	`CREATE TABLE IF NOT EXISTS tasks (
		id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL,
		type TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'running',
		progress TEXT NOT NULL DEFAULT '',
		result_json TEXT NOT NULL DEFAULT '{}',
		error TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		completed_at DATETIME
	)`,
	`CREATE INDEX IF NOT EXISTS idx_tasks_created_at ON tasks(created_at)`,

	`CREATE TABLE IF NOT EXISTS publish_rules (
		id TEXT PRIMARY KEY,
		sandbox_id TEXT NOT NULL,
		user_id TEXT NOT NULL,
		port INTEGER NOT NULL,
		alias TEXT NOT NULL UNIQUE,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(sandbox_id, port)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_publish_rules_sandbox ON publish_rules(sandbox_id)`,

	`CREATE TABLE IF NOT EXISTS volume_backups (
		id TEXT PRIMARY KEY,
		volume_name TEXT NOT NULL,
		user_id TEXT NOT NULL,
		s3_key TEXT NOT NULL,
		size_bytes INTEGER NOT NULL,
		sha256 TEXT NOT NULL DEFAULT '',
		consistency_mode TEXT NOT NULL DEFAULT 'unverified',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,
	`CREATE INDEX IF NOT EXISTS idx_volume_backups_name ON volume_backups(user_id, volume_name, created_at DESC)`,

	// Prevents a user from having two sandboxes of the same name. Needs
	// sandboxes.created_by, so it follows originalColumns.
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_sandboxes_user_name
		ON sandboxes(created_by, name) WHERE status != 'destroyed'`,

	`CREATE TABLE IF NOT EXISTS image_shares (
		image_id TEXT NOT NULL,
		user_id TEXT NOT NULL,
		PRIMARY KEY (image_id, user_id)
	)`,

	`CREATE TABLE IF NOT EXISTS events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ts TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
		type TEXT NOT NULL,
		user_id TEXT NOT NULL DEFAULT '',
		sandbox_id TEXT NOT NULL DEFAULT '',
		meta TEXT NOT NULL DEFAULT '{}'
	)`,
	`CREATE INDEX IF NOT EXISTS idx_events_ts ON events(ts)`,
	`CREATE INDEX IF NOT EXISTS idx_events_type ON events(type, ts)`,
	`CREATE INDEX IF NOT EXISTS idx_events_user ON events(user_id, ts)`,

	`CREATE TABLE IF NOT EXISTS metrics_snapshots (
		ts TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
		api_requests INTEGER NOT NULL DEFAULT 0,
		api_errors INTEGER NOT NULL DEFAULT 0,
		api_auth_failures INTEGER NOT NULL DEFAULT 0,
		api_rate_limited INTEGER NOT NULL DEFAULT 0,
		proxy_requests INTEGER NOT NULL DEFAULT 0,
		proxy_errors INTEGER NOT NULL DEFAULT 0,
		proxy_cold_wakes INTEGER NOT NULL DEFAULT 0,
		proxy_rate_limited INTEGER NOT NULL DEFAULT 0,
		events_dropped INTEGER NOT NULL DEFAULT 0,
		sandboxes_total INTEGER NOT NULL DEFAULT 0,
		sandboxes_hot INTEGER NOT NULL DEFAULT 0,
		sandboxes_warm INTEGER NOT NULL DEFAULT 0,
		sandboxes_cold INTEGER NOT NULL DEFAULT 0,
		users_total INTEGER NOT NULL DEFAULT 0,
		users_active INTEGER NOT NULL DEFAULT 0,
		websockets_active INTEGER NOT NULL DEFAULT 0,
		host_load_1m REAL NOT NULL DEFAULT 0,
		host_mem_total_mb INTEGER NOT NULL DEFAULT 0,
		host_mem_avail_mb INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS idx_ms_ts ON metrics_snapshots(ts)`,

	// What a snapshot keeps of its source's credential substitution, so a
	// resume can give its sandbox the same after the source is gone.
	`CREATE TABLE IF NOT EXISTS snapshot_cas (
		snapshot_id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL,
		cert_pem TEXT NOT NULL,
		key_enc BLOB NOT NULL,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,
	`CREATE TABLE IF NOT EXISTS snapshot_grants (
		snapshot_id TEXT NOT NULL,
		user_id TEXT NOT NULL,
		secret_name TEXT NOT NULL,
		hosts_json TEXT NOT NULL DEFAULT '[]',
		placeholder TEXT NOT NULL,
		expires_at DATETIME,
		PRIMARY KEY (snapshot_id, placeholder)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_snapshot_grants_secret ON snapshot_grants(user_id, secret_name)`,
	`CREATE TABLE IF NOT EXISTS sandbox_cas (
		sandbox_id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL,
		cert_pem TEXT NOT NULL,
		key_enc BLOB NOT NULL,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,
}

// laterColumns are the columns that tables in laterTables gained after their
// CREATE, each also in that CREATE so a new database has it from the start.
// The two definitions must agree; the tests compare a new database with
// upgraded ones.
var laterColumns = []addedColumn{
	{"volume_backups", "consistency_mode", `TEXT NOT NULL DEFAULT 'unverified'`},
	// Snapshots from before this recorded nothing of their source. The
	// source_* columns' defaults can't say that apart from a snapshot that
	// recorded an empty source, so source_recorded_at is NULL for the rows
	// that were already there.
	{"snapshots", "source_image", `TEXT NOT NULL DEFAULT ''`},
	{"snapshots", "source_disk_size_mb", `INTEGER NOT NULL DEFAULT 0`},
	{"snapshots", "source_has_init", `INTEGER NOT NULL DEFAULT 0`},
	{"snapshots", "source_recorded_at", `DATETIME`},
}

// migrateTablesAndColumns brings a database of any earlier release to the
// schema they all share: every table, column and index except the ones that
// first need their rows converted, which the steps after it handle. Each
// statement is CREATE ... IF NOT EXISTS or guarded by pragma_table_info,
// because what a database from before the ladder already has depends on the
// release that wrote it.
func migrateTablesAndColumns(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, originalTables); err != nil {
		return fmt.Errorf("create original tables: %w", err)
	}
	if err := addMissingColumns(ctx, tx, originalColumns); err != nil {
		return err
	}
	if err := execAll(ctx, tx, laterTables); err != nil {
		return err
	}
	// After laterTables: volume_backups and snapshots exist by now, for an old
	// database as the shape it came with, for a new one with these columns.
	return addMissingColumns(ctx, tx, laterColumns)
}

// migrateSecretsKey is migrateSecretsToV2 as a step. It runs after the columns
// are in place, because the copy reads value_encrypted, updated_at and user_id.
func migrateSecretsKey(ctx context.Context, tx *sql.Tx) error {
	_, err := migrateSecretsToV2(ctx, tx)
	return err
}

// migrateSecretsToV2 rebuilds the secrets table from the v1 shape (PRIMARY KEY
// name) to the v2 one (PRIMARY KEY (user_id, name)), so two users can own a
// secret of the same name. It reports whether it rebuilt.
//
// It reads the shape from pragma_table_info and leaves a v2 table alone. Every
// database from before the ladder reads as version 0, and nearly all of them
// already have the rebuilt table: their secrets must not be copied and
// swapped again on the boot that first records a version.
//
// The copy, drop and rename commit together with the caller's transaction, so
// a crash can't leave the database without a secrets table.
func migrateSecretsToV2(ctx context.Context, tx *sql.Tx) (bool, error) {
	// Composite PK on (user_id, name) means user_id is part of the
	// primary key (pk > 0 in pragma_table_info). On a v1 schema,
	// user_id doesn't exist at all and the query returns 0.
	var pkCount int
	if err := tx.QueryRowContext(
		ctx, `SELECT COUNT(*) FROM pragma_table_info('secrets') WHERE name='user_id' AND pk > 0`,
	).Scan(&pkCount); err != nil {
		return false, fmt.Errorf("inspect secrets schema: %w", err)
	}
	if pkCount > 0 {
		return false, nil
	}

	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS secrets_v2 (
		user_id TEXT NOT NULL DEFAULT '',
		name TEXT NOT NULL,
		path TEXT NOT NULL DEFAULT '',
		value_encrypted BLOB DEFAULT NULL,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (user_id, name)
	)`); err != nil {
		return false, fmt.Errorf("create secrets_v2: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO secrets_v2 (user_id, name, path, value_encrypted, created_at, updated_at)
		SELECT COALESCE(user_id, ''), name, COALESCE(path, ''), value_encrypted,
		       created_at, COALESCE(updated_at, created_at) FROM secrets`); err != nil {
		return false, fmt.Errorf("copy secrets v1→v2: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS secrets`); err != nil {
		return false, fmt.Errorf("drop secrets: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE secrets_v2 RENAME TO secrets`); err != nil {
		return false, fmt.Errorf("rename secrets_v2: %w", err)
	}
	return true, nil
}

// secretGrantsTable: a placeholder identifies one grant within a sandbox, not
// across them. A clone's memory and environment hold its source's placeholder,
// so the source and each clone are granted the same one, each by a grant of
// its own (revocable on its own).
const secretGrantsTable = `CREATE TABLE IF NOT EXISTS secret_grants (
		id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL,
		secret_name TEXT NOT NULL,
		sandbox_id TEXT NOT NULL,
		hosts_json TEXT NOT NULL DEFAULT '[]',
		placeholder TEXT NOT NULL,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		expires_at DATETIME,
		revoked_at DATETIME,
		UNIQUE (sandbox_id, placeholder)
	)`

// migrateSecretGrants converts a secret_grants table of the older shape, then
// creates the table and its indexes where they are missing. The conversion
// comes first because it drops the old table's indexes, which are recreated
// here.
func migrateSecretGrants(ctx context.Context, tx *sql.Tx) error {
	if _, err := migrateGrantPlaceholderScope(ctx, tx); err != nil {
		return err
	}
	return execAll(ctx, tx, []string{
		secretGrantsTable,
		`CREATE INDEX IF NOT EXISTS idx_secret_grants_sandbox ON secret_grants(sandbox_id)`,
		`CREATE INDEX IF NOT EXISTS idx_secret_grants_user_created ON secret_grants(user_id, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_secret_grants_placeholder ON secret_grants(placeholder)`,
	})
}

// migrateGrantPlaceholderScope rebuilds a secret_grants table that still has
// the original table-wide UNIQUE on placeholder, which a clone's grant (the
// same placeholder as its source's) would violate. SQLite can't drop a
// constraint in place, so the rows move into a table with the per-sandbox one.
//
// Idempotent: a no-op once rebuilt, or while the table doesn't exist yet.
// Returns true if the rebuild ran on this call. The rebuild commits with the
// caller's transaction. It drops the old table's indexes; the caller recreates
// them afterwards.
func migrateGrantPlaceholderScope(ctx context.Context, tx *sql.Tx) (bool, error) {
	var wide int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_index_list('secret_grants') AS il
		WHERE il."unique" = 1
		  AND (SELECT COUNT(*) FROM pragma_index_info(il.name)) = 1
		  AND (SELECT name FROM pragma_index_info(il.name)) = 'placeholder'`).Scan(&wide); err != nil {
		return false, fmt.Errorf("inspect secret_grants indexes: %w", err)
	}
	if wide == 0 {
		return false, nil
	}
	for _, stmt := range []string{
		`ALTER TABLE secret_grants RENAME TO secret_grants_wide`,
		secretGrantsTable,
		`INSERT INTO secret_grants (id, user_id, secret_name, sandbox_id, hosts_json, placeholder, created_at, expires_at, revoked_at)
		 SELECT id, user_id, secret_name, sandbox_id, hosts_json, placeholder, created_at, expires_at, revoked_at FROM secret_grants_wide`,
		`DROP TABLE secret_grants_wide`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return false, fmt.Errorf("rebuild secret_grants: %w", err)
		}
	}
	return true, nil
}

// migrateEventsCursorIndex lets a follower read one user's events after a
// cursor (WHERE user_id = ? AND id > ? ORDER BY id LIMIT n) by seeking to it.
// idx_events_user is ordered by (user_id, ts), so without this index SQLite
// finds all of the user's events and sorts them by id on every wake-up.
//
// The first step after the baseline: it runs once, on the schema the baseline
// left, so it needs no IF NOT EXISTS.
func migrateEventsCursorIndex(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `CREATE INDEX idx_events_user_id ON events(user_id, id)`)
	return err
}

// migrateSandboxEventOwners gives each sandbox's owner the events about it
// that were stored without a user, as InsertEvents now stores them, so an
// owner reading from the start of the stream sees the thermal history too.
// The owner comes from the sandbox's row, or, once destroy has deleted that,
// from the events about it that do name a user, when they all name the same
// one.
func migrateSandboxEventOwners(ctx context.Context, tx *sql.Tx) error {
	return execAll(ctx, tx, []string{
		`UPDATE events SET user_id = (SELECT created_by FROM sandboxes WHERE sandboxes.id = events.sandbox_id)
		WHERE user_id = '' AND sandbox_id IN (SELECT id FROM sandboxes WHERE created_by != '')`,
		`UPDATE events SET user_id = owner.user_id
		FROM (SELECT sandbox_id, MIN(user_id) AS user_id FROM events
			WHERE sandbox_id != '' AND user_id != ''
			GROUP BY sandbox_id HAVING COUNT(DISTINCT user_id) = 1) AS owner
		WHERE events.user_id = '' AND events.sandbox_id = owner.sandbox_id`,
	})
}
