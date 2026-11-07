package krucible

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

func TestUnitNames(t *testing.T) {
	vm, err := vmPlacement(testOwner, testID)
	if err != nil {
		t.Fatal(err)
	}
	if want := "bhatti-0123456789ab-00000000000000a1.scope"; vm.unit != want {
		t.Errorf("vm scope = %q, want %q", vm.unit, want)
	}
	if want := "bhatti-0123456789ab.slice"; vm.slice() != want {
		t.Errorf("owner slice = %q, want %q", vm.slice(), want)
	}
	if want := "/bhatti.slice/bhatti-0123456789ab.slice/bhatti-0123456789ab-00000000000000a1.scope"; vm.path() != want {
		t.Errorf("vm cgroup = %q, want %q", vm.path(), want)
	}
	netd, err := netdPlacement(testOwner)
	if err != nil {
		t.Fatal(err)
	}
	if want := "bhatti-0123456789ab-netd.scope"; netd.unit != want {
		t.Errorf("netd scope = %q, want %q", netd.unit, want)
	}
	if netd.slice() != vm.slice() {
		t.Errorf("netd is in %q, its owner's VMs in %q: they belong in one slice", netd.slice(), vm.slice())
	}
	// Every unit name begins "bhatti-" and has its owner next: the slice's own
	// name, so systemd nests the scope without being told.
	if !strings.HasPrefix(vm.unit, strings.TrimSuffix(vm.slice(), ".slice")+"-") {
		t.Errorf("%q isn't named under %q", vm.unit, vm.slice())
	}

	for _, bad := range []string{"", "NETD", "A1", "a-1", "a.1", "a/1", "../x", "a b", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0", "é"} {
		if _, err := vmPlacement(testOwner, bad); err == nil {
			t.Errorf("sandbox id %q was accepted", bad)
		}
		if _, err := vmPlacement(bad, testID); err == nil {
			t.Errorf("owner %q was accepted", bad)
		}
		if _, err := netdPlacement(bad); err == nil {
			t.Errorf("netd owner %q was accepted", bad)
		}
	}
	// "netd" is how a gateway's scope ends, so no sandbox may be called that.
	if _, err := vmPlacement(testOwner, "netd"); err == nil {
		t.Error("a sandbox called netd would collide with its owner's gateway")
	}
}

// TestOwnerSlug: the owner in a unit name is the key netd's directory already
// carries, so the two can be matched by eye on a host.
func TestOwnerSlug(t *testing.T) {
	a, b := ownerSlug("u:alice"), ownerSlug("u:bob")
	if a == b || len(a) != 12 || unitPart("owner", a) != nil {
		t.Fatalf("slugs %q, %q: want two distinct 12-digit hex keys", a, b)
	}
	if ownerSlug("u:alice") != a {
		t.Fatal("a slug must be stable")
	}
	e := &Engine{cfg: Config{SocketDir: "/s"}}
	if got, want := e.netdDir("u:alice"), "/s/netd-"+a; got != want {
		t.Fatalf("netdDir = %q, want %q", got, want)
	}
	vm := &VM{ID: testID, UserID: "alice", netdKey: "u:alice"}
	if vm.cgroupOwner() != a {
		t.Fatalf("a sandbox with a NIC sits in slice %q, its netd's owner is %q", vm.cgroupOwner(), a)
	}
	// Without a NIC there is no netd key; the sandbox still sits beside its owner's others.
	if (&VM{ID: "x", UserID: "alice"}).cgroupOwner() != a {
		t.Fatal("a sandbox without a NIC isn't in its owner's slice")
	}
	if (&VM{ID: testID}).cgroupOwner() != ownerSlug("s:"+testID) {
		t.Fatal("an unowned sandbox should have a slice of its own")
	}
}

// TestPlacementProperties: what systemd is asked for. Accounting on, nothing
// limited; the only weights are the top slice's (the default, to switch the cpu
// controller on) and the gateway's.
func TestPlacementProperties(t *testing.T) {
	allowed := []string{"Description", "Slice", "PIDs", "CPUAccounting", "MemoryAccounting", "IOAccounting", "CollectMode", "AddRef", "CPUWeight"}
	vm, _ := vmPlacement(testOwner, testID)
	netd, _ := netdPlacement(testOwner)
	for name, props := range map[string][]unitProp{
		"vm scope":   scopeProps(vm, 4242),
		"netd scope": scopeProps(netd, 4242),
		"top slice":  sliceProps("d", cpuWeightTop),
		"owner":      sliceProps("d", 0),
	} {
		for _, p := range props {
			if !slices.Contains(allowed, p.Name) {
				t.Errorf("%s asks for %s: Phase 1 sets accounting and weights, never a limit", name, p.Name)
			}
		}
		pm := propMap(props)
		for _, acct := range []string{"CPUAccounting", "MemoryAccounting", "IOAccounting"} {
			if pm[acct] != true {
				t.Errorf("%s: %s = %v, want true", name, acct, pm[acct])
			}
		}
	}

	pm := propMap(scopeProps(vm, 4242))
	if got, ok := pm["PIDs"].([]uint32); !ok || len(got) != 1 || got[0] != 4242 {
		t.Errorf("PIDs = %v", pm["PIDs"])
	}
	if pm["Slice"] != "bhatti-0123456789ab.slice" || pm["AddRef"] != true || pm["CollectMode"] != "inactive-or-failed" {
		t.Errorf("scope properties %v", pm)
	}
	if _, ok := pm["CPUWeight"]; ok {
		t.Error("a VM scope keeps the default CPU weight")
	}
	if desc := pm["Description"]; desc != "bhatti sandbox="+testID+" owner="+testOwner {
		t.Errorf("description %q", desc)
	}

	npm := propMap(scopeProps(netd, 4242))
	if w, _ := npm["CPUWeight"].(uint64); w != cpuWeightNetd || w <= 100 {
		t.Errorf("netd CPUWeight = %v, want %d, above the default 100 its owner's VMs run at", npm["CPUWeight"], cpuWeightNetd)
	}

	// No slice has a Slice=: systemd derives its parent from its name and rejects the property.
	for _, props := range [][]unitProp{sliceProps("d", cpuWeightTop), sliceProps("d", 0)} {
		if _, ok := propMap(props)["Slice"]; ok {
			t.Error("a slice was given Slice=")
		}
	}
	if w, _ := propMap(sliceProps("d", cpuWeightTop))["CPUWeight"].(uint64); w != 100 {
		t.Errorf("bhatti.slice CPUWeight = %d, want 100: the default, there to switch the cpu controller on", w)
	}
	if _, ok := propMap(sliceProps("d", 0))["CPUWeight"]; ok {
		t.Error("an owner slice should not carry a weight")
	}
}

