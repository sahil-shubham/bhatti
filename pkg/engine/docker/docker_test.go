package docker

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sahilshubham/bhatti/pkg/engine"
)

func skipIfNoDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found, skipping integration test")
	}
	// Quick ping check
	e, err := New()
	if err != nil {
		t.Skipf("cannot create docker client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := e.cli.Ping(ctx); err != nil {
		t.Skipf("docker not responding: %v", err)
	}
}

func TestDockerLifecycle(t *testing.T) {
	skipIfNoDocker(t)

	e, err := New()
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	name := "bhatti-test-" + time.Now().Format("150405")

	// Create
	info, err := e.Create(ctx, engine.SandboxSpec{
		Name:     name,
		Image:    "alpine:latest",
		CPUs:     0.5,
		MemoryMB: 64,
		Env:      map[string]string{"TEST_VAR": "hello"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Destroy(ctx, info.EngineID)

	if info.Status != "running" {
		t.Fatalf("expected running, got %s", info.Status)
	}
	if info.Name != name {
		t.Fatalf("expected name %s, got %s", name, info.Name)
	}

	// Status
	info2, err := e.Status(ctx, info.EngineID)
	if err != nil {
		t.Fatal(err)
	}
	if info2.Status != "running" {
		t.Fatalf("expected running, got %s", info2.Status)
	}

	// Exec
	result, err := e.Exec(ctx, info.EngineID, []string{"echo", "world"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(result.Stdout) != "world" {
		t.Fatalf("expected 'world', got %q", result.Stdout)
	}
	if result.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", result.ExitCode)
	}

	// Exec — env var check
	result, err = e.Exec(ctx, info.EngineID, []string{"sh", "-c", "echo $TEST_VAR"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(result.Stdout) != "hello" {
		t.Fatalf("expected 'hello', got %q", result.Stdout)
	}

	// List
	list, err := e.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range list {
		if s.EngineID == info.EngineID {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("sandbox not found in list")
	}

	// Stop
	if err := e.Stop(ctx, info.EngineID); err != nil {
		t.Fatal(err)
	}
	info3, _ := e.Status(ctx, info.EngineID)
	if info3.Status != "stopped" {
		t.Fatalf("expected stopped, got %s", info3.Status)
	}

	// Start
	if err := e.Start(ctx, info.EngineID); err != nil {
		t.Fatal(err)
	}
	info4, _ := e.Status(ctx, info.EngineID)
	if info4.Status != "running" {
		t.Fatalf("expected running, got %s", info4.Status)
	}

	// Destroy
	if err := e.Destroy(ctx, info.EngineID); err != nil {
		t.Fatal(err)
	}
	_, err = e.Status(ctx, info.EngineID)
	if err == nil {
		t.Fatal("expected error after destroy")
	}
}

func TestDockerShell(t *testing.T) {
	skipIfNoDocker(t)

	e, err := New()
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	name := "bhatti-shell-" + time.Now().Format("150405")

	info, err := e.Create(ctx, engine.SandboxSpec{
		Name:  name,
		Image: "alpine:latest",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Destroy(ctx, info.EngineID)

	// Shell
	conn, err := e.Shell(ctx, info.EngineID)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Write a command
	_, err = conn.Write([]byte("echo bhatti-test\n"))
	if err != nil {
		t.Fatal(err)
	}

	// Read output (with timeout)
	buf := make([]byte, 4096)
	done := make(chan string, 1)
	go func() {
		var total string
		for i := 0; i < 10; i++ {
			n, err := conn.Read(buf)
			if err != nil {
				break
			}
			total += string(buf[:n])
			if strings.Contains(total, "bhatti-test") {
				done <- total
				return
			}
		}
		done <- total
	}()

	select {
	case output := <-done:
		if !strings.Contains(output, "bhatti-test") {
			t.Fatalf("expected 'bhatti-test' in output, got %q", output)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for shell output")
	}

	// Resize should not error
	if err := conn.Resize(40, 120); err != nil {
		t.Fatalf("resize failed: %v", err)
	}
}
