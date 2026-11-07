package server

import (
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

type cgroupReportingEngine struct {
	*mockEngine
	status engine.CgroupStatus
}

func (e *cgroupReportingEngine) CgroupStatus() engine.CgroupStatus { return e.status }

func healthFor(t *testing.T, eng engine.Engine) map[string]any {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(t.Context(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	srv := New(eng, st, dir)
	ts := httptest.NewServer(srv)
	t.Cleanup(func() { srv.Close(); ts.Close(); st.Close() })

	resp := doReq(t, ts, "GET", "/health", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var health map[string]any
	decodeJSON(t, resp, &health)
	return health
}

func TestHealthReportsCgroupStatus(t *testing.T) {
	health := healthFor(t, &cgroupReportingEngine{
		mockEngine: newMockEngine(),
		status:     engine.CgroupStatus{Mode: engine.CgroupNone, Requested: "auto", Reason: "system bus unavailable"},
	})
	cg, ok := health["cgroups"].(map[string]any)
	if !ok {
		t.Fatalf("cgroups missing or wrong type: %v", health["cgroups"])
	}
	if cg["mode"] != "none" || cg["requested"] != "auto" || cg["reason"] != "system bus unavailable" {
		t.Fatalf("unexpected cgroups status: %v", cg)
	}
}

func TestHealthOmitsCgroupsWithoutReporter(t *testing.T) {
	health := healthFor(t, newMockEngine())
	if _, ok := health["cgroups"]; ok {
		t.Fatalf("cgroups key present for an engine that doesn't report: %v", health["cgroups"])
	}
}