type hostFake struct {
	goos     string
	pid1     string
	pid1Err  error
	cgroup2  bool
	dialErr  error
	dialed   int
	sd       *fakeSystemd
	sdClosed bool
}

func (h *hostFake) host() cgroupHost {
	if h.goos == "" {
		h.goos = "linux"
	}
	return cgroupHost{
		goos:    h.goos,
		pid1:    func() (string, error) { return h.pid1, h.pid1Err },
		cgroup2: func() (bool, error) { return h.cgroup2, nil },
		dial: func(context.Context) (systemdAPI, error) {
			h.dialed++
			if h.dialErr != nil {
				return nil, h.dialErr
			}
			return h.sd, nil
		},
		procCg: h.sd.procCg,
	}
}

func goodHost() *hostFake { return &hostFake{pid1: "systemd", cgroup2: true, sd: newFakeSystemd()} }

func TestResolveCgroups(t *testing.T) {
	denied := errors.New("org.freedesktop.DBus.Error.InteractiveAuthorizationRequired: Interactive authentication required.")
	for _, tc := range []struct {
		name       string
		requested  string
		host       func() *hostFake
		wantMode   string
		wantReason string // substring of the reason
		wantErr    string // substring of the error; "" means none
		wantDialed int
	}{
		{"none places nothing and never connects", "none", goodHost, engine.CgroupNone, "", "", 0},
		{"auto with systemd", "auto", goodHost, engine.CgroupSystemd, "", "", 1},
		{"empty means auto", "", goodHost, engine.CgroupSystemd, "", "", 1},
		{"systemd with systemd", "systemd", goodHost, engine.CgroupSystemd, "", "", 1},
		{"auto without systemd as PID 1", "auto", func() *hostFake { h := goodHost(); h.pid1 = "tini"; return h },
			engine.CgroupNone, `PID 1 is "tini", not systemd`, "", 0},
		{"auto with unreadable PID 1", "auto", func() *hostFake { h := goodHost(); h.pid1Err = errors.New("no /proc"); return h },
			engine.CgroupNone, "no /proc", "", 0},
		{"auto on a cgroup v1 host", "auto", func() *hostFake { h := goodHost(); h.cgroup2 = false; return h },
			engine.CgroupNone, "cgroup v2 is not mounted", "", 0},
		{"auto with the bus down", "auto", func() *hostFake {
			h := goodHost()
			h.dialErr = errors.New("connect: no such file or directory")
			return h
		},
			engine.CgroupNone, "systemd's bus is unreachable: connect: no such file", "", 1},
		{"auto when systemd refuses a transient slice", "auto", func() *hostFake {
			h := goodHost()
			h.sd.startErr = func(string) error { return denied }
			return h
		}, engine.CgroupNone, "Interactive authentication required", "", 1},
		{"systemd without systemd as PID 1", "systemd", func() *hostFake { h := goodHost(); h.pid1 = "init"; return h },
			"", "", `systemd is required but unavailable: PID 1 is "init"`, 0},
		{"systemd without cgroup v2", "systemd", func() *hostFake { h := goodHost(); h.cgroup2 = false; return h },
			"", "", "systemd is required but unavailable: cgroup v2", 0},
		{"systemd with the bus down", "systemd", func() *hostFake { h := goodHost(); h.dialErr = errors.New("connection refused"); return h },
			"", "", "systemd is required but unavailable: systemd's bus is unreachable: connection refused", 1},
		{"systemd when systemd refuses", "systemd", func() *hostFake {
			h := goodHost()
			h.sd.startErr = func(string) error { return denied }
			return h
		}, "", "", "systemd is required but unavailable: start bhatti.slice", 1},
		{"unknown value", "cgroup", goodHost, "", "", `cgroups must be auto, systemd or none (got "cgroup")`, 0},
		{"macOS auto observes", "auto", func() *hostFake { h := goodHost(); h.goos = "darwin"; return h },
			engine.CgroupProcess, "no cgroups", "", 0},
		{"macOS none", "none", func() *hostFake { h := goodHost(); h.goos = "darwin"; return h }, engine.CgroupNone, "", "", 0},
		{"macOS can't require systemd", "systemd", func() *hostFake { h := goodHost(); h.goos = "darwin"; return h },
			"", "", "systemd needs Linux (this is darwin)", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.host()
			m, err := resolveCgroups(tc.requested, h.host())
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			st := m.status()
			if st.Mode != tc.wantMode || !strings.Contains(st.Reason, tc.wantReason) || (tc.wantReason == "" && st.Reason != "") {
				t.Errorf("status = %+v, want mode %s, reason containing %q", st, tc.wantMode, tc.wantReason)
			}
			wantReq := tc.requested
			if wantReq == "" {
				wantReq = "auto"
			}
			if st.Requested != wantReq {
				t.Errorf("requested = %q, want %q", st.Requested, wantReq)
			}
			if h.dialed != tc.wantDialed {
				t.Errorf("connected %d times, want %d", h.dialed, tc.wantDialed)
			}
			if m.placing() != (tc.wantMode == engine.CgroupSystemd) {
				t.Errorf("placing() = %v in mode %s", m.placing(), st.Mode)
			}
		})
	}
}

