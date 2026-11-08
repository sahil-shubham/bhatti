package krucible

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Inspect the actual spec and policy emitted by launch, including directory
// grants and hard-link aliases, rather than only looking for a literal path.
func assertSnapshotHelperCannotAccess(t *testing.T, vm *VM, forbidden ...string) VMSpec {
	t.Helper()
	data, err := os.ReadFile(vm.specPath())
	if err != nil {
		t.Fatal(err)
	}
	var spec VMSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	policy, err := os.ReadFile(filepath.Join(vm.SandboxDir, "vmm.policy"))
	if err != nil {
		t.Fatal(err)
	}
	var rules []VMMRule
	for _, record := range strings.Split(string(policy), "\x00") {
		fields := strings.SplitN(record, " ", 3)
		if len(fields) != 3 || (fields[0] != "path" && fields[0] != "mount") {
			continue
		}
		access, err := strconv.ParseUint(fields[1], 16, 64)
		if err != nil {
			t.Fatalf("invalid helper policy record %q: %v", record, err)
		}
		rules = append(rules, VMMRule{Path: fields[2], Access: access})
	}
	if len(rules) == 0 {
		t.Fatal("helper launch emitted no observable filesystem policy")
	}
	accessTo := func(path string) uint64 {
		var access uint64
		for _, rule := range rules {
			if rule.Path == path {
				access |= rule.Access
			}
		}
		return access
	}
	if got := accessTo(spec.RootDisk); got&(LandlockReadFile|LandlockWriteFile) != LandlockReadFile|LandlockWriteFile {
		t.Fatalf("helper cannot read/write its own root disk: access=%x", got)
	}
	backing := slices.Clone(spec.Backing)
	for _, volume := range spec.Volumes {
		backing = append(backing, volume.Backing...)
	}
	for _, layer := range backing {
		if accessTo(layer.Path)&LandlockReadFile == 0 {
			t.Fatalf("helper cannot read its recorded ancestor %s", layer.Path)
		}
	}
	for _, path := range forbidden {
		if bytes.Contains(data, []byte(path)) {
			t.Errorf("helper spec carries forbidden disk authority %s: %s", path, data)
		}
		for _, rule := range rules {
			if rule.Access == 0 {
				continue
			}
			covers := within(rule.Path, path)
			target, targetErr := os.Stat(path)
			if a, err := os.Stat(rule.Path); err == nil && targetErr == nil {
				if os.SameFile(a, target) {
					covers = true
				}
				if a.IsDir() {
					err := filepath.WalkDir(rule.Path, func(candidate string, de os.DirEntry, err error) error {
						if err != nil {
							return err
						}
						if de.Type().IsRegular() {
							fi, err := os.Stat(candidate)
							if err != nil {
								return err
							}
							if os.SameFile(fi, target) {
								covers = true
							}
						}
						return nil
					})
					if err != nil {
						t.Fatalf("inspect helper policy directory %s: %v", rule.Path, err)
					}
				}
			}
			if covers {
				t.Errorf("helper policy grants access %x to forbidden %s through %s", rule.Access, path, rule.Path)
			}
		}
	}
	return spec
}

func assertNoLocalSnapshotManifest(t *testing.T, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		if _, err := os.Lstat(filepath.Join(dir, snapshotManifestFile)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("legacy resume created a local trust manifest in %s: %v", dir, err)
		}
	}
}

