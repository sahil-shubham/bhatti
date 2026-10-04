package enginetest

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// RunReliabilitySuite hardens the power-off cold tier:
//   - StopStartCycles: rootfs data survives every power-off/reboot and exec
//     works after each boot (RAM is intentionally discarded).
//   - ConcurrentWakeStorm: overlapping cold wakes converge on one usable VM.
//   - IdempotentTransitions: retrying Stop/Start does not corrupt state.
//
// The caller supplies a block-root engine that self-skips without a hypervisor.
func RunReliabilitySuite(t *testing.T, newEngine NewEngine) {
	eng := newEngine(t) // may t.Skip
	fe, ok := eng.(fileEngine)
	if !ok {
		t.Skip("engine does not implement the file surface (needed for RAM/rootfs markers)")
	}

	t.Run("StopStartCycles", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		info, err := eng.Create(ctx, engine.SandboxSpec{Name: "relcycle", CPUs: 1, MemoryMB: 512})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		id := info.ID
		t.Cleanup(func() { eng.Destroy(context.Background(), id) })

		// The rootfs survives Stop/Start because it is a persistent block root.
		const rootMark = "reliability-root-9a1c"
		if err := fe.FileWrite(ctx, id, "/workspace/relmark", "0644", int64(len(rootMark)), strings.NewReader(rootMark)); err != nil {
			t.Fatalf("write rootfs marker: %v", err)
		}

		const cycles = 3
		for i := 0; i < cycles; i++ {

			if err := eng.Stop(ctx, id); err != nil {
				t.Fatalf("cycle %d: Stop: %v", i, err)
			}
			if s, err := eng.Status(ctx, id); err != nil || s.Status != "stopped" {
				t.Fatalf("cycle %d: post-Stop status = %q (err %v), want stopped", i, s.Status, err)
			}
			if err := eng.Start(ctx, id); err != nil {
				t.Fatalf("cycle %d: Start: %v", i, err)
			}
			if s, err := eng.Status(ctx, id); err != nil || s.Status != "running" {
				t.Fatalf("cycle %d: post-Start status = %q (err %v), want running", i, s.Status, err)
			}

			// A fresh boot must still have a responsive agent and persisted root.
			if r, err := eng.Exec(ctx, id, []string{"echo", "cycle-ok"}); err != nil || !strings.Contains(r.Stdout, "cycle-ok") {
				t.Fatalf("cycle %d: exec-after-boot: err=%v out=%q", i, err, r.Stdout)
			}
			// rootfs survived every cycle.
			if got := readFile(t, fe, ctx, id, "/workspace/relmark"); got != rootMark {
				t.Fatalf("cycle %d: rootfs marker = %q, want %q (block root not persisted)", i, got, rootMark)
			}
		}
	})

	t.Run("IdempotentTransitions", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()

		info, err := eng.Create(ctx, engine.SandboxSpec{Name: "relidem", CPUs: 1, MemoryMB: 512})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		id := info.ID
		t.Cleanup(func() { eng.Destroy(context.Background(), id) })

		// Start-on-running is a no-op (still running, still usable).
		if err := eng.Start(ctx, id); err != nil {
			t.Fatalf("Start on running: %v", err)
		}
		if r, err := eng.Exec(ctx, id, []string{"true"}); err != nil || r.ExitCode != 0 {
			t.Fatalf("exec after redundant Start: err=%v exit=%d", err, r.ExitCode)
		}
		// Stop, then Stop again — the second is a no-op, not an error.
		if err := eng.Stop(ctx, id); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if err := eng.Stop(ctx, id); err != nil {
			t.Fatalf("redundant Stop on stopped: %v", err)
		}
		// And it still starts + runs afterwards.
		if err := eng.Start(ctx, id); err != nil {
			t.Fatalf("Start after double-Stop: %v", err)
		}
		if r, err := eng.Exec(ctx, id, []string{"echo", "idem-ok"}); err != nil || !strings.Contains(r.Stdout, "idem-ok") {
			t.Fatalf("exec after double-Stop→Start: err=%v out=%q", err, r.Stdout)
		}
	})

	t.Run("ConcurrentWakeStorm", func(t *testing.T) {
		// The realistic concurrency the daemon actually drives: a cold sandbox gets
		// a BURST of readiness requests (proxy wake + every exec/file handler calls
		// EnsureHot uncoalesced) plus execs, all at once. They must serialize into a
		// SINGLE coherent wake — no double-launch, no wedged agent — and the VM must
		// be usable after. (We deliberately do NOT race Stop against Start on one
		// sandbox: the daemon serializes lifecycle ops per sandbox at a higher
		// layer, and snapshotting a mid-boot guest is not a real-world path.)
		te, ok := eng.(thermalEngine)
		if !ok {
			t.Skip("engine has no thermal surface (EnsureHot); wake-storm N/A")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		info, err := eng.Create(ctx, engine.SandboxSpec{Name: "relconc", CPUs: 1, MemoryMB: 512})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		id := info.ID
		t.Cleanup(func() { eng.Destroy(context.Background(), id) })

		// Drive it cold (helper terminated) so each EnsureHot has real work.
		if err := eng.Stop(ctx, id); err != nil {
			t.Fatalf("Stop (drive cold): %v", err)
		}

		const workers = 12
		var wg sync.WaitGroup
		ehErrs := make([]error, workers)
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				octx, ocancel := context.WithTimeout(ctx, 3*time.Minute)
				defer ocancel()
				if i%3 == 0 {
					// An exec racing the wake may land while still cold — tolerated.
					_, _ = eng.Exec(octx, id, []string{"true"})
					return
				}
				ehErrs[i] = te.EnsureHot(octx, id)
			}(i)
		}
		wg.Wait()
		for i, e := range ehErrs {
			if e != nil {
				t.Errorf("concurrent EnsureHot[%d]: %v", i, e)
			}
		}

		// The (single) woken VM must be usable.
		if r, err := eng.Exec(ctx, id, []string{"echo", "survived"}); err != nil || !strings.Contains(r.Stdout, "survived") {
			t.Fatalf("exec after concurrent wake storm: err=%v out=%q (VM wedged / double-launched?)", err, r.Stdout)
		}
	})
}

// readFile reads a whole guest file to a string (trimmed) via the file surface.
func readFile(t *testing.T, fe fileEngine, ctx context.Context, id, path string) string {
	t.Helper()
	var buf bytes.Buffer
	if _, _, err := fe.FileRead(ctx, id, path, &buf); err != nil {
		t.Fatalf("FileRead %s: %v", path, err)
	}
	return strings.TrimSpace(buf.String())
}
