package collector

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// The fixtures under testdata/linux are real files, read out of a real systemd's
// cgroups by testdata/capture.sh (kernel and systemd versions are in
// testdata/linux/KERNEL). Tests that read a file by its exact bytes use them;
// the cases a recorded kernel doesn't have (older kernels, no PSI, a RAID
// stack) are written out in the tests below and say so.

func fixtureDir(t *testing.T, rel string) string {
	t.Helper()
	return filepath.Join("testdata", "linux", rel)
}

func fixture(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "linux", rel))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseCPUStatFixtures(t *testing.T) {
	for _, tc := range []struct {
		dir  string
		want CPU
	}{
		{"scope-busy", CPU{UsageUsec: 7080860, UserUsec: 6198073, SystemUsec: 882787}},
		{"scope-busy-later", CPU{UsageUsec: 13196603, UserUsec: 11436783, SystemUsec: 1759820}},
		{"scope-idle", CPU{UsageUsec: 1645, UserUsec: 822, SystemUsec: 822}},
		{"scope-oom", CPU{UsageUsec: 32459, UserUsec: 0, SystemUsec: 32459}},
		{"slice-owner", CPU{UsageUsec: 7378371, UserUsec: 6404900, SystemUsec: 973470}},
	} {
		got, err := parseCPUStat(fixture(t, "cgroup/"+tc.dir+"/cpu.stat"))
		if err != nil || got != tc.want {
			t.Errorf("%s: parseCPUStat = %+v, %v; want %+v", tc.dir, got, err, tc.want)
		}
		// The kernel rounds each of the three down from nanoseconds, so the sum of
		// the parts can fall a microsecond short of the whole, never further.
		if sum := got.UserUsec + got.SystemUsec; sum > got.UsageUsec || got.UsageUsec-sum > 1 {
			t.Errorf("%s: user %d + system %d vs usage %d", tc.dir, got.UserUsec, got.SystemUsec, got.UsageUsec)
		}
	}
}

// A kernel with the cpu controller off, or an older one, writes only the three
// core keys; one with core scheduling or throttling writes more. Neither moves
// what is read.
func TestParseCPUStatVariants(t *testing.T) {
	core := "usage_usec 10\nuser_usec 6\nsystem_usec 4\n"
	want := CPU{UsageUsec: 10, UserUsec: 6, SystemUsec: 4}
	for name, in := range map[string]string{
		"three core keys":   core,
		"extra keys":        core + "core_sched.force_idle_usec 0\nnr_periods 5\nnr_throttled 1\nthrottled_usec 99\n",
		"keys in any order": "system_usec 4\nuser_usec 6\nusage_usec 10\n",
		"trailing blank":    core + "\n",
		"no final newline":  strings.TrimSuffix(core, "\n"),
	} {
		if got, err := parseCPUStat([]byte(in)); err != nil || got != want {
			t.Errorf("%s: = %+v, %v; want %+v", name, got, err, want)
		}
	}
}

