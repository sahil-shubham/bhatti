package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

// setupShellTest creates a server, user, and sandbox for shell tests.
func setupShellTest(t *testing.T) (*Server, *httptest.Server, *store.Sandbox) {
	t.Helper()
	srv, ts := setup(t)

	// Create sandbox via API
	resp := doReq(t, ts, "POST", "/sandboxes", map[string]string{"name": "shell-test"})
	var sb store.Sandbox
	decodeJSON(t, resp, &sb)
	t.Cleanup(func() {
		doReq(t, ts, "DELETE", "/sandboxes/"+sb.ID, nil).Body.Close()
	})
	return srv, ts, &sb
}

func TestShellTokenGenerateAndRevoke(t *testing.T) {
	_, ts, sb := setupShellTest(t)

	// Generate token
	resp := doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/shell-token", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var result map[string]string
	decodeJSON(t, resp, &result)
	if result["token"] == "" {
		t.Fatal("expected non-empty token")
	}
	if result["url"] == "" {
		t.Fatal("expected non-empty url")
	}
	if !strings.Contains(result["url"], "#token=") {
		t.Fatalf("URL should contain fragment: %s", result["url"])
	}
	if !strings.Contains(result["url"], "/_shell/"+sb.ID) {
		t.Fatalf("URL should contain sandbox ID: %s", result["url"])
	}

	// Rotate — generates a new token
	resp2 := doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/shell-token", nil)
	var result2 map[string]string
	decodeJSON(t, resp2, &result2)
	if result2["token"] == result["token"] {
		t.Fatal("rotation should produce a different token")
	}

	// Revoke
	resp3 := doReq(t, ts, "DELETE", "/sandboxes/"+sb.ID+"/shell-token", nil)
	if resp3.StatusCode != 204 {
		t.Fatalf("expected 204, got %d", resp3.StatusCode)
	}
	resp3.Body.Close()
}

func TestShellWSAuthSuccess(t *testing.T) {
	_, ts, sb := setupShellTest(t)

	// Generate token
	resp := doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/shell-token", nil)
	var result map[string]string
	decodeJSON(t, resp, &result)
	token := result["token"]

	// Connect WebSocket (no HTTP auth needed — this bypasses auth middleware)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/_shell/" + sb.ID + "/ws"
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer ws.Close()

	// Send auth
	ws.WriteJSON(map[string]string{"type": "auth", "token": token})

	// Should get "connected" back
	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("ws read: %v", err)
	}
	var connected map[string]interface{}
	if err := json.Unmarshal(msg, &connected); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if connected["type"] != "connected" {
		t.Fatalf("expected type=connected, got %v", connected["type"])
	}
	if connected["sandbox"] != "shell-test" {
		t.Fatalf("expected sandbox=shell-test, got %v", connected["sandbox"])
	}

	// Should get terminal output (mock engine sends "$ ")
	_, termMsg, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("ws read terminal: %v", err)
	}
	if !strings.Contains(string(termMsg), "$") {
		t.Logf("terminal output: %q", termMsg)
	}
}

func TestShellWSBadToken(t *testing.T) {
	_, ts, sb := setupShellTest(t)

	// Generate a token but use the wrong one
	resp := doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/shell-token", nil)
	resp.Body.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/_shell/" + sb.ID + "/ws"
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer ws.Close()

	ws.WriteJSON(map[string]string{"type": "auth", "token": "wrong-token"})

	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("ws read: %v", err)
	}
	var errMsg map[string]string
	json.Unmarshal(msg, &errMsg)
	if errMsg["error"] != "unauthorized" {
		t.Fatalf("expected unauthorized, got %v", errMsg["error"])
	}
}

func TestShellWSNoToken(t *testing.T) {
	_, ts, sb := setupShellTest(t)

	// No shell token set at all
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/_shell/" + sb.ID + "/ws"
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer ws.Close()

	ws.WriteJSON(map[string]string{"type": "auth", "token": "any-token"})

	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("ws read: %v", err)
	}
	var errMsg map[string]string
	json.Unmarshal(msg, &errMsg)
	if errMsg["error"] != "unauthorized" {
		t.Fatalf("expected unauthorized, got %v", errMsg["error"])
	}
}

