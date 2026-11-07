package krucible

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// placementEngine is an engine in systemd mode on a fake systemd whose helpers
// are stand-in processes (standin_test.go): the real launch, recovery and
// netd paths, with placement as the thing under test and no hypervisor.
func placementEngine(t *testing.T, sd *fakeSystemd) *Engine {
	t.Helper()
	t.Setenv(standinEnv, "vmm")
	dataDir := t.TempDir()
	base := filepath.Join(dataDir, "base.ext4")
	if err := os.WriteFile(base, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	e := &Engine{
		cfg:  Config{DataDir: dataDir, SocketDir: shortSockDir(t), BaseImage: base, VMMBinary: os.Args[0], BlockRoot: true, DefaultVcpus: 1, DefaultMemMiB: 64},
		caps: VMMCapabilities{Checkpoint: true},
		vms:  map[string]*VM{},
		cg:   newTestManager(sd),
	}
	t.Cleanup(func() {
		for id := range e.vms {
			_ = e.Destroy(context.Background(), id)
		}
		e.cg.close()
	})
	return e
}

// unitSandbox is the sandbox id a scope name ends with.
func unitSandbox(unit string) string {
	unit = strings.TrimSuffix(unit, ".scope")
	return unit[strings.LastIndex(unit, "-")+1:]
}

func ownedSpec(name string) engine.SandboxSpec {
	spec := noNetworkSpec(name)
	spec.UserID = "alice"
	return spec
}

// TestCreatePlacesTheVMMBeforeItWorks: through the real launch, the VM's helper
// is in its scope, and the scope is on its record, before the helper does a
// thing; Destroy lets the scope go.
func TestCreatePlacesTheVMMBeforeItWorks(t *testing.T) {
	sd := newFakeSystemd()
	sd.moveAfter = 20 * time.Millisecond
	e := placementEngine(t, sd)
	sd.onStart = func(name string, _ map[string]any) {
		if strings.HasSuffix(name, ".scope") {
			time.Sleep(200 * time.Millisecond) // a helper that ignored its gate would have started by now
			if exists(filepath.Join(e.cfg.DataDir, "sandboxes", unitSandbox(name), standinStarted)) {
				t.Error("the helper began work while it was being placed")
			}
		}
	}
	info, err := e.Create(context.Background(), ownedSpec("a"))
	if err != nil {
		t.Fatal(err)
	}
	vm := e.vms[info.ID]
	owner := ownerSlug("u:alice")
	unit := "bhatti-" + owner + "-" + info.ID + ".scope"
	if vm.scope != (placedScope{unit: unit}) {
		t.Fatalf("vm scope = %+v, want %s placed whole", vm.scope, unit)
	}
	rec := readRecord(t, vm.SandboxDir)
	if rec.CgroupUnit != unit || rec.CgroupPartial || rec.HelperPID != vm.HelperPID || rec.HelperPID == 0 {
		t.Fatalf("state.json: unit %q partial %v pid %d (vm pid %d)", rec.CgroupUnit, rec.CgroupPartial, rec.HelperPID, vm.HelperPID)
	}
	if cg, _ := sd.procCg(vm.HelperPID); !strings.HasSuffix(cg, "/bhatti.slice/bhatti-"+owner+".slice/"+unit) {
		t.Fatalf("helper is in %q", cg)
	}
	if got := e.Accounting(info.ID); got != "cgroup" {
		t.Fatalf("accounting = %q", got)
	}
	if got, want := sd.calls("start"), []string{"bhatti.slice", "bhatti-" + owner + ".slice", unit}; !slices.Equal(got, want) {
		t.Fatalf("systemd was asked for %v, want %v", got, want)
	}
	if !exists(filepath.Join(vm.SandboxDir, standinStarted)) {
		t.Fatal("the helper never began work")
	}
	if st := e.CgroupStatus(); st.Mode != engine.CgroupSystemd {
		t.Fatalf("status %+v", st)
	}

	if err := e.Destroy(context.Background(), info.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the destroyed sandbox's scope to be released", func() bool { _, ok := sd.unit(unit); return !ok })
	if !slices.Contains(sd.calls("usage"), unit) || !slices.Contains(sd.calls("unref"), unit) {
		t.Fatalf("scope not read and released: %v", sd.log)
	}
}

// TestCreateFailsClosedWhenPlacementFails: systemd refusing the scope fails the
// create with its reason, and nothing is left running, recorded or registered.
func TestCreateFailsClosedWhenPlacementFails(t *testing.T) {
	sd := newFakeSystemd()
	var pids []uint32
	var mu sync.Mutex
	sd.onStart = func(name string, props map[string]any) {
		if strings.HasSuffix(name, ".scope") {
			mu.Lock()
			pids = append(pids, props["PIDs"].([]uint32)...)
			mu.Unlock()
		}
	}
	sd.startErr = func(name string) error {
		if strings.HasSuffix(name, ".scope") {
			return errors.New("org.freedesktop.DBus.Error.AccessDenied: Access denied")
		}
		return nil
	}
	e := placementEngine(t, sd)
	_, err := e.Create(context.Background(), ownedSpec("a"))
	if err == nil || !strings.Contains(err.Error(), "Access denied") || !strings.Contains(err.Error(), "start vmm helper") {
		t.Fatalf("create = %v, want systemd's reason", err)
	}
	if len(e.vms) != 0 {
		t.Fatalf("%d sandboxes registered after a failed create", len(e.vms))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(pids) != 1 {
		t.Fatalf("systemd was asked to place %v", pids)
	}
	waitFor(t, "the unplaced helper to be gone", func() bool { return !pidAlive(int(pids[0])) })
	if entries, _ := os.ReadDir(filepath.Join(e.cfg.DataDir, "sandboxes")); len(entries) != 0 {
		t.Fatalf("sandbox directories left behind: %v", entries)
	}
}

// TestCreateFailsClosedWhenTheMoveNeverComes: the helper never shows in its
// scope; the create fails and the scope is stopped, the helper killed.
func TestCreateFailsClosedWhenTheMoveNeverComes(t *testing.T) {
	sd := newFakeSystemd()
	sd.moveAfter = -1
	e := placementEngine(t, sd)
	e.cg.timeout = 300 * time.Millisecond
	_, err := e.Create(context.Background(), ownedSpec("a"))
	if err == nil || !strings.Contains(err.Error(), "not in its scope") {
		t.Fatalf("create = %v", err)
	}
	if got := sd.calls("stop"); len(got) != 1 || !strings.HasSuffix(got[0], ".scope") {
		t.Fatalf("stopped %v, want the scope that was made", got)
	}
	if len(e.vms) != 0 {
		t.Fatalf("%d sandboxes registered", len(e.vms))
	}
}

// TestStartAfterStopReusesTheScopeName: a sandbox stopped and started again gets
// a scope of the same name, with the last run's unit possibly still loaded.
func TestStartAfterStopReusesTheScopeName(t *testing.T) {
	sd := newFakeSystemd()
	sd.moveAfter = 5 * time.Millisecond
	e := placementEngine(t, sd)
	ctx := context.Background()
	info, err := e.Create(ctx, ownedSpec("a"))
	if err != nil {
		t.Fatal(err)
	}
	unit := e.vms[info.ID].scope.unit
	if err := e.Stop(ctx, info.ID); err != nil {
		var unclean *UncleanStopError
		if !errors.As(err, &unclean) {
			t.Fatal(err)
		}
	}
	if e.vms[info.ID].scope.unit != "" {
		t.Fatal("a stopped sandbox still names a scope")
	}
	if err := e.Start(ctx, info.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	vm := e.vms[info.ID]
	if vm.scope.unit != unit || vm.Status != "running" {
		t.Fatalf("restarted in scope %q (%s), want %q", vm.scope.unit, vm.Status, unit)
	}
	if n := slices.Index(sd.calls("start"), unit); n < 0 {
		t.Fatalf("scope never started: %v", sd.calls("start"))
	}
	if rec := readRecord(t, vm.SandboxDir); rec.CgroupUnit != unit {
		t.Fatalf("state.json names scope %q", rec.CgroupUnit)
	}
}

// recoverFixture is an engine with no sandboxes yet and a manager on sd.
func recoverFixture(t *testing.T, sd *fakeSystemd) *Engine {
	t.Helper()
	return &Engine{
		cfg: Config{DataDir: t.TempDir(), SocketDir: shortSockDir(t)}, vms: map[string]*VM{}, netds: map[string]*netdInstance{},
		cg: newTestManager(sd),
	}
}

// recordRunning writes the state.json of a running sandbox whose helper is pid.
func recordRunning(t *testing.T, e *Engine, id string, pid int, scope placedScope) placementSpec {
	t.Helper()
	recordSandbox(e, id, "running", pid)
	rec := readRecord(t, filepath.Join(e.cfg.DataDir, "sandboxes", id))
	rec.CgroupUnit, rec.CgroupPartial = scope.unit, scope.partial
	writeRecord(rec)
	ps, err := vmPlacement((&VM{ID: id}).cgroupOwner(), id)
	if err != nil {
		t.Fatal(err)
	}
	ps.alive = pidAlive
	return ps
}

// TestRecoverAdoptsAHelperInItsScope: a restart finds the helper in the scope its
// record names, checks it, and adopts it whole.
func TestRecoverAdoptsAHelperInItsScope(t *testing.T) {
	sd := newFakeSystemd()
	e := recoverFixture(t, sd)
	spec, ctl := sandboxPaths(t, e, testID)
	pid := gatedStandin(t, spec)
	fakeControl(t, ctl, "running")
	ps := recordRunning(t, e, testID, pid, placedScope{unit: "bhatti-" + ownerSlug("s:"+testID) + "-" + testID + ".scope"})
	sd.place(pid, ps)

	e.recover()
	t.Cleanup(e.Shutdown) // runs before the stand-in is killed: no watcher persists into a dir being removed
	vm := e.vms[testID]
	if vm.Status != "running" || vm.scope != (placedScope{unit: ps.unit}) {
		t.Fatalf("recovered as %s in scope %+v", vm.Status, vm.scope)
	}
	if len(sd.calls("start")) != 0 {
		t.Fatalf("a placed helper was placed again: %v", sd.calls("start"))
	}
	if got := e.Accounting(testID); got != "cgroup" {
		t.Fatalf("accounting %q", got)
	}
	if rec := readRecord(t, vm.SandboxDir); rec.CgroupUnit != ps.unit || rec.CgroupPartial {
		t.Fatalf("persisted unit %q partial %v", rec.CgroupUnit, rec.CgroupPartial)
	}
}

// TestRecoverPlacesAHelperAPreviousDaemonNeverPlaced: the upgrade case. A VMM a
// pre-placement daemon started is moved into its scope at adoption and flagged
// partial, and the flag survives the next restart.
func TestRecoverPlacesAHelperAPreviousDaemonNeverPlaced(t *testing.T) {
	sd := newFakeSystemd()
	sd.moveAfter = 5 * time.Millisecond
	e := recoverFixture(t, sd)
	spec, ctl := sandboxPaths(t, e, testID)
	pid := fakeHelper(t, spec) // started as a pre-placement daemon did: [bhatti-vmm spec], in the daemon's cgroup
	fakeControl(t, ctl, "running")
	ps := recordRunning(t, e, testID, pid, placedScope{})

	e.recover()
	t.Cleanup(e.Shutdown)
	vm := e.vms[testID]
	if vm.Status != "running" || vm.scope != (placedScope{unit: ps.unit, partial: true}) {
		t.Fatalf("recovered as %s in scope %+v, want it placed and partial", vm.Status, vm.scope)
	}
	if cg, _ := sd.procCg(pid); !memberOf(cg, ps) {
		t.Fatalf("helper is in %q", cg)
	}
	if got := e.Accounting(testID); got != "partial" {
		t.Fatalf("accounting %q", got)
	}
	if rec := readRecord(t, vm.SandboxDir); rec.CgroupUnit != ps.unit || !rec.CgroupPartial {
		t.Fatalf("persisted unit %q partial %v", rec.CgroupUnit, rec.CgroupPartial)
	}

	// The next daemon finds it in its scope and still calls it partial.
	e2 := &Engine{cfg: e.cfg, vms: map[string]*VM{}, netds: map[string]*netdInstance{}, cg: newTestManager(sd)}
	e2.recover()
	t.Cleanup(e2.Shutdown)
	if got := e2.vms[testID].scope; got != (placedScope{unit: ps.unit, partial: true}) {
		t.Fatalf("after another restart: %+v", got)
	}
	// A fresh start clears it: the new helper is placed from its first instruction.
	e2.vms[testID].kill()
	if e2.vms[testID].scope != (placedScope{}) {
		t.Fatal("a killed helper's scope was kept")
	}
}

// TestRecoverKeepsAGuestItCannotPlace: a restart doesn't reboot a guest because
// it can't be measured. It runs on, unaccounted, and the log says so.
func TestRecoverKeepsAGuestItCannotPlace(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	sd := newFakeSystemd()
	sd.startErr = func(name string) error {
		if strings.HasSuffix(name, ".scope") {
			return errors.New("the bus went away")
		}
		return nil
	}
	e := recoverFixture(t, sd)
	spec, ctl := sandboxPaths(t, e, testID)
	pid := fakeHelper(t, spec)
	fakeControl(t, ctl, "running")
	recordRunning(t, e, testID, pid, placedScope{})

	e.recover()
	t.Cleanup(e.Shutdown)
	vm := e.vms[testID]
	if vm.Status != "running" || !pidAlive(pid) {
		t.Fatalf("status %s, helper alive %v: the guest should have been left running", vm.Status, pidAlive(pid))
	}
	if vm.scope != (placedScope{}) || e.Accounting(testID) != "none" {
		t.Fatalf("scope %+v, accounting %q", vm.scope, e.Accounting(testID))
	}
	if !strings.Contains(logs.String(), "krucible.cgroup.adopt_failed") || !strings.Contains(logs.String(), "the bus went away") {
		t.Fatalf("no record of the failure in the log:\n%s", logs.String())
	}
}

// TestRecoverReportsScopesNobodyClaims: a bhatti scope no record names is
// reported and left alone: what runs in it may be a helper whose record is gone.
func TestRecoverReportsScopesNobodyClaims(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	sd := newFakeSystemd()
	sd.units["bhatti-0123456789ab-feedfacefeedface.scope"] = &fakeUnit{props: map[string]any{"Description": "bhatti sandbox=feedfacefeedface"}, active: "active"}
	e := recoverFixture(t, sd)
	e.recover()
	if !strings.Contains(logs.String(), "krucible.cgroup.orphan_scope") || !strings.Contains(logs.String(), "feedfacefeedface") {
		t.Fatalf("orphan scope not reported:\n%s", logs.String())
	}
	if len(sd.calls("stop")) != 0 {
		t.Fatalf("stopped %v", sd.calls("stop"))
	}
}

// gatewayState is what a gateway instance holds about its scope, read under its
// lock: an adopted gateway's death watcher updates it from its own goroutine.
func gatewayState(inst *netdInstance) (scope placedScope, settledPID, pid int) {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.scope, inst.scopePID, inst.pid
}

// TestAdoptedGatewayIsPlacedOrChecked: an adopted bhatti-netd goes through the
// same check as an adopted VMM, once.
func TestAdoptedGatewayIsPlacedOrChecked(t *testing.T) {
	t.Run("never placed", func(t *testing.T) {
		sd := newFakeSystemd()
		sd.moveAfter = 5 * time.Millisecond
		e, inst, _ := netdFixture(t, false)
		e.cg = newTestManager(sd)
		t.Cleanup(e.cg.close)
		if err := e.ensureNetd(inst.owner); err != nil {
			t.Fatal(err)
		}
		ps, _ := netdPlacement(ownerSlug(inst.owner))
		if scope, settled, pid := gatewayState(inst); scope != (placedScope{unit: ps.unit, partial: true}) || settled != pid {
			t.Fatalf("gateway scope %+v (pid %d, settled %d), want it placed and partial", scope, pid, settled)
		}
		var rec netdRecord
		data, err := os.ReadFile(netdStatePath(inst.dir))
		if _, _, pid := gatewayState(inst); err != nil || json.Unmarshal(data, &rec) != nil || rec.Scope != ps.unit || !rec.ScopePartial || rec.Pid != pid {
			t.Fatalf("netd.json %s (%v)", data, err)
		}
		if err := e.ensureNetd(inst.owner); err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(strings.Join(sd.calls("start"), " ")+" ", ps.unit+" "); n != 1 {
			t.Fatalf("an adopted gateway was placed %d times", n)
		}
		if u, _ := sd.unit(ps.unit); u.props["CPUWeight"] != uint64(cpuWeightNetd) {
			t.Fatalf("gateway scope properties %v", u.props)
		}
	})
	t.Run("already in its scope", func(t *testing.T) {
		sd := newFakeSystemd()
		e, inst, _ := netdFixture(t, false)
		e.cg = newTestManager(sd)
		t.Cleanup(e.cg.close)
		ps, _ := netdPlacement(ownerSlug(inst.owner))
		sd.place(inst.pid, ps)
		inst.mu.Lock()
		inst.scope = placedScope{unit: ps.unit} // from netd.json
		inst.mu.Unlock()
		if err := e.ensureNetd(inst.owner); err != nil {
			t.Fatal(err)
		}
		if scope, _, _ := gatewayState(inst); scope != (placedScope{unit: ps.unit}) || len(sd.calls("start")) != 0 {
			t.Fatalf("scope %+v, systemd asked for %v", scope, sd.calls("start"))
		}
	})
}

// buildNetd builds the real bhatti-netd for this host.
func buildNetd(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain to build bhatti-netd")
	}
	bin := filepath.Join(t.TempDir(), "bhatti-netd")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/bhatti-netd")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build bhatti-netd: %v\n%s", err, out)
	}
	return bin
}

// TestSpawnedGatewayIsPlacedBeforeItListens: the real bhatti-netd, started
// through ensureNetd, binds nothing until it is in its scope and recorded there.
func TestSpawnedGatewayIsPlacedBeforeItListens(t *testing.T) {
	sd := newFakeSystemd()
	sd.moveAfter = 20 * time.Millisecond
	e := &Engine{cfg: Config{SocketDir: shortSockDir(t), NetdBinary: buildNetd(t)}, netds: map[string]*netdInstance{}, vms: map[string]*VM{}, cg: newTestManager(sd)}
	t.Cleanup(e.cg.close)
	const key = "u:owner"
	inst, slot, err := e.acquireNetd(key, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.releaseNetd(key, slot) })
	ps, _ := netdPlacement(ownerSlug(key))
	sd.onStart = func(name string, _ map[string]any) {
		if name == ps.unit {
			time.Sleep(300 * time.Millisecond) // netd would have bound its socket long ago, if it ignored its gate
			if exists(inst.sock) || exists(inst.ctlSock) {
				t.Error("netd was listening before it was placed")
			}
			var rec netdRecord
			if data, err := os.ReadFile(netdStatePath(inst.dir)); err == nil && json.Unmarshal(data, &rec) == nil && rec.Scope != "" {
				t.Error("the scope was recorded before it existed")
			}
		}
	}
	if err := e.ensureNetd(key); err != nil {
		t.Fatal(err)
	}
	scope, settled, pid := gatewayState(inst)
	if scope != (placedScope{unit: ps.unit}) || settled != pid || pid == 0 {
		t.Fatalf("gateway scope %+v, pid %d (settled %d)", scope, pid, settled)
	}
	var rec netdRecord
	data, err := os.ReadFile(netdStatePath(inst.dir))
	if err != nil || json.Unmarshal(data, &rec) != nil || rec.Scope != ps.unit || rec.ScopePartial || rec.Pid != pid {
		t.Fatalf("netd.json %s (%v)", data, err)
	}
	if cg, _ := sd.procCg(pid); !memberOf(cg, ps) {
		t.Fatalf("gateway is in %q", cg)
	}
	if u, _ := sd.unit(ps.unit); u.props["CPUWeight"] != uint64(cpuWeightNetd) || u.props["Slice"] != ps.slice() {
		t.Fatalf("gateway scope properties %v", u.props)
	}
	// The gate travels in the environment: argv is how every daemon release
	// recognises its gateway, and must stay what it was before placement.
	if args, err := procArgs(pid); err == nil && strings.Contains(strings.Join(args, " "), "gate") {
		t.Fatalf("netd argv %q carries the launch gate", args)
	}
}

