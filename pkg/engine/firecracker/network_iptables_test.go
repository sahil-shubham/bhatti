//go:build linux

package firecracker

import (
	"reflect"
	"testing"
)

// Regression for the intra-bridge ACCEPT exemption that lets same-user
// sandbox-to-sandbox traffic survive on hosts where br_netfilter is
// loaded (bridge-nf-call-iptables=1).
//
// Surfaced during the G1.3 kubelet spike on asus-i5 (a k3s node that
// had loaded br_netfilter). The bhatti FORWARD chain's
// "10.0.0.0/8 -> 10.0.0.0/8 DROP" rule was murdering k3s worker → k3s
// control-plane traffic that should have been L2-switched intra-bridge.
//
// The fix installs a per-bridge ACCEPT at FORWARD position 1 in
// ensureUserBridge and removes it in destroyUserBridge. This test pins
// down the predicate shape so a future refactor that drops the -i/-o
// match (and lets cross-bridge traffic through, defeating the user
// isolation) is caught at code review.
func TestIntraBridgeAllowPredicate(t *testing.T) {
	got := intraBridgeAllowPredicate("brbhatti-7")
	want := []string{"-i", "brbhatti-7", "-o", "brbhatti-7", "-j", "ACCEPT"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	// The predicate MUST scope to a single bridge via both -i and -o.
	// Removing either match would either allow cross-bridge traffic
	// (breaking user isolation) or shadow the global DROP rule globally
	// (same effect). Spot-check by name to make this regression-test-
	// shaped rather than a tautology.
	sawIn, sawOut := false, false
	for i, arg := range got {
		if arg == "-i" && i+1 < len(got) && got[i+1] == "brbhatti-7" {
			sawIn = true
		}
		if arg == "-o" && i+1 < len(got) && got[i+1] == "brbhatti-7" {
			sawOut = true
		}
	}
	if !sawIn || !sawOut {
		t.Fatalf("predicate must constrain BOTH -i and -o to the same bridge: %v", got)
	}
}

// TestIntraBridgeAllowPredicate_DifferentBridgesDontOverlap ensures
// each user's bridge gets its own rule with no cross-bridge accidents.
func TestIntraBridgeAllowPredicate_DifferentBridgesDontOverlap(t *testing.T) {
	a := intraBridgeAllowPredicate("brbhatti-1")
	b := intraBridgeAllowPredicate("brbhatti-2")

	if reflect.DeepEqual(a, b) {
		t.Fatal("predicates for different bridges must differ")
	}
	// Sanity: neither predicate references the OTHER bridge.
	for _, arg := range a {
		if arg == "brbhatti-2" {
			t.Fatalf("user-1 predicate references user-2 bridge: %v", a)
		}
	}
	for _, arg := range b {
		if arg == "brbhatti-1" {
			t.Fatalf("user-2 predicate references user-1 bridge: %v", b)
		}
	}
}
