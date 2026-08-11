package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sahil-shubham/bhatti/pkg/store"
)

// seedObservability creates two running sandboxes and a few events so the
// metrics/dashboard endpoints have real data to reflect.
func seedObservability(t *testing.T, srv *Server, ts *httptest.Server) {
	t.Helper()
	createSandbox(t, ts, "obs-one")
	createSandbox(t, ts, "obs-two")

	// Insert events directly (deterministic; avoids the recorder's async flush).
	if err := srv.store.InsertEvents([]store.Event{
		{Type: "sandbox.create"},
		{Type: "sandbox.create"},
		{Type: "thermal.pause"},
	}); err != nil {
		t.Fatalf("insert events: %v", err)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	srv, ts := setup(t)
	seedObservability(t, srv, ts)

	resp := doReq(t, ts, "GET", "/metrics", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("expected text/plain content-type, got %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	s := string(body)

	for _, want := range []string{
		"bhatti_up 1",
		"# TYPE bhatti_sandboxes gauge",
		"bhatti_sandboxes{status=\"running\"",
		"# TYPE bhatti_events_total counter",
		"bhatti_events_total{type=\"sandbox.create\"} 2",
		"bhatti_start_time_seconds",
		"bhatti_sandbox_info{",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("metrics output missing %q\n---\n%s", want, s)
		}
	}
}

func TestDashboardEndpoint(t *testing.T) {
	srv, ts := setup(t)
	seedObservability(t, srv, ts)

	resp := doReq(t, ts, "GET", "/dashboard", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("expected text/html content-type, got %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `id="mon"`) {
		t.Fatalf(`dashboard HTML missing marker id="mon"`)
	}
}

func TestDashboardDataEndpoint(t *testing.T) {
	srv, ts := setup(t)
	seedObservability(t, srv, ts)

	resp := doReq(t, ts, "GET", "/dashboard/data", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var data struct {
		Sandboxes []struct {
			Name    string `json:"name"`
			Status  string `json:"status"`
			Thermal string `json:"thermal"`
		} `json:"sandboxes"`
		Events  []store.Event `json:"events"`
		Summary struct {
			ByThermal map[string]int `json:"by_thermal"`
			Total     int            `json:"total"`
		} `json:"summary"`
	}
	decodeJSON(t, resp, &data)

	if len(data.Sandboxes) != 2 {
		t.Fatalf("expected 2 sandboxes, got %d", len(data.Sandboxes))
	}
	if data.Summary.Total != 2 {
		t.Fatalf("expected summary total 2, got %d", data.Summary.Total)
	}
	if data.Summary.ByThermal == nil {
		t.Fatalf("expected by_thermal map, got nil")
	}
	if len(data.Events) < 3 {
		t.Fatalf("expected >=3 events, got %d", len(data.Events))
	}
	for _, sb := range data.Sandboxes {
		if sb.Status != "running" {
			t.Errorf("expected running sandbox, got %q", sb.Status)
		}
		if sb.Thermal == "" {
			t.Errorf("expected non-empty thermal for %q", sb.Name)
		}
	}
}

func TestObservabilityRequiresAuth(t *testing.T) {
	_, ts := setup(t)
	for _, path := range []string{"/metrics", "/dashboard", "/dashboard/data"} {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: request failed: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("%s: expected 401 without auth, got %d", path, resp.StatusCode)
		}
	}
}
