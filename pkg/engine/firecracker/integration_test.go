//go:build linux

package firecracker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// --- EnsureHot integration tests ---

func TestEnsureHotExecFromWarm(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("ensure-exec-warm"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Write state, then pause
	execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "echo state > /tmp/data"})
	eng.Pause(ctx, info.ID)

	if eng.ThermalState(info.ID) != "warm" {
		t.Fatalf("expected warm")
	}

	// EnsureHot then exec
	start := time.Now()
	if err := eng.EnsureHot(ctx, info.ID); err != nil {
		t.Fatalf("EnsureHot: %v", err)
	}
	r, err := execWithTimeout(t, eng, info.ID, []string{"cat", "/tmp/data"})
	total := time.Since(start)
	if err != nil {
		t.Fatalf("exec after ensureHot: %v", err)
	}
	if strings.TrimSpace(r.Stdout) != "state" {
		t.Errorf("state lost: %q", r.Stdout)
	}
	t.Logf("✓ ensureHot(warm) + exec in %v, data preserved", total)
}

func TestEnsureHotExecFromCold(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("ensure-exec-cold"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "echo cold-state > /tmp/data"})
	eng.Stop(ctx, info.ID)

	if eng.ThermalState(info.ID) != "cold" {
		t.Fatalf("expected cold")
	}

	start := time.Now()
	if err := eng.EnsureHot(ctx, info.ID); err != nil {
		t.Fatalf("EnsureHot: %v", err)
	}
	r, err := execWithTimeout(t, eng, info.ID, []string{"cat", "/tmp/data"})
	total := time.Since(start)
	if err != nil {
		t.Fatalf("exec after ensureHot: %v", err)
	}
	if strings.TrimSpace(r.Stdout) != "cold-state" {
		t.Errorf("state lost: %q", r.Stdout)
	}
	t.Logf("✓ ensureHot(cold) + exec in %v, data preserved", total)
}

// --- File injection via config drive ---

func TestFileInjection(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	spec := testSpec("file-inject")
	spec.Files = map[string]engine.FileSpec{
		"/home/lohar/.ssh/authorized_keys": {
			Content: []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5 test@example.com"),
			Mode:    "0600",
		},
		"/home/lohar/.env": {
			Content: []byte("DATABASE_URL=postgres://localhost/mydb\nREDIS_URL=redis://localhost\n"),
			Mode:    "0644",
		},
	}

	info, err := eng.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Verify files were written
	r, _ := execWithTimeout(t, eng, info.ID, []string{"cat", "/home/lohar/.ssh/authorized_keys"})
	if !strings.Contains(r.Stdout, "ssh-ed25519") {
		t.Errorf("authorized_keys: %q", r.Stdout)
	} else {
		t.Log("✓ SSH key file injected")
	}

	r, _ = execWithTimeout(t, eng, info.ID, []string{"cat", "/home/lohar/.env"})
	if !strings.Contains(r.Stdout, "DATABASE_URL") {
		t.Errorf(".env: %q", r.Stdout)
	} else {
		t.Log("✓ .env file injected")
	}

	// Verify file permissions
	r, _ = execWithTimeout(t, eng, info.ID, []string{"stat", "-c", "%a", "/home/lohar/.ssh/authorized_keys"})
	if strings.TrimSpace(r.Stdout) != "600" {
		t.Errorf("authorized_keys mode: %q, want 600", strings.TrimSpace(r.Stdout))
	} else {
		t.Log("✓ file mode 0600 correct")
	}

	// Verify file ownership (should be lohar/1000)
	r, _ = execWithTimeout(t, eng, info.ID, []string{"stat", "-c", "%u", "/home/lohar/.ssh/authorized_keys"})
	if strings.TrimSpace(r.Stdout) != "1000" {
		t.Errorf("authorized_keys owner: %q, want 1000", strings.TrimSpace(r.Stdout))
	} else {
		t.Log("✓ file owned by lohar (uid 1000)")
	}
}

// --- Init script ---

func TestInitScript(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	spec := testSpec("init-script")
	spec.Init = "echo init-started > /tmp/init-marker && sleep 3600"

	info, err := eng.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Wait for init to run
	time.Sleep(2 * time.Second)

	// Verify init script ran
	r, _ := execWithTimeout(t, eng, info.ID, []string{"cat", "/tmp/init-marker"})
	if strings.TrimSpace(r.Stdout) != "init-started" {
		t.Errorf("init marker: %q", r.Stdout)
	} else {
		t.Log("✓ init script ran and wrote marker file")
	}

	// Verify the init session exists and can be listed
	vm, _ := eng.getVM(info.ID)
	sessions, err := vm.Agent.SessionList(ctx)
	if err != nil {
		t.Fatalf("SessionList: %v", err)
	}
	found := false
	for _, s := range sessions {
		if s.SessionID == "init" {
			found = true
			if !s.Running {
				t.Error("init session should be running")
			}
			t.Logf("✓ init session found: argv=%q running=%v", s.Argv, s.Running)
		}
	}
	if !found {
		t.Errorf("init session not in list: %+v", sessions)
	}
}