// TestResolveCgroupsReadiesTheTopSlice: choosing systemd mode creates
// bhatti.slice, which is also what proves bhatti may start transient units. It
// carries the default CPU weight so systemd turns the cpu controller on.
func TestResolveCgroupsReadiesTheTopSlice(t *testing.T) {
	h := goodHost()
	if _, err := resolveCgroups("auto", h.host()); err != nil {
		t.Fatal(err)
	}
	u, ok := h.sd.unit("bhatti.slice")
	if !ok {
		t.Fatal("bhatti.slice wasn't started")
	}
	if u.props["CPUWeight"] != uint64(100) || u.props["CPUAccounting"] != true || u.props["MemoryAccounting"] != true || u.props["IOAccounting"] != true {
		t.Fatalf("bhatti.slice properties: %v", u.props)
	}
	if got := h.sd.calls("start"); !slices.Equal(got, []string{"bhatti.slice"}) {
		t.Fatalf("started %v, want only the top slice until a sandbox needs its owner's", got)
	}
}

func TestResolveCgroupsFallbackClosesTheConnection(t *testing.T) {
	h := goodHost()
	closed := false
	fake := &closeSpy{fakeSystemd: h.sd, closed: &closed}
	h.sd.startErr = func(string) error { return errors.New("refused") }
	host := h.host()
	host.dial = func(context.Context) (systemdAPI, error) { return fake, nil }
	m, err := resolveCgroups("auto", host)
	if err != nil || m.mode != engine.CgroupNone || m.placing() {
		t.Fatalf("auto after a refused top slice: %+v, %v", m.status(), err)
	}
	if !closed {
		t.Fatal("the connection that couldn't place anything was left open")
	}
}

type closeSpy struct {
	*fakeSystemd
	closed *bool
}

func (c *closeSpy) Close() error { *c.closed = true; return nil }

// start runs m.start on the stand-in helper and reaps it when the test ends.
func startHelper(t *testing.T, m *cgroupManager, ps placementSpec, marker string, persist func(int, string)) (*exec.Cmd, error) {
	t.Helper()
	cmd := helperCmd(t, marker)
	err := m.start(context.Background(), cmd, ps, persist)
	t.Cleanup(func() {
		if cmd.Process != nil {
			reap(t, cmd)
		}
	})
	return cmd, err
}

