//go:build linux

package firecracker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/oci"
)

// ==========================================================================
// v0.3 Integration Tests — Persistent Volumes
// ==========================================================================

// TestPersistentVolumeDataSurvivesDestroy creates a volume, writes data in
// one sandbox, destroys it, creates a new sandbox with the same volume,
// and verifies data persists.
func TestPersistentVolumeDataSurvivesDestroy(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	// Create persistent volume file
	volDir := filepath.Join(eng.cfg.DataDir, "volumes", "usr_test")
	os.MkdirAll(volDir, 0700)
	volPath := filepath.Join(volDir, "integ-ws.ext4")
	defer os.Remove(volPath)

	createVolumeFile(t, volPath, 64)

	// Sandbox 1: write data
	spec1 := testSpec("vol-persist-1")
	spec1.ResolvedVolumes = []engine.ResolvedVolume{{
		FilePath: volPath, DriveID: "vol0", Name: "integ-ws",
		Mount: "/workspace", ReadOnly: false,
	}}

	info1, err := eng.Create(ctx, spec1)
	if err != nil {
		t.Fatalf("Create sb1: %v", err)
	}

	r, err := execWithTimeout(t, eng, info1.ID, []string{"sh", "-c",
		"echo persistent-data-12345 > /workspace/test.txt && cat /workspace/test.txt"})
	if err != nil || r.ExitCode != 0 {
		t.Fatalf("write: exit=%d err=%v stderr=%q", r.ExitCode, err, r.Stderr)
	}
	if !strings.Contains(r.Stdout, "persistent-data-12345") {
		t.Fatalf("write verification failed: %q", r.Stdout)
	}
	t.Log("✓ wrote data to volume in sandbox 1")

	eng.Destroy(ctx, info1.ID)
	t.Log("✓ destroyed sandbox 1")

	// Sandbox 2: read data back from the same volume file
	spec2 := testSpec("vol-persist-2")
	spec2.ResolvedVolumes = []engine.ResolvedVolume{{
		FilePath: volPath, DriveID: "vol0", Name: "integ-ws",
		Mount: "/workspace", ReadOnly: false,
	}}

	info2, err := eng.Create(ctx, spec2)
	if err != nil {
		t.Fatalf("Create sb2: %v", err)
	}
	defer eng.Destroy(ctx, info2.ID)

	r, err = execWithTimeout(t, eng, info2.ID, []string{"cat", "/workspace/test.txt"})
	if err != nil || r.ExitCode != 0 {
		t.Fatalf("read: exit=%d err=%v stderr=%q", r.ExitCode, err, r.Stderr)
	}
	if !strings.Contains(r.Stdout, "persistent-data-12345") {
		t.Fatalf("data did not persist: %q", r.Stdout)
	}
	t.Log("✓ data persists across sandbox destroy/recreate")
}

// TestVolumeReadOnlyMount verifies the three-layer RO enforcement:
// Firecracker is_read_only:true → config drive read_only:true → lohar MS_RDONLY
func TestVolumeReadOnlyMount(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	volDir := filepath.Join(eng.cfg.DataDir, "volumes", "usr_test")
	os.MkdirAll(volDir, 0700)
	volPath := filepath.Join(volDir, "integ-ro.ext4")
	defer os.Remove(volPath)

	createVolumeFile(t, volPath, 64)

	// Write some data first (RW)
	specRW := testSpec("vol-ro-setup")
	specRW.ResolvedVolumes = []engine.ResolvedVolume{{
		FilePath: volPath, DriveID: "vol0", Name: "integ-ro",
		Mount: "/data", ReadOnly: false,
	}}
	infoRW, err := eng.Create(ctx, specRW)
	if err != nil {
		t.Fatalf("Create RW: %v", err)
	}
	execWithTimeout(t, eng, infoRW.ID, []string{"sh", "-c", "echo ro-test > /data/file.txt"})
	eng.Destroy(ctx, infoRW.ID)

	// Clean journal before RO mount
	exec.Command("e2fsck", "-f", "-y", volPath).Run()

	// Now mount read-only
	specRO := testSpec("vol-ro-test")
	specRO.ResolvedVolumes = []engine.ResolvedVolume{{
		FilePath: volPath, DriveID: "vol0", Name: "integ-ro",
		Mount: "/data", ReadOnly: true,
	}}
	infoRO, err := eng.Create(ctx, specRO)
	if err != nil {
		t.Fatalf("Create RO: %v", err)
	}
	defer eng.Destroy(ctx, infoRO.ID)

	// Verify data is readable
	r, _ := execWithTimeout(t, eng, infoRO.ID, []string{"cat", "/data/file.txt"})
	if !strings.Contains(r.Stdout, "ro-test") {
		t.Fatalf("expected data readable in RO mount, got %q", r.Stdout)
	}
	t.Log("✓ data readable through RO mount")

	// Verify mount shows 'ro'
	r, _ = execWithTimeout(t, eng, infoRO.ID, []string{"mount"})
	if !strings.Contains(r.Stdout, "/data type ext4 (ro") {
		t.Fatalf("expected RO mount, got: %s", r.Stdout)
	}
	t.Log("✓ mount shows (ro)")

	// Verify write fails
	r, _ = execWithTimeout(t, eng, infoRO.ID, []string{"sh", "-c", "touch /data/fail 2>&1; echo exit=$?"})
	if !strings.Contains(r.Stdout, "Read-only file system") {
		t.Fatalf("expected Read-only file system error, got %q", r.Stdout)
	}
	t.Log("✓ write to RO volume rejected: Read-only file system")
}

