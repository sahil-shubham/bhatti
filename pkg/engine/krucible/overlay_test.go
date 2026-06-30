//go:build krucible

package krucible

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestKrucibleCreateOverlay validates the qcow2 overlay create primitive — the
// storage layer's foundation. It exercises `bhatti-vmm create-overlay`, which
// calls krun_create_disk_overlay → imago (the same library that opens these
// images at boot): a raw base + an overlay over it must yield a small, valid
// qcow2 v3 that records the backing — instant + host-FS-independent, no qemu-img.
// (The end-to-end "imago boots it" check is the cold-tier suites on qcow2.)
func TestKrucibleCreateOverlay(t *testing.T) {
	repo := repoRoot(t)
	if !hasLibkrun() {
		t.Skip("libkrun not installed (pkg-config libkrun); skipping")
	}
	vmm := filepath.Join(repo, "bhatti-vmm")
	if _, err := os.Stat(vmm); err != nil {
		t.Skip("bhatti-vmm not built — run `make vmm`; skipping")
	}

	dir := t.TempDir()
	base := filepath.Join(dir, "base.raw")
	overlay := filepath.Join(dir, "ovl.qcow2")
	const size int64 = 64 * 1024 * 1024 // 64 MiB

	f, err := os.Create(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	f.Close()

	cmd := exec.Command(vmm, "create-overlay", overlay, base, "67108864")
	if ld := libDir(); ld != "" {
		cmd.Env = append(os.Environ(),
			"DYLD_FALLBACK_LIBRARY_PATH="+ld, "LD_LIBRARY_PATH="+ld)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create-overlay: %v\n%s", err, out)
	}

	got, err := os.ReadFile(overlay)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(got, []byte("QFI\xfb")) {
		t.Fatalf("overlay is not a qcow2 (magic %x)", got[:4])
	}
	// qcow2 version is bytes [4:8], big-endian — expect 3.
	if v := uint32(got[4])<<24 | uint32(got[5])<<16 | uint32(got[6])<<8 | uint32(got[7]); v != 3 {
		t.Fatalf("qcow2 version = %d, want 3", v)
	}
	// An empty overlay is metadata-only — a tiny delta, never a copy of the base.
	if int64(len(got)) > size/10 {
		t.Fatalf("overlay is %d bytes — expected a small delta over a %d-byte base", len(got), size)
	}
	// The backing path must be recorded in the overlay header.
	if !bytes.Contains(got, []byte(base)) {
		t.Fatalf("overlay does not record backing path %q", base)
	}
}
