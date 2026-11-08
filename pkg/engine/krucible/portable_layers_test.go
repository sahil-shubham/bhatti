package krucible

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

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

// layeredSnapshot is a memory snapshot of a sandbox that was itself resumed
// from a snapshot, with a volume it restored: its root disk is layered
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
	if err := f.e.layerRestoredVolumes(sb, &spec, []restoreVol{{path: filepath.Join(first, "vol0.img"), format: "raw"}}); err != nil {
		t.Fatal(err)
	}
	root, _, _, err := f.e.prepareNodeRoot(sb, filepath.Join(first, "rootfs.qcow2"), false, "qcow2")
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
	writeFile(t, filepath.Join(dir, checkpointFile), []byte("KRUNCKPT checkpoint"))
	writeFile(t, filepath.Join(dir, memoryFile), make([]byte, 8192))
	m = krucibleSnapManifest{
		Arch: hostSnapshotArch(), Type: "memory", Vcpus: 1, MemMiB: 64,
		DiskFile: "rootfs.qcow2", DiskFormat: "qcow2", ConfigFile: "config.ext4",
		Volumes: []snapVolume{{File: "vol0.img", Format: "qcow2"}},
	}
	captured := VMSpec{RootDisk: filepath.Join(dir, m.DiskFile), RootDiskFormat: m.DiskFormat,
		Volumes: []VMVolume{{Path: filepath.Join(dir, "vol0.img"), Format: "qcow2"}}}
	if err := f.e.recordDiskBacking(&captured); err != nil {
		t.Fatal(err)
	}
	m.Backing, m.Volumes[0].Backing = captured.Backing, captured.Volumes[0].Backing
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
	if imp.Type != "memory" {
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
	root, _, _, err := b.prepareNodeRoot(sb, filepath.Join(dest, "rootfs.qcow2"), false, "qcow2")
	if err != nil {
		t.Fatal(err)
	}
	spec := VMSpec{RootDisk: root, RootDiskFormat: "qcow2"}
	if err := b.recordDiskBacking(&spec); err != nil {
		t.Fatal(err)
	}
	writeRecord(vmRecord{ID: "x", UserID: "u1", SandboxDir: sb, BaseSpec: spec})
	if d, err := chainDepth(root); err != nil || d != 3 {
		t.Fatalf("depth %d, %v; the resumed root, the imported disk and its node make three", d, err)
	}
	final := filepath.Join(b.cfg.DataDir, "snapshots", "imported")
	if err := b.PublishSnapshotImport(context.Background(), dest, final); err != nil {
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

func plannedLayeredArchive(t *testing.T, e *Engine, dir string, m krucibleSnapManifest) (archiveManifest, []archiveEntry) {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	am, paths, err := e.planExport(dir, raw, engine.SnapshotExport{Name: "layered", IncludeBase: true})
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]archiveEntry, len(paths))
	for i, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		entries[i] = archiveEntry{am.Files[i].Name, data}
	}
	return am, entries
}

func replaceLayeredEntry(t *testing.T, am *archiveManifest, entries []archiveEntry, name string, data []byte) {
	t.Helper()
	for i := range entries {
		if entries[i].name == name {
			entries[i].data = data
			am.Files[i].Size = int64(len(data))
			return
		}
	}
	t.Fatalf("no archive entry %s", name)
}

func overlayArchiveBytes(t *testing.T, backing, format string) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "overlay.qcow2")
	if err := createQcow2OverlayOn(path, backing, format, 1<<20); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func archiveEntryBytes(t *testing.T, entries []archiveEntry, name string) []byte {
	t.Helper()
	for _, entry := range entries {
		if entry.name == name {
			return bytes.Clone(entry.data)
		}
	}
	t.Fatalf("no archive entry %s", name)
	return nil
}

func setArchiveBackingFormat(data []byte, format string) {
	binary.BigEndian.PutUint32(data[qcow2HeaderLen+4:], uint32(len(format)))
	clear(data[qcow2HeaderLen+8 : qcow2HeaderLen+16])
	copy(data[qcow2HeaderLen+8:], format)
}

func TestLayeredImportRejectsIncompleteDiskGraphs(t *testing.T) {
	f, dir, m := layeredSnapshot(t)
	cases := map[string]func(*archiveManifest, []archiveEntry) []archiveEntry{
		"unlisted qcow2 volume tail": func(am *archiveManifest, entries []archiveEntry) []archiveEntry {
			volume := archiveEntryBytes(t, entries, "vol0.img")
			setArchiveBackingFormat(volume, "qcow2")
			replaceLayeredEntry(t, am, entries, "vol0.img", volume)
			tail := overlayArchiveBytes(t, filepath.Join(t.TempDir(), "private.img"), "raw")
			replaceLayeredEntry(t, am, entries, "layer-2.img", tail)
			return entries
		},
		"raw header above another qcow2 dependency": func(am *archiveManifest, entries []archiveEntry) []archiveEntry {
			root := archiveEntryBytes(t, entries, "rootfs.qcow2")
			setArchiveBackingFormat(root, "raw")
			replaceLayeredEntry(t, am, entries, "rootfs.qcow2", root)
			return entries
		},
		"qcow2 header above the raw base": func(am *archiveManifest, entries []archiveEntry) []archiveEntry {
			layer := archiveEntryBytes(t, entries, "layer-1.img")
			setArchiveBackingFormat(layer, "qcow2")
			replaceLayeredEntry(t, am, entries, "layer-1.img", layer)
			return entries
		},
		"unsupported backing format": func(am *archiveManifest, entries []archiveEntry) []archiveEntry {
			root := archiveEntryBytes(t, entries, "rootfs.qcow2")
			setArchiveBackingFormat(root, "vmdk")
			replaceLayeredEntry(t, am, entries, "rootfs.qcow2", root)
			return entries
		},
		"duplicate backing format": func(am *archiveManifest, entries []archiveEntry) []archiveEntry {
			root := archiveEntryBytes(t, entries, "rootfs.qcow2")
			off := binary.BigEndian.Uint64(root[8:])
			size := binary.BigEndian.Uint32(root[16:])
			copy(root[256:], root[off:off+uint64(size)])
			binary.BigEndian.PutUint64(root[8:], 256)
			binary.BigEndian.PutUint32(root[120:], qcow2ExtBackingFmt)
			binary.BigEndian.PutUint32(root[124:], 3)
			copy(root[128:], "raw")
			clear(root[131:144])
			replaceLayeredEntry(t, am, entries, "rootfs.qcow2", root)
			return entries
		},
		"external data file feature": func(am *archiveManifest, entries []archiveEntry) []archiveEntry {
			root := archiveEntryBytes(t, entries, "rootfs.qcow2")
			binary.BigEndian.PutUint64(root[72:], 1<<2)
			replaceLayeredEntry(t, am, entries, "rootfs.qcow2", root)
			return entries
		},
		"external data file extension": func(am *archiveManifest, entries []archiveEntry) []archiveEntry {
			root := archiveEntryBytes(t, entries, "rootfs.qcow2")
			binary.BigEndian.PutUint32(root[qcow2HeaderLen:], 0x44415441)
			replaceLayeredEntry(t, am, entries, "rootfs.qcow2", root)
			return entries
		},
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			am, entries := plannedLayeredArchive(t, f.e, dir, m)
			entries = damage(&am, entries)
			b := portableEngine(t, "", "exit 0")
			dest, _, err := importArchive(b, craftArchive(t, am, entries))
			if !errors.Is(err, engine.ErrBadSnapshotArchive) {
				t.Fatalf("import: %v; want a bad archive", err)
			}
			mustNotExist(t, dest)
			if nodes, err := os.ReadDir(nodesDir(b.cfg.DataDir)); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			} else if len(nodes) != 0 {
				t.Fatalf("refused archive published nodes: %v", nodes)
			}
			mustNotExist(t, basesDir(b.cfg.DataDir))
		})
	}
}

