//go:build krucible

package krucible

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/oci"
)

// TestKrucibleProductionImage boots a REAL OCI-derived rootfs (a full userland
// with a shell + coreutils, not the toy multi-call util) under the block-root
// cold path and exercises a real shell plus the cold snapshot round-trip — the
// production use case. Opt-in (slow / needs a real image), one of:
//
//	KRUCIBLE_TEST_BASE_IMAGE=<prebuilt ext4>   # fast, CI-friendly
//	KRUCIBLE_TEST_OCI_REF=<ref e.g. alpine>    # built here via oci.PullAndConvert (needs network)
func TestKrucibleProductionImage(t *testing.T) {
	repo := repoRoot(t)
	if !hasLibkrun() {
		t.Skip("libkrun not installed; skipping")
	}
	vmm := filepath.Join(repo, "bhatti-vmm")
	if _, err := os.Stat(vmm); err != nil {
		t.Skip("bhatti-vmm not built — run `make vmm`; skipping")
	}

	img := os.Getenv("KRUCIBLE_TEST_BASE_IMAGE")
	if img == "" {
		ref := os.Getenv("KRUCIBLE_TEST_OCI_REF")
		if ref == "" {
			t.Skip("set KRUCIBLE_TEST_BASE_IMAGE=<ext4> or KRUCIBLE_TEST_OCI_REF=<image ref>")
		}
		img = buildOCIImage(t, repo, ref)
	}

	eng, err := New(Config{
		DataDir:   t.TempDir(),
		BaseImage: img,
		BlockRoot: true,
		VMMBinary: vmm,
		LibDir:    libDir(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "prod", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("Create (real image): %v", err)
	}
	id := info.ID
	t.Cleanup(func() { eng.Destroy(context.Background(), id) })

	// A real POSIX shell with arithmetic — the toy rootfs can't do this.
	if r, err := eng.Exec(ctx, id, []string{"/bin/sh", "-c", "echo $((6*7))"}); err != nil || strings.TrimSpace(r.Stdout) != "42" {
		t.Fatalf("real shell exec: err=%v out=%q", err, r.Stdout)
	}
	// Real coreutils.
	if r, err := eng.Exec(ctx, id, []string{"/bin/sh", "-c", "uname -a && cat /etc/os-release | head -1"}); err != nil || r.Stdout == "" {
		t.Fatalf("uname/os-release: err=%v out=%q", err, r.Stdout)
	} else {
		t.Logf("guest: %s", strings.TrimSpace(r.Stdout))
	}

	// Cold round-trip on the real userland.
	if err := eng.Stop(ctx, id); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := eng.Start(ctx, id); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if r, err := eng.Exec(ctx, id, []string{"/bin/sh", "-c", "echo restored-$((1+1))"}); err != nil || strings.TrimSpace(r.Stdout) != "restored-2" {
		t.Fatalf("exec-after-restore on real image: err=%v out=%q", err, r.Stdout)
	}
}

// buildOCIImage cross-builds lohar and converts an OCI ref to an ext4 root image
// (with /init.krun -> lohar) via the production pipeline.
func buildOCIImage(t *testing.T, repo, ref string) string {
	t.Helper()
	guestArch := "arm64"
	if v := os.Getenv("KRUCIBLE_GUEST_ARCH"); v != "" {
		guestArch = v
	}
	dir := t.TempDir()
	loharPath := filepath.Join(dir, "lohar")
	build := exec.Command("go", "build", "-o", loharPath, "./cmd/lohar")
	build.Dir = repo
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+guestArch, "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("cross-build lohar: %s: %v", out, err)
	}

	out := filepath.Join(dir, "root.img")
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	if _, err := oci.PullAndConvert(ctx, ref, out, loharPath, oci.WithPlatform("linux", guestArch)); err != nil {
		t.Skipf("OCI pull/convert %q failed (network?): %v", ref, err)
	}
	return out
}
