//go:build darwin

package collector

import (
	"errors"
	"math/big"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

func TestTicksToUsecIsExactAndDoesNotOverflow(t *testing.T) {
	for _, perSec := range []uint64{24_000_000, 1_000_000_000, 1_000_000, 3} {
		for _, ticks := range []uint64{0, 1, perSec - 1, perSec, perSec + 1, 1 << 40, 1<<63 + 12345, 1<<64 - 1} {
			want := new(big.Int).Mul(new(big.Int).SetUint64(ticks), big.NewInt(1_000_000))
			want.Div(want, new(big.Int).SetUint64(perSec))
			if !want.IsUint64() {
				continue // more microseconds than a uint64 holds: not a CPU time anything has
			}
			if got := ticksToUsec(ticks, perSec); got != want.Uint64() {
				t.Errorf("ticksToUsec(%d, %d) = %d, want %d", ticks, perSec, got, want.Uint64())
			}
		}
	}
	rng := rand.New(rand.NewPCG(1, 2))
	for range 1000 {
		ticks, perSec := rng.Uint64()>>8, 1+rng.Uint64N(1_000_000_000)
		want := new(big.Int).Mul(new(big.Int).SetUint64(ticks), big.NewInt(1_000_000))
		want.Div(want, new(big.Int).SetUint64(perSec))
		if want.IsUint64() && ticksToUsec(ticks, perSec) != want.Uint64() {
			t.Fatalf("ticksToUsec(%d, %d) = %d, want %s", ticks, perSec, ticksToUsec(ticks, perSec), want)
		}
	}
}

func TestProcessReaderReadsItsOwnProcess(t *testing.T) {
	p := newProcessReader()
	u, err := p.usage(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if u.CPU == nil || u.CPU.UsageUsec == 0 || u.CPU.UsageUsec != u.CPU.UserUsec+u.CPU.SystemUsec {
		t.Errorf("cpu = %+v", u.CPU)
	}
	if u.Memory == nil || u.Memory.Current < 1<<20 || u.Memory.HasPeak || u.Memory.HasStat {
		t.Errorf("memory = %+v: a process has a resident size and nothing more", u.Memory)
	}
	if u.IO == nil || u.IO.HasOps {
		t.Errorf("io = %+v: a process has bytes and no operation counts", u.IO)
	}
	if u.Pressure != nil || u.Events != nil {
		t.Errorf("figures a process doesn't have: %+v %+v", u.Pressure, u.Events)
	}
}

// Freeze both readings around a finite workload: under load the sampler can be
// descheduled anywhere, including between a reading and its timestamp or the
// final reading and kill. A stopped child cannot accrue unobserved CPU then.
func TestProcessReaderAgreesWithTheKernelsAccountOfAChild(t *testing.T) {
	cmd := exec.Command("sh", "-c", "kill -STOP $$; i=0; while [ \"$i\" -lt 200000 ]; do i=$((i + 1)); done; kill -STOP $$")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := false
	defer func() {
		if !reaped {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	p := newProcessReader()
	pid := cmd.Process.Pid
	waitStopped := func() {
		t.Helper()
		var status syscall.WaitStatus
		for {
			_, err := syscall.Wait4(pid, &status, syscall.WUNTRACED, nil)
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			if err != nil {
				t.Fatalf("wait for child to stop: %v", err)
			}
			// Darwin encodes SIGSTOP and continuation identically; Go calls
			// this Continued. Without WCONTINUED, wait4 only reports stops.
			if !status.Continued() {
				reaped = status.Exited() || status.Signaled()
				t.Fatalf("child status = %v, want SIGSTOP", status)
			}
			return
		}
	}
	waitStopped()
	first, err := p.usage(pid)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := cmd.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	waitStopped()
	end := time.Now()
	last, err := p.usage(pid)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := deriveRates(&sample{at: start, ident: incarnation{pid: pid}, usage: first},
		&sample{at: end, ident: incarnation{pid: pid}, usage: last})
	if !ok || r.CPUPercent <= 0 || r.CPUPercent > 100 {
		t.Errorf("the child's bounded workload used %v%% of a CPU: %+v, %v", r.CPUPercent, r, ok)
	}

	// wait4 independently reports the lifetime total in microseconds, not Mach
	// ticks. Since the last reading the child has stayed stopped, so only
	// accounting precision and the cost of exit can separate these figures.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	reaped = true
	ru, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage)
	if !ok {
		t.Fatalf("no rusage from %T", cmd.ProcessState.SysUsage())
	}
	usec := func(tv syscall.Timeval) uint64 { return uint64(tv.Sec)*1_000_000 + uint64(tv.Usec) }
	finalUser, finalSystem := usec(ru.Utime), usec(ru.Stime)
	const slack = 10_000 // µs; no scheduler-dependent gap between reading and exit
	if last.CPU.UserUsec > finalUser || finalUser-last.CPU.UserUsec > slack {
		t.Errorf("user time: read %d µs while it ran, the kernel says %d µs in all", last.CPU.UserUsec, finalUser)
	}
	if last.CPU.SystemUsec > finalSystem+10_000 || finalSystem > last.CPU.SystemUsec+slack {
		t.Errorf("system time: read %d µs while it ran, the kernel says %d µs in all", last.CPU.SystemUsec, finalSystem)
	}
	t.Logf("stopped child: proc_info user %d system %d µs; wait4 user %d system %d µs; workload %.1f%% of a CPU",
		last.CPU.UserUsec, last.CPU.SystemUsec, finalUser, finalSystem, r.CPUPercent)

	// Once it is gone, so are its figures.
	if _, err := p.usage(pid); !errors.Is(err, errGone) {
		t.Errorf("a process that has exited = %v, want errGone", err)
	}
}

func TestProcessReaderCountsBytesWrittenToDisk(t *testing.T) {
	p := newProcessReader()
	before, err := p.usage(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(t.TempDir(), "w"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	for i := range buf {
		buf[i] = byte(i*7 + 1)
	}
	const mib = 64
	for range mib {
		if _, err := f.Write(buf); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	after, err := p.usage(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if got := after.IO.WriteBytes - before.IO.WriteBytes; got < mib<<20/2 {
		t.Errorf("a process that wrote and synced %d MiB is credited with %d MiB", mib, got>>20)
	}
}

func TestProcessReaderOfAProcessThatNeverWas(t *testing.T) {
	// A pid near the top of the range: not running.
	if _, err := newProcessReader().usage(99998); !errors.Is(err, errGone) {
		t.Errorf("a pid nothing has = %v, want errGone", err)
	}
}

func TestProcessReaderWithoutATimebaseFailsEveryRead(t *testing.T) {
	p := &processReader{err: errors.New("hw.tbfrequency: unavailable")}
	if _, err := p.usage(os.Getpid()); err == nil || errors.Is(err, errGone) {
		t.Errorf("usage with no timebase = %v, want an error that isn't a vanished process", err)
	}
}

func TestMacOSHostFigures(t *testing.T) {
	dir := t.TempDir()
	h := hostReader{rd: &usageReader{}, dataDir: dir}
	host, err := h.read()
	if err != nil {
		t.Fatal(err)
	}
	if host.CPUs != runtime.NumCPU() {
		t.Errorf("cpus = %d", host.CPUs)
	}
	if host.CPU != nil || host.Pressure != nil {
		t.Errorf("Linux figures from a Mac: %+v %+v", host.CPU, host.Pressure)
	}
	m := host.Memory
	if m == nil || m.TotalBytes < 1<<30 || m.AvailableBytes > m.TotalBytes || m.SwapFreeBytes > m.SwapTotalBytes {
		t.Fatalf("memory = %+v", m)
	}
	switch m.PressureLevel {
	case "normal", "warn", "critical":
	default:
		t.Errorf("pressure level = %q", m.PressureLevel)
	}
	if d := host.Disk; d == nil || d.Path != dir || d.TotalBytes == 0 || d.FreeBytes > d.TotalBytes {
		t.Errorf("disk = %+v", d)
	}
}

func TestPressureLevelNames(t *testing.T) {
	for level, want := range map[uint32]string{1: "normal", 2: "warn", 4: "critical", 0: "", 3: "", 8: ""} {
		if got := pressureLevelName(level); got != want {
			t.Errorf("level %d = %q, want %q", level, got, want)
		}
	}
}
