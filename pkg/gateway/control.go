package gateway

// This file is the daemon->netd control channel. The daemon owns each guest's
// policy and pushes it over a dedicated control UDS keyed by guest IP. A missing
// or malformed policy registers deny, rather than retaining a previous grant.
// The daemon (re)pushes on create/destroy and when adopting a surviving netd.
//
// Wire framing is newline-delimited JSON (one ControlMsg per json.Encode); a
// single connection carries many messages. HostPattern is opaque, so the wire
// carries raw host/CIDR strings that PolicyFromWire parses gateway-side.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"
)

// NetPolicyWire is the JSON form of an EgressPolicy, plus the "none" posture:
// a sandbox with no network device at all (never reaches netd).
type NetPolicyWire struct {
	Default    string   `json:"default"`               // "none" | "deny" | "public"; required
	Siblings   string   `json:"siblings,omitempty"`    // "allow" | "deny"; omitted = deny
	AllowHosts []string `json:"allow_hosts,omitempty"` // exact ("api.x.com") or wildcard ("*.x.com")
	AllowCIDRs []string `json:"allow_cidrs,omitempty"`
}

// PostureNone is the wire posture for a sandbox with no network device.
const PostureNone = "none"

// NoNetwork reports whether w asks for no network device. nil-safe.
func (w *NetPolicyWire) NoNetwork() bool { return w != nil && w.Default == PostureNone }

// ValidateWire checks w without building a netd policy: a known posture, and
// parseable allow rules that only make sense with a network.
func ValidateWire(w NetPolicyWire) error {
	if w.Default == PostureNone {
		if w.Siblings != "" && w.Siblings != "deny" {
			return fmt.Errorf("gateway: siblings need a network (egress deny or public, not none)")
		}
		if len(w.AllowHosts) > 0 || len(w.AllowCIDRs) > 0 {
			return fmt.Errorf("gateway: allow rules need a network (egress deny or public, not none)")
		}
		return nil
	}
	_, err := PolicyFromWire(w)
	return err
}

// PolicyFromWire builds an EgressPolicy from its wire form. netd supplies
// deployment-specific hard-denied addresses before using the policy.
func PolicyFromWire(w NetPolicyWire) (*EgressPolicy, error) {
	p := &EgressPolicy{}
	switch w.Default {
	case "":
		return nil, fmt.Errorf("gateway: egress posture required")
	case "public":
		p.Default = PosturePublic
	case "deny":
		p.Default = PostureDeny
	default:
		return nil, fmt.Errorf("gateway: unknown egress posture %q (want none|deny|public)", w.Default)
	}
	switch w.Siblings {
	case "", "deny":
		p.Siblings = PostureDeny
	case "allow":
		p.Siblings = PosturePublic
	default:
		return nil, fmt.Errorf("gateway: unknown siblings posture %q (want allow|deny)", w.Siblings)
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
	// EnforcementVersion changes whenever netd's enforcement or guest-link
	// protocol changes. A daemon must not adopt a netd with another version.
	EnforcementVersion = 1
	GuestHelloSize     = 64 // hex-encoded, 256-bit per-VM attachment secret

	ControlHello ControlOp = "hello"
	ControlSet   ControlOp = "set"
	ControlDel   ControlOp = "del"
)

// ControlMsg is one framed control message.
type ControlMsg struct {
	Op         ControlOp      `json:"op"`
	GuestIP    string         `json:"guest_ip,omitempty"`
	GuestMAC   string         `json:"guest_mac,omitempty"`
	GuestToken string         `json:"guest_token,omitempty"`
	Sandbox    string         `json:"sandbox_id,omitempty"`
	Policy     *NetPolicyWire `json:"policy,omitempty"`
	// Aliases []AliasWire — added by the secret-substitution tranche.
}

type controlReply struct {
	Version int    `json:"version"`
	Error   string `json:"error,omitempty"`
}

// ControlHandler applies control messages to a gateway. A nil policy means
// deny, including when the sender omitted the policy.
type ControlHandler interface {
	SetSandbox(guestIP, sandboxID string, pol *EgressPolicy, mac, token string) error
	DelSandbox(guestIP string)
}

// ServeControl accepts control connections on ln and applies decoded messages
// until ln closes. Every operation is acknowledged after it takes effect: the
// daemon cannot launch a NIC against a policy still in the socket buffer.
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
	enc := json.NewEncoder(conn)
	for {
		var m ControlMsg
		if err := dec.Decode(&m); err != nil {
			return
		}
		reply := controlReply{Version: EnforcementVersion}
		switch m.Op {
		case ControlHello:
		case ControlSet:
			pol := &EgressPolicy{}
			if m.Policy != nil {
				p, err := PolicyFromWire(*m.Policy)
				if err != nil {
					slog.Warn("netd.policy_rejected", "sandbox_id", m.Sandbox, "guest_ip", m.GuestIP, "error", err)
				} else {
					pol = p
				}
			} else {
				slog.Warn("netd.policy_rejected", "sandbox_id", m.Sandbox, "guest_ip", m.GuestIP, "error", "egress policy required")
			}
			if err := h.SetSandbox(m.GuestIP, m.Sandbox, pol, m.GuestMAC, m.GuestToken); err != nil {
				h.DelSandbox(m.GuestIP)
				reply.Error = err.Error()
			}
		case ControlDel:
			h.DelSandbox(m.GuestIP)
		default:
			reply.Error = "unknown control operation"
		}
		if err := enc.Encode(reply); err != nil {
			return
		}
	}
}

