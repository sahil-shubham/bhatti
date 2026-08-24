package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

// cliTest holds the test server + helpers for CLI integration tests.
// The CLI binary talks to a real bhatti HTTP server — either local or remote
// via Cloudflare tunnel. Config is loaded from ~/.bhatti/config.yaml.
type cliTest struct {
	t       *testing.T
	bhatti  string // path to bhatti binary
	baseURL string // daemon URL
	token   string // auth token
}

// run executes a bhatti CLI command and returns stdout, stderr, exit code.
func (c *cliTest) run(args ...string) (stdout, stderr string, exitCode int) {
	c.t.Helper()
	cmd := exec.Command(c.bhatti, args...)
	cmd.Env = append(os.Environ(),
		"BHATTI_URL="+c.baseURL,
		"BHATTI_TOKEN="+c.token,
	)
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	exitCode = 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			c.t.Fatalf("run %v: %v", args, err)
		}
	}
	return outBuf.String(), errBuf.String(), exitCode
}

// runJSON executes a bhatti CLI command with --json and unmarshals the output.
func (c *cliTest) runJSON(v any, args ...string) (exitCode int) {
	c.t.Helper()
	fullArgs := append([]string{"--json"}, args...)
	stdout, stderr, code := c.run(fullArgs...)
	if code != 0 {
		c.t.Fatalf("run %v: exit %d\nstdout: %s\nstderr: %s", args, code, stdout, stderr)
	}
	if err := json.Unmarshal([]byte(stdout), v); err != nil {
		c.t.Fatalf("json unmarshal %v: %v\nraw: %s", args, err, stdout)
	}
	return code
}

// setupCLITest builds the bhatti binary in a temp dir and loads connection
// config from ~/.bhatti/config.yaml (api_url + auth_token). This lets CLI
// tests run from any machine — the dev laptop, CI, or on the server itself.
//
// Skip conditions:
//   - No config file with api_url + auth_token → skip (no daemon to test against)
//   - Daemon unreachable → skip
func setupCLITest(t *testing.T) *cliTest {
	t.Helper()

	// Load config — same file the real CLI uses
	cfg, err := pkg.LoadConfig()
	if err != nil || cfg == nil {
		t.Skip("no bhatti config found")
	}

	baseURL := cfg.APIURL
	token := cfg.AuthToken

	// Allow env var overrides for CI
	if v := os.Getenv("BHATTI_URL"); v != "" {
		baseURL = v
	}
	if v := os.Getenv("BHATTI_TOKEN"); v != "" {
		token = v
	}

	if baseURL == "" || token == "" {
		t.Skip("bhatti config missing api_url or auth_token")
	}

	// Verify daemon is reachable
	client := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequest("GET", baseURL+"/health", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Skipf("bhatti daemon not reachable at %s: %v", baseURL, err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Skipf("bhatti daemon unhealthy: %d", resp.StatusCode)
	}

	// Build the binary from source (tests the actual compiled code)
	binPath := filepath.Join(t.TempDir(), "bhatti")
	build := exec.Command("go", "build", "-o", binPath, "./cmd/bhatti/")
	build.Dir = projectRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build bhatti: %s\n%v", out, err)
	}

	return &cliTest{
		t:       t,
		bhatti:  binPath,
		baseURL: baseURL,
		token:   token,
	}
}

// createdID extracts the sandbox identifier from `create`'s human output.
// Current format: "sandbox/<name> created (...)" — strip the resource prefix;
// the name resolves server-side everywhere an ID does. (Tests that need the
// real ID should use runJSON on create instead.)
func createdID(stdout string) string {
	f := strings.Fields(stdout)
	if len(f) == 0 {
		return ""
	}
	return strings.TrimPrefix(f[0], "sandbox/")
}

// projectRoot returns the repo root by walking up from the test file.
func projectRoot(t *testing.T) string {
	t.Helper()
	// We're in cmd/bhatti/, go up two levels
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find project root (go.mod)")
		}
		dir = parent
	}
}

// --- Tests ---

func TestCLICreate(t *testing.T) {
	c := setupCLITest(t)

	stdout, _, code := c.run("create", "--name", "cli-test-create")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	// Output: ID\tNAME\tIP
	parts := strings.Fields(stdout)
	if len(parts) < 2 {
		t.Fatalf("unexpected output: %q", stdout)
	}
	sbID := parts[0]
	if sbID == "" {
		t.Fatal("no sandbox ID in output")
	}
	t.Logf("created: %s", strings.TrimSpace(stdout))

	// Cleanup
	t.Cleanup(func() { c.run("destroy", "-y", sbID) })
}

