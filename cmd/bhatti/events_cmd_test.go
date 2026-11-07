package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/store"
	"github.com/spf13/cobra"
)

const testEventTS = "2026-11-07T12:03:44.123Z"

type recordedEventsRequest struct {
	method, path, auth, accept string
	query                      url.Values
}

// eventsFake serves the /events contract with a caller-supplied body writer and
// records every request it receives.
type eventsFake struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []recordedEventsRequest
}

func newEventsFake(t *testing.T, h http.HandlerFunc) *eventsFake {
	t.Helper()
	f := &eventsFake{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.reqs = append(f.reqs, recordedEventsRequest{
			method: r.Method, path: r.URL.Path,
			auth: r.Header.Get("Authorization"), accept: r.Header.Get("Accept"),
			query: r.URL.Query(),
		})
		f.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *eventsFake) requests() []recordedEventsRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedEventsRequest(nil), f.reqs...)
}

// ndjsonBody replies 200 with the lines and ends the stream.
func ndjsonBody(lines ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		for _, l := range lines {
			fmt.Fprintln(w, l)
		}
	}
}

func eventJSON(id int, typ, sandboxID, meta string) string {
	s := fmt.Sprintf(`{"id":%d,"ts":%q,"type":%q,"user_id":"usr_x"`, id, testEventTS, typ)
	if sandboxID != "" {
		s += fmt.Sprintf(`,"sandbox_id":%q`, sandboxID)
	}
	if meta != "" {
		s += `,"meta":` + meta
	}
	return s + "}"
}

