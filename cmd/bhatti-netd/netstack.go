// Command bhatti-netd is the per-owner userspace network gateway (Approach A,
// DESIGN-bhatti-v2-networking.md §0c). It embeds a gVisor netstack on the
// guest's virtio-net link (libkrun unixstream frames, via pkg/gateway.FrameConn)
// and is the guest's router / DNS / egress-policer / L7 secret-substituter /
// inbound port-proxy / control door / audit chokepoint.
//
// This file is the link + stack bridge (step 1b): frames in ⇄ netstack ⇄ frames
// out. Egress policy, substitution, inbound, DNS, and control build on top.
package main

import (
	"context"
	"fmt"

	"github.com/sahil-shubham/bhatti/pkg/gateway"

	"gvisor.dev/gvisor/pkg/tcpip/header"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

const (
	nicID = 1
	mtu   = 1500
	// channelQueueLen bounds the outbound (netstack→guest) queue.
	channelQueueLen = 512
)

// Gateway is one owner's userspace network: a gVisor stack bridged to the
// guest's virtio-net link.
type Gateway struct {
	stack *stack.Stack
	ep    *channel.Endpoint
	fc    *gateway.FrameConn
	mac   tcpip.LinkAddress
}

// NewGateway builds the stack, assigns the gateway address gwIP/prefix on the
// NIC, sets a default route, and bridges it to fc. mac is the gateway's link
// address (the guest's default-route next hop).
func NewGateway(fc *gateway.FrameConn, gwIP tcpip.Address, prefixLen int, mac tcpip.LinkAddress) (*Gateway, error) {
	s := stack.New(stack.Options{
		NetworkProtocols: []stack.NetworkProtocolFactory{
			ipv4.NewProtocol, ipv6.NewProtocol, arp.NewProtocol,
		},
		TransportProtocols: []stack.TransportProtocolFactory{
			tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4, icmp.NewProtocol6,
		},
	})

	ch := channel.New(channelQueueLen, mtu, mac)
	linkEP := ethernet.New(ch)
	if err := s.CreateNIC(nicID, linkEP); err != nil {
		return nil, fmt.Errorf("create NIC: %s", err)
	}
	// The gateway answers ARP for its own address and accepts spoofed source
	// addresses (it routes on behalf of many guests).
	if err := s.SetPromiscuousMode(nicID, true); err != nil {
		return nil, fmt.Errorf("promiscuous: %s", err)
	}
	if err := s.SetSpoofing(nicID, true); err != nil {
		return nil, fmt.Errorf("spoofing: %s", err)
	}

	protoAddr := tcpip.ProtocolAddress{
		Protocol: ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{
			Address:   gwIP,
			PrefixLen: prefixLen,
		},
	}
	if err := s.AddProtocolAddress(nicID, protoAddr, stack.AddressProperties{}); err != nil {
		return nil, fmt.Errorf("add gateway address: %s", err)
	}
	// Route only the guest subnet out the NIC (so SYN-ACKs / netd-originated flows
	// to the guest route correctly). We do NOT install a default route: inbound
	// packets to arbitrary dests are delivered locally to the TCP forwarder via
	// spoofing, not forwarded — a 0.0.0.0/0-via-NIC route makes the stack try to
	// forward (disabled) and drop them.
	_ = prefixLen
	// Catch-all route out the NIC (gvproxy's config): every dest routes to the
	// NIC; inbound foreign dests are delivered locally to the forwarder via
	// promiscuous mode, and the return path (SYN-ACK to the guest) routes here.
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: nicID}})

	g := &Gateway{stack: s, ep: ch, fc: fc, mac: mac}
	// Route all outbound guest TCP through the egress guard (default: public
	// internet allowed, host/private/metadata denied). Egress policy per sandbox
	// and L7 secret substitution layer on here next.
	g.installTCPForwarder(&gateway.Dialer{
		Policy: &gateway.EgressPolicy{Default: gateway.PosturePublic},
	})
	return g, nil
}

// Run pumps frames both directions until ctx is cancelled or the link closes.
func (g *Gateway) Run(ctx context.Context) error {
	errc := make(chan error, 2)
	go func() { errc <- g.readLoop() }()
	go func() { errc <- g.writeLoop(ctx) }()
	select {
	case <-ctx.Done():
		g.ep.Close()
		return ctx.Err()
	case err := <-errc:
		g.ep.Close()
		return err
	}
}

// readLoop: guest → netstack. Reads ethernet frames off the link and injects
// them; the ethernet endpoint parses the L2 header (the injected protocol is
// ignored — see gVisor link/ethernet).
func (g *Gateway) readLoop() error {
	for {
		frame, err := g.fc.ReadFrame()
		if err != nil {
			return fmt.Errorf("netd read: %w", err)
		}
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
			Payload: buffer.MakeWithData(frame),
		})
		g.ep.InjectInbound(0 /* ignored by ethernet */, pkt)
		pkt.DecRef()
	}
}

// writeLoop: netstack → guest. Reads outbound packets (with the L2 header the
// ethernet endpoint pushed) and writes them as framed ethernet.
func (g *Gateway) writeLoop(ctx context.Context) error {
	for {
		pkt := g.ep.ReadContext(ctx)
		if pkt == nil {
			return ctx.Err() // context cancelled
		}
		buf := pkt.ToBuffer()
		frame := buf.Flatten()
		pkt.DecRef()
		if err := g.fc.WriteFrame(frame); err != nil {
			return fmt.Errorf("netd write: %w", err)
		}
	}
}
