package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
	"github.com/sahil-shubham/bhatti/pkg/broker"
	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/gateway"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

// Credential substitution, server side. A grant lets one sandbox use one
// secret toward named hosts without the guest ever holding the value: the
// guest gets a placeholder, and the owner's netd swaps in the value — in
// request headers, on TLS it terminated for those hosts with the sandbox's
// own CA — after asking the broker (pkg/broker), which checks the grant on
// every connection. Revoking a grant stops new connections at once; a
// connection already open keeps the value it resolved until it closes.

// StartCredentialBroker attaches the credential broker to an engine whose
// sandbox network can substitute credentials. Call after StartEventRecorder:
// the broker records secret.used / secret.denied.
func (s *Server) StartCredentialBroker() {
	h, ok := s.engine.(engine.CredentialBrokerHost)
	if !ok {
		return
	}
	h.SetCredentialBroker(broker.New(broker.Config{
		Store:   s.store,
		Decrypt: s.decryptSecret,
		Record:  s.RecordEvent,
	}))
}

// grantView is a grant as the API shows it.
type grantView struct {
	store.SecretGrant
	SandboxName string `json:"sandbox_name,omitempty"`
	Status      string `json:"status"` // active | expired | revoked
}

func viewGrant(g store.SecretGrant, sandboxName string, now time.Time) grantView {
	status := "active"
	switch {
	case g.RevokedAt != nil:
		status = "revoked"
	case !g.Live(now):
		status = "expired"
	}
	return grantView{SecretGrant: g, SandboxName: sandboxName, Status: status}
}

// parseTTL reads a grant lifetime: a Go duration ("90m", "24h") or whole days
// ("7d"). Empty means no expiry.
func parseTTL(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	var d time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 || n > 3650 {
			return 0, fmt.Errorf("invalid ttl %q (want e.g. 90m, 24h or 7d)", s)
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(s); err != nil || d <= 0 {
			return 0, fmt.Errorf("invalid ttl %q (want e.g. 90m, 24h or 7d)", s)
		}
	}
	return d, nil
}

// sandboxEgress is the egress policy a sandbox was created with; nil when the
// record predates it or the sandbox came from a snapshot that didn't carry it.
func sandboxEgress(sb *store.Sandbox) *gateway.NetPolicyWire {
	if len(sb.NetPolicy) == 0 {
		return nil
	}
	var np gateway.NetPolicyWire
	if err := json.Unmarshal(sb.NetPolicy, &np); err != nil {
		return nil
	}
	return &np
}

