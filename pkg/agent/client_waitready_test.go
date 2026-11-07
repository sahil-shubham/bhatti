package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
)

// A lohar that booted without its boot config has no token, so it reads our
// AUTH frame as the request. It never fetches the config again: waiting out the
// boot timeout (30 s) for it was the whole of the "agent not ready" hang.
func TestWaitReadyStopsAtAnAgentWithoutAToken(t *testing.T) {
	guest := fakeAgent(t, oldAgentReply)
	guest.token = "host-token"

	start := time.Now()
	err := guest.WaitReady(context.Background(), 30*time.Second)
	if !errors.Is(err, ErrGuestUnconfigured) {
		t.Fatalf("WaitReady = %v, want ErrGuestUnconfigured", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("WaitReady took %s to give up on a guest that can never become ready", took)
	}
}

// Without a token the client sends no AUTH frame, so an "unexpected frame"
// error is the guest's answer to something else and says nothing about its config.
func TestUnexpectedFrameWithoutAuthIsNotAnUnconfiguredGuest(t *testing.T) {
	guest := fakeAgent(t, oldAgentReply)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := guest.Exec(ctx, []string{"true"}, nil, ""); err == nil || errors.Is(err, ErrGuestUnconfigured) {
		t.Fatalf("Exec = %v, want a plain agent error", err)
	}
}

// A timeout alone doesn't say how far the guest got; the last probe's failure does.
func TestWaitReadyTimeoutCarriesTheLastProbeError(t *testing.T) {
	guest := fakeAgent(t, func(byte, []byte) (byte, []byte) { return proto.ERROR, []byte("rootfs unreadable") })

	err := guest.WaitReady(context.Background(), 300*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitReady = %v, want a deadline error", err)
	}
	if !strings.Contains(err.Error(), "rootfs unreadable") {
		t.Fatalf("WaitReady = %q, want what the guest last said", err)
	}
}
