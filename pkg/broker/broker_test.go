package broker

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/gateway"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type sink struct {
	mu     sync.Mutex
	events []store.Event
}

func (s *sink) Record(e store.Event) {
	s.mu.Lock()
	s.events = append(s.events, e)
	s.mu.Unlock()
}

func (s *sink) all() []store.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.Event(nil), s.events...)
}

func (s *sink) last(t *testing.T) store.Event {
	t.Helper()
	all := s.all()
	if len(all) == 0 {
		t.Fatal("no event recorded")
	}
	return all[len(all)-1]
}

const secretValue = "ghp_the-real-token"

type fixture struct {
	t      *testing.T
	st     *store.Store
	clock  *clock
	events *sink
	broker *Broker
	caPEM  []byte // sb1's CA
	ph     map[string]string
}

// newFixture: user u1 owns sandboxes sb1 (engine eng1) and sb2, a secret
// GH_TOKEN, and grants with different fates; user u2 owns one grant of its own.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	f := &fixture{t: t, st: st, clock: &clock{now: now}, events: &sink{}, ph: map[string]string{}}
	f.broker = New(Config{
		Store:   st,
		Decrypt: func(b []byte) ([]byte, error) { return b, nil }, // values stored in the clear for the test
		Record:  f.events.Record,
		Now:     f.clock.Now,
	})
	for _, sb := range []store.Sandbox{
		{ID: "sb1", Name: "dev", EngineID: "eng1", Status: "running", CreatedBy: "u1", CreatedAt: now},
		{ID: "sb2", Name: "other", EngineID: "eng2", Status: "running", CreatedBy: "u1", CreatedAt: now},
		{ID: "sbx", Name: "dev", EngineID: "engx", Status: "running", CreatedBy: "u2", CreatedAt: now},
	} {
		if err := st.CreateSandbox(sb); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetSecret("u1", "GH_TOKEN", []byte(secretValue)); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSecret("u2", "GH_TOKEN", []byte("u2-value")); err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := NewSandboxCA("sb1", now)
	if err != nil {
		t.Fatal(err)
	}
	f.caPEM = certPEM
	for _, sb := range []string{"sb1", "sb2"} {
		if err := st.PutSandboxCA(store.SandboxCA{SandboxID: sb, UserID: "u1", CertPEM: string(certPEM), KeyEnc: keyPEM}); err != nil {
			t.Fatal(err)
		}
	}
	past, soon := now.Add(-time.Minute), now.Add(90*time.Second)
	f.grant("live", "u1", "sb1", "GH_TOKEN", []string{"api.github.com", "*.githubusercontent.com"}, nil)
	f.grant("expiring", "u1", "sb1", "GH_TOKEN", []string{"api.github.com"}, &soon)
	f.grant("expired", "u1", "sb2", "GH_TOKEN", []string{"expired.example"}, &past)
	f.grant("revoked", "u1", "sb2", "GH_TOKEN", []string{"revoked.example"}, nil)
	if err := st.RevokeSecretGrant("u1", "revoked", now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	f.grant("nosecret", "u1", "sb1", "DELETED", []string{"api.github.com"}, nil)
	f.grant("foreign", "u2", "sbx", "GH_TOKEN", []string{"api.github.com"}, nil)
	return f
}

func (f *fixture) grant(id, user, sandbox, secret string, hosts []string, expires *time.Time) {
	f.t.Helper()
	ph, err := gateway.NewPlaceholder()
	if err != nil {
		f.t.Fatal(err)
	}
	f.ph[id] = ph
	if err := f.st.CreateSecretGrant(store.SecretGrant{
		ID: id, UserID: user, SecretName: secret, SandboxID: sandbox, Hosts: hosts,
		Placeholder: ph, CreatedAt: f.clock.Now(), ExpiresAt: expires,
	}); err != nil {
		f.t.Fatal(err)
	}
}

// serve starts the broker for owner on a fresh socket and returns a client.
func (f *fixture) serve(owner string) *gateway.BrokerClient {
	f.t.Helper()
	dir, err := os.MkdirTemp("", "brk") // short: AF_UNIX paths are capped
	if err != nil {
		f.t.Fatal(err)
	}
	path := filepath.Join(dir, "b.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { ln.Close(); os.RemoveAll(dir) })
	go f.broker.Serve(ln, owner)
	return &gateway.BrokerClient{Path: path}
}

func (f *fixture) resolve(c *gateway.BrokerClient, sandbox, ph, host string) (gateway.BrokerResponse, error) {
	return c.Do(context.Background(), gateway.BrokerRequest{Op: gateway.BrokerResolve, Sandbox: sandbox, Placeholder: ph, Host: host})
}

func refusalReason(err error) string {
	var r *gateway.BrokerRefusal
	if errors.As(err, &r) {
		return r.Reason
	}
	return ""
}

// TestResolveChecksTheGrant: a value comes back only for the grant's own
// sandbox, a host its patterns match, while it is neither revoked nor expired;
// everything else is refused and audited.
func TestResolveChecksTheGrant(t *testing.T) {
	f := newFixture(t)
	c := f.serve("u1")
	ok := []struct {
		name, sandbox, ph, host string
		ttl                     int64
	}{
		{"exact host", "sb1", f.ph["live"], "api.github.com", 0},
		{"host case and dot", "sb1", f.ph["live"], "API.GitHub.com.", 0},
		{"wildcard host", "sb1", f.ph["live"], "raw.githubusercontent.com", 0},
		{"engine identity", "eng1", f.ph["live"], "api.github.com", 0},
		{"expiring grant carries its ttl", "sb1", f.ph["expiring"], "api.github.com", 90},
	}
	for _, tc := range ok {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := f.resolve(c, tc.sandbox, tc.ph, tc.host)
			if err != nil || resp.Value != secretValue || resp.TTLSeconds != tc.ttl {
				t.Fatalf("got %+v err=%v, want the value with ttl %d", resp, err, tc.ttl)
			}
			ev := f.events.last(t)
			if ev.Type != "secret.used" || ev.UserID != "u1" || ev.SandboxID != "sb1" || ev.Meta["secret"] != "GH_TOKEN" {
				t.Fatalf("event %+v", ev)
			}
		})
	}
	refused := []struct {
		name, sandbox, ph, host, reason string
		secretNamed                     bool
	}{
		{"wrong host", "sb1", f.ph["live"], "evil.example", "host not granted", true},
		{"wildcard does not match apex", "sb1", f.ph["live"], "githubusercontent.com", "host not granted", true},
		{"wrong sandbox", "sb2", f.ph["live"], "api.github.com", "placeholder was granted to another sandbox", true},
		{"revoked", "sb2", f.ph["revoked"], "revoked.example", "grant revoked", true},
		{"expired", "sb2", f.ph["expired"], "expired.example", "grant expired", true},
		{"unknown placeholder", "sb1", "bhatti_sec_aaaaaaaaaaaaaaaaaaaaaaaa", "api.github.com", "unknown placeholder", false},
		{"another owner's placeholder", "sb1", f.ph["foreign"], "api.github.com", "unknown placeholder", false},
		{"not a placeholder", "sb1", "ghp_whatever", "api.github.com", "malformed request", false},
		{"secret gone", "sb1", f.ph["nosecret"], "api.github.com", "secret not found", true},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := f.resolve(c, tc.sandbox, tc.ph, tc.host)
			if got := refusalReason(err); got != tc.reason || resp.Value != "" {
				t.Fatalf("got %+v err=%v, want refusal %q", resp, err, tc.reason)
			}
			ev := f.events.last(t)
			if ev.Type != "secret.denied" || ev.Meta["reason"] != tc.reason {
				t.Fatalf("event %+v", ev)
			}
			if _, named := ev.Meta["secret"]; named != tc.secretNamed {
				t.Fatalf("event names the secret = %v, want %v: %+v", named, tc.secretNamed, ev)
			}
		})
	}
	for _, ev := range f.events.all() {
		for k, v := range ev.Meta {
			if s, _ := v.(string); strings.Contains(s, secretValue) {
				t.Fatalf("event %s carries the value in %q", ev.Type, k)
			}
		}
	}
}