// TestVolumeMultiplePerSandbox boots a sandbox with 2 volumes and verifies
// both are mounted at the correct paths.
func TestVolumeMultiplePerSandbox(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	volDir := filepath.Join(eng.cfg.DataDir, "volumes", "usr_test")
	os.MkdirAll(volDir, 0700)
	vol1 := filepath.Join(volDir, "integ-multi-1.ext4")
	vol2 := filepath.Join(volDir, "integ-multi-2.ext4")
	defer os.Remove(vol1)
	defer os.Remove(vol2)

	createVolumeFile(t, vol1, 32)
	createVolumeFile(t, vol2, 32)

	spec := testSpec("vol-multi")
	spec.ResolvedVolumes = []engine.ResolvedVolume{
		{FilePath: vol1, DriveID: "vol0", Name: "v1", Mount: "/vol1", ReadOnly: false},
		{FilePath: vol2, DriveID: "vol1", Name: "v2", Mount: "/vol2", ReadOnly: false},
	}

	info, err := eng.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Write different data to each
	execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "echo data-vol1 > /vol1/id.txt"})
	execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "echo data-vol2 > /vol2/id.txt"})

	r1, _ := execWithTimeout(t, eng, info.ID, []string{"cat", "/vol1/id.txt"})
	r2, _ := execWithTimeout(t, eng, info.ID, []string{"cat", "/vol2/id.txt"})

	if !strings.Contains(r1.Stdout, "data-vol1") {
		t.Fatalf("vol1: %q", r1.Stdout)
	}
	if !strings.Contains(r2.Stdout, "data-vol2") {
		t.Fatalf("vol2: %q", r2.Stdout)
	}
	t.Log("✓ two volumes mounted at correct paths with independent data")
}

// TestPersistentVolumeOwnership verifies files in volumes are owned by uid 1000.
func TestPersistentVolumeOwnership(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	volDir := filepath.Join(eng.cfg.DataDir, "volumes", "usr_test")
	os.MkdirAll(volDir, 0700)
	volPath := filepath.Join(volDir, "integ-own.ext4")
	defer os.Remove(volPath)
	createVolumeFile(t, volPath, 32)

	spec := testSpec("vol-own")
	spec.ResolvedVolumes = []engine.ResolvedVolume{{
		FilePath: volPath, DriveID: "vol0", Name: "own",
		Mount: "/workspace", ReadOnly: false,
	}}
	info, err := eng.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	r, _ := execWithTimeout(t, eng, info.ID, []string{"stat", "-c", "%u", "/workspace"})
	if strings.TrimSpace(r.Stdout) != "1000" {
		t.Fatalf("expected uid 1000, got %q", strings.TrimSpace(r.Stdout))
	}
	t.Log("✓ /workspace owned by uid 1000")
}

// TestVolumeSurvivesThermalSnapshot verifies data persists across stop+start
// (thermal snapshot/resume) when a volume is attached.
func TestVolumeSurvivesThermalSnapshot(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	volDir := filepath.Join(eng.cfg.DataDir, "volumes", "usr_test")
	os.MkdirAll(volDir, 0700)
	volPath := filepath.Join(volDir, "integ-thermal.ext4")
	defer os.Remove(volPath)
	createVolumeFile(t, volPath, 64)

	spec := testSpec("vol-thermal")
	spec.ResolvedVolumes = []engine.ResolvedVolume{{
		FilePath: volPath, DriveID: "vol0", Name: "thermal",
		Mount: "/workspace", ReadOnly: false,
	}}
	info, err := eng.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "echo thermal-data > /workspace/t.txt"})

	if err := eng.Stop(ctx, info.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := eng.Start(ctx, info.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}

	r, _ := execWithTimeout(t, eng, info.ID, []string{"cat", "/workspace/t.txt"})
	if !strings.Contains(r.Stdout, "thermal-data") {
		t.Fatalf("data lost after thermal cycle: %q", r.Stdout)
	}
	t.Log("✓ volume data survives stop/start thermal cycle")
}

