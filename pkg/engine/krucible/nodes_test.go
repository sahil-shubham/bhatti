package krucible

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
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
	root, _, _, err := f.e.prepareNodeRoot(dir, filepath.Join(snapDir, "rootfs.qcow2"), false)
	if err != nil {
		f.t.Fatal(err)
	}
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

// TestSweepNodesCascades: with the last disk gone, a chain of nodes goes in
// one call, deepest-held last.
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
	if err := f.e.layerOn(dst, disk, false); err != nil {
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
	if err := f.e.layerOn(moved, disk, true); err != nil {
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
		{path: filepath.Join(snap, "vol0.img")},
		{path: filepath.Join(snap, "vol1.img"), readOnly: true},
		{path: filepath.Join(snap, "vol2.img")},
	}, false)
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
	// The backing list the launch checks is every file named, not images.
	spec.RootDisk, spec.RootDiskFormat = f.freshRoot("a"), "qcow2"
	backing, err := f.e.diskBacking(spec)
	if err != nil || len(backing) != 2 {
		t.Fatalf("backing = %v, %v; want the two volume nodes and not the base under images/", backing, err)
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
	spec.Backing = []string{node}
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
	if err := f.e.layerRestoredVolumes(sb, &spec, []restoreVol{{path: filepath.Join(snap, "vol0.img")}}, false); err != nil {
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
	var err2 error
	if spec.Backing, err2 = f.e.diskBacking(spec); err2 != nil {
		t.Fatal(err2)
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
