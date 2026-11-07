package server

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg"
	"github.com/sahil-shubham/bhatti/pkg/promtext"
	"github.com/sahil-shubham/bhatti/pkg/promtext/promtexttest"
)

// parseMetrics reads Prometheus text with the strict test parser, so anything a
// scraper would distrust (a histogram whose buckets don't add up, a repeated
// series, a bad label) fails the test where it is produced.
func parseMetrics(t *testing.T, text string) *promtexttest.Exposition {
	t.Helper()
	exp, err := promtexttest.Parse(strings.NewReader(text))
	if err != nil {
		t.Fatalf("not a valid exposition: %v\n%s", err, text)
	}
	return exp
}

// duration finds the sample of the duration histogram called name (its
// _bucket, _sum or _count) whose labels include every given name, value pair.
func duration(exp *promtexttest.Exposition, name string, labels ...string) (float64, bool) {
	fam := exp.Family(durationFamily)
	if fam == nil {
		return 0, false
	}
	for _, s := range fam.Samples {
		if s.Name != name {
			continue
		}
		match := true
		for i := 0; i < len(labels); i += 2 {
			if got, ok := s.Labels[labels[i]]; !ok || got != labels[i+1] {
				match = false
			}
		}
		if match {
			return s.Value, true
		}
	}
	return 0, false
}

func mustDuration(t *testing.T, exp *promtexttest.Exposition, name string, labels ...string) float64 {
	t.Helper()
	v, ok := duration(exp, name, labels...)
	if !ok {
		t.Fatalf("no %s sample with labels %v", name, labels)
	}
	return v
}

// totalCount is the sum of the histogram's _count over every series, or over
// those whose status label is class when class is given.
func totalCount(exp *promtexttest.Exposition, class string) float64 {
	var n float64
	if fam := exp.Family(durationFamily); fam != nil {
		for _, s := range fam.Samples {
			if s.Name == durationCount && (class == "" || s.Labels["status"] == class) {
				n += s.Value
			}
		}
	}
	return n
}

func counterValue(t *testing.T, exp *promtexttest.Exposition, name string) float64 {
	t.Helper()
	fam := exp.Family(name)
	if fam == nil {
		t.Fatalf("no family %s", name)
	}
	if fam.Type != "counter" || fam.Help == "" {
		t.Errorf("%s: type %q, help %q; want a documented counter", name, fam.Type, fam.Help)
	}
	v, ok := fam.Value()
	if !ok {
		t.Fatalf("family %s has no sample", name)
	}
	return v
}

func renderHistogram(t *testing.T, m *requestMetrics) *promtexttest.Exposition {
	t.Helper()
	var b strings.Builder
	pw := promtext.New(&b)
	m.writeTo(pw)
	if err := pw.Flush(); err != nil {
		t.Fatal(err)
	}
	return parseMetrics(t, b.String())
}

func TestHistogramCountsARequestUnderItsPattern(t *testing.T) {
	srv, ts := setup(t)
	sb := createSandbox(t, ts, uniqueName(t, "timed"))
	for _, ref := range []string{sb.ID, sb.Name, "no-such-sandbox"} {
		doReq(t, ts, "GET", "/sandboxes/"+ref, nil).Body.Close()
	}

	exp := parseMetrics(t, srv.MetricsText())
	const route = "GET /sandboxes/{id}"
	if got := mustDuration(t, exp, durationCount, "route", route, "status", "2xx"); got != 2 {
		t.Errorf("%s 2xx count = %v, want 2 (by ID and by name)", route, got)
	}
	if got := mustDuration(t, exp, durationCount, "route", route, "status", "4xx"); got != 1 {
		t.Errorf("%s 4xx count = %v, want 1", route, got)
	}
	if got := mustDuration(t, exp, durationCount, "route", "POST /sandboxes", "status", "2xx"); got != 1 {
		t.Errorf("POST /sandboxes 2xx count = %v, want 1", got)
	}
	for _, s := range exp.Family(durationFamily).Samples {
		if strings.Contains(s.Labels["route"], sb.ID) || strings.Contains(s.Labels["route"], "no-such-sandbox") {
			t.Errorf("a series is labelled by what was in the URL, not by the route: %v", s.Labels)
		}
	}

	if got := mustDuration(t, exp, durationBucket, "route", route, "status", "2xx", "le", "+Inf"); got != 2 {
		t.Errorf("+Inf bucket = %v, want the count, 2", got)
	}
	if sum := mustDuration(t, exp, durationSum, "route", route, "status", "2xx"); sum <= 0 || sum > 5 {
		t.Errorf("_sum = %v seconds for two quick requests", sum)
	}

	// The histogram and the counter count the same requests.
	if total := counterValue(t, exp, "bhatti_api_requests_total"); total != totalCount(exp, "") || total != float64(srv.requestTotal.Load()) {
		t.Errorf("bhatti_api_requests_total = %v, histogram counts %v, requestTotal %d", total, totalCount(exp, ""), srv.requestTotal.Load())
	}
}

