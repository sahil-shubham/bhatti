package main

import (
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

// TestControlSocketTransport verifies the CLI talks to the daemon over a unix
// control socket (the #2 hole-closer): apiRequest routes through the socket, not
// TCP, when unixSocketPath is set.
func TestControlSocketTransport(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "api.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("pong"))
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()

	// Point the CLI transport at the socket (restore after).
	prevSock, prevURL := unixSocketPath, apiURL
	unixSocketPath, apiURL = sock, "http://unix"
	defer func() { unixSocketPath, apiURL = prevSock, prevURL }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err := apiRequest("GET", "/ping", nil)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(body) != "pong" {
				t.Fatalf("body = %q, want pong (wrong endpoint reached)", body)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("apiRequest over unix socket failed: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
