package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// v0.3 CLI Tests — Named Snapshots (Checkpoint/Resume)
// ==========================================================================

func TestCLISnapshotCheckpointAndResume(t *testing.T) {
	c := setupCLITest(t)

	sbName := fmt.Sprintf("cli-snap-src-%d", time.Now().UnixNano()%100000)
	snapName := fmt.Sprintf("cli-ckpt-%d", time.Now().UnixNano()%100000)
	resumeName := fmt.Sprintf("cli-resumed-%d", time.Now().UnixNano()%100000)

	// Create sandbox and write data
	stdout, stderr, code := c.run("create", "--name", sbName)
	if code != 0 {
		t.Fatalf("create exit %d: %s", code, stderr)
	}
	sbID := strings.Fields(stdout)[0]
	c.run("exec", sbName, "--", "sh", "-c", "echo snap-marker > /home/lohar/data.txt")

	// Checkpoint
	stdout, stderr, code = c.run("snapshot", "create", sbName, "--name", snapName)
	if code != 0 {
		c.run("destroy", "-y", sbID)
		t.Fatalf("snapshot create exit %d: %s\nstdout: %s", code, stderr, stdout)
	}
	t.Cleanup(func() { c.run("snapshot", "delete", "-y", snapName) })
	t.Log("✓ snapshot created")

	// Verify source VM still runs after checkpoint
	stdout, _, code = c.run("exec", sbName, "--", "echo", "still-alive")
	if code != 0 || !strings.Contains(stdout, "still-alive") {
		t.Fatalf("VM should be running after checkpoint: exit=%d out=%q", code, stdout)
	}
	t.Log("✓ source VM still running after checkpoint")

	// Destroy source
	c.run("destroy", "-y", sbID)

	// List snapshots
	stdout, _, code = c.run("snapshot", "list")
	if code != 0 {
		t.Fatalf("snapshot list exit %d", code)
	}
	if !strings.Contains(stdout, snapName) {
		t.Fatalf("snapshot not in list: %s", stdout)
	}
	t.Log("✓ snapshot in list")

	// Resume
	stdout, stderr, code = c.run("snapshot", "resume", snapName, "--name", resumeName)
	if code != 0 {
		t.Fatalf("snapshot resume exit %d: %s\nstdout: %s", code, stderr, stdout)
	}
	resumeID := strings.Fields(stdout)[0]
	t.Cleanup(func() { c.run("destroy", "-y", resumeID) })
	t.Log("✓ snapshot resumed")

	// Verify data restored
	stdout, _, code = c.run("exec", resumeName, "--", "cat", "/home/lohar/data.txt")
	if code != 0 || !strings.Contains(stdout, "snap-marker") {
		t.Fatalf("data not restored: exit=%d out=%q", code, stdout)
	}
	t.Log("✓ data restored from snapshot via CLI")
}

func TestCLISnapshotDeleteNonexistent(t *testing.T) {
	c := setupCLITest(t)

	_, _, code := c.run("snapshot", "delete", "-y", "nonexistent-snap-xyz")
	if code == 0 {
		t.Fatal("delete nonexistent snapshot should fail")
	}
	t.Log("✓ delete nonexistent snapshot rejected")
}

// ==========================================================================
// v0.3 CLI Tests — Disk Resize
// ==========================================================================

func TestCLIDiskResize(t *testing.T) {
	c := setupCLITest(t)

	sbName := fmt.Sprintf("cli-disk-%d", time.Now().UnixNano()%100000)

	stdout, stderr, code := c.run("create", "--name", sbName, "--disk-size", "4096")
	if code != 0 {
		t.Fatalf("create with disk-size exit %d: %s", code, stderr)
	}
	sbID := strings.Fields(stdout)[0]
	t.Cleanup(func() { c.run("destroy", "-y", sbID) })

	stdout, _, code = c.run("exec", sbName, "--", "df", "-m", "/")
	if code != 0 {
		t.Fatalf("df exit %d", code)
	}
	lines := strings.Split(stdout, "\n")
	if len(lines) < 2 {
		t.Fatalf("unexpected df output: %q", stdout)
	}
	fields := strings.Fields(lines[1])
	if len(fields) < 2 {
		t.Fatalf("unexpected df fields: %q", lines[1])
	}
	var sizeMB int
	fmt.Sscanf(fields[1], "%d", &sizeMB)
	if sizeMB < 3500 {
		t.Fatalf("expected ~4096MB rootfs, got %dMB", sizeMB)
	}
	t.Logf("✓ rootfs resized to %dMB via --disk-size flag", sizeMB)
}

// ==========================================================================
// v0.3 CLI Tests — Init Script
// ==========================================================================
