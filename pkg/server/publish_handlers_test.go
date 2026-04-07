package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/store"
)

// ==========================================================================
// Publish / Unpublish
// ==========================================================================

func TestPublishHTTP(t *testing.T) {
	_, ts := setup(t)
	sb := createSandbox(t, ts, uniqueName(t, "pub"))

	resp := doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/publish", map[string]interface{}{
		"port":  3000,
		"alias": "test-app",
	})
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d: %s", resp.StatusCode, body)
	}
	var result map[string]interface{}
	decodeJSON(t, resp, &result)
	if result["alias"] != "test-app" {
		t.Errorf("alias: %v", result["alias"])
	}
	if result["port"].(float64) != 3000 {
		t.Errorf("port: %v", result["port"])
	}
}

func TestPublishAutoAlias(t *testing.T) {
	_, ts := setup(t)
	sb := createSandbox(t, ts, uniqueName(t, "pub"))

	resp := doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/publish", map[string]interface{}{
		"port": 3000,
	})
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d: %s", resp.StatusCode, body)
	}
	var result map[string]interface{}
	decodeJSON(t, resp, &result)
	if result["alias"] == nil || result["alias"] == "" {
		t.Fatal("expected auto-generated alias")
	}
}

func TestPublishDuplicateAlias(t *testing.T) {
	_, ts := setup(t)
	sb1 := createSandbox(t, ts, uniqueName(t, "pub1"))
	sb2 := createSandbox(t, ts, uniqueName(t, "pub2"))

	doReq(t, ts, "POST", "/sandboxes/"+sb1.ID+"/publish", map[string]interface{}{
		"port": 3000, "alias": "dup-alias",
	})
	resp := doReq(t, ts, "POST", "/sandboxes/"+sb2.ID+"/publish", map[string]interface{}{
		"port": 3000, "alias": "dup-alias",
	})
	defer resp.Body.Close()
	if resp.StatusCode != 409 {
		t.Fatalf("expected 409, got %d", resp.StatusCode)
	}
}

func TestPublishDuplicatePort(t *testing.T) {
	_, ts := setup(t)
	sb := createSandbox(t, ts, uniqueName(t, "pub"))

	doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/publish", map[string]interface{}{
		"port": 3000, "alias": "first",
	})
	resp := doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/publish", map[string]interface{}{
		"port": 3000, "alias": "second",
	})
	defer resp.Body.Close()
	if resp.StatusCode != 409 {
		t.Fatalf("expected 409, got %d", resp.StatusCode)
	}
}

func TestUnpublishHTTP(t *testing.T) {
	_, ts := setup(t)
	sb := createSandbox(t, ts, uniqueName(t, "pub"))

	doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/publish", map[string]interface{}{
		"port": 3000, "alias": "to-delete",
	})
	resp := doReq(t, ts, "DELETE", "/sandboxes/"+sb.ID+"/publish/3000", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("expected 204, got %d", resp.StatusCode)
	}
}

func TestListPublishHTTP(t *testing.T) {
	_, ts := setup(t)
	sb := createSandbox(t, ts, uniqueName(t, "pub"))

	doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/publish", map[string]interface{}{"port": 3000, "alias": "a1"})
	doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/publish", map[string]interface{}{"port": 3001, "alias": "a2"})

	resp := doReq(t, ts, "GET", "/sandboxes/"+sb.ID+"/publish", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var rules []map[string]interface{}
	decodeJSON(t, resp, &rules)
	if len(rules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(rules))
	}
}

func TestDestroyCleanupPublish(t *testing.T) {
	srv, ts := setup(t)
	sb := createSandbox(t, ts, uniqueName(t, "pub"))

	doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/publish", map[string]interface{}{"port": 3000, "alias": "cleanup"})

	// Destroy (bypass cleanup registered by createSandbox)
	resp := doReq(t, ts, "DELETE", "/sandboxes/"+sb.ID, nil)
	resp.Body.Close()

	rules, _ := srv.store.ListPublishRules(sb.ID)
	if len(rules) != 0 {
		t.Fatalf("expected 0 rules after destroy, got %d", len(rules))
	}
}

func TestAliasValidation(t *testing.T) {
	_, ts := setup(t)
	sb := createSandbox(t, ts, uniqueName(t, "pub"))

	tests := []struct {
		alias string
		want  int
	}{
		{"UPPERCASE", 400},
		{"-leading-dash", 400},
		{"has spaces", 400},
		{"api", 400},     // reserved
		{"www", 400},     // reserved
		{"valid-alias", 201},
	}

	for _, tt := range tests {
		resp := doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/publish", map[string]interface{}{
			"port": 3000 + len(tt.alias), "alias": tt.alias,
		})
		resp.Body.Close()
		if resp.StatusCode != tt.want {
			t.Errorf("alias %q: got %d, want %d", tt.alias, resp.StatusCode, tt.want)
		}
	}
}

