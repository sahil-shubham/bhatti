//go:build linux

package krucible

// These tests run against a real systemd: they start transient units under
// bhatti.slice. They are opt-in, because on a host that is already running
// bhatti they would add to its slice:
//
//	BHATTI_TEST_SYSTEMD=1              placement, layout, weights, adoption (root, systemd as PID 1, cgroup v2)
//	BHATTI_TEST_SYSTEMD_DESTRUCTIVE=1  also the test that stops D-Bus: a throwaway container only
//
// scripts/test-cgroups-systemd.sh runs them in a privileged container with
// systemd as PID 1 and needs only Docker.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/launchgate"
)

// systemdManager is the real manager, or a skip.
func systemdManager(t *testing.T, destructive bool) *cgroupManager {
	t.Helper()
	if os.Getenv("BHATTI_TEST_SYSTEMD") != "1" {
		t.Skip("set BHATTI_TEST_SYSTEMD=1 (root, systemd as PID 1) to run against a real systemd")
	}
	if destructive && os.Getenv("BHATTI_TEST_SYSTEMD_DESTRUCTIVE") != "1" {
		t.Skip("stops D-Bus: set BHATTI_TEST_SYSTEMD_DESTRUCTIVE=1, in a throwaway container only")
	}
	if os.Geteuid() != 0 {
		t.Skip("placing helpers needs root")
	}
	m, err := resolveCgroups("systemd", defaultCgroupHost())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.close)
	return m
}

// testOwnerFor is an owner of this test's own, and its slice is stopped, with
// everything in it, when the test ends.
func testOwnerFor(t *testing.T, m *cgroupManager) string {
	t.Helper()
	owner := ownerSlug(fmt.Sprintf("systemd-test:%s:%d", t.Name(), time.Now().UnixNano()))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = m.sd.StopUnit(ctx, "bhatti-"+owner+".slice")
	})
	return owner
}

// managerRoot is the cgroup of systemd's own root, which is "" on a host and
// /docker/<id> in a container that shares the host's cgroup namespace.
func managerRoot(t *testing.T) string {
	t.Helper()
	cg, err := procCgroupPath(1)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(cg, "/init.scope")
}

// cgroupDir is where a cgroup (path below systemd's root) is on /sys/fs/cgroup.
func cgroupDir(t *testing.T, path string) string {
	t.Helper()
	return filepath.Join("/sys/fs/cgroup", managerRoot(t), path)
}

func readCgroupFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Join(dir, name), err)
	}
	return strings.TrimSpace(string(b))
}

func readInt(t *testing.T, dir, name string) int64 {
	t.Helper()
	v, err := strconv.ParseInt(readCgroupFile(t, dir, name), 10, 64)
	if err != nil {
		t.Fatalf("%s/%s: %v", dir, name, err)
	}
	return v
}

// startStandin starts the stand-in as a daemon starts a helper, placed by m.
func startStandin(t *testing.T, m *cgroupManager, ps placementSpec, mode string) (cmd *exec.Cmd, dir string, err error) {
	t.Helper()
	dir = t.TempDir()
	spec := filepath.Join(dir, "vmspec.json")
	cmd = exec.Command(os.Args[0], spec)
	cmd.Args[0] = "bhatti-vmm"
	cmd.Env = append(os.Environ(), standinEnv+"="+mode)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	ps.alive = func(pid int) bool { return pidAlive(pid) && isHelper(pid, spec) }
	err = m.start(context.Background(), cmd, ps, nil)
	t.Cleanup(func() {
		if cmd.Process != nil {
			reap(t, cmd)
		}
	})
	return cmd, dir, err
}

