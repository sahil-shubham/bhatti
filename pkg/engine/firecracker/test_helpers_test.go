//go:build linux

package firecracker

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// These tests require root + Firecracker + kernel + rootfs on the Pi.
// Run: sudo go test -v -count=1 -timeout=120s ./pkg/engine/firecracker/

func skipIfNotRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("must run as root")
	}
}

func testEngine(t *testing.T) *Engine {
	t.Helper()
	skipIfNotRoot(t)

	// Auto-detect architecture for image paths
	arch := "arm64"
	if out, _ := os.ReadFile("/proc/cpuinfo"); strings.Contains(string(out), "GenuineIntel") || strings.Contains(string(out), "AuthenticAMD") {
		arch = "amd64"
	}

	eng, err := New(Config{
		DataDir:    "/var/lib/bhatti",
		KernelPath: fmt.Sprintf("/var/lib/bhatti/images/vmlinux-%s", arch),
		BaseRootfs: fmt.Sprintf("/var/lib/bhatti/images/rootfs-minimal-%s.ext4", arch),
		FCBinary:   "/usr/local/bin/firecracker",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Pre-G1.1 the engine had no long-lived background goroutines, so
	// tests could just let it go out of scope. Now Engine.New spawns a
	// per-user DNS responder bound to the bridge gateway IP. Without
	// explicit Shutdown each test leaks the goroutine + holds the port
	// 53 bind, and consecutive tests on the same subnet (e.g. all the
	// thermal_test.go tests on usr_test/subnet 99) fail to start their
	// own responder with "address already in use".
	t.Cleanup(eng.Shutdown)
	return eng
}

func testSpec(name string) engine.SandboxSpec {
	return engine.SandboxSpec{
		Name: name, CPUs: 1, MemoryMB: 512,
		UserID: "usr_test", SubnetIndex: 99, // test user on isolated subnet
	}
}

// execWithTimeout wraps eng.Exec with a 15-second timeout to prevent tests
// from hanging if a command blocks (e.g. ping after VM resume).
func execWithTimeout(t *testing.T, eng *Engine, id string, cmd []string) (engine.ExecResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return eng.Exec(ctx, id, cmd)
}

