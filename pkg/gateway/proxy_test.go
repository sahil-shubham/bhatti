package gateway

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
)

// recordingUpstream is an httptest server that captures what the proxy forwarded.
func recordingUpstream(t *testing.T) (*httptest.Server, *capturedReq) {
	t.Helper()
	cap := &capturedReq{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.mu.Lock()
		defer cap.mu.Unlock()
		cap.auth = r.Header.Get("Authorization")
		cap.proxyAuth = r.Header.Get("Proxy-Authorization")
		cap.proxyTok = r.Header.Get("Proxy-Tokenizer")
		b, _ := io.ReadAll(r.Body)
		cap.body = string(b)
		cap.hits++
		io.WriteString(w, "upstream-ok")
	}))
	t.Cleanup(srv.Close)
	return srv, cap
}

type capturedReq struct {
	mu                        sync.Mutex
	auth, proxyAuth, proxyTok string
	body                      string
	hits                      int
}

// TestProxySubstitutesToAllowedHost is the end-to-end happy path: an alias in the
// request → real secret arrives upstream, alias gone, proxy headers stripped.
func TestProxySubstitutesToAllowedHost(t *testing.T) {
	up, cap := recordingUpstream(t)
	host := hostOnly(strings.TrimPrefix(up.URL, "http://"))

	tbl := NewAliasTable()
	alias, _ := MintAlias("sk_live_")
	tbl.Add(alias, bindingFor(t, "STRIPE_KEY", "sk_live_REALSECRET", host))

	var events []Event
	p := &Proxy{Aliases: tbl, Sandbox: "s1", Transport: up.Client().Transport,
		Audit: func(e Event) { events = append(events, e) }}

	req := httptest.NewRequest(http.MethodPost, up.URL+"/charge", strings.NewReader("token="+alias))
	req.Header.Set("Authorization", "Bearer "+alias)
	req.Header.Set("Proxy-Authorization", "Bearer sandbox-proof")
	req.Header.Set("Proxy-Tokenizer", "x")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.hits != 1 {
		t.Fatalf("upstream hits = %d, want 1", cap.hits)
	}
	if cap.auth != "Bearer sk_live_REALSECRET" {
		t.Errorf("upstream Authorization = %q, want the REAL secret", cap.auth)
	}
	if strings.Contains(cap.auth, alias) || strings.Contains(cap.body, alias) {
		t.Error("alias leaked to upstream")
	}
	if cap.body != "token=sk_live_REALSECRET" {
		t.Errorf("body substitution wrong: %q", cap.body)
	}
	if cap.proxyAuth != "" || cap.proxyTok != "" {
		t.Errorf("proxy-only headers not stripped: auth=%q tok=%q", cap.proxyAuth, cap.proxyTok)
	}
	assertEvent(t, events, "substitute")
}

// TestProxyBlocksExfil: the alias headed to a NON-allowed host is blocked, the
// real secret never leaves, upstream is never hit, and it's audited.
func TestProxyBlocksExfil(t *testing.T) {
	up, cap := recordingUpstream(t) // "attacker" upstream

	tbl := NewAliasTable()
	alias, _ := MintAlias("sk_live_")
	// Secret is only allowed to api.stripe.com, NOT this host.
	tbl.Add(alias, bindingFor(t, "STRIPE_KEY", "sk_live_REALSECRET", "api.stripe.com"))

	var events []Event
	p := &Proxy{Aliases: tbl, Sandbox: "s1", Transport: up.Client().Transport,
		Audit: func(e Event) { events = append(events, e) }}

	req := httptest.NewRequest(http.MethodGet, up.URL+"/steal", nil)
	req.Header.Set("Authorization", "Bearer "+alias)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (exfil blocked)", rec.Code)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.hits != 0 {
		t.Fatal("upstream must not be hit on an exfil attempt")
	}
	assertEvent(t, events, "exfil")
}

// TestProxyPassthroughNoAlias: an ordinary request with no secret is forwarded
// unchanged.
func TestProxyPassthroughNoAlias(t *testing.T) {
	up, cap := recordingUpstream(t)
	p := &Proxy{Aliases: NewAliasTable(), Transport: up.Client().Transport}
	req := httptest.NewRequest(http.MethodGet, up.URL+"/", nil)
	req.Header.Set("Authorization", "Bearer plain-token")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.auth != "Bearer plain-token" || cap.hits != 1 {
		t.Errorf("passthrough altered the request: auth=%q hits=%d", cap.auth, cap.hits)
	}
}

// TestProxyGuardIsWired proves the SSRF guard is in the upstream dial path: with
// the real vetting Dialer as the transport, a request to a loopback upstream is
// denied (loopback is hard-denied), so the proxy returns 502 and never connects.
func TestProxyGuardIsWired(t *testing.T) {
	up, cap := recordingUpstream(t)                                  // on 127.0.0.1
	dialer := &Dialer{Policy: &EgressPolicy{Default: PosturePublic}} // real guard
	p := &Proxy{Aliases: NewAliasTable(), Transport: &http.Transport{DialContext: dialer.DialContext}}

	req := httptest.NewRequest(http.MethodGet, up.URL+"/", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (guard denies loopback upstream)", rec.Code)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.hits != 0 {
		t.Fatal("guard must prevent connecting to a loopback upstream")
	}
}

func assertEvent(t *testing.T, events []Event, kind string) {
	t.Helper()
	for _, e := range events {
		if e.Kind == kind {
			return
		}
	}
	t.Fatalf("expected an audit event of kind %q, got %+v", kind, events)
}

// TestDialerSelectsVettedIP proves the vet-dialer connects to a permitted IP and
// skips denied ones (multi-address failover + rebinding-safety), using the
// injected dial seam so no real network is needed.
func TestDialerSelectsVettedIP(t *testing.T) {
	res := fakeResolver{
		// public.test resolves to a denied private IP AND a public IP; only the
		// public one may be dialed.
		"public.test": {mustAddr(t, "10.0.0.9"), mustAddr(t, "1.1.1.1")},
	}
	var dialed []string
	d := &Dialer{
		Policy:   &EgressPolicy{Default: PosturePublic},
		Resolver: res,
		dialAddr: func(_ context.Context, _, addr string) (net.Conn, error) {
			dialed = append(dialed, addr)
			return fakeConn{}, nil
		},
	}
	if _, err := d.DialContext(context.Background(), "tcp", "public.test:443"); err != nil {
		t.Fatalf("dial: %v", err)
	}
	if len(dialed) != 1 || dialed[0] != net.JoinHostPort("1.1.1.1", "443") {
		t.Fatalf("dialed %v, want only the vetted public IP", dialed)
	}

	// allow-cidr opts the private IP back in — now it's dialable.
	dialed = nil
	d.Policy.AllowCIDRs = []netip.Prefix{mustPrefix(t, "10.0.0.0/8")}
	if _, err := d.DialContext(context.Background(), "tcp", "public.test:443"); err != nil {
		t.Fatalf("dial with allow-cidr: %v", err)
	}
	if len(dialed) == 0 || dialed[0] != net.JoinHostPort("10.0.0.9", "443") {
		t.Fatalf("with allow-cidr, first vetted (private) IP should dial: %v", dialed)
	}
}

type fakeConn struct{ net.Conn }

func (fakeConn) Close() error { return nil }
