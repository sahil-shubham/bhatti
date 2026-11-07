package collector

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/gateway"
)

const (
	ownerA = "aaaaaaaaaaaa"
	sbA    = "0000000000000001"
	sbB    = "0000000000000002"
)

func scopePath(owner, id string) string {
	return "/bhatti.slice/bhatti-" + owner + ".slice/bhatti-" + owner + "-" + id + ".scope"
}
func slicePath(owner string) string { return "/bhatti.slice/bhatti-" + owner + ".slice" }
func netdPath(owner string) string {
	return "/bhatti.slice/bhatti-" + owner + ".slice/bhatti-" + owner + "-netd.scope"
}

// cgroupTree is a cgroup v2 mount in a temp dir, whose cgroups are made of the
// recorded files.
type cgroupTree struct {
	t    *testing.T
	root string
}

func newCgroupTree(t *testing.T) *cgroupTree { return &cgroupTree{t: t, root: t.TempDir()} }

// put makes the cgroup at path hold the files of the recorded cgroup fixture.
func (tr *cgroupTree) put(path, fixtureName string) {
	tr.t.Helper()
	src := fixtureDir(tr.t, "cgroup/"+fixtureName)
	dst := filepath.Join(tr.root, path)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		tr.t.Fatal(err)
	}
	ents, err := os.ReadDir(src)
	if err != nil {
		tr.t.Fatal(err)
	}
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			tr.t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o644); err != nil {
			tr.t.Fatal(err)
		}
	}
}

func (tr *cgroupTree) remove(path string) {
	tr.t.Helper()
	if err := os.RemoveAll(filepath.Join(tr.root, path)); err != nil {
		tr.t.Fatal(err)
	}
}

func (tr *cgroupTree) write(path, file, content string) {
	tr.t.Helper()
	if err := os.WriteFile(filepath.Join(tr.root, path, file), []byte(content), 0o644); err != nil {
		tr.t.Fatal(err)
	}
}

// fakeEngine is an engine's StatsSource whose listing a test sets.
type fakeEngine struct {
	mu sync.Mutex
	t  engine.StatsTargets
}

func (f *fakeEngine) StatsTargets() engine.StatsTargets {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeEngine) set(t engine.StatsTargets) {
	f.mu.Lock()
	f.t = t
	f.mu.Unlock()
}

// fakeClock moves only when a test says so.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// rig is a collector over a fake engine, a fake cgroup tree, the recorded /proc
// and a clock of its own.
type rig struct {
	t     *testing.T
	c     *Collector
	eng   *fakeEngine
	tree  *cgroupTree
	clock *fakeClock
	net   map[string]gateway.NetStats // control socket → what that gateway says
	netMu sync.Mutex
	// netErr, if set, is what asking the gateway at the socket returns.
	netErr map[string]error
}

func newRig(t *testing.T, mod func(*Config)) *rig {
	t.Helper()
	r := &rig{t: t, eng: &fakeEngine{}, tree: newCgroupTree(t), clock: &fakeClock{now: t0},
		net: map[string]gateway.NetStats{}, netErr: map[string]error{}}
	cfg := Config{
		Source: r.eng, DataDir: t.TempDir(), CgroupRoot: r.tree.root,
		ProcRoot: filepath.Join("testdata", "linux", "proc"), BlockRoot: t.TempDir(),
		Interval: 10 * time.Second, Now: r.clock.Now,
	}
	if mod != nil {
		mod(&cfg)
	}
	r.c = New(cfg)
	r.c.readHost = r.c.host.readProc // the recorded /proc, whatever OS the test runs on
	r.c.netStats = func(ctx context.Context, sock string) (gateway.NetStats, error) {
		r.netMu.Lock()
		defer r.netMu.Unlock()
		if err := r.netErr[sock]; err != nil {
			return gateway.NetStats{}, err
		}
		return r.net[sock], nil
	}
	return r
}

func (r *rig) sample() *Snapshot {
	r.t.Helper()
	return r.c.Sample(context.Background())
}

// oneVM is the listing of a single sandbox, its owner and its gateway, all
// placed in cgroups, as a systemd host with one running sandbox has it.
func oneVM(pid int) engine.StatsTargets {
	return engine.StatsTargets{
		Sandboxes: []engine.StatsSandbox{{
			ID: sbA, Owner: ownerA, UserID: "alice", PID: pid, Accounting: engine.AccountingCgroup,
			Cgroup: scopePath(ownerA, sbA), NetIP: "100.64.0.2",
		}},
		Owners: []engine.StatsOwner{{Owner: ownerA, UserID: "alice", Cgroup: slicePath(ownerA)}},
		Netds: []engine.StatsNetd{{
			Owner: ownerA, PID: 900, Accounting: engine.AccountingCgroup, Cgroup: netdPath(ownerA), CtlSock: "/run/netd-a/ctl.sock",
		}},
	}
}

