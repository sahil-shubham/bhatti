package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// SecretGrant lets one sandbox use one secret toward a set of hosts.
type SecretGrant struct {
	ID          string     `json:"id"`
	UserID      string     `json:"-"`
	SecretName  string     `json:"secret"`
	SandboxID   string     `json:"sandbox_id"`
	Hosts       []string   `json:"hosts"`
	Placeholder string     `json:"placeholder"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
}

func (g *SecretGrant) Live(now time.Time) bool {
	return g.RevokedAt == nil && (g.ExpiresAt == nil || now.Before(*g.ExpiresAt))
}

const grantCols = `id, user_id, secret_name, sandbox_id, hosts_json, placeholder, created_at, expires_at, revoked_at`

func (s *Store) CreateSecretGrant(g SecretGrant) error {
	hosts := g.Hosts
	if hosts == nil {
		hosts = []string{}
	}
	hostsJSON, err := json.Marshal(hosts)
	if err != nil {
		return fmt.Errorf("marshal grant hosts: %w", err)
	}
	if g.CreatedAt.IsZero() {
		g.CreatedAt = time.Now().UTC()
	}
	_, err = s.db.Exec(`INSERT INTO secret_grants (id, user_id, secret_name, sandbox_id, hosts_json, placeholder, created_at, expires_at, revoked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		g.ID, g.UserID, g.SecretName, g.SandboxID, string(hostsJSON), g.Placeholder,
		g.CreatedAt.UTC(), utcGrantTime(g.ExpiresAt), utcGrantTime(g.RevokedAt))
	if err != nil {
		return fmt.Errorf("create secret grant: %w", err)
	}
	return nil
}

func utcGrantTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}

func (s *Store) GetSecretGrant(userID, id string) (*SecretGrant, error) {
	g, err := scanSecretGrant(s.db.QueryRow(`SELECT `+grantCols+` FROM secret_grants WHERE user_id = ? AND id = ?`, userID, id))
	if err != nil {
		return nil, fmt.Errorf("get secret grant %q: %w", id, err)
	}
	return g, nil
}

// GetSandboxSecretGrantByPlaceholder is the grant of sandboxID that carries
// placeholder. A placeholder is unique within a sandbox, not across them: a
// clone's memory and environment hold its source's placeholder, so the source
// and each clone have a grant of their own for it. The broker resolves
// through this lookup, so a placeholder works only where it was granted.
func (s *Store) GetSandboxSecretGrantByPlaceholder(sandboxID, placeholder string) (*SecretGrant, error) {
	g, err := scanSecretGrant(s.db.QueryRow(`SELECT `+grantCols+` FROM secret_grants WHERE sandbox_id = ? AND placeholder = ?`, sandboxID, placeholder))
	if err != nil {
		return nil, fmt.Errorf("get secret grant by placeholder: %w", err)
	}
	return g, nil
}

// FindSecretGrantByPlaceholder is a grant of userID's that carries
// placeholder, whichever sandbox holds it (the oldest if several do). It
// names the owner's grant when a refusal is explained and authorizes
// nothing: use GetSandboxSecretGrantByPlaceholder for that.
func (s *Store) FindSecretGrantByPlaceholder(userID, placeholder string) (*SecretGrant, error) {
	g, err := scanSecretGrant(s.db.QueryRow(`SELECT `+grantCols+` FROM secret_grants WHERE user_id = ? AND placeholder = ? ORDER BY created_at, id LIMIT 1`, userID, placeholder))
	if err != nil {
		return nil, fmt.Errorf("find secret grant by placeholder: %w", err)
	}
	return g, nil
}

func (s *Store) ListSecretGrants(userID string) ([]SecretGrant, error) {
	rows, err := s.db.Query(`SELECT `+grantCols+` FROM secret_grants WHERE user_id = ? ORDER BY created_at DESC, id DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("list secret grants: %w", err)
	}
	defer rows.Close()
	return scanSecretGrants(rows)
}

func (s *Store) ListSandboxSecretGrants(sandboxID string) ([]SecretGrant, error) {
	rows, err := s.db.Query(`SELECT `+grantCols+` FROM secret_grants WHERE sandbox_id = ? ORDER BY created_at DESC, id DESC`, sandboxID)
	if err != nil {
		return nil, fmt.Errorf("list sandbox secret grants: %w", err)
	}
	defer rows.Close()
	return scanSecretGrants(rows)
}

func scanSecretGrants(rows *sql.Rows) ([]SecretGrant, error) {
	var grants []SecretGrant
	for rows.Next() {
		g, err := scanSecretGrant(rows)
		if err != nil {
			return nil, fmt.Errorf("scan secret grant: %w", err)
		}
		grants = append(grants, *g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate secret grants: %w", err)
	}
	return grants, nil
}

func scanSecretGrant(row scanner) (*SecretGrant, error) {
	var g SecretGrant
	var hostsJSON string
	var expiresAt, revokedAt sql.NullTime
	if err := row.Scan(&g.ID, &g.UserID, &g.SecretName, &g.SandboxID, &hostsJSON, &g.Placeholder, &g.CreatedAt, &expiresAt, &revokedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(hostsJSON), &g.Hosts); err != nil {
		return nil, fmt.Errorf("parse grant hosts: %w", err)
	}
	g.CreatedAt = g.CreatedAt.UTC()
	if expiresAt.Valid {
		t := expiresAt.Time.UTC()
		g.ExpiresAt = &t
	}
	if revokedAt.Valid {
		t := revokedAt.Time.UTC()
		g.RevokedAt = &t
	}
	return &g, nil
}

func (s *Store) RevokeSecretGrant(userID, id string, at time.Time) error {
	res, err := s.db.Exec(`UPDATE secret_grants SET revoked_at = COALESCE(revoked_at, ?) WHERE user_id = ? AND id = ?`, at.UTC(), userID, id)
	if err != nil {
		return fmt.Errorf("revoke secret grant %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("revoke secret grant %q: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("revoke secret grant %q: %w", id, sql.ErrNoRows)
	}
	return nil
}

func (s *Store) DeleteSandboxSecretGrants(sandboxID string) error {
	_, err := s.db.Exec(`DELETE FROM secret_grants WHERE sandbox_id = ?`, sandboxID)
	if err != nil {
		return fmt.Errorf("delete sandbox secret grants: %w", err)
	}
	return nil
}

// DeleteSecretGrantsForSecret drops every grant of a secret: the grants
// sandboxes hold and the ones snapshots keep for their resumes. A secret
// created later under the same name must not inherit either.
func (s *Store) DeleteSecretGrantsForSecret(userID, secretName string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("delete secret grants: %w", err)
	}
	defer tx.Rollback()
	for _, table := range []string{"secret_grants", "snapshot_grants"} {
		if _, err := tx.Exec(`DELETE FROM `+table+` WHERE user_id = ? AND secret_name = ?`, userID, secretName); err != nil {
			return fmt.Errorf("delete secret grants: %w", err)
		}
	}
	return tx.Commit()
}
