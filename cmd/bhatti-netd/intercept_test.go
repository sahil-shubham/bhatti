package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/gateway"
)

// --- test PKI ---

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newTestCA(t *testing.T, cn string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, key: key}
}

// leaf returns PEM cert + key for host, signed by ca. Safe off the test
// goroutine (the fake broker mints from netd's goroutines).
func (ca *testCA) leaf(host string) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, nil, err
	}
	kd, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}), nil
}

func (ca *testCA) tlsCert(t *testing.T, host string) tls.Certificate {
	t.Helper()
	c, k, err := ca.leaf(host)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(c, k)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func pool(cas ...*testCA) *x509.CertPool {
	p := x509.NewCertPool()
	for _, ca := range cas {
		p.AddCert(ca.cert)
	}
	return p
}

// --- fake broker ---

type fakeGrant struct{ sandbox, host, value string }

type fakeBroker struct {
	ca *testCA // the sandbox CA

	mu            sync.Mutex
	hosts         map[string]bool      // hosts sandbox "sb1" holds a live grant for
	grants        map[string]fakeGrant // placeholder → grant
	refuseResolve string
	certCalls     int
	resolveCalls  int
	reports       []gateway.BrokerRequest
}

func (b *fakeBroker) Do(_ context.Context, req gateway.BrokerRequest) (gateway.BrokerResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch req.Op {
	case gateway.BrokerCert:
		b.certCalls++
		if req.Sandbox != "sb1" || !b.hosts[req.Host] {
			return gateway.BrokerResponse{Error: "no grant"}, &gateway.BrokerRefusal{Reason: "no grant"}
		}
		c, k, err := b.ca.leaf(req.Host)
		if err != nil {
			return gateway.BrokerResponse{}, err
		}
		return gateway.BrokerResponse{CertPEM: string(c), KeyPEM: string(k)}, nil
	case gateway.BrokerResolve:
		b.resolveCalls++
		g, ok := b.grants[req.Placeholder]
		if b.refuseResolve != "" || !ok || g.sandbox != req.Sandbox || g.host != req.Host {
			reason := b.refuseResolve
			if reason == "" {
				reason = "not granted"
			}
			return gateway.BrokerResponse{Error: reason}, &gateway.BrokerRefusal{Reason: reason}
		}
		return gateway.BrokerResponse{Value: g.value}, nil
	case gateway.BrokerReport:
		b.reports = append(b.reports, req)
		return gateway.BrokerResponse{}, nil
	}
	return gateway.BrokerResponse{}, fmt.Errorf("unknown op %q", req.Op)
}

func (b *fakeBroker) counts() (certs, resolves int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.certCalls, b.resolveCalls
}

func (b *fakeBroker) waitReport(t *testing.T) gateway.BrokerRequest {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		if len(b.reports) > 0 {
			r := b.reports[0]
			b.mu.Unlock()
			return r
		}
		b.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no denial reported to the broker")
	return gateway.BrokerRequest{}
}

// --- upstream ---

type seenReq struct {
	host, uri string
	header    http.Header
	body      string
}

type upstream struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []seenReq
}

// newUpstream serves HTTPS as host with a leaf from ca. A request is recorded
// only once its body was read completely.
func newUpstream(t *testing.T, ca *testCA, host string, alpn ...string) *upstream {
	t.Helper()
	u := &upstream{}
	u.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		u.mu.Lock()
		u.reqs = append(u.reqs, seenReq{host: r.Host, uri: r.RequestURI, header: r.Header.Clone(), body: string(body)})
		u.mu.Unlock()
		fmt.Fprintf(w, "ok %s", r.RequestURI)
	}))
	u.srv.TLS = &tls.Config{Certificates: []tls.Certificate{ca.tlsCert(t, host)}, NextProtos: alpn}
	u.srv.StartTLS()
	t.Cleanup(u.srv.Close)
	return u
}

