package krucible

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/launchgate"
)

// Cgroup placement.
//
// In "systemd" mode every helper the engine starts — each sandbox's bhatti-vmm
// and each owner's bhatti-netd — lives in a transient systemd scope of its own,
// so the kernel accounts its CPU, memory and I/O to the sandbox that causes
// them and not to the daemon:
//
//	bhatti.slice
//	└── bhatti-<owner>.slice                  one per owner
//	    ├── bhatti-<owner>-<engineID>.scope   a sandbox's VM
//	    └── bhatti-<owner>-netd.scope         the owner's gateway
//
// <owner> is the 12 hex digits netd's directory is already named by (ownerSlug)
// and <engineID> is the VM's 16-hex id. Hex needs no systemd escaping and can't
// collide with "netd"; systemd derives each slice's parent from the dashes in
// its name, which is why a slice is never given a Slice=. The owner today is
// the sandbox's user; a principal can take its place without changing the shape.
//
// Nothing is limited: no memory, I/O or CPU ceiling, and no quota. Accounting is
// on for everything, and the placement itself is what fixes the daemon's CPU
// share. Until now every helper was a child of bhatti.service, so each vCPU
// thread of every guest (~80 on a busy host) competed with the daemon's own
// threads inside one cgroup, one thread, one share, and a loaded host starved
// the API. The top slice is a sibling of system.slice, where bhatti.service
// lives, so at the root the daemon's cgroup competes with all of the guests'
// by weight, equal to theirs, however many threads they run.
//
// That only holds if the kernel's cpu controller is on at the root, and
// accounting flags don't turn it on (on cgroup v2 cpu.stat exists without the
// controller): systemd enables it for a unit's parent chain when the unit sets a
// CPU weight. So bhatti.slice carries the kernel's default weight, 100, set
// explicitly for that effect and no other (checked on systemd 257, cgroup v2).
//
// The one weight that isn't the default is the gateway's. Every packet a guest
// sends or receives goes through its owner's netd, one userspace process, so a
// netd starved by its own owner's busy guests stalls all of them. Inside an
// owner's slice the netd scope outweighs each VM scope tenfold; weights only
// count under contention, so an idle gateway takes nothing.
const (
	cpuWeightTop  = 100  // cpu.weight of bhatti.slice: the default, set so the cpu controller is enabled
	cpuWeightNetd = 1000 // cpu.weight of an owner's netd scope; its VM scopes keep the default 100
)

const (
	cgroupPrefix = "bhatti"
	topSlice     = cgroupPrefix + ".slice"

	// placementTimeout bounds one placement from the D-Bus call to seeing the
	// pid in its scope. The spike measured 2.5 ms (p50) alone and under 120 ms
	// with 50 placements at once.
	placementTimeout = 5 * time.Second
	// maxPlacements bounds the placements in flight; a launch beyond them waits
	// for a slot before it starts its helper.
	maxPlacements = 16
	// finalWait is how long after a helper's exit its scope is given to settle
	// before the reference on it is dropped regardless.
	finalWait = 3 * time.Second
)

// ownerSlug is how an owner appears in unit names and in netd's directory: 12
// hex digits of the sha256 of its key.
func ownerSlug(ownerKey string) string {
	h := sha256.Sum256([]byte(ownerKey))
	return hex.EncodeToString(h[:6])
}

// unitPart rejects anything but lower-case hex, the only characters a unit name
// is built from: no escaping, no '-' (which would nest) and no '.' (which would
// end the name early).
func unitPart(what, s string) error {
	if s == "" || len(s) > 64 {
		return fmt.Errorf("cgroup %s %q: want 1-64 hex digits", what, s)
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("cgroup %s %q: want lower-case hex digits only", what, s)
		}
	}
	return nil
}

// placementSpec says where a helper belongs and how it is to be described.
type placementSpec struct {
	owner  string // ownerSlug of the owner
	unit   string // the scope's name
	desc   string
	weight uint64 // CPUWeight; 0 leaves systemd's default
	// alive reports whether pid is still the helper this spec places: a pid
	// alone can be reused once its process is gone. nil means any live process.
	alive func(pid int) bool
	log   []any // slog attributes naming what is placed
}