func TestCLIList(t *testing.T) {
	c := setupCLITest(t)

	// Create a sandbox
	stdout, _, _ := c.run("create", "--name", "cli-test-list")
	sbID := createdID(stdout)
	t.Cleanup(func() { c.run("destroy", "-y", sbID) })

	// List
	stdout, _, code := c.run("list")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stdout, "cli-test-list") {
		t.Errorf("sandbox not in list: %s", stdout)
	}
	t.Logf("list output:\n%s", stdout)
}

func TestCLIExec(t *testing.T) {
	c := setupCLITest(t)

	stdout, _, _ := c.run("create", "--name", "cli-test-exec")
	sbID := createdID(stdout)
	t.Cleanup(func() { c.run("destroy", "-y", sbID) })

	stdout, stderr, code := c.run("exec", "cli-test-exec", "--", "echo", "hello from cli")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if strings.TrimSpace(stdout) != "hello from cli" {
		t.Errorf("stdout: %q", stdout)
	}
}

func TestCLIExecFailure(t *testing.T) {
	c := setupCLITest(t)

	stdout, _, _ := c.run("create", "--name", "cli-test-execfail")
	sbID := createdID(stdout)
	t.Cleanup(func() { c.run("destroy", "-y", sbID) })

	_, _, code := c.run("exec", "cli-test-execfail", "--", "false")
	if code != 1 {
		t.Errorf("exit code: %d, want 1", code)
	}
}

func TestCLIExecByName(t *testing.T) {
	c := setupCLITest(t)

	stdout, _, _ := c.run("create", "--name", "cli-test-byname")
	sbID := createdID(stdout)
	t.Cleanup(func() { c.run("destroy", "-y", sbID) })

	// Resolve by name
	stdout, _, code := c.run("exec", "cli-test-byname", "--", "hostname")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.TrimSpace(stdout) != "cli-test-byname" {
		t.Errorf("hostname: %q, want cli-test-byname", strings.TrimSpace(stdout))
	}
}

func TestCLIDestroy(t *testing.T) {
	c := setupCLITest(t)

	stdout, _, _ := c.run("create", "--name", "cli-test-destroy")
	sbID := createdID(stdout)

	stdout, _, code := c.run("destroy", "-y", sbID)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stdout, "destroyed") {
		t.Errorf("output: %q", stdout)
	}

	// Verify gone from list
	stdout, _, _ = c.run("list")
	if strings.Contains(stdout, "cli-test-destroy") {
		t.Errorf("sandbox still in list after destroy")
	}
}

func TestCLIDestroyByName(t *testing.T) {
	c := setupCLITest(t)

	stdout, _, _ := c.run("create", "--name", "cli-test-rmname")
	_ = createdID(stdout)

	stdout, _, code := c.run("rm", "-y", "cli-test-rmname")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stdout, "destroyed") {
		t.Errorf("output: %q", stdout)
	}
}

func TestCLIFileWriteRead(t *testing.T) {
	c := setupCLITest(t)

	stdout, _, _ := c.run("create", "--name", "cli-test-file")
	sbID := createdID(stdout)
	t.Cleanup(func() { c.run("destroy", "-y", sbID) })

	// Write via stdin piped to the binary
	content := "hello from cli file write"
	cmd := exec.Command(c.bhatti, "file", "write", "cli-test-file", "/workspace/test.txt")
	cmd.Env = append(os.Environ(), "BHATTI_URL="+c.baseURL, "BHATTI_TOKEN="+c.token)
	cmd.Stdin = strings.NewReader(content)
	var outBuf strings.Builder
	cmd.Stdout = &outBuf
	if err := cmd.Run(); err != nil {
		t.Fatalf("file write: %v", err)
	}
	if !strings.Contains(outBuf.String(), "ok") {
		t.Errorf("write output: %q", outBuf.String())
	}

	// Read
	stdout, _, code := c.run("file", "read", "cli-test-file", "/workspace/test.txt")
	if code != 0 {
		t.Fatalf("file read exit %d", code)
	}
	if stdout != content {
		t.Errorf("read: %q, want %q", stdout, content)
	}
}