// TestStartPlacesTheHelperBeforeItWorks: the helper is behind its gate while
// systemd is asked and while the scope is recorded, however long that takes,
// and works once both are done.
func TestStartPlacesTheHelperBeforeItWorks(t *testing.T) {
	sd := newFakeSystemd()
	sd.moveAfter = 20 * time.Millisecond
	marker := filepath.Join(t.TempDir(), "worked")
	sd.onStart = func(name string, _ map[string]any) {
		if strings.HasSuffix(name, ".scope") {
			time.Sleep(200 * time.Millisecond) // a helper that ignored its gate would have worked by now
			if exists(marker) {
				t.Error("the helper worked while systemd was still being asked to place it")
			}
		}
	}
	m := newTestManager(sd)
	ps := testVM(t)
	var persisted []string
	cmd, err := startHelper(t, m, ps, marker, func(pid int, unit string) {
		persisted = append(persisted, fmt.Sprintf("%d %s", pid, unit))
		if unit != "" && exists(marker) {
			t.Error("the helper worked before its scope was recorded")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	if want := []string{fmt.Sprintf("%d ", pid), fmt.Sprintf("%d %s", pid, ps.unit)}; !slices.Equal(persisted, want) {
		t.Fatalf("persisted %q, want the pid and then the scope: %q", persisted, want)
	}
	if cg, _ := sd.procCg(pid); !memberOf(cg, ps) {
		t.Fatalf("helper is in %q, not its scope", cg)
	}
	waitFor(t, "the helper to work once placed", func() bool { return exists(marker) })

	// Top slice first, then the owner's, then the scope; each with its properties.
	if got, want := sd.calls("start"), []string{"bhatti.slice", ps.slice(), ps.unit}; !slices.Equal(got, want) {
		t.Fatalf("systemd was asked for %v, want %v", got, want)
	}
	u, _ := sd.unit(ps.unit)
	if u.props["PIDs"].([]uint32)[0] != uint32(pid) || u.props["Slice"] != ps.slice() {
		t.Fatalf("scope properties: %v", u.props)
	}
}

// TestStartWaitsForALateMove: systemd's answer came back and the pid hasn't
// moved; the helper stays at its gate until it has.
func TestStartWaitsForALateMove(t *testing.T) {
	sd := newFakeSystemd()
	sd.moveAfter = 400 * time.Millisecond
	marker := filepath.Join(t.TempDir(), "worked")
	m := newTestManager(sd)
	began := time.Now()
	cmd, err := startHelper(t, m, testVM(t), marker, nil)
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took < 350*time.Millisecond {
		t.Fatalf("start returned after %v, before the pid had moved", took)
	}
	if cg, _ := sd.procCg(cmd.Process.Pid); !memberOf(cg, testVM(t)) {
		t.Fatalf("start returned with the helper in %q", cg)
	}
	waitFor(t, "the helper to work", func() bool { return exists(marker) })
}

// TestStartFailsClosedWhenTheMoveNeverComes: a pid that doesn't show up in its
// scope stops the scope, kills and reaps the helper, and says why.
func TestStartFailsClosedWhenTheMoveNeverComes(t *testing.T) {
	sd := newFakeSystemd()
	sd.moveAfter = -1
	marker := filepath.Join(t.TempDir(), "worked")
	m := newTestManager(sd)
	m.timeout = 300 * time.Millisecond
	ps := testVM(t)
	var persisted []string
	cmd, err := startHelper(t, m, ps, marker, func(pid int, unit string) { persisted = append(persisted, fmt.Sprintf("%d %s", pid, unit)) })
	if err == nil || !strings.Contains(err.Error(), "not in its scope") || !strings.Contains(err.Error(), ps.unit) {
		t.Fatalf("start = %v, want a refusal naming the scope the helper never reached", err)
	}
	if got := sd.calls("stop"); !slices.Equal(got, []string{ps.unit}) {
		t.Errorf("stopped %v, want the scope that was made", got)
	}
	if cmd.ProcessState == nil || pidAlive(cmd.Process.Pid) {
		t.Error("the helper wasn't killed and reaped")
	}
	if last := persisted[len(persisted)-1]; last != "0 " {
		t.Errorf("last persist %q, want the pid taken back", last)
	}
	time.Sleep(100 * time.Millisecond)
	if exists(marker) {
		t.Error("the helper worked although it was never placed")
	}
	if _, held := sd.unit(ps.unit); held {
		t.Error("the scope is still loaded: its reference wasn't dropped")
	}
	if len(m.held) != 0 {
		t.Errorf("still holding %v", m.held)
	}
}

// TestStartFailsClosedWhenSystemdRefuses: no scope was made, so none is stopped.
func TestStartFailsClosedWhenSystemdRefuses(t *testing.T) {
	sd := newFakeSystemd()
	sd.startErr = func(name string) error {
		if strings.HasSuffix(name, ".scope") {
			return errors.New("org.freedesktop.DBus.Error.AccessDenied: Access denied")
		}
		return nil
	}
	marker := filepath.Join(t.TempDir(), "worked")
	m := newTestManager(sd)
	var persisted []string
	cmd, err := startHelper(t, m, testVM(t), marker, func(pid int, unit string) { persisted = append(persisted, fmt.Sprintf("%d %s", pid, unit)) })
	if err == nil || !strings.Contains(err.Error(), "Access denied") {
		t.Fatalf("start = %v, want systemd's reason", err)
	}
	if len(sd.calls("stop")) != 0 {
		t.Errorf("stopped %v although no scope was made", sd.calls("stop"))
	}
	if pidAlive(cmd.Process.Pid) || persisted[len(persisted)-1] != "0 " {
		t.Errorf("helper alive=%v, persisted %q", pidAlive(cmd.Process.Pid), persisted)
	}
	time.Sleep(100 * time.Millisecond)
	if exists(marker) {
		t.Error("the helper worked although placing it failed")
	}
}

// TestStartReportsAHelperThatDiedAtItsGate: a helper that exits while it is
// being placed is reported, with how it exited, and its scope stopped.
func TestStartReportsAHelperThatDiedAtItsGate(t *testing.T) {
	sd := newFakeSystemd()
	sd.moveAfter = 100 * time.Millisecond // long enough for the helper to be long gone
	dir := t.TempDir()
	script := filepath.Join(dir, "dies.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(script)
	m := newTestManager(sd)
	ps := testVM(t)
	err := m.start(context.Background(), cmd, ps, nil)
	if err == nil || !strings.Contains(err.Error(), "launch gate") || !strings.Contains(err.Error(), "exit status 7") {
		t.Fatalf("start = %v, want the gate that couldn't open and the helper's own exit", err)
	}
	if got := sd.calls("stop"); !slices.Equal(got, []string{ps.unit}) {
		t.Errorf("stopped %v, want the scope that was made", got)
	}
}

// TestStartWaitsOutAPreviousIncarnation: a sandbox stopped and started again
// names the scope its last run left loaded, because this daemon still held a
// reference. That unit is released and the start goes on.
func TestStartWaitsOutAPreviousIncarnation(t *testing.T) {
	sd := newFakeSystemd()
	ps := testVM(t)
	sd.units[ps.unit] = &fakeUnit{props: map[string]any{}, active: "inactive", refs: 1}
	m := newTestManager(sd)
	m.held[ps.unit] = 4242 // the dead run's reference
	marker := filepath.Join(t.TempDir(), "worked")
	if _, err := startHelper(t, m, ps, marker, nil); err != nil {
		t.Fatalf("start over a dead incarnation: %v", err)
	}
	if got := sd.calls("unref"); len(got) == 0 || got[0] != ps.unit {
		t.Errorf("the dead incarnation's reference wasn't dropped: %v", got)
	}
	waitFor(t, "the helper to work", func() bool { return exists(marker) })
}

// TestARunsWatcherLeavesTheNextRunsReference: a sandbox's scope name is used by
// each run in turn. The watcher of a run that ended must not drop the reference
// the next run took on the same name, whichever of them gets there first.
func TestARunsWatcherLeavesTheNextRunsReference(t *testing.T) {
	sd := newFakeSystemd()
	ps := testVM(t)
	sd.units[ps.unit] = &fakeUnit{props: map[string]any{}, active: "active", refs: 1}
	m := newTestManager(sd)
	m.held[ps.unit] = 2002 // the next run's

	m.release(ps.unit, 1001) // the earlier run's watcher, late
	if len(sd.calls("unref")) != 0 || m.held[ps.unit] != 2002 {
		t.Fatalf("a watcher released another run's reference: %v, held %v", sd.calls("unref"), m.held)
	}
	m.finish(ps, 1001) // and its final-numbers pass: it has nothing of its own to read
	if len(sd.calls("usage"))+len(sd.calls("unref")) != 0 || m.held[ps.unit] != 2002 {
		t.Fatalf("a finished run's watcher touched the next run's scope: %v", sd.log)
	}
	m.release(ps.unit, 2002)
	if got := sd.calls("unref"); !slices.Equal(got, []string{ps.unit}) || len(m.held) != 0 {
		t.Fatalf("the run's own release: %v, held %v", got, m.held)
	}
}

// TestStartNeverTakesARunningScope: a name held by a scope that is still
// running is refused, never stopped to make way.
func TestStartNeverTakesARunningScope(t *testing.T) {
	sd := newFakeSystemd()
	ps := testVM(t)
	sd.units[ps.unit] = &fakeUnit{props: map[string]any{}, active: "active", refs: 1}
	m := newTestManager(sd)
	m.timeout = 250 * time.Millisecond
	marker := filepath.Join(t.TempDir(), "worked")
	cmd, err := startHelper(t, m, ps, marker, nil)
	if err == nil || !strings.Contains(err.Error(), "still held by a previous incarnation") {
		t.Fatalf("start = %v", err)
	}
	if len(sd.calls("stop")) != 0 {
		t.Errorf("stopped %v: a scope that isn't ours to stop", sd.calls("stop"))
	}
	if u, ok := sd.unit(ps.unit); !ok || u.active != "active" {
		t.Error("the running scope was disturbed")
	}
	if pidAlive(cmd.Process.Pid) {
		t.Error("helper left running")
	}
}

// TestSlicesAreEnsuredOncePerDaemon, and again after a placement fails.
func TestSlicesAreEnsuredOncePerDaemon(t *testing.T) {
	sd := newFakeSystemd()
	m := newTestManager(sd)
	for i := range 2 {
		ps, _ := vmPlacement(testOwner, fmt.Sprintf("%016x", i))
		ps.alive = pidAlive
		if _, err := startHelper(t, m, ps, filepath.Join(t.TempDir(), "w"), nil); err != nil {
			t.Fatal(err)
		}
	}
	starts := sd.calls("start")
	count := func(name string) int { return strings.Count(strings.Join(starts, " ")+" ", name+" ") }
	if count("bhatti.slice") != 1 || count("bhatti-"+testOwner+".slice") != 1 {
		t.Fatalf("two sandboxes of one owner asked systemd for %v", starts)
	}

	failing := true
	sd.startErr = func(name string) error {
		if failing && strings.HasSuffix(name, ".scope") {
			failing = false
			return errors.New("transient failure")
		}
		return nil
	}
	ps, _ := vmPlacement(testOwner, fmt.Sprintf("%016x", 9))
	ps.alive = pidAlive
	if _, err := startHelper(t, m, ps, filepath.Join(t.TempDir(), "w"), nil); err == nil {
		t.Fatal("the injected failure didn't fail the placement")
	}
	if _, err := startHelper(t, m, ps, filepath.Join(t.TempDir(), "w"), nil); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(strings.Join(sd.calls("start"), " ")+" ", "bhatti.slice "); n != 2 {
		t.Fatalf("the top slice was asked for %d times; a failed placement should have made the next one ask again", n)
	}
}

// TestPlacementsInFlightAreBounded: with systemd stalled, no more than
// maxPlacements helpers are being placed, and the rest wait for a slot before
// they are even started.
func TestPlacementsInFlightAreBounded(t *testing.T) {
	const launches = 3 * maxPlacements
	sd := newFakeSystemd()
	sd.moveAfter = 0
	sd.hold = make(chan struct{})
	m := newTestManager(sd)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var cmds []*exec.Cmd
	errs := make(chan error, launches)
	for i := range launches {
		ps, _ := vmPlacement(testOwner, fmt.Sprintf("%016x", i))
		ps.alive = pidAlive
		cmd := helperCmd(t, filepath.Join(t.TempDir(), "w"))
		mu.Lock()
		cmds = append(cmds, cmd)
		mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- m.start(context.Background(), cmd, ps, nil)
		}()
	}
	t.Cleanup(func() {
		for _, c := range cmds {
			if c.Process != nil {
				_ = c.Process.Kill()
				_, _ = c.Process.Wait()
			}
		}
	})
	waitFor(t, "the first placements to be in flight", func() bool {
		sd.mu.Lock()
		defer sd.mu.Unlock()
		return sd.inflight == maxPlacements
	})
	time.Sleep(200 * time.Millisecond) // time for any beyond the bound to show
	started := 0
	mu.Lock()
	for _, c := range cmds {
		if c.Process != nil {
			started++
		}
	}
	mu.Unlock()
	sd.mu.Lock()
	inflight, maxFlight := sd.inflight, sd.maxFlight
	sd.mu.Unlock()
	if inflight != maxPlacements || maxFlight != maxPlacements {
		t.Fatalf("%d placements in flight (peak %d), want exactly %d", inflight, maxFlight, maxPlacements)
	}
	if started != maxPlacements {
		t.Fatalf("%d helpers started while systemd stalled, want %d: the others wait for a slot, not at their gates", started, maxPlacements)
	}
	close(sd.hold)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("a queued placement failed: %v", err)
		}
	}
	if sd.maxFlight > maxPlacements {
		t.Fatalf("peak in flight %d exceeded %d", sd.maxFlight, maxPlacements)
	}
}

// TestFinalNumbersAreReadThenReleased: when the helper is gone, what systemd
// kept for its scope is read once and logged, and the reference dropped so the
// unit can go.
func TestFinalNumbersAreReadThenReleased(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	sd := newFakeSystemd()
	sd.moveAfter = 0
	m := newTestManager(sd)
	ps := testVM(t)
	cmd, err := startHelper(t, m, ps, filepath.Join(t.TempDir(), "w"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if u, ok := sd.unit(ps.unit); !ok || u.refs != 1 || u.props["AddRef"] != true {
		t.Fatalf("a running scope should be referenced once by the daemon: %+v", u)
	}
	reap(t, cmd)
	waitFor(t, "the scope to be released", func() bool { _, ok := sd.unit(ps.unit); return !ok })
	m.wg.Wait() // collection can precede confirmation of the release RPC
	if got := sd.calls("usage"); !slices.Equal(got, []string{ps.unit}) {
		t.Fatalf("read usage of %v, want the scope once", got)
	}
	if got := sd.calls("unref"); !slices.Equal(got, []string{ps.unit}) {
		t.Fatalf("unreferenced %v", got)
	}
	for _, want := range []string{"krucible.cgroup.final", "mem_peak_bytes=1073741824", "cpu_ms=41000", "result=success", "sandbox=" + testID} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("final log lacks %q:\n%s", want, logs.String())
		}
	}
	if len(m.held) != 0 {
		t.Errorf("still holding %v", m.held)
	}
}

// TestCloseDoesNotWaitForHelpers: shutting the daemon down ends its watchers
// at once; the helpers they watch run on.
func TestCloseDoesNotWaitForHelpers(t *testing.T) {
	sd := newFakeSystemd()
	sd.moveAfter = 0
	m := newTestManager(sd)
	cmd, err := startHelper(t, m, testVM(t), filepath.Join(t.TempDir(), "w"), nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { m.close(); m.close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("close waited for a running helper")
	}
	if !pidAlive(cmd.Process.Pid) {
		t.Fatal("closing the manager stopped the helper")
	}
}

// adoptee starts a stand-in process for a helper the daemon didn't start.
func adoptee(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reap(t, cmd) })
	return cmd.Process.Pid
}