func (u *upstream) seen() []seenReq {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]seenReq(nil), u.reqs...)
}

// --- harness ---

type rig struct {
	t        *testing.T
	sandbox  *testCA // the sandbox CA the guest trusts
	upCA     *testCA // the public CA netd trusts
	broker   *fakeBroker
	proxy    *credProxy
	up       *upstream
	ph       string // placeholder granted for api.test
	upstream string // upstream address
}

const realValue = "s3cr3t-value-0123456789"

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, sandbox: newTestCA(t, "bhatti sandbox CA"), upCA: newTestCA(t, "public CA")}
	ph, err := gateway.NewPlaceholder()
	if err != nil {
		t.Fatal(err)
	}
	r.ph = ph
	r.broker = &fakeBroker{ca: r.sandbox, hosts: map[string]bool{"api.test": true},
		grants: map[string]fakeGrant{ph: {sandbox: "sb1", host: "api.test", value: realValue}}}
	r.proxy = newCredProxy(r.broker, pool(r.upCA))
	r.up = newUpstream(t, r.upCA, "api.test")
	r.upstream = r.up.srv.Listener.Addr().String()
	return r
}

// connect opens a guest connection to the upstream through the proxy and
// returns the guest's raw end (the TLS handshake is the caller's).
func (r *rig) connect(sandbox string) net.Conn {
	r.t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		r.t.Fatal(err)
	}
	defer ln.Close()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		r.t.Fatal(err)
	}
	guest, err := ln.Accept()
	if err != nil {
		r.t.Fatal(err)
	}
	up, err := net.Dial("tcp", r.upstream)
	if err != nil {
		r.t.Fatal(err)
	}
	go r.proxy.serve(guest, up, sandbox)
	r.t.Cleanup(func() { client.Close() })
	return client
}

// dialTLS is the guest opening TLS to sni, trusting the sandbox CA and the
// public CA (as a guest with the bundle lohar installs does).
func (r *rig) dialTLS(sni string, alpn ...string) *tls.Conn {
	r.t.Helper()
	c := tls.Client(r.connect("sb1"), &tls.Config{ServerName: sni, RootCAs: pool(r.sandbox, r.upCA), NextProtos: alpn})
	if err := c.Handshake(); err != nil {
		r.t.Fatalf("guest TLS handshake: %v", err)
	}
	return c
}

func roundTrip(t *testing.T, c net.Conn, raw string) (*http.Response, string) {
	t.Helper()
	if _, err := io.WriteString(c, raw); err != nil {
		t.Fatalf("write request: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func issuer(c *tls.Conn) string {
	return c.ConnectionState().PeerCertificates[0].Issuer.CommonName
}

// --- tests ---

// TestCredSubstitutesHeaderValue: a placeholder in a header value on a
// connection to a granted host reaches the upstream as the real value, over a
// connection the guest accepted from the sandbox CA.
func TestCredSubstitutesHeaderValue(t *testing.T) {
	r := newRig(t)
	c := r.dialTLS("api.test")
	if got := issuer(c); got != "bhatti sandbox CA" {
		t.Fatalf("guest saw a leaf issued by %q, want the sandbox CA", got)
	}
	resp, body := roundTrip(t, c, "GET /v1/user HTTP/1.1\r\nHost: api.test\r\nAuthorization: Bearer "+r.ph+"\r\nX-Other: keep\r\n\r\n")
	if resp.StatusCode != 200 || body != "ok /v1/user" {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
	seen := r.up.seen()
	if len(seen) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(seen))
	}
	if got := seen[0].header.Get("Authorization"); got != "Bearer "+realValue {
		t.Fatalf("upstream Authorization = %q, want the real value", got)
	}
	if seen[0].header.Get("X-Other") != "keep" || seen[0].header.Get("User-Agent") != "" {
		t.Fatalf("other headers altered: %v", seen[0].header)
	}
}

// TestCredBasicAuthCredential: git-style Basic auth carries the placeholder as
// the password inside base64; it is substituted there too.
func TestCredBasicAuthCredential(t *testing.T) {
	r := newRig(t)
	c := r.dialTLS("api.test")
	cred := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + r.ph))
	resp, _ := roundTrip(t, c, "GET /repo.git/info/refs HTTP/1.1\r\nHost: api.test\r\nAuthorization: Basic "+cred+"\r\n\r\n")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+realValue))
	if got := r.up.seen()[0].header.Get("Authorization"); got != want {
		t.Fatalf("upstream Authorization = %q, want %q", got, want)
	}
}

