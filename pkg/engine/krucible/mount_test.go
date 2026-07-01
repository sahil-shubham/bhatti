//go:build krucible

package krucible

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// TestKrucibleMount is the Phase-3 gate for `create --mount`: a live virtio-fs
// bind of a host directory into the guest, shared + bidirectional.
//   - host → guest: a file created on the host before boot is visible in the guest;
//   - guest → host: a file the guest writes appears on the host live.
func TestKrucibleMount(t *testing.T) {
	eng := newBlockRootEngine(t)
	ke := eng.(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	hostDir := t.TempDir()
	const hostMark = "from-host-7f3"
	if err := os.WriteFile(filepath.Join(hostDir, "marker"), []byte(hostMark), 0644); err != nil {
		t.Fatalf("seed host marker: %v", err)
	}

	info, err := eng.Create(ctx, engine.SandboxSpec{
		Name: "mnt", CPUs: 1, MemoryMB: 512,
		Mounts: []engine.FsMount{{HostPath: hostDir, GuestPath: "/host", ReadOnly: false}},
	})
	if err != nil {
		t.Fatalf("Create with --mount: %v", err)
	}
	t.Cleanup(func() { eng.Destroy(context.Background(), info.ID) })

	// host → guest: the guest sees the file the host put there before boot.
	var buf bytes.Buffer
	if _, _, err := ke.FileRead(ctx, info.ID, "/host/marker", &buf); err != nil {
		t.Fatalf("guest FileRead /host/marker (virtio-fs not mounted?): %v", err)
	}
	if got := strings.TrimSpace(buf.String()); got != hostMark {
		t.Fatalf("guest sees /host/marker = %q, want %q", got, hostMark)
	}

	// guest → host: a guest write appears on the host directory, live.
	const guestMark = "from-guest-a1b"
	if err := ke.FileWrite(ctx, info.ID, "/host/fromguest", "0644", int64(len(guestMark)), strings.NewReader(guestMark)); err != nil {
		t.Fatalf("guest FileWrite into mount: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(hostDir, "fromguest"))
	if err != nil {
		t.Fatalf("host cannot read guest-written file (bind not bidirectional?): %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != guestMark {
		t.Fatalf("host sees /host/fromguest = %q, want %q", got, guestMark)
	}
}
