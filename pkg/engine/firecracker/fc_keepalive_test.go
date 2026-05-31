//go:build linux

package firecracker

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// Regression test for the per-call fcAPIClient connection thrash.
// Tranche 0a item #5 of PLAN-bhatti-v2.md.
//
// Pre-fix, fcAPIClient set DisableKeepAlives=true. That forced one
// new Unix socket connection per request. Create's ~10-PUT boot
// configuration sequence dialed and closed the FC API socket ten
// times in series, plus the InstanceStart at the end. Cheap per
// connection but compounded under autoscaler load.
//
// Post-fix, keep-alives are on, so the Transport keeps the connection
// open across requests on the same client. A sequence of N requests
// uses a single underlying connection.
//
// The test spins up a Unix-socket HTTP server with a ConnState hook
// that counts StateNew transitions (one per accepted connection), and
// verifies the connection count is 1 across 20 sequential PUTs.

// fakeFCServer is a minimal Unix-socket HTTP server that tracks
// connection lifecycle and replies 204 No Content. Enough surface to
// exercise fcAPIClient's Transport behaviour, including the
// accumulation-of-stale-connections failure mode that broke
// TestPerfPauseResume in v1.11.11.
type fakeFCServer struct {
	socketPath string
	newConns   atomic.Int64
	closedConn atomic.Int64
	server     *http.Server
	ln         net.Listener
}

// activeConns is StateNew - StateClosed: connections that have been
// accepted but not yet closed from the server's view. With proper
// cleanup this drops to 0 after each client lifecycle; without it,
// the count grows monotonically.
func (f *fakeFCServer) activeConns() int64 {
	return f.newConns.Load() - f.closedConn.Load()
}

func newFakeFCServer(t *testing.T) *fakeFCServer {
	t.Helper()
	// Unix socket paths are 104 bytes on macOS / 108 on Linux. t.TempDir()
	// can be long on macOS — use a short name and hope the parent isn't
	// pathological.
	socketPath := filepath.Join(t.TempDir(), "fc.sock")

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix %q: %v", socketPath, err)
	}

	f := &fakeFCServer{socketPath: socketPath, ln: ln}
	f.server = &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}),
		ConnState: func(_ net.Conn, state http.ConnState) {
			switch state {
			case http.StateNew:
				f.newConns.Add(1)
			case http.StateClosed, http.StateHijacked:
				f.closedConn.Add(1)
			}
		},
	}
	go func() { _ = f.server.Serve(ln) }()
	t.Cleanup(func() {
		_ = f.server.Close()
		_ = ln.Close()
	})
	return f
}

// TestFCAPIClient_ReusesConnection is the within-call regression. A
// single client making N sequential PUTs must use a single underlying
// connection (Create's ~10-PUT boot sequence is the motivating case).
func TestFCAPIClient_ReusesConnection(t *testing.T) {
	f := newFakeFCServer(t)
	client, done := fcAPIClient(f.socketPath)
	defer done()

	const requests = 20
	for i := 0; i < requests; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		if err := fcPut(ctx, client, "/machine-config", `{}`); err != nil {
			cancel()
			t.Fatalf("request %d: %v", i, err)
		}
		cancel()
	}

	got := f.newConns.Load()
	if got != 1 {
		t.Errorf("expected 1 connection across %d sequential requests, got %d (keep-alives may be disabled or broken)",
			requests, got)
	}
}

// TestFCAPIClient_CleanupClosesConnection is the across-call regression
// for the v1.11.11 TestPerfPauseResume failure. Each short-lived client
// (the pattern that Pause/Resume/BalloonSet use) MUST close its idle
// connection when done. Without this, repeated pause/resume cycles
// accumulate stale half-open sockets on Firecracker's side and
// eventually FC closes a connection mid-write — the test failed at
// iteration 4 of TestPerfPauseResume with EPIPE.
func TestFCAPIClient_CleanupClosesConnection(t *testing.T) {
	f := newFakeFCServer(t)

	client, done := fcAPIClient(f.socketPath)
	ctx, cancel := context.WithCancel(context.Background())
	if err := fcPut(ctx, client, "/machine-config", `{}`); err != nil {
		t.Fatalf("put: %v", err)
	}
	cancel()

	// Before cleanup: connection is in keep-alive idle state — server
	// sees it as Idle, not Closed.
	if active := f.activeConns(); active != 1 {
		t.Fatalf("pre-cleanup active=%d, want 1", active)
	}

	done()

	// After cleanup: the server-side Close should fire within a tick
	// of the FIN.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f.activeConns() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("after done(): active=%d, want 0 (cleanup didn't close the connection)",
		f.activeConns())
}

// TestFCAPIClient_ManyClientsCleanupDoesNotAccumulate is the direct
// reproducer for TestPerfPauseResume's failure mode. Many short-lived
// clients each doing one PUT, with defer-cleanup, must keep the
// server-side active connection count bounded — specifically, never
// more than one in flight at a time when calls are sequential.
func TestFCAPIClient_ManyClientsCleanupDoesNotAccumulate(t *testing.T) {
	f := newFakeFCServer(t)

	const iterations = 20 // matches TestPerfPauseResume's cycle count
	for i := 0; i < iterations; i++ {
		func() {
			client, done := fcAPIClient(f.socketPath)
			defer done()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := fcPut(ctx, client, "/machine-config", `{}`); err != nil {
				t.Fatalf("iter %d: %v", i, err)
			}
		}()
	}

	// After all iterations, every connection should be closed. Give
	// the server a brief moment to observe the FINs.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f.activeConns() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	active := f.activeConns()
	newC := f.newConns.Load()
	if active != 0 {
		t.Errorf("after %d short-lived clients with defer cleanup: active=%d (connections leaked); newConns=%d",
			iterations, active, newC)
	}
	// Each short-lived client should dial fresh (we explicitly tear down
	// each one). If we ever see fewer than `iterations` new conns, the
	// test is silently sharing a Transport via some refactor and the
	// across-call regression coverage is gone.
	if newC != iterations {
		t.Errorf("newConns=%d, want %d (each client should dial fresh)", newC, iterations)
	}
}