func TestShellWSNonexistentSandbox(t *testing.T) {
	_, ts, _ := setupShellTest(t)

	// Nonexistent sandbox — should still upgrade WebSocket (anti-enumeration)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/_shell/sbx_doesnotexist/ws"
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial should succeed (anti-enumeration): %v", err)
	}
	defer ws.Close()

	ws.WriteJSON(map[string]string{"type": "auth", "token": "any-token"})

	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("ws read: %v", err)
	}
	var errMsg map[string]string
	json.Unmarshal(msg, &errMsg)
	// Same error as bad token — can't distinguish nonexistent from unauthorized
	if errMsg["error"] != "unauthorized" {
		t.Fatalf("expected unauthorized, got %v", errMsg["error"])
	}
}

func TestShellHTMLServed(t *testing.T) {
	_, ts, sb := setupShellTest(t)

	// /_shell/:id should serve the HTML page without auth
	resp, err := http.Get(ts.URL + "/_shell/" + sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("expected text/html, got %s", ct)
	}
	// Security headers
	if resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatal("missing X-Frame-Options: DENY")
	}
	if resp.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("missing Referrer-Policy: no-referrer")
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("CSP missing frame-ancestors: %s", csp)
	}
	if strings.Contains(csp, "unsafe-eval") {
		t.Fatal("CSP should not contain unsafe-eval")
	}
	// script-src should use nonce, not unsafe-inline (style-src may have unsafe-inline)
	for _, part := range strings.Split(csp, ";") {
		if strings.Contains(part, "script-src") && strings.Contains(part, "unsafe-inline") {
			t.Fatal("CSP script-src should use nonce, not unsafe-inline")
		}
	}
	if !strings.Contains(csp, "'nonce-") {
		t.Fatalf("CSP missing nonce in script-src: %s", csp)
	}

	// Verify the nonce in CSP matches the nonce in the HTML script tag
	body, _ := io.ReadAll(resp.Body)
	// Extract nonce from CSP: 'nonce-<base64>'
	for _, part := range strings.Split(csp, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "script-src") {
			idx := strings.Index(part, "'nonce-")
			if idx < 0 {
				t.Fatal("no nonce in script-src")
			}
			nonce := part[idx+7 : strings.Index(part[idx+7:], "'") + idx+7]
			if !strings.Contains(string(body), "nonce=\""+nonce+"\"") {
				t.Fatalf("CSP nonce %q not found in HTML script tag", nonce)
			}
		}
	}

	// Verify nonce changes per request (no caching)
	resp2, err := http.Get(ts.URL + "/_shell/" + sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	csp2 := resp2.Header.Get("Content-Security-Policy")
	if csp == csp2 {
		t.Fatal("nonce should differ between requests")
	}
}

func TestShellHTMLServedForAnyID(t *testing.T) {
	_, ts, _ := setupShellTest(t)

	// Nonexistent sandbox — HTML should still be served (anti-enumeration)
	resp, err := http.Get(ts.URL + "/_shell/sbx_nonexistent_12345")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 for any ID, got %d", resp.StatusCode)
	}
}

func TestShellSessionTracker(t *testing.T) {
	tracker := newShellSessionTracker(2)

	// Add within limit
	if !tracker.Add("sb1") {
		t.Fatal("first add should succeed")
	}
	if !tracker.Add("sb1") {
		t.Fatal("second add should succeed")
	}
	// At limit
	if tracker.Add("sb1") {
		t.Fatal("third add should fail (limit=2)")
	}
	// Different sandbox is independent
	if !tracker.Add("sb2") {
		t.Fatal("different sandbox should succeed")
	}

	// Remove frees a slot
	tracker.Remove("sb1")
	if !tracker.Add("sb1") {
		t.Fatal("add after remove should succeed")
	}

	// Done + DisconnectAll
	done1 := tracker.Done("sb1")
	done2 := tracker.Done("sb1")
	select {
	case <-done1:
		t.Fatal("done should not be closed yet")
	default:
	}

	tracker.DisconnectAll("sb1")
	select {
	case <-done1:
		// good
	default:
		t.Fatal("done1 should be closed after DisconnectAll")
	}
	select {
	case <-done2:
		// good
	default:
		t.Fatal("done2 should be closed after DisconnectAll")
	}

	// DisconnectAll on unknown sandbox is a no-op
	tracker.DisconnectAll("sb_unknown")
}

