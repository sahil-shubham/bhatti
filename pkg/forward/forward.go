// Package forward bridges a host-side TCP listener to a port inside a guest,
// over the engine's vsock Tunnel primitive. It is the building block for the
// `bhatti forward` dev convenience (host↔guest) and the server-brokered
// inter-sandbox mesh (each sandbox gets a stable host endpoint that other
// sandboxes reach via the host). Engine-agnostic: anything implementing
// Tunneler (both krucible and Firecracker do) works.
package forward

import (
	"context"
	"io"
	"net"
)

// Tunneler is the slice of engine.Engine the forwarder needs: a bidirectional
// byte stream to localhost:port inside the guest.
type Tunneler interface {
	Tunnel(ctx context.Context, id string, port int) (io.ReadWriteCloser, error)
}

// Serve binds listenAddr and forwards each accepted TCP connection to guest
// `port` via eng.Tunnel(id, port). It returns the listener immediately (the
// accept loop runs in the background); close the listener to stop forwarding.
// Protocol-agnostic — raw bytes, so any TCP service (HTTP, postgres, redis, …)
// works, unlike the HTTP-aware public proxy.
//
// onConnect, if non-nil, runs before each tunnel is opened — e.g. wake-on-connect
// via the server's thermal EnsureHot, so connecting to the host port transparently
// revives a warm/cold sandbox. A non-nil error from onConnect drops the connection.
func Serve(eng Tunneler, id string, port int, listenAddr string, onConnect func(context.Context) error) (net.Listener, error) {
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			go bridge(eng, id, port, conn, onConnect)
		}
	}()
	return ln, nil
}

func bridge(eng Tunneler, id string, port int, conn net.Conn, onConnect func(context.Context) error) {
	defer conn.Close()
	ctx := context.Background()
	if onConnect != nil {
		if err := onConnect(ctx); err != nil {
			return
		}
	}
	tun, err := eng.Tunnel(ctx, id, port)
	if err != nil {
		return
	}
	defer tun.Close()
	relay(conn, tun)
}

// relay copies bytes in both directions until either side ends, then closes the
// other side so its blocked Read unblocks and the goroutine exits (no leak).
func relay(a, b io.ReadWriteCloser) {
	done := make(chan struct{})
	go func() {
		io.Copy(b, a)
		b.Close()
		close(done)
	}()
	io.Copy(a, b)
	a.Close()
	<-done
}
