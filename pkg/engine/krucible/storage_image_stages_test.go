package krucible

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func storageStartupConfig(t *testing.T, data, base string) Config {
	t.Helper()
	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		// testing's shared temp parent is outside DataDir, so production
		// confinement correctly refuses to change its DAC for the fixture.
		if err := os.Chmod(filepath.Dir(data), 0o711); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat("/dev/kvm"); errors.Is(err, os.ErrNotExist) {
			f, err := os.OpenFile("/dev/kvm", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o666)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Remove("/dev/kvm") })
			if err := os.Chmod("/dev/kvm", 0o666); err != nil {
				t.Fatal(err)
			}
		}
	}
	binary := filepath.Join(t.TempDir(), "startup-vmm")
	writeFile(t, binary, []byte("#!/bin/sh\nprintf '%s\\n' '{\"pause\":true,\"checkpoint\":true}'\n"))
	if err := os.Chmod(binary, 0o755); err != nil {
		t.Fatal(err)
	}
	return Config{DataDir: data, BaseImage: base, SocketDir: shortSockDir(t),
		VMMBinary: binary, NetdBinary: binary, KernelImage: "test-kernel", BlockRoot: true, Cgroups: "none"}
}

func TestCompletedImageStagePinsNormalizedAncestors(t *testing.T) {
	f := newLegacyVMRecordFixture(t)
	stage := filepath.Join(f.e.cfg.DataDir, "images", "u1", ".image-123.ext4")
	ops := serveLegacyVMCapture(t, f, nil)
	if err := f.e.SaveImage(t.Context(), f.vm.ID, stage); err != nil {
		t.Fatal(err)
	}
	_ = readOperations(ops)
	var metadata nodeMetadata
	if found, err := readDaemonMetadata(nodeMetadataPath(stage), &metadata); err != nil || !found {
		t.Fatalf("completed stage has no trusted authority: %v", err)
	}
	if len(metadata.Backing) == 0 || filepath.Dir(metadata.Backing[0].Path) != nodesDir(f.e.cfg.DataDir) {
		t.Fatalf("capture did not normalize private ancestors: %+v", metadata.Backing)
	}
	unfinished := filepath.Join(filepath.Dir(stage), ".image-456.ext4")
	writeFile(t, unfinished, []byte("incomplete disk header"))
	if _, err := SweepNodes(f.e.cfg.DataDir, false); err != nil {
		t.Fatal(err)
	}
	if err := validateRecordedDisk(stage, metadata.Format, metadata.Backing); err != nil {
		t.Fatalf("runtime sweep reclaimed a completed publisher's ancestors: %v", err)
	}
	if _, err := os.Stat(unfinished); err != nil {
		t.Fatalf("runtime sweep removed a publisher's incomplete stage: %v", err)
	}
}

func TestStartupRemovesAbandonedImageStagesBeforeSweep(t *testing.T) {
	f := newNodeFixture(t)
	source := f.freshRoot("source")
	chain, err := readDiskChain(source, "qcow2", nil)
	if err != nil {
		t.Fatal(err)
	}
	node, err := f.e.linkNodeMetadata(source, "qcow2", chain)
	if err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(f.data, "images", "u1", ".image-123.ext4")
	if err := os.MkdirAll(filepath.Dir(stage), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := createQcow2OverlayOn(stage, node, "qcow2", uint64(f.size)); err != nil {
		t.Fatal(err)
	}
	if err := f.e.recordNodeMetadata(stage, "qcow2", append([]diskLayer{{Path: node, Format: "qcow2"}}, chain...)); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Dir(source)); err != nil {
		t.Fatal(err)
	}
	unfinished := filepath.Join(f.data, "images", ".image-456.ext4")
	writeFile(t, unfinished, []byte("partial image"))
	orphan := filepath.Join(f.data, "images", "u1", ".image-789.ext4")
	if err := f.e.recordNodeMetadata(orphan, "raw", []diskLayer{}); err != nil {
		t.Fatal(err)
	}
	if _, err := New(storageStartupConfig(t, f.data, f.base)); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{stage, nodeMetadataPath(stage), unfinished, nodeMetadataPath(orphan), node} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("startup retained abandoned image artifact %s: %v", path, err)
		}
	}
}
