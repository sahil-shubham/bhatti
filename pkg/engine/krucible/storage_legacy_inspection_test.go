package krucible

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLegacySnapshotInspectionFailureStopsReclamation(t *testing.T) {
	f := newNodeFixture(t)
	disk := filepath.Join(f.data, "snapshots", "u1", "legacy", "rootfs.qcow2")
	if err := os.MkdirAll(filepath.Dir(disk), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := createQcow2OverlayOn(disk, f.base, "raw", uint64(f.size)); err != nil {
		t.Fatal(err)
	}
	// A genuine legacy snapshot has no manifest. An unreadable/truncated
	// header is an inspection failure, not evidence that its base is unused.
	if err := os.Truncate(disk, 4); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(f.data, "garbage.img")
	writeFile(t, stage, []byte("unused node"))
	node, err := f.e.adoptNode(t.Context(), stage, "raw", []diskLayer{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(stage); err != nil {
		t.Fatal(err)
	}
	if _, err := SweepNodes(f.data, false); err == nil {
		t.Error("node sweep ignored an unreadable legacy snapshot disk")
	}
	if _, err := GCImages(f.data, nil, false); err == nil {
		t.Error("base GC ignored an unreadable legacy snapshot disk")
	}
	for _, path := range []string{f.base, node} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("inspection failure allowed reclamation of %s: %v", path, err)
		}
	}
}

func TestBackingAuthorizationRefusalIsDistinctFromInspectionFailure(t *testing.T) {
	f := newNodeFixture(t)
	outside := filepath.Join(t.TempDir(), "outside.img")
	writeFile(t, outside, []byte("private"))
	var refused *backingAuthorizationRefusal
	if err := f.e.authorizedBacking(&VM{UserID: "u1"}, outside, nil); !errors.As(err, &refused) {
		t.Fatalf("explicit ownership refusal has no distinct type: %v", err)
	}
	missing := filepath.Join(t.TempDir(), "missing.img")
	if err := f.e.authorizedBacking(&VM{UserID: "u1"}, missing, nil); errors.As(err, &refused) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inspection failure became an ownership refusal: %v", err)
	}
}

func TestLegacySnapshotMissingBaseDoesNotBlockUnrelatedDeletion(t *testing.T) {
	f := newNodeFixture(t)
	dir := filepath.Join(f.data, "snapshots", "u1", "legacy")
	disk := filepath.Join(dir, "rootfs.qcow2")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := createQcow2OverlayOn(disk, f.base, "raw", uint64(f.size)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.base); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(f.data, "snapshots", "u1", "other")
	writeFile(t, filepath.Join(other, "rootfs.img"), []byte("unrelated raw disk"))
	if err := writeSnapshotManifest(other, krucibleSnapManifest{DiskFile: "rootfs.img", DiskFormat: "raw", Backing: []diskLayer{}}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.RemoveSnapshot(t.Context(), other); err != nil {
		t.Fatalf("legacy snapshot with missing base blocked unrelated deletion: %v", err)
	}
	if _, err := os.Stat(other); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unrelated snapshot survived deletion: %v", err)
	}
	if _, err := os.Stat(disk); err != nil {
		t.Fatalf("unrelated deletion removed the broken legacy snapshot: %v", err)
	}
}
