package server

import (
	"fmt"
	"testing"
)

// Regression tests for the O(N)-eviction LRU bug in routeCache and
// publicRateLimiter. Tranche 0a item #4 of PLAN-bhatti-v2.md.
//
// Pre-fix, both bounded maps scanned every entry on each Set/getOrCreate
// past capacity to find the oldest by lastAccess timestamp. With
// max-size 10,000 and the write lock held throughout, every excess
// hot-path Set was O(N) under contention.
//
// Fix: container/list-backed LRU. The back of the list is the LRU
// entry; eviction is O(1) (pop the back, delete the map entry).
// MoveToFront on every access keeps the order honest.
//
// These tests verify:
//   - Size bound holds across many insertions.
//   - The LRU semantics are correct: a recently-accessed entry
//     survives an eviction that displaces an older one.
//   - The hot path doesn't degrade with cache size (timing-free
//     correctness checks; the O(1) win is asserted via the
//     observable LRU invariants, not wall-clock).

// --- routeCache ---

// fillRouteCacheTo populates rc with `n` entries. The aliases are
// "alias-0".."alias-(n-1)"; the resolved route's sandboxID is set to
// the alias to make ordering observable.
func fillRouteCacheTo(rc *routeCache, n int) {
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("alias-%d", i)
		rc.Set(k, resolvedRoute{sandboxID: k})
	}
}

// TestRouteCache_SizeBoundEnforced verifies the cache never grows
// beyond routeCacheMaxSize across many insertions.
func TestRouteCache_SizeBoundEnforced(t *testing.T) {
	rc := newRouteCache()

	// Insert max + 5000 unique aliases. Cache must never exceed max.
	for i := 0; i < routeCacheMaxSize+5000; i++ {
		rc.Set(fmt.Sprintf("alias-%d", i), resolvedRoute{sandboxID: "sb"})

		rc.mu.Lock()
		size := len(rc.entries)
		listLen := rc.order.Len()
		rc.mu.Unlock()

		if size > routeCacheMaxSize {
			t.Fatalf("after %d inserts: map size %d exceeded bound %d", i+1, size, routeCacheMaxSize)
		}
		if size != listLen {
			t.Fatalf("map size %d != list size %d (LRU invariant broken)", size, listLen)
		}
	}
}

// TestRouteCache_LRUEvictsLeastRecentlyUsed verifies that Get touches
// the entry's recency: an entry accessed recently survives a later
// eviction that would have removed it under FIFO.
func TestRouteCache_LRUEvictsLeastRecentlyUsed(t *testing.T) {
	rc := newRouteCache()

	// Fill to capacity. alias-0 is the oldest.
	fillRouteCacheTo(rc, routeCacheMaxSize)

	// Touch alias-0 (make it MRU).
	if _, ok := rc.Get("alias-0"); !ok {
		t.Fatal("alias-0 should be present")
	}

	// Insert one more — should evict the now-oldest, which is alias-1
	// (alias-0 was just touched and is now MRU).
	rc.Set("alias-new", resolvedRoute{sandboxID: "sb-new"})

	if _, ok := rc.Get("alias-0"); !ok {
		t.Fatal("alias-0 was evicted despite being most-recently-used; LRU broken")
	}
	if _, ok := rc.Get("alias-1"); ok {
		t.Fatal("alias-1 (the actual LRU) should have been evicted")
	}
}

// TestRouteCache_SetExistingMovesToFront verifies that re-setting an
// existing alias updates its position (and the value), not just the value.
// Catches a regression where Set on an existing key would skip the LRU
// bookkeeping.
func TestRouteCache_SetExistingMovesToFront(t *testing.T) {
	rc := newRouteCache()
	fillRouteCacheTo(rc, routeCacheMaxSize)

	// Re-set alias-0 (oldest). It should become MRU.
	rc.Set("alias-0", resolvedRoute{sandboxID: "sb-updated"})

	// Insert a fresh entry to trigger eviction.
	rc.Set("alias-trigger", resolvedRoute{sandboxID: "sb-trigger"})

	// alias-0 should still be present (it was MRU); alias-1 should not.
	if _, ok := rc.Get("alias-0"); !ok {
		t.Fatal("alias-0 evicted after Set-as-existing — Set didn't update LRU position")
	}
	if _, ok := rc.Get("alias-1"); ok {
		t.Fatal("alias-1 (LRU) should have been evicted")
	}
}

