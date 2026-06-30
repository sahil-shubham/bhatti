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

// TestKrucibleCheckpointResume is the Phase-2 named-snapshot gate (the server's
// checkpointer + snapshotResumer capabilities): a named memory snapshot of a
// RUNNING sandbox, then resume it into a NEW sandbox. The snapshot is
// independent — it must survive the source being destroyed — and resuming must
// restore guest RAM (a tmpfs marker survives, proving a memory restore, not a
// fresh boot), reusing the in-guest token.
func TestKrucibleCheckpointResume(t *testing.T) {
	eng := newBlockRootEngine(t)
	ke := eng.(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "snapsrc", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	srcDestroyed := false
	t.Cleanup(func() {
		if !srcDestroyed {
			eng.Destroy(context.Background(), info.ID)
		}
	})

	// A marker in tmpfs == guest RAM: survives a memory snapshot, NOT a fresh boot.
	const marker = "ram-marker-9d1f"
	if err := ke.FileWrite(ctx, info.ID, "/tmp/rammark", "0644", int64(len(marker)), strings.NewReader(marker)); err != nil {
		t.Fatalf("FileWrite marker: %v", err)
	}

	snapDir := t.TempDir()
	manifest, err := ke.Checkpoint(ctx, info.ID, "u", 0, "snap1", snapDir)
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	finalDir := filepath.Join(snapDir, "snap1")

	// The source keeps running after the checkpoint (snapshot-in-place).
	if r, err := eng.Exec(ctx, info.ID, []string{"echo", "alive"}); err != nil || !strings.Contains(r.Stdout, "alive") {
		t.Fatalf("source after checkpoint: err=%v out=%q", err, r.Stdout)
	}

	// The snapshot is independent of the source: destroy the source, then resume.
	if err := eng.Destroy(ctx, info.ID); err != nil {
		t.Fatalf("Destroy source: %v", err)
	}
	srcDestroyed = true

	manifestJSON, _ := json.Marshal(manifest)
	info2, err := ke.ResumeFromManifestJSON(ctx, finalDir, manifestJSON, "resumed")
	if err != nil {
		t.Fatalf("ResumeFromManifestJSON: %v", err)
	}
	t.Cleanup(func() { eng.Destroy(context.Background(), info2.ID) })

	// Guest RAM was restored (the tmpfs marker is present) — a memory restore,
	// not a fresh boot.
	var buf bytes.Buffer
	if _, _, err := ke.FileRead(ctx, info2.ID, "/tmp/rammark", &buf); err != nil {
		t.Fatalf("FileRead marker after resume: %v", err)
	}
	if got := strings.TrimSpace(buf.String()); got != marker {
		t.Fatalf("tmpfs marker after resume = %q, want %q (guest RAM not restored)", got, marker)
	}

	// And the restored sandbox is functional.
	if r, err := eng.Exec(ctx, info2.ID, []string{"echo", "restored"}); err != nil || !strings.Contains(r.Stdout, "restored") {
		t.Fatalf("exec on resumed sandbox: err=%v out=%q", err, r.Stdout)
	}
}
