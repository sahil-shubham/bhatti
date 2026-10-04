package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http/httpguts"
	"golang.org/x/sync/singleflight"

	"github.com/sahil-shubham/bhatti/pkg/gateway"
)

// Credential substitution. A guest holds placeholders, never secrets. When a
// guest opens TLS to port 443 and the SNI host is one a live secret grant of
// its sandbox names, netd terminates that TLS itself, with a leaf the broker
// mints from the sandbox's own CA (lohar put that CA in the guest's trust
// store), and opens its own TLS to the real upstream, verified against the
// system roots — so a value only ever reaches a server that proved it is the
// granted host. Then it relays HTTP/1.1 request by request, swapping each
// placeholder for its value in header values only. A placeholder anywhere
// else (request target, Host, body) gets a 403: a credential travels only
// where the grant says, in the form it says. Connections to other hosts are
// spliced byte for byte, as before.
//
// netd holds no long-lived key and no secret at rest: leaves and values come
// from the broker per connection, and values live only as long as the
// connection that asked for them.

const (
	tlsPort          = 443
	sniPeekTimeout   = 5 * time.Second  // a guest that doesn't speak first on :443 gets spliced after this
	upstreamTimeout  = 10 * time.Second // upstream TLS handshake
	certRefreshSlack = time.Hour        // refetch a cached leaf this long before it expires
	certNegativeTTL  = 10 * time.Second // remember "no grant for this host" this long
	certCacheLimit   = 4096
	maxPeek          = 128 << 10 // a ClientHello is far smaller; past this the guest isn't speaking TLS
	lingerTimeout    = 2 * time.Second
	lingerDrain      = 1 << 20
)

// brokerDoer is the broker client (gateway.BrokerClient in production).
type brokerDoer interface {
	Do(ctx context.Context, req gateway.BrokerRequest) (gateway.BrokerResponse, error)
}

// credProxy intercepts guests' TLS toward granted hosts.
type credProxy struct {
	broker      brokerDoer
	roots       *x509.CertPool // trust for upstream TLS; nil = system roots
	peekTimeout time.Duration

	mu    sync.Mutex
	certs map[certKey]certEntry
	sf    singleflight.Group
}

type certKey struct{ sandbox, host string }

// certEntry caches the broker's answer for (sandbox, host): a leaf to present,
// or nil — no live grant covers the host, so don't intercept.
type certEntry struct {
	cert  *tls.Certificate
	until time.Time
}

func newCredProxy(b brokerDoer, roots *x509.CertPool) *credProxy {
	return &credProxy{broker: b, roots: roots, peekTimeout: sniPeekTimeout, certs: map[certKey]certEntry{}}
}

var errNotIntercepted = errors.New("not intercepted")

// serve handles one guest connection to port 443 whose upstream (up, already
// vetted by the egress policy and dialed) is the address the guest connected
// to. It reads the ClientHello and either intercepts or replays what it read
// to up and splices the rest.
func (p *credProxy) serve(guest, up net.Conn, sandbox string) {
	pc := &peekConn{Conn: guest, recording: true}
	var host string
	var leaf *tls.Certificate
	srv := tls.Server(pc, &tls.Config{
		GetConfigForClient: func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
			host = normalizeHost(chi.ServerName)
			// Only HTTP/1.1 is relayed; a client that won't speak it (h2-only,
			// gRPC) is left alone rather than broken.
			if host == "" || (len(chi.SupportedProtos) > 0 && !slices.Contains(chi.SupportedProtos, "http/1.1")) {
				return nil, errNotIntercepted
			}
			if leaf = p.certFor(sandbox, host); leaf == nil {
				return nil, errNotIntercepted
			}
			pc.commit()
			return &tls.Config{
				Certificates: []tls.Certificate{*leaf},
				NextProtos:   []string{"http/1.1"},
				MinVersion:   tls.VersionTLS12,
			}, nil
		},
	})
	_ = guest.SetReadDeadline(time.Now().Add(p.peekTimeout))
	herr := srv.Handshake()
	_ = guest.SetReadDeadline(time.Time{})
	if pc.overflow {
		guest.Close()
		up.Close()
		return
	}
	if leaf == nil {
		// Not ours: the upstream gets exactly the bytes the guest sent.
		if b := pc.buf.Bytes(); len(b) > 0 {
			if _, err := up.Write(b); err != nil {
				guest.Close()
				up.Close()
				return
			}
		}
		pc.recording = false
		pc.buf = bytes.Buffer{}
		splice(guest, up)
		return
	}
	pc.recording = false
	pc.buf = bytes.Buffer{}
	if herr != nil {
		// Past the ServerHello there's no handing the stream back; a guest
		// that rejects the leaf (doesn't trust the sandbox CA) is hung up on.
		log.Printf("bhatti-netd: %s: TLS with guest for %s: %v", sandbox, host, herr)
		guest.Close()
		up.Close()
		return
	}
	p.relay(srv, up, sandbox, host)
}