func vmSpecFor(t *testing.T, owner string, n int) placementSpec {
	t.Helper()
	ps, err := vmPlacement(owner, fmt.Sprintf("%016x", n))
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

// TestSystemdPlacesBeforeWorkAndAccounts is the placement in full on a real
// systemd: layout, the cgroup the helper was in when its work began, weights,
// and the files the collector reads.
func TestSystemdPlacesBeforeWorkAndAccounts(t *testing.T) {
	m := systemdManager(t, false)
	owner := testOwnerFor(t, m)
	vmPS := vmSpecFor(t, owner, 1)
	netdPS, err := netdPlacement(owner)
	if err != nil {
		t.Fatal(err)
	}

	_, vmDir, err := startStandin(t, m, vmPS, "hog:64")
	if err != nil {
		t.Fatalf("place the VM: %v", err)
	}
	_, netdDir, err := startStandin(t, m, netdPS, "idle")
	if err != nil {
		t.Fatalf("place the gateway: %v", err)
	}
	waitFor(t, "the VM stand-in to allocate its memory", func() bool { return exists(filepath.Join(vmDir, "standin.allocated")) })
	waitFor(t, "the gateway stand-in to begin", func() bool { return exists(filepath.Join(netdDir, standinStarted)) })

	// Before the first instruction of its work the helper was already in its scope.
	for name, tc := range map[string]struct {
		dir string
		ps  placementSpec
	}{"vm": {vmDir, vmPS}, "netd": {netdDir, netdPS}} {
		began, err := os.ReadFile(filepath.Join(tc.dir, standinStarted))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(string(began), tc.ps.path()) {
			t.Errorf("%s began work in\n%s\nwant a cgroup ending in %s", name, began, tc.ps.path())
		}
	}

	// Layout: bhatti.slice / the owner's slice / the scopes.
	top := cgroupDir(t, "/"+topSlice)
	ownerSlice := cgroupDir(t, "/"+topSlice+"/"+vmPS.slice())
	vmScope, netdScope := cgroupDir(t, vmPS.path()), cgroupDir(t, netdPS.path())
	for _, dir := range []string{top, ownerSlice, vmScope, netdScope} {
		if !exists(dir) {
			t.Errorf("%s doesn't exist", dir)
		}
	}
	if self, _ := procCgroupPath(os.Getpid()); strings.Contains(self, topSlice) {
		t.Errorf("the daemon's own cgroup %q is inside bhatti.slice", self)
	}

	// Weights: the gateway outweighs its owner's VMs tenfold; bhatti.slice carries
	// the default so the cpu controller is on, which also gives system.slice (the
	// daemon's) a weight to compete with.
	if got := readCgroupFile(t, netdScope, "cpu.weight"); got != "1000" {
		t.Errorf("netd cpu.weight = %s, want 1000", got)
	}
	if got := readCgroupFile(t, vmScope, "cpu.weight"); got != "100" {
		t.Errorf("vm cpu.weight = %s, want the default 100", got)
	}
	if got := readCgroupFile(t, top, "cpu.weight"); got != "100" {
		t.Errorf("bhatti.slice cpu.weight = %s, want 100", got)
	}
	if sys := cgroupDir(t, "/system.slice"); exists(sys) {
		if got := readCgroupFile(t, sys, "cpu.weight"); got != "100" {
			t.Errorf("system.slice cpu.weight = %s: the daemon's cgroup and bhatti.slice should weigh the same", got)
		}
	}
	for _, c := range []string{"cpu", "memory", "io"} {
		if !strings.Contains(" "+readCgroupFile(t, ownerSlice, "cgroup.subtree_control")+" ", " "+c+" ") {
			t.Errorf("controller %s isn't enabled for the owner's scopes", c)
		}
	}

	// What the collector will read exists, for scopes and the owner's slice.
	for _, dir := range []string{vmScope, netdScope, ownerSlice} {
		for _, f := range []string{"cpu.stat", "memory.current", "memory.peak", "memory.stat", "memory.events", "io.stat", "cpu.pressure", "memory.pressure", "io.pressure"} {
			if !exists(filepath.Join(dir, f)) {
				t.Errorf("%s has no %s", dir, f)
			}
		}
	}
	// The guest's memory is the scope's, and the slice adds its scopes up.
	vmMem, netdMem, sliceMem := readInt(t, vmScope, "memory.current"), readInt(t, netdScope, "memory.current"), readInt(t, ownerSlice, "memory.current")
	t.Logf("memory.current: vm scope %d MiB, netd scope %d MiB, owner slice %d MiB", vmMem>>20, netdMem>>20, sliceMem>>20)
	if vmMem < 64<<20 || vmMem > 128<<20 {
		t.Errorf("vm scope memory.current = %d MiB, want about the 64 MiB the helper touched", vmMem>>20)
	}
	if netdMem > 48<<20 {
		t.Errorf("netd scope memory.current = %d MiB: the VM's memory leaked into it", netdMem>>20)
	}
	if sliceMem < vmMem+netdMem-(8<<20) {
		t.Errorf("owner slice memory.current %d MiB is less than its scopes' %d MiB", sliceMem>>20, (vmMem+netdMem)>>20)
	}

	// Systemd's inventory: both scopes, described as the plan names them.
	units, err := m.listScopes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	desc := map[string]string{}
	for _, u := range units {
		desc[u.Name] = u.Description
	}
	if desc[vmPS.unit] != "bhatti sandbox="+strings.TrimPrefix(strings.TrimSuffix(vmPS.unit, ".scope"), "bhatti-"+owner+"-")+" owner="+owner || desc[netdPS.unit] != "bhatti netd owner="+owner {
		t.Errorf("listed scopes: %v", desc)
	}
}

// TestSystemdAddRefKeepsTheUnitNotTheCgroup is the proof the plan asked for. With
// AddRef a scope's unit stays loaded, with the figures systemd saved for it,
// after its last process exits. Its cgroup does not: it is removed with the
// process, and with it memory.events (oom_kill) and the pressure totals. Without
// a reference the unit goes as well.
func TestSystemdAddRefKeepsTheUnitNotTheCgroup(t *testing.T) {
	m := systemdManager(t, false)
	owner := testOwnerFor(t, m)
	ctx := context.Background()

	for _, addRef := range []bool{true, false} {
		ps := vmSpecFor(t, owner, map[bool]int{true: 2, false: 3}[addRef])
		// Placed by hand, without the manager's watcher, so the reference is the test's to see.
		if err := m.ensureSlices(ctx, owner); err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		spec := filepath.Join(dir, "vmspec.json")
		cmd := exec.Command(os.Args[0], spec)
		cmd.Args[0] = "bhatti-vmm"
		cmd.Env = append(os.Environ(), standinEnv+"=hog:48")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		g, err := launchgate.New()
		if err != nil {
			t.Fatal(err)
		}
		if err := g.Start(cmd); err != nil {
			t.Fatal(err)
		}
		props := scopeProps(ps, cmd.Process.Pid)
		if !addRef {
			props = slices.DeleteFunc(props, func(p unitProp) bool { return p.Name == "AddRef" })
		}
		if err := m.sd.StartTransientUnit(ctx, ps.unit, props); err != nil {
			t.Fatal(err)
		}
		if err := m.waitMember(ctx, cmd.Process.Pid, ps); err != nil {
			t.Fatal(err)
		}
		if err := g.Open(); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the helper to allocate", func() bool { return exists(filepath.Join(dir, "standin.allocated")) })
		scopeDir := cgroupDir(t, ps.path())
		livePeak := readInt(t, scopeDir, "memory.peak")

		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		// Give systemd the time it needs to notice and settle.
		time.Sleep(300 * time.Millisecond)

		units, err := m.sd.Units(ctx, ps.unit)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("AddRef=%v: unit listed after exit: %v; cgroup directory exists: %v", addRef, units, exists(scopeDir))
		if exists(scopeDir) {
			t.Errorf("AddRef=%v: the scope's cgroup survived its last process: memory.events could be read after the fact", addRef)
		}
		if !addRef {
			if len(units) != 0 {
				t.Errorf("without AddRef the unit outlived its process: %v", units)
			}
			continue
		}
		if len(units) != 1 || units[0].Active != "inactive" {
			t.Fatalf("with AddRef the unit should stay loaded, inactive: %v", units)
		}
		u, err := m.sd.Usage(ctx, ps.unit)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("saved by systemd: cpu %d ms, peak memory %d MiB (live peak read %d MiB), io read %d bytes, io written %d bytes, result %s",
			u.CPUNanos/1e6, u.MemoryPeak>>20, livePeak>>20, u.IORead, u.IOWrite, u.Result)
		if u.CPUNanos <= 0 || u.MemoryPeak < 48<<20 || u.Result != "success" {
			t.Errorf("figures systemd kept: %+v", u)
		}
		if err := m.sd.UnrefUnit(ctx, ps.unit); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
		if after, _ := m.sd.Units(ctx, ps.unit); len(after) != 0 {
			t.Errorf("the unit survived its last reference: %v", after)
		}
	}
}

// TestSystemdScopeIsReleasedWhenTheHelperExits: through the manager, the final
// figures are logged and the unit is gone soon after the helper.
func TestSystemdScopeIsReleasedWhenTheHelperExits(t *testing.T) {
	var logs syncBuffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	m := systemdManager(t, false)
	ps := vmSpecFor(t, testOwnerFor(t, m), 4)
	cmd, dir, err := startStandin(t, m, ps, "hog:48")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the helper to allocate", func() bool { return exists(filepath.Join(dir, "standin.allocated")) })
	reap(t, cmd)
	waitFor(t, "the scope to be released", func() bool {
		units, _ := m.sd.Units(context.Background(), ps.unit)
		return len(units) == 0
	})
	waitFor(t, "the final figures to be logged", func() bool { return strings.Contains(logs.String(), "krucible.cgroup.final") })
	t.Log(strings.TrimSpace(logs.String()))
	if !strings.Contains(logs.String(), "result=success") || !strings.Contains(logs.String(), "sandbox="+strings.TrimSuffix(strings.TrimPrefix(ps.unit, "bhatti-"+ps.owner+"-"), ".scope")) {
		t.Errorf("final log:\n%s", logs.String())
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.held) != 0 {
		t.Errorf("still holding %v", m.held)
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestSystemdStopThenStartReusesTheName: a sandbox stopped and started again
// asks for the scope it just left, over and over, and gets it.
func TestSystemdStopThenStartReusesTheName(t *testing.T) {
	m := systemdManager(t, false)
	ps := vmSpecFor(t, testOwnerFor(t, m), 5)
	for i := range 15 {
		cmd, _, err := startStandin(t, m, ps, "idle")
		if err != nil {
			t.Fatalf("start %d over the last one's scope: %v", i, err)
		}
		reap(t, cmd)
	}
}

// TestSystemdFiftyPlacementsAtOnce: the spike's burst, through the manager and
// its bound of sixteen. Every helper ends up in its own scope, and none works
// before then.
func TestSystemdFiftyPlacementsAtOnce(t *testing.T) {
	const n = 50
	m := systemdManager(t, false)
	owner := testOwnerFor(t, m)
	type result struct {
		ps   placementSpec
		cmd  *exec.Cmd
		dir  string
		took time.Duration
		err  error
	}
	results := make([]result, n)
	var wg sync.WaitGroup
	began := time.Now()
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ps := vmSpecFor(t, owner, 100+i)
			start := time.Now()
			cmd, dir, err := startStandin(t, m, ps, "idle")
			results[i] = result{ps, cmd, dir, time.Since(start), err}
		}()
	}
	wg.Wait()
	var took []time.Duration
	for _, r := range results {
		if r.err != nil {
			t.Errorf("%s: %v", r.ps.unit, r.err)
			continue
		}
		took = append(took, r.took)
		waitFor(t, r.ps.unit+" to begin", func() bool { return exists(filepath.Join(r.dir, standinStarted)) })
		began, _ := os.ReadFile(filepath.Join(r.dir, standinStarted))
		if !strings.Contains(string(began), r.ps.path()) {
			t.Errorf("%s began work in %q", r.ps.unit, began)
		}
	}
	sort.Slice(took, func(i, j int) bool { return took[i] < took[j] })
	if len(took) == n {
		t.Logf("%d placements at once in %v: per placement (spawn included) p50 %v, p95 %v, max %v", n, time.Since(began).Round(time.Millisecond),
			took[n/2].Round(time.Millisecond), took[n*95/100].Round(time.Millisecond), took[n-1].Round(time.Millisecond))
	}
	units, err := m.listScopes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mine := 0
	for _, u := range units {
		if strings.HasPrefix(u.Name, "bhatti-"+owner+"-") {
			mine++
		}
	}
	if mine != n {
		t.Errorf("systemd lists %d of this owner's scopes, want %d", mine, n)
	}
}

// daemonRole is the environment variable that makes this test binary the
// daemon of TestSystemdAdoptsAcrossARestart: a process that places a helper and
// is then killed.
const daemonRole = "BHATTI_TEST_SYSTEMD_DAEMON"

// TestSystemdDaemonProcess is that daemon. It runs only when started as one.
func TestSystemdDaemonProcess(t *testing.T) {
	owner := os.Getenv(daemonRole)
	if owner == "" {
		t.Skip("not started as the daemon of another test")
	}
	m, err := resolveCgroups("systemd", defaultCgroupHost())
	if err != nil {
		t.Fatal(err)
	}
	ps := vmSpecFor(t, owner, 7)
	_, dir, err := startStandin(t, m, ps, "hog:48")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("PLACED %s %s\n", ps.unit, dir)
	select {} // until it is killed
}

// TestSystemdAdoptsAcrossARestart: SIGKILL the daemon that placed a helper; the
// scope and the helper live on, a new manager finds the scope listed, adopts the
// helper whole, and releases the scope when the helper goes.
func TestSystemdAdoptsAcrossARestart(t *testing.T) {
	m := systemdManager(t, false)
	owner := testOwnerFor(t, m)
	daemon := exec.Command(os.Args[0], "-test.run=^TestSystemdDaemonProcess$", "-test.v")
	daemon.Env = append(os.Environ(), daemonRole+"="+owner, "BHATTI_TEST_SYSTEMD=1")
	out, err := daemon.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	daemon.Stderr = os.Stderr
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = daemon.Process.Kill(); _ = daemon.Wait() }()
	var unit, dir string
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 3 && f[0] == "PLACED" {
			unit, dir = f[1], f[2]
			break
		}
	}
	if unit == "" {
		t.Fatal("the daemon never reported a placed helper")
	}
	ps := vmSpecFor(t, owner, 7)
	spec := filepath.Join(dir, "vmspec.json")
	ps.alive = func(pid int) bool { return pidAlive(pid) && isHelper(pid, spec) }

	// Find the helper the way recovery would: by its argv, on /proc.
	var pid int
	waitFor(t, "the helper to allocate", func() bool { return exists(filepath.Join(dir, "standin.allocated")) })
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		if p, err := strconv.Atoi(e.Name()); err == nil && isHelper(p, spec) {
			pid = p
		}
	}
	if pid == 0 {
		t.Fatal("couldn't find the helper")
	}
	defer syscall.Kill(pid, syscall.SIGKILL)

	if err := daemon.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = daemon.Process.Wait()
	time.Sleep(500 * time.Millisecond)
	if !pidAlive(pid) {
		t.Fatal("the helper died with its daemon")
	}
	units, err := m.listScopes(context.Background())
	if err != nil || !slices.ContainsFunc(units, func(u unitStatus) bool { return u.Name == unit && u.Active == "active" }) {
		t.Fatalf("the scope isn't listed active after the daemon's death: %v (%v)", units, err)
	}

	// A new daemon: a new manager, which adopts.
	m2, err := resolveCgroups("systemd", defaultCgroupHost())
	if err != nil {
		t.Fatal(err)
	}
	defer m2.close()
	got, err := m2.adopt(context.Background(), pid, ps, placedScope{unit: unit})
	if err != nil || got != (placedScope{unit: unit}) {
		t.Fatalf("adopt = %+v, %v; want it adopted whole", got, err)
	}
	if cg, _ := procCgroupPath(pid); !memberOf(cg, ps) {
		t.Fatalf("helper is in %q", cg)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	waitFor(t, "the adopted scope to be released", func() bool {
		after, _ := m2.sd.Units(context.Background(), unit)
		return len(after) == 0
	})
}

