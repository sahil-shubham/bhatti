package main

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/gateway"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

// buildTCPSyn crafts eth+IPv4+TCP-SYN from the guest to a foreign dst. When
// corrupt is true the TCP checksum is deliberately wrong — simulating a guest
// whose virtio-net offloaded the checksum (libkrun strips the virtio_net_hdr
// carrying the offload flag, so the on-wire checksum is not final).
func buildTCPSyn(dstIP [4]byte, dport uint16, corrupt bool) []byte {
	const ipLen = header.IPv4MinimumSize
	const tcpLen = header.TCPMinimumSize
	frame := make([]byte, header.EthernetMinimumSize+ipLen+tcpLen)

	header.Ethernet(frame).Encode(&header.EthernetFields{
		SrcAddr: testGuestMAC, DstAddr: testGwMAC, Type: header.IPv4ProtocolNumber,
	})
	ip := header.IPv4(frame[header.EthernetMinimumSize:])
	ip.Encode(&header.IPv4Fields{
		TotalLength: uint16(ipLen + tcpLen),
		TTL:         64,
		Protocol:    uint8(header.TCPProtocolNumber),
		SrcAddr:     tcpip.AddrFrom4(testGuestIP),
		DstAddr:     tcpip.AddrFrom4(dstIP),
	})
	ip.SetChecksum(^ip.CalculateChecksum())

	tcp := header.TCP(frame[header.EthernetMinimumSize+ipLen:])
	tcp.Encode(&header.TCPFields{
		SrcPort:    45000,
		DstPort:    dport,
		SeqNum:     1,
		DataOffset: header.TCPMinimumSize,
		Flags:      header.TCPFlagSyn,
		WindowSize: 65535,
	})
	xsum := header.PseudoHeaderChecksum(header.TCPProtocolNumber,
		tcpip.AddrFrom4(testGuestIP), tcpip.AddrFrom4(dstIP), uint16(tcpLen))
	tcp.SetChecksum(^tcp.CalculateChecksum(xsum))
	if corrupt {
		tcp.SetChecksum(tcp.Checksum() ^ 0xffff)
	}
	return frame
}

// forwardedDest injects a guest SYN to dst:dport and reports the destination the
// TCP forwarder was asked to reach (or "" if the SYN never reached it). It swaps
// in a recording forwarder so the signal doesn't depend on egress/logging.
func forwardedDest(t *testing.T, dst [4]byte, dport uint16, corrupt bool) string {
	t.Helper()
	guestSide, netdSide := net.Pipe()
	t.Cleanup(func() { guestSide.Close(); netdSide.Close() })

	gw, err := NewGateway(tcpip.AddrFrom4(testGwIP), 24, testGwMAC, hostProtection{})
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	if err := gw.SetSandbox("100.64.0.2", "sb1", nil, "52:54:00:00:00:02", testNetToken); err != nil {
		t.Fatal(err)
	}
	gw.AddGuest(gateway.NewFrameConn(netdSide), tcpip.AddrFrom4(testGuestIP), testGuestMAC, testNetToken)
	got := make(chan string, 1)
	fwd := tcp.NewForwarder(gw.stack, 0, 16, func(r *tcp.ForwarderRequest) {
		id := r.ID()
		select {
		case got <- addrString(id.LocalAddress):
		default:
		}
		r.Complete(true)
	})
	gw.stack.SetTransportProtocolHandler(tcp.ProtocolNumber, fwd.HandlePacket)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	go gw.Run(ctx)

	if err := gateway.NewFrameConn(guestSide).WriteFrame(buildTCPSyn(dst, dport, corrupt)); err != nil {
		t.Fatalf("write SYN: %v", err)
	}
	select {
	case d := <-got:
		return d
	case <-time.After(time.Second):
		return ""
	}
}

// TestForwarderReachesForeignDest is the core egress path with no VM: a guest
// SYN to a foreign public dst is delivered locally (promiscuous) to the TCP
// forwarder, which learns the real destination.
func TestForwarderReachesForeignDest(t *testing.T) {
	if d := forwardedDest(t, [4]byte{8, 8, 8, 8}, 443, false); d != "8.8.8.8" {
		t.Fatalf("forwarder dest = %q, want 8.8.8.8 (SYN never reached the forwarder)", d)
	}
}

