// Package broker is the daemon's credential broker: the one component that
// decrypts secrets for credential substitution. Each owner's netd parses
// untrusted guest traffic, so it holds nothing: per intercepted connection it
// asks the broker, over a socket only it can reach, for a leaf certificate
// (to terminate the guest's TLS toward a granted host) and for the value
// behind a placeholder. The broker answers only what a live grant allows —
// this sandbox, this secret, this host, not revoked, not expired — and audits
// every use and refusal as secret.used / secret.denied events.
package broker

import (
	"bufio"
	"database/sql"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/gateway"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

// Store is what the broker reads. *store.Store implements it.
type Store interface {
	GetSecretGrantByPlaceholder(placeholder string) (*store.SecretGrant, error)
	ListSandboxSecretGrants(sandboxID string) ([]store.SecretGrant, error)
	GetSecretValue(userID, name string) ([]byte, error)
	GetSandboxCA(sandboxID string) (*store.SandboxCA, error)
	GetSandboxByEngineID(engineID string) (*store.Sandbox, error)
}

// Config wires the broker to the daemon.
type Config struct {
	Store Store
	// Decrypt opens an age-encrypted value (a secret, a CA key) with the
	// daemon's key.
	Decrypt func(ciphertext []byte) ([]byte, error)
	// Record receives the audit events; it must not block.
	Record func(store.Event)
	// Now is the clock; nil = time.Now.
	Now func() time.Time
}

// Broker answers netd's credential requests.
type Broker struct {
	cfg    Config
	events eventLimiter
}

// New returns a broker for cfg.
func New(cfg Config) *Broker {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Broker{cfg: cfg, events: eventLimiter{buckets: map[string]*bucket{}}}
}

const (
	connIdle  = 30 * time.Second // netd sends one request per connection; this only reaps stuck ones
	leafLife  = 24 * time.Hour
	maxReason = 200
)

// Serve answers requests from userID's netd on ln until ln is closed. It
// implements engine.CredentialBroker.
func (b *Broker) Serve(ln net.Listener, userID string) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			slog.Warn("broker.accept", "user", userID, "error", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		go b.serveConn(conn, userID)
	}
}

func (b *Broker) serveConn(conn net.Conn, userID string) {
	defer conn.Close()
	br := bufio.NewReaderSize(conn, gateway.BrokerMaxRequest)
	for {
		_ = conn.SetDeadline(time.Now().Add(connIdle))
		var req gateway.BrokerRequest
		if err := gateway.ReadBrokerMessage(br, &req); err != nil {
			return
		}
		if err := gateway.WriteBrokerMessage(conn, b.handle(userID, req)); err != nil {
			return
		}
	}
}

func (b *Broker) handle(userID string, req gateway.BrokerRequest) gateway.BrokerResponse {
	// A netd of a sandbox with no owner has nothing to ask for.
	if userID == "" || req.Sandbox == "" {
		return refuse("no owner")
	}
	switch req.Op {
	case gateway.BrokerResolve:
		return b.resolve(userID, req)
	case gateway.BrokerCert:
		return b.cert(userID, req)
	case gateway.BrokerReport:
		b.report(userID, req)
		return gateway.BrokerResponse{}
	}
	return refuse("unknown op")
}

func refuse(reason string) gateway.BrokerResponse { return gateway.BrokerResponse{Error: reason} }

// sandboxFor maps the identity netd names to the server's sandbox ID: the
// server passes its own ID to the engine for the sandboxes it creates, but a
// fork or restore is created inside the engine and netd knows it by the
// engine's ID.
func (b *Broker) sandboxFor(userID, ref string) string {
	if sb, err := b.cfg.Store.GetSandboxByEngineID(ref); err == nil && sb.CreatedBy == userID {
		return sb.ID
	}
	return ref
}

func (b *Broker) resolve(userID string, req gateway.BrokerRequest) gateway.BrokerResponse {
	sandboxID := b.sandboxFor(userID, req.Sandbox)
	host := normalizeHost(req.Host)
	if !gateway.IsPlaceholder(req.Placeholder) || host == "" {
		return b.deny(userID, sandboxID, host, nil, "malformed request")
	}
	g, err := b.cfg.Store.GetSecretGrantByPlaceholder(req.Placeholder)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		slog.Error("broker.resolve", "sandbox", sandboxID, "error", err)
		return refuse("broker error")
	}
	// Another owner's placeholder is as unknown as a made-up one: its
	// existence (and secret name) is that owner's business.
	if err != nil || g.UserID != userID {
		return b.deny(userID, sandboxID, host, nil, "unknown placeholder")
	}
	now := b.cfg.Now()
	switch {
	case g.SandboxID != sandboxID:
		return b.deny(userID, sandboxID, host, g, "placeholder was granted to another sandbox")
	case g.RevokedAt != nil:
		return b.deny(userID, sandboxID, host, g, "grant revoked")
	case g.ExpiresAt != nil && !now.Before(*g.ExpiresAt):
		return b.deny(userID, sandboxID, host, g, "grant expired")
	case !hostGranted(g.Hosts, host):
		return b.deny(userID, sandboxID, host, g, "host not granted")
	}
	ciphertext, err := b.cfg.Store.GetSecretValue(g.UserID, g.SecretName)
	if err != nil {
		return b.deny(userID, sandboxID, host, g, "secret not found")
	}
	value, err := b.cfg.Decrypt(ciphertext)
	if err != nil {
		slog.Error("broker.decrypt", "sandbox", sandboxID, "secret", g.SecretName, "error", err)
		return b.deny(userID, sandboxID, host, g, "secret unreadable")
	}
	b.record("secret.used", userID, sandboxID, host, g, "")
	var ttl int64
	if g.ExpiresAt != nil {
		// Round up: a value must not outlive its grant by a truncated second.
		ttl = int64((g.ExpiresAt.Sub(now) + time.Second - 1) / time.Second)
	}
	return gateway.BrokerResponse{Value: string(value), TTLSeconds: ttl}
}

