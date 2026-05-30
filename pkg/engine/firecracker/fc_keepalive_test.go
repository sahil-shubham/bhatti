//go:build linux

package firecracker

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
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

// fakeFCServer is a minimal Unix-socket HTTP server that counts new
// connections and replies 204 No Content. Enough surface to exercise
// fcAPIClient's Transport behaviour.
type fakeFCServer struct {
	socketPath string
	newConns   atomic.Int64
	server     *http.Server
	ln         net.Listener
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
			if state == http.StateNew {
				f.newConns.Add(1)
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

// TestFCAPIClient_ReusesConnection is the regression. A single client
// making N sequential PUTs must use a single connection.
func TestFCAPIClient_ReusesConnection(t *testing.T) {
	f := newFakeFCServer(t)
	client := fcAPIClient(f.socketPath)

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
	// With keep-alives the Transport opens one connection and reuses it.
	// Without keep-alives every request dials a fresh connection.
	if got != 1 {
		t.Errorf("expected 1 connection across %d sequential requests, got %d (keep-alives may be disabled or broken)",
			requests, got)
	}
}

// TestFCAPIClient_FreshClientFreshConnection verifies the inverse
// invariant: a fresh client (the "per-call fcAPIClient" pattern) does
// NOT pick up another client's idle connection. Each fcAPIClient call
// returns a Client with its own Transport, so its pool starts empty.
// Protects against an over-eager "let's add a package-level shared
// Transport" refactor that would reintroduce the stale-socket risk
// across FC process restarts.
func TestFCAPIClient_FreshClientFreshConnection(t *testing.T) {
	f := newFakeFCServer(t)

	for i := 0; i < 5; i++ {
		client := fcAPIClient(f.socketPath)
		ctx, cancel := context.WithCancel(context.Background())
		if err := fcPut(ctx, client, "/machine-config", `{}`); err != nil {
			cancel()
			t.Fatalf("client %d: %v", i, err)
		}
		cancel()
		// Don't CloseIdleConnections here — the idle conn lives in
		// this Transport, which goes out of scope at next iteration.
	}

	// 5 separate clients = 5 separate Transports = 5 fresh connections.
	got := f.newConns.Load()
	if got != 5 {
		t.Errorf("expected 5 connections across 5 fresh clients, got %d", got)
	}
}