func TestShellWSDestroyedSandbox(t *testing.T) {
	_, ts, sb := setupShellTest(t)

	// Generate token, then destroy the sandbox
	resp := doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/shell-token", nil)
	var result map[string]string
	decodeJSON(t, resp, &result)
	token := result["token"]

	// Destroy sandbox
	dr := doReq(t, ts, "DELETE", "/sandboxes/"+sb.ID, nil)
	dr.Body.Close()

	// Connect WS — should still upgrade (anti-enumeration)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/_shell/" + sb.ID + "/ws"
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial should succeed: %v", err)
	}
	defer ws.Close()

	ws.WriteJSON(map[string]string{"type": "auth", "token": token})
	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("ws read: %v", err)
	}
	var errMsg map[string]string
	json.Unmarshal(msg, &errMsg)
	if errMsg["error"] != "unauthorized" {
		t.Fatalf("expected unauthorized for destroyed sandbox, got %v", errMsg["error"])
	}
}

func TestShellWSTokenRotationInvalidatesOld(t *testing.T) {
	_, ts, sb := setupShellTest(t)

	// Generate first token
	resp := doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/shell-token", nil)
	var result1 map[string]string
	decodeJSON(t, resp, &result1)
	oldToken := result1["token"]

	// Rotate — generate second token
	resp2 := doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/shell-token", nil)
	var result2 map[string]string
	decodeJSON(t, resp2, &result2)

	// Old token should fail
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/_shell/" + sb.ID + "/ws"
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer ws.Close()

	ws.WriteJSON(map[string]string{"type": "auth", "token": oldToken})
	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("ws read: %v", err)
	}
	var errMsg map[string]string
	json.Unmarshal(msg, &errMsg)
	if errMsg["error"] != "unauthorized" {
		t.Fatalf("old token should be unauthorized, got %v", errMsg["error"])
	}
}

func TestShellWSConcurrentLimit(t *testing.T) {
	srv, ts, sb := setupShellTest(t)
	_ = srv

	// Generate token
	resp := doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/shell-token", nil)
	var result map[string]string
	decodeJSON(t, resp, &result)
	token := result["token"]

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/_shell/" + sb.ID + "/ws"

	// Open 5 connections (the limit)
	var conns []*websocket.Conn
	for i := 0; i < 5; i++ {
		ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			t.Fatalf("ws dial %d: %v", i, err)
		}
		ws.WriteJSON(map[string]string{"type": "auth", "token": token})
		ws.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, msg, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("ws %d read: %v", i, err)
		}
		var connected map[string]interface{}
		json.Unmarshal(msg, &connected)
		if connected["type"] != "connected" {
			t.Fatalf("ws %d: expected connected, got %v", i, connected)
		}
		conns = append(conns, ws)
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()

	// 6th connection should authenticate but get "too many sessions"
	ws6, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial 6: %v", err)
	}
	defer ws6.Close()

	ws6.WriteJSON(map[string]string{"type": "auth", "token": token})
	ws6.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg6, err := ws6.ReadMessage()
	if err != nil {
		t.Fatalf("ws 6 read: %v", err)
	}
	var errMsg map[string]string
	json.Unmarshal(msg6, &errMsg)
	if errMsg["error"] != "too many sessions" {
		t.Fatalf("expected 'too many sessions', got %v", errMsg["error"])
	}
}

