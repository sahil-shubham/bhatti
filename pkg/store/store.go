package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// TemplateMountSpec defines a default volume mount for a template.
type TemplateMountSpec struct {
	VolumeName string `json:"volume_name"` // empty = "bhatti-{sandbox_name}-workspace"
	Target     string `json:"target"`
	ReadOnly   bool   `json:"readonly"`
	AutoCreate bool   `json:"auto_create"` // create volume if missing
}

// Template is a sandbox blueprint.
type Template struct {
	ID         string              `json:"id"`
	Name       string              `json:"name"`
	Engine     string              `json:"engine"`
	Image      string              `json:"image"`
	CPUs       float64             `json:"cpus"`
	MemoryMB   int                 `json:"memory_mb"`
	DiskSizeMB int                 `json:"disk_size_mb"`
	UserData   string              `json:"userdata"`
	Secrets    []string            `json:"secrets"`
	Labels     map[string]string   `json:"labels"`
	Mounts     []TemplateMountSpec `json:"mounts"`
	CreatedAt  time.Time           `json:"created_at"`
}

// Volume is a named Docker volume tracked by bhatti.
type Volume struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// SandboxVolume records a volume mounted to a sandbox.
type SandboxVolume struct {
	SandboxID  string `json:"sandbox_id"`
	VolumeName string `json:"volume_name"`
	Target     string `json:"target"`
	ReadOnly   bool   `json:"readonly"`
}

// Sandbox is a running or stopped sandbox instance.
type Sandbox struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	TemplateID string          `json:"template_id"`
	EngineID   string          `json:"engine_id"`
	Status     string          `json:"status"`
	IP         string          `json:"ip"`
	EngineMeta json.RawMessage `json:"engine_meta"`
	CreatedAt  time.Time       `json:"created_at"`
	StoppedAt  *time.Time      `json:"stopped_at,omitempty"`
}

// SecretRecord tracks an encrypted secret file.
type SecretRecord struct {
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	CreatedAt time.Time `json:"created_at"`
}

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
`

// migrations runs ALTER TABLE statements for columns added after initial schema.
const migrations = `
-- Add mounts_json to templates (idempotent via IF NOT EXISTS workaround)
ALTER TABLE templates ADD COLUMN mounts_json TEXT NOT NULL DEFAULT '[]';
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
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// --- Templates ---