func TestLayeredImportRollsBackPublishedNodes(t *testing.T) {
	f, dir, m := layeredSnapshot(t)
	am, entries := plannedLayeredArchive(t, f.e, dir, m)
	// The extensions leave room for an archive-relative name, but not a
	// node-store path. Failure must roll back the already published lower node.
	root := overlayArchiveBytes(t, "x", "qcow2")
	binary.BigEndian.PutUint32(root[20:], 9)
	clear(root[120:512])
	binary.BigEndian.PutUint32(root[120:], 0x12345678)
	binary.BigEndian.PutUint32(root[124:], 344)
	binary.BigEndian.PutUint64(root[8:], 480)
	binary.BigEndian.PutUint32(root[16:], 1)
	root[480] = 'x'
	replaceLayeredEntry(t, &am, entries, "rootfs.qcow2", root)
	b := portableEngine(t, "", "exit 0")
	dest, _, err := importArchive(b, craftArchive(t, am, entries))
	if err == nil || !strings.Contains(err.Error(), "doesn't fit the qcow2 header") {
		t.Fatalf("expected a failed header rewrite, got %v", err)
	}
	mustNotExist(t, dest)
	if nodes, err := os.ReadDir(nodesDir(b.cfg.DataDir)); err != nil {
		t.Fatal(err)
	} else if len(nodes) != 0 {
		t.Fatalf("failed import left nodes: %v", nodes)
	}
	mustNotExist(t, basesDir(b.cfg.DataDir))
}

func TestLayeredSnapshotPreservesRawQcow2Magic(t *testing.T) {
	f, dir, m := layeredSnapshot(t)
	volumeNode := backingOf(t, filepath.Join(dir, "vol0.img"))
	secret := filepath.Join(f.data, "private.img")
	writeFile(t, secret, []byte("not a disk dependency"))
	f.write(volumeNode, 0, overlayArchiveBytes(t, secret, "raw"))
	want := fileSum(t, volumeNode)
	if err := writeSnapshotManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	b := portableEngine(t, "", "exit 0")
	dest, _, err := importArchive(b, exportArchive(t, f.e, dir, m, true))
	if err != nil {
		t.Fatal(err)
	}
	chain, err := backingChain(filepath.Join(dest, "vol0.img"))
	if err != nil || len(chain) != 1 {
		t.Fatalf("a raw node's contents became a backing dependency: %v, %v", chain, err)
	}
	if fileSum(t, chain[0]) != want {
		t.Fatal("raw volume bytes were rewritten as a qcow2 header")
	}
}

func TestLayeredSnapshotImportsIndependentNodes(t *testing.T) {
	f, dir, m := layeredSnapshot(t)
	wantRoot, err := os.ReadFile(backingOf(t, filepath.Join(dir, "rootfs.qcow2")))
	if err != nil {
		t.Fatal(err)
	}
	wantVolume := fileSum(t, backingOf(t, filepath.Join(dir, "vol0.img")))
	archive := exportArchive(t, f.e, dir, m, true)
	if err := os.RemoveAll(f.data); err != nil {
		t.Fatal(err)
	}
	b := portableEngine(t, "", "exit 0")
	var roots, volumes, destinations []string
	for _, name := range []string{"first", "second"} {
		dest := filepath.Join(b.cfg.DataDir, "snapshots", name)
		if _, err := b.ImportSnapshot(context.Background(), bytes.NewReader(archive), dest); err != nil {
			t.Fatal(err)
		}
		destinations = append(destinations, dest)
		root := backingOf(t, filepath.Join(dest, "rootfs.qcow2"))
		volume := backingOf(t, filepath.Join(dest, "vol0.img"))
		roots, volumes = append(roots, root), append(volumes, volume)
		gotRoot, err := os.ReadFile(root)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(gotRoot[qcow2ClusterSize:], wantRoot[qcow2ClusterSize:]) || fileSum(t, volume) != wantVolume {
			t.Fatal("an imported chain lost its source's disk data")
		}
	}
	if roots[0] == roots[1] || volumes[0] == volumes[1] {
		t.Fatalf("independent imports reused mutable node paths: roots %v, volumes %v", roots, volumes)
	}
	if err := b.RemoveSnapshot(context.Background(), destinations[0]); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{roots[0], volumes[0]} {
		mustNotExist(t, path)
	}
	for _, path := range []string{roots[1], volumes[1]} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("deleting the first import broke the second's chain: %v", err)
		}
	}
}

