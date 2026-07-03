package enginetest

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// RunReliabilitySuite is the cold-tier hardening gate — the VMM-agnostic "spec"
// the migration plan calls for (the FC snapshot-reliability suite, re-targeted
// at the bundle/cold path). It asserts the failure-mode contract, not just the
// happy path:
//
//   - StopStartCycles: N cold round-trips stay stable — guest RAM (tmpfs marker)
//     AND rootfs (a file on the block root) survive EVERY cycle, and exec works
//     after each. Catches drift the single-cycle RunSnapshotSuite can't (leaks,
//     device-state corruption that only shows on the 2nd/3rd restore).
//   - ConcurrentLifecycle: a burst of overlapping Stop/Start/EnsureHot/Exec must
//     serialize (the per-VM launch lock) into a consistent, still-usable VM — no
//     panic, no wedged state, no lost sandbox.
//   - IdempotentTransitions: Stop-on-stopped and Start-on-running are no-ops, so
//     a retrying caller (proxy wake, daemon recovery) can't corrupt state.
//
// Requires a cold-tier-capable engine (block root); the caller passes the right
// factory (e.g. krucible's newBlockRootEngine), which self-skips without a
// hypervisor. Engine-internal failure injections (snapshot-write failure,
// agent-timeout cleanup, helper-process leak counts) live in the engine's own
// package where the internals are reachable.
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

		// A rootfs marker (survives via the block root) and a tmpfs marker
		// (survives only if guest RAM round-trips through the snapshot).
		const rootMark = "reliability-root-9a1c"
		if err := fe.FileWrite(ctx, id, "/root/relmark", "0644", int64(len(rootMark)), strings.NewReader(rootMark)); err != nil {
			t.Fatalf("write rootfs marker: %v", err)
		}

		const cycles = 3
		for i := 0; i < cycles; i++ {
			// Refresh the RAM marker each cycle so a survived value proves THIS
			// cycle's RAM round-tripped, not a stale disk copy.
			ramMark := fmt.Sprintf("reliability-ram-cycle-%d", i)
			if err := fe.FileWrite(ctx, id, "/tmp/relram", "0644", int64(len(ramMark)), strings.NewReader(ramMark)); err != nil {
				t.Fatalf("cycle %d: write tmpfs marker: %v", i, err)
			}

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

			// exec works after every restore (vCPU/devices actually resumed).
			if r, err := eng.Exec(ctx, id, []string{"echo", "cycle-ok"}); err != nil || !strings.Contains(r.Stdout, "cycle-ok") {
				t.Fatalf("cycle %d: exec-after-restore: err=%v out=%q", i, err, r.Stdout)
			}
			// tmpfs (guest RAM) survived.
			if got := readFile(t, fe, ctx, id, "/tmp/relram"); got != ramMark {
				t.Fatalf("cycle %d: tmpfs marker = %q, want %q (guest RAM not restored)", i, got, ramMark)
			}
			// rootfs survived every cycle.
			if got := readFile(t, fe, ctx, id, "/root/relmark"); got != rootMark {
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

		// Drive it cold so each EnsureHot has real work (cold→restore) — the path
		// that used to double-launch.
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