// TestCredKeepAliveResolvesOnce: requests on one connection share the value
// the first one resolved; a request without a placeholder passes unchanged.
func TestCredKeepAliveResolvesOnce(t *testing.T) {
	r := newRig(t)
	c := r.dialTLS("api.test")
	for i := range 3 {
		if resp, _ := roundTrip(t, c, fmt.Sprintf("GET /%d HTTP/1.1\r\nHost: api.test\r\nAuthorization: Bearer %s\r\n\r\n", i, r.ph)); resp.StatusCode != 200 {
			t.Fatalf("request %d: status %d", i, resp.StatusCode)
		}
	}
	if resp, _ := roundTrip(t, c, "POST /plain HTTP/1.1\r\nHost: api.test\r\nContent-Length: 5\r\n\r\nhello"); resp.StatusCode != 200 {
		t.Fatalf("plain request: status %d", resp.StatusCode)
	}
	seen := r.up.seen()
	if len(seen) != 4 || seen[3].body != "hello" {
		t.Fatalf("upstream saw %+v", seen)
	}
	if _, resolves := r.broker.counts(); resolves != 1 {
		t.Fatalf("resolved %d times on one connection, want 1", resolves)
	}
}

// TestCredPlaceholderOutsideHeadersRefused: a placeholder anywhere but a header
// value gets a 403 and a reported denial; its value is never fetched and no
// complete request reaches the upstream.
func TestCredPlaceholderOutsideHeadersRefused(t *testing.T) {
	big := strings.Repeat("a", 100<<10)
	cases := map[string]func(ph string) string{
		"path":          func(ph string) string { return "GET /keys/" + ph + " HTTP/1.1\r\nHost: api.test\r\n\r\n" },
		"query":         func(ph string) string { return "GET /v1?token=" + ph + " HTTP/1.1\r\nHost: api.test\r\n\r\n" },
		"percent-coded": func(ph string) string { return "GET /v1?t=%62" + ph[1:] + " HTTP/1.1\r\nHost: api.test\r\n\r\n" },
		"host":          func(ph string) string { return "GET / HTTP/1.1\r\nHost: " + ph + ".api.test\r\n\r\n" },
		"body": func(ph string) string {
			b := `{"api_key":"` + ph + `"}`
			return fmt.Sprintf("POST /v1 HTTP/1.1\r\nHost: api.test\r\nContent-Length: %d\r\n\r\n%s", len(b), b)
		},
		"body-after-100KiB": func(ph string) string {
			b := big + ph
			return fmt.Sprintf("POST /v1 HTTP/1.1\r\nHost: api.test\r\nContent-Length: %d\r\n\r\n%s", len(b), b)
		},
		"chunked-split": func(ph string) string {
			return "POST /v1 HTTP/1.1\r\nHost: api.test\r\nTransfer-Encoding: chunked\r\n\r\n" +
				fmt.Sprintf("%x\r\n%s\r\n", 15, ph[:15]) + fmt.Sprintf("%x\r\n%s\r\n", len(ph)-15, ph[15:]) + "0\r\n\r\n"
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			c := r.dialTLS("api.test")
			resp, body := roundTrip(t, c, mk(r.ph))
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status %d (%q), want 403", resp.StatusCode, body)
			}
			if rep := r.broker.waitReport(t); rep.Placeholder != r.ph || rep.Sandbox != "sb1" || rep.Host != "api.test" || rep.Reason == "" {
				t.Fatalf("reported %+v", rep)
			}
			if _, resolves := r.broker.counts(); resolves != 0 {
				t.Fatalf("value resolved %d times for a refused request", resolves)
			}
			for _, s := range r.up.seen() {
				if strings.Contains(s.uri+s.host+s.body, r.ph) || strings.Contains(s.body, "api_key") {
					t.Fatalf("upstream got the request: %+v", s)
				}
			}
		})
	}
}

