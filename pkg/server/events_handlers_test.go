package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/store"
)

// These tests drive GET /events over HTTP with the real server, a real store
// and (for streams) the real recorder. Events that a test only needs to exist
// are inserted straight into the table, which makes their ids and order exact;
// events a stream must be woken for go through the recorder.

const bobKey = "bob-key"

// eventsServer is a server with a second user, "bob", next to the default
// "usr_test". A recorder is started only when the test follows a stream: it is
// what wakes one.
func eventsServer(t *testing.T, recorder bool) (*Server, *httptest.Server) {
	t.Helper()
	srv, ts := setup(t)
	if recorder {
		srv.StartEventRecorder()
	}
	if err := srv.store.CreateUser(t.Context(), store.User{
		ID: "usr_bob", Name: "bob", APIKeyHash: sha256Hex(bobKey),
		MaxSandboxes: 5, MaxCPUsPerSandbox: 2, MaxMemoryMBPerSandbox: 1024,
		SubnetIndex: 2, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	return srv, ts
}

func seedEvents(t *testing.T, srv *Server, events ...store.Event) {
	t.Helper()
	if err := srv.store.InsertEvents(t.Context(), events); err != nil {
		t.Fatal(err)
	}
}

// eventsClient fails a request whose response headers do not arrive: a follow
// stream that held its headers back until the first event would hang a test
// forever otherwise. It does not limit how long a body streams.
var eventsClient = &http.Client{Transport: &http.Transport{DisableKeepAlives: true, ResponseHeaderTimeout: 5 * time.Second}}

func getEvents(t *testing.T, ts *httptest.Server, key, query string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/events"+query, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := eventsClient.Do(req)
	if err != nil {
		t.Fatalf("GET /events%s: %v", query, err)
	}
	return resp
}

// eventRecord is one NDJSON line, decoded without assuming whether it is an
// event or a control record.
type eventRecord = map[string]any

func recordID(r eventRecord) int64 { id, _ := r["id"].(float64); return int64(id) }

// recordsOf reads a response that ends, to its end.
func recordsOf(t *testing.T, resp *http.Response) []eventRecord {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var out []eventRecord
	dec := json.NewDecoder(resp.Body)
	for {
		var r eventRecord
		if err := dec.Decode(&r); err == io.EOF {
			return out
		} else if err != nil {
			t.Fatalf("response is not NDJSON: %v", err)
		}
		out = append(out, r)
	}
}

// onlyEvents drops the control records, which have no id.
func onlyEvents(records []eventRecord) []eventRecord {
	var out []eventRecord
	for _, r := range records {
		if _, ok := r["id"]; ok {
			out = append(out, r)
		}
	}
	return out
}

func idsOf(records []eventRecord) []int64 {
	out := []int64{}
	for _, r := range onlyEvents(records) {
		out = append(out, recordID(r))
	}
	return out
}

func typesOf(records []eventRecord) []string {
	out := []string{}
	for _, r := range records {
		out = append(out, fmt.Sprint(r["type"]))
	}
	return out
}

// eventStream reads a response that stays open. Reading starts at the first
// call, so a test can hold a connection open without reading it.
type eventStream struct {
	t       *testing.T
	resp    *http.Response
	once    sync.Once
	records chan eventRecord
}

func openFollow(t *testing.T, ts *httptest.Server, key, query string) *eventStream {
	t.Helper()
	resp := getEvents(t, ts, key, query)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("GET /events%s = %d: %s", query, resp.StatusCode, body)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return &eventStream{t: t, resp: resp, records: make(chan eventRecord, 8192)}
}

func (s *eventStream) start() {
	s.once.Do(func() {
		go func() {
			defer close(s.records)
			dec := json.NewDecoder(s.resp.Body)
			for {
				var r eventRecord
				if dec.Decode(&r) != nil {
					return
				}
				s.records <- r
			}
		}()
	})
}

func (s *eventStream) next() eventRecord {
	s.t.Helper()
	s.start()
	select {
	case r, ok := <-s.records:
		if !ok {
			s.t.Fatal("the stream ended")
		}
		return r
	case <-time.After(eventWait):
		s.t.Fatal("no record arrived")
		return nil
	}
}

func (s *eventStream) expectEnd() {
	s.t.Helper()
	s.start()
	select {
	case r, ok := <-s.records:
		if ok {
			s.t.Fatalf("expected the stream to end, got %v", r)
		}
	case <-time.After(eventWait):
		s.t.Fatal("the stream did not end")
	}
}

// A request that has nothing to follow returns the events after the cursor, in
// id order, exactly: the cursor event itself is not repeated, nothing after it
// is skipped, and the line carries the id a client resumes from.
func TestEventsReplayFromACursorIsExactlyTheEventsAfterIt(t *testing.T) {
	srv, ts := eventsServer(t, false)
	for i := range 8 {
		seedEvents(t, srv,
			store.Event{Type: "replay.test", UserID: "usr_test", SandboxID: "sb1", Meta: map[string]any{"i": i}},
			store.Event{Type: "replay.test", UserID: "usr_bob"},
			store.Event{Type: "daemon.started"},
		)
	}

	resp := doReq(t, ts, "GET", "/events", nil)
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("Content-Type = %q, want application/x-ndjson", ct)
	}
	all := recordsOf(t, resp)
	if len(all) != 8 || len(onlyEvents(all)) != 8 {
		t.Fatalf("got %d records (%d events), want exactly the caller's 8 events: %v", len(all), len(onlyEvents(all)), all)
	}
	ids := idsOf(all)
	for i, r := range all {
		if i > 0 && ids[i] <= ids[i-1] {
			t.Fatalf("not in id order: %v", ids)
		}
		if r["user_id"] != "usr_test" || r["type"] != "replay.test" || r["sandbox_id"] != "sb1" || r["ts"] == nil {
			t.Errorf("record %d = %v", i, r)
		}
		if meta, _ := r["meta"].(map[string]any); meta["i"] != float64(i) {
			t.Errorf("record %d meta = %v, want i=%d", i, r["meta"], i)
		}
	}

	for cut := 0; cut <= len(ids); cut++ {
		query := "?after=0"
		if cut > 0 {
			query = fmt.Sprintf("?after=%d", ids[cut-1])
		}
		got := idsOf(recordsOf(t, doReq(t, ts, "GET", "/events"+query, nil)))
		if !reflect.DeepEqual(got, ids[cut:]) {
			t.Errorf("%s: got ids %v, want %v", query, got, ids[cut:])
		}
	}
	if got := recordsOf(t, doReq(t, ts, "GET", fmt.Sprintf("/events?after=%d", ids[len(ids)-1]+1000), nil)); len(got) != 0 {
		t.Errorf("a cursor past the newest event returned %v", got)
	}
}

// Events belong to the user who caused them. A failed login, the daemon
// starting and a thermal transition (a sandbox, no owner) are nobody's, and
// another user's events are not the caller's, in a replay and in a live stream.
func TestEventsOnlyTheCallersOwnEventsAreVisible(t *testing.T) {
	srv, ts := eventsServer(t, true)
	seedEvents(t, srv,
		store.Event{Type: "old.bob", UserID: "usr_bob", SandboxID: "sb_b"},
		store.Event{Type: "old.test", UserID: "usr_test", SandboxID: "sb_t"},
		store.Event{Type: "auth.failed", Meta: map[string]any{"ip": "203.0.113.9"}},
		store.Event{Type: "thermal.pause", SandboxID: "sb_t"}, // names the caller's sandbox, but records no owner
	)
	for key, want := range map[string]string{testAPIKey: "old.test", bobKey: "old.bob"} {
		got := typesOf(recordsOf(t, getEvents(t, ts, key, "?after=0")))
		if !reflect.DeepEqual(got, []string{want}) {
			t.Errorf("replay for %s = %v, want [%s]", key, got, want)
		}
	}

	mine := openFollow(t, ts, testAPIKey, "?follow=1")
	bobs := openFollow(t, ts, bobKey, "?follow=1")
	for _, e := range []store.Event{
		{Type: "live.bob1", UserID: "usr_bob"},
		{Type: "live.system"},
		{Type: "live.thermal", SandboxID: "sb_t"},
		{Type: "live.test1", UserID: "usr_test"},
		{Type: "live.bob2", UserID: "usr_bob"},
	} {
		srv.RecordEvent(e)
	}
	// Delivery is in id order, so a stream that was going to leak an event would
	// deliver it before the caller's own.
	if got := mine.next()["type"]; got != "live.test1" {
		t.Fatalf("first live event for the caller = %v, want live.test1", got)
	}
	if got := bobs.next()["type"]; got != "live.bob1" {
		t.Fatalf("first live event for bob = %v, want live.bob1", got)
	}
	if got := bobs.next()["type"]; got != "live.bob2" {
		t.Fatalf("second live event for bob = %v, want live.bob2 (nothing of the caller's or the system's in between)", got)
	}
	srv.RecordEvent(store.Event{Type: "live.test2", UserID: "usr_test"})
	if got := mine.next()["type"]; got != "live.test2" {
		t.Fatalf("second live event for the caller = %v, want live.test2 (nothing of bob's in between)", got)
	}
}

func TestEventsTypeFilter(t *testing.T) {
	srv, ts := eventsServer(t, true)
	seedEvents(t, srv,
		store.Event{Type: "sandbox.created", UserID: "usr_test"},
		store.Event{Type: "sandbox.destroyed", UserID: "usr_test"},
		store.Event{Type: "volume.created", UserID: "usr_test"},
		store.Event{Type: "sandbox.restart_failed", UserID: "usr_test"},
	)
	for _, tc := range []struct {
		prefix string
		want   []string
	}{
		{"sandbox.", []string{"sandbox.created", "sandbox.destroyed", "sandbox.restart_failed"}},
		{"sandbox", []string{"sandbox.created", "sandbox.destroyed", "sandbox.restart_failed"}},
		{"sandbox.destroyed", []string{"sandbox.destroyed"}},
		{"sandbox.restart_", []string{"sandbox.restart_failed"}},
		{"volume", []string{"volume.created"}},
		{"nothing.like.this", []string{}},
	} {
		got := typesOf(recordsOf(t, doReq(t, ts, "GET", "/events?type="+tc.prefix, nil)))
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("type=%s: got %v, want %v", tc.prefix, got, tc.want)
		}
	}

	stream := openFollow(t, ts, testAPIKey, "?follow=1&type=live.keep")
	srv.RecordEvent(store.Event{Type: "live.skip", UserID: "usr_test"})
	srv.RecordEvent(store.Event{Type: "live.keep.one", UserID: "usr_test"})
	if got := stream.next()["type"]; got != "live.keep.one" {
		t.Fatalf("a followed type=live.keep delivered %v first", got)
	}
}

