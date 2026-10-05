package main

import (
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/sahil-shubham/bhatti/pkg/gateway"
)

// Guest DNS goes through netd rather than being relayed blind, for two reasons.
// Allow-host rules are names, but the guest connects to IPs: netd remembers
// which name each answered IP came from, so a later connection to that IP is
// vetted as a connection to that name. And under the deny posture only
// allow-listed names are resolved at all, so DNS can't be used to smuggle data
// out of a locked-down sandbox.

const (
	dnsPort        = 53
	dnsTimeout     = 5 * time.Second
	nameTTLMin     = 30 * time.Second // keep short-TTL CDN answers usable for a connect
	nameTTLMax     = time.Hour
	nameCacheLimit = 4096
)

type nameEntry struct {
	host string
	exp  time.Time
}

// nameCache maps an answered IP to the name the guest asked for, per guest.
type nameCache struct {
	mu sync.Mutex
	m  map[netip.Addr]nameEntry
}

func (c *nameCache) put(ip netip.Addr, host string, ttl time.Duration) {
	ttl = min(max(ttl, nameTTLMin), nameTTLMax)
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[netip.Addr]nameEntry{}
	}
	if len(c.m) >= nameCacheLimit {
		for k, e := range c.m {
			if now.After(e.exp) {
				delete(c.m, k)
			}
		}
		if len(c.m) >= nameCacheLimit {
			c.m = map[netip.Addr]nameEntry{} // pathological: start over rather than grow
		}
	}
	c.m[ip.Unmap()] = nameEntry{host: host, exp: now.Add(ttl)}
}

// lookup returns the name ip was resolved from, or "" if unknown or expired.
func (c *nameCache) lookup(ip netip.Addr) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[ip.Unmap()]
	if !ok || time.Now().After(e.exp) {
		return ""
	}
	return e.host
}

// serveDNS answers one guest DNS flow: each query is forwarded to the resolver
// the guest addressed (if its name is allowed), the answers are recorded, and
// the response is relayed back. Disallowed names get REFUSED.
func serveDNS(guest net.Conn, resolver string, st *guestState) {
	defer guest.Close()
	buf := make([]byte, 65535)
	for {
		guest.SetReadDeadline(time.Now().Add(udpIdleTimeout))
		n, err := guest.Read(buf)
		if err != nil {
			return
		}
		resp := answerDNS(buf[:n], resolver, st)
		if resp != nil {
			guest.Write(resp)
		}
	}
}

func answerDNS(query []byte, resolver string, st *guestState) []byte {
	var p dnsmessage.Parser
	hdr, err := p.Start(query)
	if err != nil {
		return nil
	}
	q, err := p.Question()
	if err != nil {
		return nil
	}
	if !st.pol.AllowsName(q.Name.String()) {
		return refused(hdr, q)
	}
	up, err := net.Dial("udp", resolver)
	if err != nil {
		return nil
	}
	defer up.Close()
	up.SetDeadline(time.Now().Add(dnsTimeout))
	if _, err := up.Write(query); err != nil {
		return nil
	}
	resp := make([]byte, 65535)
	n, err := up.Read(resp)
	if err != nil {
		return nil
	}
	recordAnswers(resp[:n], strings.TrimSuffix(q.Name.String(), "."), &st.names)
	return resp[:n]
}

// recordAnswers stores every A/AAAA in resp under the asked name. CNAME chains
// end in the same answer section, so the target IPs map back to the name the
// guest asked for, which is what allow-host rules are written against.
func recordAnswers(resp []byte, asked string, names *nameCache) {
	var p dnsmessage.Parser
	if _, err := p.Start(resp); err != nil {
		return
	}
	if err := p.SkipAllQuestions(); err != nil {
		return
	}
	for {
		h, err := p.AnswerHeader()
		if err != nil {
			return
		}
		ttl := time.Duration(h.TTL) * time.Second
		switch h.Type {
		case dnsmessage.TypeA:
			r, err := p.AResource()
			if err != nil {
				return
			}
			names.put(netip.AddrFrom4(r.A), asked, ttl)
		case dnsmessage.TypeAAAA:
			r, err := p.AAAAResource()
			if err != nil {
				return
			}
			names.put(netip.AddrFrom16(r.AAAA), asked, ttl)
		default:
			if err := p.SkipAnswer(); err != nil {
				return
			}
		}
	}
}

func refused(hdr dnsmessage.Header, q dnsmessage.Question) []byte {
	hdr.Response = true
	hdr.RCode = dnsmessage.RCodeRefused
	hdr.RecursionAvailable = true
	b := dnsmessage.NewBuilder(nil, hdr)
	if b.StartQuestions() != nil || b.Question(q) != nil {
		return nil
	}
	out, err := b.Finish()
	if err != nil {
		return nil
	}
	return out
}

// resolverAllowed checks the DNS server's destination independently of which
// names a guest may resolve; netd's deployment hard-denials still apply.
func resolverAllowed(ip netip.Addr, pol *gateway.EgressPolicy) bool {
	if pol == nil {
		return false
	}
	open := gateway.AllowAllEgress()
	open.ExtraHardDeny = pol.ExtraHardDeny
	return open.Check("", ip).Allow
}
