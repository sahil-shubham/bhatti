package collector

import (
	"math"
	"strconv"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

func near(got, want, tolerance float64) bool { return math.Abs(got-want) <= tolerance }

func cpuUsage(usec uint64) Usage { return Usage{CPU: &CPU{UsageUsec: usec}} }

func TestRatesFromTwoRealSamples(t *testing.T) {
	// The same busy scope read twice, ELAPSED seconds apart, by a real kernel.
	read := func(dir string) Usage {
		var r usageReader
		u, err := r.usage(fixtureDir(t, "cgroup/"+dir))
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	elapsed, err := strconv.ParseFloat(strings.TrimSpace(string(fixture(t, "cgroup/scope-busy-later/ELAPSED"))), 64)
	if err != nil {
		t.Fatal(err)
	}
	prev := &sample{at: t0, ident: incarnation{pid: 7, cgroup: "/c"}, usage: read("scope-busy")}
	cur := &sample{at: t0.Add(time.Duration(elapsed * float64(time.Second))), ident: prev.ident, usage: read("scope-busy-later")}
	r, ok := deriveRates(prev, cur)
	if !ok {
		t.Fatal("no rates")
	}
	if !near(r.Seconds, elapsed, 1e-6) {
		t.Errorf("seconds = %v, want %v", r.Seconds, elapsed)
	}
	// 6.115743 s of CPU in 5.039389 s: a bit over one CPU busy.
	if want := (13196603.0 - 7080860.0) / 1e6 / elapsed * 100; !near(r.CPUPercent, want, 1e-6) || r.CPUPercent < 100 || r.CPUPercent > 130 {
		t.Errorf("cpu = %.3f%%, want %.3f%%", r.CPUPercent, want)
	}
	if want := (3723493376.0 - 1677721600.0) / elapsed; !near(r.IOWriteBytesPerSec, want, 1) {
		t.Errorf("write = %.0f B/s, want %.0f", r.IOWriteBytesPerSec, want)
	}
	if want := (3690987520.0 - 1651507200.0) / elapsed; !near(r.IOReadBytesPerSec, want, 1) {
		t.Errorf("read = %.0f B/s, want %.0f", r.IOReadBytesPerSec, want)
	}
	// io.pressure some: 2480200 - 1429687 µs stalled.
	if want := (2480200.0 - 1429687.0) / 1e6 / elapsed * 100; !near(r.IOSomePercent, want, 1e-6) {
		t.Errorf("io some = %.3f%%, want %.3f%%", r.IOSomePercent, want)
	}
	if r.MemSomePercent != 0 || r.MemFullPercent != 0 {
		t.Errorf("memory pressure moved by itself: %+v", r)
	}
}

func TestRatesAcrossAVMMRestart(t *testing.T) {
	same := incarnation{pid: 10, cgroup: "/bhatti.slice/a.scope"}
	restarted := incarnation{pid: 11, cgroup: "/bhatti.slice/a.scope"}
	for name, tc := range map[string]struct {
		prevCPU, curCPU uint64
		prevID, curID   incarnation
		want            float64 // percent, over 10 s
	}{
		"steady":                          {100e6, 105e6, same, same, 50},
		"counter went down under one pid": {100e6, 2e6, same, same, 20},
		"new process, counter higher":     {1e6, 8e6, same, restarted, 80},
		"new process, counter lower":      {100e6, 2e6, same, restarted, 20},
		"new process, nothing used yet":   {100e6, 0, same, restarted, 0},
		"counter unchanged":               {100e6, 100e6, same, same, 0},
		"same pid in another cgroup":      {50e6, 3e6, same, incarnation{pid: 10, cgroup: "/bhatti.slice/b.scope"}, 30},
		"one CPU fully busy":              {50e6, 60e6, same, same, 100},
	} {
		prev := &sample{at: t0, ident: tc.prevID, usage: cpuUsage(tc.prevCPU)}
		cur := &sample{at: t0.Add(10 * time.Second), ident: tc.curID, usage: cpuUsage(tc.curCPU)}
		r, ok := deriveRates(prev, cur)
		if !ok || r.CPUPercent < 0 || !near(r.CPUPercent, tc.want, 1e-9) {
			t.Errorf("%s: cpu = %v%%, %v; want %v%%", name, r.CPUPercent, ok, tc.want)
		}
	}
}

func TestRatesAcrossAResetOfOnlyOneCounter(t *testing.T) {
	// One field going down is that field's reset; the others keep their
	// differences.
	prev := &sample{at: t0, usage: Usage{
		CPU: &CPU{UsageUsec: 10e6},
		IO:  &IO{ReadBytes: 5000, WriteBytes: 9000},
	}}
	cur := &sample{at: t0.Add(10 * time.Second), usage: Usage{
		CPU: &CPU{UsageUsec: 20e6},
		IO:  &IO{ReadBytes: 100, WriteBytes: 19000},
	}}
	r, _ := deriveRates(prev, cur)
	if r.CPUPercent != 100 {
		t.Errorf("cpu = %v", r.CPUPercent)
	}
	if r.IOReadBytesPerSec != 10 { // 100 bytes since the reset, in 10 s
		t.Errorf("read = %v B/s, want 10 (the reset's)", r.IOReadBytesPerSec)
	}
	if r.IOWriteBytesPerSec != 1000 {
		t.Errorf("write = %v B/s, want 1000", r.IOWriteBytesPerSec)
	}
}

func TestRatesNeedAnInterval(t *testing.T) {
	a := &sample{at: t0, usage: cpuUsage(1)}
	for name, b := range map[string]*sample{
		"no time passed":  {at: t0, usage: cpuUsage(2)},
		"clock went back": {at: t0.Add(-time.Second), usage: cpuUsage(2)},
	} {
		if r, ok := deriveRates(a, b); ok || r != (Rates{}) {
			t.Errorf("%s: rates %+v, %v", name, r, ok)
		}
	}
}

func TestRatesLeaveOutWhatEitherEndLacks(t *testing.T) {
	full := Usage{CPU: &CPU{UsageUsec: 5e6}, IO: &IO{ReadBytes: 10}, Pressure: &Pressure{Memory: &PSI{SomeUsec: 5e6, HasFull: true}}}
	prev := &sample{at: t0, usage: Usage{}}
	cur := &sample{at: t0.Add(10 * time.Second), usage: full}
	r, ok := deriveRates(prev, cur)
	if !ok || r.CPUPercent != 0 || r.IOReadBytesPerSec != 0 || r.MemSomePercent != 0 || r.NetRxBytesPerSec != 0 {
		t.Errorf("rates from a sample with nothing in it: %+v", r)
	}
	// Pressure whose kernel reports no "full" for cpu: that rate stays zero
	// rather than being made up from "some".
	p := &sample{at: t0, usage: Usage{Pressure: &Pressure{CPU: &PSI{SomeUsec: 0}}}}
	c := &sample{at: t0.Add(10 * time.Second), usage: Usage{Pressure: &Pressure{CPU: &PSI{SomeUsec: 2e6}}}}
	if r, _ := deriveRates(p, c); r.CPUSomePercent != 20 {
		t.Errorf("cpu some = %v, want 20", r.CPUSomePercent)
	}
}

func TestPressureRatesAreSharesOfTheIntervalAndNeverMore(t *testing.T) {
	prev := &sample{at: t0, usage: Usage{Pressure: &Pressure{
		Memory: &PSI{SomeUsec: 1e6, FullUsec: 0, HasFull: true},
		IO:     &PSI{SomeUsec: 0, HasFull: true},
	}}}
	cur := &sample{at: t0.Add(10 * time.Second), usage: Usage{Pressure: &Pressure{
		Memory: &PSI{SomeUsec: 4e6, FullUsec: 1e6, HasFull: true},
		// More stall time than the interval has: two clocks disagree by a little.
		IO: &PSI{SomeUsec: 10_030_000, FullUsec: 9e6, HasFull: true},
	}}}
	r, _ := deriveRates(prev, cur)
	if !near(r.MemSomePercent, 30, 1e-9) || !near(r.MemFullPercent, 10, 1e-9) {
		t.Errorf("memory: some %v full %v; want 30 and 10", r.MemSomePercent, r.MemFullPercent)
	}
	if r.IOSomePercent != 100 || !near(r.IOFullPercent, 90, 1e-9) {
		t.Errorf("io: some %v full %v; want 100 (capped) and 90", r.IOSomePercent, r.IOFullPercent)
	}
}

func TestNetRates(t *testing.T) {
	prev := &sample{at: t0, ident: incarnation{pid: 1}, net: &Net{RxBytes: 1000, TxBytes: 500}}
	cur := &sample{at: t0.Add(5 * time.Second), ident: incarnation{pid: 1}, net: &Net{RxBytes: 6000, TxBytes: 500}}
	r, _ := deriveRates(prev, cur)
	if r.NetRxBytesPerSec != 1000 || r.NetTxBytesPerSec != 0 {
		t.Errorf("net = rx %v tx %v B/s; want 1000 and 0", r.NetRxBytesPerSec, r.NetTxBytesPerSec)
	}
	// A guest the gateway forgot and was given again counts from zero.
	cur.net = &Net{RxBytes: 200, TxBytes: 100}
	r, _ = deriveRates(prev, cur)
	if r.NetRxBytesPerSec != 40 || r.NetTxBytesPerSec != 20 {
		t.Errorf("after a reset: rx %v tx %v B/s; want 40 and 20", r.NetRxBytesPerSec, r.NetTxBytesPerSec)
	}
	// Without the gateway's figures on either end, no rate.
	cur.net = nil
	if r, _ = deriveRates(prev, cur); r.NetRxBytesPerSec != 0 {
		t.Errorf("rate from nothing: %+v", r)
	}
}

func TestPointCarriesTheRatesAndMemory(t *testing.T) {
	usage := &Usage{Memory: &Memory{Current: 12345}}
	r := Rates{CPUPercent: 150.5, IOReadBytesPerSec: 10, IOWriteBytesPerSec: 20, CPUSomePercent: 1, MemSomePercent: 2, MemFullPercent: 3, IOSomePercent: 4, NetRxBytesPerSec: 5, NetTxBytesPerSec: 6}
	p := point(t0, usage, r)
	if p.At != t0 || p.MemoryBytes != 12345 || p.CPUPercent != 150.5 || p.IOReadBPS != 10 || p.IOWriteBPS != 20 ||
		p.CPUSome != 1 || p.MemSome != 2 || p.MemFull != 3 || p.IOSome != 4 || p.NetRxBPS != 5 || p.NetTxBPS != 6 {
		t.Errorf("point = %+v", p)
	}
	if p := point(t0, &Usage{}, Rates{}); p.MemoryBytes != 0 {
		t.Errorf("memory from a usage without any: %+v", p)
	}
}