// busyRig has one running, placed sandbox with a real busy scope's files, and
// its owner and gateway.
func busyRig(t *testing.T, mod func(*Config)) *rig {
	t.Helper()
	r := newRig(t, mod)
	r.tree.put(scopePath(ownerA, sbA), "scope-busy")
	r.tree.put(slicePath(ownerA), "slice-owner")
	r.tree.put(netdPath(ownerA), "scope-netd")
	r.eng.set(oneVM(100))
	r.net["/run/netd-a/ctl.sock"] = gateway.NetStats{Guests: []gateway.GuestStats{
		{GuestIP: "100.64.0.2", SandboxID: "srv-1", RxBytes: 5000, RxPackets: 50, TxBytes: 3000, TxPackets: 30,
			Dropped: gateway.DropStats{Policy: 2, Spoof: 1, HardDeny: 3}},
		{GuestIP: "100.64.0.99", SandboxID: "gone", RxBytes: 1}, // a guest nobody here knows
	}}
	return r
}

// captureLogs sends slog to a buffer for the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

func TestSampleReadsEachSandboxOwnerAndGateway(t *testing.T) {
	r := busyRig(t, nil)
	// A second sandbox that isn't running has nothing to read.
	tg := oneVM(100)
	tg.Sandboxes = append(tg.Sandboxes,
		engine.StatsSandbox{ID: sbB, Owner: ownerA, UserID: "alice", Accounting: engine.AccountingNone},
	)
	r.eng.set(tg)
	snap := r.sample()

	if len(snap.Sandboxes) != 1 {
		t.Fatalf("%d sandboxes measured, want only the running one: %+v", len(snap.Sandboxes), snap.Sandboxes)
	}
	sb := snap.Sandboxes[0]
	if sb.ID != sbA || sb.Owner != ownerA || sb.UserID != "alice" || sb.Accounting != "cgroup" || sb.PID != 100 || !sb.At.Equal(t0) {
		t.Errorf("sandbox = %+v", sb)
	}
	if sb.CPU == nil || sb.CPU.UsageUsec != 7080860 || sb.Memory == nil || sb.Memory.Current != 103555072 || sb.Memory.Peak != 104243200 {
		t.Errorf("usage = cpu %+v memory %+v", sb.CPU, sb.Memory)
	}
	if sb.IO == nil || sb.IO.WriteBytes != 1677721600 || sb.Pressure == nil || sb.Events == nil {
		t.Errorf("usage = io %+v pressure %+v events %+v", sb.IO, sb.Pressure, sb.Events)
	}
	if sb.Rates != nil {
		t.Errorf("rates %+v from a first sample", sb.Rates)
	}
	// Its gateway's figures for the guest at its address, and not for the other.
	want := Net{RxBytes: 5000, RxPackets: 50, TxBytes: 3000, TxPackets: 30, DroppedPolicy: 2, DroppedSpoof: 1, DroppedHardDeny: 3}
	if sb.Net == nil || *sb.Net != want {
		t.Errorf("net = %+v, want %+v", sb.Net, want)
	}

	if len(snap.Owners) != 1 || snap.Owners[0].ID != ownerA || snap.Owners[0].UserID != "alice" || snap.Owners[0].CPU.UsageUsec != 7378371 {
		t.Errorf("owners = %+v", snap.Owners)
	}
	if len(snap.Netds) != 1 || snap.Netds[0].Owner != ownerA || snap.Netds[0].PID != 900 || snap.Netds[0].StatsErr != "" || snap.Netds[0].CPU == nil {
		t.Errorf("netds = %+v", snap.Netds)
	}
	h := snap.Host
	if h == nil || h.CPUs != 4 || h.CPU == nil || h.Memory == nil || h.Pressure == nil || h.Disk == nil || h.CPUPercent != nil {
		t.Errorf("host = %+v", h)
	}
	if got := r.c.Snapshot(); got != snap {
		t.Error("the sample wasn't published")
	}
	if got, ok := snap.Sandbox(sbA); !ok || got.ID != sbA {
		t.Errorf("Sandbox(%s) = %+v, %v", sbA, got, ok)
	}
	if _, ok := snap.Sandbox(sbB); ok {
		t.Error("the stopped sandbox is in the snapshot")
	}
}