// TestOffloadedChecksumForwards guards the virtio-net fix: a SYN with a bad
// (offloaded) TCP checksum must still reach the forwarder, because the gateway
// advertises CapabilityRXChecksumOffload so gVisor skips checksum verification
// for frames arriving over the trusted vsock link. Without the capability the
// SYN is silently dropped and egress times out.
func TestOffloadedChecksumForwards(t *testing.T) {
	if d := forwardedDest(t, [4]byte{8, 8, 8, 8}, 443, true); d != "8.8.8.8" {
		t.Fatalf("offloaded-checksum SYN did not reach the forwarder (got %q) — RX checksum offload not honored", d)
	}
}

// udpGuestFrame supplies a complete IPv4 datagram from one attached guest.
// IPv4 permits a zero UDP checksum, matching guests with checksum offload.
func udpGuestFrame(src, dst [4]byte, mac tcpip.LinkAddress, srcPort, dstPort uint16, payload []byte) []byte {
	const network = header.IPv4MinimumSize
	const transport = header.UDPMinimumSize
	frame := make([]byte, header.EthernetMinimumSize+network+transport+len(payload))
	header.Ethernet(frame).Encode(&header.EthernetFields{
		SrcAddr: mac, DstAddr: testGwMAC, Type: header.IPv4ProtocolNumber,
	})
	ip := header.IPv4(frame[header.EthernetMinimumSize:])
	ip.Encode(&header.IPv4Fields{
		TotalLength: uint16(network + transport + len(payload)),
		TTL:         64,
		Protocol:    uint8(header.UDPProtocolNumber),
		SrcAddr:     tcpip.AddrFrom4(src),
		DstAddr:     tcpip.AddrFrom4(dst),
	})
	ip.SetChecksum(^ip.CalculateChecksum())
	udp := header.UDP(frame[header.EthernetMinimumSize+network:])
	udp.Encode(&header.UDPFields{
		SrcPort: srcPort, DstPort: dstPort,
		Length: uint16(transport + len(payload)),
	})
	copy(udp.Payload(), payload)
	return frame
}

type guestUDP struct {
	src, dst         [4]byte
	srcPort, dstPort uint16
	payload          []byte
}

func readGuestUDP(t *testing.T, guest *gateway.FrameConn) guestUDP {
	t.Helper()
	frame := readFrameCtx(t, guest, 3*time.Second)
	if len(frame) < header.EthernetMinimumSize+header.IPv4MinimumSize+header.UDPMinimumSize ||
		header.Ethernet(frame).Type() != header.IPv4ProtocolNumber {
		t.Fatalf("wanted IPv4 UDP frame, received %x", frame)
	}
	ip := header.IPv4(frame[header.EthernetMinimumSize:])
	if ip.TransportProtocol() != header.UDPProtocolNumber {
		t.Fatalf("wanted UDP, received IP protocol %d", ip.TransportProtocol())
	}
	udp := header.UDP(ip.Payload())
	return guestUDP{
		src: ip.SourceAddress().As4(), dst: ip.DestinationAddress().As4(),
		srcPort: udp.SourcePort(), dstPort: udp.DestinationPort(),
		payload: udp.Payload(),
	}
}

