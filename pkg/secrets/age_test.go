package secrets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureKeyCreatesNew(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "age.key")

	id, rcpt, err := EnsureKey(path)
	if err != nil {
		t.Fatalf("EnsureKey: %v", err)
	}
	if id == nil || rcpt == nil {
		t.Fatal("nil identity or recipient")
	}

	// File should exist
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read key file: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("empty key file")
	}

	// Permissions should be 0600
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Errorf("key file perms: %v, want 0600", info.Mode().Perm())
	}
}

func TestEnsureKeyLoadExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "age.key")

	id1, _, err := EnsureKey(path)
	if err != nil {
		t.Fatalf("first EnsureKey: %v", err)
	}

	id2, _, err := EnsureKey(path)
	if err != nil {
		t.Fatalf("second EnsureKey: %v", err)
	}

	if id1.String() != id2.String() {
		t.Error("second call returned different identity")
	}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "age.key")

	id, rcpt, _ := EnsureKey(path)

	plaintext := []byte("super-secret-api-key-12345")

	encrypted, err := Encrypt(plaintext, rcpt)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if len(encrypted) == 0 {
		t.Fatal("empty ciphertext")
	}
	// Ciphertext should be different from plaintext
	if string(encrypted) == string(plaintext) {
		t.Fatal("ciphertext equals plaintext")
	}

	decrypted, err := Decrypt(encrypted, id)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(decrypted) != string(plaintext) {
		t.Errorf("decrypted: %q, want %q", decrypted, plaintext)
	}
}

func TestDecryptWithWrongKey(t *testing.T) {
	dir := t.TempDir()

	_, rcpt1, _ := EnsureKey(filepath.Join(dir, "key1"))
	id2, _, _ := EnsureKey(filepath.Join(dir, "key2"))

	encrypted, _ := Encrypt([]byte("secret"), rcpt1)

	_, err := Decrypt(encrypted, id2)
	if err == nil {
		t.Error("expected error decrypting with wrong key")
	}
}

func TestEncryptDecryptLargePayload(t *testing.T) {
	dir := t.TempDir()
	id, rcpt, _ := EnsureKey(filepath.Join(dir, "age.key"))

	// 1MB payload
	plaintext := make([]byte, 1<<20)
	for i := range plaintext {
		plaintext[i] = byte(i % 256)
	}

	encrypted, err := Encrypt(plaintext, rcpt)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	decrypted, err := Decrypt(encrypted, id)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if len(decrypted) != len(plaintext) {
		t.Fatalf("decrypted len: %d, want %d", len(decrypted), len(plaintext))
	}
	for i := range plaintext {
		if decrypted[i] != plaintext[i] {
			t.Fatalf("mismatch at byte %d", i)
			break
		}
	}
}

func TestEnsureKeyCorruptedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "age.key")

	// Write garbage
	os.WriteFile(path, []byte("not-a-valid-age-key\n"), 0600)

	_, _, err := EnsureKey(path)
	if err == nil {
		t.Error("expected error loading corrupted key")
	} else {
		t.Logf("✓ corrupted key rejected: %v", err)
	}
}

func TestEncryptDecryptEmpty(t *testing.T) {
	dir := t.TempDir()
	id, rcpt, _ := EnsureKey(filepath.Join(dir, "age.key"))

	encrypted, err := Encrypt([]byte{}, rcpt)
	if err != nil {
		t.Fatalf("Encrypt empty: %v", err)
	}

	decrypted, err := Decrypt(encrypted, id)
	if err != nil {
		t.Fatalf("Decrypt empty: %v", err)
	}
	if len(decrypted) != 0 {
		t.Errorf("expected empty, got %d bytes", len(decrypted))
	}
}