func (ps placementSpec) slice() string { return cgroupPrefix + "-" + ps.owner + ".slice" }

// path is where the scope's cgroup sits below the manager's own root, which is
// "/" on a host and deeper in a container with its own systemd.
func (ps placementSpec) path() string { return "/" + topSlice + "/" + ps.slice() + "/" + ps.unit }

// vmPlacement places a sandbox's VM helper in <owner>/<id>'s scope.
func vmPlacement(owner, id string) (placementSpec, error) {
	if err := unitPart("owner", owner); err != nil {
		return placementSpec{}, err
	}
	if err := unitPart("sandbox id", id); err != nil {
		return placementSpec{}, err
	}
	return placementSpec{
		owner: owner,
		unit:  cgroupPrefix + "-" + owner + "-" + id + ".scope",
		desc:  "bhatti sandbox=" + id + " owner=" + owner,
		log:   []any{"sandbox", id, "owner", owner},
	}, nil
}

// netdPlacement places an owner's gateway in its own scope, outweighing the
// owner's VMs.
func netdPlacement(owner string) (placementSpec, error) {
	if err := unitPart("owner", owner); err != nil {
		return placementSpec{}, err
	}
	return placementSpec{
		owner:  owner,
		unit:   cgroupPrefix + "-" + owner + "-netd.scope",
		desc:   "bhatti netd owner=" + owner,
		weight: cpuWeightNetd,
		log:    []any{"netd_owner", owner},
	}, nil
}

// unitProp is one property of a transient unit. Value is a bool, string,
// uint64 or []uint32, the only types systemd is given.
type unitProp struct {
	Name  string
	Value any
}

func sliceProps(desc string, weight uint64) []unitProp {
	props := []unitProp{
		{"Description", desc},
		{"CPUAccounting", true}, {"MemoryAccounting", true}, {"IOAccounting", true},
	}
	if weight != 0 {
		props = append(props, unitProp{"CPUWeight", weight})
	}
	return props
}

// scopeProps is a scope holding pid: accounting on, nothing limited.
//
// CollectMode lets systemd forget the scope even when it ends failed (an OOM
// kill does that), which would otherwise leave the name taken. AddRef keeps the
// unit loaded after its last process exits, so the numbers systemd saved for it
// can still be read (finish); it does not keep the cgroup, whose counters are
// gone with the last process.
func scopeProps(ps placementSpec, pid int) []unitProp {
	props := []unitProp{
		{"Description", ps.desc},
		{"Slice", ps.slice()},
		{"PIDs", []uint32{uint32(pid)}},
		{"CPUAccounting", true}, {"MemoryAccounting", true}, {"IOAccounting", true},
		{"CollectMode", "inactive-or-failed"},
		{"AddRef", true},
	}
	if ps.weight != 0 {
		props = append(props, unitProp{"CPUWeight", ps.weight})
	}
	return props
}

// unitStatus is what systemd reports about a loaded unit.
type unitStatus struct {
	Name, Description string
	Active            string // "active", "activating", "deactivating", "inactive" or "failed"
}

func (s unitStatus) running() bool { return s.Active == "active" || s.Active == "activating" }

// unitUsage is what systemd saved of a scope's use over its life; -1 where it
// has no figure.
type unitUsage struct {
	CPUNanos, MemoryPeak, IORead, IOWrite int64
	Result                                string // "success", "oom-kill", "signal", ...
}

var (
	errNoSuchUnit = errors.New("no such unit")
	errUnitExists = errors.New("unit already exists")
)

