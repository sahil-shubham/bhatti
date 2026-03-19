//go:build linux

package firecracker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/server"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

// setupFullStack creates a real FC engine + store + HTTP server for integration tests.
func setupFullStack(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	eng := testEngine(t)
	ctx := context.Background()

	// Create a sandbox
	info, err := eng.Create(ctx, testSpec("proxy-test"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { eng.Destroy(ctx, info.ID) })

	// Set up store
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	// Register sandbox in store
	st.CreateSandbox(store.Sandbox{
		ID: info.ID, Name: info.Name, EngineID: info.EngineID,
		Status: "running", IP: info.IP,
		EngineMeta: json.RawMessage("{}"),
		CreatedAt:  time.Now(),
	})

	// Start server
	srv := server.New(eng, st, "test-token")
	ts := httptest.NewServer(srv)
	t.Cleanup(func() { srv.Close(); ts.Close() })

	// Start a python HTTP server inside the VM
	execWithTimeout(t, eng, info.ID, []string{"sh", "-c",
		"cd /tmp && echo proxy-content > index.html && python3 -m http.server 8888 </dev/null >/dev/null 2>&1 &"})
	time.Sleep(1 * time.Second)

	return ts, info.ID
}

func doProxyReq(t *testing.T, ts *httptest.Server, method, path string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+path, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestProxyHTTPGet(t *testing.T) {
	ts, sbID := setupFullStack(t)

	resp := doProxyReq(t, ts, "GET", "/sandboxes/"+sbID+"/proxy/8888/index.html")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "proxy-content") {
		t.Errorf("body: %q, want 'proxy-content'", body)
	}
	t.Log("✓ proxy HTTP GET works")
}

func TestProxyHTTPHeaders(t *testing.T) {
	ts, sbID := setupFullStack(t)

	resp := doProxyReq(t, ts, "GET", "/sandboxes/"+sbID+"/proxy/8888/index.html")
	defer resp.Body.Close()
	// Python's http.server sets Server and Content-Type headers
	if ct := resp.Header.Get("Content-Type"); ct == "" {
		t.Error("Content-Type header missing from proxied response")
	}
	t.Logf("✓ proxy passes headers: Content-Type=%s", resp.Header.Get("Content-Type"))
}

func TestProxyHTTP404(t *testing.T) {
	ts, sbID := setupFullStack(t)

	resp := doProxyReq(t, ts, "GET", "/sandboxes/"+sbID+"/proxy/8888/nonexistent")
	defer resp.Body.Close()
	// Python http.server returns 404 for missing files — should be forwarded, not 502
	if resp.StatusCode != 404 {
		t.Errorf("expected 404 from upstream, got %d", resp.StatusCode)
	}
	t.Log("✓ proxy forwards 404 (not 502)")
}

func TestProxyInvalidPort(t *testing.T) {
	ts, sbID := setupFullStack(t)

	resp := doProxyReq(t, ts, "GET", "/sandboxes/"+sbID+"/proxy/abc/")
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("expected 400 for invalid port, got %d", resp.StatusCode)
	}
	t.Log("✓ proxy rejects invalid port")
}

func TestProxySandboxNotFound(t *testing.T) {
	ts, _ := setupFullStack(t)

	resp := doProxyReq(t, ts, "GET", "/sandboxes/nonexistent/proxy/8888/")
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
	t.Log("✓ proxy returns 404 for unknown sandbox")
}

func TestProxyExecStreamViaHTTP(t *testing.T) {
	ts, sbID := setupFullStack(t)

	// Test NDJSON streaming through the full HTTP stack
	body := strings.NewReader(`{"cmd":["echo","proxy-exec"]}`)
	req, _ := http.NewRequest("POST", ts.URL+"/sandboxes/"+sbID+"/exec", body)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.Header.Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("Content-Type: %q", resp.Header.Get("Content-Type"))
	}

	scanner := bufio.NewScanner(resp.Body)
	var gotStdout, gotExit bool
	for scanner.Scan() {
		var event map[string]interface{}
		json.Unmarshal(scanner.Bytes(), &event)
		if event["type"] == "stdout" && strings.Contains(fmt.Sprint(event["data"]), "proxy-exec") {
			gotStdout = true
		}
		if event["type"] == "exit" {
			gotExit = true
		}
	}
	if !gotStdout || !gotExit {
		t.Errorf("stdout=%v exit=%v", gotStdout, gotExit)
	}
	t.Log("✓ NDJSON streaming exec works through full HTTP stack")
}
