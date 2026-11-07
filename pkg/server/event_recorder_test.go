package server

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/store"
)

func TestEventRecorder(t *testing.T) {
	st, err := store.New(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	rec := NewEventRecorder(st)

	// Record some events
	rec.Record(store.Event{
		Type: "sandbox.created", UserID: "usr_1", SandboxID: "sb_1",
		Meta: map[string]any{"name": "dev"},
	})
	rec.Record(store.Event{
		Type: "sandbox.destroyed", UserID: "usr_1", SandboxID: "sb_1",
		Meta: map[string]any{"name": "dev"},
	})
	rec.Record(store.Event{
		Type: "auth.failed",
		Meta: map[string]any{"ip": "1.2.3.4"},
	})

	// Close waits for flush
	rec.Close()

	// Verify events were persisted
	events, err := st.QueryEvents(t.Context(), store.EventFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}

	// Check dropped counter
	if rec.Dropped.Load() != 0 {
		t.Errorf("expected 0 dropped, got %d", rec.Dropped.Load())
	}
}

func TestEventRecorderDropsOnFullBuffer(t *testing.T) {
	st, err := store.New(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	rec := NewEventRecorder(st)

	// Fill the buffer (1000 capacity) + try to overflow
	for i := 0; i < 1100; i++ {
		rec.Record(store.Event{
			Type: "test.event",
			Meta: map[string]any{"i": i},
		})
	}

	// Wait a bit for buffer to drain, then close
	time.Sleep(600 * time.Millisecond)
	rec.Close()

	dropped := rec.Dropped.Load()
	if dropped == 0 {
		// The goroutine might have drained fast enough. That's ok —
		// just verify we didn't panic.
		t.Log("no drops detected (goroutine drained fast enough)")
	} else {
		t.Logf("dropped %d events as expected", dropped)
	}

	// All non-dropped events should be persisted
	events, err := st.QueryEvents(t.Context(), store.EventFilter{Limit: 2000})
	if err != nil {
		t.Fatal(err)
	}
	persisted := int64(len(events))
	total := persisted + dropped
	if total != 1100 {
		t.Errorf("persisted(%d) + dropped(%d) = %d, expected 1100", persisted, dropped, total)
	}
}

func TestSandboxEventsKeepOwnerWhenDeletedBeforeFlush(t *testing.T) {
	for _, operation := range []string{"wake", "pause", "force_pause", "snapshot", "snapshot_failed", "lost"} {
		t.Run(operation, func(t *testing.T) {
			srv, ts := setup(t)
			eng := srv.engine.(*mockEngine)
			id := createRunningBox(t, srv, eng, operation)
			sb := findSandbox(t, srv, id)
			// Hold the recorder before its loop starts so deletion always wins
			// over the flush, without relying on the 500ms timer.
			rec := &EventRecorder{
				store: srv.store, ch: make(chan store.Event, 1000),
				done: make(chan struct{}), changed: make(chan struct{}),
			}
			srv.events = rec
			flush := sync.OnceFunc(func() {
				go rec.loop()
				rec.Close()
			})
			defer flush()

			want := []string{"thermal." + operation}
			switch operation {
			case "wake":
				eng.thermal[id] = "warm"
				if err := srv.ensureHot(t.Context(), id); err != nil {
					t.Fatal(err)
				}
			case "pause", "force_pause":
				srv.lastActivity.Store(id, time.Now().Add(-time.Hour))
				var thermal ThermalEngine = eng
				if operation == "force_pause" {
					eng.ActivityErr = errors.New("agent unavailable")
					thermal = &forcePauseTestEngine{mockEngine: eng}
					srv.engine = thermal.(*forcePauseTestEngine)
					for range maxThermalFailures - 1 {
						srv.incrementThermalFails(id)
					}
				}
				srv.runThermalCycle(thermal, ThermalConfig{WarmTimeout: time.Second, ColdTimeout: time.Hour})
			case "snapshot", "snapshot_failed":
				if operation == "snapshot_failed" {
					eng.StopErr = errors.New("snapshot unavailable")
				} else {
					want = []string{"sandbox.stopped", "thermal.snapshot"}
				}
				srv.stopColdSandbox(sb, time.Hour)
			case "lost":
				if err := eng.Stop(t.Context(), id); err != nil {
					t.Fatal(err)
				}
				srv.RecoverSandboxes(t.Context())
				want = []string{"sandbox.stopped"}
			}
			if len(rec.ch) != len(want) {
				t.Fatalf("queued %d events, want %v", len(rec.ch), want)
			}
			if err := srv.store.DeleteSandbox(t.Context(), sb.CreatedBy, sb.ID); err != nil {
				t.Fatal(err)
			}
			flush()
			got := recordsOf(t, doReq(t, ts, "GET", "/events", nil))
			if len(got) != len(want) {
				t.Fatalf("owner sees %v after deletion, want %v", got, want)
			}
			for i, record := range got {
				if record["type"] != want[i] || record["user_id"] != sb.CreatedBy {
					t.Errorf("event %d = %v, want %s owned by %s", i, record, want[i], sb.CreatedBy)
				}
			}
		})
	}
}
