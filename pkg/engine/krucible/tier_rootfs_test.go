//go:build krucible

package krucible

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// TestKrucibleTierRootfsBoots boots a REAL release-tier rootfs (built by
// scripts/build-tier.sh) on the krucible engine — the coverage gap that let
// v2.1.0 ship rootfs images missing /init.krun. The other krucible tests build
// their own minimal rootfs (buildBaseRootfs), so they never exercised the
// production tier images. CI sets KRUCIBLE_TIER_ROOTFS to a freshly built tier
// ext4; skipped otherwise.
func TestKrucibleTierRootfsBoots(t *testing.T) {
	img := os.Getenv("KRUCIBLE_TIER_ROOTFS")
	if img == "" {
		t.Skip("KRUCIBLE_TIER_ROOTFS not set (path to a real tier ext4 image); CI provides it")
	}
	if _, err := os.Stat(img); err != nil {
		t.Fatalf("tier rootfs %s: %v", img, err)
	}
	repo := repoRoot(t)
	if !hasLibkrun() {
		t.Skip("libkrun not installed; skipping")
	}
	if !hasHypervisor() {
		t.Skip("no hypervisor (/dev/kvm or HVF); skipping")
	}
	vmm := filepath.Join(repo, "bhatti-vmm")
	netd := filepath.Join(repo, "bhatti-netd")
	for _, p := range []string{vmm, netd} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("%s not built; skipping", filepath.Base(p))
		}
	}
	ensureVMMSigned(t, vmm)

	eng, err := New(Config{
		DataDir:     t.TempDir(),
		SocketDir:   shortSockDir(t),
		BaseImage:   img, // block-root from the REAL tier image
		BlockRoot:   true,
		VMMBinary:   vmm,
		LibDir:      libDir(),
		NetBackend:  true,
		NetdBinary:  netd,
		KernelImage: os.Getenv("KRUCIBLE_LEAN_KERNEL"),
	})
	if err != nil {
		t.Fatalf("New(tier): %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "tierboot", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("create from tier rootfs %s failed — is it krucible-bootable (has /init.krun)? %v", img, err)
	}
	t.Cleanup(func() { eng.Destroy(context.Background(), info.ID) })

	// The guest booted (lohar as PID 1) iff the agent answers an exec.
	r, err := eng.Exec(ctx, info.ID, []string{"sh", "-c", "echo booted-ok"})
	if err != nil || r.ExitCode != 0 || !strings.Contains(r.Stdout, "booted-ok") {
		t.Fatalf("tier rootfs did not boot cleanly: err=%v exit=%d out=%q", err, r.ExitCode, strings.TrimSpace(r.Stdout))
	}
}
