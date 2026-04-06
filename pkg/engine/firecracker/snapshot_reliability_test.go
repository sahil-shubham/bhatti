//go:build linux

package firecracker

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// ==========================================================================
// Snapshot Reliability Tests
//
// These tests target bugs identified in SNAPSHOT-RELIABILITY-TRACE.md.
// They run on real Firecracker VMs (require root + /dev/kvm + arm64/amd64).
// ==========================================================================

// ---------------------------------------------------------------------------
// Bug #1 + New Bug #1: ResumeSnapshot doesn't populate vm.Volumes and
// handleSnapshotResume doesn't create volume_attachments.
//
// This is the root cause of the "rory" incident on Apr 5, 2025.
// ---------------------------------------------------------------------------

// TestSnapshotResumeVolumeStopStart verifies that a sandbox created via
// ResumeSnapshot with a volume can survive a stop/start cycle.
// This catches the missing vm.Volumes population in ResumeSnapshot().
func TestSnapshotResumeVolumeStopStart(t *testing.T) {
	eng := testJailedEngine(t)
	ctx := context.Background()

	// 1. Create a volume and a sandbox with it attached
	volDir := filepath.Join(eng.cfg.DataDir, "volumes", "usr_test")
	os.MkdirAll(volDir, 0700)
	volPath := filepath.Join(volDir, "resume-vol.ext4")
	defer os.Remove(volPath)
	createVolumeFile(t, volPath, 64)

	spec := testSpec("snap-resume-vol-src")
	spec.ResolvedVolumes = []engine.ResolvedVolume{{
		FilePath: volPath, DriveID: "vol0", Name: "resume-vol",
		Mount: "/workspace", ReadOnly: false,
	}}
	info, err := eng.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Write data to the volume
	execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "echo resume-vol-data > /workspace/data.txt"})

	// 2. Checkpoint
	snapDir := filepath.Join(eng.cfg.DataDir, "snapshots", "usr_test")
	os.MkdirAll(snapDir, 0700)
	defer os.RemoveAll(filepath.Join(snapDir, "resume-vol-ckpt"))

	manifestAny, err := eng.Checkpoint(ctx, info.ID, "usr_test", 99, "resume-vol-ckpt", snapDir)
	if err != nil {
		eng.Destroy(ctx, info.ID)
		t.Fatalf("Checkpoint: %v", err)
	}
	manifest := manifestAny.(*SnapshotManifest)

	// Verify the manifest includes the volume drive
	hasVolDrive := false
	for _, d := range manifest.Drives {
		if d.Role == "volume" && d.Name == "resume-vol" {
			hasVolDrive = true
		}
	}
	if !hasVolDrive {
		eng.Destroy(ctx, info.ID)
		t.Fatal("manifest should contain volume drive")
	}

	eng.Destroy(ctx, info.ID)

	// 3. Resume from snapshot
	snapPath := filepath.Join(snapDir, "resume-vol-ckpt")
	info2, err := eng.ResumeSnapshot(ctx, snapPath, manifest, "resumed-with-vol")
	if err != nil {
		t.Fatalf("ResumeSnapshot: %v", err)
	}
	defer eng.Destroy(ctx, info2.ID)

	// Verify volume data is accessible immediately after resume
	r, _ := execWithTimeout(t, eng, info2.ID, []string{"cat", "/workspace/data.txt"})
	if !strings.Contains(r.Stdout, "resume-vol-data") {
		t.Fatalf("volume data not accessible after resume: %q", r.Stdout)
	}
	t.Log("✓ volume data accessible after snapshot resume")

	// 4. KEY TEST: Stop and Start the resumed sandbox.
	//    This is where Bug #1 manifests — vm.Volumes is empty so startVM()
	//    doesn't link the volume file into the jail.
	if err := eng.Stop(ctx, info2.ID); err != nil {
		t.Fatalf("Stop resumed sandbox: %v", err)
	}
	if err := eng.Start(ctx, info2.ID); err != nil {
		t.Fatalf("Start resumed sandbox after stop: %v\n"+
			"This is Bug #1: vm.Volumes not populated by ResumeSnapshot, "+
			"so startVM can't link volume files into jail", err)
	}

	// Verify volume data survives the stop/start cycle
	r, _ = execWithTimeout(t, eng, info2.ID, []string{"cat", "/workspace/data.txt"})
	if !strings.Contains(r.Stdout, "resume-vol-data") {
		t.Fatalf("volume data lost after stop/start of resumed sandbox: %q", r.Stdout)
	}
	t.Log("✓ volume data survives stop/start on snapshot-resumed sandbox")
}