// The sandbox filter takes an ID or a name. Events carry the ID, so a name is
// resolved through the caller's own sandboxes; the events of a sandbox that is
// gone are still there for its ID.
func TestEventsSandboxFilter(t *testing.T) {
	srv, ts := eventsServer(t, false)
	dev := createSandbox(t, ts, "events-dev")
	other := createSandbox(t, ts, "events-other")
	gone := createSandbox(t, ts, "events-gone")
	seedEvents(t, srv,
		store.Event{Type: "filter.test", UserID: "usr_test", SandboxID: dev.ID},
		store.Event{Type: "filter.test", UserID: "usr_test", SandboxID: other.ID},
		store.Event{Type: "filter.test", UserID: "usr_test", SandboxID: dev.ID, Meta: map[string]any{"again": true}},
		store.Event{Type: "filter.test", UserID: "usr_test", SandboxID: gone.ID},
		store.Event{Type: "filter.test", UserID: "usr_test"},
	)
	sandboxesOf := func(query string) []string {
		var ids []string
		for _, r := range onlyEvents(recordsOf(t, doReq(t, ts, "GET", "/events?type=filter.test&"+query, nil))) {
			ids = append(ids, fmt.Sprint(r["sandbox_id"]))
		}
		return ids
	}

	want := []string{dev.ID, dev.ID}
	if got := sandboxesOf("sandbox=" + dev.ID); !reflect.DeepEqual(got, want) {
		t.Errorf("by id: %v, want %v", got, want)
	}
	if got := sandboxesOf("sandbox=events-dev"); !reflect.DeepEqual(got, want) {
		t.Errorf("by name: %v, want %v", got, want)
	}

	// Destroyed: the name no longer names anything, the ID still finds its events.
	expect(t, doReq(t, ts, "DELETE", "/sandboxes/"+gone.ID, nil), 200, nil)
	resp := doReq(t, ts, "GET", "/events?sandbox=events-gone", nil)
	if got := errorBody(t, resp); resp.StatusCode != http.StatusNotFound || !strings.Contains(got, "events-gone") {
		t.Errorf("a destroyed sandbox's name: %d %q, want 404 naming it", resp.StatusCode, got)
	}
	if got := sandboxesOf("sandbox=" + gone.ID); !reflect.DeepEqual(got, []string{gone.ID}) {
		t.Errorf("a destroyed sandbox's id: %v, want its one event", got)
	}

	// A typo is an error, not an empty stream; an ID that names nothing is just quiet.
	resp = doReq(t, ts, "GET", "/events?sandbox=events-dve", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown name = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
	if got := sandboxesOf("sandbox=0123456789abcdef"); len(got) != 0 {
		t.Errorf("an unknown id returned %v", got)
	}

	// Another user's sandbox is not resolvable by name, and asking for its ID
	// returns nothing of it: the owner filter applies before the sandbox one.
	resp = getEvents(t, ts, bobKey, "?sandbox=events-dev")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("bob resolved alice's sandbox name: %d", resp.StatusCode)
	}
	resp.Body.Close()
	if got := onlyEvents(recordsOf(t, getEvents(t, ts, bobKey, "?sandbox="+dev.ID))); len(got) != 0 {
		t.Errorf("bob read alice's sandbox's events: %v", got)
	}
}

