package dns

import (
	"bytes"
	"encoding/binary"
	"net"
	"reflect"
	"testing"
)

// Wire-format encode/decode tests. The RFC 1035 message structure is
// load-bearing for every query the responder handles; bugs here cause
// resolver compatibility issues that are hard to diagnose at runtime.

// TestEncodeName covers the basic label-length-prefix encoding.
func TestEncodeName(t *testing.T) {
	tests := []struct {
		in   string
		want []byte
	}{
		{"", []byte{0}},
		{"a", []byte{1, 'a', 0}},
		{"foo", []byte{3, 'f', 'o', 'o', 0}},
		{"foo.bar", []byte{3, 'f', 'o', 'o', 3, 'b', 'a', 'r', 0}},
		{"a.b.c", []byte{1, 'a', 1, 'b', 1, 'c', 0}},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := encodeName(tt.in)
			if err != nil {
				t.Fatalf("encodeName(%q): %v", tt.in, err)
			}
			if !bytes.Equal(got, tt.want) {
				t.Fatalf("got %v want %v", got, tt.want)
			}
		})
	}
}

// TestEncodeNameRejectsBadInput catches each of the validation paths
// (empty label, oversized label). 64-char labels must fail (RFC 1035
// caps at 63).
func TestEncodeNameRejectsBadInput(t *testing.T) {
	tests := []string{
		".foo",                          // leading dot -> empty label
		"foo..bar",                      // double dot -> empty label
		string(make([]byte, 64)) + ".x", // label > 63 chars
	}
	for _, in := range tests {
		t.Run(in, func(t *testing.T) {
			if _, err := encodeName(in); err == nil {
				t.Fatalf("encodeName(%q) should have errored", in)
			}
		})
	}
}