func TestPublishSnapshotImportSerializesWithGC(t *testing.T) {
	f, dir, m := layeredSnapshot(t)
	archive := exportArchive(t, f.e, dir, m, true)
	b := portableEngine(t, "", "exit 0")
	parent := filepath.Join(b.cfg.DataDir, "snapshots", "u1")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	staging, final := filepath.Join(parent, "import-pending.tmp"), filepath.Join(parent, "published")
	if _, err := b.ImportSnapshot(t.Context(), bytes.NewReader(archive), staging); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(staging, snapshotManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	nodes := []string{
		backingOf(t, filepath.Join(staging, "rootfs.qcow2")),
		backingOf(t, filepath.Join(staging, "vol0.img")),
	}
	for _, node := range nodes {
		fi, err := os.Stat(node)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Sys().(*syscall.Stat_t).Nlink != 1 {
			t.Fatalf("imported ancestor is not store-only: %s", node)
		}
	}
	// Import has released its lease. The completed managed staging manifest
	// is a stable root until publication acquires its own lease.
	if removed, err := SweepNodes(b.cfg.DataDir, false); err != nil || len(removed) != 0 {
		t.Fatalf("GC missed the completed staging manifest: %v, %v", removed, err)
	}
	release, err := lockBases(b.cfg.DataDir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := b.PublishSnapshotImport(ctx, staging, final); !errors.Is(err, context.Canceled) {
		t.Fatalf("publication renamed while GC owned the exclusive lock: %v", err)
	}
	mustNotExist(t, final)
	if got, err := os.ReadFile(filepath.Join(staging, snapshotManifestFile)); err != nil || !bytes.Equal(got, before) {
		t.Fatalf("canceled publication changed its staging root: %s, %v", got, err)
	}
	release()
	if err := b.PublishSnapshotImport(t.Context(), staging, final); err != nil {
		t.Fatal(err)
	}
	requireBasesLease(t, b.cfg.DataDir, false)
	mustNotExist(t, staging)
	if got, err := os.ReadFile(filepath.Join(final, snapshotManifestFile)); err != nil || !bytes.Equal(got, before) {
		t.Fatalf("publication changed its completed manifest: %s, %v", got, err)
	}
	if removed, err := SweepNodes(b.cfg.DataDir, false); err != nil || len(removed) != 0 {
		t.Fatalf("GC missed the final manifest: %v, %v", removed, err)
	}
	for _, node := range nodes {
		if _, err := os.Stat(node); err != nil {
			t.Fatalf("publication lost an imported ancestor: %s: %v", node, err)
		}
	}
	if err := b.RemoveSnapshot(t.Context(), final); err != nil {
		t.Fatal(err)
	}
	for _, node := range nodes {
		mustNotExist(t, node)
	}
}

func TestLayeredExportKeepsOpenInputsAfterSnapshotRemoval(t *testing.T) {
	for _, outcome := range []string{"success", "writer-error", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			f, source, m := layeredSnapshot(t)
			archive := exportArchive(t, f.e, source, m, true)
			b := portableEngine(t, "", "exit 0")
			dir, imp, err := importArchive(b, archive)
			if err != nil {
				t.Fatal(err)
			}
			nodes := []string{
				backingOf(t, filepath.Join(dir, "rootfs.qcow2")),
				backingOf(t, filepath.Join(dir, "vol0.img")),
			}
			var writeErr error
			if outcome == "writer-error" {
				writeErr = errors.New("blocked export writer failed")
			} else if outcome == "cancelled" {
				writeErr = context.Canceled
			}
			w := newGatedExportWriter(t, writeErr)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- b.ExportSnapshot(ctx, w, dir, imp.ManifestJSON, engine.SnapshotExport{Name: "layered"})
			}()
			waitForExportWrite(t, ctx, w, done)
			requireBasesLease(t, b.cfg.DataDir, false)
			if err := b.RemoveSnapshot(ctx, dir); err != nil {
				t.Fatalf("snapshot deletion waited for the exporting writer: %v", err)
			}
			for _, node := range nodes {
				mustNotExist(t, node)
			}
			if outcome == "cancelled" {
				cancel()
			}
			w.unblock()
			select {
			case err := <-done:
				if writeErr == nil && err != nil {
					t.Fatal(err)
				}
				if writeErr != nil && (err == nil || !strings.Contains(err.Error(), writeErr.Error())) {
					t.Fatalf("export lost its streaming failure: %v, want %v", err, writeErr)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("export did not finish after its writer was released")
			}
			requireBasesLease(t, b.cfg.DataDir, false)
			if outcome == "success" {
				if _, err := readArchiveManifestFrom(w.Bytes()); err != nil {
					t.Fatalf("export did not finish a whole archive: %v", err)
				}
			}
			mustNotExist(t, dir)
			for _, node := range nodes {
				mustNotExist(t, node)
			}
		})
	}
}

func TestLayeredExportBlockedWriterDoesNotBlockDeleteAndSweep(t *testing.T) {
	f, source, m := layeredSnapshot(t)
	archive := exportArchive(t, f.e, source, m, true)
	b := portableEngine(t, "", "exit 0")
	dir, imp, err := importArchive(b, archive)
	if err != nil {
		t.Fatal(err)
	}
	node := backingOf(t, filepath.Join(dir, "rootfs.qcow2"))
	w := newGatedExportWriter(t, io.ErrClosedPipe)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	finished := make(chan struct{})
	// The fake never releases during the assertion: even cancellation cannot
	// make a non-cooperating response writer return.
	t.Cleanup(func() {
		w.unblock()
		<-finished
	})
	go func() {
		done <- b.ExportSnapshot(ctx, w, dir, imp.ManifestJSON, engine.SnapshotExport{Name: "blocked"})
		close(finished)
	}()
	waitForExportWrite(t, ctx, w, done)
	cancel()
	requireBasesLease(t, b.cfg.DataDir, false)
	reclaimed := make(chan error, 1)
	go func() {
		if err := b.RemoveSnapshot(t.Context(), dir); err != nil {
			reclaimed <- err
			return
		}
		_, err := SweepNodes(b.cfg.DataDir, false)
		reclaimed <- err
	}()
	select {
	case err := <-reclaimed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("blocked export prevented snapshot deletion or node sweep")
	}
	mustNotExist(t, dir)
	mustNotExist(t, node)
	select {
	case err := <-done:
		t.Fatalf("writer returned without its cleanup release: %v", err)
	default:
	}
}

func TestArchiveRejectsLayerNamespaceCycles(t *testing.T) {
	e := portableEngine(t, "", "exit 0")
	am := archiveManifest{
		Format: archiveFormat, Arch: hostSnapshotArch(),
		Snapshot: krucibleSnapManifest{Type: "filesystem", DiskFile: "layer-2.img", DiskFormat: "qcow2", Vcpus: 1, MemMiB: 64},
		Layers: []archiveLayer{
			{Name: "layer-1.img", Format: "qcow2", Over: "layer-2.img"},
			{Name: "layer-2.img", Format: "qcow2", Over: "layer-1.img"},
		},
	}
	if _, err := e.admitArchive(&am); !errors.Is(err, engine.ErrBadSnapshotArchive) {
		t.Fatalf("a disk/layer name collision was admitted: %v", err)
	}
}

