//go:build krucible

package krucible

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// TestKrucibleSaveImageRoundTrip is the Phase-2 image-capture gate: write a
// marker to a running sandbox's filesystem, SaveImage it, then create a NEW
// sandbox from that saved image and confirm the marker is present — i.e. the
// captured filesystem boots as a reusable image (`image save` + `create
// --image`, the per-create image selection that krucible previously ignored).
func TestKrucibleSaveImageRoundTrip(t *testing.T) {
	eng := newBlockRootEngine(t)
	ke := eng.(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "saveimg", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { eng.Destroy(context.Background(), info.ID) })

	// A marker on the rootfs (the qcow2 disk, not tmpfs) — what SaveImage captures.
	const marker = "saved-state-7c2a"
	if err := ke.FileWrite(ctx, info.ID, "/etc/krmarker", "0644", int64(len(marker)), strings.NewReader(marker)); err != nil {
		t.Fatalf("FileWrite marker: %v", err)
	}

	imgPath := filepath.Join(t.TempDir(), "saved.qcow2")
	if err := ke.SaveImage(ctx, info.ID, imgPath); err != nil {
		t.Fatalf("SaveImage: %v", err)
	}

	// A fresh sandbox booted from the saved image must see the marker.
	info2, err := eng.Create(ctx, engine.SandboxSpec{Name: "fromimg", CPUs: 1, MemoryMB: 512, BaseImage: imgPath})
	if err != nil {
		t.Fatalf("Create from saved image: %v", err)
	}
	t.Cleanup(func() { eng.Destroy(context.Background(), info2.ID) })

	var buf bytes.Buffer
	if _, _, err := ke.FileRead(ctx, info2.ID, "/etc/krmarker", &buf); err != nil {
		t.Fatalf("FileRead from saved image: %v", err)
	}
	if got := strings.TrimSpace(buf.String()); got != marker {
		t.Fatalf("marker from saved image = %q, want %q", got, marker)
	}

	// And the source sandbox keeps running (SaveImage doesn't stop it).
	if r, err := eng.Exec(ctx, info.ID, []string{"echo", "alive"}); err != nil || !strings.Contains(r.Stdout, "alive") {
		t.Fatalf("source sandbox after SaveImage: err=%v out=%q", err, r.Stdout)
	}
}
