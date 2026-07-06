package gateway

import (
	"bytes"
	"io"
	"net/http"
	"strings"
)

// This file is the L7 forward proxy: the guest is pointed at it (HTTP_PROXY) and
// sends requests through it; the proxy substitutes aliases for real secrets
// (gated by allowed-hosts), strips proxy-only headers, forces TLS upstream, and
// audits. The guard's vetting Dialer is wired into the upstream Transport so the
// SSRF/private-range guard applies to every upstream connection. Transparent
// CONNECT-MITM (for HTTPS the guest speaks directly) is a follow-on layer that
// reuses this same substitution+dial core once the CA plumbing (lohar) lands.
//
// Design: DESIGN-bhatti-v2-secrets-and-trust.md §3.2/§3.6, §3.9.

// hopByHop + proxy-only headers are stripped before the request leaves the proxy
// so secret-selection headers never reach the upstream.
var stripHeaders = []string{
	"Proxy-Authorization",
	"Proxy-Tokenizer",
	"Proxy-Connection",
}

// Event is an audit record emitted by the proxy. It never contains a secret
// VALUE — Alias/Secret are safe identifiers (the alias is a placeholder, Secret
// is a name), matching the redaction-by-construction rule (§3.7).
type Event struct {
	Kind    string // "substitute" | "exfil" | "deny" | "forward"
	Sandbox string
	Host    string
	Alias   string
	Secret  string
	Reason  string
}

// Proxy is the L7 forward proxy. Transport is the upstream round-tripper; in
// production it wraps the guard's vetting Dialer (so egress policy applies) and
// forces TLS. In tests it can target an httptest server.
type Proxy struct {
	Aliases   *AliasTable
	Sandbox   string
	Transport http.RoundTripper // nil → http.DefaultTransport
	ForceTLS  bool              // rewrite http→https upstream (guest speaks plain to us)
	Audit     func(Event)
}

func (p *Proxy) audit(e Event) {
	if p.Audit != nil {
		e.Sandbox = p.Sandbox
		p.Audit(e)
	}
}

// substituteAcross runs alias substitution over the request's header values and
// body, aggregating what was used and what was an exfil attempt.
func (p *Proxy) substituteAcross(host string, h http.Header, body []byte) (http.Header, []byte, []string, []string, error) {
	usedSet := map[string]bool{}
	exfilSet := map[string]bool{}

	newBody := body
	if len(body) > 0 {
		r, err := p.Aliases.Substitute(host, body)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		newBody = r.Body
		for _, a := range r.Used {
			usedSet[a] = true
		}
		for _, a := range r.Exfil {
			exfilSet[a] = true
		}
	}

	nh := make(http.Header, len(h))
	for k, vals := range h {
		for _, v := range vals {
			r, err := p.Aliases.Substitute(host, []byte(v))
			if err != nil {
				return nil, nil, nil, nil, err
			}
			for _, a := range r.Used {
				usedSet[a] = true
			}
			for _, a := range r.Exfil {
				exfilSet[a] = true
			}
			nh.Add(k, string(r.Body))
		}
	}
	return nh, newBody, keys(usedSet), keys(exfilSet), nil
}

func keys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ServeHTTP handles a forward-proxy request (absolute-URI or Host-bearing). It
// substitutes, blocks exfil, strips proxy headers, and forwards upstream.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.URL.Hostname()
	if host == "" {
		host = hostOnly(r.Host)
	}

	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		http.Error(w, "read body", http.StatusBadGateway)
		return
	}

	newHeader, newBody, used, exfil, err := p.substituteAcross(host, r.Header, body)
	if err != nil {
		p.audit(Event{Kind: "deny", Host: host, Reason: err.Error()})
		http.Error(w, "secret resolution failed", http.StatusBadGateway)
		return
	}

	// Exfil: an alias headed to a non-allowed host. Block the whole request and
	// alert — never forward, even the (harmless) alias.
	if len(exfil) > 0 {
		for _, a := range exfil {
			p.audit(Event{Kind: "exfil", Host: host, Alias: a, Reason: "alias to non-allowed host"})
		}
		http.Error(w, "blocked: credential to a non-allowed host", http.StatusForbidden)
		return
	}

	for _, a := range used {
		p.audit(Event{Kind: "substitute", Host: host, Alias: a})
	}

	// Build the upstream request.
	up := r.Clone(r.Context())
	up.Header = newHeader
	for _, h := range stripHeaders {
		up.Header.Del(h)
	}
	up.Body = io.NopCloser(bytes.NewReader(newBody))
	up.ContentLength = int64(len(newBody))
	up.RequestURI = ""
	if p.ForceTLS && up.URL.Scheme == "http" {
		up.URL.Scheme = "https" // guest spoke plain to us; upstream is TLS
	}
	up.URL.Host = r.Host
	if up.URL.Host == "" {
		up.URL.Host = host
	}

	tr := p.Transport
	if tr == nil {
		tr = http.DefaultTransport
	}
	resp, err := tr.RoundTrip(up)
	if err != nil {
		p.audit(Event{Kind: "deny", Host: host, Reason: err.Error()})
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	p.audit(Event{Kind: "forward", Host: host})
	for k, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func hostOnly(hostport string) string {
	if i := strings.LastIndex(hostport, ":"); i > 0 && !strings.Contains(hostport[i:], "]") {
		return hostport[:i]
	}
	return hostport
}
