package store

import (
	"database/sql"
	"fmt"
	"time"
)

type Volume struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// SandboxVolume records a volume mounted to a sandbox (legacy v0.1/v0.2).
type SandboxVolume struct {
	SandboxID  string `json:"sandbox_id"`
	VolumeName string `json:"volume_name"`
	Target     string `json:"target"`
	ReadOnly   bool   `json:"readonly"`
}

// PersistentVolume is a v0.3 persistent ext4 volume with its own lifecycle.
type PersistentVolume struct {
	ID          string             `json:"id"`
	UserID      string             `json:"user_id"`
	Name        string             `json:"name"`
	SizeMB      int                `json:"size_mb"`
	Status      string             `json:"status"` // "creating" or "ready"
	FilePath    string             `json:"-"`
	Attachments []VolumeAttachment `json:"attachments"`
	CreatedAt   time.Time          `json:"created_at"`
}

// VolumeBackup records a backup of a persistent volume to S3.
type VolumeBackup struct {
	ID              string    `json:"id"`
	VolumeName      string    `json:"volume_name"`
	UserID          string    `json:"user_id"`
	S3Key           string    `json:"s3_key"`
	SizeBytes       int64     `json:"size_bytes"`
	SHA256          string    `json:"sha256"`
	ConsistencyMode string    `json:"consistency_mode"` // "frozen", "sync_only", "detached", or "unverified" for historical backups
	CreatedAt       time.Time `json:"created_at"`
}

// VolumeAttachment records a volume attached to a sandbox.
type VolumeAttachment struct {
	SandboxID string `json:"sandbox_id"`
	Mount     string `json:"mount"`
	ReadOnly  bool   `json:"read_only"`
}

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
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sandbox_volumes WHERE volume_name = ?`, name).Scan(&count); err != nil {
		return fmt.Errorf("store: count volume mounts: %w", err)
	}
	if count < 0 {
		return fmt.Errorf("store: count volume mounts: invalid count %d", count)
	}
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

// Returns error on UNIQUE violation (not idempotent — for race coordination).
func (s *Store) CreatePersistentVolume(v PersistentVolume) error {
	_, err := s.db.Exec(
		`INSERT INTO volumes_v2 (id, user_id, name, size_mb, file_path, status, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		v.ID, v.UserID, v.Name, v.SizeMB, v.FilePath, v.Status, v.CreatedAt,
	)
	return err
}

