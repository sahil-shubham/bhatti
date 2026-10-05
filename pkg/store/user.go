package store

import (
	"database/sql"
	"fmt"
	"time"
)

type User struct {
	ID                    string    `json:"id"`
	Name                  string    `json:"name"`
	APIKeyHash            string    `json:"-"` // never serialized
	MaxSandboxes          int       `json:"max_sandboxes"`
	MaxCPUsPerSandbox     int       `json:"max_cpus_per_sandbox"`
	MaxMemoryMBPerSandbox int       `json:"max_memory_mb_per_sandbox"`
	SubnetIndex           int       `json:"subnet_index"`
	MaxVolumeStorageMB    int       `json:"max_volume_storage_mb"`
	MaxImages             int       `json:"max_images"`
	MaxSnapshots          int       `json:"max_snapshots"`
	CreatedAt             time.Time `json:"created_at"`
}

func (s *Store) CreateUser(u User) error {
	_, err := s.db.Exec(
		`INSERT INTO users (id, name, api_key_hash, max_sandboxes, max_cpus_per_sandbox, max_memory_mb_per_sandbox, subnet_index, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.Name, u.APIKeyHash, u.MaxSandboxes, u.MaxCPUsPerSandbox, u.MaxMemoryMBPerSandbox, u.SubnetIndex, u.CreatedAt,
	)
	return err
}

const userSelectCols = `id, name, api_key_hash, max_sandboxes, max_cpus_per_sandbox, max_memory_mb_per_sandbox, subnet_index, COALESCE(max_volume_storage_mb, 20480), COALESCE(max_images, 10), COALESCE(max_snapshots, 5), created_at`

func scanUser(s scanner) (*User, error) {
	var u User
	err := s.Scan(&u.ID, &u.Name, &u.APIKeyHash, &u.MaxSandboxes, &u.MaxCPUsPerSandbox,
		&u.MaxMemoryMBPerSandbox, &u.SubnetIndex, &u.MaxVolumeStorageMB, &u.MaxImages,
		&u.MaxSnapshots, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// GetUserByKeyHash looks up a user by the SHA-256 hash of their API key.
func (s *Store) GetUserByKeyHash(hash string) (*User, error) {
	row := s.db.QueryRow(`SELECT `+userSelectCols+` FROM users WHERE api_key_hash = ?`, hash)
	return scanUser(row)
}

// GetUser looks up a user by ID.
func (s *Store) GetUser(id string) (*User, error) {
	row := s.db.QueryRow(`SELECT `+userSelectCols+` FROM users WHERE id = ?`, id)
	return scanUser(row)
}

// ListUsers returns all users.
func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(`SELECT ` + userSelectCols + ` FROM users ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// DeleteUser removes a user. Fails if the user has active sandboxes or secrets.
func (s *Store) DeleteUser(id string) error {
	count, err := s.CountUserSandboxes(id)
	if err != nil {
		return fmt.Errorf("store: count user sandboxes: %w", err)
	}
	if count < 0 {
		return fmt.Errorf("store: count user sandboxes: invalid count %d", count)
	}
	if count > 0 {
		return fmt.Errorf("user has %d active sandbox(es) — destroy them first", count)
	}
	secrets, err := s.ListUserSecrets(id)
	if err != nil {
		return fmt.Errorf("store: list user secrets: %w", err)
	}
	if len(secrets) > 0 {
		return fmt.Errorf("user has %d secret(s) — delete them first", len(secrets))
	}
	res, err := s.db.Exec(`DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("user %q not found", id)
	}
	return nil
}

// NextSubnetIndex returns MAX(subnet_index)+1 for allocating new user networks.
func (s *Store) NextSubnetIndex() (int, error) {
	var maxIdx sql.NullInt64
	if err := s.db.QueryRow(`SELECT MAX(subnet_index) FROM users`).Scan(&maxIdx); err != nil {
		return 0, fmt.Errorf("store: next subnet index: %w", err)
	}
	if !maxIdx.Valid {
		return 1, nil
	}
	if maxIdx.Int64 < 0 || maxIdx.Int64 >= int64(^uint(0)>>1) {
		return 0, fmt.Errorf("store: next subnet index: invalid maximum %d", maxIdx.Int64)
	}
	return int(maxIdx.Int64) + 1, nil
}

// RotateUserKey updates a user's API key hash. Returns error if user not found.
func (s *Store) RotateUserKey(id, newKeyHash string) error {
	res, err := s.db.Exec(`UPDATE users SET api_key_hash = ? WHERE id = ?`, newKeyHash, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("user %q not found", id)
	}
	return nil
}

// --- Templates ---

func (s *Store) GetUserByName(name string) (*User, error) {
	var u User
	err := s.db.QueryRow(
		`SELECT id, name, api_key_hash, max_sandboxes, max_cpus_per_sandbox,
		 max_memory_mb_per_sandbox, subnet_index, created_at
		 FROM users WHERE name = ?`, name).Scan(
		&u.ID, &u.Name, &u.APIKeyHash, &u.MaxSandboxes, &u.MaxCPUsPerSandbox,
		&u.MaxMemoryMBPerSandbox, &u.SubnetIndex, &u.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("user %q not found", name)
	}
	return &u, nil
}

// ==========================================================================
// v0.3 Snapshots
// ==========================================================================