func TestArchiveRejectsExcessiveChainDepth(t *testing.T) {
	e := portableEngine(t, "", "exit 0")
	am := archiveManifest{
		Format: archiveFormat, Arch: hostSnapshotArch(),
		Snapshot: krucibleSnapManifest{Type: "filesystem", DiskFile: "rootfs.qcow2", DiskFormat: "qcow2", Vcpus: 1, MemMiB: 64},
		Files:    []archiveFile{{Name: "rootfs.qcow2"}},
	}
	over := am.Snapshot.DiskFile
	for i := 0; i <= maxChainDepth; i++ {
		name := fmt.Sprintf("layer-%d.img", i+1)
		am.Layers = append(am.Layers, archiveLayer{Name: name, Format: "qcow2", Over: over})
		am.Files = append(am.Files, archiveFile{Name: name})
		over = name
	}
	if _, err := e.admitArchive(&am); !errors.Is(err, engine.ErrBadSnapshotArchive) {
		t.Fatalf("an over-depth chain was admitted: %v", err)
	}
}

func TestFilesystemArchiveRefusesVolumeDeviceSet(t *testing.T) {
	f, dir, m := layeredSnapshot(t)
	am, entries := plannedLayeredArchive(t, f.e, dir, m)
	m.Type, am.Snapshot.Type = "filesystem", "filesystem"
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := f.e.ExportSnapshot(context.Background(), &archive, dir, raw, engine.SnapshotExport{}); err == nil || archive.Len() != 0 {
		t.Fatalf("export silently kept unrestorable volumes: %v, %d bytes", err, archive.Len())
	}
	am.Files, entries = am.Files[2:], entries[2:]
	b := portableEngine(t, "", "exit 0")
	dest, _, err := importArchive(b, craftArchive(t, am, entries))
	if !errors.Is(err, engine.ErrBadSnapshotArchive) {
		t.Fatalf("import silently kept unrestorable volumes: %v", err)
	}
	mustNotExist(t, dest)
}

func TestLayeredArchiveRebuildsMultipleGenerations(t *testing.T) {
	f, first, m := layeredSnapshot(t)
	root := f.resume("next", first)
	var spec VMSpec
	if err := f.e.layerRestoredVolumes(filepath.Dir(root), &spec, []restoreVol{{path: filepath.Join(first, "vol0.img"), format: "qcow2"}}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.data, "snapshots", "u1", "third")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, file := range []struct{ src, name string }{
		{root, "rootfs.qcow2"},
		{spec.Volumes[0].Path, "vol0.img"},
		{filepath.Join(first, checkpointFile), checkpointFile},
		{filepath.Join(first, memoryFile), memoryFile},
	} {
		if err := cloneFile(file.src, filepath.Join(dir, file.name)); err != nil {
			t.Fatal(err)
		}
	}
	b := portableEngine(t, "", "exit 0")
	dest, _, err := importArchive(b, exportArchive(t, f.e, dir, m, true))
	if err != nil {
		t.Fatal(err)
	}
	for _, disk := range []struct {
		name  string
		depth int
	}{{"rootfs.qcow2", 3}, {"vol0.img", 2}} {
		source, err := backingChain(filepath.Join(dir, disk.name))
		if err != nil {
			t.Fatal(err)
		}
		chain, err := backingChain(filepath.Join(dest, disk.name))
		if err != nil || len(chain) != disk.depth {
			t.Fatalf("%s: chain %v, %v; want %d backing files", disk.name, chain, err, disk.depth)
		}
		if len(source) != len(chain) {
			t.Fatalf("%s lost a layer: source %v, imported %v", disk.name, source, chain)
		}
		for i, path := range chain {
			if !within(b.cfg.DataDir, path) {
				t.Fatalf("%s retained an exporting-host path %s", disk.name, path)
			}
			want, err := os.ReadFile(source[i])
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if isQcow2(source[i]) {
				want, got = want[qcow2ClusterSize:], got[qcow2ClusterSize:]
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("%s layer %d changed disk data", disk.name, i)
			}
		}
	}
}

func TestArchiveRejectsExcessiveQcow2Depth(t *testing.T) {
	am := archiveManifest{
		Format: archiveFormat, Arch: hostSnapshotArch(),
		Snapshot: krucibleSnapManifest{Type: "filesystem", DiskFile: "rootfs.qcow2", DiskFormat: "qcow2", Vcpus: 1, MemMiB: 64},
	}
	var entries []archiveEntry
	over := ""
	for i := 0; i <= maxChainDepth; i++ {
		name := "rootfs.qcow2"
		if i != 0 {
			name = fmt.Sprintf("layer-%d.img", i)
			am.Layers = append(am.Layers, archiveLayer{Name: name, Format: "qcow2", Over: over})
		}
		data := overlayArchiveBytes(t, "/source-host/disk.img", "qcow2")
		if i == maxChainDepth {
			clear(data[8:20])
		}
		am.Files = append(am.Files, archiveFile{Name: name, Size: int64(len(data))})
		entries = append(entries, archiveEntry{name, data})
		over = name
	}
	b := portableEngine(t, "", "exit 0")
	dest, _, err := importArchive(b, craftArchive(t, am, entries))
	if !errors.Is(err, engine.ErrBadSnapshotArchive) {
		t.Fatalf("nine qcow2 images were admitted under the eight-image cap: %v", err)
	}
	mustNotExist(t, dest)
	if nodes := nodeEntries(t, b.cfg.DataDir); len(nodes) != 0 {
		t.Fatalf("over-depth archive published nodes: %v", nodes)
	}
}

func TestLayeredImportRollsBackWhenBasePublicationFails(t *testing.T) {
	f, dir, m := layeredSnapshot(t)
	b := portableEngine(t, "", "exit 0")
	sum := fileSum(t, f.base)
	stem := keptBaseStem.FindStringSubmatch(filepath.Base(f.base))[1]
	squatter := filepath.Join(basesDir(b.cfg.DataDir), baseName(stem, sum))
	original := writeBase(t, squatter, 17)
	dest, _, err := importArchive(b, exportArchive(t, f.e, dir, m, true))
	if err == nil || !strings.Contains(err.Error(), "already holds another image") {
		t.Fatalf("base collision wasn't refused: %v", err)
	}
	mustNotExist(t, dest)
	if nodes := nodeEntries(t, b.cfg.DataDir); len(nodes) != 0 {
		t.Fatalf("base publication failure left nodes: %v", nodes)
	}
	got, err := os.ReadFile(squatter)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("a failed import replaced an existing base: %v", err)
	}
}