// TestSystemdAdoptsAnUnplacedHelperAsPartial: the upgrade case on real cgroups.
// A helper that has been running outside any scope is moved into one; what it
// had allocated stays charged where it was, which is what "partial" means.
func TestSystemdAdoptsAnUnplacedHelperAsPartial(t *testing.T) {
	m := systemdManager(t, false)
	ps := vmSpecFor(t, testOwnerFor(t, m), 8)
	dir := t.TempDir()
	spec := filepath.Join(dir, "vmspec.json")
	cmd := exec.Command(os.Args[0], spec)
	cmd.Args[0] = "bhatti-vmm"
	cmd.Env = append(os.Environ(), standinEnv+"=hog:64")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	g, err := launchgate.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Start(cmd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reap(t, cmd) })
	if err := g.Open(); err != nil { // running, in the cgroup it started in: a pre-placement helper
		t.Fatal(err)
	}
	waitFor(t, "the helper to allocate", func() bool { return exists(filepath.Join(dir, "standin.allocated")) })
	before, _ := procCgroupPath(cmd.Process.Pid)

	ps.alive = pidAlive
	got, err := m.adopt(context.Background(), cmd.Process.Pid, ps, placedScope{})
	if err != nil || got != (placedScope{unit: ps.unit, partial: true}) {
		t.Fatalf("adopt = %+v, %v", got, err)
	}
	after, _ := procCgroupPath(cmd.Process.Pid)
	scopeMem := readInt(t, cgroupDir(t, ps.path()), "memory.current")
	t.Logf("moved from %s to %s; its scope holds %d MiB of the 64 MiB it had touched", before, after, scopeMem>>20)
	if !memberOf(after, ps) {
		t.Fatalf("helper is in %q", after)
	}
	if scopeMem > 32<<20 {
		t.Errorf("the scope was charged %d MiB at the move: memory is charged where it was allocated, and the flag says it isn't all there", scopeMem>>20)
	}
}

