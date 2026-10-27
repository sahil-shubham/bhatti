package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type SnapshotRecord struct {
	ID            string    `json:"id"`
	UserID        string    `json:"user_id"`
	Name          string    `json:"name"`
	SourceSandbox string    `json:"source_sandbox"`
	MemPath       string    `json:"-"`
	VMPath        string    `json:"-"`
	RootfsPath    string    `json:"-"`
	ConfigPath    string    `json:"-"`
	ManifestJSON  string    `json:"-"`
	SizeMB        int       `json:"size_mb"`
	CreatedAt     time.Time `json:"created_at"`

	// SourceRecordedAt is when the snapshot recorded Source and Substitution.
	// CreateSnapshot sets it on every snapshot taken or imported since they
	// were recorded — including one that recorded nothing (no grants, no CA,
	// an imported source) — so it, not their emptiness, says whether they are
	// the snapshot's own. nil: the snapshot predates them, and what its source
	// looked like is unknown but for the live source, if there still is one.
	SourceRecordedAt *time.Time `json:"-"`
	// Source is what the source sandbox's record held when the snapshot was
	// taken: the spec a resume records for the VM it restores, beyond the
	// vCPUs and RAM the engine's manifest carries. Zero for an imported
	// snapshot, whose source is on another host.
	Source SnapshotSource `json:"-"`
	// Substitution is the credential-substitution state the snapshot's guest
	// carries, kept so every resume can give its sandbox the same even after
	// the source is gone. Host-local by design (secrets are per host): it is
	// not in the manifest, so an export never carries it. GetSnapshot loads
	// it; ListSnapshots does not.
	Substitution SnapshotSubstitution `json:"-"`
}

// SnapshotSource is the part of a sandbox's record a snapshot of it keeps.
type SnapshotSource struct {
	Image      string
	DiskSizeMB int
	HasInit    bool
}

// SnapshotGrant is a secret grant the source held when a snapshot was taken:
// what a resume (or fork) copies into a grant of the new sandbox's own.
type SnapshotGrant struct {
	SecretName  string
	Hosts       []string
	Placeholder string
	ExpiresAt   *time.Time
}

// SnapshotSubstitution is the CA a snapshot's guest trusts (SandboxID is
// unused) and the placeholders in its environment, as live grants.
type SnapshotSubstitution struct {
	CA     *SandboxCA
	Grants []SnapshotGrant
}

const snapshotCols = `id, user_id, name, source_sandbox, mem_path, vm_path,
	rootfs_path, config_path, manifest_json, size_mb, created_at,
	source_image, source_disk_size_mb, source_has_init, source_recorded_at`

// CreateSnapshot records snap and, with it, what it holds in Source and
// Substitution, all or nothing.
func (s *Store) CreateSnapshot(snap SnapshotRecord) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	hasInit := 0
	if snap.Source.HasInit {
		hasInit = 1
	}
	if _, err := tx.Exec(
		`INSERT INTO snapshots (`+snapshotCols+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		snap.ID, snap.UserID, snap.Name, snap.SourceSandbox,
		snap.MemPath, snap.VMPath, snap.RootfsPath, snap.ConfigPath,
		snap.ManifestJSON, snap.SizeMB, snap.CreatedAt,
		snap.Source.Image, snap.Source.DiskSizeMB, hasInit, time.Now().UTC(),
	); err != nil {
		return err
	}
	if ca := snap.Substitution.CA; ca != nil {
		created := ca.CreatedAt
		if created.IsZero() {
			created = time.Now()
		}
		if _, err := tx.Exec(`INSERT INTO snapshot_cas (snapshot_id, user_id, cert_pem, key_enc, created_at)
			VALUES (?, ?, ?, ?, ?)`, snap.ID, snap.UserID, ca.CertPEM, ca.KeyEnc, created.UTC()); err != nil {
			return fmt.Errorf("record snapshot CA: %w", err)
		}
	}
	for _, g := range snap.Substitution.Grants {
		hosts := g.Hosts
		if hosts == nil {
			hosts = []string{}
		}
		hostsJSON, err := json.Marshal(hosts)
		if err != nil {
			return fmt.Errorf("marshal snapshot grant hosts: %w", err)
		}
		if _, err := tx.Exec(`INSERT INTO snapshot_grants (snapshot_id, user_id, secret_name, hosts_json, placeholder, expires_at)
			VALUES (?, ?, ?, ?, ?, ?)`, snap.ID, snap.UserID, g.SecretName, string(hostsJSON), g.Placeholder, utcGrantTime(g.ExpiresAt)); err != nil {
			return fmt.Errorf("record snapshot grant: %w", err)
		}
	}
	return tx.Commit()
}

