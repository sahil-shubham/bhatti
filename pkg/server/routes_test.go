package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sahil-shubham/bhatti/pkg"
)

// matchedRequest builds a request and records in r.Pattern the route the
// server's mux picks for it, as the matchRoute middleware does before the
// limiter runs.
func matchedRequest(t *testing.T, srv *Server, method, target string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	_, r.Pattern = srv.mux.Handler(r)
	return r
}

var wildcardRe = regexp.MustCompile(`\{[^}]*\}`)

// samplePattern turns a route pattern into its method ("" when it answers every
// method) and a concrete path that it matches and that no more specific pattern
// takes over.
func samplePattern(pattern string) (method, path string) {
	method, path, found := strings.Cut(pattern, " ")
	if !found {
		method, path = "", pattern
	}
	path = wildcardRe.ReplaceAllStringFunc(path, func(wildcard string) string {
		switch {
		case wildcard == "{$}":
			return ""
		case wildcard == "{port}":
			return "8080"
		case strings.HasSuffix(wildcard, "...}"):
			return "8080/x"
		default:
			return "sample"
		}
	})
	return method, path
}

var allMethods = []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}

// generousLimits keeps a table test that fires a request per route from being
// answered 429 by the default budgets.
func generousLimits() *rateLimiter {
	big := pkg.RateBucket{PerMinute: 1e9, Burst: 1e9}
	return newRateLimiter(pkg.RateLimits{Create: big, Exec: big, Read: big})
}

// serveAs sends a request straight through the server, with the test key unless
// key is empty.
func serveAs(srv *Server, key, method, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func jsonBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not a JSON object: %q", rec.Body.String())
	}
	return body
}

// Every row of the route table is reachable by its own method. The sample
// request for it is dispatched to that very pattern, not to a catch-all or a
// sibling, and sent through the whole chain it is answered by that route:
// never the router's 405, and recorded under the pattern rather than as an
// unmatched request. This is what keeps a typo in a pattern, or a route
// shadowed by a broader one, from passing unnoticed.
func TestEveryRouteAnswersItsMethod(t *testing.T) {
	srv, _ := setup(t)
	srv.limiter = generousLimits()
	table := srv.routeTable()
	if len(table) < 60 {
		t.Fatalf("route table has %d rows; the API has more than that", len(table))
	}
	if len(srv.public) != len(table) {
		t.Fatalf("%d public flags for %d routes", len(srv.public), len(table))
	}

	for _, rt := range table {
		if srv.public[rt.pattern] != rt.public {
			t.Errorf("%s: public = %v in the table, %v as registered", rt.pattern, rt.public, srv.public[rt.pattern])
		}
		method, path := samplePattern(rt.pattern)
		methods := []string{method}
		if method == "" {
			methods = allMethods
		}
		for _, m := range methods {
			_, got := srv.mux.Handler(httptest.NewRequest(m, path, nil))
			if got != rt.pattern {
				t.Errorf("%s %s is dispatched to %q, want %q", m, path, got, rt.pattern)
			}
			if rt.public {
				continue // no user: neither logged nor counted
			}
			// Whatever the handler answers for a sandbox that doesn't exist,
			// the router has no 405 for a method its table lists.
			if rec := serveAs(srv, testAPIKey, m, path); rec.Code == http.StatusMethodNotAllowed {
				t.Errorf("%s %s answered 405 though %s lists it", m, path, rt.pattern)
			}
		}
	}

	exp := parseMetrics(t, srv.MetricsText())
	counted := map[string]bool{}
	for _, s := range exp.Family(durationFamily).Samples {
		counted[s.Labels["route"]] = true
	}
	for _, rt := range table {
		if !rt.public && !counted[rt.pattern] {
			t.Errorf("a request to %s was not recorded under that pattern", rt.pattern)
		}
	}
}