// GetPersistentVolume retrieves a persistent volume by user and name, including attachments.
func (s *Store) GetPersistentVolume(userID, name string) (*PersistentVolume, error) {
	var v PersistentVolume
	err := s.db.QueryRow(
		`SELECT id, user_id, name, size_mb, file_path, status, created_at
		 FROM volumes_v2 WHERE user_id = ? AND name = ?`, userID, name).Scan(
		&v.ID, &v.UserID, &v.Name, &v.SizeMB, &v.FilePath, &v.Status, &v.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(
		`SELECT sandbox_id, mount, read_only FROM volume_attachments WHERE volume_id = ?`, v.ID)
	if err != nil {
		return nil, fmt.Errorf("store: list volume attachments: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var a VolumeAttachment
		var ro int
		if err := rows.Scan(&a.SandboxID, &a.Mount, &ro); err != nil {
			return nil, fmt.Errorf("store: scan volume attachment: %w", err)
		}
		a.ReadOnly = ro != 0
		v.Attachments = append(v.Attachments, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list volume attachments: %w", err)
	}
	return &v, nil
}

// PersistentVolumeExists checks only the volume row; attachment scan failures
// must never make startup mistake an existing volume file for an orphan.
func (s *Store) PersistentVolumeExists(userID, name string) (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM volumes_v2 WHERE user_id = ? AND name = ?`, userID, name).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: check persistent volume existence: %w", err)
	}
	return true, nil
}

// ListPersistentVolumes returns all persistent volumes for a user.
func (s *Store) ListPersistentVolumes(userID string) ([]PersistentVolume, error) {
	rows, err := s.db.Query(
		`SELECT id, user_id, name, size_mb, file_path, status, created_at
		 FROM volumes_v2 WHERE user_id = ? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PersistentVolume
	for rows.Next() {
		var v PersistentVolume
		if err := rows.Scan(&v.ID, &v.UserID, &v.Name, &v.SizeMB, &v.FilePath, &v.Status, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// DeletePersistentVolume removes a persistent volume record. Fails if any attachments exist.
func (s *Store) DeletePersistentVolume(userID, name string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var volID string
	err = tx.QueryRow(`SELECT id FROM volumes_v2 WHERE user_id = ? AND name = ?`,
		userID, name).Scan(&volID)
	if err != nil {
		return fmt.Errorf("volume %q not found", name)
	}

	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM volume_attachments WHERE volume_id = ?`, volID).Scan(&count); err != nil {
		return fmt.Errorf("store: count volume attachments: %w", err)
	}
	if count < 0 {
		return fmt.Errorf("store: count volume attachments: invalid count %d", count)
	}
	if count > 0 {
		return fmt.Errorf("volume %q has %d active attachment(s)", name, count)
	}

	if _, err := tx.Exec(`DELETE FROM volumes_v2 WHERE id = ?`, volID); err != nil {
		return fmt.Errorf("store: delete persistent volume: %w", err)
	}
	return tx.Commit()
}

// LockPersistentVolumeAttachments prevents a new guest from attaching while
// the caller reads attachment rows and clones the volume. It is per volume,
// not a SQLite write transaction held over a potentially long file copy.
func (s *Store) LockPersistentVolumeAttachments(userID, name string) func() {
	key := userID + "\x00" + name
	s.volumeGateMu.Lock()
	if s.volumeGates == nil {
		s.volumeGates = make(map[string]*volumeAttachmentGate)
	}
	gate := s.volumeGates[key]
	if gate == nil {
		gate = &volumeAttachmentGate{}
		s.volumeGates[key] = gate
	}
	gate.refs++
	s.volumeGateMu.Unlock()

	gate.mu.Lock()
	return func() {
		gate.mu.Unlock()
		s.volumeGateMu.Lock()
		gate.refs--
		if gate.refs == 0 {
			delete(s.volumeGates, key)
		}
		s.volumeGateMu.Unlock()
	}
}

// AttachPersistentVolume attaches a persistent volume to a sandbox with concurrency checks.
func (s *Store) AttachPersistentVolume(userID, name, sandboxID, mount string, readOnly bool) error {
	unlock := s.LockPersistentVolumeAttachments(userID, name)
	defer unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var volID, status string
	err = tx.QueryRow(`SELECT id, status FROM volumes_v2 WHERE user_id = ? AND name = ?`,
		userID, name).Scan(&volID, &status)
	if err != nil {
		return fmt.Errorf("volume %q not found", name)
	}
	if status == "creating" {
		return fmt.Errorf("volume %q is being created, retry shortly", name)
	}

	var rwCount, roCount int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM volume_attachments WHERE volume_id = ? AND read_only = 0`,
		volID).Scan(&rwCount); err != nil {
		return fmt.Errorf("store: count read-write attachments: %w", err)
	}
	if err := tx.QueryRow(`SELECT COUNT(*) FROM volume_attachments WHERE volume_id = ? AND read_only = 1`,
		volID).Scan(&roCount); err != nil {
		return fmt.Errorf("store: count read-only attachments: %w", err)
	}
	if rwCount < 0 || roCount < 0 {
		return fmt.Errorf("store: count volume attachments: invalid counts rw=%d ro=%d", rwCount, roCount)
	}

	if !readOnly {
		if rwCount > 0 || roCount > 0 {
			return fmt.Errorf("volume %q already attached (rw=%d, ro=%d)", name, rwCount, roCount)
		}
	} else {
		if rwCount > 0 {
			return fmt.Errorf("volume %q has a read-write attachment, cannot attach read-only", name)
		}
	}

	ro := 0
	if readOnly {
		ro = 1
	}
	_, err = tx.Exec(
		`INSERT INTO volume_attachments (volume_id, sandbox_id, mount, read_only) VALUES (?, ?, ?, ?)`,
		volID, sandboxID, mount, ro)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// DetachPersistentVolume removes a specific volume attachment.
func (s *Store) DetachPersistentVolume(userID, name, sandboxID string) error {
	var volID string
	err := s.db.QueryRow(`SELECT id FROM volumes_v2 WHERE user_id = ? AND name = ?`,
		userID, name).Scan(&volID)
	if err != nil {
		return fmt.Errorf("volume %q not found", name)
	}
	_, err = s.db.Exec(
		`DELETE FROM volume_attachments WHERE volume_id = ? AND sandbox_id = ?`,
		volID, sandboxID)
	return err
}

// DetachAllPersistentVolumesForSandbox removes all persistent volume attachments for a sandbox.
func (s *Store) DetachAllPersistentVolumesForSandbox(sandboxID string) error {
	_, err := s.db.Exec(`DELETE FROM volume_attachments WHERE sandbox_id = ?`, sandboxID)
	return err
}

// AttachedPersistentVolumesForSandbox returns all persistent volumes attached
// to a sandbox, with their file paths and mount info. Used during recovery to
// rebuild the VM's volume list so resume can hard-link them into the jail.
func (s *Store) AttachedPersistentVolumesForSandbox(sandboxID string) ([]struct {
	VolumeName string
	FilePath   string
	Mount      string
	ReadOnly   bool
}, error) {
	rows, err := s.db.Query(
		`SELECT v.name, v.file_path, va.mount, va.read_only
		 FROM volume_attachments va
		 JOIN volumes_v2 v ON v.id = va.volume_id
		 WHERE va.sandbox_id = ?
		 ORDER BY va.rowid`, sandboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []struct {
		VolumeName string
		FilePath   string
		Mount      string
		ReadOnly   bool
	}
	for rows.Next() {
		var v struct {
			VolumeName string
			FilePath   string
			Mount      string
			ReadOnly   bool
		}
		var ro int
		if err := rows.Scan(&v.VolumeName, &v.FilePath, &v.Mount, &ro); err != nil {
			return nil, err
		}
		v.ReadOnly = ro != 0
		out = append(out, v)
	}
	return out, rows.Err()
}

// DetachOrphanedPersistentVolumes removes attachments for destroyed/missing sandboxes.
func (s *Store) DetachOrphanedPersistentVolumes() (int64, error) {
	res, err := s.db.Exec(`DELETE FROM volume_attachments
		WHERE sandbox_id IN (
			SELECT va.sandbox_id FROM volume_attachments va
			LEFT JOIN sandboxes s ON va.sandbox_id = s.id
			WHERE s.id IS NULL
			   OR s.status IN ('destroyed', 'unknown')
		)`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// UpdatePersistentVolumeSize updates the size_mb field after a resize.
func (s *Store) UpdatePersistentVolumeSize(userID, name string, sizeMB int) error {
	res, err := s.db.Exec(`UPDATE volumes_v2 SET size_mb = ? WHERE user_id = ? AND name = ?`,
		sizeMB, userID, name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("volume %q not found", name)
	}
	return nil
}

// UpdatePersistentVolumeStatus updates the status field (e.g., "creating" → "ready").
func (s *Store) UpdatePersistentVolumeStatus(userID, name, status string) error {
	_, err := s.db.Exec(`UPDATE volumes_v2 SET status = ? WHERE user_id = ? AND name = ?`,
		status, userID, name)
	return err
}

// UserVolumeStorageUsed returns the total size_mb of all persistent volumes for a user.
func (s *Store) UserVolumeStorageUsed(userID string) (int, error) {
	var total sql.NullInt64
	if err := s.db.QueryRow(`SELECT SUM(size_mb) FROM volumes_v2 WHERE user_id = ?`, userID).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: sum volume storage: %w", err)
	}
	if total.Valid && (total.Int64 < 0 || int64(int(total.Int64)) != total.Int64) {
		return 0, fmt.Errorf("store: sum volume storage: invalid total %d", total.Int64)
	}
	if !total.Valid {
		return 0, nil
	}
	return int(total.Int64), nil
}

// ==========================================================================
// v0.3 Images
// ==========================================================================

func (s *Store) CreateVolumeBackup(b VolumeBackup) error {
	if b.ConsistencyMode == "" {
		b.ConsistencyMode = "unverified"
	}
	_, err := s.db.Exec(
		`INSERT INTO volume_backups (id, volume_name, user_id, s3_key, size_bytes, sha256, consistency_mode, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		b.ID, b.VolumeName, b.UserID, b.S3Key, b.SizeBytes, b.SHA256, b.ConsistencyMode, b.CreatedAt)
	return err
}

// ListVolumeBackups returns backups for a volume, newest first.
func (s *Store) ListVolumeBackups(userID, volumeName string) ([]VolumeBackup, error) {
	rows, err := s.db.Query(
		`SELECT id, volume_name, user_id, s3_key, size_bytes, sha256, consistency_mode, created_at
		 FROM volume_backups WHERE user_id = ? AND volume_name = ?
		 ORDER BY created_at DESC`, userID, volumeName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VolumeBackup
	for rows.Next() {
		var b VolumeBackup
		if err := rows.Scan(&b.ID, &b.VolumeName, &b.UserID, &b.S3Key, &b.SizeBytes, &b.SHA256, &b.ConsistencyMode, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// GetVolumeBackup returns a single backup by ID.
func (s *Store) GetVolumeBackup(userID, backupID string) (*VolumeBackup, error) {
	var b VolumeBackup
	err := s.db.QueryRow(
		`SELECT id, volume_name, user_id, s3_key, size_bytes, sha256, consistency_mode, created_at
		 FROM volume_backups WHERE id = ? AND user_id = ?`, backupID, userID).Scan(
		&b.ID, &b.VolumeName, &b.UserID, &b.S3Key, &b.SizeBytes, &b.SHA256, &b.ConsistencyMode, &b.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// DeleteVolumeBackup removes a backup record.
func (s *Store) DeleteVolumeBackup(userID, backupID string) error {
	_, err := s.db.Exec(`DELETE FROM volume_backups WHERE id = ? AND user_id = ?`, backupID, userID)
	return err
}

// OldestVolumeBackups returns the oldest backups beyond the retention count.
func (s *Store) OldestVolumeBackups(userID, volumeName string, keepCount int) ([]VolumeBackup, error) {
	rows, err := s.db.Query(
		`SELECT id, volume_name, user_id, s3_key, size_bytes, sha256, consistency_mode, created_at
		 FROM volume_backups WHERE user_id = ? AND volume_name = ?
		 ORDER BY created_at DESC LIMIT -1 OFFSET ?`, userID, volumeName, keepCount)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VolumeBackup
	for rows.Next() {
		var b VolumeBackup
		if err := rows.Scan(&b.ID, &b.VolumeName, &b.UserID, &b.S3Key, &b.SizeBytes, &b.SHA256, &b.ConsistencyMode, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