// noGoroutineLeak fails the test if goroutines outlive it. Call it first: its
// cleanup then runs after the fake server has been closed.
func noGoroutineLeak(t *testing.T) {
	t.Helper()
	before := runtime.NumGoroutine()
	t.Cleanup(func() {
		deadline := time.Now().Add(3 * time.Second)
		for {
			http.DefaultTransport.(*http.Transport).CloseIdleConnections()
			if runtime.NumGoroutine() <= before || time.Now().After(deadline) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if n := runtime.NumGoroutine(); n > before {
			buf := make([]byte, 1<<16)
			buf = buf[:runtime.Stack(buf, true)]
			t.Errorf("%d goroutines left behind (had %d):\n%s", n, before, buf)
		}
	})
}

func newEventsTestCmd() *cobra.Command {
	c := &cobra.Command{Use: "events"}
	c.Flags().Bool("json", false, "")
	addEventsFlags(c)
	return c
}

func TestEventsFlagsBecomeExactlyTheRightQuery(t *testing.T) {
	noGoroutineLeak(t)
	for _, tc := range []struct {
		name string
		args []string
		want url.Values
	}{
		{"nothing set sends no params", nil, url.Values{}},
		{"explicit after 0 is sent", []string{"--after", "0"}, url.Values{"after": {"0"}}},
		{"after with follow", []string{"--after", "42", "--follow"}, url.Values{"after": {"42"}, "follow": {"1"}}},
		{"follow alone leaves after unset", []string{"--follow"}, url.Values{"follow": {"1"}}},
		{"short follow", []string{"-f"}, url.Values{"follow": {"1"}}},
		{"type prefix", []string{"--type", "sandbox."}, url.Values{"type": {"sandbox."}}},
		{"sandbox name", []string{"--sandbox", "dev"}, url.Values{"sandbox": {"dev"}}},
		{"sandbox/name form is resolved", []string{"--sandbox", "sandbox/dev"}, url.Values{"sandbox": {"dev"}}},
		{"all together", []string{"--after", "7", "-f", "--type", "thermal", "--sandbox", "sandbox/a b"},
			url.Values{"after": {"7"}, "follow": {"1"}, "type": {"thermal"}, "sandbox": {"a b"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newEventsFake(t, ndjsonBody())
			useTestAPI(t, srv.URL)
			apiToken = "tok-123"

			cmd := newEventsTestCmd()
			if err := cmd.ParseFlags(tc.args); err != nil {
				t.Fatal(err)
			}
			opts, err := eventsOptionsFromFlags(cmd)
			if err != nil {
				t.Fatal(err)
			}
			var out, errOut bytes.Buffer
			if err := streamEvents(t.Context(), &out, &errOut, opts); err != nil && opts.follow == false {
				t.Fatalf("streamEvents: %v", err)
			}
			reqs := srv.requests()
			if len(reqs) != 1 {
				t.Fatalf("server saw %d requests, want 1", len(reqs))
			}
			r := reqs[0]
			if !reflect.DeepEqual(r.query, tc.want) {
				t.Errorf("query = %v, want %v", r.query, tc.want)
			}
			if r.method != http.MethodGet || r.path != "/events" {
				t.Errorf("request = %s %s, want GET /events", r.method, r.path)
			}
			if r.auth != "Bearer tok-123" {
				t.Errorf("Authorization = %q", r.auth)
			}
			if r.accept != "application/x-ndjson" {
				t.Errorf("Accept = %q", r.accept)
			}
		})
	}
}

func TestEventsNegativeAfterIsRefusedBeforeAnyRequest(t *testing.T) {
	noGoroutineLeak(t)
	srv := newEventsFake(t, ndjsonBody())
	useTestAPI(t, srv.URL)
	err := streamEvents(t.Context(), &bytes.Buffer{}, &bytes.Buffer{}, eventsOptions{after: -1, afterSet: true})
	if err == nil {
		t.Fatal("negative --after accepted")
	}
	if n := len(srv.requests()); n != 0 {
		t.Fatalf("server saw %d requests, want none", n)
	}
}

func TestEventsHumanOutputIsOneLinePerEventAndControlRecordsGoToStderr(t *testing.T) {
	noGoroutineLeak(t)
	srv := newEventsFake(t, ndjsonBody(
		`{"type":"events.gap","after":5,"oldest":9}`,
		eventJSON(10, "sandbox.created", "a1b2c3d4e5f60718", `{"name":"dev","cpus":1,"memory_mb":512,"image":"ubuntu"}`),
		eventJSON(11, "thermal.pause", "a1b2c3d4e5f60718", `{"idle_s":30}`),
		eventJSON(12, "daemon.started", "", ""),
		`{"type":"events.more","after":12}`,
	))
	useTestAPI(t, srv.URL)

	var out, errOut bytes.Buffer
	opts := eventsOptions{after: 5, afterSet: true, typePrefix: "thermal", sandbox: "dev"}
	if err := streamEvents(t.Context(), &out, &errOut, opts); err != nil {
		t.Fatalf("streamEvents: %v", err)
	}

	ts := time.Date(2026, 11, 7, 12, 3, 44, 0, time.UTC).Local().Format("2006-01-02 15:04:05")
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("stdout has %d lines, want one per event (3):\n%s", len(lines), out.String())
	}
	for i, want := range []struct {
		id, typ, label, details string
	}{
		{"10", "sandbox.created", "dev", "cpus=1"}, // meta name beats the sandbox id
		{"11", "thermal.pause", "a1b2c3d4e5f60718", "idle=30s"},
		{"12", "daemon.started", "-", ""},
	} {
		line := lines[i]
		if line != strings.TrimRight(line, " ") {
			t.Errorf("line %d has trailing spaces: %q", i, line)
		}
		f := strings.Fields(line)
		if len(f) < 5 || f[0] != want.id || f[1]+" "+f[2] != ts || f[3] != want.typ || f[4] != want.label {
			t.Errorf("line %d = %q, want id %s, time %s, type %s, sandbox %s", i, line, want.id, ts, want.typ, want.label)
		}
		if !strings.Contains(line, want.details) {
			t.Errorf("line %d = %q, missing details %q", i, line, want.details)
		}
	}
	if strings.Contains(out.String(), "events.") {
		t.Errorf("a control record reached stdout:\n%s", out.String())
	}

	notices := strings.Split(strings.TrimSuffix(errOut.String(), "\n"), "\n")
	if len(notices) != 2 {
		t.Fatalf("stderr = %q, want a gap line and a more line", errOut.String())
	}
	if !strings.Contains(notices[0], "#5") || !strings.Contains(notices[0], "#9") {
		t.Errorf("gap notice %q should name the cursor 5 and the oldest stored 9", notices[0])
	}
	for _, want := range []string{"--after 12", "--type thermal", "--sandbox dev"} {
		if !strings.Contains(notices[1], want) {
			t.Errorf("more notice %q missing %q", notices[1], want)
		}
	}
}

func TestEventsMoreHintOnlyEchoesFlagsTheUserSet(t *testing.T) {
	noGoroutineLeak(t)
	srv := newEventsFake(t, ndjsonBody(eventJSON(3, "sandbox.created", "x", `{"name":"dev"}`), `{"type":"events.more","after":3}`))
	useTestAPI(t, srv.URL)
	var out, errOut bytes.Buffer
	if err := streamEvents(t.Context(), &out, &errOut, eventsOptions{}); err != nil {
		t.Fatal(err)
	}
	hint := errOut.String()
	if !strings.Contains(hint, "--after 3") || strings.Contains(hint, "--type") || strings.Contains(hint, "--sandbox") {
		t.Errorf("hint = %q, want only the cursor", hint)
	}
}

func TestEventsEmptyListingSaysSoOnStderr(t *testing.T) {
	noGoroutineLeak(t)
	srv := newEventsFake(t, ndjsonBody())
	useTestAPI(t, srv.URL)

	var out, errOut bytes.Buffer
	if err := streamEvents(t.Context(), &out, &errOut, eventsOptions{}); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 || strings.TrimSpace(errOut.String()) != "No events." {
		t.Errorf("stdout=%q stderr=%q, want nothing / No events.", out.String(), errOut.String())
	}

	out.Reset()
	errOut.Reset()
	if err := streamEvents(t.Context(), &out, &errOut, eventsOptions{json: true}); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 || errOut.Len() != 0 {
		t.Errorf("--json empty listing wrote stdout=%q stderr=%q, want silence", out.String(), errOut.String())
	}
}

func TestEventsJSONPassesRecordsThroughUnchangedInOrder(t *testing.T) {
	noGoroutineLeak(t)
	// Odd spacing and key order: only a pass-through keeps these bytes.
	lines := []string{
		`{"type":"events.gap", "after":5,"oldest":9}`,
		`{"meta":{"b":1,  "a":2},"type":"sandbox.created","id":10,"ts":"` + testEventTS + `","sandbox_id":"abc"}`,
		`{"id":11,"ts":"2026-11-07T12:03:45Z","type":"future.kind","extra":[1, 2]}`,
		`{"type":"events.more","after":11}`,
	}
	srv := newEventsFake(t, ndjsonBody(lines...))
	useTestAPI(t, srv.URL)

	var out, errOut bytes.Buffer
	if err := streamEvents(t.Context(), &out, &errOut, eventsOptions{json: true, afterSet: true, after: 5}); err != nil {
		t.Fatal(err)
	}
	if want := strings.Join(lines, "\n") + "\n"; out.String() != want {
		t.Errorf("stdout =\n%s\nwant\n%s", out.String(), want)
	}
	if errOut.Len() != 0 {
		t.Errorf("--json wrote notices to stderr: %q", errOut.String())
	}
}

// A follow server sends event 1 and holds the stream; the CLI must show it
// before the server lets go, then show event 2 and report the ended stream.
func TestEventsFollowIsLiveAndAnEndedStreamNamesTheResumeCursor(t *testing.T) {
	noGoroutineLeak(t)
	release := make(chan struct{})
	srv := newEventsFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		fmt.Fprintln(w, eventJSON(1, "sandbox.created", "abc", `{"name":"dev"}`))
		fl.Flush()
		select {
		case <-release:
		case <-time.After(10 * time.Second):
			return
		}
		fmt.Fprintln(w, eventJSON(2, "sandbox.stopped", "abc", `{"name":"dev","reason":"manual"}`))
		fl.Flush()
	})
	useTestAPI(t, srv.URL)

	out := &notifyWriter{marker: "sandbox.created", seen: make(chan struct{})}
	var errOut bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- streamEvents(t.Context(), out, &errOut, eventsOptions{follow: true, typePrefix: "sandbox."})
	}()

	select {
	case <-out.seen:
	case <-time.After(5 * time.Second):
		close(release)
		<-done
		t.Fatal("event 1 not written while the stream was still open: output is buffered")
	}
	close(release)
	err := <-done

	if err == nil {
		t.Fatal("a followed stream that ended returned nil")
	}
	for _, want := range []string{"--follow", "--after 2", "--type sandbox."} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should contain %q", err, want)
		}
	}
	got := out.String()
	if strings.Count(got, "\n") != 2 || !strings.Contains(got, "sandbox.created") || !strings.Contains(got, "sandbox.stopped") {
		t.Errorf("stdout = %q, want both events", got)
	}
}