func TestLegacySnapshotRefusesUnauthorizedBackingBeforeReading(t *testing.T) {
	for _, disk := range []string{"root", "volume"} {
		t.Run(disk, func(t *testing.T) {
			e := fakeReseedEngine(t, "supported")
			e.confineVMM, e.caps.Pause = true, true
			dir := filepath.Join(e.cfg.DataDir, "snapshots", "u1", "legacy")
			writeFile(t, filepath.Join(dir, checkpointFile), []byte("checkpoint"))
			writeFile(t, filepath.Join(dir, memoryFile), []byte("memory"))
			root := filepath.Join(dir, "rootfs.qcow2")
			if err := createQcow2OverlayOn(root, e.cfg.BaseImage, "raw", 1<<20); err != nil {
				t.Fatal(err)
			}
			m := krucibleSnapManifest{Arch: hostSnapshotArch(), Type: "memory", Vcpus: 1, MemMiB: 64,
				DiskFile: "rootfs.qcow2", DiskFormat: "qcow2", NetPolicy: noNetworkSpec("").NetPolicy,
				Backing: []diskLayer{{Path: e.cfg.BaseImage, Format: "raw"}}}
			tampered := root
			if disk == "volume" {
				tampered = filepath.Join(dir, "vol0.img")
				m.Volumes = []snapVolume{{File: "vol0.img", Format: "qcow2"}}
			}
			// A truncated outside header is a read canary: walking it before
			// authorization returns a parse error instead of the required
			// refusal at the valid source header's first unauthorized edge.
			private := filepath.Join(e.cfg.DataDir, "snapshots", "u2", "private", "rootfs.qcow2")
			canary := []byte("QFI\xfb\x00\x00\x00\x03")
			writeFile(t, private, canary)
			node, err := e.nodeLink(private)
			if err != nil {
				t.Fatal(err)
			}
			if err := createQcow2OverlayOn(tampered, node, "qcow2", 1<<20); err != nil {
				t.Fatal(err)
			}
			forged := []diskLayer{{Path: node, Format: "qcow2"}}
			if disk == "root" {
				m.Backing = forged
			} else {
				m.Volumes[0].Backing = forged
			}
			manifestJSON, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(tampered)
			if err != nil {
				t.Fatal(err)
			}
			copies := observeSnapshotCopies(t)
			_, err = e.ResumeFromManifestJSON(context.Background(), dir, manifestJSON, "rejected", "u1")
			if err == nil || !strings.Contains(err.Error(), node) || !strings.Contains(err.Error(), "authorized") {
				t.Fatalf("legacy %s reached an outside header or trusted SQLite backing before authorization: %v", disk, err)
			}
			if len(e.vms) != 0 {
				t.Fatal("rejected legacy restore registered a sandbox")
			}
			if got, err := os.ReadFile(copies); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected legacy restore copied a disk: %q, %v", got, err)
			}
			assertNoLocalSnapshotManifest(t, dir)
			if got, err := os.ReadFile(tampered); err != nil || !bytes.Equal(got, before) {
				t.Fatalf("legacy refusal changed the source disk: %v", err)
			}
			if got, err := os.ReadFile(node); err != nil || !bytes.Equal(got, canary) {
				t.Fatalf("legacy refusal changed the unauthorized node: %v", err)
			}
		})
	}
}

