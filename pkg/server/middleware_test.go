package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sahil-shubham/bhatti/pkg"
	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// logCapture collects what the server logs while a test runs, as the JSON
// records a log aggregator would see.
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func captureLogs(t *testing.T) *logCapture {
	t.Helper()
	c := &logCapture{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(c, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return c
}

// records returns the logged records whose message is msg, oldest first.
func (c *logCapture) records(msg string) []map[string]any {
	c.mu.Lock()
	data := bytes.Clone(c.buf.Bytes())
	c.mu.Unlock()
	var out []map[string]any
	for _, line := range bytes.Split(data, []byte("\n")) {
		var rec map[string]any
		if json.Unmarshal(line, &rec) == nil && rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

// requestLines returns the `request` records of one request, by its ID.
func (c *logCapture) requestLines(id string) []map[string]any {
	var out []map[string]any
	for _, rec := range c.records("request") {
		if rec["request_id"] == id {
			out = append(out, rec)
		}
	}
	return out
}

// waitFor polls until a record of msg satisfies match, since a line is written
// after the response that a client already has.
func (c *logCapture) waitFor(t *testing.T, msg string, match func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, rec := range c.records(msg) {
			if match(rec) {
				return rec
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %q record matched; have %v", msg, c.records(msg))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

var requestIDRe = regexp.MustCompile(`^req_[0-9a-f]{16}$`)

func TestEveryAPIResponseCarriesARequestID(t *testing.T) {
	srv, _ := setup(t)
	// One read per user: the second request below is refused with a 429.
	srv.limiter = newRateLimiter(pkg.RateLimits{Read: pkg.RateBucket{PerMinute: 1, Burst: 1}})

	seen := map[string]bool{}
	for _, tc := range []struct {
		name, key, method, path string
		want                    int
	}{
		{"ok", testAPIKey, "GET", "/sandboxes", 200},
		{"rate limited", testAPIKey, "GET", "/sandboxes", 429},
		{"unauthenticated", "", "GET", "/sandboxes", 401},
		{"bad key", "wrong", "GET", "/sandboxes", 401},
		{"public", "", "GET", "/health", 200},
	} {
		rec := serveAs(srv, tc.key, tc.method, tc.path)
		if rec.Code != tc.want {
			t.Fatalf("%s: %s %s = %d, want %d", tc.name, tc.method, tc.path, rec.Code, tc.want)
		}
		id := rec.Header().Get("X-Request-ID")
		if !requestIDRe.MatchString(id) {
			t.Errorf("%s: X-Request-ID = %q, want req_<16 hex>", tc.name, id)
		}
		if seen[id] {
			t.Errorf("%s: request ID %q was already used", tc.name, id)
		}
		seen[id] = true
		if rec.Header().Get("X-Bhatti-Version") == "" {
			t.Errorf("%s: no X-Bhatti-Version, which CLIs read off any response", tc.name)
		}
	}
}

// A caller can't choose the ID its request is logged under.
func TestRequestIDIsNotTakenFromTheRequest(t *testing.T) {
	srv, _ := setup(t)
	req := httptest.NewRequest("GET", "/health", nil)
	req.Header.Set("X-Request-ID", "forged")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Request-ID"); got == "forged" || !requestIDRe.MatchString(got) {
		t.Fatalf("X-Request-ID = %q", got)
	}
}

// panicEngine panics when asked for a sandbox's status.
type panicEngine struct{ *mockEngine }

func (e *panicEngine) Status(context.Context, string) (engine.SandboxInfo, error) {
	panic("status exploded")
}

func TestPanicIsAnswered500WithTheRequestID(t *testing.T) {
	srv, ts := setup(t)
	sb := createSandbox(t, ts, uniqueName(t, "panic"))
	srv.engine = &panicEngine{mockEngine: srv.engine.(*mockEngine)}
	logs := captureLogs(t)

	resp := doReq(t, ts, "GET", "/sandboxes/"+sb.ID, nil)
	var body map[string]string
	decodeJSON(t, resp, &body)
	id := resp.Header.Get("X-Request-ID")
	if resp.StatusCode != 500 || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("panicking handler = %d (%s), want a JSON 500", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if body["error"] != "internal error" || body["request_id"] != id || !requestIDRe.MatchString(id) || len(body) != 2 {
		t.Fatalf("body = %v with X-Request-ID %q, want {error: internal error, request_id: <that ID>}", body, id)
	}
	if resp.Header.Get("X-Bhatti-Version") == "" {
		t.Error("the 500 lost X-Bhatti-Version")
	}

	panicLog := logs.waitFor(t, "request.panic", func(rec map[string]any) bool { return rec["request_id"] == id })
	if panicLog["panic"] != "status exploded" || panicLog["route"] != "GET /sandboxes/{id}" {
		t.Errorf("panic record = %v", panicLog)
	}
	if stack, _ := panicLog["stack"].(string); !strings.Contains(stack, "panicEngine") || !strings.Contains(stack, "handleSandboxGet") {
		t.Errorf("panic record has no stack through the handler:\n%v", panicLog["stack"])
	}
	line := logs.waitFor(t, "request", func(rec map[string]any) bool { return rec["request_id"] == id })
	if line["status"] != float64(500) {
		t.Errorf("request line says status %v for a request answered 500", line["status"])
	}

	// The 5xx is counted under the route, and the server carries on.
	exp := parseMetrics(t, srv.MetricsText())
	if got := mustDuration(t, exp, durationCount, "route", "GET /sandboxes/{id}", "status", "5xx"); got != 1 {
		t.Errorf("GET /sandboxes/{id} 5xx count = %v, want 1", got)
	}
	if srv.requestErrors.Load() != 1 {
		t.Errorf("requestErrors = %d, want 1", srv.requestErrors.Load())
	}
	resp = doReq(t, ts, "GET", "/sandboxes", nil)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("after a panic, GET /sandboxes = %d", resp.StatusCode)
	}
}

// chainServer serves h behind the request ID and recovery layers only.
func chainServer(t *testing.T, srv *Server, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(chain(h, srv.requestID, srv.recoverPanics))
	t.Cleanup(ts.Close)
	return ts
}

// A response that has begun can't be turned into a 500, and ending it quietly
// would present a truncated body as a whole one: the client must see the
// connection fail, as it would for any handler that dies mid-response.
func TestPanicAfterTheResponseBeganAbortsTheConnection(t *testing.T) {
	srv, _ := setup(t)

	for _, tc := range []struct {
		name      string
		value     any
		wantPanic bool // logged as request.panic
	}{
		{"panic", "died halfway", true},
		{"http.ErrAbortHandler", http.ErrAbortHandler, false}, // what a reverse proxy does to cut a response it can't finish
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			ts := chainServer(t, srv, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(200)
				w.Write([]byte("partial"))
				w.(http.Flusher).Flush()
				panic(tc.value)
			})
			resp, err := testHTTPClient.Get(ts.URL)
			if err != nil {
				t.Fatalf("the headers were flushed, so the client should get them: %v", err)
			}
			defer resp.Body.Close()
			got, err := io.ReadAll(resp.Body)
			if err == nil {
				t.Fatalf("body = %q read cleanly; a truncated response must look truncated", got)
			}
			if tc.wantPanic {
				logs.waitFor(t, "request.panic", func(rec map[string]any) bool { return rec["panic"] == tc.value })
			} else if n := len(logs.records("request.panic")); n != 0 {
				t.Errorf("an abort was logged as a panic: %v", logs.records("request.panic"))
			}
		})
	}

	t.Run("abort before any response", func(t *testing.T) {
		logs := captureLogs(t)
		ts := chainServer(t, srv, func(w http.ResponseWriter, r *http.Request) { panic(http.ErrAbortHandler) })
		if resp, err := testHTTPClient.Get(ts.URL); err == nil {
			resp.Body.Close()
			t.Fatalf("an aborted request got a %d response, want the connection dropped", resp.StatusCode)
		}
		if n := len(logs.records("request.panic")); n != 0 {
			t.Errorf("an abort was logged as a panic: %v", logs.records("request.panic"))
		}
	})
}

func TestEachRequestIsLoggedOnceWithItsRoute(t *testing.T) {
	srv, ts := setup(t)
	sb := createSandbox(t, ts, uniqueName(t, "logged"))
	logs := captureLogs(t)

	for _, tc := range []struct {
		method, path, route string
		want                int
	}{
		{"GET", "/sandboxes", "GET /sandboxes", 200},
		{"GET", "/sandboxes/" + sb.ID, "GET /sandboxes/{id}", 200},
		{"GET", "/sandboxes/" + sb.Name + "/ports", "GET /sandboxes/{id}/ports", 200},
		{"GET", "/sandboxes/no-such-one", "GET /sandboxes/{id}", 404},
		{"GET", "/nowhere", "unmatched", 404},
		{"DELETE", "/sandboxes/" + sb.ID + "/exec", "unmatched", 405},
	} {
		resp := doReq(t, ts, tc.method, tc.path, nil)
		resp.Body.Close()
		id := resp.Header.Get("X-Request-ID")
		if resp.StatusCode != tc.want {
			t.Fatalf("%s %s = %d, want %d", tc.method, tc.path, resp.StatusCode, tc.want)
		}
		logs.waitFor(t, "request", func(rec map[string]any) bool { return rec["request_id"] == id })
		lines := logs.requestLines(id)
		if len(lines) != 1 {
			t.Fatalf("%s %s: %d request lines, want exactly 1: %v", tc.method, tc.path, len(lines), lines)
		}
		line := lines[0]
		if line["method"] != tc.method || line["path"] != tc.path || line["route"] != tc.route ||
			line["status"] != float64(tc.want) || line["user"] != "test-user" || line["user_id"] != "usr_test" {
			t.Errorf("%s %s: request line = %v, want route %q status %d", tc.method, tc.path, line, tc.route, tc.want)
		}
		if d, ok := line["duration_ms"].(float64); !ok || d < 0 {
			t.Errorf("%s %s: duration_ms = %v", tc.method, tc.path, line["duration_ms"])
		}
	}

	// A request that auth refuses never reaches the handler: no request line.
	rec := serveAs(srv, "", "GET", "/sandboxes")
	if rec.Code != 401 {
		t.Fatalf("unauthenticated = %d", rec.Code)
	}
	if lines := logs.requestLines(rec.Header().Get("X-Request-ID")); len(lines) != 0 {
		t.Errorf("a 401 was logged as a handled request: %v", lines)
	}
}

// A refusal by the rate limit is logged with the bucket and when to retry, as it
// was before the limiter became a middleware, and doesn't count as a handled
// request.
func TestRateLimitRefusalKeepsItsLineAndCounters(t *testing.T) {
	srv, _ := setup(t)
	srv.limiter = newRateLimiter(pkg.RateLimits{Read: pkg.RateBucket{PerMinute: 1, Burst: 1}})
	logs := captureLogs(t)

	if rec := serveAs(srv, testAPIKey, "GET", "/sandboxes"); rec.Code != 200 {
		t.Fatalf("first request = %d", rec.Code)
	}
	rec := serveAs(srv, testAPIKey, "GET", "/sandboxes")
	if rec.Code != 429 {
		t.Fatalf("second request = %d, want 429", rec.Code)
	}
	if body := jsonBody(t, rec); body["code"] != codeRateLimited || !strings.Contains(body["error"], "rate_limits.read") || rec.Header().Get("Retry-After") == "" {
		t.Errorf("429 body %v, Retry-After %q", body, rec.Header().Get("Retry-After"))
	}

	refused := logs.waitFor(t, "request", func(rec map[string]any) bool { return rec["status"] == float64(429) })
	for _, field := range []string{"method", "path", "user", "user_id", "rate_limit_bucket", "retry_after_s"} {
		if refused[field] == nil {
			t.Errorf("the 429 line lacks %q: %v", field, refused)
		}
	}
	if refused["rate_limit_bucket"] != "read" || refused["path"] != "/sandboxes" || refused["user"] != "test-user" {
		t.Errorf("429 line = %v", refused)
	}
	if got := srv.rateLimited.Load(); got != 1 {
		t.Errorf("rateLimited = %d, want 1", got)
	}
	if got := srv.requestTotal.Load(); got != 1 {
		t.Errorf("requestTotal = %d, want only the request that was handled", got)
	}
}

// Auth comes before the limiter: requests without a valid key neither spend a
// bucket nor are told they are rate limited.
func TestUnauthenticatedRequestsSpendNoBudget(t *testing.T) {
	srv, _ := setup(t)
	srv.limiter = newRateLimiter(pkg.RateLimits{Read: pkg.RateBucket{PerMinute: 1, Burst: 1}})
	for range 5 {
		if rec := serveAs(srv, "", "GET", "/sandboxes"); rec.Code != 401 {
			t.Fatalf("unauthenticated = %d, want 401 every time", rec.Code)
		}
	}
	if rec := serveAs(srv, testAPIKey, "GET", "/sandboxes"); rec.Code != 200 {
		t.Fatalf("authenticated request after the flood = %d, want its untouched budget", rec.Code)
	}
}

// What a handler gets as its ResponseWriter keeps the optional interfaces
// streaming and websocket upgrades rely on, and lets the server's own deadlines
// reach the connection.
func TestResponseWriterKeepsFlusherAndHijacker(t *testing.T) {
	srv, _ := setup(t)
	var checked bool
	h := chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checked = true
		if _, ok := w.(http.Flusher); !ok {
			t.Error("the handler's ResponseWriter is not an http.Flusher")
		}
		if _, ok := w.(http.Hijacker); !ok {
			t.Error("the handler's ResponseWriter is not an http.Hijacker")
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("ResponseController.Flush: %v", err)
		}
	}), srv.requestID, srv.recoverPanics, srv.logRequests)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if !checked {
		t.Fatal("handler did not run")
	}
}

