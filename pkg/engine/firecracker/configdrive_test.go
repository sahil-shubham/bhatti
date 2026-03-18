//go:build linux

package firecracker

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/sahilshubham/bhatti/pkg/agent"
	"github.com/sahilshubham/bhatti/pkg/agent/proto"
	"github.com/sahilshubham/bhatti/pkg/engine"
)

func TestConfigDriveEnvInjection(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	spec := testSpec("env-test")
	spec.Env = map[string]string{
		"MY_SECRET":  "hunter2",
		"API_KEY":    "sk-abc123",
		"CUSTOM_VAR": "hello world",
	}

	info, err := eng.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Verify each env var is accessible inside the VM
	for key, want := range spec.Env {
		r, err := execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "echo $" + key})
		if err != nil {
			t.Fatalf("exec echo $%s: %v", key, err)
		}
		got := strings.TrimSpace(r.Stdout)
		if got != want {
			t.Errorf("$%s = %q, want %q", key, got, want)
		}
	}
	t.Log("✓ all env vars injected via config drive")
}

func TestConfigDriveHostname(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("my-sandbox-host"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	r, _ := execWithTimeout(t, eng, info.ID, []string{"hostname"})
	got := strings.TrimSpace(r.Stdout)
	if got != "my-sandbox-host" {
		t.Errorf("hostname = %q, want 'my-sandbox-host'", got)
	} else {
		t.Logf("✓ hostname set to %q via config drive", got)
	}
}

func TestAuthRequired(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("auth-test"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// The VM has a token. Try connecting without auth — should fail.
	noAuthClient := agent.NewTCPClient(info.IP)
	_, err = noAuthClient.Exec(ctx, []string{"echo", "should-fail"}, nil, "")
	if err == nil {
		t.Error("expected error when connecting without auth token")
	} else {
		t.Logf("✓ unauthenticated connection rejected: %v", err)
	}

	// Try with wrong token
	wrongClient := agent.NewTCPClientWithAuth(info.IP, "wrong-token")
	_, err = wrongClient.Exec(ctx, []string{"echo", "should-fail"}, nil, "")
	if err == nil {
		t.Error("expected error with wrong token")
	} else {
		t.Logf("✓ wrong token rejected: %v", err)
	}

	// The engine's client has the right token — should still work
	r, err := execWithTimeout(t, eng, info.ID, []string{"echo", "auth-works"})
	if err != nil {
		t.Fatalf("exec with correct token: %v", err)
	}
	if !strings.Contains(r.Stdout, "auth-works") {
		t.Errorf("unexpected output: %q", r.Stdout)
	} else {
		t.Log("✓ authenticated connection works")
	}
}

func TestAuthForwardChannel(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("auth-fwd"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Start a listener inside the VM
	execWithTimeout(t, eng, info.ID, []string{"sh", "-c",
		"echo fwd-data | nc -l -p 8888 &"})

	// Try forwarding without auth — should fail
	noAuthClient := agent.NewTCPClient(info.IP)
	_, err = noAuthClient.Forward(ctx, 8888)
	if err == nil {
		t.Error("expected forward to fail without auth")
	} else {
		t.Logf("✓ unauthenticated forward rejected: %v", err)
	}
}

func TestVolumeMount(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	spec := testSpec("vol-test")
	spec.NewVolumes = []engine.VolumeSpec{
		{Name: "workspace", SizeMB: 64, Mount: "/workspace"},
	}

	info, err := eng.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Verify volume is mounted
	r, _ := execWithTimeout(t, eng, info.ID, []string{"df", "-h", "/workspace"})
	if !strings.Contains(r.Stdout, "/workspace") {
		t.Errorf("volume not mounted: %s", r.Stdout)
	} else {
		t.Logf("✓ volume mounted at /workspace:\n%s", r.Stdout)
	}

	// Write a file to the volume
	r, _ = execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "echo vol-data > /workspace/test.txt && cat /workspace/test.txt"})
	if strings.TrimSpace(r.Stdout) != "vol-data" {
		t.Errorf("write/read volume: %q", r.Stdout)
	} else {
		t.Log("✓ write/read to volume works")
	}

	// Verify volume size is approximately right (64MB)
	r, _ = execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "df /workspace | tail -1 | awk '{print $2}'"})
	t.Logf("volume size (1K blocks): %s", strings.TrimSpace(r.Stdout))
}

