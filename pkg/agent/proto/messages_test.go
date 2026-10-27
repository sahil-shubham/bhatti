package proto

import (
	"encoding/json"
	"testing"
	"time"
)

func TestActivityInfoOnHostClock(t *testing.T) {
	hostNow := time.Unix(1_800_000_000, 0)
	for _, c := range []struct {
		name string
		in   ActivityInfo
		want int64 // LastActivityUnix on the host clock
	}{
		// A guest paused warm for 35 s reads 35 s behind the host: stamped 4 s
		// before its reply, the activity was still 4 s ago on the host's clock.
		{"guest clock lags by its paused time", ActivityInfo{LastActivityUnix: 1_800_000_000 - 35 - 4, NowUnix: 1_800_000_000 - 35}, 1_800_000_000 - 4},
		{"guest clock in step", ActivityInfo{LastActivityUnix: 1_800_000_000 - 4, NowUnix: 1_800_000_000}, 1_800_000_000 - 4},
		{"guest clock ahead", ActivityInfo{LastActivityUnix: 1_800_000_090 - 4, NowUnix: 1_800_000_090}, 1_800_000_000 - 4},
		{"active right now", ActivityInfo{LastActivityUnix: 1_800_000_000 - 3600, NowUnix: 1_800_000_000 - 3600}, 1_800_000_000},
		// A stamp from after the reply's own clock reading (the clock stepped
		// back in between) is not activity in the future.
		{"stamp after now", ActivityInfo{LastActivityUnix: 1_800_000_000 - 100 + 50, NowUnix: 1_800_000_000 - 100}, 1_800_000_000},
		// An agent without now_unix: nothing to translate with.
		{"old agent", ActivityInfo{LastActivityUnix: 1_799_999_000}, 1_799_999_000},
		{"never active", ActivityInfo{NowUnix: 1_800_000_000 - 35}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := c.in
			in.ActiveSessions, in.AttachedSessions, in.BusyReason = 2, 1, "cpu"
			got := in.OnHostClock(hostNow)
			if got.LastActivityUnix != c.want {
				t.Fatalf("LastActivityUnix = %d, want %d", got.LastActivityUnix, c.want)
			}
			if got.ActiveSessions != 2 || got.AttachedSessions != 1 || got.BusyReason != "cpu" || got.NowUnix != c.in.NowUnix {
				t.Fatalf("OnHostClock changed more than the stamp: %+v", got)
			}
			if in.LastActivityUnix != c.in.LastActivityUnix {
				t.Fatal("OnHostClock mutated its receiver")
			}
		})
	}
}

// An agent built before now_unix/busy_reason decodes into the same struct, and a
// new one's idle reply is as small as the old one's.
func TestActivityInfoWireCompatibility(t *testing.T) {
	var old ActivityInfo
	if err := json.Unmarshal([]byte(`{"last_activity_unix":1799999000,"active_sessions":1,"attached_sessions":0}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.NowUnix != 0 || old.BusyReason != "" || old.LastActivityUnix != 1_799_999_000 {
		t.Fatalf("old agent reply decoded as %+v", old)
	}
	raw, err := json.Marshal(ActivityInfo{LastActivityUnix: 5})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"last_activity_unix":5,"active_sessions":0,"attached_sessions":0}` {
		t.Fatalf("zero-valued new fields leaked into the wire format: %s", raw)
	}
}