func TestLegacySnapshotResumesOwnerAncestorsWithoutPromotingManifest(t *testing.T) {
	e := fakeReseedEngine(t, "supported")
	e.confineVMM, e.caps.Pause = true, true
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	spec := noNetworkSpec("source")
	spec.UserID = "u1"
	source, err := e.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(e.cfg.DataDir, "snapshots", "u1")
	first, err := e.Checkpoint(ctx, source.ID, "u1", 0, "ancestor", parent)
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	ancestor := filepath.Join(parent, "ancestor")
	clone, err := e.ResumeFromManifestJSON(ctx, ancestor, firstJSON, "intermediate", "u1")
	if err != nil {
		t.Fatal(err)
	}
	captured, err := e.Checkpoint(ctx, clone.ID, "u1", 0, "legacy", parent)
	if err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(parent, "legacy")
	legacyDisk := filepath.Join(legacy, "rootfs.qcow2")
	ancestorNode := backingOf(t, legacyDisk)
	if err := e.Destroy(ctx, clone.ID); err != nil {
		t.Fatal(err)
	}
	e.waitNodeSweep()
	for _, dir := range []string{ancestor, legacy} {
		if err := os.Remove(filepath.Join(dir, snapshotManifestFile)); err != nil {
			t.Fatal(err)
		}
	}
	assertNoLocalSnapshotManifest(t, ancestor, legacy)
	before, err := os.ReadFile(legacyDisk)
	if err != nil {
		t.Fatal(err)
	}
	ancestorBefore, err := os.ReadFile(ancestorNode)
	if err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(e.cfg.DataDir, "private", "sqlite-only.img")
	writeFile(t, secret, []byte("unrelated host data"))
	m := captured.(krucibleSnapManifest)
	m.Backing = []diskLayer{{Path: secret, Format: "raw"}}
	manifestJSON, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := e.ResumeFromManifestJSON(ctx, legacy, manifestJSON, "owner-restored", "u1")
	if err != nil {
		t.Fatalf("owner-only legacy ancestry no longer resumes: %v", err)
	}
	vm, err := e.getVM(restored.ID)
	if err != nil {
		t.Fatal(err)
	}
	helper := assertSnapshotHelperCannotAccess(t, vm, secret,
		filepath.Join(ancestor, snapshotManifestFile), filepath.Join(legacy, snapshotManifestFile))
	if !slices.Contains(helper.Backing, diskLayer{Path: ancestorNode, Format: "qcow2"}) {
		t.Fatalf("restore did not retain its owner's authorized ancestor: %+v", helper.Backing)
	}
	if result, err := e.Exec(ctx, restored.ID, []string{"sync"}); err != nil || result.ExitCode != 0 {
		t.Fatalf("owner-only legacy restore is not serving: %+v, %v", result, err)
	}
	assertNoLocalSnapshotManifest(t, ancestor, legacy)
	if after, err := os.ReadFile(legacyDisk); err != nil || !bytes.Equal(after, before) {
		t.Fatalf("legacy resume changed its frozen disk: %v", err)
	}
	if after, err := os.ReadFile(ancestorNode); err != nil || !bytes.Equal(after, ancestorBefore) {
		t.Fatalf("legacy resume changed its owner's ancestor: %v", err)
	}
}

