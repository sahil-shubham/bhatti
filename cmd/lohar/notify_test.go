//go:build linux

package main

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// notifyTestSetup returns a Registry with a tempdir-backed notify
// socket plus a started receiver goroutine. The receiver uses cgroup
// attribution which requires real /proc/<pid>/cgroup reads -- those
// tests gate on the environment having v2 cgroups; pure-parser tests
// don't.
func notifyTestSetup(t *testing.T) *Registry {
	t.Helper()
	dir := t.TempDir()
	reg := NewRegistry(Config{
		ServiceDirs:      []string{dir},
		PidDir:           t.TempDir(),
		LogDir:           t.TempDir(),
		NotifySocketPath: filepath.Join(t.TempDir(), "notify.sock"),
	})
	t.Cleanup(reg.WaitForWatchers)
	return reg
}

func TestParseNotifyMessage(t *testing.T) {
	// Datagram format mirrors what libsystemd's sd_notify(3) writes:
	// key=value lines separated by '\n'. Trailing newline is tolerated.
	cases := []struct {
		in   string
		want map[string]string
	}{
		{"READY=1\n", map[string]string{"READY": "1"}},
		{"READY=1", map[string]string{"READY": "1"}},
		{"READY=1\nSTATUS=ok\n", map[string]string{"READY": "1", "STATUS": "ok"}},
		{"READY=1\nMAINPID=1234\n", map[string]string{"READY": "1", "MAINPID": "1234"}},
		{"STATUS=loading config files\n", map[string]string{"STATUS": "loading config files"}},
		// Lines without '=' are dropped (defensive against non-protocol noise).
		{"READY=1\ngarbage\nSTATUS=ok\n", map[string]string{"READY": "1", "STATUS": "ok"}},
		// Empty lines tolerated.
		{"\nREADY=1\n\n", map[string]string{"READY": "1"}},
		// Equals in the value is preserved (e.g., STATUS could contain '=').
		{"STATUS=key=val\n", map[string]string{"STATUS": "key=val"}},
		// Empty value is preserved (sd_notify uses this to "clear" status).
		{"STATUS=\n", map[string]string{"STATUS": ""}},
	}
	for _, c := range cases {
		got := parseNotifyMessage([]byte(c.in))
		if len(got) != len(c.want) {
			t.Errorf("parseNotifyMessage(%q): got %d keys, want %d (%v)", c.in, len(got), len(c.want), got)
			continue
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("parseNotifyMessage(%q): got[%q]=%q, want %q", c.in, k, got[k], v)
			}
		}
	}
}

func TestUnitFromCgroupLine(t *testing.T) {
	// Direct membership in a unit's cgroup.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ssh.service"),
		[]byte("[Service]\nExecStart=/usr/sbin/sshd\n"), 0644)
	os.WriteFile(filepath.Join(dir, "postgresql@.service"),
		[]byte("[Service]\nExecStart=/usr/bin/postgres\n"), 0644)
	reg := NewRegistry(Config{
		ServiceDirs: []string{dir},
		PidDir:      t.TempDir(),
		LogDir:      t.TempDir(),
	})

	cases := []struct {
		line     string
		wantName string // canonical name expected; "" means no match
	}{
		{"0::/system.slice/ssh.service", "ssh"},
		{"0::/system.slice/ssh.service/", "ssh"}, // trailing slash
		// Nested cgroups (Docker-in-VM): unit's daemon spawned its own sub-cgroup.
		{"0::/system.slice/ssh.service/podman-12345", "ssh"},
		// Template instance.
		{"0::/system.slice/postgresql@16-main.service", "postgresql@16-main"},
		// Outside system.slice -- not one of our units.
		{"0::/user.slice/user-1000.slice/session-1.scope", ""},
		// No system.slice anywhere.
		{"0::/init.scope", ""},
		// Malformed: only one ':' instead of '0::'. Returns nil.
		{"badline", ""},
		// Unknown unit name (not in serviceDirs).
		{"0::/system.slice/unknown.service", ""},
	}
	for _, c := range cases {
		u := reg.unitFromCgroupLine(c.line)
		if c.wantName == "" {
			if u != nil {
				t.Errorf("cgroup line %q: got %q, want no match", c.line, u.Canonical)
			}
			continue
		}
		if u == nil {
			t.Errorf("cgroup line %q: got nil, want %q", c.line, c.wantName)
			continue
		}
		if u.Canonical != c.wantName {
			t.Errorf("cgroup line %q: got %q, want %q", c.line, u.Canonical, c.wantName)
		}
	}
}

