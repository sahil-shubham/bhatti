package krucible

import (
	"bytes"
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
	// Freeze semantics come from the macOS HVF CNTVOFF vtimer adjust on resume.
	// The Linux/KVM warm-resume clock-continuity fix (KVM_SET_CLOCK/kvmclock) is
	// a documented TODO, so the guest clock still advances across a pause there.
	if runtime.GOOS != "darwin" {
		t.Skip("clock freeze is macOS-only today; linux KVM warm-resume clock continuity is a TODO")
	}
	eng := newSuiteEngine(t).(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "clk", CPUs: 1, MemoryMB: 512})
	if err != nil { t.Fatalf("Create: %v", err) }
	id := info.ID
	t.Cleanup(func() { eng.Destroy(context.Background(), id) })

	uptime := func() float64 {
		var buf bytes.Buffer
		if _, _, err := eng.FileRead(ctx, id, "/proc/uptime", &buf); err != nil {
			t.Fatalf("read /proc/uptime: %v", err)
		}
		f := strings.Fields(buf.String())
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
