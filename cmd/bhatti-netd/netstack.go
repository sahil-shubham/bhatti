// Command bhatti-netd is the per-owner userspace network gateway (Approach A,
// DESIGN-bhatti-v2-networking.md §0c). It embeds a gVisor netstack on the
// owner's guests' virtio-net links (libkrun unixstream frames, via
// pkg/gateway.FrameConn) and is their router / DNS / egress-policer / L7 secret-
// substituter / inbound port-proxy / control door / audit chokepoint.
//
// Topology: netd is an L2 learning switch. Ports are (a) the gVisor stack — the
// gateway address .1, which answers ARP for itself, runs the egress TCP
// forwarder, and (later) DNS/control; and (b) one guest link per sandbox of the
// owner. All guests share one 100.64.<owner>.0/24 segment: a frame to the
// gateway MAC goes into the stack (egress), a frame to a sibling's MAC is
// switched straight to that guest's link, and broadcast/unknown frames (ARP)
// are flooded. Sibling↔sibling traffic never touches the stack — it is pure L2
// switching — which is why siblings reach each other while the gateway still
// polices egress. The single-guest case is just this switch with one port.
package main

import (
	"context"
	"fmt"
	"sync"

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
	// channelQueueLen bounds the outbound (netstack→guests) queue.
	channelQueueLen = 512
)

// Gateway is one owner's userspace network: a gVisor stack (the .1 gateway) that
// is one port of an L2 learning switch, plus a port per guest link.
type Gateway struct {
	stack *stack.Stack
	ep    *channel.Endpoint // the gateway (stack) port's link
	gwMAC tcpip.LinkAddress

	mu       sync.RWMutex
	ports    []*guestPort                     // all guest links
	macTable map[tcpip.LinkAddress]*guestPort // learned guest MAC → port
}

// guestPort is one guest's virtio-net link (a switch port).
type guestPort struct {
	fc  *gateway.FrameConn
	wmu sync.Mutex // serialize concurrent writes to this guest link
}

func (p *guestPort) write(frame []byte) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	return p.fc.WriteFrame(frame)
}

// NewGateway builds the stack, assigns the gateway address gwIP/prefix on the
// NIC, sets a catch-all route, installs the egress forwarder, and returns the
// switch with no guest ports yet. Attach guests with AddGuest. mac is the
// gateway's link address (the guests' default-route next hop).
func NewGateway(gwIP tcpip.Address, prefixLen int, mac tcpip.LinkAddress) (*Gateway, error) {
	s := stack.New(stack.Options{
		NetworkProtocols: []stack.NetworkProtocolFactory{
			ipv4.NewProtocol, ipv6.NewProtocol, arp.NewProtocol,
		},
		TransportProtocols: []stack.TransportProtocolFactory{
			tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4, icmp.NewProtocol6,
		},
	})

	ch := channel.New(channelQueueLen, mtu, mac)
	// The guests' virtio-net offloads TX checksums (partial/pseudo-header only)
	// and libkrun strips the virtio_net_hdr carrying the offload flag, so the
	// on-wire checksums reaching us are not final. We trust frames arriving over
	// the local vsock, so advertise RX checksum offload — gVisor then marks
	// received packets checksum-validated (nic.go sets pkt.RXChecksumValidated
	// from this capability) and skips its (otherwise failing) IP+TCP checksum
	// verification. We do NOT set TX offload: netd computes real checksums on
	// frames sent to guests.
	ch.LinkEPCapabilities = stack.CapabilityRXChecksumOffload
	linkEP := ethernet.New(ch)
	if err := s.CreateNIC(nicID, linkEP); err != nil {
		return nil, fmt.Errorf("create NIC: %s", err)
	}
	// The gateway answers ARP for its own address and accepts spoofed source
	// addresses (it terminates flows on behalf of many guests).
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
	// Catch-all route out the NIC: inbound foreign dests are delivered locally to
	// the TCP forwarder via promiscuous mode, and stack-originated replies to the
	// guests route here.
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: nicID}})

	g := &Gateway{
		stack:    s,
		ep:       ch,
		gwMAC:    mac,
		macTable: make(map[tcpip.LinkAddress]*guestPort),
	}
	// Route all outbound guest TCP through the egress guard (default: public
	// internet allowed, host/private/metadata denied). Per-sandbox egress policy
	// and L7 secret substitution layer on here next.
	g.installTCPForwarder(&gateway.Dialer{
		Policy: &gateway.EgressPolicy{Default: gateway.PosturePublic},
	})
	return g, nil
}