func TestCLIFileLS(t *testing.T) {
	c := setupCLITest(t)

	stdout, _, _ := c.run("create", "--name", "cli-test-filels")
	sbID := createdID(stdout)
	t.Cleanup(func() { c.run("destroy", "-y", sbID) })

	// Create some files via exec
	c.run("exec", "cli-test-filels", "--", "sh", "-c",
		"echo a > /workspace/a.txt && echo b > /workspace/b.txt && mkdir /workspace/sub")

	stdout, _, code := c.run("file", "ls", "cli-test-filels", "/workspace/")
	if code != 0 {
		t.Fatalf("file ls exit %d", code)
	}
	if !strings.Contains(stdout, "a.txt") || !strings.Contains(stdout, "b.txt") || !strings.Contains(stdout, "sub") {
		t.Errorf("ls output: %s", stdout)
	}
	t.Logf("ls output:\n%s", stdout)
}

func TestCLIPS(t *testing.T) {
	c := setupCLITest(t)

	// Create sandbox with init script (creates a session)
	stdout, _, _ := c.run("create", "--name", "cli-test-ps", "--init", "sleep 3600")
	sbID := createdID(stdout)
	t.Cleanup(func() { c.run("destroy", "-y", sbID) })

	time.Sleep(2 * time.Second)

	stdout, _, code := c.run("ps", "cli-test-ps")
	if code != 0 {
		t.Fatalf("ps exit %d", code)
	}
	if !strings.Contains(stdout, "init") {
		t.Errorf("init session not in ps output: %s", stdout)
	}
	t.Logf("ps output:\n%s", stdout)
}

// --- Local user command tests (no daemon required) ---

func testLocalStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestGenerateAPIKey(t *testing.T) {
	key := generateAPIKey()
	if !strings.HasPrefix(key, "bht_") {
		t.Errorf("key should have bht_ prefix, got %q", key)
	}
	if len(key) != 4+64 { // "bht_" + 32 bytes hex
		t.Errorf("key length: %d, want %d", len(key), 68)
	}

	// Two keys should be different
	key2 := generateAPIKey()
	if key == key2 {
		t.Error("two generated keys should not be equal")
	}
}

func TestSha256HexCLI(t *testing.T) {
	hash := sha256HexCLI("test")
	if len(hash) != 64 {
		t.Errorf("hash length: %d, want 64", len(hash))
	}
	// Known SHA-256 of "test"
	if hash != "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08" {
		t.Errorf("unexpected hash: %s", hash)
	}
}

