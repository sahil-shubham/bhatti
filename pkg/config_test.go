package pkg

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Engine != "firecracker" {
		t.Fatalf("expected firecracker, got %s", cfg.Engine)
	}
	if cfg.Listen != ":8080" {
		t.Fatalf("expected :8080, got %s", cfg.Listen)
	}
}

func TestConfigYAMLParsing(t *testing.T) {
	content := []byte("engine: firecracker\nlisten: :9090\nauth_token: secret123\n")
	cfg := &Config{}
	if err := yaml.Unmarshal(content, cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Engine != "firecracker" || cfg.Listen != ":9090" || cfg.AuthToken != "secret123" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestEnsureKeypair(t *testing.T) {
	dir := t.TempDir()

	// First call generates
	path, err := EnsureKeypair(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("private key not created")
	}
	if _, err := os.Stat(path + ".pub"); err != nil {
		t.Fatal("public key not created")
	}

	// Read private key to verify it's valid PEM
	data, _ := os.ReadFile(path)
	if len(data) == 0 {
		t.Fatal("private key is empty")
	}

	// Second call is idempotent
	path2, err := EnsureKeypair(dir)
	if err != nil {
		t.Fatal(err)
	}
	if path != path2 {
		t.Fatal("paths should match")
	}

	// Verify key content didn't change
	data2, _ := os.ReadFile(path2)
	if string(data) != string(data2) {
		t.Fatal("key should not be regenerated")
	}
}

func TestEnsureKeypairCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "dir")
	path, err := EnsureKeypair(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("private key not created in nested dir")
	}
}

// TestLoadConfigDataDirDefault verifies that when a config file has no
// data_dir field, it defaults to ~/.bhatti. This is the root cause of
// the split-brain DB bug: the server config at /var/lib/bhatti/config.yaml
// sets data_dir explicitly, but ~/.bhatti/config.yaml doesn't, causing
// `bhatti user rotate-key` to write to ~/.bhatti/state.db instead of
// /var/lib/bhatti/state.db.
func TestLoadConfigDataDirDefault(t *testing.T) {
	origDir, _ := os.Getwd()
	origEnv := os.Getenv("BHATTI_CONFIG")
	t.Cleanup(func() {
		os.Chdir(origDir)
		os.Setenv("BHATTI_CONFIG", origEnv)
	})

	// Config with no data_dir
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(
		"api_url: https://example.com\nauth_token: tok\n"), 0644)

	os.Setenv("BHATTI_CONFIG", "")
	os.Chdir(dir)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != DefaultDataDir() {
		t.Errorf("data_dir=%q, want default %q", cfg.DataDir, DefaultDataDir())
	}

	// Config WITH explicit data_dir
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(
		"data_dir: /var/lib/bhatti\napi_url: https://example.com\n"), 0644)

	cfg, err = LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != "/var/lib/bhatti" {
		t.Errorf("data_dir=%q, want /var/lib/bhatti", cfg.DataDir)
	}
}


