package store

import (
	"context"
	"database/sql"
	"time"
)

type SecretRecord struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// Encrypted value is NOT included in JSON serialization
}

// User is an authenticated API user.

func (s *Store) SetSecret(ctx context.Context, userID, name string, encrypted []byte) error {
	now := time.Now()
	_, err := s.db.ExecContext(
		ctx, `INSERT INTO secrets (user_id, name, value_encrypted, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(user_id, name) DO UPDATE SET
		     value_encrypted = excluded.value_encrypted,
		     updated_at = excluded.updated_at`,
		userID, name, encrypted, now, now)
	return err
}

// GetSecretValue returns the encrypted bytes for a user's secret.
func (s *Store) GetSecretValue(ctx context.Context, userID, name string) ([]byte, error) {
	var encrypted []byte
	err := s.db.QueryRowContext(ctx, `SELECT value_encrypted FROM secrets WHERE name = ? AND user_id = ?`, name, userID).Scan(&encrypted)
	if err != nil {
		return nil, noRows(err, "secret %q not found", name)
	}
	return encrypted, nil
}

// ListUserSecrets returns metadata for a user's secrets (no values).
func (s *Store) ListUserSecrets(ctx context.Context, userID string) ([]SecretRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, created_at, COALESCE(updated_at, created_at) FROM secrets WHERE user_id = ? ORDER BY name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSecretRecords(rows)
}

// ListAllSecrets returns metadata for all secrets (no values). For admin/internal use.
func (s *Store) ListAllSecrets(ctx context.Context) ([]SecretRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, created_at, COALESCE(updated_at, created_at) FROM secrets ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSecretRecords(rows)
}

func scanSecretRecords(rows *sql.Rows) ([]SecretRecord, error) {
	var out []SecretRecord
	for rows.Next() {
		var sr SecretRecord
		var createdStr, updatedStr string
		if err := rows.Scan(&sr.Name, &createdStr, &updatedStr); err != nil {
			return nil, err
		}
		sr.CreatedAt, _ = time.Parse(time.DateTime, createdStr)
		sr.UpdatedAt, _ = time.Parse(time.DateTime, updatedStr)
		out = append(out, sr)
	}
	return out, rows.Err()
}

// GetSecret returns metadata for a user's secret (no value).
func (s *Store) GetSecret(ctx context.Context, userID, name string) (*SecretRecord, error) {
	var sr SecretRecord
	var createdStr, updatedStr string
	err := s.db.QueryRowContext(ctx, `SELECT name, created_at, COALESCE(updated_at, created_at) FROM secrets WHERE name = ? AND user_id = ?`, name, userID).
		Scan(&sr.Name, &createdStr, &updatedStr)
	if err != nil {
		return nil, noRows(err, "secret %q not found", name)
	}
	sr.CreatedAt, _ = time.Parse(time.DateTime, createdStr)
	sr.UpdatedAt, _ = time.Parse(time.DateTime, updatedStr)
	return &sr, nil
}

// --- Volumes ---

// CreateVolume creates a named volume record. Idempotent — ignores duplicates.

func (s *Store) DeleteSecret(ctx context.Context, userID, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM secrets WHERE name = ? AND user_id = ?`, name, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return wrap(ErrNotFound, "secret %q not found", name)
	}
	return nil
}
