package dns

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"
)

// DefaultTTL is the TTL we put on A/PTR answers. Short so that
// destroy+recreate of a sandbox doesn't leave stale IPs cached on
// other sandboxes. The IP pool reuses freed IPs FIFO, so a cached
// stale entry could resolve to a different sandbox. 5s is a balance
// between "fast enough to absorb destroy/recreate churn" and "not
// so chatty that the resolver re-queries on every TCP connect".
const DefaultTTL uint32 = 5

// SuffixSandbox is the optional suffix accepted on lookups. Both
// "myhost" and "myhost.sb" resolve. This mirrors what k3s and several
// other tools expect from a search-path-friendly resolver.
const SuffixSandbox = ".sb"

// forwardTimeout bounds a single upstream round-trip. Deliberately
// shorter than glibc's default 5s per-resolver timeout so that a dead
// upstream doesn't blow the sandbox's whole resolve budget — we'd
// rather SERVFAIL fast than hang. Two seconds is comfortably longer
// than any healthy public resolver's RTT.
const forwardTimeout = 2 * time.Second

// Server is a per-network DNS responder. Each UserNetwork wires one
// instance up at its gateway IP (10.0.N.1:53). The server holds a
// name → IP table (zone) plus a reverse-direction IP → name table
// for PTR lookups. Mutations are concurrency-safe.
type Server struct {
	bindAddr string

	mu        sync.RWMutex
	names     map[string]net.IP // lower-case dotted name without suffix → IPv4
	reverse   map[string]string // IP string → name (most recent owner for PTR)
	startTime time.Time

	// Logger is the slog.Logger this server reports operational events to.
	// Defaults to slog.Default() if nil. Tests inject a discarding logger
	// to keep CI output quiet under stress.
	Logger *slog.Logger

	// Upstreams is the ordered list of upstream resolvers (host or
	// host:port) to forward queries we are not authoritative for.
	// Empty = authoritative-only mode: a name outside our zone gets
	// NXDOMAIN (the historical behavior, kept for tests and for the
	// degraded "bind succeeded but no upstream configured" case).
	//
	// When non-empty the server becomes a recursing forwarder: A/AAAA/
	// PTR/other queries for names NOT in our zone are relayed verbatim
	// to the first upstream that answers, and that answer (including a
	// truthful NXDOMAIN from the upstream) is passed straight back to
	// the client. This is what makes a sandbox able to resolve both
	// `sibling.sb` (our zone) AND `archive.ubuntu.com` (forwarded)
	// from the single nameserver line lohar writes. Without it, glibc
	// takes our authoritative NXDOMAIN as final and never reaches a
	// public resolver — which silently broke apt/curl in every
	// sandbox (G1.1 regression, caught in CI run 26806008509).
	//
	// Set before Start; not mutated afterward (no lock needed).
	Upstreams []string

	// stop channels are closed by Stop() to signal goroutines to exit.
	udpConn  net.PacketConn
	tcpLn    net.Listener
	stopOnce sync.Once
	stopped  chan struct{}
	wg       sync.WaitGroup
}

// NewServer constructs a Server that will bind to bindAddr (host:port).
// The actual bind happens in Start.
func NewServer(bindAddr string) *Server {
	return &Server{
		bindAddr: bindAddr,
		names:    make(map[string]net.IP),
		reverse:  make(map[string]string),
		stopped:  make(chan struct{}),
	}
}

// Set registers a name → IP mapping. The name is lower-cased and
// stripped of any trailing dot or .sb suffix before storing, so the
// caller can pass either form. Reverse PTR pointer is also installed
// (last-write-wins if multiple names share an IP, which shouldn't
// happen in our per-user-bridge zone but is handled benignly).
func (s *Server) Set(name string, ip net.IP) {
	name = canonicalize(name)
	ip4 := ip.To4()
	if name == "" || ip4 == nil {
		return
	}
	s.mu.Lock()
	s.names[name] = ip4
	s.reverse[ip4.String()] = name
	s.mu.Unlock()
}

