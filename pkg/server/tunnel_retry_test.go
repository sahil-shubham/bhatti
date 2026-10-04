package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent"
	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// A sandbox that just booted fresh may not be listening yet: within the grace
// window the proxy keeps retrying refusals; without one (a warm sandbox, whose
// app never stopped) a refusal fails at once.
func TestTunnelWaitsForColdStartedApp(t *testing.T) {
	eng := newMockEngine()
	info, err := eng.Create(context.Background(), engine.SandboxSpec{Name: "app"})
	if err != nil {
		t.Fatal(err)
	}

	eng.TunnelRefusals = 3
	tr := &tunnelTransport{engine: eng, engineID: info.EngineID, port: 3000, waitUp: coldStartGrace(true)}
	conn, err := tr.openTunnel(context.Background())
	if err != nil {
		t.Fatalf("after a cold start: %v", err)
	}
	conn.Close()

	eng.TunnelRefusals = 1
	tr.waitUp = coldStartGrace(false)
	start := time.Now()
	if _, err := tr.openTunnel(context.Background()); !errors.Is(err, agent.ErrPortRefused) {
		t.Fatalf("warm sandbox, nothing listening: err=%v, want ErrPortRefused", err)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatalf("warm sandbox waited %s before failing", time.Since(start))
	}
}
