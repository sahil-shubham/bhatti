// Command bhatti-netd is the per-owner userspace network gateway (Approach A,
// DESIGN-bhatti-v2-networking.md §0c). It embeds a gVisor netstack on the
// owner's guests' virtio-net links (libkrun unixstream frames, via
// pkg/gateway.FrameConn) and is their router / DNS / egress-policer / L7 secret-
// substituter / inbound port-proxy / control door / audit chokepoint.
//
// Topology (L3-routed proxy). Each guest is point-to-point: it sees only the
// gateway .1 (address /32, on-link route to .1, default via .1) and sends ALL
// traffic — internet AND siblings — to .1. netd terminates every guest TCP flow
// at the forwarder and re-originates it: to the public internet via the host
// (policed by the egress guard), or, with an explicit sibling grant, to another
// guest of the same owner via the stack. Every attempted connection gets a
// policy verdict before dialing; no guest reaches another owner's network.
// The stack computes native checksums for the re-originated leg.
// netd is one owner's whole network; the single-guest case is just N=1.
package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"github.com/sahil-shubham/bhatti/pkg/gateway"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

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

var ownerGuestPool = netip.MustParsePrefix("100.64.0.0/10")

// Gateway is one owner's userspace network: a gVisor stack (the .1 gateway
// answering ARP and forwarding policy-checked TCP and UDP) bridged to guest links.
type Gateway struct {
	stack         *stack.Stack
	ep            *channel.Endpoint // the stack's link
	gwMAC         tcpip.LinkAddress
	gwIP          tcpip.Address
	subnet        tcpip.Subnet // the owner's guest subnet (for sibling routing)
	siblingSubnet netip.Prefix
	hardDeny      []netip.Prefix                                  // assembled locally, never taken from control wire
	hostServices  []netip.AddrPort                                // likewise: the daemon's own listeners on host addresses
	hostUDPDial   func(network, address string) (net.Conn, error) // injected only for hermetic external-UDP tests

	mu       sync.RWMutex
	ports    []*guestPort                     // all guest links
	macTable map[tcpip.LinkAddress]*guestPort // assigned guest MAC → port (stack→guest demux)

	polMu               sync.RWMutex
	guests              map[tcpip.Address]*guestState
	tokens              map[string]*guestState // attachment secret → registered IP/MAC
	defState            *guestState            // deny fallback for an unregistered guest
	lastUnregisteredLog time.Time              // one warning per gateway per minute

	cred *credProxy // credential substitution on :443; nil without a broker
}

// guestPort is one authenticated VM's virtio-net link. Its token is sent by the
// VMM before libkrun sees the fd, never by the untrusted guest's NIC.
type guestPort struct {
	fc           *gateway.FrameConn
	ip           tcpip.Address
	mac          tcpip.LinkAddress
	token        string
	lastSpoofLog time.Time // runGuest is this port's only reader
	wmu          sync.Mutex
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
	ip      tcpip.Address
	mac     tcpip.LinkAddress
	token   string
	pol     *gateway.EgressPolicy
	dialer  *gateway.Dialer
	names   nameCache // answered IP → the name the guest looked up (dns.go)
}

// NewGateway assigns the owner's /24 gateway, installs the forwarder and
// protects the host's interfaces and daemon listeners from every guest policy,
// except the daemon's own listening ports. Guest links are attached with AddGuest.
func NewGateway(gwIP tcpip.Address, prefixLen int, mac tcpip.LinkAddress, host hostProtection) (*Gateway, error) {
	if gwIP.Len() != 4 {
		return nil, fmt.Errorf("gateway address must be IPv4")
	}
	gwAddr := netip.AddrFrom4(gwIP.As4())
	if prefixLen != 24 || !ownerGuestPool.Contains(gwAddr) {
		return nil, fmt.Errorf("gateway %s/%d must be an owner /24 in %s", gwAddr, prefixLen, ownerGuestPool)
	}
	own := netip.PrefixFrom(gwAddr, prefixLen).Masked()
	hardDeny := make([]netip.Prefix, 0, len(host.addrs)+1+prefixLen-ownerGuestPool.Bits())
	hardDeny = append(hardDeny, host.addrs...)
	hardDeny = append(hardDeny, netip.PrefixFrom(gwAddr, 32))
	hardDeny = appendOtherOwnerRanges(hardDeny, own, ownerGuestPool)
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
	// not final. Only authenticated, source-validated frames reach gVisor; RX
	// checksum offload tells it to skip verification of these unfinished IP/TCP
	// checksums. TX offload stays off: replies need real checksums on the wire.
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

	defPol := &gateway.EgressPolicy{ExtraHardDeny: hardDeny, HostServices: host.services, SiblingSubnet: own}
	g := &Gateway{
		stack:         s,
		ep:            ch,
		gwMAC:         mac,
		gwIP:          gwIP,
		subnet:        protoAddr.AddressWithPrefix.Subnet(),
		siblingSubnet: own,
		hardDeny:      hardDeny,
		hostServices:  host.services,
		hostUDPDial:   net.Dial,
		macTable:      make(map[tcpip.LinkAddress]*guestPort),
		guests:        make(map[tcpip.Address]*guestState),
		tokens:        make(map[string]*guestState),
		defState:      &guestState{pol: defPol, dialer: &gateway.Dialer{Policy: defPol}},
	}
	// The forwarders resolve policy by the source IP before any host or
	// in-stack dial. No registration means no egress during create/recovery races.
	g.installTCPForwarder()
	g.installUDPForwarder()
	return g, nil
}