// certFor returns the leaf to present for host, or nil when no live grant of
// sandbox covers it. Answers are cached; concurrent misses share one request.
func (p *credProxy) certFor(sandbox, host string) *tls.Certificate {
	k := certKey{sandbox, host}
	p.mu.Lock()
	if e, ok := p.certs[k]; ok && time.Now().Before(e.until) {
		p.mu.Unlock()
		return e.cert
	}
	p.mu.Unlock()
	v, _, _ := p.sf.Do(sandbox+"\x00"+host, func() (any, error) {
		e := p.fetchCert(sandbox, host)
		p.mu.Lock()
		if len(p.certs) >= certCacheLimit {
			now := time.Now()
			for k, old := range p.certs {
				if !now.Before(old.until) {
					delete(p.certs, k)
				}
			}
			if len(p.certs) >= certCacheLimit {
				p.certs = map[certKey]certEntry{}
			}
		}
		p.certs[k] = e
		p.mu.Unlock()
		return e.cert, nil
	})
	return v.(*tls.Certificate)
}

func (p *credProxy) fetchCert(sandbox, host string) certEntry {
	now := time.Now()
	miss := certEntry{until: now.Add(certNegativeTTL)}
	resp, err := p.broker.Do(context.Background(), gateway.BrokerRequest{Op: gateway.BrokerCert, Sandbox: sandbox, Host: host})
	if err != nil {
		var refused *gateway.BrokerRefusal
		if !errors.As(err, &refused) {
			log.Printf("bhatti-netd: %s: broker cert for %s: %v", sandbox, host, err)
		}
		return miss
	}
	cert, err := tls.X509KeyPair([]byte(resp.CertPEM), []byte(resp.KeyPEM))
	if err != nil || cert.Leaf == nil {
		log.Printf("bhatti-netd: %s: broker cert for %s: unusable: %v", sandbox, host, err)
		return miss
	}
	until := cert.Leaf.NotAfter.Add(-certRefreshSlack)
	if !until.After(now) {
		until = now.Add(certNegativeTTL)
	}
	return certEntry{cert: &cert, until: until}
}

// relay speaks HTTP/1.1 between the intercepted guest connection and a
// verified TLS connection to the upstream, one exchange at a time.
func (p *credProxy) relay(guest *tls.Conn, up net.Conn, sandbox, host string) {
	defer guest.Close()
	defer up.Close()
	upTLS := tls.Client(up, &tls.Config{
		ServerName: host,
		RootCAs:    p.roots,
		NextProtos: []string{"http/1.1"},
		MinVersion: tls.VersionTLS12,
	})
	ctx, cancel := context.WithTimeout(context.Background(), upstreamTimeout)
	uerr := upTLS.HandshakeContext(ctx)
	cancel()
	budget := &headBudget{r: guest, left: maxRequestHead}
	gr := bufio.NewReader(budget)
	if uerr != nil {
		log.Printf("bhatti-netd: %s: upstream TLS for %s: %v", sandbox, host, uerr)
		// Tell the guest why on its first request, then hang up.
		if _, err := http.ReadRequest(gr); err == nil {
			writeError(guest, http.StatusBadGateway, "upstream "+host+" failed TLS verification")
			linger(guest)
		}
		return
	}
	c := &credConn{p: p, sandbox: sandbox, host: host, values: map[string]heldValue{}}
	ur := bufio.NewReader(upTLS)
	for {
		budget.left = maxRequestHead
		req, err := http.ReadRequest(gr)
		budget.left = -1 // bodies stream; only the head is held in memory
		if errors.Is(err, errHeadTooLarge) {
			writeError(guest, http.StatusRequestHeaderFieldsTooLarge, "request head too large")
			linger(guest)
			return
		}
		if err != nil {
			return
		}
		if !c.exchange(req, guest, gr, upTLS, ur) {
			return
		}
	}
}

