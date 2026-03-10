package pkg

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"
)

// Config holds all forge configuration.
type Config struct {
	Engine    string `yaml:"engine"`     // "docker" or "firecracker"
	Listen    string `yaml:"listen"`     // e.g. ":8080"
	AuthToken string `yaml:"auth_token"` // bearer token
	DataDir   string `yaml:"data_dir"`   // defaults to ~/.forge
}

// DefaultDataDir returns ~/.forge.
func DefaultDataDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".forge")
}

// LoadConfig reads ~/.forge/config.yaml or returns sensible defaults.
func LoadConfig() (*Config, error) {
	dir := DefaultDataDir()
	cfg := &Config{
		Engine:  "docker",
		Listen:  ":8080",
		DataDir: dir,
	}

	path := filepath.Join(dir, "config.yaml")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if cfg.DataDir == "" {
		cfg.DataDir = dir
	}
	return cfg, nil
}

// EnsureKeypair generates an ed25519 SSH keypair in DataDir if missing.
// Returns the path to the private key.
func EnsureKeypair(dataDir string) (string, error) {
	privPath := filepath.Join(dataDir, "id_ed25519")
	pubPath := privPath + ".pub"

	if _, err := os.Stat(privPath); err == nil {
		return privPath, nil
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", fmt.Errorf("generate keypair: %w", err)
	}

	// Write private key in PEM format
	privBytes, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return "", fmt.Errorf("marshal private key: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return "", err
	}
	if err := os.WriteFile(privPath, pem.EncodeToMemory(privBytes), 0600); err != nil {
		return "", err
	}

	// Write public key in authorized_keys format
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(pubPath, ssh.MarshalAuthorizedKey(sshPub), 0644); err != nil {
		return "", err
	}

	return privPath, nil
}
