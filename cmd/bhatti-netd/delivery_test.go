package main

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/gateway"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
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

	gw, err := NewGateway(gateway.NewFrameConn(netdSide), tcpip.AddrFrom4(testGwIP), 24, testGwMAC)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
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
