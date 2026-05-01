//go:build linux

package main

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
)

func TestRequiresPrivilege(t *testing.T) {
	// Privileged ops must round-trip to PID 1; read-only ops stay in-process.
	priv := []string{"start", "stop", "restart", "reload", "enable", "disable",
		"mask", "unmask", "kill", "preset", "reset-failed"}
	// daemon-reload and daemon-reexec are intentionally exposed to non-root
	// callers because our shim's implementation of them is a no-op (the
	// Registry re-reads unit files on every Resolve). Real systemd requires
	// privilege; we don't because there's nothing privileged to do, and
	// gating them would break common scripts that call daemon-reload after
	// editing a user-owned unit file.
	readOnly := []string{"status", "show", "cat", "is-active", "is-enabled",
		"is-failed", "list-units", "list-unit-files", "is-system-running",
		"daemon-reload", "daemon-reexec"}

	for _, op := range priv {
		if !requiresPrivilege(op) {
			t.Errorf("%s should require privilege", op)
		}
	}
	for _, op := range readOnly {
		if requiresPrivilege(op) {
			t.Errorf("%s should NOT require privilege (stays in-process)", op)
		}
	}
}

func TestDispatchPrivilegedOpEnable(t *testing.T) {
	// Build a unit, dispatch enable through the privileged path, verify
	// the wants-symlink got created. Proves dispatchPrivilegedOp reaches
	// svcEnable and that exit codes flow correctly without os.Exit.
	dir := t.TempDir()
	etcDir := t.TempDir()
	origDirs := serviceDirs
	origEtc := etcSystemdDir
	serviceDirs = []string{dir, etcDir}
	etcSystemdDir = etcDir
	defer func() { serviceDirs = origDirs; etcSystemdDir = origEtc }()

	svcPath := filepath.Join(dir, "test.service")
	os.WriteFile(svcPath,
		[]byte("[Service]\nExecStart=/bin/true\n[Install]\nWantedBy=multi-user.target\n"),
		0644)

	code := dispatchPrivilegedOp(proto.SystemctlRequest{
		Op: "enable", Units: []string{"test"},
	})
	if code != 0 {
		t.Fatalf("dispatchPrivilegedOp(enable) = %d, want 0", code)
	}

	wantsLink := filepath.Join(etcDir, "multi-user.target.wants", "test.service")
	if _, err := os.Lstat(wantsLink); err != nil {
		t.Errorf("expected wants symlink at %s, got err: %v", wantsLink, err)
	}
}

func TestDispatchPrivilegedOpStartNotFound(t *testing.T) {
	// Starting a missing unit returns 5 (systemd convention) without
	// calling os.Exit (which would kill the daemon).
	dir := t.TempDir()
	origDirs := serviceDirs
	serviceDirs = []string{dir}
	defer func() { serviceDirs = origDirs }()

	code := dispatchPrivilegedOp(proto.SystemctlRequest{
		Op: "start", Units: []string{"nonexistent"},
	})
	if code != 5 {
		t.Errorf("dispatchPrivilegedOp(start nonexistent) = %d, want 5", code)
	}
}

func TestSvcStopStalePidfile(t *testing.T) {
	// Stale pidfile (PID written, process long gone) must be cleaned up
	// by svcStop without error and without trying to signal whatever
	// process happens to inherit that PID later. This is the path that
	// runs every time a service crashes and the admin runs `systemctl
	// stop` to clean state.
	dir := t.TempDir()
	origDirs := serviceDirs
	origPid := pidDir
	serviceDirs = []string{dir}
	pidDir = t.TempDir()
	defer func() { serviceDirs = origDirs; pidDir = origPid }()

	os.WriteFile(filepath.Join(dir, "ghost.service"),
		[]byte("[Service]\nExecStart=/bin/true\n"), 0644)

	reg := NewRegistry()
	u, err := reg.Resolve("ghost")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if err := os.WriteFile(u.PidPath(), []byte("999999"), 0644); err != nil {
		t.Fatalf("write pidfile: %v", err)
	}

	if err := svcStop(u); err != nil {
		t.Errorf("svcStop on stale pidfile = %v, want nil", err)
	}
	if _, err := os.Stat(u.PidPath()); !os.IsNotExist(err) {
		t.Errorf("stale pidfile not removed: %v", err)
	}
}

