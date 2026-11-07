package krucible

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// statsEngine is an engine on a fake systemd. Its VMs and gateways are made by
// hand, each with a live process of its own for the fake to say where it is.
func statsEngine(sd *fakeSystemd) *Engine {
	return &Engine{vms: map[string]*VM{}, netds: map[string]*netdInstance{}, cg: newTestManager(sd)}
}

// liveProcess starts a process that does nothing, for a pid that is alive.
func liveProcess(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "300")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reap(t, cmd) })
	return cmd.Process.Pid
}

// addTestVM adds a running sandbox of user whose helper the fake has put in the
// scope the engine recorded for it.
func (e *Engine) addTestVM(t *testing.T, sd *fakeSystemd, id, user string, partial bool) (*VM, placementSpec) {
	t.Helper()
	vm := &VM{ID: id, UserID: user, Status: "running", HelperPID: liveProcess(t), netIP: "100.64.0." + id[len(id)-1:]}
	vm.netdKey = ownerKeyFor(user, id)
	ps, err := vmPlacement(vm.cgroupOwner(), id)
	if err != nil {
		t.Fatal(err)
	}
	vm.scope = placedScope{unit: ps.unit, partial: partial}
	sd.place(vm.HelperPID, ps)
	e.vms[id] = vm
	return vm, ps
}

