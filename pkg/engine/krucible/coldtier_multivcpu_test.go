//go:build krucible

package krucible

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// TestKrucibleColdTierMultiVcpu hardens cold-tier save/restore beyond the shared
// 1-vCPU snapshot suite, exercising the subtle parts of the GIC capture:
//
//   - Multi-vCPU (2): the GICv2 distributor registers covering the 32 private
//     IRQs (SGI/PPI) are *banked per-vCPU*, so a 2-vCPU restore is the first real
//     test of the per-vCPU banking loop in save_state/restore_state. Both vCPUs
//     must be online again after each restore (/proc/cpuinfo lists only online
//     CPUs — a vCPU wedged by a bad GIC restore would drop out).
//   - vtimer interrupt: `sleep` blocks on the arch timer (PPI 27) firing through
//     the GIC. If the banked per-vCPU enable/active state isn't restored, this
//     hangs (never wakes — caught by the ctx timeout) or returns instantly. We
//     assert it both completes and actually took the wall time.
//   - Two cold cycles: a restore must not corrupt state for the next snapshot.
//
// On arm64 this is the GICv2 KvmGicV2::{save_state,restore_state} gate; on x86
// it's extra coverage of the existing cold tier. Skips without libkrun / a
// hypervisor / mke2fs (newBlockRootEngine).
func TestKrucibleColdTierMultiVcpu(t *testing.T) {
	eng := newBlockRootEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "coldmv", CPUs: 2, MemoryMB: 512})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := info.ID
	t.Cleanup(func() { eng.Destroy(context.Background(), id) })

	// /proc/cpuinfo lists one "processor" stanza per *online* CPU.
	wantOnlineCPUs := func(tag string) {
		t.Helper()
		r, err := eng.Exec(ctx, id, []string{"cat", "/proc/cpuinfo"})
		if err != nil {
			t.Fatalf("%s: cat /proc/cpuinfo: %v", tag, err)
		}
		if n := strings.Count(r.Stdout, "processor"); n != 2 {
			t.Fatalf("%s: online CPUs = %d, want 2\n%s", tag, n, r.Stdout)
		}
	}
	wantOnlineCPUs("pre-snapshot")

	for cycle := 0; cycle < 2; cycle++ {
		if err := eng.Stop(ctx, id); err != nil {
			t.Fatalf("cycle %d Stop: %v", cycle, err)
		}
		if err := eng.Start(ctx, id); err != nil {
			t.Fatalf("cycle %d Start: %v", cycle, err)
		}

		// The vtimer must fire after restore: `sleep` blocks on the arch-timer
		// IRQ routed through the GIC. A broken banked per-vCPU restore makes this
		// hang (ctx timeout) or return instantly.
		start := time.Now()
		if _, err := eng.Exec(ctx, id, []string{"sleep", "1"}); err != nil {
			t.Fatalf("cycle %d sleep-after-restore: %v", cycle, err)
		}
		if elapsed := time.Since(start); elapsed < 800*time.Millisecond {
			t.Fatalf("cycle %d: sleep 1 returned in %v (<800ms) — vtimer IRQ not firing after restore", cycle, elapsed)
		}

		wantOnlineCPUs(fmt.Sprintf("cycle %d post-restore", cycle))
	}
}
