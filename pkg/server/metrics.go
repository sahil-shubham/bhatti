package server

import (
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/promtext"
)

// requestBuckets are the upper bounds, in seconds, of the request-duration
// histogram. They are fixed so recording is a lock-free increment. The spread
// runs from a SQLite read (~1ms) to a sandbox create or an exec that runs for
// minutes; anything slower lands in the implicit +Inf bucket.
var requestBuckets = [...]float64{
	0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 300,
}

// statusClasses name the series of a route by the first digit of the status
// the client got. An upgraded connection (101) is its own class so the
// minutes-long life of a shell doesn't bend the latency of the 2xx series.
var statusClasses = [...]string{"1xx", "2xx", "3xx", "4xx", "5xx"}

// latencyHistogram counts observations per bucket without locking.
type latencyHistogram struct {
	// counts[i] holds observations in (requestBuckets[i-1], requestBuckets[i]];
	// the last slot is +Inf. Exposition makes them cumulative.
	counts [len(requestBuckets) + 1]atomic.Uint64
	sumNs  atomic.Int64
}

func (h *latencyHistogram) observe(d time.Duration) {
	// A bucket is "le": an observation equal to a bound belongs to that bucket.
	i := sort.SearchFloat64s(requestBuckets[:], d.Seconds())
	h.counts[i].Add(1)
	h.sumNs.Add(int64(d))
}

// requestMetrics records how long API requests take, per route pattern and
// status class. Every route is known when the server is built, so the map is
// never written afterwards and a request records with no lock and no
// allocation.
type requestMetrics struct {
	routes map[string]*[len(statusClasses)]latencyHistogram
}

func newRequestMetrics(patterns []string) *requestMetrics {
	m := &requestMetrics{routes: make(map[string]*[len(statusClasses)]latencyHistogram, len(patterns)+1)}
	for _, p := range append([]string{unmatchedRoute}, patterns...) {
		m.routes[p] = new([len(statusClasses)]latencyHistogram)
	}
	return m
}

// observe records one finished request. A pattern that was never registered
// counts as unmatched, which keeps the series set bounded.
func (m *requestMetrics) observe(pattern string, status int, d time.Duration) {
	series, ok := m.routes[pattern]
	if !ok || pattern == "" {
		series = m.routes[unmatchedRoute]
	}
	series[min(max(status/100, 1), len(statusClasses))-1].observe(d)
}

const (
	durationFamily = "bhatti_api_request_duration_seconds"
	durationBucket = durationFamily + "_bucket"
	durationSum    = durationFamily + "_sum"
	durationCount  = durationFamily + "_count"
)

// writeTo adds the duration histogram to pw. Only series that saw a request are
// written, ordered by route then class so the output is stable.
func (m *requestMetrics) writeTo(pw *promtext.Writer) {
	pw.Family(durationFamily, "Time to answer an API request that passed auth and the rate limit, by the route pattern that matched (unmatched for none) and the class of the status.", promtext.Histogram)

	routes := make([]string, 0, len(m.routes))
	for route := range m.routes {
		routes = append(routes, route)
	}
	sort.Strings(routes)

	for _, route := range routes {
		for class, label := range statusClasses {
			h := &m.routes[route][class]
			var counts [len(requestBuckets) + 1]uint64
			var total uint64
			for i := range counts {
				counts[i] = h.counts[i].Load()
				total += counts[i]
			}
			if total == 0 {
				continue
			}
			var cumulative uint64
			for i, bound := range requestBuckets {
				cumulative += counts[i]
				pw.Sample(durationBucket, float64(cumulative), "route", route, "status", label, "le", strconv.FormatFloat(bound, 'g', -1, 64))
			}
			pw.Sample(durationBucket, float64(total), "route", route, "status", label, "le", "+Inf")
			pw.Sample(durationSum, float64(h.sumNs.Load())/float64(time.Second), "route", route, "status", label)
			pw.Sample(durationCount, float64(total), "route", route, "status", label)
		}
	}
}

// WriteMetrics adds the API's families to pw, for a /metrics endpoint that
// composes them with the collector's: the request counters the metrics
// snapshots also keep, and the request-duration histogram by route. Every
// family name starts with bhatti_api_.
func (s *Server) WriteMetrics(pw *promtext.Writer) {
	counter := func(name, help string, v int64) {
		pw.Family(name, help, promtext.Counter)
		pw.Sample(name, float64(v))
	}
	counter("bhatti_api_requests_total", "API requests that passed auth and the rate limit and were answered.", s.requestTotal.Load())
	counter("bhatti_api_errors_total", "Of those, the ones answered with a 5xx status.", s.requestErrors.Load())
	counter("bhatti_api_auth_failures_total", "Requests refused for a missing or unknown API key.", s.authFailures.Load())
	counter("bhatti_api_rate_limited_total", "Requests refused by the per-user rate limit.", s.rateLimited.Load())
	s.reqMetrics.writeTo(pw)
}

// MetricsText renders WriteMetrics as Prometheus text exposition (serve it as
// text/plain; version=0.0.4).
func (s *Server) MetricsText() string {
	var b strings.Builder
	pw := promtext.New(&b)
	s.WriteMetrics(pw)
	if err := pw.Flush(); err != nil {
		slog.Error("metrics.render_failed", "error", err)
	}
	return b.String()
}
