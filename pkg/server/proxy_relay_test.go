package server

import (
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// Regression tests for the goroutine leak in proxyWebSocket's
// fallback (no-deadline) path. Tranche 0a item #1 of PLAN-bhatti-v2.md.
//
// The bug: plain io.Copy in both directions, with one shared `done`
// channel and only the foreground call's exit triggering teardown.
//
//	go func() {
//	    io.Copy(tunnel, clientConn)   // bg blocks on clientConn.Read
//	    close(done)
//	}()
//	io.Copy(clientConn, tunnel)        // fg blocks on tunnel.Read
//	<-done
//
// If `tunnel` EOFs/closes while `clientConn` is still happily open
// (e.g. the engine-side connection dies but the browser is fine),
// fg returns from io.Copy, then blocks at <-done. The bg goroutine
// is still parked in clientConn.Read because nothing closed
// clientConn (the deferred Close at the function entry doesn't fire
// until proxyWebSocket returns, which it cannot do while waiting for
// `done`). The goroutine leaks indefinitely.
//
// The fix in proxy_handlers.go extracts the relay into
// relayBidirectional, which on each direction's exit closes the OTHER
// side's source to unblock the still-running direction. Both
// goroutines exit; both Close() calls return quickly.
//
// These tests exercise the function directly with net.Pipe() pairs.
// To force the no-deadline path we wrap one or both pipes in
// noDeadlineConn, which exposes only io.ReadWriteCloser (no
// SetReadDeadline), making the relay take the io.Copy branch.

// noDeadlineConn wraps a net.Conn but does NOT promote SetReadDeadline,
// so relayBidirectional fails the deadlineConn type assertion and falls
// back to plain io.Copy — the path with the original leak.
type noDeadlineConn struct {
	c      net.Conn
	closed atomic.Bool
}

func (n *noDeadlineConn) Read(p []byte) (int, error)  { return n.c.Read(p) }
func (n *noDeadlineConn) Write(p []byte) (int, error) { return n.c.Write(p) }
func (n *noDeadlineConn) Close() error {
	// Idempotent so the relay's internal Close + the test's deferred
	// Close on the outer pipe don't trip.
	if n.closed.Swap(true) {
		return nil
	}
	return n.c.Close()
}

func wrapNoDeadline(c net.Conn) *noDeadlineConn { return &noDeadlineConn{c: c} }

// runRelayWithTimeout calls relayBidirectional on a separate goroutine
// and waits up to `timeout` for it to return. A return is success; a
// timeout means a direction is still blocked, which is the leak.
func runRelayWithTimeout(t *testing.T, a, b io.ReadWriteCloser, idle, timeout time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		relayBidirectional(a, b, idle)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatal("relayBidirectional did not return within timeout — a direction is still blocked (goroutine leak)")
	}
}

// TestRelayBidirectional_NoDeadline_TunnelClosesFirst_NoLeak is the
// regression for the leak. Both ends are wrapped to hide deadlines,
// so the relay takes the io.Copy branch. The tunnel side closes
// first; with the fix the relay returns promptly. Without the fix
// (no close-the-other-side teardown) the test times out because the
// bg goroutine is stuck reading the still-open client side.
func TestRelayBidirectional_NoDeadline_TunnelClosesFirst_NoLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	clientOuter, clientInner := net.Pipe()
	tunnelOuter, tunnelInner := net.Pipe()
	defer clientOuter.Close()
	defer tunnelOuter.Close()

	clientWrapped := wrapNoDeadline(clientInner)
	tunnelWrapped := wrapNoDeadline(tunnelInner)

	// Start the relay; while it runs, close the tunnel side from
	// the outer end. The relay's tunnel-side Read returns EOF, the
	// foreground returns, and (with the fix) it closes the client
	// side so the bg goroutine's clientConn.Read returns ErrClosedPipe.
	go func() {
		// Small delay so the relay is definitely inside its Read
		// calls before we close. Not strictly necessary but makes
		// the scenario deterministic.
		time.Sleep(20 * time.Millisecond)
		tunnelOuter.Close()
	}()

	// 2-second timeout is generous — with the fix this returns in
	// well under 100ms.
	runRelayWithTimeout(t, clientWrapped, tunnelWrapped, time.Minute, 2*time.Second)
}