// TestParseName covers basic label parsing including a multi-label name.
func TestParseName(t *testing.T) {
	tests := []struct {
		buf  []byte
		want string
	}{
		{[]byte{0}, ""},
		{[]byte{3, 'f', 'o', 'o', 0}, "foo"},
		{[]byte{3, 'f', 'o', 'o', 3, 'b', 'a', 'r', 0}, "foo.bar"},
		// Lower-cased on parse: case-insensitivity per RFC 1035 §2.3.3.
		{[]byte{3, 'F', 'O', 'O', 0}, "foo"},
	}
	for _, tt := range tests {
		t.Run(string(tt.buf), func(t *testing.T) {
			got, off, err := parseName(tt.buf, 0)
			if err != nil {
				t.Fatalf("parseName: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
			if off != len(tt.buf) {
				t.Fatalf("offset got %d want %d", off, len(tt.buf))
			}
		})
	}
}

// TestParseNameFollowsCompressionPointer: standard DNS message
// compression. The name in the question is "foo.bar"; a later record
// references it via a pointer to offset 12.
func TestParseNameFollowsCompressionPointer(t *testing.T) {
	// Layout:
	//   offset 0..11: dummy header padding
	//   offset 12: encoded "foo.bar" (9 bytes: 3 f o o 3 b a r 0)
	//   offset 21: pointer back to offset 12 (0xC0 0x0C)
	buf := make([]byte, 12) // padding to mimic header
	buf = append(buf,
		3, 'f', 'o', 'o', 3, 'b', 'a', 'r', 0,
		0xC0, 0x0C, // pointer to offset 12
	)

	name, off, err := parseName(buf, 21)
	if err != nil {
		t.Fatalf("parseName: %v", err)
	}
	if name != "foo.bar" {
		t.Fatalf("got %q want foo.bar", name)
	}
	if off != 23 {
		t.Fatalf("offset got %d want 23 (past the 2-byte pointer)", off)
	}
}

// TestParseNameRejectsPointerLoop catches a cycle in compression
// pointers. A malicious or buggy sender can craft a self-referential
// pointer that would otherwise spin forever.
func TestParseNameRejectsPointerLoop(t *testing.T) {
	buf := make([]byte, 14)
	// At offset 12: pointer to offset 12 (self-reference).
	buf[12] = 0xC0
	buf[13] = 0x0C
	_, _, err := parseName(buf, 12)
	if err == nil {
		t.Fatal("expected error for pointer loop")
	}
}

// TestParseMessage_QueryRoundTrip: build a query manually, parse it,
// verify the parsed structure matches.
func TestParseMessage_QueryRoundTrip(t *testing.T) {
	// Build a query for "myhost" A IN.
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, uint16(0x1234)) // ID
	binary.Write(&buf, binary.BigEndian, FlagRD)         // flags: RD set
	binary.Write(&buf, binary.BigEndian, uint16(1))      // QDCOUNT
	binary.Write(&buf, binary.BigEndian, uint16(0))      // ANCOUNT
	binary.Write(&buf, binary.BigEndian, uint16(0))      // NSCOUNT
	binary.Write(&buf, binary.BigEndian, uint16(0))      // ARCOUNT
	nb, _ := encodeName("myhost")
	buf.Write(nb)
	binary.Write(&buf, binary.BigEndian, QTypeA)
	binary.Write(&buf, binary.BigEndian, QClassIN)

	m, err := ParseMessage(buf.Bytes())
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	if m.Header.ID != 0x1234 {
		t.Errorf("ID: got 0x%x want 0x1234", m.Header.ID)
	}
	if m.Header.Flags&FlagRD == 0 {
		t.Errorf("RD flag should be set")
	}
	if len(m.Questions) != 1 {
		t.Fatalf("Questions: got %d want 1", len(m.Questions))
	}
	q := m.Questions[0]
	if q.Name != "myhost" || q.QType != QTypeA || q.Class != QClassIN {
		t.Errorf("question: %+v", q)
	}
}

// TestBuildResponse_A: build an A-record response for a parsed query,
// re-parse to verify the wire shape.
func TestBuildResponse_A(t *testing.T) {
	query := &Message{
		Header: Header{
			ID:      0xABCD,
			Flags:   FlagRD,
			QDCount: 1,
		},
		Questions: []Question{
			{Name: "myhost", QType: QTypeA, Class: QClassIN},
		},
	}
	rdata, err := ARecordRData(net.IPv4(10, 0, 1, 2))
	if err != nil {
		t.Fatal(err)
	}
	answers := []Answer{
		{Name: "myhost", Type: QTypeA, Class: QClassIN, TTL: 5, RData: rdata},
	}
	resp, err := BuildResponse(query, answers, RCodeNoError)
	if err != nil {
		t.Fatal(err)
	}

	// Re-parse and verify the header / counts.
	parsed, err := ParseMessage(resp)
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	if parsed.Header.ID != 0xABCD {
		t.Errorf("ID round-trip: got 0x%x want 0xABCD", parsed.Header.ID)
	}
	if parsed.Header.Flags&FlagQR == 0 {
		t.Error("QR should be set in response")
	}
	if parsed.Header.Flags&FlagAA == 0 {
		t.Error("AA should be set (we are authoritative)")
	}
	if parsed.Header.Flags&FlagRD == 0 {
		t.Error("RD should be preserved from query")
	}
	if parsed.Header.RCode() != RCodeNoError {
		t.Errorf("RCODE: got %d want %d", parsed.Header.RCode(), RCodeNoError)
	}
	if parsed.Header.QDCount != 1 || parsed.Header.ANCount != 1 {
		t.Errorf("counts: QD=%d AN=%d want 1, 1", parsed.Header.QDCount, parsed.Header.ANCount)
	}

	// Question section round-trips.
	if len(parsed.Questions) != 1 || parsed.Questions[0].Name != "myhost" {
		t.Fatalf("question section: %+v", parsed.Questions)
	}

	// The answer section is at offset 12 + (encoded question + 4).
	// Rather than re-parsing the answer section (which our ParseMessage
	// doesn't do), pull the RDATA bytes from the back of the buffer.
	if len(resp) < 4 {
		t.Fatal("response too short")
	}
	if !bytes.Equal(resp[len(resp)-4:], []byte{10, 0, 1, 2}) {
		t.Errorf("A RDATA: got %v want [10, 0, 1, 2]", resp[len(resp)-4:])
	}
}

// TestBuildResponse_NXDOMAIN: a query for an unknown name returns
// NXDOMAIN with no answers. Common path; resolvers care that the RCODE
// is exactly 3 (NameError) so they cache the negative result.
func TestBuildResponse_NXDOMAIN(t *testing.T) {
	query := &Message{
		Header:    Header{ID: 1, QDCount: 1},
		Questions: []Question{{Name: "unknown", QType: QTypeA, Class: QClassIN}},
	}
	resp, err := BuildResponse(query, nil, RCodeNXDomain)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := ParseMessage(resp)
	if parsed.Header.RCode() != RCodeNXDomain {
		t.Fatalf("RCODE: got %d want NXDOMAIN(%d)", parsed.Header.RCode(), RCodeNXDomain)
	}
	if parsed.Header.ANCount != 0 {
		t.Errorf("ANCOUNT: got %d want 0 for NXDOMAIN", parsed.Header.ANCount)
	}
}

// TestIPv4ToInAddrArpa_Roundtrip: 10.0.1.2 <-> "2.1.0.10.in-addr.arpa".
func TestIPv4ToInAddrArpa_Roundtrip(t *testing.T) {
	tests := []struct {
		ip   net.IP
		name string
	}{
		{net.IPv4(10, 0, 1, 2), "2.1.0.10.in-addr.arpa"},
		{net.IPv4(127, 0, 0, 1), "1.0.0.127.in-addr.arpa"},
		{net.IPv4(192, 168, 1, 254), "254.1.168.192.in-addr.arpa"},
	}
	for _, tt := range tests {
		t.Run(tt.ip.String(), func(t *testing.T) {
			name, err := IPv4ToInAddrArpa(tt.ip)
			if err != nil {
				t.Fatal(err)
			}
			if name != tt.name {
				t.Fatalf("got %q want %q", name, tt.name)
			}
			ip, ok := InAddrArpaToIPv4(name)
			if !ok {
				t.Fatalf("InAddrArpaToIPv4(%q) failed", name)
			}
			if !ip.Equal(tt.ip) {
				t.Fatalf("roundtrip: got %v want %v", ip, tt.ip)
			}
		})
	}
}

// TestInAddrArpaToIPv4_RejectsBadInput catches malformed reverse names.
// A future caller that builds queries by string concatenation must not
// be able to confuse our parser.
func TestInAddrArpaToIPv4_RejectsBadInput(t *testing.T) {
	bad := []string{
		"not-a-reverse-name",
		"1.2.3.in-addr.arpa",       // 3 octets, not 4
		"1.2.3.4.5.in-addr.arpa",   // 5 octets
		"abc.def.ghi.jkl.in-addr.arpa", // non-numeric
		"-1.0.0.0.in-addr.arpa",    // negative
		"256.0.0.0.in-addr.arpa",   // > 255
	}
	for _, name := range bad {
		t.Run(name, func(t *testing.T) {
			if _, ok := InAddrArpaToIPv4(name); ok {
				t.Fatalf("InAddrArpaToIPv4(%q) should have failed", name)
			}
		})
	}
}

// TestParseMessage_RejectsTruncated ensures we don't crash on
// short/malformed packets. Resolvers (and attackers) send all kinds
// of garbage; the responder should return FORMERR rather than panic.
func TestParseMessage_RejectsTruncated(t *testing.T) {
	tests := [][]byte{
		nil,                    // empty
		make([]byte, 0),
		make([]byte, 5),        // shorter than 12-byte header
		make([]byte, 11),       // one byte shy of header
	}
	for i, buf := range tests {
		t.Run(string(rune('a'+i)), func(t *testing.T) {
			if _, err := ParseMessage(buf); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

// TestParseMessage_HandlesQueryWithoutQuestion catches QDCOUNT=0 (which
// some opportunistic probes send). We should parse the header cleanly
// and produce an empty Questions slice.
func TestParseMessage_HandlesQueryWithoutQuestion(t *testing.T) {
	buf := make([]byte, 12) // 12-byte header, all zero -> QDCOUNT=0
	m, err := ParseMessage(buf)
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	if len(m.Questions) != 0 {
		t.Fatalf("expected no questions, got %d", len(m.Questions))
	}
}

// TestEncodeName_LongLabel: 63-byte label is OK, 64 is rejected.
func TestEncodeName_LongLabel(t *testing.T) {
	ok63 := string(bytes.Repeat([]byte{'a'}, 63))
	if _, err := encodeName(ok63); err != nil {
		t.Errorf("63-char label should be valid: %v", err)
	}
	bad64 := string(bytes.Repeat([]byte{'a'}, 64))
	if _, err := encodeName(bad64); err == nil {
		t.Error("64-char label should be rejected")
	}
}

// TestHeaderFlagAccessors: SetRCode preserves the non-rcode bits.
func TestHeaderFlagAccessors(t *testing.T) {
	h := Header{Flags: FlagQR | FlagAA | FlagRD}
	h.SetRCode(RCodeNXDomain)
	if h.Flags&FlagQR == 0 || h.Flags&FlagAA == 0 || h.Flags&FlagRD == 0 {
		t.Errorf("SetRCode clobbered other flags: 0x%x", h.Flags)
	}
	if h.RCode() != RCodeNXDomain {
		t.Errorf("RCode: got %d want NXDOMAIN", h.RCode())
	}
	h.SetRCode(RCodeNoError)
	if h.RCode() != RCodeNoError {
		t.Errorf("reset RCode: got %d", h.RCode())
	}
}

// TestBuildResponse_PreservesArbitraryQuestions: even if the question
// section has multiple entries (rare in practice but RFC-legal), we
// echo all of them. A client noticing QDCOUNT mismatch flags it as
// a protocol error.
func TestBuildResponse_PreservesArbitraryQuestions(t *testing.T) {
	q := &Message{
		Header:  Header{ID: 7, QDCount: 2},
		Questions: []Question{
			{Name: "a", QType: QTypeA, Class: QClassIN},
			{Name: "b", QType: QTypeA, Class: QClassIN},
		},
	}
	resp, err := BuildResponse(q, nil, RCodeNoError)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := ParseMessage(resp)
	if parsed.Header.QDCount != 2 || len(parsed.Questions) != 2 {
		t.Fatalf("QDCOUNT/Questions: %+v", parsed.Header)
	}
	if parsed.Questions[0].Name != "a" || parsed.Questions[1].Name != "b" {
		t.Fatalf("questions: %v", parsed.Questions)
	}
}

// TestARecordRData_RejectsIPv6: the helper must refuse non-IPv4
// addresses so we never produce an invalid 4-byte RDATA from a 16-byte
// IPv6 input.
func TestARecordRData_RejectsIPv6(t *testing.T) {
	if _, err := ARecordRData(net.ParseIP("::1")); err == nil {
		t.Fatal("expected error for IPv6 input")
	}
	// IPv4 in 16-byte form (most common net.IP shape) should still work
	// because To4() does the conversion.
	if _, err := ARecordRData(net.ParseIP("10.0.0.1")); err != nil {
		t.Fatalf("IPv4 should succeed: %v", err)
	}
}

// Self-test: reflect.DeepEqual sanity for the Question round-trip,
// catches scribbling bugs in parse/build.
func TestQuestionRoundTripDeepEqual(t *testing.T) {
	orig := Question{Name: "host", QType: QTypeA, Class: QClassIN}
	q := &Message{Header: Header{ID: 9, QDCount: 1}, Questions: []Question{orig}}
	wire, err := BuildResponse(q, nil, RCodeNoError)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := ParseMessage(wire)
	if !reflect.DeepEqual(parsed.Questions[0], orig) {
		t.Fatalf("got %+v want %+v", parsed.Questions[0], orig)
	}
}
