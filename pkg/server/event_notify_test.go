package server

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/sahil-shubham/bhatti/pkg/store"
)

// The recorder stores events in batches and wakes readers after each batch is
// committed. A reader keeps a cursor into the events table (store.EventsAfter)
// and reads from it when woken, so these tests read the table as a consumer
// does, with a real store and the real recorder.

// newTestRecorder is a small helper: temp dir, real store, attached
// recorder, t.Cleanup wires the close.
func newTestRecorder(t *testing.T) (*EventRecorder, *store.Store) {
	t.Helper()
	st, err := store.New(t.Context(), filepath.Join(t.TempDir(), "ev.db"))
	if err != nil {
		t.Fatal(err)
	}
	rec := NewEventRecorder(st)
	t.Cleanup(func() {
		rec.Close()
		st.Close()
	})
	return rec, st
}

// readAfter is one read of a reader's cursor: user's events after the id.
func readAfter(t *testing.T, st *store.Store, user string, after int64) []store.Event {
	t.Helper()
	events, err := st.EventsAfter(t.Context(), store.EventQuery{UserID: user, After: after, Limit: 5000})
	if err != nil {
		t.Fatal(err)
	}
	return events.Events
}

// waitForCount waits, as a reader does (take the wake-up channel, then read,
// then wait), until the table holds n of user's events.
func waitForCount(t *testing.T, rec *EventRecorder, st *store.Store, user string, n int) {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		changed := rec.Changed()
		if countEvents(t, st, user) >= n {
			return
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("the table never reached %d events for %s", n, user)
		}
	}
}

// followTable is a reader as the stream handler is one: it takes the wake-up
// channel, reads from its cursor, and waits. It returns the ids it saw once it
// has want of them, or fails if a wake-up that should have come never did.
func followTable(t *testing.T, rec *EventRecorder, st *store.Store, user string, want int) []int64 {
	t.Helper()
	var seen []int64
	cursor := int64(0)
	deadline := time.After(20 * time.Second)
	for len(seen) < want {
		changed := rec.Changed()
		for _, e := range readAfter(t, st, user, cursor) {
			seen = append(seen, e.ID)
			cursor = e.ID
		}
		if len(seen) >= want {
			break
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("reader saw %d of %d events and was never woken for the rest", len(seen), want)
		}
	}
	return seen
}

// countEvents is how many events the table holds for user, without reading them.
func countEvents(t *testing.T, st *store.Store, user string) int {
	t.Helper()
	n, err := st.CountEvents(t.Context(), store.EventFilter{UserID: user})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func waitWoken(t *testing.T, changed <-chan struct{}) {
	t.Helper()
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("readers were not woken after events were recorded")
	}
}

// The wake-up comes after the commit, never before: a reader woken early would
// find nothing new and go back to sleep, and the rows landing a moment later
// would wake nobody. The batch is big enough that its commit takes a while, so a
// wake-up sent ahead of the commit is caught reading an empty table.
func TestWakeComesOnlyAfterEventsAreReadable(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	rec, st := newTestRecorder(t)

	pad := strings.Repeat("x", 64<<10)
	for round := range 3 {
		before := countEvents(t, st, "usr_a")
		changed := rec.Changed()
		for range 100 { // a full batch flushes at once
			rec.Record(store.Event{Type: "wake.test", UserID: "usr_a", Meta: map[string]any{"pad": pad}})
		}
		waitWoken(t, changed)
		if now := countEvents(t, st, "usr_a"); now <= before {
			t.Fatalf("round %d: woken, but the table holds %d events, no more than the %d before", round, now, before)
		}
		waitForCount(t, rec, st, "usr_a", before+100)
	}
}

// tail takes the wake-up channel before it reads, and that order is the whole
// guarantee: a reader that took it after its read would lose a commit landing
// between the two, and sleep on rows it has not seen. Here the table is a
// counter and every commit is waited for, so a lost wake-up shows as a commit the
// reader never gets to see, within a few thousand rounds at most.
func TestTailNeverSleepsThroughACommit(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	rec, _ := newTestRecorder(t)

	var committed, seen atomic.Int64
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	done := make(chan error, 1)
	go func() {
		done <- rec.tail(ctx, func() error {
			seen.Store(committed.Load())
			return nil
		})
	}()

	const commits = 20000
	for i := int64(1); i <= commits; i++ {
		committed.Store(i)
		rec.committed()
		deadline := time.Now().Add(5 * time.Second)
		for seen.Load() < i {
			if time.Now().After(deadline) {
				t.Fatalf("commit %d never reached the reader: it slept through a flush", i)
			}
			runtime.Gosched()
		}
	}
	stop()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("tail returned %v after its context ended, want context.Canceled", err)
	}
}

