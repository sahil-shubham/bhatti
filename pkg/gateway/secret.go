package gateway

import (
	"bytes"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"sync"
)

// This file is the L7 secret layer: minting per-sandbox aliases and substituting
// them for real credentials on egress. The guest only ever holds the alias; the
// real value lives here and is spliced in only for requests to the secret's
// allowed hosts. See docs/internal/DESIGN-bhatti-v2-secrets-and-trust.md §3.2/§3.6b.

// aliasMarker is a fixed infix embedded in every alias so a leaked alias is
// recognizable as bhatti's even if it's no longer in a live table (a canary the
// audit layer can scan for globally). High-entropy random bytes surround it.
const aliasMarker = "bht"

var aliasEnc = base32.StdEncoding.WithPadding(base32.NoPadding)

// MintAlias returns a fresh, unique, format-preserving alias. prefix mimics the
// real credential's shape so client-side format checks pass (e.g. "sk-" for
// OpenAI, "sk_live_" for Stripe); pass "" for an opaque token. The alias is not
// a secret — it is safe in the guest env, config drive, and snapshots.
func MintAlias(prefix string) (string, error) {
	var b [20]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mint alias: %w", err)
	}
	return prefix + aliasMarker + aliasEnc.EncodeToString(b[:]), nil
}

// Binding is what an alias resolves to: the real secret value and the hosts it
// may be substituted toward (the per-secret anti-exfil allow-list).
type Binding struct {
	SecretName   string
	AllowedHosts []HostPattern
	// Value returns the plaintext secret just-in-time (in production it opens the
	// sealed value; the gateway never holds a plaintext store at rest).
	Value func() (string, error)
}

func (b Binding) hostAllowed(host string) bool {
	for _, hp := range b.AllowedHosts {
		if hp.Match(host) {
			return true
		}
	}
	return false
}

// AliasTable maps a sandbox's live aliases to their bindings. It is per-owner
// (aliases are minted per sandbox), consulted by the L7 proxy on every request.
type AliasTable struct {
	mu sync.RWMutex
	m  map[string]Binding
}

func NewAliasTable() *AliasTable { return &AliasTable{m: map[string]Binding{}} }

func (t *AliasTable) Add(alias string, b Binding) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.m[alias] = b
}

func (t *AliasTable) Remove(alias string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.m, alias)
}

func (t *AliasTable) snapshot() map[string]Binding {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[string]Binding, len(t.m))
	for k, v := range t.m {
		out[k] = v
	}
	return out
}

// SubstituteResult reports the outcome of scanning+rewriting a request.
type SubstituteResult struct {
	Body  []byte   // the request bytes with allowed aliases substituted
	Used  []string // aliases legitimately substituted (dest host allowed)
	Exfil []string // aliases seen heading to a NON-allowed host (canary — block + alert)
}

// Substitute scans body for any live alias and, for each found:
//   - if the destination host is in that alias's allowed hosts, replaces the
//     alias with the real secret value (the legitimate path);
//   - otherwise records it as an exfil attempt and leaves it unreplaced — the
//     caller must BLOCK the request and raise an alert (the alias is a canary).
//
// Substitution works only for secrets that travel verbatim (bearer tokens, API
// keys). Signature-auth (SigV4/JWT/HMAC) never puts the secret on the wire and
// is handled by processors, not here.
func (t *AliasTable) Substitute(host string, body []byte) (SubstituteResult, error) {
	res := SubstituteResult{Body: body}
	for alias, b := range t.snapshot() {
		ab := []byte(alias)
		if !bytes.Contains(res.Body, ab) {
			continue
		}
		if !b.hostAllowed(host) {
			res.Exfil = append(res.Exfil, alias) // canary: leave in place, caller blocks
			continue
		}
		val, err := b.Value()
		if err != nil {
			return res, fmt.Errorf("resolve secret %q: %w", b.SecretName, err)
		}
		res.Body = bytes.ReplaceAll(res.Body, ab, []byte(val))
		res.Used = append(res.Used, alias)
	}
	return res, nil
}
