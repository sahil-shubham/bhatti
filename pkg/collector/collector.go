package collector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"
	"sync"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/gateway"
)

const (
	// DefaultInterval is how often everything is read. At fifty sandboxes a
	// sample is a few hundred small file reads.
	DefaultInterval = 10 * time.Second
	// DefaultRetain is how much of each sandbox's history is kept: 360 points
	// at the default interval.
	DefaultRetain = time.Hour

	defaultCgroupRoot = "/sys/fs/cgroup"
	defaultProcRoot   = "/proc"
	defaultBlockRoot  = "/sys/dev/block"

	// maxNetdPolls bounds the gateways asked for their figures at once, so one
	// that doesn't answer delays a sample by its timeout and not by the number
	// of gateways times it.
	maxNetdPolls = 8
)

// Config says what to sample and how. The zero value samples the host only,
// every DefaultInterval, and keeps DefaultRetain.
type Config struct {
	// Source lists what to measure. nil measures the host alone.
	Source engine.StatsSource
	// DataDir is the filesystem whose free space is reported.
	DataDir  string
	Interval time.Duration
	Retain   time.Duration

	// Where the kernel's files are, for hosts that mount them elsewhere and for
	// tests. Empty means /sys/fs/cgroup, /proc and /sys/dev/block.
	CgroupRoot string
	ProcRoot   string
	BlockRoot  string

	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Collector reads the figures every Interval and keeps them.
type Collector struct {
	src       engine.StatsSource
	interval  time.Duration
	retain    time.Duration
	capacity  int // points kept per sandbox
	now       func() time.Time
	cgroupDir string

	// Where figures come from that tests replace.
	netStats  func(ctx context.Context, ctlSock string) (gateway.NetStats, error)
	procUsage func(pid int) (Usage, error)
	readHost  func() (Host, error)

	// sampleMu serializes samples and guards what only a sample touches.
	sampleMu    sync.Mutex
	rd          usageReader
	host        hostReader
	prevSandbox map[string]*sample
	prevOwner   map[string]*sample
	prevNetd    map[string]*sample
	prevHostCPU *HostCPU
	prevHostAt  time.Time
	faults      faults // what is failing, for logging it once

	// mu guards what readers see.
	mu     sync.RWMutex
	latest *Snapshot
	rings  map[string]*ring[Point]

	startMu sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	stopped bool
}

// New makes a collector. It reads nothing until Start or Sample.
func New(cfg Config) *Collector {
	c := &Collector{
		src:       cfg.Source,
		interval:  cfg.Interval,
		retain:    cfg.Retain,
		now:       cfg.Now,
		cgroupDir: cfg.CgroupRoot,

		prevSandbox: map[string]*sample{},
		prevOwner:   map[string]*sample{},
		prevNetd:    map[string]*sample{},
		faults:      faults{cur: map[string]string{}},
		rings:       map[string]*ring[Point]{},
		latest:      &Snapshot{Sandboxes: []Sandbox{}, Owners: []Owner{}, Netds: []Netd{}},
	}
	if c.interval <= 0 {
		c.interval = DefaultInterval
	}
	if c.retain <= 0 {
		c.retain = DefaultRetain
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.cgroupDir == "" {
		c.cgroupDir = defaultCgroupRoot
	}
	procRoot, blockRoot := cfg.ProcRoot, cfg.BlockRoot
	if procRoot == "" {
		procRoot = defaultProcRoot
	}
	if blockRoot == "" {
		blockRoot = defaultBlockRoot
	}
	c.capacity = max(int(c.retain/c.interval), 1)
	c.rd.stacked = newBlockTopology(blockRoot, c.now).stacked
	c.host = hostReader{rd: &c.rd, procRoot: procRoot, dataDir: cfg.DataDir}
	c.readHost = c.host.read
	c.netStats = gateway.QueryStats
	c.procUsage = newProcessReader().usage
	return c
}

// Start begins sampling in the background: once now, then every interval. It
// returns at once; Stop ends it. Starting twice, or after Stop, does nothing.
func (c *Collector) Start() {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	if c.cancel != nil || c.stopped {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel, c.done = cancel, make(chan struct{})
	go c.loop(ctx, c.done)
	slog.Info("collector.started", "interval", c.interval.String(), "retain", c.retain.String())
}

// Stop ends sampling and waits for a sample in progress to finish.
func (c *Collector) Stop() {
	c.startMu.Lock()
	cancel, done := c.cancel, c.done
	c.stopped = true
	c.startMu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
	slog.Info("collector.stopped")
}

func (c *Collector) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	c.sampleSafely(ctx)
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.sampleSafely(ctx)
		}
	}
}

