// Package gateway is bhatti's host-side egress gateway: the single point that
// enforces where a sandbox may connect (the private-range / SSRF guard + the
// per-sandbox egress policy) and, at L7, injects credentials on the guest's
// behalf. This file is the L4 guard — pure, arch-agnostic, VM-free logic that
// both the TSI egress filter and the virtio-net gateway share.
//
// Design: docs/internal/DESIGN-bhatti-v2-networking.md (§5.3) +
// docs/internal/DESIGN-bhatti-v2-secrets-and-trust.md (§3.6a). An omitted
// egress posture denies traffic; public access requires an explicit policy.
// Host, loopback, link-local, metadata and other owners' networks cannot be
// opened by allow rules.
package gateway

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// class is the trust classification of a destination address.
type class int

const (
	classPublic   class = iota // routable public internet
	classSoftDeny              // private (RFC-1918/ULA/CGNAT): denied unless an explicit allow-cidr opts in
	classHardDeny              // loopback/link-local/host/multicast/unspecified: never allowable
)

// nat64Prefix is the well-known NAT64 range; its low 32 bits embed an IPv4
// address (RFC 6052), a classic way to smuggle a denied v4 past a v6 check.
var nat64Prefix = netip.MustParsePrefix("64:ff9b::/96")

// cgnatPrefix is RFC-6598 carrier-grade NAT space. netip.Addr.IsPrivate does
// NOT cover it, and we use it for per-owner vnets, so treat it as private.
var cgnatPrefix = netip.MustParsePrefix("100.64.0.0/10")

// canonical unmaps IPv4-in-IPv6 (::ffff:a.b.c.d) and decodes NAT64 so the
// embedded IPv4 is classified as the v4 address it really is — closing the
// ::ffff:127.0.0.1 / 64:ff9b::7f00:1 bypasses.
func canonical(a netip.Addr) netip.Addr {
	a = a.Unmap()
	if a.Is6() && nat64Prefix.Contains(a) {
		b := a.As16()
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})
	}
	return a
}

// classify returns the trust class of a, after canonicalization. extraHardDeny
// carries deployment-specific never-allowable ranges (the host's own addresses,
// the daemon API, other tenants' vnet ranges).
func classify(a netip.Addr, extraHardDeny []netip.Prefix) class {
	a = canonical(a)
	if !a.IsValid() || a.IsUnspecified() || a.IsLoopback() ||
		a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsMulticast() || a.IsInterfaceLocalMulticast() {
		return classHardDeny // incl. 169.254.169.254 metadata (link-local unicast)
	}
	for _, p := range extraHardDeny {
		if p.Contains(a) {
			return classHardDeny
		}
	}
	if a.IsPrivate() || cgnatPrefix.Contains(a) { // RFC-1918 + ULA (fc00::/7) + CGNAT
		return classSoftDeny
	}
	return classPublic
}

// Posture is the default egress stance for destinations that aren't otherwise
// matched by an allow rule.
type Posture int

const (
	PostureDeny   Posture = iota // deny everything not explicitly allow-listed
	PosturePublic                // allow the public internet (not host/private/siblings)
)

// HostPattern matches a destination hostname: either an exact host or a
// left-anchored label wildcard ("*.stripe.com" matches api.stripe.com but NOT
// stripe.com.evil.com or evilstripe.com). Regex is deliberately not supported
// here (a loose regex is a bypass); a reviewed regex mode can be added later.
type HostPattern struct {
	exact  string // lowercased exact host; empty if wildcard
	suffix string // ".stripe.com" for "*.stripe.com"; empty if exact
}

// ParseHostPattern parses "api.stripe.com" or "*.stripe.com".
func ParseHostPattern(s string) (HostPattern, error) {
	s = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(s, ".")))
	if s == "" {
		return HostPattern{}, fmt.Errorf("empty host pattern")
	}
	if rest, ok := strings.CutPrefix(s, "*."); ok {
		if rest == "" || strings.Contains(rest, "*") {
			return HostPattern{}, fmt.Errorf("bad wildcard host pattern %q", s)
		}
		return HostPattern{suffix: "." + rest}, nil
	}
	if strings.Contains(s, "*") {
		return HostPattern{}, fmt.Errorf("unsupported wildcard host pattern %q (only leading *. )", s)
	}
	return HostPattern{exact: s}, nil
}

// Match reports whether host matches the pattern (case-insensitive, trailing
// dot ignored).
func (h HostPattern) Match(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if h.exact != "" {
		return host == h.exact
	}
	// suffix like ".stripe.com": require a real label before it (the leading dot
	// anchors the boundary, so "stripe.com.evil.com" and "evilstripe.com" fail).
	return len(host) > len(h.suffix) && strings.HasSuffix(host, h.suffix)
}

