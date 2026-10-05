package main

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/gateway"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

var (
	testGwIP     = [4]byte{100, 64, 0, 1}
	testGuestIP  = [4]byte{100, 64, 0, 2}
	testGwMAC    = tcpip.LinkAddress("\x52\x54\x00\x00\x00\x01")
	testGuestMAC = tcpip.LinkAddress("\x52\x54\x00\x00\x00\x02")
	testNetToken = strings.Repeat("ab", 32)
)

// arpRequestFrame builds "who has gwIP? tell guestIP/guestMAC" (broadcast).
func arpRequestFrame() []byte {
	eth := make([]byte, header.EthernetMinimumSize)
	header.Ethernet(eth).Encode(&header.EthernetFields{
		SrcAddr: testGuestMAC,
		DstAddr: header.EthernetBroadcastAddress,
		Type:    header.ARPProtocolNumber,
	})
	a := make([]byte, header.ARPSize)
	arpv := header.ARP(a)
	arpv.SetIPv4OverEthernet()
	arpv.SetOp(header.ARPRequest)
	copy(arpv.HardwareAddressSender(), testGuestMAC)
	copy(arpv.ProtocolAddressSender(), testGuestIP[:])
	copy(arpv.ProtocolAddressTarget(), testGwIP[:])
	return append(eth, a...)
}

// assertARPReply verifies frame is an ARP reply from the gateway.
func assertARPReply(t *testing.T, frame []byte) {
	t.Helper()
	if len(frame) < header.EthernetMinimumSize+header.ARPSize {
		t.Fatalf("reply too short: %d bytes", len(frame))
	}
	if et := header.Ethernet(frame).Type(); et != header.ARPProtocolNumber {
		t.Fatalf("reply ethertype = %#x, want ARP", et)
	}
	arpR := header.ARP(frame[header.EthernetMinimumSize:])
	if arpR.Op() != header.ARPReply {
		t.Fatalf("ARP op = %d, want reply", arpR.Op())
	}
	if got := tcpip.LinkAddress(arpR.HardwareAddressSender()); got != testGwMAC {
		t.Fatalf("ARP reply sender MAC = %x, want gateway %x", got, testGwMAC)
	}
	if got := [4]byte(arpR.ProtocolAddressSender()); got != testGwIP {
		t.Fatalf("ARP reply sender IP = %v, want gateway %v", got, testGwIP)
	}
}

// readFrameCtx reads one frame with a deadline (net.Pipe/unix reads can hang).
func readFrameCtx(t *testing.T, fc *gateway.FrameConn, d time.Duration) []byte {
	t.Helper()
	type res struct {
		f   []byte
		err error
	}
	ch := make(chan res, 1)
	go func() { f, err := fc.ReadFrame(); ch <- res{f, err} }()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("ReadFrame: %v", r.err)
		}
		return r.f
	case <-time.After(d):
		t.Fatal("no frame within deadline")
		return nil
	}
}

// TestGatewayAnswersARP exercises the bridge in isolation (in-memory pipe):
// FrameConn read → InjectInbound → ethernet parse → netstack ARP → outbound →
// FrameConn write.
func TestGatewayAnswersARP(t *testing.T) {
	guestSide, netdSide := net.Pipe()
	defer guestSide.Close()
	defer netdSide.Close()

	gw, err := NewGateway(tcpip.AddrFrom4(testGwIP), 24, testGwMAC, hostProtection{})
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	if err := gw.SetSandbox("100.64.0.2", "sb1", nil, "52:54:00:00:00:02", testNetToken); err != nil {
		t.Fatal(err)
	}
	gw.AddGuest(gateway.NewFrameConn(netdSide), tcpip.AddrFrom4(testGuestIP), testGuestMAC, testNetToken)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go gw.Run(ctx)

	guest := gateway.NewFrameConn(guestSide)
	if err := guest.WriteFrame(arpRequestFrame()); err != nil {
		t.Fatalf("send ARP request: %v", err)
	}
	assertARPReply(t, readFrameCtx(t, guest, 4*time.Second))
}

