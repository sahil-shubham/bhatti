package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
)

// The thermal manager measures idle time as time.Since(LastActivityUnix) on the
// host clock. A guest that spent 35 s paused warm has a clock 35 s behind, so a
// stamp it wrote 4 s ago must still read as 4 s ago, not 39.
func TestActivityStampIsOnTheHostClock(t *testing.T) {
	const lag = 35 * time.Second
	reply := func(info proto.ActivityInfo) *AgentClient {
		return fakeAgent(t, func(typ byte, _ []byte) (byte, []byte) {
			if typ != proto.ACTIVITY_REQ {
				return oldAgentReply(typ, nil)
			}
			raw, _ := json.Marshal(info)
			return proto.ACTIVITY_RESP, raw
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	guestNow := time.Now().Add(-lag)

	info, err := reply(proto.ActivityInfo{
		LastActivityUnix: guestNow.Add(-4 * time.Second).Unix(),
		NowUnix:          guestNow.Unix(),
		BusyReason:       "cpu",
	}).Activity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if idle := time.Since(time.Unix(info.LastActivityUnix, 0)); idle < 3*time.Second || idle > 7*time.Second {
		t.Fatalf("idle %s on the host clock; the guest was active 4s before its reply", idle)
	}
	if info.BusyReason != "cpu" {
		t.Fatalf("busy reason lost: %+v", info)
	}

	// A lohar that predates now_unix is taken at its word, as before.
	stamp := guestNow.Add(-4 * time.Second).Unix()
	old, err := reply(proto.ActivityInfo{LastActivityUnix: stamp}).Activity(ctx)
	if err != nil || old.LastActivityUnix != stamp {
		t.Fatalf("old agent stamp = %+v, %v; want %d untouched", old, err, stamp)
	}
}