// When the cursor is older than what retention kept, the response says so
// before any event, so a client knows its stream is not continuous there.
func TestEventsGapWhenTheCursorPredatesRetention(t *testing.T) {
	srv, ts := eventsServer(t, true)
	seedEvents(t, srv, store.Event{Type: "pruned.one", UserID: "usr_test"}, store.Event{Type: "pruned.two", UserID: "usr_test"}, store.Event{Type: "pruned.three", UserID: "usr_test"})
	before := idsOf(recordsOf(t, doReq(t, ts, "GET", "/events", nil)))
	if n, err := srv.store.PurgeOldEvents(t.Context(), -time.Hour); err != nil || n != 3 {
		t.Fatalf("purge removed %d, %v; want 3", n, err)
	}
	seedEvents(t, srv, store.Event{Type: "kept.one", UserID: "usr_test"}, store.Event{Type: "kept.two", UserID: "usr_test"})
	kept := idsOf(recordsOf(t, doReq(t, ts, "GET", "/events", nil)))
	oldest := kept[0]

	got := recordsOf(t, doReq(t, ts, "GET", fmt.Sprintf("/events?after=%d", before[0]), nil))
	if len(got) != 3 || got[0]["type"] != "events.gap" || recordID(got[0]) != 0 {
		t.Fatalf("got %v, want a gap record then the two kept events", got)
	}
	if got[0]["after"] != float64(before[0]) || got[0]["oldest"] != float64(oldest) {
		t.Errorf("gap = %v, want after=%d oldest=%d", got[0], before[0], oldest)
	}
	if !reflect.DeepEqual(idsOf(got), kept) {
		t.Errorf("events after the gap = %v, want %v", idsOf(got), kept)
	}

	// The type filter is about events; the gap is about the stream.
	got = recordsOf(t, doReq(t, ts, "GET", fmt.Sprintf("/events?after=%d&type=nothing", before[0]), nil))
	if !reflect.DeepEqual(typesOf(got), []string{"events.gap"}) {
		t.Errorf("filtered response = %v, want the gap alone", got)
	}

	// No gap when nothing between the cursor and the oldest event is missing,
	// or when the client claimed no position.
	for _, query := range []string{
		fmt.Sprintf("?after=%d", oldest-1), // the next event is the oldest: nothing was lost
		fmt.Sprintf("?after=%d", oldest),
		"?after=0", // asks for everything retained
		"",
	} {
		for _, r := range recordsOf(t, doReq(t, ts, "GET", "/events"+query, nil)) {
			if r["type"] == "events.gap" {
				t.Errorf("GET /events%s reported a gap: %v", query, r)
			}
		}
	}

	// A follow stream says it too, before the live part starts.
	stream := openFollow(t, ts, testAPIKey, fmt.Sprintf("?after=%d&follow=1", before[0]))
	if first := stream.next(); first["type"] != "events.gap" || first["oldest"] != float64(oldest) {
		t.Errorf("first record of a followed stale cursor = %v, want the gap", first)
	}
}

