package collector

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/promtext"
	"github.com/sahil-shubham/bhatti/pkg/promtext/promtexttest"
)

// exposition renders what the collector has and runs it through the strict
// parser: whatever is asserted below is asserted of text a scraper accepts.
func exposition(t *testing.T, s *Snapshot, opts RenderOptions) (*promtexttest.Exposition, string) {
	t.Helper()
	var buf bytes.Buffer
	if err := s.WritePrometheus(&buf, opts); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	e, err := promtexttest.Parse(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("the exposition is not valid: %v\n%s", err, buf.String())
	}
	return e, buf.String()
}

func mustValue(t *testing.T, e *promtexttest.Exposition, family string, labels ...string) float64 {
	t.Helper()
	f := e.Family(family)
	if f == nil {
		t.Fatalf("no family %s", family)
	}
	v, ok := f.Value(labels...)
	if !ok {
		t.Fatalf("%s has no series %v; has %+v", family, labels, f.Samples)
	}
	return v
}

func TestPrometheusTextOfARealBusyScope(t *testing.T) {
	r := busyRig(t, nil)
	snap := r.sample()
	e, text := exposition(t, snap, RenderOptions{})
	sb := []string{"sandbox", sbA, "owner", ownerA}

	// Counters are in seconds and bytes, as the kernel gave them.
	if got := mustValue(t, e, "bhatti_sandbox_cpu_usage_seconds_total", sb...); got != 7.08086 {
		t.Errorf("cpu usage = %v, want 7.08086", got)
	}
	if got := mustValue(t, e, "bhatti_sandbox_cpu_user_seconds_total", sb...); got != 6.198073 {
		t.Errorf("cpu user = %v", got)
	}
	if got := mustValue(t, e, "bhatti_sandbox_cpu_system_seconds_total", sb...); got != 0.882787 {
		t.Errorf("cpu system = %v", got)
	}
	for name, want := range map[string]float64{
		"bhatti_sandbox_memory_current_bytes":    103555072,
		"bhatti_sandbox_memory_peak_bytes":       104243200,
		"bhatti_sandbox_memory_anon_bytes":       102584320,
		"bhatti_sandbox_memory_file_bytes":       0, // a reading of zero is still a series
		"bhatti_sandbox_memory_oom_events_total": 0, "bhatti_sandbox_memory_oom_kills_total": 0,
		"bhatti_sandbox_io_read_bytes_total":                   1651507200,
		"bhatti_sandbox_io_write_bytes_total":                  1677721600,
		"bhatti_sandbox_io_read_ops_total":                     1895,
		"bhatti_sandbox_io_write_ops_total":                    1984,
		"bhatti_sandbox_pressure_cpu_waiting_seconds_total":    0.015822,
		"bhatti_sandbox_pressure_cpu_stalled_seconds_total":    0.015413,
		"bhatti_sandbox_pressure_memory_waiting_seconds_total": 0,
		"bhatti_sandbox_pressure_memory_stalled_seconds_total": 0,
		"bhatti_sandbox_pressure_io_waiting_seconds_total":     1.429687,
		"bhatti_sandbox_pressure_io_stalled_seconds_total":     1.290871,
	} {
		if got := mustValue(t, e, name, sb...); got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	if got := mustValue(t, e, "bhatti_sandbox_info", "sandbox", sbA, "owner", ownerA, "accounting", "cgroup"); got != 1 {
		t.Errorf("info = %v", got)
	}

	// The owner and its gateway, by owner alone.
	if got := mustValue(t, e, "bhatti_owner_cpu_usage_seconds_total", "owner", ownerA); got != 7.378371 {
		t.Errorf("owner cpu = %v", got)
	}
	if got := mustValue(t, e, "bhatti_owner_memory_oom_kills_total", "owner", ownerA); got != 1 {
		t.Errorf("owner oom kills = %v, want the 1 the slice counted", got)
	}
	if got := mustValue(t, e, "bhatti_owner_memory_file_bytes", "owner", ownerA); got != 33624064 {
		t.Errorf("owner file memory = %v", got)
	}
	if e.Family("bhatti_netd_cpu_usage_seconds_total") == nil || e.Family("bhatti_netd_memory_current_bytes") == nil {
		t.Error("the gateway's own use is missing")
	}

	// Per guest, from the gateway.
	for name, want := range map[string]float64{
		"bhatti_netd_rx_bytes_total": 5000, "bhatti_netd_rx_packets_total": 50,
		"bhatti_netd_tx_bytes_total": 3000, "bhatti_netd_tx_packets_total": 30,
	} {
		if got := mustValue(t, e, name, "owner", ownerA, "sandbox", sbA); got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	for reason, want := range map[string]float64{"policy": 2, "spoof": 1, "hard_deny": 3} {
		if got := mustValue(t, e, "bhatti_netd_dropped_frames_total", "owner", ownerA, "sandbox", sbA, "reason", reason); got != want {
			t.Errorf("dropped %s = %v, want %v", reason, got, want)
		}
	}
	if f := e.Family("bhatti_netd_dropped_frames_total"); f == nil || len(f.Samples) != 3 {
		t.Errorf("dropped frames has %+v: one series per reason", f)
	}
	if strings.Contains(text, "100.64.0.99") || strings.Contains(text, "gone") {
		t.Errorf("a guest nobody here owns is in the text:\n%s", text)
	}

	// The host.
	if got := mustValue(t, e, "bhatti_host_cpus"); got != 4 {
		t.Errorf("host cpus = %v", got)
	}
	if got := mustValue(t, e, "bhatti_host_cpu_seconds_total", "mode", "idle"); got != 1258169.9 {
		t.Errorf("host idle = %v", got)
	}
	for name, want := range map[string]float64{
		"bhatti_host_memory_total_bytes":                    4012892 << 10,
		"bhatti_host_memory_available_bytes":                3090844 << 10,
		"bhatti_host_swap_total_bytes":                      2097148 << 10,
		"bhatti_host_swap_free_bytes":                       2094820 << 10,
		"bhatti_host_pressure_io_waiting_seconds_total":     148.132277,
		"bhatti_host_pressure_io_stalled_seconds_total":     132.556407,
		"bhatti_host_pressure_memory_waiting_seconds_total": 4.87546,
	} {
		if got := mustValue(t, e, name); got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	if f := e.Family("bhatti_host_disk_free_bytes"); f == nil || len(f.Samples) != 1 || f.Samples[0].Labels["path"] == "" {
		t.Errorf("disk free = %+v", f)
	}
	if e.Family("bhatti_host_memory_pressure_level") != nil {
		t.Error("a macOS-only family on a host with pressure stall information")
	}
	if e.Family("bhatti_host_pressure_cpu_stalled_seconds_total") != nil {
		t.Error("the host's cpu full pressure, which the kernel keeps at zero, is rendered")
	}
	if got := mustValue(t, e, "bhatti_collector_last_sample_timestamp_seconds"); got != float64(t0.Unix()) {
		t.Errorf("sample timestamp = %v", got)
	}
	if e.Family("bhatti_collector_sample_duration_seconds") == nil {
		t.Error("no sample duration")
	}
}

func TestPrometheusTextIsTheSameEveryTime(t *testing.T) {
	r := busyRig(t, nil)
	tg := oneVM(100)
	tg.Sandboxes = append(tg.Sandboxes, engine.StatsSandbox{ID: "0000000000000000", Owner: ownerA, PID: 101, Accounting: engine.AccountingCgroup, Cgroup: scopePath(ownerA, "0000000000000000")})
	r.tree.put(scopePath(ownerA, "0000000000000000"), "scope-idle")
	r.eng.set(tg)
	snap := r.sample()
	var a, b bytes.Buffer
	if err := snap.WritePrometheus(&a, RenderOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := snap.WritePrometheus(&b, RenderOptions{}); err != nil {
		t.Fatal(err)
	}
	if a.String() != b.String() {
		t.Error("two renderings of one snapshot differ")
	}
	// Sorted by sandbox id, so a scrape diffs cleanly.
	if i, j := strings.Index(a.String(), `sandbox="0000000000000000"`), strings.Index(a.String(), `sandbox="`+sbA+`"`); i < 0 || j < 0 || i > j {
		t.Errorf("sandboxes aren't in id order (%d, %d)", i, j)
	}
}

func TestPrometheusLeavesOutWhatIsNotThere(t *testing.T) {
	// A kernel without memory.peak or PSI, with the io controller off: its
	// sandboxes have no such series, and no zeros standing in for them.
	r := newRig(t, nil)
	r.tree.put(scopePath(ownerA, sbA), "scope-busy")
	for _, f := range []string{"memory.peak", "io.stat", "cpu.pressure", "memory.pressure", "io.pressure", "memory.events"} {
		r.tree.remove(scopePath(ownerA, sbA) + "/" + f)
	}
	tg := oneVM(100)
	tg.Owners, tg.Netds = nil, nil
	r.eng.set(tg)
	e, text := exposition(t, r.sample(), RenderOptions{})
	if e.Family("bhatti_sandbox_cpu_usage_seconds_total") == nil || e.Family("bhatti_sandbox_memory_current_bytes") == nil || e.Family("bhatti_sandbox_memory_anon_bytes") == nil {
		t.Fatalf("what is there is missing:\n%s", text)
	}
	for _, name := range []string{
		"bhatti_sandbox_memory_peak_bytes", "bhatti_sandbox_io_read_bytes_total", "bhatti_sandbox_io_write_ops_total",
		"bhatti_sandbox_pressure_io_waiting_seconds_total", "bhatti_sandbox_pressure_cpu_waiting_seconds_total",
		"bhatti_sandbox_memory_oom_kills_total", "bhatti_owner_cpu_usage_seconds_total", "bhatti_netd_cpu_usage_seconds_total",
	} {
		if e.Family(name) != nil {
			t.Errorf("%s is rendered although nothing was read for it", name)
		}
	}
}

func TestPrometheusKernelWithoutAFullLineForCPUPressure(t *testing.T) {
	// Synthetic: before Linux 5.13 cpu.pressure has only "some".
	r := newRig(t, nil)
	r.tree.put(scopePath(ownerA, sbA), "scope-busy")
	r.tree.write(scopePath(ownerA, sbA), "cpu.pressure", "some avg10=0.00 avg60=0.00 avg300=0.00 total=15822\n")
	tg := oneVM(100)
	tg.Owners, tg.Netds = nil, nil
	r.eng.set(tg)
	e, _ := exposition(t, r.sample(), RenderOptions{})
	if got := mustValue(t, e, "bhatti_sandbox_pressure_cpu_waiting_seconds_total", "sandbox", sbA, "owner", ownerA); got != 0.015822 {
		t.Errorf("cpu waiting = %v", got)
	}
	if e.Family("bhatti_sandbox_pressure_cpu_stalled_seconds_total") != nil {
		t.Error("a stalled series made up from a file with no full line")
	}
	if e.Family("bhatti_sandbox_pressure_memory_stalled_seconds_total") == nil {
		t.Error("memory's full line, which is there, is missing")
	}
}

func TestPrometheusProcessAccountingHasOnlyWhatAProcessHas(t *testing.T) {
	r := newRig(t, nil)
	r.c.procUsage = func(pid int) (Usage, error) {
		return Usage{
			CPU:    &CPU{UsageUsec: 2_500_000, UserUsec: 2_000_000, SystemUsec: 500_000},
			Memory: &Memory{Current: 600 << 20},
			IO:     &IO{ReadBytes: 100, WriteBytes: 200},
		}, nil
	}
	r.eng.set(engine.StatsTargets{Sandboxes: []engine.StatsSandbox{{ID: sbA, Owner: ownerA, PID: 7, Accounting: engine.AccountingProcess}}})
	e, text := exposition(t, r.sample(), RenderOptions{})
	sb := []string{"sandbox", sbA, "owner", ownerA}
	if got := mustValue(t, e, "bhatti_sandbox_cpu_usage_seconds_total", sb...); got != 2.5 {
		t.Errorf("cpu = %v", got)
	}
	if got := mustValue(t, e, "bhatti_sandbox_memory_current_bytes", sb...); got != 600<<20 {
		t.Errorf("memory = %v", got)
	}
	if got := mustValue(t, e, "bhatti_sandbox_io_write_bytes_total", sb...); got != 200 {
		t.Errorf("write bytes = %v", got)
	}
	if got := mustValue(t, e, "bhatti_sandbox_info", "sandbox", sbA, "owner", ownerA, "accounting", "process"); got != 1 {
		t.Errorf("info = %v", got)
	}
	for _, name := range []string{
		"bhatti_sandbox_memory_peak_bytes", "bhatti_sandbox_memory_anon_bytes", "bhatti_sandbox_memory_file_bytes",
		"bhatti_sandbox_io_read_ops_total", "bhatti_sandbox_io_write_ops_total",
		"bhatti_sandbox_pressure_io_waiting_seconds_total", "bhatti_sandbox_memory_oom_kills_total",
	} {
		if e.Family(name) != nil {
			t.Errorf("%s: a process has no such figure\n%s", name, text)
		}
	}
}

func TestPrometheusLabelsAreEscapedAndRelabelled(t *testing.T) {
	r := busyRig(t, nil)
	snap := r.sample()
	nasty := "a\"b\\c\nd"
	e, text := exposition(t, snap, RenderOptions{SandboxLabel: func(id string) string {
		if id != sbA {
			t.Errorf("relabelled %q", id)
		}
		return nasty
	}})
	if got := mustValue(t, e, "bhatti_sandbox_cpu_usage_seconds_total", "sandbox", nasty, "owner", ownerA); got != 7.08086 {
		t.Errorf("series under the relabelled sandbox = %v", got)
	}
	if got := mustValue(t, e, "bhatti_netd_rx_bytes_total", "owner", ownerA, "sandbox", nasty); got != 5000 {
		t.Errorf("guest series under the relabelled sandbox = %v", got)
	}
	if strings.Contains(text, sbA) {
		t.Errorf("the engine's id leaked into relabelled text:\n%s", text)
	}
	if !strings.Contains(text, `sandbox="a\"b\\c\nd"`) {
		t.Errorf("label not escaped as the format wants:\n%s", text)
	}
	// Invalid UTF-8 in a label is repaired by the writer, not passed on.
	e, _ = exposition(t, snap, RenderOptions{SandboxLabel: func(string) string { return "x\xffy" }})
	if got := mustValue(t, e, "bhatti_sandbox_cpu_usage_seconds_total", "sandbox", "x\uFFFDy", "owner", ownerA); got != 7.08086 {
		t.Errorf("series under a repaired label = %v", got)
	}
}

func TestPrometheusOfNothingSampledIsEmptyAndValid(t *testing.T) {
	c := New(Config{})
	var buf bytes.Buffer
	if err := c.WritePrometheus(&buf, RenderOptions{}); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Errorf("text before the first sample: %q", buf.String())
	}
	if _, err := promtexttest.Parse(&buf); err != nil {
		t.Errorf("an empty exposition is rejected: %v", err)
	}
}

func TestPrometheusOfAHostAlone(t *testing.T) {
	r := newRig(t, nil)
	e, _ := exposition(t, r.sample(), RenderOptions{})
	for _, name := range []string{"bhatti_host_cpus", "bhatti_host_memory_total_bytes", "bhatti_collector_last_sample_timestamp_seconds"} {
		if e.Family(name) == nil {
			t.Errorf("%s is missing from a host-only exposition", name)
		}
	}
	for _, f := range e.Families {
		if strings.HasPrefix(f.Name, "bhatti_sandbox_") || strings.HasPrefix(f.Name, "bhatti_owner_") || strings.HasPrefix(f.Name, "bhatti_netd_") {
			t.Errorf("%s from a host with no sandboxes", f.Name)
		}
	}
}

func TestPrometheusFamiliesShareAnExpositionWithOthers(t *testing.T) {
	// The server will add its own families to the same exposition: neither may
	// start a family the other has, and one flush covers both.
	r := busyRig(t, nil)
	snap := r.sample()
	var buf bytes.Buffer
	pw := promtext.New(&buf)
	snap.WriteTo(pw, RenderOptions{})
	pw.Family("bhatti_api_requests_total", "Requests served.", promtext.Counter)
	pw.Sample("bhatti_api_requests_total", 12, "code", "200")
	if err := pw.Flush(); err != nil {
		t.Fatalf("the families clash: %v", err)
	}
	e, err := promtexttest.Parse(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if e.Family("bhatti_sandbox_cpu_usage_seconds_total") == nil || e.Family("bhatti_api_requests_total") == nil {
		t.Error("one side's families are missing")
	}
}

func TestPrometheusMacOSMemoryPressureLevel(t *testing.T) {
	// Synthetic: the host figures a Mac reports, rendered from a snapshot as
	// they would be on one.
	snap := &Snapshot{At: t0, Duration: time.Millisecond, Host: &Host{
		At: t0, CPUs: 10,
		Memory: &HostMemory{TotalBytes: 24 << 30, AvailableBytes: 16 << 30, SwapTotalBytes: 8 << 30, SwapFreeBytes: 1 << 30, PressureLevel: "warn"},
	}}
	e, _ := exposition(t, snap, RenderOptions{})
	if got := mustValue(t, e, "bhatti_host_memory_pressure_level"); got != 1 {
		t.Errorf("pressure level = %v, want 1 for warn", got)
	}
	if e.Family("bhatti_host_cpu_seconds_total") != nil || e.Family("bhatti_host_pressure_io_waiting_seconds_total") != nil {
		t.Error("Linux figures from a Mac")
	}
	for level, want := range map[string]float64{"normal": 0, "critical": 2} {
		snap.Host.Memory.PressureLevel = level
		e, _ := exposition(t, snap, RenderOptions{})
		if got := mustValue(t, e, "bhatti_host_memory_pressure_level"); got != want {
			t.Errorf("level %s = %v, want %v", level, got, want)
		}
	}
	snap.Host.Memory.PressureLevel = ""
	if e, _ := exposition(t, snap, RenderOptions{}); e.Family("bhatti_host_memory_pressure_level") != nil {
		t.Error("a pressure level for a host that doesn't report one")
	}
}

func TestSnapshotJSONNamesWhatAnAPIWillServe(t *testing.T) {
	r := busyRig(t, nil)
	r.sample()
	r.clock.advance(10 * time.Second)
	snap := r.sample()
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, want := range []string{`"accounting":"cgroup"`, `"cpu_percent"`, `"usage_usec":7080860`, `"oom_kill"`, `"rx_bytes":5000`, `"dropped_hard_deny":3`} {
		if !strings.Contains(text, want) {
			t.Errorf("the JSON of a snapshot lacks %s:\n%s", want, text)
		}
	}
	// The flags that say what was read are for the renderer, not the API.
	if strings.Contains(text, "HasPeak") || strings.Contains(text, "HasStat") || strings.Contains(text, "HasOps") || strings.Contains(text, "HasFull") {
		t.Errorf("internal flags in the JSON:\n%s", text)
	}
}