func TestParseKeyValueFilesRejectWhatTheyDontUnderstand(t *testing.T) {
	// A kernel that changed a format should be noticed, not read as zeros.
	for name, tc := range map[string]struct {
		parse func([]byte) error
		in    string
	}{
		"cpu.stat without usage_usec": {func(b []byte) error { _, err := parseCPUStat(b); return err }, "user_usec 1\nsystem_usec 2\n"},
		"cpu.stat empty":              {func(b []byte) error { _, err := parseCPUStat(b); return err }, ""},
		"cpu.stat a word":             {func(b []byte) error { _, err := parseCPUStat(b); return err }, "usage_usec lots\n"},
		"cpu.stat a negative":         {func(b []byte) error { _, err := parseCPUStat(b); return err }, "usage_usec -1\n"},
		"cpu.stat no value":           {func(b []byte) error { _, err := parseCPUStat(b); return err }, "usage_usec\n"},
		"cpu.stat overflow":           {func(b []byte) error { _, err := parseCPUStat(b); return err }, "usage_usec 18446744073709551616\n"},
		"memory.stat without file":    {func(b []byte) error { _, _, err := parseMemoryStat(b); return err }, "anon 5\n"},
		"memory.stat without anon":    {func(b []byte) error { _, _, err := parseMemoryStat(b); return err }, "file 5\n"},
		"memory.stat a word":          {func(b []byte) error { _, _, err := parseMemoryStat(b); return err }, "anon x\nfile 5\n"},
		"memory.events a word":        {func(b []byte) error { _, err := parseMemEvents(b); return err }, "oom_kill many\n"},
		"memory.current a word":       {func(b []byte) error { _, err := parseUint(b); return err }, "max\n"},
		"memory.current empty":        {func(b []byte) error { _, err := parseUint(b); return err }, "\n"},
	} {
		if err := tc.parse([]byte(tc.in)); err == nil {
			t.Errorf("%s: accepted %q", name, tc.in)
		}
	}
}

func TestAtouBounds(t *testing.T) {
	for in, want := range map[string]struct {
		v  uint64
		ok bool
	}{
		"0":                    {0, true},
		"7":                    {7, true},
		"18446744073709551615": {1<<64 - 1, true},
		"18446744073709551616": {0, false},
		"99999999999999999999": {0, false},
		"":                     {0, false},
		"-1":                   {0, false},
		"+1":                   {0, false},
		"1 ":                   {0, false},
		"0x10":                 {0, false},
	} {
		if v, ok := atou([]byte(in)); v != want.v || ok != want.ok {
			t.Errorf("atou(%q) = %d, %v; want %d, %v", in, v, ok, want.v, want.ok)
		}
	}
}

func TestParseMemoryFixtures(t *testing.T) {
	for _, tc := range []struct {
		dir        string
		cur, peak  uint64
		anon, file uint64
	}{
		{"scope-busy", 103555072, 104243200, 102584320, 0},
		{"scope-idle", 151552, 286720, 90112, 0},
		{"scope-oom", 331776, 33554432, 184320, 0},
		{"slice-owner", 139042816, 139739136, 103047168, 33624064},
	} {
		cur, err := parseUint(fixture(t, "cgroup/"+tc.dir+"/memory.current"))
		if err != nil || cur != tc.cur {
			t.Errorf("%s memory.current = %d, %v; want %d", tc.dir, cur, err, tc.cur)
		}
		peak, err := parseUint(fixture(t, "cgroup/"+tc.dir+"/memory.peak"))
		if err != nil || peak != tc.peak {
			t.Errorf("%s memory.peak = %d, %v; want %d", tc.dir, peak, err, tc.peak)
		}
		anon, file, err := parseMemoryStat(fixture(t, "cgroup/"+tc.dir+"/memory.stat"))
		if err != nil || anon != tc.anon || file != tc.file {
			t.Errorf("%s memory.stat = anon %d file %d, %v; want %d %d", tc.dir, anon, file, err, tc.anon, tc.file)
		}
		if peak < cur {
			t.Errorf("%s: peak %d below current %d", tc.dir, peak, cur)
		}
	}
}