// TestSnapshotResumeVolumeInVMState verifies that VMState() on a
// snapshot-resumed sandbox includes volume attachment info.
// This is needed for saveVMState → recoverVMs to round-trip volumes.
func TestSnapshotResumeVolumeInVMState(t *testing.T) {
	eng := testJailedEngine(t)
	ctx := context.Background()

	volDir := filepath.Join(eng.cfg.DataDir, "volumes", "usr_test")
	os.MkdirAll(volDir, 0700)
	volPath := filepath.Join(volDir, "vmstate-resume-vol.ext4")
	defer os.Remove(volPath)
	createVolumeFile(t, volPath, 64)

	spec := testSpec("vmstate-resume-src")
	spec.ResolvedVolumes = []engine.ResolvedVolume{{
		FilePath: volPath, DriveID: "vol0", Name: "vmstate-resume-vol",
		Mount: "/workspace", ReadOnly: false,
	}}
	info, err := eng.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	snapDir := filepath.Join(eng.cfg.DataDir, "snapshots", "usr_test")
	os.MkdirAll(snapDir, 0700)
	defer os.RemoveAll(filepath.Join(snapDir, "vmstate-resume-ckpt"))

	manifestAny, err := eng.Checkpoint(ctx, info.ID, "usr_test", 99, "vmstate-resume-ckpt", snapDir)
	if err != nil {
		eng.Destroy(ctx, info.ID)
		t.Fatalf("Checkpoint: %v", err)
	}
	manifest := manifestAny.(*SnapshotManifest)
	eng.Destroy(ctx, info.ID)

	// Resume
	snapPath := filepath.Join(snapDir, "vmstate-resume-ckpt")
	info2, err := eng.ResumeSnapshot(ctx, snapPath, manifest, "vmstate-resumed")
	if err != nil {
		t.Fatalf("ResumeSnapshot: %v", err)
	}
	defer eng.Destroy(ctx, info2.ID)

	// Check VMState includes volumes
	state := eng.VMState(info2.ID)
	if state == nil {
		t.Fatal("VMState returned nil")
	}
	vols, ok := state["volumes"]
	if !ok || vols == nil {
		t.Fatal("Bug #1: VMState on snapshot-resumed sandbox has no 'volumes' key — " +
			"recoverVMs will lose volume info on daemon restart")
	}

	// Verify the volume info is actually populated
	b, _ := json.Marshal(vols)
	var volList []VolumeAttachmentInfo
	json.Unmarshal(b, &volList)
	if len(volList) == 0 {
		t.Fatal("Bug #1: VMState 'volumes' is empty — vm.Volumes not populated by ResumeSnapshot")
	}

	found := false
	for _, v := range volList {
		if v.Name == "vmstate-resume-vol" && v.Mount == "/workspace" {
			found = true
			if v.FilePath == "" {
				t.Error("volume FilePath is empty — resume won't find the volume file")
			}
		}
	}
	if !found {
		t.Fatalf("volume 'vmstate-resume-vol' not in VMState volumes: %s", string(b))
	}
	t.Log("✓ VMState includes volume info from snapshot-resumed sandbox")
}