func (s *Store) CreateTemplate(t Template) error {
	secretsJSON, _ := json.Marshal(t.Secrets)
	labelsJSON, _ := json.Marshal(t.Labels)
	mountsJSON, _ := json.Marshal(t.Mounts)
	if t.Mounts == nil {
		mountsJSON = []byte("[]")
	}
	_, err := s.db.Exec(
		`INSERT INTO templates (id, name, engine, image, cpus, memory_mb, disk_size_mb, userdata, secrets_json, labels_json, mounts_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Name, t.Engine, t.Image, t.CPUs, t.MemoryMB, t.DiskSizeMB, t.UserData,
		string(secretsJSON), string(labelsJSON), string(mountsJSON), t.CreatedAt,
	)
	return err
}

func (s *Store) GetTemplate(id string) (*Template, error) {
	row := s.db.QueryRow(`SELECT id, name, engine, image, cpus, memory_mb, disk_size_mb, userdata, secrets_json, labels_json, mounts_json, created_at FROM templates WHERE id = ?`, id)
	return scanTemplate(row)
}

func (s *Store) ListTemplates() ([]Template, error) {
	rows, err := s.db.Query(`SELECT id, name, engine, image, cpus, memory_mb, disk_size_mb, userdata, secrets_json, labels_json, mounts_json, created_at FROM templates ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Template
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func (s *Store) DeleteTemplate(id string) error {
	res, err := s.db.Exec(`DELETE FROM templates WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("template %q not found", id)
	}
	return nil
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanTemplate(s scanner) (*Template, error) {
	var t Template
	var secretsJSON, labelsJSON, mountsJSON string
	err := s.Scan(&t.ID, &t.Name, &t.Engine, &t.Image, &t.CPUs, &t.MemoryMB, &t.DiskSizeMB, &t.UserData, &secretsJSON, &labelsJSON, &mountsJSON, &t.CreatedAt)
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(secretsJSON), &t.Secrets)
	json.Unmarshal([]byte(labelsJSON), &t.Labels)
	json.Unmarshal([]byte(mountsJSON), &t.Mounts)
	return &t, nil
}

// --- Sandboxes ---

func (s *Store) CreateSandbox(sb Sandbox) error {
	if sb.EngineMeta == nil {
		sb.EngineMeta = json.RawMessage("{}")
	}
	_, err := s.db.Exec(
		`INSERT INTO sandboxes (id, name, template_id, engine_id, status, ip, engine_meta_json, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sb.ID, sb.Name, sb.TemplateID, sb.EngineID, sb.Status, sb.IP, string(sb.EngineMeta), sb.CreatedAt,
	)
	return err
}

func (s *Store) GetSandbox(id string) (*Sandbox, error) {
	row := s.db.QueryRow(`SELECT id, name, template_id, engine_id, status, ip, engine_meta_json, created_at, stopped_at FROM sandboxes WHERE id = ?`, id)
	return scanSandbox(row)
}

func (s *Store) ListSandboxes() ([]Sandbox, error) {
	rows, err := s.db.Query(`SELECT id, name, template_id, engine_id, status, ip, engine_meta_json, created_at, stopped_at FROM sandboxes ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sandbox
	for rows.Next() {
		sb, err := scanSandbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sb)
	}
	return out, rows.Err()
}

func (s *Store) UpdateSandboxStatus(id, status string) error {
	_, err := s.db.Exec(`UPDATE sandboxes SET status = ? WHERE id = ?`, status, id)
	return err
}

func (s *Store) UpdateSandboxEngine(id, engineID, ip string) error {
	_, err := s.db.Exec(`UPDATE sandboxes SET engine_id = ?, ip = ? WHERE id = ?`, engineID, ip, id)
	return err
}

func (s *Store) StopSandbox(id string) error {
	now := time.Now()
	_, err := s.db.Exec(`UPDATE sandboxes SET status = 'stopped', stopped_at = ? WHERE id = ?`, now, id)
	return err
}

func (s *Store) DeleteSandbox(id string) error {
	res, err := s.db.Exec(`DELETE FROM sandboxes WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("sandbox %q not found", id)
	}
	return nil
}

func scanSandbox(s scanner) (*Sandbox, error) {
	var sb Sandbox
	var metaJSON string
	var stoppedAt sql.NullTime
	err := s.Scan(&sb.ID, &sb.Name, &sb.TemplateID, &sb.EngineID, &sb.Status, &sb.IP, &metaJSON, &sb.CreatedAt, &stoppedAt)
	if err != nil {
		return nil, err
	}
	sb.EngineMeta = json.RawMessage(metaJSON)
	if stoppedAt.Valid {
		sb.StoppedAt = &stoppedAt.Time
	}
	return &sb, nil
}

// --- Secrets ---

func (s *Store) CreateSecret(sr SecretRecord) error {
	_, err := s.db.Exec(`INSERT INTO secrets (name, path, created_at) VALUES (?, ?, ?)`, sr.Name, sr.Path, sr.CreatedAt)
	return err
}

func (s *Store) GetSecret(name string) (*SecretRecord, error) {
	var sr SecretRecord
	err := s.db.QueryRow(`SELECT name, path, created_at FROM secrets WHERE name = ?`, name).
		Scan(&sr.Name, &sr.Path, &sr.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &sr, nil
}

func (s *Store) ListSecrets() ([]SecretRecord, error) {
	rows, err := s.db.Query(`SELECT name, path, created_at FROM secrets ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SecretRecord
	for rows.Next() {
		var sr SecretRecord
		if err := rows.Scan(&sr.Name, &sr.Path, &sr.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, sr)
	}
	return out, rows.Err()
}

// --- Volumes ---

// CreateVolume creates a named volume record. Idempotent — ignores duplicates.
func (s *Store) CreateVolume(name string) error {
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO volumes (name, created_at) VALUES (?, ?)`,
		name, time.Now(),
	)
	return err
}

// GetVolume retrieves a volume by name.
func (s *Store) GetVolume(name string) (*Volume, error) {
	var v Volume
	err := s.db.QueryRow(`SELECT name, created_at FROM volumes WHERE name = ?`, name).
		Scan(&v.Name, &v.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// ListVolumes returns all tracked volumes.
func (s *Store) ListVolumes() ([]Volume, error) {
	rows, err := s.db.Query(`SELECT name, created_at FROM volumes ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Volume
	for rows.Next() {
		var v Volume
		if err := rows.Scan(&v.Name, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// DeleteVolume removes a volume record. Fails if any sandbox is using it.
func (s *Store) DeleteVolume(name string) error {
	var count int
	s.db.QueryRow(`SELECT COUNT(*) FROM sandbox_volumes WHERE volume_name = ?`, name).Scan(&count)
	if count > 0 {
		return fmt.Errorf("volume %q is in use by %d sandbox(es)", name, count)
	}
	res, err := s.db.Exec(`DELETE FROM volumes WHERE name = ?`, name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("volume %q not found", name)
	}
	return nil
}

// AttachVolume records a volume mount for a sandbox.
func (s *Store) AttachVolume(sandboxID, volumeName, target string, readonly bool) error {
	ro := 0
	if readonly {
		ro = 1
	}
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO sandbox_volumes (sandbox_id, volume_name, target, readonly) VALUES (?, ?, ?, ?)`,
		sandboxID, volumeName, target, ro,
	)
	return err
}

// GetSandboxVolumes returns all volume mounts for a sandbox.
func (s *Store) GetSandboxVolumes(sandboxID string) ([]SandboxVolume, error) {
	rows, err := s.db.Query(
		`SELECT sandbox_id, volume_name, target, readonly FROM sandbox_volumes WHERE sandbox_id = ?`,
		sandboxID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SandboxVolume
	for rows.Next() {
		var sv SandboxVolume
		var ro int
		if err := rows.Scan(&sv.SandboxID, &sv.VolumeName, &sv.Target, &ro); err != nil {
			return nil, err
		}
		sv.ReadOnly = ro != 0
		out = append(out, sv)
	}
	return out, rows.Err()
}

// DetachVolumes removes all volume mount records for a sandbox (called on destroy).
func (s *Store) DetachVolumes(sandboxID string) error {
	_, err := s.db.Exec(`DELETE FROM sandbox_volumes WHERE sandbox_id = ?`, sandboxID)
	return err
}

func (s *Store) DeleteSecret(name string) error {
	res, err := s.db.Exec(`DELETE FROM secrets WHERE name = ?`, name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("secret %q not found", name)
	}
	return nil
}