// TestSystemdModeSelection: "auto" picks systemd here, and falls back to "none"
// where the bus isn't; "systemd" refuses.
func TestSystemdModeSelection(t *testing.T) {
	m := systemdManager(t, false)
	if st := m.status(); st.Mode != engine.CgroupSystemd || st.Reason != "" {
		t.Fatalf("systemd mode status %+v", st)
	}
	auto, err := resolveCgroups("auto", defaultCgroupHost())
	if err != nil || auto.mode != engine.CgroupSystemd {
		t.Fatalf("auto = %+v, %v; want systemd on this host", auto.status(), err)
	}
	auto.close()

	t.Setenv("DBUS_SYSTEM_BUS_ADDRESS", "unix:path=/nonexistent/bhatti-test-bus")
	started := time.Now()
	none, err := resolveCgroups("auto", defaultCgroupHost())
	if err != nil || none.mode != engine.CgroupNone || !strings.Contains(none.reason, "systemd's bus is unreachable") {
		t.Fatalf("auto without the bus = %+v, %v", none.status(), err)
	}
	t.Logf("auto without the bus: %s (decided in %v)", none.reason, time.Since(started).Round(time.Microsecond))
	if _, err := resolveCgroups("systemd", defaultCgroupHost()); err == nil || !strings.Contains(err.Error(), "systemd is required but unavailable") {
		t.Fatalf("systemd without the bus = %v, want a refusal", err)
	}
}

