package krucible

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// readArchiveManifestFrom is the manifest an export wrote at the start of
// archive.
func readArchiveManifestFrom(archive []byte) (archiveManifest, error) {
	zr, err := zstd.NewReader(bytes.NewReader(archive))
	if err != nil {
		return archiveManifest{}, err
	}
	defer zr.Close()
	return readArchiveManifest(tar.NewReader(zr))
}

// layeredSnapshot is a disk-only snapshot of a sandbox that was itself
// resumed from a snapshot, with a volume it restored: its root disk is layered
// over a node, which is over the base, and its volume over a raw node.
func layeredSnapshot(t *testing.T) (f *nodeFixture, dir string, m krucibleSnapManifest) {
	t.Helper()
	f = newNodeFixture(t)
	first := f.snapshot("u1", "first", f.freshRoot("src"))
	f.write(filepath.Join(first, "rootfs.qcow2"), 7*qcow2ClusterSize, []byte("written before the first snapshot"))
	writeFile(t, filepath.Join(first, "vol0.img"), bytes.Repeat([]byte("volume.."), 1<<17))

	sb := filepath.Join(f.data, "sandboxes", "r1")
	if err := os.MkdirAll(sb, 0o700); err != nil {
		t.Fatal(err)
	}
	var spec VMSpec
	if err := f.e.layerRestoredVolumes(sb, &spec, []restoreVol{{path: filepath.Join(first, "vol0.img")}}, false); err != nil {
		t.Fatal(err)
	}
	root, _, _, err := f.e.prepareNodeRoot(sb, filepath.Join(first, "rootfs.qcow2"), false)
	if err != nil {
		t.Fatal(err)
	}
	f.write(root, 9*qcow2ClusterSize, []byte("written by the resumed sandbox"))

	// The second snapshot: copies of the resumed sandbox's overlays.
	dir = filepath.Join(f.data, "snapshots", "u1", "second")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := cloneFile(root, filepath.Join(dir, "rootfs.qcow2")); err != nil {
		t.Fatal(err)
	}
	if err := cloneFile(spec.Volumes[0].Path, filepath.Join(dir, "vol0.img")); err != nil {
		t.Fatal(err)
	}
	m = krucibleSnapManifest{
		Arch: hostSnapshotArch(), Type: "filesystem", Vcpus: 1, MemMiB: 64,
		DiskFile: "rootfs.qcow2", ConfigFile: "config.ext4",
		Volumes: []snapVolume{{File: "vol0.img"}},
	}
	return f, dir, m
}

// TestLayeredSnapshotMovesToAnotherHost: a snapshot taken from a resumed
// sandbox holds only what that sandbox wrote; the export carries what its
// disks are layered over (a node under the root disk, one under the volume),
// the import puts each in the new host's node store and re-points the header
// above it, so the snapshot resumes there over the same bytes.
func TestLayeredSnapshotMovesToAnotherHost(t *testing.T) {
	f, dir, m := layeredSnapshot(t)
	rootNode := backingOf(t, filepath.Join(dir, "rootfs.qcow2"))
	volNode := backingOf(t, filepath.Join(dir, "vol0.img"))
	if filepath.Dir(rootNode) != nodesDir(f.data) || filepath.Dir(volNode) != nodesDir(f.data) {
		t.Fatalf("the snapshot isn't layered over nodes: %s, %s", rootNode, volNode)
	}
	archive := exportArchive(t, f.e, dir, m, true)

	b := portableEngine(t, "", "exit 0")
	dest, imp, err := importArchive(b, archive)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if imp.Type != "filesystem" {
		t.Fatalf("imported %+v", imp)
	}
	if left, _ := filepath.Glob(filepath.Join(dest, "layer-*")); len(left) != 0 {
		t.Fatalf("layers left in the snapshot directory: %v", left)
	}

	chain, err := backingChain(filepath.Join(dest, "rootfs.qcow2"))
	if err != nil || len(chain) != 2 {
		t.Fatalf("root disk chain %v, %v; want a node and the base", chain, err)
	}
	if filepath.Dir(chain[0]) != nodesDir(b.cfg.DataDir) || filepath.Dir(chain[1]) != basesDir(b.cfg.DataDir) {
		t.Fatalf("root disk is layered over %v; want a node of this host's store, then a base of this host's", chain)
	}
	if got, want := fileSum(t, chain[1]), fileSum(t, f.base); got != want {
		t.Fatal("the base changed on the way")
	}
	// The node's clusters are the same bytes; only the header names a new file.
	src, _ := os.ReadFile(rootNode)
	got, _ := os.ReadFile(chain[0])
	if len(got) != len(src) || !bytes.Equal(got[qcow2ClusterSize:], src[qcow2ClusterSize:]) {
		t.Fatal("the node's clusters changed on the way")
	}
	if backingFormatOf(t, filepath.Join(dest, "rootfs.qcow2")) != "qcow2" || backingFormatOf(t, chain[0]) != "raw" {
		t.Fatal("the headers lost the format of what they are layered over")
	}

	vchain, err := backingChain(filepath.Join(dest, "vol0.img"))
	if err != nil || len(vchain) != 1 || filepath.Dir(vchain[0]) != nodesDir(b.cfg.DataDir) {
		t.Fatalf("volume chain %v, %v", vchain, err)
	}
	if fileSum(t, vchain[0]) != fileSum(t, volNode) {
		t.Fatal("the volume's node changed on the way")
	}
	if entries := nodeEntries(t, b.cfg.DataDir); len(entries) != 2 {
		t.Fatalf("the new host's store holds %v, want the two nodes", entries)
	}

	// It resumes there as it did here, layered over the imported disk, and
	// what is layered over it keeps it from being deleted.
	sb := filepath.Join(b.cfg.DataDir, "sandboxes", "x")
	os.MkdirAll(sb, 0o700)
	root, _, _, err := b.prepareNodeRoot(sb, filepath.Join(dest, "rootfs.qcow2"), false)
	if err != nil {
		t.Fatal(err)
	}
	if d, err := chainDepth(root); err != nil || d != 3 {
		t.Fatalf("depth %d, %v; the resumed root, the imported disk and its node make three", d, err)
	}
	final := filepath.Join(b.cfg.DataDir, "snapshots", "imported")
	if err := os.Rename(dest, final); err != nil {
		t.Fatal(err)
	}
	if err := b.RemoveSnapshot(context.Background(), final); !errors.Is(err, engine.ErrSnapshotInUse) {
		t.Fatalf("remove of an imported snapshot with a clone: %v", err)
	}
	os.RemoveAll(sb)
	if err := b.RemoveSnapshot(context.Background(), final); err != nil {
		t.Fatalf("remove after the clone went: %v", err)
	}
	if entries := nodeEntries(t, b.cfg.DataDir); len(entries) != 0 {
		t.Fatalf("nodes left behind: %v", entries)
	}
}