func TestRatesAndHistoryFollowTheCounters(t *testing.T) {
	r := busyRig(t, nil)
	r.sample()
	if n := len(r.c.Series(sbA)); n != 0 {
		t.Fatalf("%d points after one sample: a point is an interval, and there isn't one yet", n)
	}

	// Five seconds later the kernel's counters have moved to what they really
	// were then.
	elapsed, _ := strconv.ParseFloat(strings.TrimSpace(string(fixture(t, "cgroup/scope-busy-later/ELAPSED"))), 64)
	r.tree.put(scopePath(ownerA, sbA), "scope-busy-later")
	r.tree.write(slicePath(ownerA), "cpu.stat", "usage_usec 12378371\nuser_usec 6404900\nsystem_usec 5973471\n") // five more seconds
	r.clock.advance(time.Duration(elapsed * float64(time.Second)))
	r.net["/run/netd-a/ctl.sock"] = gateway.NetStats{Guests: []gateway.GuestStats{
		{GuestIP: "100.64.0.2", RxBytes: 5000 + 10_000, TxBytes: 3000, RxPackets: 60, TxPackets: 30},
	}}
	snap := r.sample()

	sb, _ := snap.Sandbox(sbA)
	if sb.Rates == nil {
		t.Fatal("no rates from a second sample")
	}
	if want := (13196603.0 - 7080860.0) / 1e6 / elapsed * 100; !near(sb.Rates.CPUPercent, want, 1e-6) {
		t.Errorf("cpu = %.3f%%, want %.3f%%", sb.Rates.CPUPercent, want)
	}
	if want := 10000 / elapsed; !near(sb.Rates.NetRxBytesPerSec, want, 1e-6) || sb.Rates.NetTxBytesPerSec != 0 {
		t.Errorf("net rate = rx %.1f tx %.1f B/s, want %.1f and 0", sb.Rates.NetRxBytesPerSec, sb.Rates.NetTxBytesPerSec, want)
	}
	series := r.c.Series(sbA)
	if len(series) != 1 {
		t.Fatalf("%d points, want 1", len(series))
	}
	p := series[0]
	if !p.At.Equal(snap.At) || !near(float64(p.CPUPercent), sb.Rates.CPUPercent, 1e-3) || p.MemoryBytes != 103546880 {
		t.Errorf("point = %+v, rates %+v", p, sb.Rates)
	}
	if o := snap.Owners[0]; o.Rates == nil || !near(o.Rates.CPUPercent, 5.0/elapsed*100, 1e-6) {
		t.Errorf("the owner's rates = %+v, want %.3f%% of CPU", o.Rates, 5.0/elapsed*100)
	}
	// The recorded /proc/stat doesn't move, so no CPU time passed on the host:
	// there is nothing to divide by, and no figure is better than a made-up one.
	if snap.Host.CPUPercent != nil {
		t.Errorf("host cpu = %v%% from a /proc/stat that didn't move", *snap.Host.CPUPercent)
	}
}

func TestAVMMRestartIsNotANegativeRateAndKeepsItsHistory(t *testing.T) {
	r := busyRig(t, nil)
	r.tree.put(scopePath(ownerA, sbA), "scope-busy-later") // 13.2 s of CPU so far
	r.sample()

	// The VMM dies and starts again: a new pid, a new scope, counters at 7.08 s
	// (the fixture's busy scope) instead of 13.2.
	r.clock.advance(10 * time.Second)
	r.tree.put(scopePath(ownerA, sbA), "scope-busy")
	r.eng.set(oneVM(101))
	snap := r.sample()
	sb, _ := snap.Sandbox(sbA)
	if sb.Rates == nil || sb.Rates.CPUPercent < 0 || !near(sb.Rates.CPUPercent, 7.080860/10*100, 1e-6) {
		t.Fatalf("rates across the restart = %+v, want 70.8%% (what the new VMM used in the interval)", sb.Rates)
	}
	if sb.Rates.IOWriteBytesPerSec != 1677721600.0/10 {
		t.Errorf("write rate = %v, want the new scope's counter over the interval", sb.Rates.IOWriteBytesPerSec)
	}

	// And the restart that goes unnoticed in the pid: the same pid, its scope
	// recreated with lower counters.
	r.clock.advance(10 * time.Second)
	r.tree.put(scopePath(ownerA, sbA), "scope-idle")
	sb, _ = r.sample().Sandbox(sbA)
	if sb.Rates == nil || sb.Rates.CPUPercent < 0 || sb.Rates.CPUPercent > 1 || sb.Rates.IOWriteBytesPerSec != 0 {
		t.Errorf("rates when counters fall under one pid = %+v", sb.Rates)
	}
	if n := len(r.c.Series(sbA)); n != 2 {
		t.Errorf("%d points, want the two intervals after the first sample", n)
	}
	for _, p := range r.c.Series(sbA) {
		if p.CPUPercent < 0 || p.IOWriteBPS < 0 {
			t.Errorf("negative point %+v", p)
		}
	}
}

