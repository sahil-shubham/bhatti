//go:build krucible

package krucible

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
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

// TestKrucibleMountGuestUserWrites: on a root daemon, the guest's own user
// (uid 1000) creates a file in a read-write mount, and on the host it is that
// uid's — as when the helper itself was root. libkrun's file server creates
// each file as the guest caller, so the confined helper keeps the capabilities
// that takes when it has a mount. (A daemon that isn't root runs the helper as
// itself, and only guest ids matching its own can create.)
func TestKrucibleMountGuestUserWrites(t *testing.T) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("needs a root daemon on Linux")
	}
	eng := newBlockRootEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	hostDir := t.TempDir()
	if err := os.Chmod(hostDir, 0o777); err != nil { // the guest user may create in it
		t.Fatal(err)
	}
	info, err := eng.Create(ctx, engine.SandboxSpec{
		Name: "mntuser", CPUs: 1, MemoryMB: 512,
		Mounts: []engine.FsMount{{HostPath: hostDir, GuestPath: "/host"}},
	})
	if err != nil {
		t.Fatalf("Create with --mount: %v", err)
	}
	t.Cleanup(func() { eng.Destroy(context.Background(), info.ID) })

	if r, err := eng.Exec(ctx, info.ID, []string{"writeuid", "/host/byuser"}); err != nil || r.ExitCode != 0 {
		t.Fatalf("exec writeuid: err=%v exit=%d", err, r.ExitCode)
	}
	path := filepath.Join(hostDir, "byuser")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the guest user's file isn't on the host: %v", err)
	}
	if got := string(data); got != "1000" {
		t.Fatalf("written by guest uid %q, want 1000", got)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if uid := fi.Sys().(*syscall.Stat_t).Uid; uid != 1000 {
		t.Fatalf("host file owned by uid %d, want the guest user's 1000", uid)
	}
}