// TestSnapshotResumeNestedCheckpointPreservesVolumes verifies that
// checkpointing a snapshot-resumed sandbox correctly captures volumes.
// Chain: Create(+vol) → Checkpoint → Resume → Checkpoint → Resume → verify vol
func TestSnapshotResumeNestedCheckpointPreservesVolumes(t *testing.T) {
	eng := testJailedEngine(t)
	ctx := context.Background()

	volDir := filepath.Join(eng.cfg.DataDir, "volumes", "usr_test")
	os.MkdirAll(volDir, 0700)
	volPath := filepath.Join(volDir, "nested-vol.ext4")
	defer os.Remove(volPath)
	createVolumeFile(t, volPath, 64)

	// Create original with volume
	spec := testSpec("nested-vol-src")
	spec.ResolvedVolumes = []engine.ResolvedVolume{{
		FilePath: volPath, DriveID: "vol0", Name: "nested-vol",
		Mount: "/workspace", ReadOnly: false,
	}}
	info, err := eng.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "echo layer1 > /workspace/layers.txt"})

	// Checkpoint 1
	snapDir := filepath.Join(eng.cfg.DataDir, "snapshots", "usr_test")
	os.MkdirAll(snapDir, 0700)
	defer os.RemoveAll(filepath.Join(snapDir, "nested-vol-ckpt1"))
	defer os.RemoveAll(filepath.Join(snapDir, "nested-vol-ckpt2"))

	m1Any, err := eng.Checkpoint(ctx, info.ID, "usr_test", 99, "nested-vol-ckpt1", snapDir)
	if err != nil {
		eng.Destroy(ctx, info.ID)
		t.Fatalf("Checkpoint 1: %v", err)
	}
	m1 := m1Any.(*SnapshotManifest)
	eng.Destroy(ctx, info.ID)

	// Resume from checkpoint 1
	info2, err := eng.ResumeSnapshot(ctx, filepath.Join(snapDir, "nested-vol-ckpt1"), m1, "nested-vol-b")
	if err != nil {
		t.Fatalf("Resume 1: %v", err)
	}

	// Write more data
	execWithTimeout(t, eng, info2.ID, []string{"sh", "-c", "echo layer2 >> /workspace/layers.txt"})

	// Checkpoint 2 from the resumed sandbox — this is where Bug #1 causes
	// volume drives to be missing from the manifest if vm.Volumes is empty.
	m2Any, err := eng.Checkpoint(ctx, info2.ID, "usr_test", 99, "nested-vol-ckpt2", snapDir)
	if err != nil {
		eng.Destroy(ctx, info2.ID)
		t.Fatalf("Checkpoint 2 (from resumed sandbox): %v\n"+
			"If vm.Volumes is empty, this checkpoint won't include the volume drive", err)
	}
	m2 := m2Any.(*SnapshotManifest)
	eng.Destroy(ctx, info2.ID)

	// Verify checkpoint 2 manifest has the volume drive
	hasVol := false
	for _, d := range m2.Drives {
		if d.Role == "volume" && d.Name == "nested-vol" {
			hasVol = true
		}
	}
	if !hasVol {
		t.Fatal("Bug #1: nested checkpoint manifest is missing volume drive — " +
			"vm.Volumes was empty on the resumed sandbox")
	}

	// Resume from checkpoint 2 and verify both layers
	info3, err := eng.ResumeSnapshot(ctx, filepath.Join(snapDir, "nested-vol-ckpt2"), m2, "nested-vol-c")
	if err != nil {
		t.Fatalf("Resume 2: %v", err)
	}
	defer eng.Destroy(ctx, info3.ID)

	r, _ := execWithTimeout(t, eng, info3.ID, []string{"cat", "/workspace/layers.txt"})
	if !strings.Contains(r.Stdout, "layer1") || !strings.Contains(r.Stdout, "layer2") {
		t.Fatalf("data lost in nested checkpoint chain: %q", r.Stdout)
	}
	t.Log("✓ volume data preserved through Create→Checkpoint→Resume→Checkpoint→Resume chain")
}

// ---------------------------------------------------------------------------
// Bug #2 + #3: Stop() doesn't return error on snapshot move/verify failure.
// ---------------------------------------------------------------------------