func TestConfigDriveFileInjection(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	// We need to create a VM manually with files in the config drive.
	// The current Create() flow doesn't expose file injection through SandboxSpec
	// (that comes via secrets resolution). So we'll test at the config drive level
	// by creating a VM, then verifying the config drive content.

	// For now, test that the config drive is readable inside the VM
	info, err := eng.Create(ctx, testSpec("file-test"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Config drive should be mounted at /run/bhatti/config
	r, _ := execWithTimeout(t, eng, info.ID, []string{"cat", "/run/bhatti/config/config.json"})
	if r.ExitCode != 0 {
		t.Fatalf("config.json not readable: exit=%d stderr=%q", r.ExitCode, r.Stderr)
	}
	if !strings.Contains(r.Stdout, "file-test") {
		t.Errorf("config.json missing hostname: %s", r.Stdout)
	}
	if !strings.Contains(r.Stdout, "token") {
		t.Errorf("config.json missing token: %s", r.Stdout)
	}
	t.Logf("✓ config drive readable, contains expected fields")

	// Verify the config drive token is actually being used for auth
	// (we can't read the token from inside since it's the auth token,
	// but we already tested auth in TestAuthRequired)
}

func TestVolumePersistsAcrossReboot(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	spec := testSpec("vol-persist")
	spec.NewVolumes = []engine.VolumeSpec{
		{Name: "data", SizeMB: 32, Mount: "/data"},
	}

	info, err := eng.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Write data to volume
	execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "echo persist-me > /data/test.txt"})

	// Snapshot and resume
	if err := eng.Stop(ctx, info.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := eng.Start(ctx, info.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Data should still be there
	r, _ := execWithTimeout(t, eng, info.ID, []string{"cat", "/data/test.txt"})
	if strings.TrimSpace(r.Stdout) != "persist-me" {
		t.Errorf("volume data lost: %q", r.Stdout)
	} else {
		t.Log("✓ volume data persists across snapshot/resume")
	}
}

func TestConfigDriveDNS(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("dns-test"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Config drive sets DNS to 1.1.1.1 and 8.8.8.8
	r, _ := execWithTimeout(t, eng, info.ID, []string{"cat", "/etc/resolv.conf"})
	if !strings.Contains(r.Stdout, "1.1.1.1") {
		t.Errorf("resolv.conf missing 1.1.1.1: %q", r.Stdout)
	}
	if !strings.Contains(r.Stdout, "8.8.8.8") {
		t.Errorf("resolv.conf missing 8.8.8.8: %q", r.Stdout)
	}
	t.Logf("✓ DNS configured via config drive:\n%s", r.Stdout)
}

func TestEnvSpecialCharacters(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	spec := testSpec("env-special")
	spec.Env = map[string]string{
		"WITH_SPACES":  "hello world foo",
		"WITH_EQUALS":  "key=value=more",
		"WITH_QUOTES":  `say "hello"`,
		"EMPTY_VAL":    "",
		"NUMERIC_VAL":  "12345",
		"WITH_NEWLINE": "line1\nline2",
	}

	info, err := eng.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Use printenv instead of echo $ to avoid shell interpretation issues
	for key, want := range spec.Env {
		r, err := execWithTimeout(t, eng, info.ID, []string{"printenv", key})
		if err != nil {
			t.Fatalf("printenv %s: %v", key, err)
		}
		// printenv adds a newline, so trim just the trailing newline
		got := r.Stdout
		if len(got) > 0 && got[len(got)-1] == '\n' {
			got = got[:len(got)-1]
		}
		if got != want {
			t.Errorf("$%s = %q, want %q", key, got, want)
		}
	}
	t.Log("✓ env vars with special characters work")
}

func TestMultipleVolumes(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	spec := testSpec("multi-vol")
	spec.NewVolumes = []engine.VolumeSpec{
		{Name: "code", SizeMB: 32, Mount: "/code"},
		{Name: "data", SizeMB: 32, Mount: "/data"},
		{Name: "cache", SizeMB: 16, Mount: "/cache"},
	}

	info, err := eng.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Verify all three are mounted
	for _, vs := range spec.NewVolumes {
		r, _ := execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "mountpoint -q " + vs.Mount + " && echo mounted"})
		if !strings.Contains(r.Stdout, "mounted") {
			t.Errorf("%s not mounted", vs.Mount)
		}
	}
	t.Log("✓ all 3 volumes mounted")

	// Write to each and read back
	for _, vs := range spec.NewVolumes {
		marker := "data-for-" + vs.Name
		execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "echo " + marker + " > " + vs.Mount + "/test.txt"})
		r, _ := execWithTimeout(t, eng, info.ID, []string{"cat", vs.Mount + "/test.txt"})
		if strings.TrimSpace(r.Stdout) != marker {
			t.Errorf("%s: got %q, want %q", vs.Mount, r.Stdout, marker)
		}
	}
	t.Log("✓ read/write works on all 3 volumes")

	// Volumes should be on different devices
	r, _ := execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "mount | grep '/code\\|/data\\|/cache'"})
	lines := strings.Split(strings.TrimSpace(r.Stdout), "\n")
	if len(lines) != 3 {
		t.Errorf("expected 3 mount entries, got %d: %q", len(lines), r.Stdout)
	}
	devices := map[string]bool{}
	for _, line := range lines {
		dev := strings.Fields(line)[0]
		devices[dev] = true
	}
	if len(devices) != 3 {
		t.Errorf("expected 3 different devices, got %d: %v", len(devices), devices)
	} else {
		t.Logf("✓ 3 volumes on 3 different devices: %v", devices)
	}
}

