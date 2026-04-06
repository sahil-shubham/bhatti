//go:build linux

package firecracker

import (
	"testing"
)

// ---------------------------------------------------------------------------
// Bug #4: IP pool corruption on TryAllocate failure in ResumeSnapshot.
//
// The bug: ResumeSnapshot sets guestIP = manifest.Network.GuestIP BEFORE
// calling TryAllocate. If TryAllocate fails, the cleanup defer sees a
// non-empty guestIP and calls pool.Release() on an IP that was never
// allocated — freeing another sandbox's IP.
//
// These tests verify pool invariants that the fix must satisfy.
// The integration test (TestResumeIPConflictNoPoolCorruption) tests the
// actual ResumeSnapshot code path end-to-end.
// ---------------------------------------------------------------------------

// TestIPPoolTryAllocateRejectsInUse verifies that TryAllocate returns an
// error when the requested IP is already allocated by another sandbox.
func TestIPPoolTryAllocateRejectsInUse(t *testing.T) {
	p := newIPPool("10.0.99.1")

	// Sandbox A allocates .2
	ipA, err := p.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if ipA != "10.0.99.2" {
		t.Fatalf("expected 10.0.99.2, got %s", ipA)
	}

	// TryAllocate .2 must fail — it's in use
	err = p.TryAllocate("10.0.99.2")
	if err == nil {
		t.Fatal("TryAllocate should reject an in-use IP")
	}

	// After the failed TryAllocate, .2 must STILL be allocated to A.
	// Verify by allocating the next IP — it should be .3, not .2.
	ipB, err := p.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if ipB != "10.0.99.3" {
		t.Fatalf("expected .3 (next free), got %s — TryAllocate failure corrupted the pool", ipB)
	}
	t.Logf("✓ TryAllocate failure didn't corrupt pool: A=%s, next=%s", ipA, ipB)
}

// TestIPPoolReleaseOnlyOwned verifies that Release on an IP that is
// legitimately in use by another caller frees it (i.e., Release has no
// ownership check — it's the CALLER's responsibility not to release
// IPs they don't own). This documents why Bug #4 matters: the pool
// can't protect against this; the caller must get it right.
func TestIPPoolReleaseOnlyOwned(t *testing.T) {
	p := newIPPool("10.0.99.1")

	// A allocates .2
	ipA, _ := p.Allocate()

	// Releasing .2 (even though we're "B") frees it — pool has no ownership
	p.Release(ipA)

	// .2 is now free and will be handed out again
	ipNext, _ := p.Allocate()
	if ipNext != ipA {
		t.Fatalf("expected %s to be re-allocatable after Release, got %s", ipA, ipNext)
	}

	// This confirms: Release is unconditional. The fix for Bug #4 must
	// prevent the cleanup defer from calling Release on an IP that wasn't
	// successfully allocated, because the pool itself can't stop it.
	t.Log("✓ Release is unconditional — caller must only release IPs they own")
}

// TestIPPoolTryAllocateSuccessThenRelease verifies the correct cleanup
// path: TryAllocate succeeds, then Release correctly frees the IP.
func TestIPPoolTryAllocateSuccessThenRelease(t *testing.T) {
	p := newIPPool("10.0.99.1")

	// TryAllocate .5 (nothing else using it)
	err := p.TryAllocate("10.0.99.5")
	if err != nil {
		t.Fatalf("TryAllocate .5: %v", err)
	}

	// .5 is now allocated — sequential Allocate should skip it
	ip1, _ := p.Allocate()
	if ip1 != "10.0.99.2" {
		t.Fatalf("expected .2, got %s", ip1)
	}

	// Release .5 (simulating cleanup after a failed resume that DID
	// successfully allocate the IP before failing at a later step)
	p.Release("10.0.99.5")

	// .5 should be available again
	ip2, _ := p.Allocate() // .3
	ip3, _ := p.Allocate() // .4
	ip4, _ := p.Allocate() // .5 (now free)
	if ip4 != "10.0.99.5" {
		t.Fatalf("expected .5 after release, got %s (got .3=%s .4=%s)", ip4, ip2, ip3)
	}
	t.Log("✓ TryAllocate + Release round-trip works correctly")
}
