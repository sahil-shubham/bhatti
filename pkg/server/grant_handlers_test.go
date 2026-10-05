package server

import (
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/gateway"
)

type grantJSON struct {
	ID          string     `json:"id"`
	Secret      string     `json:"secret"`
	SandboxID   string     `json:"sandbox_id"`
	SandboxName string     `json:"sandbox_name"`
	Hosts       []string   `json:"hosts"`
	Placeholder string     `json:"placeholder"`
	Status      string     `json:"status"`
	ExpiresAt   *time.Time `json:"expires_at"`
}

type createdJSON struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	NetPolicy    json.RawMessage `json:"net_policy"`
	SecretGrants []grantJSON     `json:"secret_grants"`
}

// expect asserts the status and decodes the body into v (if non-nil).
func expect(t *testing.T, resp *http.Response, status int, v any) {
	t.Helper()
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != status {
		t.Fatalf("status %d, want %d: %s", resp.StatusCode, status, body)
	}
	if v != nil {
		if err := json.Unmarshal(body, v); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
	}
}

func grantSetup(t *testing.T) (*Server, *httptest.Server, *mockEngine) {
	t.Helper()
	srv, ts := setup(t)
	expect(t, doReq(t, ts, "POST", "/secrets", map[string]string{"name": "GH_TOKEN", "value": "ghp_real"}), 201, nil)
	return srv, ts, srv.engine.(*mockEngine)
}

func createWith(t *testing.T, ts *httptest.Server, body map[string]any) createdJSON {
	t.Helper()
	var sb createdJSON
	expect(t, doReq(t, ts, "POST", "/sandboxes", body), 201, &sb)
	return sb
}

// TestSandboxGetsItsOwnCA: a sandbox with a network is created with a CA of
// its own in its boot config and the store; one without a network gets none.
func TestSandboxGetsItsOwnCA(t *testing.T) {
	srv, ts, eng := grantSetup(t)
	sb := createWith(t, ts, map[string]any{"name": "netted", "net_policy": map[string]any{"default": "public"}})
	spec := eng.LastCreateSpec
	if spec.SandboxID != sb.ID {
		t.Fatalf("engine got sandbox ID %q, want the server's %q", spec.SandboxID, sb.ID)
	}
	block, _ := pem.Decode([]byte(spec.CACert))
	if block == nil {
		t.Fatal("no CA certificate in the boot config")
	}
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil || !ca.IsCA {
		t.Fatalf("boot config CA: %v isCA=%v", err, ca != nil && ca.IsCA)
	}
	stored, err := srv.store.GetSandboxCA(sb.ID)
	if err != nil || stored.CertPEM != spec.CACert {
		t.Fatalf("stored CA: %v", err)
	}
	if len(stored.KeyEnc) == 0 || isPEMKey(stored.KeyEnc) {
		t.Fatal("CA key stored in the clear")
	}
	none := createWith(t, ts, map[string]any{"name": "offline"})
	if eng.LastCreateSpec.CACert != "" {
		t.Fatal("a sandbox without a network got a CA")
	}
	if _, err := srv.store.GetSandboxCA(none.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("CA stored for a sandbox without a network: %v", err)
	}
}

func isPEMKey(b []byte) bool {
	block, _ := pem.Decode(b)
	return block != nil
}

// TestSecretGrantLifecycle: grant, list, revoke through the API; hosts must lie
// inside the sandbox's egress allow list.
func TestSecretGrantLifecycle(t *testing.T) {
	_, ts, _ := grantSetup(t)
	sb := createWith(t, ts, map[string]any{"name": "dev", "net_policy": map[string]any{
		"default": "deny", "allow_hosts": []string{"api.github.com", "*.githubusercontent.com"}}})

	var g grantJSON
	expect(t, doReq(t, ts, "POST", "/secrets/GH_TOKEN/grants", map[string]any{
		"sandbox": "dev", "hosts": []string{"API.github.com"}, "ttl": "7d"}), 201, &g)
	if !gateway.IsPlaceholder(g.Placeholder) || g.Secret != "GH_TOKEN" || g.SandboxID != sb.ID ||
		g.SandboxName != "dev" || g.Status != "active" || len(g.Hosts) != 1 || g.Hosts[0] != "api.github.com" {
		t.Fatalf("grant %+v", g)
	}
	if g.ExpiresAt == nil || time.Until(*g.ExpiresAt) < 7*24*time.Hour-time.Minute {
		t.Fatalf("expires_at %v, want ~7 days out", g.ExpiresAt)
	}
	var wild grantJSON
	expect(t, doReq(t, ts, "POST", "/secrets/GH_TOKEN/grants", map[string]any{
		"sandbox": sb.ID, "hosts": []string{"*.githubusercontent.com"}}), 201, &wild)
	if wild.ExpiresAt != nil {
		t.Fatal("a grant without ttl expires")
	}

	for name, body := range map[string]map[string]any{
		"host outside the allow list": {"sandbox": "dev", "hosts": []string{"evil.example"}},
		"broader than the allow rule": {"sandbox": "dev", "hosts": []string{"*.github.com"}},
		"no hosts":                    {"sandbox": "dev", "hosts": []string{}},
		"bad ttl":                     {"sandbox": "dev", "hosts": []string{"api.github.com"}, "ttl": "soon"},
	} {
		resp := doReq(t, ts, "POST", "/secrets/GH_TOKEN/grants", body)
		if resp.StatusCode != 400 {
			t.Errorf("%s: status %d, want 400", name, resp.StatusCode)
		}
		resp.Body.Close()
	}
	expect(t, doReq(t, ts, "POST", "/secrets/NOPE/grants", map[string]any{"sandbox": "dev", "hosts": []string{"api.github.com"}}), 404, nil)
	expect(t, doReq(t, ts, "POST", "/secrets/GH_TOKEN/grants", map[string]any{"sandbox": "ghost", "hosts": []string{"api.github.com"}}), 404, nil)

	expect(t, doReq(t, ts, "DELETE", "/secrets/grants/"+g.ID, nil), 200, nil)
	expect(t, doReq(t, ts, "DELETE", "/secrets/grants/"+g.ID, nil), 200, nil) // idempotent
	expect(t, doReq(t, ts, "DELETE", "/secrets/grants/nope", nil), 404, nil)
	var list []grantJSON
	expect(t, doReq(t, ts, "GET", "/secrets/grants", nil), 200, &list)
	status := map[string]string{}
	for _, l := range list {
		status[l.ID] = l.Status
	}
	if len(list) != 2 || status[g.ID] != "revoked" || status[wild.ID] != "active" {
		t.Fatalf("list %+v", list)
	}
}