// TestResolveAfterExpiry: the clock passing expires_at turns a live grant away.
func TestResolveAfterExpiry(t *testing.T) {
	f := newFixture(t)
	c := f.serve("u1")
	if _, err := f.resolve(c, "sb1", f.ph["expiring"], "api.github.com"); err != nil {
		t.Fatalf("before expiry: %v", err)
	}
	f.clock.advance(91 * time.Second)
	if _, err := f.resolve(c, "sb1", f.ph["expiring"], "api.github.com"); refusalReason(err) != "grant expired" {
		t.Fatalf("after expiry: err=%v, want grant expired", err)
	}
}

// TestCertOnlyForLiveGrantedHosts: a leaf, signed by the sandbox's CA for
// exactly the asked host, exists only while a live grant covers that host.
func TestCertOnlyForLiveGrantedHosts(t *testing.T) {
	f := newFixture(t)
	c := f.serve("u1")
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(f.caPEM)
	cert := func(sandbox, host string) (gateway.BrokerResponse, error) {
		return c.Do(context.Background(), gateway.BrokerRequest{Op: gateway.BrokerCert, Sandbox: sandbox, Host: host})
	}
	for _, host := range []string{"api.github.com", "objects.githubusercontent.com"} {
		resp, err := cert("sb1", host)
		if err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		pair, err := tls.X509KeyPair([]byte(resp.CertPEM), []byte(resp.KeyPEM))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pair.Leaf.Verify(x509.VerifyOptions{DNSName: host, Roots: roots, CurrentTime: f.clock.Now()}); err != nil {
			t.Fatalf("%s: leaf doesn't verify against the sandbox CA: %v", host, err)
		}
		if life := pair.Leaf.NotAfter.Sub(f.clock.Now()); life > 25*time.Hour || life < 23*time.Hour {
			t.Fatalf("%s: leaf valid for %s, want about a day", host, life)
		}
		if _, err := pair.Leaf.Verify(x509.VerifyOptions{DNSName: "evil.example", Roots: roots, CurrentTime: f.clock.Now()}); err == nil {
			t.Fatal("leaf verifies for a host it wasn't issued for")
		}
	}
	for _, tc := range []struct{ name, sandbox, host string }{
		{"ungranted host", "sb1", "evil.example"},
		{"only a revoked grant", "sb2", "revoked.example"},
		{"only an expired grant", "sb2", "expired.example"},
		{"another sandbox's grant", "sb2", "api.github.com"},
		{"another owner's sandbox", "sbx", "api.github.com"},
		{"not a hostname", "sb1", "api.github.com\r\nx"},
	} {
		if resp, err := cert(tc.sandbox, tc.host); err == nil || resp.CertPEM != "" {
			t.Errorf("%s: issued a certificate", tc.name)
		}
	}
}

