//go:build krucible

package krucible

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// TestKrucibleConcurrentWakeNoDoubleLaunch is the regression test for the
// concurrent wake-on-request race: the public proxy and every exec/file handler
// call ensureHot UNCOALESCED, so a burst of requests landing on a non-hot
// sandbox used to each spawn a helper — racing on the shared vsock UDS paths and
// orphaning processes (observed under benchmarking: 10+ bhatti-vmm for one
// sandbox, then a hang). The per-VM launchMu must collapse the burst into a
// single launch. Cross-arch (pure-Go engine) — guards macOS + both Linux arches.
func TestKrucibleConcurrentWakeNoDoubleLaunch(t *testing.T) {
	eng := newBlockRootEngine(t).(*Engine) // skips if libkrun/vmm/mke2fs unavailable
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "wake", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := info.ID
	t.Cleanup(func() { eng.Destroy(context.Background(), id) })

	// Drive it cold (snapshot + free RAM + kill the helper) so each EnsureHot has
	// real work to do (cold → Start → launch), the exact path that double-spawned.
	if err := eng.Stop(ctx, id); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	const N = 20
	var wg sync.WaitGroup
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); errs[i] = eng.EnsureHot(ctx, id) }(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Errorf("concurrent EnsureHot[%d]: %v", i, e)
		}
	}

	// Exactly one helper process must back the sandbox. A double-launch leaves
	// orphaned, untracked helpers pointed at the same sandbox dir.
	if n := countHelpers(t, info.ID); n != 1 {
		t.Fatalf("double-launch: %d helper processes for one sandbox, want 1", n)
	}

	// And the (single) restored VM must be usable.
	if r, err := eng.Exec(ctx, id, []string{"/bin/true"}); err != nil {
		t.Fatalf("exec after concurrent wake: err=%v out=%q", err, r.Stdout)
	}

	// Mirror for the warm path: pause, then a concurrent resume burst.
	if err := eng.Pause(ctx, id); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	wg = sync.WaitGroup{}
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); errs[i] = eng.EnsureHot(ctx, id) }(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Errorf("concurrent warm EnsureHot[%d]: %v", i, e)
		}
	}
	if n := countHelpers(t, info.ID); n != 1 {
		t.Fatalf("warm path: %d helper processes for one sandbox, want 1", n)
	}
}

// countHelpers counts running bhatti-vmm processes whose argv references this
// sandbox id (the spec path is <dataDir>/sandboxes/<id>/vmspec.json). Portable
// across macOS (BSD ps) and Linux (GNU ps).
func countHelpers(t *testing.T, sandboxID string) int {
	t.Helper()
	// Brief settle: a racing-but-doomed second helper may still be exiting.
	time.Sleep(300 * time.Millisecond)
	out, err := exec.Command("ps", "ax", "-o", "command").Output()
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "bhatti-vmm") && strings.Contains(line, "/sandboxes/"+sandboxID+"/") {
			n++
		}
	}
	return n
}