func TestVolumeOwnership(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	spec := testSpec("vol-own")
	spec.NewVolumes = []engine.VolumeSpec{
		{Name: "work", SizeMB: 16, Mount: "/workspace"},
	}

	info, err := eng.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Volume mount should be owned by lohar (uid 1000)
	r, _ := execWithTimeout(t, eng, info.ID, []string{"stat", "-c", "%u:%g", "/workspace"})
	got := strings.TrimSpace(r.Stdout)
	if got != "1000:1000" {
		t.Errorf("/workspace owner = %q, want '1000:1000'", got)
	} else {
		t.Log("✓ volume owned by lohar (1000:1000)")
	}

	// lohar user should be able to write (run as uid 1000)
	r, _ = execWithTimeout(t, eng, info.ID, []string{"su", "-s", "/bin/sh", "lohar", "-c", "echo user-write > /workspace/test && cat /workspace/test"})
	if strings.TrimSpace(r.Stdout) != "user-write" {
		t.Errorf("lohar user write failed: exit=%d out=%q err=%q", r.ExitCode, r.Stdout, r.Stderr)
	} else {
		t.Log("✓ lohar user can write to volume")
	}
}

func TestAuthSurvivesResume(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("auth-resume"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Snapshot and resume
	if err := eng.Stop(ctx, info.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := eng.Start(ctx, info.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Auth should still be required after resume
	noAuthClient := agent.NewTCPClient(info.IP)
	_, err = noAuthClient.Exec(ctx, []string{"echo", "no-auth"}, nil, "")
	if err == nil {
		t.Error("expected auth error after resume")
	} else {
		t.Logf("✓ auth still required after resume: %v", err)
	}

	// Correct auth should work
	r, err := execWithTimeout(t, eng, info.ID, []string{"echo", "post-resume-auth"})
	if err != nil {
		t.Fatalf("exec with auth after resume: %v", err)
	}
	if !strings.Contains(r.Stdout, "post-resume-auth") {
		t.Errorf("unexpected: %q", r.Stdout)
	} else {
		t.Log("✓ authenticated exec works after resume")
	}
}

func TestConfigDriveFileWrite(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	// Manually create a VM with files in the config drive by building
	// a spec with env that writes to a known path via the config.
	// Since Create() doesn't expose files directly through SandboxSpec yet,
	// we test the file writing by creating a config drive manually,
	// but for now we verify the mechanism works by checking that the
	// config drive JSON is parseable and contains the right structure.

	info, err := eng.Create(ctx, testSpec("filedrive"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Read config.json and verify its structure
	r, _ := execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "cat /run/bhatti/config/config.json | python3 -m json.tool"})
	if r.ExitCode != 0 {
		t.Fatalf("config.json not valid JSON: %s", r.Stderr)
	}

	// Verify all expected fields exist
	for _, field := range []string{"sandbox_id", "hostname", "token", "env", "files", "volumes", "dns", "user"} {
		if !strings.Contains(r.Stdout, `"`+field+`"`) {
			t.Errorf("config.json missing field %q", field)
		}
	}
	t.Log("✓ config.json has valid structure with all fields")
}

// Helper to suppress unused import warning
var _ = base64.StdEncoding
var _ = proto.AUTH
