package gateway

import (
	"context"
	"net/netip"
	"testing"
)

func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("parse addr %q: %v", s, err)
	}
	return a
}

func mustPrefix(t *testing.T, s string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(s)
	if err != nil {
		t.Fatalf("parse prefix %q: %v", s, err)
	}
	return p
}

// TestClassify is the heart of the guard: every hard/soft/public case incl. the
// IPv6 smuggling bypasses (::ffff: mapped, NAT64-embedded v4).
func TestClassify(t *testing.T) {
	cases := []struct {
		ip   string
		want class
	}{
		// hard deny
		{"127.0.0.1", classHardDeny},
		{"127.0.0.53", classHardDeny},
		{"::1", classHardDeny},
		{"0.0.0.0", classHardDeny},
		{"::", classHardDeny},
		{"169.254.0.5", classHardDeny},
		{"169.254.169.254", classHardDeny}, // cloud metadata (link-local)
		{"fe80::1", classHardDeny},
		{"224.0.0.1", classHardDeny},
		{"ff02::1", classHardDeny},
		{"::ffff:127.0.0.1", classHardDeny},       // IPv4-mapped loopback bypass
		{"::ffff:169.254.169.254", classHardDeny}, // IPv4-mapped metadata bypass
		{"64:ff9b::7f00:1", classHardDeny},        // NAT64-embedded 127.0.0.1
		{"64:ff9b::a9fe:a9fe", classHardDeny},     // NAT64-embedded 169.254.169.254
		// soft deny (private)
		{"10.0.0.1", classSoftDeny},
		{"10.5.3.1", classSoftDeny},
		{"172.16.0.1", classSoftDeny},
		{"192.168.1.1", classSoftDeny},
		{"100.64.0.1", classSoftDeny}, // CGNAT
		{"fc00::1", classSoftDeny},    // ULA
		{"::ffff:10.0.0.1", classSoftDeny},
		{"64:ff9b::a00:1", classSoftDeny}, // NAT64-embedded 10.0.0.1
		// public
		{"1.1.1.1", classPublic},
		{"8.8.8.8", classPublic},
		{"140.82.121.4", classPublic},         // github
		{"2606:4700:4700::1111", classPublic}, // cloudflare v6
	}
	for _, c := range cases {
		if got := classify(mustAddr(t, c.ip), nil); got != c.want {
			t.Errorf("classify(%s) = %d, want %d", c.ip, got, c.want)
		}
	}
}

func TestClassifyExtraHardDeny(t *testing.T) {
	// The host's own public-ish addr + another tenant's vnet, added as hard-deny.
	extra := []netip.Prefix{mustPrefix(t, "203.0.113.7/32"), mustPrefix(t, "100.80.0.0/16")}
	if classify(mustAddr(t, "203.0.113.7"), extra) != classHardDeny {
		t.Error("host's own address should be hard-denied")
	}
	if classify(mustAddr(t, "100.80.1.2"), extra) != classHardDeny {
		t.Error("other tenant's vnet should be hard-denied")
	}
	if classify(mustAddr(t, "203.0.113.8"), extra) != classPublic {
		t.Error("a neighbouring public addr should stay public")
	}
}

func TestHostPatternMatch(t *testing.T) {
	exact, err := ParseHostPattern("api.stripe.com")
	if err != nil {
		t.Fatal(err)
	}
	wild, err := ParseHostPattern("*.stripe.com")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		p    HostPattern
		host string
		want bool
	}{
		{exact, "api.stripe.com", true},
		{exact, "API.Stripe.Com", true},  // case-insensitive
		{exact, "api.stripe.com.", true}, // trailing dot
		{exact, "evil.com", false},
		{exact, "x.api.stripe.com", false},
		{wild, "api.stripe.com", true},
		{wild, "a.b.stripe.com", true},
		{wild, "stripe.com", false},          // wildcard needs a subdomain label
		{wild, "stripe.com.evil.com", false}, // classic suffix-smuggle
		{wild, "evilstripe.com", false},      // no label boundary
		{wild, "notstripe.com", false},
	}
	for _, c := range cases {
		if got := c.p.Match(c.host); got != c.want {
			t.Errorf("Match(%+v, %q) = %v, want %v", c.p, c.host, got, c.want)
		}
	}
}