func TestHistoryHoldsAnHourAndForgetsTheOldest(t *testing.T) {
	r := busyRig(t, func(c *Config) { c.Retain = 30 * time.Second }) // 3 points at 10 s
	var wantTimes []time.Time
	r.sample()
	for range 7 {
		r.clock.advance(10 * time.Second)
		snap := r.sample()
		wantTimes = append(wantTimes, snap.At)
	}
	got := r.c.Series(sbA)
	if len(got) != 3 {
		t.Fatalf("%d points, want the capacity of 3", len(got))
	}
	for i, p := range got {
		if want := wantTimes[len(wantTimes)-3+i]; !p.At.Equal(want) {
			t.Errorf("point %d at %v, want %v: not the newest three, oldest first", i, p.At, want)
		}
	}

	// The default keeps an hour of 10 s samples: 360 points.
	if c := New(Config{}); c.capacity != 360 {
		t.Errorf("default capacity = %d, want 360", c.capacity)
	}
}

func TestHistoryOutlivesTheVMNotTheSandbox(t *testing.T) {
	r := busyRig(t, func(c *Config) { c.Retain = time.Minute })
	r.sample()
	r.clock.advance(10 * time.Second)
	r.sample()
	if len(r.c.Series(sbA)) != 1 {
		t.Fatal("no history to keep")
	}

	// The VM stops; the engine still knows the sandbox. Its history stays, and
	// it is no longer in the snapshot.
	stopped := oneVM(0)
	stopped.Sandboxes[0].Accounting = engine.AccountingNone
	stopped.Sandboxes[0].Cgroup = ""
	stopped.Owners, stopped.Netds = nil, nil
	r.eng.set(stopped)
	r.clock.advance(10 * time.Second)
	snap := r.sample()
	if _, ok := snap.Sandbox(sbA); ok {
		t.Error("a stopped VM is in the snapshot")
	}
	if len(r.c.Series(sbA)) != 1 {
		t.Error("the history went with the VM")
	}

	// It starts again, and the history carries on from where it was.
	r.eng.set(oneVM(102))
	r.clock.advance(10 * time.Second)
	r.sample() // the first sample of the new VM: no interval yet
	r.clock.advance(10 * time.Second)
	r.sample()
	if n := len(r.c.Series(sbA)); n != 2 {
		t.Errorf("%d points after a restart, want the old one and the new", n)
	}

	// Stopped for longer than the retention: gone.
	r.eng.set(stopped)
	r.clock.advance(61 * time.Second)
	r.sample()
	if r.c.Series(sbA) != nil {
		t.Error("the history of a sandbox that has been stopped past the retention is still held")
	}
}

func TestHistoryIsDroppedWhenTheEngineForgetsTheSandbox(t *testing.T) {
	r := busyRig(t, nil)
	r.sample()
	r.clock.advance(10 * time.Second)
	r.sample()
	if len(r.c.Series(sbA)) != 1 {
		t.Fatal("no history to forget")
	}
	r.eng.set(engine.StatsTargets{}) // destroyed
	r.clock.advance(10 * time.Second)
	r.sample()
	if r.c.Series(sbA) != nil {
		t.Error("the history of a destroyed sandbox is kept")
	}
	if len(r.c.prevSandbox) != 0 || len(r.c.prevOwner) != 0 || len(r.c.prevNetd) != 0 {
		t.Errorf("baselines kept for what is gone: %d %d %d", len(r.c.prevSandbox), len(r.c.prevOwner), len(r.c.prevNetd))
	}
}

func TestSeriesIsACopy(t *testing.T) {
	r := busyRig(t, nil)
	r.sample()
	r.clock.advance(10 * time.Second)
	r.sample()
	s := r.c.Series(sbA)
	s[0].CPUPercent = 12345
	if r.c.Series(sbA)[0].CPUPercent == 12345 {
		t.Error("a caller changed the collector's history")
	}
}

