package server

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/sahil-shubham/bhatti/pkg/store"
)

// Tests for the EventRecorder's in-process pub/sub fan-out.
// G1.5 of PLAN-bhatti-v2.md. The recorder still persists events to
// SQLite in batches; what's new is that Record() also fans out to any
// in-process Subscribe()-rs.

// newTestRecorder is a small helper: temp dir, real store, attached
// recorder, t.Cleanup wires the close.
func newTestRecorder(t *testing.T) *EventRecorder {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "ev.db"))
	if err != nil {
		t.Fatal(err)
	}
	rec := NewEventRecorder(st)
	t.Cleanup(func() {
		rec.Close()
		st.Close()
	})
	return rec
}

// recvWithTimeout pulls one event from c or fails the test if nothing
// arrives within d.
func recvWithTimeout(t *testing.T, c <-chan store.Event, d time.Duration) store.Event {
	t.Helper()
	select {
	case e, ok := <-c:
		if !ok {
			t.Fatal("channel closed unexpectedly")
		}
		return e
	case <-time.After(d):
		t.Fatalf("no event in %s", d)
		return store.Event{}
	}
}

// expectClosed asserts c is closed within d. A closed channel returns
// the zero value immediately; an open-but-empty channel blocks.
func expectClosed(t *testing.T, c <-chan store.Event, d time.Duration) {
	t.Helper()
	select {
	case _, ok := <-c:
		if ok {
			t.Fatal("expected closed channel, got an event")
		}
	case <-time.After(d):
		t.Fatal("channel did not close within timeout")
	}
}

// TestSubscribe_ReceivesMatchingEvent: zero-value filter receives all
// events. Sanity check that the fan-out wiring works at all.
func TestSubscribe_ReceivesMatchingEvent(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	rec := newTestRecorder(t)

	sub := rec.Subscribe(SubscriptionFilter{})
	defer sub.Cancel()

	rec.Record(store.Event{Type: "sandbox.created", SandboxID: "sb1"})
	got := recvWithTimeout(t, sub.C, time.Second)
	if got.Type != "sandbox.created" || got.SandboxID != "sb1" {
		t.Fatalf("got %+v", got)
	}
}