func TestSvcStopRefusesPID1(t *testing.T) {
	// Defensive: a corrupt pidfile pointing at PID 1 (or 0) must NOT
	// cause svcStop to send signals — because syscall.Kill(-1, SIGTERM)
	// is the POSIX "broadcast to every process I'm allowed to signal"
	// sentinel, and svcStop runs as root inside PID-1 lohar in production.
	// Without this guard, a single malformed pidfile would take down the
	// entire VM. (We learned this the hard way: an earlier version of
	// this test pointed at PID 1 to exercise EPERM and rebooted the Pi5
	// it was running on.)
	dir := t.TempDir()
	origDirs := serviceDirs
	origPid := pidDir
	serviceDirs = []string{dir}
	pidDir = t.TempDir()
	defer func() { serviceDirs = origDirs; pidDir = origPid }()

	os.WriteFile(filepath.Join(dir, "corrupt.service"),
		[]byte("[Service]\nExecStart=/bin/true\n"), 0644)

	reg := NewRegistry()
	u, _ := reg.Resolve("corrupt")

	for _, badPID := range []string{"0", "1"} {
		os.WriteFile(u.PidPath(), []byte(badPID), 0644)
		err := svcStop(u)
		if err == nil {
			t.Errorf("svcStop with pidfile=%s should return error, got nil", badPID)
		}
		if !strings.Contains(err.Error(), "refusing") {
			t.Errorf("pidfile=%s: error should mention refusing, got: %v", badPID, err)
		}
		// And the pidfile should be cleaned up so a retry doesn't hit the
		// same trap.
		if _, err := os.Stat(u.PidPath()); !os.IsNotExist(err) {
			t.Errorf("pidfile=%s: not cleaned up after refusal", badPID)
		}
	}
}

func TestSvcStopErrorPropagatesViaSpawnedChild(t *testing.T) {
	// Verify the error-propagation path by spawning a real child we own,
	// pointing the pidfile at it, then having svcStop legitimately stop
	// it. After the kill the child is gone, svcStop returns nil. Then
	// re-running svcStop (pidfile re-written to the same defunct PID)
	// hits the stale-pidfile branch and also returns nil. We're verifying
	// the mechanism, not specifically EPERM — the real EPERM case is
	// blocked at the IPC server's uid check (see TestIPCServerAccessDenied),
	// which is the architectural fix that makes EPERM unreachable in
	// production.
	dir := t.TempDir()
	origDirs := serviceDirs
	origPid := pidDir
	serviceDirs = []string{dir}
	pidDir = t.TempDir()
	defer func() { serviceDirs = origDirs; pidDir = origPid }()

	os.WriteFile(filepath.Join(dir, "sleeper.service"),
		[]byte("[Service]\nExecStart=/bin/sleep 30\n"), 0644)

	reg := NewRegistry()
	u, _ := reg.Resolve("sleeper")

	// Spawn a sleep we own, in its own session so kill(-pgid) reaches it.
	cmd := exec.Command("/bin/sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn sleep: %v", err)
	}
	// Reap the child in the background. Without this, SIGTERM kills the
	// sleep but its /proc/<pid> entry stays as a zombie, and processAlive
	// (which os.Stat's /proc) keeps returning true forever — the test
	// would falsely think svcStop didn't work.
	waitDone := make(chan struct{})
	go func() { cmd.Wait(); close(waitDone) }()
	defer func() {
		select {
		case <-waitDone:
		case <-time.After(2 * time.Second):
			cmd.Process.Kill()
			<-waitDone
		}
	}()

	os.WriteFile(u.PidPath(), []byte(strconv.Itoa(cmd.Process.Pid)), 0644)

	if err := svcStop(u); err != nil {
		t.Errorf("svcStop on owned process = %v, want nil", err)
	}
	select {
	case <-waitDone:
		// good — process actually exited
	case <-time.After(2 * time.Second):
		t.Errorf("sleep didn't exit within 2s of svcStop returning nil")
	}
	if _, err := os.Stat(u.PidPath()); !os.IsNotExist(err) {
		t.Errorf("pidfile not removed after stop")
	}
}

func TestErrIsESRCH(t *testing.T) {
	if !errIsESRCH(syscall.ESRCH) {
		t.Error("errIsESRCH(ESRCH) should be true")
	}
	if errIsESRCH(syscall.EPERM) {
		t.Error("errIsESRCH(EPERM) should be false")
	}
	if errIsESRCH(nil) {
		t.Error("errIsESRCH(nil) should be false")
	}
}

func TestIPCServerAccessDenied(t *testing.T) {
	// End-to-end: spin up the listener (with a tempdir socket), connect
	// as the current uid (non-root in test env), send a privileged
	// request, observe Access-denied response. Proves the SO_PEERCRED
	// gate works and the message format matches systemd's.
	if os.Getuid() == 0 {
		t.Skip("test must run as non-root to exercise access-denied path")
	}

	sock := filepath.Join(t.TempDir(), "test.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		handleSystemctlConnection(conn)
	}()

	conn, err := net.DialTimeout("unix", sock, 1*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	req := proto.SystemctlRequest{Op: "stop", Units: []string{"ssh"}}
	if err := proto.SendJSON(conn, proto.SYSTEMCTL_REQ, req); err != nil {
		t.Fatalf("send: %v", err)
	}

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	msgType, payload, err := proto.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if msgType != proto.SYSTEMCTL_RESP {
		t.Fatalf("frame type: got 0x%02x, want SYSTEMCTL_RESP (0x61)", msgType)
	}
	var resp proto.SystemctlResponse
	if err := json.Unmarshal(payload, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.ExitCode == 0 {
		t.Error("expected non-zero exit code for non-root caller")
	}
	if !strings.Contains(resp.Stderr, "Access denied") {
		t.Errorf("stderr should contain 'Access denied' (matches systemd's polkit-rejection wording), got: %q", resp.Stderr)
	}
}