func TestArchivePreservesExplicitRawVolumeWithQcow2Magic(t *testing.T) {
	f, dir, m := layeredSnapshot(t)
	volume := filepath.Join(dir, "vol0.img")
	writeFile(t, volume, overlayArchiveBytes(t, filepath.Join(f.data, "private.img"), "raw"))
	m.Volumes[0].Format = "raw"
	want := fileSum(t, volume)
	archive := exportArchive(t, f.e, dir, m, true)
	am, err := readArchiveManifestFrom(archive)
	if err != nil {
		t.Fatal(err)
	}
	if len(am.Layers) != 1 {
		t.Fatalf("raw volume bytes became archive dependencies: %+v", am.Layers)
	}
	b := portableEngine(t, "", "exit 0")
	dest, imp, err := importArchive(b, archive)
	if err != nil {
		t.Fatal(err)
	}
	var imported krucibleSnapManifest
	if err := json.Unmarshal(imp.ManifestJSON, &imported); err != nil {
		t.Fatal(err)
	}
	if len(imported.Volumes) != 1 || imported.Volumes[0].Format != "raw" {
		t.Fatalf("the import lost the source volume's raw intent: %+v", imported.Volumes)
	}
	if fileSum(t, filepath.Join(dest, "vol0.img")) != want {
		t.Fatal("raw guest bytes were repointed as a qcow2 header")
	}
}

func TestArchiveRejectsUnknownVolumeFormat(t *testing.T) {
	f, dir, m := layeredSnapshot(t)
	am, entries := plannedLayeredArchive(t, f.e, dir, m)
	am.Snapshot.Volumes[0].Format = "vmdk"
	b := portableEngine(t, "", "exit 0")
	dest, _, err := importArchive(b, craftArchive(t, am, entries))
	if !errors.Is(err, engine.ErrBadSnapshotArchive) {
		t.Fatalf("unsupported volume dependency format was admitted: %v", err)
	}
	mustNotExist(t, dest)
}

func TestLayeredArchiveAllowsLegacyVolumeFormats(t *testing.T) {
	f, dir, m := layeredSnapshot(t)
	m.Volumes[0].Format = ""
	b := portableEngine(t, "", "exit 0")
	dest, _, err := importArchive(b, exportArchive(t, f.e, dir, m, true))
	if err != nil {
		t.Fatalf("legacy snapshot volume without explicit format: %v", err)
	}
	chain, err := backingChain(filepath.Join(dest, "vol0.img"))
	if err != nil || len(chain) != 1 || !within(nodesDir(b.cfg.DataDir), chain[0]) {
		t.Fatalf("legacy volume's chain wasn't rebuilt: %v, %v", chain, err)
	}
}

func setArchiveBackingPath(t *testing.T, data []byte, path string) {
	t.Helper()
	off := binary.BigEndian.Uint64(data[8:])
	size := binary.BigEndian.Uint32(data[16:])
	if off == 0 || off+uint64(max(int(size), len(path))) >= qcow2ClusterSize {
		t.Fatal("archive fixture has no room for its poisoned backing path")
	}
	clear(data[off : off+uint64(size)])
	copy(data[off:], path)
	binary.BigEndian.PutUint32(data[16:], uint32(len(path)))
}

func TestLayeredImportDiscardsSourceHostBackingPaths(t *testing.T) {
	f, dir, m := layeredSnapshot(t)
	am, entries := plannedLayeredArchive(t, f.e, dir, m)
	poisoned := map[string]string{
		"rootfs.qcow2": "/missing-source-host/private-root.qcow2",
		"layer-1.img":  "../../missing-source-host/private-base.img",
		"vol0.img":     "/missing-source-host/private-volume.img",
	}
	for name, path := range poisoned {
		data := archiveEntryBytes(t, entries, name)
		setArchiveBackingPath(t, data, path)
		replaceLayeredEntry(t, &am, entries, name, data)
	}
	b := portableEngine(t, "", "exit 0")
	hostPath := filepath.Join(b.cfg.DataDir, "private", "not-a-disk.img")
	writeFile(t, hostPath, []byte("an import must not open this host path"))
	am.Snapshot.Backing = []diskLayer{{Path: hostPath, Format: "vmdk"}}
	am.Snapshot.Volumes[0].Backing = []diskLayer{{Path: hostPath, Format: "qcow2"}}
	dest, imp, err := importArchive(b, craftArchive(t, am, entries))
	if err != nil {
		t.Fatalf("source-host paths were followed instead of the archive graph: %v", err)
	}
	var imported krucibleSnapManifest
	if err := json.Unmarshal(imp.ManifestJSON, &imported); err != nil {
		t.Fatal(err)
	}
	if imported.DiskFormat != "qcow2" || len(imported.Backing) != 2 ||
		imported.Backing[0].Format != "qcow2" || imported.Backing[1].Format != "raw" ||
		len(imported.Volumes) != 1 || imported.Volumes[0].Format != "qcow2" ||
		len(imported.Volumes[0].Backing) != 1 || imported.Volumes[0].Backing[0].Format != "raw" {
		t.Fatalf("the rebuilt per-disk metadata lost the graph's formats: %+v", imported)
	}
	for _, layer := range append(imported.Backing, imported.Volumes[0].Backing...) {
		if !within(b.cfg.DataDir, layer.Path) || layer.Path == hostPath {
			t.Fatalf("incoming backing lists became local authority: %+v", layer)
		}
	}
	for _, path := range []string{filepath.Join(dest, imported.DiskFile), imported.Backing[0].Path, filepath.Join(dest, imported.Volumes[0].File)} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, source := range poisoned {
			if bytes.Contains(data, []byte(source)) {
				t.Fatalf("%s retained the incoming host backing path %s", path, source)
			}
		}
	}
	local, found, err := readSnapshotManifest(dest)
	if err != nil || !found {
		t.Fatalf("successful import did not publish a daemon-owned manifest: %v, %v", found, err)
	}
	localJSON, err := json.Marshal(local)
	if err != nil || !bytes.Equal(localJSON, imp.ManifestJSON) {
		t.Fatalf("the local manifest differs from the imported metadata: %s, %v", localJSON, err)
	}
}

func TestSnapshotExportUsesLocalDiskMetadata(t *testing.T) {
	f, dir, m := layeredSnapshot(t)
	if err := writeSnapshotManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	sqlite := m
	sqlite.DiskFile, sqlite.DiskFormat = "../private.img", "raw"
	sqlite.Backing = []diskLayer{{Path: "/not-authorized-by-sqlite", Format: "raw"}}
	sqlite.Volumes = []snapVolume{{File: "../private-volume.img", Format: "raw"}}
	raw, err := json.Marshal(sqlite)
	if err != nil {
		t.Fatal(err)
	}
	am, paths, err := f.e.planExport(dir, raw, engine.SnapshotExport{IncludeBase: true})
	if err != nil {
		t.Fatalf("SQLite overrode the local manifest: %v", err)
	}
	if am.Snapshot.DiskFile != m.DiskFile || am.Snapshot.DiskFormat != "qcow2" ||
		len(am.Layers) != 2 || am.Layers[0].Format != "qcow2" || am.Layers[1].Format != "raw" ||
		len(am.Snapshot.Backing) != 0 || len(am.Snapshot.Volumes[0].Backing) != 0 {
		t.Fatalf("export did not use and sanitize the local per-disk metadata: %+v", am)
	}
	for _, path := range paths {
		if strings.Contains(path, "private") || strings.Contains(path, "not-authorized") {
			t.Fatalf("SQLite added an archive source: %s", path)
		}
	}
}

