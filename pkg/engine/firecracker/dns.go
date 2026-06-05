//go:build linux

package firecracker

import (
	"context"
	"log/slog"
	"net"

	"github.com/sahil-shubham/bhatti/pkg/dns"
)

// DNS lifecycle glue between the per-user bridge and the standalone
// DNS responder in pkg/dns. G1.1 of PLAN-bhatti-v2.md.
//
// Each UserNetwork owns one *dns.Server bound to its gateway IP on
// port 53. The lifecycle is:
//
//	ensureUserBridge   → startDNSForBridge   (bind UDP+TCP on N.1:53)
//	Engine.Create      → net.DNS.Set(name, ip)
//	Engine.Destroy     → net.DNS.Delete(name)
//	destroyUserBridge  → stopDNSForBridge    (close listeners)
//
// Recovery (engine restart) re-seeds the zone from store; see
// recover.go's per-VM loop. Sandbox renames are NOT reflected today —
// the v2 plan doesn't promise it and rename is rare; if it becomes a
// pain point, hook into PATCH /sandboxes/:id name path.
//
// Bind failures (e.g. another DNS service already on 53) are logged
// but do NOT fail bridge creation. L2/L3 traffic still works; only
// name resolution is lost for that user.

// startDNSForBridge binds and starts a DNS server for the given user
// network. The server's lifecycle is tied to e.ctx (cancelled on
// engine shutdown). On bind failure, returns nil and logs — caller
// should not treat that as fatal.
func startDNSForBridge(ctx context.Context, net *UserNetwork, logger *slog.Logger) *dns.Server {
	bindAddr := net.GatewayIP + ":53"
	s := dns.NewServer(bindAddr)
	s.Logger = logger
	if err := s.Start(ctx); err != nil {
		logger.Warn("dns: failed to start responder; names won't resolve for this user",
			"bridge", net.BridgeName, "bind", bindAddr, "err", err)
		return nil
	}
	logger.Info("dns: responder started",
		"bridge", net.BridgeName, "bind", bindAddr)
	return s
}

// stopDNSForBridge tears down the responder. Safe to call on a nil
// server (no-op).
func stopDNSForBridge(s *dns.Server) {
	if s == nil {
		return
	}
	s.Stop()
}

// dnsSet registers a sandbox name → IP mapping on the user's DNS
// responder. No-op if the user network doesn't have a DNS server
// (bind failed earlier). The engine.mu must NOT be held by the caller;
// this method acquires the read lock itself.
func (e *Engine) dnsSet(userID, name, ip string) {
	if name == "" || ip == "" {
		return
	}
	e.mu.RLock()
	un, ok := e.userNetworks[userID]
	e.mu.RUnlock()
	if !ok || un.DNS == nil {
		return
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return
	}
	un.DNS.Set(name, parsed)
}

// dnsDelete removes a sandbox name from the user's DNS responder.
// Same lock discipline as dnsSet.
func (e *Engine) dnsDelete(userID, name string) {
	if name == "" {
		return
	}
	e.mu.RLock()
	un, ok := e.userNetworks[userID]
	e.mu.RUnlock()
	if !ok || un.DNS == nil {
		return
	}
	un.DNS.Delete(name)
}
