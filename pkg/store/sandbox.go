package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

type Sandbox struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	TemplateID string          `json:"template_id"`
	EngineID   string          `json:"engine_id"`
	Status     string          `json:"status"`
	IP         string          `json:"ip"`
	EngineMeta json.RawMessage `json:"engine_meta"`
	CreatedBy  string          `json:"created_by"`
	CreatedAt      time.Time       `json:"created_at"`
	StoppedAt      *time.Time      `json:"stopped_at,omitempty"`
	KeepHot        bool            `json:"keep_hot"`
	ShellTokenHash string          `json:"-"` // never expose in API responses
}

// SecretRecord tracks an encrypted secret.

const sandboxCols = `id, name, template_id, engine_id, status, ip, engine_meta_json, created_by, created_at, stopped_at, keep_hot, COALESCE(shell_token_hash,'')`

func (s *Store) CreateSandbox(sb Sandbox) error {
	if sb.EngineMeta == nil {
		sb.EngineMeta = json.RawMessage("{}")
	}
	keepHot := 0
	if sb.KeepHot {
		keepHot = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO sandboxes (id, name, template_id, engine_id, status, ip, engine_meta_json, created_by, created_at, keep_hot) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sb.ID, sb.Name, sb.TemplateID, sb.EngineID, sb.Status, sb.IP, string(sb.EngineMeta), sb.CreatedBy, sb.CreatedAt, keepHot,
	)
	return err
}

// GetSandbox returns a sandbox scoped to a user, matching by ID first then by name.
// Use GetSandboxByID for internal/unscoped access.
func (s *Store) GetSandbox(userID, idOrName string) (*Sandbox, error) {
	// Try exact ID match first (deterministic, always unique)
	row := s.db.QueryRow(`SELECT `+sandboxCols+` FROM sandboxes WHERE id = ? AND created_by = ?`, idOrName, userID)
	if sb, err := scanSandbox(row); err == nil {
		return sb, nil
	}
	// Fall back to name match
	row = s.db.QueryRow(`SELECT `+sandboxCols+` FROM sandboxes WHERE name = ? AND created_by = ?`, idOrName, userID)
	return scanSandbox(row)
}

// GetActiveSandboxByName returns a non-destroyed sandbox by name for a user.
// Returns sql.ErrNoRows if no active sandbox with that name exists.
func (s *Store) GetActiveSandboxByName(userID, name string) (*Sandbox, error) {
	row := s.db.QueryRow(`SELECT `+sandboxCols+` FROM sandboxes WHERE name = ? AND created_by = ? AND status != 'destroyed'`, name, userID)
	return scanSandbox(row)
}

// GetSandboxByID returns a sandbox by ID regardless of owner. For internal use (thermal manager, recovery).
func (s *Store) GetSandboxByID(id string) (*Sandbox, error) {
	row := s.db.QueryRow(`SELECT `+sandboxCols+` FROM sandboxes WHERE id = ?`, id)
	return scanSandbox(row)
}

// GetSandboxByEngineID looks up a sandbox by its engine-assigned ID.
func (s *Store) GetSandboxByEngineID(engineID string) (*Sandbox, error) {
	row := s.db.QueryRow(`SELECT `+sandboxCols+` FROM sandboxes WHERE engine_id = ?`, engineID)
	return scanSandbox(row)
}