func TestStatusWriterRecordsWhatTheClientGot(t *testing.T) {
	t.Run("first status wins and later ones are ignored", func(t *testing.T) {
		sw := trackWriter(httptest.NewRecorder())
		if sw.status != 200 || sw.wroteHeader {
			t.Fatalf("fresh writer: status %d, written %v", sw.status, sw.wroteHeader)
		}
		sw.WriteHeader(404)
		sw.WriteHeader(500)
		if sw.status != 404 || !sw.wroteHeader {
			t.Fatalf("status %d, written %v; want 404 written", sw.status, sw.wroteHeader)
		}
	})
	t.Run("interim 1xx is not the answer", func(t *testing.T) {
		sw := trackWriter(httptest.NewRecorder())
		sw.WriteHeader(http.StatusEarlyHints)
		if sw.wroteHeader {
			t.Fatal("a 103 ended the response")
		}
		sw.WriteHeader(201)
		if sw.status != 201 {
			t.Fatalf("status %d, want 201", sw.status)
		}
	})
	t.Run("a body byte is an implicit 200", func(t *testing.T) {
		sw := trackWriter(httptest.NewRecorder())
		sw.Write([]byte("x"))
		if sw.status != 200 || !sw.wroteHeader {
			t.Fatalf("status %d, written %v", sw.status, sw.wroteHeader)
		}
	})
	t.Run("layers share one record", func(t *testing.T) {
		sw := trackWriter(httptest.NewRecorder())
		if trackWriter(sw) != sw {
			t.Fatal("trackWriter wrapped a statusWriter again")
		}
	})
	t.Run("hijacking a connection that can't be hijacked fails cleanly", func(t *testing.T) {
		sw := trackWriter(httptest.NewRecorder())
		if _, _, err := sw.Hijack(); err == nil || sw.wroteHeader {
			t.Fatalf("Hijack on a recorder: err %v, written %v", err, sw.wroteHeader)
		}
	})
}

