package gateway

// This file is the daemon's read of a netd: per guest, how much traffic crossed
// its NIC and how many attempts netd refused to carry. It rides the control
// channel as one more op, on a connection of its own (the one ControlClient
// keeps is for pushing policy, and a slow netd must not stall a push).
//
// The op is not an enforcement change, so EnforcementVersion stays put: a bump
// makes the daemon replace every running netd, and that cuts their guests off.
// A netd that predates it answers "unknown control operation" and QueryStats
// reports ErrStatsUnsupported.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"time"
)

// ControlStats asks a netd for its per-guest traffic report.
const ControlStats ControlOp = "stats"

// statsTimeout caps one query, dial and reply together. The collector polls
// every owner's netd in turn, so a wedged netd delays the poll by this and no
// more.
const statsTimeout = time.Second

// DropStats counts the guest's attempts netd did not carry, by reason.
type DropStats struct {
	Policy   uint64 `json:"policy"`    // refused by the guest's egress policy
	Spoof    uint64 `json:"spoof"`     // frames failing the link identity check
	HardDeny uint64 `json:"hard_deny"` // refused because the destination can never be allowed (host, loopback, link-local/metadata, gateway, other owners' ranges)
}

// GuestStats is one registered guest's traffic as its own NIC would count it:
// Rx is what netd delivered to the guest, Tx what the guest sent and netd
// accepted. Bytes are Ethernet frame lengths. Counters only grow while the
// guest stays registered: after a delete, or when its address passes to another
// sandbox, they start again from zero, so a reader treats a drop as a reset.
type GuestStats struct {
	GuestIP   string    `json:"guest_ip"`
	SandboxID string    `json:"sandbox_id"` // what the daemon registered the guest as
	RxBytes   uint64    `json:"rx_bytes"`
	RxPackets uint64    `json:"rx_packets"`
	TxBytes   uint64    `json:"tx_bytes"`
	TxPackets uint64    `json:"tx_packets"`
	Dropped   DropStats `json:"dropped"`
}

// NetStats is one netd's report, guests ordered by IP address (numerically).
type NetStats struct {
	Guests []GuestStats `json:"guests"`
}

// StatsProvider is implemented by a ControlHandler that can report traffic
// (cmd/bhatti-netd's *Gateway does). ServeControl finds it by type assertion
// because adding a method to ControlHandler would break every fake of it.
type StatsProvider interface {
	Stats() NetStats
}

// ErrStatsUnsupported means the netd answered but cannot report traffic: one
// built before the stats op existed.
var ErrStatsUnsupported = errors.New("netd does not support stats")

// answerStats fills reply from h's report. A handler without one gets the
// answer an old netd gives, since to the asker both mean "cannot report".
func (r *controlReply) answerStats(h ControlHandler) {
	sp, ok := h.(StatsProvider)
	if !ok {
		r.Error = errUnknownOp
		return
	}
	s := sp.Stats()
	if s.Guests == nil {
		s.Guests = []GuestStats{} // the wire carries an empty list, never null
	}
	r.Stats = &s
}

// QueryStats asks the netd whose control socket is at path for its per-guest
// traffic. It uses a short-lived connection and gives up when ctx ends or a
// second has passed, whichever is first. A netd that does not know the op, or
// a handler with nothing to report, yields ErrStatsUnsupported.
func QueryStats(ctx context.Context, path string) (NetStats, error) {
	ctx, cancel := context.WithTimeout(ctx, statsTimeout)
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return NetStats{}, statsErr(ctx, err)
	}
	defer conn.Close()
	// A connection's deadline knows nothing of ctx, so cancellation becomes a
	// deadline in the past, which wakes a read that is blocked on a silent netd.
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()

	if err := json.NewEncoder(conn).Encode(ControlMsg{Op: ControlStats}); err != nil {
		return NetStats{}, statsErr(ctx, err)
	}
	var reply controlReply
	if err := json.NewDecoder(conn).Decode(&reply); err != nil {
		return NetStats{}, statsErr(ctx, err)
	}
	switch {
	case reply.Error == errUnknownOp:
		return NetStats{}, ErrStatsUnsupported
	case reply.Error != "":
		return NetStats{}, fmt.Errorf("netd stats: %s", reply.Error)
	case reply.Stats == nil:
		return NetStats{}, errors.New("netd stats: reply carried no stats")
	}
	return *reply.Stats, nil
}

// statsErr names a failed exchange. The only deadline on the connection is the
// one taken from ctx, so a read or write that timed out means ctx's time ran
// out, even in the instant before ctx's own timer has noted it. Either way the
// context's reason is the truer one, not the i/o error that merely woke the call.
func statsErr(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		err = cerr
	} else if errors.Is(err, os.ErrDeadlineExceeded) {
		err = context.DeadlineExceeded
	}
	return fmt.Errorf("netd stats: %w", err)
}