// TestSecretGrantRefusals: no network, no CA, another user's sandbox.
func TestSecretGrantRefusals(t *testing.T) {
	srv, ts, _ := grantSetup(t)
	createWith(t, ts, map[string]any{"name": "offline"})
	expect(t, doReq(t, ts, "POST", "/secrets/GH_TOKEN/grants", map[string]any{"sandbox": "offline", "hosts": []string{"api.github.com"}}), 400, nil)

	legacy := createWith(t, ts, map[string]any{"name": "legacy", "net_policy": map[string]any{"default": "public"}})
	if err := srv.store.DeleteSandboxCA(legacy.ID); err != nil { // as if created before substitution existed
		t.Fatal(err)
	}
	expect(t, doReq(t, ts, "POST", "/secrets/GH_TOKEN/grants", map[string]any{"sandbox": "legacy", "hosts": []string{"api.github.com"}}), 409, nil)
}

func TestSecretGrantCrossUser(t *testing.T) {
	_, alice, bob := setupTwoUsers(t)
	expect(t, alice(t, "POST", "/secrets", map[string]string{"name": "K", "value": "v"}), 201, nil)
	expect(t, bob(t, "POST", "/secrets", map[string]string{"name": "K", "value": "v"}), 201, nil)
	var sb createdJSON
	expect(t, alice(t, "POST", "/sandboxes", map[string]any{"name": "a", "net_policy": map[string]any{"default": "public"}}), 201, &sb)
	expect(t, bob(t, "POST", "/secrets/K/grants", map[string]any{"sandbox": sb.ID, "hosts": []string{"x.example"}}), 404, nil)
	var g grantJSON
	expect(t, alice(t, "POST", "/secrets/K/grants", map[string]any{"sandbox": "a", "hosts": []string{"x.example"}}), 201, &g)
	expect(t, bob(t, "DELETE", "/secrets/grants/"+g.ID, nil), 404, nil)
	var bobs []grantJSON
	expect(t, bob(t, "GET", "/secrets/grants", nil), 200, &bobs)
	if len(bobs) != 0 {
		t.Fatalf("bob sees alice's grants: %+v", bobs)
	}
}

// TestCreateTimeSecretGrant: the guest boots with the placeholder, never the
// value, and the grant is bound to the sandbox's own ID.
func TestCreateTimeSecretGrant(t *testing.T) {
	srv, ts, eng := grantSetup(t)
	sb := createWith(t, ts, map[string]any{"name": "bot", "net_policy": map[string]any{"default": "public"},
		"env":           map[string]string{"OTHER": "x"},
		"secret_grants": []map[string]any{{"secret": "GH_TOKEN", "hosts": []string{"api.github.com", "uploads.github.com"}}}})
	if len(sb.SecretGrants) != 1 {
		t.Fatalf("response grants %+v", sb.SecretGrants)
	}
	g := sb.SecretGrants[0]
	env := eng.LastCreateSpec.Env
	if env["GH_TOKEN"] != g.Placeholder || !gateway.IsPlaceholder(env["GH_TOKEN"]) || env["OTHER"] != "x" {
		t.Fatalf("guest env %v, want GH_TOKEN = the placeholder %q", env, g.Placeholder)
	}
	stored, err := srv.store.GetSecretGrantByPlaceholder(g.Placeholder)
	if err != nil || stored.SandboxID != sb.ID || stored.SandboxID != eng.LastCreateSpec.SandboxID || len(stored.Hosts) != 2 {
		t.Fatalf("stored grant %+v err=%v", stored, err)
	}
	var np gateway.NetPolicyWire
	if err := json.Unmarshal(sb.NetPolicy, &np); err != nil || np.Default != "public" {
		t.Fatalf("recorded egress policy %s", sb.NetPolicy)
	}
}