func TestParseMemEvents(t *testing.T) {
	got, err := parseMemEvents(fixture(t, "cgroup/scope-oom/memory.events"))
	if want := (MemEvents{Max: 37, OOM: 1, OOMKill: 1}); err != nil || got != want {
		t.Errorf("OOM scope: %+v, %v; want %+v", got, err, want)
	}
	got, err = parseMemEvents(fixture(t, "cgroup/scope-busy/memory.events"))
	if err != nil || got != (MemEvents{}) {
		t.Errorf("busy scope: %+v, %v; want zeros", got, err)
	}
	// The owner slice's counts include its scopes': that is how an OOM kill is
	// still visible once the scope that took it is gone.
	got, err = parseMemEvents(fixture(t, "cgroup/slice-owner/memory.events"))
	if err != nil || got.OOMKill != 1 {
		t.Errorf("owner slice: %+v, %v; want oom_kill 1", got, err)
	}
	// Synthetic: a kernel before 5.17 has no oom_group_kill, and one before 4.x
	// fewer keys still. What isn't there stays zero.
	got, err = parseMemEvents([]byte("low 1\nhigh 2\nmax 3\noom 4\noom_kill 5\n"))
	if want := (MemEvents{Low: 1, High: 2, Max: 3, OOM: 4, OOMKill: 5}); err != nil || got != want {
		t.Errorf("old kernel: %+v, %v; want %+v", got, err, want)
	}
}

func TestParsePSIFixtures(t *testing.T) {
	for _, tc := range []struct {
		file string
		want PSI
	}{
		{"cgroup/scope-busy/cpu.pressure", PSI{SomeUsec: 15822, FullUsec: 15413, HasFull: true}},
		{"cgroup/scope-busy/memory.pressure", PSI{HasFull: true}},
		{"cgroup/scope-busy/io.pressure", PSI{SomeUsec: 1429687, FullUsec: 1290871, HasFull: true}},
		{"cgroup/scope-oom/memory.pressure", PSI{SomeUsec: 284, FullUsec: 284, HasFull: true}},
		{"proc/pressure/cpu", PSI{SomeUsec: 696353163, HasFull: true}}, // the host's cpu "full" line is always zero
		{"proc/pressure/io", PSI{SomeUsec: 148132277, FullUsec: 132556407, HasFull: true}},
		{"proc/pressure/memory", PSI{SomeUsec: 4875460, FullUsec: 4513467, HasFull: true}},
	} {
		got, err := parsePSI(fixture(t, tc.file))
		if err != nil || got != tc.want {
			t.Errorf("%s: %+v, %v; want %+v", tc.file, got, err, tc.want)
		}
	}
}

func TestParsePSIVariants(t *testing.T) {
	// Synthetic: before Linux 5.13 cpu.pressure has no full line; the caller has
	// to be able to tell that from a full line that says zero.
	got, err := parsePSI([]byte("some avg10=1.00 avg60=2.00 avg300=3.00 total=77\n"))
	if want := (PSI{SomeUsec: 77}); err != nil || got != want {
		t.Errorf("some only: %+v, %v; want %+v", got, err, want)
	}
	if got.HasFull {
		t.Error("a file with no full line says it has one")
	}
	for name, in := range map[string]string{
		"empty":         "",
		"no total":      "some avg10=0.00 avg60=0.00 avg300=0.00\n",
		"bad total":     "some avg10=0.00 total=x\n",
		"unknown kind":  "partial avg10=0.00 total=1\n",
		"full only":     "full avg10=0.00 avg60=0.00 avg300=0.00 total=5\n",
		"kind only":     "some\n",
		"total overrun": "some total=18446744073709551616\n",
	} {
		if _, err := parsePSI([]byte(in)); err == nil {
			t.Errorf("%s: accepted %q", name, in)
		}
	}
}