// TestAdoptTakesAHelperInItsScope: a helper a previous daemon placed, found in
// the scope it was recorded in, is adopted as it is.
func TestAdoptTakesAHelperInItsScope(t *testing.T) {
	sd := newFakeSystemd()
	m := newTestManager(sd)
	t.Cleanup(m.close)
	ps, pid := testVM(t), adoptee(t)
	sd.place(pid, ps)

	got, err := m.adopt(context.Background(), pid, ps, placedScope{unit: ps.unit})
	if err != nil || got != (placedScope{unit: ps.unit}) {
		t.Fatalf("adopt = %+v, %v", got, err)
	}
	if len(sd.calls("start")) != 0 {
		t.Errorf("asked systemd for %v: a placed helper stays where it is", sd.calls("start"))
	}
	if r := sd.calls("ref"); !slices.Equal(r, []string{ps.unit}) {
		t.Errorf("references taken: %v; the new daemon needs one to release the scope later", r)
	}
	// A helper that was partial stays so until it next starts.
	again, err := m.adopt(context.Background(), pid, ps, placedScope{unit: ps.unit, partial: true})
	if err != nil || !again.partial {
		t.Fatalf("a partial helper adopted as %+v, %v", again, err)
	}
	if m.accounting(got) != "cgroup" || m.accounting(again) != "partial" {
		t.Fatalf("accounting %q / %q", m.accounting(got), m.accounting(again))
	}
}