// TestCredHostHeaderMustBeInterceptedHost: the value goes only to the host the
// upstream proved it is; a different Host header (fronting) is refused.
func TestCredHostHeaderMustBeInterceptedHost(t *testing.T) {
	r := newRig(t)
	c := r.dialTLS("api.test")
	resp, _ := roundTrip(t, c, "GET / HTTP/1.1\r\nHost: evil.test\r\nAuthorization: Bearer "+r.ph+"\r\n\r\n")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d, want 403", resp.StatusCode)
	}
	r.broker.waitReport(t)
	if len(r.up.seen()) != 0 {
		t.Fatal("upstream got a fronted request")
	}
	if _, resolves := r.broker.counts(); resolves != 0 {
		t.Fatal("value resolved for a fronted request")
	}
}

// TestCredBrokerRefusalIs403: a placeholder the broker won't resolve (revoked,
// expired, another sandbox's) is answered 403; nothing is forwarded.
func TestCredBrokerRefusalIs403(t *testing.T) {
	r := newRig(t)
	r.broker.refuseResolve = "grant revoked"
	c := r.dialTLS("api.test")
	resp, body := roundTrip(t, c, "GET / HTTP/1.1\r\nHost: api.test\r\nAuthorization: Bearer "+r.ph+"\r\n\r\n")
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "grant revoked") {
		t.Fatalf("got %d %q, want 403 naming the refusal", resp.StatusCode, body)
	}
	if len(r.up.seen()) != 0 {
		t.Fatal("upstream got a request whose credential was refused")
	}
}

// TestCredUpstreamMustVerify: netd only sends a value to an upstream whose
// certificate chains to the system roots for the granted name — a server that
// can't prove it gets nothing, and the value is never even fetched.
func TestCredUpstreamMustVerify(t *testing.T) {
	cases := map[string]func(r *rig){
		"untrusted CA": func(r *rig) {
			r.up = newUpstream(t, newTestCA(t, "rogue CA"), "api.test")
		},
		"wrong name": func(r *rig) {
			r.up = newUpstream(t, r.upCA, "other.test")
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			setup(r)
			r.upstream = r.up.srv.Listener.Addr().String()
			c := r.dialTLS("api.test")
			resp, body := roundTrip(t, c, "GET / HTTP/1.1\r\nHost: api.test\r\nAuthorization: Bearer "+r.ph+"\r\n\r\n")
			if resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("status %d (%q), want 502", resp.StatusCode, body)
			}
			if len(r.up.seen()) != 0 {
				t.Fatal("an unverified upstream received the request")
			}
			if _, resolves := r.broker.counts(); resolves != 0 {
				t.Fatal("value fetched for an unverified upstream")
			}
		})
	}
}