// String is the normalized pattern: "api.stripe.com" or "*.stripe.com".
func (h HostPattern) String() string {
	if h.exact != "" {
		return h.exact
	}
	return "*" + h.suffix
}

// Covers reports whether every host o matches is also matched by h, so a rule
// set that admits h admits o (a secret grant may only name hosts its sandbox's
// allow rules already let through).
func (h HostPattern) Covers(o HostPattern) bool {
	if o.exact != "" {
		return h.Match(o.exact)
	}
	// o is a wildcard: only a wildcard whose suffix ends o's is at least as broad.
	return h.suffix != "" && strings.HasSuffix(o.suffix, h.suffix)
}

// EgressPolicy is a sandbox's egress rule set, evaluated per connection. Order
// is fixed: hard-deny → siblings → allow-cidr → allow-host → soft-deny → default.
// SiblingSubnet, ExtraHardDeny and HostServices are assigned by netd, never by
// the wire policy.
type EgressPolicy struct {
	Default       Posture
	Siblings      Posture
	SiblingSubnet netip.Prefix
	AllowCIDRs    []netip.Prefix
	AllowHosts    []HostPattern
	ExtraHardDeny []netip.Prefix // host addrs, daemon API, gateway, other owners' vnets
	// HostServices are the daemon's own TCP listeners on host addresses. The
	// internet can already reach them, so a guest may too: that is how a
	// sandbox calls its node's bhatti API or a published URL. Only the
	// hard-deny on that address and port is lifted; the rest of the policy
	// still decides, and every other port on the host stays hard-denied.
	HostServices []netip.AddrPort
}

// AllowAllEgress allows a guest to reach public internet IPs, but not private
// ranges (including CGNAT), siblings, the host, the gateway, loopback,
// link-local addresses or cloud metadata; allow-cidr may opt into private
// ranges except destinations protected by netd's ExtraHardDeny.
func AllowAllEgress() *EgressPolicy {
	return &EgressPolicy{Default: PosturePublic}
}

// Verdict is the outcome of a policy check.
type Verdict struct {
	Allow  bool
	Reason string
}

func allow(reason string) Verdict { return Verdict{Allow: true, Reason: reason} }
func deny(reason string) Verdict  { return Verdict{Allow: false, Reason: reason} }

// hostAllowed reports whether host matches any AllowHosts pattern.
func (p *EgressPolicy) hostAllowed(host string) bool {
	for _, hp := range p.AllowHosts {
		if hp.Match(host) {
			return true
		}
	}
	return false
}

func cidrsContain(cidrs []netip.Prefix, a netip.Addr) bool {
	a = canonical(a)
	for _, c := range cidrs {
		if c.Contains(a) {
			return true
		}
	}
	return false
}

// Check decides whether a connection to (host, ip) is permitted. host is the
// name the guest asked for (may be empty for a literal-IP dial); ip is a
// resolved destination address.
func (p *EgressPolicy) Check(host string, ip netip.Addr) Verdict {
	return p.check(host, ip, p.ExtraHardDeny)
}

// CheckTCP is Check for a TCP connection to dst, which may be one of
// HostServices.
func (p *EgressPolicy) CheckTCP(host string, dst netip.AddrPort) Verdict {
	a := canonical(dst.Addr())
	for _, s := range p.HostServices {
		if s.Port() == dst.Port() && canonical(s.Addr()) == a {
			return p.check(host, a, withoutAddr(p.ExtraHardDeny, a))
		}
	}
	return p.check(host, dst.Addr(), p.ExtraHardDeny)
}

// withoutAddr is prefixes minus the single-address entries for a, so lifting a
// host service never lifts a wider range (another owner's subnet) around it.
func withoutAddr(prefixes []netip.Prefix, a netip.Addr) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(prefixes))
	for _, p := range prefixes {
		if p.IsSingleIP() && canonical(p.Addr()) == a {
			continue
		}
		out = append(out, p)
	}
	return out
}

func (p *EgressPolicy) check(host string, ip netip.Addr, extraHardDeny []netip.Prefix) Verdict {
	c := classify(ip, extraHardDeny)
	if c == classHardDeny {
		return deny("destination is in a hard-denied range (host/loopback/link-local/metadata)")
	}
	if p.SiblingSubnet.IsValid() && p.SiblingSubnet.Contains(canonical(ip)) {
		if p.Siblings == PosturePublic {
			return allow("siblings")
		}
		return deny("sibling access denied (use --allow-siblings to opt in)")
	}
	// Explicit allow-cidr opts back into an otherwise soft-denied private range.
	if cidrsContain(p.AllowCIDRs, ip) {
		return allow("allow-cidr")
	}
	if host != "" && p.hostAllowed(host) {
		// An allowed *name* that resolves into private space is a rebinding
		// attempt — refuse (allow-host is for reaching public services by name).
		if c == classSoftDeny {
			return deny("allowed host resolved into a private range (rebinding?)")
		}
		return allow("allow-host")
	}
	if c == classSoftDeny {
		return deny("private range denied by default (use --allow-cidr to opt in)")
	}
	// Public destination.
	if p.Default == PosturePublic {
		return allow("public")
	}
	return deny("not in egress allow-list (default deny)")
}