func TestEventsFollowEndedBeforeAnyEventResumesFromTheUsersCursor(t *testing.T) {
	noGoroutineLeak(t)
	srv := newEventsFake(t, ndjsonBody())
	useTestAPI(t, srv.URL)

	var errOut bytes.Buffer
	err := streamEvents(t.Context(), &bytes.Buffer{}, &errOut, eventsOptions{follow: true, afterSet: true, after: 7})
	if err == nil || !strings.Contains(err.Error(), "--after 7") {
		t.Errorf("err = %v, want the --after value to resume from", err)
	}
	err = streamEvents(t.Context(), &bytes.Buffer{}, &errOut, eventsOptions{follow: true})
	if err == nil || strings.Contains(err.Error(), "--after") {
		t.Errorf("err = %v, want an error with no cursor to resume from", err)
	}
	if errOut.Len() != 0 {
		t.Errorf("stderr = %q; a followed stream is not 'No events'", errOut.String())
	}
}

func TestEventsConnectionDroppedMidStream(t *testing.T) {
	noGoroutineLeak(t)
	drop := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(200)
		fmt.Fprintln(w, eventJSON(7, "sandbox.created", "abc", `{"name":"dev"}`))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}

	t.Run("follow resumes from the last id", func(t *testing.T) {
		srv := newEventsFake(t, drop)
		useTestAPI(t, srv.URL)
		var out bytes.Buffer
		err := streamEvents(t.Context(), &out, &bytes.Buffer{}, eventsOptions{follow: true})
		if err == nil || !strings.Contains(err.Error(), "--after 7") {
			t.Errorf("err = %v, want resume cursor 7", err)
		}
		if !strings.Contains(out.String(), "sandbox.created") {
			t.Errorf("stdout = %q, event 7 sent before the drop was lost", out.String())
		}
	})
	t.Run("a cut-off listing is an error, not a complete one", func(t *testing.T) {
		srv := newEventsFake(t, drop)
		useTestAPI(t, srv.URL)
		if err := streamEvents(t.Context(), &bytes.Buffer{}, &bytes.Buffer{}, eventsOptions{}); err == nil {
			t.Error("truncated non-follow stream returned nil")
		}
	})
}