// TestRouteCache_InvalidateRemovesFromBothStructures verifies that
// Invalidate keeps the map and list consistent. A regression where
// only one is updated would silently grow the other unbounded.
func TestRouteCache_InvalidateRemovesFromBothStructures(t *testing.T) {
	rc := newRouteCache()
	rc.Set("a", resolvedRoute{sandboxID: "sb-a"})
	rc.Set("b", resolvedRoute{sandboxID: "sb-b"})

	rc.Invalidate("a")

	rc.mu.Lock()
	mapSize := len(rc.entries)
	listLen := rc.order.Len()
	rc.mu.Unlock()
	if mapSize != 1 || listLen != 1 {
		t.Fatalf("after invalidate: mapSize=%d listLen=%d, expected both 1", mapSize, listLen)
	}
}

// TestRouteCache_InvalidateSandboxRemovesFromBothStructures verifies the
// same invariant for the sandbox-scoped invalidator.
func TestRouteCache_InvalidateSandboxRemovesFromBothStructures(t *testing.T) {
	rc := newRouteCache()
	rc.Set("a", resolvedRoute{sandboxID: "sb-target"})
	rc.Set("b", resolvedRoute{sandboxID: "sb-target"})
	rc.Set("c", resolvedRoute{sandboxID: "sb-other"})

	rc.InvalidateSandbox("sb-target")

	rc.mu.Lock()
	mapSize := len(rc.entries)
	listLen := rc.order.Len()
	rc.mu.Unlock()
	if mapSize != 1 || listLen != 1 {
		t.Fatalf("after invalidate sandbox: mapSize=%d listLen=%d, expected both 1", mapSize, listLen)
	}
}

// --- publicRateLimiter ---

// TestPublicRateLimiter_SizeBoundEnforced — same invariant for the
// per-IP map. Insert distinct IPs past capacity; map must never exceed
// publicRateLimiterMaxSize.
func TestPublicRateLimiter_SizeBoundEnforced(t *testing.T) {
	rl := newPublicRateLimiter()

	for i := 0; i < publicRateLimiterMaxSize+2000; i++ {
		ip := fmt.Sprintf("10.0.%d.%d", i/256, i%256)
		rl.Allow("alias", ip)

		rl.mu.Lock()
		mapSize := len(rl.perIP)
		listLen := rl.perIPOrder.Len()
		rl.mu.Unlock()

		if mapSize > publicRateLimiterMaxSize {
			t.Fatalf("after %d Allow()s: per-IP map size %d > bound %d", i+1, mapSize, publicRateLimiterMaxSize)
		}
		if mapSize != listLen {
			t.Fatalf("per-IP map size %d != list size %d", mapSize, listLen)
		}
	}
}

// TestPublicRateLimiter_LRUEvictsLeastRecentlyUsed is the LRU regression.
// Fill to capacity, touch one entry, insert one more — the touched
// entry survives; the actual LRU is gone.
func TestPublicRateLimiter_LRUEvictsLeastRecentlyUsed(t *testing.T) {
	rl := newPublicRateLimiter()

	// Fill per-IP map to capacity with distinct IPs.
	for i := 0; i < publicRateLimiterMaxSize; i++ {
		rl.Allow("alias", fmt.Sprintf("10.1.%d.%d", i/256, i%256))
	}

	// Touch the oldest (IP 10.1.0.0) to make it MRU.
	rl.Allow("alias", "10.1.0.0")

	// Insert one more — should evict the now-oldest (the second
	// inserted: 10.1.0.1).
	rl.Allow("alias", "192.168.99.99")

	rl.mu.Lock()
	_, oldest := rl.perIP["10.1.0.0"]
	_, lru := rl.perIP["10.1.0.1"]
	rl.mu.Unlock()

	if !oldest {
		t.Fatal("10.1.0.0 was evicted despite being MRU; LRU broken")
	}
	if lru {
		t.Fatal("10.1.0.1 (the actual LRU) should have been evicted")
	}
}

// No wall-clock test for the O(N)→O(1) improvement directly: timing
// assertions are flake-prone on CI, and the algorithmic shape is
// self-evident from the diff (container/list back-pop vs map-wide
// scan). The LRU-semantics tests above pin down the new
// implementation; a future regression that reintroduces a map scan
// would still pass those, but it would also be a visible undo of the
// container/list approach — caught at code review, not by this file.
