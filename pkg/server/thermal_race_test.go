package server

import (
	"sync"
	"testing"
)

// Regression tests for the sync.Map LoadOrStore+Store TOCTOU race on
// the thermal manager's failure counters (`thermalFails` and
// `snapshotFailures`). Tranche 0a of PLAN-bhatti-v2.md.
//
// The bug: the pre-fix shape of incrementThermalFails was
//
//	val, _ := s.thermalFails.LoadOrStore(eid, 0)
//	count := val.(int) + 1
//	s.thermalFails.Store(eid, count)
//
// which allows this interleaving:
//
//	A (thermal loop):  LoadOrStore  -> val=5
//	B (HTTP handler):  Delete       -> map state: {}
//	A (thermal loop):  Store(6)     -> map state: {eid: 6}  ← BUG
//
// touchActivity on the HTTP path is the source of B; it calls
// resetThermalFails after every authenticated request. The contract
// is "user activity resets the failure counter, period." Pre-fix,
// the resurrection from A's Store silently undoes that contract.
//
// `go test -race` does NOT catch this. sync.Map.Load and Store are
// individually atomic; the race is logical (between two map ops),
// not a data race. So these tests assert the observable invariant
// directly: after concurrent increment + reset, the map state must
// be either empty (reset won) or have value 1 (reset ran first, then
// a fresh increment started). Any value > 1 means resurrection.
//
// To verify pre-fix: restore the LoadOrStore→Load→+1→Store shape in
// increment{Snapshot,Thermal}Failures. On a multi-core machine the
// bug then fires ~1-15 times per 50,000 iterations — reliably enough
// that the test fails. Post-fix it's zero. Iteration count is the
// only knob, since the race window is just a few instructions and we
// won't instrument production code to widen it.

const raceIterations = 50000

// stressIncrementVsReset runs `iterations` rounds of (seed counter to
// `seed`, then concurrently increment + reset). It returns the number
// of iterations where the post-state shows resurrection (value > 1).
//
// `incr` and `reset` and `count` are the helpers under test, parameterised
// so the same harness covers both thermalFails and snapshotFailures.
func stressIncrementVsReset(
	t *testing.T,
	iterations, seed int,
	incr func(),
	reset func(),
	count func() int,
) int {
	t.Helper()

	resurrections := 0
	for i := 0; i < iterations; i++ {
		// Seed sequentially so we start each round at a known count.
		for j := 0; j < seed; j++ {
			incr()
		}

		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); <-start; incr() }()
		go func() { defer wg.Done(); <-start; reset() }()
		close(start)
		wg.Wait()

		// Valid post-states:
		//   - count == 0: reset ran last, won the race.
		//   - count == 1: reset ran first, then increment started fresh.
		// Anything > 1 means the increment's write undid the reset's
		// Delete (the resurrection bug).
		if got := count(); got > 1 {
			resurrections++
		}

		// Hard reset between iterations so state doesn't leak.
		reset()
	}
	return resurrections
}

func TestThermalFailsResetDoesNotResurrect(t *testing.T) {
	srv, _ := setup(t)
	eid := "test-engine-thermal-race"

	resurrections := stressIncrementVsReset(t, raceIterations, 5,
		func() { srv.incrementThermalFails(eid) },
		func() { srv.resetThermalFails(eid) },
		func() int { return srv.thermalFailsCount(eid) },
	)
	if resurrections > 0 {
		t.Fatalf("thermalFails resurrected by concurrent increment in %d/%d iterations",
			resurrections, raceIterations)
	}
}

func TestSnapshotFailuresResetDoesNotResurrect(t *testing.T) {
	srv, _ := setup(t)
	eid := "test-engine-snapshot-race"

	resurrections := stressIncrementVsReset(t, raceIterations, 5,
		func() { srv.incrementSnapshotFailures(eid) },
		func() { srv.resetSnapshotFailures(eid) },
		func() int { return srv.snapshotFailuresCount(eid) },
	)
	if resurrections > 0 {
		t.Fatalf("snapshotFailures resurrected by concurrent increment in %d/%d iterations",
			resurrections, raceIterations)
	}
}