// Delete removes a name → IP mapping. The matching reverse entry is
// only removed if it still points to the deleted name (so a Set
// followed by Delete on the same name doesn't leak the PTR).
func (s *Server) Delete(name string) {
	name = canonicalize(name)
	s.mu.Lock()
	if ip, ok := s.names[name]; ok {
		delete(s.names, name)
		if s.reverse[ip.String()] == name {
			delete(s.reverse, ip.String())
		}
	}
	s.mu.Unlock()
}

// Lookup returns the IP associated with name. The lookup applies the
// same canonicalisation as Set: lower-case, strip trailing dot,
// strip optional .sb suffix.
func (s *Server) Lookup(name string) (net.IP, bool) {
	name = canonicalize(name)
	s.mu.RLock()
	ip, ok := s.names[name]
	s.mu.RUnlock()
	return ip, ok
}

// LookupReverse returns the name registered for an IP, if any.
func (s *Server) LookupReverse(ip net.IP) (string, bool) {
	ip4 := ip.To4()
	if ip4 == nil {
		return "", false
	}
	s.mu.RLock()
	name, ok := s.reverse[ip4.String()]
	s.mu.RUnlock()
	return name, ok
}

// Names returns a sorted snapshot of registered names. Useful for tests
// and admin tools; production code should not enumerate the zone on
// the hot path.
func (s *Server) Names() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.names))
	for n := range s.names {
		out = append(out, n)
	}
	return out
}

// canonicalize normalises a query name to the key we store under:
// lower-case, trailing dot stripped, optional .sb suffix stripped.
func canonicalize(name string) string {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	name = strings.TrimSuffix(name, SuffixSandbox)
	return name
}

// Start binds the UDP and TCP listeners and runs accept loops in
// background goroutines. The Server stops when ctx is cancelled OR
// Stop() is called. Start returns once the listeners are bound, so
// callers can begin sending queries; it does not block on the accept
// loops.
//
// Errors during accept (a client connection failing) are logged but
// do not stop the server. A failure to bind either listener is
// returned synchronously.
func (s *Server) Start(ctx context.Context) error {
	logger := s.logger()
	udp, err := net.ListenPacket("udp", s.bindAddr)
	if err != nil {
		return fmt.Errorf("dns: bind UDP %s: %w", s.bindAddr, err)
	}
	tcp, err := net.Listen("tcp", s.bindAddr)
	if err != nil {
		udp.Close()
		return fmt.Errorf("dns: bind TCP %s: %w", s.bindAddr, err)
	}
	s.udpConn = udp
	s.tcpLn = tcp
	s.startTime = time.Now()

	s.wg.Add(2)
	go s.serveUDP(logger)
	go s.serveTCP(logger)

	// Shutdown on either ctx done or Stop().
	go func() {
		select {
		case <-ctx.Done():
		case <-s.stopped:
		}
		s.stopOnce.Do(func() {
			close(s.stopped)
			udp.Close()
			tcp.Close()
		})
	}()
	return nil
}

// Addr returns the actual bound address. Useful when bindAddr was
// "127.0.0.1:0" and the kernel assigned a port — tests need to know
// where to send queries.
func (s *Server) Addr() net.Addr {
	if s.udpConn == nil {
		return nil
	}
	return s.udpConn.LocalAddr()
}

// Stop tears the server down. Idempotent. Returns after all in-flight
// query handlers have drained.
func (s *Server) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopped)
		if s.udpConn != nil {
			s.udpConn.Close()
		}
		if s.tcpLn != nil {
			s.tcpLn.Close()
		}
	})
	s.wg.Wait()
}

