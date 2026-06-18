package krucible

import (
	"context"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// TestKrucibleClockFreeze verifies warm-tier freeze semantics: a ~3s pause must NOT advance the guest's monotonic
// clock by 3s (the vtimer offset is nudged on resume). Reads /proc/uptime via
// the agent before and after a paused interval.
func TestKrucibleClockFreeze(t *testing.T) {
	// Freeze semantics: macOS uses the HVF CNTVOFF vtimer adjust; linux/x86 uses
	// the VM-level kvmclock rewind (KVM_SET_CLOCK). linux/arm64 continuity needs
	// per-vCPU CNTVOFF and lands with the Tier-3 arm64 cold work — skip there.
	if runtime.GOOS == "linux" && runtime.GOARCH == "arm64" {
		t.Skip("linux/arm64 warm-clock continuity (CNTVOFF) pending Tier-3 arm64 work")
	}
	eng := newSuiteEngine(t).(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "clk", CPUs: 1, MemoryMB: 512})
	if err != nil { t.Fatalf("Create: %v", err) }
	id := info.ID
	t.Cleanup(func() { eng.Destroy(context.Background(), id) })

	// Read uptime via exec+cat (Go's os.ReadFile handles 0-size procfs files),
	// not FileRead (which can't size a procfs file).
	uptime := func() float64 {
		r, err := eng.Exec(ctx, id, []string{"cat", "/proc/uptime"})
		if err != nil {
			t.Fatalf("exec cat /proc/uptime: %v", err)
		}
		f := strings.Fields(r.Stdout)
		if len(f) == 0 {
			t.Fatalf("empty /proc/uptime: %q", r.Stdout)
		}
		v, _ := strconv.ParseFloat(f[0], 64)
		return v
	}

	before := uptime()
	if err := eng.Pause(ctx, id); err != nil { t.Fatalf("Pause: %v", err) }
	time.Sleep(3 * time.Second)
	if err := eng.EnsureHot(ctx, id); err != nil { t.Fatalf("EnsureHot: %v", err) }
	after := uptime()

	delta := after - before
	t.Logf("guest uptime before=%.2fs after=%.2fs delta=%.2fs (wall paused ~3s)", before, after, delta)
	if delta > 1.5 {
		t.Fatalf("guest clock jumped %.2fs across a paused interval (freeze semantics broken)", delta)
	}
}