// TestSpawnedGatewayFailsClosedWhenPlacementFails: no placement, no gateway: the
// error says why, and netd was never let go.
func TestSpawnedGatewayFailsClosedWhenPlacementFails(t *testing.T) {
	sd := newFakeSystemd()
	sd.startErr = func(name string) error {
		if strings.HasSuffix(name, ".scope") {
			return errors.New("the bus went away")
		}
		return nil
	}
	e := &Engine{cfg: Config{SocketDir: shortSockDir(t), NetdBinary: buildNetd(t)}, netds: map[string]*netdInstance{}, vms: map[string]*VM{}, cg: newTestManager(sd)}
	t.Cleanup(e.cg.close)
	const key = "u:owner"
	inst, slot, err := e.acquireNetd(key, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.releaseNetd(key, slot) })
	err = e.ensureNetd(key)
	if err == nil || !strings.Contains(err.Error(), "the bus went away") || !strings.Contains(err.Error(), "start bhatti-netd") {
		t.Fatalf("ensureNetd = %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if scope, _, pid := gatewayState(inst); exists(inst.sock) || pid != 0 || scope != (placedScope{}) {
		t.Fatalf("a gateway came up unplaced: socket %v, pid %d, scope %+v", exists(inst.sock), pid, scope)
	}
	var rec netdRecord
	if data, err := os.ReadFile(netdStatePath(inst.dir)); err == nil && json.Unmarshal(data, &rec) == nil && (rec.Pid != 0 || rec.Scope != "") {
		t.Fatalf("netd.json still names a gateway: %s", data)
	}
}