// A response without follow is one page. When more matched, the last line says
// where the next page starts; when exactly a page matched, it does not.
func TestEventsPageLimitAndContinuation(t *testing.T) {
	srv, ts := eventsServer(t, false)
	batch := make([]store.Event, 0, eventsPageLimit+5)
	for i := range eventsPageLimit + 5 {
		batch = append(batch, store.Event{Type: "page.test", UserID: "usr_test", Meta: map[string]any{"i": i}})
	}
	seedEvents(t, srv, batch...)

	first := recordsOf(t, doReq(t, ts, "GET", "/events", nil))
	if len(first) != eventsPageLimit+1 {
		t.Fatalf("first page has %d records, want %d events and the continuation", len(first), eventsPageLimit)
	}
	more := first[len(first)-1]
	last := first[len(first)-2]
	if more["type"] != "events.more" || recordID(more) != 0 || more["after"] != last["id"] {
		t.Fatalf("last record = %v, want events.more pointing after %v", more, last["id"])
	}
	if meta, _ := last["meta"].(map[string]any); meta["i"] != float64(eventsPageLimit-1) {
		t.Errorf("the page's last event is #%v, want #%d", meta["i"], eventsPageLimit-1)
	}

	second := recordsOf(t, doReq(t, ts, "GET", fmt.Sprintf("/events?after=%d", int64(more["after"].(float64))), nil))
	if len(second) != 5 {
		t.Fatalf("second page has %d records, want the last 5 events and no continuation: %v", len(second), typesOf(second))
	}
	if meta, _ := second[0]["meta"].(map[string]any); meta["i"] != float64(eventsPageLimit) {
		t.Errorf("second page starts at #%v, want #%d: a page boundary skipped or repeated an event", meta["i"], eventsPageLimit)
	}

	// The page counts what matches, and a page that is exactly full is not "more".
	seedEvents(t, srv, store.Event{Type: "other.test", UserID: "usr_test"})
	exactly := recordsOf(t, doReq(t, ts, "GET", fmt.Sprintf("/events?after=%d&type=page.test", idsOf(first)[4]), nil))
	if len(exactly) != eventsPageLimit {
		t.Fatalf("got %d records, want exactly the %d page.test events after the 5th", len(exactly), eventsPageLimit)
	}
	for _, r := range exactly {
		if r["type"] == "events.more" {
			t.Fatalf("a response that held everything that matched ended with %v", r)
		}
	}
}

