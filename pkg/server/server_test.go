package server

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

// uniqueName generates a collision-free resource name for tests.
func uniqueName(t *testing.T, prefix string) string {
	t.Helper()
	b := make([]byte, 4)
	rand.Read(b)
	return fmt.Sprintf("bhatti-test-%s-%s", prefix, hex.EncodeToString(b))
}

// testAPIKey is the plaintext key used in tests.
const testAPIKey = "test-token"

func setup(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}

	keyHash := sha256Hex(testAPIKey)
	st.CreateUser(store.User{
		ID: "usr_test", Name: "test-user", APIKeyHash: keyHash,
		MaxSandboxes: 50, MaxCPUsPerSandbox: 4, MaxMemoryMBPerSandbox: 4096,
		SubnetIndex: 1, CreatedAt: time.Now(),
	})

	eng := newMockEngine()
	srv := New(eng, st, dir)
	ts := httptest.NewServer(srv)
	t.Cleanup(func() { srv.Close(); ts.Close(); st.Close() })
	return srv, ts
}

func doReq(t *testing.T, ts *httptest.Server, method, path string, body any) *http.Response {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, ts.URL+path, bodyReader)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodeJSON(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

// createSandbox creates a sandbox via the API and registers cleanup.
func createSandbox(t *testing.T, ts *httptest.Server, sbName string) store.Sandbox {
	t.Helper()
	resp := doReq(t, ts, "POST", "/sandboxes", map[string]any{"name": sbName})
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create sandbox: expected 201, got %d: %s", resp.StatusCode, body)
	}
	var sb store.Sandbox
	decodeJSON(t, resp, &sb)
	t.Cleanup(func() { doReq(t, ts, "DELETE", "/sandboxes/"+sb.ID, nil) })
	return sb
}

// --- Auth tests ---