// ProbeVersion treats old netds (which ignore hello), malformed answers and
// unresponsive sockets as incompatible, not as evidence of current enforcement.
func ProbeVersion(path string) error {
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
		return err
	}
	if err := json.NewEncoder(conn).Encode(ControlMsg{Op: ControlHello}); err != nil {
		return err
	}
	var reply controlReply
	if err := json.NewDecoder(conn).Decode(&reply); err != nil {
		return fmt.Errorf("netd hello: %w", err)
	}
	if reply.Version != EnforcementVersion || reply.Error != "" {
		return fmt.Errorf("netd enforcement version %d (want %d): %s", reply.Version, EnforcementVersion, reply.Error)
	}
	return nil
}

// ControlClient is the daemon side: a persistent, lazily-dialed sender to one
// netd's control UDS. It redials on a broken connection so a netd restart (or a
// not-yet-listening socket right after spawn) is transparent to callers.
type ControlClient struct {
	path string
	mu   sync.Mutex
	conn net.Conn
	enc  *json.Encoder
	dec  *json.Decoder
}

// NewControlClient returns a client for the control UDS at path.
func NewControlClient(path string) *ControlClient { return &ControlClient{path: path} }

// Send delivers one message, dialing if needed. It waits for the netd to
// apply the operation and acknowledge the matching enforcement version.
func (c *ControlClient) Send(m ControlMsg) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.enc == nil {
		conn, err := net.DialTimeout("unix", c.path, time.Second)
		if err != nil {
			return err
		}
		c.conn, c.enc, c.dec = conn, json.NewEncoder(conn), json.NewDecoder(conn)
	}
	_ = c.conn.SetDeadline(time.Now().Add(2 * time.Second))
	var reply controlReply
	err := c.enc.Encode(m)
	if err == nil {
		err = c.dec.Decode(&reply)
	}
	if err == nil && (reply.Version != EnforcementVersion || reply.Error != "") {
		err = fmt.Errorf("netd control version %d (want %d): %s", reply.Version, EnforcementVersion, reply.Error)
	}
	if err != nil {
		c.conn.Close()
		c.conn, c.enc, c.dec = nil, nil, nil
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
	c.conn, c.enc, c.dec = nil, nil, nil
	return err
}