// A follow stream pages through a backlog bigger than one page on its own, with
// no continuation lines, and then carries on live.
func TestEventsFollowCatchesUpAcrossPagesThenGoesLive(t *testing.T) {
	srv, ts := eventsServer(t, true)
	const backlog = 2*eventsPageLimit + 50
	batch := make([]store.Event, 0, backlog)
	for i := range backlog {
		batch = append(batch, store.Event{Type: "backlog.test", UserID: "usr_test", Meta: map[string]any{"i": i}})
	}
	seedEvents(t, srv, batch...)

	stream := openFollow(t, ts, testAPIKey, "?after=0&follow=1")
	var last int64
	for i := range backlog {
		r := stream.next()
		if r["type"] != "backlog.test" {
			t.Fatalf("record %d = %v, want backlog event %d", i, r, i)
		}
		if meta, _ := r["meta"].(map[string]any); meta["i"] != float64(i) {
			t.Fatalf("record %d carries #%v: an event was skipped or repeated at a page boundary", i, meta["i"])
		}
		last = recordID(r)
	}
	srv.RecordEvent(store.Event{Type: "live.after.backlog", UserID: "usr_test"})
	live := stream.next()
	if live["type"] != "live.after.backlog" || recordID(live) <= last {
		t.Fatalf("after the backlog the stream delivered %v, want the live event with a later id", live)
	}
}

// A follow stream with no cursor starts at the live edge: what is already
// stored is not replayed, what is stored later arrives, in order. The client has
// its response headers before any event exists (openFollow would fail otherwise).
func TestEventsFollowDeliversEventsRecordedAfterTheStreamOpened(t *testing.T) {
	srv, ts := eventsServer(t, true)
	seedEvents(t, srv, store.Event{Type: "before.open", UserID: "usr_test"})

	stream := openFollow(t, ts, testAPIKey, "?follow=1")
	for _, kind := range []string{"after.open.1", "after.open.2", "after.open.3"} {
		srv.RecordEvent(store.Event{Type: kind, UserID: "usr_test"})
	}
	var ids []int64
	for _, kind := range []string{"after.open.1", "after.open.2", "after.open.3"} {
		r := stream.next()
		if r["type"] != kind {
			t.Fatalf("got %v, want %s (the event stored before the stream opened is not replayed)", r, kind)
		}
		ids = append(ids, recordID(r))
	}
	if !(ids[0] < ids[1] && ids[1] < ids[2]) {
		t.Fatalf("live ids not increasing: %v", ids)
	}

	// Later still, a lone event is delivered as soon as it is stored, not at the
	// next one: the stream waits, it does not poll for a batch.
	srv.RecordEvent(store.Event{Type: "after.quiet", UserID: "usr_test"})
	if r := stream.next(); r["type"] != "after.quiet" {
		t.Fatalf("got %v after a quiet spell, want after.quiet", r)
	}
}