func TestACgroupThatVanishesMidSampleIsSkippedQuietly(t *testing.T) {
	logs := captureLogs(t)
	r := busyRig(t, nil)
	r.tree.remove(scopePath(ownerA, sbA)) // the VM exited after the engine listed it
	snap := r.sample()
	if _, ok := snap.Sandbox(sbA); ok {
		t.Error("a sandbox whose cgroup is gone was measured")
	}
	if len(snap.Owners) != 1 || len(snap.Netds) != 1 {
		t.Errorf("the rest wasn't measured: %d owners, %d gateways", len(snap.Owners), len(snap.Netds))
	}
	if strings.Contains(logs.String(), "collector.read_failed") {
		t.Errorf("a cgroup going away was logged as a failure:\n%s", logs)
	}
}

func TestAReadFaultIsLoggedOnceForAsLongAsItLasts(t *testing.T) {
	logs := captureLogs(t)
	r := busyRig(t, nil)
	// A directory where io.stat should be: a fault, and not a missing file.
	if err := os.Remove(filepath.Join(r.tree.root, scopePath(ownerA, sbA), "io.stat")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(r.tree.root, scopePath(ownerA, sbA), "io.stat"), 0o755); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		r.clock.advance(10 * time.Second)
		snap := r.sample()
		sb, ok := snap.Sandbox(sbA)
		if !ok || sb.CPU == nil || sb.IO != nil {
			t.Fatalf("what could be read wasn't kept, or the unreadable was made up: %+v", sb)
		}
	}
	if n := strings.Count(logs.String(), "collector.read_failed"); n != 1 {
		t.Fatalf("%d warnings over three samples of one lasting fault:\n%s", n, logs)
	}
	if !strings.Contains(logs.String(), "io.stat") || !strings.Contains(logs.String(), "sandbox "+sbA) {
		t.Errorf("the warning doesn't say what failed:\n%s", logs)
	}

	// Fixed, then broken again: it is news again.
	if err := os.Remove(filepath.Join(r.tree.root, scopePath(ownerA, sbA), "io.stat")); err != nil {
		t.Fatal(err)
	}
	r.tree.write(scopePath(ownerA, sbA), "io.stat", "")
	r.clock.advance(10 * time.Second)
	r.sample()
	if err := os.Remove(filepath.Join(r.tree.root, scopePath(ownerA, sbA), "io.stat")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(r.tree.root, scopePath(ownerA, sbA), "io.stat"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.clock.advance(10 * time.Second)
	r.sample()
	if n := strings.Count(logs.String(), "collector.read_failed"); n != 2 {
		t.Errorf("%d warnings after the fault came back, want 2", n)
	}
}

func TestAGatewayThatPredatesStatsIsNotedOnceAndLeavesTheRestAlone(t *testing.T) {
	logs := captureLogs(t)
	r := busyRig(t, nil)
	r.netErr["/run/netd-a/ctl.sock"] = fmt.Errorf("stats: %w", gateway.ErrStatsUnsupported)
	for range 3 {
		r.clock.advance(10 * time.Second)
		snap := r.sample()
		sb, _ := snap.Sandbox(sbA)
		if sb.Net != nil {
			t.Fatalf("net figures from a gateway that has none: %+v", sb.Net)
		}
		if sb.CPU == nil || len(snap.Netds) != 1 || snap.Netds[0].CPU == nil {
			t.Fatalf("the cgroup figures were lost with the gateway's: %+v", snap)
		}
		if !strings.Contains(snap.Netds[0].StatsErr, "predates") {
			t.Errorf("StatsErr = %q", snap.Netds[0].StatsErr)
		}
	}
	if n := strings.Count(logs.String(), "collector.netd_stats_unsupported"); n != 1 {
		t.Errorf("%d notices over three samples:\n%s", n, logs)
	}
	if strings.Contains(logs.String(), "collector.netd_stats_failed") {
		t.Errorf("an old gateway was logged as a failure:\n%s", logs)
	}

	// Replaced by a gateway that answers: its figures come in.
	delete(r.netErr, "/run/netd-a/ctl.sock")
	r.clock.advance(10 * time.Second)
	snap := r.sample()
	if sb, _ := snap.Sandbox(sbA); sb.Net == nil || snap.Netds[0].StatsErr != "" {
		t.Errorf("after the gateway was replaced: net %+v, err %q", sb.Net, snap.Netds[0].StatsErr)
	}
}

func TestAGatewayThatFailsIsLoggedOnceWithItsReason(t *testing.T) {
	logs := captureLogs(t)
	r := busyRig(t, nil)
	r.netErr["/run/netd-a/ctl.sock"] = errors.New("dial unix /run/netd-a/ctl.sock: connect: connection refused")
	for range 3 {
		r.clock.advance(10 * time.Second)
		snap := r.sample()
		if !strings.Contains(snap.Netds[0].StatsErr, "connection refused") {
			t.Fatalf("StatsErr = %q", snap.Netds[0].StatsErr)
		}
	}
	if n := strings.Count(logs.String(), "collector.netd_stats_failed"); n != 1 {
		t.Errorf("%d warnings over three samples:\n%s", n, logs)
	}
}

func TestASandboxWithNoAccountingIsListedWithOnlyItsNetwork(t *testing.T) {
	// A host that doesn't place helpers in cgroups (accounting none) still has a
	// gateway that counts each guest's traffic.
	r := busyRig(t, nil)
	tg := oneVM(100)
	tg.Sandboxes[0].Accounting = engine.AccountingNone
	tg.Sandboxes[0].Cgroup = ""
	tg.Owners = nil
	tg.Netds[0].Accounting = engine.AccountingNone
	tg.Netds[0].Cgroup = ""
	r.eng.set(tg)
	snap := r.sample()
	sb, ok := snap.Sandbox(sbA)
	if !ok || sb.Accounting != "none" || sb.CPU != nil || sb.Memory != nil || sb.IO != nil || sb.Pressure != nil || sb.Events != nil {
		t.Fatalf("sandbox = %+v", sb)
	}
	if sb.Net == nil || sb.Net.RxBytes != 5000 {
		t.Errorf("net = %+v", sb.Net)
	}
	if len(snap.Netds) != 1 || snap.Netds[0].CPU != nil {
		t.Errorf("netds = %+v", snap.Netds)
	}
}

func TestProcessAccountingReadsPerProcessFigures(t *testing.T) {
	// macOS: no cgroups, one process per VM and per gateway.
	r := newRig(t, nil)
	var mu sync.Mutex
	cpu := map[int]uint64{300: 1_000_000, 301: 500_000}
	r.c.procUsage = func(pid int) (Usage, error) {
		mu.Lock()
		defer mu.Unlock()
		if pid == 666 {
			return Usage{}, errGone
		}
		return Usage{
			CPU:    &CPU{UsageUsec: cpu[pid], UserUsec: cpu[pid]},
			Memory: &Memory{Current: uint64(pid) << 20},
			IO:     &IO{ReadBytes: cpu[pid] * 10},
		}, nil
	}
	r.eng.set(engine.StatsTargets{
		Sandboxes: []engine.StatsSandbox{
			{ID: sbA, Owner: ownerA, PID: 300, Accounting: engine.AccountingProcess, NetIP: "100.64.0.2"},
			{ID: sbB, Owner: ownerA, PID: 666, Accounting: engine.AccountingProcess}, // exited since it was listed
		},
		Netds: []engine.StatsNetd{{Owner: ownerA, PID: 301, Accounting: engine.AccountingProcess, CtlSock: "/c"}},
	})
	r.net["/c"] = gateway.NetStats{Guests: []gateway.GuestStats{{GuestIP: "100.64.0.2", TxBytes: 7}}}
	snap := r.sample()

	if len(snap.Sandboxes) != 1 || snap.Sandboxes[0].Accounting != "process" || snap.Sandboxes[0].CPU.UsageUsec != 1_000_000 {
		t.Fatalf("sandboxes = %+v", snap.Sandboxes)
	}
	if sb := snap.Sandboxes[0]; sb.Memory.Current != 300<<20 || sb.Net == nil || sb.Net.TxBytes != 7 || sb.Memory.HasPeak || sb.IO.HasOps {
		t.Errorf("sandbox = %+v net %+v", sb, sb.Net)
	}
	if len(snap.Owners) != 0 {
		t.Errorf("owners from a host without slices: %+v", snap.Owners)
	}
	if len(snap.Netds) != 1 || snap.Netds[0].CPU.UsageUsec != 500_000 {
		t.Errorf("netds = %+v", snap.Netds)
	}

	// A CPU burner for 10 s is 100% of one CPU.
	mu.Lock()
	cpu[300] += 10_000_000
	mu.Unlock()
	r.clock.advance(10 * time.Second)
	sb, _ := r.sample().Sandbox(sbA)
	if sb.Rates == nil || !near(sb.Rates.CPUPercent, 100, 1e-9) {
		t.Errorf("rates = %+v, want 100%% of one CPU", sb.Rates)
	}
}

func TestSampleCutShortByStopIsNotPublished(t *testing.T) {
	r := busyRig(t, nil)
	first := r.sample()
	r.clock.advance(10 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	r.c.netStats = func(ctx context.Context, sock string) (gateway.NetStats, error) {
		cancel() // the daemon is stopping while the gateway is being asked
		<-ctx.Done()
		return gateway.NetStats{}, ctx.Err()
	}
	got := r.c.Sample(ctx)
	if got != first || r.c.Snapshot() != first {
		t.Error("a sample that was cut short replaced the last complete one")
	}
	if len(r.c.Series(sbA)) != 0 {
		t.Error("a cut-short sample reached the history")
	}
}

func TestGatewaysAreAskedSeveralAtATimeButNotAllAtOnce(t *testing.T) {
	r := newRig(t, nil)
	var targets engine.StatsTargets
	for i := range 30 {
		owner := fmt.Sprintf("%012x", i)
		targets.Netds = append(targets.Netds, engine.StatsNetd{Owner: owner, PID: 1000 + i, Accounting: engine.AccountingNone, CtlSock: "/s/" + owner})
	}
	r.eng.set(targets)
	var inFlight, peak atomic.Int32
	r.c.netStats = func(ctx context.Context, sock string) (gateway.NetStats, error) {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		inFlight.Add(-1)
		return gateway.NetStats{}, nil
	}
	snap := r.sample()
	if len(snap.Netds) != 30 {
		t.Fatalf("%d gateways in the snapshot", len(snap.Netds))
	}
	if p := peak.Load(); p < 2 || p > maxNetdPolls {
		t.Errorf("%d gateways asked at once, want 2..%d", p, maxNetdPolls)
	}
}

func TestStartSamplesAndStopJoins(t *testing.T) {
	defer goleak.VerifyNone(t)
	captureLogs(t)
	r := busyRig(t, func(c *Config) { c.Interval = 5 * time.Millisecond; c.Now = nil })
	r.c.Stop() // stopping what never started is fine
	r2 := busyRig(t, func(c *Config) { c.Interval = 5 * time.Millisecond; c.Now = nil })
	r2.c.Start()
	r2.c.Start() // twice is once
	deadline := time.Now().Add(5 * time.Second)
	for len(r2.c.Series(sbA)) < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("no samples arrived: %d points", len(r2.c.Series(sbA)))
		}
		time.Sleep(2 * time.Millisecond)
	}
	// Readers and the sampler share state; -race is what checks it.
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				_ = r2.c.Snapshot().Sandboxes
				_ = r2.c.Series(sbA)
				var buf bytes.Buffer
				_ = r2.c.WritePrometheus(&buf, RenderOptions{})
			}
		}()
	}
	wg.Wait()
	r2.c.Stop()
	r2.c.Stop() // and so is stopping twice
	n := len(r2.c.Series(sbA))
	time.Sleep(50 * time.Millisecond)
	if after := len(r2.c.Series(sbA)); after != n {
		t.Errorf("sampling went on after Stop: %d points, then %d", n, after)
	}
	r2.c.Start() // after Stop it stays stopped
	time.Sleep(50 * time.Millisecond)
	if after := len(r2.c.Series(sbA)); after != n {
		t.Errorf("a stopped collector started again: %d points, then %d", n, after)
	}
}

