package server

import (
	"net/http"
	"time"

	_ "embed"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

//go:embed dashboard.html
var dashboardHTML []byte

// handleDashboard serves the embedded, self-contained read-only dashboard SPA.
// Mirrors serveShellHTML's embed-and-serve pattern (shell_handlers.go). The
// page is gated by the same bearer auth as every other s.mux route (see
// Server.ServeHTTP, server.go:830-851); the SPA re-uses the caller's token
// for its polling fetch to /dashboard/data.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errResp(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(dashboardHTML)
}

// dashboardSandbox is the per-sandbox row the dashboard renders.
type dashboardSandbox struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	Thermal   string    `json:"thermal"`
	IP        string    `json:"ip"`
	CPUs      float64   `json:"cpus"`
	MemoryMB  int       `json:"memory_mb"`
	CPUPct    float64   `json:"cpu_pct"`
	RSSBytes  int64     `json:"rss_bytes"`
	UptimeS   int64     `json:"uptime_s"`
	CreatedAt time.Time `json:"created_at"`
}

// dashboardData is the JSON payload polled by the SPA.
type dashboardData struct {
	Sandboxes []dashboardSandbox `json:"sandboxes"`
	Events    []store.Event      `json:"events"`
	Summary   dashboardSummary   `json:"summary"`
}

type dashboardSummary struct {
	ByThermal map[string]int `json:"by_thermal"`
	Total     int            `json:"total"`
	RSSBytes  int64          `json:"rss_bytes"`
}

// handleDashboardData returns the live JSON snapshot the dashboard polls.
// Read-only over the store + engine thermal state; no VM interaction.
func (s *Server) handleDashboardData(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errResp(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	sandboxes, _ := s.store.ListAllSandboxes()
	te, hasThermal := s.engine.(ThermalEngine)
	sp, hasStats := s.engine.(engine.StatsProvider)

	out := dashboardData{
		Sandboxes: make([]dashboardSandbox, 0, len(sandboxes)),
		Summary:   dashboardSummary{ByThermal: map[string]int{"hot": 0, "warm": 0, "cold": 0}},
	}

	for _, sb := range sandboxes {
		thermal := "cold"
		if hasThermal && sb.Status == "running" {
			switch te.ThermalState(sb.EngineID) {
			case "hot":
				thermal = "hot"
			case "warm":
				thermal = "warm"
			default:
				thermal = "cold"
			}
		}
		out.Summary.ByThermal[thermal]++
		var cpu float64
		var rss int64
		if hasStats && sb.Status == "running" {
			if st, serr := sp.Stats(r.Context(), sb.EngineID); serr == nil {
				cpu, rss = st.CPUPct, st.RSSBytes
			}
		}
		out.Summary.RSSBytes += rss
		out.Sandboxes = append(out.Sandboxes, dashboardSandbox{
			ID:        sb.ID,
			Name:      sb.Name,
			Status:    sb.Status,
			Thermal:   thermal,
			IP:        sb.IP,
			CPUs:      sb.CPUs,
			MemoryMB:  sb.MemoryMB,
			CPUPct:    cpu,
			RSSBytes:  rss,
			UptimeS:   int64(time.Since(sb.CreatedAt).Seconds()),
			CreatedAt: sb.CreatedAt,
		})
	}
	out.Summary.Total = len(sandboxes)

	events, _ := s.store.QueryEvents(store.EventFilter{Limit: 50})
	if events == nil {
		events = []store.Event{}
	}
	out.Events = events

	writeJSON(w, http.StatusOK, out)
}
