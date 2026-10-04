package store

import (
	"fmt"
	"time"
)

// SandboxCA is a sandbox's own certificate authority: netd terminates TLS
// toward the guest with leaves it signs. KeyEnc is the age-encrypted PEM key.
type SandboxCA struct {
	SandboxID string
	UserID    string
	CertPEM   string
	KeyEnc    []byte
	CreatedAt time.Time
}

func (s *Store) PutSandboxCA(ca SandboxCA) error {
	if ca.CreatedAt.IsZero() {
		ca.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.Exec(`INSERT INTO sandbox_cas (sandbox_id, user_id, cert_pem, key_enc, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(sandbox_id) DO UPDATE SET user_id = excluded.user_id,
		cert_pem = excluded.cert_pem, key_enc = excluded.key_enc, created_at = excluded.created_at`,
		ca.SandboxID, ca.UserID, ca.CertPEM, ca.KeyEnc, ca.CreatedAt.UTC())
	if err != nil {
		return fmt.Errorf("put sandbox CA: %w", err)
	}
	return nil
}

func (s *Store) GetSandboxCA(sandboxID string) (*SandboxCA, error) {
	var ca SandboxCA
	err := s.db.QueryRow(`SELECT sandbox_id, user_id, cert_pem, key_enc, created_at FROM sandbox_cas WHERE sandbox_id = ?`, sandboxID).
		Scan(&ca.SandboxID, &ca.UserID, &ca.CertPEM, &ca.KeyEnc, &ca.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("get sandbox CA %q: %w", sandboxID, err)
	}
	ca.CreatedAt = ca.CreatedAt.UTC()
	return &ca, nil
}

func (s *Store) DeleteSandboxCA(sandboxID string) error {
	_, err := s.db.Exec(`DELETE FROM sandbox_cas WHERE sandbox_id = ?`, sandboxID)
	if err != nil {
		return fmt.Errorf("delete sandbox CA %q: %w", sandboxID, err)
	}
	return nil
}
