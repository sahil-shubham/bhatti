package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const maxInFlightConn = 2048

// installTCPForwarder terminates guest TCP and checks the source sandbox's
// policy before dialing either the host network or an in-stack sibling.
// Dial-first RSTs a denied or unreachable destination cleanly.
func (g *Gateway) installTCPForwarder() {
	fwd := tcp.NewForwarder(g.stack, 0, maxInFlightConn, func(r *tcp.ForwarderRequest) {
		id := r.ID()

		st := g.stateFor(id.RemoteAddress)
		ip, perr := netip.ParseAddr(addrString(id.LocalAddress))
		if perr != nil {
			r.Complete(true)
			return
		}
		sibling := g.isSibling(id.LocalAddress)
		var up net.Conn
		var err error
		if sibling {
			if !g.siblingAllowed(st, "tcp", ip, id.LocalPort) {
				r.Complete(true)
				return
			}
			// In-stack dialing routes to the sibling's link, not the host.
			up, err = gonet.DialContextTCP(context.Background(), g.stack,
				tcpip.FullAddress{Addr: id.LocalAddress, Port: id.LocalPort}, ipv4.ProtocolNumber)
		} else {
			up, err = st.dialer.DialAs(context.Background(), "tcp", st.names.lookup(ip), ip, id.LocalPort)
		}
		if err != nil {
			r.Complete(true) // RST: denied by policy or unreachable
			return
		}
		var wq waiter.Queue
		ep, terr := r.CreateEndpoint(&wq)
		if terr != nil {
			up.Close()
			r.Complete(true)
			return
		}
		r.Complete(false)
		guest := gonet.NewTCPConn(&wq, ep)
		// TLS to the internet from a registered sandbox may carry a placeholder
		// for a granted host (intercept.go); the policy check above already
		// passed, and up is the address the guest asked for.
		if g.cred != nil && !sibling && st.sandbox != "" && id.LocalPort == tlsPort {
			go g.cred.serve(guest, up, st.sandbox)
			return
		}
		go splice(guest, up)
	})
	g.stack.SetTransportProtocolHandler(tcp.ProtocolNumber, fwd.HandlePacket)
}

// siblingAllowed uses the same verdict for TCP and UDP, before either transport
// dials. A debug verdict records both granted and denied sibling attempts.
func (g *Gateway) siblingAllowed(st *guestState, protocol string, ip netip.Addr, port uint16) bool {
	v := st.pol.Check("", ip)
	slog.Debug("netd.egress_verdict", "sandbox_id", st.sandbox, "protocol", protocol,
		"destination", ip, "port", port, "allow", v.Allow, "reason", v.Reason)
	return v.Allow
}

// splice copies bidirectionally between the guest endpoint and the upstream,
// half-closing each direction on EOF and tearing down when both are done.
func splice(guest, up net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(up, guest)
	go cp(guest, up)
	<-done
	<-done
	guest.Close()
	up.Close()
}

func addrString(a tcpip.Address) string {
	if a.Len() == 4 {
		b := a.As4()
		return net.IP(b[:]).String()
	}
	b := a.As16()
	return net.IP(b[:]).String()
}

const udpIdleTimeout = 30 * time.Second

// installUDPForwarder mirrors TCP: sibling datagrams are checked and routed
// inside the stack, while non-sibling datagrams use the host dialer. External
// DNS is still proxied for allow-host name recording (dns.go).
func (g *Gateway) installUDPForwarder() {
	fwd := udp.NewForwarder(g.stack, func(r *udp.ForwarderRequest) (handled bool) {
		id := r.ID()
		st := g.stateFor(id.RemoteAddress)
		ip, err := netip.ParseAddr(addrString(id.LocalAddress))
		if err != nil {
			return true
		}
		dest := net.JoinHostPort(ip.String(), fmt.Sprint(id.LocalPort))
		sibling := g.isSibling(id.LocalAddress)
		if sibling {
			if !g.siblingAllowed(st, "udp", ip, id.LocalPort) {
				return true // handled and dropped, not relayed to the host
			}
		} else if id.LocalPort == dnsPort {
			if !resolverAllowed(ip, st.pol) {
				return true
			}
			var wq waiter.Queue
			ep, terr := r.CreateEndpoint(&wq)
			if terr != nil {
				return true
			}
			go serveDNS(gonet.NewUDPConn(&wq, ep), dest, st)
			return true
		} else if !st.pol.Check("", ip).Allow {
			return true // handled and dropped
		}
		var wq waiter.Queue
		ep, terr := r.CreateEndpoint(&wq)
		if terr != nil {
			return true
		}
		guest := gonet.NewUDPConn(&wq, ep)
		var up net.Conn
		var derr error
		if sibling {
			// Spoofing lets an unbound UDP socket select the sibling's IP
			// as its source, producing a self-addressed frame to our MAC.
			// Bind the re-originated leg to the gateway's assigned IP.
			up, derr = gonet.DialUDP(g.stack, &tcpip.FullAddress{NIC: nicID, Addr: g.gwIP},
				&tcpip.FullAddress{NIC: nicID, Addr: id.LocalAddress, Port: id.LocalPort}, ipv4.ProtocolNumber)
		} else {
			up, derr = g.hostUDPDial("udp", dest)
		}
		if derr != nil {
			guest.Close()
			return true
		}
		go udpRelay(guest, up, udpIdleTimeout)
		return true
	})
	g.stack.SetTransportProtocolHandler(udp.ProtocolNumber, fwd.HandlePacket)
}

// udpRelay copies datagrams both ways until either side is idle for `idle`
// (UDP has no EOF, so an idle deadline reaps the flow after the round-trip).
func udpRelay(guest, up net.Conn, idle time.Duration) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		buf := make([]byte, 64*1024)
		for {
			src.SetReadDeadline(time.Now().Add(idle))
			n, rerr := src.Read(buf)
			if n > 0 {
				dst.Write(buf[:n])
			}
			if rerr != nil {
				break
			}
		}
		done <- struct{}{}
	}
	go cp(up, guest)
	go cp(guest, up)
	<-done
	guest.Close()
	up.Close()
	<-done
}
