package server

import (
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/store"
)

// subscriberBuffer is the per-subscriber channel capacity. A subscriber
// whose consumer is too slow to drain at this rate has its channel
// filled and is disconnected on the next fan-out. The substrate is
// not a message queue — consumers that need at-least-once delivery
// route through the events table via /events?since=<id> (Bet 4).
const subscriberBuffer = 64

// SubscriptionFilter matches a subset of events. Empty fields match all
// events on that dimension. To subscribe to every event, pass a
// zero-value SubscriptionFilter.
type SubscriptionFilter struct {
	TypePrefix string // e.g. "thermal" matches "thermal.snapshot", "thermal.pause"
	SandboxID  string // exact match
	UserID     string // exact match
}

// matches returns true if e satisfies the filter.
func (f SubscriptionFilter) matches(e store.Event) bool {
	if f.TypePrefix != "" && !strings.HasPrefix(e.Type, f.TypePrefix) {
		return false
	}
	if f.SandboxID != "" && f.SandboxID != e.SandboxID {
		return false
	}
	if f.UserID != "" && f.UserID != e.UserID {
		return false
	}
	return true
}

// Subscription is the consumer side of an event subscription. C delivers
// every event matching the filter passed to Subscribe. The channel is
// closed when Cancel is called, when EventRecorder.Close is called, or
// when the subscriber is disconnected for being slow (its buffer would
// have blocked a fan-out write). Range over C until it closes; a closed
// channel signals "no more events for this subscription, for whatever
// reason."
type Subscription struct {
	C      <-chan store.Event
	Cancel func()
}

// subscriber is the internal-only state for one Subscription.
type subscriber struct {
	filter SubscriptionFilter
	ch     chan store.Event
	closed atomic.Bool // ensures close(ch) happens at most once
}

// EventRecorder buffers events and flushes them to SQLite in batches,
// AND fans them out in-process to subscribers registered via Subscribe.
// Record() is non-blocking: if the SQLite buffer is full, the event is
// dropped from the persistent stream (counted in Dropped). Fan-out to
// subscribers happens inline on the Record caller's goroutine; slow
// subscribers (whose per-subscriber buffer would block) are
// disconnected, not blocked.
type EventRecorder struct {
	store   *store.Store
	ch      chan store.Event
	done    chan struct{}
	Dropped atomic.Int64

	// Subscriber state. subsMu serialises fan-out, Subscribe, and
	// disconnect/Cancel — channel close is performed under the lock
	// so a concurrent fan-out can't observe a closed channel and
	// panic on send.
	subsMu sync.Mutex
	subs   map[int64]*subscriber
	nextID atomic.Int64
}

// NewEventRecorder creates and starts an EventRecorder.
// Channel buffer: 1000 events. Flushes every 500ms or at 100 events.
func NewEventRecorder(st *store.Store) *EventRecorder {
	r := &EventRecorder{
		store: st,
		ch:    make(chan store.Event, 1000),
		done:  make(chan struct{}),
		subs:  make(map[int64]*subscriber),
	}
	go r.loop()
	return r
}

// Record enqueues an event for persistence and fans it out to every
// matching subscriber. The persistent enqueue is non-blocking (drops if
// the SQLite buffer is full and bumps Dropped). The fan-out is also
// non-blocking per subscriber; subscribers whose buffer is full are
// disconnected.
func (r *EventRecorder) Record(e store.Event) {
	select {
	case r.ch <- e:
	default:
		r.Dropped.Add(1)
	}
	r.fanOut(e)
}

// Subscribe registers a new subscriber and returns the consumer side.
// The returned Subscription.C delivers every event matching f until
// Cancel is called, the subscriber is disconnected for being slow, or
// EventRecorder.Close is called.
func (r *EventRecorder) Subscribe(f SubscriptionFilter) *Subscription {
	id := r.nextID.Add(1)
	sub := &subscriber{
		filter: f,
		ch:     make(chan store.Event, subscriberBuffer),
	}
	r.subsMu.Lock()
	r.subs[id] = sub
	r.subsMu.Unlock()
	return &Subscription{
		C:      sub.ch,
		Cancel: func() { r.disconnect(id) },
	}
}

// disconnect removes the subscriber from the map and closes its
// channel. Idempotent (the atomic.Bool guards the close call).
func (r *EventRecorder) disconnect(id int64) {
	r.subsMu.Lock()
	defer r.subsMu.Unlock()
	sub, ok := r.subs[id]
	if !ok {
		return
	}
	delete(r.subs, id)
	if sub.closed.CompareAndSwap(false, true) {
		close(sub.ch)
	}
}

// fanOut delivers e to every matching subscriber. Subscribers whose
// buffered channel is full are disconnected — the substrate refuses
// to provide back-pressure to producers because of slow consumers.
//
// Holds subsMu for the duration so that Cancel/disconnect can't race
// with a send-to-channel here. Channel sends to a non-full buffered
// channel are O(1) and don't block, so the critical section is bounded
// by len(subs) × a few nanoseconds.
func (r *EventRecorder) fanOut(e store.Event) {
	r.subsMu.Lock()
	defer r.subsMu.Unlock()
	var disconnected []int64
	for id, sub := range r.subs {
		if !sub.filter.matches(e) {
			continue
		}
		select {
		case sub.ch <- e:
		default:
			disconnected = append(disconnected, id)
		}
	}
	for _, id := range disconnected {
		sub := r.subs[id]
		delete(r.subs, id)
		if sub.closed.CompareAndSwap(false, true) {
			close(sub.ch)
		}
	}
}

// Close drains remaining events, stops the background goroutine, and
// closes every active subscription's channel so consumers ranging
// over Subscription.C exit cleanly.
func (r *EventRecorder) Close() {
	r.subsMu.Lock()
	for id, sub := range r.subs {
		if sub.closed.CompareAndSwap(false, true) {
			close(sub.ch)
		}
		delete(r.subs, id)
	}
	r.subsMu.Unlock()
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