// TestSystemdFailsClosedWithoutTheBus stops D-Bus under a running manager:
// placement fails, with the reason, leaving no helper and no scope; helpers
// already placed are untouched; and when the bus is back the next placement
// works, on a connection the manager redialled.
func TestSystemdFailsClosedWithoutTheBus(t *testing.T) {
	m := systemdManager(t, true)
	owner := testOwnerFor(t, m)
	placed, _, err := startStandin(t, m, vmSpecFor(t, owner, 20), "idle")
	if err != nil {
		t.Fatal(err)
	}

	systemctl := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("systemctl", args...).CombinedOutput(); err != nil {
			t.Fatalf("systemctl %v: %v\n%s", args, err, out)
		}
	}
	// Stopping dbus.socket stops listening but leaves the socket file: the bus is
	// up when a connection to it is accepted.
	busUp := func() bool {
		c, err := net.DialTimeout("unix", "/run/dbus/system_bus_socket", time.Second)
		if err == nil {
			c.Close()
		}
		return err == nil
	}
	t.Cleanup(func() { _ = exec.Command("systemctl", "start", "dbus.socket").Run() })
	systemctl("stop", "dbus.service", "dbus.socket")
	waitFor(t, "the system bus to go away", func() bool { return !busUp() })

	failed := vmSpecFor(t, owner, 21)
	began := time.Now()
	cmd, dir, err := startStandin(t, m, failed, "idle")
	t.Logf("placement with the bus stopped: %v (after %v)", err, time.Since(began).Round(time.Millisecond))
	if err == nil {
		t.Fatal("a helper was placed with the bus stopped")
	}
	if cmd.ProcessState == nil || pidAlive(cmd.Process.Pid) {
		t.Error("the unplaced helper wasn't killed and reaped")
	}
	if exists(filepath.Join(dir, standinStarted)) {
		t.Error("the helper worked although it was never placed")
	}
	if !strings.Contains(err.Error(), "place "+failed.unit) {
		t.Errorf("error doesn't name what couldn't be placed: %v", err)
	}
	if !pidAlive(placed.Process.Pid) {
		t.Error("a helper placed earlier died with the bus")
	}
	if _, err := resolveCgroups("systemd", defaultCgroupHost()); err == nil {
		t.Error("a daemon asking for systemd started without the bus")
	}
	if auto, err := resolveCgroups("auto", defaultCgroupHost()); err != nil || auto.mode != engine.CgroupNone {
		t.Errorf("auto without the bus: %+v, %v", auto.status(), err)
	}

	systemctl("start", "dbus.socket")
	waitFor(t, "the system bus to come back", busUp)
	again := vmSpecFor(t, owner, 22)
	waitFor(t, "placement to work again", func() bool {
		_, _, err := startStandin(t, m, again, "idle")
		return err == nil
	})
}

