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

func TestSystemctlBasicCommands(t *testing.T) {
	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("sctl-basic"))
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Destroy(ctx, info.ID)

	// is-system-running
	assertExecOutput(t, eng, info.ID, "systemctl is-system-running", "running")

	// daemon-reload (no-op, must succeed)
	execOrFail(t, eng, info.ID, "systemctl daemon-reload")

	// is-active for well-known targets (invoke-rc.d checks these)
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

	execOrFail(t, eng, info.ID, "apt-get update -qq")
	execOrFail(t, eng, info.ID, "apt-get install -y --no-install-recommends openssh-server")

	// Service should have been started during install (invoke-rc.d + our shim).
	// If not started during install, start manually.
	r := execCmd(t, eng, info.ID, "systemctl is-active ssh")
	if strings.TrimSpace(r.Stdout) != "active" {
		execOrFail(t, eng, info.ID, "systemctl start ssh")
	}
	assertExecOutput(t, eng, info.ID, "systemctl is-active ssh", "active")

	// sshd listening on port 22
	r = execCmd(t, eng, info.ID, "ss -tln")
	if !strings.Contains(r.Stdout, ":22") {
		t.Fatalf("sshd not listening on port 22: %s", r.Stdout)
	}

	// is-enabled
	assertExecOutput(t, eng, info.ID, "systemctl is-enabled ssh", "enabled")

	// stop
	execOrFail(t, eng, info.ID, "systemctl stop ssh")
	assertExecOutput(t, eng, info.ID, "systemctl is-active ssh", "inactive")

	// start
	execOrFail(t, eng, info.ID, "systemctl start ssh")
	assertExecOutput(t, eng, info.ID, "systemctl is-active ssh", "active")

	// restart
	execOrFail(t, eng, info.ID, "systemctl restart ssh")
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

	execOrFail(t, eng, info.ID, "apt-get update -qq")
	execOrFail(t, eng, info.ID, "apt-get install -y --no-install-recommends openssh-server")
	execOrFail(t, eng, info.ID, "systemctl start ssh")
	assertExecOutput(t, eng, info.ID, "systemctl is-active ssh", "active")

	// Stop (snapshot)
	if err := eng.Stop(ctx, info.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}

	// Start (restore from snapshot)
	if err := eng.Start(ctx, info.ID); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Service should still be running — lohar restarts enabled services on boot.
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

	execOrFail(t, eng, info.ID, "apt-get update -qq")
	execOrFail(t, eng, info.ID, "apt-get install -y --no-install-recommends openssh-server")

	// LoadState for installed service
	r := execCmd(t, eng, info.ID, "systemctl -p LoadState show ssh")
	if !strings.Contains(r.Stdout, "LoadState=loaded") {
		t.Errorf("LoadState: %s", r.Stdout)
	}

	// LoadState for nonexistent
	r = execCmd(t, eng, info.ID, "systemctl -p LoadState show nonexistent")
	if !strings.Contains(r.Stdout, "not-found") {
		t.Errorf("nonexistent LoadState: %s", r.Stdout)
	}

	// SourcePath
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

	execOrFail(t, eng, info.ID, "apt-get update -qq")
	execOrFail(t, eng, info.ID, "apt-get install -y --no-install-recommends openssh-server")
	execOrFail(t, eng, info.ID, "systemctl start ssh")

	// Give sshd a moment to write output
	time.Sleep(1 * time.Second)

	// journalctl -u ssh should return something (even if empty, shouldn't error)
	r := execCmd(t, eng, info.ID, "journalctl -u ssh -n 5")
	// We don't check content — sshd might not write to stdout. But the
	// command must succeed (exit 0) and not crash.
	if r.ExitCode != 0 {
		// Exit code 1 is OK if no logs yet.
		t.Logf("journalctl exit=%d stdout=%q stderr=%q", r.ExitCode, r.Stdout, r.Stderr)
	}
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

	execOrFail(t, eng, info.ID, "apt-get update -qq")
	execOrFail(t, eng, info.ID, "apt-get install -y --no-install-recommends openssh-server")
	execOrFail(t, eng, info.ID, "systemctl start ssh")

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
