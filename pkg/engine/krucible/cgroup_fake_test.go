package krucible

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/launchgate"
)

// fakeSystemd is systemd as placement sees it, with the two behaviours placement
// has to be right about: a request is answered before the pid has moved, and
// units outlive their last process while something holds a reference. Any
// platform can run it; the tests against the real one are
// cgroup_systemd_test.go.
type fakeSystemd struct {
	mu    sync.Mutex
	units map[string]*fakeUnit
	log   []string       // "start <unit>", "stop <unit>", "ref <unit>", "unref <unit>"
	cg    map[int]string // pid → the cgroup /proc/<pid>/cgroup shows
	root  string         // what a manager that isn't the host's puts before every path

	moveAfter time.Duration // how long after the answer a pid shows in its scope; <0: never
	startErr  func(name string) error
	unrefErr  func(name string) error
	onStart   func(name string, props map[string]any) // runs inside StartTransientUnit, before it answers
	hold      chan struct{}                           // if set, a scope's StartTransientUnit waits for it to close
	inflight  int
	maxFlight int
}

type fakeUnit struct {
	props  map[string]any
	active string
	refs   int
}

func newFakeSystemd() *fakeSystemd {
	return &fakeSystemd{units: map[string]*fakeUnit{}, cg: map[int]string{}}
}

func propMap(props []unitProp) map[string]any {
	m := map[string]any{}
	for _, p := range props {
		m[p.Name] = p.Value
	}
	return m
}

func (f *fakeSystemd) record(format string, args ...any) {
	f.log = append(f.log, fmt.Sprintf(format, args...))
}

// calls returns the log lines that start with verb.
func (f *fakeSystemd) calls(verb string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, l := range f.log {
		if strings.HasPrefix(l, verb+" ") {
			out = append(out, strings.TrimPrefix(l, verb+" "))
		}
	}
	return out
}

func (f *fakeSystemd) unit(name string) (fakeUnit, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.units[name]
	if !ok {
		return fakeUnit{}, false
	}
	return *u, true
}

func (f *fakeSystemd) StartTransientUnit(ctx context.Context, name string, props []unitProp) error {
	scope := strings.HasSuffix(name, ".scope")
	f.mu.Lock()
	f.inflight++
	f.maxFlight = max(f.maxFlight, f.inflight)
	hold, onStart := f.hold, f.onStart
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inflight--
		f.mu.Unlock()
	}()
	pm := propMap(props)
	if scope && hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if onStart != nil {
		onStart(name, pm)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("start %s", name)
	if f.startErr != nil {
		if err := f.startErr(name); err != nil {
			return err
		}
	}
	if _, ok := f.units[name]; ok {
		return fmt.Errorf("%w: fake: %s was already loaded", errUnitExists, name)
	}
	u := &fakeUnit{props: pm, active: "active"}
	if ref, _ := pm["AddRef"].(bool); ref {
		u.refs++
	}
	f.units[name] = u
	if pids, ok := pm["PIDs"].([]uint32); ok && len(pids) == 1 && f.moveAfter >= 0 {
		pid, where := int(pids[0]), f.root+"/"+topSlice+"/"+pm["Slice"].(string)+"/"+name
		move := func() {
			f.mu.Lock()
			f.cg[pid] = where
			f.mu.Unlock()
			// Systemd ends a scope when its last process goes.
			go func() {
				for pidAlive(pid) {
					time.Sleep(2 * time.Millisecond)
				}
				f.end(name)
			}()
		}
		time.AfterFunc(f.moveAfter, move)
	}
	return nil
}

// end is a unit's last process leaving it: systemd forgets the unit unless
// something still references it.
func (f *fakeSystemd) end(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u, ok := f.units[name]; ok {
		u.active = "inactive"
		if u.refs <= 0 {
			delete(f.units, name)
		}
	}
}

func (f *fakeSystemd) StopUnit(ctx context.Context, name string) error {
	f.mu.Lock()
	_, ok := f.units[name]
	f.record("stop %s", name)
	f.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: fake", errNoSuchUnit)
	}
	f.end(name)
	return nil
}