func TestEventsFollowFromACursorReplaysThenGoesLive(t *testing.T) {
	srv, ts := eventsServer(t, true)
	seedEvents(t, srv,
		store.Event{Type: "cursor.1", UserID: "usr_test"},
		store.Event{Type: "cursor.2", UserID: "usr_test"},
		store.Event{Type: "cursor.3", UserID: "usr_test"},
	)
	ids := idsOf(recordsOf(t, doReq(t, ts, "GET", "/events", nil)))

	stream := openFollow(t, ts, testAPIKey, fmt.Sprintf("?after=%d&follow=true", ids[0]))
	srv.RecordEvent(store.Event{Type: "cursor.live", UserID: "usr_test"})
	for _, want := range []string{"cursor.2", "cursor.3", "cursor.live"} {
		if got := stream.next()["type"]; got != want {
			t.Fatalf("got %v, want %s", got, want)
		}
	}
}

// A client that stops reading is not disconnected and loses nothing: the
// recorder has no per-client buffer to overflow, and the rest of what was stored
// is read from the table when the client reads again. Another client is
// unaffected meanwhile.
func TestEventsSlowFollowerIsNeverDisconnectedAndLosesNothing(t *testing.T) {
	srv, ts := eventsServer(t, true)
	slow := openFollow(t, ts, testAPIKey, "?follow=1")
	fast := openFollow(t, ts, testAPIKey, "?follow=1")

	// Enough bytes that the slow client's socket buffers fill and its handler
	// blocks in a write, where the old design would have dropped it.
	pad := strings.Repeat("x", 2048)
	const rounds, perRound = 10, 100
	var fastIDs []int64
	for round := range rounds {
		for i := range perRound {
			srv.RecordEvent(store.Event{Type: "flood.test", UserID: "usr_test", Meta: map[string]any{"n": round*perRound + i, "pad": pad}})
		}
		// Paced by the fast client, so the recorder's buffer never overflows.
		for range perRound {
			fastIDs = append(fastIDs, recordID(fast.next()))
		}
	}
	if srv.events.Dropped.Load() != 0 {
		t.Fatalf("%d events dropped, so what follows proves nothing", srv.events.Dropped.Load())
	}

	for i := range rounds * perRound {
		r := slow.next()
		if recordID(r) != fastIDs[i] {
			t.Fatalf("slow client's event %d has id %d, the fast client's has %d", i, recordID(r), fastIDs[i])
		}
		if meta, _ := r["meta"].(map[string]any); meta["n"] != float64(i) {
			t.Fatalf("slow client's event %d carries #%v", i, meta["n"])
		}
	}
}

// A follow stream ends with the server, and the body ends cleanly: a client
// reading it gets EOF, not an error.
func TestEventsFollowEndsWhenTheServerShutsDown(t *testing.T) {
	srv, ts := eventsServer(t, true)
	stream := openFollow(t, ts, testAPIKey, "?follow=1")
	srv.RecordEvent(store.Event{Type: "before.shutdown", UserID: "usr_test"})
	if got := stream.next()["type"]; got != "before.shutdown" {
		t.Fatalf("got %v", got)
	}

	srv.Close()
	stream.expectEnd()
}

