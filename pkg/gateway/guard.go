// Package gateway is bhatti's host-side egress gateway: the single point that
// enforces where a sandbox may connect (the private-range / SSRF guard + the
// per-sandbox egress policy) and, at L7, injects credentials on the guest's
// behalf. This file is the L4 guard — pure, arch-agnostic, VM-free logic that
// both the TSI egress filter and the virtio-net gateway share.
//
// Design: docs/internal/DESIGN-bhatti-v2-networking.md (§5.3) +
// docs/internal/DESIGN-bhatti-v2-secrets-and-trust.md (§3.6a). The guard is
// deny-by-construction for the host: a sandbox reaches the internet by default
// but never the host, loopback, link-local (incl. cloud metadata), or other
// tenants — and only reaches RFC-1918 with an explicit opt-in.
package gateway

import (
	"context"
	"fmt"
	"net"
	"net/netip"
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
	PosturePublic Posture = iota // allow the public internet (deny host/private) — the default
	PostureDeny                  // deny everything not explicitly allow-listed (locked-down agent box)
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

// EgressPolicy is a sandbox's egress rule set, evaluated per connection. Order
// of evaluation is fixed (not rule-order-dependent), so a manifest diff is
// stable: hard-deny → allow-cidr → allow-host → soft-deny → default.
type EgressPolicy struct {
	Default       Posture
	AllowCIDRs    []netip.Prefix
	AllowHosts    []HostPattern
	ExtraHardDeny []netip.Prefix // host addrs, daemon API, other tenants' vnets
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
	c := classify(ip, p.ExtraHardDeny)
	if c == classHardDeny {
		return deny("destination is in a hard-denied range (host/loopback/link-local/metadata)")
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

	var vetted []netip.Addr
	lastReason := "no addresses resolved"
	for _, ip := range ips {
		v := d.Policy.Check(host, ip)
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

func addrForErr(host, addr string) string {
	if host != "" {
		return host
	}
	return addr
}
