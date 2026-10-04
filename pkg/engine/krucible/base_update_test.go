//go:build krucible

package krucible

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// TestKrucibleBaseUpdateKeepsSandboxes is the update that used to destroy
// every sandbox on a host. A sandbox made on the tier image as an older
// install laid it out (a plain file every root names by its tier path)
// writes 40 MB and stops; the tier is then updated to a rebuild of the same
// tree (a fresh mke2fs -d, as a release build makes it) the way the installer
// does it, and the daemon restarts. The old sandbox must boot with its data
// intact and a filesystem e2fsck finds clean, and a new sandbox must get the
// new image. Overwriting the tier file in place instead (what install.sh
// used to do) fails all of it.
func TestKrucibleBaseUpdateKeepsSandboxes(t *testing.T) {
	for _, tool := range []string{"qemu-img", "e2fsck", "mke2fs"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	if !hasLibkrun() || !hasHypervisor() {
		t.Skip("needs libkrun and a hypervisor")
	}
	repo := repoRoot(t)
	vmm := filepath.Join(repo, "bhatti-vmm")
	if _, err := os.Stat(vmm); err != nil {
		t.Skip("bhatti-vmm not built — run `make vmm`; skipping")
	}
	dataDir := t.TempDir()
	sockDir := shortSockDir(t)
	tree := buildBaseRootfs(t, repo)
	tier := filepath.Join(dataDir, "images", "rootfs-test-"+runtime.GOARCH+".ext4")
	if err := os.MkdirAll(filepath.Dir(tier), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := buildBaseImage(tree, tier); err != nil {
		t.Fatal(err)
	}
	newEngine := func() *Engine {
		eng, err := New(Config{
			DataDir: dataDir, SocketDir: sockDir, BaseImage: tier, VMMBinary: vmm,
			LibDir: libDir(), BlockRoot: true, KernelImage: requireLeanKernel(t, repo), NetdBinary: requireNetd(t, repo),
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return eng
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	eng1 := newEngine()
	info, err := eng1.Create(ctx, engine.SandboxSpec{Name: "old", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := info.ID
	payload := make([]byte, 40<<20)
	rand.Read(payload)
	if err := eng1.FileWrite(ctx, id, "/workspace/payload.bin", "0644", int64(len(payload)), bytes.NewReader(payload)); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	want := md5.Sum(payload)
	if err := eng1.Stop(ctx, id); err != nil {
		t.Fatalf("stop: %v", err)
	}
	eng1.Shutdown()
	root := filepath.Join(dataDir, "sandboxes", id, "root.qcow2")
	if got := backingOf(t, root); got != tier {
		t.Fatalf("pre-migration root names %q, want the tier path %s (the layout being upgraded)", got, tier)
	}

	// The release's image: the same tree, rebuilt, plus a marker to tell it by.
	if err := os.WriteFile(filepath.Join(tree, "etc", "base-marker"), []byte("rebuilt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rebuilt := filepath.Join(dataDir, "rebuilt.ext4")
	if err := buildBaseImage(tree, rebuilt); err != nil {
		t.Fatal(err)
	}
	newBase := installTierImage(t, dataDir, tier, rebuilt)

	eng2 := newEngine() // the daemon restarting after the update
	t.Cleanup(func() {
		eng2.Destroy(context.Background(), id)
		eng2.Shutdown()
	})
	if err := eng2.Start(ctx, id); err != nil {
		t.Fatalf("the old sandbox no longer boots after the update: %v", err)
	}
	h := md5.New()
	if _, _, err := eng2.FileRead(ctx, id, "/workspace/payload.bin", h); err != nil {
		t.Fatalf("read payload after the update: %v", err)
	}
	if got := h.Sum(nil); !bytes.Equal(got, want[:]) {
		t.Fatalf("payload md5 %s after the update, want %s", hex.EncodeToString(got), hex.EncodeToString(want[:]))
	}
	if err := eng2.Stop(ctx, id); err != nil {
		t.Fatalf("stop: %v", err)
	}
	fsckClean(t, root)

	fresh, err := eng2.Create(ctx, engine.SandboxSpec{Name: "new", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("create after the update: %v", err)
	}
	t.Cleanup(func() { eng2.Destroy(context.Background(), fresh.ID) })
	if got := backingOf(t, filepath.Join(dataDir, "sandboxes", fresh.ID, "root.qcow2")); got != newBase {
		t.Fatalf("a new sandbox backs onto %q, want the new base %s", got, newBase)
	}
	var marker bytes.Buffer
	if _, _, err := eng2.FileRead(ctx, fresh.ID, "/etc/base-marker", &marker); err != nil || marker.String() != "rebuilt\n" {
		t.Fatalf("a new sandbox doesn't see the new image: %q, %v", marker.String(), err)
	}
}

// installTierImage installs img as the tier's image the way install.sh's
// install_rootfs does: migrate the data dir (`bhatti admin migrate-images`),
// publish the image under bases/ by content, then re-point the tier name at
// it with one rename. It returns the new base.
func installTierImage(t *testing.T, dataDir, tier, img string) string {
	t.Helper()
	if m, err := MigrateImages(dataDir); err != nil {
		t.Fatalf("migrate: %v (%+v)", err, m)
	}
	sum, _, err := hashFile(img)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(basesDir(dataDir), baseName(strings.TrimSuffix(filepath.Base(tier), ".ext4"), sum))
	if err := os.Rename(img, base); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(tier), "."+filepath.Base(tier)+".new")
	if err := os.Symlink(filepath.Join("bases", filepath.Base(base)), link); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(link, tier); err != nil {
		t.Fatal(err)
	}
	return base
}

// fsckClean fails t unless e2fsck finds the filesystem on the qcow2 disk
// clean (read-only check of a raw copy).
func fsckClean(t *testing.T, disk string) {
	t.Helper()
	raw := filepath.Join(t.TempDir(), "disk.raw")
	if out, err := exec.Command("qemu-img", "convert", "-f", "qcow2", "-O", "raw", disk, raw).CombinedOutput(); err != nil {
		t.Fatalf("qemu-img convert: %v\n%s", err, out)
	}
	out, err := exec.Command("e2fsck", "-fn", raw).CombinedOutput()
	if err != nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		t.Fatalf("e2fsck finds the old sandbox's filesystem damaged (%v, %d lines):\n%s", err, len(lines), strings.Join(lines[:min(len(lines), 30)], "\n"))
	}
}