func TestParseHostPatternRejectsBad(t *testing.T) {
	for _, bad := range []string{"", "*", "*.", "a.*.com", "*.*.com", "foo.*"} {
		if _, err := ParseHostPattern(bad); err == nil {
			t.Errorf("ParseHostPattern(%q) should error", bad)
		}
	}
}

func TestEgressPolicyCheck(t *testing.T) {
	stripe, _ := ParseHostPattern("api.stripe.com")

	t.Run("DefaultPublic", func(t *testing.T) {
		p := &EgressPolicy{Default: PosturePublic}
		if v := p.Check("api.example.com", mustAddr(t, "1.1.1.1")); !v.Allow {
			t.Errorf("public should be allowed by default: %s", v.Reason)
		}
		if v := p.Check("", mustAddr(t, "127.0.0.1")); v.Allow {
			t.Error("loopback must be denied even under default-public")
		}
		if v := p.Check("", mustAddr(t, "10.0.0.1")); v.Allow {
			t.Error("private must be denied by default")
		}
	})

	t.Run("AllowCIDROptInPrivate", func(t *testing.T) {
		p := &EgressPolicy{Default: PosturePublic, AllowCIDRs: []netip.Prefix{mustPrefix(t, "10.0.0.0/8")}}
		if v := p.Check("nas.local", mustAddr(t, "10.5.3.1")); !v.Allow {
			t.Errorf("explicit allow-cidr should permit the private range: %s", v.Reason)
		}
	})

	t.Run("AllowCIDRCannotOverrideHardDeny", func(t *testing.T) {
		p := &EgressPolicy{Default: PosturePublic, AllowCIDRs: []netip.Prefix{mustPrefix(t, "127.0.0.0/8"), mustPrefix(t, "0.0.0.0/0")}}
		if v := p.Check("", mustAddr(t, "127.0.0.1")); v.Allow {
			t.Error("no allow-cidr (even 0.0.0.0/0) may re-open loopback")
		}
		if v := p.Check("", mustAddr(t, "169.254.169.254")); v.Allow {
			t.Error("no allow-cidr may re-open the metadata address")
		}
	})

	t.Run("PostureDeny", func(t *testing.T) {
		p := &EgressPolicy{Default: PostureDeny, AllowHosts: []HostPattern{stripe}}
		if v := p.Check("api.stripe.com", mustAddr(t, "1.2.3.4")); !v.Allow {
			t.Errorf("allow-host public IP should pass under deny: %s", v.Reason)
		}
		if v := p.Check("api.openai.com", mustAddr(t, "1.2.3.4")); v.Allow {
			t.Error("a non-allow-listed public host must be denied under default-deny")
		}
	})

	t.Run("AllowedHostRebindingToPrivateDenied", func(t *testing.T) {
		p := &EgressPolicy{Default: PostureDeny, AllowHosts: []HostPattern{stripe}}
		if v := p.Check("api.stripe.com", mustAddr(t, "10.0.0.1")); v.Allow {
			t.Error("an allowed host that resolves into private space must be denied (rebinding)")
		}
	})
}

// fakeResolver returns canned addresses — used to simulate DNS rebinding.
type fakeResolver map[string][]netip.Addr

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	return f[host], nil
}

func TestDialerVetsResolvedIP(t *testing.T) {
	// A malicious name resolves to loopback (DNS rebinding). The dialer must
	// refuse before connecting — no real dial should be attempted.
	res := fakeResolver{
		"rebind.evil.com": {mustAddr(t, "127.0.0.1")},
		"multi.example":   {mustAddr(t, "10.0.0.1"), mustAddr(t, "1.1.1.1")},
	}
	d := &Dialer{Policy: &EgressPolicy{Default: PosturePublic}, Resolver: res}

	_, err := d.DialContext(context.Background(), "tcp", "rebind.evil.com:443")
	var de *DeniedError
	if err == nil {
		t.Fatal("dial to a name that resolves to loopback must be denied")
	}
	if !asDenied(err, &de) {
		t.Fatalf("want *DeniedError, got %T: %v", err, err)
	}

	// A literal private IP is denied without any resolver involvement.
	if _, err := d.DialContext(context.Background(), "tcp", "192.168.1.1:80"); err == nil {
		t.Fatal("literal private IP must be denied")
	}
}

func asDenied(err error, target **DeniedError) bool {
	for err != nil {
		if de, ok := err.(*DeniedError); ok {
			*target = de
			return true
		}
		type unwrap interface{ Unwrap() error }
		u, ok := err.(unwrap)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