// maxRequestHead bounds what a guest can make netd hold for one request's
// line and headers (Go's reader doesn't bound them itself).
const maxRequestHead = 256 << 10

var errHeadTooLarge = errors.New("request head too large")

// headBudget counts the bytes read while a request head is parsed; left < 0
// means unbounded (a body is being streamed).
type headBudget struct {
	r    io.Reader
	left int64
}

func (b *headBudget) Read(p []byte) (int, error) {
	if b.left < 0 {
		return b.r.Read(p)
	}
	if b.left == 0 {
		return 0, errHeadTooLarge
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.r.Read(p)
	b.left -= int64(n)
	return n, err
}

// credConn is one intercepted connection's state: the values it resolved,
// which die with it.
type credConn struct {
	p       *credProxy
	sandbox string
	host    string
	values  map[string]heldValue
}

type heldValue struct {
	value string
	until time.Time // zero = while the connection lasts
}

// refusal is a request netd answers itself instead of forwarding.
type refusal struct {
	status      int
	reason      string
	placeholder string
	report      bool // netd's own decision: tell the broker so it's audited
}

func (r *refusal) Error() string { return r.reason }

// exchange relays one request and its response. It reports whether the
// connection can carry another.
func (c *credConn) exchange(req *http.Request, guest net.Conn, gr *bufio.Reader, up net.Conn, ur *bufio.Reader) bool {
	if r := placeholderOutsideHeaders(req); r != nil {
		c.refuse(guest, r)
		return false
	}
	if err := c.substitute(req); err != nil {
		var r *refusal
		if !errors.As(err, &r) {
			r = &refusal{status: http.StatusBadGateway, reason: err.Error()}
		}
		c.refuse(guest, r)
		return false
	}
	// We swallow Expect so the upstream doesn't wait on a 100 we'd have to
	// interleave; the guest gets its 100 from us instead.
	if strings.EqualFold(req.Header.Get("Expect"), "100-continue") {
		req.Header.Del("Expect")
		if _, err := io.WriteString(guest, "HTTP/1.1 100 Continue\r\n\r\n"); err != nil {
			return false
		}
	}
	var guard *bodyGuard
	if req.Body != nil && req.Body != http.NoBody {
		guard = &bodyGuard{r: req.Body}
		req.Body = guard
	}
	if _, ok := req.Header["User-Agent"]; !ok {
		req.Header["User-Agent"] = []string{""} // Request.Write would otherwise add Go's own
	}
	werr := req.Write(up)
	if guard != nil && guard.found != "" {
		c.refuse(guest, &refusal{status: http.StatusForbidden, reason: "placeholder in request body", placeholder: guard.found, report: true})
		return false
	}
	if werr != nil {
		return false
	}
	for {
		resp, err := http.ReadResponse(ur, req)
		if err != nil {
			return false
		}
		if resp.StatusCode == http.StatusSwitchingProtocols {
			if writeResponseHead(guest, resp) != nil {
				return false
			}
			tunnel(guest, gr, up, ur)
			return false
		}
		if resp.StatusCode >= 100 && resp.StatusCode < 200 {
			if writeResponseHead(guest, resp) != nil {
				return false
			}
			continue
		}
		err = resp.Write(guest)
		resp.Body.Close()
		return err == nil && !req.Close && !resp.Close
	}
}

// substitute replaces every placeholder in req's header values with its value,
// resolved by the broker for this connection's host.
func (c *credConn) substitute(req *http.Request) error {
	var found []string
	for name, vals := range req.Header {
		for _, v := range vals {
			found = append(found, gateway.FindPlaceholders([]byte(v))...)
			if dec, ok := basicCredential(name, v); ok {
				found = append(found, gateway.FindPlaceholders(dec)...)
			}
		}
	}
	if len(found) == 0 {
		return nil
	}
	// The upstream is verified as c.host; a Host header naming anything else
	// would route the value past that check (domain fronting).
	if !strings.EqualFold(normalizeHost(stripPort(req.Host)), c.host) {
		return &refusal{status: http.StatusForbidden, placeholder: found[0], report: true,
			reason: fmt.Sprintf("Host %q is not the intercepted host %s", req.Host, c.host)}
	}
	vals := make(map[string]string, len(found))
	for _, ph := range found {
		if _, done := vals[ph]; done {
			continue
		}
		v, err := c.value(ph)
		if err != nil {
			return err
		}
		vals[ph] = v
	}
	for name, vs := range req.Header {
		for i, v := range vs {
			nv := replacePlaceholders(v, vals)
			if dec, ok := basicCredential(name, nv); ok && gateway.ContainsPlaceholder(dec) {
				nv = "Basic " + base64.StdEncoding.EncodeToString([]byte(replacePlaceholders(string(dec), vals)))
			}
			if nv == v {
				continue
			}
			if !httpguts.ValidHeaderFieldValue(nv) {
				return &refusal{status: http.StatusBadGateway, reason: "a secret's value is not a valid HTTP header value"}
			}
			vs[i] = nv
		}
	}
	return nil
}

// value resolves ph for this connection, once.
func (c *credConn) value(ph string) (string, error) {
	now := time.Now()
	if hv, ok := c.values[ph]; ok && (hv.until.IsZero() || now.Before(hv.until)) {
		return hv.value, nil
	}
	resp, err := c.p.broker.Do(context.Background(), gateway.BrokerRequest{
		Op: gateway.BrokerResolve, Sandbox: c.sandbox, Placeholder: ph, Host: c.host,
	})
	if err != nil {
		var refused *gateway.BrokerRefusal
		if errors.As(err, &refused) {
			// The broker audits its own refusals.
			return "", &refusal{status: http.StatusForbidden, reason: "credential refused: " + refused.Reason}
		}
		log.Printf("bhatti-netd: %s: broker resolve for %s: %v", c.sandbox, c.host, err)
		return "", &refusal{status: http.StatusServiceUnavailable, reason: "credential broker unavailable"}
	}
	hv := heldValue{value: resp.Value}
	if resp.TTLSeconds > 0 {
		hv.until = now.Add(time.Duration(resp.TTLSeconds) * time.Second)
	}
	c.values[ph] = hv
	return resp.Value, nil
}

func (c *credConn) refuse(guest net.Conn, r *refusal) {
	if r.report {
		go c.p.report(c.sandbox, r.placeholder, c.host, r.reason)
	}
	writeError(guest, r.status, r.reason)
	linger(guest)
}

func (p *credProxy) report(sandbox, placeholder, host, reason string) {
	_, err := p.broker.Do(context.Background(), gateway.BrokerRequest{
		Op: gateway.BrokerReport, Sandbox: sandbox, Placeholder: placeholder, Host: host, Reason: reason,
	})
	if err != nil {
		log.Printf("bhatti-netd: %s: report denial for %s: %v", sandbox, host, err)
	}
}

// placeholderOutsideHeaders refuses a placeholder in the request target or the
// Host header, raw or percent-decoded.
func placeholderOutsideHeaders(req *http.Request) *refusal {
	check := func(where, s string) *refusal {
		forms := []string{s}
		if u, err := url.PathUnescape(s); err == nil {
			forms = append(forms, u)
		}
		if u, err := url.QueryUnescape(s); err == nil {
			forms = append(forms, u)
		}
		for _, f := range forms {
			if phs := gateway.FindPlaceholders([]byte(f)); len(phs) > 0 {
				return &refusal{status: http.StatusForbidden, reason: "placeholder in " + where, placeholder: phs[0], report: true}
			}
		}
		return nil
	}
	if r := check("request target", req.RequestURI); r != nil {
		return r
	}
	return check("Host header", req.Host)
}

// basicCredential decodes an Authorization: Basic credential, where tools like
// git put a token (as the password).
func basicCredential(name, v string) ([]byte, bool) {
	if name != "Authorization" && name != "Proxy-Authorization" {
		return nil, false
	}
	const prefix = "basic "
	if len(v) <= len(prefix) || !strings.EqualFold(v[:len(prefix)], prefix) {
		return nil, false
	}
	dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v[len(prefix):]))
	if err != nil {
		return nil, false
	}
	return dec, true
}