// TestCredOnlyGrantedHostsIntercepted: TLS to a host no grant names, or from a
// client that won't speak HTTP/1.1, or that isn't TLS at all, is spliced
// untouched — the guest sees the upstream's own certificate.
func TestCredOnlyGrantedHostsIntercepted(t *testing.T) {
	t.Run("ungranted host", func(t *testing.T) {
		r := newRig(t)
		r.up = newUpstream(t, r.upCA, "other.test")
		r.upstream = r.up.srv.Listener.Addr().String()
		c := r.dialTLS("other.test")
		if got := issuer(c); got != "public CA" {
			t.Fatalf("guest saw issuer %q: an ungranted host was intercepted", got)
		}
		resp, _ := roundTrip(t, c, "GET / HTTP/1.1\r\nHost: other.test\r\nAuthorization: Bearer "+r.ph+"\r\n\r\n")
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		if got := r.up.seen()[0].header.Get("Authorization"); got != "Bearer "+r.ph {
			t.Fatalf("ungranted host got %q, want the placeholder untouched", got)
		}
		if _, resolves := r.broker.counts(); resolves != 0 {
			t.Fatal("value resolved for an ungranted host")
		}
	})
	t.Run("h2-only client", func(t *testing.T) {
		r := newRig(t)
		r.up = newUpstream(t, r.upCA, "api.test", "h2", "http/1.1")
		r.upstream = r.up.srv.Listener.Addr().String()
		c := r.dialTLS("api.test", "h2")
		if got := issuer(c); got != "public CA" {
			t.Fatalf("guest saw issuer %q: an h2-only client was intercepted", got)
		}
	})
	t.Run("other sandbox", func(t *testing.T) {
		r := newRig(t)
		c := tls.Client(r.connect("sb2"), &tls.Config{ServerName: "api.test", RootCAs: pool(r.sandbox, r.upCA)})
		if err := c.Handshake(); err != nil {
			t.Fatal(err)
		}
		if got := issuer(c); got != "public CA" {
			t.Fatalf("issuer %q: a sandbox without the grant was intercepted", got)
		}
	})
	t.Run("not TLS", func(t *testing.T) {
		r := newRig(t)
		// A plaintext server on the upstream address: netd must replay what it
		// peeked and splice, not eat the bytes.
		plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			io.WriteString(w, "plain "+req.URL.Path)
		}))
		t.Cleanup(plain.Close)
		r.upstream = plain.Listener.Addr().String()
		c := r.connect("sb1")
		resp, body := roundTrip(t, c, "GET /x HTTP/1.1\r\nHost: api.test\r\n\r\n")
		if resp.StatusCode != 200 || body != "plain /x" {
			t.Fatalf("got %d %q", resp.StatusCode, body)
		}
	})
}

// TestBodyGuard: a clean body passes through byte for byte at any read size; a
// placeholder is caught even when it arrives one byte at a time, and none of
// it is released first.
func TestBodyGuard(t *testing.T) {
	ph, _ := gateway.NewPlaceholder()
	clean := bytes.Repeat([]byte("bhatti_sec_ almost "), 5000)
	for _, step := range []int{1, 7, 34, 35, 4096, 1 << 20} {
		var out bytes.Buffer
		if _, err := io.Copy(&out, &bodyGuard{r: &chunky{data: clean, step: step}}); err != nil {
			t.Fatalf("step %d: %v", step, err)
		}
		if !bytes.Equal(out.Bytes(), clean) {
			t.Fatalf("step %d: body altered", step)
		}
	}
	dirty := append(append(bytes.Repeat([]byte("x"), 1000), ph...), "tail"...)
	for _, step := range []int{1, 3, 34, 35, 999, 1035} {
		g := &bodyGuard{r: &chunky{data: dirty, step: step}}
		var out bytes.Buffer
		_, err := io.Copy(&out, g)
		if err == nil || g.found != ph {
			t.Fatalf("step %d: err=%v found=%q, want the placeholder caught", step, err, g.found)
		}
		if bytes.Contains(out.Bytes(), []byte("bhatti_sec_")) {
			t.Fatalf("step %d: released part of the placeholder: %q", step, out.Bytes()[max(0, out.Len()-40):])
		}
	}
}

type chunky struct {
	data []byte
	step int
}

func (c *chunky) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	n := min(c.step, len(p), len(c.data))
	copy(p, c.data[:n])
	c.data = c.data[n:]
	return n, nil
}