func (f *fakeSystemd) Units(ctx context.Context, pattern string) ([]unitStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []unitStatus
	for name, u := range f.units {
		if ok, _ := path.Match(pattern, name); ok {
			desc, _ := u.props["Description"].(string)
			out = append(out, unitStatus{Name: name, Description: desc, Active: u.active})
		}
	}
	return out, nil
}

func (f *fakeSystemd) RefUnit(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.units[name]
	if !ok {
		return fmt.Errorf("%w: fake", errNoSuchUnit)
	}
	f.record("ref %s", name)
	u.refs++
	return nil
}

func (f *fakeSystemd) UnrefUnit(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.units[name]
	if !ok {
		return fmt.Errorf("%w: fake", errNoSuchUnit)
	}
	f.record("unref %s", name)
	if f.unrefErr != nil {
		if err := f.unrefErr(name); err != nil {
			return err
		}
	}
	if u.refs == 0 {
		return fmt.Errorf("%w: fake", errUnitNotReferenced)
	}
	u.refs--
	if u.active != "active" && u.refs <= 0 {
		delete(f.units, name)
	}
	return nil
}

func (f *fakeSystemd) Usage(ctx context.Context, name string) (unitUsage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.units[name]; !ok {
		return unitUsage{}, fmt.Errorf("%w: fake", errNoSuchUnit)
	}
	f.record("usage %s", name)
	return unitUsage{CPUNanos: 41e9, MemoryPeak: 1 << 30, IORead: 7, IOWrite: 9, Result: "success"}, nil
}

func (f *fakeSystemd) Close() error { return nil }

// procCg is /proc/<pid>/cgroup: where pid is, once the fake has moved it.
// Outside any scope a process is in the cgroup the daemon's own children start in.
func (f *fakeSystemd) procCg(pid int) (string, error) {
	if !pidAlive(pid) {
		return "", fmt.Errorf("pid %d: %w", pid, syscall.ESRCH)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if cg, ok := f.cg[pid]; ok {
		return cg, nil
	}
	return "/system.slice/bhatti.service", nil
}

// place puts pid straight into the cgroup it would have in unit, as a previous
// daemon's placement left it.
func (f *fakeSystemd) place(pid int, ps placementSpec) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cg[pid] = f.root + ps.path()
	f.units[ps.unit] = &fakeUnit{props: map[string]any{"Description": ps.desc}, active: "active"}
}

// newTestManager is a manager in systemd mode on the fake.
func newTestManager(sd *fakeSystemd) *cgroupManager {
	return &cgroupManager{
		mode: engine.CgroupSystemd, requested: "systemd",
		sd: sd, procCg: sd.procCg,
		slots: make(chan struct{}, maxPlacements), timeout: 3 * time.Second,
		slices: map[string]bool{}, held: map[string]int{}, stop: make(chan struct{}),
	}
}

// gatedHelperScript is a stand-in helper with the gate protocol and no hypervisor:
// `<script> <marker>`, its gate in the environment, reads its gate first, then
// works (writes <marker>) and stays up. A failed gate leaves <marker>.failed
// and exit 3.
func gatedHelperScript(t *testing.T) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "helper.sh")
	body := "#!/bin/sh\n" +
		"marker=$1\n" +
		"eval \"b=\\$(dd bs=1 count=1 <&${" + launchgate.Env + ":?} 2>/dev/null)\"\n" +
		"if [ \"$b\" != G ]; then echo \"gate: $b\" > \"$marker.failed\"; exit 3; fi\n" +
		"echo worked > \"$marker\"\n" +
		"exec sleep 60\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// helperCmd is the command the daemon would start for the stand-in script.
func helperCmd(t *testing.T, marker string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(gatedHelperScript(t), marker)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// waitFor polls cond for up to 10 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

const (
	testOwner = "0123456789ab"
	testID    = "00000000000000a1"
)

func testVM(t *testing.T) placementSpec {
	t.Helper()
	ps, err := vmPlacement(testOwner, testID)
	if err != nil {
		t.Fatal(err)
	}
	ps.alive = pidAlive
	return ps
}

// killHelper stops a stand-in the test started, and reaps it.
func reap(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
}
