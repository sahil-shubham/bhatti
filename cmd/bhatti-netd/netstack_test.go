package main

import (
	"context"
	"net"
	"path/filepath"
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

	gw, err := NewGateway(tcpip.AddrFrom4(testGwIP), 24, testGwMAC)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	gw.AddGuest(gateway.NewFrameConn(netdSide))
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
	sock := filepath.Join(t.TempDir(), "net.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	served := make(chan error, 1)
	go func() {
		served <- serve(ctx, ln, gwConfig{ip: tcpip.AddrFrom4(testGwIP), prefix: 24, mac: testGwMAC})
	}()

	// Peer connects (as libkrun would).
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial %s: %v", sock, err)
	}
	defer conn.Close()

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

var testSiblingMAC = tcpip.LinkAddress("\x52\x54\x00\x00\x00\x03")

// ethFrame builds a minimal ethernet frame with an arbitrary payload.
func ethFrame(dst, src tcpip.LinkAddress, ethertype uint16, payload []byte) []byte {
	f := make([]byte, header.EthernetMinimumSize+len(payload))
	header.Ethernet(f).Encode(&header.EthernetFields{
		SrcAddr: src, DstAddr: dst, Type: tcpip.NetworkProtocolNumber(ethertype),
	})
	copy(f[header.EthernetMinimumSize:], payload)
	return f
}

// TestGatewaySwitchesSiblings is the sibling-reachability mechanism (no VM): two
// guest links on one netd. A broadcast (ARP) from one guest is flooded to the
// other; a unicast to a learned sibling MAC is switched straight to that guest's
// link — the traffic never enters the stack. This is what lets sibling sandboxes
// of the same owner reach each other.
func TestGatewaySwitchesSiblings(t *testing.T) {
	gw, err := NewGateway(tcpip.AddrFrom4(testGwIP), 24, testGwMAC)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go gw.Run(ctx)

	gaHost, gaNetd := net.Pipe() // guest A
	gbHost, gbNetd := net.Pipe() // guest B
	defer gaHost.Close()
	defer gbHost.Close()
	gw.AddGuest(gateway.NewFrameConn(gaNetd))
	gw.AddGuest(gateway.NewFrameConn(gbNetd))
	guestA := gateway.NewFrameConn(gaHost)
	guestB := gateway.NewFrameConn(gbHost)

	// 1. Broadcast ARP from A must be flooded to B.
	if err := guestA.WriteFrame(arpRequestFrame()); err != nil {
		t.Fatalf("A broadcast: %v", err)
	}
	got := readFrameCtx(t, guestB, 3*time.Second)
	if header.Ethernet(got).Type() != header.ARPProtocolNumber {
		t.Fatalf("B did not receive the flooded ARP (type=%#x)", header.Ethernet(got).Type())
	}

	// 2. Teach the switch where B is: B sends a unicast to the gateway (learned,
	// not flooded), then a unicast from A to B's MAC must be switched to B.
	if err := guestB.WriteFrame(ethFrame(testGwMAC, testSiblingMAC, uint16(header.IPv4ProtocolNumber), []byte("to-gw"))); err != nil {
		t.Fatalf("B->gw: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // let the switch learn B's MAC

	payload := []byte("hello-sibling")
	if err := guestA.WriteFrame(ethFrame(testSiblingMAC, testGuestMAC, uint16(header.IPv4ProtocolNumber), payload)); err != nil {
		t.Fatalf("A->B: %v", err)
	}
	sib := readFrameCtx(t, guestB, 3*time.Second)
	if string(sib[header.EthernetMinimumSize:]) != string(payload) {
		t.Fatalf("B received %q, want sibling unicast %q", sib[header.EthernetMinimumSize:], payload)
	}
	if src := tcpip.LinkAddress(header.Ethernet(sib).SourceAddress()); src != testGuestMAC {
		t.Fatalf("switched frame src MAC = %x, want A %x", src, testGuestMAC)
	}
}