func scanSnapshot(row scanner) (*SnapshotRecord, error) {
	var snap SnapshotRecord
	var hasInit int
	var recordedAt sql.NullTime
	if err := row.Scan(
		&snap.ID, &snap.UserID, &snap.Name, &snap.SourceSandbox,
		&snap.MemPath, &snap.VMPath, &snap.RootfsPath, &snap.ConfigPath,
		&snap.ManifestJSON, &snap.SizeMB, &snap.CreatedAt,
		&snap.Source.Image, &snap.Source.DiskSizeMB, &hasInit, &recordedAt,
	); err != nil {
		return nil, err
	}
	snap.Source.HasInit = hasInit != 0
	if recordedAt.Valid {
		t := recordedAt.Time.UTC()
		snap.SourceRecordedAt = &t
	}
	return &snap, nil
}

// GetSnapshot retrieves a snapshot by user and name, with its Substitution.
func (s *Store) GetSnapshot(userID, name string) (*SnapshotRecord, error) {
	snap, err := scanSnapshot(s.db.QueryRow(
		`SELECT `+snapshotCols+` FROM snapshots WHERE user_id = ? AND name = ?`, userID, name))
	if err != nil {
		return nil, fmt.Errorf("snapshot %q not found", name)
	}
	if snap.Substitution, err = s.snapshotSubstitution(snap.ID); err != nil {
		return nil, err
	}
	return snap, nil
}

func (s *Store) snapshotSubstitution(snapshotID string) (SnapshotSubstitution, error) {
	var sub SnapshotSubstitution
	var ca SandboxCA
	err := s.db.QueryRow(`SELECT user_id, cert_pem, key_enc, created_at FROM snapshot_cas WHERE snapshot_id = ?`, snapshotID).
		Scan(&ca.UserID, &ca.CertPEM, &ca.KeyEnc, &ca.CreatedAt)
	switch {
	case err == nil:
		ca.CreatedAt = ca.CreatedAt.UTC()
		sub.CA = &ca
	case !errors.Is(err, sql.ErrNoRows):
		return sub, fmt.Errorf("get snapshot CA: %w", err)
	}
	rows, err := s.db.Query(`SELECT secret_name, hosts_json, placeholder, expires_at FROM snapshot_grants
		WHERE snapshot_id = ? ORDER BY secret_name, placeholder`, snapshotID)
	if err != nil {
		return sub, fmt.Errorf("get snapshot grants: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var g SnapshotGrant
		var hostsJSON string
		var expiresAt sql.NullTime
		if err := rows.Scan(&g.SecretName, &hostsJSON, &g.Placeholder, &expiresAt); err != nil {
			return sub, fmt.Errorf("scan snapshot grant: %w", err)
		}
		if err := json.Unmarshal([]byte(hostsJSON), &g.Hosts); err != nil {
			return sub, fmt.Errorf("parse snapshot grant hosts: %w", err)
		}
		if expiresAt.Valid {
			t := expiresAt.Time.UTC()
			g.ExpiresAt = &t
		}
		sub.Grants = append(sub.Grants, g)
	}
	if err := rows.Err(); err != nil {
		return sub, fmt.Errorf("iterate snapshot grants: %w", err)
	}
	return sub, nil
}

// ListSnapshots returns all snapshots for a user, without their Substitution.
func (s *Store) ListSnapshots(userID string) ([]SnapshotRecord, error) {
	rows, err := s.db.Query(
		`SELECT `+snapshotCols+` FROM snapshots WHERE user_id = ? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SnapshotRecord
	for rows.Next() {
		snap, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *snap)
	}
	return out, rows.Err()
}

// DeleteSnapshot removes a snapshot record and the CA and grants it kept.
func (s *Store) DeleteSnapshot(userID, name string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id string
	if err := tx.QueryRow(`SELECT id FROM snapshots WHERE user_id = ? AND name = ?`, userID, name).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("snapshot %q not found", name)
		}
		return err
	}
	for _, stmt := range []string{
		`DELETE FROM snapshot_cas WHERE snapshot_id = ?`,
		`DELETE FROM snapshot_grants WHERE snapshot_id = ?`,
		`DELETE FROM snapshots WHERE id = ?`,
	} {
		if _, err := tx.Exec(stmt, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ==========================================================================
// v0.3 Tasks (async operations)
// ==========================================================================
