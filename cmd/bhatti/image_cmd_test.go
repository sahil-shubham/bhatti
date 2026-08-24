package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// ==========================================================================
// v0.3 CLI Tests — Images
// ==========================================================================

func TestCLIImagePullAndBoot(t *testing.T) {
	c := setupCLITest(t)

	imgName := fmt.Sprintf("cli-alpine-%d", time.Now().UnixNano()%100000)
	sbName := fmt.Sprintf("cli-img-sb-%d", time.Now().UnixNano()%100000)

	// Pull alpine (async — returns task ID)
	stdout, stderr, code := c.run("image", "pull", "alpine:latest", "--name", imgName)
	if code != 0 {
		t.Fatalf("image pull exit %d: %s\nstdout: %s", code, stderr, stdout)
	}
	t.Logf("pull output: %s", strings.TrimSpace(stdout))

	// The CLI should poll until completion. Verify success message.
	if !strings.Contains(stdout, imgName) && !strings.Contains(stdout, "pulled") &&
		!strings.Contains(stdout, "completed") {
		// The pull may still be async — check image list
		t.Log("pull may be async, waiting for image to appear...")
		for i := 0; i < 60; i++ {
			time.Sleep(2 * time.Second)
			listOut, _, _ := c.run("image", "list")
			if strings.Contains(listOut, imgName) {
				t.Log("✓ image appeared in list")
				goto pullDone
			}
		}
		t.Fatal("image did not appear in list after 120s")
	}
pullDone:
	t.Cleanup(func() { c.run("image", "delete", "-y", imgName) })

	// List should show the image
	stdout, _, code = c.run("image", "list")
	if code != 0 {
		t.Fatalf("image list exit %d", code)
	}
	if !strings.Contains(stdout, imgName) {
		t.Fatalf("image not in list: %s", stdout)
	}
	t.Log("✓ image in list")

	// Boot from the pulled image
	stdout, stderr, code = c.run("create", "--name", sbName, "--image", imgName)
	if code != 0 {
		t.Fatalf("create with image exit %d: %s", code, stderr)
	}
	sbID := strings.Fields(stdout)[0]
	t.Cleanup(func() { c.run("destroy", "-y", sbID) })

	// Verify it's Alpine
	stdout, _, code = c.run("exec", sbName, "--", "cat", "/etc/os-release")
	if code != 0 || !strings.Contains(stdout, "Alpine") {
		t.Fatalf("expected Alpine: %s", stdout)
	}
	t.Log("✓ booted from pulled OCI image via CLI")
}

func TestCLIImageSaveAndBoot(t *testing.T) {
	c := setupCLITest(t)

	srcName := fmt.Sprintf("cli-save-src-%d", time.Now().UnixNano()%100000)
	imgName := fmt.Sprintf("cli-saved-%d", time.Now().UnixNano()%100000)
	dstName := fmt.Sprintf("cli-save-dst-%d", time.Now().UnixNano()%100000)

	// Create source sandbox and write a marker
	stdout, stderr, code := c.run("create", "--name", srcName)
	if code != 0 {
		t.Fatalf("create src exit %d: %s", code, stderr)
	}
	srcID := strings.Fields(stdout)[0]
	c.run("exec", srcName, "--", "sh", "-c", "echo cli-saved-marker > /home/lohar/marker.txt")

	// Save image
	stdout, stderr, code = c.run("image", "save", srcName, "--name", imgName)
	if code != 0 {
		c.run("destroy", "-y", srcID)
		t.Fatalf("image save exit %d: %s", code, stderr)
	}
	c.run("destroy", "-y", srcID)
	t.Cleanup(func() { c.run("image", "delete", "-y", imgName) })
	t.Log("✓ image saved from running sandbox")

	// Boot from saved image
	stdout, stderr, code = c.run("create", "--name", dstName, "--image", imgName)
	if code != 0 {
		t.Fatalf("create from saved exit %d: %s", code, stderr)
	}
	dstID := strings.Fields(stdout)[0]
	t.Cleanup(func() { c.run("destroy", "-y", dstID) })

	// Verify marker
	stdout, _, code = c.run("exec", dstName, "--", "cat", "/home/lohar/marker.txt")
	if code != 0 || !strings.Contains(stdout, "cli-saved-marker") {
		t.Fatalf("marker not found: %s", stdout)
	}
	t.Log("✓ booted from saved image, marker present")
}

func TestCLIImageDeleteNonexistent(t *testing.T) {
	c := setupCLITest(t)

	_, _, code := c.run("image", "delete", "-y", "nonexistent-image-xyz")
	if code == 0 {
		t.Fatal("delete nonexistent image should fail")
	}
	t.Log("✓ delete nonexistent image rejected")
}

// ==========================================================================