// SetSandbox atomically binds the per-VM attachment secret to its assigned IP,
// MAC and policy. A missing/malformed identity is never allowed to register.
func (g *Gateway) SetSandbox(guestIP, sandboxID string, pol *gateway.EgressPolicy, mac, token string) error {
	ip, err := netip.ParseAddr(guestIP)
	if err != nil || !ip.Is4() || !g.siblingSubnet.Contains(ip) || tcpip.AddrFrom4(ip.As4()) == g.gwIP {
		return fmt.Errorf("invalid guest IP")
	}
	guestAddr := tcpip.AddrFrom4(ip.As4())
	hw, err := net.ParseMAC(mac)
	if err != nil || len(hw) != 6 || isBroadcastOrMulticast(tcpip.LinkAddress(hw)) || tcpip.LinkAddress(hw) == g.gwMAC {
		return fmt.Errorf("invalid guest MAC")
	}
	if len(token) != gateway.GuestHelloSize {
		return fmt.Errorf("invalid guest attachment secret")
	}
	raw, err := hex.DecodeString(token)
	if err != nil || len(raw) != 32 || hex.EncodeToString(raw) != token {
		return fmt.Errorf("invalid guest attachment secret")
	}
	if pol == nil {
		pol = &gateway.EgressPolicy{}
	}
	copyPol := *pol
	copyPol.SiblingSubnet = g.siblingSubnet
	copyPol.ExtraHardDeny = g.hardDeny
	copyPol.HostServices = g.hostServices
	if len(pol.ExtraHardDeny) > 0 {
		copyPol.ExtraHardDeny = append(append(make([]netip.Prefix, 0, len(pol.ExtraHardDeny)+len(g.hardDeny)), pol.ExtraHardDeny...), g.hardDeny...)
	}
	st := &guestState{sandbox: sandboxID, ip: guestAddr, mac: tcpip.LinkAddress(hw), token: token, pol: &copyPol, dialer: &gateway.Dialer{Policy: &copyPol}}
	g.polMu.Lock()
	defer g.polMu.Unlock()
	if other := g.tokens[token]; other != nil && other != g.guests[guestAddr] {
		return fmt.Errorf("guest attachment secret already in use")
	}
	if old := g.guests[guestAddr]; old != nil {
		delete(g.tokens, old.token)
	}
	g.guests[guestAddr] = st
	g.tokens[token] = st
	return nil
}

// DelSandbox revokes policy and attachment identity together.
func (g *Gateway) DelSandbox(guestIP string) {
	ip, err := netip.ParseAddr(guestIP)
	if err != nil || !ip.Is4() {
		return
	}
	key := tcpip.AddrFrom4(ip.As4())
	g.polMu.Lock()
	if old := g.guests[key]; old != nil {
		delete(g.tokens, old.token)
	}
	delete(g.guests, key)
	g.polMu.Unlock()
}

// stateFor returns deny for an unregistered source; one warning per gateway
// per minute avoids a spoofed source flooding logs or growing an IP cache.
func (g *Gateway) stateFor(src tcpip.Address) *guestState {
	g.polMu.RLock()
	st := g.guests[src]
	g.polMu.RUnlock()
	if st != nil {
		return st
	}
	now := time.Now()
	g.polMu.Lock()
	shouldLog := now.Sub(g.lastUnregisteredLog) >= time.Minute
	if shouldLog {
		g.lastUnregisteredLog = now
	}
	g.polMu.Unlock()
	if shouldLog {
		slog.Warn("netd.unregistered_guest", "guest_ip", addrString(src))
	}
	return g.defState
}

