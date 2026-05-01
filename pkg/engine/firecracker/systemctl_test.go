//go:build linux

package firecracker

import (
	"context"
	"strings"
	"testing"
	"time"
)

// These tests run on real Firecracker VMs on the Pi cluster.
// They require the systemctl shim to be baked into the rootfs
// (/usr/bin/systemctl -> /usr/local/bin/lohar).
//
// Privilege model: lohar exec runs as uid 1000 (lohar user).
// Package installs and service management need root — use sudo,
// same as on any Linux server. Read-only queries (is-active,
// status, show) work as uid 1000.

func TestSystemctlBasicCommands(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("sctl-basic"))
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Read-only commands — no sudo needed.
	assertExecOutput(t, eng, info.ID, "systemctl is-system-running", "running")

	execOrFail(t, eng, info.ID, "systemctl daemon-reload")

	// invoke-rc.d checks these targets to determine runlevel.
	assertExecOutput(t, eng, info.ID, "systemctl is-active sysinit.target", "active")
	assertExecOutput(t, eng, info.ID, "systemctl is-active multi-user.target", "active")
}

func TestSystemctlInstallOpenssh(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	spec := testSpec("sctl-ssh")
	spec.DiskSizeMB = 2048
	info, err := eng.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Package install needs root — sudo, like any Linux server.
	execOrFail(t, eng, info.ID, "sudo apt-get update -qq")
	execOrFail(t, eng, info.ID, "sudo apt-get install -y --no-install-recommends openssh-server")

	// invoke-rc.d runs as root during install, so it calls our shim as root.
	// Service may or may not be started depending on invoke-rc.d's checks.
	// If not started during install, start manually.
	r := execCmd(t, eng, info.ID, "systemctl is-active ssh")
	if strings.TrimSpace(r.Stdout) != "active" {
		execOrFail(t, eng, info.ID, "sudo systemctl start ssh")
	}

	// Read-only checks — no sudo needed.
	assertExecOutput(t, eng, info.ID, "systemctl is-active ssh", "active")

	r = execCmd(t, eng, info.ID, "ss -tln")
	if !strings.Contains(r.Stdout, ":22") {
		t.Fatalf("sshd not listening on port 22: %s", r.Stdout)
	}

	assertExecOutput(t, eng, info.ID, "systemctl is-enabled ssh", "enabled")

	// Service management needs root.
	execOrFail(t, eng, info.ID, "sudo systemctl stop ssh")
	assertExecOutput(t, eng, info.ID, "systemctl is-active ssh", "inactive")

	execOrFail(t, eng, info.ID, "sudo systemctl start ssh")
	assertExecOutput(t, eng, info.ID, "systemctl is-active ssh", "active")

	execOrFail(t, eng, info.ID, "sudo systemctl restart ssh")
	assertExecOutput(t, eng, info.ID, "systemctl is-active ssh", "active")
}