// TestGatewayOverUnixSocket exercises the REAL path libkrun uses: netd LISTENS
// on a unix socket, the peer (libkrun, here a test client) CONNECTS, and the
// gateway serves it. Guards the listen/accept wiring + the connect direction
// (the bug this replaced: netd was dialing instead of listening).
func TestGatewayOverUnixSocket(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "bnet")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "net.sock")
	ctl := filepath.Join(dir, "ctl.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctlLn, err := net.Listen("unix", ctl)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	served := make(chan error, 1)
	go func() {
		served <- serve(ctx, ln, gwConfig{ip: tcpip.AddrFrom4(testGwIP), prefix: 24, mac: testGwMAC}, ctlLn, nil)
	}()
	client := gateway.NewControlClient(ctl)
	defer client.Close()
	if err := client.Send(gateway.ControlMsg{Op: gateway.ControlSet, GuestIP: "100.64.0.2",
		GuestMAC: "52:54:00:00:00:02", GuestToken: testNetToken, Sandbox: "sb1",
		Policy: &gateway.NetPolicyWire{Default: "deny"}}); err != nil {
		t.Fatal(err)
	}

	// Peer connects (as libkrun would).
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial %s: %v", sock, err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(testNetToken)); err != nil {
		t.Fatal(err)
	}

	guest := gateway.NewFrameConn(conn)
	if err := guest.WriteFrame(arpRequestFrame()); err != nil {
		t.Fatalf("send ARP request: %v", err)
	}
	assertARPReply(t, readFrameCtx(t, guest, 4*time.Second))

	cancel()
	select {
	case <-served:
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not return after ctx cancel")
	}
}

// TestIsSibling checks the sibling classifier: an address in the owner's guest
// subnet (but not the gateway) is routed via the stack; the gateway itself and
// out-of-subnet addresses are not.
func TestIsSibling(t *testing.T) {
	gw, err := NewGateway(tcpip.AddrFrom4(testGwIP), 24, testGwMAC, hostProtection{})
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	cases := []struct {
		addr [4]byte
		want bool
	}{
		{[4]byte{100, 64, 0, 2}, true},   // a sibling
		{[4]byte{100, 64, 0, 254}, true}, // another sibling
		{[4]byte{100, 64, 0, 1}, false},  // the gateway itself
		{[4]byte{1, 1, 1, 1}, false},     // public internet
		{[4]byte{100, 64, 1, 2}, false},  // a different owner's subnet
	}
	for _, c := range cases {
		if got := gw.isSibling(tcpip.AddrFrom4(c.addr)); got != c.want {
			t.Errorf("isSibling(%v) = %v, want %v", c.addr, got, c.want)
		}
	}
}

