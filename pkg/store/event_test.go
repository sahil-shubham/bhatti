package store

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestInsertAndQueryEvents(t *testing.T) {
	st, err := New(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Insert a batch of events
	events := []Event{
		{Type: "sandbox.created", UserID: "usr_alice", SandboxID: "sb_1",
			Meta: map[string]any{"name": "dev", "cpus": 2}},
		{Type: "sandbox.destroyed", UserID: "usr_alice", SandboxID: "sb_1",
			Meta: map[string]any{"name": "dev", "lifetime_s": 3600}},
		{Type: "thermal.pause", SandboxID: "sb_2",
			Meta: map[string]any{"sandbox": "api-server", "idle_s": 30}},
		{Type: "thermal.wake", SandboxID: "sb_2",
			Meta: map[string]any{"sandbox": "api-server", "from_state": "warm", "wake_ms": 52}},
		{Type: "auth.failed",
			Meta: map[string]any{"ip": "1.2.3.4"}},
	}
	if err := st.InsertEvents(t.Context(), events); err != nil {
		t.Fatal(err)
	}

	// Query all
	all, err := st.QueryEvents(t.Context(), EventFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 5 {
		t.Fatalf("expected 5 events, got %d", len(all))
	}

	// Query by exact type
	sandboxCreated, err := st.QueryEvents(t.Context(), EventFilter{Type: "sandbox.created"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sandboxCreated) != 1 {
		t.Fatalf("expected 1 sandbox.created, got %d", len(sandboxCreated))
	}
	if sandboxCreated[0].Meta["name"] != "dev" {
		t.Errorf("expected name=dev, got %v", sandboxCreated[0].Meta["name"])
	}

	// Query by prefix type (thermal matches thermal.*)
	thermal, err := st.QueryEvents(t.Context(), EventFilter{Type: "thermal"})
	if err != nil {
		t.Fatal(err)
	}
	if len(thermal) != 2 {
		t.Fatalf("expected 2 thermal events, got %d", len(thermal))
	}

	// Query by user
	alice, err := st.QueryEvents(t.Context(), EventFilter{UserID: "usr_alice"})
	if err != nil {
		t.Fatal(err)
	}
	if len(alice) != 2 {
		t.Fatalf("expected 2 alice events, got %d", len(alice))
	}

	// Query by sandbox_id
	sb2, err := st.QueryEvents(t.Context(), EventFilter{SandboxID: "sb_2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sb2) != 2 {
		t.Fatalf("expected 2 sb_2 events, got %d", len(sb2))
	}

	// Count
	count, err := st.CountEvents(t.Context(), EventFilter{Type: "sandbox"})
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("expected count=2, got %d", count)
	}
}

func TestMetricsSnapshots(t *testing.T) {
	st, err := New(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Insert snapshots
	for i := 0; i < 3; i++ {
		snap := MetricsSnapshot{
			APIRequests:    int64(10 + i),
			APIErrors:      int64(i),
			SandboxesTotal: 5,
			SandboxesHot:   2,
			SandboxesWarm:  1,
			SandboxesCold:  2,
			HostLoad1m:     0.5,
		}
		if err := st.InsertMetricsSnapshot(t.Context(), snap); err != nil {
			t.Fatal(err)
		}
	}

	// Query all
	snaps, err := st.QueryMetricsSnapshots(t.Context(), time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 3 {
		t.Fatalf("expected 3 snapshots, got %d", len(snaps))
	}

	// Sum
	sums, err := st.SumMetricsSnapshots(t.Context(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	// 10 + 11 + 12 = 33
	if sums.APIRequests != 33 {
		t.Errorf("expected sum api_requests=33, got %d", sums.APIRequests)
	}

	// Latest
	latest, err := st.LatestMetricsSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if latest.APIRequests != 12 {
		t.Errorf("expected latest api_requests=12, got %d", latest.APIRequests)
	}
}

func TestRetention(t *testing.T) {
	st, err := New(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Insert events
	if err := st.InsertEvents(t.Context(), []Event{
		{Type: "sandbox.created", Meta: map[string]any{"name": "test"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertMetricsSnapshot(t.Context(), MetricsSnapshot{APIRequests: 1}); err != nil {
		t.Fatal(err)
	}

	// Small sleep so the cutoff timestamp is strictly after the inserted rows
	time.Sleep(10 * time.Millisecond)

	// Purge with 0 retention (deletes everything)
	n, err := st.PurgeOldEvents(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected 1 event purged, got %d", n)
	}

	n, err = st.PurgeOldMetricsSnapshots(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected 1 snapshot purged, got %d", n)
	}

	// Verify empty
	events, _ := st.QueryEvents(t.Context(), EventFilter{Limit: 100})
	if len(events) != 0 {
		t.Errorf("expected 0 events after purge, got %d", len(events))
	}
}

// ids lists the ids of events, in the order given.
func ids(events []Event) []int64 {
	out := make([]int64, len(events))
	for i, e := range events {
		out[i] = e.ID
	}
	return out
}

func mustInsert(t *testing.T, st *Store, events ...Event) {
	t.Helper()
	if err := st.InsertEvents(t.Context(), events); err != nil {
		t.Fatal(err)
	}
}

func mustAfter(t *testing.T, st *Store, q EventQuery) []Event {
	t.Helper()
	if q.Limit == 0 {
		q.Limit = 1000
	}
	events, err := st.EventsAfter(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

// A reader that keeps the last id it handled gets exactly the events after it,
// once each and in id order, whoever else is writing in between.
func TestEventsAfterIsExactlyThePastTheCursorInIDOrder(t *testing.T) {
	st := testStore(t)
	for i := range 6 {
		mustInsert(t, st,
			Event{Type: "sandbox.created", UserID: "usr_a", SandboxID: "sb1", Meta: map[string]any{"i": i}},
			Event{Type: "sandbox.created", UserID: "usr_b", SandboxID: "sb2"},
		)
	}

	all := mustAfter(t, st, EventQuery{UserID: "usr_a"})
	if len(all) != 6 {
		t.Fatalf("usr_a has 6 events, got %d", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].ID <= all[i-1].ID {
			t.Fatalf("not in id order: %v", ids(all))
		}
	}
	for i, e := range all {
		if got := e.Meta["i"]; got != float64(i) {
			t.Errorf("event %d carries meta i=%v: rows came back out of insert order", i, got)
		}
	}

	for cut := 0; cut <= len(all); cut++ {
		after := int64(0)
		if cut > 0 {
			after = all[cut-1].ID
		}
		got := mustAfter(t, st, EventQuery{UserID: "usr_a", After: after})
		if !reflect.DeepEqual(ids(got), ids(all[cut:])) {
			t.Errorf("after event %d: got ids %v, want %v", cut, ids(got), ids(all[cut:]))
		}
	}
	if got := mustAfter(t, st, EventQuery{UserID: "usr_a", After: all[len(all)-1].ID + 1000}); len(got) != 0 {
		t.Errorf("a cursor past the newest event returned %d events", len(got))
	}
}

// Paging with the last id of each page reproduces the whole stream: nothing is
// skipped at a page boundary and nothing is repeated.
func TestEventsAfterPagesCompose(t *testing.T) {
	st := testStore(t)
	var batch []Event
	for range 25 {
		batch = append(batch, Event{Type: "t", UserID: "usr_a"}, Event{Type: "t", UserID: "usr_b"})
	}
	mustInsert(t, st, batch...)

	want := ids(mustAfter(t, st, EventQuery{UserID: "usr_a"}))
	var got []int64
	cursor := int64(0)
	for {
		page := mustAfter(t, st, EventQuery{UserID: "usr_a", After: cursor, Limit: 7})
		if len(page) == 0 {
			break
		}
		if len(page) > 7 {
			t.Fatalf("page of %d with limit 7", len(page))
		}
		got = append(got, ids(page)...)
		cursor = page[len(page)-1].ID
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pages gave %v, the full read gave %v", got, want)
	}
}

// Events belong to the user who caused them. A read for one user never returns
// another's, and an event with no user (a failed login, the daemon starting)
// is nobody's to read; asking for no user must fail, not return those.
func TestEventsAfterIsScopedToTheUser(t *testing.T) {
	st := testStore(t)
	mustInsert(t, st,
		Event{Type: "sandbox.created", UserID: "usr_a", SandboxID: "sb1"},
		Event{Type: "sandbox.created", UserID: "usr_b", SandboxID: "sb1"}, // same sandbox id on purpose
		Event{Type: "auth.failed", Meta: map[string]any{"ip": "203.0.113.9"}},
		Event{Type: "thermal.pause", SandboxID: "sb1"},
	)
	for _, user := range []string{"usr_a", "usr_b"} {
		got := mustAfter(t, st, EventQuery{UserID: user})
		if len(got) != 1 || got[0].UserID != user {
			t.Fatalf("%s read %+v, want only its own created event", user, got)
		}
	}
	if got := mustAfter(t, st, EventQuery{UserID: "usr_nobody"}); len(got) != 0 {
		t.Fatalf("a user with no events read %+v", got)
	}
	if got, err := st.EventsAfter(t.Context(), EventQuery{Limit: 100}); err == nil {
		t.Fatalf("a read with no user succeeded and returned %+v", got)
	}
}

func TestEventsAfterFilters(t *testing.T) {
	st := testStore(t)
	mustInsert(t, st,
		Event{Type: "sandbox.created", UserID: "usr_a", SandboxID: "sb1"},
		Event{Type: "sandbox.restart_failed", UserID: "usr_a", SandboxID: "sb1"},
		Event{Type: "sandbox.restartXfailed", UserID: "usr_a", SandboxID: "sb2"},
		Event{Type: "sandbox.destroyed", UserID: "usr_a", SandboxID: "sb2"},
		Event{Type: "thermal.pause", UserID: "usr_a", SandboxID: "sb1"},
		Event{Type: "thermalx", UserID: "usr_a", SandboxID: "sb2"},
	)
	types := func(events []Event) []string {
		var out []string
		for _, e := range events {
			out = append(out, e.Type)
		}
		return out
	}
	for _, tc := range []struct {
		name string
		q    EventQuery
		want []string
	}{
		{"no filter", EventQuery{}, []string{"sandbox.created", "sandbox.restart_failed", "sandbox.restartXfailed", "sandbox.destroyed", "thermal.pause", "thermalx"}},
		{"family prefix", EventQuery{TypePrefix: "sandbox."}, []string{"sandbox.created", "sandbox.restart_failed", "sandbox.restartXfailed", "sandbox.destroyed"}},
		{"bare word is a plain prefix", EventQuery{TypePrefix: "thermal"}, []string{"thermal.pause", "thermalx"}},
		{"whole type", EventQuery{TypePrefix: "sandbox.destroyed"}, []string{"sandbox.destroyed"}},
		{"underscore is literal", EventQuery{TypePrefix: "sandbox.restart_"}, []string{"sandbox.restart_failed"}},
		{"percent is literal", EventQuery{TypePrefix: "sandbox.%"}, nil},
		{"prefix is case-sensitive", EventQuery{TypePrefix: "Sandbox."}, nil},
		{"longer than any type", EventQuery{TypePrefix: "sandbox.created.and.more"}, nil},
		{"sandbox", EventQuery{SandboxID: "sb2"}, []string{"sandbox.restartXfailed", "sandbox.destroyed", "thermalx"}},
		{"sandbox and type are both required", EventQuery{SandboxID: "sb2", TypePrefix: "sandbox."}, []string{"sandbox.restartXfailed", "sandbox.destroyed"}},
		{"unknown sandbox", EventQuery{SandboxID: "sb9"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.q.UserID = "usr_a"
			if got := types(mustAfter(t, st, tc.q)); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEventsAfterLimit(t *testing.T) {
	st := testStore(t)
	var batch []Event
	for range 10 {
		batch = append(batch, Event{Type: "t", UserID: "usr_a"})
	}
	mustInsert(t, st, batch...)
	all := mustAfter(t, st, EventQuery{UserID: "usr_a"})

	got := mustAfter(t, st, EventQuery{UserID: "usr_a", Limit: 3})
	if !reflect.DeepEqual(ids(got), ids(all[:3])) {
		t.Fatalf("limit 3 returned %v, want the first three of %v", ids(got), ids(all))
	}
	for _, limit := range []int{0, -1} {
		if _, err := st.EventsAfter(t.Context(), EventQuery{UserID: "usr_a", Limit: limit}); err == nil {
			t.Errorf("limit %d was accepted", limit)
		}
	}
}

// The gap record relies on this: the id of a deleted event is never given to a
// later one, so ids below the oldest stored one are known to be gone.
func TestEventIDsAreNotReusedAfterRetention(t *testing.T) {
	st := testStore(t)
	oldest, newest, err := st.EventIDBounds(t.Context())
	if err != nil || oldest != 0 || newest != 0 {
		t.Fatalf("empty table bounds = %d, %d, %v; want 0, 0", oldest, newest, err)
	}

	mustInsert(t, st, Event{Type: "t", UserID: "usr_a"}, Event{Type: "t", UserID: "usr_a"}, Event{Type: "t", UserID: "usr_a"})
	first := mustAfter(t, st, EventQuery{UserID: "usr_a"})
	oldest, newest, err = st.EventIDBounds(t.Context())
	if err != nil || oldest != first[0].ID || newest != first[2].ID {
		t.Fatalf("bounds = %d, %d, %v; want %d, %d", oldest, newest, err, first[0].ID, first[2].ID)
	}

	if n, err := st.PurgeOldEvents(t.Context(), -time.Hour); err != nil || n != 3 {
		t.Fatalf("purge removed %d, %v; want 3", n, err)
	}
	mustInsert(t, st, Event{Type: "t", UserID: "usr_a"})
	after := mustAfter(t, st, EventQuery{UserID: "usr_a"})
	if len(after) != 1 || after[0].ID <= first[2].ID {
		t.Fatalf("event stored after retention got id %v, but ids up to %d were already used", ids(after), first[2].ID)
	}
	oldest, newest, err = st.EventIDBounds(t.Context())
	if err != nil || oldest != after[0].ID || newest != after[0].ID {
		t.Fatalf("bounds after retention = %d, %d, %v; want both %d", oldest, newest, err, after[0].ID)
	}
}

// One damaged row must not stop a reader from getting the rows after it.
func TestEventsAfterReadsPastAnUnreadableMeta(t *testing.T) {
	st := testStore(t)
	mustInsert(t, st, Event{Type: "before", UserID: "usr_a", Meta: map[string]any{"k": "v"}})
	if _, err := st.db.ExecContext(t.Context(), `INSERT INTO events (type, user_id, meta) VALUES ('damaged', 'usr_a', 'not json')`); err != nil {
		t.Fatal(err)
	}
	mustInsert(t, st, Event{Type: "after", UserID: "usr_a"})

	got := mustAfter(t, st, EventQuery{UserID: "usr_a"})
	if len(got) != 3 || got[0].Type != "before" || got[1].Type != "damaged" || got[2].Type != "after" {
		t.Fatalf("read %+v, want before, damaged, after", got)
	}
	if got[0].Meta["k"] != "v" || len(got[1].Meta) != 0 {
		t.Errorf("meta = %v, %v; want the intact one kept and the damaged one empty", got[0].Meta, got[1].Meta)
	}
	if time.Since(got[0].Timestamp) > time.Minute || got[0].Timestamp.IsZero() {
		t.Errorf("timestamp %v was not read back", got[0].Timestamp)
	}
}
