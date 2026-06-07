//go:build linux

package firecracker

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent"
	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
	"github.com/sahil-shubham/bhatti/pkg/engine"
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

	// Try forwarding without auth — should fail fast (no listener needed,
	// auth is checked before the forward request is even processed)
	noAuthClient := agent.NewTCPClient(info.IP)
	start := time.Now()
	_, err = noAuthClient.Forward(ctx, 8888)
	elapsed := time.Since(start)
	if err == nil {
		t.Error("expected forward to fail without auth")
	} else {
		t.Logf("✓ unauthenticated forward rejected in %v: %v", elapsed, err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("auth rejection too slow: %v (want <3s)", elapsed)
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

	// Test file injection via SandboxSpec.Files — the config drive is
	// unmounted after boot (security), so we verify injected files are
	// written to the guest filesystem, not by reading the config drive.
	spec := testSpec("file-test")
	spec.Files = map[string]engine.FileSpec{
		"/home/lohar/.env": {
			Content: []byte("KEY=injected-value"),
			Mode:    "0600",
		},
	}

	info, err := eng.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Verify the injected file exists with correct content
	r, _ := execWithTimeout(t, eng, info.ID, []string{"cat", "/home/lohar/.env"})
	if r.ExitCode != 0 {
		t.Fatalf("injected file not readable: exit=%d stderr=%q", r.ExitCode, r.Stderr)
	}
	if !strings.Contains(r.Stdout, "KEY=injected-value") {
		t.Fatalf("injected file content wrong: %q", r.Stdout)
	}
	t.Log("✓ file injected via config drive")

	// Verify file permissions
	r, _ = execWithTimeout(t, eng, info.ID, []string{"stat", "-c", "%a", "/home/lohar/.env"})
	if strings.TrimSpace(r.Stdout) != "600" {
		t.Errorf("file mode = %q, want 600", strings.TrimSpace(r.Stdout))
	}
	t.Log("✓ injected file has correct mode (0600)")

	// Verify config drive is unmounted (security: token + env plaintext)
	r, _ = execWithTimeout(t, eng, info.ID, []string{"ls", "/run/bhatti/config"})
	if r.ExitCode == 0 {
		t.Error("config drive should be unmounted after boot")
	} else {
		t.Log("✓ config drive unmounted after boot (security)")
	}
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

	// G1.1: with the per-user DNS responder up (testEngine's New()
	// defaults DNSUpstreams, so it binds and forwards), resolv.conf
	// must list ONLY the in-cluster responder at the bridge gateway
	// (10.0.N.1) — NOT 1.1.1.1/8.8.8.8. The responder forwards public
	// names upstream itself; listing public resolvers alongside it
	// would let glibc round-robin away from the responder and miss
	// sibling names. This test previously asserted the OPPOSITE
	// (public DNS present), which pinned the bug that broke sandbox
	// internet access — see lohar.applyDNS for the full story.
	r, _ := execWithTimeout(t, eng, info.ID, []string{"cat", "/etc/resolv.conf"})
	gw := "10.0.99.1" // testSpec uses SubnetIndex 99 → gateway 10.0.99.1
	if !strings.Contains(r.Stdout, "nameserver "+gw) {
		t.Errorf("resolv.conf missing in-cluster responder %q: %q", gw, r.Stdout)
	}
	if strings.Contains(r.Stdout, "1.1.1.1") || strings.Contains(r.Stdout, "8.8.8.8") {
		t.Errorf("resolv.conf should NOT list public DNS when responder is up "+
			"(responder forwards upstream itself): %q", r.Stdout)
	}
	if !strings.Contains(r.Stdout, "options timeout:2 attempts:1") {
		t.Errorf("resolv.conf missing fast-timeout option: %q", r.Stdout)
	}
	t.Logf("✓ DNS configured via config drive (responder-only):\n%s", r.Stdout)
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

	// exec already runs as uid 1000 (lohar) — verify we can write
	r, _ = execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "echo user-write > /workspace/test && cat /workspace/test"})
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

func TestConfigDriveMultipleFiles(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	// Verify multiple files can be injected with different modes.
	// The config drive is unmounted after boot, but injected files
	// are written to the guest filesystem by lohar before unmount.
	spec := testSpec("multifile")
	spec.Files = map[string]engine.FileSpec{
		"/home/lohar/a.txt": {Content: []byte("file-a"), Mode: "0644"},
		"/home/lohar/b.txt": {Content: []byte("file-b"), Mode: "0600"},
		"/opt/custom/c.sh":  {Content: []byte("#!/bin/sh\necho hello"), Mode: "0755"},
	}

	info, err := eng.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Verify all files
	for path, expected := range map[string]string{
		"/home/lohar/a.txt": "file-a",
		"/home/lohar/b.txt": "file-b",
		"/opt/custom/c.sh":  "#!/bin/sh\necho hello",
	} {
		r, _ := execWithTimeout(t, eng, info.ID, []string{"cat", path})
		if r.ExitCode != 0 {
			t.Errorf("%s: not readable (exit=%d stderr=%q)", path, r.ExitCode, r.Stderr)
			continue
		}
		if strings.TrimSpace(r.Stdout) != strings.TrimSpace(expected) {
			t.Errorf("%s: got %q, want %q", path, r.Stdout, expected)
		}
	}
	t.Log("✓ multiple files injected with correct content")

	// Verify config drive is unmounted
	r, _ := execWithTimeout(t, eng, info.ID, []string{"mountpoint", "-q", "/run/bhatti/config"})
	if r.ExitCode == 0 {
		t.Error("config drive should be unmounted after boot")
	}
	t.Log("✓ config drive unmounted (security)")
}

// Helper to suppress unused import warning
var _ = base64.StdEncoding
var _ = proto.AUTH