func findSandbox(t *testing.T, ts engine.StatsTargets, id string) engine.StatsSandbox {
	t.Helper()
	for _, s := range ts.Sandboxes {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no sandbox %s in %+v", id, ts.Sandboxes)
	return engine.StatsSandbox{}
}

func TestStatsTargetsReportAPlacedVMWithItsCgroup(t *testing.T) {
	sd := newFakeSystemd()
	e := statsEngine(sd)
	vm, ps := e.addTestVM(t, sd, "0000000000000001", "alice", false)

	ts := e.StatsTargets()
	s := findSandbox(t, ts, vm.ID)
	if s.Owner != ownerSlug("u:alice") || s.UserID != "alice" || s.PID != vm.HelperPID || s.NetIP != "100.64.0.1" {
		t.Errorf("sandbox = %+v", s)
	}
	if s.Accounting != engine.AccountingCgroup || s.Cgroup != ps.path() {
		t.Errorf("accounting %q cgroup %q, want cgroup %q", s.Accounting, s.Cgroup, ps.path())
	}
	// The owner's slice is the scope's parent: what the kernel sums its
	// sandboxes and gateway in.
	if len(ts.Owners) != 1 || ts.Owners[0] != (engine.StatsOwner{Owner: s.Owner, UserID: "alice", Cgroup: "/bhatti.slice/bhatti-" + s.Owner + ".slice"}) {
		t.Errorf("owners = %+v", ts.Owners)
	}
}

func TestStatsTargetsKeepAContainerManagersPrefix(t *testing.T) {
	// Inside a container with its own systemd and the host's cgroup namespace,
	// /proc shows /docker/<id> before every path; the path reported is the one
	// that exists on /sys/fs/cgroup, prefix included.
	sd := newFakeSystemd()
	sd.root = "/docker/0123456789abcdef"
	e := statsEngine(sd)
	_, ps := e.addTestVM(t, sd, "0000000000000001", "alice", false)
	ts := e.StatsTargets()
	s := findSandbox(t, ts, "0000000000000001")
	if want := sd.root + ps.path(); s.Cgroup != want || s.Accounting != engine.AccountingCgroup {
		t.Errorf("cgroup %q (%s), want %q", s.Cgroup, s.Accounting, want)
	}
	if want := sd.root + "/bhatti.slice/bhatti-" + s.Owner + ".slice"; len(ts.Owners) != 1 || ts.Owners[0].Cgroup != want {
		t.Errorf("owners = %+v, want the slice at %s", ts.Owners, want)
	}
}

func TestStatsTargetsFlagAPartiallyAccountedVM(t *testing.T) {
	sd := newFakeSystemd()
	e := statsEngine(sd)
	e.addTestVM(t, sd, "0000000000000001", "alice", true)
	if s := findSandbox(t, e.StatsTargets(), "0000000000000001"); s.Accounting != engine.AccountingPartial || s.Cgroup == "" {
		t.Errorf("a helper placed after it started = %+v", s)
	}
}

// The cgroup of a helper that isn't in the scope the engine recorded must not
// be read as that sandbox's: it is the daemon's own, or another sandbox's.
func TestStatsTargetsNeverNameACgroupTheHelperIsNotIn(t *testing.T) {
	sd := newFakeSystemd()
	e := statsEngine(sd)

	// Recorded as placed, but the helper is in the daemon's own cgroup.
	vm, _ := e.addTestVM(t, sd, "0000000000000001", "alice", false)
	sd.mu.Lock()
	delete(sd.cg, vm.HelperPID)
	sd.mu.Unlock()
	ts := e.StatsTargets()
	if s := findSandbox(t, ts, vm.ID); s.Cgroup != "" || s.Accounting != engine.AccountingNone {
		t.Errorf("a helper in the daemon's cgroup was given %q (%s)", s.Cgroup, s.Accounting)
	}
	if len(ts.Owners) != 0 {
		t.Errorf("an owner listed with nothing measured in its slice: %+v", ts.Owners)
	}

	// Recorded as placed, but the helper is in another sandbox's scope.
	other, err := vmPlacement(ownerSlug("u:alice"), "00000000000000ff")
	if err != nil {
		t.Fatal(err)
	}
	sd.place(vm.HelperPID, other)
	if s := findSandbox(t, e.StatsTargets(), vm.ID); s.Cgroup != "" || s.Accounting != engine.AccountingNone {
		t.Errorf("a helper in another sandbox's scope was given %q (%s)", s.Cgroup, s.Accounting)
	}

	// Never placed (adoption failed): the record has no scope.
	vm.mu.Lock()
	vm.scope = placedScope{}
	vm.mu.Unlock()
	if s := findSandbox(t, e.StatsTargets(), vm.ID); s.Cgroup != "" || s.Accounting != engine.AccountingNone {
		t.Errorf("a helper with no scope was given %q (%s)", s.Cgroup, s.Accounting)
	}
}

func TestStatsTargetsListAStoppedVMWithoutAPid(t *testing.T) {
	sd := newFakeSystemd()
	e := statsEngine(sd)
	vm, _ := e.addTestVM(t, sd, "0000000000000001", "alice", false)
	vm.mu.Lock()
	vm.Status, vm.HelperPID, vm.scope = "stopped", 0, placedScope{}
	vm.mu.Unlock()
	ts := e.StatsTargets()
	s := findSandbox(t, ts, vm.ID)
	if s.PID != 0 || s.Cgroup != "" || s.Accounting != engine.AccountingNone || s.Owner == "" {
		t.Errorf("stopped sandbox = %+v: listed, so its history is kept, but with nothing to read", s)
	}
	if len(ts.Owners) != 0 {
		t.Errorf("owners = %+v", ts.Owners)
	}
}

func TestStatsTargetsListGatewaysAndOwnersTogether(t *testing.T) {
	sd := newFakeSystemd()
	e := statsEngine(sd)
	vm, vmPS := e.addTestVM(t, sd, "0000000000000001", "alice", false)

	// alice's gateway, placed; bob's, running but never placed; carol's, whose
	// lock is held by a start in progress; dave's, which has no process.
	slug := ownerSlug("u:alice")
	netdPS, err := netdPlacement(slug)
	if err != nil {
		t.Fatal(err)
	}
	alice := &netdInstance{owner: "u:alice", ctlSock: "/run/alice/ctl.sock", pid: liveProcess(t), scope: placedScope{unit: netdPS.unit}}
	sd.place(alice.pid, netdPS)
	bob := &netdInstance{owner: "u:bob", ctlSock: "/run/bob/ctl.sock", pid: liveProcess(t)}
	carol := &netdInstance{owner: "u:carol", ctlSock: "/run/carol/ctl.sock", pid: liveProcess(t)}
	dave := &netdInstance{owner: "u:dave", ctlSock: "/run/dave/ctl.sock"}
	e.netds = map[string]*netdInstance{"u:alice": alice, "u:bob": bob, "u:carol": carol, "u:dave": dave}

	carol.mu.Lock() // a gateway being started holds its lock through its placement
	done := make(chan engine.StatsTargets, 1)
	go func() { done <- e.StatsTargets() }()
	var ts engine.StatsTargets
	select {
	case ts = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("StatsTargets waits for a gateway that is being started")
	}
	carol.mu.Unlock()

	got := map[string]engine.StatsNetd{}
	for _, n := range ts.Netds {
		got[n.Owner] = n
	}
	if len(got) != 2 {
		t.Fatalf("gateways = %+v, want alice's and bob's only", ts.Netds)
	}
	a := got[slug]
	if a.PID != alice.pid || a.CtlSock != "/run/alice/ctl.sock" || a.Accounting != engine.AccountingCgroup || a.Cgroup != netdPS.path() {
		t.Errorf("alice's gateway = %+v", a)
	}
	b := got[ownerSlug("u:bob")]
	if b.CtlSock != "/run/bob/ctl.sock" || b.Accounting != engine.AccountingNone || b.Cgroup != "" {
		t.Errorf("bob's gateway = %+v: still asked for its per-guest figures, but with no cgroup", b)
	}
	// alice's slice holds her sandbox and her gateway and is listed once, with her user.
	if len(ts.Owners) != 1 || ts.Owners[0].Owner != slug || ts.Owners[0].UserID != "alice" {
		t.Errorf("owners = %+v", ts.Owners)
	}
	if s := findSandbox(t, ts, vm.ID); s.Cgroup != vmPS.path() {
		t.Errorf("sandbox cgroup = %q", s.Cgroup)
	}
	for i := 1; i < len(ts.Netds); i++ {
		if ts.Netds[i-1].Owner > ts.Netds[i].Owner {
			t.Error("gateways aren't in order")
		}
	}
}

func TestStatsTargetsOfAnOwnerWithOnlyAGatewayHaveItsUserFromItsSandboxes(t *testing.T) {
	// alice's only running thing in a cgroup is her gateway (her sandbox is
	// stopped); the slice is still listed, and her user is still known.
	sd := newFakeSystemd()
	e := statsEngine(sd)
	vm, _ := e.addTestVM(t, sd, "0000000000000001", "alice", false)
	vm.mu.Lock()
	vm.Status, vm.HelperPID, vm.scope = "stopped", 0, placedScope{}
	vm.mu.Unlock()
	slug := ownerSlug("u:alice")
	netdPS, _ := netdPlacement(slug)
	alice := &netdInstance{owner: "u:alice", pid: liveProcess(t), scope: placedScope{unit: netdPS.unit}}
	sd.place(alice.pid, netdPS)
	e.netds["u:alice"] = alice
	ts := e.StatsTargets()
	if len(ts.Owners) != 1 || ts.Owners[0].UserID != "alice" || ts.Owners[0].Cgroup != "/bhatti.slice/bhatti-"+slug+".slice" {
		t.Errorf("owners = %+v", ts.Owners)
	}
}

func TestStatsTargetsWithoutPlacementMeasureNothingByCgroup(t *testing.T) {
	e := &Engine{vms: map[string]*VM{}, netds: map[string]*netdInstance{}} // cg is nil: placement off
	vm := &VM{ID: "0000000000000001", UserID: "alice", Status: "running", HelperPID: 4242, netIP: "100.64.0.2"}
	e.vms[vm.ID] = vm
	e.netds["u:alice"] = &netdInstance{owner: "u:alice", pid: 4243, ctlSock: "/c"}
	ts := e.StatsTargets()
	s := findSandbox(t, ts, vm.ID)
	if s.Accounting != engine.AccountingNone || s.Cgroup != "" || s.PID != 4242 || s.NetIP != "100.64.0.2" {
		t.Errorf("sandbox = %+v", s)
	}
	if len(ts.Netds) != 1 || ts.Netds[0].Accounting != engine.AccountingNone || ts.Netds[0].CtlSock != "/c" {
		t.Errorf("gateways = %+v: its figures for each guest don't need a cgroup", ts.Netds)
	}
	if len(ts.Owners) != 0 {
		t.Errorf("owners = %+v", ts.Owners)
	}
}

func TestStatsTargetsOnAHostWithoutCgroupsAreMeasuredPerProcess(t *testing.T) {
	// macOS: the manager is in process mode. Nothing is placed; each helper is
	// read by pid.
	e := &Engine{vms: map[string]*VM{}, netds: map[string]*netdInstance{}, cg: &cgroupManager{mode: engine.CgroupProcess}}
	e.vms["0000000000000001"] = &VM{ID: "0000000000000001", UserID: "alice", Status: "running", HelperPID: 4242}
	e.vms["0000000000000002"] = &VM{ID: "0000000000000002", UserID: "alice", Status: "stopped"}
	e.netds["u:alice"] = &netdInstance{owner: "u:alice", pid: 4243, ctlSock: "/c"}
	ts := e.StatsTargets()
	if s := findSandbox(t, ts, "0000000000000001"); s.Accounting != engine.AccountingProcess || s.PID != 4242 || s.Cgroup != "" {
		t.Errorf("running = %+v", s)
	}
	if s := findSandbox(t, ts, "0000000000000002"); s.PID != 0 {
		t.Errorf("stopped = %+v", s)
	}
	if len(ts.Netds) != 1 || ts.Netds[0].Accounting != engine.AccountingProcess || ts.Netds[0].PID != 4243 {
		t.Errorf("gateways = %+v", ts.Netds)
	}
	if len(ts.Owners) != 0 {
		t.Errorf("owners from a host without slices: %+v", ts.Owners)
	}
}

func TestStatsTargetsAreSortedAndOwnerlessSandboxesAreTheirOwnOwners(t *testing.T) {
	sd := newFakeSystemd()
	e := statsEngine(sd)
	// A sandbox with no user is its own owner, with a slice of its own.
	e.addTestVM(t, sd, "0000000000000009", "", false)
	e.addTestVM(t, sd, "0000000000000003", "", false)
	ts := e.StatsTargets()
	if len(ts.Sandboxes) != 2 || ts.Sandboxes[0].ID != "0000000000000003" || ts.Sandboxes[1].ID != "0000000000000009" {
		t.Fatalf("sandboxes = %+v", ts.Sandboxes)
	}
	if ts.Sandboxes[0].Owner == ts.Sandboxes[1].Owner || ts.Sandboxes[0].UserID != "" {
		t.Errorf("two ownerless sandboxes share an owner: %+v", ts.Sandboxes)
	}
	if len(ts.Owners) != 2 || ts.Owners[0].Owner > ts.Owners[1].Owner {
		t.Errorf("owners = %+v", ts.Owners)
	}
}

// The listing is read while the engine runs: VMs start and stop, gateways come
// and go. -race is what checks this.
func TestStatsTargetsWhileTheEngineChanges(t *testing.T) {
	sd := newFakeSystemd()
	e := statsEngine(sd)
	vms := make([]*VM, 8)
	for i := range vms {
		vms[i], _ = e.addTestVM(t, sd, fmt.Sprintf("%016x", i+1), "alice", false)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			vm := vms[i%len(vms)]
			vm.mu.Lock()
			if vm.Status == "running" {
				vm.Status, vm.HelperPID = "stopped", 0
			} else {
				vm.Status, vm.HelperPID = "running", os.Getpid()
			}
			vm.mu.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			key := fmt.Sprintf("u:%d", i%4)
			e.netdMu.Lock()
			if e.netds[key] == nil {
				e.netds[key] = &netdInstance{owner: key, pid: os.Getpid()}
			} else {
				delete(e.netds, key)
			}
			e.netdMu.Unlock()
		}
	}()
	for range 300 {
		for _, s := range e.StatsTargets().Sandboxes {
			if s.PID == 0 && s.Cgroup != "" {
				t.Fatalf("a stopped sandbox with a cgroup: %+v", s)
			}
		}
	}
	close(stop)
	wg.Wait()
}