func TestUserCreateAndLookup(t *testing.T) {
	st := testLocalStore(t)

	key := generateAPIKey()
	hash := sha256HexCLI(key)

	err := st.CreateUser(store.User{
		ID: "usr_clitest", Name: "cli-user", APIKeyHash: hash,
		MaxSandboxes: 10, MaxCPUsPerSandbox: 2, MaxMemoryMBPerSandbox: 2048,
		SubnetIndex: 1, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Lookup by hash (simulates auth middleware)
	u, err := st.GetUserByKeyHash(hash)
	if err != nil {
		t.Fatal(err)
	}
	if u.Name != "cli-user" || u.MaxSandboxes != 10 {
		t.Fatalf("unexpected user: %+v", u)
	}

	// List
	users, _ := st.ListUsers()
	if len(users) != 1 {
		t.Fatalf("expected 1 user, got %d", len(users))
	}
}

func TestUserRotateKeyFlow(t *testing.T) {
	st := testLocalStore(t)

	oldKey := generateAPIKey()
	oldHash := sha256HexCLI(oldKey)

	st.CreateUser(store.User{
		ID: "usr_rot", Name: "rotate-test", APIKeyHash: oldHash,
		MaxSandboxes: 5, SubnetIndex: 1, CreatedAt: time.Now(),
	})

	// Rotate
	newKey := generateAPIKey()
	newHash := sha256HexCLI(newKey)
	if err := st.RotateUserKey("usr_rot", newHash); err != nil {
		t.Fatal(err)
	}

	// Old key doesn't work
	_, err := st.GetUserByKeyHash(oldHash)
	if err == nil {
		t.Fatal("old key should not work after rotation")
	}

	// New key works
	u, err := st.GetUserByKeyHash(newHash)
	if err != nil {
		t.Fatal(err)
	}
	if u.Name != "rotate-test" {
		t.Fatalf("unexpected user: %+v", u)
	}
}

func TestUserDeleteFlowWithProtection(t *testing.T) {
	st := testLocalStore(t)

	st.CreateUser(store.User{
		ID: "usr_delf", Name: "del-flow", APIKeyHash: "h1",
		MaxSandboxes: 5, SubnetIndex: 1, CreatedAt: time.Now(),
	})

	// Create a sandbox
	st.CreateSandbox(store.Sandbox{
		ID: "sb_delf", Name: "del-sb", Status: "running",
		CreatedBy: "usr_delf", CreatedAt: time.Now(),
	})

	// Delete should fail
	err := st.DeleteUser("usr_delf")
	if err == nil || !strings.Contains(err.Error(), "active sandbox") {
		t.Fatalf("expected deletion refused, got: %v", err)
	}

	// Destroy sandbox
	st.DeleteSandboxByID("sb_delf")

	// Now delete succeeds
	if err := st.DeleteUser("usr_delf"); err != nil {
		t.Fatal(err)
	}

	users, _ := st.ListUsers()
	if len(users) != 0 {
		t.Fatal("expected 0 users after delete")
	}
}

func TestCLISecretCRUD(t *testing.T) {
	c := setupCLITest(t)

	// Set
	stdout, _, code := c.run("secret", "set", "cli-test-key", "cli-test-val")
	if code != 0 {
		t.Fatalf("secret set exit %d", code)
	}
	if !strings.Contains(stdout, "ok") {
		t.Errorf("set output: %q", stdout)
	}

	// List
	stdout, _, code = c.run("secret", "list")
	if code != 0 {
		t.Fatalf("secret list exit %d", code)
	}
	if !strings.Contains(stdout, "cli-test-key") {
		t.Errorf("secret not in list: %s", stdout)
	}

	// Delete
	stdout, _, code = c.run("secret", "delete", "-y", "cli-test-key")
	if code != 0 {
		t.Fatalf("secret delete exit %d", code)
	}

	// Verify gone
	stdout, _, _ = c.run("secret", "list")
	if strings.Contains(stdout, "cli-test-key") {
		t.Error("secret still in list after delete")
	}
}

// ==========================================================================

func TestCLIInitScript(t *testing.T) {
	c := setupCLITest(t)

	sbName := fmt.Sprintf("cli-init-%d", time.Now().UnixNano()%100000)

	stdout, stderr, code := c.run("create", "--name", sbName,
		"--init", "echo init-ran > /tmp/init-marker")
	if code != 0 {
		t.Fatalf("create with init exit %d: %s", code, stderr)
	}
	sbID := createdID(stdout)
	t.Cleanup(func() { c.run("destroy", "-y", sbID) })

	// Wait for init to run
	time.Sleep(3 * time.Second)

	stdout, _, code = c.run("exec", sbName, "--", "cat", "/tmp/init-marker")
	if code != 0 || !strings.Contains(stdout, "init-ran") {
		t.Fatalf("init marker not found: exit=%d out=%q", code, stdout)
	}
	t.Log("✓ init script ran successfully")
}

// ==========================================================================
// v0.3 CLI Tests — JSON Output
// ==========================================================================

func TestCLIJSONOutput(t *testing.T) {
	c := setupCLITest(t)

	sbName := fmt.Sprintf("cli-json-%d", time.Now().UnixNano()%100000)

	// Create with --json
	stdout, stderr, code := c.run("--json", "create", "--name", sbName)
	if code != 0 {
		t.Fatalf("create --json exit %d: %s", code, stderr)
	}

	var sb map[string]interface{}
	if err := json.Unmarshal([]byte(stdout), &sb); err != nil {
		t.Fatalf("json parse create: %v\nraw: %s", err, stdout)
	}
	sbID, ok := sb["id"].(string)
	if !ok || sbID == "" {
		t.Fatalf("missing id in json: %v", sb)
	}
	t.Cleanup(func() { c.run("destroy", "-y", sbID) })
	t.Log("✓ create --json returns valid JSON with id")

	// List with --json
	stdout, _, code = c.run("--json", "list")
	if code != 0 {
		t.Fatalf("list --json exit %d", code)
	}
	var list []map[string]interface{}
	if err := json.Unmarshal([]byte(stdout), &list); err != nil {
		t.Fatalf("json parse list: %v\nraw: %s", err, stdout)
	}
	found := false
	for _, s := range list {
		if s["name"] == sbName {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("sandbox not in json list")
	}
	t.Log("✓ list --json returns valid JSON array with sandbox")

	// Volume list with --json
	stdout, _, code = c.run("--json", "volume", "list")
	if code != 0 {
		t.Fatalf("volume list --json exit %d", code)
	}
	var vols []interface{}
	if err := json.Unmarshal([]byte(stdout), &vols); err != nil {
		t.Fatalf("json parse volume list: %v\nraw: %s", err, stdout)
	}
	t.Log("✓ volume list --json returns valid JSON")
}

// ==========================================================================
// v0.3 CLI Tests — Error Cases
// ==========================================================================

func TestCLICreateNonexistentImage(t *testing.T) {
	c := setupCLITest(t)

	_, stderr, code := c.run("create", "--name", "cli-bad-img", "--image", "nonexistent-image-xyz")
	if code == 0 {
		t.Cleanup(func() { c.run("destroy", "-y", "cli-bad-img") })
		t.Fatal("create with nonexistent image should fail")
	}
	if !strings.Contains(stderr, "not found") {
		t.Fatalf("expected 'not found' error: %s", stderr)
	}
	t.Log("✓ create with nonexistent image rejected")
}

func TestCLICreateNonexistentVolume(t *testing.T) {
	c := setupCLITest(t)

	_, stderr, code := c.run("create", "--name", "cli-bad-vol", "--volume", "nonexistent-vol-xyz:/data")
	if code == 0 {
		t.Cleanup(func() { c.run("destroy", "-y", "cli-bad-vol") })
		t.Fatal("create with nonexistent volume should fail")
	}
	if !strings.Contains(stderr, "not found") {
		t.Fatalf("expected 'not found' error: %s", stderr)
	}
	t.Log("✓ create with nonexistent volume rejected")
}

func TestCLISandboxNameValidation(t *testing.T) {
	c := setupCLITest(t)

	badNames := []string{"../etc/passwd", "a/b", "", "a b"}
	for _, name := range badNames {
		if name == "" {
			continue // empty name gets auto-generated
		}
		_, _, code := c.run("create", "--name", name)
		if code == 0 {
			c.run("destroy", "-y", name)
			t.Fatalf("name %q should be rejected", name)
		}
	}
	t.Log("✓ invalid sandbox names rejected")
}

func TestCLIVolumeNameValidation(t *testing.T) {
	c := setupCLITest(t)

	_, _, code := c.run("volume", "create", "--name", "../escape", "--size", "64")
	if code == 0 {
		c.run("volume", "delete", "-y", "../escape")
		t.Fatal("volume name with path traversal should be rejected")
	}
	t.Log("✓ invalid volume name rejected")
}

func TestCLIExecNonexistentSandbox(t *testing.T) {
	c := setupCLITest(t)

	_, _, code := c.run("exec", "nonexistent-sandbox-xyz", "--", "echo", "hi")
	if code == 0 {
		t.Fatal("exec on nonexistent sandbox should fail")
	}
	t.Log("✓ exec on nonexistent sandbox rejected")
}

func TestCLIDestroyNonexistentSandbox(t *testing.T) {
	c := setupCLITest(t)

	_, _, code := c.run("destroy", "-y", "nonexistent-sandbox-xyz")
	if code == 0 {
		t.Fatal("destroy nonexistent sandbox should fail")
	}
	t.Log("✓ destroy nonexistent sandbox rejected")
}

// ==========================================================================
// v0.3 CLI Tests — Version
// ==========================================================================

func TestCLIVersion(t *testing.T) {
	c := setupCLITest(t)

	stdout, _, code := c.run("version")
	if code != 0 {
		t.Fatalf("version exit %d", code)
	}
	if !strings.Contains(stdout, "bhatti") {
		t.Fatalf("version output: %s", stdout)
	}
	if !strings.Contains(stdout, "api:") {
		t.Fatalf("version should show api endpoint: %s", stdout)
	}
	t.Logf("✓ version: %s", strings.TrimSpace(stdout))
}

// TestCompareVersions tests the semver comparison used for CLI update checks.
// Does not require a running server.
func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"0.4.0", "0.4.0", 0},
		{"0.3.9", "0.4.0", -1},
		{"0.4.1", "0.4.0", 1},
		{"v0.4.0", "0.4.0", 0},
		{"0.4.0", "v0.4.0", 0},
		{"1.0.0", "0.99.99", 1},
		{"0.3.0", "0.4.0", -1},
		{"0.4.0", "0.4.1", -1},
		{"v1.2.3", "v1.2.4", -1},
		{"0.4", "0.4.0", 0},   // missing patch = 0
		{"0.4", "0.4.1", -1},
	}
	for _, tt := range tests {
		got := compareVersions(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}