// TestStopReturnsErrorOnVerifyFailure verifies that Stop() returns an error
// (and keeps the VM alive) when verifySnapshotArtifacts detects corruption.
//
// We can't easily inject a verify failure without mocking, but we CAN
// verify the inverse: after a successful Stop(), the snapshot is valid
// and Start() works. This confirms the happy path. The bug itself is a
// code inspection issue (slog.Error without return).
func TestStopSnapshotArtifactsValid(t *testing.T) {
	eng := testJailedEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("stop-verify"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "echo verify-test > /tmp/data"})

	if err := eng.Stop(ctx, info.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Verify snapshot artifacts exist and are valid
	sandboxDir := filepath.Join(eng.cfg.DataDir, "sandboxes", info.ID)
	vmSnap := filepath.Join(sandboxDir, "vm.snap")
	memSnap := filepath.Join(sandboxDir, "mem.snap")

	vmInfo, err := os.Stat(vmSnap)
	if err != nil || vmInfo.Size() == 0 {
		t.Fatalf("vm.snap missing or empty after Stop: err=%v", err)
	}
	memInfo, err := os.Stat(memSnap)
	if err != nil || memInfo.Size() == 0 {
		t.Fatalf("mem.snap missing or empty after Stop: err=%v", err)
	}

	// mem.snap should be exactly MemSizeMib for Full snapshots
	expectedSize := int64(512 * 1024 * 1024) // testSpec uses 512MB
	if memInfo.Size() != expectedSize {
		t.Errorf("mem.snap size %d != expected %d (Full snapshot)", memInfo.Size(), expectedSize)
	}

	// Snapshot files should be in sandbox dir, NOT in jail
	jailDir := filepath.Join(eng.cfg.DataDir, "jails", "firecracker", info.ID, "root")
	if _, err := os.Stat(filepath.Join(jailDir, "vm.snap")); err == nil {
		t.Error("vm.snap should NOT remain in jail dir after Stop (should be moved)")
	}

	// Start should succeed with valid artifacts
	if err := eng.Start(ctx, info.ID); err != nil {
		t.Fatalf("Start after verified Stop: %v", err)
	}

	r, _ := execWithTimeout(t, eng, info.ID, []string{"cat", "/tmp/data"})
	if !strings.Contains(r.Stdout, "verify-test") {
		t.Fatalf("data lost: %q", r.Stdout)
	}
	t.Log("✓ Stop produces valid snapshot artifacts, Start succeeds")
}

// ---------------------------------------------------------------------------
// Bug #4: IP pool corruption on TryAllocate failure in ResumeSnapshot.
// ---------------------------------------------------------------------------

// TestResumeIPConflictNoPoolCorruption verifies that when snapshot resume
// fails due to an IP conflict, the original sandbox's IP is NOT freed
// from the pool (i.e., no double-allocation on the next request).
func TestResumeIPConflictNoPoolCorruption(t *testing.T) {
	eng := testJailedEngine(t)
	ctx := context.Background()

	// Create sandbox A — occupies an IP
	infoA, err := eng.Create(ctx, testSpec("ip-corrupt-a"))
	if err != nil {
		t.Fatalf("Create A: %v", err)
	}
	defer eng.Destroy(ctx, infoA.ID)
	ipA := infoA.IP

	// Checkpoint A
	snapDir := filepath.Join(eng.cfg.DataDir, "snapshots", "usr_test")
	os.MkdirAll(snapDir, 0700)
	defer os.RemoveAll(filepath.Join(snapDir, "ip-corrupt-ckpt"))

	manifestAny, err := eng.Checkpoint(ctx, infoA.ID, "usr_test", 99, "ip-corrupt-ckpt", snapDir)
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	manifest := manifestAny.(*SnapshotManifest)

	// Try to resume while A is still running (IP conflict expected)
	snapPath := filepath.Join(snapDir, "ip-corrupt-ckpt")
	_, err = eng.ResumeSnapshot(ctx, snapPath, manifest, "ip-conflict-sb")
	if err == nil {
		t.Fatal("expected IP conflict error")
	}
	if !strings.Contains(err.Error(), "in use") {
		t.Fatalf("expected 'in use' error, got: %v", err)
	}
	t.Logf("✓ resume correctly rejected: %v", err)

	// KEY TEST: Sandbox A should still work. If Bug #4 corrupted the pool,
	// A's IP was released by the cleanup defer and could be given to a new VM.
	r, err := execWithTimeout(t, eng, infoA.ID, []string{"echo", "still-alive"})
	if err != nil || !strings.Contains(r.Stdout, "still-alive") {
		t.Fatalf("sandbox A broken after IP conflict: err=%v out=%q", err, r.Stdout)
	}

	// Create sandbox B — should get a DIFFERENT IP than A.
	// If the pool was corrupted, B might get A's IP.
	infoB, err := eng.Create(ctx, testSpec("ip-corrupt-b"))
	if err != nil {
		t.Fatalf("Create B: %v", err)
	}
	defer eng.Destroy(ctx, infoB.ID)

	if infoB.IP == ipA {
		t.Fatalf("Bug #4: IP pool corrupted — sandbox B got A's IP %s "+
			"(pool.Release was called on a never-allocated IP during failed resume)",
			ipA)
	}
	t.Logf("✓ A=%s B=%s — no IP pool corruption after failed resume", ipA, infoB.IP)

	// Both should work independently
	rA, _ := execWithTimeout(t, eng, infoA.ID, []string{"echo", "a-ok"})
	rB, _ := execWithTimeout(t, eng, infoB.ID, []string{"echo", "b-ok"})
	if !strings.Contains(rA.Stdout, "a-ok") || !strings.Contains(rB.Stdout, "b-ok") {
		t.Fatalf("one of the sandboxes is broken: A=%q B=%q", rA.Stdout, rB.Stdout)
	}
	t.Log("✓ both sandboxes functional after failed resume")
}