// A websocket is hijacked, so the request is over as far as HTTP goes, but it
// is logged when the connection ends, as a 101 that lasted the whole session.
func TestUpgradedConnectionIsLoggedWhenItEnds(t *testing.T) {
	srv, ts := setup(t)
	sb := createSandbox(t, ts, uniqueName(t, "upgraded"))
	logs := captureLogs(t)
	const route = "GET /sandboxes/{id}/ws"

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/sandboxes/" + sb.ID + "/ws"
	ws, resp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Authorization": {"Bearer " + testAPIKey}})
	if err != nil {
		t.Fatalf("dial through the chain: %v", err)
	}
	defer ws.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade = %d, want 101", resp.StatusCode)
	}
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, msg, err := ws.ReadMessage(); err != nil || !strings.Contains(string(msg), "$") {
		t.Fatalf("no terminal output through the hijacked connection: %q %v", msg, err)
	}

	time.Sleep(100 * time.Millisecond)
	for _, rec := range logs.records("request") {
		if rec["route"] == route {
			t.Fatalf("the session was logged while it was still open: %v", rec)
		}
	}
	if got := srv.requestTotal.Load(); got != 1 { // only the create, which is over
		t.Fatalf("requestTotal = %d while the session is open, want 1", got)
	}

	ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	ws.Close()
	line := logs.waitFor(t, "request", func(rec map[string]any) bool { return rec["route"] == route })
	if line["status"] != float64(http.StatusSwitchingProtocols) || line["user"] != "test-user" {
		t.Errorf("request line = %v, want status 101 for test-user", line)
	}
	if d, _ := line["duration_ms"].(float64); d < 100 {
		t.Errorf("duration_ms = %v, want the session's life (at least 100ms)", line["duration_ms"])
	}
	exp := parseMetrics(t, srv.MetricsText())
	if got := mustDuration(t, exp, durationCount, "route", route, "status", "1xx"); got != 1 {
		t.Errorf("%s 1xx count = %v, want 1", route, got)
	}
}

// What a published app answers is the app's response: it gets none of the API's
// headers, while the API host's responses do.
func TestProxyZoneResponsesAreNotStampedAsAPI(t *testing.T) {
	_, ts := setupDomainMode(t)

	resp := doReqWithHost(t, ts, "GET", "ghost.test.sh", "/", false)
	resp.Body.Close()
	for _, h := range []string{"X-Request-ID", "X-Bhatti-Version"} {
		if v := resp.Header.Get(h); v != "" {
			t.Errorf("a proxy zone response carries %s: %q", h, v)
		}
	}

	resp = doReqWithHost(t, ts, "GET", "api.test.sh", "/sandboxes", false)
	resp.Body.Close()
	if resp.StatusCode != 401 || !requestIDRe.MatchString(resp.Header.Get("X-Request-ID")) || resp.Header.Get("X-Bhatti-Version") == "" {
		t.Errorf("API host without a key: %d, X-Request-ID %q, X-Bhatti-Version %q",
			resp.StatusCode, resp.Header.Get("X-Request-ID"), resp.Header.Get("X-Bhatti-Version"))
	}
}