// AddGuest registers a guest link as a switch port and immediately starts
// pumping its frames into the switch. Safe to call concurrently as sibling VMs
// connect (before or after Run).
func (g *Gateway) AddGuest(fc *gateway.FrameConn) *guestPort {
	p := &guestPort{fc: fc}
	g.mu.Lock()
	g.ports = append(g.ports, p)
	g.mu.Unlock()
	go g.runGuest(p)
	return p
}

// Run pumps the stack's outbound frames to the guests until ctx is cancelled or
// the stack link closes. Guest links are pumped by AddGuest.
func (g *Gateway) Run(ctx context.Context) error {
	err := g.stackOutLoop(ctx)
	g.ep.Close()
	return err
}

// runGuest pumps one guest link into the switch until it closes.
func (g *Gateway) runGuest(p *guestPort) {
	for {
		frame, err := p.fc.ReadFrame()
		if err != nil {
			g.removePort(p)
			return
		}
		g.switchFromGuest(p, frame)
	}
}

// switchFromGuest routes a frame received from guest port p: to the stack
// (gateway MAC), to a sibling (learned unicast), or flooded (broadcast/unknown).
func (g *Gateway) switchFromGuest(src *guestPort, frame []byte) {
	if len(frame) < header.EthernetMinimumSize {
		return
	}
	eth := header.Ethernet(frame)
	g.learn(eth.SourceAddress(), src)

	dst := eth.DestinationAddress()
	switch {
	case dst == g.gwMAC:
		g.toStack(frame)
	case isBroadcastOrMulticast(dst):
		// ARP / broadcast: the stack must see it (to answer ARP for .1) and every
		// other guest must see it (so siblings can be discovered).
		g.toStack(frame)
		g.flood(src, frame)
	default:
		if p := g.lookup(dst); p != nil {
			_ = p.write(frame)
		} else {
			// Unknown unicast: flood like a learning switch until the dst is learned.
			g.flood(src, frame)
		}
	}
}

// toStack injects a frame into the gVisor stack (the gateway port).
func (g *Gateway) toStack(frame []byte) {
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(frame),
	})
	g.ep.InjectInbound(0 /* ignored by ethernet */, pkt)
	pkt.DecRef()
}

// flood writes a frame to every guest port except the source.
func (g *Gateway) flood(src *guestPort, frame []byte) {
	g.mu.RLock()
	ports := append([]*guestPort(nil), g.ports...)
	g.mu.RUnlock()
	for _, p := range ports {
		if p != src {
			_ = p.write(frame)
		}
	}
}

// stackOutLoop pumps frames the stack emits (ARP replies for .1, forwarder
// SYN-ACKs, DNS) to the guest whose MAC they target (or floods broadcast).
func (g *Gateway) stackOutLoop(ctx context.Context) error {
	for {
		pkt := g.ep.ReadContext(ctx)
		if pkt == nil {
			return ctx.Err()
		}
		buf := pkt.ToBuffer()
		frame := buf.Flatten()
		pkt.DecRef()
		if len(frame) < header.EthernetMinimumSize {
			continue
		}
		dst := header.Ethernet(frame).DestinationAddress()
		if isBroadcastOrMulticast(dst) {
			g.flood(nil, frame)
			continue
		}
		if p := g.lookup(dst); p != nil {
			_ = p.write(frame)
		} else {
			g.flood(nil, frame) // not yet learned; flood
		}
	}
}

func (g *Gateway) learn(mac tcpip.LinkAddress, p *guestPort) {
	if mac == g.gwMAC || isBroadcastOrMulticast(mac) {
		return
	}
	g.mu.Lock()
	g.macTable[mac] = p
	g.mu.Unlock()
}

func (g *Gateway) lookup(mac tcpip.LinkAddress) *guestPort {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.macTable[mac]
}

func (g *Gateway) removePort(dead *guestPort) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, p := range g.ports {
		if p == dead {
			g.ports = append(g.ports[:i], g.ports[i+1:]...)
			break
		}
	}
	for mac, p := range g.macTable {
		if p == dead {
			delete(g.macTable, mac)
		}
	}
}

func isBroadcastOrMulticast(mac tcpip.LinkAddress) bool {
	if len(mac) == 0 {
		return true
	}
	return mac[0]&0x01 != 0 // multicast/broadcast group bit
}
