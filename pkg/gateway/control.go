package gateway

// This file is the daemon->netd control channel. netd runs one gateway per
// owner and, until now, applied a single hardcoded egress policy to every
// guest. The daemon is the real source of truth for per-sandbox state (egress
// rules today; secret alias bindings next), so it pushes that state over a
// dedicated control UDS, keyed by the guest's gateway IP. The daemon (re)pushes
// on create/destroy and after adopting a netd that survived a daemon restart.
//
// Wire framing is newline-delimited JSON (one ControlMsg per json.Encode); a
// single connection carries many messages. HostPattern is opaque, so the wire
// carries raw host/CIDR strings that PolicyFromWire parses gateway-side.

import (
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"sync"
)

// NetPolicyWire is the JSON form of an EgressPolicy.
type NetPolicyWire struct {
	Default    string   `json:"default,omitempty"`     // "public" (default) | "deny"
	AllowHosts []string `json:"allow_hosts,omitempty"` // exact ("api.x.com") or wildcard ("*.x.com")
	AllowCIDRs []string `json:"allow_cidrs,omitempty"`
}

// PolicyFromWire builds an EgressPolicy from its wire form. The private-range /
// SSRF hard-deny is applied unconditionally by Check regardless of this policy,
// so an allow rule can never widen past it.
func PolicyFromWire(w NetPolicyWire) (*EgressPolicy, error) {
	p := &EgressPolicy{}
	switch w.Default {
	case "", "public":
		p.Default = PosturePublic
	case "deny":
		p.Default = PostureDeny
	default:
		return nil, fmt.Errorf("gateway: unknown egress posture %q (want public|deny)", w.Default)
	}
	for _, h := range w.AllowHosts {
		hp, err := ParseHostPattern(h)
		if err != nil {
			return nil, fmt.Errorf("gateway: allow_host %q: %w", h, err)
		}
		p.AllowHosts = append(p.AllowHosts, hp)
	}
	for _, c := range w.AllowCIDRs {
		pfx, err := netip.ParsePrefix(c)
		if err != nil {
			return nil, fmt.Errorf("gateway: allow_cidr %q: %w", c, err)
		}
		p.AllowCIDRs = append(p.AllowCIDRs, pfx)
	}
	return p, nil
}

// ControlOp is a control-channel operation.
type ControlOp string

const (
	ControlSet ControlOp = "set" // upsert a guest's per-sandbox state
	ControlDel ControlOp = "del" // drop a guest (on destroy)
)

// ControlMsg is one framed control message.
type ControlMsg struct {
	Op      ControlOp      `json:"op"`
	GuestIP string         `json:"guest_ip"`
	Sandbox string         `json:"sandbox_id,omitempty"`
	Policy  *NetPolicyWire `json:"policy,omitempty"`
	// Aliases []AliasWire — added by the secret-substitution tranche.
}

// ControlHandler applies control messages to a gateway. The per-owner gateway
// implements it; nil pol means "no explicit policy" (gateway uses its default).
type ControlHandler interface {
	SetSandbox(guestIP, sandboxID string, pol *EgressPolicy)
	DelSandbox(guestIP string)
}

// ServeControl accepts control connections on ln and applies each decoded
// message to h until ln closes. A malformed policy is skipped (logged nowhere —
// the daemon is trusted; a decode error just means resync on the next push)
// rather than tearing down the connection.
func ServeControl(ln net.Listener, h ControlHandler) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go serveControlConn(conn, h)
	}
}

func serveControlConn(conn net.Conn, h ControlHandler) {
	defer conn.Close()
	dec := json.NewDecoder(conn)
	for {
		var m ControlMsg
		if err := dec.Decode(&m); err != nil {
			return // EOF or malformed framing: daemon will redial + re-push
		}
		switch m.Op {
		case ControlSet:
			var pol *EgressPolicy
			if m.Policy != nil {
				p, err := PolicyFromWire(*m.Policy)
				if err != nil {
					continue // skip this message; keep the connection
				}
				pol = p
			}
			h.SetSandbox(m.GuestIP, m.Sandbox, pol)
		case ControlDel:
			h.DelSandbox(m.GuestIP)
		}
	}
}

// ControlClient is the daemon side: a persistent, lazily-dialed sender to one
// netd's control UDS. It redials on a broken connection so a netd restart (or a
// not-yet-listening socket right after spawn) is transparent to callers.
type ControlClient struct {
	path string
	mu   sync.Mutex
	conn net.Conn
	enc  *json.Encoder
}

// NewControlClient returns a client for the control UDS at path.
func NewControlClient(path string) *ControlClient { return &ControlClient{path: path} }

// Send delivers one message, dialing if needed. On a write error it drops the
// connection so the next Send redials.
func (c *ControlClient) Send(m ControlMsg) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.enc == nil {
		conn, err := net.Dial("unix", c.path)
		if err != nil {
			return err
		}
		c.conn, c.enc = conn, json.NewEncoder(conn)
	}
	if err := c.enc.Encode(m); err != nil {
		c.conn.Close()
		c.conn, c.enc = nil, nil
		return err
	}
	return nil
}

// Close releases the underlying connection.
func (c *ControlClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn, c.enc = nil, nil
	return err
}
