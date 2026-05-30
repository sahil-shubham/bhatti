package store

import (
	"database/sql"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

// TemplateMountSpec defines a default volume mount for a template.
// Volume is a named Docker volume tracked by bhatti (legacy v0.1/v0.2).
// ImageRecord is a v0.3 rootfs image (admin or user-scoped).
// SnapshotRecord is a v0.3 named VM snapshot.
// TaskRecord tracks an async operation (e.g., image pull).
// Store wraps SQLite operations.
type Store struct {
	db *sql.DB
}

const schema = `
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

// migrations runs ALTER TABLE statements for columns added after initial schema.
// Duplicate column errors are silently ignored (idempotent).
const migrations = `
ALTER TABLE templates ADD COLUMN mounts_json TEXT NOT NULL DEFAULT '[]';
ALTER TABLE sandboxes ADD COLUMN rootfs_path TEXT DEFAULT '';
ALTER TABLE sandboxes ADD COLUMN snap_mem_path TEXT DEFAULT '';
ALTER TABLE sandboxes ADD COLUMN snap_vm_path TEXT DEFAULT '';
ALTER TABLE sandboxes ADD COLUMN vsock_cid INTEGER DEFAULT 0;
ALTER TABLE sandboxes ADD COLUMN tap_device TEXT DEFAULT '';
ALTER TABLE sandboxes ADD COLUMN guest_ip TEXT DEFAULT '';
ALTER TABLE sandboxes ADD COLUMN guest_mac TEXT DEFAULT '';
ALTER TABLE sandboxes ADD COLUMN vcpu_count REAL DEFAULT 1;
ALTER TABLE sandboxes ADD COLUMN mem_size_mib INTEGER DEFAULT 512;
ALTER TABLE sandboxes ADD COLUMN socket_path TEXT DEFAULT '';
ALTER TABLE sandboxes ADD COLUMN vsock_path TEXT DEFAULT '';
ALTER TABLE secrets ADD COLUMN value_encrypted BLOB DEFAULT NULL;
ALTER TABLE secrets ADD COLUMN updated_at DATETIME DEFAULT CURRENT_TIMESTAMP;
ALTER TABLE sandboxes ADD COLUMN created_by TEXT NOT NULL DEFAULT '';
ALTER TABLE secrets ADD COLUMN user_id TEXT NOT NULL DEFAULT '';
ALTER TABLE sandboxes ADD COLUMN agent_token TEXT DEFAULT '';
ALTER TABLE sandboxes ADD COLUMN has_base_snapshot INTEGER DEFAULT 0;
ALTER TABLE users ADD COLUMN max_volume_storage_mb INTEGER NOT NULL DEFAULT 20480;
ALTER TABLE users ADD COLUMN max_images INTEGER NOT NULL DEFAULT 10;
ALTER TABLE users ADD COLUMN max_snapshots INTEGER NOT NULL DEFAULT 5;
ALTER TABLE sandboxes ADD COLUMN keep_hot INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sandboxes ADD COLUMN fc_path_origin TEXT DEFAULT '';
ALTER TABLE sandboxes ADD COLUMN shell_token_hash TEXT DEFAULT '';
ALTER TABLE sandboxes ADD COLUMN cpus REAL NOT NULL DEFAULT 1;
ALTER TABLE sandboxes ADD COLUMN memory_mb INTEGER NOT NULL DEFAULT 1024;
ALTER TABLE sandboxes ADD COLUMN disk_size_mb INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sandboxes ADD COLUMN image TEXT NOT NULL DEFAULT 'minimal';
`

// New opens (or creates) the SQLite database and runs migrations.
func New(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	// Run additive migrations — ignore "duplicate column" errors
	for _, stmt := range strings.Split(migrations, ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" || strings.HasPrefix(stmt, "--") {
			continue
		}
		db.Exec(stmt) // ignore errors (column already exists)
	}

	// v0.3 tables: persistent volumes, images, snapshots, tasks
	db.Exec(`CREATE TABLE IF NOT EXISTS volumes_v2 (
		id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL,
		name TEXT NOT NULL,
		size_mb INTEGER NOT NULL,
		file_path TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'ready',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(user_id, name)
	)`)
	db.Exec(`CREATE TABLE IF NOT EXISTS volume_attachments (
		volume_id TEXT NOT NULL,
		sandbox_id TEXT NOT NULL,
		mount TEXT NOT NULL,
		read_only INTEGER NOT NULL DEFAULT 0,
		attached_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (volume_id, sandbox_id)
	)`)
	db.Exec(`CREATE TABLE IF NOT EXISTS images (
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
	)`)
	db.Exec(`CREATE TABLE IF NOT EXISTS snapshots (
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
	)`)
	db.Exec(`CREATE TABLE IF NOT EXISTS tasks (
		id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL,
		type TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'running',
		progress TEXT NOT NULL DEFAULT '',
		result_json TEXT NOT NULL DEFAULT '{}',
		error TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		completed_at DATETIME
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_tasks_created_at ON tasks(created_at)`)

	// v0.4: publish rules for public proxy
	db.Exec(`CREATE TABLE IF NOT EXISTS publish_rules (
		id TEXT PRIMARY KEY,
		sandbox_id TEXT NOT NULL,
		user_id TEXT NOT NULL,
		port INTEGER NOT NULL,
		alias TEXT NOT NULL UNIQUE,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(sandbox_id, port)
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_publish_rules_sandbox ON publish_rules(sandbox_id)`)

	// v0.5: volume backups
	db.Exec(`CREATE TABLE IF NOT EXISTS volume_backups (
		id TEXT PRIMARY KEY,
		volume_name TEXT NOT NULL,
		user_id TEXT NOT NULL,
		s3_key TEXT NOT NULL,
		size_bytes INTEGER NOT NULL,
		sha256 TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_volume_backups_name ON volume_backups(user_id, volume_name, created_at DESC)`)

	// Create unique index on (created_by, name) for non-destroyed sandboxes.
	// Prevents a user from having two sandboxes with the same name.
	db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_sandboxes_user_name
		ON sandboxes(created_by, name) WHERE status != 'destroyed'`)

	// Migrate secrets from v1 (PRIMARY KEY name) to v2 (PRIMARY KEY
	// (user_id, name)). Idempotent — a no-op once the table is v2.
	if _, err := migrateSecretsToV2(db); err != nil {
		return nil, fmt.Errorf("migrate secrets v1→v2: %w", err)
	}

	// Image sharing table — allows sharing images with specific users
	db.Exec(`CREATE TABLE IF NOT EXISTS image_shares (
		image_id TEXT NOT NULL,
		user_id TEXT NOT NULL,
		PRIMARY KEY (image_id, user_id)
	)`)

	// Observability: events + metrics snapshots
	db.Exec(`CREATE TABLE IF NOT EXISTS events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ts TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
		type TEXT NOT NULL,
		user_id TEXT NOT NULL DEFAULT '',
		sandbox_id TEXT NOT NULL DEFAULT '',
		meta TEXT NOT NULL DEFAULT '{}'
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_events_ts ON events(ts)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_events_type ON events(type, ts)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_events_user ON events(user_id, ts)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS metrics_snapshots (
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
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_ms_ts ON metrics_snapshots(ts)`)

	return &Store{db: db}, nil
}

// migrateSecretsToV2 converts the legacy v1 secrets schema
// (PRIMARY KEY name) to the v2 composite-key schema
// (PRIMARY KEY (user_id, name)). The v1 shape prevented two users
// from owning a secret with the same name.
//
// Idempotent. Detects the v2 schema via pragma_table_info and is a
// no-op when already migrated. Returns true if the rewrite ran on
// this call, false if it was a no-op.
//
// Pre-fix, the rewrite ran unconditionally on every boot: CREATE
// secrets_v2, INSERT from secrets, DROP secrets, RENAME. That
// re-copies every secret on every restart and opens a narrow crash
// window between DROP and RENAME where the secrets table doesn't
// exist. Tranche 0a item #6 of PLAN-bhatti-v2.md.
func migrateSecretsToV2(db *sql.DB) (bool, error) {
	// Composite PK on (user_id, name) means user_id is part of the
	// primary key (pk > 0 in pragma_table_info). On a v1 schema,
	// user_id doesn't exist at all and the query returns 0.
	var pkCount int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('secrets') WHERE name='user_id' AND pk > 0`,
	).Scan(&pkCount); err != nil {
		return false, fmt.Errorf("inspect secrets schema: %w", err)
	}
	if pkCount > 0 {
		return false, nil
	}

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS secrets_v2 (
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
	if _, err := db.Exec(`INSERT OR IGNORE INTO secrets_v2 (user_id, name, path, value_encrypted, created_at, updated_at)
		SELECT COALESCE(user_id, ''), name, COALESCE(path, ''), value_encrypted,
		       created_at, COALESCE(updated_at, created_at) FROM secrets`); err != nil {
		return false, fmt.Errorf("copy secrets v1→v2: %w", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS secrets`); err != nil {
		return false, fmt.Errorf("drop secrets: %w", err)
	}
	if _, err := db.Exec(`ALTER TABLE secrets_v2 RENAME TO secrets`); err != nil {
		return false, fmt.Errorf("rename secrets_v2: %w", err)
	}
	return true, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// --- Users ---

// CreateUser creates a new API user.

// SetSecret creates or updates an encrypted secret for a user.

// CreateImage inserts a new image record.

// CreateSnapshot inserts a new snapshot record.
// CreateTask inserts a new task record.
// CreateVolumeBackup records a new backup.