// grantHosts accepts grants only for a known network policy. Under deny, a
// grant host must already be covered by allow_host; missing/invalid policies
// cannot authorize substitution on an otherwise unreachable destination.
func grantHosts(hosts []string, np *gateway.NetPolicyWire) ([]string, error) {
	if len(hosts) == 0 {
		return nil, errors.New("at least one host is required")
	}
	if np == nil {
		return nil, errors.New("the sandbox has no valid network policy")
	}
	if err := gateway.ValidateWire(*np); err != nil {
		return nil, fmt.Errorf("invalid sandbox network policy: %w", err)
	}
	if np.NoNetwork() {
		return nil, errors.New("the sandbox has no network (egress none)")
	}
	var allow []gateway.HostPattern
	deny := np.Default == "deny"
	if deny {
		for _, h := range np.AllowHosts {
			if p, err := gateway.ParseHostPattern(h); err == nil {
				allow = append(allow, p)
			}
		}
	}
	out := make([]string, 0, len(hosts))
	seen := map[string]bool{}
	for _, h := range hosts {
		p, err := gateway.ParseHostPattern(h)
		if err != nil {
			return nil, fmt.Errorf("host %q: %w", h, err)
		}
		if deny && !coveredBy(allow, p) {
			return nil, fmt.Errorf("host %q is outside the sandbox's egress allow list", h)
		}
		if n := p.String(); !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out, nil
}

func coveredBy(allow []gateway.HostPattern, p gateway.HostPattern) bool {
	for _, a := range allow {
		if a.Covers(p) {
			return true
		}
	}
	return false
}

// mintSandboxCA creates the sandbox's CA and stores it (key encrypted like a
// secret), returning the certificate for the guest's trust store.
func (s *Server) mintSandboxCA(userID, sandboxID, name string) (string, error) {
	certPEM, keyPEM, err := broker.NewSandboxCA("bhatti sandbox "+name, time.Now())
	if err != nil {
		return "", err
	}
	keyEnc, err := s.encryptSecret(keyPEM)
	if err != nil {
		return "", fmt.Errorf("encrypt sandbox CA key: %w", err)
	}
	if err := s.store.PutSandboxCA(store.SandboxCA{SandboxID: sandboxID, UserID: userID, CertPEM: string(certPEM), KeyEnc: keyEnc}); err != nil {
		return "", err
	}
	return string(certPEM), nil
}

// captureSubstitution reads what a copy of sandboxID inherits: the CA its
// guest trusts and each live grant, whose placeholders sit in its environment.
// A fork takes it from its live source; a snapshot keeps it, so every resume
// has it after the source is gone.
func (s *Server) captureSubstitution(userID, sandboxID string) (store.SnapshotSubstitution, error) {
	var sub store.SnapshotSubstitution
	ca, err := s.store.GetSandboxCA(sandboxID)
	switch {
	case err == nil && ca.UserID == userID:
		sub.CA = ca
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return sub, fmt.Errorf("read sandbox CA: %w", err)
	}
	grants, err := s.store.ListSandboxSecretGrants(sandboxID)
	if err != nil {
		return sub, fmt.Errorf("read sandbox grants: %w", err)
	}
	now := time.Now()
	for _, g := range grants {
		if g.UserID == userID && g.Live(now) {
			sub.Grants = append(sub.Grants, store.SnapshotGrant{
				SecretName: g.SecretName, Hosts: g.Hosts, Placeholder: g.Placeholder, ExpiresAt: g.ExpiresAt,
			})
		}
	}
	return sub, nil
}

// inheritSubstitution gives the sandbox to — a fork, or a resume of a
// snapshot — the substitution state its guest came back with: the CA in its
// trust store and, for each placeholder in its environment, a grant of its
// own with the same placeholder (netd can only swap what the guest holds),
// hosts and expiry. The broker resolves a placeholder per sandbox, so a copy
// works for to alone, and is revoked alone. A grant whose secret was deleted
// since is skipped. Failures are logged, not fatal: the clone is already
// running and without a grant just gets no substitution. Returns the grants
// it made.
func (s *Server) inheritSubstitution(userID string, to store.Sandbox, from string, sub store.SnapshotSubstitution) []store.SecretGrant {
	if sub.CA != nil {
		ca := *sub.CA
		ca.SandboxID, ca.UserID, ca.CreatedAt = to.ID, userID, time.Now()
		if err := s.store.PutSandboxCA(ca); err != nil {
			slog.Warn("sandbox.ca_inherit", "from", from, "to", to.ID, "error", err)
		}
	}
	now := time.Now().UTC()
	var made []store.SecretGrant
	for _, g := range sub.Grants {
		if _, err := s.store.GetSecret(userID, g.SecretName); err != nil {
			slog.Info("sandbox.grant_inherit_skipped", "from", from, "to", to.ID, "secret", g.SecretName, "error", err)
			continue
		}
		copied := store.SecretGrant{
			ID: genID(), UserID: userID, SecretName: g.SecretName, SandboxID: to.ID,
			Hosts: g.Hosts, Placeholder: g.Placeholder, CreatedAt: now, ExpiresAt: g.ExpiresAt,
		}
		if err := s.store.CreateSecretGrant(copied); err != nil {
			slog.Warn("sandbox.grant_inherit", "from", from, "to", to.ID, "secret", g.SecretName, "error", err)
			continue
		}
		made = append(made, copied)
	}
	return made
}

// inheritFromSandbox is inheritSubstitution from a live source.
func (s *Server) inheritFromSandbox(userID string, src *store.Sandbox, to store.Sandbox) []store.SecretGrant {
	sub, err := s.captureSubstitution(userID, src.ID)
	if err != nil {
		slog.Warn("sandbox.substitution_inherit", "from", src.ID, "to", to.ID, "error", err)
		return nil
	}
	return s.inheritSubstitution(userID, to, "sandbox:"+src.ID, sub)
}

// liveSnapshotSource stands in for what a snapshot taken before snapshots
// recorded their source never kept: the live source, if it still exists, is
// the best record there is — its image, disk and boot config for the new
// sandbox's record, the CA its guest trusts and its active grants (the resume
// uses those for a memory snapshot only). A source destroyed since leaves
// nothing. Only for snapshots without a record of their own
// (SourceRecordedAt nil): one that has a record is never mixed with the live
// source, whatever the source gained since.
func (s *Server) liveSnapshotSource(userID, sourceID string) (store.SnapshotSource, store.SnapshotSubstitution) {
	src, err := s.store.GetSandboxByID(sourceID)
	if err != nil || src.CreatedBy != userID {
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			slog.Warn("snapshot.source_lookup", "source", sourceID, "error", err)
		}
		return store.SnapshotSource{}, store.SnapshotSubstitution{}
	}
	source := store.SnapshotSource{Image: src.Image, DiskSizeMB: src.DiskSizeMB, HasInit: src.HasInit}
	sub, err := s.captureSubstitution(userID, src.ID)
	if err != nil {
		slog.Warn("snapshot.source_substitution", "source", sourceID, "error", err)
		return source, store.SnapshotSubstitution{}
	}
	return source, sub
}

