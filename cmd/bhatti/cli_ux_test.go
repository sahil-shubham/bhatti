package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ==========================================================================
// Release B — CLI/UX Integration Tests (PLAN-cli-ux.md, B16)
//
// Tests are organized in three tiers:
//   Tier 1: Must-have for HN launch (first-impression path)
//   Tier 2: Important polish (zero-coverage commands + new B flags)
//   Tier 3: Edge cases for long-term safety
//
// Tests that depend on unshipped B items call t.Skip("requires B<N>").
// Remove the skip as each B item lands.
// ==========================================================================

// --- Tier 1: Must-have for launch ---

func TestCLICreateVerboseOutput(t *testing.T) {
	c := setupCLITest(t)

	name := fmt.Sprintf("cli-verbose-%d", time.Now().UnixNano()%100000)
	stdout, stderr, code := c.run("create", "--name", name)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	t.Cleanup(func() { c.run("destroy", name, "-y") })

	// Must show the new multi-line format
	if !strings.Contains(stdout, "sandbox/"+name+" created") {
		t.Errorf("expected 'sandbox/%s created' line, got:\n%s", name, stdout)
	}
	if !strings.Contains(stdout, "IP:") {
		t.Errorf("expected IP line, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Shell:") {
		t.Errorf("expected Shell hint line, got:\n%s", stdout)
	}
	// Must show resources (vCPU + memory, disk only if explicitly set)
	if !strings.Contains(stdout, "vCPU") || !strings.Contains(stdout, "MB") {
		t.Errorf("expected resource summary (vCPU, MB), got:\n%s", stdout)
	}
}

func TestCLICreateIdempotent(t *testing.T) {
	c := setupCLITest(t)

	name := fmt.Sprintf("cli-idemp-%d", time.Now().UnixNano()%100000)
	stdout, _, code := c.run("create", "--name", name)
	if code != 0 {
		t.Fatalf("first create failed: exit %d", code)
	}
	t.Cleanup(func() { c.run("destroy", name, "-y") })

	// Second create with same name — should not error
	stdout, _, code = c.run("create", "--name", name)
	if code != 0 {
		t.Fatalf("second create should succeed (idempotent): exit %d", code)
	}
	if !strings.Contains(stdout, "unchanged") {
		t.Errorf("expected 'unchanged' message, got:\n%s", stdout)
	}
}

func TestCLIStreamingExecNDJSON(t *testing.T) {
	c := setupCLITest(t)

	name := fmt.Sprintf("cli-stream-%d", time.Now().UnixNano()%100000)
	stdout, _, _ := c.run("create", "--name", name)
	if stdout == "" {
		t.Fatal("create failed")
	}
	t.Cleanup(func() { c.run("destroy", name, "-y") })

	// BHATTI_FORCE_STREAM=1 bypasses the TTY check in tests
	stdout, stderr, code := c.runWithEnv(
		[]string{"BHATTI_FORCE_STREAM=1"},
		"exec", name, "--", "sh", "-c",
		`echo line1; sleep 0.1; echo line2; sleep 0.1; echo line3`,
	)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) < 3 {
		t.Fatalf("expected 3+ lines of streaming output, got %d:\n%s", len(lines), stdout)
	}
	if !strings.Contains(stdout, "line1") || !strings.Contains(stdout, "line3") {
		t.Errorf("missing expected lines:\n%s", stdout)
	}
}

func TestCLIErrorExecOnStopped(t *testing.T) {
	c := setupCLITest(t)

	name := fmt.Sprintf("cli-errstop-%d", time.Now().UnixNano()%100000)
	c.run("create", "--name", name)
	t.Cleanup(func() { c.run("destroy", name, "-y") })

	c.run("stop", name)

	_, stderr, code := c.run("exec", name, "--", "echo", "hi")
	if code == 0 {
		t.Fatal("exec on stopped sandbox should fail")
	}
	if !strings.Contains(stderr, "not running") {
		t.Errorf("expected 'not running' in error, got:\n%s", stderr)
	}
	if !strings.Contains(stderr, "bhatti start") {
		t.Errorf("expected recovery hint 'bhatti start' in error, got:\n%s", stderr)
	}
}

