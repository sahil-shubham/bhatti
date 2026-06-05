// Package dns implements a minimal DNS responder for bhatti's per-user
// sandbox networks. Subset of RFC 1035: A and PTR record lookups, with
// AAAA queries returning NOERROR/no-answers (we don't ship IPv6).
// G1.1 of PLAN-bhatti-v2.md.
//
// The wire format implementation is hand-rolled rather than using
// miekg/dns. The substrate accumulates dependencies slowly by design
// (P5/P6 of the v2 plan); the DNS surface we need is small and stable
// enough that ~200 LOC of explicit encoding is worth more than a
// 150KB library we'd reach into for 5% of its features.
package dns

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
)

// QType: query types we recognise. RFC 1035 §3.2.2.
const (
	QTypeA    uint16 = 1  // IPv4 address
	QTypePTR  uint16 = 12 // reverse-lookup (IP -> name)
	QTypeAAAA uint16 = 28 // IPv6 address (we serve NOERROR/no-answers)
)

// QClass: query classes. We only handle IN (internet).
const (
	QClassIN uint16 = 1
)

// RCode: response codes. RFC 1035 §4.1.1.
const (
	RCodeNoError  uint8 = 0
	RCodeFormErr  uint8 = 1
	RCodeServFail uint8 = 2
	RCodeNXDomain uint8 = 3
)

// Header is the 12-byte DNS message header.
//
//	0  1  2  3  4  5  6  7  8  9 10 11 12 13 14 15
//	+--+--+--+--+--+--+--+--+--+--+--+--+--+--+--+--+
//	|                       ID                      |
//	+--+--+--+--+--+--+--+--+--+--+--+--+--+--+--+--+
//	|QR|   Opcode  |AA|TC|RD|RA|   Z   |   RCODE   |
//	+--+--+--+--+--+--+--+--+--+--+--+--+--+--+--+--+
//	|                    QDCOUNT                    |
//	+--+--+--+--+--+--+--+--+--+--+--+--+--+--+--+--+
//	|                    ANCOUNT                    |
//	+--+--+--+--+--+--+--+--+--+--+--+--+--+--+--+--+
//	|                    NSCOUNT                    |
//	+--+--+--+--+--+--+--+--+--+--+--+--+--+--+--+--+
//	|                    ARCOUNT                    |
//	+--+--+--+--+--+--+--+--+--+--+--+--+--+--+--+--+
type Header struct {
	ID      uint16
	Flags   uint16
	QDCount uint16
	ANCount uint16
	NSCount uint16
	ARCount uint16
}

// Header flag bits.
const (
	FlagQR uint16 = 1 << 15 // response (set on outgoing)
	FlagAA uint16 = 1 << 10 // authoritative answer
	FlagTC uint16 = 1 << 9  // truncated
	FlagRD uint16 = 1 << 8  // recursion desired (from query)
	FlagRA uint16 = 1 << 7  // recursion available (we set 0 — we don't recurse)
)

// Question is one entry in the question section.
type Question struct {
	Name  string // dotted, lower-case, no trailing dot
	QType uint16
	Class uint16
}

// Answer is one entry in the answer section.
type Answer struct {
	Name  string
	Type  uint16
	Class uint16
	TTL   uint32
	RData []byte // wire-format RDATA (for A: 4-byte IP; for PTR: encoded name)
}

// Message is a parsed DNS message.
type Message struct {
	Header    Header
	Questions []Question
	Answers   []Answer
}

// RCode extracts the response code from the header flags.
func (h Header) RCode() uint8 {
	return uint8(h.Flags & 0x000F)
}

// SetRCode sets the response code while preserving other flag bits.
func (h *Header) SetRCode(code uint8) {
	h.Flags = (h.Flags &^ 0x000F) | uint16(code&0x0F)
}