func TestInitSessionAttach(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	spec := testSpec("init-attach")
	spec.Init = "echo INIT_OUTPUT_MARKER && sleep 3600"

	info, err := eng.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	time.Sleep(2 * time.Second)

	// Attach to the init session
	vm, _ := eng.getVM(info.ID)
	sessInfo, term, err := vm.Agent.SessionAttach(ctx, "init", false)
	if err != nil {
		t.Fatalf("SessionAttach init: %v", err)
	}
	defer term.Close()

	if sessInfo.SessionID != "init" {
		t.Errorf("session ID: %q, want 'init'", sessInfo.SessionID)
	}

	// Read scrollback — should contain the init output
	output := readTermOutput(term, 3*time.Second, "INIT_OUTPUT_MARKER")
	if !strings.Contains(output, "INIT_OUTPUT_MARKER") {
		t.Errorf("init scrollback: %q", output)
	} else {
		t.Log("✓ attached to init session, scrollback contains init output")
	}
}

func TestInitScriptRunsAsUser(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	spec := testSpec("init-user")
	spec.Init = "id > /tmp/init-user && whoami >> /tmp/init-user"

	info, err := eng.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	time.Sleep(2 * time.Second)

	r, _ := execWithTimeout(t, eng, info.ID, []string{"cat", "/tmp/init-user"})
	if !strings.Contains(r.Stdout, "uid=1000") {
		t.Errorf("init didn't run as lohar: %q", r.Stdout)
	} else {
		t.Log("✓ init script runs as lohar (uid=1000)")
	}
}

// --- Rootfs tooling tests ---

func TestRootfsHasRipgrep(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("rg-test"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Verify rg exists and runs
	r, err := execWithTimeout(t, eng, info.ID, []string{"rg", "--version"})
	if err != nil || r.ExitCode != 0 {
		t.Fatalf("rg --version failed: err=%v exit=%d stderr=%q", err, r.ExitCode, r.Stderr)
	}
	t.Logf("✓ ripgrep installed: %s", strings.TrimSpace(r.Stdout))

	// Functional test: write files, grep for content
	execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "mkdir -p /workspace/src && echo 'func main() {}' > /workspace/src/main.go && echo 'hello world' > /workspace/src/readme.txt"})

	r, _ = execWithTimeout(t, eng, info.ID, []string{"rg", "--no-heading", "func main", "/workspace/src"})
	if !strings.Contains(r.Stdout, "func main") {
		t.Errorf("rg didn't find 'func main': %q", r.Stdout)
	} else {
		t.Log("✓ rg search works")
	}

	// JSON output mode (used by pi's grep tool)
	r, _ = execWithTimeout(t, eng, info.ID, []string{"rg", "--json", "hello", "/workspace/src"})
	if !strings.Contains(r.Stdout, "hello") {
		t.Errorf("rg --json didn't find 'hello': %q", r.Stdout)
	} else {
		t.Log("✓ rg --json output works")
	}
}

func TestRootfsHasFd(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("fd-test"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Verify fd exists and runs
	r, err := execWithTimeout(t, eng, info.ID, []string{"fd", "--version"})
	if err != nil || r.ExitCode != 0 {
		t.Fatalf("fd --version failed: err=%v exit=%d stderr=%q", err, r.ExitCode, r.Stderr)
	}
	t.Logf("✓ fd-find installed: %s", strings.TrimSpace(r.Stdout))

	// Functional test: create files, find by pattern
	execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "mkdir -p /workspace/src/pkg && touch /workspace/src/main.go /workspace/src/pkg/util.go /workspace/src/readme.md"})

	r, _ = execWithTimeout(t, eng, info.ID, []string{"fd", "-e", "go", ".", "/workspace/src"})
	goFiles := strings.Split(strings.TrimSpace(r.Stdout), "\n")
	if len(goFiles) < 2 {
		t.Errorf("fd didn't find .go files: %q", r.Stdout)
	} else {
		t.Logf("✓ fd glob works: found %d .go files", len(goFiles))
	}

	// Find by name pattern
	r, _ = execWithTimeout(t, eng, info.ID, []string{"fd", "main", "/workspace/src"})
	if !strings.Contains(r.Stdout, "main.go") {
		t.Errorf("fd didn't find main.go: %q", r.Stdout)
	} else {
		t.Log("✓ fd name pattern works")
	}
}
