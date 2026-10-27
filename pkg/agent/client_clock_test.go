package agent

import (
	"context"
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
)

func TestSyncClockSendsHostTimeAndRequiresEmptyAck(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	received := make(chan []byte, 1)
	guest := fakeAgent(t, func(typ byte, payload []byte) (byte, []byte) {
		if typ != proto.SET_TIME {
			return proto.ERROR, []byte("unexpected frame")
		}
		received <- append([]byte(nil), payload...)
		return proto.SET_TIME, nil
	})
	before := time.Now()
	if err := guest.SyncClock(ctx); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	payload := <-received
	if len(payload) != 8 {
		t.Fatalf("payload is %d bytes, want 8 (big-endian Unix nanoseconds)", len(payload))
	}
	sent := time.Unix(0, int64(binary.BigEndian.Uint64(payload)))
	if sent.Before(before) || sent.After(after) {
		t.Fatalf("stamp %s is outside the call window [%s, %s]", sent, before, after)
	}

	for _, tc := range []struct {
		name    string
		typ     byte
		payload []byte
		want    string
	}{
		{"guest error", proto.ERROR, []byte("set time: operation not permitted"), "operation not permitted"},
		{"old agent", proto.ERROR, []byte("unexpected frame type 0x1c"), "unexpected frame type 0x1c"},
		{"wrong ack", proto.RESEED, nil, "expected empty SET_TIME ack"},
		{"nonempty ack", proto.SET_TIME, []byte("x"), "expected empty SET_TIME ack"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			broken := fakeAgent(t, func(byte, []byte) (byte, []byte) { return tc.typ, tc.payload })
			if err := broken.SyncClock(ctx); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("SyncClock = %v, want error containing %q", err, tc.want)
			}
		})
	}
}
