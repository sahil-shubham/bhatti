//go:build krucible

package krucible

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// Guest-hardening + init behaviors (migration plan P2). These assert at the
// krucible VM level what lohar's unit tests can only assert in test-mode.

// pollFile reads a guest file until it has the wanted content or the deadline
// passes (init runs asynchronously after the agent is ready).
func pollFile(t *testing.T, eng *Engine, ctx context.Context, id, path string, timeout time.Duration) (string, bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var b bytes.Buffer
		if _, _, err := eng.FileRead(ctx, id, path, &b); err == nil && b.Len() > 0 {
			return strings.TrimSpace(b.String()), true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return "", false
}

// TestKrucibleInitScriptRunsAsUser is the FC `InitScriptRunsAsUser` behavior:
// `create --init "<cmd>"` runs the command once after boot, AS the sandbox user
// (uid 1000) — not root. It regression-guards the bug where krucible dropped
// spec.Init from the config drive entirely (--init was a silent no-op).
//
// The init command writes the caller's uid to a file; we read it back and assert
// it ran (file exists) AND ran as uid 1000 (content).
func TestKrucibleInitScriptRunsAsUser(t *testing.T) {
	eng := newBlockRootEngine(t).(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	info, err := eng.Create(ctx, engine.SandboxSpec{
		Name: "initrun", CPUs: 1, MemoryMB: 512,
		Init: "writeuid /tmp/init.uid",
	})
	if err != nil {
		t.Fatalf("Create --init: %v", err)
	}
	id := info.ID
	t.Cleanup(func() { eng.Destroy(context.Background(), id) })

	got, ok := pollFile(t, eng, ctx, id, "/tmp/init.uid", 20*time.Second)
	if !ok {
		t.Fatal("init command never ran (/tmp/init.uid absent) — --init dropped from the config drive?")
	}
	if got != "1000" {
		t.Fatalf("init ran as uid %q, want 1000 (should run as the sandbox user, not root)", got)
	}
}

// TestKrucibleExecRunsAsUid1000 pins the FC `ExecRunsAsUser`/`part4` behavior:
// the exec surface runs commands as uid 1000, so an escaped process can't act as
// root by default (sudo is the explicit escalation path).
func TestKrucibleExecRunsAsUid1000(t *testing.T) {
	eng := newBlockRootEngine(t).(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "execuid", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := info.ID
	t.Cleanup(func() { eng.Destroy(context.Background(), id) })

	if _, err := eng.Exec(ctx, id, []string{"writeuid", "/tmp/exec.uid"}); err != nil {
		t.Fatalf("exec writeuid: %v", err)
	}
	var b bytes.Buffer
	if _, _, err := eng.FileRead(ctx, id, "/tmp/exec.uid", &b); err != nil {
		t.Fatalf("read exec uid: %v", err)
	}
	if got := strings.TrimSpace(b.String()); got != "1000" {
		t.Fatalf("exec ran as uid %q, want 1000", got)
	}
}

// TestKrucibleConfigDriveUnmountedAfterBoot is the FC `ConfigDriveUnmounted`
// guest-hardening behavior: after lohar applies the config drive it unmounts +
// removes /run/bhatti/config, so the in-guest auth token (and the rest of the
// boot config) isn't left readable to sandbox processes.
func TestKrucibleConfigDriveUnmountedAfterBoot(t *testing.T) {
	eng := newBlockRootEngine(t).(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "cdunmount", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := info.ID
	t.Cleanup(func() { eng.Destroy(context.Background(), id) })

	// The mount point (and its config.json) must be gone.
	if _, err := eng.FileStat(ctx, id, "/run/bhatti/config/config.json"); err == nil {
		t.Fatal("/run/bhatti/config/config.json still present — config drive not unmounted/removed (token exposed)")
	}
	if _, err := eng.FileStat(ctx, id, "/run/bhatti/config"); err == nil {
		t.Fatal("/run/bhatti/config still present after boot — config drive mount not cleaned up")
	}
}
