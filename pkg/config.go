package pkg

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"
)

// Config holds all bhatti configuration.
type Config struct {
	Engine    string `yaml:"engine"`     // "firecracker" (default)
	Listen    string `yaml:"listen"`     // e.g. ":8080"
	APIURL    string `yaml:"api_url"`    // CLI: remote API endpoint (e.g. https://api.bhatti.sh)
	AuthToken string `yaml:"auth_token"` // CLI: API key for remote requests
	DataDir   string `yaml:"data_dir"`   // defaults to ~/.bhatti

	// Firecracker-specific
	FirecrackerBin    string `yaml:"firecracker_bin"`    // path to firecracker binary
	FirecrackerKernel string `yaml:"firecracker_kernel"` // path to vmlinux
	FirecrackerRootfs string `yaml:"firecracker_rootfs"` // path to base rootfs.ext4
}

// DefaultDataDir returns ~/.bhatti.
func DefaultDataDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".bhatti")
}

// LoadConfig reads config.yaml from one of these locations (first match wins):
//  1. $BHATTI_CONFIG (explicit path)
//  2. ./config.yaml (working directory — for systemd WorkingDirectory)
//  3. ~/.bhatti/config.yaml (user default)
//
// Returns sensible defaults if no config file is found.
func LoadConfig() (*Config, error) {
	dir := DefaultDataDir()
	cfg := &Config{
		Engine:  "firecracker",
		Listen:  ":8080",
		DataDir: dir,
	}

	// Search order for config file
	candidates := []string{
		os.Getenv("BHATTI_CONFIG"),          // explicit override
		"config.yaml",                       // working directory
		filepath.Join(dir, "config.yaml"),   // ~/.bhatti/config.yaml
	}

	var data []byte
	var loadedFrom string
	for _, path := range candidates {
		if path == "" {
			continue
		}
		d, err := os.ReadFile(path)
		if err == nil {
			data = d
			loadedFrom = path
			break
		}
	}

	if data == nil {
		return cfg, nil // no config found, use defaults
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", loadedFrom, err)
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