func TestPublishNonexistentSandbox(t *testing.T) {
	_, ts := setup(t)
	resp := doReq(t, ts, "POST", "/sandboxes/nonexistent/publish", map[string]interface{}{
		"port": 3000, "alias": "nope",
	})
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// ==========================================================================
// Phase 2: Domain Mode — Host-Based Routing
// ==========================================================================

func setupDomainMode(t *testing.T) (*Server, *httptest.Server) {
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
	srv := New(eng, st, dir,
		WithProxyZone("test.sh"),
		WithAPIHost("api.test.sh"),
	)
	pub := NewPublicProxyHandler(eng, st, srv.ResumeSem(),
		func(engineID string) { srv.TouchActivity(engineID) },
		func(ctx context.Context, engineID string) error { return srv.EnsureHot(ctx, engineID) },
	)
	srv.SetPublicProxy(pub)

	ts := httptest.NewServer(srv)
	t.Cleanup(func() { srv.Close(); ts.Close(); st.Close() })
	return srv, ts
}

func doReqWithHost(t *testing.T, ts *httptest.Server, method, host, path string, auth bool) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+path, nil)
	req.Host = host
	if auth {
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
	}
	// Don't follow redirects
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestHostBasedRouting(t *testing.T) {
	srv, ts := setupDomainMode(t)

	// Create a sandbox and publish it
	sb := createSandbox(t, ts, uniqueName(t, "domain"))
	resp := doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/publish", map[string]interface{}{
		"port": 8080, "alias": "my-app",
	})
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("publish: %d", resp.StatusCode)
	}
	_ = srv // used for setup

	// Request with proxy zone host should hit public proxy, NOT demand auth.
	// The mock engine's Tunnel returns a pipe that blocks, so use a short
	// client timeout. We only care about routing: status != 401.
	shortClient := &http.Client{
		Timeout: 2 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, _ := http.NewRequest("GET", ts.URL+"/", nil)
	req.Host = "my-app.test.sh"
	proxyResp, err := shortClient.Do(req)
	if err != nil {
		// Timeout is expected (mock pipe blocks). That proves routing
		// reached the proxy (not auth). If it hit auth, we'd get 401 instantly.
		t.Logf("proxy request timed out as expected (mock pipe): %v", err)
	} else {
		proxyResp.Body.Close()
		if proxyResp.StatusCode == 401 {
			t.Fatal("proxy zone request should NOT require auth")
		}
		t.Logf("proxy zone routed correctly, status=%d", proxyResp.StatusCode)
	}
}

func TestAPIHostRouting(t *testing.T) {
	_, ts := setupDomainMode(t)

	// Request with API host goes through auth
	resp := doReqWithHost(t, ts, "GET", "api.test.sh", "/health", false)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("health on api host: expected 200, got %d", resp.StatusCode)
	}

	// Authenticated request to API host works
	resp = doReqWithHost(t, ts, "GET", "api.test.sh", "/sandboxes", true)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("sandboxes on api host: expected 200, got %d", resp.StatusCode)
	}
}

func TestUnknownHostReturns404(t *testing.T) {
	_, ts := setupDomainMode(t)

	resp := doReqWithHost(t, ts, "GET", "evil.example.com", "/", false)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("unknown host: expected 404, got %d", resp.StatusCode)
	}
}

func TestLocalhostBypassesDomainCheck(t *testing.T) {
	_, ts := setupDomainMode(t)

	// localhost should pass through to normal auth flow (for internal API)
	resp := doReqWithHost(t, ts, "GET", "localhost", "/health", false)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("localhost health: expected 200, got %d", resp.StatusCode)
	}
}

func TestHostPolicyAllowsAPIHost(t *testing.T) {
	srv, _ := setupDomainMode(t)
	if err := srv.HostPolicy(context.Background(), "api.test.sh"); err != nil {
		t.Fatalf("HostPolicy should allow API host: %v", err)
	}
}

func TestHostPolicyAllowsPublishedAlias(t *testing.T) {
	srv, ts := setupDomainMode(t)
	sb := createSandbox(t, ts, uniqueName(t, "hp"))
	resp := doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/publish", map[string]interface{}{
		"port": 3000, "alias": "hp-test",
	})
	resp.Body.Close()

	if err := srv.HostPolicy(context.Background(), "hp-test.test.sh"); err != nil {
		t.Fatalf("HostPolicy should allow published alias: %v", err)
	}
}

func TestHostPolicyRejectsUnknown(t *testing.T) {
	srv, _ := setupDomainMode(t)
	if err := srv.HostPolicy(context.Background(), "nonexistent.test.sh"); err == nil {
		t.Fatal("HostPolicy should reject unknown alias")
	}
	if err := srv.HostPolicy(context.Background(), "evil.example.com"); err == nil {
		t.Fatal("HostPolicy should reject unknown host")
	}
}
