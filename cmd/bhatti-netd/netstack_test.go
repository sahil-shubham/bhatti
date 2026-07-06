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

	gw, err := NewGateway(gateway.NewFrameConn(netdSide), tcpip.AddrFrom4(testGwIP), 24, testGwMAC)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
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
