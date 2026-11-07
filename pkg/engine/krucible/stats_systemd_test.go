//go:build linux

package krucible

// The collector against a real systemd and a real kernel: helpers placed in
// scopes by the real placement, each doing one kind of work, read through the
// engine's own listing. Opt-in like the placement tests (BHATTI_TEST_SYSTEMD=1,
// root, systemd as PID 1); scripts/test-cgroups-systemd.sh runs it in a
// privileged container:
//
//	scripts/test-cgroups-systemd.sh -test.run TestSystemdCollector

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/collector"
	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/launchgate"
	"github.com/sahil-shubham/bhatti/pkg/promtext/promtexttest"
)

// workHelper starts a shell stand-in for a VMM, placed in ps's scope by the real
// placement. Once its gate opens it runs work, which it and its children do
// inside the scope; it writes <marker> first.
func workHelper(t *testing.T, m *cgroupManager, ps placementSpec, work string) (cmd *exec.Cmd, marker string) {
	t.Helper()
	dir := t.TempDir()
	marker = filepath.Join(dir, "started")
	script := filepath.Join(dir, "helper.sh")
	body := "#!/bin/sh\n" +
		"eval \"b=\\$(dd bs=1 count=1 <&${" + launchgate.Env + ":?} 2>/dev/null)\"\n" +
		"[ \"$b\" = G ] || exit 3\n" +
		"echo started > \"$1\"\n" +
		work + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command(script, marker)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := m.start(context.Background(), cmd, ps, nil); err != nil {
		t.Fatalf("place %s: %v", ps.unit, err)
	}
	t.Cleanup(func() { reap(t, cmd) })
	return cmd, marker
}

func workHelperCPU(t *testing.T, ps placementSpec) uint64 {
	t.Helper()
	var usec uint64
	if _, err := fmt.Sscanf(readCgroupFile(t, cgroupDir(t, ps.path()), "cpu.stat"), "usage_usec %d", &usec); err != nil {
		t.Fatal(err)
	}
	return usec
}

