package krucible

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// slotEngine has nothing but netd bookkeeping: acquiring a slot spawns no gateway.
func slotEngine(t *testing.T) *Engine {
	t.Helper()
	return &Engine{cfg: Config{DataDir: t.TempDir(), SocketDir: shortSockDir(t)}, vms: map[string]*VM{}, netds: map[string]*netdInstance{}}
}

// checkGuestIdentity fails unless slot idx maps to a host address on the /24
// and a well-formed MAC, the two things netd and the VMM accept.
func checkGuestIdentity(t *testing.T, subnet, idx int) {
	t.Helper()
	ip, err := netip.ParseAddr(netGuestIPFor(subnet, idx))
	if err != nil || ip.As4()[3] < 2 || ip.As4()[3] > 254 {
		t.Fatalf("slot %d: guest IP %q is not a host address on the /24", idx, netGuestIPFor(subnet, idx))
	}
	if hw, err := net.ParseMAC(netGuestMACFor(idx)); err != nil || len(hw) != 6 {
		t.Fatalf("slot %d: guest MAC %q is malformed", idx, netGuestMACFor(idx))
	}
}

func fillSlots(t *testing.T, e *Engine, owner string, n int) []string {
	t.Helper()
	var ips []string
	for i := range n {
		inst, idx, err := e.acquireNetd(owner, 1)
		if err != nil {
			t.Fatalf("acquire %d of %d: %v", i+1, n, err)
		}
		checkGuestIdentity(t, inst.subnetIdx, idx)
		ips = append(ips, netGuestIPFor(inst.subnetIdx, idx))
	}
	return ips
}

func TestGuestSlotsAreBoundedAndReused(t *testing.T) {
	e := slotEngine(t)
	const owner = "u:owner"
	ips := fillSlots(t, e, owner, netGuestSlots)
	seen := map[string]bool{}
	for _, ip := range ips {
		if seen[ip] {
			t.Fatalf("%s handed out twice", ip)
		}
		seen[ip] = true
	}

	// Every address is taken: refuse clearly, without leaking a reference.
	if _, _, err := e.acquireNetd(owner, 1); !errors.Is(err, engine.ErrNetworkFull) || !strings.Contains(err.Error(), "253") {
		t.Fatalf("acquire on a full /24: %v, want ErrNetworkFull naming the limit", err)
	}
	if refs := e.netds[owner].refs; refs != netGuestSlots {
		t.Fatalf("refs after a refused acquire = %d, want %d", refs, netGuestSlots)
	}

	// A released address is handed out again.
	e.releaseNetd(owner, 17)
	if _, idx, err := e.acquireNetd(owner, 1); err != nil || idx != 17 {
		t.Fatalf("acquire after freeing slot 17 = %d, %v; want 17", idx, err)
	}
}

// agni-02, 2026-10-27: the guest counter only went up, so after a few hundred
// short-lived forks every new address was past .254 although only three
// sandboxes held one. Recovery rebuilds what is held from the VMs and hands out
// the rest, never an address a sandbox still holds.
func TestRecoveredSlotsAreNotReissued(t *testing.T) {
	e := slotEngine(t)
	const owner = "u:usr_b727140a"
	dir := e.netdDir(owner)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	rec, _ := json.Marshal(netdRecord{Owner: owner, Dir: dir, SubnetIdx: 1, NextGuest: 313})
	if err := os.WriteFile(netdStatePath(dir), rec, 0o600); err != nil {
		t.Fatal(err)
	}
	held := []string{"100.64.1.2", "100.64.1.62", "100.64.1.245"}
	for _, ip := range held {
		e.readoptNetd(&VM{netdKey: owner, subnetIdx: 1, netIP: ip})
	}

	taken := map[string]bool{}
	for _, ip := range held {
		taken[ip] = true
	}
	for _, ip := range fillSlots(t, e, owner, netGuestSlots-len(held)) {
		if taken[ip] {
			t.Fatalf("handed out %s, which a sandbox still holds", ip)
		}
		taken[ip] = true
	}
	if _, _, err := e.acquireNetd(owner, 1); !errors.Is(err, engine.ErrNetworkFull) {
		t.Fatalf("acquire with every address held: %v, want ErrNetworkFull", err)
	}
}

func TestDestroyFreesItsGuestSlot(t *testing.T) {
	e := slotEngine(t)
	const owner = "u:owner"
	ips := fillSlots(t, e, owner, netGuestSlots)
	vm := &VM{ID: "gone", netdKey: owner, subnetIdx: 1, netIP: ips[40], SandboxDir: t.TempDir(), SockDir: t.TempDir(), Status: "stopped"}
	e.vms[vm.ID] = vm
	if err := e.Destroy(context.Background(), vm.ID); err != nil {
		t.Fatal(err)
	}
	inst, idx, err := e.acquireNetd(owner, 1)
	if err != nil || netGuestIPFor(inst.subnetIdx, idx) != ips[40] {
		t.Fatalf("acquire after destroying the sandbox at %s: slot %d, %v", ips[40], idx, err)
	}
}
