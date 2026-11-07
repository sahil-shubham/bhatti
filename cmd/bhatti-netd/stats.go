package main

import (
	"bytes"
	"errors"
	"slices"
	"sync/atomic"

	"github.com/sahil-shubham/bhatti/pkg/gateway"
)

// guestCounters is one registered guest's traffic and refusals, counted the way
// its own NIC would. Every field is an atomic: the link reader, the stack's
// output loop and the forwarders bump them concurrently and Stats reads them
// without stopping any of them. They are reached through the registration the
// frame path already looks up, so counting adds no lock and no allocation.
//
// A nil *guestCounters counts nothing. That is what a guestState built by hand
// has, and what the deny fallback for an unregistered source must have: a
// guest that was never registered, or a spoofed source address, gets no state
// to grow.
type guestCounters struct {
	rxBytes, rxPackets atomic.Uint64
	txBytes, txPackets atomic.Uint64
	policy, spoof      atomic.Uint64
	hardDeny           atomic.Uint64
}

// rx counts one frame netd wrote to the guest's link.
func (c *guestCounters) rx(frameLen int) {
	if c == nil {
		return
	}
	c.rxPackets.Add(1)
	c.rxBytes.Add(uint64(frameLen))
}

// tx counts one frame the guest sent that passed the identity check.
func (c *guestCounters) tx(frameLen int) {
	if c == nil {
		return
	}
	c.txPackets.Add(1)
	c.txBytes.Add(uint64(frameLen))
}

// spoofed counts one frame that failed the identity check.
func (c *guestCounters) spoofed() {
	if c != nil {
		c.spoof.Add(1)
	}
}

// denied counts one attempt netd refused to carry. hard says no policy could
// have admitted its destination (gateway.Verdict.HardDeny).
func (c *guestCounters) denied(hard bool) {
	switch {
	case c == nil:
	case hard:
		c.hardDeny.Add(1)
	default:
		c.policy.Add(1)
	}
}

// dialFailed counts err as a refusal when the egress guard caused it. A dial
// that failed for network reasons (unreachable, timed out, reset) is the
// destination's doing, not netd's, and is not a drop.
func (c *guestCounters) dialFailed(err error) {
	var de *gateway.DeniedError
	if errors.As(err, &de) {
		c.denied(de.HardDeny)
	}
}

// read fills s with the counters' current values. Each is read on its own, so
// a frame in flight can show in its packet count a moment before its bytes.
func (c *guestCounters) read(s *gateway.GuestStats) {
	if c == nil {
		return
	}
	s.RxBytes, s.RxPackets = c.rxBytes.Load(), c.rxPackets.Load()
	s.TxBytes, s.TxPackets = c.txBytes.Load(), c.txPackets.Load()
	s.Dropped = gateway.DropStats{
		Policy:   c.policy.Load(),
		Spoof:    c.spoof.Load(),
		HardDeny: c.hardDeny.Load(),
	}
}

// ServeControl finds the stats op by asserting this interface: lose the method
// and the daemon would read every netd as one that predates stats.
var _ gateway.StatsProvider = (*Gateway)(nil)

// Stats reports every registered guest, ordered by address. It holds the
// registry lock only to list the registrations, never while reading counters.
func (g *Gateway) Stats() gateway.NetStats {
	g.polMu.RLock()
	guests := make([]*guestState, 0, len(g.guests))
	for _, st := range g.guests {
		guests = append(guests, st)
	}
	g.polMu.RUnlock()
	slices.SortFunc(guests, func(a, b *guestState) int {
		x, y := a.ip.As4(), b.ip.As4()
		return bytes.Compare(x[:], y[:])
	})
	out := make([]gateway.GuestStats, len(guests))
	for i, st := range guests {
		out[i].GuestIP = addrString(st.ip)
		out[i].SandboxID = st.sandbox
		st.stats.read(&out[i])
	}
	return gateway.NetStats{Guests: out}
}