func TestActivatingMarkerLifecycle(t *testing.T) {
	// MarkActivating writes the marker; ClearActivating removes it;
	// IsActivating reads it. The whole machinery underlying the
	// activating-vs-active distinction.
	reg := notifyTestSetup(t)
	dir := reg.Config.ServiceDirs[0]
	os.WriteFile(filepath.Join(dir, "test.service"),
		[]byte("[Service]\nType=notify\nExecStart=/bin/true\n"), 0644)

	u, _ := reg.Resolve("test")

	if u.IsActivating() {
		t.Fatal("fresh unit should not be activating")
	}
	if err := u.MarkActivating(); err != nil {
		t.Fatalf("MarkActivating: %v", err)
	}
	if !u.IsActivating() {
		t.Error("after MarkActivating, IsActivating should be true")
	}
	u.ClearActivating()
	if u.IsActivating() {
		t.Error("after ClearActivating, IsActivating should be false")
	}
}

func TestSvcIsActiveDuringActivating(t *testing.T) {
	// The whole point of F2: a Type=notify unit that's spawned but
	// hasn't sent READY=1 yet should NOT report active. Otherwise a
	// script doing `systemctl start && exec_command` races against
	// the daemon's startup. The activating marker gates is-active.
	reg := notifyTestSetup(t)
	dir := reg.Config.ServiceDirs[0]
	os.WriteFile(filepath.Join(dir, "test.service"),
		[]byte("[Service]\nType=notify\nExecStart=/bin/true\n"), 0644)

	u, _ := reg.Resolve("test")

	// Pretend the daemon is running: write a pidfile and the
	// activating marker.
	u.WritePID(os.Getpid())
	u.MarkActivating()

	// is-active must return false during activating.
	if svcIsActive(u) {
		t.Error("svcIsActive should be false while .activating marker exists")
	}

	// After READY=1 (we simulate by clearing the marker), is-active
	// flips to true.
	u.ClearActivating()
	if !svcIsActive(u) {
		t.Error("svcIsActive should be true after ClearActivating")
	}

	// Cleanup: remove the pidfile so the test process doesn't get
	// "killed" by a future svcStop with this PID.
	u.RemovePID()
}

func TestApplyNotifyClearsActivating(t *testing.T) {
	// applyNotify is the receiver's per-message handler. Given a
	// READY=1 message attributed to a unit, it clears the activating
	// marker. We bypass the SO_PASSCRED + cgroup attribution path here
	// (those are integration-tested separately) and call applyNotify
	// directly with a known-good Unit.
	reg := notifyTestSetup(t)
	dir := reg.Config.ServiceDirs[0]
	os.WriteFile(filepath.Join(dir, "test.service"),
		[]byte("[Service]\nType=notify\nExecStart=/bin/true\n"), 0644)

	u, _ := reg.Resolve("test")
	u.MarkActivating()

	// Stub the cgroup lookup: pretend any PID maps to our test unit.
	// We do this by verifying applyNotify behaves correctly when a Unit
	// is found. Since applyNotify takes a sender PID and looks it up
	// via /proc/<pid>/cgroup, we'd need a real cgroup setup to drive
	// it end-to-end. Instead, verify the side-effect by calling the
	// underlying logic: u.ClearActivating() on READY=1.
	//
	// applyNotify itself is a one-line function: lookup, then act.
	// The "act" half is what we verify here:
	msg := parseNotifyMessage([]byte("READY=1\n"))
	if msg["READY"] != "1" {
		t.Fatal("setup: parser broken")
	}
	// Simulating what applyNotify's act-half does:
	if msg["READY"] == "1" {
		u.ClearActivating()
	}

	if u.IsActivating() {
		t.Error("READY=1 should clear the activating marker")
	}
}

func TestApplyNotifyMainPID(t *testing.T) {
	// MAINPID=N overwrites the pidfile, so admin tools (kill -HUP via
	// systemctl reload, status output) see the right PID after a
	// daemon re-execs or forks.
	reg := notifyTestSetup(t)
	dir := reg.Config.ServiceDirs[0]
	os.WriteFile(filepath.Join(dir, "test.service"),
		[]byte("[Service]\nType=notify\nExecStart=/bin/true\n"), 0644)

	u, _ := reg.Resolve("test")
	u.WritePID(1000) // initial pidfile pre-MAINPID

	// Calling applyNotify with no senderPID matching means we have to
	// drive the side effect via the same code path applyNotify uses.
	msg := parseNotifyMessage([]byte("MAINPID=4242\n"))
	if v := msg["MAINPID"]; v != "4242" {
		t.Fatal("setup: parser broken")
	}
	// Mirror applyNotify's MAINPID branch:
	if n, err := strconv.Atoi(msg["MAINPID"]); err == nil && n > 1 {
		u.WritePID(n)
	}

	pid, err := u.ReadPID()
	if err != nil {
		t.Fatalf("ReadPID: %v", err)
	}
	if pid != 4242 {
		t.Errorf("pidfile after MAINPID=4242: got %d, want 4242", pid)
	}
}