func (s *Server) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// serveUDP is the UDP packet loop. One goroutine, recvfrom +
// dispatch + sendto. Errors decoding individual messages are logged
// and the loop continues; a fatal Listener error returns.
//
// Each message is handled inline (no goroutine per query). DNS
// responses are small and the lookup is O(1), so dedicating a
// goroutine per query would be wasted scheduler work.
func (s *Server) serveUDP(logger *slog.Logger) {
	defer s.wg.Done()
	// 1232 is the EDNS0 safe max; we don't do EDNS0 but the same buffer
	// size is fine — DNS over UDP is capped at 512 octets without it.
	// Use 1500 to be safe against well-meaning resolvers.
	buf := make([]byte, 1500)
	for {
		n, addr, err := s.udpConn.ReadFrom(buf)
		if err != nil {
			if isClosed(err) {
				return
			}
			logger.Warn("dns: udp read", "err", err)
			continue
		}
		// Recover from any panic in the handler so one bad query can't
		// take down the server. The corresponding TCP path has the same
		// guard. Goal 1 acceptance criterion: "Responder under load:
		// 1000 queries/sec across 50 sandboxes ... ~0 dropped."
		func() {
			defer func() {
				if r := recover(); r != nil {
					logger.Error("dns: udp handler panic",
						"panic", r, "from", addr.String())
				}
			}()
			resp := s.handle(buf[:n], "udp")
			if resp == nil {
				return
			}
			if _, err := s.udpConn.WriteTo(resp, addr); err != nil {
				logger.Debug("dns: udp write", "to", addr.String(), "err", err)
			}
		}()
	}
}

// serveTCP is the TCP accept loop. RFC 1035 §4.2.2: messages over TCP
// are length-prefixed (2-byte big-endian length, then the message).
// We handle one query per connection and close — keep-alive isn't
// worth the complexity at our scale.
func (s *Server) serveTCP(logger *slog.Logger) {
	defer s.wg.Done()
	for {
		conn, err := s.tcpLn.Accept()
		if err != nil {
			if isClosed(err) {
				return
			}
			logger.Warn("dns: tcp accept", "err", err)
			continue
		}
		s.wg.Add(1)
		go func(c net.Conn) {
			defer s.wg.Done()
			defer c.Close()
			defer func() {
				if r := recover(); r != nil {
					logger.Error("dns: tcp handler panic",
						"panic", r, "from", c.RemoteAddr().String())
				}
			}()
			c.SetDeadline(time.Now().Add(10 * time.Second))
			var lenBuf [2]byte
			if _, err := io.ReadFull(c, lenBuf[:]); err != nil {
				return
			}
			msgLen := binary.BigEndian.Uint16(lenBuf[:])
			if msgLen == 0 || msgLen > 8192 {
				return // sanity cap
			}
			buf := make([]byte, msgLen)
			if _, err := io.ReadFull(c, buf); err != nil {
				return
			}
			resp := s.handle(buf, "tcp")
			if resp == nil {
				return
			}
			var respLen [2]byte
			binary.BigEndian.PutUint16(respLen[:], uint16(len(resp)))
			c.Write(respLen[:])
			c.Write(resp)
		}(conn)
	}
}