func TestParseIOStatFixtures(t *testing.T) {
	// One device row.
	got, n, err := parseIOStat(fixture(t, "cgroup/scope-busy/io.stat"), nil)
	if want := (IO{ReadBytes: 1651507200, WriteBytes: 1677721600, ReadOps: 1895, WriteOps: 1984, HasOps: true}); err != nil || n != 1 || got != want {
		t.Errorf("one device: %+v over %d rows, %v; want %+v", got, n, err, want)
	}
	// A cgroup that has done no I/O has an empty file, which is a zero, not an error.
	got, n, err = parseIOStat(fixture(t, "cgroup/scope-idle/io.stat"), nil)
	if err != nil || n != 0 || got != (IO{HasOps: true}) {
		t.Errorf("empty file: %+v over %d rows, %v; want a zero IO", got, n, err)
	}
	// Three devices summed: the root disk, another disk, and a loop device.
	got, n, err = parseIOStat(fixture(t, "cgroup/slice-owner/io.stat"), nil)
	if want := (IO{ReadBytes: 10240 + 16777216 + 1735393280, WriteBytes: 33554432 + 0 + 1744830464, ReadOps: 10 + 16 + 2039, WriteOps: 64 + 0 + 2048, HasOps: true}); err != nil || n != 3 || got != want {
		t.Errorf("three devices: %+v over %d rows, %v; want %+v", got, n, err, want)
	}
	// The host's root cgroup lists every block device, 39 of them with no
	// counters at all (a row with only its device number and a trailing space).
	got, n, err = parseIOStat(fixture(t, "cgroup/root-host/io.stat"), nil)
	if want := (IO{ReadBytes: 40499319808, WriteBytes: 137662945280, ReadOps: 378230, WriteOps: 2682194, HasOps: true}); err != nil || n != 42 || got != want {
		t.Errorf("root cgroup: %+v over %d rows, %v; want %+v over 42", got, n, err, want)
	}
}

// Synthetic: a RAID or LVM volume is listed with the disks beneath it for the
// same I/O, so summing every row counts it more than once. The rows of a
// stacked device are the ones to leave out.
func TestParseIOStatSkipsStackedDevices(t *testing.T) {
	const in = "9:0 rbytes=100 wbytes=200 rios=1 wios=2 dbytes=0 dios=0\n" + // md0, built on the two below
		"259:0 rbytes=100 wbytes=200 rios=1 wios=2 dbytes=0 dios=0\n" +
		"259:1 rbytes=0 wbytes=200 rios=0 wios=2 dbytes=0 dios=0\n"
	all, n, err := parseIOStat([]byte(in), nil)
	if err != nil || n != 3 || all.WriteBytes != 600 {
		t.Fatalf("every row: %+v over %d rows, %v", all, n, err)
	}
	stacked := func(major, minor uint32) bool { return major == 9 }
	got, n, err := parseIOStat([]byte(in), stacked)
	if want := (IO{ReadBytes: 100, WriteBytes: 400, ReadOps: 1, WriteOps: 4, HasOps: true}); err != nil || n != 3 || got != want {
		t.Errorf("without the stacked device: %+v over %d rows, %v; want %+v over 3", got, n, err, want)
	}
	// Whatever is skipped still has to be well formed: a broken row is a
	// broken file, wherever it is.
	if _, _, err := parseIOStat([]byte("9:0 rbytes=x\n"), stacked); err == nil {
		t.Error("a malformed row of a skipped device was accepted")
	}
}

func TestParseIOStatVariants(t *testing.T) {
	// Synthetic: with iocost on, each row carries cost.* keys; they and the
	// discard counters are not I/O to add up.
	got, _, err := parseIOStat([]byte("8:0 rbytes=1 wbytes=2 rios=3 wios=4 dbytes=9 dios=9 cost.usage=5 cost.wait=6 cost.indebt=7 cost.indelay=8\n"), nil)
	if want := (IO{ReadBytes: 1, WriteBytes: 2, ReadOps: 3, WriteOps: 4, HasOps: true}); err != nil || got != want {
		t.Errorf("iocost keys: %+v, %v; want %+v", got, err, want)
	}
	// A kernel that leaves a key out of a row leaves it out of the sum.
	got, _, err = parseIOStat([]byte("8:0 rbytes=1 wbytes=2\n"), nil)
	if want := (IO{ReadBytes: 1, WriteBytes: 2, HasOps: true}); err != nil || got != want {
		t.Errorf("short row: %+v, %v; want %+v", got, err, want)
	}
	for name, in := range map[string]string{
		"no device number":  "rbytes=1 wbytes=2\n",
		"bad device number": "x:y rbytes=1\n",
		"half a number":     "8: rbytes=1\n",
		"device too large":  "4294967296:0 rbytes=1\n",
		"field without =":   "8:0 rbytes\n",
		"bad value":         "8:0 rbytes=1.5\n",
		"negative value":    "8:0 wbytes=-1\n",
	} {
		if _, _, err := parseIOStat([]byte(in), nil); err == nil {
			t.Errorf("%s: accepted %q", name, in)
		}
	}
}