func TestEventsServerErrorsSurfaceTheMessageAndPrintNothing(t *testing.T) {
	noGoroutineLeak(t)
	for _, tc := range []struct {
		status int
		body   string
		want   string
	}{
		{400, `{"error":"after must be a non-negative integer"}`, "after must be a non-negative integer"},
		{404, `{"error":"sandbox \"x\" not found"}`, `sandbox "x" not found`},
		{401, `{"error":"unauthorized"}`, "unauthorized"},
		{405, `{"error":"method not allowed"}`, "method not allowed"},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			srv := newEventsFake(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})
			useTestAPI(t, srv.URL)
			for _, follow := range []bool{false, true} {
				var out, errOut bytes.Buffer
				err := streamEvents(t.Context(), &out, &errOut, eventsOptions{follow: follow})
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("follow=%v: err = %v, want the server's message %q", follow, err, tc.want)
				}
				if out.Len() != 0 || errOut.Len() != 0 {
					t.Errorf("follow=%v: wrote stdout=%q stderr=%q, want nothing", follow, out.String(), errOut.String())
				}
			}
		})
	}
}

func TestEventsRetriesRateLimitBeforeTheStream(t *testing.T) {
	noGoroutineLeak(t)
	var mu sync.Mutex
	n := 0
	srv := newEventsFake(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		first := n == 1
		mu.Unlock()
		if first {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":"slow down"}`)
			return
		}
		ndjsonBody(eventJSON(4, "sandbox.created", "abc", `{"name":"dev"}`))(w, r)
	})
	slept := useTestAPI(t, srv.URL)

	var out bytes.Buffer
	if err := streamEvents(t.Context(), &out, &bytes.Buffer{}, eventsOptions{afterSet: true, after: 3}); err != nil {
		t.Fatalf("streamEvents: %v", err)
	}
	if len(*slept) != 1 || (*slept)[0] != time.Second {
		t.Errorf("slept %v, want one Retry-After wait", *slept)
	}
	reqs := srv.requests()
	if len(reqs) != 2 || reqs[1].query.Get("after") != "3" {
		t.Errorf("requests = %+v, want the same query sent twice", reqs)
	}
	if !strings.Contains(out.String(), "sandbox.created") {
		t.Errorf("stdout = %q, the retried response's event is missing", out.String())
	}
}