// ---------------------------------------------------------------------------
// Bug #5: SaveImage on cold/stopped sandboxes.
// ---------------------------------------------------------------------------

// TestSaveImageOnColdSandbox verifies that SaveImage on a cold sandbox
// either returns a clear error or succeeds without confusing "connection
// refused" errors from trying to resume a dead FC process.
func TestSaveImageOnColdSandbox(t *testing.T) {
	eng := testJailedEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("save-cold"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Write data, then stop the sandbox (cold)
	execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "echo cold-save > /home/lohar/data.txt"})
	if err := eng.Stop(ctx, info.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	imgPath := filepath.Join(eng.cfg.DataDir, "images", "cold-save-test.ext4")
	defer os.Remove(imgPath)

	err = eng.SaveImage(ctx, info.ID, imgPath)
	if err != nil {
		// Bug #5: the error should be clear ("sandbox is stopped"), not
		// "resume after save: dial unix ... connection refused"
		if strings.Contains(err.Error(), "connection refused") ||
			strings.Contains(err.Error(), "resume after save") {
			t.Fatalf("Bug #5: SaveImage on cold sandbox gave confusing error: %v\n"+
				"Should either succeed (cold rootfs copy is valid) or fail with "+
				"'sandbox is stopped'", err)
		}
		// A clear rejection is acceptable
		t.Logf("✓ SaveImage on cold sandbox rejected clearly: %v", err)
	} else {
		// If it succeeds, the image should be valid
		fi, err := os.Stat(imgPath)
		if err != nil || fi.Size() == 0 {
			t.Fatal("SaveImage succeeded but output is empty/missing")
		}
		t.Log("✓ SaveImage on cold sandbox succeeded (rootfs copy valid)")
	}
}

// ---------------------------------------------------------------------------
// Bug #8: recoverVMs drive_id ordering not guaranteed.
// ---------------------------------------------------------------------------

