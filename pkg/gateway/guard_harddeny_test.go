package gateway

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
)

// A refusal netd counts as "hard" is one no rule could have avoided; everything
// else it counts as policy. Callers read Verdict.HardDeny and DeniedError.HardDeny
// for the difference, never the Reason text, so the flag has to follow the
// classification exactly.
func TestVerdictHardDenyIsSetOnlyForDestinationsNoRuleCanOpen(t *testing.T) {
	own := mustPrefix(t, "100.64.0.0/24")
	extra := []netip.Prefix{mustPrefix(t, "203.0.113.42/32"), mustPrefix(t, "100.64.1.0/24")} // the host, another owner
	stripe, err := ParseHostPattern("api.stripe.com")
	if err != nil {
		t.Fatal(err)
	}
	deny := &EgressPolicy{SiblingSubnet: own, ExtraHardDeny: extra}
	public := &EgressPolicy{Default: PosturePublic, SiblingSubnet: own, ExtraHardDeny: extra}
	// The widest policy there is: only hard denials can stand in its way.
	everything := &EgressPolicy{Default: PosturePublic, Siblings: PosturePublic, SiblingSubnet: own, ExtraHardDeny: extra,
		AllowCIDRs: []netip.Prefix{mustPrefix(t, "0.0.0.0/0")}}
	byName := &EgressPolicy{Default: PostureDeny, SiblingSubnet: own, ExtraHardDeny: extra, AllowHosts: []HostPattern{stripe}}

	for _, tc := range []struct {
		name        string
		pol         *EgressPolicy
		host, ip    string
		allow, hard bool
	}{
		{"public destination, deny posture", deny, "", "8.8.8.8", false, false},
		{"public destination, public posture", public, "", "8.8.8.8", true, false},
		{"private range", public, "", "10.1.2.3", false, false},
		{"private range opened by allow-cidr", everything, "", "10.1.2.3", true, false},
		{"sibling without opt-in", public, "", "100.64.0.3", false, false},
		{"sibling with opt-in", everything, "", "100.64.0.3", true, false},
		{"allowed name rebinding into a private range", byName, "api.stripe.com", "10.0.0.9", false, false},
		{"allowed name at a public address", byName, "api.stripe.com", "8.8.8.8", true, false},
		{"loopback", public, "", "127.0.0.1", false, true},
		{"IPv4-mapped loopback", public, "", "::ffff:127.0.0.1", false, true},
		{"cloud metadata, deny posture", deny, "", "169.254.169.254", false, true},
		{"cloud metadata, widest policy", everything, "", "169.254.169.254", false, true},
		{"multicast", public, "", "224.0.0.251", false, true},
		{"unspecified", public, "", "0.0.0.0", false, true},
		{"the host's own address, widest policy", everything, "", "203.0.113.42", false, true},
		{"another owner's range, widest policy", everything, "", "100.64.1.7", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := tc.pol.Check(tc.host, mustAddr(t, tc.ip))
			if v.Allow != tc.allow || v.HardDeny != tc.hard {
				t.Fatalf("Check(%q, %s) = {Allow:%v HardDeny:%v} (%s), want {Allow:%v HardDeny:%v}",
					tc.host, tc.ip, v.Allow, v.HardDeny, v.Reason, tc.allow, tc.hard)
			}
		})
	}
}

// Lifting a daemon port on a host address must not make the rest of the host
// anything but a hard denial.
func TestHardDenyFlagSurvivesHostServiceLift(t *testing.T) {
	svc := netip.MustParseAddrPort("203.0.113.42:443")
	pol := &EgressPolicy{
		Default:       PosturePublic,
		ExtraHardDeny: []netip.Prefix{mustPrefix(t, "203.0.113.42/32")},
		HostServices:  []netip.AddrPort{svc},
	}
	if v := pol.CheckTCP("", svc); !v.Allow || v.HardDeny {
		t.Fatalf("the daemon's own port = %+v, want allowed", v)
	}
	if v := pol.CheckTCP("", netip.MustParseAddrPort("203.0.113.42:22")); v.Allow || !v.HardDeny {
		t.Fatalf("another port on the host = %+v, want a hard denial", v)
	}
}