func TestSandboxHardDenySurvivesBroadCIDRs(t *testing.T) {
	// The /24 mask on the interface must not hard-deny its whole subnet.
	iface := &net.IPNet{IP: net.ParseIP("203.0.113.42"), Mask: net.CIDRMask(24, 32)}
	protected, err := protectHost([]net.Addr{iface}, []string{"198.51.100.10:8080", "0.0.0.0:443"})
	if err != nil {
		t.Fatal(err)
	}
	gw, err := NewGateway(tcpip.AddrFrom4(testGwIP), 24, testGwMAC, protected)
	if err != nil {
		t.Fatal(err)
	}
	for _, cidr := range []string{"100.64.0.0/10", "0.0.0.0/0"} {
		t.Run(cidr, func(t *testing.T) {
			p, err := gateway.PolicyFromWire(gateway.NetPolicyWire{Default: "public", AllowCIDRs: []string{cidr}})
			if err != nil {
				t.Fatal(err)
			}
			gw.SetSandbox("100.64.0.2", "sb1", p, "52:54:00:00:00:02", testNetToken)
			st := gw.stateFor(tcpip.AddrFrom4(testGuestIP))
			for _, dest := range []string{
				"203.0.113.42",  // host interface
				"198.51.100.10", // daemon listener
				"100.64.0.1",    // owner's gateway
				"100.64.1.2",    // another owner's guest
				"100.127.255.254",
			} {
				v := st.pol.Check("", netip.MustParseAddr(dest))
				if v.Allow || !strings.Contains(v.Reason, "hard-denied") {
					t.Errorf("%s did not hard-deny protected destination %s: %+v", cidr, dest, v)
				}
			}
			for second := 64; second <= 127; second++ {
				for third := 0; third <= 255; third++ {
					if second == 64 && third == 0 {
						continue // this owner's /24
					}
					ip := netip.AddrFrom4([4]byte{100, byte(second), byte(third), 2})
					v := st.pol.Check("", ip)
					if v.Allow || !strings.Contains(v.Reason, "hard-denied") {
						t.Fatalf("%s reached another owner's /24 via %s: %+v", cidr, ip, v)
					}
				}
			}
			if v := st.pol.Check("", netip.MustParseAddr("203.0.113.43")); !v.Allow {
				t.Errorf("%s denied neighboring public address: %+v", cidr, v)
			}
			if v := st.pol.Check("", netip.MustParseAddr("10.1.2.3")); v.Allow != (cidr == "0.0.0.0/0") {
				t.Errorf("%s wrong private-range verdict: %+v", cidr, v)
			}
		})
	}
}

// A guest reaches its node's bhatti API/published URLs the way the internet
// does, under its own policy; nothing else on the host opens with them.
func TestHostServicesReachableOnlyOnDaemonPorts(t *testing.T) {
	iface := &net.IPNet{IP: net.ParseIP("203.0.113.42"), Mask: net.CIDRMask(24, 32)}
	host, err := protectHost([]net.Addr{iface}, []string{":443", "198.51.100.10:8080", "127.0.0.1:9000"})
	if err != nil {
		t.Fatal(err)
	}
	gw, err := NewGateway(tcpip.AddrFrom4(testGwIP), 24, testGwMAC, host)
	if err != nil {
		t.Fatal(err)
	}
	at := func(s string) netip.AddrPort { return netip.MustParseAddrPort(s) }
	for _, tc := range []struct {
		wire  gateway.NetPolicyWire
		host  string
		dst   string
		allow bool
	}{
		{gateway.NetPolicyWire{Default: "public"}, "", "203.0.113.42:443", true},
		{gateway.NetPolicyWire{Default: "public"}, "", "198.51.100.10:8080", true},
		{gateway.NetPolicyWire{Default: "public"}, "", "203.0.113.42:22", false},   // other host port
		{gateway.NetPolicyWire{Default: "public"}, "", "198.51.100.10:443", false}, // listener bound elsewhere
		{gateway.NetPolicyWire{Default: "public"}, "", "127.0.0.1:9000", false},    // loopback never opens
		{gateway.NetPolicyWire{Default: "public", AllowCIDRs: []string{"0.0.0.0/0"}}, "", "203.0.113.42:22", false},
		{gateway.NetPolicyWire{Default: "deny"}, "", "203.0.113.42:443", false}, // posture still applies
		{gateway.NetPolicyWire{Default: "deny", AllowHosts: []string{"node.example"}}, "node.example", "203.0.113.42:443", true},
	} {
		p, err := gateway.PolicyFromWire(tc.wire)
		if err != nil {
			t.Fatal(err)
		}
		if err := gw.SetSandbox("100.64.0.2", "sb1", p, "52:54:00:00:00:02", testNetToken); err != nil {
			t.Fatal(err)
		}
		st := gw.stateFor(tcpip.AddrFrom4(testGuestIP))
		if v := st.pol.CheckTCP(tc.host, at(tc.dst)); v.Allow != tc.allow {
			t.Errorf("%+v %s → %s: %+v, want allow=%v", tc.wire, tc.host, tc.dst, v, tc.allow)
		}
		if v := st.pol.Check(tc.host, at(tc.dst).Addr()); v.Allow {
			t.Errorf("address-only (UDP) check opened host address %s: %+v", tc.dst, v)
		}
	}
}