// systemdAPI is the part of systemd's manager interface placement uses. The
// real one speaks D-Bus (cgroup_linux.go); tests supply a fake.
type systemdAPI interface {
	// StartTransientUnit creates and starts a unit. It returns once systemd has
	// accepted the job, which is not when the job has run. errUnitExists if the
	// name is taken.
	StartTransientUnit(ctx context.Context, name string, props []unitProp) error
	// StopUnit stops a unit, killing what runs in it. errNoSuchUnit if it isn't loaded.
	StopUnit(ctx context.Context, name string) error
	// Units lists the loaded units whose names match the glob.
	Units(ctx context.Context, pattern string) ([]unitStatus, error)
	RefUnit(ctx context.Context, name string) error
	UnrefUnit(ctx context.Context, name string) error
	// Usage reads what systemd saved of a unit. errNoSuchUnit if it isn't loaded.
	Usage(ctx context.Context, name string) (unitUsage, error)
	Close() error
}

// cgroupHost is what the mode decision looks at, so the decision can be tested
// without a host that is, or isn't, running systemd.
type cgroupHost struct {
	goos    string
	pid1    func() (string, error)                        // PID 1's executable name
	cgroup2 func() (bool, error)                          // whether cgroup v2 is mounted
	dial    func(ctx context.Context) (systemdAPI, error) // a connection to systemd's manager
	procCg  func(pid int) (string, error)                 // the cgroup path of a pid
}

// cgroupManager places helpers. A nil manager, or one not in systemd mode, places
// nothing and leaves helpers exactly as they were started.
type cgroupManager struct {
	mode      string // engine.CgroupSystemd, CgroupNone or CgroupProcess
	requested string // the configured value
	reason    string // why an automatic choice isn't systemd

	sd      systemdAPI
	procCg  func(pid int) (string, error)
	slots   chan struct{}
	timeout time.Duration

	mu     sync.Mutex
	slices map[string]bool // slices ensured by this daemon
	held   map[string]int  // scopes this daemon holds a reference on → the pid they were taken for
	closed bool
	stop   chan struct{}
	wg     sync.WaitGroup
}

func (m *cgroupManager) placing() bool { return m != nil && m.mode == engine.CgroupSystemd }

func (m *cgroupManager) status() engine.CgroupStatus {
	if m == nil {
		return engine.CgroupStatus{Mode: engine.CgroupNone}
	}
	return engine.CgroupStatus{Mode: m.mode, Requested: m.requested, Reason: m.reason}
}

// resolveCgroups settles how helpers are placed. systemd is required where it
// is asked for by name and used where it works when left to "auto": PID 1 is
// systemd, cgroup v2 is mounted, and the system bus answers a request to create
// bhatti's top slice, which also proves bhatti may start transient units at all.
func resolveCgroups(requested string, host cgroupHost) (*cgroupManager, error) {
	req := requested
	if req == "" {
		req = "auto"
	}
	m := &cgroupManager{requested: req, procCg: host.procCg}
	switch req {
	case "none":
		m.mode = engine.CgroupNone
		return m, nil
	case "auto", "systemd":
	default:
		return nil, fmt.Errorf("krucible: cgroups must be auto, systemd or none (got %q)", requested)
	}
	if host.goos != "linux" {
		if req == "systemd" {
			return nil, fmt.Errorf("krucible: cgroups: systemd needs Linux (this is %s); use auto or none", host.goos)
		}
		m.mode = engine.CgroupProcess
		m.reason = host.goos + " has no cgroups: sandboxes can be observed per process, not placed"
		return m, nil
	}
	fallback := func(err error) (*cgroupManager, error) {
		if req == "systemd" {
			return nil, fmt.Errorf("krucible: cgroups: systemd is required but unavailable: %w", err)
		}
		m.mode, m.reason = engine.CgroupNone, err.Error()
		return m, nil
	}
	sd, err := host.connect()
	if err != nil {
		return fallback(err)
	}
	m.mode = engine.CgroupSystemd
	m.sd = sd
	m.timeout = placementTimeout
	m.slots = make(chan struct{}, maxPlacements)
	m.slices = map[string]bool{}
	m.held = map[string]int{}
	m.stop = make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), m.timeout)
	defer cancel()
	if err := m.ensureSlices(ctx, ""); err != nil {
		sd.Close()
		m.sd, m.mode = nil, ""
		return fallback(err)
	}
	return m, nil
}