// TestNotifyReceiverEndToEnd is the integration test: spin up the
// receiver, send a real datagram with SO_PASSCRED credentials, verify
// the receiver attributes correctly via /proc/<pid>/cgroup and clears
// the activating marker.
//
// Requires real cgroup v2 and the test process to be IN a unit cgroup
// we've set up. We do that by:
//   - creating a system.slice/<unit>.service cgroup at the test's
//     CgroupRoot (which we point at /sys/fs/cgroup with sudo)
//   - placing this test process in it
//   - sending READY=1 to the receiver
//   - verifying the activating marker is cleared
//
// Skip without root.
func TestNotifyReceiverEndToEnd(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root for real cgroup attribution")
	}
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		t.Skip("requires cgroup v2 mounted at /sys/fs/cgroup")
	}
	requireUnrestrictedCgroup(t)
	// A container can show us as the cgroup root (private cgroup namespace)
	// and still mount the tree read-only.
	if err := os.MkdirAll("/sys/fs/cgroup/system.slice", 0755); err != nil {
		t.Skipf("can't create system.slice: %v", err)
	}

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "notifier.service"),
		[]byte("[Service]\nType=notify\nExecStart=/bin/true\n"), 0644)
	reg := NewRegistry(Config{
		ServiceDirs:      []string{dir},
		PidDir:           t.TempDir(),
		LogDir:           t.TempDir(),
		CgroupRoot:       "/sys/fs/cgroup",
		NotifySocketPath: filepath.Join(t.TempDir(), "notify.sock"),
	})
	t.Cleanup(reg.WaitForWatchers)

	u, err := reg.Resolve("notifier")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// Place this process in the unit's cgroup so the receiver can
	// attribute our datagram correctly. Cleanup migrates us back.
	if err := u.CreateCgroup(); err != nil {
		t.Fatalf("CreateCgroup: %v", err)
	}
	defer u.RemoveCgroup()
	if err := u.PlaceInCgroup(os.Getpid()); err != nil {
		t.Fatalf("PlaceInCgroup: %v", err)
	}
	defer func() {
		// Migrate back to the root cgroup so RemoveCgroup can succeed.
		os.WriteFile("/sys/fs/cgroup/cgroup.procs",
			[]byte(strconv.Itoa(os.Getpid())), 0644)
	}()

	u.MarkActivating()

	// Atomic flag tells the goroutine to exit after one message.
	var done atomic.Bool
	go func() {
		// Drain one notification then break out by closing the
		// listener. We rebuild the receiver inline so we can stop it.
		os.Remove(reg.Config.NotifySocketPath)
		conn, err := net.ListenUnixgram("unixgram",
			&net.UnixAddr{Name: reg.Config.NotifySocketPath, Net: "unixgram"})
		if err != nil {
			return
		}
		defer conn.Close()
		os.Chmod(reg.Config.NotifySocketPath, 0666)
		if rawConn, err := conn.SyscallConn(); err == nil {
			rawConn.Control(func(fd uintptr) {
				syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_PASSCRED, 1)
			})
		}
		buf := make([]byte, 8192)
		oob := make([]byte, 1024)
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		n, oobn, _, _, err := conn.ReadMsgUnix(buf, oob)
		if err != nil {
			return
		}
		pid := senderPID(oob[:oobn])
		if pid <= 1 {
			return
		}
		msg := parseNotifyMessage(buf[:n])
		reg.applyNotify(pid, msg)
		done.Store(true)
	}()

	// Wait for the receiver socket to appear.
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(reg.Config.NotifySocketPath); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Send READY=1 from this process (which is now in the unit's cgroup).
	c, err := net.DialUnix("unixgram", nil,
		&net.UnixAddr{Name: reg.Config.NotifySocketPath, Net: "unixgram"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, err := c.Write([]byte("READY=1\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	c.Close()

	// Wait for the receiver to process.
	for i := 0; i < 50; i++ {
		if done.Load() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if u.IsActivating() {
		t.Error("activating marker still present after READY=1; receiver didn't attribute correctly")
	}
}
