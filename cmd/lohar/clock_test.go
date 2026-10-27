//go:build linux

package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
)

// controlExchange runs one frame through handleControlConnection and returns
// the reply, with setRealtimeClock replaced so the test host's clock is never
// touched.
func controlExchange(t *testing.T, set func(time.Time) error, typ byte, payload []byte) (byte, []byte) {
	t.Helper()
	oldSet, oldToken := setRealtimeClock, agentToken
	setRealtimeClock, agentToken = set, ""
	t.Cleanup(func() { setRealtimeClock, agentToken = oldSet, oldToken })
	host, guest := net.Pipe()
	defer host.Close()
	go handleControlConnection(guest)
	if err := proto.WriteFrame(host, typ, payload); err != nil {
		t.Fatal(err)
	}
	replyType, reply, err := proto.ReadFrame(host)
	if err != nil {
		t.Fatal(err)
	}
	return replyType, reply
}

func timestamp(nanos int64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(nanos))
	return b[:]
}

func TestSetTimeStepsRealtimeClockToHostTime(t *testing.T) {
	atomic.StoreInt64(&lastActivity, 0)
	want := time.Date(2026, 10, 27, 12, 30, 15, 123456789, time.UTC)
	var got []time.Time
	typ, reply := controlExchange(t, func(at time.Time) error {
		got = append(got, at)
		return nil
	}, proto.SET_TIME, timestamp(want.UnixNano()))
	if typ != proto.SET_TIME || len(reply) != 0 {
		t.Fatalf("reply type 0x%02x (%d bytes), want empty SET_TIME ack", typ, len(reply))
	}
	if len(got) != 1 || !got[0].Equal(want) {
		t.Fatalf("clock set to %v, want exactly %v", got, want)
	}
	// Stamps written before a forward step read older afterwards, so the
	// handler restamps activity on the new clock.
	if atomic.LoadInt64(&lastActivity) == 0 {
		t.Fatal("activity not restamped after the clock step")
	}
}

func TestSetTimeRejectsBadRequestsWithoutTouchingTheClock(t *testing.T) {
	for name, payload := range map[string][]byte{
		"empty":     nil,
		"truncated": {1, 2, 3},
		"too long":  append(timestamp(1), 0),
		"zero":      timestamp(0),
		"negative":  timestamp(-5),
	} {
		t.Run(name, func(t *testing.T) {
			typ, reply := controlExchange(t, func(time.Time) error {
				t.Error("clock set for an invalid request")
				return nil
			}, proto.SET_TIME, payload)
			if typ != proto.ERROR || !strings.HasPrefix(string(reply), "set time:") {
				t.Fatalf("reply 0x%02x %q, want ERROR \"set time: ...\"", typ, reply)
			}
		})
	}
}

func TestSetTimeReportsSetterFailure(t *testing.T) {
	atomic.StoreInt64(&lastActivity, 0)
	typ, reply := controlExchange(t, func(time.Time) error { return errors.New("operation not permitted") },
		proto.SET_TIME, timestamp(time.Now().UnixNano()))
	if typ != proto.ERROR || !strings.Contains(string(reply), "operation not permitted") {
		t.Fatalf("reply 0x%02x %q, want the setter's error", typ, reply)
	}
	if atomic.LoadInt64(&lastActivity) != 0 {
		t.Fatal("activity restamped although the clock step failed")
	}
}

func TestInfoAdvertisesClockSync(t *testing.T) {
	typ, reply := controlExchange(t, nil, proto.INFO_REQ, nil)
	var info proto.AgentInfo
	if typ != proto.INFO_RESP || json.Unmarshal(reply, &info) != nil || !info.Has(proto.FeatureClockSync) {
		t.Fatalf("info reply 0x%02x %s does not advertise %q", typ, reply, proto.FeatureClockSync)
	}
}
