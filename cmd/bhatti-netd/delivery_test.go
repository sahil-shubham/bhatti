package main

import (
	"context"
	"encoding/hex"
	"net"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/gateway"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// buildTCPSyn crafts eth+IPv4+TCP-SYN from the guest to a foreign dst, with
// valid checksums, so we can inject it and see whether the stack delivers it to
// the transport layer (forwarder) or drops it as an invalid destination.
func buildTCPSyn(dstIP [4]byte, dport uint16) []byte {
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
	return frame
}

// TestForeignDestDelivered is the fast, no-VM repro of the egress bug: a SYN to a
// foreign public dst must be delivered locally to the transport layer (so the
// TCP forwarder catches it), NOT dropped as an invalid destination.
func TestForeignDestDelivered(t *testing.T) {
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
	if err := guest.WriteFrame(buildTCPSyn([4]byte{8, 8, 8, 8}, 443)); err != nil {
		t.Fatalf("write SYN: %v", err)
	}
	// Give the stack a moment to process.
	time.Sleep(300 * time.Millisecond)

	ipStats := gw.stack.Stats().IP
	inv := ipStats.InvalidDestinationAddressesReceived.Value()
	delivered := ipStats.PacketsDelivered.Value()
	t.Logf("ip: received=%d delivered=%d invalidDst=%d",
		ipStats.PacketsReceived.Value(), delivered, inv)
	if inv > 0 {
		t.Fatalf("foreign-dest SYN dropped as invalid destination (delivery broken): invalidDst=%d", inv)
	}
	if delivered == 0 {
		t.Fatalf("foreign-dest SYN not delivered to the transport layer")
	}
}

// TestRealFrameDelivered injects the EXACT bytes captured from a real guest SYN
// to 1.1.1.1 (padded to 74B) — to tell whether the cluster's invalidDst is the
// frame bytes or the live path.
func TestRealFrameDelivered(t *testing.T) {
	hexFrame := "525400000001525400000002080045" +
		"00003c45ac400040068ecc6440000201010101bafe01bb7a6ceaaa00000000a002faf066720000020405b40402"
	full := make([]byte, 74)
	raw := mustHex(t, hexFrame)
	copy(full, raw)

	guestSide, netdSide := net.Pipe()
	defer guestSide.Close()
	defer netdSide.Close()
	gw, err := NewGateway(gateway.NewFrameConn(netdSide), tcpip.AddrFrom4(testGwIP), 24, testGwMAC)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go gw.Run(ctx)

	if err := gateway.NewFrameConn(guestSide).WriteFrame(full); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	ip := gw.stack.Stats().IP
	t.Logf("real-frame: recv=%d delivered=%d invalidDst=%d malformed=%d",
		ip.PacketsReceived.Value(), ip.PacketsDelivered.Value(),
		ip.InvalidDestinationAddressesReceived.Value(), ip.MalformedPacketsReceived.Value())
	if ip.InvalidDestinationAddressesReceived.Value() > 0 {
		t.Fatal("real frame → invalidDst (it's the bytes)")
	}
}

func mustHex(t *testing.T, s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestRealFrameOverUnixSocket is TestRealFrameDelivered but over a REAL unix
// socket (as libkrun uses), to isolate net.Pipe vs socket transport.
func TestRealFrameOverUnixSocket(t *testing.T) {
	dir := t.TempDir()
	sock := dir + "/net.sock"
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	gwCh := make(chan *Gateway, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		gw, err := NewGateway(gateway.NewFrameConn(conn), tcpip.AddrFrom4(testGwIP), 24, testGwMAC)
		if err != nil {
			t.Errorf("NewGateway: %v", err)
			return
		}
		gwCh <- gw
		gw.Run(context.Background())
	}()

	client, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	hexFrame := "525400000001525400000002080045" +
		"00003c45ac400040068ecc6440000201010101bafe01bb7a6ceaaa00000000a002faf066720000020405b40402"
	full := make([]byte, 74)
	copy(full, mustHex(t, hexFrame))
	if err := gateway.NewFrameConn(client).WriteFrame(full); err != nil {
		t.Fatal(err)
	}

	gw := <-gwCh
	time.Sleep(400 * time.Millisecond)
	ip := gw.stack.Stats().IP
	t.Logf("socket real-frame: recv=%d delivered=%d invalidDst=%d",
		ip.PacketsReceived.Value(), ip.PacketsDelivered.Value(), ip.InvalidDestinationAddressesReceived.Value())
	if ip.InvalidDestinationAddressesReceived.Value() > 0 {
		t.Fatal("real frame over unix socket → invalidDst")
	}
}

// TestArpThenSyn replicates the real VM sequence: guest ARPs the gateway first,
// THEN sends the SYN — to see if the ordering/state triggers invalidDst.
func TestArpThenSyn(t *testing.T) {
	guestSide, netdSide := net.Pipe()
	defer guestSide.Close()
	defer netdSide.Close()
	gw, err := NewGateway(gateway.NewFrameConn(netdSide), tcpip.AddrFrom4(testGwIP), 24, testGwMAC)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go gw.Run(ctx)

	guest := gateway.NewFrameConn(guestSide)
	// 1. ARP request (and drain the reply so the pipe doesn't block).
	if err := guest.WriteFrame(arpRequestFrame()); err != nil {
		t.Fatal(err)
	}
	go guest.ReadFrame() // drain ARP reply
	time.Sleep(100 * time.Millisecond)
	// 2. SYN to 1.1.1.1.
	if err := guest.WriteFrame(buildTCPSyn([4]byte{1, 1, 1, 1}, 443)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	ip := gw.stack.Stats().IP
	t.Logf("arp-then-syn: delivered=%d invalidDst=%d", ip.PacketsDelivered.Value(), ip.InvalidDestinationAddressesReceived.Value())
	if ip.InvalidDestinationAddressesReceived.Value() > 0 {
		t.Fatal("ARP-then-SYN → invalidDst (ordering triggers it)")
	}
}