func TestCLIStopStartConfirmVerbs(t *testing.T) {
	c := setupCLITest(t)

	name := fmt.Sprintf("cli-verbs-%d", time.Now().UnixNano()%100000)
	c.run("create", "--name", name)
	t.Cleanup(func() { c.run("destroy", name, "-y") })

	// Stop
	stdout, _, code := c.run("stop", name)
	if code != 0 {
		t.Fatalf("stop exit %d", code)
	}
	if !strings.Contains(stdout, "sandbox/"+name+" stopped") {
		t.Errorf("expected 'sandbox/%s stopped', got: %q", name, stdout)
	}

	// Stop again — idempotent
	_, _, code = c.run("stop", name)
	if code != 0 {
		t.Errorf("double stop should not error, got exit %d", code)
	}

	// Start
	stdout, _, code = c.run("start", name)
	if code != 0 {
		t.Fatalf("start exit %d", code)
	}
	if !strings.Contains(stdout, "sandbox/"+name+" started") {
		t.Errorf("expected 'sandbox/%s started', got: %q", name, stdout)
	}

	// Start again — idempotent
	_, _, code = c.run("start", name)
	if code != 0 {
		t.Errorf("double start should not error, got exit %d", code)
	}

	// Destroy
	stdout, _, code = c.run("destroy", name, "-y")
	if code != 0 {
		t.Fatalf("destroy exit %d", code)
	}
	if !strings.Contains(stdout, "sandbox/"+name+" destroyed") {
		t.Errorf("expected 'sandbox/%s destroyed', got: %q", name, stdout)
	}
}

func TestCLIStopStartRoundTrip(t *testing.T) {
	c := setupCLITest(t)

	name := fmt.Sprintf("cli-roundtrip-%d", time.Now().UnixNano()%100000)
	c.run("create", "--name", name)
	t.Cleanup(func() { c.run("destroy", name, "-y") })

	// Write marker
	c.run("exec", name, "--", "sh", "-c", "echo roundtrip-data > /tmp/marker.txt")

	// Stop (snapshot)
	_, _, code := c.run("stop", name)
	if code != 0 {
		t.Fatalf("stop exit %d", code)
	}

	// Start (restore)
	_, _, code = c.run("start", name)
	if code != 0 {
		t.Fatalf("start exit %d", code)
	}

	// Read marker — data must survive
	stdout, _, code := c.run("exec", name, "--", "cat", "/tmp/marker.txt")
	if code != 0 || !strings.Contains(stdout, "roundtrip-data") {
		t.Fatalf("data did not survive stop/start: exit=%d out=%q", code, stdout)
	}
	t.Log("✓ data survives stop/start round-trip")
}

func TestCLIInspectRichOutput(t *testing.T) {
	c := setupCLITest(t)

	name := fmt.Sprintf("cli-inspect-%d", time.Now().UnixNano()%100000)
	_, _, code := c.run("create", "--name", name, "--cpus", "2", "--memory", "2048", "--disk-size", "4096")
	if code != 0 {
		t.Fatalf("create exit %d", code)
	}
	t.Cleanup(func() { c.run("destroy", name, "-y") })

	// Write some data to show disk usage
	c.run("exec", name, "--", "sh", "-c", "dd if=/dev/zero of=/tmp/fill bs=1M count=10 2>/dev/null")

	// Text output
	stdout, _, code := c.run("inspect", name)
	if code != 0 {
		t.Fatalf("inspect exit %d", code)
	}
	for _, field := range []string{"Name:", "ID:", "Status:", "Image:", "Created:", "CPUs:", "Memory:", "Disk:", "IP:"} {
		if !strings.Contains(stdout, field) {
			t.Errorf("inspect missing field %q:\n%s", field, stdout)
		}
	}
	// Disk should show used/free
	if !strings.Contains(stdout, "used") || !strings.Contains(stdout, "free") {
		t.Errorf("expected disk usage (used/free), got:\n%s", stdout)
	}

	// JSON output
	stdout, _, code = c.run("--json", "inspect", name)
	if code != 0 {
		t.Fatalf("inspect --json exit %d", code)
	}
	var sb map[string]interface{}
	if err := json.Unmarshal([]byte(stdout), &sb); err != nil {
		t.Fatalf("json parse: %v\nraw: %s", err, stdout)
	}
	for _, field := range []string{"cpus", "memory_mb", "disk_size_mb", "image"} {
		if _, ok := sb[field]; !ok {
			t.Errorf("JSON missing field %q", field)
		}
	}
	if cpus, _ := sb["cpus"].(float64); cpus != 2 {
		t.Errorf("cpus: %v, want 2", sb["cpus"])
	}
	if mem, _ := sb["memory_mb"].(float64); mem != 2048 {
		t.Errorf("memory_mb: %v, want 2048", sb["memory_mb"])
	}
	// Created with no --image, so the user-visible image name should be the
	// "minimal" default — never empty, never a stale value from another path.
	if img, _ := sb["image"].(string); img != "minimal" {
		t.Errorf("image: %q, want \"minimal\"", img)
	}
}