// TestAdoptPlacesAHelperThatNeverWasPlaced: one a pre-placement daemon started
// is moved into its scope now, and flagged partial.
func TestAdoptPlacesAHelperThatNeverWasPlaced(t *testing.T) {
	sd := newFakeSystemd()
	sd.moveAfter = 5 * time.Millisecond
	m := newTestManager(sd)
	t.Cleanup(m.close)
	ps, pid := testVM(t), adoptee(t)

	got, err := m.adopt(context.Background(), pid, ps, placedScope{})
	if err != nil || got != (placedScope{unit: ps.unit, partial: true}) {
		t.Fatalf("adopt = %+v, %v", got, err)
	}
	if cg, _ := sd.procCg(pid); !memberOf(cg, ps) {
		t.Fatalf("helper is in %q", cg)
	}
	u, _ := sd.unit(ps.unit)
	if u.props["PIDs"].([]uint32)[0] != uint32(pid) {
		t.Fatalf("scope was made for %v, not for the adopted helper", u.props["PIDs"])
	}
	if m.accounting(got) != "partial" {
		t.Fatalf("accounting %q", m.accounting(got))
	}
	// Its scope is released when it goes, like any other.
	cmdKill(t, pid)
	waitFor(t, "the adopted scope to be released", func() bool { _, ok := sd.unit(ps.unit); return !ok })
}

func cmdKill(t *testing.T, pid int) {
	t.Helper()
	_ = syscall.Kill(pid, syscall.SIGKILL)
	// The adoptee is this test's child; reap it so the scope's process is really gone.
	var ws syscall.WaitStatus
	_, _ = syscall.Wait4(pid, &ws, 0, nil)
}

// TestAdoptLeavesAScopeThatHoldsSomethingElse: a running scope with the helper's
// name that doesn't contain the helper is not made to take it, and not stopped.
func TestAdoptLeavesAScopeThatHoldsSomethingElse(t *testing.T) {
	sd := newFakeSystemd()
	m := newTestManager(sd)
	ps, pid := testVM(t), adoptee(t)
	sd.units[ps.unit] = &fakeUnit{props: map[string]any{}, active: "active"}
	_, err := m.adopt(context.Background(), pid, ps, placedScope{unit: ps.unit})
	if err == nil || !strings.Contains(err.Error(), "holds something else") {
		t.Fatalf("adopt = %v", err)
	}
	if len(sd.calls("start"))+len(sd.calls("stop")) != 0 {
		t.Errorf("touched systemd: %v", sd.log)
	}
}

// TestAdoptDoesNotTrustACgroupSystemdDoesNotKnow: a helper whose cgroup looks
// right but whose unit systemd doesn't list as running isn't adopted on that.
func TestAdoptDoesNotTrustACgroupSystemdDoesNotKnow(t *testing.T) {
	sd := newFakeSystemd()
	m := newTestManager(sd)
	ps, pid := testVM(t), adoptee(t)
	sd.cg[pid] = ps.path() // the cgroup, with no unit behind it
	if _, err := m.adopt(context.Background(), pid, ps, placedScope{unit: ps.unit}); err == nil || !strings.Contains(err.Error(), "doesn't list that scope as running") {
		t.Fatalf("adopt = %v", err)
	}
}

// TestAdoptNeverStopsAScopeItCouldNotFill: if a running guest's move into its
// scope isn't seen in time, the scope is left, not stopped: a late move into a
// stopped scope would be the guest's death.
func TestAdoptNeverStopsAScopeItCouldNotFill(t *testing.T) {
	sd := newFakeSystemd()
	sd.moveAfter = -1
	m := newTestManager(sd)
	m.timeout = 250 * time.Millisecond
	ps, pid := testVM(t), adoptee(t)
	if _, err := m.adopt(context.Background(), pid, ps, placedScope{}); err == nil {
		t.Fatal("adopt succeeded although the helper never moved")
	}
	if len(sd.calls("stop")) != 0 {
		t.Fatalf("stopped %v while adopting a running helper", sd.calls("stop"))
	}
	if !pidAlive(pid) {
		t.Fatal("the helper died")
	}
	if len(m.held) != 0 {
		t.Fatalf("still holding %v after a failed adoption", m.held)
	}
}

func TestAdoptWithoutPlacementDoesNothing(t *testing.T) {
	for _, m := range []*cgroupManager{nil, {mode: engine.CgroupNone}, {mode: engine.CgroupProcess}} {
		got, err := m.adopt(context.Background(), os.Getpid(), testVM(t), placedScope{})
		if err != nil || got != (placedScope{}) {
			t.Errorf("adopt without placement = %+v, %v", got, err)
		}
	}
}

func TestAccountingLabels(t *testing.T) {
	sd := newFakeSystemd()
	for _, tc := range []struct {
		m    *cgroupManager
		s    placedScope
		want string
	}{
		{nil, placedScope{}, engine.CgroupNone},
		{&cgroupManager{mode: engine.CgroupNone}, placedScope{unit: "x.scope"}, engine.CgroupNone},
		{&cgroupManager{mode: engine.CgroupProcess}, placedScope{}, engine.CgroupProcess},
		{newTestManager(sd), placedScope{}, engine.CgroupNone},
		{newTestManager(sd), placedScope{unit: "x.scope"}, "cgroup"},
		{newTestManager(sd), placedScope{unit: "x.scope", partial: true}, "partial"},
	} {
		if got := tc.m.accounting(tc.s); got != tc.want {
			t.Errorf("accounting(%+v) in %v = %q, want %q", tc.s, tc.m.status().Mode, got, tc.want)
		}
	}
}