// connect checks what has to be true of the host and connects to systemd.
func (h cgroupHost) connect() (systemdAPI, error) {
	comm, err := h.pid1()
	if err != nil {
		return nil, fmt.Errorf("can't tell what PID 1 is: %w", err)
	}
	if comm != "systemd" {
		return nil, fmt.Errorf("PID 1 is %q, not systemd", comm)
	}
	ok, err := h.cgroup2()
	if err != nil {
		return nil, fmt.Errorf("can't tell whether cgroup v2 is mounted: %w", err)
	}
	if !ok {
		return nil, errors.New("cgroup v2 is not mounted at /sys/fs/cgroup")
	}
	ctx, cancel := context.WithTimeout(context.Background(), placementTimeout)
	defer cancel()
	sd, err := h.dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("systemd's bus is unreachable: %w", err)
	}
	return sd, nil
}

// ensureSlices makes sure bhatti.slice and, for an owner, its slice exist. They
// are started on demand, top first, once per daemon: a failed placement forgets
// what was ensured, so an operator who stopped a slice can't take the next one
// by surprise. An existing slice is as good as a new one.
func (m *cgroupManager) ensureSlices(ctx context.Context, owner string) error {
	if err := m.ensureSlice(ctx, topSlice, "bhatti sandboxes", cpuWeightTop); err != nil {
		return err
	}
	if owner == "" {
		return nil
	}
	return m.ensureSlice(ctx, cgroupPrefix+"-"+owner+".slice", "bhatti sandboxes of owner "+owner, 0)
}

func (m *cgroupManager) ensureSlice(ctx context.Context, name, desc string, weight uint64) error {
	m.mu.Lock()
	known := m.slices[name]
	m.mu.Unlock()
	if known {
		return nil
	}
	if err := m.sd.StartTransientUnit(ctx, name, sliceProps(desc, weight)); err != nil && !errors.Is(err, errUnitExists) {
		return fmt.Errorf("start %s: %w", name, err)
	}
	m.mu.Lock()
	m.slices[name] = true
	m.mu.Unlock()
	return nil
}

func (m *cgroupManager) forgetSlices() {
	m.mu.Lock()
	m.slices = map[string]bool{}
	m.mu.Unlock()
}