// TestCLIInspectImageNonDefault locks in the fix for the direct-creation
// path dropping req.Image on the floor. Before the fix, `bhatti inspect`
// always reported "minimal" regardless of --image, because the server
// built engine.SandboxSpec without copying req.Image into spec.Image and
// the storage layer fell back to "minimal".
func TestCLIInspectImageNonDefault(t *testing.T) {
	c := setupCLITest(t)

	// Use a non-default tier image that install.sh registers as a system
	// image. "browser" is the canonical second tier; if the test rig only
	// has "minimal", skip rather than false-fail.
	const wantImage = "browser"
	if _, _, code := c.run("image", "list"); code != 0 {
		t.Skip("image list unavailable on this rig")
	}
	listOut, _, _ := c.run("image", "list")
	if !strings.Contains(listOut, wantImage) {
		t.Skipf("image %q not registered on this rig", wantImage)
	}

	name := fmt.Sprintf("cli-img-%d", time.Now().UnixNano()%100000)
	_, stderr, code := c.run("create", "--name", name, "--image", wantImage)
	if code != 0 {
		t.Fatalf("create --image %s exit %d: %s", wantImage, code, stderr)
	}
	t.Cleanup(func() { c.run("destroy", name, "-y") })

	// Text output must show the real image, not "minimal".
	stdout, _, code := c.run("inspect", name)
	if code != 0 {
		t.Fatalf("inspect exit %d", code)
	}
	if !strings.Contains(stdout, "Image:") || !strings.Contains(stdout, wantImage) {
		t.Errorf("inspect should show Image: %s, got:\n%s", wantImage, stdout)
	}
	if strings.Contains(stdout, "Image:      minimal") {
		t.Errorf("inspect reported minimal but sandbox was created with --image %s:\n%s", wantImage, stdout)
	}

	// JSON must agree.
	jsonOut, _, _ := c.run("--json", "inspect", name)
	var sb map[string]interface{}
	if err := json.Unmarshal([]byte(jsonOut), &sb); err != nil {
		t.Fatalf("json parse: %v\nraw: %s", err, jsonOut)
	}
	if got, _ := sb["image"].(string); got != wantImage {
		t.Errorf("json image: %q, want %q", got, wantImage)
	}
}

func TestCLIPorts(t *testing.T) {
	c := setupCLITest(t)

	name := fmt.Sprintf("cli-ports-%d", time.Now().UnixNano()%100000)
	c.run("create", "--name", name)
	t.Cleanup(func() { c.run("destroy", name, "-y") })

	// Start a listener
	c.run("exec", name, "--", "sh", "-c",
		"python3 -m http.server 9090 &>/dev/null &")
	time.Sleep(1 * time.Second)

	// Text output
	stdout, _, code := c.run("ports", name)
	if code != 0 {
		t.Fatalf("ports exit %d", code)
	}
	if !strings.Contains(stdout, "9090") {
		t.Errorf("expected port 9090 in output:\n%s", stdout)
	}

	// JSON output
	stdout, _, code = c.run("--json", "ports", name)
	if code != 0 {
		t.Fatalf("ports --json exit %d", code)
	}
	var portsJSON []map[string]interface{}
	if err := json.Unmarshal([]byte(stdout), &portsJSON); err != nil {
		t.Fatalf("json parse: %v\nraw: %s", err, stdout)
	}
}

func TestCLIListCleanDefault(t *testing.T) {
	c := setupCLITest(t)

	name := fmt.Sprintf("cli-listclean-%d", time.Now().UnixNano()%100000)
	c.run("create", "--name", name)
	t.Cleanup(func() { c.run("destroy", name, "-y") })

	stdout, _, code := c.run("list")
	if code != 0 {
		t.Fatalf("list exit %d", code)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected header + data, got:\n%s", stdout)
	}
	header := lines[0]
	// Default columns: NAME STATUS THERMAL IP — no ID
	if !strings.Contains(header, "NAME") || !strings.Contains(header, "STATUS") {
		t.Errorf("expected NAME/STATUS columns, got: %q", header)
	}
	if strings.Contains(header, "ID") {
		t.Errorf("ID column should not be in default list: %q", header)
	}
}

func TestCLIListWideMode(t *testing.T) {
	c := setupCLITest(t)

	name := fmt.Sprintf("cli-listwide-%d", time.Now().UnixNano()%100000)
	c.run("create", "--name", name, "--cpus", "2", "--memory", "2048")
	t.Cleanup(func() { c.run("destroy", name, "-y") })

	stdout, _, code := c.run("list", "-o", "wide")
	if code != 0 {
		t.Fatalf("list -o wide exit %d", code)
	}
	header := strings.Split(stdout, "\n")[0]
	for _, col := range []string{"NAME", "STATUS", "CPUS", "MEMORY", "DISK", "IMAGE"} {
		if !strings.Contains(header, col) {
			t.Errorf("wide header missing %q: %q", col, header)
		}
	}
	// Find the data row for our sandbox
	for _, line := range strings.Split(stdout, "\n")[1:] {
		if strings.Contains(line, name) {
			if !strings.Contains(line, "2") { // cpus
				t.Errorf("expected CPUS=2 in row: %q", line)
			}
			if !strings.Contains(line, "2048") { // memory
				t.Errorf("expected MEMORY=2048 in row: %q", line)
			}
			return
		}
	}
	t.Errorf("sandbox %q not found in wide list:\n%s", name, stdout)
}