func TestEventDetailsFallbackIsDeterministic(t *testing.T) {
	e := store.Event{Type: "custom.thing", Meta: map[string]any{}}
	for _, k := range []string{"zeta", "alpha", "mid", "beta", "omega", "gamma", "delta", "kappa", "eta", "iota"} {
		e.Meta[k] = k
	}
	seen := map[string]bool{}
	for range 50 {
		seen[formatEventDetails(e)] = true
	}
	if len(seen) != 1 {
		t.Fatalf("formatEventDetails gave %d distinct strings for one event: %v", len(seen), seen)
	}
	for got := range seen {
		if want := "alpha=alpha beta=beta delta=delta"; got != want {
			t.Errorf("details = %q, want the first three keys in sorted order %q", got, want)
		}
	}
}

// `bhatti events` and `bhatti admin events` must label and describe an event
// identically, so both go through eventSandboxLabel and formatEventDetails.
func TestEventLineUsesTheSharedLabelAndDetailHelpers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		e     store.Event
		label string
	}{
		{"meta name beats meta sandbox and id", store.Event{SandboxID: "id1", Meta: map[string]any{"name": "dev", "sandbox": "other"}}, "dev"},
		{"meta sandbox beats id", store.Event{SandboxID: "id1", Meta: map[string]any{"sandbox": "other"}}, "other"},
		{"id when meta has no name", store.Event{SandboxID: "id1", Meta: map[string]any{"cpus": 1}}, "id1"},
		{"dash when there is nothing", store.Event{}, "-"},
		{"empty name falls through", store.Event{SandboxID: "id1", Meta: map[string]any{"name": ""}}, "id1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.e.ID, tc.e.Type, tc.e.Timestamp = 9, "thermal.pause", time.Now()
			if got := eventSandboxLabel(tc.e); got != tc.label {
				t.Errorf("eventSandboxLabel = %q, want %q", got, tc.label)
			}
			f := strings.Fields(eventLine(tc.e))
			if len(f) < 5 || f[4] != tc.label {
				t.Errorf("eventLine = %q, want sandbox column %q", eventLine(tc.e), tc.label)
			}
			if want := formatEventDetails(tc.e); want != "" && !strings.HasSuffix(strings.TrimSuffix(eventLine(tc.e), "\n"), want) {
				t.Errorf("eventLine = %q does not end with the details %q", eventLine(tc.e), want)
			}
		})
	}
}
