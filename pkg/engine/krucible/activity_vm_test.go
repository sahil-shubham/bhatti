//go:build krucible

package krucible

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/gateway"
)

// A sandbox nobody talks to must still look busy when it is working, and idle
// when it is not. The thermal manager pauses a sandbox whose guest-reported
// idle time (host clock) passes 30 s, so each guest is left alone for longer
// than that before its Activity is read — no exec, no file access, nothing the
// host would count as use.
func TestKrucibleGuestActivityWithoutHostRequests(t *testing.T) {
	eng := newNetEngine(t)
	ke := eng.(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel) // not defer: the parallel subtests outlive this function's body

	const (
		quiet     = 40 * time.Second
		fresh     = 10 * time.Second // "within about 10 s of now"
		warmAfter = 30 * time.Second // the thermal manager's WarmTimeout
	)
	create := func(t *testing.T, name string) string {
		t.Helper()
		info, err := eng.Create(ctx, engine.SandboxSpec{Name: name, CPUs: 2, MemoryMB: 512,
			NetPolicy: &gateway.NetPolicyWire{Default: "public"}})
		if err != nil {
			t.Fatalf("Create(%s): %v", name, err)
		}
		t.Cleanup(func() { eng.Destroy(context.Background(), info.ID) })
		return info.ID
	}
	start := func(t *testing.T, id, output string, argv ...string) {
		t.Helper()
		if _, _, err := ke.ExecDetached(ctx, id, argv, output); err != nil {
			t.Fatalf("start %v: %v", argv, err)
		}
	}
	// idleness is what the thermal manager computes from the guest's report.
	idleness := func(t *testing.T, id string) (time.Duration, *proto.ActivityInfo) {
		t.Helper()
		info, err := ke.Activity(ctx, id)
		if err != nil {
			t.Fatalf("Activity: %v", err)
		}
		idle := time.Since(time.Unix(info.LastActivityUnix, 0))
		t.Logf("idle %s, busy_reason %q, guest clock %s behind the host", idle.Round(time.Second), info.BusyReason,
			time.Since(time.Unix(info.NowUnix, 0)).Round(time.Second))
		return idle, info
	}

	t.Run("busy loop", func(t *testing.T) {
		t.Parallel()
		id := create(t, "act-spin")
		start(t, id, "", "spin") // what `nohup sh -c 'while :; do :; done' &` is in a real rootfs
		time.Sleep(quiet)
		idle, info := idleness(t, id)
		if idle > fresh || !strings.Contains(info.BusyReason, "cpu") {
			t.Fatalf("a guest burning a CPU for %s looks idle for %s (busy_reason %q)", quiet, idle, info.BusyReason)
		}
	})

	t.Run("untouched", func(t *testing.T) {
		t.Parallel()
		id := create(t, "act-idle")
		time.Sleep(quiet + 5*time.Second)
		idle, info := idleness(t, id)
		// ARP/ND/MLD chatter on eth0 and the guest's own housekeeping are not work.
		if idle <= warmAfter || info.BusyReason != "" {
			t.Fatalf("an untouched sandbox looks busy: idle %s, busy_reason %q", idle, info.BusyReason)
		}
	})

	// Public servers drop a connection that sends nothing within 10-20 s
	// (measured against 1.1.1.1:443), so the idle connection is held between
	// two guest processes over eth0's own address — a peer that is not loopback.
	t.Run("idle connection", func(t *testing.T) {
		t.Parallel()
		id := create(t, "act-conn")
		waitFor := func(what string, ok func() bool) {
			t.Helper()
			for deadline := time.Now().Add(20 * time.Second); !ok(); time.Sleep(500 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatalf("timed out waiting for %s", what)
				}
			}
		}
		start(t, id, "", "netcheck", "serve", "9100")
		r, err := eng.Exec(ctx, id, []string{"netcheck", "localip"})
		if err != nil || r.ExitCode != 0 {
			t.Fatalf("netcheck localip: %+v, %v", r, err)
		}
		addr := strings.TrimSpace(r.Stdout) + ":9100"
		waitFor("the guest's server", func() bool {
			r, err := eng.Exec(ctx, id, []string{"netcheck", "dial", addr})
			return err == nil && r.ExitCode == 0
		})
		// Some work first (connections only count for a while after it), then
		// an ESTABLISHED connection that moves no bytes.
		start(t, id, "", "spin", "2")
		start(t, id, "/tmp/hold.out", "netcheck", "hold", addr, "300")
		waitFor("the idle connection", func() bool {
			var out bytes.Buffer
			_, _, err := ke.FileRead(ctx, id, "/tmp/hold.out", &out)
			return err == nil && strings.Contains(out.String(), "OK hold")
		})
		time.Sleep(quiet)
		idle, info := idleness(t, id)
		if idle > fresh || info.BusyReason != "conn" {
			t.Fatalf("a guest holding an idle connection looks idle for %s (busy_reason %q)", idle, info.BusyReason)
		}
	})

	// A guest paused warm keeps a frozen wall clock, so whatever it stamps lags
	// the host's clock by the time it spent paused (35 s here, more than
	// warmAfter): the stamp must still read as recent on the host.
	t.Run("busy after a warm pause", func(t *testing.T) {
		t.Parallel()
		if !ke.caps.Pause {
			t.Skip("bhatti-vmm build can't pause")
		}
		id := create(t, "act-warm")
		if err := ke.Pause(ctx, id); err != nil {
			t.Fatalf("Pause: %v", err)
		}
		time.Sleep(35 * time.Second)
		if err := ke.EnsureHot(ctx, id); err != nil {
			t.Fatalf("EnsureHot: %v", err)
		}
		start(t, id, "", "spin")
		time.Sleep(quiet)
		idle, info := idleness(t, id)
		if idle > fresh || !strings.Contains(info.BusyReason, "cpu") {
			t.Fatalf("a busy guest that was paused warm looks idle for %s (busy_reason %q)", idle, info.BusyReason)
		}
	})
}
