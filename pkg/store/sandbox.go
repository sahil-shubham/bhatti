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
	CPUs       float64         `json:"cpus"`
	MemoryMB   int             `json:"memory_mb"`
	DiskSizeMB int             `json:"disk_size_mb"`
	Image      string          `json:"image"`
	// Labels is operator-controlled metadata for fleet enumeration
	// (e.g. {"pool": "workers", "env": "prod"}). Persisted as JSON in
	// the labels column. Empty/nil maps round-trip as the SQL default
	// '{}'. Filtering uses exact match on both key and value; see
	// ListSandboxesWithFilter. G1.6 of PLAN-bhatti-v2.md.
	Labels map[string]string `json:"labels,omitempty"`
}

// SecretRecord tracks an encrypted secret.

const sandboxCols = `id, name, template_id, engine_id, status, ip, engine_meta_json, created_by, created_at, stopped_at, keep_hot, COALESCE(shell_token_hash,''), COALESCE(cpus,1), COALESCE(memory_mb,1024), COALESCE(disk_size_mb,0), COALESCE(image,'minimal'), COALESCE(labels,'{}')`

func (s *Store) CreateSandbox(sb Sandbox) error {
	if sb.EngineMeta == nil {
		sb.EngineMeta = json.RawMessage("{}")
	}
	keepHot := 0
	if sb.KeepHot {
		keepHot = 1
	}
	labelsJSON, err := marshalLabels(sb.Labels)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO sandboxes (id, name, template_id, engine_id, status, ip, engine_meta_json, created_by, created_at, keep_hot, cpus, memory_mb, disk_size_mb, image, labels) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sb.ID, sb.Name, sb.TemplateID, sb.EngineID, sb.Status, sb.IP, string(sb.EngineMeta), sb.CreatedBy, sb.CreatedAt, keepHot, sb.CPUs, sb.MemoryMB, sb.DiskSizeMB, sb.Image, labelsJSON,
	)
	return err
}

// marshalLabels serialises a labels map. An empty/nil map becomes "{}"
// so the column's NOT NULL constraint is satisfied.
func marshalLabels(labels map[string]string) (string, error) {
	if len(labels) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(labels)
	if err != nil {
		return "", fmt.Errorf("marshal labels: %w", err)
	}
	return string(b), nil
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

// ListSandboxesWithFilter returns sandboxes for a user, optionally
// filtered by labels. AND semantics: a sandbox matches only if every
// (key, value) pair in filter is present on the sandbox. Empty/nil
// filter is equivalent to ListSandboxes.
//
// Filtering is done in Go after the SQL fetch. For our scale (hundreds
// of sandboxes per user at most), the wire transfer + in-memory walk
// is cheaper than rewriting the query with json_extract per filter
// dimension. If this becomes a bottleneck, the next optimisation is
// a JSON1 WHERE clause; the wire shape stays the same.
func (s *Store) ListSandboxesWithFilter(userID string, filter map[string]string) ([]Sandbox, error) {
	all, err := s.ListSandboxes(userID)
	if err != nil {
		return nil, err
	}
	if len(filter) == 0 {
		return all, nil
	}
	out := all[:0]
	for _, sb := range all {
		if labelsMatch(sb.Labels, filter) {
			out = append(out, sb)
		}
	}
	return out, nil
}

// labelsMatch returns true if every (k,v) in filter is present in
// labels with the same value. Filter keys missing from labels never
// match; extra labels on the sandbox don't break the match.
func labelsMatch(labels, filter map[string]string) bool {
	for k, v := range filter {
		if labels[k] != v {
			return false
		}
	}
	return true
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

// RenameSandbox updates the user-visible name of a sandbox, scoped to the
// owning user. The partial unique index idx_sandboxes_user_name (see
// store.go) enforces per-user name uniqueness among non-destroyed sandboxes;
// on conflict the returned error contains "UNIQUE", which the handler maps
// to a 409. Returns a "not found" error if no row matches the (id, userID)
// pair — same semantics as DeleteSandbox.
func (s *Store) RenameSandbox(userID, id, newName string) error {
	res, err := s.db.Exec(
		`UPDATE sandboxes SET name = ? WHERE id = ? AND created_by = ?`,
		newName, id, userID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("sandbox %q not found", id)
	}
	return nil
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

// UpdateSandboxLabels merges labels for a sandbox: keys in `set` are
// inserted or overwritten, keys in `remove` are deleted. Other existing
// labels are preserved (no full replace). Runs in a transaction so
// concurrent updates from another writer don't lose entries.
//
// Empty set + empty remove is a no-op (still does one round-trip to
// validate ownership; returns "not found" if the (id, userID) pair is
// absent).
func (s *Store) UpdateSandboxLabels(userID, id string, set map[string]string, remove []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var labelsJSON string
	err = tx.QueryRow(
		`SELECT COALESCE(labels, '{}') FROM sandboxes WHERE id = ? AND created_by = ?`,
		id, userID,
	).Scan(&labelsJSON)
	if err == sql.ErrNoRows {
		return fmt.Errorf("sandbox %q not found", id)
	}
	if err != nil {
		return err
	}

	labels := map[string]string{}
	if labelsJSON != "" && labelsJSON != "{}" {
		if err := json.Unmarshal([]byte(labelsJSON), &labels); err != nil {
			return fmt.Errorf("parse existing labels: %w", err)
		}
	}
	for k, v := range set {
		labels[k] = v
	}
	for _, k := range remove {
		delete(labels, k)
	}

	out, err := marshalLabels(labels)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE sandboxes SET labels = ? WHERE id = ? AND created_by = ?`,
		out, id, userID,
	); err != nil {
		return err
	}
	return tx.Commit()
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
	var metaJSON, labelsJSON string
	var stoppedAt sql.NullTime
	var keepHot int
	err := s.Scan(&sb.ID, &sb.Name, &sb.TemplateID, &sb.EngineID, &sb.Status, &sb.IP, &metaJSON, &sb.CreatedBy, &sb.CreatedAt, &stoppedAt, &keepHot, &sb.ShellTokenHash, &sb.CPUs, &sb.MemoryMB, &sb.DiskSizeMB, &sb.Image, &labelsJSON)
	if err != nil {
		return nil, err
	}
	sb.EngineMeta = json.RawMessage(metaJSON)
	if stoppedAt.Valid {
		sb.StoppedAt = &stoppedAt.Time
	}
	sb.KeepHot = keepHot != 0
	if labelsJSON != "" && labelsJSON != "{}" {
		if err := json.Unmarshal([]byte(labelsJSON), &sb.Labels); err != nil {
			return nil, fmt.Errorf("parse sandbox labels: %w", err)
		}
	}
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