// A request with a method the path has no route for is answered 405 as JSON,
// with an Allow header naming the methods it does have, behind the same
// authentication as everything else.
func TestWrongMethodIsAJSON405WithAllow(t *testing.T) {
	srv, _ := setup(t)
	srv.limiter = generousLimits()

	for _, rt := range srv.routeTable() {
		method, path := samplePattern(rt.pattern)
		if method == "" {
			continue // answers every method
		}
		wrong := ""
		for _, m := range allMethods {
			if _, got := srv.mux.Handler(httptest.NewRequest(m, path, nil)); got == "" {
				wrong = m
				break
			}
		}
		if wrong == "" {
			t.Errorf("%s: every method is dispatched somewhere on %s, so nothing is left to be refused", rt.pattern, path)
			continue
		}

		rec := serveAs(srv, testAPIKey, wrong, path)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d %s, want 405", wrong, path, rec.Code, rec.Body)
			continue
		}
		allow := strings.Split(rec.Header().Get("Allow"), ", ")
		if !slices.Contains(allow, method) {
			t.Errorf("%s %s: Allow = %q, want it to include %s", wrong, path, rec.Header().Get("Allow"), method)
		}
		if method == http.MethodGet && !slices.Contains(allow, http.MethodHead) {
			t.Errorf("%s %s: Allow = %q, a GET route also answers HEAD", wrong, path, rec.Header().Get("Allow"))
		}
		if slices.Contains(allow, wrong) {
			t.Errorf("%s %s: Allow = %q lists the refused method", wrong, path, rec.Header().Get("Allow"))
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s %s: Content-Type = %q, want application/json", wrong, path, ct)
		}
		if body := jsonBody(t, rec); body["error"] != "method not allowed" || len(body) != 1 {
			t.Errorf("%s %s: body = %v, want the handlers' {\"error\":\"method not allowed\"}", wrong, path, body)
		}
	}
}

func TestUnknownPathIsAJSON404(t *testing.T) {
	srv, _ := setup(t)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/nowhere"},
		{"GET", "/sandboxes/abc/unknown"},
		{"GET", "/sandboxes/abc/"}, // a trailing slash is a different path
		{"GET", "/sandboxes/"},     // no id
		{"POST", "/images/pull/more"},
		{"GET", "/volumes/abc/unknown"},
		{"GET", "/templates/a/b"},
	} {
		rec := serveAs(srv, testAPIKey, tc.method, tc.path)
		if rec.Code != 404 || rec.Header().Get("Content-Type") != "application/json" || jsonBody(t, rec)["error"] != "not found" {
			t.Errorf("%s %s = %d %q (%s), want 404 {\"error\":\"not found\"}", tc.method, tc.path, rec.Code, rec.Body, rec.Header().Get("Content-Type"))
		}
	}
}

// The router answers behind authentication: a client without a key learns
// nothing about which paths and methods exist.
func TestRouterAnswersOnlyBehindAuth(t *testing.T) {
	srv, _ := setup(t)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/nowhere"},
		{"GET", "/sandboxes/abc/exec"}, // wrong method on a real route
		{"PUT", "/sandboxes/abc/exec"}, // another wrong method
		{"POST", "/_shell/../sandboxes"},
	} {
		rec := serveAs(srv, "", tc.method, tc.path)
		if rec.Code != 401 || jsonBody(t, rec)["error"] != "authorization required" {
			t.Errorf("%s %s without a key = %d %s, want 401", tc.method, tc.path, rec.Code, rec.Body)
		}
		rec = serveAs(srv, "wrong-key", tc.method, tc.path)
		if rec.Code != 401 || jsonBody(t, rec)["error"] != "invalid api key" {
			t.Errorf("%s %s with a bad key = %d %s, want 401", tc.method, tc.path, rec.Code, rec.Body)
		}
	}
}

// The public routes need no key, whatever the key says, and nothing else under
// /_shell is reachable by anyone.
func TestPublicRoutesNeedNoKey(t *testing.T) {
	srv, _ := setup(t)
	srv.limiter = newRateLimiter(pkg.RateLimits{Read: pkg.RateBucket{PerMinute: 1, Burst: 1}})

	for _, tc := range []struct {
		method, path string
		want         int
		contentType  string
	}{
		{"GET", "/health", 200, "application/json"},
		{"HEAD", "/health", 200, "application/json"},
		{"POST", "/health", 200, "application/json"}, // /health has always answered every method
		{"OPTIONS", "/health", 200, "application/json"},
		{"GET", "/_shell/abc", 200, "text/html; charset=utf-8"},
		{"GET", "/_shell/abc/", 200, "text/html; charset=utf-8"},
		{"POST", "/_shell/abc", 200, "text/html; charset=utf-8"},
		{"GET", "/_shell/", 404, "application/json"},
		{"GET", "/_shell", 404, "application/json"}, // not redirected to /_shell/
		{"GET", "/_shell/abc/other", 404, "application/json"},
		{"GET", "/_shell/abc/ws/", 404, "application/json"},
	} {
		// Five times over: a public route has no user, so no rate limit.
		for range 5 {
			rec := serveAs(srv, "", tc.method, tc.path)
			if rec.Code != tc.want || rec.Header().Get("Content-Type") != tc.contentType {
				t.Fatalf("%s %s = %d (%s), want %d (%s)", tc.method, tc.path, rec.Code, rec.Header().Get("Content-Type"), tc.want, tc.contentType)
			}
		}
	}
}

