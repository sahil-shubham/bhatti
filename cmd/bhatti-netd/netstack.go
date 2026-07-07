// Command bhatti-netd is the per-owner userspace network gateway (Approach A,
// DESIGN-bhatti-v2-networking.md §0c). It embeds a gVisor netstack on the
// owner's guests' virtio-net links (libkrun unixstream frames, via
// pkg/gateway.FrameConn) and is their router / DNS / egress-policer / L7 secret-
// substituter / inbound port-proxy / control door / audit chokepoint.
//
// Topology (L3-routed proxy). Each guest is point-to-point: it sees only the
// gateway .1 (address /32, on-link route to .1, default via .1) and sends ALL
// traffic — internet AND siblings — to .1. netd terminates every guest TCP flow
// at the forwarder and re-originates it: to the internet via the host (policed
// by the egress guard), or to a sibling (same 100.64.<owner>.0/24) via the stack
// itself, which routes to that guest's link. So every flow — egress and
// sibling — passes through the same policed, observable chokepoint, guests never
// reach each other directly, and checksums are native (the stack computes them
// for the re-originated leg; guest RX checksum offload is honored on ingress).
// netd is one owner's whole network; the single-guest case is just N=1.
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

// Gateway is one owner's userspace network: a gVisor stack (the .1 gateway that
// answers ARP, runs the egress + sibling TCP forwarder, and later DNS/control)
// bridged to N point-to-point guest links.
type Gateway struct {
	stack  *stack.Stack
	ep     *channel.Endpoint // the stack's link
	gwMAC  tcpip.LinkAddress
	gwIP   tcpip.Address
	subnet tcpip.Subnet // the owner's guest subnet (for sibling routing)

	mu       sync.RWMutex
	ports    []*guestPort                     // all guest links
	macTable map[tcpip.LinkAddress]*guestPort // learned guest MAC → port (stack→guest demux)
}

// guestPort is one guest's virtio-net link.
type guestPort struct {
	fc  *gateway.FrameConn
	wmu sync.Mutex // serialize concurrent writes to this guest link
}

func (p *guestPort) write(frame []byte) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	return p.fc.WriteFrame(frame)
}

// NewGateway builds the stack, assigns the gateway address gwIP/prefix, sets a
// catch-all route (so it can reach any guest link), installs the forwarder, and
// returns the gateway with no guest links yet. Attach guests with AddGuest. mac
// is the gateway's link address (the guests' next hop).
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
	// Guests offload TX checksums (partial/pseudo-header only) and libkrun strips
	// the virtio_net_hdr flag that says so, so on-wire checksums reaching us are
	// not final. We trust frames arriving over the local vsock, so advertise RX
	// checksum offload — gVisor marks received packets checksum-validated and
	// skips its (otherwise failing) IP+TCP checksum verification. We do NOT set
	// TX offload: the stack computes real checksums on frames it sends to guests
	// (including a re-originated sibling leg), so no manual fixups are needed.
	ch.LinkEPCapabilities = stack.CapabilityRXChecksumOffload
	linkEP := ethernet.New(ch)
	if err := s.CreateNIC(nicID, linkEP); err != nil {
		return nil, fmt.Errorf("create NIC: %s", err)
	}
	// Promiscuous so foreign egress dests are locally delivered to the forwarder;
	// spoofing so the stack can originate the sibling leg.
	if err := s.SetPromiscuousMode(nicID, true); err != nil {
		return nil, fmt.Errorf("promiscuous: %s", err)
	}
	if err := s.SetSpoofing(nicID, true); err != nil {
		return nil, fmt.Errorf("spoofing: %s", err)
	}

	protoAddr := tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: gwIP, PrefixLen: prefixLen},
	}
	if err := s.AddProtocolAddress(nicID, protoAddr, stack.AddressProperties{}); err != nil {
		return nil, fmt.Errorf("add gateway address: %s", err)
	}
	// Catch-all route out the NIC: inbound foreign dests are delivered locally to
	// the forwarder (promiscuous); a re-originated sibling leg routes here and
	// ARPs the target guest on its link.
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: nicID}})

	g := &Gateway{
		stack:    s,
		ep:       ch,
		gwMAC:    mac,
		gwIP:     gwIP,
		subnet:   protoAddr.AddressWithPrefix.Subnet(),
		macTable: make(map[tcpip.LinkAddress]*guestPort),
	}
	// Every guest TCP flow is terminated here and re-originated: internet via the
	// egress guard (public allowed; host/private/metadata denied), siblings via
	// the stack. Per-sandbox egress policy and L7 substitution layer on next.
	g.installTCPForwarder(&gateway.Dialer{
		Policy: &gateway.EgressPolicy{Default: gateway.PosturePublic},
	})
	return g, nil
}

// isSibling reports whether addr is another guest of this owner (in the guest
// subnet, but not the gateway itself) — routed via the stack, not the host.
func (g *Gateway) isSibling(addr tcpip.Address) bool {
	return addr != g.gwIP && g.subnet.Contains(addr)
}

// AddGuest registers a guest link and starts pumping its frames into the stack.
// Safe to call concurrently as sibling VMs connect (before or after Run).
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

// runGuest pumps one guest link into the stack until it closes.
func (g *Gateway) runGuest(p *guestPort) {
	for {
		frame, err := p.fc.ReadFrame()
		if err != nil {
			g.removePort(p)
			return
		}
		if len(frame) < header.EthernetMinimumSize {
			continue
		}
		// Guests only ever talk to the gateway (.1), so every guest frame goes to
		// the stack. Learn the source MAC so stack-originated replies can be
		// demuxed back to this link.
		g.learn(header.Ethernet(frame).SourceAddress(), p)
		g.toStack(frame)
	}
}

// toStack injects a frame into the gVisor stack.
func (g *Gateway) toStack(frame []byte) {
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(frame),
	})
	g.ep.InjectInbound(0 /* ignored by ethernet */, pkt)
	pkt.DecRef()
}

// flood writes a frame to every guest port (used for stack-originated broadcasts
// like the ARP the stack sends to reach a sibling).
func (g *Gateway) flood(frame []byte) {
	g.mu.RLock()
	ports := append([]*guestPort(nil), g.ports...)
	g.mu.RUnlock()
	for _, p := range ports {
		_ = p.write(frame)
	}
}

// stackOutLoop pumps frames the stack emits (ARP replies for .1, forwarder
// SYN-ACKs, the ARP/SYN of a re-originated sibling leg, DNS) to the guest whose
// MAC they target — or floods broadcast (e.g. the stack's ARP for a sibling).
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
			g.flood(frame)
			continue
		}
		if p := g.lookup(dst); p != nil {
			_ = p.write(frame)
		} else {
			g.flood(frame) // not yet learned; flood
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
