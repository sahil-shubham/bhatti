package krucible

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
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

func nodeAccessLayeredSnapshot(t *testing.T, e *Engine, owner string) (string, *VM) {
	t.Helper()
	ctx := t.Context()
	spec := noNetworkSpec(owner + "-source")
	spec.UserID = owner
	volume := filepath.Join(e.cfg.DataDir, "volumes", owner+".img")
	writeFile(t, volume, bytes.Repeat([]byte("private volume"), 512))
	readOnlyVolume := filepath.Join(e.cfg.DataDir, "volumes", owner+"-readonly.img")
	writeFile(t, readOnlyVolume, bytes.Repeat([]byte("private read-only volume"), 512))
	spec.ResolvedVolumes = []engine.ResolvedVolume{
		{FilePath: volume, Mount: "/data"},
		{FilePath: readOnlyVolume, Mount: "/readonly", ReadOnly: true},
	}
	source, err := e.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(e.cfg.DataDir, "snapshots", owner)
	if _, err := e.Checkpoint(ctx, source.ID, owner, 0, "first", parent); err != nil {
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

func privateSandboxReadPlan(t *testing.T, e *Engine, vm *VM) ([]string, nodeDACCredentials) {
	t.Helper()
	if vm.UserID != "" || len(vm.baseSpec.Backing) == 0 {
		t.Fatalf("fixture lost empty ownership or root chain: %+v", vm.baseSpec)
	}
	gid := requireNodeGID(t, e, vm)
	paths := privateDACChain(vm.baseSpec, e.cfg.BaseImage)
	for _, path := range paths {
		if !within(vm.SandboxDir, path) || within(nodesDir(e.cfg.DataDir), path) {
			t.Fatalf("empty-owner helper retained a non-private ancestor: %s", path)
		}
		assertNodeMode(t, path, gid, 0o640)
	}
	paths = append(paths, vm.baseSpec.RootDisk)
	for _, volume := range vm.baseSpec.Volumes {
		if volume.Format == "qcow2" && len(volume.Backing) == 0 {
			t.Fatalf("fixture lost volume backing: %+v", volume)
		}
		if volume.ReadOnly {
			assertNodeMode(t, volume.Path, gid, 0o640)
		}
		paths = append(paths, volume.Path)
	}
	for _, path := range paths {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Sys().(*syscall.Stat_t).Nlink != 1 {
			t.Fatalf("empty-owner disk is a hard link rather than a private copy: %s", path)
		}
	}
	return paths, nodePolicyCredentials(t, filepath.Join(vm.SandboxDir, "vmm.policy"))
}

func TestAnonymousLayeredRestoreAndForkUsePrivateCopies(t *testing.T) {
	e := rootReseedStorageEngine(t)
	binary := nodeDACTestBinary(t, e.cfg.DataDir)
	type sandbox struct {
		vm    *VM
		read  []string
		creds nodeDACCredentials
	}
	var sandboxes []sandbox
	var sourceNodes []string
	var named *VM
	for _, owner := range []string{"owner-a", "owner-b"} {
		snapshot, parent := nodeAccessLayeredSnapshot(t, e, owner)
		named = parent
		nodes := privateDACChain(parent.baseSpec, e.cfg.BaseImage)
		if len(nodes) < 3 {
			t.Fatalf("fixture lacks named root and volume nodes: %v", nodes)
		}
		sourceNodes = append(sourceNodes, nodes...)
		gid := requireNodeGID(t, e, parent)
		restored, err := e.ResumeFromManifestJSON(t.Context(), snapshot, nil, owner+"-anonymous", "")
		if err != nil {
			t.Fatalf("restore named owner's layered snapshot anonymously: %v", err)
		}
		vm, err := e.getVM(restored.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(vm.baseSpec.Volumes) != 2 || len(vm.baseSpec.Backing) != len(parent.baseSpec.Backing) {
			t.Fatalf("restore lost captured volume: %+v", vm.baseSpec)
		}
		read, creds := privateSandboxReadPlan(t, e, vm)
		if slices.Contains(creds.Groups, gid) {
			t.Fatal("empty-owner restore inherited its named source's node group")
		}
		for _, node := range nodes {
			assertNodeMode(t, node, gid, 0o640)
		}
		sandboxes = append(sandboxes, sandbox{vm, read, creds})
		if owner == "owner-a" {
			first := filepath.Join(filepath.Dir(snapshot), "first")
			raw, err := e.ResumeFromManifestJSON(t.Context(), first, nil, "anonymous-raw-volumes", "")
			if err != nil {
				t.Fatal(err)
			}
			rawVM, err := e.getVM(raw.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(rawVM.baseSpec.Volumes) != 2 || rawVM.baseSpec.Volumes[1].Format != "raw" {
				t.Fatal("fixture lacks a restored raw read-only volume")
			}
			read, creds := privateSandboxReadPlan(t, e, rawVM)
			sandboxes = append(sandboxes, sandbox{rawVM, read, creds})
		}
	}
	if requireNodeGID(t, e, sandboxes[0].vm) == requireNodeGID(t, e, sandboxes[1].vm) {
		t.Fatal("empty-owner restores share a node group")
	}
	child, err := e.Fork(t.Context(), sandboxes[0].vm.ID, "anonymous-fork")
	if err != nil {
		t.Fatalf("fork an empty-owner layered restore: %v", err)
	}
	fork, err := e.getVM(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(fork.baseSpec.Volumes) != 2 {
		t.Fatal("fork lost the restored volume")
	}
	read, creds := privateSandboxReadPlan(t, e, fork)
	sandboxes = append(sandboxes, sandbox{fork, read, creds})
	image := filepath.Join(e.cfg.DataDir, "images", "global.ext4")
	if err := e.SaveImage(t.Context(), named.ID, image); err != nil {
		t.Fatal(err)
	}
	spec := noNetworkSpec("anonymous-from-global-image")
	spec.BaseImage = image
	created, err := e.Create(t.Context(), spec)
	if err != nil {
		t.Fatalf("create anonymously from a global typed layered image: %v", err)
	}
	vm, err := e.getVM(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	read, creds = privateSandboxReadPlan(t, e, vm)
	sandboxes = append(sandboxes, sandbox{vm, read, creds})
	for i, sb := range sandboxes {
		denied := slices.Clone(sourceNodes)
		for j, other := range sandboxes {
			if i != j {
				if requireNodeGID(t, e, sb.vm) == requireNodeGID(t, e, other.vm) {
					t.Fatal("empty-owner restore, fork or create shares a node group")
				}
				denied = append(denied, other.read...)
			}
		}
		runNodeDAC(t, binary, sb.creds, nodeDACPlan{Read: sb.read, Denied: denied})
	}
}

func TestUnprivilegedAnonymousRestoreKeepsNodeLayering(t *testing.T) {
	e := fakeReseedEngine(t, "supported")
	e.caps.Pause = true
	snapshot, parent := nodeAccessLayeredSnapshot(t, e, "owner")
	restored, err := e.ResumeFromManifestJSON(t.Context(), snapshot, nil, "anonymous", "")
	if err != nil {
		t.Fatal(err)
	}
	vm, err := e.getVM(restored.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(vm.baseSpec.Backing) != len(parent.baseSpec.Backing)+1 ||
		len(vm.baseSpec.Volumes) != 2 ||
		len(vm.baseSpec.Volumes[0].Backing) != len(parent.baseSpec.Volumes[0].Backing)+1 {
		t.Fatalf("non-root restore stopped layering: %+v", vm.baseSpec)
	}
	for _, path := range privateDACChain(vm.baseSpec, e.cfg.BaseImage) {
		if !within(nodesDir(e.cfg.DataDir), path) {
			t.Fatalf("non-root restore copied its ancestor: %s", path)
		}
	}
	for i, volume := range vm.baseSpec.Volumes {
		if len(volume.Backing) != len(parent.baseSpec.Volumes[i].Backing)+1 ||
			volume.ReadOnly != parent.baseSpec.Volumes[i].ReadOnly {
			t.Fatalf("non-root volume restore changed: %+v", volume)
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
			wantDepth := 3
			if owner == "" {
				wantDepth = 2
				privateSandboxReadPlan(t, e, vm)
			}
			if len(vm.baseSpec.Backing) < wantDepth || len(vm.baseSpec.Volumes) != 1 {
				t.Fatalf("restore lost imported layered root or volume: %+v", vm.baseSpec)
			}
			for _, path := range chain {
				if within(nodesDir(e.cfg.DataDir), path) {
					assertNodeMode(t, path, gid, 0o640)
				}
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
