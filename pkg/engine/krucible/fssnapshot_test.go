//go:build krucible

package krucible

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// TestKrucibleFilesystemSnapshot is the Phase-2 #5 gate: a `--type filesystem`
// snapshot is disk-only — it captures the filesystem but NOT RAM, and restores
// with a cold boot. So a disk marker (/etc) survives into the resumed sandbox,
// but a tmpfs/RAM marker (/tmp) does NOT (the cold boot starts tmpfs fresh) —
// the distinction from a memory snapshot.
func TestKrucibleFilesystemSnapshot(t *testing.T) {
	eng := newBlockRootEngine(t)
	ke := eng.(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	src, err := eng.Create(ctx, engine.SandboxSpec{Name: "fssrc", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { eng.Destroy(context.Background(), src.ID) })

	const diskMark = "disk-mark-a1"
	const ramMark = "ram-mark-b2"
	if err := ke.FileWrite(ctx, src.ID, "/etc/fsmark", "0644", int64(len(diskMark)), strings.NewReader(diskMark)); err != nil {
		t.Fatalf("write disk marker: %v", err)
	}
	if err := ke.FileWrite(ctx, src.ID, "/tmp/rammark", "0644", int64(len(ramMark)), strings.NewReader(ramMark)); err != nil {
		t.Fatalf("write ram marker: %v", err)
	}

	snapDir := t.TempDir()
	manifest, err := ke.CheckpointTyped(ctx, src.ID, "u", 0, "fs1", snapDir, "filesystem")
	if err != nil {
		t.Fatalf("CheckpointTyped(filesystem): %v", err)
	}

	manifestJSON, _ := json.Marshal(manifest)
	info2, err := ke.ResumeFromManifestJSON(ctx, filepath.Join(snapDir, "fs1"), manifestJSON, "fsresumed")
	if err != nil {
		t.Fatalf("ResumeFromManifestJSON (filesystem): %v", err)
	}
	t.Cleanup(func() { eng.Destroy(context.Background(), info2.ID) })

	// The disk (filesystem) was restored.
	var disk bytes.Buffer
	if _, _, err := ke.FileRead(ctx, info2.ID, "/etc/fsmark", &disk); err != nil {
		t.Fatalf("FileRead disk marker after fs resume: %v", err)
	}
	if got := strings.TrimSpace(disk.String()); got != diskMark {
		t.Fatalf("disk marker after fs resume = %q, want %q", got, diskMark)
	}

	// RAM was NOT restored: the tmpfs marker is gone (a cold boot, not a memory restore).
	var ram bytes.Buffer
	if _, _, err := ke.FileRead(ctx, info2.ID, "/tmp/rammark", &ram); err == nil && strings.TrimSpace(ram.String()) == ramMark {
		t.Fatalf("tmpfs marker survived a filesystem snapshot — expected a cold boot (no RAM restore)")
	}
}