// discardSubstitution drops a sandbox's grants and CA (destroyed, or its
// create failed after they were minted).
func (s *Server) discardSubstitution(sandboxID string) {
	if err := s.store.DeleteSandboxSecretGrants(sandboxID); err != nil {
		slog.Warn("sandbox.grants_cleanup", "sandbox", sandboxID, "error", err)
	}
	if err := s.store.DeleteSandboxCA(sandboxID); err != nil {
		slog.Warn("sandbox.ca_cleanup", "sandbox", sandboxID, "error", err)
	}
}

// createGrantReq is one create-time grant: the guest's env var named after
// the secret holds the placeholder from boot.
type createGrantReq struct {
	Secret string   `json:"secret"`
	Hosts  []string `json:"hosts"`
}

// newGrant mints a placeholder and records the grant.
func (s *Server) newGrant(userID, secret, sandboxID string, hosts []string, ttl time.Duration) (store.SecretGrant, error) {
	ph, err := gateway.NewPlaceholder()
	if err != nil {
		return store.SecretGrant{}, err
	}
	now := time.Now().UTC()
	g := store.SecretGrant{
		ID: genID(), UserID: userID, SecretName: secret, SandboxID: sandboxID,
		Hosts: hosts, Placeholder: ph, CreatedAt: now,
	}
	if ttl > 0 {
		exp := now.Add(ttl)
		g.ExpiresAt = &exp
	}
	if err := s.store.CreateSecretGrant(g); err != nil {
		return store.SecretGrant{}, err
	}
	return g, nil
}

// recordGrant audits a grant given to a sandbox: minted for it, or (with
// inheritedFrom set) copied from the clone's source.
func (s *Server) recordGrant(g store.SecretGrant, sandboxName, inheritedFrom string) {
	meta := map[string]any{"secret": g.SecretName, "grant": g.ID, "hosts": g.Hosts, "sandbox": g.SandboxID}
	if g.ExpiresAt != nil {
		meta["expires_at"] = g.ExpiresAt.Format(time.RFC3339)
	}
	logAttrs := []any{"secret", g.SecretName, "grant", g.ID, "sandbox", g.SandboxID, "name", sandboxName, "hosts", g.Hosts}
	if inheritedFrom != "" {
		meta["inherited_from"] = inheritedFrom
		logAttrs = append(logAttrs, "inherited_from", inheritedFrom)
	}
	slog.Info("secret.granted", logAttrs...)
	s.RecordEvent(store.Event{Type: "secret.granted", UserID: g.UserID, SandboxID: g.SandboxID, Meta: meta})
}

// createdSandbox is the reply to creating a sandbox, whether by boot, fork or
// snapshot resume: the sandbox and the secret grants it starts with.
type createdSandbox struct {
	store.Sandbox
	SecretGrants []grantView `json:"secret_grants,omitempty"`
}

// announceGrants audits grants given to a new sandbox and returns them as the
// API shows them.
func (s *Server) announceGrants(grants []store.SecretGrant, sandboxName, inheritedFrom string) []grantView {
	now := time.Now()
	views := make([]grantView, 0, len(grants))
	for _, g := range grants {
		s.recordGrant(g, sandboxName, inheritedFrom)
		views = append(views, viewGrant(g, sandboxName, now))
	}
	return views
}