func TestTrustedArchivePreservesRawDisksWithQcow2Magic(t *testing.T) {
	f := newNodeFixture(t)
	dir := filepath.Join(f.data, "snapshots", "u1", "raw")
	hostPath := filepath.Join(f.data, "private", "never-open.img")
	payload := overlayArchiveBytes(t, hostPath, "raw")
	binary.BigEndian.PutUint64(payload[72:], 1<<2)
	for _, name := range []string{"root.img", "vol0.img"} {
		writeFile(t, filepath.Join(dir, name), payload)
	}
	writeFile(t, filepath.Join(dir, checkpointFile), []byte("KRUNCKPT checkpoint"))
	writeFile(t, filepath.Join(dir, memoryFile), make([]byte, 8192))
	m := krucibleSnapManifest{
		Arch: hostSnapshotArch(), Type: "memory", Vcpus: 1, MemMiB: 64,
		DiskFile: "root.img", DiskFormat: "raw",
		Volumes: []snapVolume{{File: "vol0.img", Format: "raw"}},
	}
	if err := writeSnapshotManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	archive := exportArchive(t, f.e, dir, m, true)
	am, err := readArchiveManifestFrom(archive)
	if err != nil {
		t.Fatal(err)
	}
	if am.Snapshot.DiskFormat != "raw" || am.Base != nil || len(am.Layers) != 0 {
		t.Fatalf("raw payload bytes became archive metadata: %+v", am)
	}
	b := portableEngine(t, "", "exit 0")
	dest, imp, err := importArchive(b, archive)
	if err != nil {
		t.Fatalf("raw disk payload was parsed as a qcow2 header: %v", err)
	}
	var imported krucibleSnapManifest
	if err := json.Unmarshal(imp.ManifestJSON, &imported); err != nil {
		t.Fatal(err)
	}
	if imported.DiskFormat != "raw" || len(imported.Backing) != 0 ||
		imported.Volumes[0].Format != "raw" || len(imported.Volumes[0].Backing) != 0 {
		t.Fatalf("raw disks gained backing authority: %+v", imported)
	}
	for _, name := range []string{"root.img", "vol0.img"} {
		got, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("%s's raw guest bytes were rewritten: %v", name, err)
		}
	}
}

func TestLegacyExportRejectsSQLiteBackingAuthority(t *testing.T) {
	f, dir, m := layeredSnapshot(t)
	foreignSnapshot := f.snapshot("u2", "private", f.freshRoot("foreign"))
	foreign, err := f.e.nodeLink(filepath.Join(foreignSnapshot, "rootfs.qcow2"))
	if err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(f.data, "private", "unreadable-tail.img")
	if err := setQcow2Backing(foreign, secret); err != nil {
		t.Fatal(err)
	}
	if err := setQcow2Backing(filepath.Join(dir, m.DiskFile), foreign); err != nil {
		t.Fatal(err)
	}
	m.Type, m.DiskFormat, m.Volumes = "filesystem", "", nil
	m.Backing = []diskLayer{{Path: foreign, Format: "qcow2"}, {Path: secret, Format: "raw"}}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	err = f.e.ExportSnapshot(context.Background(), &archive, dir, raw, engine.SnapshotExport{IncludeBase: true})
	if err == nil || archive.Len() != 0 || !strings.Contains(err.Error(), foreign) || strings.Contains(err.Error(), secret) {
		t.Fatalf("legacy export did not authorize the next layer before opening it: %v, %d bytes", err, archive.Len())
	}
}

func TestArchiveRequiresExplicitDiskAndLayerFormats(t *testing.T) {
	f, dir, m := layeredSnapshot(t)
	cases := map[string]func(*archiveManifest){
		"root missing":   func(am *archiveManifest) { am.Snapshot.DiskFormat = "" },
		"root unknown":   func(am *archiveManifest) { am.Snapshot.DiskFormat = "vmdk" },
		"volume missing": func(am *archiveManifest) { am.Snapshot.Volumes[0].Format = "" },
		"layer missing":  func(am *archiveManifest) { am.Layers[0].Format = "" },
		"layer unknown":  func(am *archiveManifest) { am.Layers[0].Format = "vmdk" },
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			am, entries := plannedLayeredArchive(t, f.e, dir, m)
			damage(&am)
			b := portableEngine(t, "", "exit 0")
			dest, _, err := importArchive(b, craftArchive(t, am, entries))
			if !errors.Is(err, engine.ErrBadSnapshotArchive) {
				t.Fatalf("import admitted an untyped archive dependency: %v", err)
			}
			mustNotExist(t, dest)
		})
	}
}

func TestLegacyArchiveUsesParentFormatsWithoutProbingRawLayers(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			f, dir, m := layeredSnapshot(t)
			am, entries := plannedLayeredArchive(t, f.e, dir, m)
			am.Format, am.Snapshot.DiskFormat, am.Snapshot.Volumes[0].Format = version, "", ""
			for i := range am.Layers {
				am.Layers[i].Format = ""
			}
			raw := overlayArchiveBytes(t, "/private-host-file.img", "raw")
			binary.BigEndian.PutUint64(raw[72:], 1<<2)
			replaceLayeredEntry(t, &am, entries, "layer-2.img", raw)
			b := portableEngine(t, "", "exit 0")
			dest, imp, err := importArchive(b, craftArchive(t, am, entries))
			if err != nil {
				t.Fatalf("legacy parent formats were not preserved: %v", err)
			}
			var imported krucibleSnapManifest
			if err := json.Unmarshal(imp.ManifestJSON, &imported); err != nil {
				t.Fatal(err)
			}
			volume := imported.Volumes[0]
			if imported.DiskFormat != "qcow2" || volume.Format != "qcow2" ||
				len(volume.Backing) != 1 || volume.Backing[0].Format != "raw" {
				t.Fatalf("legacy formats were not made explicit: %+v", imported)
			}
			got, err := os.ReadFile(volume.Backing[0].Path)
			if err != nil || !bytes.Equal(got, raw) {
				t.Fatalf("raw legacy layer was parsed or rewritten: %v", err)
			}
			local, found, err := readSnapshotManifest(dest)
			if err != nil || !found || local.Volumes[0].Backing[0].Format != "raw" {
				t.Fatalf("legacy import did not publish explicit local formats: %+v, %v", local, err)
			}
		})
	}
}

