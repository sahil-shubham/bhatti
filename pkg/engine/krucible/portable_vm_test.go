//go:build krucible

package krucible

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/forward"
)

// secondHost is another engine on its own data dir and sockets, as on another
// node, whose copy of base lives under another path.
func secondHost(t *testing.T, a *Engine, base string) *Engine {
	t.Helper()
	data, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	copyB := filepath.Join(t.TempDir(), "images", "rootfs-copy.ext4")
	writeFile(t, copyB, data)
	b, err := New(Config{
		DataDir: t.TempDir(), BaseImage: copyB, BlockRoot: true,
		VMMBinary: a.cfg.VMMBinary, LibDir: a.cfg.LibDir, KernelImage: a.cfg.KernelImage,
		NetdBinary: a.cfg.NetdBinary, SocketDir: shortSockDir(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// guestServes checks the netcheck server started as pid still runs in sandbox
// id and answers through a forwarded port.
func guestServes(t *testing.T, e *Engine, ctx context.Context, id string, pid int) {
	t.Helper()
	var cmdline bytes.Buffer
	if _, _, err := e.FileRead(ctx, id, "/proc/"+strconv.Itoa(pid)+"/cmdline", &cmdline); err != nil || !strings.Contains(cmdline.String(), "netcheck") {
		t.Fatalf("process %d: %q, %v", pid, cmdline.String(), err)
	}
	ln, err := forward.Serve(e, id, guestPort, "127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	httpGetRetry(t, "http://"+ln.Addr().String()+"/", "hello-from-guest", 15*time.Second)
}

const guestPort = 18081

// TestKrucibleSnapshotMovesToAnotherEngine: a running process (an HTTP server,
// same PID, still answering), a tmpfs file and a root-disk file survive an export, an import
// into an engine on another data dir that has the base image only under
// another path, and a resume there; the disk file is still there after that
// engine cold-boots the sandbox from its disk.
func TestKrucibleSnapshotMovesToAnotherEngine(t *testing.T) {
	a := newCheckpointEngine(t).(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	src, err := a.Create(ctx, engine.SandboxSpec{Name: "export-src", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Destroy(context.Background(), src.ID) })
	pid, _, err := a.ExecDetached(ctx, src.ID, []string{"/bin/netcheck", "serve", strconv.Itoa(guestPort)}, "/tmp/serve.log")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []struct{ path, data string }{{"/tmp/in-ram", "tmpfs-only"}, {"/workspace/on-disk", "root-disk"}} {
		if err := a.FileWrite(ctx, src.ID, f.path, "0644", int64(len(f.data)), strings.NewReader(f.data)); err != nil {
			t.Fatal(err)
		}
	}
	guestServes(t, a, ctx, src.ID, pid)
	parent := t.TempDir()
	m, err := a.Checkpoint(ctx, src.ID, "", 0, "moved", parent)
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := a.ExportSnapshot(ctx, &archive, filepath.Join(parent, "moved"), checkpointJSON(t, m), engine.SnapshotExport{Name: "moved"}); err != nil {
		t.Fatalf("export: %v", err)
	}
	_ = a.Destroy(ctx, src.ID)
	os.RemoveAll(parent)

	b := secondHost(t, a, filepath.Join(a.cfg.DataDir, "base.img"))
	dest := filepath.Join(b.cfg.DataDir, "snapshots", "u", "moved")
	os.MkdirAll(filepath.Dir(dest), 0o700)
	imp, err := b.ImportSnapshot(ctx, bytes.NewReader(archive.Bytes()), dest)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if got, _ := qcow2Backing(filepath.Join(dest, "rootfs.qcow2")); got != b.cfg.BaseImage {
		t.Fatalf("imported root disk backs onto %q, want %s", got, b.cfg.BaseImage)
	}
	restored, err := b.ResumeFromManifestJSON(ctx, dest, imp.ManifestJSON, "moved", "")
	if err != nil {
		t.Fatalf("resume on the second engine: %v", err)
	}
	t.Cleanup(func() { _ = b.Destroy(context.Background(), restored.ID) })
	guestServes(t, b, ctx, restored.ID, pid)
	checkpointMarker(t, b, ctx, restored.ID, "/tmp/in-ram", "tmpfs-only")
	checkpointMarker(t, b, ctx, restored.ID, "/workspace/on-disk", "root-disk")

	if err := b.Stop(ctx, restored.ID); err != nil {
		t.Fatal(err)
	}
	if err := b.Start(ctx, restored.ID); err != nil {
		t.Fatalf("cold boot from the imported disk: %v", err)
	}
	checkpointMarker(t, b, ctx, restored.ID, "/workspace/on-disk", "root-disk")
}

// TestKrucibleSnapshotImportRefusesAnotherCPU: libkrun's host check, run by
// the import before the RAM image arrives, refuses a checkpoint recorded on
// another CPU vendor or with a feature this CPU lacks.
func TestKrucibleSnapshotImportRefusesAnotherCPU(t *testing.T) {
	a := newCheckpointEngine(t).(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	src, err := a.Create(ctx, engine.SandboxSpec{Name: "cpu-src", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Destroy(context.Background(), src.ID) })
	parent := t.TempDir()
	m, err := a.Checkpoint(ctx, src.ID, "", 0, "cpu", parent)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(parent, "cpu")
	// The host record follows the 20-byte header as a length-prefixed section:
	// the 12-byte vendor first, then family, model, stepping, page size, TSC
	// kHz, KVM caps, and the CPUID words (count, then leaf/subleaf/reg/bits).
	const record = 20 + 4
	cases := []struct {
		name, want string
		at         int64
		value      []byte
	}{
		{"vendor", "CPU vendor differs", record, []byte("AuthenticAMD")},
		// Leaf 1 ECX is the first word: claim every feature.
		{"feature", "lacks features the guest uses", record + 12 + 6*4 + 4 + 12, []byte{0xff, 0xff, 0xff, 0xff}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			copyDir := filepath.Join(t.TempDir(), "cpu")
			os.MkdirAll(copyDir, 0o700)
			for _, f := range []string{checkpointFile, memoryFile, "rootfs.qcow2"} {
				if err := cloneFile(filepath.Join(dir, f), filepath.Join(copyDir, f)); err != nil {
					t.Fatal(err)
				}
			}
			ck, err := os.OpenFile(filepath.Join(copyDir, checkpointFile), os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			var leaf [4]byte
			ck.ReadAt(leaf[:], tc.at-12)
			if tc.name == "feature" && binary.LittleEndian.Uint32(leaf[:]) != 1 {
				t.Fatalf("expected leaf 1 first, found %d", binary.LittleEndian.Uint32(leaf[:]))
			}
			ck.WriteAt(tc.value, tc.at)
			ck.Close()
			var archive bytes.Buffer
			if err := a.ExportSnapshot(ctx, &archive, copyDir, checkpointJSON(t, m), engine.SnapshotExport{Name: "cpu"}); err != nil {
				t.Fatal(err)
			}
			dest := filepath.Join(t.TempDir(), "imported")
			_, err = a.ImportSnapshot(ctx, &archive, dest)
			if !errors.Is(err, engine.ErrSnapshotIncompatible) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("import: %v; want %q", err, tc.want)
			}
			t.Log(err)
			if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("refused import left %s", dest)
			}
		})
	}
}
