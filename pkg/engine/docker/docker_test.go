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

func TestDockerCreateLocalImage(t *testing.T) {
	skipIfNoDocker(t)

	e, err := New()
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	// Build a local-only image that doesn't exist on any registry.
	// This is the real-world case: `make sandbox` builds bhatti-sandbox:latest locally.
	imgName := "bhatti-test-local-only:latest"
	cmd := exec.Command("docker", "build", "-t", imgName, "-")
	cmd.Stdin = strings.NewReader("FROM alpine:latest\nCMD [\"sleep\", \"infinity\"]\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build local image: %v\n%s", err, out)
	}
	defer exec.Command("docker", "rmi", imgName).Run()

	name := "bhatti-local-img-" + time.Now().Format("150405")

	// Create should succeed without trying to pull from a registry.
	// Before the fix, this would hang forever trying to pull from Docker Hub.
	createCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	info, err := e.Create(createCtx, engine.SandboxSpec{
		Name:     name,
		Image:    imgName,
		CPUs:     0.5,
		MemoryMB: 64,
	})
	if err != nil {
		t.Fatalf("Create with local-only image failed: %v", err)
	}
	defer e.Destroy(ctx, info.EngineID)

	if info.Status != "running" {
		t.Fatalf("expected running, got %s", info.Status)
	}
}

func TestDockerCreateMissingImage(t *testing.T) {
	skipIfNoDocker(t)

	e, err := New()
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	name := "bhatti-missing-" + time.Now().Format("150405")

	// A clearly non-existent image should return an error, not hang.
	_, err = e.Create(ctx, engine.SandboxSpec{
		Name:     name,
		Image:    "bhatti-nonexistent-image-xyzzy:latest",
		CPUs:     0.5,
		MemoryMB: 64,
	})
	if err == nil {
		// Clean up if it somehow succeeded
		e.Destroy(context.Background(), name)
		t.Fatal("expected error for non-existent image, got nil")
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

func TestDockerVolumeMount(t *testing.T) {
	skipIfNoDocker(t)

	e, err := New()
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	volName := "bhatti-test-vol-" + time.Now().Format("150405.000")
	name := "bhatti-test-volmount-" + time.Now().Format("150405.000")

	// Create a real Docker volume
	if out, err := exec.Command("docker", "volume", "create", volName).CombinedOutput(); err != nil {
		t.Fatalf("docker volume create: %v\n%s", err, out)
	}
	defer exec.Command("docker", "volume", "rm", "-f", volName).Run()

	info, err := e.Create(ctx, engine.SandboxSpec{
		Name:     name,
		Image:    "alpine:latest",
		CPUs:     0.5,
		MemoryMB: 64,
		Volumes: []engine.VolumeMount{
			{Name: volName, Target: "/data"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Destroy(ctx, info.EngineID)

	// Write to the mounted volume
	result, err := e.Exec(ctx, info.EngineID, []string{"sh", "-c", "echo vol-test > /data/file.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("write failed: exit=%d stderr=%s", result.ExitCode, result.Stderr)
	}

	// Read back
	result, err = e.Exec(ctx, info.EngineID, []string{"cat", "/data/file.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(result.Stdout) != "vol-test" {
		t.Fatalf("expected 'vol-test', got %q", result.Stdout)
	}
}

func TestDockerVolumePersistence(t *testing.T) {
	skipIfNoDocker(t)

	e, err := New()
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	volName := "bhatti-test-persist-" + time.Now().Format("150405.000")
	name1 := "bhatti-test-persist1-" + time.Now().Format("150405.000")
	name2 := "bhatti-test-persist2-" + time.Now().Format("150405.000")

	if out, err := exec.Command("docker", "volume", "create", volName).CombinedOutput(); err != nil {
		t.Fatalf("docker volume create: %v\n%s", err, out)
	}
	defer exec.Command("docker", "volume", "rm", "-f", volName).Run()

	// First container: write data
	info1, err := e.Create(ctx, engine.SandboxSpec{
		Name:     name1,
		Image:    "alpine:latest",
		Volumes:  []engine.VolumeMount{{Name: volName, Target: "/data"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := e.Exec(ctx, info1.EngineID, []string{"sh", "-c", "echo persist-me > /data/survive.txt"})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("write failed: err=%v exit=%d stderr=%s", err, result.ExitCode, result.Stderr)
	}

	// Destroy first container
	if err := e.Destroy(ctx, info1.EngineID); err != nil {
		t.Fatal(err)
	}

	// Second container: verify data survived
	info2, err := e.Create(ctx, engine.SandboxSpec{
		Name:     name2,
		Image:    "alpine:latest",
		Volumes:  []engine.VolumeMount{{Name: volName, Target: "/data"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Destroy(ctx, info2.EngineID)

	result, err = e.Exec(ctx, info2.EngineID, []string{"cat", "/data/survive.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(result.Stdout) != "persist-me" {
		t.Fatalf("data did not persist: expected 'persist-me', got %q", result.Stdout)
	}
}

func TestDockerVolumeReadOnly(t *testing.T) {
	skipIfNoDocker(t)

	e, err := New()
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	volName := "bhatti-test-ro-" + time.Now().Format("150405.000")
	name := "bhatti-test-romount-" + time.Now().Format("150405.000")

	if out, err := exec.Command("docker", "volume", "create", volName).CombinedOutput(); err != nil {
		t.Fatalf("docker volume create: %v\n%s", err, out)
	}
	defer exec.Command("docker", "volume", "rm", "-f", volName).Run()

	info, err := e.Create(ctx, engine.SandboxSpec{
		Name:     name,
		Image:    "alpine:latest",
		Volumes:  []engine.VolumeMount{{Name: volName, Target: "/data", ReadOnly: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Destroy(ctx, info.EngineID)

	// Writing to a readonly mount must fail
	result, err := e.Exec(ctx, info.EngineID, []string{"sh", "-c", "echo test > /data/file.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode == 0 {
		t.Fatal("expected write to readonly volume to fail, but it succeeded")
	}
}
