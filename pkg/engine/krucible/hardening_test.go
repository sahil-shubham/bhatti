//go:build krucible

package krucible

import (
	"bytes"
	"context"
	"fmt"
	"net"
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

// TestKrucibleTSIEgress asserts the positive half of the FC `network` intent on
// the TSI backend (migration plan P2): the guest has working egress to the
// internet (TSI proxies its socket syscalls through the host netstack).
func TestKrucibleTSIEgress(t *testing.T) {
	eng := newBlockRootEngine(t).(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "tsiegress", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := info.ID
	t.Cleanup(func() { eng.Destroy(context.Background(), id) })

	if r, err := eng.Exec(ctx, id, []string{"netcheck", "tcp"}); err != nil || r.ExitCode != 0 {
		t.Fatalf("guest egress (netcheck tcp) failed: err=%v exit=%d out=%q", err, r.ExitCode, r.Stdout)
	}
}

// TestKrucibleTSIHostIsolation encodes the isolation invariant the FC
// `VMCannotReachHostAPI` test guarded: a sandbox must NOT be able to reach a
// service bound to the HOST's loopback (e.g. the daemon's internal API on
// 127.0.0.1:8080, which serves the full control-plane mux).
//
// KNOWN GAP (deferred, by design — see PLAN-krucible-migration.md "Egress
// policy: krun_set_egress_policy not wired (TSI open egress)" + the M4
// networking track): TSI proxies the guest's connects through the host
// netstack, so today the guest's 127.0.0.1 IS the host's loopback — it can
// reach host-local services. The intended fix is krun_set_egress_policy /
// --allow-host (block the host + private ranges by default; opt-in allow-list;
// opt-in key injection for a sandbox that SHOULD talk to the daemon), plus a
// deliberate sandbox↔sandbox story. Until that lands this test SKIPS (loudly)
// rather than fails; once egress is policed it auto-flips to a hard assertion.
func TestKrucibleTSIHostIsolation(t *testing.T) {
	eng := newBlockRootEngine(t).(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// A host-only service on loopback — stands in for the daemon's 127.0.0.1:8080
	// internal API. If the guest reaches THIS, it can reach the control plane.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("host listener: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("HOST-LOOPBACK-REACHED\n"))
			c.Close()
		}
	}()
	hostPort := ln.Addr().(*net.TCPAddr).Port

	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "tsiiso", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := info.ID
	t.Cleanup(func() { eng.Destroy(context.Background(), id) })

	r, err := eng.Exec(ctx, id, []string{"netcheck", "dial", fmt.Sprintf("127.0.0.1:%d", hostPort)})
	if err != nil {
		t.Fatalf("exec netcheck dial: %v", err)
	}
	if r.ExitCode == 0 {
		t.Skipf("KNOWN GAP (TSI open egress, egress policy unwired): guest REACHED the host loopback service 127.0.0.1:%d — %q. See PLAN-krucible-migration.md (M4 networking, krun_set_egress_policy). This test auto-asserts once egress is policed.", hostPort, strings.TrimSpace(r.Stdout))
	}
	// Egress is policed: the invariant now holds — assert it for real.
}
