// Package secrets provides age-based encryption for sandbox secrets.
package secrets

import (
	"bytes"
	"fmt"
	"io"
	"os"

	"filippo.io/age"
)

// EnsureKey loads an age identity from path, or generates one if it doesn't exist.
// Returns the identity (for decryption) and recipient (for encryption).
func EnsureKey(path string) (*age.X25519Identity, *age.X25519Recipient, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		// Key exists — parse it
		id, err := age.ParseX25519Identity(string(bytes.TrimSpace(data)))
		if err != nil {
			return nil, nil, fmt.Errorf("parse age key %s: %w", path, err)
		}
		return id, id.Recipient(), nil
	}
	if !os.IsNotExist(err) {
		return nil, nil, fmt.Errorf("read age key %s: %w", path, err)
	}

	// Generate new key
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, nil, fmt.Errorf("generate age key: %w", err)
	}
	if err := os.WriteFile(path, []byte(id.String()+"\n"), 0600); err != nil {
		return nil, nil, fmt.Errorf("write age key %s: %w", path, err)
	}
	return id, id.Recipient(), nil
}

// Encrypt encrypts plaintext with the given recipient.
func Encrypt(plaintext []byte, recipient *age.X25519Recipient) ([]byte, error) {
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, recipient)
	if err != nil {
		return nil, fmt.Errorf("age encrypt init: %w", err)
	}
	if _, err := w.Write(plaintext); err != nil {
		return nil, fmt.Errorf("age encrypt write: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("age encrypt close: %w", err)
	}
	return buf.Bytes(), nil
}

// Decrypt decrypts ciphertext with the given identity.
func Decrypt(ciphertext []byte, identity *age.X25519Identity) ([]byte, error) {
	r, err := age.Decrypt(bytes.NewReader(ciphertext), identity)
	if err != nil {
		return nil, fmt.Errorf("age decrypt: %w", err)
	}
	return io.ReadAll(r)
}
