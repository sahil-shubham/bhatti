package server

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

func tunnelURL(tsURL, sandbox, query string) string {
	return "ws" + strings.TrimPrefix(tsURL, "http") + "/sandboxes/" + sandbox + "/tunnel" + query
}

func tunnelHeader(token string) http.Header {
	header := make(http.Header)
	header.Set("Authorization", "Bearer "+token)
	return header
}

func waitTunnelDetached(t *testing.T, srv *Server, engineID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for srv.hasInteractiveAttach(engineID) {
		if time.Now().After(deadline) {
			t.Fatal("tunnel remained attached after the stream closed")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSandboxTunnelRoundTripAndDetach(t *testing.T) {
	srv, ts := setup(t)
	sb := createSandbox(t, ts, "tunnel-roundtrip")
	eng := srv.engine.(*mockEngine)
	eng.TunnelPeer = make(chan net.Conn, 1)

	ws, resp, err := websocket.DefaultDialer.Dial(tunnelURL(ts.URL, sb.Name, "?port=8080"), tunnelHeader(testAPIKey))
	if err != nil {
		t.Fatalf("upgrade: %v (response: %v)", err, resp)
	}
	defer ws.Close()
	var guest net.Conn
	select {
	case guest = <-eng.TunnelPeer:
	case <-time.After(2 * time.Second):
		t.Fatal("engine.Tunnel was not called")
	}
	defer guest.Close()
	if !srv.hasInteractiveAttach(sb.EngineID) {
		t.Fatal("tunnel did not pin its sandbox hot while open")
	}
	eng.mu.Lock()
	thermal := eng.thermal[sb.EngineID]
	eng.mu.Unlock()
	if thermal != "hot" {
		t.Fatalf("tunnel did not wake sandbox: thermal state %q", thermal)
	}

	// Separate WS messages form one raw TCP byte stream in the guest.
	if err := ws.WriteMessage(websocket.BinaryMessage, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteMessage(websocket.BinaryMessage, []byte("second")); err != nil {
		t.Fatal(err)
	}
	if err := guest.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len("firstsecond"))
	if _, err := io.ReadFull(guest, got); err != nil {
		t.Fatalf("read guest bytes: %v", err)
	}
	if !bytes.Equal(got, []byte("firstsecond")) {
		t.Fatalf("guest received %q, want firstsecond", got)
	}

	want := []byte{0, 1, 2, 0xff, 0x80}
	writeDone := make(chan error, 1)
	go func() {
		_, err := guest.Write(want)
		writeDone <- err
	}()
	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	kind, data, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read WS bytes: %v", err)
	}
	if kind != websocket.BinaryMessage || !bytes.Equal(data, want) {
		t.Fatalf("websocket received frame %d %v, want binary %v", kind, data, want)
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("write guest bytes: %v", err)
	}

	ws.Close()
	guest.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := guest.Read(make([]byte, 1)); err == nil {
		t.Fatal("guest stream stayed open after websocket closed")
	}
	waitTunnelDetached(t, srv, sb.EngineID)
}

func TestSandboxTunnelGuestCloseDetaches(t *testing.T) {
	srv, ts := setup(t)
	sb := createSandbox(t, ts, "tunnel-guest-close")
	eng := srv.engine.(*mockEngine)
	eng.TunnelPeer = make(chan net.Conn, 1)
	ws, resp, err := websocket.DefaultDialer.Dial(tunnelURL(ts.URL, sb.ID, "?port=8080"), tunnelHeader(testAPIKey))
	if err != nil {
		t.Fatalf("upgrade: %v (response: %v)", err, resp)
	}
	defer ws.Close()
	var guest net.Conn
	select {
	case guest = <-eng.TunnelPeer:
	case <-time.After(2 * time.Second):
		t.Fatal("engine.Tunnel was not called")
	}
	if !srv.hasInteractiveAttach(sb.EngineID) {
		t.Fatal("live guest tunnel was not attached")
	}
	guest.Close()
	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := ws.ReadMessage(); err == nil {
		t.Fatal("guest closure left websocket open")
	}
	waitTunnelDetached(t, srv, sb.EngineID)
}

func TestSandboxTunnelRejectsMissingTokenAndOtherUser(t *testing.T) {
	srv, ts := setup(t)
	sb := createSandbox(t, ts, "tunnel-private")
	url := tunnelURL(ts.URL, sb.ID, "?port=8080")
	if err := srv.store.CreateUser(store.User{
		ID: "usr_tunnel_other", Name: "tunnel-other", APIKeyHash: sha256Hex("other-token"),
		MaxSandboxes: 5, MaxCPUsPerSandbox: 4, MaxMemoryMBPerSandbox: 4096,
		SubnetIndex: 2, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		header http.Header
		status int
	}{
		{name: "missing token", status: http.StatusUnauthorized},
		{name: "other owner", header: tunnelHeader("other-token"), status: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, resp, err := websocket.DefaultDialer.Dial(url, tc.header)
			if ws != nil {
				ws.Close()
			}
			if err == nil || resp == nil {
				t.Fatalf("expected HTTP %d before upgrade, got response %v, error %v", tc.status, resp, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("got HTTP %d, want %d", resp.StatusCode, tc.status)
			}
		})
	}
}

func TestSandboxTunnelInvalidPort(t *testing.T) {
	_, ts := setup(t)
	sb := createSandbox(t, ts, "tunnel-ports")
	for _, query := range []string{"", "?port=", "?port=0", "?port=65536", "?port=not-a-port", "?port=80&port=81"} {
		t.Run(query, func(t *testing.T) {
			ws, resp, err := websocket.DefaultDialer.Dial(tunnelURL(ts.URL, sb.ID, query), tunnelHeader(testAPIKey))
			if ws != nil {
				ws.Close()
			}
			if err == nil || resp == nil {
				t.Fatalf("expected HTTP 400 before upgrade, got response %v, error %v", resp, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("got HTTP %d, want 400", resp.StatusCode)
			}
		})
	}
}

func TestSandboxTunnelRefusedPortClosesWithReason(t *testing.T) {
	srv, ts := setup(t)
	sb := createSandbox(t, ts, "tunnel-refused")
	eng := srv.engine.(*mockEngine)
	eng.TunnelRefusals = 1
	ws, resp, err := websocket.DefaultDialer.Dial(tunnelURL(ts.URL, sb.ID, "?port=4321"), tunnelHeader(testAPIKey))
	if err != nil {
		t.Fatalf("upgrade: %v (response: %v)", err, resp)
	}
	defer ws.Close()
	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err = ws.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseTryAgainLater || closeErr.Text != "nothing listening on guest port 4321" {
		t.Fatalf("guest refusal should send close reason, got %v", err)
	}
	waitTunnelDetached(t, srv, sb.EngineID)
}

func TestSandboxTunnelDestroyClosesLiveStream(t *testing.T) {
	srv, ts := setup(t)
	sb := createSandbox(t, ts, "tunnel-destroy")
	eng := srv.engine.(*mockEngine)
	eng.TunnelPeer = make(chan net.Conn, 1)
	ws, resp, err := websocket.DefaultDialer.Dial(tunnelURL(ts.URL, sb.ID, "?port=8080"), tunnelHeader(testAPIKey))
	if err != nil {
		t.Fatalf("upgrade: %v (response: %v)", err, resp)
	}
	defer ws.Close()
	var guest net.Conn
	select {
	case guest = <-eng.TunnelPeer:
	case <-time.After(2 * time.Second):
		t.Fatal("engine.Tunnel was not called")
	}
	defer guest.Close()
	if !srv.hasInteractiveAttach(sb.EngineID) {
		t.Fatal("live tunnel was not attached")
	}

	// mockEngine.Destroy intentionally leaves net.Pipe open. Only the server's
	// active-tunnel cleanup can close this connection promptly.
	response := doReq(t, ts, http.MethodDelete, "/sandboxes/"+sb.ID, nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("destroy returned HTTP %d", response.StatusCode)
	}
	guest.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := guest.Read(make([]byte, 1)); err == nil {
		t.Fatal("Destroy left guest tunnel open")
	}
	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := ws.ReadMessage(); err == nil {
		t.Fatal("Destroy left WebSocket tunnel open")
	}
	waitTunnelDetached(t, srv, sb.EngineID)
}

func TestRemovedSandboxForwardRoute(t *testing.T) {
	_, ts := setup(t)
	sb := createSandbox(t, ts, "tunnel-no-old-forward")
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		resp := doReq(t, ts, method, "/sandboxes/"+sb.ID+"/forward", nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("old %s /forward returned %d, want 404", method, resp.StatusCode)
		}
	}
}

func TestSandboxTunnelRequiresGET(t *testing.T) {
	_, ts := setup(t)
	sb := createSandbox(t, ts, "tunnel-get-only")
	resp := doReq(t, ts, http.MethodPost, "/sandboxes/"+sb.ID+"/tunnel?port=8080", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /tunnel returned %d, want 405", resp.StatusCode)
	}
}

func TestTunnelReadBucketAllowsBrowserBurst(t *testing.T) {
	limiter := newRateLimiter()
	req, err := http.NewRequest(http.MethodGet, "http://localhost/sandboxes/ws/tunnel?port=8080", nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 40 {
		if !limiter.Allow("usr_browser", req) {
			t.Fatalf("request %d in a 40-connection burst was rate limited", i+1)
		}
	}
}
