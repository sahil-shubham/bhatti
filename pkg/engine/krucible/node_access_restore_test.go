package krucible

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = check-checkpoint ]; then exit 0; fi\nBHATTI_FAKE_RESEED_VMM=1 BHATTI_RESEED_MODE=supported exec %q -test.run=^TestFakeReseedVMMProcess$ \"$1\"\n", binary)
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

func nodeAccessLayeredSnapshot(t *testing.T, e *Engine, sourceID, owner string) (string, *VM) {
	t.Helper()
	ctx := t.Context()
	parent := filepath.Join(e.cfg.DataDir, "snapshots", owner)
	if _, err := e.Checkpoint(ctx, sourceID, owner, 0, "first", parent); err != nil {
		t.Fatal(err)
	}
	layered, err := e.ResumeFromManifestJSON(ctx, filepath.Join(parent, "first"), nil, owner+"-layered", owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Checkpoint(ctx, layered.ID, owner, 0, "second", parent); err != nil {
		t.Fatal(err)
	}
	vm, err := e.getVM(layered.ID)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(parent, "second"), vm
}

func TestRecoveredHistoricalOwnersKeepLaunchAndRestoreNodesIsolated(t *testing.T) {
	e := rootReseedStorageEngine(t)
	type sandbox struct {
		id, owner    string
		vm, restored *VM
		nodes        []string
	}
	sandboxes := []sandbox{{id: "historical-a", owner: "owner-a"}, {id: "historical-b", owner: "owner-b"}}
	for _, sb := range sandboxes {
		writeOwnerlessLegacyVM(t, e, sb.id)
	}
	if _, err := os.Stat(nodesDir(e.cfg.DataDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("historical fixtures already have a node store: %v", err)
	}
	if err := e.recover(); err != nil {
		t.Fatal(err)
	}
	binary := nodeDACTestBinary(t, e.cfg.DataDir)
	for i := range sandboxes {
		sb := &sandboxes[i]
		vm, err := e.getVM(sb.id)
		if err != nil {
			t.Fatal(err)
		}
		sb.vm = vm
		if vm.UserID != "" || vm.baseSpec.Backing != nil || len(vm.baseSpec.Volumes) != 2 ||
			vm.baseSpec.Volumes[0].Backing != nil || vm.baseSpec.Volumes[1].Backing != nil {
			t.Fatalf("recovery did not preserve the ownerless historical wire shape: %+v", vm.baseSpec)
		}
		if err := e.RecoverOwner(sb.id, sb.owner); err != nil {
			t.Fatal(err)
		}
		if rec := readRecord(t, vm.SandboxDir); rec.UserID != sb.owner {
			t.Fatalf("historical owner was not persisted before launch: %q", rec.UserID)
		}
		if err := e.Start(t.Context(), sb.id); err != nil {
			t.Fatalf("launch recovered historical sandbox: %v", err)
		}
		if vm.UserID != sb.owner || vm.Status != "running" || vm.HelperPID == 0 {
			t.Fatalf("historical sandbox did not launch with its repaired owner: %q, %s, %d", vm.UserID, vm.Status, vm.HelperPID)
		}
		snapshot, parent := nodeAccessLayeredSnapshot(t, e, sb.id, sb.owner)
		restored, err := e.ResumeFromManifestJSON(t.Context(), snapshot, nil, sb.owner+"-restored", vm.UserID)
		if err != nil {
			t.Fatalf("restore recovered owner's layered snapshot: %v", err)
		}
		sb.restored, err = e.getVM(restored.ID)
		if err != nil {
			t.Fatal(err)
		}
		spec := sb.restored.baseSpec
		if sb.restored.UserID != sb.owner || sb.restored.Status != "running" || sb.restored.HelperPID == 0 ||
			len(spec.Backing) != len(parent.baseSpec.Backing)+1 || len(spec.Volumes) != 2 {
			t.Fatalf("restore lost the repaired owner or layered disks: %q, %+v", sb.restored.UserID, spec)
		}
		for i, volume := range spec.Volumes {
			if len(volume.Backing) != len(parent.baseSpec.Volumes[i].Backing)+1 || volume.ReadOnly != (i == 1) {
				t.Fatalf("restore lost the historical volume chain or read-only policy: %+v", volume)
			}
		}
		sb.nodes = privateDACChain(spec, e.cfg.BaseImage)
		if len(sb.nodes) < 6 {
			t.Fatalf("fixture lacks multi-generation root and volume nodes: %v", sb.nodes)
		}
		gid := requireNodeGID(t, e, vm)
		for _, path := range sb.nodes {
			if !within(nodesDir(e.cfg.DataDir), path) {
				t.Fatalf("restore copied an ancestor instead of retaining a shared node: %s", path)
			}
			assertNodeMode(t, path, gid, 0o640)
		}
	}
	for i, sb := range sandboxes {
		other := sandboxes[1-i]
		gid, otherGID := requireNodeGID(t, e, sb.vm), requireNodeGID(t, e, other.vm)
		if gid == otherGID {
			t.Fatal("recovered owners share a node group")
		}
		for _, vm := range []*VM{sb.vm, sb.restored} {
			creds := nodePolicyCredentials(t, filepath.Join(vm.SandboxDir, "vmm.policy"))
			if !slices.Contains(creds.Groups, vmmGroup) || !slices.Contains(creds.Groups, gid) ||
				slices.Contains(creds.Groups, otherGID) {
				t.Fatalf("launch/restore policy has incorrect owner groups: %v", creds.Groups)
			}
			read := append(slices.Clone(sb.nodes), vm.baseSpec.RootDisk, e.cfg.BaseImage)
			for _, volume := range vm.baseSpec.Volumes {
				read = append(read, volume.Path)
			}
			runNodeDAC(t, binary, creds, nodeDACPlan{Read: read, Denied: other.nodes})
		}
	}
}

func TestImportedDirectSnapshotMigrationKeepsOwnerUnassigned(t *testing.T) {
	for _, owner := range []string{"named-owner", ""} {
		t.Run("owner="+owner, func(t *testing.T) {
			e := rootReseedStorageEngine(t)
			source, dir, m := layeredSnapshot(t)
			m.NetPolicy = noNetworkSpec("").NetPolicy
			archive := exportArchive(t, source.e, dir, m, true)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			stage := filepath.Join(e.cfg.DataDir, "snapshots", "import.tmp")
			if err := os.MkdirAll(filepath.Dir(stage), 0o700); err != nil {
				t.Fatal(err)
			}
			if _, err := e.ImportSnapshot(ctx, bytes.NewReader(archive), stage); err != nil {
				t.Fatal(err)
			}
			snap := filepath.Join(e.cfg.DataDir, "snapshots", "portable")
			if err := e.PublishSnapshotImport(ctx, stage, snap); err != nil {
				t.Fatal(err)
			}
			// Startup migration must not interpret "portable" as a user ID.
			restarted := &Engine{cfg: e.cfg, dropVMM: true}
			if err := restarted.migrateNodeAccess(); err != nil {
				t.Fatal(err)
			}
			restored, err := e.ResumeFromManifestJSON(ctx, snap, nil, "restored", owner)
			if err != nil {
				t.Fatalf("restore imported direct snapshot after migration: %v", err)
			}
			vm, err := e.getVM(restored.ID)
			if err != nil {
				t.Fatal(err)
			}
			gid := requireNodeGID(t, e, vm)
			chain := privateDACChain(vm.baseSpec, e.cfg.BaseImage)
			if owner == "" && gid != requireNodeGID(t, e, &VM{ID: "another-anonymous"}) {
				t.Fatal("anonymous imported restore did not join shared anonymous storage")
			}
			if len(vm.baseSpec.Backing) < 3 || len(vm.baseSpec.Volumes) != 1 {
				t.Fatalf("restore lost imported layered root or volume: %+v", vm.baseSpec)
			}
			for _, path := range chain {
				// An imported shared base need not be the configured creation base.
				if within(filepath.Join(e.cfg.DataDir, "images", "bases"), path) {
					continue
				}
				if !within(nodesDir(e.cfg.DataDir), path) {
					t.Fatalf("imported restore copied an ancestor instead of layering shared nodes: %s", path)
				}
				assertNodeMode(t, path, gid, 0o640)
			}
		})
	}
}

func TestDirectSnapshotFilesDoNotConferOwnerProvenance(t *testing.T) {
	f, _ := rootNodeAccessFixture(t)
	// A legacy direct snapshot can lack a local manifest. Its disk name still
	// distinguishes it from the snapshots/<user>/<name> owner layout.
	snap := f.snapshot("", "portable", f.freshRoot("source"))
	node, err := f.e.nodeLink(filepath.Join(snap, "rootfs.qcow2"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(snap, snapshotManifestFile)); err != nil {
		t.Fatal(err)
	}
	if err := (&Engine{cfg: f.e.cfg, dropVMM: true}).migrateNodeAccess(); err != nil {
		t.Fatal(err)
	}
	assertNodeMode(t, node, 0, 0o600)
}