func TestLegacyArchiveRejectsAmbiguousLayerFormat(t *testing.T) {
	f, dir, m := layeredSnapshot(t)
	am, entries := plannedLayeredArchive(t, f.e, dir, m)
	am.Format, am.Snapshot.DiskFormat = 2, ""
	for i := range am.Layers {
		am.Layers[i].Format = ""
	}
	root := archiveEntryBytes(t, entries, "rootfs.qcow2")
	binary.BigEndian.PutUint32(root[qcow2HeaderLen:], 0)
	replaceLayeredEntry(t, &am, entries, "rootfs.qcow2", root)
	b := portableEngine(t, "", "exit 0")
	dest, _, err := importArchive(b, craftArchive(t, am, entries))
	if !errors.Is(err, engine.ErrBadSnapshotArchive) || !strings.Contains(err.Error(), "no explicit backing format") {
		t.Fatalf("an ambiguous legacy layer was inferred from its payload: %v", err)
	}
	mustNotExist(t, dest)
}

func TestTrustedExportRejectsUnrecordedBacking(t *testing.T) {
	for _, disk := range []string{"root", "volume"} {
		t.Run(disk, func(t *testing.T) {
			f, dir, m := layeredSnapshot(t)
			if disk == "root" {
				m.Backing = nil
			} else {
				m.Volumes[0].Backing = nil
			}
			if err := writeSnapshotManifest(dir, m); err != nil {
				t.Fatal(err)
			}
			var archive bytes.Buffer
			err := f.e.ExportSnapshot(context.Background(), &archive, dir, nil, engine.SnapshotExport{IncludeBase: true})
			if err == nil || archive.Len() != 0 {
				t.Fatalf("explicit local format fell back to header-derived backing: %v, %d bytes", err, archive.Len())
			}
		})
	}
}

func TestLayeredImportRebuildsLinksFromGraph(t *testing.T) {
	cases := []string{"missing root link", "missing layer link", "missing volume link", "unlisted terminal host path", "unlisted volume backing"}
	for _, damage := range cases {
		t.Run(damage, func(t *testing.T) {
			f, dir, m := layeredSnapshot(t)
			am, entries := plannedLayeredArchive(t, f.e, dir, m)
			rootDepth, volumeDepth := 2, 1
			switch damage {
			case "missing root link", "missing layer link", "missing volume link":
				name := map[string]string{
					"missing root link": "rootfs.qcow2", "missing layer link": "layer-1.img", "missing volume link": "vol0.img",
				}[damage]
				data := archiveEntryBytes(t, entries, name)
				clear(data[8:20])
				replaceLayeredEntry(t, &am, entries, name, data)
			case "unlisted terminal host path":
				am.Base = nil
				am.Files, entries = am.Files[:len(am.Files)-1], entries[:len(entries)-1]
				rootDepth = 1
				data := archiveEntryBytes(t, entries, "layer-1.img")
				setArchiveBackingPath(t, data, "/source-host/private-terminal.img")
				replaceLayeredEntry(t, &am, entries, "layer-1.img", data)
			case "unlisted volume backing":
				am.Layers = am.Layers[:1]
				for i, entry := range entries {
					if entry.name == "layer-2.img" {
						am.Files = append(am.Files[:i], am.Files[i+1:]...)
						entries = append(entries[:i], entries[i+1:]...)
						break
					}
				}
				volumeDepth = 0
			}
			b := portableEngine(t, "", "exit 0")
			dest, imp, err := importArchive(b, craftArchive(t, am, entries))
			if err != nil {
				t.Fatalf("incoming header references overrode the archive graph: %v", err)
			}
			var imported krucibleSnapManifest
			if err := json.Unmarshal(imp.ManifestJSON, &imported); err != nil {
				t.Fatal(err)
			}
			if len(imported.Backing) != rootDepth || len(imported.Volumes[0].Backing) != volumeDepth {
				t.Fatalf("import did not rebuild exactly the declared chains: %+v", imported)
			}
			if damage == "unlisted terminal host path" {
				terminal := imported.Backing[0].Path
				backing, err := qcow2Backing(terminal)
				if err != nil || backing != "" {
					t.Fatalf("terminal qcow2 layer kept a host reference: %s, %v", backing, err)
				}
				data, err := os.ReadFile(terminal)
				if err != nil || bytes.Contains(data, []byte("/source-host/private-terminal.img")) {
					t.Fatalf("terminal source-host backing path was not erased: %v", err)
				}
			}
			if damage == "unlisted volume backing" {
				backing, err := qcow2Backing(filepath.Join(dest, imported.Volumes[0].File))
				if err != nil || backing != "" {
					t.Fatalf("standalone qcow2 volume kept a host reference: %s, %v", backing, err)
				}
			}
		})
	}
}

func TestArchiveRebuildsMissingHeaderFormatsFromMetadata(t *testing.T) {
	f, dir, m := layeredSnapshot(t)
	am, entries := plannedLayeredArchive(t, f.e, dir, m)
	for _, name := range []string{"rootfs.qcow2", "layer-1.img", "vol0.img"} {
		data := archiveEntryBytes(t, entries, name)
		binary.BigEndian.PutUint32(data[qcow2HeaderLen:], 0)
		replaceLayeredEntry(t, &am, entries, name, data)
	}
	raw := overlayArchiveBytes(t, "/private-host-file.img", "raw")
	binary.BigEndian.PutUint64(raw[72:], 1<<2)
	replaceLayeredEntry(t, &am, entries, "layer-2.img", raw)
	b := portableEngine(t, "", "exit 0")
	dest, imp, err := importArchive(b, craftArchive(t, am, entries))
	if err != nil {
		t.Fatalf("explicit graph formats were not rebuilt: %v", err)
	}
	var imported krucibleSnapManifest
	if err := json.Unmarshal(imp.ManifestJSON, &imported); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct{ path, format string }{
		{filepath.Join(dest, imported.DiskFile), "qcow2"},
		{imported.Backing[0].Path, "raw"},
		{filepath.Join(dest, imported.Volumes[0].File), "raw"},
	} {
		if got, err := qcow2BackingFormat(check.path); err != nil || got != check.format {
			t.Fatalf("%s's rebuilt backing format is %q, %v; want %s", check.path, got, err, check.format)
		}
	}
	got, err := os.ReadFile(imported.Volumes[0].Backing[0].Path)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("the raw layer was probed or rewritten: %v", err)
	}
}

