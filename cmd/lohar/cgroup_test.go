//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestParseMemoryValue(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "max"},
		{"infinity", "max"},
		{"max", "max"},
		{"512", "512"},                  // bare bytes
		{"1K", "1024"},
		{"512M", strconv.FormatUint(512*1024*1024, 10)},
		{"1G", "1073741824"},
		{"1MB", strconv.FormatUint(1024*1024, 10)}, // trailing B accepted
		{"garbage", "max"},                          // unparseable -> permissive default
	}
	for _, c := range cases {
		if got := parseMemoryValue(c.in); got != c.want {
			t.Errorf("parseMemoryValue(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseInfinityOrInt(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "max"},
		{"infinity", "max"},
		{"512", "512"},
		{"garbage", "max"},
	}
	for _, c := range cases {
		if got := parseInfinityOrInt(c.in); got != c.want {
			t.Errorf("parseInfinityOrInt(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseCPUQuota(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"50%", "50000 100000"},
		{"100%", "100000 100000"},
		{"200%", "200000 100000"},
		{"25%", "25000 100000"},
		{"", "max 100000"},          // empty/zero -> no limit
		{"garbage", "max 100000"},
	}
	for _, c := range cases {
		if got := parseCPUQuota(c.in); got != c.want {
			t.Errorf("parseCPUQuota(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestKillModeFor(t *testing.T) {
	mk := func(km string) *Unit {
		return &Unit{
			Sections: serviceFile{sections: map[string][]kvPair{
				"Service": {{key: "KillMode", value: km}},
			}},
		}
	}
	for _, km := range []string{"control-group", "process", "mixed", "none"} {
		if got := killModeFor(mk(km)); got != km {
			t.Errorf("killModeFor(%q) = %q", km, got)
		}
	}
	// Default + unrecognised both yield control-group (matches systemd's
	// permissive parser).
	if got := killModeFor(mk("")); got != "control-group" {
		t.Errorf("killModeFor(unset) = %q, want control-group", got)
	}
	if got := killModeFor(mk("garbage")); got != "control-group" {
		t.Errorf("killModeFor(garbage) = %q, want control-group", got)
	}
}

// cgroupTestSetup returns a Unit pointing into a fake cgroup root under
// t.TempDir(). Most cgroup operations work on regular dirs (mkdir,
// writes), so the tests exercise the real code paths without needing
// root or a real cgroup mount. KillCgroup is the exception (writes "1"
// to a file which won't actually kill anything in a fake tree) -- those
// tests are gated separately.
//
// Cleanup waits for any watcher goroutines spawned by the test before
// allowing tempdir teardown.
func cgroupTestSetup(t *testing.T) *Unit {
	t.Helper()
	dir := t.TempDir()
	reg := NewRegistry(Config{
		ServiceDirs: []string{dir},
		CgroupRoot:  t.TempDir(),
		PidDir:      t.TempDir(),
		LogDir:      t.TempDir(),
	})
	t.Cleanup(reg.WaitForWatchers)

	os.WriteFile(filepath.Join(dir, "test.service"), []byte(`
[Service]
ExecStart=/bin/true
MemoryMax=512M
TasksMax=128
CPUQuota=50%
`), 0644)
	u, err := reg.Resolve("test")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return u
}

func TestCgroupPath(t *testing.T) {
	u := cgroupTestSetup(t)
	want := filepath.Join(u.reg.Config.CgroupRoot, "system.slice", "test.service")
	if got := u.CgroupPath(); got != want {
		t.Errorf("CgroupPath = %q, want %q", got, want)
	}
}

func TestCreateCgroupAppliesLimits(t *testing.T) {
	u := cgroupTestSetup(t)
	if err := u.CreateCgroup(); err != nil {
		t.Fatalf("CreateCgroup: %v", err)
	}
	cg := u.CgroupPath()
	if _, err := os.Stat(cg); err != nil {
		t.Fatalf("cgroup dir not created: %v", err)
	}
	// Resource limits should have been written to control files.
	cases := []struct {
		file, want string
	}{
		{"memory.max", strconv.FormatUint(512*1024*1024, 10)},
		{"pids.max", "128"},
		{"cpu.max", "50000 100000"},
	}
	for _, c := range cases {
		data, err := os.ReadFile(filepath.Join(cg, c.file))
		if err != nil {
			t.Errorf("read %s: %v", c.file, err)
			continue
		}
		got := strings.TrimSpace(string(data))
		if got != c.want {
			t.Errorf("%s = %q, want %q", c.file, got, c.want)
		}
	}
}

func TestPlaceInCgroup(t *testing.T) {
	u := cgroupTestSetup(t)
	if err := u.CreateCgroup(); err != nil {
		t.Fatalf("CreateCgroup: %v", err)
	}
	// Pretend-write a PID. In real use the kernel rejects PIDs that
	// aren't actually running, but in a fake cgroup tree any write
	// succeeds \u2014 we're testing that PlaceInCgroup writes the right value
	// to the right file, not the kernel's accept logic.
	if err := u.PlaceInCgroup(1234); err != nil {
		t.Fatalf("PlaceInCgroup: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(u.CgroupPath(), "cgroup.procs"))
	if err != nil {
		t.Fatalf("read cgroup.procs: %v", err)
	}
	if strings.TrimSpace(string(data)) != "1234" {
		t.Errorf("cgroup.procs = %q, want 1234", string(data))
	}
	if !u.CgroupHasProcs() {
		t.Error("CgroupHasProcs should be true after PlaceInCgroup")
	}
}

func TestRemoveCgroup(t *testing.T) {
	// In production cgroup v2, the directory contains kernel-virtual
	// files (memory.max etc.) that auto-clean when the dir is rmdir'd.
	// In our tempdir fake those are real files — so we drop them first
	// to mirror the production end-state, then verify RemoveCgroup
	// (which is just rmdir) succeeds.
	u := cgroupTestSetup(t)
	u.CreateCgroup()
	entries, _ := os.ReadDir(u.CgroupPath())
	for _, e := range entries {
		os.Remove(filepath.Join(u.CgroupPath(), e.Name()))
	}
	if err := u.RemoveCgroup(); err != nil {
		t.Errorf("RemoveCgroup: %v", err)
	}
	if _, err := os.Stat(u.CgroupPath()); !os.IsNotExist(err) {
		t.Errorf("cgroup not removed: %v", err)
	}
}

func TestCgroupKillIntegration(t *testing.T) {
	// Real kernel cgroup test: requires root + a real cgroup v2 mount +
	// kernel >= 5.14. Skip in unprivileged environments.
	if os.Getuid() != 0 {
		t.Skip("requires root for real cgroup operations")
	}
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		t.Skip("requires cgroup v2 mounted at /sys/fs/cgroup")
	}
	requireUnrestrictedCgroup(t)
	if _, err := os.Stat("/sys/fs/cgroup/system.slice"); err != nil {
		// Try to create it for the test.
		if err := os.MkdirAll("/sys/fs/cgroup/system.slice", 0755); err != nil {
			t.Skipf("can't create system.slice: %v", err)
		}
	}

	dir := t.TempDir()
	// Real kernel test: production cgroup root, but tempdirs for the
	// shim's own state.
	reg := NewRegistry(Config{
		ServiceDirs: []string{dir},
		CgroupRoot:  "/sys/fs/cgroup",
		PidDir:      t.TempDir(),
		LogDir:      t.TempDir(),
	})
	t.Cleanup(reg.WaitForWatchers)

	// Create a unit whose ExecStart spawns a child that intentionally
	// daemonises (setsid + fork) to escape the parent's PGID. Without
	// cgroup-per-unit, PGID-kill would miss it. With cgroup.kill, the
	// kernel SIGKILLs everything in the cgroup atomically.
	os.WriteFile(filepath.Join(dir, "fork-bomb.service"), []byte(`
[Service]
ExecStart=/bin/sh -c "( /bin/sleep 60 & ) ; /bin/sleep 60"
`), 0644)
	u, _ := reg.Resolve("fork-bomb")

	if err := svcStart(u); err != nil {
		t.Fatalf("svcStart: %v", err)
	}
	defer u.RemoveCgroup()

	// Give the children time to start.
	time.Sleep(200 * time.Millisecond)

	// At least 2 procs in the cgroup: the shell + each background sleep.
	// The kernel may have reaped the shell already; we just need >= 1.
	if !u.CgroupHasProcs() {
		t.Fatal("cgroup is empty after svcStart")
	}

	if err := u.KillCgroup(); err != nil {
		t.Fatalf("KillCgroup: %v", err)
	}
	if !u.WaitCgroupDrain(2 * time.Second) {
		t.Errorf("cgroup did not drain after KillCgroup")
	}
}

// TestCgroupMemoryMaxEnforcement is the real-kernel test that the
// MemoryMax= directive actually causes the kernel to OOM-kill a process
// that allocates beyond the limit. Without this test, our F1 unit tests
// only verify that we wrote a number to memory.max -- not that the
// kernel enforces it. This is the user-visible promise of F1: a
// misbehaving redis can't OOM nginx.
//
// Approach: spawn a unit whose ExecStart is a tiny shell script that
// allocates memory by repeatedly doubling a string. MemoryMax=10M, so
// the kernel's OOM-killer terminates it. The watcher (C6) then writes
// the failed marker; we observe it.
//
// Requires root + real cgroup v2 + working memory controller.
func TestCgroupMemoryMaxEnforcement(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root for real cgroup operations")
	}
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		t.Skip("requires cgroup v2 mounted at /sys/fs/cgroup")
	}
	requireUnrestrictedCgroup(t)
	rootCtrl, _ := os.ReadFile("/sys/fs/cgroup/cgroup.subtree_control")
	if !strings.Contains(string(rootCtrl), "memory") {
		t.Skip("memory controller not enabled in /sys/fs/cgroup/cgroup.subtree_control")
	}
	// Memory controller must also be enabled on system.slice for our
	// child cgroups to inherit it.
	os.MkdirAll("/sys/fs/cgroup/system.slice", 0755)
	os.WriteFile("/sys/fs/cgroup/system.slice/cgroup.subtree_control",
		[]byte("+memory"), 0644)

	dir := t.TempDir()
	// Real cgroup root (the kernel must enforce limits), but tempdirs
	// for shim state.
	reg := NewRegistry(Config{
		ServiceDirs: []string{dir},
		CgroupRoot:  "/sys/fs/cgroup",
		PidDir:      t.TempDir(),
		LogDir:      t.TempDir(),
	})
	t.Cleanup(reg.WaitForWatchers)

	// A tiny memory hog: bash variable doubling. 30 iterations of
	// doubling a string blows well past 10MB. Restart=no so we don't
	// loop after the kernel kills it.
	unit := `
[Service]
Type=simple
ExecStart=/bin/sh -c 'a=x; for i in $(seq 1 30); do a="$a$a$a$a$a$a$a"; done; echo done'
MemoryMax=10M
Restart=no
`
	os.WriteFile(filepath.Join(dir, "memhog.service"), []byte(unit), 0644)

	u, err := reg.Resolve("memhog")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	defer u.RemoveCgroup()

	if err := svcStart(u); err != nil {
		t.Fatalf("svcStart: %v", err)
	}

	// Wait for the watcher to observe the OOM-kill (or the script
	// exiting via memory error) and write the failed marker. Typical
	// time from spawn to OOM is well under a second.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if u.IsFailed() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !u.IsFailed() {
		peak, _ := os.ReadFile(filepath.Join(u.CgroupPath(), "memory.peak"))
		current, _ := os.ReadFile(filepath.Join(u.CgroupPath(), "memory.current"))
		t.Fatalf("memhog not marked failed after MemoryMax=10M; memory.peak=%s memory.current=%s",
			strings.TrimSpace(string(peak)), strings.TrimSpace(string(current)))
	}

	// Exit code: 137 (128 + SIGKILL) when OOM-kill fires; 1 if the
	// shell errors out before allocating enough. Either way, non-zero.
	if code := u.LastExitCode(); code == 0 {
		t.Errorf("LastExitCode = 0; expected non-zero from OOM-kill or shell error")
	}
}

// TestStartDaemonPlacesProcessInUnitCgroup is the test that would have
// caught the v1.11.9 bug a year ago. It exercises the full svcStart
// path — startDaemon spawning through `lohar spawn` into the daemon —
// and asserts that /proc/<pid>/cgroup shows the unit's cgroup, not the
// root cgroup (0::/).
//
// Requires a real cgroup v2 hierarchy because /proc/<pid>/cgroup
// reflects the kernel's view; a synthetic tempdir CgroupRoot doesn't
// reach the kernel. Skips on dev Mac and non-root Linux; runs on the
// Pi cluster integration runner, which executes as root with cgroup v2
// mounted.
//
// What this guards against:
//   - Reverting the spawn-helper wiring back to the post-cmd.Start()
//     PlaceInCgroup write (which races against forking daemons).
//   - Future tier additions whose ExecStart wrapping accidentally
//     bypasses lohar spawn.
func TestStartDaemonPlacesProcessInUnitCgroup(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root for real cgroup operations")
	}
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		t.Skip("requires cgroup v2 mounted at /sys/fs/cgroup")
	}
	requireUnrestrictedCgroup(t)
	if _, err := os.Stat("/sys/fs/cgroup/system.slice"); err != nil {
		if err := os.MkdirAll("/sys/fs/cgroup/system.slice", 0755); err != nil {
			t.Skipf("can't create system.slice: %v", err)
		}
	}

	dir := t.TempDir()
	// Real kernel cgroup root, tempdirs for shim state. svcStart will
	// CreateCgroup under /sys/fs/cgroup/system.slice/<unit>.service and
	// then spawn the daemon through lohar spawn, which writes its PID
	// into that cgroup before exec'ing into the daemon.
	reg := NewRegistry(Config{
		ServiceDirs: []string{dir},
		CgroupRoot:  "/sys/fs/cgroup",
		PidDir:      t.TempDir(),
		LogDir:      t.TempDir(),
	})
	t.Cleanup(reg.WaitForWatchers)

	// /bin/sleep doesn't fork — but the test isn't about forks; it's
	// about "did the supervisor's first cgroup-placement happen at all?"
	// If lohar spawn is invoked correctly, the daemon (sleep) ends up
	// in the unit's cgroup; if the supervisor regressed to the old
	// post-cmd.Start() path with no helper, sleep would still end up
	// there too — BUT only if PlaceInCgroup ran. The way this test
	// catches a regression is by asserting the placement happens at all.
	// A future variant that uses a forking daemon (Xkasmvnc, dbus-daemon)
	// would catch the race specifically; that lives in the agni-01
	// smoke test.
	os.WriteFile(filepath.Join(dir, "spawn-test.service"), []byte(`
[Service]
Type=simple
ExecStart=/bin/sleep 30
Restart=no
`), 0644)

	u, err := reg.Resolve("spawn-test")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	defer func() {
		svcStop(u)
		u.RemoveCgroup()
	}()

	if err := svcStart(u); err != nil {
		t.Fatalf("svcStart: %v", err)
	}

	// Give spawn → sh → sleep a moment to complete the execve chain
	// and land the PID in the cgroup. In practice the cgroup write
	// happens before the first execve, so this is the time between
	// supervisor returning from cmd.Start() and being able to read the
	// pidfile back — microseconds in normal operation.
	deadline := time.Now().Add(2 * time.Second)
	var pid int
	for time.Now().Before(deadline) {
		pid, err = u.ReadPID()
		if err == nil && pid > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if pid <= 0 {
		t.Fatalf("no valid pid after svcStart: pid=%d err=%v", pid, err)
	}

	// Read /proc/<pid>/cgroup and assert the unit's cgroup, not 0::/.
	// cgroup v2 format: "0::/system.slice/spawn-test.service\n"
	cgData, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		t.Fatalf("read /proc/%d/cgroup: %v", pid, err)
	}
	cgLine := strings.TrimSpace(string(cgData))

	if cgLine == "0::/" || strings.HasSuffix(cgLine, "::/") {
		t.Fatalf("daemon landed in root cgroup (the v1.11.9 bug): %q", cgLine)
	}
	wantSuffix := "system.slice/spawn-test.service"
	if !strings.HasSuffix(cgLine, wantSuffix) {
		t.Errorf("/proc/%d/cgroup = %q, want suffix %q", pid, cgLine, wantSuffix)
	}
}

// TestSvcStopUsesCgroupKillIfAvailable exercises the svcStop path that
// writes to cgroup.kill. We can't fake the kernel side, so we use a
// non-root path: a tempdir cgroup tree where cgroup.kill is just a
// regular file -- the WRITE succeeds (no actual kill happens) and svcStop
// proceeds through its drain+remove cleanup.
func TestSvcStopUsesCgroupKillPathWhenFileExists(t *testing.T) {
	u := cgroupTestSetup(t)

	// Pretend a daemon is running: write a pidfile pointing at a real
	// process we own (so processAlive returns true), create the cgroup,
	// and put cgroup.kill in place so svcStop's KillCgroup write succeeds.
	cmd := exec.Command("/bin/sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer cmd.Process.Kill()
	go cmd.Wait() // reap zombie

	u.CreateCgroup()
	// Touch cgroup.kill so the write path succeeds. Don't actually put
	// the sleep into our fake cgroup \u2014 we want svcStop to "succeed via
	// cgroup.kill" without actually killing the test's sleep.
	os.WriteFile(filepath.Join(u.CgroupPath(), "cgroup.kill"), []byte{}, 0644)

	u.WritePID(cmd.Process.Pid)

	// svcStop should write 1 to cgroup.kill and proceed. Our fake
	// cgroup.kill is a regular file, so the write succeeds without any
	// actual kill effect. WaitCgroupDrain returns immediately because
	// cgroup.procs is empty (we didn't add the sleep). Cleanup proceeds.
	//
	// Note: the test sleep is NOT killed by this svcStop, because our
	// cgroup.kill is a regular file, not a kernel-backed control file.
	// We separately kill it in the deferred cmd.Process.Kill().
	if err := svcStop(u); err != nil {
		t.Errorf("svcStop: %v", err)
	}

	// Pidfile should be gone.
	if _, err := os.Stat(u.PidPath()); !os.IsNotExist(err) {
		t.Errorf("pidfile not removed")
	}
	// Verify svcStop took the control-group path by checking cgroup.kill
	// got the "1" write. In production the kernel SIGKILLs the cgroup
	// and rmdir succeeds on the now-empty virtual dir; in our tempdir
	// fake, leftover memory.max/pids.max/cpu.max real files block rmdir,
	// so the cgroup dir survives. That's a tempdir artefact, not a bug
	// in the production code. The crucial assertion is that svcStop
	// wrote the right control file:
	data, err := os.ReadFile(filepath.Join(u.CgroupPath(), "cgroup.kill"))
	if err != nil {
		t.Fatalf("read cgroup.kill: %v", err)
	}
	if strings.TrimSpace(string(data)) != "1" {
		t.Errorf("cgroup.kill contents = %q, want 1 (svcStop didn't take the control-group path)", string(data))
	}
}

// TestSvcStopKillModeProcessTakesPGIDPath verifies that when a unit
// declares KillMode=process, svcStop uses the legacy PGID-kill path
// (kill -TERM -<pid>) and NOT the cgroup.kill primitive. This is the
// opt-out path for services that explicitly want only the main
// process killed, not the whole cgroup. The default (control-group)
// kills everything; KillMode=process keeps the daemon's own children
// alive after stop, which a few historical Type=forking services rely
// on.
//
// We verify by setting up a fake cgroup with a cgroup.kill file and
// pointing the pidfile at a real spawned sleep we own. After svcStop,
// cgroup.kill should still be EMPTY (svcStop didn't take that path);
// the sleep should be gone (svcStop took the PGID path instead).
func TestSvcStopKillModeProcessTakesPGIDPath(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry(Config{
		ServiceDirs: []string{dir},
		CgroupRoot:  t.TempDir(),
		PidDir:      t.TempDir(),
		LogDir:      t.TempDir(),
	})
	t.Cleanup(reg.WaitForWatchers)

	os.WriteFile(filepath.Join(dir, "legacy.service"),
		[]byte("[Service]\nExecStart=/bin/sleep 30\nKillMode=process\n"), 0644)

	u, _ := reg.Resolve("legacy")

	// Spawn a real sleep we own, in its own session so PGID kill works.
	cmd := exec.Command("/bin/sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn: %v", err)
	}
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

	// Set up a fake cgroup with cgroup.kill so we can detect whether
	// svcStop wrote to it (which would indicate it took the wrong path).
	u.CreateCgroup()
	os.WriteFile(filepath.Join(u.CgroupPath(), "cgroup.kill"), []byte{}, 0644)

	u.WritePID(cmd.Process.Pid)

	if err := svcStop(u); err != nil {
		t.Errorf("svcStop: %v", err)
	}

	// The sleep should have been killed by the PGID path.
	select {
	case <-waitDone:
		// good
	case <-time.After(2 * time.Second):
		t.Errorf("sleep didn't exit within 2s; svcStop didn't kill it via the PGID path")
	}

	// cgroup.kill should still be empty -- svcStop must NOT have taken
	// the control-group path.
	data, err := os.ReadFile(filepath.Join(u.CgroupPath(), "cgroup.kill"))
	if err != nil {
		t.Fatalf("read cgroup.kill: %v", err)
	}
	if strings.TrimSpace(string(data)) == "1" {
		t.Errorf("cgroup.kill = %q; svcStop wrote to it but KillMode=process should have taken the PGID path", string(data))
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		n    uint64
		want string
	}{
		{0, "0B"},
		{512, "512B"},
		{1024, "1.0K"},
		{1536, "1.5K"},
		{1024 * 1024, "1.0M"},
		{124*1024*1024 + 512*1024, "124.5M"},
		{1024 * 1024 * 1024, "1.0G"},
	}
	for _, c := range cases {
		if got := humanBytes(c.n); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}
