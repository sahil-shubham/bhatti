package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sahilshubham/bhatti/pkg/engine"
	"github.com/sahilshubham/bhatti/pkg/store"
)

// mockEngine implements engine.Engine for testing without Docker.
type mockEngine struct {
	containers map[string]*engine.SandboxInfo
}

func newMockEngine() *mockEngine {
	return &mockEngine{containers: make(map[string]*engine.SandboxInfo)}
}

func (m *mockEngine) Create(_ context.Context, spec engine.SandboxSpec) (engine.SandboxInfo, error) {
	id := "mock-" + spec.Name + "-000000000000"
	info := engine.SandboxInfo{
		ID:       id[:12],
		Name:     spec.Name,
		Status:   "running",
		IP:       "172.17.0.2",
		EngineID: id,
	}
	m.containers[id] = &info
	return info, nil
}

func (m *mockEngine) Destroy(_ context.Context, id string) error {
	delete(m.containers, id)
	return nil
}

func (m *mockEngine) Stop(_ context.Context, id string) error {
	if c, ok := m.containers[id]; ok {
		c.Status = "stopped"
	}
	return nil
}

func (m *mockEngine) Start(_ context.Context, id string) error {
	if c, ok := m.containers[id]; ok {
		c.Status = "running"
	}
	return nil
}

func (m *mockEngine) Status(_ context.Context, id string) (engine.SandboxInfo, error) {
	if c, ok := m.containers[id]; ok {
		return *c, nil
	}
	return engine.SandboxInfo{}, io.EOF
}

func (m *mockEngine) List(_ context.Context) ([]engine.SandboxInfo, error) {
	var out []engine.SandboxInfo
	for _, c := range m.containers {
		out = append(out, *c)
	}
	return out, nil
}

func (m *mockEngine) Exec(_ context.Context, id string, cmd []string) (engine.ExecResult, error) {
	return engine.ExecResult{
		ExitCode: 0,
		Stdout:   "mock output: " + strings.Join(cmd, " "),
	}, nil
}

func (m *mockEngine) Shell(_ context.Context, id string) (engine.TerminalConn, error) {
	return nil, io.EOF // not tested via HTTP in unit tests
}

func setup(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	srv := New(newMockEngine(), st, "test-token")
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return srv, ts
}

