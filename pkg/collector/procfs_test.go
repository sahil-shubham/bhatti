package collector

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseProcStatFixture(t *testing.T) {
	cpu, cpus, err := parseProcStat(fixture(t, "proc/stat"))
	if err != nil {
		t.Fatal(err)
	}
	// cpu  832286 0 372699 125816990 44697 0 75697 0 0 0, in 10 ms ticks.
	want := HostCPU{User: 8322.86, Nice: 0, System: 3726.99, Idle: 1258169.90, IOWait: 446.97, IRQ: 0, SoftIRQ: 756.97, Steal: 0}
	if cpu != want {
		t.Errorf("cpu = %+v, want %+v", cpu, want)
	}
	if cpus != 4 {
		t.Errorf("cpus = %d, want the 4 cpuN lines of the file", cpus)
	}
}

func TestParseProcStatRejectsWhatItCannotRead(t *testing.T) {
	for name, in := range map[string]string{
		"no cpu line":    "intr 1 2 3\nctxt 5\n",
		"a short line":   "cpu  1 2 3\n",
		"not a number":   "cpu  1 2 x 4 5 6 7 8\n",
		"only per-cpu":   "cpu0 1 2 3 4 5 6 7 8\n",
		"empty":          "",
		"negative ticks": "cpu  -1 0 0 0 0 0 0 0\n",
	} {
		if _, _, err := parseProcStat([]byte(in)); err == nil {
			t.Errorf("%s: accepted %q", name, in)
		}
	}
	// Synthetic: guest and guest_nice columns, and the extra ones a newer
	// kernel adds, are not read: guest time is already in user time.
	cpu, cpus, err := parseProcStat([]byte("cpu  100 0 100 800 0 0 0 0 55 0 7\ncpu0 100 0 100 800 0 0 0 0 55 0 7\n"))
	if err != nil || cpus != 1 || cpu.User != 1 || cpu.Idle != 8 {
		t.Errorf("guest columns: %+v over %d cpus, %v", cpu, cpus, err)
	}
}

func TestParseMeminfoFixture(t *testing.T) {
	m, err := parseMeminfo(fixture(t, "proc/meminfo"))
	if err != nil {
		t.Fatal(err)
	}
	want := HostMemory{TotalBytes: 4012892 << 10, AvailableBytes: 3090844 << 10, SwapTotalBytes: 2097148 << 10, SwapFreeBytes: 2094820 << 10}
	if m != want {
		t.Errorf("meminfo = %+v, want %+v", m, want)
	}
}

func TestParseMeminfoRejectsWhatItCannotRead(t *testing.T) {
	const ok = "MemTotal: 10 kB\nMemAvailable: 5 kB\nSwapTotal: 4 kB\nSwapFree: 3 kB\n"
	if m, err := parseMeminfo([]byte(ok)); err != nil || m.AvailableBytes != 5<<10 || m.SwapFreeBytes != 3<<10 {
		t.Fatalf("baseline: %+v, %v", m, err)
	}
	for name, in := range map[string]string{
		// MemAvailable came in Linux 3.14; without it there is no honest answer.
		"no MemAvailable": "MemTotal: 10 kB\nSwapTotal: 4 kB\nSwapFree: 3 kB\n",
		"no swap lines":   "MemTotal: 10 kB\nMemAvailable: 5 kB\n",
		"not a number":    strings.Replace(ok, "MemTotal: 10", "MemTotal: ten", 1),
		"another unit":    strings.Replace(ok, "MemTotal: 10 kB", "MemTotal: 10 MB", 1),
		"no unit":         strings.Replace(ok, "MemTotal: 10 kB", "MemTotal: 10", 1),
		"overflow":        strings.Replace(ok, "MemTotal: 10 kB", "MemTotal: 18446744073709551615 kB", 1),
		"empty":           "",
	} {
		if _, err := parseMeminfo([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestReadProcOfARealHost(t *testing.T) {
	h := hostReader{rd: &usageReader{}, procRoot: filepath.Join("testdata", "linux", "proc"), dataDir: t.TempDir()}
	host, err := h.readProc()
	if err != nil {
		t.Fatal(err)
	}
	if host.CPUs != 4 || host.CPU == nil || host.CPU.Idle != 1258169.90 {
		t.Errorf("cpu: %d cpus, %+v", host.CPUs, host.CPU)
	}
	if host.Memory == nil || host.Memory.TotalBytes != 4012892<<10 {
		t.Errorf("memory = %+v", host.Memory)
	}
	if p := host.Pressure; p == nil || p.CPU == nil || p.Memory == nil || p.IO == nil || p.IO.SomeUsec != 148132277 || p.IO.FullUsec != 132556407 {
		t.Errorf("pressure = %+v", host.Pressure)
	}
	if d := host.Disk; d == nil || d.TotalBytes == 0 || d.FreeBytes > d.TotalBytes {
		t.Errorf("disk = %+v", host.Disk)
	}
}

func TestReadProcWithoutPressureStallInformation(t *testing.T) {
	// Synthetic: a host booted without PSI has no /proc/pressure. That is no
	// error and no pressure.
	root := t.TempDir()
	for _, f := range []string{"stat", "meminfo"} {
		if err := os.WriteFile(filepath.Join(root, f), fixture(t, "proc/"+f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h := hostReader{rd: &usageReader{}, procRoot: root}
	host, err := h.readProc()
	if err != nil {
		t.Fatalf("no PSI is no error: %v", err)
	}
	if host.Pressure != nil || host.CPU == nil || host.Memory == nil || host.Disk != nil {
		t.Errorf("host = %+v", host)
	}
}

func TestReadProcReportsWhatItCouldNotRead(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "stat"), []byte("garbage\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// meminfo is missing altogether.
	h := hostReader{rd: &usageReader{}, procRoot: root}
	host, err := h.readProc()
	if err == nil {
		t.Fatal("two unreadable files raised no error")
	}
	for _, want := range []string{"/proc/stat", "/proc/meminfo"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q doesn't name %s", err, want)
		}
	}
	if host.CPU != nil || host.Memory != nil {
		t.Errorf("figures from files that failed: %+v", host)
	}
}

func TestHostCPUPercent(t *testing.T) {
	prev := &HostCPU{User: 100, System: 50, Idle: 800, IOWait: 50}
	cur := &HostCPU{User: 130, System: 60, Idle: 860, IOWait: 50, Steal: 0}
	// 100 s passed (30+10+60+0); 60 of them idle.
	if got, ok := hostCPUPercent(prev, cur); !ok || got != 40 {
		t.Errorf("busy = %v, %v; want 40", got, ok)
	}
	// Time spent waiting for I/O isn't busy.
	cur = &HostCPU{User: 100, System: 50, Idle: 800, IOWait: 150}
	if got, ok := hostCPUPercent(prev, cur); !ok || got != 0 {
		t.Errorf("all iowait = %v, %v; want 0", got, ok)
	}
	// Nothing passed, or a counter went back (a reboot between readings): no answer.
	if _, ok := hostCPUPercent(prev, prev); ok {
		t.Error("no time passed yet there is an answer")
	}
	if _, ok := hostCPUPercent(cur, prev); ok {
		t.Error("counters that went down gave an answer")
	}
}