// A path that merely reaches a public route through "..", or an authenticated
// one, is cleaned by redirect after authentication. It never serves a route
// the caller couldn't have asked for directly.
func TestTraversalIsRedirectedBehindAuth(t *testing.T) {
	srv, _ := setup(t)

	rec := serveAs(srv, "", "GET", "/_shell/../sandboxes")
	if rec.Code != 401 {
		t.Fatalf("unauthenticated /_shell/../sandboxes = %d, want 401", rec.Code)
	}
	rec = serveAs(srv, testAPIKey, "GET", "/_shell/../sandboxes")
	if rec.Code/100 != 3 || rec.Header().Get("Location") != "/sandboxes" {
		t.Fatalf("authenticated /_shell/../sandboxes = %d to %q, want a redirect to /sandboxes", rec.Code, rec.Header().Get("Location"))
	}
	rec = serveAs(srv, "", "GET", "/_shell/../health")
	if rec.Code/100 != 3 || rec.Header().Get("Location") != "/health" {
		t.Fatalf("/_shell/../health = %d to %q, want a redirect to /health", rec.Code, rec.Header().Get("Location"))
	}
}

// Every route that names a sandbox takes its name or its ID, resolved through
// getUserSandbox, and so answers the same for both.
func TestSandboxRoutesTakeNameOrID(t *testing.T) {
	srv, ts := setup(t)
	srv.limiter = generousLimits()
	sb := createSandbox(t, ts, uniqueName(t, "either"))

	for _, tc := range []struct {
		method, suffix string
		want           int
	}{
		{"GET", "", 200},
		{"GET", "/ports", 200},
		{"GET", "/publish", 200},
		{"POST", "/exec", 200},
		{"POST", "/shell-token", 200},
		{"DELETE", "/shell-token", 204},
	} {
		for _, ref := range []string{sb.ID, sb.Name} {
			var body any
			if tc.method == "POST" && tc.suffix == "/exec" {
				body = map[string]any{"cmd": []string{"true"}}
			}
			resp := doReq(t, ts, tc.method, "/sandboxes/"+ref+tc.suffix, body)
			resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("%s /sandboxes/%s%s = %d, want %d", tc.method, ref, tc.suffix, resp.StatusCode, tc.want)
			}
		}
	}

	resp := doReq(t, ts, "GET", "/sandboxes/no-such-sandbox/ports", nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 404 || !strings.Contains(string(body), `sandbox \"no-such-sandbox\" not found`) {
		t.Fatalf("unknown sandbox = %d %s, want the 404 getUserSandbox gives", resp.StatusCode, body)
	}
}

// A rate class that names a route the table no longer has would quietly demote
// it to the read bucket.
func TestRateClassesNameRealRoutes(t *testing.T) {
	srv, _ := setup(t)
	for pattern, class := range rateClasses {
		if _, ok := srv.public[pattern]; !ok {
			t.Errorf("rateClasses names %q, which is not in the route table", pattern)
		}
		if class != bucketCreate && class != bucketExec {
			t.Errorf("rateClasses[%q] = %q, want create or exec (read is the default)", pattern, class)
		}
	}
}