// TestBrokerSocketIsOwnerScoped: a socket serves one owner; another owner's
// grants, and an ownerless netd, get nothing.
func TestBrokerSocketIsOwnerScoped(t *testing.T) {
	f := newFixture(t)
	u2 := f.serve("u2")
	if _, err := f.resolve(u2, "sb1", f.ph["live"], "api.github.com"); refusalReason(err) != "unknown placeholder" {
		t.Fatalf("u2's socket resolved u1's grant: %v", err)
	}
	if _, err := u2.Do(context.Background(), gateway.BrokerRequest{Op: gateway.BrokerCert, Sandbox: "sb1", Host: "api.github.com"}); err == nil {
		t.Fatal("u2's socket issued a certificate for u1's sandbox")
	}
	unowned := f.serve("")
	if _, err := f.resolve(unowned, "sb1", f.ph["live"], "api.github.com"); refusalReason(err) != "no owner" {
		t.Fatalf("an ownerless socket resolved a grant: %v", err)
	}
}

// TestOversizeRequestDropped: a request line past the bound gets no answer.
func TestOversizeRequestDropped(t *testing.T) {
	f := newFixture(t)
	c := f.serve("u1")
	conn, err := net.Dial("unix", c.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	big := `{"op":"resolve","sandbox":"sb1","host":"` + strings.Repeat("a", gateway.BrokerMaxRequest) + "\"}\n"
	go conn.Write([]byte(big))
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if line, err := bufio.NewReader(conn).ReadString('\n'); err == nil {
		t.Fatalf("answered an oversize request: %q", line)
	}
}

// TestReportRecordsDenial: netd's own refusals are audited, naming the secret
// only for the owner's own placeholders.
func TestReportRecordsDenial(t *testing.T) {
	f := newFixture(t)
	c := f.serve("u1")
	report := func(ph string) store.Event {
		t.Helper()
		if _, err := c.Do(context.Background(), gateway.BrokerRequest{Op: gateway.BrokerReport, Sandbox: "eng1",
			Placeholder: ph, Host: "api.github.com", Reason: "placeholder in request body"}); err != nil {
			t.Fatal(err)
		}
		return f.events.last(t)
	}
	ev := report(f.ph["live"])
	if ev.Type != "secret.denied" || ev.SandboxID != "sb1" || ev.Meta["secret"] != "GH_TOKEN" || ev.Meta["grant"] != "live" ||
		ev.Meta["reason"] != "placeholder in request body" {
		t.Fatalf("event %+v", ev)
	}
	if ev := report(f.ph["foreign"]); ev.Meta["secret"] != nil {
		t.Fatalf("report named another owner's secret: %+v", ev)
	}
}

// TestDenialEventsRateLimited: a guest hammering bad placeholders can't flood
// the event log; what was dropped is counted on the next event let through.
func TestDenialEventsRateLimited(t *testing.T) {
	f := newFixture(t)
	c := f.serve("u1")
	for range 200 {
		f.resolve(c, "sb1", "bhatti_sec_aaaaaaaaaaaaaaaaaaaaaaaa", "api.github.com")
	}
	if n := len(f.events.all()); n > eventBurst {
		t.Fatalf("%d events recorded for one sandbox in an instant, want at most %d", n, eventBurst)
	}
	f.clock.advance(10 * time.Second)
	f.resolve(c, "sb1", "bhatti_sec_aaaaaaaaaaaaaaaaaaaaaaaa", "api.github.com")
	if d, _ := f.events.last(t).Meta["dropped"].(int); d != 200-eventBurst {
		t.Fatalf("dropped = %v, want %d", f.events.last(t).Meta["dropped"], 200-eventBurst)
	}
}

// TestNewSandboxCA: the CA signs leaves but can't mint intermediates.
func TestNewSandboxCA(t *testing.T) {
	now := time.Now()
	certPEM, keyPEM, err := NewSandboxCA("test", now)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := pem.Decode(certPEM)
	ca, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !ca.IsCA || !ca.MaxPathLenZero || ca.MaxPathLen != 0 || ca.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Fatalf("CA constraints: IsCA=%v MaxPathLenZero=%v usage=%v", ca.IsCA, ca.MaxPathLenZero, ca.KeyUsage)
	}
	leafPEM, _, err := issueLeaf(certPEM, keyPEM, "api.example.com", now, leafLife)
	if err != nil {
		t.Fatal(err)
	}
	lb, _ := pem.Decode(leafPEM)
	leaf, err := x509.ParseCertificate(lb.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if leaf.IsCA || len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "api.example.com" {
		t.Fatalf("leaf: IsCA=%v names=%v", leaf.IsCA, leaf.DNSNames)
	}
}
