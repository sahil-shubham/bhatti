package krucible

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// nodeFixture is an engine on a data dir of its own with a raw base image, and
// helpers to make what the node store holds: sandboxes' root disks, snapshots
// of them, and resumes of those.
type nodeFixture struct {
	t    *testing.T
	e    *Engine
	data string
	base string
	size int64
}

func newNodeFixture(t *testing.T) *nodeFixture {
	t.Helper()
	data := t.TempDir()
	base := filepath.Join(data, "images", "bases", "rootfs-test-0123456789abcdef.ext4")
	b := writeBase(t, base, 1)
	return &nodeFixture{t: t, e: &Engine{cfg: Config{DataDir: data, BaseImage: base}}, data: data, base: base, size: int64(len(b))}
}

// freshRoot is the root disk of a sandbox booted from the base.
func (f *nodeFixture) freshRoot(id string) string {
	f.t.Helper()
	root := filepath.Join(f.data, "sandboxes", id, "root.qcow2")
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := createQcow2Overlay(root, f.base, uint64(f.size)); err != nil {
		f.t.Fatal(err)
	}
	spec := VMSpec{RootDisk: root, RootDiskFormat: "qcow2"}
	if err := f.e.recordDiskBacking(&spec); err != nil {
		f.t.Fatal(err)
	}
	writeRecord(vmRecord{ID: id, UserID: "u1", SandboxDir: filepath.Dir(root), BaseSpec: spec})
	return root
}