func TestSystemctlServiceSurvivesSnapshot(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	spec := testSpec("sctl-snap")
	spec.DiskSizeMB = 2048
	info, err := eng.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Destroy(ctx, info.ID)

	execOrFail(t, eng, info.ID, "sudo apt-get update -qq")
	execOrFail(t, eng, info.ID, "sudo apt-get install -y --no-install-recommends openssh-server")
	execOrFail(t, eng, info.ID, "sudo systemctl start ssh")
	assertExecOutput(t, eng, info.ID, "systemctl is-active ssh", "active")

	if err := eng.Stop(ctx, info.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if err := eng.Start(ctx, info.ID); err != nil {
		t.Fatalf("start: %v", err)
	}

	// lohar restarts enabled services on boot.
	assertExecOutput(t, eng, info.ID, "systemctl is-active ssh", "active")
	r := execCmd(t, eng, info.ID, "ss -tln")
	if !strings.Contains(r.Stdout, ":22") {
		t.Fatalf("sshd not listening after restore: %s", r.Stdout)
	}
}

func TestSystemctlShow(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	spec := testSpec("sctl-show")
	spec.DiskSizeMB = 2048
	info, err := eng.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Destroy(ctx, info.ID)

	execOrFail(t, eng, info.ID, "sudo apt-get update -qq")
	execOrFail(t, eng, info.ID, "sudo apt-get install -y --no-install-recommends openssh-server")

	// show is read-only — no sudo needed.
	r := execCmd(t, eng, info.ID, "systemctl -p LoadState show ssh")
	if !strings.Contains(r.Stdout, "LoadState=loaded") {
		t.Errorf("LoadState: %s", r.Stdout)
	}

	r = execCmd(t, eng, info.ID, "systemctl -p LoadState show nonexistent")
	if !strings.Contains(r.Stdout, "not-found") {
		t.Errorf("nonexistent LoadState: %s", r.Stdout)
	}

	r = execCmd(t, eng, info.ID, "systemctl show --value --property SourcePath ssh")
	if !strings.Contains(r.Stdout, "ssh.service") {
		t.Errorf("SourcePath: %s", r.Stdout)
	}
}

func TestSystemctlJournalctl(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	spec := testSpec("sctl-journal")
	spec.DiskSizeMB = 2048
	info, err := eng.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Destroy(ctx, info.ID)

	execOrFail(t, eng, info.ID, "sudo apt-get update -qq")
	execOrFail(t, eng, info.ID, "sudo apt-get install -y --no-install-recommends openssh-server")
	execOrFail(t, eng, info.ID, "sudo systemctl start ssh")

	time.Sleep(1 * time.Second)

	// journalctl reads log files — no sudo needed.
	r := execCmd(t, eng, info.ID, "journalctl -u ssh -n 5")
	if r.ExitCode != 0 {
		t.Logf("journalctl exit=%d stdout=%q stderr=%q", r.ExitCode, r.Stdout, r.Stderr)
	}
}

// TestSystemctlAliasUnification is the integration-level regression test
// for issue #12. The bug Fastidious reported was: ssh.service has
// [Install] Alias=sshd.service, the daemon was started with `systemctl
// start ssh`, but `systemctl status sshd` reported "inactive" while
// `status ssh` reported "active" — same daemon, two answers. Worse,
// `systemctl stop sshd` was a silent no-op while the daemon kept
// running.
//
// The unit tests (TestRegistryAliasResolution, TestUnitStateUnification)
// lock this in at the data-model level. This test locks it in at the
// user-visible level: a real openssh-server install on a real Firecracker
// VM, both alias forms exercised end-to-end, with the most dangerous
// case (stop-by-alias actually stops the listener) verified.
func TestSystemctlAliasUnification(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	spec := testSpec("sctl-alias")
	spec.DiskSizeMB = 2048
	info, err := eng.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Destroy(ctx, info.ID)

	execOrFail(t, eng, info.ID, "sudo apt-get update -qq")
	execOrFail(t, eng, info.ID, "sudo apt-get install -y --no-install-recommends openssh-server")
	execOrFail(t, eng, info.ID, "sudo systemctl start ssh")

	// Both name forms must report the SAME state. Pre-fix this is where
	// the user saw the discrepancy.
	assertExecOutput(t, eng, info.ID, "systemctl is-active ssh", "active")
	assertExecOutput(t, eng, info.ID, "systemctl is-active sshd", "active")

	// status output by both names must show the same Active line.
	rCanon := execCmd(t, eng, info.ID, "systemctl status ssh")
	rAlias := execCmd(t, eng, info.ID, "systemctl status sshd")
	if !strings.Contains(rCanon.Stdout, "Active: active") {
		t.Errorf("status ssh did not show Active: active\n%s", rCanon.Stdout)
	}
	if !strings.Contains(rAlias.Stdout, "Active: active") {
		t.Errorf("status sshd (alias) did not show Active: active — the issue #12 bug is back\n%s", rAlias.Stdout)
	}

	// Stop by alias name MUST actually stop the daemon. Pre-fix this
	// returned exit 0 silently while the daemon kept listening on :22.
	execOrFail(t, eng, info.ID, "sudo systemctl stop sshd")

	// Verify both names report inactive AND port 22 is no longer
	// listening. The port check is the user-visible truth that
	// distinguishes a real fix from one that just updates internal
	// bookkeeping.
	assertExecOutput(t, eng, info.ID, "systemctl is-active ssh", "inactive")
	assertExecOutput(t, eng, info.ID, "systemctl is-active sshd", "inactive")
	rPorts := execCmd(t, eng, info.ID, "ss -tln")
	if strings.Contains(rPorts.Stdout, ":22") {
		t.Errorf("port 22 still listening after `stop sshd` — stop-by-alias was a silent no-op (issue #12 regression)\nss output: %s", rPorts.Stdout)
	}

	// And the inverse direction: start by alias must actually start.
	execOrFail(t, eng, info.ID, "sudo systemctl start sshd")
	assertExecOutput(t, eng, info.ID, "systemctl is-active ssh", "active")
	rPorts = execCmd(t, eng, info.ID, "ss -tln")
	if !strings.Contains(rPorts.Stdout, ":22") {
		t.Errorf("port 22 not listening after `start sshd`: %s", rPorts.Stdout)
	}
}

// TestSystemctlRestartOnFailureUnderVM exercises C6's Restart= policy on
// real Firecracker. A unit that exits non-zero with Restart=on-failure
// must be auto-restarted by the watcher goroutine; StartLimitBurst
// must eventually give up. Unit tests cover this with /bin/false; this
// test runs it inside a VM to catch any goroutine/timer interactions
// that only manifest under the real lohar runtime.
func TestSystemctlRestartOnFailureUnderVM(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	spec := testSpec("sctl-restart")
	info, err := eng.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Drop a tiny crashing unit. It exits 1 immediately. Restart=on-failure
	// + RestartSec=10ms means the watcher hits StartLimitBurst (default 5)
	// in well under a second.
	unitFile := `[Unit]
Description=Test crasher

[Service]
Type=simple
ExecStart=/bin/false
Restart=on-failure
RestartSec=10ms
StartLimitBurst=3
StartLimitIntervalSec=10
`
	execOrFail(t, eng, info.ID,
		"sudo sh -c 'cat > /etc/systemd/system/crasher.service' <<EOF\n"+unitFile+"EOF")
	execOrFail(t, eng, info.ID, "sudo systemctl start crasher")

	// Give the watcher time to hit the burst limit and give up.
	time.Sleep(2 * time.Second)

	// is-failed must return "failed" (exit 0), not "inactive" (exit 1).
	r := execCmd(t, eng, info.ID, "systemctl is-failed crasher")
	if strings.TrimSpace(r.Stdout) != "failed" {
		t.Errorf("is-failed crasher = %q (exit %d), want \"failed\" — watcher didn't mark the failed marker",
			strings.TrimSpace(r.Stdout), r.ExitCode)
	}

	// status should show "Active: failed" and the exit code.
	rStatus := execCmd(t, eng, info.ID, "systemctl status crasher")
	if !strings.Contains(rStatus.Stdout, "Active: failed") {
		t.Errorf("status crasher did not show Active: failed\n%s", rStatus.Stdout)
	}

	// reset-failed clears the marker.
	execOrFail(t, eng, info.ID, "sudo systemctl reset-failed crasher")
	rAfter := execCmd(t, eng, info.ID, "systemctl is-failed crasher")
	if strings.TrimSpace(rAfter.Stdout) == "failed" {
		t.Errorf("is-failed crasher after reset-failed still says failed: %q", rAfter.Stdout)
	}
}

// TestSystemctlPrivilegeBoundary exercises C4's IPC dispatch from the
// non-root caller's perspective: a privileged op without sudo must fail
// with "Access denied", not silently succeed (the EPERM-swallow bug from
// v1.10.x). The unit test TestIPCServerAccessDenied covers the server
// side; this covers the integrated round-trip from the client running
// inside a real VM.
func TestSystemctlPrivilegeBoundary(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	spec := testSpec("sctl-priv")
	spec.DiskSizeMB = 2048
	info, err := eng.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Destroy(ctx, info.ID)

	execOrFail(t, eng, info.ID, "sudo apt-get update -qq")
	execOrFail(t, eng, info.ID, "sudo apt-get install -y --no-install-recommends openssh-server")
	execOrFail(t, eng, info.ID, "sudo systemctl start ssh")

	// `bhatti exec` runs as uid 1000 (lohar user). Calling `systemctl
	// stop` WITHOUT sudo must:
	//   1. Return non-zero exit code (was: silently exit 0 in v1.10.x)
	//   2. Stderr contains "Access denied" (matches systemd polkit wording)
	//   3. The daemon must STILL be running (the dangerous case)
	r := execCmd(t, eng, info.ID, "systemctl stop ssh")
	if r.ExitCode == 0 {
		t.Errorf("non-root `systemctl stop ssh` exited 0; expected non-zero (the silent-EPERM-swallow bug is back)")
	}
	if !strings.Contains(r.Stderr, "Access denied") {
		t.Errorf("non-root stop stderr should contain 'Access denied' (matches systemd's polkit wording), got: %q", r.Stderr)
	}

	// Most important: the daemon is STILL running. The integration cost
	// of getting this wrong is exactly Fastidious's class of
	// hours-of-debugging bug — admin runs stop, daemon is still up,
	// admin assumes their command worked.
	assertExecOutput(t, eng, info.ID, "systemctl is-active ssh", "active")
	rPorts := execCmd(t, eng, info.ID, "ss -tln")
	if !strings.Contains(rPorts.Stdout, ":22") {
		t.Errorf("port 22 not listening — non-root stop somehow killed it: %s", rPorts.Stdout)
	}

	// Sanity: with sudo, the same call works. Confirms the IPC path is
	// actually engaged for root (not silently bypassed).
	execOrFail(t, eng, info.ID, "sudo systemctl stop ssh")
	assertExecOutput(t, eng, info.ID, "systemctl is-active ssh", "inactive")
}

func TestSystemctlThermalCycles(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	spec := testSpec("sctl-thermal")
	spec.DiskSizeMB = 2048
	info, err := eng.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Destroy(ctx, info.ID)

	execOrFail(t, eng, info.ID, "sudo apt-get update -qq")
	execOrFail(t, eng, info.ID, "sudo apt-get install -y --no-install-recommends openssh-server")
	execOrFail(t, eng, info.ID, "sudo systemctl start ssh")

	for i := 0; i < 3; i++ {
		if err := eng.Stop(ctx, info.ID); err != nil {
			t.Fatalf("stop cycle %d: %v", i, err)
		}
		if err := eng.Start(ctx, info.ID); err != nil {
			t.Fatalf("start cycle %d: %v", i, err)
		}
		assertExecOutput(t, eng, info.ID, "systemctl is-active ssh", "active")
	}
}

// --- Test helpers ---

func execCmd(t *testing.T, eng *Engine, id, cmd string) struct {
	Stdout   string
	Stderr   string
	ExitCode int
} {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	r, err := eng.Exec(ctx, id, strings.Fields(cmd))
	if err != nil {
		t.Fatalf("exec %q: %v", cmd, err)
	}
	return struct {
		Stdout   string
		Stderr   string
		ExitCode int
	}{r.Stdout, r.Stderr, r.ExitCode}
}

func execOrFail(t *testing.T, eng *Engine, id, cmd string) {
	t.Helper()
	r := execCmd(t, eng, id, cmd)
	if r.ExitCode != 0 {
		t.Fatalf("exec %q: exit %d\nstdout: %s\nstderr: %s", cmd, r.ExitCode, r.Stdout, r.Stderr)
	}
}

func assertExecOutput(t *testing.T, eng *Engine, id, cmd, want string) {
	t.Helper()
	r := execCmd(t, eng, id, cmd)
	got := strings.TrimSpace(r.Stdout)
	if got != want {
		t.Errorf("exec %q: got %q, want %q (exit=%d stderr=%q)",
			cmd, got, want, r.ExitCode, r.Stderr)
	}
}