// start starts cmd, a helper that belongs in ps's scope, so that it does no work
// before it is there. Without systemd mode it is cmd.Start and nothing more.
//
// With it, the helper is started behind a launch gate (launchgate): it can do
// nothing until the daemon writes its byte. The daemon asks systemd to move the
// helper into its scope and waits until /proc shows it there — the reply to the
// request does not mean the pid has moved — then calls persist(pid, unit), so
// the record names the scope before the helper can run, and only then opens the
// gate. A failure at any step stops the scope, kills and reaps the helper and
// returns the reason: a sandbox never runs unaccounted where accounting is on.
//
// persist is called with the pid the moment the helper exists (unit empty),
// again with the unit once it is placed, and with (0, "") if it is torn down
// again. At most maxPlacements helpers are being placed at once; the wait for a
// slot comes before the helper is started, so none waits at its gate for the
// queue.
func (m *cgroupManager) start(ctx context.Context, cmd *exec.Cmd, ps placementSpec, persist func(pid int, unit string)) (err error) {
	if persist == nil {
		persist = func(int, string) {}
	}
	if !m.placing() {
		if err := cmd.Start(); err != nil {
			return err
		}
		persist(cmd.Process.Pid, "")
		return nil
	}
	select {
	case m.slots <- struct{}{}:
		defer func() { <-m.slots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	gate, err := launchgate.New()
	if err != nil {
		return err
	}
	defer gate.Close() // the helper's only way to learn we gave up
	if err := gate.Start(cmd); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	persist(pid, "")
	placed := false
	defer func() {
		if err == nil {
			return
		}
		if placed {
			m.discard(ps.unit)
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		m.release(ps.unit, pid)
		persist(0, "")
		if st := cmd.ProcessState; st != nil && st.Exited() {
			err = fmt.Errorf("%w (the helper itself exited: %s)", err, st)
		}
	}()
	if err := m.place(ctx, pid, ps, true); err != nil {
		return err
	}
	placed = true
	persist(pid, ps.unit)
	if err := gate.Open(); err != nil {
		return err
	}
	m.track(ps, pid)
	slog.Debug("krucible.cgroup.placed", append([]any{"unit", ps.unit, "pid", pid}, ps.log...)...)
	return nil
}

// place moves pid into ps's scope, creating the scope, and returns once pid's
// cgroup is that scope. When pid never shows up in it, the scope is stopped
// again if stopOnFail: right for a helper still held at its gate, wrong for a
// running guest being adopted, which a late move into a scope that was then
// stopped would kill.
func (m *cgroupManager) place(ctx context.Context, pid int, ps placementSpec, stopOnFail bool) error {
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	if err := m.ensureSlices(ctx, ps.owner); err != nil {
		m.forgetSlices()
		return fmt.Errorf("place %s: %w", ps.unit, err)
	}
	if err := m.startScope(ctx, pid, ps); err != nil {
		m.forgetSlices()
		return fmt.Errorf("place %s: %w", ps.unit, err)
	}
	if err := m.waitMember(ctx, pid, ps); err != nil {
		if stopOnFail {
			m.discard(ps.unit)
		}
		return fmt.Errorf("place %s: %w", ps.unit, err)
	}
	return nil
}

// startScope asks systemd for the scope. A name still held by the previous
// incarnation of the same sandbox or gateway — its process gone, its unit not
// yet forgotten, or kept loaded by this daemon's reference — is waited out and
// released, not failed on: a stop followed at once by a start is ordinary. A
// name held by a scope that is still running is never taken from it.
func (m *cgroupManager) startScope(ctx context.Context, pid int, ps placementSpec) error {
	for delay := time.Millisecond; ; delay = min(delay*2, 50*time.Millisecond) {
		err := m.sd.StartTransientUnit(ctx, ps.unit, scopeProps(ps, pid))
		if err == nil {
			m.mu.Lock()
			m.held[ps.unit] = pid // AddRef: this incarnation's reference, ours to drop
			m.mu.Unlock()
			return nil
		}
		if !errors.Is(err, errUnitExists) {
			return err
		}
		units, lerr := m.sd.Units(ctx, ps.unit)
		if lerr != nil {
			return fmt.Errorf("%w; and listing it: %v", err, lerr)
		}
		if len(units) == 1 && !units[0].running() {
			m.unref(ps.unit) // a dead incarnation this daemon kept loaded
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the scope is still held by a previous incarnation: %w", err)
		case <-time.After(delay):
		}
	}
}

// waitMember polls until pid's cgroup is ps's scope. Polling checks what
// matters, where a job's completion signal would only suggest it.
func (m *cgroupManager) waitMember(ctx context.Context, pid int, ps placementSpec) error {
	for delay := time.Millisecond; ; delay = min(delay*2, 25*time.Millisecond) {
		cg, err := m.procCg(pid)
		if err != nil {
			return fmt.Errorf("pid %d exited while it was being placed: %w", pid, err)
		}
		if memberOf(cg, ps) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("pid %d is still in %q after %s, not in its scope", pid, cg, m.timeout)
		case <-time.After(delay):
		}
	}
}

// memberOf reports whether cgroup path cg is ps's scope. A manager that isn't at
// the host's root, inside a container, puts its own prefix before it.
func memberOf(cg string, ps placementSpec) bool {
	return strings.HasSuffix(cg, ps.path())
}

// discard stops a scope that placement made and then gave up on, so it can't
// outlive the helper that was to run in it. Best effort: it is only ever called
// for a scope this daemon just created.
func (m *cgroupManager) discard(unit string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := m.sd.StopUnit(ctx, unit); err != nil && !errors.Is(err, errNoSuchUnit) {
		slog.Warn("krucible.cgroup.stop_failed", "unit", unit, "error", err)
	}
}

// unref drops this daemon's reference on a scope, whoever it was taken for, if
// it holds one.
func (m *cgroupManager) unref(unit string) {
	m.mu.Lock()
	_, held := m.held[unit]
	delete(m.held, unit)
	m.mu.Unlock()
	if !held {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = m.sd.UnrefUnit(ctx, unit) // already unreferenced is fine
}

// release drops the reference taken for pid, and only that one: a scope's name
// is reused by the sandbox's next run, whose own reference must outlive the
// previous run's watcher.
func (m *cgroupManager) release(unit string, pid int) {
	m.mu.Lock()
	mine := m.held[unit] == pid
	m.mu.Unlock()
	if mine {
		m.unref(unit)
	}
}

// track watches a placed (or adopted) helper and lets go of its scope when it
// is gone. AddRef kept the unit loaded after its last process left, so what
// systemd saved of its life — CPU time, peak memory, bytes read and written,
// and how it ended — can be read here, once, before the reference is dropped
// and the unit with it. The cgroup itself is gone by then: its memory.events
// and pressure counters can't be read after the fact.
func (m *cgroupManager) track(ps placementSpec, pid int) {
	alive := ps.alive
	if alive == nil {
		alive = pidAlive
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		if !waitProcessExit(pid, func() bool { return alive(pid) }, m.stop) {
			return // the daemon is shutting down; its connection takes the reference with it
		}
		m.finish(ps, pid)
	}()
}

// finish records the final numbers of the scope pid ran in, and releases it.
func (m *cgroupManager) finish(ps placementSpec, pid int) {
	defer m.release(ps.unit, pid)
	ctx, cancel := context.WithTimeout(context.Background(), finalWait)
	defer cancel()
	for delay := 5 * time.Millisecond; ; delay = min(delay*2, 100*time.Millisecond) {
		m.mu.Lock()
		mine := m.held[ps.unit] == pid
		m.mu.Unlock()
		units, err := m.sd.Units(ctx, ps.unit)
		if !mine || err != nil || len(units) == 0 {
			return // taken over by the next run, or systemd doesn't have it: nothing was kept
		}
		if st := units[0]; !st.running() && st.Active != "deactivating" {
			break
		}
		select {
		case <-ctx.Done():
			return // something still runs in it: nothing final to report
		case <-m.stop:
			return
		case <-time.After(delay):
		}
	}
	u, err := m.sd.Usage(ctx, ps.unit)
	if err != nil {
		slog.Debug("krucible.cgroup.final_unavailable", append([]any{"unit", ps.unit, "error", err}, ps.log...)...)
		return
	}
	slog.Info("krucible.cgroup.final", append([]any{"unit", ps.unit,
		"cpu_ms", nanosToMillis(u.CPUNanos), "mem_peak_bytes", u.MemoryPeak,
		"io_read_bytes", u.IORead, "io_write_bytes", u.IOWrite, "result", u.Result}, ps.log...)...)
}

func nanosToMillis(ns int64) int64 {
	if ns < 0 {
		return -1
	}
	return ns / int64(time.Millisecond)
}

// placedScope is where a helper was found to run: its scope, and whether
// anything it did before it was moved there is accounted somewhere else.
type placedScope struct {
	unit    string
	partial bool // placed after it started: its earlier memory stays in the daemon's cgroup until its next start
}

// accounting says what the numbers for a helper in this state cover:
// "cgroup" (all of its use), "partial", "process" (per-process only, where
// there are no cgroups) or "none".
func (m *cgroupManager) accounting(s placedScope) string {
	switch {
	case m == nil:
		return engine.CgroupNone
	case m.mode == engine.CgroupProcess:
		return engine.CgroupProcess
	case !m.placing() || s.unit == "":
		return engine.CgroupNone
	case s.partial:
		return "partial"
	}
	return "cgroup"
}

// adopt takes over a helper this daemon didn't start: one a previous daemon left
// running, already identified as ps's helper by its caller. rec is what the
// helper's record says about its scope.
//
// A scope is never taken on its name alone. The helper must be in the cgroup
// the scope has, and systemd must list the unit as running. A helper in its
// scope is adopted as recorded. One outside it — started by a daemon before
// placement existed, or moved by someone since — is placed now, and flagged
// partial: what it allocated before the move was charged to the daemon's cgroup
// and stays there until the helper next starts. A scope that is running but
// doesn't hold the helper is left alone: it holds something else.
func (m *cgroupManager) adopt(ctx context.Context, pid int, ps placementSpec, rec placedScope) (placedScope, error) {
	if !m.placing() {
		return placedScope{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	cg, err := m.procCg(pid)
	if err != nil {
		return placedScope{}, fmt.Errorf("read cgroup of pid %d: %w", pid, err)
	}
	units, err := m.sd.Units(ctx, ps.unit)
	if err != nil {
		return placedScope{}, fmt.Errorf("look up %s: %w", ps.unit, err)
	}
	running := len(units) == 1 && units[0].running()
	if memberOf(cg, ps) {
		if !running {
			return placedScope{}, fmt.Errorf("pid %d is in %s's cgroup but systemd doesn't list that scope as running", pid, ps.unit)
		}
		if err := m.sd.RefUnit(ctx, ps.unit); err != nil {
			slog.Debug("krucible.cgroup.ref_failed", "unit", ps.unit, "error", err)
		} else {
			m.mu.Lock()
			m.held[ps.unit] = pid
			m.mu.Unlock()
		}
		m.track(ps, pid)
		return placedScope{unit: ps.unit, partial: rec.partial}, nil
	}
	if running {
		return placedScope{}, fmt.Errorf("scope %s is running but pid %d is in %q, not in it: not moving a helper into a scope that holds something else", ps.unit, pid, cg)
	}
	if err := m.place(ctx, pid, ps, false); err != nil {
		m.release(ps.unit, pid)
		return placedScope{}, err
	}
	m.track(ps, pid)
	return placedScope{unit: ps.unit, partial: true}, nil
}

// listScopes names the scopes systemd has for bhatti, for the report of any no
// recovered helper claims.
func (m *cgroupManager) listScopes(ctx context.Context) ([]unitStatus, error) {
	if !m.placing() {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	return m.sd.Units(ctx, cgroupPrefix+"-*.scope")
}

// close ends the watchers and drops the connection to systemd, which drops the
// references this daemon held with it.
func (m *cgroupManager) close() {
	if !m.placing() {
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	m.mu.Unlock()
	close(m.stop)
	m.wg.Wait()
	_ = m.sd.Close()
}

// defaultCgroupHost is the host this process runs on; cgroup_linux.go and
// cgroup_other.go fill in what differs.
func defaultCgroupHost() cgroupHost {
	h := hostCgroups()
	h.goos = runtime.GOOS
	return h
}

// cgroupOwner is the owner slug of vm's slice: the key its netd is keyed by, so
// a sandbox with no NIC, and so no netd, still sits in its owner's slice beside
// the others.
func (vm *VM) cgroupOwner() string {
	key := vm.netdKey
	if key == "" {
		key = ownerKeyFor(vm.UserID, vm.ID)
	}
	return ownerSlug(key)
}

// placementFor says where vm's helper belongs. With placement off it is the
// zero spec, which the manager never reads.
func (e *Engine) placementFor(vm *VM) (placementSpec, error) {
	if !e.cg.placing() {
		return placementSpec{}, nil
	}
	ps, err := vmPlacement(vm.cgroupOwner(), vm.ID)
	if err != nil {
		return ps, err
	}
	spec := vm.specPath()
	ps.alive = func(pid int) bool { return pidAlive(pid) && isHelper(pid, spec) }
	return ps, nil
}

// netdPlacementFor says where the owner's gateway belongs.
func (e *Engine) netdPlacementFor(inst *netdInstance) (placementSpec, error) {
	if !e.cg.placing() {
		return placementSpec{}, nil
	}
	ps, err := netdPlacement(ownerSlug(inst.owner))
	if err != nil {
		return ps, err
	}
	sock := inst.sock
	ps.alive = func(pid int) bool { return pidAlive(pid) && isNetd(pid, sock) }
	return ps, nil
}

// CgroupStatus reports how this engine places helpers, for /health.
func (e *Engine) CgroupStatus() engine.CgroupStatus { return e.cg.status() }

var _ engine.CgroupReporter = (*Engine)(nil)

// Accounting says what the usage figures for sandbox id cover: "cgroup" (all
// of what its VM does), "partial" (its VM was placed after it had started, so
// what it used before is in the daemon's cgroup), "process" (per-process
// numbers only: this host has no cgroups) or "none".
func (e *Engine) Accounting(id string) string {
	vm, err := e.getVM(id)
	if err != nil {
		return engine.CgroupNone
	}
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if vm.Status != "running" {
		return e.cg.accounting(placedScope{})
	}
	return e.cg.accounting(vm.scope)
}

// adoptScope settles the scope of a helper recovery has just adopted. A failure
// leaves the guest running, unaccounted, and says so: stopping a guest because
// it can't be measured would turn a daemon restart into a reboot.
func (e *Engine) adoptScope(vm *VM) {
	if !e.cg.placing() {
		return
	}
	vm.mu.Lock()
	pid, recorded := vm.HelperPID, vm.scope
	vm.mu.Unlock()
	ps, err := e.placementFor(vm)
	var scope placedScope
	if err == nil {
		scope, err = e.cg.adopt(context.Background(), pid, ps, recorded)
	}
	if err != nil {
		scope = placedScope{}
		slog.Error("krucible.cgroup.adopt_failed", "sandbox_id", vm.ID, "pid", pid, "error", err)
	} else {
		slog.Info("krucible.cgroup.adopted", "sandbox_id", vm.ID, "unit", scope.unit, "partial", scope.partial)
	}
	vm.mu.Lock()
	vm.scope = scope
	vm.mu.Unlock()
}

// adoptNetdScopeLocked is adoptScope for an adopted gateway. It is retried by
// the next ensureNetd until it holds. Caller holds inst.mu.
func (e *Engine) adoptNetdScopeLocked(inst *netdInstance) {
	if !e.cg.placing() || inst.scopePID == inst.pid {
		return
	}
	ps, err := e.netdPlacementFor(inst)
	if err == nil {
		var scope placedScope
		if scope, err = e.cg.adopt(context.Background(), inst.pid, ps, inst.scope); err == nil {
			inst.scope, inst.scopePID = scope, inst.pid
			writeNetdRecord(inst)
			slog.Info("krucible.cgroup.adopted", "netd_owner", ps.owner, "unit", scope.unit, "partial", scope.partial)
			return
		}
	}
	slog.Error("krucible.cgroup.adopt_failed", "netd_owner", inst.owner, "pid", inst.pid, "error", err)
}

// reportOrphanScopes names the scopes systemd holds for bhatti that no recovered
// sandbox or gateway claims. A stray helper of a sandbox recorded stopped is
// gone by now (killStrays); what remains is left alone: it may be a helper
// whose record was lost, and stopping it would kill a guest.
func (e *Engine) reportOrphanScopes() {
	if !e.cg.placing() {
		return
	}
	units, err := e.cg.listScopes(context.Background())
	if err != nil {
		slog.Warn("krucible.cgroup.list_failed", "error", err)
		return
	}
	claimed := map[string]bool{}
	e.mu.RLock()
	for _, vm := range e.vms {
		vm.mu.Lock()
		claimed[vm.scope.unit] = true
		vm.mu.Unlock()
	}
	e.mu.RUnlock()
	e.netdMu.Lock()
	for _, inst := range e.netds {
		inst.mu.Lock()
		claimed[inst.scope.unit] = true
		inst.mu.Unlock()
	}
	e.netdMu.Unlock()
	for _, u := range units {
		if !claimed[u.Name] {
			slog.Warn("krucible.cgroup.orphan_scope", "unit", u.Name, "description", u.Description, "state", u.Active)
		}
	}
}