func TestSiblingUDPDatagramsRoundTrip(t *testing.T) {
	aIP, bIP := testGuestIP, [4]byte{100, 64, 0, 3}
	aMAC, bMAC := testGuestMAC, tcpip.LinkAddress("\x52\x54\x00\x00\x00\x03")
	bToken := strings.Repeat("cd", 32)
	aGuest, aNetd := net.Pipe()
	bGuest, bNetd := net.Pipe()
	t.Cleanup(func() { aGuest.Close(); aNetd.Close(); bGuest.Close(); bNetd.Close() })
	gw, err := NewGateway(tcpip.AddrFrom4(testGwIP), 24, testGwMAC, hostProtection{})
	if err != nil {
		t.Fatal(err)
	}
	pol := &gateway.EgressPolicy{Siblings: gateway.PosturePublic}
	for _, peer := range []struct {
		ip, id, mac, token string
	}{
		{"100.64.0.2", "a", "52:54:00:00:00:02", testNetToken},
		{"100.64.0.3", "b", "52:54:00:00:00:03", bToken},
	} {
		if err := gw.SetSandbox(peer.ip, peer.id, pol, peer.mac, peer.token); err != nil {
			t.Fatal(err)
		}
	}
	gw.AddGuest(gateway.NewFrameConn(aNetd), tcpip.AddrFrom4(aIP), aMAC, testNetToken)
	gw.AddGuest(gateway.NewFrameConn(bNetd), tcpip.AddrFrom4(bIP), bMAC, bToken)
	// TCP reaches siblings on real guests; skip guest ARP machinery here to
	// isolate UDP forwarding and port demultiplexing.
	for _, peer := range []struct {
		ip  [4]byte
		mac tcpip.LinkAddress
	}{{aIP, aMAC}, {bIP, bMAC}} {
		if err := gw.stack.AddStaticNeighbor(nicID, ipv4.ProtocolNumber, tcpip.AddrFrom4(peer.ip), peer.mac); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go gw.Run(ctx)
	a, b := gateway.NewFrameConn(aGuest), gateway.NewFrameConn(bGuest)
	if err := a.WriteFrame(udpGuestFrame(aIP, bIP, aMAC, 45000, 8001, []byte("ping"))); err != nil {
		t.Fatal(err)
	}
	in := readGuestUDP(t, b)
	if in.dst != bIP || in.dstPort != 8001 || !bytes.Equal(in.payload, []byte("ping")) {
		t.Fatalf("sibling UDP receive = %+v, want ping at B:8001", in)
	}
	if in.src != testGwIP {
		t.Fatalf("sibling UDP source IP = %v, want gateway %v", in.src, testGwIP)
	}
	if err := b.WriteFrame(udpGuestFrame(bIP, in.src, bMAC, 8001, in.srcPort, []byte("pong"))); err != nil {
		t.Fatal(err)
	}
	out := readGuestUDP(t, a)
	if out.src != bIP || out.srcPort != 8001 || out.dst != aIP || out.dstPort != 45000 || !bytes.Equal(out.payload, []byte("pong")) {
		t.Fatalf("sibling UDP reply = %+v, want B:8001 → A:45000 pong", out)
	}
}

func TestInternetUDPReplyReachesGuest(t *testing.T) {
	// The guest still requests a public IP; redirect the already-approved
	// upstream dial to a local UDP echo so this test needs no internet.
	host, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { host.Close() })
	guestSide, netdSide := net.Pipe()
	t.Cleanup(func() { guestSide.Close(); netdSide.Close() })
	gw, err := NewGateway(tcpip.AddrFrom4(testGwIP), 24, testGwMAC, hostProtection{})
	if err != nil {
		t.Fatal(err)
	}
	if err := gw.SetSandbox("100.64.0.2", "a", &gateway.EgressPolicy{Default: gateway.PosturePublic},
		"52:54:00:00:00:02", testNetToken); err != nil {
		t.Fatal(err)
	}
	dialed := make(chan string, 1)
	gw.hostUDPDial = func(network, address string) (net.Conn, error) {
		dialed <- network + " " + address
		return net.Dial("udp", host.LocalAddr().String())
	}
	gw.AddGuest(gateway.NewFrameConn(netdSide), tcpip.AddrFrom4(testGuestIP), testGuestMAC, testNetToken)
	if err := gw.stack.AddStaticNeighbor(nicID, ipv4.ProtocolNumber, tcpip.AddrFrom4(testGuestIP), testGuestMAC); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go gw.Run(ctx)

	guest := gateway.NewFrameConn(guestSide)
	publicIP := [4]byte{8, 8, 8, 8}
	if err := guest.WriteFrame(udpGuestFrame(testGuestIP, publicIP, testGuestMAC, 45001, 8001, []byte("ping"))); err != nil {
		t.Fatal(err)
	}
	select {
	case dest := <-dialed:
		if dest != "udp 8.8.8.8:8001" {
			t.Fatalf("external UDP dial = %q, want public guest destination", dest)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("external UDP was not dialed")
	}
	if err := host.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	n, remote, err := host.ReadFromUDP(buf)
	if err != nil || !bytes.Equal(buf[:n], []byte("ping")) {
		t.Fatalf("external UDP request = %q, error %v", buf[:n], err)
	}
	if _, err := host.WriteToUDP([]byte("pong"), remote); err != nil {
		t.Fatal(err)
	}
	out := readGuestUDP(t, guest)
	if out.src != publicIP || out.srcPort != 8001 || out.dst != testGuestIP || out.dstPort != 45001 ||
		!bytes.Equal(out.payload, []byte("pong")) {
		t.Fatalf("internet UDP reply = %+v, want 8.8.8.8:8001 → A:45001 pong", out)
	}
}
