package server

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/store"
)

// handleMetrics serves a Prometheus text-exposition (format 0.0.4) view over
// data that already exists: the SQLite store's sandboxes, the engine's
// thermal state, and the event recorder's persisted events. Hand-rolled —
// bhatti takes no client_golang dependency (self-hosted, tiny surface).
//
// Auth: registered on s.mux, so it sits behind the bearer-token check in
// Server.ServeHTTP (server.go:830-851) exactly like every other API route.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errResp(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var b strings.Builder

	// --- process liveness ---
	writeMetricHeader(&b, "bhatti_up", "1 if the bhatti server is up.", "gauge")
	fmt.Fprintf(&b, "bhatti_up 1\n")

	writeMetricHeader(&b, "bhatti_start_time_seconds",
		"Unix timestamp of when the bhatti server started.", "gauge")
	fmt.Fprintf(&b, "bhatti_start_time_seconds %d\n", s.startTime.Unix())

	// --- sandboxes gauge, grouped by (status, thermal) ---
	sandboxes, _ := s.store.ListAllSandboxes()
	te, hasThermal := s.engine.(ThermalEngine)

	// Count sandboxes keyed by status(+thermal). Thermal is only meaningful
	// for running sandboxes with a thermal-capable engine; otherwise the
	// label is omitted so the series stays honest.
	type sbKey struct{ status, thermal string }
	counts := map[sbKey]int{}
	for _, sb := range sandboxes {
		thermal := ""
		if hasThermal && sb.Status == "running" {
			thermal = te.ThermalState(sb.EngineID)
			if thermal == "" {
				thermal = "cold"
			}
		}
		counts[sbKey{sb.Status, thermal}]++
	}

	writeMetricHeader(&b, "bhatti_sandboxes",
		"Number of sandboxes grouped by status and thermal state.", "gauge")
	// Deterministic ordering for stable scrapes/tests.
	keys := make([]sbKey, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].status != keys[j].status {
			return keys[i].status < keys[j].status
		}
		return keys[i].thermal < keys[j].thermal
	})
	if len(keys) == 0 {
		// Emit a zero series so the metric name is always present.
		fmt.Fprintf(&b, "bhatti_sandboxes{status=\"\"} 0\n")
	}
	for _, k := range keys {
		if k.thermal == "" {
			fmt.Fprintf(&b, "bhatti_sandboxes{status=\"%s\"} %d\n",
				escapeLabel(k.status), counts[k])
		} else {
			fmt.Fprintf(&b, "bhatti_sandboxes{status=\"%s\",thermal=\"%s\"} %d\n",
				escapeLabel(k.status), escapeLabel(k.thermal), counts[k])
		}
	}

	// --- per-running-sandbox info + uptime ---
	writeMetricHeader(&b, "bhatti_sandbox_info",
		"Static info for each running sandbox (value is always 1).", "gauge")
	writeMetricHeader(&b, "bhatti_sandbox_uptime_seconds",
		"Uptime in seconds of each running sandbox (from created_at).", "gauge")
	now := time.Now()
	for _, sb := range sandboxes {
		if sb.Status != "running" {
			continue
		}
		fmt.Fprintf(&b, "bhatti_sandbox_info{id=\"%s\",name=\"%s\",ip=\"%s\"} 1\n",
			escapeLabel(sb.ID), escapeLabel(sb.Name), escapeLabel(sb.IP))
		uptime := now.Sub(sb.CreatedAt).Seconds()
		if uptime < 0 {
			uptime = 0
		}
		fmt.Fprintf(&b, "bhatti_sandbox_uptime_seconds{id=\"%s\",name=\"%s\"} %d\n",
			escapeLabel(sb.ID), escapeLabel(sb.Name), int64(uptime))
	}

	// --- events counter, grouped by type ---
	// Aggregate recent persisted events by Type. Bounded read (the store has
	// no group-by helper); events are pruned by retention so this is the
	// recent-window count, which is the honest thing we can cheaply expose.
	writeMetricHeader(&b, "bhatti_events_total",
		"Count of recorded events grouped by type (recent retention window).", "counter")
	events, _ := s.store.QueryEvents(store.EventFilter{Limit: 10000})
	byType := map[string]int{}
	for _, e := range events {
		byType[e.Type]++
	}
	types := make([]string, 0, len(byType))
	for t := range byType {
		types = append(types, t)
	}
	sort.Strings(types)
	if len(types) == 0 {
		fmt.Fprintf(&b, "bhatti_events_total{type=\"\"} 0\n")
	}
	for _, t := range types {
		fmt.Fprintf(&b, "bhatti_events_total{type=\"%s\"} %d\n", escapeLabel(t), byType[t])
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}

// writeMetricHeader emits the # HELP and # TYPE lines for a metric.
func writeMetricHeader(b *strings.Builder, name, help, typ string) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s %s\n", name, typ)
}

// escapeLabel escapes a Prometheus label value per the exposition format:
// backslash, double-quote, and newline. Callers wrap the result in literal
// double quotes ("%s"), never %q, which would re-escape the backslashes.
func escapeLabel(v string) string {
	r := strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`)
	return r.Replace(v)
}