func TestSystemdCollectorSeesPlacedHelpersMove(t *testing.T) {
	m := systemdManager(t, false)
	key := fmt.Sprintf("collector-test:%d", time.Now().UnixNano())
	owner := ownerSlug(key)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = m.sd.StopUnit(ctx, "bhatti-"+owner+".slice") // and every process in it
	})
	// Disk-backed, or the writer would only dirty memory: /tmp is a tmpfs on
	// current Debian.
	scratch, err := os.MkdirTemp("/var/tmp", "bhatti-collector-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(scratch) })
	var fsStat syscall.Statfs_t
	if err := syscall.Statfs(scratch, &fsStat); err != nil {
		t.Fatal(err)
	}
	const tmpfsMagic = 0x01021994
	onTmpfs := int64(fsStat.Type) == tmpfsMagic

	// One owner, four sandboxes doing one thing each, and its gateway.
	e := &Engine{vms: map[string]*VM{}, netds: map[string]*netdInstance{}, cg: m}
	type load struct {
		id, work string
	}
	loads := map[string]load{
		"burn":  {"00000000000000b1", "while :; do :; done"},
		"hog":   {"00000000000000b2", "dd if=/dev/zero bs=96M count=1 2>/dev/null | sleep 600"},
		"disk":  {"00000000000000b3", "while :; do dd if=/dev/zero of=" + scratch + "/w bs=1M count=32 oflag=direct 2>/dev/null || dd if=/dev/zero of=" + scratch + "/w bs=1M count=32 conv=fsync 2>/dev/null; done"},
		"quiet": {"00000000000000b4", "exec sleep 600"},
	}
	markers := map[string]string{}
	for name, l := range loads {
		ps, err := vmPlacement(owner, l.id)
		if err != nil {
			t.Fatal(err)
		}
		cmd, marker := workHelper(t, m, ps, l.work)
		markers[name] = marker
		e.vms[l.id] = &VM{ID: l.id, Status: "running", HelperPID: cmd.Process.Pid, netdKey: key, scope: placedScope{unit: ps.unit}}
	}
	netdPS, err := netdPlacement(owner)
	if err != nil {
		t.Fatal(err)
	}
	netd, netdMarker := workHelper(t, m, netdPS, "exec sleep 600")
	e.netds[key] = &netdInstance{owner: key, pid: netd.Process.Pid, scope: placedScope{unit: netdPS.unit}}
	for name, marker := range markers {
		waitFor(t, name+" to start work", func() bool { return exists(marker) })
	}
	waitFor(t, "the gateway to start", func() bool { return exists(netdMarker) })
	hogPS, _ := vmPlacement(owner, loads["hog"].id)
	waitFor(t, "the memory hog to touch its allocation", func() bool {
		return readInt(t, cgroupDir(t, hogPS.path()), "memory.current") >= 90<<20
	})

	col := collector.New(collector.Config{Source: e, DataDir: scratch, Interval: time.Second})
	ctx := context.Background()

	first := col.Sample(ctx)
	for name, l := range loads {
		sb, ok := first.Sandbox(l.id)
		if !ok {
			t.Fatalf("%s isn't in the first sample: %+v", name, first.Sandboxes)
		}
		if sb.Accounting != engine.AccountingCgroup || sb.CPU == nil || sb.Memory == nil || sb.Rates != nil {
			t.Fatalf("%s: first sample = %+v", name, sb)
		}
	}
	if len(first.Owners) != 1 || len(first.Netds) != 1 || first.Host == nil || first.Host.CPU == nil || first.Host.Memory == nil {
		t.Fatalf("the rest of the first sample: %d owners, %d gateways, host %+v", len(first.Owners), len(first.Netds), first.Host)
	}

	burnPS, _ := vmPlacement(owner, loads["burn"].id)
	advance := func(prev *collector.Snapshot) *collector.Snapshot {
		t.Helper()
		burn, _ := prev.Sandbox(loads["burn"].id)
		// Completion is measured work, not a fixed window that assumes the
		// container received a particular share of the host's CPUs.
		waitFor(t, "another CPU second in the burner's scope", func() bool {
			return workHelperCPU(t, burnPS) >= burn.CPU.UsageUsec+1_000_000
		})
		return col.Sample(ctx)
	}
	second := advance(first)
	third := advance(second)

	got := func(s *collector.Snapshot, name string) collector.Sandbox {
		t.Helper()
		sb, ok := s.Sandbox(loads[name].id)
		if !ok || sb.Rates == nil {
			t.Fatalf("%s has no rates in %+v", name, s.Sandboxes)
		}
		return sb
	}
	for _, name := range []string{"burn", "hog", "disk", "quiet"} {
		sb := got(second, name)
		t.Logf("%-5s cpu %6.1f%%  mem %7.1f MiB (peak %.1f)  io read %8.1f KiB/s write %9.1f KiB/s  io pressure some %.1f%% full %.1f%%  oom_kills %d",
			name, sb.Rates.CPUPercent, float64(sb.Memory.Current)/(1<<20), float64(sb.Memory.Peak)/(1<<20),
			sb.Rates.IOReadBytesPerSec/1024, sb.Rates.IOWriteBytesPerSec/1024, sb.Rates.IOSomePercent, sb.Rates.IOFullPercent, sb.Events.OOMKill)
	}

	// A loaded host can give the burner less than one CPU; attribution, not
	// fairness, is the contract. The sleeper must not inherit its CPU time.
	burn, quiet := got(second, "burn"), got(second, "quiet")
	if burn.Rates.CPUPercent <= 0 || burn.Rates.CPUPercent > 130 {
		t.Errorf("the single-threaded CPU burner used %.1f%% of a CPU", burn.Rates.CPUPercent)
	}
	if quiet.Rates.CPUPercent > 5 {
		t.Errorf("the sleeper used %.1f%% of a CPU", quiet.Rates.CPUPercent)
	}
	// The burner's CPU time keeps counting between the samples.
	if b3 := got(third, "burn"); b3.CPU.UsageUsec < burn.CPU.UsageUsec+1_000_000 {
		t.Errorf("burner's CPU time went from %d to %d µs", burn.CPU.UsageUsec, b3.CPU.UsageUsec)
	}

	// The hog holds the 96 MiB it touched; the sleeper holds almost nothing.
	hog := got(second, "hog")
	if hog.Memory.Current < 90<<20 || hog.Memory.Peak < hog.Memory.Current || hog.Memory.Anon < 90<<20 {
		t.Errorf("the memory hog: current %d peak %d anon %d, want 96 MiB resident", hog.Memory.Current, hog.Memory.Peak, hog.Memory.Anon)
	}
	if quiet.Memory.Current > 16<<20 {
		t.Errorf("the sleeper holds %d bytes", quiet.Memory.Current)
	}

	// The disk writer's bytes move. Only where the scratch directory is on a
	// disk, and the kernel gives cgroups an io.stat, is there anything to see;
	// the test says so where there isn't.
	disk := got(second, "disk")
	switch {
	case onTmpfs:
		t.Logf("/var/tmp is a tmpfs here: the writer dirties memory and no block I/O happens")
	case disk.IO == nil:
		t.Logf("this host's cgroups have no io.stat for the disk writer: block I/O isn't accounted here")
	default:
		d3 := got(third, "disk")
		if disk.IO.WriteBytes == 0 || d3.IO.WriteBytes <= disk.IO.WriteBytes || disk.Rates.IOWriteBytesPerSec <= 0 {
			t.Errorf("the disk writer: %d bytes written, then %d, %.0f B/s", disk.IO.WriteBytes, d3.IO.WriteBytes, disk.Rates.IOWriteBytesPerSec)
		}
		if burn.IO != nil && burn.IO.WriteBytes > disk.IO.WriteBytes/10 {
			t.Errorf("the CPU burner is charged %d bytes of the writer's %d", burn.IO.WriteBytes, disk.IO.WriteBytes)
		}
	}

	// The owner's slice sums its sandboxes (and the gateway): it has at least
	// what the burner and the hog have.
	if len(second.Owners) != 1 {
		t.Fatalf("owners = %+v", second.Owners)
	}
	o := second.Owners[0]
	if o.ID != owner || o.CPU == nil || o.CPU.UsageUsec < burn.CPU.UsageUsec || o.Memory.Current < hog.Memory.Current {
		t.Errorf("owner slice: cpu %+v memory %+v, want at least the burner's CPU (%d µs) and the hog's memory (%d)", o.CPU, o.Memory, burn.CPU.UsageUsec, hog.Memory.Current)
	}
	if o.Rates == nil || o.Rates.CPUPercent < burn.Rates.CPUPercent*0.8 {
		t.Errorf("owner rates = %+v, want at least about the burner's %.1f%%", o.Rates, burn.Rates.CPUPercent)
	}
	if len(second.Netds) != 1 || second.Netds[0].Accounting != engine.AccountingCgroup || second.Netds[0].CPU == nil || second.Netds[0].Owner != owner {
		t.Errorf("gateway = %+v", second.Netds)
	}
	if h := second.Host; h.CPUPercent == nil || *h.CPUPercent <= 0 || h.Memory.AvailableBytes == 0 || h.Disk == nil {
		t.Errorf("host = %+v (cpu %v)", h, h.CPUPercent)
	}

	// The sandbox is not the owner's whole slice and not the daemon's cgroup:
	// each is in its own scope.
	for name, l := range loads {
		sb, _ := second.Sandbox(l.id)
		ps, _ := vmPlacement(owner, l.id)
		cg, err := procCgroupPath(sb.PID)
		if err != nil || !memberOf(cg, ps) {
			t.Errorf("%s's helper is in %q (%v), not in its scope", name, cg, err)
		}
	}

	// Its history is a series of intervals, newest last.
	if series := col.Series(loads["burn"].id); len(series) != 2 || !series[0].At.Before(series[1].At) || series[1].CPUPercent <= 0 {
		t.Errorf("burner history = %+v", series)
	}

	// And the text a scraper would read says the same, in a form it accepts.
	var buf bytes.Buffer
	if err := col.WritePrometheus(&buf, collector.RenderOptions{}); err != nil {
		t.Fatal(err)
	}
	exp, err := promtexttest.Parse(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("invalid exposition: %v\n%s", err, buf.String())
	}
	f := exp.Family("bhatti_sandbox_cpu_usage_seconds_total")
	if f == nil {
		t.Fatalf("no cpu family:\n%s", buf.String())
	}
	if v, ok := f.Value("sandbox", loads["burn"].id, "owner", owner); !ok || v < 2 {
		t.Errorf("the burner's cpu_usage_seconds_total = %v, %v; want the CPU time it has used", v, ok)
	}
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(line, "bhatti_sandbox_cpu_usage_seconds_total{") || strings.HasPrefix(line, "bhatti_sandbox_memory_current_bytes{") || strings.HasPrefix(line, "bhatti_owner_") && strings.Contains(line, "cpu_usage") {
			t.Log(line)
		}
	}
}