func TestUsageOfARealBusyScope(t *testing.T) {
	var r usageReader
	got, err := r.usage(fixtureDir(t, "cgroup/scope-busy"))
	if err != nil {
		t.Fatal(err)
	}
	if got.CPU == nil || *got.CPU != (CPU{UsageUsec: 7080860, UserUsec: 6198073, SystemUsec: 882787}) {
		t.Errorf("cpu = %+v", got.CPU)
	}
	wantMem := Memory{Current: 103555072, Peak: 104243200, Anon: 102584320, File: 0, HasPeak: true, HasStat: true}
	if got.Memory == nil || *got.Memory != wantMem {
		t.Errorf("memory = %+v, want %+v", got.Memory, wantMem)
	}
	if got.IO == nil || got.IO.WriteBytes != 1677721600 || !got.IO.HasOps {
		t.Errorf("io = %+v", got.IO)
	}
	if got.Pressure == nil || got.Pressure.CPU == nil || got.Pressure.Memory == nil || got.Pressure.IO == nil {
		t.Fatalf("pressure = %+v", got.Pressure)
	}
	if got.Pressure.IO.SomeUsec != 1429687 || got.Pressure.Memory.SomeUsec != 0 || !got.Pressure.Memory.HasFull {
		t.Errorf("pressure = %+v %+v %+v", got.Pressure.CPU, got.Pressure.Memory, got.Pressure.IO)
	}
	if got.Events == nil || *got.Events != (MemEvents{}) {
		t.Errorf("events = %+v", got.Events)
	}
	// A file-backed figure of zero is a reading, not a missing one: the group
	// says it was read, so it is reported as 0 and not left out.
	if !got.Memory.HasStat {
		t.Error("memory.stat was read but isn't marked read")
	}
}