func TestSiblingVerdictBothTransports(t *testing.T) {
	gw, err := NewGateway(tcpip.AddrFrom4(testGwIP), 24, testGwMAC, hostProtection{})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	original := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(original)
	sibling := netip.MustParseAddr("100.64.0.3")
	for _, posture := range []gateway.Posture{gateway.PostureDeny, gateway.PosturePublic} {
		pol := &gateway.EgressPolicy{
			Default:    posture,
			AllowCIDRs: []netip.Prefix{netip.MustParsePrefix("100.64.0.0/10")},
		}
		gw.SetSandbox("100.64.0.2", "sb1", pol, "52:54:00:00:00:02", testNetToken)
		st := gw.stateFor(tcpip.AddrFrom4(testGuestIP))
		for _, protocol := range []string{"tcp", "udp"} {
			if gw.siblingAllowed(st, protocol, sibling, 8080) {
				t.Errorf("%s under %v admitted sibling without opt-in", protocol, posture)
			}
		}
		pol.Siblings = gateway.PosturePublic
		gw.SetSandbox("100.64.0.2", "sb1", pol, "52:54:00:00:00:02", testNetToken)
		st = gw.stateFor(tcpip.AddrFrom4(testGuestIP))
		for _, protocol := range []string{"tcp", "udp"} {
			if !gw.siblingAllowed(st, protocol, sibling, 8080) {
				t.Errorf("%s under %v denied explicitly granted sibling", protocol, posture)
			}
		}
		if v := st.pol.Check("", netip.MustParseAddr("100.64.1.3")); v.Allow {
			t.Fatalf("siblings=allow admitted another owner's subnet: %+v", v)
		}
	}
	gw.SetSandbox("100.64.0.2", "sb1", nil, "52:54:00:00:00:02", testNetToken)
	if gw.siblingAllowed(gw.stateFor(tcpip.AddrFrom4(testGuestIP)), "tcp", sibling, 8080) {
		t.Fatal("nil policy admitted sibling")
	}
	if v := gw.stateFor(tcpip.AddrFrom4(testGuestIP)).pol.Check("", netip.MustParseAddr("8.8.8.8")); v.Allow {
		t.Fatalf("nil policy admitted public internet: %+v", v)
	}
	for _, needle := range []string{"protocol=tcp", "protocol=udp", "allow=true", "allow=false", "reason=siblings"} {
		if !strings.Contains(logs.String(), needle) {
			t.Errorf("sibling verdict log missing %q: %s", needle, logs.String())
		}
	}
}

func TestUnregisteredGuestWarningRateLimited(t *testing.T) {
	gw, err := NewGateway(tcpip.AddrFrom4(testGwIP), 24, testGwMAC, hostProtection{})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	original := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(original)
	for _, src := range [][4]byte{{100, 64, 0, 2}, {100, 64, 0, 3}} {
		if v := gw.stateFor(tcpip.AddrFrom4(src)).pol.Check("", netip.MustParseAddr("8.8.8.8")); v.Allow {
			t.Fatalf("unregistered %v admitted public egress: %+v", src, v)
		}
	}
	if got := strings.Count(logs.String(), "netd.unregistered_guest"); got != 1 {
		t.Fatalf("two unregistered sources emitted %d warnings, want one: %s", got, logs.String())
	}
	gw.polMu.Lock()
	gw.lastUnregisteredLog = time.Now().Add(-2 * time.Minute)
	gw.polMu.Unlock()
	gw.stateFor(tcpip.AddrFrom4(testGuestIP))
	if got := strings.Count(logs.String(), "netd.unregistered_guest"); got != 2 {
		t.Fatalf("warning never resumed after rate window: %s", logs.String())
	}
}