// ParseMessage decodes a wire-format DNS message. Only the header and
// question section are parsed; answer/authority/additional sections are
// ignored (we only ever parse incoming queries, which carry questions).
//
// Compression pointers (RFC 1035 §4.1.4) are followed during name parse.
func ParseMessage(buf []byte) (*Message, error) {
	if len(buf) < 12 {
		return nil, errors.New("dns: message too short for header")
	}
	m := &Message{
		Header: Header{
			ID:      binary.BigEndian.Uint16(buf[0:2]),
			Flags:   binary.BigEndian.Uint16(buf[2:4]),
			QDCount: binary.BigEndian.Uint16(buf[4:6]),
			ANCount: binary.BigEndian.Uint16(buf[6:8]),
			NSCount: binary.BigEndian.Uint16(buf[8:10]),
			ARCount: binary.BigEndian.Uint16(buf[10:12]),
		},
	}
	offset := 12
	for i := 0; i < int(m.Header.QDCount); i++ {
		name, newOffset, err := parseName(buf, offset)
		if err != nil {
			return nil, fmt.Errorf("dns: question %d: %w", i, err)
		}
		offset = newOffset
		if offset+4 > len(buf) {
			return nil, errors.New("dns: truncated question fields")
		}
		q := Question{
			Name:  name,
			QType: binary.BigEndian.Uint16(buf[offset : offset+2]),
			Class: binary.BigEndian.Uint16(buf[offset+2 : offset+4]),
		}
		offset += 4
		m.Questions = append(m.Questions, q)
	}
	return m, nil
}

// parseName decodes a domain name starting at buf[offset]. Returns the
// dotted name (lower-cased, no trailing dot), the offset past the name,
// and any error. Compression pointers are followed; cycles abort.
func parseName(buf []byte, offset int) (string, int, error) {
	var (
		parts          []string
		curOffset      = offset
		nextOffset     = -1 // set when we follow a pointer; final offset is past the pointer, not where it points
		followedHops   = 0
		maxFollowedHops = 16 // arbitrary cycle guard
	)
	for {
		if curOffset >= len(buf) {
			return "", 0, errors.New("dns: name runs past end of message")
		}
		b := buf[curOffset]
		// Pointer: top two bits = 11.
		if b&0xC0 == 0xC0 {
			if curOffset+1 >= len(buf) {
				return "", 0, errors.New("dns: truncated pointer")
			}
			followedHops++
			if followedHops > maxFollowedHops {
				return "", 0, errors.New("dns: compression pointer loop")
			}
			ptr := int(binary.BigEndian.Uint16(buf[curOffset:curOffset+2]) & 0x3FFF)
			if nextOffset == -1 {
				nextOffset = curOffset + 2
			}
			curOffset = ptr
			continue
		}
		// Length byte for next label (0..63). 0 = end of name.
		if b&0xC0 != 0 {
			return "", 0, fmt.Errorf("dns: reserved bits set in label length 0x%02x", b)
		}
		labelLen := int(b)
		curOffset++
		if labelLen == 0 {
			break
		}
		if labelLen > 63 {
			return "", 0, fmt.Errorf("dns: label too long: %d", labelLen)
		}
		if curOffset+labelLen > len(buf) {
			return "", 0, errors.New("dns: label runs past end of message")
		}
		parts = append(parts, strings.ToLower(string(buf[curOffset:curOffset+labelLen])))
		curOffset += labelLen
	}
	if nextOffset == -1 {
		nextOffset = curOffset
	}
	return strings.Join(parts, "."), nextOffset, nil
}

// encodeName writes a domain name in DNS wire format (no compression).
// Empty name is encoded as a single 0 byte (the root).
func encodeName(name string) ([]byte, error) {
	if name == "" {
		return []byte{0}, nil
	}
	parts := strings.Split(name, ".")
	out := make([]byte, 0, len(name)+2)
	for _, p := range parts {
		if p == "" {
			return nil, fmt.Errorf("dns: empty label in %q", name)
		}
		if len(p) > 63 {
			return nil, fmt.Errorf("dns: label %q too long", p)
		}
		out = append(out, byte(len(p)))
		out = append(out, []byte(p)...)
	}
	out = append(out, 0)
	return out, nil
}

