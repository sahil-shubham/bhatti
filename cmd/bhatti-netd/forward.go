package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/gateway"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const maxInFlightConn = 2048

// installTCPForwarder terminates EVERY guest TCP connection here and
// re-originates it: a sibling destination (same owner subnet) is dialed via the
// stack, which routes to that guest's link; everything else goes through the
// egress guard's vetting dialer — host/private/metadata denied, public allowed
// (the isolation TSI couldn't give). Dial-first so a denied or unreachable
// destination RSTs the guest cleanly.
func (g *Gateway) installTCPForwarder(dialer *gateway.Dialer) {
	fwd := tcp.NewForwarder(g.stack, 0, maxInFlightConn, func(r *tcp.ForwarderRequest) {
		id := r.ID()

		var up net.Conn
		var err error
		if g.isSibling(id.LocalAddress) {
			// Same-owner sibling: dial via the stack so it routes to the sibling's
			// link (native checksums, mediated + observable by netd).
			up, err = gonet.DialContextTCP(context.Background(), g.stack,
				tcpip.FullAddress{Addr: id.LocalAddress, Port: id.LocalPort}, ipv4.ProtocolNumber)
		} else {
			dest := net.JoinHostPort(addrString(id.LocalAddress), fmt.Sprint(id.LocalPort))
			up, err = dialer.DialContext(context.Background(), "tcp", dest)
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
		go splice(gonet.NewTCPConn(&wq, ep), up)
	})
	g.stack.SetTransportProtocolHandler(tcp.ProtocolNumber, fwd.HandlePacket)
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

// installUDPForwarder mirrors installTCPForwarder for UDP: it terminates each
// guest UDP flow and re-originates it host-side, vetted by the SAME egress
// policy (public allowed; host/private/metadata — and thus siblings in the
// 100.64/10 space — denied). Its purpose is DNS egress (UDP:53 to the public
// resolvers in the guest's resolv.conf); the address a guest subsequently
// TCP-connects to is still vetted by the TCP forwarder, so DNS rebinding into
// host/private space stays closed.
func (g *Gateway) installUDPForwarder(policy *gateway.EgressPolicy) {
	fwd := udp.NewForwarder(g.stack, func(r *udp.ForwarderRequest) (handled bool) {
		id := r.ID()
		ip, err := netip.ParseAddr(addrString(id.LocalAddress))
		if err != nil || !policy.Check("", ip).Allow {
			return false // drop: unparseable, or denied (host/private/metadata/sibling)
		}
		var wq waiter.Queue
		ep, terr := r.CreateEndpoint(&wq)
		if terr != nil {
			return false
		}
		guest := gonet.NewUDPConn(&wq, ep)
		dest := net.JoinHostPort(addrString(id.LocalAddress), fmt.Sprint(id.LocalPort))
		up, derr := net.Dial("udp", dest)
		if derr != nil {
			guest.Close()
			return false
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