// Resolver resolves a hostname to addresses; pluggable so the vetting dialer can
// be tested without real DNS and so callers can force a trusted resolver.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// DeniedError is returned when every resolved address for a destination fails
// the guard (so callers can distinguish policy denial from a network error).
type DeniedError struct {
	Host   string
	Reason string
}

func (e *DeniedError) Error() string {
	return fmt.Sprintf("egress denied to %q: %s", e.Host, e.Reason)
}

// Dialer is a resolve-and-vet dialer: it resolves the destination, checks EVERY
// resolved address against the policy, and dials only a vetted address — closing
// the DNS-rebinding TOCTOU (the connected IP is always one we checked, never a
// re-resolved surprise). It does NOT pin a name to an IP across connections:
// each dial re-resolves, so CDNs/round-robin/failover work normally.
type Dialer struct {
	Policy   *EgressPolicy
	Resolver Resolver                                        // nil → net.DefaultResolver
	Net      *net.Dialer                                     // nil → &net.Dialer{}
	OnDeny   func(host string, ip netip.Addr, reason string) // optional audit hook

	// dialAddr connects to an already-vetted ip:port; a test seam (nil → Net).
	// Unexported so the vetting can never be bypassed by a caller.
	dialAddr func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (d *Dialer) dialOne(ctx context.Context, network, addr string) (net.Conn, error) {
	if d.dialAddr != nil {
		return d.dialAddr(ctx, network, addr)
	}
	nd := d.Net
	if nd == nil {
		nd = &net.Dialer{}
	}
	return nd.DialContext(ctx, network, addr)
}

func (d *Dialer) resolver() Resolver {
	if d.Resolver != nil {
		return d.Resolver
	}
	return net.DefaultResolver
}

// DialContext resolves addr, vets each address, and dials a permitted one.
func (d *Dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("gateway dial: bad address %q: %w", addr, err)
	}

	var ips []netip.Addr
	if lit, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{lit} // literal IP — still vetted, host="" so no name-allow
		host = ""
	} else {
		ips, err = d.resolver().LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("gateway dial: resolve %q: %w", host, err)
		}
	}

	portNum, err := net.LookupPort(network, port)
	if err != nil {
		return nil, fmt.Errorf("gateway dial: bad port %q: %w", port, err)
	}
	var vetted []netip.Addr
	lastReason := "no addresses resolved"
	for _, ip := range ips {
		v := d.vet(network, host, ip, uint16(portNum))
		if v.Allow {
			vetted = append(vetted, ip)
			continue
		}
		lastReason = v.Reason
		if d.OnDeny != nil {
			d.OnDeny(host, ip, v.Reason)
		}
	}
	if len(vetted) == 0 {
		return nil, &DeniedError{Host: addrForErr(host, addr), Reason: lastReason}
	}

	var dialErr error
	for _, ip := range vetted { // try vetted addrs in order → failover preserved
		conn, err := d.dialOne(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		dialErr = err
	}
	return nil, dialErr
}

// DialAs vets a connection the guest made to ip as a connection to host (the
// name the guest resolved to get ip; "" if unknown) and dials ip itself, so
// the connection goes where the guest pointed it rather than to a fresh
// resolution of host.
func (d *Dialer) DialAs(ctx context.Context, network, host string, ip netip.Addr, port uint16) (net.Conn, error) {
	if v := d.vet(network, host, ip, port); !v.Allow {
		if d.OnDeny != nil {
			d.OnDeny(host, ip, v.Reason)
		}
		return nil, &DeniedError{Host: addrForErr(host, ip.String()), Reason: v.Reason}
	}
	return d.dialOne(ctx, network, net.JoinHostPort(ip.String(), strconv.Itoa(int(port))))
}

// vet applies the port-aware TCP check to TCP dials and the address check to
// everything else.
func (d *Dialer) vet(network, host string, ip netip.Addr, port uint16) Verdict {
	if strings.HasPrefix(network, "tcp") {
		return d.Policy.CheckTCP(host, netip.AddrPortFrom(ip, port))
	}
	return d.Policy.Check(host, ip)
}

// AllowsName reports whether DNS lookups of host should be answered for a
// guest under this policy: every name under the public posture, only
// allow-listed names otherwise.
func (p *EgressPolicy) AllowsName(host string) bool {
	return p.Default == PosturePublic || p.hostAllowed(host)
}

func addrForErr(host, addr string) string {
	if host != "" {
		return host
	}
	return addr
}
