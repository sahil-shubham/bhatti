//go:build krucible

package krucible

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// TestKrucibleLeanKernel boots a real block-root image under (a) our own
// external lean kernel and (b) libkrunfw's bundled kernel, and compares
// cold-start (Create = boot → agent-ready). The external-kernel keystone spike.
// Opt-in:
//
//	KRUCIBLE_TEST_BASE_IMAGE=<ext4 block image>   (e.g. dist/krucible-base-alpine.img)
//	KRUCIBLE_LEAN_KERNEL=<raw arm64 Image>        (e.g. dist/leankernel/Image-lean-6.12.91)
func TestKrucibleLeanKernel(t *testing.T) {
	repo := repoRoot(t)
	if !hasLibkrun() {
		t.Skip("libkrun not installed; skipping")
	}
	vmm := filepath.Join(repo, "bhatti-vmm")
	if _, err := os.Stat(vmm); err != nil {
		t.Skip("bhatti-vmm not built — run `make vmm`; skipping")
	}
	img := os.Getenv("KRUCIBLE_TEST_BASE_IMAGE")
	lean := os.Getenv("KRUCIBLE_LEAN_KERNEL")
	if img == "" || lean == "" {
		t.Skip("set KRUCIBLE_TEST_BASE_IMAGE=<ext4> + KRUCIBLE_LEAN_KERNEL=<Image>")
	}

	// bootOnce builds a fresh engine (kernel="" → bundled libkrunfw) and times a
	// Create (boot → lohar agent ready), then verifies the guest is usable.
	bootOnce := func(t *testing.T, kernel string) (time.Duration, string) {
		eng, err := New(Config{
			DataDir: t.TempDir(), BaseImage: img, BlockRoot: true,
			VMMBinary: vmm, LibDir: libDir(), KernelImage: kernel,
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		t0 := time.Now()
		info, err := eng.Create(ctx, engine.SandboxSpec{Name: "k", CPUs: 1, MemoryMB: 512})
		if err != nil {
			t.Fatalf("Create(kernel=%q): %v", kernel, err)
		}
		d := time.Since(t0)
		r, eerr := eng.Exec(ctx, info.ID, []string{"/bin/sh", "-c", "uname -r"})
		eng.Destroy(context.Background(), info.ID)
		if eerr != nil {
			t.Fatalf("exec(kernel=%q): %v", kernel, eerr)
		}
		return d, strings.TrimSpace(r.Stdout)
	}

	const N = 6
	stat := func(label, kernel string) {
		var ds []time.Duration
		var guest string
		for i := 0; i < N; i++ {
			d, g := bootOnce(t, kernel)
			ds = append(ds, d)
			guest = g
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		t.Logf("%-7s kernel=%q guest-uname=%s", label, kernel, guest)
		t.Logf("   create(boot→agent) n=%d  min=%v  median=%v  max=%v",
			N, ds[0], ds[N/2], ds[N-1])
	}

	stat("LEAN", lean) // our external lean kernel
	stat("BUNDLE", "") // libkrunfw bundled kernel
}