func TestResumeLocalManifestCannotBeExpandedBySQLite(t *testing.T) {
	for _, disk := range []string{"root", "volume"} {
		t.Run(disk, func(t *testing.T) {
			e := fakeReseedEngine(t, "supported")
			e.confineVMM, e.caps.Pause = true, true
			dir := filepath.Join(e.cfg.DataDir, "snapshots", "u1", "local")
			writeFile(t, filepath.Join(dir, checkpointFile), []byte("checkpoint"))
			writeFile(t, filepath.Join(dir, memoryFile), []byte("memory"))
			root := filepath.Join(dir, "rootfs.qcow2")
			if err := createQcow2OverlayOn(root, e.cfg.BaseImage, "raw", 1<<20); err != nil {
				t.Fatal(err)
			}
			spec := VMSpec{RootDisk: root, RootDiskFormat: "qcow2"}
			tampered := root
			if disk == "volume" {
				tampered = filepath.Join(dir, "vol0.img")
				if err := createQcow2OverlayOn(tampered, e.cfg.BaseImage, "raw", 1<<20); err != nil {
					t.Fatal(err)
				}
				spec.Volumes = []VMVolume{{BlockID: "vol0", Path: tampered, Format: "qcow2"}}
			}
			if err := e.recordDiskBacking(&spec); err != nil {
				t.Fatal(err)
			}
			m := krucibleSnapManifest{Arch: hostSnapshotArch(), Type: "memory", Vcpus: 1, MemMiB: 64,
				DiskFile: "rootfs.qcow2", DiskFormat: "qcow2", Backing: spec.Backing,
				NetPolicy: noNetworkSpec("").NetPolicy}
			if disk == "volume" {
				m.Volumes = []snapVolume{{File: "vol0.img", Format: "qcow2", Backing: spec.Volumes[0].Backing}}
			}
			if err := writeSnapshotManifest(dir, m); err != nil {
				t.Fatal(err)
			}
			manifest := filepath.Join(dir, snapshotManifestFile)
			manifestBefore, err := os.ReadFile(manifest)
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(e.cfg.DataDir, "images", "u1", "alternate.qcow2")
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := createQcow2OverlayOn(target, e.cfg.BaseImage, "raw", 1<<20); err != nil {
				t.Fatal(err)
			}
			if !e.vmmBaseAllowed(&VM{UserID: "u1"}, target) {
				t.Fatal("broader SQLite fixture does not name an independently authorized owner image")
			}
			if err := createQcow2OverlayOn(tampered, target, "qcow2", 1<<20); err != nil {
				t.Fatal(err)
			}
			broader := []diskLayer{{Path: target, Format: "qcow2"}, {Path: e.cfg.BaseImage, Format: "raw"}}
			if disk == "root" {
				m.Backing = broader
			} else {
				m.Volumes[0].Backing = broader
			}
			manifestJSON, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			copies := observeSnapshotCopies(t)
			_, err = e.ResumeFromManifestJSON(context.Background(), dir, manifestJSON, "rejected", "u1")
			if err == nil || !strings.Contains(err.Error(), target) || !strings.Contains(err.Error(), "recorded") {
				t.Fatalf("SQLite expanded the authoritative local %s chain: %v", disk, err)
			}
			if len(e.vms) != 0 {
				t.Fatal("SQLite-expanded restore registered a sandbox")
			}
			if got, err := os.ReadFile(copies); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("SQLite-expanded restore copied a disk: %q, %v", got, err)
			}
			if after, err := os.ReadFile(manifest); err != nil || !bytes.Equal(after, manifestBefore) {
				t.Fatalf("resume promoted SQLite authority into the local manifest: %q, %v", after, err)
			}
		})
	}
}

