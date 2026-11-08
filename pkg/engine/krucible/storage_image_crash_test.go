package krucible

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestStartupRecoversImageAuthorityAfterDiskRename(t *testing.T) {
	const childEnv = "BHATTI_TEST_IMAGE_RENAME_CRASH_DATA"
	if data := os.Getenv(childEnv); data != "" {
		stage, image := os.Getenv("BHATTI_TEST_IMAGE_STAGE"), os.Getenv("BHATTI_TEST_IMAGE_FINAL")
		renameNode = func(old, new string) error {
			err := os.Rename(old, new)
			if err == nil && old == stage && new == image {
				os.Exit(96)
			}
			return err
		}
		e := &Engine{cfg: Config{DataDir: data}}
		if err := e.PublishSavedImage(t.Context(), stage, image); err != nil {
			t.Fatal(err)
		}
		t.Fatal("publisher did not stop between disk and metadata renames")
	}
	f := newLegacyVMRecordFixture(t)
	stage := filepath.Join(f.e.cfg.DataDir, "images", "u1", ".image-123.ext4")
	image := filepath.Join(filepath.Dir(stage), "saved.ext4")
	ops := serveLegacyVMCapture(t, f, nil)
	if err := f.e.SaveImage(t.Context(), f.vm.ID, stage); err != nil {
		t.Fatal(err)
	}
	_ = readOperations(ops)
	before, err := os.ReadFile(stage)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestStartupRecoversImageAuthorityAfterDiskRename$")
	cmd.Env = append(os.Environ(), childEnv+"="+f.e.cfg.DataDir, "BHATTI_TEST_IMAGE_STAGE="+stage, "BHATTI_TEST_IMAGE_FINAL="+image)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 96 {
		t.Fatalf("image publication crash boundary = %v: %s", err, out)
	}
	if _, err := os.Stat(image); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(nodeMetadataPath(image)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("crash occurred after metadata publication: %v", err)
	}
	if _, err := os.Stat(nodeMetadataPath(stage)); err != nil {
		t.Fatalf("crash lost its staging authority: %v", err)
	}
	if _, err := New(storageStartupConfig(t, f.e.cfg.DataDir, f.e.cfg.BaseImage)); err != nil {
		t.Fatal(err)
	}
	var metadata nodeMetadata
	if found, err := readDaemonMetadata(nodeMetadataPath(image), &metadata); err != nil || !found {
		t.Fatalf("startup discarded an already-published image's authority: %t, %v", found, err)
	}
	if err := validateRecordedDisk(image, metadata.Format, metadata.Backing); err != nil {
		t.Fatalf("startup lost the saved image's normalized ancestors: %v", err)
	}
	if after, err := os.ReadFile(image); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("startup changed published disk bytes: %v", err)
	}
	keys, err := ownedNodeKeys(f.e.cfg.DataDir, "u1")
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(metadata.Backing[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := keyOf(fi)
	if !keys[key] {
		t.Fatal("recovered sidecar did not retain its owner's normalized graph")
	}
	for _, path := range []string{stage, nodeMetadataPath(stage)} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("startup retained image stage %s: %v", path, err)
		}
	}
}

func TestImageMetadataCannotAuthorizeReplacementInode(t *testing.T) {
	f := newNodeFixture(t)
	image, node, _ := f.savedForkImage("u1", "saved")
	nodeInfo, err := os.Stat(node)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := keyOf(nodeInfo)
	replacement := filepath.Join(filepath.Dir(image), ".replacement.ext4")
	writeFile(t, replacement, []byte("new raw inode"))
	if err := os.Rename(replacement, image); err != nil {
		t.Fatal(err)
	}
	keys, err := ownedNodeKeys(f.data, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if keys[key] {
		t.Fatalf("stale image metadata authorized ancestor %s for a replacement inode", node)
	}
	catalog, err := readDiskCatalog(f.data)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(image)
	if err != nil {
		t.Fatal(err)
	}
	replacementKey, _ := keyOf(fi)
	if _, found := catalog.metadata[replacementKey]; found {
		t.Fatal("replacement inherited stale disk format authority")
	}
}

func TestStartupDiscardsImageAuthorityForUnpublishedInode(t *testing.T) {
	for _, kind := range []string{"missing", "different"} {
		t.Run(kind, func(t *testing.T) {
			f := newNodeFixture(t)
			image, node, _ := f.savedForkImage("u1", "saved")
			stage := filepath.Join(filepath.Dir(image), ".image-456.ext4")
			if err := os.Rename(image, stage); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(nodeMetadataPath(image), nodeMetadataPath(stage)); err != nil {
				t.Fatal(err)
			}
			var metadata nodeMetadata
			if found, err := readDaemonMetadata(nodeMetadataPath(stage), &metadata); err != nil || !found {
				t.Fatalf("stage authority missing: %t, %v", found, err)
			}
			metadata.ImagePath = image
			if err := writeDaemonMetadata(nodeMetadataPath(stage), metadata); err != nil {
				t.Fatal(err)
			}
			if kind == "different" {
				writeFile(t, image, []byte("unrelated raw image"))
			}
			if _, err := New(storageStartupConfig(t, f.data, f.base)); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{stage, nodeMetadataPath(stage), nodeMetadataPath(image), node} {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("startup retained unpublished authority or graph %s: %v", path, err)
				}
			}
			if kind == "different" {
				if data, err := os.ReadFile(image); err != nil || string(data) != "unrelated raw image" {
					t.Fatalf("startup changed a different final image inode: %q, %v", data, err)
				}
			}
		})
	}
}