// TestSubscribe_FilterByTypePrefix: only "thermal.*" events arrive.
func TestSubscribe_FilterByTypePrefix(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	rec := newTestRecorder(t)

	sub := rec.Subscribe(SubscriptionFilter{TypePrefix: "thermal"})
	defer sub.Cancel()

	rec.Record(store.Event{Type: "sandbox.created"})
	rec.Record(store.Event{Type: "thermal.snapshot"})
	rec.Record(store.Event{Type: "auth.failed"})
	rec.Record(store.Event{Type: "thermal.pause"})

	got1 := recvWithTimeout(t, sub.C, time.Second)
	got2 := recvWithTimeout(t, sub.C, time.Second)

	if got1.Type != "thermal.snapshot" || got2.Type != "thermal.pause" {
		t.Fatalf("got %q, %q; want thermal.snapshot, thermal.pause", got1.Type, got2.Type)
	}

	// Sanity: no third event in the next 100ms (the two non-thermal
	// records must not have been delivered).
	select {
	case e := <-sub.C:
		t.Fatalf("unexpected event %+v", e)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestSubscribe_FilterBySandbox: SandboxID exact match.
func TestSubscribe_FilterBySandbox(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	rec := newTestRecorder(t)

	sub := rec.Subscribe(SubscriptionFilter{SandboxID: "sb_target"})
	defer sub.Cancel()

	rec.Record(store.Event{Type: "sandbox.created", SandboxID: "sb_other"})
	rec.Record(store.Event{Type: "sandbox.destroyed", SandboxID: "sb_target"})
	rec.Record(store.Event{Type: "sandbox.destroyed", SandboxID: "sb_other"})

	got := recvWithTimeout(t, sub.C, time.Second)
	if got.SandboxID != "sb_target" {
		t.Fatalf("got SandboxID=%q, want sb_target", got.SandboxID)
	}
}

// TestSubscribe_MultipleSubscribers: each gets a copy of every matching
// event. Tests the fan-out shape (one Record -> N subscribers).
func TestSubscribe_MultipleSubscribers(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	rec := newTestRecorder(t)

	a := rec.Subscribe(SubscriptionFilter{})
	b := rec.Subscribe(SubscriptionFilter{})
	defer a.Cancel()
	defer b.Cancel()

	rec.Record(store.Event{Type: "x"})

	gotA := recvWithTimeout(t, a.C, time.Second)
	gotB := recvWithTimeout(t, b.C, time.Second)
	if gotA.Type != "x" || gotB.Type != "x" {
		t.Fatalf("a=%q b=%q", gotA.Type, gotB.Type)
	}
}

// TestSubscribe_CancelClosesChannel: Cancel must close C so consumers
// ranging over it exit.
func TestSubscribe_CancelClosesChannel(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	rec := newTestRecorder(t)

	sub := rec.Subscribe(SubscriptionFilter{})
	sub.Cancel()
	expectClosed(t, sub.C, time.Second)

	// Idempotent: a second Cancel must not panic.
	sub.Cancel()
}

// TestSubscribe_CloseEventRecorderClosesAllSubs: when the EventRecorder
// itself shuts down, every active subscription's channel closes.
func TestSubscribe_CloseEventRecorderClosesAllSubs(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	st, err := store.New(filepath.Join(t.TempDir(), "ev.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	rec := NewEventRecorder(st)
	a := rec.Subscribe(SubscriptionFilter{})
	b := rec.Subscribe(SubscriptionFilter{})

	rec.Close()

	expectClosed(t, a.C, time.Second)
	expectClosed(t, b.C, time.Second)
}

// TestSubscribe_SlowSubscriberDisconnected: a subscriber that doesn't
// drain its channel gets disconnected once the buffer (subscriberBuffer)
// fills. The substrate is not a queue.
func TestSubscribe_SlowSubscriberDisconnected(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	rec := newTestRecorder(t)

	// Subscribe, then deliberately don't drain.
	sub := rec.Subscribe(SubscriptionFilter{})

	// Push subscriberBuffer events (fills the channel) + one more
	// (triggers the disconnect on the over-fill attempt).
	for i := 0; i < subscriberBuffer+1; i++ {
		rec.Record(store.Event{Type: "burst", SandboxID: fmt.Sprintf("sb%d", i)})
	}

	// Drain what made it in (subscriberBuffer events), then verify
	// the channel is closed.
	got := 0
	for {
		select {
		case _, ok := <-sub.C:
			if !ok {
				goto closed
			}
			got++
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("channel not closed after %d events drained (expected close at %d)", got, subscriberBuffer)
		}
	}
closed:
	if got != subscriberBuffer {
		t.Fatalf("drained %d events before close, expected %d", got, subscriberBuffer)
	}
}

// TestSubscribe_ConcurrentRecordSubscribeCancel exercises the locking
// under load. Concurrent producers + consumers + churning subscriptions.
// goleak.VerifyNone catches any goroutine that escapes.
func TestSubscribe_ConcurrentRecordSubscribeCancel(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	rec := newTestRecorder(t)

	const producers = 4
	const subscribers = 8
	const eventsPerProducer = 200

	// Long-lived drainers that just consume until their channel closes.
	subs := make([]*Subscription, subscribers)
	var subsWG sync.WaitGroup
	for i := 0; i < subscribers; i++ {
		subs[i] = rec.Subscribe(SubscriptionFilter{})
		subsWG.Add(1)
		go func(s *Subscription) {
			defer subsWG.Done()
			for range s.C {
			}
		}(subs[i])
	}

	// Producers
	var prodWG sync.WaitGroup
	for i := 0; i < producers; i++ {
		prodWG.Add(1)
		go func(pid int) {
			defer prodWG.Done()
			for j := 0; j < eventsPerProducer; j++ {
				rec.Record(store.Event{
					Type:     "x.y",
					SandboxID: fmt.Sprintf("sb_%d_%d", pid, j),
				})
			}
		}(i)
	}

	// Churners: subscribe + cancel in a tight loop.
	churnDone := make(chan struct{})
	var churnWG sync.WaitGroup
	churnWG.Add(1)
	go func() {
		defer churnWG.Done()
		for {
			select {
			case <-churnDone:
				return
			default:
				s := rec.Subscribe(SubscriptionFilter{TypePrefix: "x"})
				s.Cancel()
			}
		}
	}()

	prodWG.Wait()
	close(churnDone)
	churnWG.Wait()

	// Cancel all long-lived subs to let their drainers exit.
	for _, s := range subs {
		s.Cancel()
	}
	subsWG.Wait()
}