// write puts data where a guest write to the disk would, in the file only: the
// tests below read headers, not clusters.
func (f *nodeFixture) write(disk string, off int64, data []byte) {
	f.t.Helper()
	w, err := os.OpenFile(disk, os.O_WRONLY, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.WriteAt(data, off); err != nil {
		f.t.Fatal(err)
	}
}

// snapshot is what a checkpoint of the sandbox whose root disk is root leaves
// in the snapshot directory user/name: a copy of that overlay, whose header
// names whatever the root's does.
func (f *nodeFixture) snapshot(user, name, root string) string {
	f.t.Helper()
	dir := filepath.Join(f.data, "snapshots", user, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := cloneFile(root, filepath.Join(dir, "rootfs.qcow2")); err != nil {
		f.t.Fatal(err)
	}
	spec := VMSpec{RootDisk: root, RootDiskFormat: "qcow2"}
	if err := f.e.recordDiskBacking(&spec); err != nil {
		f.t.Fatal(err)
	}
	if err := writeSnapshotManifest(dir, krucibleSnapManifest{
		DiskFile: "rootfs.qcow2", DiskFormat: "qcow2", Backing: spec.Backing,
	}); err != nil {
		f.t.Fatal(err)
	}
	return dir
}

// resume is what a resume makes of the snapshot: a sandbox whose root is an
// overlay over its disk.
func (f *nodeFixture) resume(id, snapDir string) string {
	f.t.Helper()
	dir := filepath.Join(f.data, "sandboxes", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	root, format, _, err := f.e.prepareNodeRoot(dir, filepath.Join(snapDir, "rootfs.qcow2"), false, "qcow2")
	if err != nil {
		f.t.Fatal(err)
	}
	spec := VMSpec{RootDisk: root, RootDiskFormat: format}
	if err := f.e.recordDiskBacking(&spec); err != nil {
		f.t.Fatal(err)
	}
	writeRecord(vmRecord{ID: id, UserID: filepath.Base(filepath.Dir(snapDir)), SandboxDir: dir, BaseSpec: spec})
	return root
}

func allocated(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Blocks * 512
}

func nodeEntries(t *testing.T, data string) []string {
	t.Helper()
	entries, err := os.ReadDir(nodesDir(data))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, de := range entries {
		if strings.HasPrefix(de.Name(), ".") && strings.HasSuffix(de.Name(), ".img.json") {
			continue
		}
		names = append(names, de.Name())
	}
	return names
}

// backingFormatOf reads the backing-format header extension of an overlay.
func backingFormatOf(t *testing.T, path string) string {
	t.Helper()
	h, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	be := binary.BigEndian
	for off := int(be.Uint32(h[100:])); ; {
		typ, n := be.Uint32(h[off:]), int(be.Uint32(h[off+4:]))
		if typ == 0 {
			t.Fatalf("%s has no backing format extension", path)
		}
		if typ == qcow2ExtBackingFmt {
			return string(h[off+8 : off+8+n])
		}
		off += 8 + (n+7)&^7
	}
}

// TestResumeLayersOverTheSnapshotDisk: a resumed sandbox's root is an overlay
// whose backing file is the snapshot's disk (through the node store, which is
// another name for the same file), the size of its disk, and nothing like the
// size of what the snapshot holds. Two resumes share the one node.
func TestResumeLayersOverTheSnapshotDisk(t *testing.T) {
	f := newNodeFixture(t)
	src := f.freshRoot("src")
	// A delta the size of a large build: written far into the sparse file.
	big := bytes.Repeat([]byte("delta"), 1<<20)
	if err := os.Truncate(src, 512<<20); err != nil {
		t.Fatal(err)
	}
	f.write(src, 256<<20, big)
	snap := f.snapshot("u1", "golden", src)
	disk := filepath.Join(snap, "rootfs.qcow2")
	snapBefore, err := os.ReadFile(disk)
	if err != nil {
		t.Fatal(err)
	}

	a, b := f.resume("a", snap), f.resume("b", snap)

	diskInfo, _ := os.Stat(disk)
	for _, root := range []string{a, b} {
		backing := backingOf(t, root)
		if filepath.Dir(backing) != nodesDir(f.data) {
			t.Fatalf("%s is layered over %s, not a node of the store", root, backing)
		}
		if fi, err := os.Stat(backing); err != nil || !os.SameFile(fi, diskInfo) {
			t.Fatalf("node %s is not the snapshot's own file (%v)", backing, err)
		}
		if got := backingFormatOf(t, root); got != "qcow2" {
			t.Fatalf("%s names its backing file as %q", root, got)
		}
		want, _ := qcow2VirtualSize(disk)
		if got, _ := qcow2VirtualSize(root); got != want {
			t.Fatalf("virtual size %d, the snapshot's disk is %d", got, want)
		}
		if n := allocated(t, root); n > 1<<20 {
			t.Fatalf("%s takes %d bytes on disk: a resume copied the snapshot's delta", root, n)
		}
	}
	if backingOf(t, a) != backingOf(t, b) {
		t.Fatalf("two resumes of one snapshot are layered over different files")
	}
	if got := nodeEntries(t, f.data); len(got) != 1 {
		t.Fatalf("node store holds %v, want the one node", got)
	}
	// Nothing wrote the snapshot's disk.
	if after, _ := os.ReadFile(disk); !bytes.Equal(after, snapBefore) {
		t.Fatal("the snapshot's disk changed")
	}
	if d, err := chainDepth(a); err != nil || d != 2 {
		t.Fatalf("depth of a resumed root = %d, %v; want 2 (the overlay and the snapshot's disk)", d, err)
	}
}

// TestChainDepthIsCappedByCopying: each resume of a snapshot of a resumed
// sandbox adds a layer, up to maxChainDepth; the next copies the snapshot's
// own layer instead, so no disk goes past the cap, and nothing is refused.
func TestChainDepthIsCappedByCopying(t *testing.T) {
	f := newNodeFixture(t)
	root := f.freshRoot("gen0")
	depths := []int{}
	var last string
	for gen := 1; gen <= maxChainDepth+2; gen++ {
		snap := f.snapshot("u1", "gen"+string(rune('a'+gen)), root)
		root = f.resume("gen"+string(rune('a'+gen))+"-sb", snap)
		d, err := chainDepth(root)
		if err != nil {
			t.Fatal(err)
		}
		depths = append(depths, d)
		last = snap
	}
	want := []int{2, 3, 4, 5, 6, 7, 8, 8, 8, 8}
	for i := range want {
		if depths[i] != want[i] {
			t.Fatalf("depth by generation = %v, want %v", depths, want)
		}
	}
	// Past the cap the new disk is a copy: it names the snapshot's own backing,
	// not the snapshot's disk, so it doesn't depend on that snapshot at all.
	disk := filepath.Join(last, "rootfs.qcow2")
	if backingOf(t, root) != backingOf(t, disk) {
		t.Fatalf("a disk at the cap is layered over %s, want a copy of the snapshot's layer (over %s)", backingOf(t, root), backingOf(t, disk))
	}
	users, err := usersOf(f.data, last)
	if err != nil {
		t.Fatal(err)
	}
	if users.any() {
		t.Fatalf("a copy at the cap still counts as layered over the snapshot: %v", users)
	}
	// The policy and the scan read the whole chain: the deepest disk is in
	// bounds, and one more layer is not.
	chain, err := backingChain(root)
	if err != nil || len(chain) != maxChainDepth {
		t.Fatalf("chain of the deepest disk: %d files, %v", len(chain), err)
	}
	over := filepath.Join(f.data, "over.qcow2")
	node, err := f.e.nodeLink(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := createQcow2OverlayOn(over, node, "qcow2", uint64(f.size)); err != nil {
		t.Fatal(err)
	}
	if _, err := backingChain(over); err == nil || !strings.Contains(err.Error(), "deeper than") {
		t.Fatalf("a chain one layer past the cap: %v", err)
	}
}

// TestRemoveSnapshotRefusedWhileLayeredOver: a snapshot a disk is layered over
// can't be deleted, and says how many sandboxes depend on it; once they are
// destroyed it can. The answer comes from the disks on the file system, so a
// daemon that restarted in between gives it too.
func TestRemoveSnapshotRefusedWhileLayeredOver(t *testing.T) {
	f := newNodeFixture(t)
	snap := f.snapshot("u1", "golden", f.freshRoot("src"))
	f.resume("a", snap)
	f.resume("b", snap)
	ctx := context.Background()

	err := f.e.RemoveSnapshot(ctx, snap)
	if !errors.Is(err, engine.ErrSnapshotInUse) || !strings.Contains(err.Error(), "2 sandboxes") {
		t.Fatalf("remove with two clones: %v", err)
	}
	if _, serr := os.Stat(snap); serr != nil {
		t.Fatalf("a refused removal took the snapshot: %v", serr)
	}

	// A daemon with nothing in memory refuses the same.
	restarted := &Engine{cfg: f.e.cfg}
	if err := restarted.RemoveSnapshot(ctx, snap); !errors.Is(err, engine.ErrSnapshotInUse) {
		t.Fatalf("remove after a restart: %v", err)
	}

	os.RemoveAll(filepath.Join(f.data, "sandboxes", "a"))
	if err := restarted.RemoveSnapshot(ctx, snap); !errors.Is(err, engine.ErrSnapshotInUse) || !strings.Contains(err.Error(), "1 sandbox,") {
		t.Fatalf("remove with one clone left: %v", err)
	}
	os.RemoveAll(filepath.Join(f.data, "sandboxes", "b"))
	if err := restarted.RemoveSnapshot(ctx, snap); err != nil {
		t.Fatalf("remove with the clones destroyed: %v", err)
	}
	if _, err := os.Stat(snap); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the snapshot is still there (%v)", err)
	}
	if got := nodeEntries(t, f.data); len(got) != 0 {
		t.Fatalf("the node of a deleted snapshot is still in the store: %v", got)
	}
	// Removing what is gone is not an error: a retry after a half-finished
	// delete has nothing left to refuse.
	if err := restarted.RemoveSnapshot(ctx, snap); err != nil {
		t.Fatalf("remove of a snapshot already gone: %v", err)
	}
}

// TestRemoveSnapshotCountsEverythingLayeredOver: the dependents of a snapshot
// are found by what the disks name, so a snapshot taken from a resumed
// sandbox, and an image saved from one, hold their parent as a sandbox does.
func TestRemoveSnapshotCountsEverythingLayeredOver(t *testing.T) {
	f := newNodeFixture(t)
	parent := f.snapshot("u1", "parent", f.freshRoot("src"))
	root := f.resume("child", parent)
	child := f.snapshot("u1", "child", root) // a snapshot of the resumed sandbox
	saved := filepath.Join(f.data, "images", "u1", "saved.ext4")
	if err := os.MkdirAll(filepath.Dir(saved), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := cloneFile(root, saved); err != nil { // what image save leaves
		t.Fatal(err)
	}
	ctx := context.Background()

	err := f.e.RemoveSnapshot(ctx, parent)
	if !errors.Is(err, engine.ErrSnapshotInUse) {
		t.Fatalf("remove: %v", err)
	}
	for _, want := range []string{"1 sandbox", "1 snapshot", "1 saved image"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("%q doesn't say %q", err, want)
		}
	}
	os.RemoveAll(filepath.Join(f.data, "sandboxes", "child"))
	os.Remove(saved)
	if err := f.e.RemoveSnapshot(ctx, parent); !errors.Is(err, engine.ErrSnapshotInUse) {
		t.Fatalf("the child snapshot alone must still hold its parent: %v", err)
	}
	// Children go first: the child can be deleted at any time, and then so can
	// the parent.
	if err := f.e.RemoveSnapshot(ctx, child); err != nil {
		t.Fatalf("remove the child: %v", err)
	}
	if err := f.e.RemoveSnapshot(ctx, parent); err != nil {
		t.Fatalf("remove the parent after the child: %v", err)
	}
	if got := nodeEntries(t, f.data); len(got) != 0 {
		t.Fatalf("nodes left behind: %v", got)
	}
}

// TestDeletingASnapshotCantBreakADiskMadeOverIt: the node store keeps the
// disk's blocks, so even a removal that went around the check (the directory
// deleted by hand) leaves the clones' chains whole.
func TestDeletingASnapshotCantBreakADiskMadeOverIt(t *testing.T) {
	f := newNodeFixture(t)
	snap := f.snapshot("u1", "golden", f.freshRoot("src"))
	root := f.resume("a", snap)
	want, _ := os.ReadFile(filepath.Join(snap, "rootfs.qcow2"))

	os.RemoveAll(snap)
	if _, err := backingChain(root); err != nil {
		t.Fatal(err)
	}
	node := backingOf(t, root)
	if got, err := os.ReadFile(node); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("the node under a clone changed or went with the snapshot's directory (%v)", err)
	}
	// Its only name left is the store's, and a disk still names it.
	removed, err := SweepNodes(f.data, false)
	if err != nil || len(removed) != 0 {
		t.Fatalf("sweep removed %v (%v) a node a disk is layered over", removed, err)
	}
}

// TestSweepNodesFollowsWhatHoldsThem: a node goes when its snapshot is gone
// (nothing holds it under another name) and no disk names it, and the nodes
// only it was layered over go with it, in the one sweep.
func TestSweepNodesFollowsWhatHoldsThem(t *testing.T) {
	f := newNodeFixture(t)
	s1 := f.snapshot("u1", "s1", f.freshRoot("src"))
	r1 := f.resume("r1", s1)
	// A fork: a staging copy of r1's overlay, a disk layered over it, and the
	// staging copy gone.
	stage := f.snapshot("fork", "snap", r1)
	r2 := f.resume("r2", stage)
	os.RemoveAll(stage)
	os.RemoveAll(s1) // the snapshot is deleted; r1 and r2 remain

	if removed, err := SweepNodes(f.data, false); err != nil || len(removed) != 0 {
		t.Fatalf("sweep with disks over both nodes removed %v (%v)", removed, err)
	}
	if got := nodeEntries(t, f.data); len(got) != 2 {
		t.Fatalf("store holds %v, want the two nodes", got)
	}
	os.RemoveAll(filepath.Join(f.data, "sandboxes", "r2"))
	// The second node has nothing over it; the first is held by it still.
	removed, err := SweepNodes(f.data, false)
	if err != nil || len(removed) != 1 {
		t.Fatalf("sweep after the fork's child went removed %v (%v); want its node", removed, err)
	}
	if _, err := backingChain(r1); err != nil {
		t.Fatalf("the first node went with the second: %v", err)
	}
	os.RemoveAll(filepath.Join(f.data, "sandboxes", "r1"))
	if removed, err := SweepNodes(f.data, false); err != nil || len(removed) != 1 {
		t.Fatalf("sweep after the last disk went removed %v (%v)", removed, err)
	}
	if got := nodeEntries(t, f.data); len(got) != 0 {
		t.Fatalf("store holds %v", got)
	}
	_ = r2
}

// The last sandbox is the only managed root of the whole chain: one sweep
// must release every unheld node, not leave its parents for later runs.
func TestSweepNodesCascades(t *testing.T) {
	f := newNodeFixture(t)
	root := f.freshRoot("gen0")
	var snaps []string
	for gen := 1; gen <= 3; gen++ {
		snap := f.snapshot("u1", "g"+string(rune('0'+gen)), root)
		root = f.resume("sb"+string(rune('0'+gen)), snap)
		snaps = append(snaps, snap)
	}
	for _, s := range snaps {
		os.RemoveAll(s)
	}
	os.RemoveAll(filepath.Join(f.data, "sandboxes"))
	removed, err := SweepNodes(f.data, false)
	if err != nil || len(removed) != 3 {
		t.Fatalf("sweep removed %v (%v), want all three", removed, err)
	}
}

// TestSweepNodesKeepsNodesASnapshotHolds: a node whose file is also in a
// snapshot directory is that snapshot's, with or without disks over it.
func TestSweepNodesKeepsNodesASnapshotHolds(t *testing.T) {
	f := newNodeFixture(t)
	snap := f.snapshot("u1", "golden", f.freshRoot("src"))
	root := f.resume("a", snap)
	os.RemoveAll(filepath.Dir(root))
	if removed, err := SweepNodes(f.data, false); err != nil || len(removed) != 0 {
		t.Fatalf("sweep removed %v (%v) the node of a snapshot that still exists", removed, err)
	}
	if removed, err := SweepNodes(f.data, true); err != nil || len(removed) != 0 {
		t.Fatalf("dry run: %v (%v)", removed, err)
	}
	os.RemoveAll(snap)
	removed, err := SweepNodes(f.data, true)
	if err != nil || len(removed) != 1 {
		t.Fatalf("dry run after the snapshot went: %v (%v)", removed, err)
	}
	if got := nodeEntries(t, f.data); len(got) != 1 {
		t.Fatalf("a dry run removed a node: %v", got)
	}
	// GC of images does what the sweep does.
	gone, err := GCImages(f.data, nil, false)
	if err != nil || len(gone) != 1 || filepath.Dir(gone[0]) != nodesDir(f.data) {
		t.Fatalf("GCImages removed %v (%v), want the node", gone, err)
	}
}

// TestSweepNodesStopsAtAnImageItCantRead: an image whose header can't be read
// might name any node, so nothing is removed.
func TestSweepNodesStopsAtAnImageItCantRead(t *testing.T) {
	f := newNodeFixture(t)
	snap := f.snapshot("u1", "golden", f.freshRoot("src"))
	root := f.resume("a", snap)
	os.RemoveAll(filepath.Dir(root))
	os.RemoveAll(snap)
	// Looks like a qcow2 image and is cut short.
	if err := os.WriteFile(filepath.Join(f.data, "sandboxes-damaged.qcow2"), []byte("QFI\xfb"), 0o600); err != nil {
		t.Fatal(err)
	}
	damaged := filepath.Join(f.data, "sandboxes", "x", "root.qcow2")
	if err := os.MkdirAll(filepath.Dir(damaged), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(damaged, []byte("QFI\xfb\x00\x00\x00\x03"), 0o600); err != nil {
		t.Fatal(err)
	}
	if removed, err := SweepNodes(f.data, false); err == nil || len(removed) != 0 {
		t.Fatalf("sweep went on past an unreadable image: removed %v, %v", removed, err)
	}
	if got := nodeEntries(t, f.data); len(got) != 1 {
		t.Fatalf("the node was removed: %v", got)
	}
}

// TestLayerOnCopiesWhereItCannotLayer: an overlay is made only when it pays
// and can be: across file systems (no link) the disk is copied, as a resume
// was before nodes, and a fork's staging copy is moved rather than copied
// again.
func TestLayerOnCopiesWhereItCannotLayer(t *testing.T) {
	f := newNodeFixture(t)
	snap := f.snapshot("u1", "golden", f.freshRoot("src"))
	disk := filepath.Join(snap, "rootfs.qcow2")
	f.write(disk, 5*qcow2ClusterSize, []byte("what the sandbox wrote"))
	want, _ := os.ReadFile(disk)

	linkNode = func(string, string) error { return &os.LinkError{Op: "link", Err: syscall.EXDEV} }
	t.Cleanup(func() { linkNode = os.Link })

	dst := filepath.Join(f.data, "sandboxes", "copy", "root.qcow2")
	os.MkdirAll(filepath.Dir(dst), 0o700)
	if _, err := f.e.layerOn(dst, disk, false, "qcow2"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dst); !bytes.Equal(got, want) {
		t.Fatal("the fallback isn't a copy of the snapshot's disk")
	}
	if fi, _ := os.Stat(disk); fi == nil {
		t.Fatal("the snapshot's disk is gone")
	}

	moved := filepath.Join(f.data, "sandboxes", "moved", "root.qcow2")
	os.MkdirAll(filepath.Dir(moved), 0o700)
	if _, err := f.e.layerOn(moved, disk, true, "qcow2"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(disk); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an owned disk was copied, not moved (%v)", err)
	}
	if got, _ := os.ReadFile(moved); !bytes.Equal(got, want) {
		t.Fatal("the moved disk's contents changed")
	}
	if got := nodeEntries(t, f.data); len(got) != 0 {
		t.Fatalf("a failed link left %v in the store", got)
	}
}

// TestRestoredVolumesAreOverlays: a frozen volume is re-attached as a disk of
// the sandbox's own over it, whether it is attached read-write or read-only,
// and one that isn't a whole number of sectors is copied, since an overlay
// would be a sector larger than the device the guest was saved with.
func TestRestoredVolumesAreOverlays(t *testing.T) {
	f := newNodeFixture(t)
	snap := filepath.Join(f.data, "snapshots", "u1", "withvols")
	vols := map[string][]byte{
		"vol0.img": bytes.Repeat([]byte("v0"), 1<<20),
		"vol1.img": bytes.Repeat([]byte("v1"), 1<<19),
		"vol2.img": bytes.Repeat([]byte{7}, 1<<20+100),
	}
	for name, data := range vols {
		writeFile(t, filepath.Join(snap, name), data)
	}
	sb := filepath.Join(f.data, "sandboxes", "a")
	os.MkdirAll(sb, 0o700)
	spec := VMSpec{}
	err := f.e.layerRestoredVolumes(sb, &spec, []restoreVol{
		{path: filepath.Join(snap, "vol0.img"), format: "raw"},
		{path: filepath.Join(snap, "vol1.img"), format: "raw", readOnly: true},
		{path: filepath.Join(snap, "vol2.img"), format: "raw"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Volumes) != 3 {
		t.Fatalf("volumes %+v", spec.Volumes)
	}
	for i, name := range []string{"vol0.img", "vol1.img"} {
		v := spec.Volumes[i]
		if v.Format != "qcow2" || v.ReadOnly != (i == 1) {
			t.Fatalf("volume %d = %+v", i, v)
		}
		if got := backingFormatOf(t, v.Path); got != "raw" {
			t.Fatalf("volume %d is over its backing file as %q", i, got)
		}
		if got, _ := qcow2VirtualSize(v.Path); got != uint64(len(vols[name])) {
			t.Fatalf("volume %d has virtual size %d, want %d", i, got, len(vols[name]))
		}
		if n := allocated(t, v.Path); n > 1<<20 {
			t.Fatalf("volume %d takes %d bytes: it was copied", i, n)
		}
		if b := backingOf(t, v.Path); filepath.Dir(b) != nodesDir(f.data) {
			t.Fatalf("volume %d names %s", i, b)
		}
	}
	if v := spec.Volumes[2]; v.Format != "raw" {
		t.Fatalf("an odd-sized volume is %+v, want a raw copy", v)
	}
	if got, _ := os.ReadFile(spec.Volumes[2].Path); !bytes.Equal(got, vols["vol2.img"]) {
		t.Fatal("the copied volume's contents changed")
	}
	// Each device owns its complete chain, including the raw base.
	spec.RootDisk, spec.RootDiskFormat = f.freshRoot("a"), "qcow2"
	if err := f.e.recordDiskBacking(&spec); err != nil {
		t.Fatal(err)
	}
	if len(spec.Backing) != 1 || spec.Backing[0] != (diskLayer{Path: f.base, Format: "raw"}) {
		t.Fatalf("root backing = %v, want only its raw base", spec.Backing)
	}
	for i, v := range spec.Volumes {
		if i == 2 {
			if len(v.Backing) != 0 {
				t.Fatalf("raw fallback volume has backing %v", v.Backing)
			}
			continue
		}
		if len(v.Backing) != 1 || v.Backing[0].Format != "raw" || v.Backing[0].Path != backingOf(t, v.Path) {
			t.Fatalf("volume %d backing = %v, want its raw node", i, v.Backing)
		}
	}
}

// TestCreateQcow2OverlayNamesItsBackingFormat: the format of the backing file
// is in the header, so a reader never has to guess it, and a name that
// doesn't fit the header is refused rather than truncated.
func TestCreateQcow2OverlayNamesItsBackingFormat(t *testing.T) {
	dir := t.TempDir()
	for _, format := range []string{"raw", "qcow2"} {
		p := filepath.Join(dir, format+".qcow2")
		if err := createQcow2OverlayOn(p, filepath.Join(dir, "backing"), format, 1<<20); err != nil {
			t.Fatal(err)
		}
		if got := backingFormatOf(t, p); got != format {
			t.Fatalf("header says %q, want %q", got, format)
		}
		if got := backingOf(t, p); got != filepath.Join(dir, "backing") {
			t.Fatalf("backing name %q", got)
		}
		if size, err := qcow2VirtualSize(p); err != nil || size != 1<<20 {
			t.Fatalf("virtual size %d, %v", size, err)
		}
	}
	if err := createQcow2OverlayOn(filepath.Join(dir, "x"), "/b", "vmdk", 1<<20); err == nil {
		t.Fatal("an unknown backing format was written")
	}
	if err := createQcow2OverlayOn(filepath.Join(dir, "y"), "/"+strings.Repeat("d/", 600), "raw", 1<<20); err == nil {
		t.Fatal("a backing name longer than the header allows was written")
	}
}

func TestDiskUsersReads(t *testing.T) {
	for _, c := range []struct {
		u    diskUsers
		want string
	}{
		{diskUsers{Sandboxes: 1}, "1 sandbox"},
		{diskUsers{Sandboxes: 3}, "3 sandboxes"},
		{diskUsers{Sandboxes: 2, Snapshots: 1}, "2 sandboxes and 1 snapshot"},
		{diskUsers{Sandboxes: 2, Snapshots: 2, Images: 1}, "2 sandboxes, 2 snapshots and 1 saved image"},
	} {
		if got := c.u.String(); got != c.want {
			t.Errorf("%+v reads %q, want %q", c.u, got, c.want)
		}
	}
}

// TestConfineLaunchTrustsOnlyTheBackingTheDaemonRecorded: the helper may
// write its root disk's header, so what the header names must be an image of
// this host's or something the daemon recorded when it made the disk. A node
// the daemon recorded is readable by the helper; a header rewritten to name
// anything else is refused, recorded nodes or not.
func TestConfineLaunchTrustsOnlyTheBackingTheDaemonRecorded(t *testing.T) {
	f := newNodeFixture(t)
	snap := f.snapshot("u1", "golden", f.freshRoot("src"))
	root := f.resume("a", snap)
	node := backingOf(t, root)
	vm := &VM{ID: "a", SandboxDir: filepath.Dir(root), SockDir: filepath.Join(f.data, "sock")}
	launch := func(spec VMSpec) (string, error) {
		_, policy, cleanup, err := f.e.confineLaunch(vm, spec, filepath.Join(vm.SandboxDir, "vmspec.json"))
		if err != nil {
			return "", err
		}
		defer cleanup()
		data, err := os.ReadFile(policy)
		return string(data), err
	}
	spec := VMSpec{RootDisk: root, RootDiskFormat: "qcow2"}

	if _, err := launch(spec); err == nil || !strings.Contains(err.Error(), node) {
		t.Fatalf("a node nobody recorded was admitted: %v", err)
	}
	spec.Backing = []diskLayer{{Path: node, Format: "qcow2"}, {Path: f.base, Format: "raw"}}
	policy, err := launch(spec)
	if err != nil {
		t.Fatalf("a recorded node was refused: %v", err)
	}
	for _, want := range []string{node, f.base} {
		if !strings.Contains(policy, "path "+"4"+" "+want+"\x00") {
			t.Fatalf("the helper isn't allowed to read %s: %q", want, policy)
		}
	}

	// The helper rewrites its header to name another file.
	secret := filepath.Join(f.data, "elsewhere", "secret.img")
	writeFile(t, secret, bytes.Repeat([]byte("s"), 1<<20))
	if err := setQcow2Backing(root, secret); err != nil {
		t.Fatal(err)
	}
	if _, err := launch(spec); err == nil || !strings.Contains(err.Error(), secret) {
		t.Fatalf("a rewritten header got a file nobody recorded: %v", err)
	}
}

// TestConfineLaunchReadiesAVolumesChain: a volume that is an overlay is held
// to the same rule, and its backing file gets the helper's read access.
func TestConfineLaunchReadiesAVolumesChain(t *testing.T) {
	f := newNodeFixture(t)
	snap := filepath.Join(f.data, "snapshots", "u1", "v")
	writeFile(t, filepath.Join(snap, "vol0.img"), bytes.Repeat([]byte("v"), 1<<20))
	sb := filepath.Join(f.data, "sandboxes", "a")
	os.MkdirAll(sb, 0o700)
	spec := VMSpec{RootDisk: f.freshRoot("a"), RootDiskFormat: "qcow2"}
	if err := f.e.layerRestoredVolumes(sb, &spec, []restoreVol{{path: filepath.Join(snap, "vol0.img"), format: "raw"}}); err != nil {
		t.Fatal(err)
	}
	node := backingOf(t, spec.Volumes[0].Path)
	vm := &VM{ID: "a", SandboxDir: sb, SockDir: filepath.Join(f.data, "sock")}
	_, _, cleanup, err := f.e.confineLaunch(vm, spec, filepath.Join(sb, "vmspec.json"))
	if err == nil || !strings.Contains(err.Error(), node) {
		t.Fatalf("a volume over an unrecorded node was admitted: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if err := f.e.recordDiskBacking(&spec); err != nil {
		t.Fatal(err)
	}
	_, policy, cleanup, err := f.e.confineLaunch(vm, spec, filepath.Join(sb, "vmspec.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	data, _ := os.ReadFile(policy)
	if !strings.Contains(string(data), "path 4 "+node+"\x00") {
		t.Fatalf("the helper can't read the volume's backing file: %q", data)
	}
}

// TestRemoveSnapshotGivesUpWhenItsCallerDoes: a removal waits for the bases
// lock as long as the request that asked does, behind an import or a create
// that holds it, and no longer; what it didn't get to is untouched.
func TestRemoveSnapshotGivesUpWhenItsCallerDoes(t *testing.T) {
	f := newNodeFixture(t)
	snap := f.snapshot("u1", "golden", f.freshRoot("src"))
	release, err := lockBases(f.data, false) // an import, say
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := f.e.RemoveSnapshot(ctx, snap); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("remove behind a holder of the lock: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("the removal waited %v on a request that had given up after 100ms", time.Since(start))
	}
	if _, err := os.Stat(snap); err != nil {
		t.Fatalf("a removal that never got the lock took the snapshot: %v", err)
	}
	release()
	if err := f.e.RemoveSnapshot(context.Background(), snap); err != nil {
		t.Fatalf("remove once the lock is free: %v", err)
	}
}

func TestLayerOnPreservesRelativeBacking(t *testing.T) {
	for _, name := range []string{"copy", "move", "capped-copy", "capped-move"} {
		t.Run(name, func(t *testing.T) {
			f := newNodeFixture(t)
			root := f.freshRoot("src")
			if strings.HasPrefix(name, "capped-") {
				for gen := 1; gen < maxChainDepth; gen++ {
					id := string(rune('a' + gen))
					root = f.resume("gen-"+id, f.snapshot("u1", "gen-"+id, root))
				}
			}
			snap := f.snapshot("u1", "relative", root)
			src := filepath.Join(snap, "rootfs.qcow2")
			backing := backingOf(t, src)
			relative, err := filepath.Rel(filepath.Dir(src), backing)
			if err != nil {
				t.Fatal(err)
			}
			if err := setQcow2Backing(src, relative); err != nil {
				t.Fatal(err)
			}
			f.write(src, 5*qcow2ClusterSize, []byte("only this layer is copied"))
			before, err := os.ReadFile(src)
			if err != nil {
				t.Fatal(err)
			}
			depth, err := chainDepth(src)
			if err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(f.data, "sandboxes", "new", "root.qcow2")
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				t.Fatal(err)
			}
			own := strings.HasSuffix(name, "move")
			if _, err := f.e.layerOn(dst, src, own, "qcow2"); err != nil {
				t.Fatal(err)
			}
			if got := backingOf(t, dst); got != backing {
				t.Fatalf("relocated layer names %q, want its original backing %q", got, backing)
			}
			if got, err := chainDepth(dst); err != nil || got != depth {
				t.Fatalf("relocated depth = %d, %v; want %d", got, err, depth)
			}
			after, err := os.ReadFile(dst)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before[qcow2ClusterSize:], after[qcow2ClusterSize:]) {
				t.Fatal("relocating the header changed the copied layer's data")
			}
			if own {
				if _, err := os.Stat(src); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("owned source was not moved: %v", err)
				}
			} else if got, err := os.ReadFile(src); err != nil || !bytes.Equal(got, before) {
				t.Fatalf("rebasing the new disk changed the frozen source: %v", err)
			}
		})
	}
}

func TestLayerOnRejectsAlreadyOverDepthSource(t *testing.T) {
	f := newNodeFixture(t)
	src := f.freshRoot("src")
	dir := filepath.Join(f.data, "snapshots", "u1", "too-deep")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for gen := range maxChainDepth {
		next := filepath.Join(dir, string(rune('a'+gen))+".qcow2")
		if err := createQcow2OverlayOn(next, src, "qcow2", uint64(f.size)); err != nil {
			t.Fatal(err)
		}
		src = next
	}
	dst := filepath.Join(f.data, "sandboxes", "new", "root.qcow2")
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.layerOn(dst, src, false, "qcow2"); err == nil || !strings.Contains(err.Error(), "deeper than") {
		t.Fatalf("an over-depth source produced another over-depth disk: %v", err)
	}
	if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the rejected source left a destination: %v", err)
	}
}

func TestNodeLinkPublishesNameBeforeReturning(t *testing.T) {
	f := newNodeFixture(t)
	snap := f.snapshot("u1", "golden", f.freshRoot("src"))
	src := filepath.Join(snap, "rootfs.qcow2")
	fi, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	k, ok := keyOf(fi)
	if !ok {
		t.Fatal("no file identity")
	}
	node := filepath.Join(nodesDir(f.data), nodeName(k))
	dst := filepath.Join(f.data, "sandboxes", "new", "root.qcow2")
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		t.Fatal(err)
	}
	oldSync := syncNodeDir
	t.Cleanup(func() { syncNodeDir = oldSync })
	syncs := 0
	syncNodeDir = func(dir string) {
		syncs++
		if dir != nodesDir(f.data) {
			t.Fatalf("synced %s, not the node directory", dir)
		}
		if have, err := os.Stat(node); err != nil || !os.SameFile(fi, have) {
			t.Fatalf("the publication barrier ran before the link existed: %v", err)
		}
		if syncs == 1 {
			if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the overlay was published before its backing name: %v", err)
			}
		}
	}
	if _, err := f.e.layerOn(dst, src, false, "qcow2"); err != nil {
		t.Fatal(err)
	}
	if syncs != 1 {
		t.Fatalf("nodeLink returned without its publication barrier: %d barriers", syncs)
	}
	// A second publisher can observe the link before the first publisher's
	// barrier. Seeing EEXIST is not proof that the name is already durable.
	if got, err := f.e.nodeLink(src); err != nil || got != node || syncs != 2 {
		t.Fatalf("reused link was not published: %s, %v, %d barriers", got, err, syncs)
	}
}

func TestSweepNodesFollowsSymlinkRoots(t *testing.T) {
	for _, kind := range []string{"data-dir", "disk-file", "disk-dir", "node-file", "node-dir", "store-dir"} {
		t.Run(kind, func(t *testing.T) {
			f := newNodeFixture(t)
			snap := f.snapshot("u1", "golden", f.freshRoot("src"))
			root := f.resume("a", snap)
			node := backingOf(t, root)
			if err := os.RemoveAll(snap); err != nil {
				t.Fatal(err)
			}
			data := f.data
			holders := []string{filepath.Dir(root)}
			switch kind {
			case "data-dir":
				data = filepath.Join(t.TempDir(), "data")
				if err := os.Symlink(f.data, data); err != nil {
					t.Fatal(err)
				}
			case "disk-file":
				outside := filepath.Join(t.TempDir(), "root.qcow2")
				if err := os.Rename(root, outside); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, root); err != nil {
					t.Fatal(err)
				}
				holders = append(holders, outside)
			case "disk-dir":
				outside := filepath.Join(t.TempDir(), "sandbox")
				if err := os.Rename(filepath.Dir(root), outside); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Dir(root)); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(outside, "loop")); err != nil {
					t.Fatal(err)
				}
				holders = append(holders, outside)
			case "node-file":
				if err := os.Remove(root); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(node, root); err != nil {
					t.Fatal(err)
				}
			case "node-dir":
				if err := os.Remove(root); err != nil {
					t.Fatal(err)
				}
				alias := filepath.Join(f.data, "images", "linked-nodes")
				if err := os.Symlink(nodesDir(f.data), alias); err != nil {
					t.Fatal(err)
				}
				holders = append(holders, alias)
			case "store-dir":
				outside := filepath.Join(t.TempDir(), "nodes")
				if err := os.Rename(nodesDir(f.data), outside); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, nodesDir(f.data)); err != nil {
					t.Fatal(err)
				}
			}
			if removed, err := SweepNodes(data, false); err != nil || len(removed) != 0 {
				t.Fatalf("swept the node through a live %s root: %v, %v", kind, removed, err)
			}
			if _, err := os.Stat(node); err != nil {
				t.Fatalf("the in-use node name was deleted: %v", err)
			}
			for _, holder := range holders {
				if err := os.RemoveAll(holder); err != nil {
					t.Fatal(err)
				}
			}
			if removed, err := SweepNodes(data, false); err != nil || len(removed) != 1 {
				t.Fatalf("node was not reclaimed after the linked root went: %v, %v", removed, err)
			}
		})
	}
}

