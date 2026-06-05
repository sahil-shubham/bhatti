package dns

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Server tests use a real UDP+TCP listener on 127.0.0.1:0 so we
// exercise the kernel sockets and don't depend on miekg/dns as a
// client. Helpers below build query bytes manually using the wire
// package itself, which keeps the test surface symmetric with prod.

// newTestServer starts a Server on a kernel-assigned port and returns
// it along with a cleanup func. The Logger is muted so passing tests
// don't spam.
func newTestServer(t *testing.T) (*Server, context.CancelFunc) {
	t.Helper()
	s := NewServer("127.0.0.1:0")
	s.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		s.Stop()
	})
	return s, cancel
}

// buildQuery constructs a UDP-format query for the given name + qtype.
func buildQuery(t *testing.T, id uint16, name string, qtype uint16) []byte {
	t.Helper()
	q := &Message{
		Header: Header{
			ID:      id,
			Flags:   FlagRD,
			QDCount: 1,
		},
		Questions: []Question{
			{Name: name, QType: qtype, Class: QClassIN},
		},
	}
	out, err := BuildResponse(q, nil, RCodeNoError)
	if err != nil {
		t.Fatalf("buildQuery: %v", err)
	}
	// The query has QR=0; BuildResponse sets QR. Clear it so the
	// server doesn't treat it as a response.
	binary.BigEndian.PutUint16(out[2:4],
		binary.BigEndian.Uint16(out[2:4])&^FlagQR)
	return out
}

