//go:build krucible

package krucible

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/server"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

// TestKrucibleServerIntegration drives the FULL daemon stack (HTTP API + store +
// thermal manager) over a real krucible block-root engine — the "whole suite on
// the engine" milestone. It proves the production wake-on-request path: an exec
// against a COLD (snapshotted, helper-killed) sandbox transparently cold-restores
// it via the server's ensureHot -> EnsureHot -> Start.
func TestKrucibleServerIntegration(t *testing.T) {
	_, do := krucibleServer(t) // skips if libkrun/vmm/mke2fs unavailable

	// --- create over HTTP ---
	resp := do("POST", "/sandboxes", map[string]any{"name": "srv-it"})
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create: want 201, got %d: %s", resp.StatusCode, b)
	}
	var sb store.Sandbox
	json.NewDecoder(resp.Body).Decode(&sb)
	resp.Body.Close()
	t.Cleanup(func() { do("DELETE", "/sandboxes/"+sb.ID, nil) })

	exec := func(want string, cmd ...string) {
		t.Helper()
		resp := do("POST", "/sandboxes/"+sb.ID+"/exec", map[string]any{"cmd": cmd})
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("exec %v: want 200, got %d: %s", cmd, resp.StatusCode, b)
		}
		var res engine.ExecResult
		json.NewDecoder(resp.Body).Decode(&res)
		if !strings.Contains(res.Stdout, want) {
			t.Fatalf("exec %v: stdout %q does not contain %q", cmd, res.Stdout, want)
		}
	}

	t.Run("ExecHot", func(t *testing.T) { exec("srv-hello", "echo", "srv-hello") })

	t.Run("ColdStop", func(t *testing.T) {
		resp := do("POST", "/sandboxes/"+sb.ID+"/stop", nil)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("stop: want 200, got %d", resp.StatusCode)
		}
	})

	// The key assertion: exec against a COLD sandbox auto-wakes it (cold restore)
	// through the server's wake-on-request path — no explicit /start needed.
	t.Run("ExecAutoWakesFromCold", func(t *testing.T) { exec("woke-cold", "echo", "woke-cold") })
}

// doFunc issues an authenticated request against the krucible-backed test daemon.
type doFunc func(method, path string, body any) *http.Response

// krucibleServer stands up the full daemon (HTTP API + store + thermal) over a
// real krucible block-root engine and returns an httptest server + an
// authenticated request helper. Skips if libkrun/vmm/mke2fs are unavailable.
func krucibleServer(t *testing.T) (*httptest.Server, doFunc) {
	t.Helper()
	eng := newBlockRootEngine(t)
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	const apiKey = "test-token"
	sum := sha256.Sum256([]byte(apiKey))
	if err := st.CreateUser(store.User{
		ID: "usr_test", Name: "test-user", APIKeyHash: hex.EncodeToString(sum[:]),
		MaxSandboxes: 50, MaxCPUsPerSandbox: 4, MaxMemoryMBPerSandbox: 4096,
		SubnetIndex: 1, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	srv := server.New(eng, st, dir)
	ts := httptest.NewServer(srv)
	t.Cleanup(func() { srv.Close(); ts.Close() })

	do := func(method, path string, body any) *http.Response {
		t.Helper()
		var br io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			br = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, ts.URL+path, br)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return resp
	}
	return ts, do
}

// TestKrucibleServerForward drives `bhatti forward` end to end through the full
// daemon over a real VM (no mock): create -> start a guest HTTP server
// (detached exec) -> POST /forward -> the daemon binds a host port and bridges
// it to the guest over the vsock tunnel -> a GET to that host port returns the
// guest's response.
func TestKrucibleServerForward(t *testing.T) {
	_, do := krucibleServer(t)

	resp := do("POST", "/sandboxes", map[string]any{"name": "fwd-srv"})
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create: want 201, got %d: %s", resp.StatusCode, b)
	}
	var sb store.Sandbox
	json.NewDecoder(resp.Body).Decode(&sb)
	resp.Body.Close()
	t.Cleanup(func() { do("DELETE", "/sandboxes/"+sb.ID, nil) })

	// Start a real HTTP server inside the guest (detached).
	resp = do("POST", "/sandboxes/"+sb.ID+"/exec", map[string]any{
		"cmd": []string{"/bin/netcheck", "serve", "8080"}, "detach": true, "output_file": "/tmp/serve.log",
	})
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("detached exec: want 200, got %d: %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	// Ask the daemon to forward a host port to guest :8080.
	resp = do("POST", "/sandboxes/"+sb.ID+"/forward", map[string]any{"guest_port": 8080})
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("forward: want 200, got %d: %s", resp.StatusCode, b)
	}
	var fwd struct {
		HostAddr string `json:"host_addr"`
	}
	json.NewDecoder(resp.Body).Decode(&fwd)
	resp.Body.Close()
	if fwd.HostAddr == "" {
		t.Fatal("forward returned no host_addr")
	}

	body := httpGetRetry(t, "http://"+fwd.HostAddr+"/", 25*time.Second)
	if body != "hello-from-guest\n" {
		t.Fatalf("forwarded response = %q, want %q", body, "hello-from-guest\n")
	}
}