func TestSweepNodesFollowsRawNodeSymlink(t *testing.T) {
	f := newNodeFixture(t)
	src := filepath.Join(f.data, "snapshots", "u1", "raw", "vol0.img")
	writeFile(t, src, bytes.Repeat([]byte{1}, 1<<20))
	node, err := f.e.nodeLink(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.e.recordNodeMetadata(node, "raw", []diskLayer{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(f.data, "images", "raw.img")
	if err := os.Symlink(node, alias); err != nil {
		t.Fatal(err)
	}
	if removed, err := SweepNodes(f.data, false); err != nil || len(removed) != 0 {
		t.Fatalf("a raw node still named by an image was removed: %v, %v", removed, err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if removed, err := SweepNodes(f.data, false); err != nil || len(removed) != 1 {
		t.Fatalf("the unlinked raw image's node remained: %v, %v", removed, err)
	}
}

func TestSweepNodesFollowsPersistedExternalDisks(t *testing.T) {
	f := newNodeFixture(t)
	snap := f.snapshot("u1", "with-volumes", f.freshRoot("src"))
	root := f.resume("a", snap)
	outside := t.TempDir()
	externalRoot := filepath.Join(outside, "root.qcow2")
	if err := os.Rename(root, externalRoot); err != nil {
		t.Fatal(err)
	}
	volume := filepath.Join(snap, "vol0.img")
	writeFile(t, volume, bytes.Repeat([]byte("volume"), 1<<18))
	externalVol := filepath.Join(outside, "volume.qcow2")
	if _, err := f.e.layerOn(externalVol, volume, false, "raw"); err != nil {
		t.Fatal(err)
	}
	sb := filepath.Dir(root)
	spec := VMSpec{
		RootDisk: externalRoot, RootDiskFormat: "qcow2",
		Volumes: []VMVolume{{Path: externalVol, Format: "qcow2"}},
	}
	if err := f.e.recordDiskBacking(&spec); err != nil {
		t.Fatal(err)
	}
	writeRecord(vmRecord{ID: "a", UserID: "u1", SandboxDir: sb, Status: "stopped", BaseSpec: spec})
	if _, err := os.Stat(stateFilePath(sb)); err != nil {
		t.Fatal(err)
	}
	restarted := &Engine{cfg: f.e.cfg}
	if err := restarted.RemoveSnapshot(context.Background(), snap); !errors.Is(err, engine.ErrSnapshotInUse) ||
		!strings.Contains(err.Error(), "1 sandbox,") {
		t.Fatalf("external disks of a recovered sandbox were not counted: %v", err)
	}
	if err := os.RemoveAll(snap); err != nil {
		t.Fatal(err)
	}
	if removed, err := SweepNodes(f.data, false); err != nil || len(removed) != 0 {
		t.Fatalf("swept nodes an external disk still names: %v, %v", removed, err)
	}
	for _, disk := range []string{externalRoot, externalVol} {
		if _, err := backingChain(disk); err != nil {
			t.Fatalf("external disk lost its chain: %v", err)
		}
	}
	if err := os.RemoveAll(sb); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(outside); err != nil {
		t.Fatal(err)
	}
	if removed, err := SweepNodes(f.data, false); err != nil || len(removed) != 2 {
		t.Fatalf("external disks' nodes were not reclaimed: %v, %v", removed, err)
	}
}

func TestSweepNodesFollowsPersistedRawNode(t *testing.T) {
	for _, disk := range []string{"root", "volume"} {
		t.Run(disk, func(t *testing.T) {
			f := newNodeFixture(t)
			src := filepath.Join(f.data, "snapshots", "u1", "raw", "vol0.img")
			writeFile(t, src, bytes.Repeat([]byte{7}, 1<<20))
			node, err := f.e.nodeLink(src)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(src); err != nil {
				t.Fatal(err)
			}
			sb := filepath.Join(f.data, "sandboxes", "raw")
			if err := os.MkdirAll(sb, 0o700); err != nil {
				t.Fatal(err)
			}
			spec := VMSpec{RootDisk: node, RootDiskFormat: "raw"}
			if disk == "volume" {
				spec = VMSpec{RootDisk: f.freshRoot("raw"), RootDiskFormat: "qcow2",
					Volumes: []VMVolume{{Path: node, Format: "raw"}}}
			}
			if err := f.e.recordDiskBacking(&spec); err != nil {
				t.Fatal(err)
			}
			writeRecord(vmRecord{ID: "raw", SandboxDir: sb, BaseSpec: spec})
			if _, err := os.Stat(stateFilePath(sb)); err != nil {
				t.Fatal(err)
			}
			if removed, err := SweepNodes(f.data, false); err != nil || len(removed) != 0 {
				t.Fatalf("removed a node directly attached as a raw %s: %v, %v", disk, removed, err)
			}
			if err := os.RemoveAll(sb); err != nil {
				t.Fatal(err)
			}
			if removed, err := SweepNodes(f.data, false); err != nil || len(removed) != 1 {
				t.Fatalf("raw node survived the last persisted root: %v, %v", removed, err)
			}
		})
	}
}

func TestSweepNodesReclaimsUnreachableCyclesAndDamagedNodes(t *testing.T) {
	for _, kind := range []string{"cycle", "damaged"} {
		t.Run(kind, func(t *testing.T) {
			f := newNodeFixture(t)
			aSnap := f.snapshot("u1", "a", f.freshRoot("src"))
			a, err := f.e.nodeLink(filepath.Join(aSnap, "rootfs.qcow2"))
			if err != nil {
				t.Fatal(err)
			}
			wantRemoved := 1
			if kind == "cycle" {
				bSnap := f.snapshot("u1", "b", f.freshRoot("other"))
				b, err := f.e.nodeLink(filepath.Join(bSnap, "rootfs.qcow2"))
				if err != nil {
					t.Fatal(err)
				}
				if err := setQcow2Backing(a, b); err != nil {
					t.Fatal(err)
				}
				if err := setQcow2Backing(b, a); err != nil {
					t.Fatal(err)
				}
				if err := os.RemoveAll(bSnap); err != nil {
					t.Fatal(err)
				}
				wantRemoved++
			} else if err := os.WriteFile(a, []byte("QFI\xfb\x00\x00\x00\x03"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(aSnap); err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(f.data, "sandboxes", "live", "root.qcow2")
			if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := createQcow2OverlayOn(root, a, "qcow2", uint64(f.size)); err != nil {
				t.Fatal(err)
			}
			if removed, err := SweepNodes(f.data, false); err == nil || len(removed) != 0 {
				t.Fatalf("an unreadable live chain was not protected: %v, %v", removed, err)
			}
			if err := os.RemoveAll(filepath.Dir(root)); err != nil {
				t.Fatal(err)
			}
			if removed, err := SweepNodes(f.data, false); err != nil || len(removed) != wantRemoved {
				t.Fatalf("the orphan %s still pins nodes forever: %v, %v", kind, removed, err)
			}
		})
	}
}

func TestSweepNodesTreatsExplicitRawQFIAsData(t *testing.T) {
	f := newNodeFixture(t)
	deadSnap := f.snapshot("u1", "dead", f.freshRoot("src"))
	dead, err := f.e.nodeLink(filepath.Join(deadSnap, "rootfs.qcow2"))
	if err != nil {
		t.Fatal(err)
	}
	rawSrc := filepath.Join(f.data, "snapshots", "u1", "raw", "vol0.img")
	if err := os.MkdirAll(filepath.Dir(rawSrc), 0o700); err != nil {
		t.Fatal(err)
	}
	// A valid qcow2 header in a raw volume is guest data, including this
	// pointer. It must not keep an unrelated node alive.
	if err := createQcow2OverlayOn(rawSrc, dead, "qcow2", uint64(f.size)); err != nil {
		t.Fatal(err)
	}
	rawNode, err := f.e.nodeLink(rawSrc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(deadSnap); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(rawSrc); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(f.data, "sandboxes", "raw", "root.qcow2")
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := createQcow2OverlayOn(root, rawNode, "raw", uint64(f.size)); err != nil {
		t.Fatal(err)
	}
	spec := VMSpec{RootDisk: root, RootDiskFormat: "qcow2"}
	if err := f.e.recordDiskBacking(&spec); err != nil {
		t.Fatal(err)
	}
	writeRecord(vmRecord{ID: "raw", UserID: "u1", SandboxDir: filepath.Dir(root), BaseSpec: spec})
	if depth, err := chainDepth(root); err != nil || depth != 1 {
		t.Fatalf("raw payload was counted as a qcow2 layer: %d, %v", depth, err)
	}
	if removed, err := SweepNodes(f.data, false); err != nil || len(removed) != 1 || removed[0] != dead {
		t.Fatalf("raw data's fake backing kept an orphan alive: %v, %v", removed, err)
	}
	if _, err := os.Stat(rawNode); err != nil {
		t.Fatalf("the explicit-raw node was swept: %v", err)
	}
	if err := os.RemoveAll(filepath.Dir(root)); err != nil {
		t.Fatal(err)
	}
	if removed, err := SweepNodes(f.data, false); err != nil || len(removed) != 1 || removed[0] != rawNode {
		t.Fatalf("raw node was not reclaimed: %v, %v", removed, err)
	}
}

func TestSweepNodesReclaimsCrashedStages(t *testing.T) {
	f := newNodeFixture(t)
	src := f.freshRoot("src")
	stage, err := os.MkdirTemp(f.data, "fork-")
	if err != nil {
		t.Fatal(err)
	}
	snap := filepath.Join(stage, "snap")
	if err := os.Mkdir(snap, 0o700); err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(snap, "rootfs.qcow2")
	if err := cloneFile(src, disk); err != nil {
		t.Fatal(err)
	}
	sb := filepath.Join(f.data, "sandboxes", "child")
	if err := os.MkdirAll(sb, 0o700); err != nil {
		t.Fatal(err)
	}
	root, format, _, err := f.e.prepareNodeRoot(sb, disk, true, "qcow2")
	if err != nil {
		t.Fatal(err)
	}
	spec := VMSpec{RootDisk: root, RootDiskFormat: format}
	if err := f.e.recordDiskBacking(&spec); err != nil {
		t.Fatal(err)
	}
	writeRecord(vmRecord{ID: "child", UserID: "u1", SandboxDir: sb, BaseSpec: spec})
	if err := writeSnapshotManifest(snap, krucibleSnapManifest{DiskFile: "rootfs.qcow2"}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(snap, ".manifest-123"), []byte("interrupted metadata write"))
	adopt, err := os.CreateTemp(nodesDir(f.data), ".adopt-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := adopt.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cloneFile(src, adopt.Name()); err != nil {
		t.Fatal(err)
	}
	adoptedNode, err := f.e.nodeLink(adopt.Name())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.e.recordNodeMetadata(adoptedNode, "qcow2", []diskLayer{{Path: f.base, Format: "raw"}}); err != nil {
		t.Fatal(err)
	}
	untouched := []string{
		filepath.Join(f.data, "fork-manual", "snap", "rootfs.qcow2"),
		filepath.Join(f.data, "fork-123", "notes.txt"),
		filepath.Join(nodesDir(f.data), ".adopt-manual"),
	}
	for _, path := range untouched {
		writeFile(t, path, []byte("not an engine stage"))
	}
	linkedStage := filepath.Join(f.data, "fork-456", "snap")
	if err := os.MkdirAll(linkedStage, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "keep")
	writeFile(t, outside, []byte("outside the stage"))
	if err := os.Symlink(outside, filepath.Join(linkedStage, "rootfs.qcow2")); err != nil {
		t.Fatal(err)
	}
	if _, err := SweepNodes(f.data, true); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{stage, adopt.Name()} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("dry run removed stage %s: %v", path, err)
		}
	}
	removed, err := SweepNodes(f.data, false)
	if err != nil || len(removed) != 1 || removed[0] != adoptedNode {
		t.Fatalf("crashed stages pinned nodes: %v, %v", removed, err)
	}
	for _, path := range []string{stage, adopt.Name()} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("crashed stage %s survived: %v", path, err)
		}
	}
	for _, path := range append(untouched, outside, linkedStage) {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("sweep removed an unrecognized stage shape %s: %v", path, err)
		}
	}
	if _, err := backingChain(root); err != nil {
		t.Fatalf("cleaning a fork's stage broke its child: %v", err)
	}
	if err := os.RemoveAll(sb); err != nil {
		t.Fatal(err)
	}
	if removed, err := SweepNodes(f.data, false); err != nil || len(removed) != 1 {
		t.Fatalf("the fork's last node was not reclaimed: %v, %v", removed, err)
	}
}

func TestReadDiskChainAuthorizesBeforeOpeningBacking(t *testing.T) {
	dir := t.TempDir()
	blocked := filepath.Join(dir, "blocked.img")
	writeFile(t, blocked, []byte("QFI\xfb"))
	top := filepath.Join(dir, "top.qcow2")
	if err := createQcow2OverlayOn(top, blocked, "qcow2", 1<<20); err != nil {
		t.Fatal(err)
	}
	denied := errors.New("backing not owned")
	_, err := readDiskChain(top, "qcow2", func(path string) error {
		if path != blocked {
			t.Fatalf("unexpected backing authorization for %s", path)
		}
		return denied
	})
	if !errors.Is(err, denied) {
		t.Fatalf("unowned backing was opened before authorization: %v", err)
	}
	chain, err := readDiskChain(top, "raw", func(string) error {
		t.Fatal("raw contents requested backing authorization")
		return denied
	})
	if err != nil || len(chain) != 0 {
		t.Fatalf("raw disk acquired a backing chain: %v, %v", chain, err)
	}
}

func TestRecordDiskBackingKeepsRawDisksOpaque(t *testing.T) {
	f := newNodeFixture(t)
	raw := filepath.Join(f.data, "volumes", "u1", "raw.img")
	if err := os.MkdirAll(filepath.Dir(raw), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := createQcow2OverlayOn(raw, filepath.Join(f.data, "missing.img"), "qcow2", 1<<20); err != nil {
		t.Fatal(err)
	}
	spec := VMSpec{RootDisk: raw, RootDiskFormat: "raw",
		Volumes: []VMVolume{{BlockID: "vol0", Path: raw, Format: "raw"}}}
	if err := f.e.recordDiskBacking(&spec); err != nil {
		t.Fatalf("raw guest payload was interpreted: %v", err)
	}
	if len(spec.Backing) != 0 || len(spec.Volumes[0].Backing) != 0 {
		t.Fatalf("raw disks acquired backing: root=%v volume=%v", spec.Backing, spec.Volumes[0].Backing)
	}
	standalone := filepath.Join(f.data, "standalone.qcow2")
	if err := createQcow2OverlayOn(standalone, f.base, "raw", 1<<20); err != nil {
		t.Fatal(err)
	}
	f.write(standalone, 8, make([]byte, 12))
	spec = VMSpec{RootDisk: standalone, RootDiskFormat: "qcow2"}
	if err := f.e.recordDiskBacking(&spec); err != nil {
		t.Fatal(err)
	}
	if spec.Backing == nil || len(spec.Backing) != 0 {
		t.Fatalf("verified standalone disk lost its metadata marker: %v", spec.Backing)
	}
}

func TestRawSnapshotPayloadCannotAddDiskReferences(t *testing.T) {
	for _, device := range []string{"root", "volume"} {
		t.Run(device, func(t *testing.T) {
			f := newNodeFixture(t)
			target := f.snapshot("u2", "private", f.freshRoot("target"))
			dead, err := f.e.nodeLink(filepath.Join(target, "rootfs.qcow2"))
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(f.data, "snapshots", "u1", "raw")
			m := krucibleSnapManifest{DiskFile: "rootfs.qcow2", DiskFormat: "raw"}
			name := m.DiskFile
			if device == "volume" {
				dir = f.snapshot("u1", "raw", f.freshRoot("holder"))
				var found bool
				m, found, err = readSnapshotManifest(dir)
				if err != nil || !found {
					t.Fatalf("read fixture manifest: %v, %v", found, err)
				}
				name = "vol0.img"
				m.Volumes = []snapVolume{{File: name, Format: "raw"}}
			}
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			raw := filepath.Join(dir, name)
			if err := createQcow2OverlayOn(raw, dead, "qcow2", 1<<20); err != nil {
				t.Fatal(err)
			}
			if err := writeSnapshotManifest(dir, m); err != nil {
				t.Fatal(err)
			}
			rawNode, err := f.e.nodeLink(raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.e.recordNodeMetadata(rawNode, "raw", []diskLayer{}); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(f.data, "images", "u1", "raw-alias.ext4")
			if err := os.MkdirAll(filepath.Dir(alias), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(raw, alias); err != nil {
				t.Fatal(err)
			}
			if users, err := usersOf(f.data, target); err != nil || users.any() {
				t.Fatalf("raw %s invented snapshot dependents: %v, %v", device, users, err)
			}
			if err := os.RemoveAll(target); err != nil {
				t.Fatal(err)
			}
			if removed, err := SweepNodes(f.data, false); err != nil || len(removed) != 1 || removed[0] != dead {
				t.Fatalf("raw %s pinned another node: %v, %v", device, removed, err)
			}
			if _, err := os.Stat(rawNode); err != nil {
				t.Fatalf("raw %s lost its own inode: %v", device, err)
			}
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(alias); err != nil {
				t.Fatal(err)
			}
			if removed, err := SweepNodes(f.data, false); err != nil || len(removed) != 1 || removed[0] != rawNode {
				t.Fatalf("raw %s node outlived its last holder: %v, %v", device, removed, err)
			}
		})
	}
}

func TestDiskRefsNeverExpandsTrustedChainsFromHeaders(t *testing.T) {
	f := newNodeFixture(t)
	target := f.snapshot("u2", "private", f.freshRoot("target"))
	dead, err := f.e.nodeLink(filepath.Join(target, "rootfs.qcow2"))
	if err != nil {
		t.Fatal(err)
	}
	root := f.freshRoot("guest")
	trusted := f.snapshot("u1", "trusted", root)
	for _, path := range []string{root, filepath.Join(trusted, "rootfs.qcow2")} {
		if err := createQcow2OverlayOn(path, dead, "qcow2", uint64(f.size)); err != nil {
			t.Fatal(err)
		}
	}
	if users, err := usersOf(f.data, target); err != nil || users.any() {
		t.Fatalf("guest headers expanded daemon-recorded dependencies: %v, %v", users, err)
	}
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}
	if removed, err := SweepNodes(f.data, false); err != nil || len(removed) != 1 || removed[0] != dead {
		t.Fatalf("an unrecorded header pointer pinned a node: %v, %v", removed, err)
	}
}

func TestNodeMetadataPreservesExternalSnapshotAncestors(t *testing.T) {
	f := newNodeFixture(t)
	parent := f.snapshot("u1", "parent", f.freshRoot("source"))
	root := f.resume("child", parent)
	external := filepath.Join(t.TempDir(), "rootfs.qcow2")
	if err := cloneFile(root, external); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(t.TempDir(), "overlay.qcow2")
	if _, err := f.e.layerOn(scratch, external, false, "qcow2"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{parent, filepath.Join(f.data, "sandboxes"), scratch} {
		if err := os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
	}
	nodes := nodeEntries(t, f.data)
	if len(nodes) != 2 {
		t.Fatalf("external lineage holds %v, want two nodes", nodes)
	}
	if removed, err := SweepNodes(f.data, false); err != nil || len(removed) != 0 {
		t.Fatalf("external snapshot lost an ancestor: %v, %v", removed, err)
	}
	if err := os.Remove(external); err != nil {
		t.Fatal(err)
	}
	if removed, err := SweepNodes(f.data, false); err != nil || len(removed) != 2 {
		t.Fatalf("external snapshot's graph survived its last holder: %v, %v", removed, err)
	}
	for _, name := range nodes {
		if _, err := os.Stat(nodeMetadataPath(filepath.Join(nodesDir(f.data), name))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("reclaimed node retained metadata for inode reuse: %s, %v", name, err)
		}
	}
}

func TestOwnedNodeKeysUsesPerDiskMetadataOnly(t *testing.T) {
	f := newNodeFixture(t)
	var nodes []string
	for _, name := range []string{"snapshot-root", "snapshot-volume", "sandbox-root", "sandbox-volume", "foreign"} {
		src := filepath.Join(t.TempDir(), name+".img")
		writeFile(t, src, bytes.Repeat([]byte(name), 4096))
		node, err := f.e.nodeLink(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.e.recordNodeMetadata(node, "raw", []diskLayer{}); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(src); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, node)
	}
	snap := filepath.Join(f.data, "snapshots", "u1", "trusted")
	sb := filepath.Join(f.data, "sandboxes", "owned")
	for _, dir := range []string{snap, sb} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	m := krucibleSnapManifest{DiskFile: "rootfs.qcow2", DiskFormat: "qcow2",
		Backing: []diskLayer{{Path: nodes[0], Format: "raw"}},
		Volumes: []snapVolume{
			{File: "vol0.img", Format: "qcow2", Backing: []diskLayer{{Path: nodes[1], Format: "raw"}}},
			{File: "vol1.img", Format: "raw"},
		}}
	for _, name := range []string{m.DiskFile, "vol0.img", "vol1.img"} {
		if err := createQcow2OverlayOn(filepath.Join(snap, name), nodes[4], "raw", 1<<20); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeSnapshotManifest(snap, m); err != nil {
		t.Fatal(err)
	}
	spec := VMSpec{RootDisk: filepath.Join(sb, "root.qcow2"), RootDiskFormat: "qcow2",
		Backing: []diskLayer{{Path: nodes[2], Format: "raw"}},
		Volumes: []VMVolume{{Path: filepath.Join(sb, "vol0.img"), Format: "qcow2",
			Backing: []diskLayer{{Path: nodes[3], Format: "raw"}}}}}
	for _, path := range []string{spec.RootDisk, spec.Volumes[0].Path} {
		if err := createQcow2OverlayOn(path, nodes[4], "raw", 1<<20); err != nil {
			t.Fatal(err)
		}
	}
	writeRecord(vmRecord{ID: "owned", UserID: "u1", SandboxDir: sb, BaseSpec: spec})
	owned, err := ownedNodeKeys(f.data, "u1")
	if err != nil {
		t.Fatal(err)
	}
	for i, node := range nodes {
		fi, err := os.Stat(node)
		if err != nil {
			t.Fatal(err)
		}
		key, ok := keyOf(fi)
		if !ok || owned[key] != (i < 4) {
			t.Fatalf("node %d ownership = %v, want %v (identity available=%v)", i, owned[key], i < 4, ok)
		}
	}
}

func TestMigrateImagesKeepsRecordedRawPayloadsUnchanged(t *testing.T) {
	f := newNodeFixture(t)
	tier := filepath.Join(f.data, "images", "rootfs-legacy.ext4")
	writeBase(t, tier, 3)
	dir := filepath.Join(f.data, "snapshots", "u1", "raw")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	m := krucibleSnapManifest{DiskFile: "rootfs.qcow2", DiskFormat: "raw",
		Volumes: []snapVolume{{File: "vol0.img", Format: "raw"}}}
	before := map[string][]byte{}
	for _, name := range []string{m.DiskFile, m.Volumes[0].File} {
		path := filepath.Join(dir, name)
		if err := createQcow2OverlayOn(path, tier, "raw", 1<<20); err != nil {
			t.Fatal(err)
		}
		var err error
		before[path], err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := writeSnapshotManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(f.data, "images", "raw-alias.ext4")
	if err := os.Link(filepath.Join(dir, "vol0.img"), alias); err != nil {
		t.Fatal(err)
	}
	migration, err := MigrateImages(f.data)
	if err != nil || len(migration.Rewritten) != 0 {
		t.Fatalf("migration interpreted raw guest data: %+v, %v", migration, err)
	}
	for path, want := range before {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("migration changed raw payload %s: %v", path, err)
		}
	}
}

func TestSweepNodesRejectsUntypedLiveHardlinks(t *testing.T) {
	f := newNodeFixture(t)
	src := filepath.Join(t.TempDir(), "ancestor.img")
	writeFile(t, src, bytes.Repeat([]byte{1}, 1<<20))
	ancestor, err := f.e.nodeLink(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.e.recordNodeMetadata(ancestor, "raw", []diskLayer{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "legacy.qcow2")
	if err := createQcow2OverlayOn(external, ancestor, "raw", 1<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.nodeLink(external); err != nil {
		t.Fatal(err)
	}
	if removed, err := SweepNodes(f.data, false); err == nil || len(removed) != 0 ||
		!strings.Contains(err.Error(), "no daemon-recorded disk metadata") {
		t.Fatalf("untyped external snapshot was guessed or lost an ancestor: %v, %v", removed, err)
	}
	if err := os.Remove(external); err != nil {
		t.Fatal(err)
	}
	if removed, err := SweepNodes(f.data, false); err != nil || len(removed) != 2 {
		t.Fatalf("unreachable untyped graph could not be reclaimed: %v, %v", removed, err)
	}
}

func TestImageMetadataKeepsRawBytesOpaqueAndExpiresWithImage(t *testing.T) {
	f := newNodeFixture(t)
	snap := f.snapshot("u2", "private", f.freshRoot("target"))
	dead, err := f.e.nodeLink(filepath.Join(snap, "rootfs.qcow2"))
	if err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(f.data, "images", "u1", "raw.ext4")
	if err := os.MkdirAll(filepath.Dir(image), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := createQcow2OverlayOn(image, dead, "qcow2", 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := f.e.recordNodeMetadata(image, "raw", []diskLayer{}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(snap); err != nil {
		t.Fatal(err)
	}
	if removed, err := SweepNodes(f.data, false); err != nil || len(removed) != 1 || removed[0] != dead {
		t.Fatalf("saved raw image's payload invented a dependency: %v, %v", removed, err)
	}
	if err := os.Remove(image); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(filepath.Dir(image), ".notes.json")
	writeFile(t, unrelated, []byte("not a disk metadata record"))
	if _, err := SweepNodes(f.data, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(nodeMetadataPath(image)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted image left disk metadata behind: %v", err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("metadata cleanup removed unrelated hidden JSON: %v", err)
	}
}

func TestRecordedRawPayloadIsNeverOpened(t *testing.T) {
	const probeEnv = "BHATTI_TEST_RAW_DISK_LOCK"
	if path := os.Getenv(probeEnv); path != "" {
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		lock := syscall.Flock_t{Type: syscall.F_WRLCK}
		err = syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &lock)
		if errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EAGAIN) {
			return
		}
		t.Fatalf("raw file's parent-process record lock was lost: %v", err)
	}
	f := newNodeFixture(t)
	dir := filepath.Join(f.data, "snapshots", "u1", "raw")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := filepath.Join(dir, "rootfs.qcow2")
	if err := createQcow2OverlayOn(raw, filepath.Join(f.data, "private.img"), "qcow2", 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(raw, filepath.Join(dir, "vol0.img")); err != nil {
		t.Fatal(err)
	}
	if err := writeSnapshotManifest(dir, krucibleSnapManifest{
		DiskFile: "rootfs.qcow2", DiskFormat: "raw",
		Volumes: []snapVolume{{File: "vol0.img", Format: "raw"}},
	}); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(f.data, "images", "raw-alias.ext4")
	if err := os.Link(raw, alias); err != nil {
		t.Fatal(err)
	}
	node, err := f.e.nodeLink(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.e.recordNodeMetadata(node, "raw", []diskLayer{}); err != nil {
		t.Fatal(err)
	}
	sb := filepath.Join(f.data, "sandboxes", "overlay")
	if err := os.MkdirAll(sb, 0o700); err != nil {
		t.Fatal(err)
	}
	spec := VMSpec{RootDisk: filepath.Join(sb, "root.qcow2"), RootDiskFormat: "qcow2"}
	if err := createQcow2OverlayOn(spec.RootDisk, node, "raw", 1<<20); err != nil {
		t.Fatal(err)
	}
	locked, err := os.OpenFile(raw, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Close()
	lock := syscall.Flock_t{Type: syscall.F_WRLCK}
	if err := syscall.FcntlFlock(locked.Fd(), syscall.F_SETLK, &lock); err != nil {
		t.Fatal(err)
	}
	// Closing any descriptor to this inode drops this process's POSIX record
	// lock. A subprocess therefore detects even an otherwise invisible magic
	// probe, including a probe through another hard link.
	probe := func() {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestRecordedRawPayloadIsNeverOpened$")
		cmd.Env = append(os.Environ(), probeEnv+"="+raw)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("a metadata-only operation opened raw guest bytes: %v\n%s", err, out)
		}
	}
	probe()
	if err := f.e.recordDiskBacking(&spec); err != nil {
		t.Fatal(err)
	}
	probe()
	writeRecord(vmRecord{ID: "overlay", UserID: "u1", SandboxDir: sb, BaseSpec: spec})
	if _, err := diskRefs(f.data); err != nil {
		t.Fatal(err)
	}
	probe()
	if _, err := MigrateImages(f.data); err != nil {
		t.Fatal(err)
	}
	probe()
}

// Test fixtures can probe a legacy format; production layer creation always
// uses its captured format instead of inferring one from guest bytes.
func chainDepth(path string) (int, error) {
	if !maybeQcow2(path) {
		return 0, nil
	}
	chain, err := readDiskChain(path, "", nil)
	if err != nil {
		return 0, err
	}
	depth := 1 + len(chain)
	if len(chain) > 0 && chain[len(chain)-1].Format == "raw" {
		depth--
	}
	return depth, nil
}

func backingChain(path string) ([]string, error) {
	chain, err := readDiskChain(path, "", nil)
	if err != nil {
		return nil, err
	}
	paths := make([]string, len(chain))
	for i, layer := range chain {
		paths[i] = layer.Path
	}
	return paths, nil
}

func (f *nodeFixture) savedForkImage(owner, name string) (image, node string, backing []diskLayer) {
	f.t.Helper()
	source := f.freshRoot(owner + "-" + name + "-source")
	snap := f.snapshot(owner, name, source)
	root := f.resume(owner+"-"+name+"-fork", snap)
	node = backingOf(f.t, root)
	image = filepath.Join(f.data, "images", owner, name+".ext4")
	if err := os.MkdirAll(filepath.Dir(image), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := cloneFile(root, image); err != nil {
		f.t.Fatal(err)
	}
	spec := VMSpec{RootDisk: root, RootDiskFormat: "qcow2"}
	if err := f.e.recordDiskBacking(&spec); err != nil {
		f.t.Fatal(err)
	}
	if err := f.e.recordNodeMetadata(image, spec.RootDiskFormat, spec.Backing); err != nil {
		f.t.Fatal(err)
	}
	for _, dir := range []string{snap, filepath.Dir(root), filepath.Dir(source)} {
		if err := os.RemoveAll(dir); err != nil {
			f.t.Fatal(err)
		}
	}
	return image, node, spec.Backing
}

func TestSavedImageMetadataPreservesItsOwnersForkAuthority(t *testing.T) {
	for _, linkedRoot := range []bool{false, true} {
		name := "local-images"
		if linkedRoot {
			name = "linked-images-root"
		}
		t.Run(name, func(t *testing.T) {
			f := newNodeFixture(t)
			ownImage, ownNode, _ := f.savedForkImage("u1", "own")
			foreignImage, foreignNode, _ := f.savedForkImage("u2", "foreign")
			if linkedRoot {
				images := filepath.Join(f.data, "images")
				outside := filepath.Join(t.TempDir(), "images")
				if err := os.Rename(images, outside); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, images); err != nil {
					t.Fatal(err)
				}
			}
			for _, c := range []struct {
				owner, image, node, otherImage, otherNode string
			}{
				{"u1", ownImage, ownNode, foreignImage, foreignNode},
				{"u2", foreignImage, foreignNode, ownImage, ownNode},
			} {
				owned, err := ownedNodeKeys(f.data, c.owner)
				if err != nil {
					t.Fatal(err)
				}
				vm := &VM{UserID: c.owner}
				if _, err := readDiskChain(c.image, "qcow2", func(path string) error {
					return f.e.authorizedBacking(vm, path, owned)
				}); err != nil {
					t.Fatalf("saved image lost its destroyed fork's authority: %v", err)
				}
				for _, path := range []string{c.image, c.node} {
					fi, err := os.Stat(path)
					if err != nil {
						t.Fatal(err)
					}
					key, ok := keyOf(fi)
					if !ok || !owned[key] {
						t.Fatalf("owner %s lost saved-image provenance for %s", c.owner, path)
					}
				}
				for _, path := range []string{c.otherImage, c.otherNode} {
					fi, err := os.Stat(path)
					if err != nil {
						t.Fatal(err)
					}
					key, ok := keyOf(fi)
					if !ok || owned[key] {
						t.Fatalf("owner %s inherited another owner's image provenance for %s", c.owner, path)
					}
				}
				if err := f.e.authorizedBacking(vm, c.otherNode, owned); err == nil {
					t.Fatalf("owner %s may read another owner's hidden fork node", c.owner)
				}
			}
			if removed, err := SweepNodes(f.data, false); err != nil || len(removed) != 0 {
				t.Fatalf("saved images lost their hidden fork ancestors: %v, %v", removed, err)
			}
		})
	}
}

func TestSavedImageMetadataCannotCrossOwnerAliases(t *testing.T) {
	for _, alias := range []string{"owner-directory", "nested-directory", "image-file", "sidecar-file", "hardlinked-sidecar"} {
		t.Run(alias, func(t *testing.T) {
			f := newNodeFixture(t)
			image, node, backing := f.savedForkImage("u2", "foreign")
			ownerDir := filepath.Join(f.data, "images", "u1")
			if alias == "owner-directory" {
				if err := os.Symlink(filepath.Dir(image), ownerDir); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.MkdirAll(ownerDir, 0o700); err != nil {
					t.Fatal(err)
				}
				ownImage := filepath.Join(ownerDir, "copied.ext4")
				switch alias {
				case "nested-directory":
					if err := os.Symlink(filepath.Dir(image), filepath.Join(ownerDir, "nested")); err != nil {
						t.Fatal(err)
					}
				case "image-file":
					if err := os.Symlink(image, ownImage); err != nil {
						t.Fatal(err)
					}
					if err := f.e.recordNodeMetadata(ownImage, "qcow2", backing); err != nil {
						t.Fatal(err)
					}
				case "sidecar-file", "hardlinked-sidecar":
					if err := cloneFile(image, ownImage); err != nil {
						t.Fatal(err)
					}
					if alias == "sidecar-file" {
						if err := os.Symlink(nodeMetadataPath(image), nodeMetadataPath(ownImage)); err != nil {
							t.Fatal(err)
						}
					} else if err := os.Link(nodeMetadataPath(image), nodeMetadataPath(ownImage)); err != nil {
						t.Fatal(err)
					}
				}
			}
			owned, err := ownedNodeKeys(f.data, "u1")
			if alias == "owner-directory" || alias == "hardlinked-sidecar" {
				if err == nil {
					t.Fatal("unsafe owner metadata alias was not rejected")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			fi, err := os.Stat(node)
			if err != nil {
				t.Fatal(err)
			}
			key, ok := keyOf(fi)
			if !ok || owned[key] {
				t.Fatal("an alias conferred another owner's saved-image authority")
			}
		})
	}
}

func TestSweepNodesReclaimsLegacyNormalizationStages(t *testing.T) {
	f := newNodeFixture(t)
	source := f.freshRoot("source")
	stage, err := os.MkdirTemp(f.data, "legacy-resume-")
	if err != nil {
		t.Fatal(err)
	}
	disk, err := os.CreateTemp(stage, "disk-*.qcow2")
	if err != nil {
		t.Fatal(err)
	}
	if err := disk.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cloneFile(source, disk.Name()); err != nil {
		t.Fatal(err)
	}
	node, err := f.e.adoptNode(disk.Name(), "qcow2", []diskLayer{{Path: f.base, Format: "raw"}})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(stage, ".manifest-123"), []byte("interrupted metadata"))
	untouched := []string{
		filepath.Join(f.data, "legacy-resume-manual", "disk-123.qcow2"),
		filepath.Join(f.data, "legacy-resume-123", "notes.txt"),
		filepath.Join(f.data, "legacy-resume-456", "subdir", "disk-123.qcow2"),
		filepath.Join(f.data, "legacy-resume-789", "disk-manual.qcow2"),
		filepath.Join(f.data, "legacy-resume-987", ".manifest-manual"),
	}
	for _, path := range untouched {
		writeFile(t, path, []byte("not a normalization stage"))
	}
	outside := filepath.Join(t.TempDir(), "outside.img")
	writeFile(t, outside, []byte("outside the stage"))
	linked := filepath.Join(f.data, "legacy-resume-654", "disk-123.qcow2")
	if err := os.MkdirAll(filepath.Dir(linked), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, linked); err != nil {
		t.Fatal(err)
	}
	if _, err := SweepNodes(f.data, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stage); err != nil {
		t.Fatalf("dry run removed a normalization stage: %v", err)
	}
	if removed, err := SweepNodes(f.data, false); err != nil || len(removed) != 1 || removed[0] != node {
		t.Fatalf("crashed normalization stage pinned its adopted node: %v, %v", removed, err)
	}
	if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("normalization stage survived cleanup: %v", err)
	}
	if _, err := os.Stat(nodeMetadataPath(node)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("normalization cleanup left reclaimed node metadata: %v", err)
	}
	for _, path := range append(untouched, outside, linked) {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("normalization cleanup removed an unrecognized path %s: %v", path, err)
		}
	}
}

func TestAdoptNodeSyncsFallbackBeforePublishing(t *testing.T) {
	for _, failSync := range []bool{false, true} {
		name := "sync-before-link"
		if failSync {
			name = "sync-failure"
		}
		t.Run(name, func(t *testing.T) {
			f := newNodeFixture(t)
			source := f.freshRoot("source")
			sourceReal, err := resolveBasePath(source)
			if err != nil {
				t.Fatal(err)
			}
			originalLink, originalSync := linkNode, syncNodeFile
			t.Cleanup(func() {
				linkNode, syncNodeFile = originalLink, originalSync
			})
			synced, syncs := false, 0
			syncErr := errors.New("adopted copy sync failed")
			syncNodeFile = func(paths ...string) error {
				syncs++
				if len(paths) != 1 || filepath.Dir(paths[0]) != nodesDir(f.data) ||
					!numberedStage(filepath.Base(paths[0]), ".adopt-", "") {
					t.Fatalf("synced the source instead of its new inode: %v", paths)
				}
				if failSync {
					return syncErr
				}
				if err := originalSync(paths...); err != nil {
					return err
				}
				synced = true
				return nil
			}
			linkNode = func(src, dst string) error {
				if src == sourceReal {
					return &os.LinkError{Op: "link", Old: src, New: dst, Err: syscall.EXDEV}
				}
				if !synced {
					t.Fatal("adopted copy was published before its file sync")
				}
				return originalLink(src, dst)
			}
			node, err := f.e.adoptNode(source, "qcow2", []diskLayer{{Path: f.base, Format: "raw"}})
			if failSync {
				if !errors.Is(err, syncErr) || len(nodeEntries(t, f.data)) != 0 {
					t.Fatalf("failed file sync published a node: %s, %v", node, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				before, err := os.Stat(source)
				if err != nil {
					t.Fatal(err)
				}
				after, err := os.Stat(node)
				if err != nil || os.SameFile(before, after) {
					t.Fatalf("cross-filesystem adoption did not publish a copied inode: %v", err)
				}
			}
			if syncs != 1 {
				t.Fatalf("fallback copy crossed %d file-sync barriers, want one", syncs)
			}
		})
	}
}

func TestRestoredVolumeOwnershipIsPerDisk(t *testing.T) {
	for _, format := range []string{"raw", "qcow2"} {
		t.Run(format, func(t *testing.T) {
			f := newNodeFixture(t)
			source := filepath.Join(f.data, "snapshots", "u1", "raw", "vol0.img")
			if format == "raw" {
				writeFile(t, source, bytes.Repeat([]byte{1}, 513))
			} else {
				root := f.freshRoot("source")
				for gen := 1; gen < maxChainDepth; gen++ {
					name := "depth-" + string(rune('a'+gen))
					root = f.resume(name, f.snapshot("u1", name, root))
				}
				source = filepath.Join(f.snapshot("u1", "capped", root), "rootfs.qcow2")
			}
			sharedInfo, err := os.Stat(source)
			if err != nil {
				t.Fatal(err)
			}
			private := filepath.Join(t.TempDir(), "normalized.img")
			if err := cloneFile(source, private); err != nil {
				t.Fatal(err)
			}
			privateInfo, err := os.Stat(private)
			if err != nil {
				t.Fatal(err)
			}
			sb := filepath.Join(f.data, "sandboxes", "restored")
			if err := os.MkdirAll(sb, 0o700); err != nil {
				t.Fatal(err)
			}
			var spec VMSpec
			if err := f.e.layerRestoredVolumes(sb, &spec, []restoreVol{
				{path: source, format: format},
				{path: private, format: format, owned: true},
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(source); err != nil {
				t.Fatalf("restoring a private disk moved the untouched shared source: %v", err)
			}
			sharedCopy, err := os.Stat(spec.Volumes[0].Path)
			if err != nil || os.SameFile(sharedInfo, sharedCopy) {
				t.Fatalf("shared fallback was not copied: %v", err)
			}
			if _, err := os.Stat(private); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("private fallback was copied a second time instead of moved: %v", err)
			}
			privateCopy, err := os.Stat(spec.Volumes[1].Path)
			if err != nil || !os.SameFile(privateInfo, privateCopy) {
				t.Fatalf("private fallback did not retain its normalized inode: %v", err)
			}
			for _, volume := range spec.Volumes {
				if volume.Format != format {
					t.Fatalf("fallback lost explicit source format: %+v", volume)
				}
			}
		})
	}
}

func TestSweepNodesFindsTrustedSnapshotMetadataAtAnyDepth(t *testing.T) {
	for _, nested := range []bool{false, true} {
		name := "direct-import"
		if nested {
			name = "nested-import"
		}
		t.Run(name, func(t *testing.T) {
			f := newNodeFixture(t)
			parent := f.snapshot("u1", "parent", f.freshRoot("source"))
			root := f.resume("fork", parent)
			node := backingOf(t, root)
			spec := VMSpec{RootDisk: root, RootDiskFormat: "qcow2"}
			if err := f.e.recordDiskBacking(&spec); err != nil {
				t.Fatal(err)
			}
			var children []string
			for _, child := range []string{"published-a", "published-b"} {
				dir := filepath.Join(f.data, "snapshots", child)
				if nested {
					dir = filepath.Join(f.data, "snapshots", "u1", "group", child)
				}
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := cloneFile(root, filepath.Join(dir, "rootfs.qcow2")); err != nil {
					t.Fatal(err)
				}
				if err := writeSnapshotManifest(dir, krucibleSnapManifest{
					DiskFile: "rootfs.qcow2", DiskFormat: "qcow2", Backing: spec.Backing,
				}); err != nil {
					t.Fatal(err)
				}
				children = append(children, dir)
			}
			if err := os.RemoveAll(filepath.Join(f.data, "sandboxes")); err != nil {
				t.Fatal(err)
			}
			if users, err := usersOf(f.data, parent); err != nil || users.Snapshots != 2 || users.Other != 0 {
				t.Fatalf("noncanonical snapshots lost their dependent identity: %v, %v", users, err)
			}
			if err := os.RemoveAll(parent); err != nil {
				t.Fatal(err)
			}
			for _, child := range children {
				if removed, err := SweepNodes(f.data, false); err != nil || len(removed) != 0 {
					t.Fatalf("trusted snapshot metadata was undiscovered: %v, %v", removed, err)
				}
				if err := os.RemoveAll(child); err != nil {
					t.Fatal(err)
				}
			}
			if removed, err := SweepNodes(f.data, false); err != nil || len(removed) != 1 || removed[0] != node {
				t.Fatalf("snapshot lineage survived its last metadata root: %v, %v", removed, err)
			}
		})
	}
}

func TestSnapshotCatalogDoesNotTrustNestedDirectoryAliases(t *testing.T) {
	f := newNodeFixture(t)
	target := f.snapshot("u2", "private", f.freshRoot("target"))
	dead, err := f.e.nodeLink(filepath.Join(target, "rootfs.qcow2"))
	if err != nil {
		t.Fatal(err)
	}
	save := filepath.Join(f.data, "sandboxes", "helper", "save", "checkpoint")
	if err := os.MkdirAll(save, 0o700); err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(save, "rootfs.qcow2")
	if err := createQcow2OverlayOn(disk, dead, "qcow2", 1<<20); err != nil {
		t.Fatal(err)
	}
	// A non-root helper can share the daemon's UID. Location, not just the
	// metadata file's ownership, distinguishes this writable save from a
	// published snapshot.
	if err := writeSnapshotManifest(save, krucibleSnapManifest{
		DiskFile: "rootfs.qcow2", DiskFormat: "qcow2",
		Backing: []diskLayer{{Path: dead, Format: "qcow2"}, {Path: f.base, Format: "raw"}},
	}); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(f.data, "snapshots", "u1", "forged")
	if err := os.MkdirAll(filepath.Dir(alias), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(save, alias); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}
	if removed, err := SweepNodes(f.data, false); err != nil || len(removed) != 1 || removed[0] != dead {
		t.Fatalf("helper-written metadata gained snapshot authority through an alias: %v, %v", removed, err)
	}
	for _, path := range []string{disk, filepath.Join(save, snapshotManifestFile)} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("GC removed helper state while ignoring its metadata: %v", err)
		}
	}
}
