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

	polMu    sync.RWMutex
	guests   map[string]*guestState // guest gateway IP -> per-sandbox egress state
	defState *guestState            // fallback for an unregistered guest (public)
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

// guestState is one guest's per-sandbox egress config, delivered by the daemon
// over the control channel and keyed by the guest's gateway IP.
type guestState struct {
	sandbox string
	pol     *gateway.EgressPolicy
	dialer  *gateway.Dialer
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

	defPol := &gateway.EgressPolicy{Default: gateway.PosturePublic}
	g := &Gateway{
		stack:    s,
		ep:       ch,
		gwMAC:    mac,
		gwIP:     gwIP,
		subnet:   protoAddr.AddressWithPrefix.Subnet(),
		macTable: make(map[tcpip.LinkAddress]*guestPort),
		guests:   make(map[string]*guestState),
		// Unregistered guests keep today's behavior (public egress); the daemon
		// tightens each sandbox via the control channel (SetSandbox).
		defState: &guestState{pol: defPol, dialer: &gateway.Dialer{Policy: defPol}},
	}
	// Every guest TCP/UDP flow is terminated here and re-originated per-sandbox:
	// the egress guard vets the destination against THAT guest's policy (public
	// allowed; host/private/metadata denied), siblings route via the stack, and
	// UDP is DNS egress. Policy is looked up by source IP at request time.
	g.installTCPForwarder()
	g.installUDPForwarder()
	return g, nil
}

// SetSandbox registers or replaces a guest's per-sandbox egress state, keyed by
// its gateway IP. A nil policy means "use the default posture" (public).
// Implements gateway.ControlHandler — the daemon pushes over the control UDS.
func (g *Gateway) SetSandbox(guestIP, sandboxID string, pol *gateway.EgressPolicy) {
	if pol == nil {
		pol = &gateway.EgressPolicy{Default: gateway.PosturePublic}
	}
	g.polMu.Lock()
	g.guests[guestIP] = &guestState{sandbox: sandboxID, pol: pol, dialer: &gateway.Dialer{Policy: pol}}
	g.polMu.Unlock()
}

// DelSandbox drops a guest's state (on sandbox destroy).
func (g *Gateway) DelSandbox(guestIP string) {
	g.polMu.Lock()
	delete(g.guests, guestIP)
	g.polMu.Unlock()
}

// stateFor returns the per-guest egress state for a source address, or the
// default (public) when the guest isn't registered — so an unregistered guest
// keeps today's behavior instead of being hard-denied by a create/push race.
func (g *Gateway) stateFor(src tcpip.Address) *guestState {
	g.polMu.RLock()
	st := g.guests[addrString(src)]
	g.polMu.RUnlock()
	if st == nil {
		return g.defState
	}
	return st
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
