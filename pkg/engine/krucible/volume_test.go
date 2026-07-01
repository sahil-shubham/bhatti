//go:build krucible

package krucible

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// TestKrucibleVolume is the Phase-3 block-volume gate: a data disk attached at
// /dev/vdc (after root=vda + config=vdb, exercising the get_block_cfg compose
// fix) and mounted in the guest — writable, and INDEPENDENT of the sandbox: its
// data outlives one sandbox and re-attaches to the next (the defining property
// vs an ephemeral root).
func TestKrucibleVolume(t *testing.T) {
	eng := newBlockRootEngine(t)
	ke := eng.(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Second)
	defer cancel()

	// A standalone ext4 volume image on the host (outlives any sandbox).
	volPath := filepath.Join(t.TempDir(), "vol.ext4")
	if out, err := exec.Command("mke2fs", "-t", "ext4", "-F", "-q", volPath, "64M").CombinedOutput(); err != nil {
		t.Fatalf("mke2fs volume: %v: %s", err, out)
	}
	vol := engine.ResolvedVolume{FilePath: volPath, DriveID: "vol0", Name: "data", Mount: "/data"}

	// sandbox 1: attach the volume, write a marker into it.
	s1, err := eng.Create(ctx, engine.SandboxSpec{Name: "vol1", CPUs: 1, MemoryMB: 512, ResolvedVolumes: []engine.ResolvedVolume{vol}})
	if err != nil {
		t.Fatalf("Create s1 with volume: %v", err)
	}
	const mark = "vol-persist-3c"
	if err := ke.FileWrite(ctx, s1.ID, "/data/vmark", "0644", int64(len(mark)), strings.NewReader(mark)); err != nil {
		eng.Destroy(context.Background(), s1.ID)
		t.Fatalf("write to /data volume in s1 (not attached/mounted?): %v", err)
	}
	var b1 bytes.Buffer
	if _, _, err := ke.FileRead(ctx, s1.ID, "/data/vmark", &b1); err != nil || strings.TrimSpace(b1.String()) != mark {
		eng.Destroy(context.Background(), s1.ID)
		t.Fatalf("read volume marker in s1: err=%v got=%q", err, b1.String())
	}
	// Flush the guest page cache to /dev/vdc so the write reaches the host image
	// before we tear the sandbox down (apps that want durability sync; Destroy is
	// a hard kill). Then the volume file carries the data to the next sandbox.
	if _, err := eng.Exec(ctx, s1.ID, []string{"sync"}); err != nil {
		t.Fatalf("sync s1: %v", err)
	}
	// destroy s1 — the volume image persists on the host.
	if err := eng.Destroy(context.Background(), s1.ID); err != nil {
		t.Fatalf("destroy s1: %v", err)
	}

	// sandbox 2: attach the SAME volume; the marker must still be there.
	s2, err := eng.Create(ctx, engine.SandboxSpec{Name: "vol2", CPUs: 1, MemoryMB: 512, ResolvedVolumes: []engine.ResolvedVolume{vol}})
	if err != nil {
		t.Fatalf("Create s2 with volume: %v", err)
	}
	t.Cleanup(func() { eng.Destroy(context.Background(), s2.ID) })
	var b2 bytes.Buffer
	if _, _, err := ke.FileRead(ctx, s2.ID, "/data/vmark", &b2); err != nil {
		t.Fatalf("read volume marker in s2 (volume not persistent/re-attached?): %v", err)
	}
	if got := strings.TrimSpace(b2.String()); got != mark {
		t.Fatalf("volume marker in s2 = %q, want %q (data did not persist across sandboxes)", got, mark)
	}
}
