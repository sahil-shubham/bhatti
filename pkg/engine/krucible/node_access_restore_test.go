package krucible

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// rootReseedStorageEngine exercises production storage DAC preparation without
// a hypervisor. The protocol-speaking helper uses the ordinary launch path.
func rootReseedStorageEngine(t *testing.T) *Engine {
	t.Helper()
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("requires root Linux storage DAC")
	}
	e := fakeReseedEngine(t, "supported")
	e.dropVMM, e.confineVMM, e.caps.Pause = true, true, true
	e.vmmIDs = map[uint32]string{}
	// Go's test executable is beneath a private build directory. Copy it to
	// the daemon-owned fixture rather than opening external build directories.
	executable, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(e.cfg.DataDir, "test-vmm")
	if err := os.WriteFile(binary, executable, 0o755); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\nBHATTI_FAKE_RESEED_VMM=1 BHATTI_RESEED_MODE=supported exec %q -test.run=^TestFakeReseedVMMProcess$ \"$1\"\n", binary)
	if err := os.WriteFile(e.cfg.VMMBinary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// Both directories belong solely to this test; their /tmp ancestor is public.
	if err := os.Chmod(filepath.Dir(e.cfg.DataDir), 0o711); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{e.cfg.DataDir, e.cfg.SocketDir, filepath.Join(e.cfg.DataDir, "sandboxes")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := e.vmmReachable(dir); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func TestAnonymousSnapshotRepeatedRestoreWithNodeDAC(t *testing.T) {
	e := rootReseedStorageEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	source, err := e.Create(ctx, noNetworkSpec("anonymous-source"))
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(e.cfg.DataDir, "snapshots")
	if _, err := e.Checkpoint(ctx, source.ID, "", 0, "reusable", parent); err != nil {
		t.Fatal(err)
	}
	snap := filepath.Join(parent, "reusable")
	var firstGID uint32
	for i := range 2 {
		restored, err := e.ResumeFromManifestJSON(ctx, snap, nil, fmt.Sprintf("anonymous-%d", i), "")
		if err != nil {
			t.Fatalf("anonymous restore %d: %v", i+1, err)
		}
		vm, err := e.getVM(restored.ID)
		if err != nil {
			t.Fatal(err)
		}
		gid := requireNodeGID(t, e, vm)
		if i == 0 {
			firstGID = gid
		} else if gid != firstGID {
			t.Fatalf("restore changed anonymous node group: %d -> %d", firstGID, gid)
		}
		for _, path := range privateDACChain(vm.baseSpec, e.cfg.BaseImage) {
			assertNodeMode(t, path, gid, 0o640)
		}
		if err := e.Destroy(ctx, restored.ID); err != nil {
			t.Fatal(err)
		}
	}
}
