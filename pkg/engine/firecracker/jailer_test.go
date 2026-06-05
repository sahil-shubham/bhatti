//go:build linux

package firecracker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testJailedEngine returns an engine with the jailer configured.
// Skips the test if jailer binary isn't available.
func testJailedEngine(t *testing.T) *Engine {
	t.Helper()
	skipIfNotRoot(t)

	jailerPath := "/usr/local/bin/jailer"
	if _, err := os.Stat(jailerPath); err != nil {
		t.Skipf("jailer not found at %s — skipping jailer tests", jailerPath)
	}

	// Ensure bhatti-vm user exists
	if _, err := exec.Command("id", "-u", "bhatti-vm").Output(); err != nil {
		t.Skip("bhatti-vm user not found — skipping jailer tests")
	}

	arch := "arm64"
	if out, _ := os.ReadFile("/proc/cpuinfo"); strings.Contains(string(out), "GenuineIntel") || strings.Contains(string(out), "AuthenticAMD") {
		arch = "amd64"
	}

	eng, err := New(Config{
		DataDir:      "/var/lib/bhatti",
		KernelPath:   fmt.Sprintf("/var/lib/bhatti/images/vmlinux-%s", arch),
		BaseRootfs:   fmt.Sprintf("/var/lib/bhatti/images/rootfs-minimal-%s.ext4", arch),
		FCBinary:     "/usr/local/bin/firecracker",
		JailerBinary: jailerPath,
		JailUID:      10000,
		JailGID:      10000,
	})
	if err != nil {
		t.Fatalf("New (jailed): %v", err)
	}
	// Same as testEngine: after G1.1 the engine owns DNS responder
	// goroutines bound to bridge gateway IPs. Without Shutdown each
	// test leaks the responder and the next test on the same subnet
	// hits "bind: address already in use". Caught in integration run
	// 26741170133 when TestRecoveryMultiVolumeOrdering (which uses
	// testJailedEngine, not testEngine) didn't clean up its DNS and
	// every subsequent thermal_test.go test ran without a responder.
	t.Cleanup(eng.Shutdown)
	return eng
}

func TestJailerBootAndExec(t *testing.T) {
	eng := testJailedEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("jail-boot"))
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Destroy(ctx, info.ID)

	r, err := execWithTimeout(t, eng, info.ID, []string{"echo", "jailed-hello"})
	if err != nil || r.ExitCode != 0 {
		t.Fatalf("exec: err=%v exit=%d stderr=%s", err, r.ExitCode, r.Stderr)
	}
	if !strings.Contains(r.Stdout, "jailed-hello") {
		t.Errorf("expected 'jailed-hello', got %q", r.Stdout)
	}
}

func TestJailerChroot(t *testing.T) {
	eng := testJailedEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("jail-chroot"))
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Verify chroot by checking that FC cannot see host files.
	// The guest agent runs inside the VM (not the chroot), so we can't
	// directly test the chroot from exec. Instead verify the jail dir
	// exists and the FC process is running with the expected UID.
	vm, _ := eng.getVM(info.ID)
	if vm.jailRoot == "" {
		t.Fatal("expected jailRoot to be set")
	}
	if _, err := os.Stat(vm.jailRoot); err != nil {
		t.Errorf("jail root dir should exist: %v", err)
	}
	// Verify the chroot contains the FC binary and devices
	entries, _ := os.ReadDir(vm.jailRoot)
	names := make(map[string]bool)
	for _, e := range entries {
		names[e.Name()] = true
	}
	if !names["firecracker"] {
		t.Error("firecracker binary not found in chroot")
	}
	if !names["dev"] {
		t.Error("/dev not found in chroot")
	}
	if !names["rootfs.ext4"] {
		t.Error("rootfs.ext4 not found in chroot")
	}
}