// cert issues a leaf for host from the sandbox's CA, only while a live grant
// of the sandbox covers host. Refusals aren't audited: netd asks for every
// TLS connection to :443, granted or not.
func (b *Broker) cert(userID string, req gateway.BrokerRequest) gateway.BrokerResponse {
	sandboxID := b.sandboxFor(userID, req.Sandbox)
	host := normalizeHost(req.Host)
	if !validHostname(host) {
		return refuse("invalid host")
	}
	grants, err := b.cfg.Store.ListSandboxSecretGrants(sandboxID)
	if err != nil {
		slog.Error("broker.cert", "sandbox", sandboxID, "error", err)
		return refuse("broker error")
	}
	now := b.cfg.Now()
	covered := false
	for i := range grants {
		if g := &grants[i]; g.UserID == userID && g.Live(now) && hostGranted(g.Hosts, host) {
			covered = true
			break
		}
	}
	if !covered {
		return refuse("no live grant covers host")
	}
	ca, err := b.cfg.Store.GetSandboxCA(sandboxID)
	if err != nil || ca.UserID != userID {
		return refuse("sandbox has no CA")
	}
	caKey, err := b.cfg.Decrypt(ca.KeyEnc)
	if err != nil {
		slog.Error("broker.cert", "sandbox", sandboxID, "error", err)
		return refuse("broker error")
	}
	certPEM, keyPEM, err := issueLeaf([]byte(ca.CertPEM), caKey, host, now, leafLife)
	if err != nil {
		slog.Error("broker.cert", "sandbox", sandboxID, "host", host, "error", err)
		return refuse("broker error")
	}
	return gateway.BrokerResponse{CertPEM: string(certPEM), KeyPEM: string(keyPEM)}
}

// report records a refusal netd made itself.
func (b *Broker) report(userID string, req gateway.BrokerRequest) {
	sandboxID := b.sandboxFor(userID, req.Sandbox)
	var g *store.SecretGrant
	if gateway.IsPlaceholder(req.Placeholder) {
		if got, err := b.cfg.Store.GetSecretGrantByPlaceholder(req.Placeholder); err == nil && got.UserID == userID {
			g = got
		}
	}
	reason := req.Reason
	if len(reason) > maxReason {
		reason = reason[:maxReason]
	}
	if reason == "" {
		reason = "refused by netd"
	}
	b.deny(userID, sandboxID, normalizeHost(req.Host), g, reason)
}

func (b *Broker) deny(userID, sandboxID, host string, g *store.SecretGrant, reason string) gateway.BrokerResponse {
	b.record("secret.denied", userID, sandboxID, host, g, reason)
	return refuse(reason)
}

// record emits an audit event (never a value). A guest can trigger these as
// fast as it can open connections, so each sandbox gets a budget; what's
// dropped is counted on the next event that gets through.
func (b *Broker) record(typ, userID, sandboxID, host string, g *store.SecretGrant, reason string) {
	ok, dropped := b.events.allow(sandboxID, b.cfg.Now())
	if !ok {
		return
	}
	meta := map[string]any{"sandbox": sandboxID, "host": host}
	if g != nil {
		meta["secret"] = g.SecretName
		meta["grant"] = g.ID
	}
	if reason != "" {
		meta["reason"] = reason
	}
	if dropped > 0 {
		meta["dropped"] = dropped
	}
	if typ == "secret.denied" {
		slog.Info(typ, "sandbox", sandboxID, "host", host, "secret", meta["secret"], "reason", reason)
	} else {
		slog.Debug(typ, "sandbox", sandboxID, "host", host, "secret", meta["secret"])
	}
	if b.cfg.Record != nil {
		b.cfg.Record(store.Event{Type: typ, UserID: userID, SandboxID: sandboxID, Meta: meta})
	}
}

func hostGranted(patterns []string, host string) bool {
	for _, s := range patterns {
		if p, err := gateway.ParseHostPattern(s); err == nil && p.Match(host) {
			return true
		}
	}
	return false
}

func normalizeHost(h string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))
}

// validHostname accepts what a certificate's DNS name may hold: LDH labels
// (and underscores, which real hosts use), at most 253 bytes.
func validHostname(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for label := range strings.SplitSeq(h, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for _, c := range []byte(label) {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}

const (
	eventRate   = 5.0 // per second, per sandbox
	eventBurst  = 50
	bucketLimit = 4096
)

type eventLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens  float64
	last    time.Time
	dropped int
}

// allow takes a token for key, reporting how many events were dropped since
// the last one allowed.
func (l *eventLimiter) allow(key string, now time.Time) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	bk := l.buckets[key]
	if bk == nil {
		if len(l.buckets) >= bucketLimit {
			for k, old := range l.buckets {
				if now.Sub(old.last) > time.Minute {
					delete(l.buckets, k)
				}
			}
		}
		bk = &bucket{tokens: eventBurst, last: now}
		l.buckets[key] = bk
	}
	bk.tokens = min(eventBurst, bk.tokens+now.Sub(bk.last).Seconds()*eventRate)
	bk.last = now
	if bk.tokens < 1 {
		bk.dropped++
		return false, 0
	}
	bk.tokens--
	dropped := bk.dropped
	bk.dropped = 0
	return true, dropped
}