// A client can't mint series: whatever it sends, the route label is one of the
// registered patterns or "unmatched".
func TestRouteLabelsAreBoundedByTheRouteTable(t *testing.T) {
	srv, _ := setup(t)
	srv.limiter = generousLimits()
	for i := range 30 {
		serveAs(srv, testAPIKey, "GET", fmt.Sprintf("/made/up/%d", i))                  // 404, no route
		serveAs(srv, testAPIKey, "GET", fmt.Sprintf("/sandboxes/id-%d", i))             // a route, a handler 404
		serveAs(srv, testAPIKey, "GET", fmt.Sprintf("/sandboxes/id-%d/not-a-route", i)) // 404, no route
		serveAs(srv, testAPIKey, "PUT", fmt.Sprintf("/sandboxes/id-%d", i))             // 405, no route
	}
	// Two redirects. Which pattern the mux names for one depends on the Go
	// version: that of the cleaned path, or the request's own path with a slash
	// added. Neither may ever become a label unless it is a registered pattern.
	for _, path := range []string{"/sandboxes/a//b", "/sandboxes/abc/proxy"} {
		if rec := serveAs(srv, testAPIKey, "GET", path); rec.Code/100 != 3 {
			t.Fatalf("%s = %d, want a redirect", path, rec.Code)
		}
	}

	known := map[string]bool{unmatchedRoute: true}
	for _, rt := range srv.routeTable() {
		known[rt.pattern] = true
	}
	exp := parseMetrics(t, srv.MetricsText())
	routes := map[string]bool{}
	for _, s := range exp.Family(durationFamily).Samples {
		route := s.Labels["route"]
		routes[route] = true
		if !known[route] {
			t.Errorf("metrics carry a route label %q that is not a registered pattern", route)
		}
	}
	if !routes[unmatchedRoute] || !routes["GET /sandboxes/{id}"] {
		t.Errorf("route labels = %v, want at least unmatched and GET /sandboxes/{id}", routes)
	}
	if got := mustDuration(t, exp, durationCount, "route", unmatchedRoute, "status", "4xx"); got != 90 {
		t.Errorf("unmatched 4xx count = %v, want 90", got)
	}
	if got := mustDuration(t, exp, durationCount, "route", "GET /sandboxes/{id}", "status", "4xx"); got != 30 {
		t.Errorf("GET /sandboxes/{id} 4xx count = %v, want 30", got)
	}
	if got := totalCount(exp, "3xx"); got != 2 {
		t.Errorf("3xx count = %v, want the 2 redirects", got)
	}
}

func TestHistogramBucketsAreCumulative(t *testing.T) {
	m := newRequestMetrics([]string{"GET /x"})
	for _, d := range []time.Duration{
		500 * time.Microsecond, // le 0.001
		time.Millisecond,       // exactly 0.001: an upper bound is inclusive
		2 * time.Millisecond,   // le 0.0025
		3 * time.Second,        // le 5
		time.Hour,              // only +Inf
	} {
		m.observe("GET /x", 200, d)
	}
	// The strict parser has already checked that the bounds ascend to +Inf, that
	// the counts never decrease and that _count equals the +Inf bucket.
	exp := renderHistogram(t, m)
	for le, want := range map[string]float64{
		"0.001": 2, "0.0025": 3, "0.005": 3, "2.5": 3, "5": 4, "300": 4, "+Inf": 5,
	} {
		if got := mustDuration(t, exp, durationBucket, "route", "GET /x", "status", "2xx", "le", le); got != want {
			t.Errorf("le=%s: %v, want %v", le, got, want)
		}
	}
	if got := mustDuration(t, exp, durationCount, "route", "GET /x", "status", "2xx"); got != 5 {
		t.Errorf("count = %v, want 5", got)
	}
	wantSum := 0.0005 + 0.001 + 0.002 + 3 + 3600
	if got := mustDuration(t, exp, durationSum, "route", "GET /x", "status", "2xx"); math.Abs(got-wantSum) > 1e-6 {
		t.Errorf("sum = %v, want %v", got, wantSum)
	}
}