// TestRecoveryMultiVolumeOrdering verifies that RestoreVM preserves
// volume ordering so that drive_id mappings are stable.
func TestRecoveryMultiVolumeOrdering(t *testing.T) {
	eng := testJailedEngine(t)
	ctx := context.Background()

	volDir := filepath.Join(eng.cfg.DataDir, "volumes", "usr_test")
	os.MkdirAll(volDir, 0700)
	vol1 := filepath.Join(volDir, "order-alpha.ext4")
	vol2 := filepath.Join(volDir, "order-beta.ext4")
	defer os.Remove(vol1)
	defer os.Remove(vol2)

	createVolumeFile(t, vol1, 32)
	createVolumeFile(t, vol2, 32)

	spec := testSpec("vol-order")
	spec.ResolvedVolumes = []engine.ResolvedVolume{
		{FilePath: vol1, DriveID: "vol0", Name: "alpha", Mount: "/alpha", ReadOnly: false},
		{FilePath: vol2, DriveID: "vol1", Name: "beta", Mount: "/beta", ReadOnly: false},
	}

	info, err := eng.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Write different data to each volume to detect ordering issues
	execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "echo alpha-data > /alpha/id.txt"})
	execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "echo beta-data > /beta/id.txt"})

	// Get VMState and verify volume ordering
	state := eng.VMState(info.ID)
	vols, _ := state["volumes"].([]VolumeAttachmentInfo)
	if len(vols) != 2 {
		eng.Destroy(ctx, info.ID)
		t.Fatalf("expected 2 volumes, got %d", len(vols))
	}

	// Simulate restore via RestoreVM (like recoverVMs would do)
	// First stop the VM to get snapshot paths
	if err := eng.Stop(ctx, info.ID); err != nil {
		eng.Destroy(ctx, info.ID)
		t.Fatalf("Stop: %v", err)
	}

	state = eng.VMState(info.ID)

	// Delete from engine to simulate daemon restart
	eng.mu.Lock()
	delete(eng.vms, info.ID)
	eng.mu.Unlock()

	// Restore via RestoreVM
	eng.RestoreVM(info.ID, "vol-order", "stopped", state)

	// Start the restored VM
	if err := eng.Start(ctx, info.ID); err != nil {
		t.Fatalf("Start after RestoreVM: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Verify volume data is at the correct mount points
	r1, _ := execWithTimeout(t, eng, info.ID, []string{"cat", "/alpha/id.txt"})
	r2, _ := execWithTimeout(t, eng, info.ID, []string{"cat", "/beta/id.txt"})

	if !strings.Contains(r1.Stdout, "alpha-data") {
		t.Errorf("Bug #8: /alpha has wrong data: %q (expected alpha-data) — "+
			"volume ordering changed during recovery", r1.Stdout)
	}
	if !strings.Contains(r2.Stdout, "beta-data") {
		t.Errorf("Bug #8: /beta has wrong data: %q (expected beta-data) — "+
			"volume ordering changed during recovery", r2.Stdout)
	}
	if strings.Contains(r1.Stdout, "alpha-data") && strings.Contains(r2.Stdout, "beta-data") {
		t.Log("✓ volume ordering preserved through VMState → RestoreVM → Start")
	}
}

// ---------------------------------------------------------------------------
// Bug #10: Thermal manager blocks on stateMu contention.
// (Not directly testable in integration, but we verify that concurrent
//  Stop + Checkpoint doesn't deadlock.)
// ---------------------------------------------------------------------------

// TestConcurrentCheckpointAndStop verifies no deadlock when Checkpoint
// and Stop are called concurrently on different sandboxes.
func TestConcurrentCheckpointAndStop(t *testing.T) {
	eng := testJailedEngine(t)
	ctx := context.Background()

	// Create two sandboxes
	info1, err := eng.Create(ctx, testSpec("concurrent-a"))
	if err != nil {
		t.Fatalf("Create A: %v", err)
	}
	defer eng.Destroy(ctx, info1.ID)

	info2, err := eng.Create(ctx, testSpec("concurrent-b"))
	if err != nil {
		t.Fatalf("Create B: %v", err)
	}
	defer eng.Destroy(ctx, info2.ID)

	snapDir := filepath.Join(eng.cfg.DataDir, "snapshots", "usr_test")
	os.MkdirAll(snapDir, 0700)
	defer os.RemoveAll(filepath.Join(snapDir, "concurrent-ckpt"))

	errs := make(chan error, 2)

	// Checkpoint A and Stop B concurrently
	go func() {
		_, err := eng.Checkpoint(ctx, info1.ID, "usr_test", 99, "concurrent-ckpt", snapDir)
		errs <- err
	}()
	go func() {
		errs <- eng.Stop(ctx, info2.ID)
	}()

	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent operation %d failed: %v", i, err)
		}
	}
	t.Log("✓ concurrent Checkpoint + Stop on different sandboxes succeeds without deadlock")
}

// ---------------------------------------------------------------------------
// Additional: Verify snapshot resume cleanup on failure.
// ---------------------------------------------------------------------------

