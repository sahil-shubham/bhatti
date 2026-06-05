//go:build linux

package firecracker

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/dns"
)

// Tests for the DNS lifecycle glue (G1.1 chunk 2). The dns package
// itself is heavily tested in isolation; this file covers the
// engine-side wiring: dnsSet/dnsDelete routing, seedDNS pre-population,
// idempotency of the lifecycle helpers.
//
// The ensureUserBridge bring-up runs real `ip` commands and is not
// covered here — the lifecycle tests in network_test.go already
// exercise that, gated on os.Geteuid()==0. What we test here is the
// portable Go logic that's reachable on every dev machine.

// newTestEngine returns an Engine with just enough state set up for
// the DNS helpers to work: empty userNetworks + vms, fresh ctx.
// Does NOT call New() because that needs disk + iptables.
func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &Engine{
		vms:          make(map[string]*VM),
		userNetworks: make(map[string]*UserNetwork),
		ctx:          ctx,
		cancel:       cancel,
	}
}

// withTestDNS attaches a real DNS server bound to a kernel-assigned
// local port. We can't bind to 10.0.N.1:53 in tests (no bridge, would
// need root), so we use 127.0.0.1:0 — the routing logic (which user's
// DNS to call) is what we're testing.
func withTestDNS(t *testing.T, un *UserNetwork) {
	t.Helper()
	s := dns.NewServer("127.0.0.1:0")
	s.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	if err := s.Start(ctx); err != nil {
		t.Fatalf("dns server start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		s.Stop()
	})
	un.DNS = s
}

func TestEngine_DNSSet_RoutesToCorrectUser(t *testing.T) {
	e := newTestEngine(t)

	// Two users, each with a DNS server.
	alpha := &UserNetwork{BridgeName: "brbhatti-1", GatewayIP: "10.0.1.1"}
	beta := &UserNetwork{BridgeName: "brbhatti-2", GatewayIP: "10.0.2.1"}
	withTestDNS(t, alpha)
	withTestDNS(t, beta)
	e.userNetworks["usr_a"] = alpha
	e.userNetworks["usr_b"] = beta

	e.dnsSet("usr_a", "alpha-host", "10.0.1.5")
	e.dnsSet("usr_b", "beta-host", "10.0.2.5")

	if ip, ok := alpha.DNS.Lookup("alpha-host"); !ok || !ip.Equal(net.IPv4(10, 0, 1, 5)) {
		t.Errorf("alpha zone: ok=%v ip=%v", ok, ip)
	}
	if ip, ok := beta.DNS.Lookup("beta-host"); !ok || !ip.Equal(net.IPv4(10, 0, 2, 5)) {
		t.Errorf("beta zone: ok=%v ip=%v", ok, ip)
	}
	// Cross-user: alpha must NOT see beta's name and vice versa.
	if _, ok := alpha.DNS.Lookup("beta-host"); ok {
		t.Error("alpha should not see beta-host")
	}
	if _, ok := beta.DNS.Lookup("alpha-host"); ok {
		t.Error("beta should not see alpha-host")
	}
}

func TestEngine_DNSDelete_RemovesEntry(t *testing.T) {
	e := newTestEngine(t)
	un := &UserNetwork{BridgeName: "brbhatti-1", GatewayIP: "10.0.1.1"}
	withTestDNS(t, un)
	e.userNetworks["usr"] = un

	e.dnsSet("usr", "host", "10.0.1.5")
	if _, ok := un.DNS.Lookup("host"); !ok {
		t.Fatal("Set should have populated the entry")
	}
	e.dnsDelete("usr", "host")
	if _, ok := un.DNS.Lookup("host"); ok {
		t.Fatal("Delete should have removed the entry")
	}
}

func TestEngine_DNSSet_NoOpForUnknownUser(t *testing.T) {
	// A user that doesn't have a UserNetwork (yet?) — dnsSet must not
	// panic, just no-op.
	e := newTestEngine(t)
	e.dnsSet("usr_ghost", "host", "10.0.1.5")
	// Pass: didn't panic.
}

func TestEngine_DNSSet_NoOpWhenDNSServerNil(t *testing.T) {
	// Network exists but the DNS bind failed (or never ran). dnsSet
	// must tolerate this silently — bind failures are non-fatal.
	e := newTestEngine(t)
	un := &UserNetwork{BridgeName: "brbhatti-1", GatewayIP: "10.0.1.1"}
	// un.DNS deliberately nil
	e.userNetworks["usr"] = un
	e.dnsSet("usr", "host", "10.0.1.5")
	// Pass: didn't panic.
}