// handle parses a query, looks up the answer, and returns the response
// bytes. proto is "udp" or "tcp" — it selects the transport used when
// forwarding to an upstream (we forward over the same protocol the
// client used, so a TCP client that needed TCP for a large answer gets
// TCP all the way through). Returns nil if the query is too malformed
// to construct a response (e.g. truncated header — there's no ID to
// echo).
func (s *Server) handle(query []byte, proto string) []byte {
	m, err := ParseMessage(query)
	if err != nil {
		// We don't have an ID; drop silently. Resolvers retry on timeout.
		return nil
	}

	// One question per message is the universal convention. The protocol
	// allows more, but real clients send exactly one and we treat extras
	// as a protocol error (FORMERR).
	if len(m.Questions) != 1 {
		resp, _ := BuildResponse(m, nil, RCodeFormErr)
		return resp
	}
	q := m.Questions[0]
	if q.Class != QClassIN {
		// We only serve the IN class. NOTIMP would be more correct
		// (RCODE 4) but our wire code doesn't expose that constant
		// because we don't otherwise need it; SERVFAIL conveys the
		// same "we won't answer this" to the client.
		resp, _ := BuildResponse(m, nil, RCodeServFail)
		return resp
	}

	forwarding := len(s.Upstreams) > 0

	switch q.QType {
	case QTypeA:
		if _, ok := s.Lookup(q.Name); ok {
			return s.answerA(m, q.Name)
		}
		if forwarding {
			return s.forward(m, query, proto)
		}
		// Authoritative-only: the name isn't ours and we have nowhere
		// to forward, so as far as we can tell it doesn't exist.
		resp, _ := BuildResponse(m, nil, RCodeNXDomain)
		return resp

	case QTypeAAAA:
		if _, ok := s.Lookup(q.Name); ok {
			// The name is ours but we have no IPv6. RFC 4074 §3:
			// respond NOERROR with no answers (NOT NXDOMAIN!) so the
			// client falls back to the A record instead of negative-
			// caching the name as nonexistent.
			resp, _ := BuildResponse(m, nil, RCodeNoError)
			return resp
		}
		if forwarding {
			// Not ours — a public name may legitimately have AAAA
			// records, so forward rather than synthesise empty.
			return s.forward(m, query, proto)
		}
		// Authoritative-only legacy behavior: empty NOERROR for any
		// AAAA (matches pre-forwarding tests).
		resp, _ := BuildResponse(m, nil, RCodeNoError)
		return resp

	case QTypePTR:
		if ip, ok := InAddrArpaToIPv4(q.Name); ok {
			if _, ok := s.LookupReverse(ip); ok {
				return s.answerPTR(m, q.Name)
			}
		}
		if forwarding {
			// O4: we are only authoritative for our own bridge's
			// subnet reverse zone, not all of 10.0.0.0/8. An IP we
			// don't have a name for (in or out of our subnet) gets
			// forwarded; the upstream returns the truthful answer
			// (usually NXDOMAIN for private space, which we relay).
			return s.forward(m, query, proto)
		}
		resp, _ := BuildResponse(m, nil, RCodeNXDomain)
		return resp

	default:
		// Other QTYPEs (MX, TXT, SRV, ...). If the name is ours,
		// return empty NOERROR — the name exists, just not with this
		// record type, and NXDOMAIN would lie about its existence and
		// risk the client giving up on the A query too. If the name
		// isn't ours, forward (or empty NOERROR in authoritative-only
		// mode, the historical safe default).
		if _, ok := s.Lookup(q.Name); ok {
			resp, _ := BuildResponse(m, nil, RCodeNoError)
			return resp
		}
		if forwarding {
			return s.forward(m, query, proto)
		}
		resp, _ := BuildResponse(m, nil, RCodeNoError)
		return resp
	}
}

// forward relays a query we are not authoritative for to the configured
// upstreams, trying each in order until one responds. The upstream's
// response bytes are returned verbatim: because we forwarded the
// client's ORIGINAL query (which carries the client's transaction ID),
// the response already has the matching ID and can go straight back to
// the client without any rewriting.
//
// On total upstream failure (all timed out / unreachable) we synthesise
// SERVFAIL so the client gets a definite negative rather than hanging
// until its own timeout.
//
// Note on shutdown latency: a forward in flight when Stop() is called
// is bounded by forwardTimeout (2s), so Stop()'s wg.Wait() can block up
// to that long. Acceptable at our scale; revisit with a context if it
// ever matters.
func (s *Server) forward(m *Message, query []byte, proto string) []byte {
	logger := s.logger()
	for _, up := range s.Upstreams {
		addr := withDefaultPort(up)
		var resp []byte
		var err error
		if proto == "tcp" {
			resp, err = forwardTCP(addr, query)
		} else {
			resp, err = forwardUDP(addr, query)
		}
		if err != nil {
			logger.Debug("dns: upstream failed", "upstream", addr, "err", err)
			continue
		}
		return resp
	}
	logger.Warn("dns: all upstreams failed; returning SERVFAIL",
		"upstreams", s.Upstreams)
	resp, _ := BuildResponse(m, nil, RCodeServFail)
	return resp
}

