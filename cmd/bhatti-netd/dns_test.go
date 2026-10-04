package main

import (
	"net"
	"net/netip"
	"sync/atomic"
	"testing"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/sahil-shubham/bhatti/pkg/gateway"
)

// fakeResolver answers every A query for a name with a fixed address and
// counts the queries it saw.
func fakeResolver(t *testing.T, addr [4]byte) (string, *atomic.Int32) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	var seen atomic.Int32
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			seen.Add(1)
			var p dnsmessage.Parser
			hdr, err := p.Start(buf[:n])
			if err != nil {
				continue
			}
			q, err := p.Question()
			if err != nil {
				continue
			}
			hdr.Response = true
			b := dnsmessage.NewBuilder(nil, hdr)
			b.StartQuestions()
			b.Question(q)
			b.StartAnswers()
			b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 60}, dnsmessage.AResource{A: addr})
			out, _ := b.Finish()
			pc.WriteTo(out, from)
		}
	}()
	return pc.LocalAddr().String(), &seen
}

func query(t *testing.T, name string) []byte {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 7, RecursionDesired: true})
	b.StartQuestions()
	b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET})
	out, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func rcode(t *testing.T, resp []byte) dnsmessage.RCode {
	t.Helper()
	var p dnsmessage.Parser
	h, err := p.Start(resp)
	if err != nil {
		t.Fatalf("parse response: %v", err)
	}
	return h.RCode
}

// Under deny, an allow-listed name is resolved upstream and its answer is
// remembered, so a connection to that IP is vetted as that name; any other
// name is refused without ever reaching the resolver.
func TestDNSProxyDenyPosture(t *testing.T) {
	resolver, seen := fakeResolver(t, [4]byte{93, 184, 215, 14})
	pol, err := gateway.PolicyFromWire(gateway.NetPolicyWire{Default: "deny", AllowHosts: []string{"example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	st := &guestState{pol: pol, dialer: &gateway.Dialer{Policy: pol}}
	ip := netip.MustParseAddr("93.184.215.14")

	resp := answerDNS(query(t, "github.com."), resolver, st)
	if resp == nil || rcode(t, resp) != dnsmessage.RCodeRefused {
		t.Fatalf("disallowed name: want REFUSED, got %v", resp)
	}
	if seen.Load() != 0 {
		t.Fatal("disallowed name was sent to the resolver")
	}

	resp = answerDNS(query(t, "example.com."), resolver, st)
	if resp == nil || rcode(t, resp) != dnsmessage.RCodeSuccess {
		t.Fatalf("allowed name: want an answer, got %v", resp)
	}
	if got := st.names.lookup(ip); got != "example.com" {
		t.Fatalf("answered IP recorded as %q, want example.com", got)
	}
	if !pol.Check(st.names.lookup(ip), ip).Allow {
		t.Fatal("connection to the answered IP is not allowed as example.com")
	}
	if pol.Check("", ip).Allow {
		t.Fatal("the same IP must stay denied when no name vouches for it")
	}
}