// TestDiskResize verifies that spec.DiskSizeMB actually expands the rootfs.
func TestDiskResize(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	spec := testSpec("disk-resize")
	spec.DiskSizeMB = 4096 // 4GB

	info, err := eng.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	r, _ := execWithTimeout(t, eng, info.ID, []string{"df", "-m", "/"})
	// Parse df output for root filesystem size
	lines := strings.Split(r.Stdout, "\n")
	if len(lines) < 2 {
		t.Fatalf("unexpected df output: %q", r.Stdout)
	}
	fields := strings.Fields(lines[1])
	if len(fields) < 2 {
		t.Fatalf("unexpected df fields: %q", lines[1])
	}
	var sizeMB int
	fmt.Sscanf(fields[1], "%d", &sizeMB)
	if sizeMB < 3500 { // some overhead, but should be close to 4096
		t.Fatalf("expected ~4096MB rootfs, df shows %dMB", sizeMB)
	}
	t.Logf("✓ rootfs resized to %dMB (requested 4096)", sizeMB)
}

// ==========================================================================
// v0.3 Integration Tests — SaveImage
// ==========================================================================

// TestSaveImageAndBoot saves a running sandbox's rootfs as an image,
// then boots a new sandbox from it and verifies files are present.
func TestSaveImageAndBoot(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	// Create sandbox and write a marker file to the rootfs (NOT /tmp which is tmpfs)
	info1, err := eng.Create(ctx, testSpec("save-img-src"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	execWithTimeout(t, eng, info1.ID, []string{"sh", "-c", "echo saved-marker > /home/lohar/marker.txt"})

	// Save as image
	imgPath := filepath.Join(eng.cfg.DataDir, "images", "test-saved.ext4")
	defer os.Remove(imgPath)

	if err := eng.SaveImage(ctx, info1.ID, imgPath); err != nil {
		eng.Destroy(ctx, info1.ID)
		t.Fatalf("SaveImage: %v", err)
	}
	eng.Destroy(ctx, info1.ID)
	t.Log("✓ saved image from running sandbox")

	// Boot new sandbox from saved image
	spec2 := testSpec("save-img-dst")
	spec2.BaseImage = imgPath

	info2, err := eng.Create(ctx, spec2)
	if err != nil {
		t.Fatalf("Create from saved: %v", err)
	}
	defer eng.Destroy(ctx, info2.ID)

	r, _ := execWithTimeout(t, eng, info2.ID, []string{"cat", "/home/lohar/marker.txt"})
	if !strings.Contains(r.Stdout, "saved-marker") {
		t.Fatalf("marker not found in saved image: %q", r.Stdout)
	}
	t.Log("✓ booted from saved image, marker file present")
}

// TestSaveImageVMContinuesRunning verifies the source VM is resumed
// after save (not left paused).
func TestSaveImageVMContinuesRunning(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("save-continues"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	imgPath := filepath.Join(eng.cfg.DataDir, "images", "test-continues.ext4")
	defer os.Remove(imgPath)

	if err := eng.SaveImage(ctx, info.ID, imgPath); err != nil {
		t.Fatalf("SaveImage: %v", err)
	}

	// VM should still be running and executable
	r, err := execWithTimeout(t, eng, info.ID, []string{"echo", "still-alive"})
	if err != nil || r.ExitCode != 0 {
		t.Fatalf("exec after save: err=%v exit=%d", err, r.ExitCode)
	}
	if !strings.Contains(r.Stdout, "still-alive") {
		t.Fatalf("unexpected output: %q", r.Stdout)
	}
	t.Log("✓ VM continues running after save-as-image")
}

// ==========================================================================
// v0.3 Integration Tests — OCI Image Pull + Boot
// ==========================================================================

// TestOCIPullAndBoot pulls alpine:latest, converts to ext4, boots it,
// and verifies it's actually Alpine.
func TestOCIPullAndBoot(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	loharPath := filepath.Join(eng.cfg.DataDir, "lohar")
	if _, err := os.Stat(loharPath); err != nil {
		t.Skip("lohar binary not found at", loharPath)
	}

	imgPath := filepath.Join(eng.cfg.DataDir, "images", "test-alpine.ext4")
	defer os.Remove(imgPath)

	t.Log("pulling alpine:latest...")
	config, err := oci.PullAndConvert(ctx, "alpine:latest", imgPath, loharPath,
		oci.WithProgress(func(msg string) { t.Logf("  %s", msg) }))
	if err != nil {
		t.Fatalf("PullAndConvert: %v", err)
	}
	t.Logf("✓ pulled alpine (size=%dMB, env keys=%d)", config.TotalSize/1024/1024, len(config.Env))

	// Boot from the pulled image
	spec := testSpec("oci-boot")
	spec.BaseImage = imgPath

	info, err := eng.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Verify it's Alpine
	r, _ := execWithTimeout(t, eng, info.ID, []string{"cat", "/etc/os-release"})
	if !strings.Contains(r.Stdout, "Alpine") {
		t.Fatalf("expected Alpine, got: %s", r.Stdout)
	}
	t.Log("✓ booted Alpine from OCI image")

	// Verify lohar was injected
	r, _ = execWithTimeout(t, eng, info.ID, []string{"ls", "/usr/local/bin/lohar"})
	if r.ExitCode != 0 {
		t.Fatal("lohar not found in OCI image")
	}
	t.Log("✓ lohar injected into OCI image")

	// Verify shell exists (Alpine has /bin/sh)
	r, _ = execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "echo shell-works"})
	if !strings.Contains(r.Stdout, "shell-works") {
		t.Fatalf("shell: %q", r.Stdout)
	}
	t.Log("✓ /bin/sh works in OCI image")
}

// TestVolumeAttachmentInfoInVMState verifies that volume attachment info
// is returned by VMState() and survives RestoreVM().
func TestVolumeAttachmentInfoInVMState(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	volDir := filepath.Join(eng.cfg.DataDir, "volumes", "usr_test")
	os.MkdirAll(volDir, 0700)
	volPath := filepath.Join(volDir, "integ-vmstate.ext4")
	defer os.Remove(volPath)
	createVolumeFile(t, volPath, 32)

	spec := testSpec("vmstate-vol")
	spec.ResolvedVolumes = []engine.ResolvedVolume{{
		FilePath: volPath, DriveID: "vol0", Name: "vmstate-vol",
		Mount: "/workspace", ReadOnly: false,
	}}
	info, err := eng.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	state := eng.VMState(info.ID)
	if state == nil {
		t.Fatal("VMState returned nil")
	}
	vols, ok := state["volumes"]
	if !ok || vols == nil {
		t.Fatal("VMState missing 'volumes' key")
	}
	volList, ok := vols.([]VolumeAttachmentInfo)
	if !ok || len(volList) == 0 {
		t.Fatalf("expected volume attachments, got %T: %v", vols, vols)
	}
	if volList[0].Mount != "/workspace" || volList[0].DriveID != "vol0" {
		t.Fatalf("unexpected attachment: %+v", volList[0])
	}
	t.Logf("✓ VMState includes volume attachment info: %+v", volList[0])
}

// TestEphemeralVolumesStillWork verifies the legacy NewVolumes path
// (ephemeral volumes inside sandbox dir) still functions.
func TestEphemeralVolumesStillWork(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	spec := testSpec("ephemeral-vol")
	spec.NewVolumes = []engine.VolumeSpec{{
		Name: "scratch", SizeMB: 32, Mount: "/scratch",
	}}

	info, err := eng.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	r, _ := execWithTimeout(t, eng, info.ID, []string{"sh", "-c",
		"echo eph-data > /scratch/test.txt && cat /scratch/test.txt"})
	if !strings.Contains(r.Stdout, "eph-data") {
		t.Fatalf("ephemeral volume: %q", r.Stdout)
	}

	// Verify mount shows the volume
	r, _ = execWithTimeout(t, eng, info.ID, []string{"mount"})
	if !strings.Contains(r.Stdout, "/scratch") {
		t.Fatalf("mount doesn't show /scratch: %s", r.Stdout)
	}
	t.Log("✓ legacy ephemeral volumes still work")
}

// ==========================================================================
// v0.3 Integration Tests — Config Drive
// ==========================================================================

// TestConfigDriveLargePayload verifies the mke2fs -d config drive handles
// large payloads (many env vars, volumes).
func TestConfigDriveLargePayload(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	spec := testSpec("large-config")
	spec.Env = make(map[string]string)
	for i := 0; i < 50; i++ {
		spec.Env[fmt.Sprintf("VAR_%03d", i)] = strings.Repeat("x", 200)
	}

	info, err := eng.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create with large config: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Verify one of the env vars made it
	r, _ := execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "echo $VAR_025"})
	expected := strings.Repeat("x", 200)
	if strings.TrimSpace(r.Stdout) != expected {
		t.Fatalf("env var VAR_025: got %d chars, want %d", len(strings.TrimSpace(r.Stdout)), len(expected))
	}
	t.Log("✓ config drive handles 50 env vars (large payload)")
}

// ==========================================================================
// Helpers
// ==========================================================================

func createVolumeFile(t *testing.T, path string, sizeMB int) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Truncate(int64(sizeMB) << 20)
	f.Close()
	if out, err := exec.Command("mkfs.ext4", "-F", "-q", path).CombinedOutput(); err != nil {
		t.Fatalf("mkfs.ext4: %s: %v", out, err)
	}
}