// A VMM that is killed and started again gets a new cgroup, counters from zero
// and a new pid: the sampler must report the new run's own rate and carry on,
// not a negative or an enormous one.
func TestSystemdCollectorAcrossAVMMRestart(t *testing.T) {
	m := systemdManager(t, false)
	key := fmt.Sprintf("collector-restart:%d", time.Now().UnixNano())
	owner := ownerSlug(key)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = m.sd.StopUnit(ctx, "bhatti-"+owner+".slice")
	})
	e := &Engine{vms: map[string]*VM{}, netds: map[string]*netdInstance{}, cg: m}
	const id = "00000000000000c1"
	ps, _ := vmPlacement(owner, id)
	start := func() *exec.Cmd {
		cmd, marker := workHelper(t, m, ps, "while :; do :; done")
		waitFor(t, "the burner to start", func() bool { return exists(marker) })
		e.vms[id] = &VM{ID: id, Status: "running", HelperPID: cmd.Process.Pid, netdKey: key, scope: placedScope{unit: ps.unit}}
		return cmd
	}
	old := start()
	col := collector.New(collector.Config{Source: e, Interval: time.Second})
	ctx := context.Background()
	col.Sample(ctx)
	baseline := workHelperCPU(t, ps)
	waitFor(t, "the old VMM to use two CPU seconds", func() bool {
		return workHelperCPU(t, ps) >= baseline+2_000_000
	})
	before, ok := col.Sample(ctx).Sandbox(id)
	if !ok || before.Rates == nil || before.Rates.CPUPercent <= 0 {
		t.Fatalf("before the restart: %+v", before)
	}

	// The VMM is killed, and once its scope is gone started again in a new one.
	_ = syscall.Kill(-old.Process.Pid, syscall.SIGKILL)
	_, _ = old.Process.Wait()
	waitFor(t, "the old scope to go", func() bool { return !exists(cgroupDir(t, ps.path())) })
	start()
	waitFor(t, "the new VMM to use a CPU second", func() bool {
		return workHelperCPU(t, ps) >= 1_000_000
	})
	after, ok := col.Sample(ctx).Sandbox(id)
	if !ok || after.Rates == nil {
		t.Fatalf("after the restart: %+v", after)
	}
	t.Logf("before: pid %d, %.1f%% of a CPU, %d µs counted; after the restart: pid %d, %.1f%%, %d µs counted (a counter that went %s)",
		before.PID, before.Rates.CPUPercent, before.CPU.UsageUsec, after.PID, after.Rates.CPUPercent, after.CPU.UsageUsec,
		map[bool]string{true: "down", false: "up"}[after.CPU.UsageUsec < before.CPU.UsageUsec])
	if after.PID == before.PID || after.Rates.CPUPercent <= 0 || after.Rates.CPUPercent > 130 {
		t.Errorf("rates across the restart = %+v (pid %d → %d): want the new burner's own positive rate", after.Rates, before.PID, after.PID)
	}
	if n := len(col.Series(id)); n != 2 {
		t.Errorf("%d history points, want one per completed interval: 2", n)
	}
}