// Producers write while a reader follows: the reader ends up with every stored
// event exactly once and in id order.
func TestReaderSeesEveryEventOnceInOrderWhileProducersRun(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	rec, st := newTestRecorder(t)

	const producers, perProducer = 4, 200 // under the buffer, so none are dropped
	var wg sync.WaitGroup
	for range producers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perProducer {
				user := "usr_a"
				if i%4 == 3 {
					user = "usr_b" // someone else's, which the reader must not see
				}
				rec.Record(store.Event{Type: "race.test", UserID: user})
				if i%10 == 0 {
					time.Sleep(time.Millisecond) // spread the events across several flushes
				}
			}
		}()
	}
	want := producers * perProducer * 3 / 4
	seen := followTable(t, rec, st, "usr_a", want)
	wg.Wait()

	if len(seen) != want {
		t.Fatalf("saw %d events, want %d", len(seen), want)
	}
	if rec.Dropped.Load() != 0 {
		t.Fatalf("%d events dropped, so the count above proves nothing", rec.Dropped.Load())
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] <= seen[i-1] {
			t.Fatalf("ids not strictly increasing at %d: %v", i, seen[i-1:i+1])
		}
	}
}

// A reader that is not reading costs the recorder nothing and loses nothing:
// there is no per-reader buffer to overflow, so nobody is disconnected for being
// slow. Hundreds of events arrive while it is away, and it gets all of them in
// order when it comes back, as does a reader that only starts afterwards.
func TestSlowReaderLosesNothingAndBlocksNobody(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	rec, st := newTestRecorder(t)

	const total = 300
	for i := range total {
		rec.Record(store.Event{Type: "burst", UserID: "usr_a", SandboxID: fmt.Sprintf("sb%d", i)})
	}
	waitForCount(t, rec, st, "usr_a", total)
	if rec.Dropped.Load() != 0 {
		t.Fatalf("%d events dropped", rec.Dropped.Load())
	}

	for _, reader := range []string{"the reader that was away", "a reader that arrives later"} {
		events := readAfter(t, st, "usr_a", 0)
		if len(events) != total {
			t.Fatalf("%s read %d events, want %d", reader, len(events), total)
		}
		for i, e := range events {
			if e.SandboxID != fmt.Sprintf("sb%d", i) {
				t.Fatalf("%s: event %d is %q, events are out of order", reader, i, e.SandboxID)
			}
		}
	}
}

// Close stores what is still buffered, and the readers waiting for it are woken
// by that last flush; after Close a Record is ignored rather than stored or
// panicking.
func TestCloseStoresBufferedEventsAndWakesReaders(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	st, err := store.New(t.Context(), filepath.Join(t.TempDir(), "ev.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rec := NewEventRecorder(st)

	changed := rec.Changed()
	rec.Record(store.Event{Type: "last.words", UserID: "usr_a"})
	rec.Close() // well inside the 500ms flush interval
	waitWoken(t, changed)

	if got := readAfter(t, st, "usr_a", 0); len(got) != 1 || got[0].Type != "last.words" {
		t.Fatalf("after Close the table holds %+v, want the buffered event", got)
	}
	rec.Record(store.Event{Type: "too.late", UserID: "usr_a"})
	rec.Close() // idempotent
	if got := readAfter(t, st, "usr_a", 0); len(got) != 1 {
		t.Fatalf("an event recorded after Close was stored: %+v", got)
	}
}

// eventWatch reads the events table from a cursor, for a test that waits on
// something the server records. It starts at the newest event, so it sees what
// is recorded after it is made.
//
// It reads with QueryEvents, not EventsAfter: tests also check who an event is
// attributed to, which a read scoped to one user could not show.
type eventWatch struct {
	t      *testing.T
	srv    *Server
	filter store.EventFilter
	cursor int64
}

func watchEvents(t *testing.T, srv *Server, f store.EventFilter) *eventWatch {
	t.Helper()
	_, newest, err := srv.store.EventIDBounds(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	f.Limit = 500
	return &eventWatch{t: t, srv: srv, filter: f, cursor: newest}
}

// wait returns the next event past the cursor that match accepts, consuming the
// ones it skips. It reports false if none is stored within timeout; from a
// goroutine other than the test's, that is the only way to fail.
func (w *eventWatch) wait(match func(store.Event) bool, timeout time.Duration) (store.Event, bool) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		changed := w.srv.events.Changed() // before the read, as every reader does
		events, err := w.srv.store.QueryEvents(w.t.Context(), w.filter)
		if err != nil {
			return store.Event{}, false
		}
		sort.Slice(events, func(i, j int) bool { return events[i].ID < events[j].ID })
		for _, e := range events {
			if e.ID <= w.cursor {
				continue
			}
			w.cursor = e.ID
			if match(e) {
				return e, true
			}
		}
		select {
		case <-changed:
		case <-deadline.C:
			return store.Event{}, false
		}
	}
}

// The recorder stores events up to 500ms after they are recorded, so the wait
// is generous: a pass returns as soon as the event is there.
const eventWait = 5 * time.Second

func (w *eventWatch) next(kind string) store.Event {
	w.t.Helper()
	e, ok := w.wait(func(e store.Event) bool { return e.Type == kind }, eventWait)
	if !ok {
		w.t.Fatalf("timed out waiting for %s", kind)
	}
	return e
}

func (w *eventWatch) nextWithPrefix(prefix string) store.Event {
	w.t.Helper()
	e, ok := w.wait(func(e store.Event) bool { return strings.HasPrefix(e.Type, prefix) }, eventWait)
	if !ok {
		w.t.Fatalf("timed out waiting for an event of type %s*", prefix)
	}
	return e
}