// ListSandboxes returns sandboxes for a user.
func (s *Store) ListSandboxes(userID string) ([]Sandbox, error) {
	rows, err := s.db.Query(`SELECT `+sandboxCols+` FROM sandboxes WHERE created_by = ? ORDER BY created_at DESC`, userID)
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

// ListAllSandboxes returns all sandboxes regardless of owner. For internal use (thermal manager, recovery, port scanner).
func (s *Store) ListAllSandboxes() ([]Sandbox, error) {
	rows, err := s.db.Query(`SELECT ` + sandboxCols + ` FROM sandboxes ORDER BY created_at DESC`)
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

// CountUserSandboxes returns the number of non-destroyed sandboxes for a user.
func (s *Store) CountUserSandboxes(userID string) (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM sandboxes WHERE created_by = ? AND status != 'destroyed'`, userID).Scan(&count)
	return count, err
}

func (s *Store) UpdateSandboxStatus(id, status string) error {
	_, err := s.db.Exec(`UPDATE sandboxes SET status = ? WHERE id = ?`, status, id)
	return err
}

func (s *Store) UpdateSandboxEngine(id, engineID, ip string) error {
	_, err := s.db.Exec(`UPDATE sandboxes SET engine_id = ?, ip = ? WHERE id = ?`, engineID, ip, id)
	return err
}

// UpdateSandboxKeepHot sets or clears the keep_hot flag for a sandbox.
func (s *Store) UpdateSandboxKeepHot(id string, keepHot bool) error {
	v := 0
	if keepHot {
		v = 1
	}
	_, err := s.db.Exec(`UPDATE sandboxes SET keep_hot = ? WHERE id = ?`, v, id)
	return err
}

func (s *Store) StopSandbox(id string) error {
	now := time.Now()
	_, err := s.db.Exec(`UPDATE sandboxes SET status = 'stopped', stopped_at = ? WHERE id = ?`, now, id)
	return err
}

// DeleteSandbox removes a sandbox scoped to a user.
func (s *Store) DeleteSandbox(userID, id string) error {
	res, err := s.db.Exec(`DELETE FROM sandboxes WHERE id = ? AND created_by = ?`, id, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("sandbox %q not found", id)
	}
	return nil
}

// DeleteSandboxByID removes a sandbox by ID regardless of owner. For internal cleanup.
func (s *Store) DeleteSandboxByID(id string) error {
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
	var keepHot int
	err := s.Scan(&sb.ID, &sb.Name, &sb.TemplateID, &sb.EngineID, &sb.Status, &sb.IP, &metaJSON, &sb.CreatedBy, &sb.CreatedAt, &stoppedAt, &keepHot, &sb.ShellTokenHash)
	if err != nil {
		return nil, err
	}
	sb.EngineMeta = json.RawMessage(metaJSON)
	if stoppedAt.Valid {
		sb.StoppedAt = &stoppedAt.Time
	}
	sb.KeepHot = keepHot != 0
	return &sb, nil
}

// SetShellToken stores the SHA-256 hash of the shell token.
func (s *Store) SetShellToken(sandboxID, hash string) error {
	_, err := s.db.Exec(
		`UPDATE sandboxes SET shell_token_hash = ? WHERE id = ?`,
		hash, sandboxID)
	return err
}

// ClearShellToken clears the shell token hash (revokes shell access).
func (s *Store) ClearShellToken(sandboxID string) error {
	_, err := s.db.Exec(
		`UPDATE sandboxes SET shell_token_hash = '' WHERE id = ?`,
		sandboxID)
	return err
}

type FirecrackerState struct {
	RootfsPath      string
	SnapMemPath     string
	SnapVMPath      string
	VsockCID        int
	TapDevice       string
	GuestIP         string
	GuestMAC        string
	VcpuCount       float64
	MemSizeMib      int
	SocketPath      string
	VsockPath       string
	AgentToken      string
	HasBaseSnapshot bool
	FCPathOrigin    string
}

// SaveFirecrackerState persists Firecracker-specific VM state.
func (s *Store) SaveFirecrackerState(id string, st FirecrackerState) error {
	hasSnap := 0
	if st.HasBaseSnapshot {
		hasSnap = 1
	}
	_, err := s.db.Exec(`UPDATE sandboxes SET
		rootfs_path = ?, snap_mem_path = ?, snap_vm_path = ?,
		vsock_cid = ?, tap_device = ?, guest_ip = ?, guest_mac = ?,
		vcpu_count = ?, mem_size_mib = ?, socket_path = ?, vsock_path = ?,
		agent_token = ?, has_base_snapshot = ?, fc_path_origin = ?
		WHERE id = ?`,
		st.RootfsPath, st.SnapMemPath, st.SnapVMPath,
		st.VsockCID, st.TapDevice, st.GuestIP, st.GuestMAC,
		st.VcpuCount, st.MemSizeMib, st.SocketPath, st.VsockPath,
		st.AgentToken, hasSnap, st.FCPathOrigin,
		id)
	return err
}

// LoadFirecrackerState loads Firecracker-specific VM state.
func (s *Store) LoadFirecrackerState(id string) (*FirecrackerState, error) {
	var st FirecrackerState
	var hasSnap int
	err := s.db.QueryRow(`SELECT
		COALESCE(rootfs_path,''), COALESCE(snap_mem_path,''), COALESCE(snap_vm_path,''),
		COALESCE(vsock_cid,0), COALESCE(tap_device,''), COALESCE(guest_ip,''), COALESCE(guest_mac,''),
		COALESCE(vcpu_count,1), COALESCE(mem_size_mib,512), COALESCE(socket_path,''), COALESCE(vsock_path,''),
		COALESCE(agent_token,''), COALESCE(has_base_snapshot,0), COALESCE(fc_path_origin,'')
		FROM sandboxes WHERE id = ?`, id).Scan(
		&st.RootfsPath, &st.SnapMemPath, &st.SnapVMPath,
		&st.VsockCID, &st.TapDevice, &st.GuestIP, &st.GuestMAC,
		&st.VcpuCount, &st.MemSizeMib, &st.SocketPath, &st.VsockPath,
		&st.AgentToken, &hasSnap, &st.FCPathOrigin)
	if err != nil {
		return nil, err
	}
	st.HasBaseSnapshot = hasSnap != 0
	return &st, nil
}

// ==========================================================================
// v0.3 Persistent Volumes
// ==========================================================================

// CreatePersistentVolume inserts a new persistent volume record.