// TestRelayBidirectional_NoDeadline_ClientClosesFirst_NoLeak is the
// mirror case: client side disconnects first, tunnel side is the
// blocked direction the fix must unblock.
func TestRelayBidirectional_NoDeadline_ClientClosesFirst_NoLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	clientOuter, clientInner := net.Pipe()
	tunnelOuter, tunnelInner := net.Pipe()
	defer clientOuter.Close()
	defer tunnelOuter.Close()

	clientWrapped := wrapNoDeadline(clientInner)
	tunnelWrapped := wrapNoDeadline(tunnelInner)

	go func() {
		time.Sleep(20 * time.Millisecond)
		clientOuter.Close()
	}()

	runRelayWithTimeout(t, clientWrapped, tunnelWrapped, time.Minute, 2*time.Second)
}

// TestRelayBidirectional_DeadlineCapable_NoLeak exercises the path
// where both sides implement SetReadDeadline (the production case for
// the authenticated proxy). The fix changes nothing semantically for
// this path; the test ensures we didn't regress it. With the close-
// the-other-side teardown, this returns immediately on remote close
// instead of waiting up to wsIdleTimeout for the deadline.
func TestRelayBidirectional_DeadlineCapable_NoLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	clientOuter, clientInner := net.Pipe()
	tunnelOuter, tunnelInner := net.Pipe()
	defer clientOuter.Close()
	defer tunnelOuter.Close()

	go func() {
		time.Sleep(20 * time.Millisecond)
		tunnelOuter.Close()
	}()

	// Pass net.Conns directly (they implement deadlineConn).
	// idle is short but not relevant — the close should win the race.
	runRelayWithTimeout(t, clientInner, tunnelInner, time.Minute, 2*time.Second)
}

// TestRelayBidirectional_NoDeadline_DataFlows verifies the happy path:
// bytes go both directions, then a graceful close ends the relay
// without leaking. Catches a regression where the fix-up close call
// could prematurely close a side that's still actively transferring.
func TestRelayBidirectional_NoDeadline_DataFlows(t *testing.T) {
	defer goleak.VerifyNone(t)

	clientOuter, clientInner := net.Pipe()
	tunnelOuter, tunnelInner := net.Pipe()
	defer clientOuter.Close()
	defer tunnelOuter.Close()

	clientWrapped := wrapNoDeadline(clientInner)
	tunnelWrapped := wrapNoDeadline(tunnelInner)

	relayDone := make(chan struct{})
	go func() {
		relayBidirectional(clientWrapped, tunnelWrapped, time.Minute)
		close(relayDone)
	}()

	// Client writes a request; tunnel reads it.
	clientReq := []byte("GET /ws HTTP/1.1\r\nUpgrade: websocket\r\n\r\n")
	go func() { clientOuter.Write(clientReq) }()
	got := make([]byte, len(clientReq))
	if _, err := tunnelOuter.Read(got); err != nil {
		t.Fatalf("tunnel read: %v", err)
	}
	if string(got) != string(clientReq) {
		t.Fatalf("tunnel got %q, want %q", got, clientReq)
	}

	// Tunnel writes a response; client reads it.
	tunnelResp := []byte("HTTP/1.1 101 Switching Protocols\r\n\r\n")
	go func() { tunnelOuter.Write(tunnelResp) }()
	got2 := make([]byte, len(tunnelResp))
	if _, err := clientOuter.Read(got2); err != nil {
		t.Fatalf("client read: %v", err)
	}
	if string(got2) != string(tunnelResp) {
		t.Fatalf("client got %q, want %q", got2, tunnelResp)
	}

	// Now close one side to end the relay.
	tunnelOuter.Close()

	select {
	case <-relayDone:
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not return after tunnel close")
	}
}
