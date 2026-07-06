package main

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/gateway"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// TestGatewayAnswersARP is netd's first behavioral test with no VM: a crafted
// ARP request for the gateway IP goes in over the frame codec, and the embedded
// netstack must answer with an ARP reply carrying the gateway MAC. This
// exercises the whole bridge — FrameConn read → InjectInbound → ethernet parse →
// netstack ARP → outbound packet → FrameConn write.
func TestGatewayAnswersARP(t *testing.T) {
	guestSide, netdSide := net.Pipe()
	defer guestSide.Close()
	defer netdSide.Close()

	gwIP := [4]byte{100, 64, 0, 1}
	guestIP := [4]byte{100, 64, 0, 2}
	gwMAC := tcpip.LinkAddress("\x52\x54\x00\x00\x00\x01")
	guestMAC := tcpip.LinkAddress("\x52\x54\x00\x00\x00\x02")

	gw, err := NewGateway(gateway.NewFrameConn(netdSide), tcpip.AddrFrom4(gwIP), 24, gwMAC)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go gw.Run(ctx)

	guest := gateway.NewFrameConn(guestSide)

	// Build "who has 100.64.0.1? tell 100.64.0.2".
	eth := make([]byte, header.EthernetMinimumSize)
	header.Ethernet(eth).Encode(&header.EthernetFields{
		SrcAddr: guestMAC,
		DstAddr: header.EthernetBroadcastAddress,
		Type:    header.ARPProtocolNumber,
	})
	a := make([]byte, header.ARPSize)
	arpv := header.ARP(a)
	arpv.SetIPv4OverEthernet()
	arpv.SetOp(header.ARPRequest)
	copy(arpv.HardwareAddressSender(), guestMAC)
	copy(arpv.ProtocolAddressSender(), guestIP[:])
	copy(arpv.ProtocolAddressTarget(), gwIP[:])

	if err := guest.WriteFrame(append(eth, a...)); err != nil {
		t.Fatalf("send ARP request: %v", err)
	}

	// Read the reply (guard the pipe read with the ctx via a goroutine).
	type res struct {
		f   []byte
		err error
	}
	ch := make(chan res, 1)
	go func() { f, err := guest.ReadFrame(); ch <- res{f, err} }()

	var reply []byte
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("read ARP reply: %v", r.err)
		}
		reply = r.f
	case <-time.After(4 * time.Second):
		t.Fatal("no ARP reply within 4s")
	}

	if len(reply) < header.EthernetMinimumSize+header.ARPSize {
		t.Fatalf("reply too short: %d bytes", len(reply))
	}
	ethR := header.Ethernet(reply)
	if ethR.Type() != header.ARPProtocolNumber {
		t.Fatalf("reply ethertype = %#x, want ARP", ethR.Type())
	}
	arpR := header.ARP(reply[header.EthernetMinimumSize:])
	if arpR.Op() != header.ARPReply {
		t.Fatalf("ARP op = %d, want reply", arpR.Op())
	}
	// The sender of the reply must be the gateway (its MAC + IP).
	if got := tcpip.LinkAddress(arpR.HardwareAddressSender()); got != gwMAC {
		t.Fatalf("ARP reply sender MAC = %x, want gateway %x", got, gwMAC)
	}
	if got := [4]byte(arpR.ProtocolAddressSender()); got != gwIP {
		t.Fatalf("ARP reply sender IP = %v, want gateway %v", got, gwIP)
	}
}