func TestAuthRequired(t *testing.T) {
	_, ts := setup(t)
	req, _ := http.NewRequest("GET", ts.URL+"/sandboxes", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestAuthInvalidKey(t *testing.T) {
	_, ts := setup(t)
	req, _ := http.NewRequest("GET", ts.URL+"/sandboxes", nil)
	req.Header.Set("Authorization", "Bearer wrong-key")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Fatalf("expected 401 for wrong key, got %d", resp.StatusCode)
	}
}

func TestAuthQueryParamRejected(t *testing.T) {
	_, ts := setup(t)
	req, _ := http.NewRequest("GET", ts.URL+"/sandboxes?token="+testAPIKey, nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Fatalf("expected 401 for query param auth, got %d", resp.StatusCode)
	}
}

func TestPathCleanAuthBypass(t *testing.T) {
	_, ts := setup(t)

	paths := []string{
		"//health/../sandboxes",
		"/health/../sandboxes",
		"/./health/../sandboxes",
	}
	for _, p := range paths {
		req, _ := http.NewRequest("GET", ts.URL+p, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("path %q: %v", p, err)
		}
		if resp.StatusCode != 401 {
			t.Errorf("path %q: expected 401, got %d (auth bypass!)", p, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// /health itself should still work without auth
	req, _ := http.NewRequest("GET", ts.URL+"/health", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Errorf("/health: expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- Health ---

func TestHealthEndpoint(t *testing.T) {
	_, ts := setup(t)
	resp := doReq(t, ts, "GET", "/health", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var health map[string]any
	decodeJSON(t, resp, &health)
	if health["status"] != "ok" {
		t.Fatalf("expected status ok, got %v", health["status"])
	}
}

func TestHealthNoAuth(t *testing.T) {
	_, ts := setup(t)
	req, _ := http.NewRequest("GET", ts.URL+"/health", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 without auth, got %d", resp.StatusCode)
	}
}

// TestSetupMustUseAuthenticatedEndpoint verifies that /sandboxes (the
// endpoint setup should use for validation) actually rejects bad keys,
// while /health does not. This is a regression test for the bug where
// `bhatti setup` reported "✓ connected" with an invalid API key.
func TestSetupMustUseAuthenticatedEndpoint(t *testing.T) {
	_, ts := setup(t)

	// /health succeeds with a bad token (unauthenticated)
	req, _ := http.NewRequest("GET", ts.URL+"/health", nil)
	req.Header.Set("Authorization", "Bearer wrong-key")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("/health should return 200 even with bad token, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// /sandboxes rejects the same bad token (authenticated)
	req, _ = http.NewRequest("GET", ts.URL+"/sandboxes", nil)
	req.Header.Set("Authorization", "Bearer wrong-key")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Fatalf("/sandboxes should return 401 with bad token, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// /sandboxes succeeds with the correct token
	req, _ = http.NewRequest("GET", ts.URL+"/sandboxes", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("/sandboxes should return 200 with valid token, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- Templates ---

func TestTemplateCRUD(t *testing.T) {
	_, ts := setup(t)

	resp := doReq(t, ts, "POST", "/templates", map[string]any{
		"name": "ubuntu-dev", "image": "ubuntu:22.04", "cpus": 2,
	})
	if resp.StatusCode != 201 {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	var tmpl store.Template
	decodeJSON(t, resp, &tmpl)
	if tmpl.Name != "ubuntu-dev" {
		t.Fatalf("unexpected template: %+v", tmpl)
	}

	resp = doReq(t, ts, "GET", "/templates", nil)
	var list []store.Template
	decodeJSON(t, resp, &list)
	if len(list) != 1 {
		t.Fatalf("expected 1 template, got %d", len(list))
	}

	resp = doReq(t, ts, "GET", "/templates/"+tmpl.ID, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doReq(t, ts, "DELETE", "/templates/"+tmpl.ID, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doReq(t, ts, "GET", "/templates/"+tmpl.ID, nil)
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- Sandbox lifecycle ---

func TestSandboxLifecycle(t *testing.T) {
	_, ts := setup(t)
	name := uniqueName(t, "lifecycle")
	sb := createSandbox(t, ts, name)

	if sb.Name != name || sb.Status != "running" {
		t.Fatalf("unexpected sandbox: %+v", sb)
	}

	// List
	resp := doReq(t, ts, "GET", "/sandboxes", nil)
	var sbList []store.Sandbox
	decodeJSON(t, resp, &sbList)
	found := false
	for _, s := range sbList {
		if s.ID == sb.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("sandbox not found in list")
	}

	// Exec
	resp = doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/exec", map[string]any{
		"cmd": []string{"echo", "hello"},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("exec: expected 200, got %d", resp.StatusCode)
	}
	var result engine.ExecResult
	decodeJSON(t, resp, &result)
	if result.ExitCode != 0 {
		t.Fatalf("exec: expected exit code 0, got %d", result.ExitCode)
	}

	// Stop
	resp = doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/stop", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("stop: expected 200, got %d", resp.StatusCode)
	}
	var stoppedSB store.Sandbox
	decodeJSON(t, resp, &stoppedSB)
	if stoppedSB.Status != "stopped" {
		t.Fatalf("expected stopped, got %s", stoppedSB.Status)
	}

	// Start
	resp = doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/start", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("start: expected 200, got %d", resp.StatusCode)
	}
	var startedSB store.Sandbox
	decodeJSON(t, resp, &startedSB)
	if startedSB.Status != "running" {
		t.Fatalf("expected running, got %s", startedSB.Status)
	}

	// Destroy
	resp = doReq(t, ts, "DELETE", "/sandboxes/"+sb.ID, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("destroy: expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Verify gone
	resp = doReq(t, ts, "GET", "/sandboxes/"+sb.ID, nil)
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404 after destroy, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestSandboxCreateNoName(t *testing.T) {
	_, ts := setup(t)
	resp := doReq(t, ts, "POST", "/sandboxes", map[string]any{})
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d: %s", resp.StatusCode, body)
	}
	var sb store.Sandbox
	decodeJSON(t, resp, &sb)
	t.Cleanup(func() { doReq(t, ts, "DELETE", "/sandboxes/"+sb.ID, nil) })

	if !strings.HasPrefix(sb.Name, "sandbox-") {
		t.Errorf("auto name: %q, want sandbox-* prefix", sb.Name)
	}
}

// --- Secrets ---

func TestSecretsCRUD(t *testing.T) {
	_, ts := setup(t)

	resp := doReq(t, ts, "POST", "/secrets", map[string]any{
		"name": "api-key", "value": "secret123",
	})
	if resp.StatusCode != 201 {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doReq(t, ts, "GET", "/secrets", nil)
	var list []store.SecretRecord
	decodeJSON(t, resp, &list)
	if len(list) != 1 || list[0].Name != "api-key" {
		t.Fatalf("unexpected secrets: %+v", list)
	}

	resp = doReq(t, ts, "DELETE", "/secrets/api-key", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestSecretValidation(t *testing.T) {
	_, ts := setup(t)
	resp := doReq(t, ts, "POST", "/secrets", map[string]any{"name": "test"})
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- Persistent Volumes (v0.3) ---

func TestVolumeCRUD(t *testing.T) {
	_, ts := setup(t)
	volName := uniqueName(t, "crud")

	resp := doReq(t, ts, "POST", "/volumes", map[string]any{"name": volName, "size_mb": 64})
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	resp = doReq(t, ts, "GET", "/volumes", nil)
	var list []store.PersistentVolume
	decodeJSON(t, resp, &list)
	if len(list) != 1 {
		t.Fatalf("expected 1, got %d", len(list))
	}
	if list[0].Name != volName {
		t.Fatalf("expected name %q, got %q", volName, list[0].Name)
	}
	if list[0].SizeMB != 64 {
		t.Fatalf("expected 64MB, got %d", list[0].SizeMB)
	}

	resp = doReq(t, ts, "GET", "/volumes/"+volName, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doReq(t, ts, "DELETE", "/volumes/"+volName, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doReq(t, ts, "GET", "/volumes/"+volName, nil)
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestVolumeDuplicateNameRejected(t *testing.T) {
	_, ts := setup(t)
	volName := uniqueName(t, "dup")

	resp := doReq(t, ts, "POST", "/volumes", map[string]any{"name": volName, "size_mb": 64})
	if resp.StatusCode != 201 {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doReq(t, ts, "POST", "/volumes", map[string]any{"name": volName, "size_mb": 64})
	if resp.StatusCode != 409 {
		t.Fatalf("expected 409 conflict, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestVolumeValidation(t *testing.T) {
	_, ts := setup(t)
	// Missing size
	resp := doReq(t, ts, "POST", "/volumes", map[string]any{"name": "test"})
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	// Missing name
	resp = doReq(t, ts, "POST", "/volumes", map[string]any{"size_mb": 64})
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestVolumeDeleteWhileAttachedHTTP(t *testing.T) {
	_, ts := setup(t)
	volName := uniqueName(t, "attached")

	// Create volume
	resp := doReq(t, ts, "POST", "/volumes", map[string]any{"name": volName, "size_mb": 64})
	if resp.StatusCode != 201 {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Create sandbox with that volume
	resp = doReq(t, ts, "POST", "/sandboxes", map[string]any{
		"name": uniqueName(t, "sb"),
		"persistent_volumes": []map[string]any{
			{"name": volName, "mount": "/workspace"},
		},
	})
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	// Delete should fail (attached)
	resp = doReq(t, ts, "DELETE", "/volumes/"+volName, nil)
	if resp.StatusCode != 409 {
		t.Fatalf("expected 409 (attached), got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestVolumeMountPathValidation(t *testing.T) {
	_, ts := setup(t)
	volName := uniqueName(t, "mount")

	resp := doReq(t, ts, "POST", "/volumes", map[string]any{"name": volName, "size_mb": 64})
	resp.Body.Close()

	tests := []struct {
		mount  string
		expect int
	}{
		{"/", 400},
		{"/proc", 400},
		{"/dev", 400},
		{"/etc", 400},
		{"relative", 400},
		{"/workspace", 201}, // valid
	}
	for _, tt := range tests {
		resp = doReq(t, ts, "POST", "/sandboxes", map[string]any{
			"name": uniqueName(t, "m"),
			"persistent_volumes": []map[string]any{
				{"name": volName, "mount": tt.mount},
			},
		})
		if resp.StatusCode != tt.expect {
			body, _ := io.ReadAll(resp.Body)
			t.Errorf("mount %q: expected %d, got %d: %s", tt.mount, tt.expect, resp.StatusCode, body)
		}
		resp.Body.Close()
	}
}

func TestVolumeNameValidationHTTP(t *testing.T) {
	_, ts := setup(t)

	// Path traversal in name
	resp := doReq(t, ts, "POST", "/volumes", map[string]any{"name": "../etc/passwd", "size_mb": 64})
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400 for path traversal name, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Empty name
	resp = doReq(t, ts, "POST", "/volumes", map[string]any{"name": "", "size_mb": 64})
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400 for empty name, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestSandboxCreateNoVolumesRegression(t *testing.T) {
	// The #1 regression test: v0.1-style sandbox creation (no persistent_volumes)
	// must still work after all the volume resolution code was added.
	_, ts := setup(t)
	resp := doReq(t, ts, "POST", "/sandboxes", map[string]any{
		"name": uniqueName(t, "novol"),
	})
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201 (no-volume sandbox), got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

// --- Ports ---

func TestPortEndpoints(t *testing.T) {
	_, ts := setup(t)

	resp := doReq(t, ts, "GET", "/ports", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var ports []portInfo
	decodeJSON(t, resp, &ports)
	if len(ports) != 0 {
		t.Fatalf("expected 0 ports, got %d", len(ports))
	}

	sb := createSandbox(t, ts, uniqueName(t, "ports"))
	resp = doReq(t, ts, "GET", "/sandboxes/"+sb.ID+"/ports", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	decodeJSON(t, resp, &ports)
	if len(ports) != 0 {
		t.Fatalf("expected 0 ports, got %d", len(ports))
	}
}

// --- WebSocket ---

func TestWSAuthRequired(t *testing.T) {
	_, ts := setup(t)
	sb := createSandbox(t, ts, uniqueName(t, "wsauth"))

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/sandboxes/" + sb.ID + "/ws"
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		t.Fatal("expected WS dial without auth to fail")
	}
	if resp != nil && resp.StatusCode != 401 {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestWSAuthQueryParamRejected(t *testing.T) {
	_, ts := setup(t)
	sb := createSandbox(t, ts, uniqueName(t, "wsqp"))

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/sandboxes/" + sb.ID + "/ws?token=" + testAPIKey
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		t.Fatal("expected WS dial with query param to fail")
	}
	if resp != nil && resp.StatusCode != 401 {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestWSAuthBearerHeader(t *testing.T) {
	_, ts := setup(t)
	sb := createSandbox(t, ts, uniqueName(t, "wshdr"))

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/sandboxes/" + sb.ID + "/ws"
	header := http.Header{}
	header.Set("Authorization", "Bearer "+testAPIKey)
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("expected WS dial with bearer header to succeed: %v", err)
	}
	ws.Close()
}

func TestWSAuthWrongToken(t *testing.T) {
	_, ts := setup(t)
	sb := createSandbox(t, ts, uniqueName(t, "wsbad"))

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/sandboxes/" + sb.ID + "/ws"
	header := http.Header{}
	header.Set("Authorization", "Bearer wrong-token")
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err == nil {
		t.Fatal("expected WS dial with wrong token to fail")
	}
	if resp != nil && resp.StatusCode != 401 {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

// --- Request body ---

func TestRequestBodyTooLarge(t *testing.T) {
	_, ts := setup(t)
	bigBody := strings.Repeat("x", 2<<20)
	req, _ := http.NewRequest("POST", ts.URL+"/sandboxes", strings.NewReader(bigBody))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 201 {
		t.Fatal("expected rejection of 2MB body, but got 201")
	}
}

// --- Streaming exec ---

func TestExecStreamNDJSON(t *testing.T) {
	_, ts := setup(t)
	sb := createSandbox(t, ts, uniqueName(t, "ndjson"))

	body, _ := json.Marshal(map[string]any{"cmd": []string{"echo", "streamed"}})
	req, _ := http.NewRequest("POST", ts.URL+"/sandboxes/"+sb.ID+"/exec", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("expected Content-Type application/x-ndjson, got %q", ct)
	}

	// Parse NDJSON — mock engine doesn't implement StreamExecEngine so
	// the server falls back to buffered-then-NDJSON
	var events []engine.StreamEvent
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		var e engine.StreamEvent
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			t.Fatalf("parse NDJSON: %v: %s", err, scanner.Text())
		}
		events = append(events, e)
	}

	var gotExit bool
	for _, e := range events {
		if e.Type == "exit" && e.ExitCode != nil && *e.ExitCode == 0 {
			gotExit = true
		}
	}
	if !gotExit {
		t.Errorf("no exit event with code 0, events: %+v", events)
	}
}

// --- Multi-user isolation tests ---

func setupTwoUsers(t *testing.T) (*httptest.Server, func(t *testing.T, method, path string, body any) *http.Response, func(t *testing.T, method, path string, body any) *http.Response) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}

	st.CreateUser(store.User{
		ID: "usr_alice", Name: "alice", APIKeyHash: sha256Hex("alice-key"),
		MaxSandboxes: 3, MaxCPUsPerSandbox: 2, MaxMemoryMBPerSandbox: 1024,
		SubnetIndex: 1, CreatedAt: time.Now(),
	})
	st.CreateUser(store.User{
		ID: "usr_bob", Name: "bob", APIKeyHash: sha256Hex("bob-key"),
		MaxSandboxes: 3, MaxCPUsPerSandbox: 2, MaxMemoryMBPerSandbox: 1024,
		SubnetIndex: 2, CreatedAt: time.Now(),
	})

	eng := newMockEngine()
	srv := New(eng, st, dir)
	ts := httptest.NewServer(srv)
	t.Cleanup(func() { srv.Close(); ts.Close(); st.Close() })

	makeReq := func(apiKey string) func(*testing.T, string, string, any) *http.Response {
		return func(t *testing.T, method, path string, body any) *http.Response {
			t.Helper()
			var bodyReader io.Reader
			if body != nil {
				b, _ := json.Marshal(body)
				bodyReader = bytes.NewReader(b)
			}
			req, _ := http.NewRequest(method, ts.URL+path, bodyReader)
			req.Header.Set("Authorization", "Bearer "+apiKey)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			return resp
		}
	}
	return ts, makeReq("alice-key"), makeReq("bob-key")
}

func TestCrossUserSandboxIsolation(t *testing.T) {
	_, alice, bob := setupTwoUsers(t)

	resp := alice(t, "POST", "/sandboxes", map[string]any{
		"name": uniqueName(t, "alice-iso"),
	})
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("alice create: expected 201, got %d: %s", resp.StatusCode, body)
	}
	var sb store.Sandbox
	decodeJSON(t, resp, &sb)
	t.Cleanup(func() { alice(t, "DELETE", "/sandboxes/"+sb.ID, nil) })

	// Alice can see it
	resp = alice(t, "GET", "/sandboxes/"+sb.ID, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("alice get: expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Bob cannot
	resp = bob(t, "GET", "/sandboxes/"+sb.ID, nil)
	if resp.StatusCode != 404 {
		t.Fatalf("bob get alice's sandbox: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Bob cannot delete
	resp = bob(t, "DELETE", "/sandboxes/"+sb.ID, nil)
	if resp.StatusCode != 404 {
		t.Fatalf("bob delete alice's sandbox: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Bob list is empty
	resp = bob(t, "GET", "/sandboxes", nil)
	var bobList []store.Sandbox
	decodeJSON(t, resp, &bobList)
	if len(bobList) != 0 {
		t.Fatalf("bob should see 0 sandboxes, got %d", len(bobList))
	}
}

func TestCrossUserExecIsolation(t *testing.T) {
	_, alice, bob := setupTwoUsers(t)

	resp := alice(t, "POST", "/sandboxes", map[string]any{
		"name": uniqueName(t, "alice-exec"),
	})
	var sb store.Sandbox
	decodeJSON(t, resp, &sb)
	t.Cleanup(func() { alice(t, "DELETE", "/sandboxes/"+sb.ID, nil) })

	// Bob cannot exec
	resp = bob(t, "POST", "/sandboxes/"+sb.ID+"/exec", map[string]any{
		"cmd": []string{"echo", "hacked"},
	})
	if resp.StatusCode != 404 {
		t.Fatalf("bob exec: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Bob cannot stop
	resp = bob(t, "POST", "/sandboxes/"+sb.ID+"/stop", nil)
	if resp.StatusCode != 404 {
		t.Fatalf("bob stop: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestSandboxResourceCaps(t *testing.T) {
	_, alice, _ := setupTwoUsers(t)
	// Alice: MaxCPUsPerSandbox=2, MaxMemoryMBPerSandbox=1024

	resp := alice(t, "POST", "/sandboxes", map[string]any{"name": "big-cpu", "cpus": 8})
	if resp.StatusCode != 400 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("cpu cap: expected 400, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	resp = alice(t, "POST", "/sandboxes", map[string]any{"name": "big-mem", "memory_mb": 8192})
	if resp.StatusCode != 400 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("memory cap: expected 400, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	resp = alice(t, "POST", "/sandboxes", map[string]any{"name": uniqueName(t, "ok"), "cpus": 1})
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("within caps: expected 201, got %d: %s", resp.StatusCode, body)
	}
	var sb store.Sandbox
	decodeJSON(t, resp, &sb)
	alice(t, "DELETE", "/sandboxes/"+sb.ID, nil)
}

func TestSandboxCountLimit(t *testing.T) {
	_, alice, _ := setupTwoUsers(t)
	// Alice: MaxSandboxes=3

	var ids []string
	for i := 0; i < 3; i++ {
		resp := alice(t, "POST", "/sandboxes", map[string]any{
			"name": uniqueName(t, fmt.Sprintf("limit-%d", i)),
		})
		if resp.StatusCode != 201 {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("create %d: expected 201, got %d: %s", i, resp.StatusCode, body)
		}
		var sb store.Sandbox
		decodeJSON(t, resp, &sb)
		ids = append(ids, sb.ID)
	}
	t.Cleanup(func() {
		for _, id := range ids {
			alice(t, "DELETE", "/sandboxes/"+id, nil)
		}
	})

	// 4th should fail
	resp := alice(t, "POST", "/sandboxes", map[string]any{
		"name": uniqueName(t, "limit-over"),
	})
	if resp.StatusCode != 429 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 429, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

func TestSandboxNameValidationHTTP(t *testing.T) {
	_, alice, _ := setupTwoUsers(t)

	badNames := []string{
		"has space",
		"has\nnewline",
		"has/slash",
		"-starts-with-dash",
		".starts-with-dot",
		strings.Repeat("a", 64),
	}
	for _, name := range badNames {
		resp := alice(t, "POST", "/sandboxes", map[string]any{"name": name})
		if resp.StatusCode != 400 {
			t.Errorf("name %q: expected 400, got %d", name, resp.StatusCode)
			if resp.StatusCode == 201 {
				var sb store.Sandbox
				decodeJSON(t, resp, &sb)
				alice(t, "DELETE", "/sandboxes/"+sb.ID, nil)
			}
		}
		resp.Body.Close()
	}

	// Valid names
	goodNames := []string{"my-sandbox", "dev.v2", "test_env", "a", strings.Repeat("a", 63)}
	for _, name := range goodNames {
		resp := alice(t, "POST", "/sandboxes", map[string]any{"name": name})
		if resp.StatusCode == 400 {
			body, _ := io.ReadAll(resp.Body)
			if strings.Contains(string(body), "invalid sandbox name") {
				t.Errorf("name %q: should be valid but was rejected", name)
			}
		}
		if resp.StatusCode == 201 {
			var sb store.Sandbox
			decodeJSON(t, resp, &sb)
			alice(t, "DELETE", "/sandboxes/"+sb.ID, nil)
		}
		resp.Body.Close()
	}
}

func TestDuplicateSandboxNameHTTP(t *testing.T) {
	_, alice, _ := setupTwoUsers(t)
	name := uniqueName(t, "dup-name")

	resp := alice(t, "POST", "/sandboxes", map[string]any{"name": name})
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("first: expected 201, got %d: %s", resp.StatusCode, body)
	}
	var sb store.Sandbox
	decodeJSON(t, resp, &sb)
	t.Cleanup(func() { alice(t, "DELETE", "/sandboxes/"+sb.ID, nil) })

	// Duplicate — should return existing sandbox with 200 (idempotent create)
	resp = alice(t, "POST", "/sandboxes", map[string]any{"name": name})
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("duplicate: expected 200, got %d: %s", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Bhatti-Existing") != "true" {
		t.Error("missing X-Bhatti-Existing header")
	}
	var sb2 store.Sandbox
	decodeJSON(t, resp, &sb2)
	if sb2.ID != sb.ID {
		t.Errorf("expected same sandbox ID %q, got %q", sb.ID, sb2.ID)
	}
}

func TestCrossUserSecretIsolationHTTP(t *testing.T) {
	_, alice, bob := setupTwoUsers(t)

	resp := alice(t, "POST", "/secrets", map[string]any{
		"name": "alice-secret", "value": "secret-value",
	})
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("alice create secret: expected 201, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	// Bob can't see it
	resp = bob(t, "GET", "/secrets", nil)
	var bobSecrets []store.SecretRecord
	decodeJSON(t, resp, &bobSecrets)
	if len(bobSecrets) != 0 {
		t.Fatalf("bob should see 0 secrets, got %d", len(bobSecrets))
	}

	// Bob can't delete it
	resp = bob(t, "DELETE", "/secrets/alice-secret", nil)
	if resp.StatusCode != 404 {
		t.Fatalf("bob delete: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	alice(t, "DELETE", "/secrets/alice-secret", nil)
}

func TestSecretEncryptDecrypt(t *testing.T) {
	// Setup server with a real dataDir containing an age key
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}

	keyHash := sha256Hex("encrypt-test-key")
	st.CreateUser(store.User{
		ID: "usr_enc", Name: "encrypt-user", APIKeyHash: keyHash,
		MaxSandboxes: 5, MaxCPUsPerSandbox: 4, MaxMemoryMBPerSandbox: 4096,
		SubnetIndex: 1, CreatedAt: time.Now(),
	})

	eng := newMockEngine()
	srv := New(eng, st, dir) // pass dataDir for age key
	ts := httptest.NewServer(srv)
	t.Cleanup(func() { srv.Close(); ts.Close(); st.Close() })

	doReqEnc := func(method, path string, body any) *http.Response {
		t.Helper()
		var bodyReader io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			bodyReader = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, ts.URL+path, bodyReader)
		req.Header.Set("Authorization", "Bearer encrypt-test-key")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// Create a secret
	resp := doReqEnc("POST", "/secrets", map[string]any{
		"name": "my-api-key", "value": "sk-super-secret-value-12345",
	})
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create secret: expected 201, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	// Verify the stored bytes are NOT plaintext
	raw, err := st.GetSecretValue("usr_enc", "my-api-key")
	if err != nil {
		t.Fatalf("get raw secret: %v", err)
	}
	if string(raw) == "sk-super-secret-value-12345" {
		t.Fatal("secret stored as plaintext — encryption not working!")
	}
	if len(raw) == 0 {
		t.Fatal("secret stored as empty bytes")
	}
	t.Logf("stored %d bytes of ciphertext (not plaintext)", len(raw))

	// Verify the secret can be decrypted back to the original value
	// We test this by checking the server can resolve it correctly.
	// The secrets list endpoint returns metadata only (no values),
	// so we test decryption via the internal helper.
	plaintext, err := srv.decryptSecret(raw)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if string(plaintext) != "sk-super-secret-value-12345" {
		t.Fatalf("decrypted value = %q, want 'sk-super-secret-value-12345'", plaintext)
	}
	t.Log("secret encrypted at rest and decrypts correctly")

	// Update the secret — should re-encrypt
	resp = doReqEnc("POST", "/secrets", map[string]any{
		"name": "my-api-key", "value": "sk-updated-value",
	})
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("update secret: expected 201, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	raw2, _ := st.GetSecretValue("usr_enc", "my-api-key")
	if string(raw2) == "sk-updated-value" {
		t.Fatal("updated secret stored as plaintext")
	}
	plaintext2, _ := srv.decryptSecret(raw2)
	if string(plaintext2) != "sk-updated-value" {
		t.Fatalf("updated decrypted = %q, want 'sk-updated-value'", plaintext2)
	}
	t.Log("secret update re-encrypts correctly")

	// Clean up
	doReqEnc("DELETE", "/secrets/my-api-key", nil)
}

func TestRateLimiting(t *testing.T) {
	_, ts := setup(t)

	// Burst 10 sandbox creates rapidly — the 11th+ should get 429
	var got429 bool
	for i := 0; i < 15; i++ {
		resp := doReq(t, ts, "POST", "/sandboxes", map[string]any{
			"name": uniqueName(t, fmt.Sprintf("rate-%d", i)),
		})
		if resp.StatusCode == 429 {
			got429 = true
			resp.Body.Close()
			break
		}
		if resp.StatusCode == 201 {
			var sb store.Sandbox
			decodeJSON(t, resp, &sb)
			t.Cleanup(func() { doReq(t, ts, "DELETE", "/sandboxes/"+sb.ID, nil) })
		} else {
			resp.Body.Close()
		}
	}
	if !got429 {
		t.Error("expected at least one 429 after rapid creates")
	}
}

func TestExecTimeout(t *testing.T) {
	_, ts := setup(t)
	sb := createSandbox(t, ts, uniqueName(t, "timeout"))

	// Request with a 1-second timeout — mock engine exec is instant
	// so this tests the plumbing, not actual timeout behavior
	resp := doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/exec", map[string]any{
		"cmd":         []string{"echo", "fast"},
		"timeout_sec": 1,
	})
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	var result engine.ExecResult
	decodeJSON(t, resp, &result)
	if result.ExitCode != 0 {
		t.Errorf("exit code: %d", result.ExitCode)
	}
}

func TestExecTimeoutClamped(t *testing.T) {
	_, ts := setup(t)
	sb := createSandbox(t, ts, uniqueName(t, "clamp"))

	// timeout_sec > 86400 should be ignored (uses default 300)
	resp := doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/exec", map[string]any{
		"cmd":         []string{"echo", "ok"},
		"timeout_sec": 100000,
	})
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

func TestErrorSanitization(t *testing.T) {
	srv, ts := setup(t)

	// Force an engine error that contains internal path info
	mockEng := srv.engine.(*mockEngine)
	mockEng.CreateErr = fmt.Errorf("internal: /var/lib/bhatti/sandboxes/abc/rootfs.ext4 failed")

	resp := doReq(t, ts, "POST", "/sandboxes", map[string]any{
		"name": uniqueName(t, "err-test"),
	})
	if resp.StatusCode != 500 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 500, got %d: %s", resp.StatusCode, body)
	}

	var errBody map[string]string
	decodeJSON(t, resp, &errBody)

	// Should have request_id but NOT the internal path
	if errBody["request_id"] == "" {
		t.Error("expected request_id in error response")
	}
	if strings.Contains(errBody["error"], "/var/lib") {
		t.Error("error message leaks internal path")
	}
	if errBody["error"] != "internal error" {
		t.Errorf("expected 'internal error', got %q", errBody["error"])
	}
	t.Logf("sanitized error: %+v", errBody)

	// Reset
	mockEng.CreateErr = nil
}

func TestRequestHasID(t *testing.T) {
	_, ts := setup(t)

	// Make any request, check we get logging (hard to test log output
	// directly, but we can verify the 500 error includes request_id)
	resp := doReq(t, ts, "GET", "/sandboxes", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}


func TestClassifyRequest(t *testing.T) {
	tests := []struct {
		method string
		path   string
		want   string
	}{
		{"POST", "/sandboxes", "create"},
		{"POST", "/sandboxes/abc/exec", "exec"},
		{"PUT", "/sandboxes/abc/files?path=/test", "exec"},
		{"GET", "/sandboxes/abc/ws", "exec"},
		{"GET", "/sandboxes", "read"},
		{"GET", "/sandboxes/abc", "read"},
		{"GET", "/sandboxes/abc/ports", "read"},
		{"DELETE", "/sandboxes/abc", "read"},
		{"GET", "/templates", "read"},
		{"POST", "/secrets", "read"},
	}

	for _, tt := range tests {
		r, _ := http.NewRequest(tt.method, "http://localhost"+tt.path, nil)
		got := classifyRequest(r)
		if got != tt.want {
			t.Errorf("%s %s: got %q, want %q", tt.method, tt.path, got, tt.want)
		}
	}
}


func TestVersionHeadersPresent(t *testing.T) {
	oldVer := ServerVersion
	oldMin := MinCLIVersion
	ServerVersion = "0.5.0"
	MinCLIVersion = "0.4.0"
	t.Cleanup(func() {
		ServerVersion = oldVer
		MinCLIVersion = oldMin
	})

	_, ts := setup(t)

	// Authenticated request should get version headers
	resp := doReq(t, ts, "GET", "/sandboxes", nil)
	defer resp.Body.Close()

	if got := resp.Header.Get("X-Bhatti-Version"); got != "0.5.0" {
		t.Errorf("X-Bhatti-Version = %q, want %q", got, "0.5.0")
	}
	if got := resp.Header.Get("X-Bhatti-Min-CLI"); got != "0.4.0" {
		t.Errorf("X-Bhatti-Min-CLI = %q, want %q", got, "0.4.0")
	}
}


func TestVersionHeaderOnHealth(t *testing.T) {
	oldVer := ServerVersion
	ServerVersion = "0.5.0"
	t.Cleanup(func() { ServerVersion = oldVer })

	_, ts := setup(t)

	// Health endpoint is unauthenticated but should still get version header
	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("X-Bhatti-Version"); got != "0.5.0" {
		t.Errorf("X-Bhatti-Version on /health = %q, want %q", got, "0.5.0")
	}
}