// Sandboxes named like routes must not spend another route's budget: these
// names used to move a request between buckets because the class was read off
// substrings of the path.
func TestSandboxNamedLikeARouteSpendsTheReadBucket(t *testing.T) {
	srv, ts := setup(t)
	// One exec for the whole test. Reads and creates keep their defaults.
	srv.limiter = newRateLimiter(pkg.RateLimits{Exec: pkg.RateBucket{PerMinute: 1, Burst: 1}})

	ws := createSandbox(t, ts, "ws-1")
	ex := createSandbox(t, ts, "exec-x")

	for range 5 {
		for _, rq := range []struct{ method, path string }{
			{"GET", "/sandboxes/ws-1"},
			{"GET", "/sandboxes/ws-1/ports"},
			{"GET", "/sandboxes/exec-x"},
			{"POST", "/sandboxes/exec-x/stop"},
			{"POST", "/sandboxes/exec-x/start"},
		} {
			resp := doReq(t, ts, rq.method, rq.path, nil)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("%s %s = %d %s: spent the exec bucket", rq.method, rq.path, resp.StatusCode, body)
			}
		}
	}

	// The exec bucket was not touched: its one token is still there, then gone.
	exec := func(id string) int {
		resp := doReq(t, ts, "POST", "/sandboxes/"+id+"/exec", map[string]any{"cmd": []string{"true"}})
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := exec(ws.ID); got != 200 {
		t.Fatalf("first exec = %d, want 200 from an untouched bucket", got)
	}
	if got := exec(ex.ID); got != 429 {
		t.Fatalf("second exec = %d, want 429 from the spent exec bucket", got)
	}
}

// What the middleware knows about a route comes from the route table. A pattern
// the mux serves but the table doesn't list (the mux names such patterns in some
// of its redirects) is no route as far as auth, the limiter and the metrics are
// concerned: it is never public, and its requests are labelled "unmatched".
func TestPatternsOutsideTheTableAreNeverPublicOrLabelled(t *testing.T) {
	srv, _ := setup(t)
	srv.mux.HandleFunc("GET /extra/{x}", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })

	if rec := serveAs(srv, "", "GET", "/extra/1"); rec.Code != 401 {
		t.Fatalf("a pattern outside the table served an unauthenticated request: %d", rec.Code)
	}
	if rec := serveAs(srv, testAPIKey, "GET", "/extra/1"); rec.Code != http.StatusNoContent {
		t.Fatalf("authenticated GET /extra/1 = %d, want the handler's 204", rec.Code)
	}
	exp := parseMetrics(t, srv.MetricsText())
	for _, s := range exp.Family(durationFamily).Samples {
		if strings.Contains(s.Labels["route"], "/extra/") {
			t.Errorf("a pattern outside the table became a label: %v", s.Labels)
		}
	}
	if got := mustDuration(t, exp, durationCount, "route", unmatchedRoute, "status", "2xx"); got != 1 {
		t.Errorf("unmatched 2xx = %v, want the one request to /extra/1", got)
	}
}

// Publishing has always accepted its path with a trailing slash.
func TestPublishAcceptsATrailingSlash(t *testing.T) {
	_, ts := setup(t)
	sb := createSandbox(t, ts, uniqueName(t, "slash"))

	resp := doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/publish/", map[string]any{"port": 8080, "alias": "slash-app"})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("POST .../publish/ = %d %s, want 201", resp.StatusCode, body)
	}
	resp = doReq(t, ts, "GET", "/sandboxes/"+sb.ID+"/publish/", nil)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "slash-app") {
		t.Fatalf("GET .../publish/ = %d %s, want the rule just made", resp.StatusCode, body)
	}
}

// Whether a request needs a key is decided on the route that will serve it. It
// was decided on the cleaned, decoded path while the mux routed on the encoded
// one, so "/secrets/%2e%2e/health" (which cleans to /health) skipped auth and
// reached handlers that assume a user: a nil dereference.
func TestEncodedDotSegmentsCannotReachAuthenticatedHandlers(t *testing.T) {
	srv, _ := setup(t)
	for _, tc := range []struct{ method, path string }{
		{"DELETE", "/secrets/%2e%2e/health"},
		{"GET", "/images/%2e%2e/health"},
		{"GET", "/snapshots/%2e%2e/health"},
		{"GET", "/volumes/%2e%2e/health"},
		{"GET", "/tasks/%2e%2e/health"},
		{"GET", "/sandboxes/%2e%2e/health"},
	} {
		rec := serveAs(srv, "", tc.method, tc.path)
		if rec.Code != 401 {
			t.Errorf("unauthenticated %s %s = %d %s, want 401", tc.method, tc.path, rec.Code, rec.Body)
		}
	}
}
