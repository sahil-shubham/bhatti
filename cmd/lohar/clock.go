//go:build linux

package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
	"golang.org/x/sys/unix"
)

// setRealtimeClock is a variable so tests can observe the step without
// moving the test host's clock.
var setRealtimeClock = func(t time.Time) error {
	ts := unix.NsecToTimespec(t.UnixNano())
	return unix.ClockSettime(unix.CLOCK_REALTIME, &ts)
}

// handleSetTime steps CLOCK_REALTIME to the host's wall time. A warm pause
// freezes kvmclock and RESUME restores it, so CLOCK_REALTIME falls behind the
// host by the length of every pause; CLOCK_MONOTONIC is left alone, since no
// time passed for the guest's processes. The host stamps the time just before
// sending, so the result is off by one vsock hop: far inside the accuracy
// that skew-sensitive callers (SigV4, JWT expiry, TLS) need, so there is no
// latency compensation here.
func handleSetTime(conn net.Conn, payload []byte) {
	if len(payload) != 8 {
		proto.WriteFrame(conn, proto.ERROR, []byte(fmt.Sprintf("set time: expected 8 bytes, got %d", len(payload))))
		return
	}
	nanos := int64(binary.BigEndian.Uint64(payload))
	if nanos <= 0 {
		proto.WriteFrame(conn, proto.ERROR, []byte(fmt.Sprintf("set time: invalid timestamp %d", nanos)))
		return
	}
	if err := setRealtimeClock(time.Unix(0, nanos)); err != nil {
		proto.WriteFrame(conn, proto.ERROR, []byte(fmt.Sprintf("set time: %v", err)))
		return
	}
	// lastActivity is a CLOCK_REALTIME stamp: after a forward step every stamp
	// written before it reads that much older. Waking the guest is activity
	// anyway, and this stamp is taken on the new clock.
	updateActivity()
	proto.WriteFrame(conn, proto.SET_TIME, nil)
}
