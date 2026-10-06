package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sahil-shubham/bhatti/pkg/forward"
)

func TestParsePortSpec(t *testing.T) {
	for _, tt := range []struct {
		spec  string
		local int
		guest int
		valid bool
	}{
		{"5174", 5174, 5174, true},
		{"3000:8080", 3000, 8080, true},
		{"0:5174", 0, 5174, true},
		{"65535:1", 65535, 1, true},
		{"", 0, 0, false},
		{"0", 0, 0, false},
		{"0:0", 0, 0, false},
		{"65536", 0, 0, false},
		{"123:65536", 0, 0, false},
		{"-1:80", 0, 0, false},
		{"3:abc", 0, 0, false},
		{"3:4:5", 0, 0, false},
		{":80", 0, 0, false},
		{"80:", 0, 0, false},
	} {
		t.Run(tt.spec, func(t *testing.T) {
			got, err := parsePortSpec(tt.spec)
			if (err == nil) != tt.valid {
				t.Fatalf("parsePortSpec(%q) = %+v, %v; valid = %v", tt.spec, got, err, tt.valid)
			}
			if tt.valid && (got.local != tt.local || got.guest != tt.guest) {
				t.Fatalf("parsePortSpec(%q) = %+v; want local=%d guest=%d", tt.spec, got, tt.local, tt.guest)
			}
		})
	}
}

type forwardEchoEngine struct{ connects atomic.Int32 }

func (e *forwardEchoEngine) Tunnel(_ context.Context, id string, port int) (io.ReadWriteCloser, error) {
	if id != "spc-dev" || (port != 5174 && port != 3000) {
		return nil, fmt.Errorf("unexpected guest %s:%d", id, port)
	}
	host, guest := net.Pipe()
	e.connects.Add(1)
	go func() {
		defer guest.Close()
		io.Copy(guest, guest)
	}()
	return host, nil
}

func forwardEchoHandler(engine *forwardEchoEngine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/sandboxes/spc-dev/tunnel" || r.Method != http.MethodGet {
			http.Error(w, `{"error":"wrong tunnel path"}`, http.StatusNotFound)
			return
		}
		port, err := strconv.Atoi(r.URL.Query().Get("port"))
		if err != nil {
			http.Error(w, `{"error":"bad port"}`, http.StatusBadRequest)
			return
		}
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		tunnel, err := engine.Tunnel(r.Context(), "spc-dev", port)
		if err != nil {
			return
		}
		forward.Relay(forward.NewWSConn(ws), tunnel)
	}
}

func setForwardEndpoint(t *testing.T, endpoint, token string) {
	t.Helper()
	prevURL, prevToken, prevSocket := apiURL, apiToken, unixSocketPath
	apiURL, apiToken, unixSocketPath = endpoint, token, ""
	t.Cleanup(func() { apiURL, apiToken, unixSocketPath = prevURL, prevToken, prevSocket })
}

func startForwardTest(t *testing.T, specs []portSpec, stderr io.Writer) ([]forwardMapping, context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	reader, writer := io.Pipe()
	t.Cleanup(func() {
		cancel()
		reader.Close()
	})
	done := make(chan error, 1)
	go func() {
		defer writer.Close()
		done <- runForward(ctx, "spc-dev", specs, "127.0.0.1", true, writer, stderr)
	}()
	var mappings []forwardMapping
	if err := json.NewDecoder(reader).Decode(&mappings); err != nil {
		select {
		case runErr := <-done:
			t.Fatalf("forward did not bind: %v (output: %v)", runErr, err)
		default:
			t.Fatal(err)
		}
	}
	return mappings, cancel, done
}

func assertForwardRoundTrip(t *testing.T, addr string, payload string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, payload); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil || string(got) != payload {
		conn.Close()
		t.Fatalf("round trip to %s: got %q, %v; want %q", addr, got, err, payload)
	}
	return conn
}

func TestForwardLoopRemoteMultiPortJSONAndShutdown(t *testing.T) {
	engine := &forwardEchoEngine{}
	srv := httptest.NewServer(forwardEchoHandler(engine))
	defer srv.Close()
	setForwardEndpoint(t, srv.URL, "test-token")
	mappings, cancel, done := startForwardTest(t, []portSpec{{local: 0, guest: 5174}, {local: 0, guest: 3000}}, io.Discard)
	if len(mappings) != 2 {
		t.Fatalf("got %d bound ports, want two", len(mappings))
	}
	for i, guest := range []int{5174, 3000} {
		if mappings[i].Sandbox != "spc-dev" || mappings[i].LocalPort == 0 || mappings[i].GuestPort != guest ||
			!strings.HasPrefix(mappings[i].LocalAddr, "127.0.0.1:") {
			t.Fatalf("mapping %d = %+v", i, mappings[i])
		}
	}
	select {
	case err := <-done:
		t.Fatalf("--json exited after printing addresses: %v", err)
	default:
	}
	vite := assertForwardRoundTrip(t, mappings[0].LocalAddr, "GET /vite HTTP/1.1\r\n\r\n")
	defer vite.Close()
	api := assertForwardRoundTrip(t, mappings[1].LocalAddr, "guest api")
	api.Close()
	if engine.connects.Load() != 2 {
		t.Fatalf("opened %d guest tunnels, want two", engine.connects.Load())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("forward did not stop while a TCP connection was still open")
	}
	_, err := vite.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("local connection remained open after shutdown")
	}
	for _, mapping := range mappings {
		conn, err := net.DialTimeout("tcp", mapping.LocalAddr, time.Second)
		if err == nil {
			conn.Close()
			t.Errorf("listener still accepting on %s after cancellation", mapping.LocalAddr)
		}
	}
}

