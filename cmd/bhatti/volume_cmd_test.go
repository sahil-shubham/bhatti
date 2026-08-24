package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// v0.3 CLI Tests — Persistent Volumes
// ==========================================================================

func TestCLIVolumeCRUD(t *testing.T) {
	c := setupCLITest(t)

	name := fmt.Sprintf("cli-vol-%d", time.Now().UnixNano()%100000)

	// Create
	stdout, stderr, code := c.run("volume", "create", "--name", name, "--size", "64")
	if code != 0 {
		t.Fatalf("volume create exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, name) {
		t.Fatalf("volume create output missing name: %s", stdout)
	}
	t.Logf("✓ volume create: %s", strings.TrimSpace(stdout))

	// List
	stdout, _, code = c.run("volume", "list")
	if code != 0 {
		t.Fatalf("volume list exit %d", code)
	}
	if !strings.Contains(stdout, name) {
		t.Fatalf("volume not in list: %s", stdout)
	}
	t.Log("✓ volume list shows created volume")

	// Delete
	stdout, _, code = c.run("volume", "delete", "-y", name)
	if code != 0 {
		t.Fatalf("volume delete exit %d", code)
	}
	t.Log("✓ volume deleted")

	// Verify gone
	stdout, _, _ = c.run("volume", "list")
	if strings.Contains(stdout, name) {
		t.Error("volume still in list after delete")
	}
	t.Log("✓ volume absent from list after delete")
}

func TestCLIVolumeResize(t *testing.T) {
	c := setupCLITest(t)

	name := fmt.Sprintf("cli-resize-%d", time.Now().UnixNano()%100000)

	c.run("volume", "create", "--name", name, "--size", "64")
	t.Cleanup(func() { c.run("volume", "delete", "-y", name) })

	// Resize up
	stdout, stderr, code := c.run("volume", "resize", name, "--size", "128")
	if code != 0 {
		t.Fatalf("volume resize exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "128") && !strings.Contains(stdout, "resized") {
		t.Fatalf("resize output: %s", stdout)
	}
	t.Log("✓ volume resized to 128MB")

	// Resize down should fail
	_, stderr, code = c.run("volume", "resize", name, "--size", "32")
	if code == 0 {
		t.Fatal("resize shrink should fail")
	}
	t.Logf("✓ volume resize shrink rejected: %s", strings.TrimSpace(stderr))
}

func TestCLIVolumeAttachToSandbox(t *testing.T) {
	c := setupCLITest(t)

	volName := fmt.Sprintf("cli-attach-%d", time.Now().UnixNano()%100000)
	sbName := fmt.Sprintf("cli-vol-sb-%d", time.Now().UnixNano()%100000)

	// Create volume
	c.run("volume", "create", "--name", volName, "--size", "64")
	t.Cleanup(func() { c.run("volume", "delete", "-y", volName) })

	// Create sandbox with volume attached
	stdout, stderr, code := c.run("create", "--name", sbName,
		"--volume", volName+":/workspace")
	if code != 0 {
		t.Fatalf("create with volume exit %d: %s", code, stderr)
	}
	sbID := strings.Fields(stdout)[0]
	t.Cleanup(func() { c.run("destroy", "-y", sbID) })
	t.Log("✓ sandbox created with volume attached")

	// Write data to volume
	stdout, stderr, code = c.run("exec", sbName, "--", "sh", "-c",
		"echo cli-vol-data > /workspace/test.txt && cat /workspace/test.txt")
	if code != 0 {
		t.Fatalf("exec write exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "cli-vol-data") {
		t.Fatalf("write verification failed: %s", stdout)
	}
	t.Log("✓ wrote data to volume via exec")

	// Destroy sandbox
	c.run("destroy", "-y", sbID)
	t.Log("✓ sandbox destroyed, volume should survive")

	// Create new sandbox with same volume
	sbName2 := sbName + "-2"
	stdout, stderr, code = c.run("create", "--name", sbName2,
		"--volume", volName+":/workspace")
	if code != 0 {
		t.Fatalf("create sb2 exit %d: %s", code, stderr)
	}
	sbID2 := strings.Fields(stdout)[0]
	t.Cleanup(func() { c.run("destroy", "-y", sbID2) })

	// Read data back
	stdout, _, code = c.run("exec", sbName2, "--", "cat", "/workspace/test.txt")
	if code != 0 || !strings.Contains(stdout, "cli-vol-data") {
		t.Fatalf("data did not persist: exit=%d stdout=%q", code, stdout)
	}
	t.Log("✓ data persists across sandbox destroy/recreate via CLI")
}

func TestCLIVolumeReadOnly(t *testing.T) {
	c := setupCLITest(t)

	volName := fmt.Sprintf("cli-ro-%d", time.Now().UnixNano()%100000)
	sbName := fmt.Sprintf("cli-ro-sb-%d", time.Now().UnixNano()%100000)

	// Create and seed volume with data
	c.run("volume", "create", "--name", volName, "--size", "64")
	t.Cleanup(func() { c.run("volume", "delete", "-y", volName) })

	// Seed with data (RW mount)
	stdout, stderr, code := c.run("create", "--name", sbName, "--volume", volName+":/data")
	if code != 0 {
		t.Fatalf("create RW exit %d: %s", code, stderr)
	}
	sbID := strings.Fields(stdout)[0]
	c.run("exec", sbName, "--", "sh", "-c", "echo ro-test-data > /data/file.txt")
	c.run("destroy", "-y", sbID)

	// Now mount read-only
	sbName2 := sbName + "-ro"
	stdout, stderr, code = c.run("create", "--name", sbName2, "--volume", volName+":/data:ro")
	if code != 0 {
		t.Fatalf("create RO exit %d: %s", code, stderr)
	}
	sbID2 := strings.Fields(stdout)[0]
	t.Cleanup(func() { c.run("destroy", "-y", sbID2) })

	// Data should be readable
	stdout, _, code = c.run("exec", sbName2, "--", "cat", "/data/file.txt")
	if !strings.Contains(stdout, "ro-test-data") {
		t.Fatalf("expected data readable in RO: %q", stdout)
	}
	t.Log("✓ data readable through RO volume")

	// Write should fail
	stdout, _, code = c.run("exec", sbName2, "--", "sh", "-c", "touch /data/nope 2>&1; echo exit=$?")
	if !strings.Contains(stdout, "Read-only") {
		t.Fatalf("expected Read-only error: %q", stdout)
	}
	t.Log("✓ write rejected on RO volume")
}

func TestCLIVolumeDeleteWhileAttached(t *testing.T) {
	c := setupCLITest(t)

	volName := fmt.Sprintf("cli-del-att-%d", time.Now().UnixNano()%100000)
	sbName := fmt.Sprintf("cli-del-sb-%d", time.Now().UnixNano()%100000)

	c.run("volume", "create", "--name", volName, "--size", "64")
	stdout, _, _ := c.run("create", "--name", sbName, "--volume", volName+":/workspace")
	sbID := strings.Fields(stdout)[0]
	t.Cleanup(func() {
		c.run("destroy", "-y", sbID)
		c.run("volume", "delete", "-y", volName)
	})

	// Delete should fail while attached
	_, stderr, code := c.run("volume", "delete", "-y", volName)
	if code == 0 {
		t.Fatal("volume delete should fail while attached")
	}
	if !strings.Contains(stderr, "attachment") {
		t.Fatalf("expected attachment error: %s", stderr)
	}
	t.Log("✓ volume delete blocked while attached")
}