func TestCLIForceStart(t *testing.T) {
	c := setupCLITest(t)

	name := fmt.Sprintf("cli-force-%d", time.Now().UnixNano()%100000)
	c.run("create", "--name", name)
	t.Cleanup(func() { c.run("destroy", name, "-y") })

	c.run("stop", name)

	// --force should be accepted and start should succeed
	_, _, code := c.run("start", "--force", name)
	if code != 0 {
		t.Fatalf("start --force exit %d", code)
	}

	stdout, _, code := c.run("exec", name, "--", "echo", "force-ok")
	if code != 0 || !strings.Contains(stdout, "force-ok") {
		t.Fatalf("exec after force start: exit=%d out=%q", code, stdout)
	}
}

// --- Tier 2: Important polish ---

func TestCLIDetachedExec(t *testing.T) {
	c := setupCLITest(t)

	name := fmt.Sprintf("cli-detach-%d", time.Now().UnixNano()%100000)
	c.run("create", "--name", name)
	t.Cleanup(func() { c.run("destroy", name, "-y") })

	stdout, _, code := c.run("exec", name, "--detach", "--", "sleep", "300")
	if code != 0 {
		t.Fatalf("detached exec exit %d", code)
	}
	if !strings.Contains(stdout, "pid") {
		t.Errorf("expected pid in output: %q", stdout)
	}
	if !strings.Contains(stdout, "output") {
		t.Errorf("expected output file path in output: %q", stdout)
	}
}

func TestCLIHugepagesFlag(t *testing.T) {
	c := setupCLITest(t)

	name := fmt.Sprintf("cli-huge-%d", time.Now().UnixNano()%100000)
	_, _, code := c.run("create", "--name", name, "--hugepages")
	if code != 0 {
		t.Fatalf("create --hugepages exit %d", code)
	}
	t.Cleanup(func() { c.run("destroy", name, "-y") })

	// Verify via inspect JSON
	stdout, _, _ := c.run("--json", "inspect", name)
	var sb map[string]interface{}
	json.Unmarshal([]byte(stdout), &sb)
	if sb["hugepages"] != true {
		t.Errorf("hugepages: %v, want true", sb["hugepages"])
	}
}

func TestCLIVolumeClone(t *testing.T) {
	c := setupCLITest(t)

	src := fmt.Sprintf("cli-clsrc-%d", time.Now().UnixNano()%100000)
	dst := fmt.Sprintf("cli-cldst-%d", time.Now().UnixNano()%100000)
	sb1 := fmt.Sprintf("cli-clsb1-%d", time.Now().UnixNano()%100000)
	sb2 := fmt.Sprintf("cli-clsb2-%d", time.Now().UnixNano()%100000)

	// Create volume, write data
	c.run("volume", "create", "--name", src, "--size", "64")
	t.Cleanup(func() {
		c.run("volume", "delete", src)
		c.run("volume", "delete", dst)
	})

	c.run("create", "--name", sb1, "--volume", src+":/data")
	c.run("exec", sb1, "--", "sh", "-c", "echo clone-marker > /data/file.txt")
	c.run("destroy", sb1, "-y")

	// Clone
	_, _, code := c.run("volume", "clone", src, "--name", dst)
	if code != 0 {
		t.Fatalf("volume clone exit %d", code)
	}

	// Verify data in clone
	c.run("create", "--name", sb2, "--volume", dst+":/data")
	t.Cleanup(func() { c.run("destroy", sb2, "-y") })

	stdout, _, code := c.run("exec", sb2, "--", "cat", "/data/file.txt")
	if code != 0 || !strings.Contains(stdout, "clone-marker") {
		t.Fatalf("clone data missing: exit=%d out=%q", code, stdout)
	}
}

func TestCLICreateWithSecret(t *testing.T) {
	c := setupCLITest(t)

	secretName := fmt.Sprintf("cli-sec-%d", time.Now().UnixNano()%100000)
	sbName := fmt.Sprintf("cli-secbox-%d", time.Now().UnixNano()%100000)

	// Store secret
	_, _, code := c.run("secret", "set", secretName, "secret-value-42")
	if code != 0 {
		t.Fatalf("secret set exit %d", code)
	}
	t.Cleanup(func() { c.run("secret", "delete", secretName) })

	// Create with --secret
	_, _, code = c.run("create", "--name", sbName, "--secret", secretName)
	if code != 0 {
		t.Fatalf("create --secret exit %d", code)
	}
	t.Cleanup(func() { c.run("destroy", sbName, "-y") })

	// Secret should be available as env var
	stdout, _, code := c.run("exec", sbName, "--", "printenv", secretName)
	if code != 0 {
		t.Fatalf("printenv exit %d", code)
	}
	if strings.TrimSpace(stdout) != "secret-value-42" {
		t.Errorf("secret value: %q, want %q", strings.TrimSpace(stdout), "secret-value-42")
	}
}