// BuildResponse constructs the wire-format response for a parsed query.
// answers carries the answers to include; rcode is the response code
// (NOERROR for ordinary success/no-answer, NXDOMAIN for unknown name,
// etc.). The question section is echoed back per RFC 1035 convention.
func BuildResponse(query *Message, answers []Answer, rcode uint8) ([]byte, error) {
	hdr := Header{
		ID:      query.Header.ID,
		Flags:   FlagQR | FlagAA, // we are authoritative, we don't recurse
		QDCount: uint16(len(query.Questions)),
		ANCount: uint16(len(answers)),
	}
	// Preserve the RD bit from the query so clients see their request
	// state echoed (some clients log discrepancies as errors).
	hdr.Flags |= query.Header.Flags & FlagRD
	hdr.SetRCode(rcode)

	out := make([]byte, 12)
	binary.BigEndian.PutUint16(out[0:2], hdr.ID)
	binary.BigEndian.PutUint16(out[2:4], hdr.Flags)
	binary.BigEndian.PutUint16(out[4:6], hdr.QDCount)
	binary.BigEndian.PutUint16(out[6:8], hdr.ANCount)
	binary.BigEndian.PutUint16(out[8:10], hdr.NSCount)
	binary.BigEndian.PutUint16(out[10:12], hdr.ARCount)

	for _, q := range query.Questions {
		nb, err := encodeName(q.Name)
		if err != nil {
			return nil, err
		}
		out = append(out, nb...)
		var qbuf [4]byte
		binary.BigEndian.PutUint16(qbuf[0:2], q.QType)
		binary.BigEndian.PutUint16(qbuf[2:4], q.Class)
		out = append(out, qbuf[:]...)
	}

	for _, a := range answers {
		nb, err := encodeName(a.Name)
		if err != nil {
			return nil, err
		}
		out = append(out, nb...)
		var hbuf [10]byte
		binary.BigEndian.PutUint16(hbuf[0:2], a.Type)
		binary.BigEndian.PutUint16(hbuf[2:4], a.Class)
		binary.BigEndian.PutUint32(hbuf[4:8], a.TTL)
		binary.BigEndian.PutUint16(hbuf[8:10], uint16(len(a.RData)))
		out = append(out, hbuf[:]...)
		out = append(out, a.RData...)
	}
	return out, nil
}

// ARecordRData returns the 4-byte RDATA for an A record.
func ARecordRData(ip net.IP) ([]byte, error) {
	ip4 := ip.To4()
	if ip4 == nil {
		return nil, fmt.Errorf("dns: %v is not an IPv4 address", ip)
	}
	return []byte(ip4), nil
}

// PTRRecordRData encodes a PTR record's RDATA (the target domain name).
func PTRRecordRData(name string) ([]byte, error) {
	return encodeName(name)
}

// IPv4ToInAddrArpa converts an IP to its reverse-lookup name.
//
//	10.0.1.2 -> "2.1.0.10.in-addr.arpa"
func IPv4ToInAddrArpa(ip net.IP) (string, error) {
	ip4 := ip.To4()
	if ip4 == nil {
		return "", fmt.Errorf("dns: not IPv4: %v", ip)
	}
	return fmt.Sprintf("%d.%d.%d.%d.in-addr.arpa", ip4[3], ip4[2], ip4[1], ip4[0]), nil
}

// InAddrArpaToIPv4 is the inverse of IPv4ToInAddrArpa. Returns
// ok=false if name isn't a well-formed in-addr.arpa name.
func InAddrArpaToIPv4(name string) (net.IP, bool) {
	const suffix = ".in-addr.arpa"
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	if !strings.HasSuffix(name, suffix) {
		return nil, false
	}
	prefix := strings.TrimSuffix(name, suffix)
	parts := strings.Split(prefix, ".")
	if len(parts) != 4 {
		return nil, false
	}
	// Reverse the octets back into normal order.
	var ip [4]byte
	for i, p := range parts {
		var v int
		if _, err := fmt.Sscanf(p, "%d", &v); err != nil || v < 0 || v > 255 {
			return nil, false
		}
		ip[3-i] = byte(v)
	}
	return net.IPv4(ip[0], ip[1], ip[2], ip[3]).To4(), true
}
