//go:build krucible

package krucible

import (
	"context"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// guestWallOffset is the guest's CLOCK_REALTIME minus the host's, with the
// host side taken at the midpoint of the exec round trip. The test rootfs's
// date prints the guest's time.Now().UnixNano().
func guestWallOffset(t *testing.T, ctx context.Context, e *Engine, id string) time.Duration {
	t.Helper()
	before := time.Now()
	r, err := e.Exec(ctx, id, []string{"date"})
	after := time.Now()
	if err != nil || r.ExitCode != 0 {
		t.Fatalf("guest date: err=%v result=%+v", err, r)
	}
	nanos, err := strconv.ParseInt(strings.TrimSpace(r.Stdout), 10, 64)
	if err != nil {
		t.Fatalf("guest date %q: %v", r.Stdout, err)
	}
	return time.Duration(nanos - before.Add(after.Sub(before)/2).UnixNano())
}

func guestUptime(t *testing.T, ctx context.Context, e *Engine, id string) float64 {
	t.Helper()
	r, err := e.Exec(ctx, id, []string{"cat", "/proc/uptime"})
	if err != nil || r.ExitCode != 0 {
		t.Fatalf("guest uptime: err=%v result=%+v", err, r)
	}
	f := strings.Fields(r.Stdout)
	if len(f) == 0 {
		t.Fatalf("empty /proc/uptime: %q", r.Stdout)
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func requireNearHost(t *testing.T, what string, off time.Duration) {
	t.Helper()
	t.Logf("%s: guest wall clock is %+.3fs from the host's", what, off.Seconds())
	if math.Abs(off.Seconds()) > 1 {
		t.Fatalf("%s: guest wall clock is %+.3fs from the host's, want within 1s", what, off.Seconds())
	}
}

// Every warm cycle used to leave CLOCK_REALTIME behind by the time spent
// paused (kvmclock is frozen across PAUSE and restored by RESUME), so a reused
// sandbox drifted by minutes. A wake must put the wall clock back, and leave
// the monotonic clock frozen as TestKrucibleClockFreeze requires.
func TestKrucibleWarmWakeSyncsWallClock(t *testing.T) {
	e := newPauseEngine(t).(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	info, err := e.Create(ctx, engine.SandboxSpec{Name: "warm-wall", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Destroy(context.Background(), info.ID) })
	requireNearHost(t, "fresh boot", guestWallOffset(t, ctx, e, info.ID))

	for _, paused := range []time.Duration{35 * time.Second, 6 * time.Second} {
		upBefore := guestUptime(t, ctx, e, info.ID)
		if err := e.Pause(ctx, info.ID); err != nil {
			t.Fatal(err)
		}
		time.Sleep(paused)
		if err := e.EnsureHot(ctx, info.ID); err != nil {
			t.Fatal(err)
		}
		requireNearHost(t, "after a "+paused.String()+" warm pause", guestWallOffset(t, ctx, e, info.ID))
		if d := guestUptime(t, ctx, e, info.ID) - upBefore; d > 1.5 {
			t.Fatalf("guest monotonic clock advanced %.2fs across a %s pause; it must stay frozen", d, paused)
		}
	}
}

// A memory image keeps the wall clock it was saved with. A source that lagged
// (a clock push that failed, or a PAUSE that bypassed the engine) must not
// pass its lag to a fork, and the source itself must be corrected once the
// checkpoint's brief pause ends.
func TestKrucibleCheckpointOfLaggedGuestClock(t *testing.T) {
	e := newCheckpointEngine(t).(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	src, err := e.Create(ctx, engine.SandboxSpec{Name: "lag-src", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Destroy(context.Background(), src.ID) })
	vm, err := e.getVM(src.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Lag the guest without the engine's wake-up clock push.
	if _, err := controlCmd(ctx, vm.CtlSockUDS, "PAUSE"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(12 * time.Second)
	if _, err := controlCmd(ctx, vm.CtlSockUDS, "RESUME"); err != nil {
		t.Fatal(err)
	}
	lag := guestWallOffset(t, ctx, e, src.ID)
	t.Logf("source guest is %+.3fs from the host after an unsynced pause", lag.Seconds())
	if lag > -10*time.Second {
		t.Fatalf("could not stage a lagging guest: offset %s", lag)
	}

	parent := t.TempDir()
	m, err := e.Checkpoint(ctx, src.ID, "", 0, "lagged", parent)
	if err != nil {
		t.Fatal(err)
	}
	requireNearHost(t, "source after its checkpoint pause", guestWallOffset(t, ctx, e, src.ID))

	restored, err := e.ResumeFromManifestJSON(ctx, parent+"/lagged", checkpointJSON(t, m), "lag-fork", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Destroy(context.Background(), restored.ID) })
	requireNearHost(t, "fork restored from the lagged image", guestWallOffset(t, ctx, e, restored.ID))
}