// TestSystemdOOMKillOutlivesTheScope: what is left to attribute a kernel OOM
// kill once the scope is gone. A scope given a deliberately small MemoryMax
// (this test only: nothing else sets a limit) has its helper OOM-killed. The
// scope's own memory.events goes with it, as AddRef does not keep the cgroup;
// the owner slice's counter, and systemd's saved Result, do not.
func TestSystemdOOMKillOutlivesTheScope(t *testing.T) {
	m := systemdManager(t, false)
	owner := testOwnerFor(t, m)
	ctx := context.Background()
	ps := vmSpecFor(t, owner, 9)
	if err := m.ensureSlices(ctx, owner); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	spec := filepath.Join(dir, "vmspec.json")
	cmd := exec.Command(os.Args[0], spec)
	cmd.Args[0] = "bhatti-vmm"
	cmd.Env = append(os.Environ(), standinEnv+"=hog:200")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	g, err := launchgate.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Start(cmd); err != nil {
		t.Fatal(err)
	}
	// Swap off for this scope, or the kernel just pushes the pages out: with a
	// swap file on the host a MemoryMax alone may never kill anything.
	props := append(scopeProps(ps, cmd.Process.Pid), unitProp{"MemoryMax", uint64(32 << 20)}, unitProp{"MemorySwapMax", uint64(0)})
	if err := m.sd.StartTransientUnit(ctx, ps.unit, props); err != nil {
		t.Fatal(err)
	}
	if err := m.waitMember(ctx, cmd.Process.Pid, ps); err != nil {
		t.Fatal(err)
	}
	ownerSlice := cgroupDir(t, "/"+topSlice+"/"+ps.slice())
	before := readCgroupFile(t, ownerSlice, "memory.events")
	scopeDir := cgroupDir(t, ps.path())
	if err := g.Open(); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	t.Logf("helper ended: %v", err)
	var scopeEvents string
	if b, rerr := os.ReadFile(filepath.Join(scopeDir, "memory.events")); rerr == nil { // while it still exists
		scopeEvents = strings.ReplaceAll(strings.TrimSpace(string(b)), "\n", " ")
	}
	time.Sleep(500 * time.Millisecond)
	t.Logf("scope memory.events read at exit: %q; cgroup still there 500ms later: %v", scopeEvents, exists(scopeDir))
	after := readCgroupFile(t, ownerSlice, "memory.events")
	t.Logf("owner slice memory.events\n before: %s\n after:  %s", strings.ReplaceAll(before, "\n", " "), strings.ReplaceAll(after, "\n", " "))
	units, _ := m.sd.Units(ctx, ps.unit)
	u, uerr := m.sd.Usage(ctx, ps.unit)
	t.Logf("systemd kept: %v; usage %+v (%v)", units, u, uerr)
	if err := m.sd.UnrefUnit(ctx, ps.unit); err != nil {
		t.Fatal(err)
	}
	// CollectMode=inactive-or-failed: a scope that ended failed is forgotten too,
	// or its name would stay taken.
	waitFor(t, "the failed scope to be forgotten", func() bool {
		left, _ := m.sd.Units(ctx, ps.unit)
		return len(left) == 0
	})

	oomKills := func(events string) int {
		for _, line := range strings.Split(events, "\n") {
			if rest, ok := strings.CutPrefix(line, "oom_kill "); ok {
				n, _ := strconv.Atoi(rest)
				return n
			}
		}
		return -1
	}
	if oomKills(after) != oomKills(before)+1 {
		t.Errorf("the owner slice's oom_kill went from %d to %d: its counter should outlive the scope and show the kill", oomKills(before), oomKills(after))
	}
	if uerr != nil || u.Result != "oom-kill" {
		t.Errorf("systemd's saved Result = %q (%v), want oom-kill", u.Result, uerr)
	}
}