func TestCLICreateWithFile(t *testing.T) {
	c := setupCLITest(t)

	sbName := fmt.Sprintf("cli-fileinj-%d", time.Now().UnixNano()%100000)

	// Write local temp file
	tmpFile := filepath.Join(t.TempDir(), "inject.conf")
	os.WriteFile(tmpFile, []byte("injected-config-content"), 0644)

	// Create with --file
	_, _, code := c.run("create", "--name", sbName,
		"--file", tmpFile+":/app/config.conf",
		"--init", "cp /app/config.conf /tmp/init-proof")
	if code != 0 {
		t.Fatalf("create --file exit %d", code)
	}
	t.Cleanup(func() { c.run("destroy", sbName, "-y") })

	time.Sleep(2 * time.Second) // wait for init

	// File should exist with correct content
	stdout, _, code := c.run("exec", sbName, "--", "cat", "/app/config.conf")
	if code != 0 || strings.TrimSpace(stdout) != "injected-config-content" {
		t.Fatalf("file content: exit=%d out=%q", code, stdout)
	}

	// File should have been available when init ran
	stdout, _, code = c.run("exec", sbName, "--", "cat", "/tmp/init-proof")
	if code != 0 || strings.TrimSpace(stdout) != "injected-config-content" {
		t.Fatalf("init proof missing (file not available at boot): exit=%d out=%q", code, stdout)
	}
}

func TestCLIExitCodeContract(t *testing.T) {
	c := setupCLITest(t)

	name := fmt.Sprintf("cli-exit-%d", time.Now().UnixNano()%100000)
	c.run("create", "--name", name)
	t.Cleanup(func() { c.run("destroy", name, "-y") })

	// true → 0
	_, _, code := c.run("exec", name, "--", "true")
	if code != 0 {
		t.Errorf("exec true: exit %d, want 0", code)
	}

	// false → 1
	_, _, code = c.run("exec", name, "--", "false")
	if code != 1 {
		t.Errorf("exec false: exit %d, want 1", code)
	}

	// exit 42 → 42
	_, _, code = c.run("exec", name, "--", "sh", "-c", "exit 42")
	if code != 42 {
		t.Errorf("exec exit 42: exit %d, want 42", code)
	}

	// nonexistent sandbox → non-zero
	_, _, code = c.run("exec", "nonexistent-sb-xyz", "--", "echo", "hi")
	if code == 0 {
		t.Error("exec on nonexistent sandbox should fail")
	}

	// destroy nonexistent → non-zero
	_, _, code = c.run("destroy", "nonexistent-sb-xyz", "-y")
	if code == 0 {
		t.Error("destroy nonexistent should fail")
	}
}