// sampleSafely keeps a bug in the sampler from taking the daemon down with it:
// what it measures is not worth a crash.
func (c *Collector) sampleSafely(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("collector.panic", "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
		}
	}()
	c.Sample(ctx)
}

// Snapshot is the latest complete sample; empty before the first. It is never
// modified, so the caller may keep and read it freely.
func (c *Collector) Snapshot() *Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.latest
}

// Series is a sandbox's last hour: one point per sampled interval, oldest
// first. It is kept after the sandbox stops, until the hour is up or the engine
// forgets it; nil for one never sampled.
func (c *Collector) Series(sandboxID string) []Point {
	c.mu.RLock()
	defer c.mu.RUnlock()
	r := c.rings[sandboxID]
	if r == nil {
		return nil
	}
	return r.appendTo(make([]Point, 0, r.len()))
}

// Sandbox is a sandbox's sample in this snapshot, if it was measured then.
func (s *Snapshot) Sandbox(id string) (Sandbox, bool) {
	i := sort.Search(len(s.Sandboxes), func(i int) bool { return s.Sandboxes[i].ID >= id })
	if i < len(s.Sandboxes) && s.Sandboxes[i].ID == id {
		return s.Sandboxes[i], true
	}
	return Sandbox{}, false
}

// Sample reads everything once, publishes it as the latest snapshot, adds to
// the sandboxes' histories, and returns it. Start does this every interval;
// tests call it directly. A sample cut short by ctx is dropped.
func (c *Collector) Sample(ctx context.Context) *Snapshot {
	c.sampleMu.Lock()
	defer c.sampleMu.Unlock()
	start := c.now()
	c.faults.next()

	var targets engine.StatsTargets
	if c.src != nil {
		targets = c.src.StatsTargets()
	}
	snap := &Snapshot{At: start, Sandboxes: []Sandbox{}, Owners: []Owner{}, Netds: []Netd{}}
	snap.Host = c.sampleHost(start)

	sandboxes := c.readSandboxes(targets.Sandboxes)
	owners := c.readOwners(targets.Owners)
	netds := c.readNetds(ctx, targets.Netds)
	if ctx.Err() != nil {
		return c.Snapshot() // stopping: what was half read isn't published
	}

	// Each gateway's per-guest figures belong to the sandbox at that address.
	guests := map[[2]string]gateway.GuestStats{}
	for _, n := range netds {
		for _, g := range n.stats.Guests {
			guests[[2]string{n.Owner, g.GuestIP}] = g
		}
	}
	for i := range sandboxes {
		if g, ok := guests[[2]string{sandboxes[i].Owner, sandboxes[i].netIP}]; ok && sandboxes[i].netIP != "" {
			sandboxes[i].Net = &Net{
				RxBytes: g.RxBytes, RxPackets: g.RxPackets, TxBytes: g.TxBytes, TxPackets: g.TxPackets,
				DroppedPolicy: g.Dropped.Policy, DroppedSpoof: g.Dropped.Spoof, DroppedHardDeny: g.Dropped.HardDeny,
			}
		}
	}

	// Rates against the previous sample of each; the sandboxes' also make the
	// history.
	points := map[string]Point{}
	keep := map[string]bool{}
	for i := range sandboxes {
		s := &sandboxes[i]
		s.Rates = c.rated(c.prevSandbox, s.ID, &sample{at: s.At, ident: s.ident, usage: s.Usage, net: s.Net})
		if s.Rates != nil {
			points[s.ID] = point(s.At, &s.Usage, *s.Rates)
		}
		keep[s.ID] = true
		snap.Sandboxes = append(snap.Sandboxes, s.Sandbox)
	}
	prune(c.prevSandbox, keep)
	keep = map[string]bool{}
	for i := range owners {
		o := &owners[i]
		o.Rates = c.rated(c.prevOwner, o.ID, &sample{at: o.At, ident: o.ident, usage: o.Usage})
		keep[o.ID] = true
		snap.Owners = append(snap.Owners, o.Owner)
	}
	prune(c.prevOwner, keep)
	keep = map[string]bool{}
	for i := range netds {
		n := &netds[i]
		n.Rates = c.rated(c.prevNetd, n.Owner, &sample{at: n.At, ident: n.ident, usage: n.Usage})
		keep[n.Owner] = true
		snap.Netds = append(snap.Netds, n.Netd)
	}
	prune(c.prevNetd, keep)

	sort.Slice(snap.Sandboxes, func(i, j int) bool { return snap.Sandboxes[i].ID < snap.Sandboxes[j].ID })
	sort.Slice(snap.Owners, func(i, j int) bool { return snap.Owners[i].ID < snap.Owners[j].ID })
	sort.Slice(snap.Netds, func(i, j int) bool { return snap.Netds[i].Owner < snap.Netds[j].Owner })
	snap.Duration = c.now().Sub(start)
	c.publish(snap, targets.Sandboxes, points)
	slog.Debug("collector.sampled", "sandboxes", len(snap.Sandboxes), "owners", len(snap.Owners), "netds", len(snap.Netds), "took", snap.Duration.String())
	return snap
}