func replacePlaceholders(s string, vals map[string]string) string {
	if !strings.Contains(s, gateway.PlaceholderPrefix) {
		return s
	}
	for ph, v := range vals {
		s = strings.ReplaceAll(s, ph, v)
	}
	return s
}

// bodyGuard passes a request body through, holding back enough bytes that a
// placeholder is seen whole before any of it is released; on one it stops.
type bodyGuard struct {
	r     io.Reader
	buf   []byte // read but not yet released: buf[off:]
	off   int
	eof   bool
	found string
}

var errPlaceholderInBody = errors.New("placeholder in request body")

const bodyGuardChunk = 32 << 10

func (g *bodyGuard) Read(p []byte) (int, error) {
	const hold = gateway.PlaceholderLen - 1
	for {
		if g.found != "" {
			return 0, errPlaceholderInBody
		}
		avail := len(g.buf) - g.off
		release := avail
		if !g.eof {
			release = avail - hold
		}
		if release > 0 {
			n := copy(p, g.buf[g.off:g.off+release])
			g.off += n
			return n, nil
		}
		if g.eof {
			return 0, io.EOF
		}
		if g.off > 0 {
			g.buf = g.buf[:copy(g.buf, g.buf[g.off:])]
			g.off = 0
		}
		if g.buf == nil {
			g.buf = make([]byte, 0, bodyGuardChunk+hold)
		}
		// Only a placeholder that includes new bytes can be new; it starts at
		// most hold bytes back.
		scan := max(len(g.buf)-hold, 0)
		n, err := g.r.Read(g.buf[len(g.buf):cap(g.buf)])
		g.buf = g.buf[:len(g.buf)+n]
		if phs := gateway.FindPlaceholders(g.buf[scan:]); len(phs) > 0 {
			g.found = phs[0]
			return 0, errPlaceholderInBody
		}
		if err == io.EOF {
			g.eof = true
		} else if err != nil {
			return 0, err
		}
	}
}