func TestEngine_DNSSet_InvalidIPIgnored(t *testing.T) {
	e := newTestEngine(t)
	un := &UserNetwork{BridgeName: "brbhatti-1", GatewayIP: "10.0.1.1"}
	withTestDNS(t, un)
	e.userNetworks["usr"] = un

	e.dnsSet("usr", "host", "not-an-ip")
	if _, ok := un.DNS.Lookup("host"); ok {
		t.Fatal("Set with invalid IP should not have populated the entry")
	}
}

// TestEngine_SeedDNS_PrePopulatesFromRecoveredVMs covers the recovery
// path: VMs are loaded into e.vms before any bridge is brought up
// (RestoreVM doesn't call bringUpUserNetwork). The FIRST bringUpUserNetwork
// call seeds the DNS zone from e.vms — without this, peer sandboxes
// can't resolve recovered names until each is individually woken.
func TestEngine_SeedDNS_PrePopulatesFromRecoveredVMs(t *testing.T) {
	e := newTestEngine(t)
	un := &UserNetwork{BridgeName: "brbhatti-1", GatewayIP: "10.0.1.1"}
	e.userNetworks["usr_a"] = un

	// Populate e.vms as recovery would: VMs exist, bridge not up yet.
	e.vms["sb_alpha"] = &VM{ID: "sb_alpha", Name: "alpha", UserID: "usr_a", GuestIP: "10.0.1.2"}
	e.vms["sb_beta"] = &VM{ID: "sb_beta", Name: "beta", UserID: "usr_a", GuestIP: "10.0.1.3"}
	// A VM from a DIFFERENT user must NOT leak into usr_a's zone.
	e.vms["sb_gamma"] = &VM{ID: "sb_gamma", Name: "gamma", UserID: "usr_b", GuestIP: "10.0.2.2"}
	other := &UserNetwork{BridgeName: "brbhatti-2", GatewayIP: "10.0.2.1"}
	e.userNetworks["usr_b"] = other

	withTestDNS(t, un)
	e.seedDNS(un)

	if ip, ok := un.DNS.Lookup("alpha"); !ok || !ip.Equal(net.IPv4(10, 0, 1, 2)) {
		t.Errorf("alpha: ok=%v ip=%v", ok, ip)
	}
	if ip, ok := un.DNS.Lookup("beta"); !ok || !ip.Equal(net.IPv4(10, 0, 1, 3)) {
		t.Errorf("beta: ok=%v ip=%v", ok, ip)
	}
	if _, ok := un.DNS.Lookup("gamma"); ok {
		t.Errorf("gamma should NOT be in usr_a's zone (it belongs to usr_b)")
	}
}

// TestEngine_SeedDNS_SkipsVMsWithMissingFields exercises the
// defensive checks (no name, no IP, no userID) — a partially-constructed
// VM in e.vms during a recovery hiccup shouldn't crash the seed loop.
func TestEngine_SeedDNS_SkipsVMsWithMissingFields(t *testing.T) {
	e := newTestEngine(t)
	un := &UserNetwork{BridgeName: "brbhatti-1", GatewayIP: "10.0.1.1"}
	e.userNetworks["usr_a"] = un

	e.vms["sb1"] = &VM{ID: "sb1", Name: "", UserID: "usr_a", GuestIP: "10.0.1.2"} // no name
	e.vms["sb2"] = &VM{ID: "sb2", Name: "x", UserID: "", GuestIP: "10.0.1.3"}     // no user
	e.vms["sb3"] = &VM{ID: "sb3", Name: "y", UserID: "usr_a", GuestIP: ""}        // no IP
	e.vms["sb4"] = &VM{ID: "sb4", Name: "z", UserID: "usr_a", GuestIP: "10.0.1.4"} // good

	withTestDNS(t, un)
	e.seedDNS(un)

	names := un.DNS.Names()
	if len(names) != 1 || names[0] != "z" {
		t.Fatalf("seed populated wrong entries: %v", names)
	}
}

// TestEngine_DNSSet_Concurrent verifies that the Engine.mu lock
// discipline holds under concurrent dnsSet/dnsDelete calls.
// `-race` would catch a missing lock.
func TestEngine_DNSSet_Concurrent(t *testing.T) {
	e := newTestEngine(t)
	un := &UserNetwork{BridgeName: "brbhatti-1", GatewayIP: "10.0.1.1"}
	withTestDNS(t, un)
	e.userNetworks["usr"] = un

	const goroutines = 16
	const perGoroutine = 100
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				name := "host"
				ip := "10.0.1." + itoaSlow(g*perGoroutine+i)
				if (i & 1) == 0 {
					e.dnsSet("usr", name, ip)
				} else {
					e.dnsDelete("usr", name)
				}
			}
		}(g)
	}
	wg.Wait()
}