func TestLegacyExportRebuildsMissingHeaderFormats(t *testing.T) {
	f, dir, m := layeredSnapshot(t)
	for _, path := range []string{filepath.Join(dir, m.DiskFile), m.Backing[0].Path, filepath.Join(dir, m.Volumes[0].File)} {
		f.write(path, qcow2HeaderLen, make([]byte, 8))
	}
	b := portableEngine(t, "", "exit 0")
	dest, imp, err := importArchive(b, exportArchive(t, f.e, dir, m, true))
	if err != nil {
		t.Fatalf("authorized legacy inference produced an unimportable archive: %v", err)
	}
	var imported krucibleSnapManifest
	if err := json.Unmarshal(imp.ManifestJSON, &imported); err != nil {
		t.Fatal(err)
	}
	if imported.DiskFormat != "qcow2" || len(imported.Backing) != 2 ||
		imported.Backing[0].Format != "qcow2" || imported.Backing[1].Format != "raw" ||
		len(imported.Volumes[0].Backing) != 1 || imported.Volumes[0].Backing[0].Format != "raw" {
		t.Fatalf("legacy formats were not recorded in the rebuilt graph: %+v", imported)
	}
	if got, err := qcow2BackingFormat(filepath.Join(dest, imported.DiskFile)); err != nil || got != "qcow2" {
		t.Fatalf("the imported root still requires format probing: %q, %v", got, err)
	}
}

func TestLegacyArchiveDefaultsStandaloneVolumesToRaw(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			f, dir, m := layeredSnapshot(t)
			am, entries := plannedLayeredArchive(t, f.e, dir, m)
			am.Format, am.Snapshot.DiskFormat, am.Snapshot.Volumes[0].Format = version, "", ""
			am.Layers = am.Layers[:1]
			am.Layers[0].Format = ""
			for i, entry := range entries {
				if entry.name == "layer-2.img" {
					am.Files = append(am.Files[:i], am.Files[i+1:]...)
					entries = append(entries[:i], entries[i+1:]...)
					break
				}
			}
			raw := overlayArchiveBytes(t, "/private-host-file.img", "raw")
			binary.BigEndian.PutUint64(raw[72:], 1<<2)
			replaceLayeredEntry(t, &am, entries, "vol0.img", raw)
			b := portableEngine(t, "", "exit 0")
			dest, imp, err := importArchive(b, craftArchive(t, am, entries))
			if err != nil {
				t.Fatalf("legacy raw-volume default was replaced by payload probing: %v", err)
			}
			var imported krucibleSnapManifest
			if err := json.Unmarshal(imp.ManifestJSON, &imported); err != nil {
				t.Fatal(err)
			}
			if imported.Volumes[0].Format != "raw" || len(imported.Volumes[0].Backing) != 0 {
				t.Fatalf("legacy standalone volume gained backing authority: %+v", imported.Volumes[0])
			}
			got, err := os.ReadFile(filepath.Join(dest, imported.Volumes[0].File))
			if err != nil || !bytes.Equal(got, raw) {
				t.Fatalf("legacy raw guest bytes changed: %v", err)
			}
		})
	}
}

func TestLayeredSnapshotImportsWithoutHardlinks(t *testing.T) {
	for _, linkErr := range []error{syscall.EXDEV, syscall.EOPNOTSUPP} {
		t.Run(linkErr.Error(), func(t *testing.T) {
			f, dir, m := layeredSnapshot(t)
			wantRoot, err := os.ReadFile(backingOf(t, filepath.Join(dir, m.DiskFile)))
			if err != nil {
				t.Fatal(err)
			}
			wantVolume := fileSum(t, backingOf(t, filepath.Join(dir, "vol0.img")))
			archive := exportArchive(t, f.e, dir, m, true)
			am, err := readArchiveManifestFrom(archive)
			if err != nil {
				t.Fatal(err)
			}
			b := portableEngine(t, "", "exit 0")
			base := b.keptBasePath(am.Base.SHA256, am.Base.Name)
			if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := cloneFile(f.base, base); err != nil {
				t.Fatal(err)
			}
			oldLink := linkNode
			t.Cleanup(func() { linkNode = oldLink })
			links := 0
			linkNode = func(src, dst string) error {
				links++
				return &os.LinkError{Op: "link", Old: src, New: dst, Err: linkErr}
			}
			var roots, volumes, snapshots []string
			for _, name := range []string{"first", "second"} {
				dest := filepath.Join(b.cfg.DataDir, "snapshots", "u1", name)
				if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
					t.Fatal(err)
				}
				if _, err := b.ImportSnapshot(context.Background(), bytes.NewReader(archive), dest); err != nil {
					t.Fatal(err)
				}
				local, found, err := readSnapshotManifest(dest)
				if err != nil || !found || len(local.Backing) != 2 || len(local.Volumes) != 1 || len(local.Volumes[0].Backing) != 1 {
					t.Fatalf("no-link import lost typed chains: %+v, %v", local, err)
				}
				if local.Backing[0].Format != "qcow2" || local.Backing[1].Format != "raw" || local.Backing[1].Path != base ||
					local.Volumes[0].Backing[0].Format != "raw" {
					t.Fatalf("no-link import changed dependency drivers: %+v", local)
				}
				root, volume := local.Backing[0].Path, local.Volumes[0].Backing[0].Path
				if filepath.Dir(root) != nodesDir(b.cfg.DataDir) || filepath.Dir(volume) != nodesDir(b.cfg.DataDir) {
					t.Fatal("import retained source-host nodes")
				}
				gotRoot, err := os.ReadFile(root)
				if err != nil || len(gotRoot) != len(wantRoot) ||
					!bytes.Equal(gotRoot[qcow2ClusterSize:], wantRoot[qcow2ClusterSize:]) ||
					fileSum(t, volume) != wantVolume {
					t.Fatalf("copy adoption changed disk bytes: %v", err)
				}
				roots, volumes, snapshots = append(roots, root), append(volumes, volume), append(snapshots, dest)
			}
			if links != 4 || roots[0] == roots[1] || volumes[0] == volumes[1] {
				t.Fatalf("no-link imports reused nodes or retried hard links: links=%d roots=%v volumes=%v", links, roots, volumes)
			}
			if err := b.RemoveSnapshot(context.Background(), snapshots[0]); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{roots[0], volumes[0], nodeMetadataPath(roots[0]), nodeMetadataPath(volumes[0])} {
				mustNotExist(t, path)
			}
			for _, path := range []string{roots[1], volumes[1], base} {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("first import cleanup broke second: %s: %v", path, err)
				}
			}
			if err := b.RemoveSnapshot(context.Background(), snapshots[1]); err != nil {
				t.Fatal(err)
			}
			if entries := nodeStoreEntries(t, b.cfg.DataDir); len(entries) != 0 {
				t.Fatalf("no-link imports left stale nodes/metadata: %v", entries)
			}
		})
	}
}
