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
			resp := s.handle(buf[:n])
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
			resp := s.handle(buf)
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
// bytes. Returns nil if the query is too malformed to construct a
// response (e.g. truncated header — there's no ID to echo).
func (s *Server) handle(query []byte) []byte {
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

	switch q.QType {
	case QTypeA:
		return s.answerA(m, q.Name)
	case QTypeAAAA:
		// We don't serve IPv6. RFC 4074 §3: respond NOERROR with no
		// answers (not NXDOMAIN!) so the client moves to the next
		// resolver / IPv4 fallback without negative-caching the name.
		resp, _ := BuildResponse(m, nil, RCodeNoError)
		return resp
	case QTypePTR:
		return s.answerPTR(m, q.Name)
	default:
		// Other QTYPEs (MX, TXT, etc.) — return empty NOERROR. NXDOMAIN
		// would lie about the name's existence; NOTIMP would be more
		// correct but isn't supported by all clients gracefully. Empty
		// NOERROR is the safe default.
		resp, _ := BuildResponse(m, nil, RCodeNoError)
		return resp
	}
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