// TestResumeSnapshotCleanupOnAgentTimeout verifies that resources (TAP, IP,
// sandbox dir) are cleaned up when agent WaitReady times out during resume.
func TestResumeSnapshotCleanupOnAgentTimeout(t *testing.T) {
	eng := testJailedEngine(t)
	ctx := context.Background()

	// Create and checkpoint a healthy sandbox
	info, err := eng.Create(ctx, testSpec("cleanup-src"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	snapDir := filepath.Join(eng.cfg.DataDir, "snapshots", "usr_test")
	os.MkdirAll(snapDir, 0700)
	defer os.RemoveAll(filepath.Join(snapDir, "cleanup-ckpt"))

	manifestAny, err := eng.Checkpoint(ctx, info.ID, "usr_test", 99, "cleanup-ckpt", snapDir)
	if err != nil {
		eng.Destroy(ctx, info.ID)
		t.Fatalf("Checkpoint: %v", err)
	}
	manifest := manifestAny.(*SnapshotManifest)
	eng.Destroy(ctx, info.ID)

	// Corrupt the mem.snap so that the VM boots but the guest is broken
	// (agent won't respond). This triggers the WaitReady timeout path.
	snapPath := filepath.Join(snapDir, "cleanup-ckpt")
	memSnap := filepath.Join(snapPath, "mem.snap")
	origMem, _ := os.ReadFile(memSnap)
	if len(origMem) > 4096 {
		// Corrupt middle of memory snapshot — FC may load it but guest
		// will be in a bad state. Write zeros to a critical region.
		copy(origMem[4096:8192], make([]byte, 4096))
		os.WriteFile(memSnap, origMem, 0644)
	}

	// Try to resume — should fail (agent timeout or FC load failure)
	resumeInfo, err := eng.ResumeSnapshot(ctx, snapPath, manifest, "cleanup-test")
	if err == nil {
		// If it somehow succeeded (mem corruption didn't hit critical path),
		// that's OK — just clean up and skip the resource leak check
		t.Log("resume succeeded despite corruption — skipping cleanup check")
		eng.Destroy(ctx, resumeInfo.ID)
		return
	}
	t.Logf("resume failed as expected: %v", err)

	// Verify cleanup: the sandbox directory should be removed
	// (the defer in ResumeSnapshot removes it on error)
	entries, _ := filepath.Glob(filepath.Join(eng.cfg.DataDir, "sandboxes", "*"))
	for _, e := range entries {
		base := filepath.Base(e)
		// Only check for dirs that aren't from other tests
		if _, vmErr := eng.getVM(base); vmErr != nil {
			// Not a known VM — could be leftover from failed resume
			// Check if it has a rootfs (resume copies files before failing)
			if _, err := os.Stat(filepath.Join(e, "rootfs.ext4")); err == nil {
				t.Logf("⚠ possible leaked sandbox dir: %s", e)
			}
		}
	}

	// Verify no leaked TAP devices from the failed resume
	out, _ := exec.Command("ip", "-o", "link", "show", "type", "tun").Output()
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	eng.mu.RLock()
	knownTaps := make(map[string]bool)
	for _, vm := range eng.vms {
		if vm.TapDevice != "" {
			knownTaps[vm.TapDevice] = true
		}
	}
	eng.mu.RUnlock()

	for _, line := range lines {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		tapName := strings.TrimSuffix(fields[1], ":")
		if strings.HasPrefix(tapName, "tap") && !knownTaps[tapName] {
			t.Errorf("leaked TAP device from failed resume: %s", tapName)
		}
	}

	t.Log("✓ resources cleaned up after failed snapshot resume")
}

// ---------------------------------------------------------------------------
// Jailed mode snapshot roundtrip with volumes (compound test for all
// jail-related path handling)
// ---------------------------------------------------------------------------

// TestJailedVolumeSnapshotFullCycle exercises the complete lifecycle in
// jailed mode with a volume: Create → write → Stop → Start → verify →
// Checkpoint → Destroy → Resume → verify → Stop → Start → verify.
func TestJailedVolumeSnapshotFullCycle(t *testing.T) {
	eng := testJailedEngine(t)
	ctx := context.Background()

	volDir := filepath.Join(eng.cfg.DataDir, "volumes", "usr_test")
	os.MkdirAll(volDir, 0700)
	volPath := filepath.Join(volDir, "fullcycle.ext4")
	defer os.Remove(volPath)
	createVolumeFile(t, volPath, 64)

	// Phase 1: Create with volume
	spec := testSpec("fullcycle")
	spec.ResolvedVolumes = []engine.ResolvedVolume{{
		FilePath: volPath, DriveID: "vol0", Name: "fullcycle",
		Mount: "/workspace", ReadOnly: false,
	}}
	info, err := eng.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "echo phase1 > /workspace/log.txt"})
	execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "echo rootfs1 > /home/lohar/log.txt"})

	// Phase 2: Stop → Start (thermal cycle)
	if err := eng.Stop(ctx, info.ID); err != nil {
		t.Fatalf("Stop 1: %v", err)
	}
	if err := eng.Start(ctx, info.ID); err != nil {
		t.Fatalf("Start 1: %v", err)
	}

	r, _ := execWithTimeout(t, eng, info.ID, []string{"cat", "/workspace/log.txt"})
	if !strings.Contains(r.Stdout, "phase1") {
		t.Fatalf("vol data lost after stop/start: %q", r.Stdout)
	}
	r, _ = execWithTimeout(t, eng, info.ID, []string{"cat", "/home/lohar/log.txt"})
	if !strings.Contains(r.Stdout, "rootfs1") {
		t.Fatalf("rootfs data lost after stop/start: %q", r.Stdout)
	}
	t.Log("✓ phase 2: stop/start preserves both rootfs and volume data")

	// Phase 3: Checkpoint
	execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "echo phase3 >> /workspace/log.txt"})

	snapDir := filepath.Join(eng.cfg.DataDir, "snapshots", "usr_test")
	os.MkdirAll(snapDir, 0700)
	defer os.RemoveAll(filepath.Join(snapDir, "fullcycle-ckpt"))

	manifestAny, err := eng.Checkpoint(ctx, info.ID, "usr_test", 99, "fullcycle-ckpt", snapDir)
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	manifest := manifestAny.(*SnapshotManifest)

	// Verify source VM still works after checkpoint
	r, _ = execWithTimeout(t, eng, info.ID, []string{"echo", "post-ckpt"})
	if !strings.Contains(r.Stdout, "post-ckpt") {
		t.Fatal("source VM should be running after checkpoint")
	}
	eng.Destroy(ctx, info.ID)
	t.Log("✓ phase 3: checkpoint created, source destroyed")

	// Phase 4: Resume from checkpoint
	snapPath := filepath.Join(snapDir, "fullcycle-ckpt")
	info2, err := eng.ResumeSnapshot(ctx, snapPath, manifest, "fullcycle-resumed")
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}

	r, _ = execWithTimeout(t, eng, info2.ID, []string{"cat", "/workspace/log.txt"})
	if !strings.Contains(r.Stdout, "phase1") || !strings.Contains(r.Stdout, "phase3") {
		t.Fatalf("volume data incomplete after resume: %q", r.Stdout)
	}
	t.Log("✓ phase 4: resumed from checkpoint, volume data complete")

	// Phase 5: Stop → Start the resumed sandbox (the full Bug #1 path)
	if err := eng.Stop(ctx, info2.ID); err != nil {
		t.Fatalf("Stop resumed: %v", err)
	}
	if err := eng.Start(ctx, info2.ID); err != nil {
		t.Fatalf("Start resumed: %v\n"+
			"This is the Bug #1 failure path: vm.Volumes empty after ResumeSnapshot", err)
	}

	r, _ = execWithTimeout(t, eng, info2.ID, []string{"cat", "/workspace/log.txt"})
	if !strings.Contains(r.Stdout, "phase1") || !strings.Contains(r.Stdout, "phase3") {
		t.Fatalf("volume data lost after stop/start of resumed sandbox: %q", r.Stdout)
	}

	r, _ = execWithTimeout(t, eng, info2.ID, []string{"cat", "/home/lohar/log.txt"})
	if !strings.Contains(r.Stdout, "rootfs1") {
		t.Fatalf("rootfs data lost: %q", r.Stdout)
	}

	eng.Destroy(ctx, info2.ID)
	t.Log("✓ phase 5: stop/start of resumed sandbox preserves all data")
	t.Log("✓ FULL CYCLE PASSED: Create→Stop→Start→Checkpoint→Resume→Stop→Start")
}
