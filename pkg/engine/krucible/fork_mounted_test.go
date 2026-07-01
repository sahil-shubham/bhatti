//go:build krucible

package krucible

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// TestKrucibleForkMountedRefused pins the honest limit: libkrun can't restore a
// virtio-fs device from a memory snapshot, so forking/memory-snapshotting a
// sandbox that has a --mount is REFUSED — cleanly and fast (not a hang, not an
// unrestorable snapshot). The source is undisturbed; a filesystem snapshot is
// the supported path for mounted sandboxes.
func TestKrucibleForkMountedRefused(t *testing.T) {
	eng := newBlockRootEngine(t)
	ke := eng.(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	hostDir := t.TempDir()
	src, err := eng.Create(ctx, engine.SandboxSpec{Name: "fmg", CPUs: 1, MemoryMB: 512, Mounts: []engine.FsMount{{HostPath: hostDir, GuestPath: "/host"}}})
	if err != nil {
		t.Fatalf("create mounted: %v", err)
	}
	t.Cleanup(func() { eng.Destroy(context.Background(), src.ID) })

	start := time.Now()
	if _, err := ke.Fork(ctx, src.ID, "fmgfork"); err == nil {
		t.Fatal("Fork of a --mount sandbox should be refused (virtio-fs not memory-restorable), got success")
	} else if time.Since(start) > 30*time.Second {
		t.Fatalf("Fork of a --mount sandbox took %s — should fail fast, not hang: %v", time.Since(start), err)
	} else {
		t.Logf("refused as expected in %s: %v", time.Since(start), err)
	}

	// The source is undisturbed by the refused fork.
	if r, err := eng.Exec(ctx, src.ID, []string{"echo", "alive"}); err != nil || !strings.Contains(r.Stdout, "alive") {
		t.Fatalf("source not alive after refused fork: %v (%q)", err, r.Stdout)
	}
}