func TestStatusClassesAndUnregisteredRoutes(t *testing.T) {
	m := newRequestMetrics([]string{"GET /x"})
	for status, class := range map[int]string{101: "1xx", 200: "2xx", 204: "2xx", 301: "3xx", 404: "4xx", 429: "4xx", 500: "5xx", 503: "5xx", 999: "5xx"} {
		before, _ := duration(renderHistogram(t, m), durationCount, "route", "GET /x", "status", class)
		m.observe("GET /x", status, time.Millisecond)
		after, ok := duration(renderHistogram(t, m), durationCount, "route", "GET /x", "status", class)
		if !ok || after != before+1 {
			t.Errorf("status %d is not counted under %s: %v then %v", status, class, before, after)
		}
	}
	// A pattern nobody registered, and a request no route matched, share a series.
	m.observe("GET /never-registered", 404, time.Millisecond)
	m.observe("", 404, time.Millisecond)
	if got := mustDuration(t, renderHistogram(t, m), durationCount, "route", unmatchedRoute, "status", "4xx"); got != 2 {
		t.Errorf("unmatched count = %v, want 2", got)
	}
}

func TestSeriesAppearOnlyOnceTheyHaveARequest(t *testing.T) {
	m := newRequestMetrics([]string{"GET /x", "GET /y"})
	exp := renderHistogram(t, m)
	if fam := exp.Family(durationFamily); fam == nil || fam.Type != "histogram" || len(fam.Samples) != 0 {
		t.Fatalf("an idle histogram should declare its family and have no series: %+v", fam)
	}
	m.observe("GET /y", 200, time.Millisecond)
	for _, s := range renderHistogram(t, m).Family(durationFamily).Samples {
		if s.Labels["route"] != "GET /y" {
			t.Fatalf("only the route that saw a request has a series, got %v", s.Labels)
		}
	}
}

func TestMetricsCountAuthFailuresAndRefusals(t *testing.T) {
	srv, _ := setup(t)
	srv.limiter = newRateLimiter(pkg.RateLimits{Read: pkg.RateBucket{PerMinute: 1, Burst: 1}})

	serveAs(srv, "wrong-key", "GET", "/sandboxes")
	serveAs(srv, "wrong-key", "GET", "/sandboxes")
	serveAs(srv, testAPIKey, "GET", "/sandboxes")
	serveAs(srv, testAPIKey, "GET", "/sandboxes") // 429

	exp := parseMetrics(t, srv.MetricsText())
	for name, want := range map[string]float64{
		"bhatti_api_requests_total":      1,
		"bhatti_api_errors_total":        0,
		"bhatti_api_auth_failures_total": 2,
		"bhatti_api_rate_limited_total":  1,
	} {
		if got := counterValue(t, exp, name); got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
}

// The families can be added to an exposition that has others in it, which is
// how the endpoint that serves them will use them.
func TestWriteMetricsComposesWithOtherFamilies(t *testing.T) {
	srv, _ := setup(t)
	serveAs(srv, testAPIKey, "GET", "/sandboxes")

	var b strings.Builder
	pw := promtext.New(&b)
	pw.Family("bhatti_host_load1", "Another source's gauge.", promtext.Gauge)
	pw.Sample("bhatti_host_load1", 0.5)
	srv.WriteMetrics(pw)
	if err := pw.Flush(); err != nil {
		t.Fatalf("the families collide or are malformed: %v", err)
	}
	exp := parseMetrics(t, b.String())
	if exp.Family("bhatti_host_load1") == nil || exp.Family("bhatti_api_requests_total") == nil || exp.Family(durationFamily) == nil {
		t.Fatalf("families = %v, want the other source's and the API's", exp.Families)
	}
}

// Requests record from many goroutines at once, with no lock: under -race the
// totals must still be exact.
func TestObserveIsSafeConcurrently(t *testing.T) {
	m := newRequestMetrics([]string{"GET /a", "GET /b"})
	const workers, each = 8, 2000
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			route := "GET /a"
			if w%2 == 1 {
				route = "GET /b"
			}
			for i := range each {
				m.observe(route, 200+(i%2)*204, time.Duration(i)*time.Microsecond)
			}
		})
	}
	wg.Wait()

	exp := renderHistogram(t, m)
	const want = workers / 2 * each / 2
	for _, route := range []string{"GET /a", "GET /b"} {
		for _, class := range []string{"2xx", "4xx"} {
			if got := mustDuration(t, exp, durationCount, "route", route, "status", class); got != want {
				t.Errorf("%s %s count = %v, want %d", route, class, got, want)
			}
		}
	}
}