// TestStartWithoutPlacementIsPlainStart: with nothing to place into, a helper is
// started exactly as before: no gate flag, no extra files, its pid recorded.
func TestStartWithoutPlacementIsPlainStart(t *testing.T) {
	for _, m := range []*cgroupManager{nil, {mode: engine.CgroupNone}, {mode: engine.CgroupProcess}} {
		var persisted []string
		cmd := exec.Command("sleep", "60")
		if err := m.start(context.Background(), cmd, placementSpec{}, func(pid int, unit string) { persisted = append(persisted, fmt.Sprintf("%d %s", pid, unit)) }); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(cmd.Args, []string{"sleep", "60"}) || len(cmd.ExtraFiles) != 0 {
			t.Errorf("args %q, %d extra files: the helper's command line changed without placement", cmd.Args, len(cmd.ExtraFiles))
		}
		if want := fmt.Sprintf("%d ", cmd.Process.Pid); !slices.Equal(persisted, []string{want}) {
			t.Errorf("persisted %q, want %q", persisted, want)
		}
		reap(t, cmd)
	}
}

// TestIsHelperRecognisesAGatedHelper: a helper started to be placed has the
// same argv shape as any other, `<configured-binary> <spec>` (its gate is in the
// environment), so every daemon release, including those before placement,
// tells it apart from any other process. A rolled-back daemon that didn't
// would record its sandbox stopped and boot a second VM on the same disks.
func TestIsHelperRecognisesAGatedHelper(t *testing.T) {
	spec := filepath.Join(t.TempDir(), "vmspec.json")
	pid := gatedStandin(t, spec)
	if args, _ := procArgs(pid); !slices.Equal(args, []string{filepath.Base(os.Args[0]), spec}) || !isHelper(pid, spec, os.Args[0]) {
		t.Fatalf("a gated helper isn't taken for its helper: argv %q", args)
	}
	if isHelper(pid, spec+".other", os.Args[0]) {
		t.Fatal("a helper taken for another sandbox's")
	}
	if isHelper(adoptee(t), spec, os.Args[0]) {
		t.Fatal("sleep taken for a helper")
	}
}

// TestRecoveryKillsAStrayHelperOfAStoppedSandbox: a helper still running the
// spec of a sandbox recorded stopped (a daemon that couldn't recognise it let
// it run) is killed at recovery, before anything can boot that sandbox again
// on the same disks. A running sandbox's helper and an unrelated process
// editing the stopped sandbox's exact spec are left alone.
func TestRecoveryKillsAStrayHelperOfAStoppedSandbox(t *testing.T) {
	stoppedDir, runningDir := t.TempDir(), t.TempDir()
	stray := gatedStandin(t, filepath.Join(stoppedDir, "vmspec.json"))
	editorSpec := filepath.Join(stoppedDir, "vmspec.json")
	editor := gatedStandinAs(t, editorSpec, "vim")
	live := gatedStandin(t, filepath.Join(runningDir, "vmspec.json"))

	killStrays([]*VM{
		{ID: "stopped", SandboxDir: stoppedDir, Status: "stopped"},
		{ID: "running", SandboxDir: runningDir, Status: "running"},
	}, os.Args[0])

	waitFor(t, "the stray to die", func() bool { return !pidAlive(stray) || zombie(stray) })
	if !running(live, filepath.Base(os.Args[0]), filepath.Join(runningDir, "vmspec.json")) {
		t.Fatal("the helper of a running sandbox was killed")
	}
	if !running(editor, "vim", editorSpec) {
		t.Fatal("an unrelated process editing the stopped sandbox's spec was killed")
	}
}

// A recorded PID is no safer than a scanned PID: the exact spec argument alone
// must not authorize killing an unrelated executable.
func TestKillHelperRejectsUnrelatedProcessWithMatchingSpec(t *testing.T) {
	spec := filepath.Join(t.TempDir(), "vmspec.json")
	editor := gatedStandinAs(t, spec, "vim")
	if isHelper(editor, spec, os.Args[0]) {
		t.Error("an unrelated argv[0] was recognised as the configured helper")
	}
	killHelper(editor, spec, os.Args[0])
	if !running(editor, "vim", spec) {
		t.Fatal("killHelper killed an unrelated process with the matching spec")
	}

	helper := gatedStandin(t, spec)
	if !isHelper(helper, spec, os.Args[0]) {
		t.Fatal("the configured helper was not recognised")
	}
	killHelper(helper, spec, os.Args[0])
	waitFor(t, "the configured helper to die", func() bool { return !pidAlive(helper) || zombie(helper) })
}

// zombie reports whether pid has exited but not been reaped: the stand-ins
// are this test's children, so a killed one lingers until the test reaps it.
func zombie(pid int) bool {
	args, err := procArgs(pid)
	return err == nil && len(args) == 0
}

func TestFailedUnrefRemainsPendingForRestart(t *testing.T) {
	sd := newFakeSystemd()
	m := newTestManager(sd)
	t.Cleanup(m.close)
	ps := testVM(t)
	const oldPID = 4242
	sd.units[ps.unit] = &fakeUnit{props: map[string]any{}, active: "inactive", refs: 1}
	m.held[ps.unit] = oldPID
	rejected := true
	sd.unrefErr = func(string) error {
		if rejected {
			rejected = false
			return errors.New("temporarily denied")
		}
		return nil
	}

	m.release(ps.unit, oldPID)
	if got := m.held[ps.unit]; got != oldPID {
		t.Fatalf("failed release forgot pid %d's reference: held %v", oldPID, m.held)
	}
	if u, ok := sd.unit(ps.unit); !ok || u.refs != 1 {
		t.Fatalf("a rejected release changed the systemd reference: %+v, loaded %v", u, ok)
	}
	cmd, err := startHelper(t, m, ps, filepath.Join(t.TempDir(), "worked"), nil)
	if err != nil {
		t.Fatalf("restart could not retry the pending release: %v", err)
	}
	if u, ok := sd.unit(ps.unit); !ok || u.refs != 1 || u.props["PIDs"].([]uint32)[0] != uint32(cmd.Process.Pid) {
		t.Fatalf("restart did not retain its own reference: %+v, loaded %v", u, ok)
	}
}