func TestJailerUIDDrop(t *testing.T) {
	eng := testJailedEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("jail-uid"))
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Destroy(ctx, info.ID)

	vm, _ := eng.getVM(info.ID)
	if vm.cmd == nil || vm.cmd.Process == nil {
		t.Fatal("FC process not found")
	}

	// Read the UID of the FC process
	statusBytes, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", vm.cmd.Process.Pid))
	if err != nil {
		t.Fatalf("read proc status: %v", err)
	}
	status := string(statusBytes)

	// Look for Uid line — format: "Uid: real effective saved fs"
	for _, line := range strings.Split(status, "\n") {
		if strings.HasPrefix(line, "Uid:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[1] != "10000" {
				// The jailer process starts as root then drops — check effective (field 2)
				if len(fields) >= 3 && fields[2] != "10000" {
					t.Errorf("expected effective UID 10000, got: %s", line)
				}
			}
			break
		}
	}
}

func TestJailerCgroupLimits(t *testing.T) {
	eng := testJailedEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("jail-cgroup"))
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Check cgroup v2 limits
	cgroupPath := fmt.Sprintf("/sys/fs/cgroup/firecracker/%s", info.ID)

	// cpu.max
	cpuMax, err := os.ReadFile(filepath.Join(cgroupPath, "cpu.max"))
	if err != nil {
		t.Logf("cpu.max not found at %s (cgroup may use different path): %v", cgroupPath, err)
	} else if !strings.Contains(string(cpuMax), "100000") {
		t.Errorf("unexpected cpu.max: %s", string(cpuMax))
	}

	// memory.max
	memMax, err := os.ReadFile(filepath.Join(cgroupPath, "memory.max"))
	if err != nil {
		t.Logf("memory.max not found: %v", err)
	} else {
		memStr := strings.TrimSpace(string(memMax))
		if memStr == "max" {
			t.Error("expected memory.max to be set, got 'max' (unlimited)")
		}
	}
}

func TestJailerSnapshotRoundtrip(t *testing.T) {
	eng := testJailedEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("jail-snap"))
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Write data
	execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "echo jail-snap-data > /tmp/jailtest"})

	// Stop (creates snapshot inside chroot, moves to sandbox dir)
	if err := eng.Stop(ctx, info.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Verify snapshot files are in sandbox dir (not chroot)
	sandboxDir := filepath.Join(eng.cfg.DataDir, "sandboxes", info.ID)
	for _, name := range []string{"vm.snap", "mem.snap"} {
		fi, err := os.Stat(filepath.Join(sandboxDir, name))
		if err != nil {
			t.Errorf("%s not found in sandbox dir: %v", name, err)
		} else if fi.Size() == 0 {
			t.Errorf("%s is empty", name)
		}
	}

	// Start (resumes from snapshot via new chroot)
	if err := eng.Start(ctx, info.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Verify data survived
	r, err := execWithTimeout(t, eng, info.ID, []string{"cat", "/tmp/jailtest"})
	if err != nil || !strings.Contains(r.Stdout, "jail-snap-data") {
		t.Fatalf("data lost after snapshot roundtrip: err=%v out=%q", err, r.Stdout)
	}
}

func TestJailerCleanupOnDestroy(t *testing.T) {
	eng := testJailedEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("jail-cleanup"))
	if err != nil {
		t.Fatal(err)
	}

	jailDir := filepath.Join(eng.cfg.DataDir, "jails", "firecracker", info.ID)
	if _, err := os.Stat(jailDir); err != nil {
		t.Errorf("jail dir should exist while VM is running: %v", err)
	}

	eng.Destroy(ctx, info.ID)

	if _, err := os.Stat(jailDir); err == nil {
		t.Error("jail dir should be removed after destroy")
	}
}

func TestJailerDevModeFallback(t *testing.T) {
	// Bare mode — no jailer configured. This is the existing test engine.
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("bare-mode"))
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Destroy(ctx, info.ID)

	r, err := execWithTimeout(t, eng, info.ID, []string{"echo", "bare-ok"})
	if err != nil || !strings.Contains(r.Stdout, "bare-ok") {
		t.Fatalf("bare mode exec: err=%v out=%q", err, r.Stdout)
	}

	// jailRoot should be empty in bare mode
	vm, _ := eng.getVM(info.ID)
	if vm.jailRoot != "" {
		t.Errorf("expected empty jailRoot in bare mode, got %q", vm.jailRoot)
	}
}
