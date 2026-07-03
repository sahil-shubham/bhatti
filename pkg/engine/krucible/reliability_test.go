//go:build krucible

package krucible

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// These are the krucible-internal cold-tier hardening tests (migration plan P1)
// — the ones that need engine internals to inject a failure or inspect the
// device set, complementing the VMM-agnostic enginetest.RunReliabilitySuite.

// makeVolume creates a standalone ext4 volume image on the host (outlives any
// sandbox) and returns a ResolvedVolume mounting it at mount.
func makeVolume(t *testing.T, name, mount string) engine.ResolvedVolume {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".ext4")
	if out, err := exec.Command("mke2fs", "-t", "ext4", "-F", "-q", path, "64M").CombinedOutput(); err != nil {
		t.Fatalf("mke2fs %s: %v: %s", name, err, out)
	}
	return engine.ResolvedVolume{FilePath: path, DriveID: name, Name: name, Mount: mount}
}

// TestKrucibleSnapshotFailureRecoverable is the FC `VMRecoverableAfterSnapshotFailure`
// behavior on the cold path: if the SNAPSHOT write fails mid-Stop, the guest —
// already PAUSED — must be RESUMEd and left usable, not frozen or half-killed.
// Injection: make the bundle dir unwritable so the helper's memory.img write
// (into it) fails with EACCES after the PAUSE. Deterministic, no disk-fill.
func TestKrucibleSnapshotFailureRecoverable(t *testing.T) {
	eng := newBlockRootEngine(t).(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "snapfail", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := info.ID
	t.Cleanup(func() { eng.Destroy(context.Background(), id) })

	// Prove it's alive before we sabotage the snapshot.
	if r, err := eng.Exec(ctx, id, []string{"echo", "pre-fail"}); err != nil || !strings.Contains(r.Stdout, "pre-fail") {
		t.Fatalf("pre-fail exec: err=%v out=%q", err, r.Stdout)
	}

	// Pre-create the bundle dir read-only so the helper cannot write memory.img
	// into it — SNAPSHOT fails AFTER the PAUSE, exercising the RESUME-on-failure
	// path in Stop. (MkdirAll on an existing dir is a no-op regardless of mode.)
	vm, err := eng.getVM(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(vm.BundleDir, 0o500); err != nil {
		t.Fatalf("pre-create read-only bundle dir: %v", err)
	}
	// Restore perms so Destroy / TempDir teardown can clean up.
	t.Cleanup(func() { os.Chmod(vm.BundleDir, 0o700) })

	// Stop must fail (snapshot couldn't be written)…
	if err := eng.Stop(ctx, id); err == nil {
		t.Fatal("Stop succeeded despite an unwritable bundle dir — expected snapshot failure")
	}
	// …but the VM must stay running (RESUMEd) and fully usable.
	if s, err := eng.Status(ctx, id); err != nil || s.Status != "running" {
		t.Fatalf("post-failure status = %q (err %v), want running (VM should be RESUMEd, not left stopped/frozen)", s.Status, err)
	}
	if r, err := eng.Exec(ctx, id, []string{"echo", "post-fail"}); err != nil || !strings.Contains(r.Stdout, "post-fail") {
		t.Fatalf("exec after recovered snapshot failure: err=%v out=%q (guest left frozen?)", err, r.Stdout)
	}
}

// TestKrucibleMultiVolumeSnapshotOrdering is the FC `RecoveryMultiVolumeOrdering`
// behavior on the bundle path: a memory snapshot of a sandbox with MULTIPLE
// volumes must restore the device set IN ORDER — so vol0→/data0 and vol1→/data1
// stay matched, never swapped, and each carries its own data. A swap would show
// /data1's marker under /data0 (or a missing mount).
func TestKrucibleMultiVolumeSnapshotOrdering(t *testing.T) {
	eng := newBlockRootEngine(t).(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	v0 := makeVolume(t, "vol0", "/data0")
	v1 := makeVolume(t, "vol1", "/data1")

	src, err := eng.Create(ctx, engine.SandboxSpec{
		Name: "mvsrc", CPUs: 1, MemoryMB: 512,
		ResolvedVolumes: []engine.ResolvedVolume{v0, v1},
	})
	if err != nil {
		t.Fatalf("Create with 2 volumes: %v", err)
	}
	t.Cleanup(func() { eng.Destroy(context.Background(), src.ID) })

	wr := func(id, path, content string) {
		t.Helper()
		if err := eng.FileWrite(ctx, id, path, "0644", int64(len(content)), strings.NewReader(content)); err != nil {
			t.Fatalf("FileWrite %s %s: %v (volume not attached/mounted?)", id, path, err)
		}
	}
	rd := func(id, path string) string {
		t.Helper()
		var b bytes.Buffer
		if _, _, err := eng.FileRead(ctx, id, path, &b); err != nil {
			t.Fatalf("FileRead %s %s: %v", id, path, err)
		}
		return strings.TrimSpace(b.String())
	}

	// Distinct markers so a device-order swap is detectable.
	wr(src.ID, "/data0/m", "zero-aaaa")
	wr(src.ID, "/data1/m", "one-bbbb")
	if _, err := eng.Exec(ctx, src.ID, []string{"sync"}); err != nil {
		t.Fatalf("sync src: %v", err)
	}

	// Memory-checkpoint → restore into a new sandbox (the fork machinery, but we
	// keep both so we can compare).
	tmp := t.TempDir()
	manifest, err := eng.Checkpoint(ctx, src.ID, "", 0, "snap", tmp)
	if err != nil {
		t.Fatalf("Checkpoint (memory, 2 volumes): %v", err)
	}
	mjson, _ := json.Marshal(manifest)
	restored, err := eng.ResumeFromManifestJSON(ctx, filepath.Join(tmp, "snap"), mjson, "mvrestore")
	if err != nil {
		t.Fatalf("ResumeFromManifestJSON: %v", err)
	}
	t.Cleanup(func() { eng.Destroy(context.Background(), restored.ID) })

	// Ordering preserved: each mount carries its OWN volume's data.
	if got := rd(restored.ID, "/data0/m"); got != "zero-aaaa" {
		t.Fatalf("/data0/m after restore = %q, want zero-aaaa (device-set order broken)", got)
	}
	if got := rd(restored.ID, "/data1/m"); got != "one-bbbb" {
		t.Fatalf("/data1/m after restore = %q, want one-bbbb (device-set order broken)", got)
	}
}

// TestKrucibleCreateCleansUpOnLaunchFailure is the FC `ResumeCleanupOnAgentTimeout`
// behavior: when a create/launch fails because the agent never comes up, the
// create() cleanup path must leave NOTHING behind — no sandbox dir, no socket
// dir, no registered VM. Injection: a fake helper that starts but never brings
// up the agent, plus a short deadline so WaitReady gives up fast. No hypervisor
// needed (the fake helper isn't a real VM), only mke2fs for the block root.
func TestKrucibleCreateCleansUpOnLaunchFailure(t *testing.T) {
	if _, err := exec.LookPath("mke2fs"); err != nil {
		t.Skip("mke2fs not found; skipping")
	}
	repo := repoRoot(t)
	dataDir := t.TempDir()
	sockDir := t.TempDir()

	// Fake helper: services the `create-overlay` subcommand (so create() gets past
	// disk prep), but on the VM-run invocation just sleeps — the agent socket never
	// appears, so WaitReady must give up at the caller's deadline and create() must
	// clean up. Distinguishing the two invocations is essential: create() shells to
	// the SAME binary for create-overlay, so a blanket sleep would wedge disk prep.
	fake := filepath.Join(t.TempDir(), "fake-vmm")
	script := "#!/bin/sh\ncase \"$1\" in\n  create-overlay) : > \"$2\"; exit 0 ;;\n  *) exec sleep 30 ;;\nesac\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	eng, err := New(Config{
		DataDir:    dataDir,
		SocketDir:  sockDir,
		BaseRootfs: buildBaseRootfs(t, repo),
		VMMBinary:  fake,
		BlockRoot:  true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Short deadline: Create should abort readiness at ~ctx, not block for the
	// internal 30s WaitReady floor. Measure it — a create that ignores the
	// caller's deadline is a reliability bug (a stuck helper wedges the request).
	const ctxTimeout = 6 * time.Second
	cctx, ccancel := context.WithTimeout(context.Background(), ctxTimeout)
	defer ccancel()
	start := time.Now()
	if _, err := eng.Create(cctx, engine.SandboxSpec{Name: "doomed", CPUs: 1, MemoryMB: 512}); err == nil {
		t.Fatal("Create succeeded with a fake helper that never readies the agent")
	}
	if elapsed := time.Since(start); elapsed > ctxTimeout+12*time.Second {
		t.Errorf("Create took %s with a %s deadline — readiness wait ignores the caller's ctx", elapsed, ctxTimeout)
	}

	// Nothing registered.
	if list, err := eng.List(context.Background()); err != nil {
		t.Fatalf("List: %v", err)
	} else if len(list) != 0 {
		t.Fatalf("List has %d entries after a failed Create, want 0 (leaked VM)", len(list))
	}
	// No sandbox dir left on disk.
	entries, _ := os.ReadDir(filepath.Join(dataDir, "sandboxes"))
	if len(entries) != 0 {
		t.Fatalf("%d leftover sandbox dir(s) after a failed Create, want 0: %v", len(entries), entries)
	}
	// No socket dir left on disk.
	socks, _ := os.ReadDir(sockDir)
	if len(socks) != 0 {
		t.Fatalf("%d leftover socket dir(s) after a failed Create, want 0", len(socks))
	}
}

// TestKrucibleCheckpointDuplicateNameRefused pins that Checkpoint refuses to
// overwrite an existing named snapshot (FC `CheckpointDuplicateName`) — a
// clobber would silently corrupt a restore point.
func TestKrucibleCheckpointDuplicateNameRefused(t *testing.T) {
	eng := newBlockRootEngine(t).(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "dupsnap", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := info.ID
	t.Cleanup(func() { eng.Destroy(context.Background(), id) })

	snapDir := t.TempDir()
	if _, err := eng.Checkpoint(ctx, id, "", 0, "snap1", snapDir); err != nil {
		t.Fatalf("first Checkpoint: %v", err)
	}
	if _, err := eng.Checkpoint(ctx, id, "", 0, "snap1", snapDir); err == nil {
		t.Fatal("second Checkpoint with the same name succeeded — should be refused (would clobber)")
	}
}

// TestKrucibleConcurrentCheckpointAndDestroy guards launchMu serialization on the
// checkpoint path (FC `ConcurrentCheckpointAndStop`): a checkpoint racing a
// destroy must not panic, deadlock, or leak a helper — one serializes after the
// other and the sandbox ends up gone with no orphaned bhatti-vmm.
func TestKrucibleConcurrentCheckpointAndDestroy(t *testing.T) {
	eng := newBlockRootEngine(t).(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "ccd", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := info.ID

	snapDir := t.TempDir()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = eng.Checkpoint(ctx, id, "", 0, "snap", snapDir) }()
	go func() { defer wg.Done(); _ = eng.Destroy(ctx, id) }()
	wg.Wait()

	// Ensure it's gone either way, then assert no orphaned helper for this id.
	_ = eng.Destroy(context.Background(), id)
	if n := countHelpers(t, id); n != 0 {
		t.Fatalf("%d orphaned helper(s) for %s after concurrent checkpoint+destroy, want 0", n, id)
	}
}