// forwardUDP sends query to a single upstream over UDP and returns the
// raw response. One socket per call — simple, and the per-query
// connect cost is negligible at sandbox query volumes. The kernel
// routes the response back to this socket, so concurrent forwards with
// colliding transaction IDs can't cross-talk.
func forwardUDP(upstream string, query []byte) ([]byte, error) {
	conn, err := net.DialTimeout("udp", upstream, forwardTimeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(forwardTimeout))
	if _, err := conn.Write(query); err != nil {
		return nil, err
	}
	resp := make([]byte, 1500)
	n, err := conn.Read(resp)
	if err != nil {
		return nil, err
	}
	return resp[:n], nil
}

// forwardTCP sends query to a single upstream over TCP (RFC 1035
// §4.2.2 length-prefixed framing) and returns the raw response body
// (without the length prefix). Used when the client reached us over
// TCP — typically because a prior UDP answer was truncated and the
// client retried over TCP per spec.
func forwardTCP(upstream string, query []byte) ([]byte, error) {
	conn, err := net.DialTimeout("tcp", upstream, forwardTimeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(forwardTimeout))
	var lenBuf [2]byte
	binary.BigEndian.PutUint16(lenBuf[:], uint16(len(query)))
	if _, err := conn.Write(append(lenBuf[:], query...)); err != nil {
		return nil, err
	}
	var respLenBuf [2]byte
	if _, err := io.ReadFull(conn, respLenBuf[:]); err != nil {
		return nil, err
	}
	respLen := binary.BigEndian.Uint16(respLenBuf[:])
	if respLen == 0 {
		return nil, fmt.Errorf("dns: upstream %s returned zero-length response", upstream)
	}
	resp := make([]byte, respLen)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// withDefaultPort appends :53 to an upstream address that doesn't
// already carry a port. Handles bare IPv4 ("1.1.1.1"), IPv6
// ("2606:4700:4700::1111"), and already-ported forms
// ("1.1.1.1:5353", "[2606:...]:53").
func withDefaultPort(addr string) string {
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr // already host:port
	}
	return net.JoinHostPort(addr, "53")
}

func (s *Server) answerA(query *Message, name string) []byte {
	ip, ok := s.Lookup(name)
	if !ok {
		resp, _ := BuildResponse(query, nil, RCodeNXDomain)
		return resp
	}
	rdata, err := ARecordRData(ip)
	if err != nil {
		resp, _ := BuildResponse(query, nil, RCodeServFail)
		return resp
	}
	answers := []Answer{{
		Name:  name, // echo the queried name verbatim
		Type:  QTypeA,
		Class: QClassIN,
		TTL:   DefaultTTL,
		RData: rdata,
	}}
	resp, err := BuildResponse(query, answers, RCodeNoError)
	if err != nil {
		fallback, _ := BuildResponse(query, nil, RCodeServFail)
		return fallback
	}
	return resp
}

func (s *Server) answerPTR(query *Message, name string) []byte {
	ip, ok := InAddrArpaToIPv4(name)
	if !ok {
		resp, _ := BuildResponse(query, nil, RCodeNXDomain)
		return resp
	}
	target, ok := s.LookupReverse(ip)
	if !ok {
		resp, _ := BuildResponse(query, nil, RCodeNXDomain)
		return resp
	}
	// Some clients (resolver libraries) prefer FQDN-shaped PTR responses
	// — append ".sb" so the answer reads "myhost.sb" rather than just
	// "myhost". Reverse lookups exist mostly for log readability, so the
	// suffix being explicit doesn't hurt.
	rdata, err := PTRRecordRData(target + SuffixSandbox)
	if err != nil {
		resp, _ := BuildResponse(query, nil, RCodeServFail)
		return resp
	}
	answers := []Answer{{
		Name:  name,
		Type:  QTypePTR,
		Class: QClassIN,
		TTL:   DefaultTTL,
		RData: rdata,
	}}
	resp, err := BuildResponse(query, answers, RCodeNoError)
	if err != nil {
		fallback, _ := BuildResponse(query, nil, RCodeServFail)
		return fallback
	}
	return resp
}

// isClosed returns true for the errors that signal the listener was
// torn down (Stop or ctx done), so the goroutine can exit cleanly.
func isClosed(err error) bool {
	return errors.Is(err, net.ErrClosed) ||
		strings.Contains(err.Error(), "use of closed network connection")
}
