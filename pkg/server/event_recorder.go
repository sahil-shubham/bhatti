package server

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/store"
)

// EventRecorder buffers events and flushes them to SQLite in batches. The
// events table is the stream: a reader keeps its own cursor into it (see
// store.EventsAfter) and uses Changed to learn when to look again. Readers
// share one periodic live-edge check for external commits; a slow reader holds
// no recorder buffer because its unread events remain in the table.
//
// Record never blocks: when the buffer is full the event is not stored and is
// counted in Dropped. A recorded event is stored within about 500ms.
type EventRecorder struct {
	store     *store.Store
	ch        chan store.Event
	done      chan struct{}
	Dropped   atomic.Int64
	mu        sync.Mutex // coordinates Record's send with Close's channel close
	closed    bool
	closeOnce sync.Once

	// Local flushes and observed external commits close and replace changed.
	// Nothing is sent on it, so no waiter can be too slow for the recorder.
	followers atomic.Int64
	changedMu sync.Mutex
	changed   chan struct{}
}

// NewEventRecorder creates and starts an EventRecorder.
// Channel buffer: 1000 events. Flushes every 500ms or at 100 events.
func NewEventRecorder(st *store.Store) *EventRecorder {
	r := &EventRecorder{
		store:   st,
		ch:      make(chan store.Event, 1000),
		done:    make(chan struct{}),
		changed: make(chan struct{}),
	}
	goSafe("event recorder", r.loop)
	return r
}

// Record enqueues an event for persistence. It does not block: if the buffer is
// full the event is dropped from the persistent stream and Dropped is bumped.
func (r *EventRecorder) Record(e store.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	select {
	case r.ch <- e:
	default:
		r.Dropped.Add(1)
	}
}

// Changed returns a channel closed after a local flush or an observed external
// commit. Take it before reading the table, never after: otherwise a commit
// during the read can be missed and the reader sleeps on rows it has not seen.
// tail also registers its reader for the shared external-commit check.
func (r *EventRecorder) Changed() <-chan struct{} {
	r.changedMu.Lock()
	defer r.changedMu.Unlock()
	return r.changed
}

// tail calls read after each observed commit until ctx ends or read fails.
// Its readers share the loop's periodic check for commits by other processes.
func (r *EventRecorder) tail(ctx context.Context, read func() error) error {
	r.followers.Add(1)
	defer r.followers.Add(-1)
	for {
		changed := r.Changed()
		if err := read(); err != nil {
			return err
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// committed wakes every reader waiting on Changed, once the rows they will
// look for are in the table.
func (r *EventRecorder) committed() {
	r.changedMu.Lock()
	close(r.changed)
	r.changed = make(chan struct{})
	r.changedMu.Unlock()
}

// Close stores the events still buffered and stops the background goroutine.
// That last flush wakes the readers waiting on Changed if it stored anything;
// nothing wakes them after it, so a reader that must end with the server needs
// a context of its own, as the stream handlers have.
func (r *EventRecorder) Close() {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		close(r.ch)
		r.mu.Unlock()
		<-r.done
	})
}

// StartEventRecorder creates and attaches an EventRecorder to the server.
func (s *Server) StartEventRecorder() {
	s.bgMu.Lock()
	defer s.bgMu.Unlock()
	if s.closed || s.events != nil {
		return
	}
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
	var observedNewest int64

	flush := func() {
		if len(batch) == 0 {
			return
		}
		// Not the server's context: the last batch is flushed from Close, after
		// every context the server owns has been cancelled.
		if err := r.store.InsertEvents(context.Background(), batch); err != nil {
			slog.Error("event recorder flush failed", "count", len(batch), "error", err)
			// Events are lost on flush failure — acceptable trade-off
			// vs blocking the API. The dropped counter doesn't track
			// flush failures (those are rare and worth an error log).
		} else {
			r.committed()
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
			if r.followers.Load() > 0 {
				// One indexed MAX seek per tick, shared by all followers. Unlike
				// data_version it is comparable across pooled connections and
				// ignores unrelated writes to sandboxes and metrics.
				newest, err := r.store.NewestEventID(context.Background())
				if err != nil {
					slog.Error("events.poll_failed", "error", err)
				} else if newest != observedNewest {
					observedNewest = newest
					r.committed()
				}
			}
		}
	}
}
