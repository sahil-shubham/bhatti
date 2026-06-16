package enginetest

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// RunSnapshotSuite is the cold-tier gate: a sandbox survives a Stop (snapshot to
// disk + free RAM) / Start (restore) round-trip with both its guest RAM and its
// rootfs intact, and exec works on the restored guest. The SAME assertions are
// meant to pass on FC and krucible.
//
// What it proves:
//   - exec-after-restore works (guest vCPU/devices resumed; rootfs survived);
//   - a marker written to tmpfs (guest RAM) before Stop is intact after Start
//     (the memory image round-tripped, not just a fresh reboot).
func RunSnapshotSuite(t *testing.T, newEngine NewEngine) {
	eng := newEngine(t) // may t.Skip
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
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

	// A marker in tmpfs lives in guest RAM — it must survive the memory snapshot.
	const marker = "cold-marker-7f3a"
	if err := fe.FileWrite(ctx, id, "/tmp/snap-marker", "0644", int64(len(marker)), strings.NewReader(marker)); err != nil {
		t.Fatalf("FileWrite marker: %v", err)
	}
	if r, err := eng.Exec(ctx, id, []string{"echo", "pre-stop"}); err != nil || !strings.Contains(r.Stdout, "pre-stop") {
		t.Fatalf("pre-stop exec: err=%v out=%q", err, r.Stdout)
	}

	t.Run("Stop", func(t *testing.T) {
		if err := eng.Stop(ctx, id); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if s, err := eng.Status(ctx, id); err != nil || s.Status != "stopped" {
			t.Fatalf("post-Stop status = %q (err %v), want stopped", s.Status, err)
		}
	})

	t.Run("Start", func(t *testing.T) {
		if err := eng.Start(ctx, id); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if s, err := eng.Status(ctx, id); err != nil || s.Status != "running" {
			t.Fatalf("post-Start status = %q (err %v), want running", s.Status, err)
		}
	})

	t.Run("ExecAfterRestore", func(t *testing.T) {
		r, err := eng.Exec(ctx, id, []string{"echo", "post-restore"})
		if err != nil || !strings.Contains(r.Stdout, "post-restore") {
			t.Fatalf("exec-after-restore: err=%v out=%q", err, r.Stdout)
		}
	})

	t.Run("RAMSurvived", func(t *testing.T) {
		var buf bytes.Buffer
		if _, _, err := fe.FileRead(ctx, id, "/tmp/snap-marker", &buf); err != nil {
			t.Fatalf("FileRead marker after restore: %v", err)
		}
		if buf.String() != marker {
			t.Fatalf("tmpfs marker after restore = %q, want %q (guest RAM not restored)", buf.String(), marker)
		}
	})
}