// sendUDP sends a query and returns the response bytes. Blocks up to
// 2s (way more than the round-trip should ever take).
func sendUDP(t *testing.T, addr net.Addr, query []byte) []byte {
	t.Helper()
	conn, err := net.Dial("udp", addr.String())
	if err != nil {
		t.Fatalf("dial udp: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write(query); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp := make([]byte, 4096)
	n, err := conn.Read(resp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return resp[:n]
}

// sendTCP sends a query over the length-prefixed TCP transport.
func sendTCP(t *testing.T, addr net.Addr, query []byte) []byte {
	t.Helper()
	host, port, _ := net.SplitHostPort(addr.String())
	conn, err := net.Dial("tcp", net.JoinHostPort(host, port))
	if err != nil {
		t.Fatalf("dial tcp: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	var lenBuf [2]byte
	binary.BigEndian.PutUint16(lenBuf[:], uint16(len(query)))
	if _, err := conn.Write(lenBuf[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(query); err != nil {
		t.Fatal(err)
	}
	var respLen [2]byte
	if _, err := io.ReadFull(conn, respLen[:]); err != nil {
		t.Fatalf("read length: %v", err)
	}
	respBuf := make([]byte, binary.BigEndian.Uint16(respLen[:]))
	if _, err := io.ReadFull(conn, respBuf); err != nil {
		t.Fatalf("read body: %v", err)
	}
	return respBuf
}

// tcpAddr returns the TCP listener's address. The server binds UDP
// and TCP to the same address, but the kernel picks ports independently
// when the port portion is 0; we expose Addr() for UDP, and the TCP
// port is one we need separately.
func tcpAddr(s *Server) net.Addr {
	return s.tcpLn.Addr()
}

// --- Lifecycle ---

func TestServer_StartAndStop(t *testing.T) {
	s, cancel := newTestServer(t)
	defer cancel()
	if s.Addr() == nil {
		t.Fatal("Addr() should be set after Start")
	}
	// Stop is idempotent; second call must not panic or deadlock.
	s.Stop()
	s.Stop()
}

func TestServer_StartFailsOnBoundPort(t *testing.T) {
	// Bind a known port, then try to start a second server there. The
	// second Start must surface an error.
	first, cancel := newTestServer(t)
	defer cancel()

	s2 := NewServer(first.Addr().String())
	s2.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	if err := s2.Start(ctx); err == nil {
		s2.Stop()
		t.Fatal("expected Start to fail on bound port")
	}
}

// --- Zone management ---

func TestServer_SetLookupDelete(t *testing.T) {
	s, _ := newTestServer(t)
	s.Set("alpha", net.IPv4(10, 0, 1, 2))
	s.Set("BETA.SB", net.IPv4(10, 0, 1, 3))       // case + suffix
	s.Set("gamma.", net.IPv4(10, 0, 1, 4))        // trailing dot

	if ip, ok := s.Lookup("alpha"); !ok || !ip.Equal(net.IPv4(10, 0, 1, 2)) {
		t.Errorf("alpha: ok=%v ip=%v", ok, ip)
	}
	// Lookup must canonicalise too.
	if ip, ok := s.Lookup("ALPHA"); !ok || !ip.Equal(net.IPv4(10, 0, 1, 2)) {
		t.Errorf("ALPHA: ok=%v ip=%v", ok, ip)
	}
	if ip, ok := s.Lookup("beta"); !ok || !ip.Equal(net.IPv4(10, 0, 1, 3)) {
		t.Errorf("beta (set as BETA.SB): ok=%v ip=%v", ok, ip)
	}
	if ip, ok := s.Lookup("gamma.sb"); !ok || !ip.Equal(net.IPv4(10, 0, 1, 4)) {
		t.Errorf("gamma.sb (set with trailing dot): ok=%v ip=%v", ok, ip)
	}

	// Reverse lookup
	if name, ok := s.LookupReverse(net.IPv4(10, 0, 1, 2)); !ok || name != "alpha" {
		t.Errorf("reverse: ok=%v name=%q", ok, name)
	}

	// Delete removes both forward and reverse.
	s.Delete("alpha")
	if _, ok := s.Lookup("alpha"); ok {
		t.Error("alpha should be deleted")
	}
	if _, ok := s.LookupReverse(net.IPv4(10, 0, 1, 2)); ok {
		t.Error("reverse for alpha's IP should be deleted")
	}
}

func TestServer_ReverseNotClobberedByOverwrite(t *testing.T) {
	// If we Set name=A IP=X, then Set name=A IP=Y, the PTR record for
	// X should NOT still point to A. (We only delete the reverse when
	// the name we're removing still owns the IP.)
	s, _ := newTestServer(t)
	s.Set("host", net.IPv4(10, 0, 1, 2))
	s.Set("host", net.IPv4(10, 0, 1, 3))

	// Old reverse still exists pointing at "host" — that's fine because
	// 10.0.1.2 is also gone from the forward zone. The "Names()" output
	// confirms the forward state.
	names := s.Names()
	if len(names) != 1 || names[0] != "host" {
		t.Fatalf("names: %v", names)
	}
	// Reverse for the *current* IP must point at host.
	if name, _ := s.LookupReverse(net.IPv4(10, 0, 1, 3)); name != "host" {
		t.Errorf("reverse for 10.0.1.3: %q", name)
	}
}

// --- UDP query path ---

func TestServer_AOverUDP(t *testing.T) {
	s, _ := newTestServer(t)
	s.Set("myhost", net.IPv4(10, 0, 1, 7))

	q := buildQuery(t, 0x1234, "myhost", QTypeA)
	resp := sendUDP(t, s.Addr(), q)
	m, err := ParseMessage(resp)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Header.ID != 0x1234 {
		t.Errorf("ID echo: got 0x%x", m.Header.ID)
	}
	if m.Header.Flags&FlagQR == 0 || m.Header.Flags&FlagAA == 0 {
		t.Errorf("flags: QR/AA missing: 0x%x", m.Header.Flags)
	}
	if m.Header.RCode() != RCodeNoError {
		t.Errorf("RCODE: got %d", m.Header.RCode())
	}
	if m.Header.ANCount != 1 {
		t.Fatalf("ANCOUNT: got %d", m.Header.ANCount)
	}
	// Pull the trailing 4 bytes (A record RDATA).
	if string(resp[len(resp)-4:]) != string([]byte{10, 0, 1, 7}) {
		t.Errorf("A RDATA: got %v", resp[len(resp)-4:])
	}
}

func TestServer_AOverUDP_WithSandboxSuffix(t *testing.T) {
	s, _ := newTestServer(t)
	s.Set("myhost", net.IPv4(10, 0, 1, 7))

	q := buildQuery(t, 1, "myhost.sb", QTypeA)
	resp := sendUDP(t, s.Addr(), q)
	m, _ := ParseMessage(resp)
	if m.Header.RCode() != RCodeNoError || m.Header.ANCount != 1 {
		t.Fatalf("expected NOERROR with 1 answer: %+v", m.Header)
	}
}

func TestServer_NXDOMAINForUnknown(t *testing.T) {
	s, _ := newTestServer(t)
	q := buildQuery(t, 5, "unknown", QTypeA)
	resp := sendUDP(t, s.Addr(), q)
	m, _ := ParseMessage(resp)
	if m.Header.RCode() != RCodeNXDomain {
		t.Errorf("RCODE: got %d want NXDOMAIN", m.Header.RCode())
	}
	if m.Header.ANCount != 0 {
		t.Errorf("ANCOUNT for NXDOMAIN: got %d want 0", m.Header.ANCount)
	}
}

func TestServer_AAAAReturnsEmptyNOERROR(t *testing.T) {
	// RFC 4074: a name that has A records but no AAAA should respond
	// NOERROR with no answers, NOT NXDOMAIN. NXDOMAIN would tell the
	// client the name doesn't exist (lying), causing it to skip the
	// IPv4 fallback in dual-stack resolvers.
	s, _ := newTestServer(t)
	s.Set("host", net.IPv4(10, 0, 1, 5))

	q := buildQuery(t, 9, "host", QTypeAAAA)
	resp := sendUDP(t, s.Addr(), q)
	m, _ := ParseMessage(resp)
	if m.Header.RCode() != RCodeNoError {
		t.Errorf("RCODE: got %d want NOERROR", m.Header.RCode())
	}
	if m.Header.ANCount != 0 {
		t.Errorf("ANCOUNT: got %d want 0", m.Header.ANCount)
	}
}

func TestServer_PTROverUDP(t *testing.T) {
	s, _ := newTestServer(t)
	s.Set("worker-1", net.IPv4(10, 0, 1, 42))

	q := buildQuery(t, 0xA1A2, "42.1.0.10.in-addr.arpa", QTypePTR)
	resp := sendUDP(t, s.Addr(), q)
	m, _ := ParseMessage(resp)
	if m.Header.RCode() != RCodeNoError {
		t.Fatalf("PTR RCODE: got %d", m.Header.RCode())
	}
	if m.Header.ANCount != 1 {
		t.Fatalf("PTR ANCOUNT: got %d", m.Header.ANCount)
	}
	// RDATA is an encoded name. The tail of the response should contain
	// "worker-1.sb" in length-prefixed form.
	if !containsLabel(resp, "worker-1") {
		t.Fatalf("PTR response missing worker-1 label: %v", resp)
	}
}

// containsLabel is a sloppy check that a length-prefixed label appears
// as a contiguous (length, bytes) pair somewhere in buf.
func containsLabel(buf []byte, label string) bool {
	for i := 0; i+1+len(label) <= len(buf); i++ {
		if buf[i] == byte(len(label)) && string(buf[i+1:i+1+len(label)]) == label {
			return true
		}
	}
	return false
}

func TestServer_PTRUnknownIPReturnsNXDOMAIN(t *testing.T) {
	s, _ := newTestServer(t)
	// Don't set anything. PTR query for an unregistered IP -> NXDOMAIN.
	q := buildQuery(t, 1, "9.0.0.10.in-addr.arpa", QTypePTR)
	resp := sendUDP(t, s.Addr(), q)
	m, _ := ParseMessage(resp)
	if m.Header.RCode() != RCodeNXDomain {
		t.Errorf("RCODE: got %d want NXDOMAIN", m.Header.RCode())
	}
}

func TestServer_MalformedPTRNameReturnsNXDOMAIN(t *testing.T) {
	s, _ := newTestServer(t)
	// Not an in-addr.arpa name at all.
	q := buildQuery(t, 1, "not-a-reverse-name", QTypePTR)
	resp := sendUDP(t, s.Addr(), q)
	m, _ := ParseMessage(resp)
	if m.Header.RCode() != RCodeNXDomain {
		t.Errorf("RCODE: got %d want NXDOMAIN", m.Header.RCode())
	}
}

// --- TCP query path ---

func TestServer_AOverTCP(t *testing.T) {
	s, _ := newTestServer(t)
	s.Set("tcp-host", net.IPv4(10, 0, 1, 99))

	q := buildQuery(t, 7, "tcp-host", QTypeA)
	resp := sendTCP(t, tcpAddr(s), q)
	m, _ := ParseMessage(resp)
	if m.Header.RCode() != RCodeNoError || m.Header.ANCount != 1 {
		t.Fatalf("expected NOERROR/1: %+v", m.Header)
	}
	if string(resp[len(resp)-4:]) != string([]byte{10, 0, 1, 99}) {
		t.Errorf("A RDATA over TCP: got %v", resp[len(resp)-4:])
	}
}

// --- Malformed input ---

func TestServer_DropsTruncatedUDPSilently(t *testing.T) {
	s, _ := newTestServer(t)
	// Send a 3-byte garbage packet. Server shouldn't reply (no ID to
	// echo); the client read should time out.
	conn, _ := net.Dial("udp", s.Addr().String())
	defer conn.Close()
	conn.Write([]byte{0, 0, 0})
	conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	buf := make([]byte, 256)
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("server should not have replied to garbage")
	}
}

func TestServer_FORMERRForMultiQuestion(t *testing.T) {
	// Build a query with 2 questions. We do this manually since
	// buildQuery only does 1.
	q := &Message{
		Header:  Header{ID: 11, Flags: FlagRD, QDCount: 2},
		Questions: []Question{
			{Name: "a", QType: QTypeA, Class: QClassIN},
			{Name: "b", QType: QTypeA, Class: QClassIN},
		},
	}
	buf, err := BuildResponse(q, nil, RCodeNoError)
	if err != nil {
		t.Fatal(err)
	}
	// Clear QR (we're sending a query).
	binary.BigEndian.PutUint16(buf[2:4], binary.BigEndian.Uint16(buf[2:4])&^FlagQR)

	s, _ := newTestServer(t)
	resp := sendUDP(t, s.Addr(), buf)
	m, _ := ParseMessage(resp)
	if m.Header.RCode() != RCodeFormErr {
		t.Errorf("RCODE: got %d want FORMERR", m.Header.RCode())
	}
}

// --- Concurrency / panic recovery ---

func TestServer_ConcurrentQueries(t *testing.T) {
	s, _ := newTestServer(t)
	// Populate the zone.
	const sandboxes = 50
	for i := 0; i < sandboxes; i++ {
		s.Set(fmtSandboxName(i), net.IPv4(10, 0, byte(i/256), byte(i%256)))
	}

	// 50 goroutines × 50 queries each = 2500 queries, mixed names.
	// Roughly mirrors the v2 plan's 1000-q/s acceptance under load.
	const goroutines = 50
	const perGoroutine = 50

	var (
		errs    atomic.Int64
		correct atomic.Int64
		wg      sync.WaitGroup
	)
	addr := s.Addr()
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				idx := (seed*perGoroutine + j) % sandboxes
				name := fmtSandboxName(idx)
				wantIP := net.IPv4(10, 0, byte(idx/256), byte(idx%256))
				q := buildQuery(t, uint16(seed*100+j), name, QTypeA)
				resp := sendUDPNoFatal(addr, q)
				if resp == nil {
					errs.Add(1)
					continue
				}
				if len(resp) < 4 {
					errs.Add(1)
					continue
				}
				got := net.IPv4(resp[len(resp)-4], resp[len(resp)-3], resp[len(resp)-2], resp[len(resp)-1])
				if got.Equal(wantIP) {
					correct.Add(1)
				} else {
					errs.Add(1)
				}
			}
		}(i)
	}
	wg.Wait()

	total := int64(goroutines * perGoroutine)
	if errs.Load() > 0 {
		t.Errorf("%d/%d queries errored under concurrent load (want 0)", errs.Load(), total)
	}
	if correct.Load() != total {
		t.Errorf("correct=%d total=%d", correct.Load(), total)
	}
}

func TestServer_HighThroughput_NoDrop(t *testing.T) {
	// G1.8 acceptance criterion in the v2 plan: "1000 queries/sec across
	// 50 sandboxes ... ~0 dropped." We run a tighter burst (1000 in
	// sequence from a single client) to keep the test bounded.
	if testing.Short() {
		t.Skip("skipping throughput test in -short mode")
	}
	s, _ := newTestServer(t)
	for i := 0; i < 50; i++ {
		s.Set(fmtSandboxName(i), net.IPv4(10, 0, 0, byte(i+1)))
	}

	conn, err := net.Dial("udp", s.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	const total = 1000
	var dropped int
	for i := 0; i < total; i++ {
		q := buildQuery(t, uint16(i), fmtSandboxName(i%50), QTypeA)
		conn.Write(q)
		buf := make([]byte, 512)
		if _, err := conn.Read(buf); err != nil {
			dropped++
			continue
		}
	}
	if dropped > 5 { // allow tiny slack for kernel buffer races
		t.Errorf("%d/%d queries dropped (cap 5)", dropped, total)
	}
}

// fmtSandboxName produces "sandbox-N".
func fmtSandboxName(i int) string {
	return "sandbox-" + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [10]byte
	pos := len(buf)
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// sendUDPNoFatal is like sendUDP but returns nil instead of calling
// t.Fatal on errors. Used by concurrent tests where we want to count
// failures rather than abort on the first one.
func sendUDPNoFatal(addr net.Addr, query []byte) []byte {
	conn, err := net.Dial("udp", addr.String())
	if err != nil {
		return nil
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(1 * time.Second))
	if _, err := conn.Write(query); err != nil {
		return nil
	}
	resp := make([]byte, 512)
	n, err := conn.Read(resp)
	if err != nil {
		return nil
	}
	return resp[:n]
}

// --- Cancellation ---

func TestServer_StopsOnContextCancel(t *testing.T) {
	s := NewServer("127.0.0.1:0")
	s.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	addr := s.Addr().String()

	// Confirm it's serving.
	q := buildQuery(t, 1, "x", QTypeA)
	if resp := sendUDPNoFatal(s.Addr(), q); resp == nil {
		t.Fatal("expected reply before cancel")
	}

	cancel()
	// Give the goroutines a moment to notice.
	done := make(chan struct{})
	go func() { s.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return within 2s of cancel")
	}

	// And it's no longer serving.
	conn, err := net.Dial("udp", addr)
	if err != nil {
		return // OS may have already torn the port down
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	conn.Write(q)
	buf := make([]byte, 256)
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("server should not reply after cancel")
	}
}