func TestUnrefTerminalAnswersForgetOnlyTheReleasedReference(t *testing.T) {
	for _, answer := range []error{errNoSuchUnit, errUnitNotReferenced} {
		t.Run(answer.Error(), func(t *testing.T) {
			sd := newFakeSystemd()
			m := newTestManager(sd)
			ps := testVM(t)
			sd.units[ps.unit] = &fakeUnit{props: map[string]any{}, active: "active"}
			m.held[ps.unit] = 4242
			sd.unrefErr = func(string) error { return fmt.Errorf("systemd: %w", answer) }

			m.release(ps.unit, 4242)
			m.release(ps.unit, 4242)
			if len(m.held) != 0 || len(sd.calls("unref")) != 1 {
				t.Fatalf("terminal release was not completed once: held %v, calls %v", m.held, sd.calls("unref"))
			}
		})
	}
}

type pausedUnrefSystemd struct {
	systemdAPI
	entered chan struct{}
	resume  chan struct{}
}

func (s *pausedUnrefSystemd) UnrefUnit(ctx context.Context, unit string) error {
	close(s.entered)
	select {
	case <-s.resume:
		return s.systemdAPI.UnrefUnit(ctx, unit)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestReleaseSerializesConcurrentIncarnationHandover(t *testing.T) {
	sd := newFakeSystemd()
	m := newTestManager(sd)
	ps := testVM(t)
	const oldPID, nextPID = 1001, 2002
	sd.units[ps.unit] = &fakeUnit{props: map[string]any{}, active: "inactive", refs: 1}
	m.held[ps.unit] = oldPID
	paused := &pausedUnrefSystemd{systemdAPI: sd, entered: make(chan struct{}), resume: make(chan struct{})}
	m.sd = paused
	released := make(chan struct{})
	go func() {
		m.release(ps.unit, oldPID)
		close(released)
	}()
	<-paused.entered

	// A takeover must not publish a new incarnation while an earlier watcher
	// can still unreference that name. The fake makes that overlap observable
	// without depending on the scheduler pausing between two mutex operations.
	replaced := make(chan struct{})
	replace := func() {
		sd.mu.Lock()
		sd.units[ps.unit] = &fakeUnit{props: map[string]any{}, active: "active", refs: 1}
		m.held[ps.unit] = nextPID
		sd.mu.Unlock()
		m.mu.Unlock()
		close(replaced)
	}
	if m.mu.TryLock() {
		replace()
	} else {
		go func() {
			m.mu.Lock()
			replace()
		}()
	}
	close(paused.resume)
	<-released
	<-replaced
	if u, ok := sd.unit(ps.unit); !ok || u.refs != 1 || m.held[ps.unit] != nextPID {
		t.Fatalf("old watcher released the next incarnation: unit %+v, loaded %v, held %v", u, ok, m.held)
	}
}

type lostScopeReplySystemd struct {
	systemdAPI
	unit string
	lost bool
}

func (s *lostScopeReplySystemd) StartTransientUnit(ctx context.Context, name string, props []unitProp) error {
	err := s.systemdAPI.StartTransientUnit(ctx, name, props)
	if err != nil || name != s.unit || s.lost {
		return err
	}
	s.lost = true
	<-ctx.Done()
	return ctx.Err()
}

func (s *lostScopeReplySystemd) Units(ctx context.Context, pattern string) ([]unitStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.systemdAPI.Units(ctx, pattern)
}

func (s *lostScopeReplySystemd) UnrefUnit(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.systemdAPI.UnrefUnit(ctx, name)
}

func TestStartReconcilesLostScopeReply(t *testing.T) {
	netd, err := netdPlacement(testOwner)
	if err != nil {
		t.Fatal(err)
	}
	for _, ps := range []placementSpec{testVM(t), netd} {
		t.Run(ps.unit, func(t *testing.T) {
			sd := newFakeSystemd()
			m := newTestManager(sd)
			m.timeout = 50 * time.Millisecond
			m.sd = &lostScopeReplySystemd{systemdAPI: sd, unit: ps.unit}
			t.Cleanup(m.close)
			marker := filepath.Join(t.TempDir(), "worked")
			cmd, err := startHelper(t, m, ps, marker, nil)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("lost reply = %v, want the interrupted creation error", err)
			}
			if pidAlive(cmd.Process.Pid) || cmd.ProcessState == nil || exists(marker) {
				t.Fatal("the helper escaped its failed placement")
			}
			if got := sd.calls("unref"); !slices.Equal(got, []string{ps.unit}) {
				t.Fatalf("accepted creation's reference was not reconciled: %v", got)
			}
			waitFor(t, "the failed scope to be collected", func() bool { _, ok := sd.unit(ps.unit); return !ok })
			if _, err := startHelper(t, m, ps, filepath.Join(t.TempDir(), "retry"), nil); err != nil {
				t.Fatalf("same-name retry after a lost reply: %v", err)
			}
		})
	}
}

func TestAdoptReconcilesLostReplyWithoutStoppingTheHelper(t *testing.T) {
	sd := newFakeSystemd()
	m := newTestManager(sd)
	m.timeout = 50 * time.Millisecond
	t.Cleanup(m.close)
	ps, pid := testVM(t), adoptee(t)
	m.sd = &lostScopeReplySystemd{systemdAPI: sd, unit: ps.unit}

	if _, err := m.adopt(context.Background(), pid, ps, placedScope{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("adopt = %v, want the interrupted creation error", err)
	}
	if len(sd.calls("stop")) != 0 || !pidAlive(pid) {
		t.Fatal("reconciling adoption stopped a running helper")
	}
	if u, ok := sd.unit(ps.unit); !ok || u.refs != 0 || len(m.held) != 0 {
		t.Fatalf("uncertain adoption leaked its reference: %+v, loaded %v, held %v", u, ok, m.held)
	}
}
