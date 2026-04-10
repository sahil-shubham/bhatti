package server

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/store"
)

func TestEventRecorder(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "test.db"))
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
	events, err := st.QueryEvents(store.EventFilter{Limit: 100})
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
	st, err := store.New(filepath.Join(t.TempDir(), "test.db"))
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
	events, err := st.QueryEvents(store.EventFilter{Limit: 2000})
	if err != nil {
		t.Fatal(err)
	}
	persisted := int64(len(events))
	total := persisted + dropped
	if total != 1100 {
		t.Errorf("persisted(%d) + dropped(%d) = %d, expected 1100", persisted, dropped, total)
	}
}