func TestStopDoesNotWaitOnAGatewayThatNeverAnswers(t *testing.T) {
	defer goleak.VerifyNone(t)
	captureLogs(t)
	r := busyRig(t, func(c *Config) { c.Now = nil })
	asked := make(chan struct{})
	var once sync.Once
	r.c.netStats = func(ctx context.Context, sock string) (gateway.NetStats, error) {
		once.Do(func() { close(asked) })
		<-ctx.Done() // a wedged netd: the daemon's context is the only way out
		return gateway.NetStats{}, ctx.Err()
	}
	r.c.Start()
	<-asked
	done := make(chan struct{})
	go func() { r.c.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop is stuck behind a gateway that doesn't answer")
	}
}

func TestASamplePanicDoesNotTakeTheDaemonDown(t *testing.T) {
	defer goleak.VerifyNone(t)
	logs := captureLogs(t)
	r := busyRig(t, func(c *Config) { c.Interval = 5 * time.Millisecond; c.Now = nil })
	var calls atomic.Int32
	r.c.procUsage = func(int) (Usage, error) { return Usage{}, nil }
	src := &panickySource{inner: r.eng, calls: &calls}
	r.c.src = src
	r.c.Start()
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatal("the sampler stopped after a panic")
		}
		time.Sleep(2 * time.Millisecond)
	}
	r.c.Stop()
	if !strings.Contains(logs.String(), "collector.panic") {
		t.Errorf("a panic wasn't logged:\n%s", logs)
	}
}