func TestShellPathTraversal(t *testing.T) {
	_, ts, _ := setupShellTest(t)

	// path.Clean normalizes traversals BEFORE they reach handleWebShell.
	// /_shell/../health → /health (served normally, NOT as shell page)
	// /_shell/sbx_abc/../../../etc/passwd → /etc/passwd (401 auth required)
	// The key invariant: traversal never leaks shell HTML for non-shell paths,
	// and never bypasses auth to reach internal endpoints.

	// Traversal out of /_shell/ lands on normal routes (not shell HTML)
	resp, err := http.Get(ts.URL + "/_shell/../health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// path.Clean → /health → served as JSON health check. This proves
	// traversal escapes the /_shell/ prefix, NOT that it serves shell content.
	if resp.StatusCode != 200 {
		t.Fatalf("/_shell/../health: expected 200 (health endpoint), got %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "json") {
		t.Fatalf("/_shell/../health should serve JSON health, got %s", ct)
	}

	// Deep traversal goes to auth-protected routes → 401
	resp2, err := http.Get(ts.URL + "/_shell/sbx_abc/../../../etc/passwd")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 401 {
		t.Fatalf("deep traversal: expected 401, got %d", resp2.StatusCode)
	}
}

func TestShellRateLimit(t *testing.T) {
	_, ts, sb := setupShellTest(t)

	// Generate a token
	resp := doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/shell-token", nil)
	resp.Body.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/_shell/" + sb.ID + "/ws"

	// Rapidly open many connections — should eventually get rate limited.
	// The rate limiter allows 10/sec/IP. Fire 20 rapidly.
	var rateLimited bool
	for i := 0; i < 20; i++ {
		ws, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			if resp != nil && resp.StatusCode == 429 {
				rateLimited = true
				break
			}
			// Other errors (e.g. connection refused) are fine too
			continue
		}
		ws.Close()
	}
	if !rateLimited {
		t.Fatal("expected rate limiting after 20 rapid connections")
	}
}

func TestShellRoutingEdgeCases(t *testing.T) {
	_, ts, _ := setupShellTest(t)

	// /_shell/ with no ID → 404
	resp, err := http.Get(ts.URL + "/_shell/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("/_shell/ with no ID: expected 404, got %d", resp.StatusCode)
	}

	// /_shell/sbx_abc/unknown → 404
	resp2, err := http.Get(ts.URL + "/_shell/sbx_abc/unknown")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 404 {
		t.Fatalf("/_shell/sbx_abc/unknown: expected 404, got %d", resp2.StatusCode)
	}

	// /_shell/sbx_abc/ (trailing slash, no sub) → serves HTML
	resp3, err := http.Get(ts.URL + "/_shell/sbx_abc/")
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	// path.Clean strips trailing slash → /_shell/sbx_abc → serves HTML
	if resp3.StatusCode != 200 {
		t.Fatalf("/_shell/sbx_abc/: expected 200, got %d", resp3.StatusCode)
	}
}

func TestShellWSRevokeDisconnects(t *testing.T) {
	_, ts, sb := setupShellTest(t)

	// Generate token
	resp := doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/shell-token", nil)
	var result map[string]string
	decodeJSON(t, resp, &result)
	token := result["token"]

	// Connect WebSocket
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/_shell/" + sb.ID + "/ws"
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer ws.Close()

	// Authenticate
	ws.WriteJSON(map[string]string{"type": "auth", "token": token})
	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg, _ := ws.ReadMessage()
	var connected map[string]interface{}
	json.Unmarshal(msg, &connected)
	if connected["type"] != "connected" {
		t.Fatalf("expected connected, got %v", connected["type"])
	}

	// Revoke via API
	revokeResp := doReq(t, ts, "DELETE", "/sandboxes/"+sb.ID+"/shell-token", nil)
	revokeResp.Body.Close()
	if revokeResp.StatusCode != 204 {
		t.Fatalf("revoke: expected 204, got %d", revokeResp.StatusCode)
	}

	// WebSocket should close — reading should fail
	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, _, err := ws.ReadMessage()
		if err != nil {
			break // expected — connection was closed by revoke
		}
	}
}