func TestDeniedErrorCarriesTheVerdictsFlag(t *testing.T) {
	errUnreachable := errors.New("network is unreachable")
	dialed := 0
	seam := func(context.Context, string, string) (net.Conn, error) { dialed++; return nil, errUnreachable }
	deny := &Dialer{Policy: &EgressPolicy{Default: PostureDeny}, dialAddr: seam}
	public := &Dialer{Policy: &EgressPolicy{Default: PosturePublic}, dialAddr: seam}
	ctx := context.Background()

	denied := func(t *testing.T, err error) *DeniedError {
		t.Helper()
		var de *DeniedError
		if !errors.As(err, &de) {
			t.Fatalf("error %v (%T) is not a DeniedError", err, err)
		}
		return de
	}

	_, err := deny.DialAs(ctx, "tcp", "", mustAddr(t, "169.254.169.254"), 80)
	if de := denied(t, err); !de.HardDeny {
		t.Errorf("DialAs to cloud metadata: %+v, want HardDeny", de)
	}
	_, err = deny.DialAs(ctx, "tcp", "", mustAddr(t, "8.8.8.8"), 443)
	if de := denied(t, err); de.HardDeny {
		t.Errorf("DialAs to a public address under deny: %+v, want a policy denial", de)
	}
	_, err = deny.DialContext(ctx, "tcp", "169.254.169.254:80")
	if de := denied(t, err); !de.HardDeny {
		t.Errorf("DialContext to cloud metadata: %+v, want HardDeny", de)
	}
	_, err = deny.DialContext(ctx, "tcp", "8.8.8.8:443")
	if de := denied(t, err); de.HardDeny {
		t.Errorf("DialContext to a public address under deny: %+v, want a policy denial", de)
	}
	if dialed != 0 {
		t.Fatalf("a refused destination was dialed %d times", dialed)
	}

	// A permitted destination that cannot be reached is the network's doing,
	// not a refusal, and must not look like one.
	for name, dial := range map[string]func() error{
		"DialAs":      func() error { _, err := public.DialAs(ctx, "tcp", "", mustAddr(t, "8.8.8.8"), 443); return err },
		"DialContext": func() error { _, err := public.DialContext(ctx, "tcp", "8.8.8.8:443"); return err },
	} {
		err := dial()
		var de *DeniedError
		if !errors.Is(err, errUnreachable) || errors.As(err, &de) {
			t.Errorf("%s to an unreachable destination = %v, want the network error and no DeniedError", name, err)
		}
	}
}

// With several resolved addresses all refused, the error reports the last
// refusal's reason; the flag must describe that same refusal, or a counter and
// the log line beside it would disagree.
func TestDeniedErrorFlagMatchesTheReasonItReports(t *testing.T) {
	pol := &EgressPolicy{Default: PosturePublic}
	res := fakeResolver{
		"soft-then-hard.test": {mustAddr(t, "10.0.0.1"), mustAddr(t, "127.0.0.1")},
		"hard-then-soft.test": {mustAddr(t, "127.0.0.1"), mustAddr(t, "10.0.0.1")},
	}
	d := &Dialer{Policy: pol, Resolver: res}
	for name, last := range map[string]string{"soft-then-hard.test": "127.0.0.1", "hard-then-soft.test": "10.0.0.1"} {
		_, err := d.DialContext(context.Background(), "tcp", name+":443")
		var de *DeniedError
		if !errors.As(err, &de) {
			t.Fatalf("%s: %v is not a DeniedError", name, err)
		}
		want := pol.Check("", mustAddr(t, last))
		if de.Reason != want.Reason || de.HardDeny != want.HardDeny {
			t.Errorf("%s: error says {%q hard=%v}, the last refusal was {%q hard=%v}", name, de.Reason, de.HardDeny, want.Reason, want.HardDeny)
		}
	}
	if _, err := d.DialContext(context.Background(), "tcp", "nothing.test:443"); err != nil {
		var de *DeniedError
		if !errors.As(err, &de) || de.HardDeny {
			t.Errorf("a name that resolves to nothing = %v, want a plain (non-hard) denial", err)
		}
	}
}