func TestCLIJSONCreateInspectListPorts(t *testing.T) {
	c := setupCLITest(t)

	name := fmt.Sprintf("cli-jsonall-%d", time.Now().UnixNano()%100000)

	// Create --json
	stdout, _, code := c.run("--json", "create", "--name", name)
	if code != 0 {
		t.Fatalf("create --json exit %d", code)
	}
	t.Cleanup(func() { c.run("destroy", name, "-y") })

	var createResp map[string]interface{}
	if err := json.Unmarshal([]byte(stdout), &createResp); err != nil {
		t.Fatalf("create json: %v\nraw: %s", err, stdout)
	}
	if createResp["id"] == nil || createResp["name"] == nil {
		t.Errorf("create json missing id/name: %v", createResp)
	}

	// Inspect --json
	stdout, _, code = c.run("--json", "inspect", name)
	if code != 0 {
		t.Fatalf("inspect --json exit %d", code)
	}
	var inspectResp map[string]interface{}
	if err := json.Unmarshal([]byte(stdout), &inspectResp); err != nil {
		t.Fatalf("inspect json: %v", err)
	}
	for _, key := range []string{"id", "name", "status", "ip"} {
		if inspectResp[key] == nil {
			t.Errorf("inspect json missing %q", key)
		}
	}

	// List --json
	stdout, _, code = c.run("--json", "list")
	if code != 0 {
		t.Fatalf("list --json exit %d", code)
	}
	var listResp []map[string]interface{}
	if err := json.Unmarshal([]byte(stdout), &listResp); err != nil {
		t.Fatalf("list json: %v", err)
	}
	found := false
	for _, s := range listResp {
		if s["name"] == name {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("sandbox %q not in json list", name)
	}
}

func TestCLIEditKeepHot(t *testing.T) {
	c := setupCLITest(t)

	name := fmt.Sprintf("cli-edit-%d", time.Now().UnixNano()%100000)
	c.run("create", "--name", name)
	t.Cleanup(func() { c.run("destroy", name, "-y") })

	// Set keep-hot
	_, _, code := c.run("edit", name, "--keep-hot")
	if code != 0 {
		t.Fatalf("edit --keep-hot exit %d", code)
	}

	// Verify via inspect
	stdout, _, _ := c.run("--json", "inspect", name)
	var sb map[string]interface{}
	json.Unmarshal([]byte(stdout), &sb)
	if sb["keep_hot"] != true {
		t.Errorf("keep_hot after --keep-hot: %v, want true", sb["keep_hot"])
	}

	// Set allow-cold
	_, _, code = c.run("edit", name, "--allow-cold")
	if code != 0 {
		t.Fatalf("edit --allow-cold exit %d", code)
	}
	stdout, _, _ = c.run("--json", "inspect", name)
	json.Unmarshal([]byte(stdout), &sb)
	if sb["keep_hot"] != false {
		t.Errorf("keep_hot after --allow-cold: %v, want false", sb["keep_hot"])
	}

	// Conflicting flags → error
	_, _, code = c.run("edit", name, "--keep-hot", "--allow-cold")
	if code == 0 {
		t.Error("--keep-hot + --allow-cold should error")
	}
}

func TestCLIEditRename(t *testing.T) {
	c := setupCLITest(t)

	oldName := fmt.Sprintf("cli-rn-%d", time.Now().UnixNano()%100000)
	newName := oldName + "-renamed"
	c.run("create", "--name", oldName)
	// Cleanup uses whichever name is current at teardown time. Fall back to
	// destroying both to be safe in any failure mode.
	t.Cleanup(func() {
		c.run("destroy", newName, "-y")
		c.run("destroy", oldName, "-y")
	})

	// Rename
	_, stderr, code := c.run("edit", oldName, "--name", newName)
	if code != 0 {
		t.Fatalf("edit --name exit %d, stderr: %s", code, stderr)
	}

	// Inspect by new name works
	stdout, _, code := c.run("--json", "inspect", newName)
	if code != 0 {
		t.Fatalf("inspect new name: exit %d", code)
	}
	var sb map[string]interface{}
	json.Unmarshal([]byte(stdout), &sb)
	if sb["name"] != newName {
		t.Errorf("name after rename: %v, want %q", sb["name"], newName)
	}

	// Inspect by old name fails
	_, _, code = c.run("inspect", oldName)
	if code == 0 {
		t.Error("inspect by old name should fail after rename")
	}

	// Renaming to a same-name target is a no-op (200) and not an error.
	_, _, code = c.run("edit", newName, "--name", newName)
	if code != 0 {
		t.Errorf("same-name rename should succeed as no-op, got exit %d", code)
	}
}

func TestCLIPublishUnpublish(t *testing.T) {
	c := setupCLITest(t)

	name := fmt.Sprintf("cli-pub-%d", time.Now().UnixNano()%100000)
	c.run("create", "--name", name)
	t.Cleanup(func() { c.run("destroy", name, "-y") })

	// Start a listener
	c.run("exec", name, "--", "sh", "-c",
		"python3 -m http.server 9090 &>/dev/null &")
	time.Sleep(1 * time.Second)

	// Publish
	stdout, _, code := c.run("publish", name, "-p", "9090")
	if code != 0 {
		t.Fatalf("publish exit %d", code)
	}
	if !strings.Contains(stdout, "Published") && !strings.Contains(stdout, "http") {
		t.Errorf("expected URL in publish output: %q", stdout)
	}

	// Unpublish
	stdout, _, code = c.run("unpublish", name, "-p", "9090")
	if code != 0 {
		t.Fatalf("unpublish exit %d", code)
	}
}

func TestCLIShareRevoke(t *testing.T) {
	c := setupCLITest(t)

	name := fmt.Sprintf("cli-share-%d", time.Now().UnixNano()%100000)
	c.run("create", "--name", name)
	t.Cleanup(func() { c.run("destroy", name, "-y") })

	// Share
	stdout, _, code := c.run("share", name)
	if code != 0 {
		t.Fatalf("share exit %d", code)
	}
	if !strings.Contains(stdout, "Shell:") && !strings.Contains(stdout, "http") {
		t.Errorf("expected URL in share output: %q", stdout)
	}

	// Revoke
	_, _, code = c.run("share", name, "--revoke")
	if code != 0 {
		t.Fatalf("share --revoke exit %d", code)
	}
}

func TestCLIAdminStatus(t *testing.T) {
	c := setupCLITest(t)

	stdout, _, code := c.run("admin", "status")
	if code != 0 {
		t.Fatalf("admin status exit %d", code)
	}
	if stdout == "" {
		t.Error("admin status returned empty output")
	}

	// JSON
	stdout, _, code = c.run("--json", "admin", "status")
	if code != 0 {
		t.Fatalf("admin status --json exit %d", code)
	}
	var status map[string]interface{}
	if err := json.Unmarshal([]byte(stdout), &status); err != nil {
		t.Fatalf("json parse: %v\nraw: %s", err, stdout)
	}
}

func TestCLITimingFlag(t *testing.T) {
	c := setupCLITest(t)

	name := fmt.Sprintf("cli-timing-%d", time.Now().UnixNano()%100000)
	c.run("create", "--name", name)
	t.Cleanup(func() { c.run("destroy", name, "-y") })

	stdout, stderr, code := c.run("exec", name, "--timing", "--", "echo", "timing-test")
	if code != 0 {
		t.Fatalf("exec --timing exit %d", code)
	}
	if !strings.Contains(stdout, "timing-test") {
		t.Errorf("stdout should have command output: %q", stdout)
	}
	if !strings.Contains(stderr, "total:") {
		t.Errorf("stderr should have timing breakdown with 'total:': %q", stderr)
	}
}

// --- Tier 3: Edge cases ---

func TestCLILifecycleFullCycle(t *testing.T) {
	c := setupCLITest(t)

	name := fmt.Sprintf("cli-lifecycle-%d", time.Now().UnixNano()%100000)
	c.run("create", "--name", name)
	t.Cleanup(func() { c.run("destroy", name, "-y") })

	// Write marker
	c.run("exec", name, "--", "sh", "-c", "echo cycle-data > /tmp/lifecycle.txt")

	// Thermal cycle 1
	c.run("stop", name)
	c.run("start", name)
	stdout, _, code := c.run("exec", name, "--", "cat", "/tmp/lifecycle.txt")
	if code != 0 || !strings.Contains(stdout, "cycle-data") {
		t.Fatalf("cycle 1 failed: exit=%d out=%q", code, stdout)
	}

	// Thermal cycle 2
	c.run("stop", name)
	c.run("start", name)
	stdout, _, code = c.run("exec", name, "--", "cat", "/tmp/lifecycle.txt")
	if code != 0 || !strings.Contains(stdout, "cycle-data") {
		t.Fatalf("cycle 2 failed: exit=%d out=%q", code, stdout)
	}

	// Destroy
	_, _, code = c.run("destroy", name, "-y")
	if code != 0 {
		t.Fatalf("final destroy exit %d", code)
	}
}

func TestCLIConcurrentExec(t *testing.T) {
	c := setupCLITest(t)

	name := fmt.Sprintf("cli-conc-%d", time.Now().UnixNano()%100000)
	c.run("create", "--name", name)
	t.Cleanup(func() { c.run("destroy", name, "-y") })

	var wg sync.WaitGroup
	results := make([]string, 5)
	errors := make([]error, 5)

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			expected := fmt.Sprintf("concurrent-%d", idx)
			stdout, _, code := c.run("exec", name, "--", "echo", expected)
			if code != 0 {
				errors[idx] = fmt.Errorf("exec %d: exit %d", idx, code)
				return
			}
			results[idx] = strings.TrimSpace(stdout)
		}(i)
	}
	wg.Wait()

	for i := 0; i < 5; i++ {
		if errors[i] != nil {
			t.Errorf("goroutine %d: %v", i, errors[i])
			continue
		}
		expected := fmt.Sprintf("concurrent-%d", i)
		if results[i] != expected {
			t.Errorf("goroutine %d: got %q, want %q", i, results[i], expected)
		}
	}
}

