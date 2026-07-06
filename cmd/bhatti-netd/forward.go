package main

import (
	"context"
	"fmt"
	"io"
	"net"

	"github.com/sahil-shubham/bhatti/pkg/gateway"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const maxInFlightConn = 2048

// installTCPForwarder makes the gateway route EVERY outbound guest TCP
// connection through the egress guard's vetting dialer — host/private/metadata
// denied, public allowed (the isolation TSI couldn't give) — and splices the
// accepted guest endpoint to the real upstream. Dial-first so a denied or
// unreachable destination RSTs the guest cleanly.
func (g *Gateway) installTCPForwarder(dialer *gateway.Dialer) {
	fwd := tcp.NewForwarder(g.stack, 0, maxInFlightConn, func(r *tcp.ForwarderRequest) {
		id := r.ID()
		dest := net.JoinHostPort(addrString(id.LocalAddress), fmt.Sprint(id.LocalPort))

		up, err := dialer.DialContext(context.Background(), "tcp", dest)
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