// itoaSlow is a no-dependency int-to-string for the test loop above.
// Just enough to satisfy net.ParseIP("10.0.1.<n>") for n up to 254.
func itoaSlow(i int) string {
	if i < 0 || i > 254 {
		// Test loop generates 16*100=1600 values, but we constrain to
		// 0..254 (legal last octet) by mod.
		i = i % 255
	}
	if i == 0 {
		return "0"
	}
	var buf [3]byte
	pos := 3
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}

// TestEngine_DNSEndToEndOverRealBridge is the integration test the
// rest of dns_test.go can't be: real bridge interface, real bind to
// the bridge gateway on port 53, real UDP query over the network.
// Mirrors the runtime path a sandbox would hit when its libresolv
// queries 10.0.N.1:53. Gated on root for ip link / iptables / port 53.
//
// Lives alongside the rest of the DNS glue tests but only runs on
// arc-runner-set (the integration runner). The CI step that runs
// `go test ./pkg/engine/firecracker/` with sudo picks it up.
func TestEngine_DNSEndToEndOverRealBridge(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("must run as root (creates bridge + binds port 53)")
	}
	// Docker-as-root without iproute2 (e.g. plain golang:1.25 dev
	// containers) lands here. Skip rather than fail on the missing
	// `ip` invocation — the test belongs on the integration runner
	// which installs iproute2 + iptables explicitly.
	if _, err := exec.LookPath("ip"); err != nil {
		t.Skip("iproute2 not installed; integration runner has it")
	}
	if _, err := exec.LookPath("iptables"); err != nil {
		t.Skip("iptables not installed; integration runner has it")
	}

	e := newTestEngine(t)
	// Use 10.99.99.0/24 — the test subnet TestUserBridgeLifecycle
	// already conventions on. Won't collide with anything in normal
	// use. Bridge name must fit in Linux's 15-char IFNAMSIZ minus the
	// NUL terminator; "brbhatti-d99" is 12 chars.
	un := &UserNetwork{
		BridgeName: "brbhatti-d99",
		GatewayIP:  "10.99.99.1",
		Subnet:     "10.99.99.0/24",
		Pool:       newIPPool("10.99.99.1"),
	}
	e.userNetworks["usr_dnstest"] = un

	// Pre-populate a VM so seedDNS has work to do (the recovery-shaped
	// case where bringUpUserNetwork must populate from e.vms).
	e.vms["sb_dnstest"] = &VM{
		ID: "sb_dnstest", Name: "alpha", UserID: "usr_dnstest",
		GuestIP: "10.99.99.7",
	}

	// Bring up the bridge + start DNS via the actual production path.
	if err := e.bringUpUserNetwork(un); err != nil {
		t.Fatalf("bringUpUserNetwork: %v", err)
	}
	t.Cleanup(func() {
		stopDNSForBridge(un.DNS)
		// Cleanup bridge + the per-bridge iptables ACCEPT that
		// ensureUserBridge installed. Both are idempotent on absence.
		destroyUserBridge(un.BridgeName)
	})
	if un.DNS == nil {
		t.Fatal("bringUpUserNetwork should have started the DNS server")
	}

	// Query the responder over the network. The gateway IP is bound
	// to the bridge interface on this host, so a UDP packet to
	// 10.99.99.1:53 reaches the responder via the loopback path on
	// the bridge.
	query := makeDNSQuery(t, "alpha", 1)
	resp := udpQuery(t, "10.99.99.1:53", query, 2*time.Second)
	if resp == nil {
		// On failure dump iptables + interface state — this is what
		// caught the 412d82a slice-ordering inversion. The packet was
		// matching the DROP NEW rule (counter went 0 → 1) instead of
		// the port-53 ACCEPT (still 0), proving the ACCEPTs were below
		// the DROP in the chain. Keep these dumps for the next time
		// this test starts mysteriously timing out.
		dumpDiag := func(label string, name string, args ...string) {
			out, err := exec.Command(name, args...).CombinedOutput()
			t.Logf("=== %s ===\n%s (err=%v)", label, string(out), err)
		}
		dumpDiag("iptables INPUT", "iptables", "-L", "INPUT", "-v", "-n", "--line-numbers")
		dumpDiag("iptables FORWARD", "iptables", "-L", "FORWARD", "-v", "-n", "--line-numbers")
		dumpDiag("ip addr", "ip", "-d", "addr", "show", "brbhatti-d99")
		dumpDiag("ip route", "ip", "route")
		dumpDiag("ss", "sh", "-c", "ss -tunlp 2>&1 | head -20")
		dumpDiag("sysctl rp_filter", "sh", "-c",
			"sysctl net.ipv4.conf.all.rp_filter net.ipv4.conf.brbhatti-d99.rp_filter net.ipv4.conf.lo.rp_filter 2>&1")
		t.Fatal("no DNS response (see diagnostics above)")
	}

	// Last 4 bytes of an A response = IPv4 RDATA. Easier than re-
	// parsing the whole message and equivalent to what the dns/
	// package tests already verify.
	if len(resp) < 4 {
		t.Fatalf("response too short: %v", resp)
	}
	tail := resp[len(resp)-4:]
	want := []byte{10, 99, 99, 7}
	for i := 0; i < 4; i++ {
		if tail[i] != want[i] {
			t.Fatalf("A RDATA: got %v want %v (full response: %v)", tail, want, resp)
		}
	}

	// Sandbox added AFTER bringUpUserNetwork (the Engine.Create case)
	// must also show up via dnsSet.
	e.vms["sb_late"] = &VM{
		ID: "sb_late", Name: "beta", UserID: "usr_dnstest",
		GuestIP: "10.99.99.8",
	}
	e.dnsSet("usr_dnstest", "beta", "10.99.99.8")
	resp = udpQuery(t, "10.99.99.1:53", makeDNSQuery(t, "beta", 2), 2*time.Second)
	if resp == nil {
		t.Fatal("no DNS response for late-registered name")
	}
	tail = resp[len(resp)-4:]
	want = []byte{10, 99, 99, 8}
	for i := 0; i < 4; i++ {
		if tail[i] != want[i] {
			t.Fatalf("late A RDATA: got %v want %v", tail, want)
		}
	}

	// Destroy path: dnsDelete should make the name un-resolvable
	// before the actual VM teardown happens (the Engine.Destroy
	// ordering). NXDOMAIN, not silent failure.
	e.dnsDelete("usr_dnstest", "alpha")
	resp = udpQuery(t, "10.99.99.1:53", makeDNSQuery(t, "alpha", 3), 2*time.Second)
	if resp == nil {
		t.Fatal("no DNS response after dnsDelete (server should still respond, just NXDOMAIN)")
	}
	// Header byte 3 (low byte of flags) holds the RCODE in the bottom
	// 4 bits. NXDOMAIN = 3.
	if rcode := resp[3] & 0x0F; rcode != 3 {
		t.Errorf("after dnsDelete: RCODE=%d, want 3 (NXDOMAIN); response=%v", rcode, resp)
	}
}