// rated returns the rates between the thing's previous sample and cur, and
// makes cur the previous one. nil on a thing's first sample.
func (c *Collector) rated(prev map[string]*sample, key string, cur *sample) *Rates {
	p := prev[key]
	prev[key] = cur
	if p == nil {
		return nil
	}
	r, ok := deriveRates(p, cur)
	if !ok {
		return nil
	}
	return &r
}

// prune forgets the samples of what is no longer measured, so the next sample
// of something that comes back is its first.
func prune(prev map[string]*sample, keep map[string]bool) {
	for k := range prev {
		if !keep[k] {
			delete(prev, k)
		}
	}
}

// publish makes snap the latest and adds the sandboxes' points to their
// histories. A history is dropped when the engine no longer knows its sandbox,
// or when nothing has been added to it for the retention.
func (c *Collector) publish(snap *Snapshot, known []engine.StatsSandbox, points map[string]Point) {
	exists := make(map[string]bool, len(known))
	for _, t := range known {
		exists[t.ID] = true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, p := range points {
		r := c.rings[id]
		if r == nil {
			r = newRing[Point](c.capacity)
			c.rings[id] = r
		}
		r.push(p)
	}
	horizon := snap.At.Add(-c.retain)
	for id, r := range c.rings {
		if last, ok := r.last(); !exists[id] || !ok || last.At.Before(horizon) {
			delete(c.rings, id)
		}
	}
	c.latest = snap
}

// sandboxRead is a sandbox sampled, with what joining and rating it needs.
type sandboxRead struct {
	Sandbox
	ident incarnation
	netIP string
}

// ownerRead is an owner sampled.
type ownerRead struct {
	Owner
	ident incarnation
}

// netdRead is a gateway sampled, with what it said about its guests.
type netdRead struct {
	Netd
	ident incarnation
	stats gateway.NetStats
}

func (c *Collector) readSandboxes(targets []engine.StatsSandbox) []sandboxRead {
	reads := make([]sandboxRead, 0, len(targets))
	for _, t := range targets {
		if t.PID <= 0 {
			continue // not running: nothing of it to read
		}
		u, ok := c.measure("sandbox "+t.ID, t.Accounting, t.Cgroup, t.PID)
		if !ok {
			continue
		}
		reads = append(reads, sandboxRead{
			Sandbox: Sandbox{ID: t.ID, Owner: t.Owner, UserID: t.UserID, Accounting: t.Accounting, PID: t.PID, At: c.now(), Usage: u},
			ident:   incarnation{t.PID, t.Cgroup},
			netIP:   t.NetIP,
		})
	}
	return reads
}

func (c *Collector) readOwners(targets []engine.StatsOwner) []ownerRead {
	reads := make([]ownerRead, 0, len(targets))
	for _, t := range targets {
		u, ok := c.measure("owner "+t.Owner, engine.AccountingCgroup, t.Cgroup, 0)
		if !ok {
			continue
		}
		reads = append(reads, ownerRead{Owner: Owner{ID: t.Owner, UserID: t.UserID, At: c.now(), Usage: u}, ident: incarnation{cgroup: t.Cgroup}})
	}
	return reads
}

// readNetds reads each gateway's own use, then asks each for what it carried
// for its guests, several at a time.
func (c *Collector) readNetds(ctx context.Context, targets []engine.StatsNetd) []netdRead {
	reads := make([]netdRead, 0, len(targets))
	for _, t := range targets {
		u, ok := c.measure("netd "+t.Owner, t.Accounting, t.Cgroup, t.PID)
		if !ok {
			continue
		}
		reads = append(reads, netdRead{
			Netd:  Netd{Owner: t.Owner, PID: t.PID, Accounting: t.Accounting, At: c.now(), Usage: u},
			ident: incarnation{t.PID, t.Cgroup},
		})
	}
	var wg sync.WaitGroup
	slots := make(chan struct{}, maxNetdPolls)
	errs := make([]error, len(reads))
	sockOf := make(map[string]string, len(targets))
	for _, t := range targets {
		sockOf[t.Owner] = t.CtlSock
	}
	for i := range reads {
		sock := sockOf[reads[i].Owner]
		if sock == "" {
			continue
		}
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			reads[i].stats, errs[i] = c.netStats(ctx, sock)
		}()
	}
	wg.Wait()
	for i := range reads {
		reads[i].StatsErr = c.noteNetdStats(reads[i].Owner, errs[i])
	}
	return reads
}