// appendOtherOwnerRanges covers 100.64/10 except this owner's /24. Each
// prefix excludes one sibling branch in the binary CIDR tree; append into
// the pre-sized hard-deny slice rather than allocating a temporary range list.
func appendOtherOwnerRanges(dst []netip.Prefix, own, pool netip.Prefix) []netip.Prefix {
	addr := own.Addr().As4()
	for bits := pool.Bits() + 1; bits <= own.Bits(); bits++ {
		other := addr
		bit := bits - 1
		other[bit/8] ^= 1 << uint(7-bit%8)
		dst = append(dst, netip.PrefixFrom(netip.AddrFrom4(other), bits).Masked())
	}
	return dst
}

// isSibling reports whether addr is another guest of this owner (in the guest
// subnet, but not the gateway itself) — routed via the stack, not the host.
func (g *Gateway) isSibling(addr tcpip.Address) bool {
	return addr != g.gwIP && g.subnet.Contains(addr)
}

// authenticateGuest consumes a VMM-only, pre-frame secret on the stream.
// Unregistered or guessed tokens never become a gVisor port.
func (g *Gateway) authenticateGuest(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var hello [gateway.GuestHelloSize]byte
	if _, err := io.ReadFull(conn, hello[:]); err != nil {
		conn.Close()
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	g.polMu.RLock()
	st := g.tokens[string(hello[:])]
	if st == nil {
		g.polMu.RUnlock()
		conn.Close()
		return
	}
	g.AddGuest(gateway.NewFrameConn(conn), st.ip, st.mac, st.token)
	g.polMu.RUnlock()
}

// AddGuest installs a port only after authentication (or from in-process tests).
func (g *Gateway) AddGuest(fc *gateway.FrameConn, ip tcpip.Address, mac tcpip.LinkAddress, token string) *guestPort {
	p := &guestPort{fc: fc, ip: ip, mac: mac, token: token}
	g.mu.Lock()
	g.ports = append(g.ports, p)
	g.macTable[mac] = p
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
		if !g.allowIngress(p, frame) {
			continue
		}
		g.toStack(frame)
	}
}

// allowIngress gates policy lookup by the connection's registered identity,
// not by a guest-controlled Ethernet/IP header. No IPv6 identity is assigned.
func (g *Gateway) allowIngress(p *guestPort, frame []byte) bool {
	g.polMu.RLock()
	st := g.guests[p.ip]
	active := st != nil && st.token == p.token && st.mac == p.mac
	g.polMu.RUnlock()
	if active && len(frame) >= header.EthernetMinimumSize {
		eth := header.Ethernet(frame)
		if eth.SourceAddress() == p.mac {
			body := frame[header.EthernetMinimumSize:]
			switch eth.Type() {
			case header.IPv4ProtocolNumber:
				if len(body) >= header.IPv4MinimumSize && header.IPv4(body).SourceAddress() == p.ip {
					return true
				}
			case header.ARPProtocolNumber:
				if len(body) >= header.ARPSize {
					a := header.ARP(body)
					if a.IsValid() && tcpip.LinkAddress(a.HardwareAddressSender()) == p.mac && tcpip.AddrFrom4([4]byte(a.ProtocolAddressSender())) == p.ip {
						return true
					}
				}
			}
		}
	}
	if now := time.Now(); now.Sub(p.lastSpoofLog) >= time.Minute {
		p.lastSpoofLog = now
		slog.Warn("netd.spoofed_frame", "guest_ip", addrString(p.ip), "guest_mac", p.mac)
	}
	return false
}

// toStack injects a frame into the gVisor stack.
func (g *Gateway) toStack(frame []byte) {
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(frame),
	})
	g.ep.InjectInbound(0 /* ignored by ethernet */, pkt)
	pkt.DecRef()
}

// flood sends broadcasts only to currently registered attachments.
func (g *Gateway) flood(frame []byte) {
	g.mu.RLock()
	ports := append([]*guestPort(nil), g.ports...)
	g.mu.RUnlock()
	for _, p := range ports {
		if g.portActive(p) {
			_ = p.write(frame)
		}
	}
}

func (g *Gateway) portActive(p *guestPort) bool {
	g.polMu.RLock()
	st := g.guests[p.ip]
	active := st != nil && st.token == p.token && st.mac == p.mac
	g.polMu.RUnlock()
	return active
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
		if p := g.lookup(dst); p != nil && g.portActive(p) {
			_ = p.write(frame)
		} else {
			g.flood(frame)
		}
	}
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