// The daemon ends streams as HTTP shutdown begins, before the server itself is
// closed: http.Server.Shutdown waits for requests to finish, and a follow stream
// would hold it for the whole drain timeout.
func TestEndEventStreamsEndsStreamsWhileTheServerRuns(t *testing.T) {
	srv, ts := eventsServer(t, true)
	stream := openFollow(t, ts, testAPIKey, "?follow=1")
	srv.EndEventStreams()
	stream.expectEnd()

	// A stream opened after that ends at once, and ordinary requests still work.
	late := openFollow(t, ts, testAPIKey, "?after=0&follow=1")
	late.expectEnd()
	recordsOf(t, doReq(t, ts, "GET", "/events", nil))
	resp := doReq(t, ts, "GET", "/sandboxes", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /sandboxes after EndEventStreams = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// A real http.Server drains as soon as streams are ended, instead of waiting
// out its timeout for a connection that never goes idle.
func TestHTTPShutdownDrainsOnceEventStreamsAreEnded(t *testing.T) {
	srv, ts := eventsServer(t, true)
	stream := openFollow(t, ts, testAPIKey, "?follow=1")
	stream.start()

	srv.EndEventStreams()
	ctx, cancel := context.WithTimeout(t.Context(), eventWait)
	defer cancel()
	if err := ts.Config.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v: an event stream held the drain", err)
	}
}

// A client that hangs up frees its handler without the server shutting down:
// the handler is waiting on a flush that may be far off, so it is the request's
// own context that has to wake it. Shutdown waits for every handler.
func TestEventsFollowEndsWhenTheClientDisconnects(t *testing.T) {
	_, ts := eventsServer(t, true)
	stream := openFollow(t, ts, testAPIKey, "?follow=1")
	stream.resp.Body.Close()

	ctx, cancel := context.WithTimeout(t.Context(), eventWait)
	defer cancel()
	if err := ts.Config.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v: the handler of a client that left is still running", err)
	}
}

func TestEventsBadRequests(t *testing.T) {
	_, ts := eventsServer(t, true)
	for _, query := range []string{
		"?after=abc", "?after=-1", "?after=", "?after=1.5", "?after=99999999999999999999",
		"?follow=maybe", "?follow=", "?after=1&follow=yes",
	} {
		resp := doReq(t, ts, "GET", "/events"+query, nil)
		got := errorBody(t, resp)
		if resp.StatusCode != http.StatusBadRequest || got == "" {
			t.Errorf("GET /events%s = %d %q, want 400 with a message", query, resp.StatusCode, got)
		}
	}

	resp := doReq(t, ts, "POST", "/events", map[string]any{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /events = %d, want 405", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/events", nil)
	resp, err := eventsClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /events without a key = %d, want 401", resp.StatusCode)
	}
}

// Without a recorder nothing would ever wake a follow stream, so it is refused
// rather than left to hang; reading what is stored needs no recorder.
func TestEventsFollowNeedsTheRecorder(t *testing.T) {
	srv, ts := eventsServer(t, false)
	seedEvents(t, srv, store.Event{Type: "stored", UserID: "usr_test"})

	resp := doReq(t, ts, "GET", "/events?follow=1", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("follow without a recorder = %d, want 503", resp.StatusCode)
	}
	if got := typesOf(recordsOf(t, doReq(t, ts, "GET", "/events", nil))); !reflect.DeepEqual(got, []string{"stored"}) {
		t.Errorf("replay without a recorder = %v", got)
	}
}

func TestEventsHEADReturnsAndAllowsConnectionReuse(t *testing.T) {
	for _, recorder := range []bool{false, true} {
		for _, query := range []string{"", "?follow=1", "?after=0&follow=1"} {
			t.Run(fmt.Sprintf("recorder=%t/%s", recorder, query), func(t *testing.T) {
				_, ts := eventsServer(t, recorder)
				conn, err := net.Dial("tcp", ts.Listener.Addr().String())
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(eventWait))
				// Pipeline a second request: net/http cannot serve it until the
				// HEAD handler returns, even though it suppresses its body.
				if _, err := fmt.Fprintf(conn, "HEAD /events%s HTTP/1.1\r\nHost: test\r\nAuthorization: Bearer %s\r\n\r\nGET /health HTTP/1.1\r\nHost: test\r\nConnection: close\r\n\r\n", query, testAPIKey); err != nil {
					t.Fatal(err)
				}
				reader := bufio.NewReader(conn)
				head, err := http.ReadResponse(reader, &http.Request{Method: http.MethodHead})
				if err != nil {
					t.Fatal(err)
				}
				head.Body.Close()
				if head.StatusCode != http.StatusOK || head.Header.Get("Content-Type") != "application/x-ndjson" || head.Header.Get("X-Content-Type-Options") != "nosniff" {
					t.Fatalf("HEAD /events%s = %d, headers %v", query, head.StatusCode, head.Header)
				}
				next, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
				if err != nil {
					t.Fatalf("request after HEAD /events%s: %v", query, err)
				}
				defer next.Body.Close()
				if next.StatusCode != http.StatusOK {
					t.Fatalf("request after HEAD = %d", next.StatusCode)
				}
			})
		}
	}
}