func TestCreateTimeSecretGrantRefusals(t *testing.T) {
	_, ts, _ := grantSetup(t)
	grant := []map[string]any{{"secret": "GH_TOKEN", "hosts": []string{"api.github.com"}}}
	for name, body := range map[string]map[string]any{
		"no network":         {"name": "a", "secret_grants": grant},
		"outside allow list": {"name": "b", "net_policy": map[string]any{"allow_hosts": []string{"example.com"}}, "secret_grants": grant},
		"also plain":         {"name": "c", "net_policy": map[string]any{"default": "public"}, "secrets": []string{"GH_TOKEN"}, "secret_grants": grant},
		"unknown secret":     {"name": "d", "net_policy": map[string]any{"default": "public"}, "secret_grants": []map[string]any{{"secret": "NOPE", "hosts": []string{"api.github.com"}}}},
		"granted twice":      {"name": "e", "net_policy": map[string]any{"default": "public"}, "secret_grants": append(grant, grant...)},
		"fork":               {"name": "f", "from": "whatever", "secret_grants": grant},
	} {
		resp := doReq(t, ts, "POST", "/sandboxes", body)
		if resp.StatusCode != 400 {
			b, _ := io.ReadAll(resp.Body)
			t.Errorf("%s: status %d (%s), want 400", name, resp.StatusCode, b)
		}
		resp.Body.Close()
	}
}

// TestFailedCreateLeavesNoGrants: grants and the CA minted for a boot that
// failed don't outlive it.
func TestFailedCreateLeavesNoGrants(t *testing.T) {
	srv, ts, eng := grantSetup(t)
	eng.CreateErr = fmt.Errorf("boom")
	resp := doReq(t, ts, "POST", "/sandboxes", map[string]any{"name": "doomed", "net_policy": map[string]any{"default": "public"},
		"secret_grants": []map[string]any{{"secret": "GH_TOKEN", "hosts": []string{"api.github.com"}}}})
	resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if grants, _ := srv.store.ListSecretGrants("usr_test"); len(grants) != 0 {
		t.Fatalf("grants outlived the failed create: %+v", grants)
	}
}

// TestDestroyAndSecretDeleteDropGrants: a destroyed sandbox takes its grants
// and CA along; a deleted secret takes its grants.
func TestDestroyAndSecretDeleteDropGrants(t *testing.T) {
	srv, ts, _ := grantSetup(t)
	grant := []map[string]any{{"secret": "GH_TOKEN", "hosts": []string{"api.github.com"}}}
	a := createWith(t, ts, map[string]any{"name": "a", "net_policy": map[string]any{"default": "public"}, "secret_grants": grant})
	createWith(t, ts, map[string]any{"name": "b", "net_policy": map[string]any{"default": "public"}, "secret_grants": grant})
	expect(t, doReq(t, ts, "DELETE", "/sandboxes/"+a.ID, nil), 200, nil)
	if _, err := srv.store.GetSandboxCA(a.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("CA survived its sandbox: %v", err)
	}
	if grants, _ := srv.store.ListSecretGrants("usr_test"); len(grants) != 1 || grants[0].SandboxID == a.ID {
		t.Fatalf("grants after destroy: %+v", grants)
	}
	expect(t, doReq(t, ts, "DELETE", "/secrets/GH_TOKEN", nil), 200, nil)
	if grants, _ := srv.store.ListSecretGrants("usr_test"); len(grants) != 0 {
		t.Fatalf("grants outlived their secret: %+v", grants)
	}
}

func TestParseTTL(t *testing.T) {
	for in, want := range map[string]time.Duration{"": 0, "90m": 90 * time.Minute, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour} {
		if got, err := parseTTL(in); err != nil || got != want {
			t.Errorf("parseTTL(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"0d", "-1h", "0s", "7days", "d", "99999d"} {
		if _, err := parseTTL(bad); err == nil {
			t.Errorf("parseTTL(%q) accepted", bad)
		}
	}
}

func TestGrantRejectsMissingOrInvalidNetworkPolicy(t *testing.T) {
	for _, np := range []*gateway.NetPolicyWire{
		nil,
		{},
		{Default: "unknown"},
		{Default: gateway.PostureNone},
	} {
		if hosts, err := grantHosts([]string{"api.example.com"}, np); err == nil {
			t.Errorf("grantHosts(%+v) authorized %v without a valid network policy", np, hosts)
		}
	}
	if hosts, err := grantHosts([]string{"api.example.com"}, &gateway.NetPolicyWire{Default: "public"}); err != nil || len(hosts) != 1 {
		t.Fatalf("public guest grant = %v, %v", hosts, err)
	}
}
