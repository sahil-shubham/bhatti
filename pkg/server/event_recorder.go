package server

import (
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/store"
)

// EventRecorder buffers events and flushes them to SQLite in batches.
// Record() is non-blocking — if the buffer is full, the event is dropped
// and the dropped counter is incremented. The metrics snapshot goroutine
// reads the dropped counter.
type EventRecorder struct {
	store   *store.Store
	ch      chan store.Event
	done    chan struct{}
	Dropped atomic.Int64
}

// NewEventRecorder creates and starts an EventRecorder.
// Channel buffer: 1000 events. Flushes every 500ms or at 100 events.
func NewEventRecorder(st *store.Store) *EventRecorder {
	r := &EventRecorder{
		store: st,
		ch:    make(chan store.Event, 1000),
		done:  make(chan struct{}),
	}
	go r.loop()
	return r
}

// Record enqueues an event. Non-blocking — drops if buffer is full.
func (r *EventRecorder) Record(e store.Event) {
	select {
	case r.ch <- e:
	default:
		r.Dropped.Add(1)
	}
}

// Close drains remaining events and stops the background goroutine.
func (r *EventRecorder) Close() {
	close(r.ch)
	<-r.done
}

// StartEventRecorder creates and attaches an EventRecorder to the server.
func (s *Server) StartEventRecorder() {
	s.events = NewEventRecorder(s.store)
}

// RecordEvent is a convenience method that records an event if the recorder exists.
func (s *Server) RecordEvent(e store.Event) {
	if s.events != nil {
		s.events.Record(e)
	}
}

func (r *EventRecorder) loop() {
	defer close(r.done)

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	var batch []store.Event

	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := r.store.InsertEvents(batch); err != nil {
			slog.Error("event recorder flush failed", "count", len(batch), "error", err)
			// Events are lost on flush failure — acceptable trade-off
			// vs blocking the API. The dropped counter doesn't track
			// flush failures (those are rare and worth an error log).
		}
		batch = batch[:0]
	}

	for {
		select {
		case e, ok := <-r.ch:
			if !ok {
				// Channel closed — drain and exit.
				flush()
				return
			}
			batch = append(batch, e)
			if len(batch) >= 100 {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}