// rejectUnrefOnceSystemd leaves the real connection and its reference intact
// when one release is rejected, as a temporary authorization failure would.
type rejectUnrefOnceSystemd struct {
	systemdAPI
	unit     string
	once     sync.Once
	rejected chan struct{}
}

func (s *rejectUnrefOnceSystemd) UnrefUnit(ctx context.Context, name string) error {
	if name == s.unit {
		reject := false
		s.once.Do(func() { reject = true })
		if reject {
			close(s.rejected)
			return fmt.Errorf("temporary reference release failure")
		}
	}
	return s.systemdAPI.UnrefUnit(ctx, name)
}

func TestSystemdFailedUnrefCanBeRetried(t *testing.T) {
	m := systemdManager(t, false)
	ps := vmSpecFor(t, testOwnerFor(t, m), 901)
	real := m.sd
	sd := &rejectUnrefOnceSystemd{systemdAPI: real, unit: ps.unit, rejected: make(chan struct{})}
	m.sd = sd

	cmd, dir, err := startStandin(t, m, ps, "idle")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first helper to begin", func() bool { return exists(filepath.Join(dir, standinStarted)) })
	reap(t, cmd)

	// Let the exit watcher's release finish before retrying, so this tests a
	// failed RPC, not a race with the next incarnation's reference.
	watcherDone := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(watcherDone)
	}()
	select {
	case <-watcherDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the exited helper's cleanup did not finish")
	}
	select {
	case <-sd.rejected:
	default:
		t.Fatal("the exit cleanup never attempted the rejected release")
	}
	waitFor(t, "the inactive scope retained after the rejected release", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		units, err := real.Units(ctx, ps.unit)
		return err == nil && len(units) == 1 && units[0].Active == "inactive"
	})

	// This can only reuse the name if the manager remembers the failed release
	// and retries it on the still-open connection.
	again, againDir, err := startStandin(t, m, ps, "idle")
	if err != nil {
		t.Fatalf("restart after a temporary UnrefUnit failure: %v", err)
	}
	waitFor(t, "the replacement helper to begin", func() bool { return exists(filepath.Join(againDir, standinStarted)) })
	if cg, err := procCgroupPath(again.Process.Pid); err != nil || !memberOf(cg, ps) {
		t.Fatalf("replacement helper's cgroup = %q, %v; want %s", cg, err, ps.path())
	}
	reap(t, again)
	waitFor(t, "the replacement scope to release normally", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		units, err := real.Units(ctx, ps.unit)
		return err == nil && len(units) == 0
	})
}