func TestLegacySnapshotRejectsOutsideBackingBeforeAnyRead(t *testing.T) {
	for _, disk := range []string{"root", "volume"} {
		t.Run(disk, func(t *testing.T) {
			e := fakeReseedEngine(t, "supported")
			dir := filepath.Join(e.cfg.DataDir, "snapshots", "u1", "legacy")
			writeFile(t, filepath.Join(dir, checkpointFile), []byte("checkpoint"))
			writeFile(t, filepath.Join(dir, memoryFile), []byte("memory"))
			root := filepath.Join(dir, "rootfs.qcow2")
			if err := createQcow2OverlayOn(root, e.cfg.BaseImage, "raw", 1<<20); err != nil {
				t.Fatal(err)
			}
			prefix, err := os.ReadFile(root)
			if err != nil {
				t.Fatal(err)
			}
			prefix = prefix[:qcow2HeaderLen]
			outside := filepath.Join(t.TempDir(), "private.qcow2")
			if err := syscall.Mkfifo(outside, 0o600); err != nil {
				t.Fatal(err)
			}
			// A preloaded FIFO exposes even a four-byte magic probe. Keeping
			// both ends open makes unauthorized opens nonblocking; reading
			// through the syscall leaves an empty FIFO observable as EAGAIN.
			fd, err := syscall.Open(outside, syscall.O_RDWR|syscall.O_NONBLOCK, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			defer syscall.Close(fd)
			if n, err := syscall.Write(fd, prefix); err != nil || n != len(prefix) {
				t.Fatalf("prime outside read tripwire: %d, %v", n, err)
			}
			tampered := root
			m := krucibleSnapManifest{Arch: hostSnapshotArch(), Type: "memory", Vcpus: 1, MemMiB: 64,
				DiskFile: "rootfs.qcow2", NetPolicy: noNetworkSpec("").NetPolicy}
			if disk == "volume" {
				tampered = filepath.Join(dir, "vol0.img")
				m.Volumes = []snapVolume{{File: "vol0.img"}}
			}
			if err := createQcow2OverlayOn(tampered, outside, "qcow2", 1<<20); err != nil {
				t.Fatal(err)
			}
			// Missing backing-format extensions are valid legacy qcow2. An
			// attacker must not get a sniff of the next file to infer one.
			f, err := os.OpenFile(tampered, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := f.WriteAt(make([]byte, 8), qcow2HeaderLen)
			closeErr := f.Close()
			if writeErr != nil || closeErr != nil {
				t.Fatalf("remove legacy format extension: %v, %v", writeErr, closeErr)
			}
			if format, err := qcow2BackingFormat(tampered); err != nil || format != "" {
				t.Fatalf("read tripwire is not a valid unknown-format legacy header: %q, %v", format, err)
			}
			manifestJSON, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			_, resumeErr := e.ResumeFromManifestJSON(context.Background(), dir, manifestJSON, "rejected", "u1")
			got := make([]byte, len(prefix)+1)
			n, readErr := syscall.Read(fd, got)
			if readErr != nil || n != len(prefix) || !bytes.Equal(got[:n], prefix) {
				t.Fatalf("legacy %s read outside before authorization: %d/%d bytes remain, %v; resume: %v",
					disk, n, len(prefix), readErr, resumeErr)
			}
			if resumeErr == nil || !strings.Contains(resumeErr.Error(), outside) ||
				!strings.Contains(resumeErr.Error(), "authorized") {
				t.Fatalf("legacy %s did not refuse the unauthorized edge: %v", disk, resumeErr)
			}
			assertNoLocalSnapshotManifest(t, dir)
		})
	}
}

func TestSnapshotRestorePolicyCannotReadOrWriteLocalManifest(t *testing.T) {
	f := newNodeFixture(t)
	source := f.freshRoot("source")
	recorded := VMSpec{RootDisk: source, RootDiskFormat: "qcow2"}
	if err := f.e.recordDiskBacking(&recorded); err != nil {
		t.Fatal(err)
	}
	snap := f.snapshot("u1", "memory", source)
	writeFile(t, filepath.Join(snap, checkpointFile), []byte("checkpoint"))
	writeFile(t, filepath.Join(snap, memoryFile), []byte("memory"))
	m := krucibleSnapManifest{Arch: hostSnapshotArch(), Type: "memory", Vcpus: 1, MemMiB: 64,
		DiskFile: "rootfs.qcow2", DiskFormat: "qcow2", Backing: recorded.Backing}
	if err := writeSnapshotManifest(snap, m); err != nil {
		t.Fatal(err)
	}
	root := f.resume("restored", snap)
	spec := VMSpec{RootDisk: root, RootDiskFormat: "qcow2", SnapshotDir: snap}
	if err := f.e.recordDiskBacking(&spec); err != nil {
		t.Fatal(err)
	}
	vm := &VM{ID: "restored", UserID: "u1", SandboxDir: filepath.Dir(root), SockDir: shortSockDir(t)}
	helper, _, cleanup, err := f.e.confineLaunch(t.Context(), vm, spec, vm.specPath())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	data, err := json.Marshal(helper)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vm.specPath(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	// Inspect while restore aliases still exist: launch removes them after
	// readiness, which would hide a transient manifest grant from this test.
	assertSnapshotHelperCannotAccess(t, vm, filepath.Join(snap, snapshotManifestFile))
	if helper.SnapshotDir == snap || !within(vm.SandboxDir, helper.SnapshotDir) {
		t.Fatalf("helper can restore through the daemon's snapshot directory: %s", helper.SnapshotDir)
	}
	entries, err := os.ReadDir(helper.SnapshotDir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if !slices.Equal(names, []string{checkpointFile, memoryFile}) {
		t.Fatalf("helper restore directory contains daemon authority: %v", names)
	}
	for _, name := range names {
		original, err := os.Stat(filepath.Join(snap, name))
		if err != nil {
			t.Fatal(err)
		}
		alias, err := os.Stat(filepath.Join(helper.SnapshotDir, name))
		if err != nil || !os.SameFile(original, alias) {
			t.Fatalf("restore alias %s is not the captured VM state: %v", name, err)
		}
	}
}

func TestLegacySnapshotMissingBackingFormatsResumesWithoutChangingSources(t *testing.T) {
	for _, disk := range []string{"root", "volume"} {
		t.Run(disk, func(t *testing.T) {
			e := fakeReseedEngine(t, "supported")
			e.confineVMM, e.caps.Pause = true, true
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			ancestor := filepath.Join(e.cfg.DataDir, "snapshots", "u1", "ancestor")
			dir := filepath.Join(e.cfg.DataDir, "snapshots", "u1", "legacy")
			writeFile(t, filepath.Join(dir, checkpointFile), []byte("checkpoint"))
			writeFile(t, filepath.Join(dir, memoryFile), []byte("memory"))
			if err := os.MkdirAll(ancestor, 0o700); err != nil {
				t.Fatal(err)
			}
			stripFormat := func(path string) {
				t.Helper()
				f, err := os.OpenFile(path, os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, writeErr := f.WriteAt(make([]byte, 8), int64(qcow2HeaderLen))
				closeErr := f.Close()
				if writeErr != nil || closeErr != nil {
					t.Fatalf("remove optional backing-format extension: %v, %v", writeErr, closeErr)
				}
				if format, err := qcow2BackingFormat(path); err != nil || format != "" {
					t.Fatalf("legacy fixture still has a backing format: %q, %v", format, err)
				}
			}
			ancestorDisk := filepath.Join(ancestor, "rootfs.qcow2")
			if err := createQcow2OverlayOn(ancestorDisk, e.cfg.BaseImage, "raw", 1<<20); err != nil {
				t.Fatal(err)
			}
			stripFormat(ancestorDisk)
			ancestorNode, err := e.nodeLink(ancestorDisk)
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(dir, "rootfs.qcow2")
			if err := createQcow2OverlayOn(root, e.cfg.BaseImage, "raw", 1<<20); err != nil {
				t.Fatal(err)
			}
			m := krucibleSnapManifest{Arch: hostSnapshotArch(), Type: "memory", Vcpus: 1, MemMiB: 64,
				DiskFile: "rootfs.qcow2", Token: "legacy-token", NetPolicy: noNetworkSpec("").NetPolicy}
			tampered := root
			if disk == "volume" {
				tampered = filepath.Join(dir, "vol0.img")
				m.Volumes = []snapVolume{{File: "vol0.img"}}
			}
			if err := createQcow2OverlayOn(tampered, ancestorNode, "qcow2", 1<<20); err != nil {
				t.Fatal(err)
			}
			stripFormat(tampered)
			secret := filepath.Join(e.cfg.DataDir, "private", "sqlite-only.img")
			writeFile(t, secret, []byte("not part of the legacy disk graph"))
			m.Backing = []diskLayer{{Path: secret, Format: "raw"}}
			if disk == "volume" {
				m.Volumes[0].Backing = []diskLayer{{Path: secret, Format: "raw"}}
			}
			originals := map[string][]byte{}
			for _, path := range []string{e.cfg.BaseImage, ancestorDisk, ancestorNode, root, tampered,
				filepath.Join(dir, checkpointFile), filepath.Join(dir, memoryFile)} {
				originals[path], err = os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
			}
			assertNoLocalSnapshotManifest(t, ancestor, dir)
			manifestJSON, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			restored, err := e.ResumeFromManifestJSON(ctx, dir, manifestJSON, "legacy-restored", "u1")
			if err != nil {
				t.Fatalf("authorized legacy %s with optional format extensions absent cannot resume: %v", disk, err)
			}
			vm, err := e.getVM(restored.ID)
			if err != nil {
				t.Fatal(err)
			}
			helper := assertSnapshotHelperCannotAccess(t, vm, secret, tampered, ancestorDisk, ancestorNode,
				filepath.Join(dir, snapshotManifestFile), filepath.Join(ancestor, snapshotManifestFile))
			wantVolumes := 0
			if disk == "volume" {
				wantVolumes = 1
			}
			if len(helper.Volumes) != wantVolumes {
				t.Fatalf("legacy restore changed its volume device set: %+v", helper.Volumes)
			}
			affected := helper.Backing
			if disk == "volume" {
				affected = helper.Volumes[0].Backing
			}
			if len(affected) != 3 || affected[0].Format != "qcow2" || affected[1].Format != "qcow2" ||
				affected[2] != (diskLayer{Path: e.cfg.BaseImage, Format: "raw"}) {
				t.Fatalf("normalization dropped a frozen legacy layer: %+v", affected)
			}
			for i, original := range []string{tampered, ancestorDisk} {
				normalized, err := os.ReadFile(affected[i].Path)
				before := originals[original]
				if err != nil || len(normalized) != len(before) ||
					!bytes.Equal(normalized[qcow2ClusterSize:], before[qcow2ClusterSize:]) {
					t.Fatalf("normalization changed legacy clusters beyond the rewritten header: %s, %v", original, err)
				}
			}
			disks := []struct {
				path, format string
				backing      []diskLayer
			}{{helper.RootDisk, helper.RootDiskFormat, helper.Backing}}
			for _, volume := range helper.Volumes {
				disks = append(disks, struct {
					path, format string
					backing      []diskLayer
				}{volume.Path, volume.Format, volume.Backing})
			}
			allowed := map[string]bool{vm.specPath(): true, "/dev/kvm": true, vm.SockDir: true,
				filepath.Join(vm.SandboxDir, "save"): true, helper.SnapshotDir: true}
			ancestors := map[string]bool{}
			for _, disk := range disks {
				allowed[disk.path] = true
				actual, err := readDiskChain(disk.path, disk.format, nil)
				if err != nil || !slices.Equal(actual, disk.backing) {
					t.Fatalf("child helper graph is not the exact normalized on-disk graph: %+v, %+v, %v",
						actual, disk.backing, err)
				}
				if len(actual) == 0 || actual[len(actual)-1] != (diskLayer{Path: e.cfg.BaseImage, Format: "raw"}) {
					t.Fatalf("normalized child lost its authorized shared raw terminal: %+v", actual)
				}
				for _, layer := range actual {
					allowed[layer.Path], ancestors[layer.Path] = true, true
				}
			}
			policy, err := os.ReadFile(filepath.Join(vm.SandboxDir, "vmm.policy"))
			if err != nil {
				t.Fatal(err)
			}
			for _, record := range strings.Split(string(policy), "\x00") {
				fields := strings.SplitN(record, " ", 3)
				if len(fields) != 3 || (fields[0] != "path" && fields[0] != "mount") {
					continue
				}
				access, err := strconv.ParseUint(fields[1], 16, 64)
				if err != nil {
					t.Fatal(err)
				}
				if access&LandlockReadFile != 0 && !allowed[fields[2]] {
					t.Errorf("child helper can read outside its exact authorized graph: %q", record)
				}
				if ancestors[fields[2]] && access != vmmReadFile {
					t.Errorf("child helper can write an immutable normalized ancestor: %q", record)
				}
			}
			assertNoLocalSnapshotManifest(t, ancestor, dir)
			for path, before := range originals {
				if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
					t.Errorf("legacy normalization changed an original disk/state file %s: %v", path, err)
				}
			}
			if result, err := e.Exec(ctx, restored.ID, []string{"sync"}); err != nil || result.ExitCode != 0 {
				t.Fatalf("normalized legacy child is not serving: %+v, %v", result, err)
			}
		})
	}
}