// noteNetdStats turns the outcome of asking a gateway for its figures into the
// reason to show for them being missing ("" if they aren't), logging a lasting
// fault once.
func (c *Collector) noteNetdStats(owner string, err error) string {
	key := "netd stats " + owner
	switch {
	case err == nil, errors.Is(err, context.Canceled):
		return ""
	case errors.Is(err, gateway.ErrStatsUnsupported):
		if c.faults.note(key, "unsupported") {
			slog.Info("collector.netd_stats_unsupported", "netd_owner", owner, "hint", "this gateway predates the stats request; it is replaced when its owner's sandboxes next restart")
		}
		return "unsupported: this gateway predates the stats request"
	}
	if c.faults.note(key, err.Error()) {
		slog.Warn("collector.netd_stats_failed", "netd_owner", owner, "error", err.Error())
	}
	return err.Error()
}

// measure reads one thing's figures the way its accounting says. A sandbox
// with none is listed with nothing read; ok is false for what has gone since it
// was listed. Faults are logged once and what could be read is kept.
func (c *Collector) measure(what, accounting, cgroup string, pid int) (u Usage, ok bool) {
	var err error
	switch accounting {
	case engine.AccountingCgroup, engine.AccountingPartial:
		if cgroup == "" {
			return Usage{}, true
		}
		u, err = c.rd.usage(c.cgroupDir + cgroup)
	case engine.AccountingProcess:
		u, err = c.procUsage(pid)
	default:
		return Usage{}, true
	}
	if errors.Is(err, errGone) {
		return Usage{}, false
	}
	c.noteRead(what, err)
	return u, true
}

// noteRead logs a read fault once for as long as it lasts, and again if it
// comes back.
func (c *Collector) noteRead(what string, err error) {
	if err != nil && c.faults.note(what, err.Error()) {
		slog.Warn("collector.read_failed", "what", what, "error", err.Error())
	}
}

// faults remembers what is failing and how, so that a fault that lasts is
// logged once. A thing that isn't reported failing in a sample (it recovered, or
// is gone) is forgotten, so the log can't grow with what has come and gone and
// a fault that returns is news again.
type faults struct{ last, cur map[string]string }

// next starts a sample.
func (f *faults) next() { f.last, f.cur = f.cur, map[string]string{} }

// note records that key is failing with msg and reports whether that is news.
func (f *faults) note(key, msg string) bool {
	f.cur[key] = msg
	return f.last[key] != msg
}

// sampleHost reads the host and derives its CPU use from the last reading.
func (c *Collector) sampleHost(at time.Time) *Host {
	h, err := c.readHost()
	c.noteRead("host", err)
	h.At = at
	if h.CPU != nil {
		if c.prevHostCPU != nil {
			if p, ok := hostCPUPercent(c.prevHostCPU, h.CPU); ok && at.After(c.prevHostAt) {
				h.CPUPercent = &p
			}
		}
		cpu := *h.CPU
		c.prevHostCPU, c.prevHostAt = &cpu, at
	}
	return &h
}

// hostCPUPercent is the share of the CPU time that passed between two
// readings that was neither idle nor waiting for I/O. A reading lower than the
// last (a counter that can't go down did) has no answer.
func hostCPUPercent(prev, cur *HostCPU) (float64, bool) {
	total := func(c *HostCPU) float64 {
		return c.User + c.Nice + c.System + c.Idle + c.IOWait + c.IRQ + c.SoftIRQ + c.Steal
	}
	dTotal := total(cur) - total(prev)
	dIdle := cur.Idle + cur.IOWait - prev.Idle - prev.IOWait
	if dTotal <= 0 || dIdle < 0 || dIdle > dTotal {
		return 0, false
	}
	return (dTotal - dIdle) / dTotal * 100, true
}
