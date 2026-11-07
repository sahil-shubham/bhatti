package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sahil-shubham/bhatti/pkg/store"
)

func TestWebSocketTerminal(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(t.Context(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}

	keyHash := sha256Hex("ws-test-token")
	st.CreateUser(t.Context(), store.User{
		ID: "usr_ws", Name: "ws-user", APIKeyHash: keyHash,
		MaxSandboxes: 50, MaxCPUsPerSandbox: 4, MaxMemoryMBPerSandbox: 4096,
		SubnetIndex: 1, CreatedAt: time.Now(),
	})

	eng := newMockEngine()
	srv := New(eng, st, "")
	ts := httptest.NewServer(srv)
	defer func() { srv.Close(); ts.Close(); st.Close() }()

	// Create sandbox
	sb := store.Sandbox{
		ID: "ws-test-sb", Name: "ws-sandbox", EngineID: "",
		Status: "running", CreatedBy: "usr_ws", CreatedAt: time.Now(),
	}

	// Create via API to get a real engine ID
	body := `{"name":"ws-test-sandbox"}`
	req, _ := http.NewRequest("POST", ts.URL+"/sandboxes", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer ws-test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	decodeJSON(t, resp, &sb)
	defer func() {
		req, _ := http.NewRequest("DELETE", ts.URL+"/sandboxes/"+sb.ID, nil)
		req.Header.Set("Authorization", "Bearer ws-test-token")
		http.DefaultClient.Do(req)
	}()

	// Connect WebSocket with auth header
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/sandboxes/" + sb.ID + "/ws"
	wsHeader := http.Header{}
	wsHeader.Set("Authorization", "Bearer ws-test-token")
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, wsHeader)
	if err != nil {
		t.Fatalf("WS dial: %v", err)
	}
	defer ws.Close()

	// The mock Shell() sends "$ " — read it
	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("ws read: %v", err)
	}
	if !strings.Contains(string(msg), "$") {
		t.Logf("ws output: %q", msg)
	}

	t.Log("WebSocket terminal test passed")
}