func TestCLILargeOutput(t *testing.T) {
	c := setupCLITest(t)

	name := fmt.Sprintf("cli-large-%d", time.Now().UnixNano()%100000)
	c.run("create", "--name", name)
	t.Cleanup(func() { c.run("destroy", name, "-y") })

	stdout, _, code := c.run("exec", name, "--", "seq", "1", "100000")
	if code != 0 {
		t.Fatalf("seq exit %d", code)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if lines[0] != "1" {
		t.Errorf("first line: %q, want '1'", lines[0])
	}
	if lines[len(lines)-1] != "100000" {
		t.Errorf("last line: %q, want '100000'", lines[len(lines)-1])
	}
	if len(lines) != 100000 {
		t.Errorf("line count: %d, want 100000", len(lines))
	}
}

func TestCLIExecTimeout(t *testing.T) {
	c := setupCLITest(t)

	name := fmt.Sprintf("cli-timeout-%d", time.Now().UnixNano()%100000)
	c.run("create", "--name", name)
	t.Cleanup(func() { c.run("destroy", name, "-y") })

	start := time.Now()
	_, _, code := c.run("exec", name, "--timeout", "2", "--", "sleep", "30")
	elapsed := time.Since(start)

	if code == 0 {
		t.Error("timed-out exec should have non-zero exit code")
	}
	if elapsed > 15*time.Second {
		t.Errorf("timeout took %v, should be ~2s not 300s default", elapsed)
	}
}

func TestCLICreateAllFlags(t *testing.T) {
	c := setupCLITest(t)

	name := fmt.Sprintf("cli-allflags-%d", time.Now().UnixNano()%100000)
	volName := fmt.Sprintf("cli-afvol-%d", time.Now().UnixNano()%100000)
	secretName := fmt.Sprintf("cli-afsec-%d", time.Now().UnixNano()%100000)

	// Setup: volume + secret + local file
	c.run("volume", "create", "--name", volName, "--size", "64")
	t.Cleanup(func() { c.run("volume", "delete", volName) })

	c.run("secret", "set", secretName, "all-flags-secret")
	t.Cleanup(func() { c.run("secret", "delete", secretName) })

	tmpFile := filepath.Join(t.TempDir(), "allflags.conf")
	os.WriteFile(tmpFile, []byte("allflags-config"), 0644)

	// Create with every flag
	_, _, code := c.run("create", "--name", name,
		"--cpus", "2", "--memory", "2048", "--disk-size", "4096",
		"--volume", volName+":/data",
		"--init", "echo init-ok > /tmp/init-proof",
		"--env", "FOO=bar",
		"--secret", secretName,
		"--file", tmpFile+":/app/config.conf",
		"--keep-hot",
	)
	if code != 0 {
		t.Fatalf("create with all flags exit %d", code)
	}
	t.Cleanup(func() { c.run("destroy", name, "-y") })

	time.Sleep(3 * time.Second) // wait for init

	// Verify init ran
	stdout, _, code := c.run("exec", name, "--", "cat", "/tmp/init-proof")
	if code != 0 || !strings.Contains(stdout, "init-ok") {
		t.Errorf("init: exit=%d out=%q", code, stdout)
	}

	// Verify env
	stdout, _, _ = c.run("exec", name, "--", "printenv", "FOO")
	if strings.TrimSpace(stdout) != "bar" {
		t.Errorf("FOO env: %q, want 'bar'", strings.TrimSpace(stdout))
	}

	// Verify secret
	stdout, _, _ = c.run("exec", name, "--", "printenv", secretName)
	if strings.TrimSpace(stdout) != "all-flags-secret" {
		t.Errorf("secret: %q, want 'all-flags-secret'", strings.TrimSpace(stdout))
	}

	// Verify file
	stdout, _, _ = c.run("exec", name, "--", "cat", "/app/config.conf")
	if strings.TrimSpace(stdout) != "allflags-config" {
		t.Errorf("file: %q, want 'allflags-config'", strings.TrimSpace(stdout))
	}

	// Verify volume mounted
	_, _, code = c.run("exec", name, "--", "test", "-d", "/data")
	if code != 0 {
		t.Error("volume /data not mounted")
	}
}

func TestCLIStopDestroyShortcut(t *testing.T) {
	c := setupCLITest(t)

	name := fmt.Sprintf("cli-stopdest-%d", time.Now().UnixNano()%100000)
	c.run("create", "--name", name)

	_, _, code := c.run("stop", name)
	if code != 0 {
		t.Fatalf("stop exit %d", code)
	}

	// Destroy while stopped — should not need start first
	_, _, code = c.run("destroy", name, "-y")
	if code != 0 {
		t.Fatalf("destroy stopped sandbox exit %d", code)
	}

	// Verify gone
	stdout, _, _ := c.run("list")
	if strings.Contains(stdout, name) {
		t.Error("sandbox still in list after destroy")
	}
}

func TestCLIInspectStoppedSandbox(t *testing.T) {
	c := setupCLITest(t)

	name := fmt.Sprintf("cli-instsop-%d", time.Now().UnixNano()%100000)
	c.run("create", "--name", name)
	t.Cleanup(func() { c.run("destroy", name, "-y") })

	c.run("stop", name)

	stdout, _, code := c.run("--json", "inspect", name)
	if code != 0 {
		t.Fatalf("inspect stopped exit %d", code)
	}
	var sb map[string]interface{}
	if err := json.Unmarshal([]byte(stdout), &sb); err != nil {
		t.Fatalf("json parse: %v", err)
	}
	if sb["status"] != "stopped" {
		t.Errorf("status: %v, want 'stopped'", sb["status"])
	}
	if sb["stopped_at"] == nil {
		t.Error("stopped_at should be set")
	}
}

// --- Helpers ---

// runWithEnv extends run() with extra environment variables.
func (c *cliTest) runWithEnv(extraEnv []string, args ...string) (stdout, stderr string, exitCode int) {
	c.t.Helper()
	cmd := exec.Command(c.bhatti, args...)
	cmd.Env = append(os.Environ(),
		"BHATTI_URL="+c.baseURL,
		"BHATTI_TOKEN="+c.token,
	)
	cmd.Env = append(cmd.Env, extraEnv...)
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