// TestLayeredSnapshotExportIsRefusedBeforeWriting: a disk layered over a file
// that isn't one of this host's nodes names a file the archive has no way to
// carry, whether it is the root disk's or a volume's.
func TestLayeredSnapshotExportIsRefusedBeforeWriting(t *testing.T) {
	for _, which := range []string{"rootfs.qcow2", "vol0.img"} {
		t.Run(which, func(t *testing.T) {
			f, dir, m := layeredSnapshot(t)
			stray := filepath.Join(t.TempDir(), "stray.img")
			if err := createQcow2Overlay(stray, f.base, uint64(f.size)); err != nil {
				t.Fatal(err)
			}
			if err := setQcow2Backing(filepath.Join(dir, which), stray); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(m)
			var buf bytes.Buffer
			err := f.e.ExportSnapshot(context.Background(), &buf, dir, raw, engine.SnapshotExport{Name: "x"})
			if !errors.Is(err, engine.ErrNotSupported) || buf.Len() != 0 {
				t.Fatalf("export: %v after %d bytes", err, buf.Len())
			}
		})
	}
}

// TestImportRefusesLayersThatDontHangFromTheSnapshot: the layers an archive
// lists are checked as the rest of it is: named in order, each hung from a
// disk or a layer, none twice, none in a loop.
func TestImportRefusesLayersThatDontHangFromTheSnapshot(t *testing.T) {
	f, dir, m := layeredSnapshot(t)
	e := portableEngine(t, f.base, "exit 0")
	good := func() archiveManifest {
		var buf bytes.Buffer
		raw, _ := json.Marshal(m)
		if err := f.e.ExportSnapshot(context.Background(), &buf, dir, raw, engine.SnapshotExport{Name: "x", IncludeBase: true}); err != nil {
			t.Fatal(err)
		}
		am, err := readArchiveManifestFrom(buf.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		return am
	}
	cases := map[string]func(am *archiveManifest){
		"a layer misnamed":          func(am *archiveManifest) { am.Layers[0].Name = "layer-9.img" },
		"hung from nothing":         func(am *archiveManifest) { am.Layers[0].Over = "nowhere.img" },
		"hung from itself":          func(am *archiveManifest) { am.Layers[0].Over = am.Layers[0].Name },
		"two layers from one entry": func(am *archiveManifest) { am.Layers[1].Over = am.Layers[0].Over },
		"the base under no chain":   func(am *archiveManifest) { am.Base.Over = "vol0.img" },
		"the base above a layer":    func(am *archiveManifest) { am.Base.Over = "rootfs.qcow2" },
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			am := good()
			damage(&am)
			if _, err := e.admitArchive(&am); !errors.Is(err, engine.ErrBadSnapshotArchive) {
				t.Fatalf("admitted: %v", err)
			}
		})
	}
	am := good()
	if _, err := e.admitArchive(&am); err != nil {
		t.Fatalf("the undamaged archive: %v", err)
	}
	if len(am.Layers) != 2 || !strings.HasPrefix(am.Layers[0].Name, archiveLayerPrefix) {
		t.Fatalf("layers %+v", am.Layers)
	}
}