func TestForwardLoopUsesUnixControlSocket(t *testing.T) {
	engine := &forwardEchoEngine{}
	serveCLI(t, forwardEchoHandler(engine))
	apiToken = "test-token"
	mappings, cancel, done := startForwardTest(t, []portSpec{{local: 0, guest: 5174}}, io.Discard)
	if len(mappings) != 1 {
		t.Fatalf("got mappings %+v", mappings)
	}
	conn := assertForwardRoundTrip(t, mappings[0].LocalAddr, "over unix")
	conn.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("unix-socket forward did not stop")
	}
}

type forwardErrorLines chan string

func (lines forwardErrorLines) Write(p []byte) (int, error) {
	lines <- string(p)
	return len(p), nil
}

func TestForwardReportsGuestRefusalWithoutExiting(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		ws.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "nothing listening on guest port 5174"),
			time.Now().Add(time.Second))
	}))
	defer srv.Close()
	setForwardEndpoint(t, srv.URL, "test-token")
	logs := make(forwardErrorLines, 4)
	mappings, cancel, done := startForwardTest(t, []portSpec{{local: 0, guest: 5174}}, logs)
	conn, err := net.DialTimeout("tcp", mappings[0].LocalAddr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	io.Copy(io.Discard, conn)
	conn.Close()
	select {
	case message := <-logs:
		if !strings.Contains(message, "nothing listening on guest port 5174") {
			t.Fatalf("forward error = %q, want guest refusal", message)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("guest refusal was not printed")
	}
	select {
	case err := <-done:
		t.Fatalf("forward stopped after one refused connection: %v", err)
	default:
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("forward did not stop")
	}
}

func TestForwardConnectionErrorsAreRateLimited(t *testing.T) {
	var output strings.Builder
	session := &forwardSession{stderr: &output}
	session.report(errors.New("guest refused"))
	session.report(errors.New("guest refused"))
	if got := output.String(); strings.Count(got, "guest refused") != 1 {
		t.Fatalf("connection errors not rate limited: %q", got)
	}
	session.lastError = time.Now().Add(-6 * time.Second)
	session.report(errors.New("network unavailable"))
	if got := output.String(); !strings.Contains(got, "1 connection errors suppressed") ||
		!strings.Contains(got, "network unavailable") {
		t.Fatalf("missing suppressed count or later error: %q", got)
	}
}

func TestForwardFailsFastAndReleasesAllBoundPorts(t *testing.T) {
	first, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	firstPort := first.Addr().(*net.TCPAddr).Port
	first.Close()
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	busyPort := busy.Addr().(*net.TCPAddr).Port
	setForwardEndpoint(t, "http://unix", "test-token")
	var output strings.Builder
	err = runForward(t.Context(), "spc-dev", []portSpec{
		{local: firstPort, guest: 5174}, {local: busyPort, guest: 3000},
	}, "127.0.0.1", false, &output, io.Discard)
	if err == nil || !strings.Contains(err.Error(), strconv.Itoa(busyPort)) || !strings.Contains(err.Error(), "cannot bind") {
		t.Fatalf("bind error = %v, want clear busy-port error", err)
	}
	if output.Len() != 0 {
		t.Fatalf("forward printed successful mapping before all binds succeeded: %q", output.String())
	}
	rebound, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(firstPort)))
	if err != nil {
		t.Fatalf("first listener not released after second failed: %v", err)
	}
	rebound.Close()
}

func TestTunnelEndpointUsesWSSForRemoteHTTPS(t *testing.T) {
	setForwardEndpoint(t, "https://agni-02.karkhana.dev/", "test-token")
	endpoint, err := tunnelEndpoint("spc-dev", 5174)
	if err != nil || endpoint != "wss://agni-02.karkhana.dev/sandboxes/spc-dev/tunnel?port=5174" {
		t.Fatalf("tunnel endpoint = %q, %v", endpoint, err)
	}
}
