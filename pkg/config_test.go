package pkg

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestLoadConfigDefaults(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("engine: krucible\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BHATTI_CONFIG", cfgPath)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Engine != "krucible" {
		t.Fatalf("expected krucible, got %s", cfg.Engine)
	}
	if cfg.Listen != "" {
		t.Fatalf("default listen = %q, want empty (socket-only)", cfg.Listen)
	}
	if cfg.APISocketGID != 0 {
		t.Fatalf("default api_socket_gid = %d, want 0 (owner-only)", cfg.APISocketGID)
	}
}

func TestConfigYAMLParsing(t *testing.T) {
	content := []byte("engine: krucible\nlisten: :9090\napi_socket_gid: 123\nauth_token: secret123\n")
	cfg := &Config{}
	if err := yaml.Unmarshal(content, cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Engine != "krucible" || cfg.Listen != ":9090" || cfg.APISocketGID != 123 || cfg.AuthToken != "secret123" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestConfigYAMLOmitsUnsetSocketGID(t *testing.T) {
	data, err := yaml.Marshal(&Config{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "api_socket_gid:") {
		t.Fatalf("unset api_socket_gid should be omitted: %s", data)
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

// TestLoadConfigExplicitPath verifies $BHATTI_CONFIG takes priority.
func TestLoadConfigExplicitPath(t *testing.T) {
	origEnv := os.Getenv("BHATTI_CONFIG")
	t.Cleanup(func() { os.Setenv("BHATTI_CONFIG", origEnv) })

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "custom.yaml")
	os.WriteFile(cfgPath, []byte(
		"listen: :9999\ndata_dir: /tmp/test-bhatti\n"), 0644)

	os.Setenv("BHATTI_CONFIG", cfgPath)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":9999" {
		t.Errorf("listen=%q, want :9999", cfg.Listen)
	}
	if cfg.DataDir != "/tmp/test-bhatti" {
		t.Errorf("data_dir=%q, want /tmp/test-bhatti", cfg.DataDir)
	}
	if cfg.ConfigPath != cfgPath {
		t.Errorf("config_path=%q, want %q", cfg.ConfigPath, cfgPath)
	}
}

// TestLoadConfigDataDirDefault verifies that when a config file has no
// data_dir field, it defaults to ~/.bhatti.
func TestLoadConfigDataDirDefault(t *testing.T) {
	origEnv := os.Getenv("BHATTI_CONFIG")
	t.Cleanup(func() { os.Setenv("BHATTI_CONFIG", origEnv) })

	// Config with no data_dir
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte(
		"api_url: https://example.com\nauth_token: tok\n"), 0644)

	os.Setenv("BHATTI_CONFIG", cfgPath)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != DefaultDataDir() {
		t.Errorf("data_dir=%q, want default %q", cfg.DataDir, DefaultDataDir())
	}

	// Config WITH explicit data_dir
	os.WriteFile(cfgPath, []byte(
		"data_dir: /var/lib/bhatti\napi_url: https://example.com\n"), 0644)

	cfg, err = LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != "/var/lib/bhatti" {
		t.Errorf("data_dir=%q, want /var/lib/bhatti", cfg.DataDir)
	}
}

// TestLoadConfigPathIsSet verifies ConfigPath is populated.
func TestLoadConfigPathIsSet(t *testing.T) {
	origEnv := os.Getenv("BHATTI_CONFIG")
	t.Cleanup(func() { os.Setenv("BHATTI_CONFIG", origEnv) })

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte("listen: :7777\n"), 0644)

	os.Setenv("BHATTI_CONFIG", cfgPath)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConfigPath != cfgPath {
		t.Errorf("config_path=%q, want %q", cfg.ConfigPath, cfgPath)
	}
}

func TestLoadConfigTracksEveryReadFile(t *testing.T) {
	base := t.TempDir()
	systemPath := filepath.Join(base, "system", "config.yaml")
	userPath := filepath.Join(base, "user", "config.yaml")
	for _, path := range []string{systemPath, userPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(systemPath, []byte("listen: :9090\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userPath, []byte("auth_token: secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{DataDir: filepath.Join(base, "data")}
	if err := loadLayeredConfig(cfg, systemPath, filepath.Join(base, "missing.yaml"), userPath); err != nil {
		t.Fatal(err)
	}
	if cfg.ConfigPath != systemPath || len(cfg.ConfigPaths) != 2 ||
		cfg.ConfigPaths[0] != systemPath || cfg.ConfigPaths[1] != userPath ||
		cfg.AuthToken != "secret" {
		t.Fatalf("loaded paths and credentials = %+v", cfg)
	}
}

func TestLoadConfigRejectsMalformedUserConfig(t *testing.T) {
	base := t.TempDir()
	systemPath := filepath.Join(base, "server.yaml")
	userPath := filepath.Join(base, "user.yaml")
	if err := os.WriteFile(systemPath, []byte("listen: :9090\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userPath, []byte("api_url: [invalid\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{}
	err := loadLayeredConfig(cfg, systemPath, filepath.Join(base, "missing.yaml"), userPath)
	if err == nil || !strings.Contains(err.Error(), userPath) {
		t.Fatalf("malformed user config error = %v, want path-specific parse error", err)
	}
}

func TestLoadConfigExplicitPathTracksOnlyThatFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "daemon.yaml")
	if err := os.WriteFile(file, []byte("listen: :9090\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BHATTI_CONFIG", file)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ConfigPaths) != 1 || cfg.ConfigPaths[0] != file {
		t.Fatalf("explicit config paths = %v, want only %s", cfg.ConfigPaths, file)
	}
}

// TestDefaultDataDirHonorsSudoUser verifies that running under sudo
// resolves the data dir to the *invoking* user's home, not root's.
// This is the bug that caused `sudo bhatti setup` to write to
// /var/root/.bhatti while `bhatti list` looked at /home/alice/.bhatti.
func TestDefaultDataDirHonorsSudoUser(t *testing.T) {
	orig := os.Getenv("SUDO_USER")
	t.Cleanup(func() { os.Setenv("SUDO_USER", orig) })

	// Without SUDO_USER: returns the current user's home/.bhatti.
	os.Unsetenv("SUDO_USER")
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".bhatti")
	if got := DefaultDataDir(); got != want {
		t.Errorf("no SUDO_USER: DefaultDataDir()=%q, want %q", got, want)
	}

	// SUDO_USER=root behaves the same — we treat "root" as not-sudo so
	// real-root invocations (no sudo) don't pivot to a phantom user.
	os.Setenv("SUDO_USER", "root")
	if got := DefaultDataDir(); got != want {
		t.Errorf("SUDO_USER=root: DefaultDataDir()=%q, want %q", got, want)
	}

	// SUDO_USER set to a real user: should resolve to that user's home.
	// Use the current process owner so the test works on any host.
	curUser, err := user.Current()
	if err != nil {
		t.Skip("cannot resolve current user")
	}
	os.Setenv("SUDO_USER", curUser.Username)
	if got := DefaultDataDir(); got != filepath.Join(curUser.HomeDir, ".bhatti") {
		t.Errorf("SUDO_USER=%s: DefaultDataDir()=%q, want %q",
			curUser.Username, got, filepath.Join(curUser.HomeDir, ".bhatti"))
	}

	// Same expectation for InvokingUID: when SUDO_USER points at a real
	// account, InvokingUID returns *that* uid, not root's.
	curUID, _, ok := InvokingUserIDs()
	if !ok {
		t.Errorf("InvokingUserIDs() ok=false with SUDO_USER=%s", curUser.Username)
	} else if uidStr := curUser.Uid; uidStr != "" {
		wantUID, _ := strconv.Atoi(uidStr)
		if curUID != wantUID {
			t.Errorf("InvokingUserIDs uid=%d, want %d", curUID, wantUID)
		}
	}
}

// TestLoadConfigNoFile verifies defaults when no config exists.
func TestLoadConfigNoFile(t *testing.T) {
	origEnv := os.Getenv("BHATTI_CONFIG")
	t.Cleanup(func() { os.Setenv("BHATTI_CONFIG", origEnv) })

	os.Setenv("BHATTI_CONFIG", "/nonexistent/config.yaml")

	// This will fail the BHATTI_CONFIG candidate, then try /etc/bhatti/
	// and ~/.bhatti/. On a test machine without those, we get defaults.
	// To be deterministic, point to a dir with no config.yaml.
	os.Setenv("BHATTI_CONFIG", "")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConfigPath != "" {
		// May pick up ~/.bhatti/config.yaml if it exists on the dev machine.
		// That's OK — this test just verifies no crash on missing config.
		t.Logf("found config at %s (dev machine has config)", cfg.ConfigPath)
	}
	if cfg.Engine != "krucible" {
		t.Errorf("engine=%q, want krucible", cfg.Engine)
	}
}

func TestConfigCgroupsYAML(t *testing.T) {
	cfg := &Config{}
	if err := yaml.Unmarshal([]byte("cgroups: systemd\n"), cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Cgroups != CgroupsSystemd {
		t.Fatalf("Cgroups = %q, want systemd", cfg.Cgroups)
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	back := &Config{}
	if err := yaml.Unmarshal(out, back); err != nil {
		t.Fatal(err)
	}
	if back.Cgroups != CgroupsSystemd {
		t.Fatalf("round-trip Cgroups = %q, want systemd", back.Cgroups)
	}

	def := &Config{}
	if err := yaml.Unmarshal([]byte("engine: krucible\n"), def); err != nil {
		t.Fatal(err)
	}
	if def.Cgroups != "" {
		t.Fatalf("default Cgroups = %q, want empty", def.Cgroups)
	}
	data, _ := yaml.Marshal(&Config{})
	if strings.Contains(string(data), "cgroups") {
		t.Fatalf("unset cgroups should be omitted:\n%s", data)
	}
}

func TestCgroupsModeValidate(t *testing.T) {
	for _, tc := range []struct {
		mode CgroupsMode
		ok   bool
	}{
		{"", true}, {"auto", true}, {"systemd", true}, {"none", true},
		{"Systemd", false}, {"cgroup", false}, {"yes", false},
	} {
		err := tc.mode.Validate()
		if (err == nil) != tc.ok {
			t.Errorf("Validate(%q) = %v, want ok=%v", tc.mode, err, tc.ok)
		}
		if err != nil && !strings.Contains(err.Error(), "cgroups must be auto, systemd or none") {
			t.Errorf("Validate(%q) error %q doesn't name the key and allowed values", tc.mode, err)
		}
	}
}

// TestRateLimitsValidate: the daemon refuses budgets that could never admit a
// request instead of starting with an API that answers 429 to everything.
func TestRateLimitsValidate(t *testing.T) {
	for _, tc := range []struct {
		yaml string
		ok   bool
	}{
		{"", true},
		{"create: {per_minute: 120}", true},
		{"exec: {per_minute: 0.5, burst: 1}", true},
		{"read: {burst: 0.5}", false},
		{"create: {per_minute: -1}", false},
		{"exec: {per_minute: .inf}", false},
	} {
		var l RateLimits
		if err := yaml.Unmarshal([]byte(tc.yaml), &l); err != nil {
			t.Fatal(err)
		}
		if err := l.Validate(); (err == nil) != tc.ok {
			t.Errorf("%q: Validate() = %v, want ok=%v", tc.yaml, err, tc.ok)
		}
	}
}