func TestUsageOfAScopeThatTookAnOOMKill(t *testing.T) {
	var r usageReader
	got, err := r.usage(fixtureDir(t, "cgroup/scope-oom"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Events == nil || got.Events.OOMKill != 1 || got.Events.OOM != 1 || got.Events.Max != 37 {
		t.Errorf("events = %+v", got.Events)
	}
	if got.Memory.Peak != 32<<20 {
		t.Errorf("peak = %d, want the 32 MiB limit it hit", got.Memory.Peak)
	}
	if got.IO == nil || *got.IO != (IO{HasOps: true}) {
		t.Errorf("io = %+v: an empty io.stat is no I/O, not an unavailable one", got.IO)
	}
}

func TestUsageWithWhatTheKernelLacks(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Synthetic: a kernel before 5.19 (no memory.peak), without PSI (no
	// *.pressure files), with the io controller off (no io.stat).
	write("cpu.stat", "usage_usec 5\nuser_usec 3\nsystem_usec 2\n")
	write("memory.current", "4096\n")
	write("memory.stat", "anon 4096\nfile 0\n")
	write("memory.events", "low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\n")
	var r usageReader
	got, err := r.usage(dir)
	if err != nil {
		t.Fatalf("a kernel without those files is no error: %v", err)
	}
	if got.CPU == nil || got.Memory == nil || got.Events == nil {
		t.Fatalf("lost what is there: %+v", got)
	}
	if got.Memory.HasPeak || got.IO != nil || got.Pressure != nil {
		t.Errorf("invented what isn't: peak %v, io %v, pressure %v", got.Memory.HasPeak, got.IO, got.Pressure)
	}
	if !got.Memory.HasStat || got.Memory.Anon != 4096 {
		t.Errorf("memory = %+v", got.Memory)
	}
	// A controller that is off for the cgroup leaves its group out, and the
	// rest is still read.
	if err := os.Remove(filepath.Join(dir, "memory.current")); err != nil {
		t.Fatal(err)
	}
	got, err = r.usage(dir)
	if err != nil || got.Memory != nil || got.CPU == nil {
		t.Errorf("memory controller off: %+v, %v", got, err)
	}
}

func TestUsageOfAFilePartlyUnreadable(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cpu.stat"), []byte("usage_usec 5\nuser_usec 3\nsystem_usec 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A directory where a file should be: opening it works and reading it
	// fails, which is a fault, not an absent file.
	if err := os.Mkdir(filepath.Join(dir, "io.stat"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.events"), []byte("oom x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var r usageReader
	got, err := r.usage(dir)
	if err == nil {
		t.Fatal("unreadable and malformed files raised no error")
	}
	for _, want := range []string{"io.stat", "memory.events"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q doesn't name %s", err, want)
		}
	}
	if errors.Is(err, errGone) {
		t.Error("a fault was taken for the cgroup going away")
	}
	if got.CPU == nil || got.CPU.UsageUsec != 5 {
		t.Errorf("what could be read was thrown away with it: %+v", got)
	}
	if got.IO != nil || got.Events != nil {
		t.Errorf("figures from files that failed: %+v %+v", got.IO, got.Events)
	}
}

func TestUsageOfARemovedCgroup(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cpu.stat"), []byte("usage_usec 5\nuser_usec 3\nsystem_usec 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var r usageReader
	if _, err := r.usage(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := r.usage(dir); !errors.Is(err, errGone) {
		t.Errorf("a cgroup that was removed = %v, want errGone", err)
	}
}

func TestAbsentAndFaults(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		absent bool
	}{
		"no such file":        {&fs.PathError{Op: "open", Path: "x", Err: syscall.ENOENT}, true},
		"PSI off":             {&fs.PathError{Op: "read", Path: "x", Err: syscall.EOPNOTSUPP}, true},
		"permission denied":   {&fs.PathError{Op: "open", Path: "x", Err: syscall.EACCES}, false},
		"input/output error":  {&fs.PathError{Op: "read", Path: "x", Err: syscall.EIO}, false},
		"a removed cgroup":    {&fs.PathError{Op: "read", Path: "x", Err: syscall.ENODEV}, false},
		"invalid (a bad arg)": {&fs.PathError{Op: "read", Path: "x", Err: syscall.EINVAL}, false},
	} {
		if got := absent(tc.err); got != tc.absent {
			t.Errorf("%s: absent = %v, want %v", name, got, tc.absent)
		}
	}
}

func TestReadGrowsItsBufferForALargeFile(t *testing.T) {
	// memory.stat is a few kilobytes and io.stat grows with the number of
	// devices; a file larger than the buffer is read whole all the same.
	path := filepath.Join(t.TempDir(), "big")
	want := strings.Repeat("0123456789abcdef", 3000) // 48000 bytes
	if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}
	var r usageReader
	got, err := r.read(path)
	if err != nil || string(got) != want {
		t.Fatalf("read %d bytes, err %v; want %d", len(got), err, len(want))
	}
	if got, err := r.read(filepath.Join(filepath.Dir(path), "nope")); err == nil || len(got) != 0 {
		t.Errorf("a missing file read as %q, %v", got, err)
	}
	// The buffer is reused, so what comes back is only good until the next read.
	small := filepath.Join(filepath.Dir(path), "small")
	if err := os.WriteFile(small, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := r.read(small); err != nil || string(got) != "hello\n" {
		t.Errorf("small file after a large one = %q, %v", got, err)
	}
}
