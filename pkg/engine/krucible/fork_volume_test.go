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

// TestKrucibleForkWithVolume is the Phase-3 "fork your whole environment" gate:
// forking a running sandbox that has an attached volume must reproduce the
// device set — the fork comes up with the volume re-attached and its data
// present (proving the memory snapshot captured + restored the device set, not
// just RAM+root) — AND the fork's volume is an independent copy (writes in the
// fork don't touch the source's volume).
func TestKrucibleForkWithVolume(t *testing.T) {
	eng := newBlockRootEngine(t)
	ke := eng.(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Second)
	defer cancel()

	wr := func(id, path, content string) {
		t.Helper()
		if err := ke.FileWrite(ctx, id, path, "0644", int64(len(content)), strings.NewReader(content)); err != nil {
			t.Fatalf("FileWrite %s %s: %v", id, path, err)
		}
	}
	rd := func(id, path string) string {
		t.Helper()
		var b bytes.Buffer
		if _, _, err := ke.FileRead(ctx, id, path, &b); err != nil {
			t.Fatalf("FileRead %s %s: %v", id, path, err)
		}
		return strings.TrimSpace(b.String())
	}

	volPath := filepath.Join(t.TempDir(), "vol.ext4")
	if out, err := exec.Command("mke2fs", "-t", "ext4", "-F", "-q", volPath, "64M").CombinedOutput(); err != nil {
		t.Fatalf("mke2fs volume: %v: %s", err, out)
	}
	vol := engine.ResolvedVolume{FilePath: volPath, DriveID: "vol0", Name: "data", Mount: "/data"}

	src, err := eng.Create(ctx, engine.SandboxSpec{Name: "fvsrc", CPUs: 1, MemoryMB: 512, ResolvedVolumes: []engine.ResolvedVolume{vol}})
	if err != nil {
		t.Fatalf("Create source with volume: %v", err)
	}
	t.Cleanup(func() { eng.Destroy(context.Background(), src.ID) })

	// A marker on the volume (disk) + one in tmpfs (RAM).
	wr(src.ID, "/data/vmark", "src-vol")
	wr(src.ID, "/tmp/rammark", "src-ram")
	if _, err := eng.Exec(ctx, src.ID, []string{"sync"}); err != nil {
		t.Fatalf("sync source: %v", err)
	}

	fork, err := ke.Fork(ctx, src.ID, "fvfork")
	if err != nil {
		t.Fatalf("Fork with volume: %v", err)
	}
	t.Cleanup(func() { eng.Destroy(context.Background(), fork.ID) })

	// Device set reproduced: the fork has the volume re-attached with its data.
	if got := rd(fork.ID, "/data/vmark"); got != "src-vol" {
		t.Fatalf("fork /data/vmark = %q, want src-vol (volume device set not restored)", got)
	}
	// And RAM restored (a real memory clone, not a cold boot).
	if got := rd(fork.ID, "/tmp/rammark"); got != "src-ram" {
		t.Fatalf("fork /tmp/rammark = %q, want src-ram (RAM not restored)", got)
	}

	// The fork's volume is independent: writing it must not change the source's.
	wr(fork.ID, "/data/vmark", "fork-vol")
	if got := rd(fork.ID, "/data/vmark"); got != "fork-vol" {
		t.Fatalf("fork volume rewrite: got %q", got)
	}
	if got := rd(src.ID, "/data/vmark"); got != "src-vol" {
		t.Fatalf("source /data/vmark = %q after fork wrote its own copy \u2014 volume not independent", got)
	}
}