func doReq(t *testing.T, ts *httptest.Server, method, path string, body any) *http.Response {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, ts.URL+path, bodyReader)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodeJSON(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

// --- Tests ---

func TestAuthRequired(t *testing.T) {
	_, ts := setup(t)
	req, _ := http.NewRequest("GET", ts.URL+"/templates", nil)
	// No auth header
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestNoAuthWhenTokenEmpty(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.New(filepath.Join(dir, "test.db"))
	defer st.Close()

	srv := New(newMockEngine(), st, "") // empty token = no auth
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp, _ := http.Get(ts.URL + "/templates")
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestTemplateCRUD(t *testing.T) {
	_, ts := setup(t)

	// Create
	resp := doReq(t, ts, "POST", "/templates", map[string]any{
		"name":  "ubuntu-dev",
		"image": "ubuntu:22.04",
		"cpus":  2,
	})
	if resp.StatusCode != 201 {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	var tmpl store.Template
	decodeJSON(t, resp, &tmpl)
	if tmpl.Name != "ubuntu-dev" || tmpl.Image != "ubuntu:22.04" {
		t.Fatalf("unexpected template: %+v", tmpl)
	}

	// List
	resp = doReq(t, ts, "GET", "/templates", nil)
	var list []store.Template
	decodeJSON(t, resp, &list)
	if len(list) != 1 {
		t.Fatalf("expected 1 template, got %d", len(list))
	}

	// Get
	resp = doReq(t, ts, "GET", "/templates/"+tmpl.ID, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Delete
	resp = doReq(t, ts, "DELETE", "/templates/"+tmpl.ID, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Verify gone
	resp = doReq(t, ts, "GET", "/templates/"+tmpl.ID, nil)
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestSandboxLifecycle(t *testing.T) {
	_, ts := setup(t)

	// Create template first
	resp := doReq(t, ts, "POST", "/templates", map[string]any{
		"name":  "alpine",
		"image": "alpine:latest",
	})
	var tmpl store.Template
	decodeJSON(t, resp, &tmpl)

	// Create sandbox
	resp = doReq(t, ts, "POST", "/sandboxes", map[string]any{
		"template_id": tmpl.ID,
		"name":        "test-sb",
	})
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d: %s", resp.StatusCode, body)
	}
	var sb store.Sandbox
	decodeJSON(t, resp, &sb)
	if sb.Name != "test-sb" || sb.Status != "running" {
		t.Fatalf("unexpected sandbox: %+v", sb)
	}

	// List
	resp = doReq(t, ts, "GET", "/sandboxes", nil)
	var sbList []store.Sandbox
	decodeJSON(t, resp, &sbList)
	if len(sbList) != 1 {
		t.Fatalf("expected 1 sandbox, got %d", len(sbList))
	}

	// Exec
	resp = doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/exec", map[string]any{
		"cmd": []string{"echo", "hello"},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var result engine.ExecResult
	decodeJSON(t, resp, &result)
	if !strings.Contains(result.Stdout, "echo hello") {
		t.Fatalf("unexpected exec result: %+v", result)
	}

	// Stop — should return full sandbox object
	resp = doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/stop", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var stoppedSB store.Sandbox
	decodeJSON(t, resp, &stoppedSB)
	if stoppedSB.Status != "stopped" {
		t.Fatalf("expected stopped status, got %s", stoppedSB.Status)
	}
	if stoppedSB.StoppedAt == nil {
		t.Fatal("expected stopped_at to be set")
	}
	if stoppedSB.ID != sb.ID {
		t.Fatalf("expected same sandbox ID, got %s", stoppedSB.ID)
	}

	// Start — should return full sandbox object with refreshed info
	resp = doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/start", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var startedSB store.Sandbox
	decodeJSON(t, resp, &startedSB)
	if startedSB.Status != "running" {
		t.Fatalf("expected running status, got %s", startedSB.Status)
	}
	if startedSB.ID != sb.ID {
		t.Fatalf("expected same sandbox ID, got %s", startedSB.ID)
	}

	// Destroy
	resp = doReq(t, ts, "DELETE", "/sandboxes/"+sb.ID, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Verify gone
	resp = doReq(t, ts, "GET", "/sandboxes/"+sb.ID, nil)
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestSandboxCreateRequiresTemplate(t *testing.T) {
	_, ts := setup(t)

	resp := doReq(t, ts, "POST", "/sandboxes", map[string]any{
		"template_id": "nonexistent",
	})
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestSecretsCRUD(t *testing.T) {
	_, ts := setup(t)

	// Create
	resp := doReq(t, ts, "POST", "/secrets", map[string]any{
		"name":  "api-key",
		"value": "secret123",
	})
	if resp.StatusCode != 201 {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}

	// List
	resp = doReq(t, ts, "GET", "/secrets", nil)
	var list []store.SecretRecord
	decodeJSON(t, resp, &list)
	if len(list) != 1 || list[0].Name != "api-key" {
		t.Fatalf("unexpected secrets: %+v", list)
	}

	// Delete
	resp = doReq(t, ts, "DELETE", "/secrets/api-key", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestVolumeCRUD(t *testing.T) {
	_, ts := setup(t)

	// Create
	resp := doReq(t, ts, "POST", "/volumes", map[string]any{"name": "test-vol"})
	if resp.StatusCode != 201 {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	var vol store.Volume
	decodeJSON(t, resp, &vol)
	if vol.Name != "test-vol" {
		t.Fatalf("expected test-vol, got %s", vol.Name)
	}

	// List
	resp = doReq(t, ts, "GET", "/volumes", nil)
	var list []store.Volume
	decodeJSON(t, resp, &list)
	if len(list) != 1 {
		t.Fatalf("expected 1 volume, got %d", len(list))
	}

	// Get
	resp = doReq(t, ts, "GET", "/volumes/test-vol", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Delete
	resp = doReq(t, ts, "DELETE", "/volumes/test-vol", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Get deleted
	resp = doReq(t, ts, "GET", "/volumes/test-vol", nil)
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestVolumeDeleteConflict(t *testing.T) {
	_, ts := setup(t)

	// Create volume
	doReq(t, ts, "POST", "/volumes", map[string]any{"name": "busy-vol"})

	// Create template and sandbox with that volume
	resp := doReq(t, ts, "POST", "/templates", map[string]any{
		"name":  "alpine",
		"image": "alpine:latest",
	})
	var tmpl store.Template
	decodeJSON(t, resp, &tmpl)

	resp = doReq(t, ts, "POST", "/sandboxes", map[string]any{
		"template_id": tmpl.ID,
		"name":        "vol-sb",
		"volumes":     []map[string]any{{"name": "busy-vol", "target": "/data"}},
	})
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d: %s", resp.StatusCode, body)
	}
	var sb store.Sandbox
	decodeJSON(t, resp, &sb)

	// Try delete volume — should fail with 409
	resp = doReq(t, ts, "DELETE", "/volumes/busy-vol", nil)
	if resp.StatusCode != 409 {
		t.Fatalf("expected 409, got %d", resp.StatusCode)
	}

	// Destroy sandbox, then delete volume
	doReq(t, ts, "DELETE", "/sandboxes/"+sb.ID, nil)
	resp = doReq(t, ts, "DELETE", "/volumes/busy-vol", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestSandboxWithTemplateMounts(t *testing.T) {
	_, ts := setup(t)

	// Create template with default mounts
	resp := doReq(t, ts, "POST", "/templates", map[string]any{
		"name":  "dev-template",
		"image": "alpine:latest",
		"mounts": []map[string]any{
			{"volume_name": "shared-data", "target": "/data", "auto_create": true},
			{"target": "/workspace", "auto_create": true},
		},
	})
	if resp.StatusCode != 201 {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	var tmpl store.Template
	decodeJSON(t, resp, &tmpl)
	if len(tmpl.Mounts) != 2 {
		t.Fatalf("expected 2 mounts in template, got %d", len(tmpl.Mounts))
	}

	// Create sandbox — should use template mounts
	resp = doReq(t, ts, "POST", "/sandboxes", map[string]any{
		"template_id": tmpl.ID,
		"name":        "mount-test",
	})
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d: %s", resp.StatusCode, body)
	}
	var sb store.Sandbox
	decodeJSON(t, resp, &sb)

	// Verify the auto-created volumes exist
	resp = doReq(t, ts, "GET", "/volumes", nil)
	var vols []store.Volume
	decodeJSON(t, resp, &vols)

	volNames := map[string]bool{}
	for _, v := range vols {
		volNames[v.Name] = true
	}
	if !volNames["shared-data"] {
		t.Fatal("expected shared-data volume to be auto-created")
	}
	if !volNames["bhatti-mount-test-workspace"] {
		t.Fatalf("expected bhatti-mount-test-workspace volume, got %v", volNames)
	}

	// Cleanup
	doReq(t, ts, "DELETE", "/sandboxes/"+sb.ID, nil)
}

func TestVolumeValidation(t *testing.T) {
	_, ts := setup(t)

	// Missing name
	resp := doReq(t, ts, "POST", "/volumes", map[string]any{})
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestSecretValidation(t *testing.T) {
	_, ts := setup(t)

	// Missing value
	resp := doReq(t, ts, "POST", "/secrets", map[string]any{
		"name": "test",
	})
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}