func (g *bodyGuard) Close() error {
	if c, ok := g.r.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// tunnel hands a connection that switched protocols (WebSocket) over to a
// byte splice, first forwarding whatever either reader already buffered.
func tunnel(guest net.Conn, gr *bufio.Reader, up net.Conn, ur *bufio.Reader) {
	if n := gr.Buffered(); n > 0 {
		b, _ := gr.Peek(n)
		if _, err := up.Write(b); err != nil {
			return
		}
	}
	if n := ur.Buffered(); n > 0 {
		b, _ := ur.Peek(n)
		if _, err := guest.Write(b); err != nil {
			return
		}
	}
	splice(guest, up)
}

func writeResponseHead(w io.Writer, resp *http.Response) error {
	var b bytes.Buffer
	fmt.Fprintf(&b, "HTTP/%d.%d %s\r\n", resp.ProtoMajor, resp.ProtoMinor, resp.Status)
	if err := resp.Header.Write(&b); err != nil {
		return err
	}
	b.WriteString("\r\n")
	_, err := w.Write(b.Bytes())
	return err
}

func writeError(w io.Writer, status int, msg string) {
	body := "bhatti: " + msg + "\n"
	fmt.Fprintf(w, "HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, http.StatusText(status), len(body), body)
}

// linger lets the guest read our answer before we hang up: closing with its
// unread request bytes still queued would turn the answer into a reset.
func linger(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
	_ = c.SetReadDeadline(time.Now().Add(lingerTimeout))
	_, _ = io.Copy(io.Discard, io.LimitReader(c, lingerDrain))
}

func normalizeHost(h string) string {
	return strings.ToLower(strings.TrimSuffix(h, "."))
}

func stripPort(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

// peekConn records what the guest sends during the ClientHello, so it can be
// replayed to the upstream if netd doesn't intercept, and discards what the
// TLS server writes until netd commits to intercepting.
type peekConn struct {
	net.Conn
	recording bool
	committed bool
	overflow  bool
	buf       bytes.Buffer
}

func (c *peekConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if c.recording && n > 0 {
		if c.buf.Len()+n > maxPeek {
			c.overflow = true
			return 0, errors.New("client hello too large")
		}
		c.buf.Write(b[:n])
	}
	return n, err
}

func (c *peekConn) Write(b []byte) (int, error) {
	if !c.committed {
		return len(b), nil
	}
	return c.Conn.Write(b)
}

func (c *peekConn) commit() { c.committed = true }
