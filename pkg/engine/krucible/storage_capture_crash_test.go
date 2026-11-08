package krucible

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestInterruptedLegacyCaptureDoesNotBlockSweepOrDeletion(t *testing.T) {
	const childEnv = "BHATTI_TEST_CAPTURE_CRASH_DATA"
	if data := os.Getenv(childEnv); data != "" {
		e := &Engine{cfg: Config{DataDir: data, BaseImage: os.Getenv("BHATTI_TEST_CAPTURE_BASE"),
			VMMBinary: os.Args[0], SocketDir: shortSockDir(t)}, caps: VMMCapabilities{Pause: true, Checkpoint: true},
			vms: map[string]*VM{}, netds: map[string]*netdInstance{}}
		if err := e.recover(); err != nil {
			t.Fatal(err)
		}
		vm, err := e.getVM("legacy")
		if err != nil {
			t.Fatal(err)
		}
		f := &legacyVMRecordFixture{e: e, vm: vm}
		serveLegacyVMCapture(t, f, nil, func(op string) error {
			if op == "RESUME" {
				os.Exit(94)
			}
			return nil
		})
		_, err = e.CheckpointTyped(t.Context(), vm.ID, "u1", 0, "interrupted", filepath.Join(data, "snapshots", "u1"), "filesystem")
		t.Fatalf("capture did not stop after normalization and before manifest publication: %v", err)
	}
	f := newLegacyVMRecordFixture(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestInterruptedLegacyCaptureDoesNotBlockSweepOrDeletion$")
	cmd.Env = append(os.Environ(), childEnv+"="+f.e.cfg.DataDir, "BHATTI_TEST_CAPTURE_BASE="+f.e.cfg.BaseImage)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 94 {
		t.Fatalf("capture crash boundary = %v: %s", err, out)
	}
	dir := filepath.Join(f.e.cfg.DataDir, "snapshots", "u1", "interrupted")
	root := filepath.Join(dir, "rootfs.qcow2")
	if _, found, err := readSnapshotManifest(dir); err != nil || found {
		t.Fatalf("crash did not leave a manifestless capture: found %t, %v", found, err)
	}
	chain, err := readDiskChain(root, "qcow2", nil)
	if err != nil || len(chain) == 0 || filepath.Dir(chain[0].Path) != nodesDir(f.e.cfg.DataDir) {
		t.Fatalf("crash missed adopted private ancestors: %+v, %v", chain, err)
	}
	manifest, err := json.Marshal(krucibleSnapManifest{Type: "filesystem", DiskFile: "rootfs.qcow2", DiskFormat: "qcow2"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.ResumeFromManifestJSON(t.Context(), dir, manifest, "refused", "u1"); err == nil {
		t.Fatal("manifestless capture authorized its own normalized ancestors")
	}
	refs, err := diskRefs(f.e.cfg.DataDir)
	if err != nil {
		t.Fatalf("interrupted capture blocked dependency discovery: %v", err)
	}
	for _, layer := range chain {
		if filepath.Dir(layer.Path) != nodesDir(f.e.cfg.DataDir) {
			continue
		}
		fi, err := os.Stat(layer.Path)
		if err != nil {
			t.Fatal(err)
		}
		key, _ := keyOf(fi)
		if len(refs[key]) != 0 {
			t.Fatalf("unresumable capture pinned private ancestor %s: %v", layer.Path, refs[key])
		}
	}
	other := filepath.Join(f.e.cfg.DataDir, "snapshots", "u1", "other")
	writeFile(t, filepath.Join(other, "rootfs.img"), []byte("raw disk"))
	if err := writeSnapshotManifest(other, krucibleSnapManifest{DiskFile: "rootfs.img", DiskFormat: "raw", Backing: []diskLayer{}}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.RemoveSnapshot(t.Context(), other); err != nil {
		t.Fatalf("interrupted capture blocked unrelated snapshot deletion: %v", err)
	}
	if _, err := SweepNodes(f.e.cfg.DataDir, false); err != nil {
		t.Fatalf("interrupted capture blocked sweep: %v", err)
	}
	if _, err := os.Stat(chain[0].Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incomplete capture retained its private ancestor: %v", err)
	}
	if err := f.e.RemoveSnapshot(t.Context(), dir); err != nil {
		t.Fatalf("interrupted capture could not be deleted: %v", err)
	}
	f.assertSourcesUnchanged(t)
}
