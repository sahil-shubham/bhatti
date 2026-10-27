package server

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
)

// A guest that works without any host request reports fresh activity and says
// why; the thermal manager keeps it hot and the debug log names the reason.
func TestThermalBusyGuestStaysHotAndSaysWhy(t *testing.T) {
	srv, _ := setup(t)
	eng := srv.engine.(*mockEngine)
	cfg := ThermalConfig{WarmTimeout: 30 * time.Second, ColdTimeout: time.Hour}
	eid := createRunningBox(t, srv, eng, "busy-box")
	// No host request for a minute, so the host's own cache cannot vouch for it.
	srv.lastActivity.Store(eid, time.Now().Add(-time.Minute))

	logs := &syncLogWriter{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)

	report := func(info proto.ActivityInfo) {
		eng.mu.Lock()
		eng.ActivityResult = &info
		eng.mu.Unlock()
	}
	state := func() string {
		eng.mu.Lock()
		defer eng.mu.Unlock()
		return eng.thermal[eid]
	}
	te := srv.engine.(ThermalEngine)

	report(proto.ActivityInfo{LastActivityUnix: time.Now().Unix(), BusyReason: "cpu"})
	srv.runThermalCycle(te, cfg)
	if got := state(); got != "hot" {
		t.Fatalf("busy guest was moved to %q", got)
	}
	if out := logs.String(); !strings.Contains(out, "thermal skip: guest busy") || !strings.Contains(out, "reason=cpu") {
		t.Fatalf("skip not logged with its reason: %q", out)
	}

	// Non-vacuous: once it stops working, the same sandbox pauses.
	report(proto.ActivityInfo{LastActivityUnix: time.Now().Add(-time.Minute).Unix()})
	srv.lastActivity.Store(eid, time.Now().Add(-time.Minute))
	srv.runThermalCycle(te, cfg)
	if got := state(); got != "warm" {
		t.Fatalf("idle guest was left %q", got)
	}
}
