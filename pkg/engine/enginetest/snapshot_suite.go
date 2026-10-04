package enginetest

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

// checkpointEngine is the optional named memory-checkpoint/restore surface.
type checkpointEngine interface {
	Checkpoint(ctx context.Context, sandboxID, userID string, subnetIndex int, snapName, snapDir string) (any, error)
	ResumeFromManifestJSON(ctx context.Context, snapDir string, manifestJSON []byte, newName, ownerUserID string) (engine.SandboxInfo, error)
}

// RunSnapshotSuite checks that a named checkpoint restores RAM and a usable
// guest into a new sandbox without stopping the original.
func RunSnapshotSuite(t *testing.T, newEngine NewEngine) {
	eng := newEngine(t) // may t.Skip
	cp, ok := eng.(checkpointEngine)
	if !ok {
		t.Skip("engine does not implement named checkpoint/restore")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "snap", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := info.ID
	t.Cleanup(func() { eng.Destroy(context.Background(), id) })

	fe, ok := eng.(fileEngine)
	if !ok {
		t.Skip("engine does not implement the file surface")
	}

	// A marker in tmpfs lives in guest RAM; a fresh boot cannot recover it.
	const marker = "checkpoint-marker-7f3a"
	if err := fe.FileWrite(ctx, id, "/tmp/snap-marker", "0644", int64(len(marker)), strings.NewReader(marker)); err != nil {
		t.Fatalf("FileWrite marker: %v", err)
	}
	snapParent := t.TempDir()
	manifest, err := cp.Checkpoint(ctx, id, "", 0, "memory", snapParent)
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if s, err := eng.Status(ctx, id); err != nil || s.Status != "running" {
		t.Fatalf("source after checkpoint status = %q (err %v), want running", s.Status, err)
	}
	if r, err := eng.Exec(ctx, id, []string{"echo", "still-running"}); err != nil || !strings.Contains(r.Stdout, "still-running") {
		t.Fatalf("source exec after checkpoint: err=%v out=%q", err, r.Stdout)
	}

	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	restored, err := cp.ResumeFromManifestJSON(ctx, filepath.Join(snapParent, "memory"), manifestJSON, "restored", "")
	if err != nil {
		t.Fatalf("ResumeFromManifestJSON: %v", err)
	}
	t.Cleanup(func() { eng.Destroy(context.Background(), restored.ID) })
	if restored.ID == id {
		t.Fatal("restore reused the source sandbox instead of creating a new one")
	}
	if r, err := eng.Exec(ctx, restored.ID, []string{"echo", "post-restore"}); err != nil || !strings.Contains(r.Stdout, "post-restore") {
		t.Fatalf("exec-after-restore: err=%v out=%q", err, r.Stdout)
	}
	var buf bytes.Buffer
	if _, _, err := fe.FileRead(ctx, restored.ID, "/tmp/snap-marker", &buf); err != nil {
		t.Fatalf("FileRead marker after restore: %v", err)
	}
	if buf.String() != marker {
		t.Fatalf("tmpfs marker after restore = %q, want %q (guest RAM not restored)", buf.String(), marker)
	}
}