// handleSecretGrantCreate: POST /secrets/{name}/grants
func (s *Server) handleSecretGrantCreate(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		errResp(w, 405, "method not allowed")
		return
	}
	user := UserFromContext(r.Context())
	var req struct {
		Sandbox string   `json:"sandbox"`
		Hosts   []string `json:"hosts"`
		TTL     string   `json:"ttl,omitempty"`
	}
	if err := readJSON(r, &req); err != nil {
		errResp(w, 400, "invalid json: "+err.Error())
		return
	}
	if _, err := s.store.GetSecret(user.ID, name); err != nil {
		errResp(w, 404, fmt.Sprintf("secret %q not found", name))
		return
	}
	ttl, err := parseTTL(req.TTL)
	if err != nil {
		errResp(w, 400, err.Error())
		return
	}
	sb := s.getUserSandbox(w, r, req.Sandbox)
	if sb == nil {
		return
	}
	hosts, err := grantHosts(req.Hosts, sandboxEgress(sb))
	if err != nil {
		errResp(w, 400, "invalid grant: "+err.Error())
		return
	}
	caps, ok := s.engine.(engine.GuestAgentCapabilities)
	if !ok {
		errResp(w, 501, "engine does not support guest-agent capability checks for secret grants")
		return
	}
	if err := caps.RequireGuestAgentFeature(r.Context(), sb.EngineID, proto.FeatureSandboxCA); err != nil {
		errRespInternal(w, r, "guest agent cannot install sandbox CA", err)
		return
	}
	if _, err := s.store.GetSandboxCA(sb.ID); errors.Is(err, sql.ErrNoRows) {
		errResp(w, 409, fmt.Sprintf("sandbox %q has no substitution CA (it was created without a network, before credential substitution, or from a filesystem snapshot); recreate it to grant secrets", sb.Name))
		return
	} else if err != nil {
		errRespInternal(w, r, "load sandbox CA failed", err)
		return
	}
	g, err := s.newGrant(user.ID, name, sb.ID, hosts, ttl)
	if err != nil {
		errRespInternal(w, r, "create grant failed", err)
		return
	}
	s.recordGrant(g, sb.Name, "")
	writeJSON(w, 201, viewGrant(g, sb.Name, time.Now()))
}

// handleSecretGrants: GET /secrets/grants
func (s *Server) handleSecretGrants(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errResp(w, 405, "method not allowed")
		return
	}
	user := UserFromContext(r.Context())
	grants, err := s.store.ListSecretGrants(user.ID)
	if err != nil {
		errRespInternal(w, r, "list grants failed", err)
		return
	}
	names := map[string]string{}
	if sbs, err := s.store.ListSandboxes(user.ID); err == nil {
		for _, sb := range sbs {
			names[sb.ID] = sb.Name
		}
	}
	now := time.Now()
	out := make([]grantView, 0, len(grants))
	for _, g := range grants {
		out = append(out, viewGrant(g, names[g.SandboxID], now))
	}
	writeJSON(w, 200, out)
}

// handleSecretGrantRevoke: DELETE /secrets/grants/{id}
func (s *Server) handleSecretGrantRevoke(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodDelete {
		errResp(w, 405, "method not allowed")
		return
	}
	user := UserFromContext(r.Context())
	g, err := s.store.GetSecretGrant(user.ID, id)
	if err != nil {
		errResp(w, 404, fmt.Sprintf("grant %q not found", id))
		return
	}
	if err := s.store.RevokeSecretGrant(user.ID, id, time.Now().UTC()); err != nil {
		errRespInternal(w, r, "revoke grant failed", err)
		return
	}
	slog.Info("secret.revoked", "secret", g.SecretName, "grant", id, "sandbox", g.SandboxID)
	s.RecordEvent(store.Event{Type: "secret.revoked", UserID: user.ID, SandboxID: g.SandboxID,
		Meta: map[string]any{"secret": g.SecretName, "grant": id, "sandbox": g.SandboxID}})
	writeJSON(w, 200, map[string]string{"status": "revoked"})
}