// makeDNSQuery builds a minimal A-query for the given name. ID is
// echoed by the server so we can correlate responses if needed.
func makeDNSQuery(t *testing.T, name string, id uint16) []byte {
	t.Helper()
	// 12-byte header + name + 4-byte (qtype,qclass).
	var buf []byte
	hdr := make([]byte, 12)
	binary.BigEndian.PutUint16(hdr[0:2], id)
	binary.BigEndian.PutUint16(hdr[2:4], 0x0100) // RD=1
	binary.BigEndian.PutUint16(hdr[4:6], 1)      // QDCOUNT
	buf = append(buf, hdr...)

	// Encode name as length-prefixed labels.
	for _, label := range splitLabels(name) {
		buf = append(buf, byte(len(label)))
		buf = append(buf, []byte(label)...)
	}
	buf = append(buf, 0) // root label

	var qtail [4]byte
	binary.BigEndian.PutUint16(qtail[0:2], 1) // QType A
	binary.BigEndian.PutUint16(qtail[2:4], 1) // QClass IN
	buf = append(buf, qtail[:]...)
	return buf
}

func splitLabels(name string) []string {
	var out []string
	start := 0
	for i := 0; i < len(name); i++ {
		if name[i] == '.' {
			out = append(out, name[start:i])
			start = i + 1
		}
	}
	if start < len(name) {
		out = append(out, name[start:])
	}
	return out
}

func udpQuery(t *testing.T, addr string, query []byte, timeout time.Duration) []byte {
	t.Helper()
	conn, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write(query); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp := make([]byte, 512)
	n, err := conn.Read(resp)
	if err != nil {
		return nil
	}
	return resp[:n]
}


