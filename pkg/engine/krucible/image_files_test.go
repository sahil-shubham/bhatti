package krucible

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

func TestImageDeleteAndRawReplacementRetireSavedMetadata(t *testing.T) {
	f := newNodeFixture(t)
	source := f.freshRoot("source")
	image := filepath.Join(f.data, "images", "u1", "same.ext4")
	if err := os.MkdirAll(filepath.Dir(image), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := cloneFile(source, image); err != nil {
		t.Fatal(err)
	}
	chain, err := readDiskChain(source, "qcow2", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.e.recordNodeMetadata(image, "qcow2", chain); err != nil {
		t.Fatal(err)
	}
	if err := f.e.RemoveImageFiles(context.Background(), image); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{image, nodeMetadataPath(image)} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("delete retained %s: %v", path, err)
		}
	}
	// Simulate a stale sidecar from an older daemon, without an intervening
	// sweep. A raw publisher must replace it, not infer from the new bytes.
	if err := f.e.recordNodeMetadata(image, "qcow2", chain); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(filepath.Dir(image), ".raw-stage.ext4")
	outside := filepath.Join(t.TempDir(), "not-an-authorized-ancestor")
	if err := createQcow2OverlayOn(staging, outside, "raw", uint64(f.size)); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(staging)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.e.PublishRawImage(context.Background(), staging, image); err != nil {
		t.Fatal(err)
	}
	var metadata nodeMetadata
	found, err := readDaemonMetadata(nodeMetadataPath(image), &metadata)
	if err != nil || !found || metadata.Format != "raw" || metadata.Backing == nil || len(metadata.Backing) != 0 {
		t.Fatalf("replacement authority = %+v, found %v, error %v", metadata, found, err)
	}
	dir := filepath.Join(f.data, "sandboxes", "replacement")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	root, format, base, backing, err := f.e.prepareRootDisk(dir, engine.SandboxSpec{BaseImage: image, UserID: "u1"})
	if err != nil {
		t.Fatalf("create from same-name raw replacement: %v", err)
	}
	if format != "qcow2" || base != image || len(backing) != 1 || backing[0] != (diskLayer{Path: image, Format: "raw"}) {
		t.Fatalf("replacement root format %q, base %s, backing %+v", format, base, backing)
	}
	if err := validateRecordedDisk(root, format, backing); err != nil {
		t.Fatal(err)
	}
	if after, err := os.ReadFile(image); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("raw image was interpreted or rewritten: %v", err)
	}
}

func TestSavedImagePublicationMovesDiskAndMetadata(t *testing.T) {
	f := newNodeFixture(t)
	source := f.freshRoot("source")
	staging, image := filepath.Join(f.data, "images", ".save.ext4"), filepath.Join(f.data, "images", "saved.ext4")
	if err := cloneFile(source, staging); err != nil {
		t.Fatal(err)
	}
	chain, err := readDiskChain(staging, "qcow2", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.e.recordNodeMetadata(staging, "qcow2", chain); err != nil {
		t.Fatal(err)
	}
	if err := f.e.PublishSavedImage(context.Background(), staging, image); err != nil {
		t.Fatal(err)
	}
	var metadata nodeMetadata
	if found, err := readDaemonMetadata(nodeMetadataPath(image), &metadata); err != nil || !found {
		t.Fatalf("published metadata missing: %v", err)
	}
	if err := validateRecordedDisk(image, metadata.Format, metadata.Backing); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{staging, nodeMetadataPath(staging)} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("publication retained stage %s: %v", path, err)
		}
	}
}

func TestImageCleanupRetiresMetadataWithoutDisk(t *testing.T) {
	f := newNodeFixture(t)
	image := filepath.Join(f.data, "images", "interrupted.ext4")
	if err := f.e.recordNodeMetadata(image, "raw", []diskLayer{}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := f.e.RemoveImageFiles(context.Background(), image); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Lstat(nodeMetadataPath(image)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cleanup retained metadata: %v", err)
	}
}