type panickySource struct {
	inner *fakeEngine
	calls *atomic.Int32
}

func (p *panickySource) StatsTargets() engine.StatsTargets {
	if p.calls.Add(1) == 1 {
		panic("boom")
	}
	return p.inner.StatsTargets()
}

func TestHostCPUUseComesFromTwoReadings(t *testing.T) {
	proc := t.TempDir()
	if err := os.WriteFile(filepath.Join(proc, "meminfo"), fixture(t, "proc/meminfo"), 0o644); err != nil {
		t.Fatal(err)
	}
	stat := func(user, system, idle, iowait int) {
		t.Helper()
		line := fmt.Sprintf("cpu  %d 0 %d %d %d 0 0 0 0 0\ncpu0 %d 0 %d %d %d 0 0 0 0 0\n", user, system, idle, iowait, user, system, idle, iowait)
		if err := os.WriteFile(filepath.Join(proc, "stat"), []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := newRig(t, func(c *Config) { c.ProcRoot = proc })
	stat(1000, 500, 8000, 500)
	if snap := r.sample(); snap.Host.CPUPercent != nil {
		t.Fatalf("host cpu = %v%% from one reading", *snap.Host.CPUPercent)
	}
	// 1000 ticks passed: 400 busy (300 user, 100 system), 600 idle.
	stat(1300, 600, 8600, 500)
	r.clock.advance(10 * time.Second)
	snap := r.sample()
	if p := snap.Host.CPUPercent; p == nil || !near(*p, 40, 1e-9) {
		t.Errorf("host cpu = %v, want 40%%", p)
	}
	// The counters can't go down; if they did the host was rebooted under us,
	// and there is no honest figure for that interval.
	stat(10, 10, 10, 10)
	r.clock.advance(10 * time.Second)
	if snap := r.sample(); snap.Host.CPUPercent != nil {
		t.Errorf("host cpu = %v%% across counters that went down", *snap.Host.CPUPercent)
	}
}

func TestFaultsOfWhatHasGoneAreForgotten(t *testing.T) {
	captureLogs(t)
	r := busyRig(t, nil)
	scope := filepath.Join(r.tree.root, scopePath(ownerA, sbA))
	if err := os.Remove(filepath.Join(scope, "io.stat")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(scope, "io.stat"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.netErr["/run/netd-a/ctl.sock"] = errors.New("connection refused")
	r.sample()
	if n := len(r.c.faults.cur); n != 2 {
		t.Fatalf("%d faults remembered, want the sandbox's and the gateway's", n)
	}
	// The sandbox is destroyed and its owner's gateway with it; their faults
	// can't be remembered for ever.
	r.eng.set(engine.StatsTargets{})
	r.clock.advance(10 * time.Second)
	r.sample()
	if n := len(r.c.faults.cur); n != 0 {
		t.Errorf("%d faults remembered for what no longer exists: %v", n, r.c.faults.cur)
	}
}

// BenchmarkSample is a sample of fifty sandboxes, their owner and gateway, and
// the host: about five hundred small files read.
func BenchmarkSample(b *testing.B) {
	root := b.TempDir()
	copyCgroup := func(path, fixtureName string) {
		src := filepath.Join("testdata", "linux", "cgroup", fixtureName)
		dst := filepath.Join(root, path)
		if err := os.MkdirAll(dst, 0o755); err != nil {
			b.Fatal(err)
		}
		ents, err := os.ReadDir(src)
		if err != nil {
			b.Fatal(err)
		}
		for _, e := range ents {
			data, err := os.ReadFile(filepath.Join(src, e.Name()))
			if err != nil {
				b.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dst, e.Name()), data, 0o644); err != nil {
				b.Fatal(err)
			}
		}
	}
	tg := engine.StatsTargets{}
	for i := range 50 {
		id := fmt.Sprintf("%016x", i+1)
		copyCgroup(scopePath(ownerA, id), "scope-busy")
		tg.Sandboxes = append(tg.Sandboxes, engine.StatsSandbox{ID: id, Owner: ownerA, PID: 100 + i, Accounting: engine.AccountingCgroup, Cgroup: scopePath(ownerA, id)})
	}
	copyCgroup(slicePath(ownerA), "slice-owner")
	copyCgroup(netdPath(ownerA), "scope-netd")
	tg.Owners = []engine.StatsOwner{{Owner: ownerA, Cgroup: slicePath(ownerA)}}
	tg.Netds = []engine.StatsNetd{{Owner: ownerA, PID: 900, Accounting: engine.AccountingCgroup, Cgroup: netdPath(ownerA)}}
	c := New(Config{Source: &fakeEngine{t: tg}, CgroupRoot: root, ProcRoot: filepath.Join("testdata", "linux", "proc"), BlockRoot: b.TempDir()})
	c.readHost = c.host.readProc
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		c.Sample(ctx)
	}
}
