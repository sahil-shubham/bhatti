package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	dockerengine "github.com/sahil-shubham/bhatti/pkg/engine/docker"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

var (
	dockerCheckOnce sync.Once
	dockerAvailable bool
	alpinePullOnce  sync.Once
)

// TestMain runs once per package. It cleans up stale containers and volumes
// left behind by previous crashed test runs before any tests execute.
func TestMain(m *testing.M) {
	cleanupStaleTestResources()
	os.Exit(m.Run())
}

func cleanupStaleTestResources() {
	// Only clean stopped/exited/dead containers — never running ones,
	// because other test packages may be running in parallel.
	for _, status := range []string{"exited", "dead", "created"} {
		out, err := exec.Command(
			"docker", "ps", "-a",
			"--filter", "label=bhatti.managed=true",
			"--filter", "status="+status,
			"--format", "{{.Names}}",
		).Output()
		if err != nil {
			return
		}
		for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if strings.HasPrefix(name, "bhatti-test-") {
				exec.Command("docker", "rm", "-f", name).Run()
			}
		}
	}
	// Clean orphaned test volumes (only those not currently mounted)
	out, _ := exec.Command("docker", "volume", "ls", "--format", "{{.Name}}").Output()
	for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.HasPrefix(name, "bhatti-test-") {
			// -f won't remove in-use volumes, so this is safe
			exec.Command("docker", "volume", "rm", name).Run()
		}
	}
}

func skipIfNoDocker(t *testing.T) {
	t.Helper()
	dockerCheckOnce.Do(func() {
		if _, err := exec.LookPath("docker"); err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
			return
		}
		dockerAvailable = true
	})
	if !dockerAvailable {
		t.Skip("docker not available, skipping integration test")
	}
}

func ensureAlpinePulled(t *testing.T) {
	t.Helper()
	alpinePullOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "docker", "pull", "alpine:latest")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("failed to pre-pull alpine:latest: %v\n%s", err, out)
		}
	})
}

// uniqueName generates a collision-free resource name for tests.
// All names start with "bhatti-test-" so cleanupStaleTestResources can find them.
func uniqueName(t *testing.T, prefix string) string {
	t.Helper()
	b := make([]byte, 4)
	rand.Read(b)
	return fmt.Sprintf("bhatti-test-%s-%s", prefix, hex.EncodeToString(b))
}

// cleanupDockerVolume defers removal of a Docker volume created during a test.
func cleanupDockerVolume(t *testing.T, name string) {
	t.Helper()
	t.Cleanup(func() {
		exec.Command("docker", "volume", "rm", "-f", name).Run()
	})
}

func setup(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	skipIfNoDocker(t)
	ensureAlpinePulled(t)

	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}

	eng, err := dockerengine.New()
	if err != nil {
		t.Fatal(err)
	}

	srv := New(eng, st, "test-token")
	ts := httptest.NewServer(srv)
	t.Cleanup(func() {
		srv.Close()
		ts.Close()
		st.Close()
	})
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

// createTemplateAndSandbox is a helper that creates an alpine template and a
// sandbox from it, returning both. The sandbox container is automatically
// destroyed via t.Cleanup.
func createTemplateAndSandbox(t *testing.T, ts *httptest.Server, sbName string, volumes []map[string]any) (store.Template, store.Sandbox) {
	t.Helper()

	resp := doReq(t, ts, "POST", "/templates", map[string]any{
		"name":  "alpine",
		"image": "alpine:latest",
	})
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create template: expected 201, got %d: %s", resp.StatusCode, body)
	}
	var tmpl store.Template
	decodeJSON(t, resp, &tmpl)

	sbReq := map[string]any{
		"template_id": tmpl.ID,
		"name":        sbName,
	}
	if volumes != nil {
		sbReq["volumes"] = volumes
	}

	resp = doReq(t, ts, "POST", "/sandboxes", sbReq)
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create sandbox: expected 201, got %d: %s", resp.StatusCode, body)
	}
	var sb store.Sandbox
	decodeJSON(t, resp, &sb)

	t.Cleanup(func() {
		doReq(t, ts, "DELETE", "/sandboxes/"+sb.ID, nil)
	})

	return tmpl, sb
}

// --- Tests ---

func TestAuthRequired(t *testing.T) {
	_, ts := setup(t)
	req, _ := http.NewRequest("GET", ts.URL+"/templates", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestNoAuthWhenTokenEmpty(t *testing.T) {
	skipIfNoDocker(t)
	ensureAlpinePulled(t)

	dir := t.TempDir()
	st, _ := store.New(filepath.Join(dir, "test.db"))
	defer st.Close()

	eng, err := dockerengine.New()
	if err != nil {
		t.Fatal(err)
	}

	srv := New(eng, st, "") // empty token = no auth
	ts := httptest.NewServer(srv)
	defer func() { srv.Close(); ts.Close() }()

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
	name := uniqueName(t, "lifecycle")

	tmpl, sb := createTemplateAndSandbox(t, ts, name, nil)
	_ = tmpl

	if sb.Name != name || sb.Status != "running" {
		t.Fatalf("unexpected sandbox: %+v", sb)
	}

	// List — should contain our sandbox
	resp := doReq(t, ts, "GET", "/sandboxes", nil)
	var sbList []store.Sandbox
	decodeJSON(t, resp, &sbList)
	found := false
	for _, s := range sbList {
		if s.ID == sb.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("sandbox not found in list")
	}

	// Exec — real command runs inside the container
	resp = doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/exec", map[string]any{
		"cmd": []string{"echo", "hello"},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("exec: expected 200, got %d", resp.StatusCode)
	}
	var result engine.ExecResult
	decodeJSON(t, resp, &result)
	if strings.TrimSpace(result.Stdout) != "hello" {
		t.Fatalf("exec: expected 'hello', got %q", result.Stdout)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exec: expected exit code 0, got %d", result.ExitCode)
	}

	// Exec — env var from template (none set, just verify exec works with sh)
	resp = doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/exec", map[string]any{
		"cmd": []string{"sh", "-c", "echo $HOME"},
	})
	var envResult engine.ExecResult
	decodeJSON(t, resp, &envResult)
	if envResult.ExitCode != 0 {
		t.Fatalf("env exec failed: exit=%d stderr=%s", envResult.ExitCode, envResult.Stderr)
	}

	// Stop
	resp = doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/stop", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("stop: expected 200, got %d", resp.StatusCode)
	}
	var stoppedSB store.Sandbox
	decodeJSON(t, resp, &stoppedSB)
	if stoppedSB.Status != "stopped" {
		t.Fatalf("expected stopped, got %s", stoppedSB.Status)
	}
	if stoppedSB.StoppedAt == nil {
		t.Fatal("expected stopped_at to be set")
	}

	// Exec on stopped container — should fail
	resp = doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/exec", map[string]any{
		"cmd": []string{"echo", "should-fail"},
	})
	if resp.StatusCode != 500 {
		t.Fatalf("exec on stopped: expected 500, got %d", resp.StatusCode)
	}

	// Start
	resp = doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/start", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("start: expected 200, got %d", resp.StatusCode)
	}
	var startedSB store.Sandbox
	decodeJSON(t, resp, &startedSB)
	if startedSB.Status != "running" {
		t.Fatalf("expected running, got %s", startedSB.Status)
	}

	// Exec after restart — should work again
	resp = doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/exec", map[string]any{
		"cmd": []string{"echo", "back"},
	})
	var restartResult engine.ExecResult
	decodeJSON(t, resp, &restartResult)
	if strings.TrimSpace(restartResult.Stdout) != "back" {
		t.Fatalf("exec after restart: expected 'back', got %q", restartResult.Stdout)
	}

	// Destroy (explicit — t.Cleanup is the safety net)
	resp = doReq(t, ts, "DELETE", "/sandboxes/"+sb.ID, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("destroy: expected 200, got %d", resp.StatusCode)
	}

	// Verify gone
	resp = doReq(t, ts, "GET", "/sandboxes/"+sb.ID, nil)
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404 after destroy, got %d", resp.StatusCode)
	}
}

func TestSandboxCreateBadTemplate(t *testing.T) {
	_, ts := setup(t)

	resp := doReq(t, ts, "POST", "/sandboxes", map[string]any{
		"template_id": "nonexistent",
	})
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestCreateWithoutTemplate(t *testing.T) {
	_, ts := setup(t)
	name := uniqueName(t, "notempl")

	resp := doReq(t, ts, "POST", "/sandboxes", map[string]any{
		"name":      name,
		"cpus":      2,
		"memory_mb": 256,
	})
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d: %s", resp.StatusCode, body)
	}
	var sb store.Sandbox
	decodeJSON(t, resp, &sb)
	t.Cleanup(func() { doReq(t, ts, "DELETE", "/sandboxes/"+sb.ID, nil) })

	if sb.Name != name {
		t.Errorf("name: %q, want %q", sb.Name, name)
	}
	if sb.TemplateID != "" {
		t.Errorf("template_id: %q, want empty", sb.TemplateID)
	}
	if sb.Status != "running" {
		t.Errorf("status: %q, want running", sb.Status)
	}

	// Exec should work
	resp = doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/exec", map[string]any{
		"cmd": []string{"echo", "hello"},
	})
	var result engine.ExecResult
	decodeJSON(t, resp, &result)
	if strings.TrimSpace(result.Stdout) != "hello" {
		t.Errorf("exec: %q, want 'hello'", result.Stdout)
	}
}

func TestCreateWithoutTemplateDefaults(t *testing.T) {
	_, ts := setup(t)

	// Minimal request — only name
	resp := doReq(t, ts, "POST", "/sandboxes", map[string]any{
		"name": uniqueName(t, "defaults"),
	})
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d: %s", resp.StatusCode, body)
	}
	var sb store.Sandbox
	decodeJSON(t, resp, &sb)
	t.Cleanup(func() { doReq(t, ts, "DELETE", "/sandboxes/"+sb.ID, nil) })

	if sb.Status != "running" {
		t.Errorf("status: %q, want running", sb.Status)
	}
}

func TestCreateWithoutTemplateAutoName(t *testing.T) {
	_, ts := setup(t)

	// Empty body — should auto-generate name
	resp := doReq(t, ts, "POST", "/sandboxes", map[string]any{})
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d: %s", resp.StatusCode, body)
	}
	var sb store.Sandbox
	decodeJSON(t, resp, &sb)
	t.Cleanup(func() { doReq(t, ts, "DELETE", "/sandboxes/"+sb.ID, nil) })

	if !strings.HasPrefix(sb.Name, "sandbox-") {
		t.Errorf("auto name: %q, want sandbox-* prefix", sb.Name)
	}
}

func TestCreateWithTemplateStillWorks(t *testing.T) {
	_, ts := setup(t)
	name := uniqueName(t, "tmplworks")

	// Existing template-based flow should be unaffected
	_, sb := createTemplateAndSandbox(t, ts, name, nil)
	if sb.Name != name || sb.Status != "running" || sb.TemplateID == "" {
		t.Errorf("template sandbox: name=%q status=%q tmpl=%q", sb.Name, sb.Status, sb.TemplateID)
	}

	resp := doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/exec", map[string]any{
		"cmd": []string{"echo", "template works"},
	})
	var result engine.ExecResult
	decodeJSON(t, resp, &result)
	if strings.TrimSpace(result.Stdout) != "template works" {
		t.Errorf("exec: %q", result.Stdout)
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
	volName := uniqueName(t, "crud")

	// Create
	resp := doReq(t, ts, "POST", "/volumes", map[string]any{"name": volName})
	if resp.StatusCode != 201 {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	var vol store.Volume
	decodeJSON(t, resp, &vol)
	if vol.Name != volName {
		t.Fatalf("expected %s, got %s", volName, vol.Name)
	}

	// List
	resp = doReq(t, ts, "GET", "/volumes", nil)
	var list []store.Volume
	decodeJSON(t, resp, &list)
	if len(list) != 1 {
		t.Fatalf("expected 1 volume, got %d", len(list))
	}

	// Get
	resp = doReq(t, ts, "GET", "/volumes/"+volName, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Delete
	resp = doReq(t, ts, "DELETE", "/volumes/"+volName, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Verify gone
	resp = doReq(t, ts, "GET", "/volumes/"+volName, nil)
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestVolumeDeleteConflict(t *testing.T) {
	_, ts := setup(t)

	volName := uniqueName(t, "busy")
	sbName := uniqueName(t, "volsb")
	cleanupDockerVolume(t, volName)

	// Create volume in store
	doReq(t, ts, "POST", "/volumes", map[string]any{"name": volName})

	// Create sandbox that mounts the volume
	_, sb := createTemplateAndSandbox(t, ts, sbName, []map[string]any{
		{"name": volName, "target": "/data"},
	})

	// Delete volume — should fail with 409 while sandbox is attached
	resp := doReq(t, ts, "DELETE", "/volumes/"+volName, nil)
	if resp.StatusCode != 409 {
		t.Fatalf("expected 409, got %d", resp.StatusCode)
	}

	// Destroy sandbox, then delete volume should succeed
	doReq(t, ts, "DELETE", "/sandboxes/"+sb.ID, nil)
	resp = doReq(t, ts, "DELETE", "/volumes/"+volName, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestSandboxWithTemplateMounts(t *testing.T) {
	_, ts := setup(t)
	sbName := uniqueName(t, "tmplmnt")
	sharedVol := uniqueName(t, "shared")
	autoVol := "bhatti-" + sbName + "-workspace"
	cleanupDockerVolume(t, sharedVol)
	cleanupDockerVolume(t, autoVol)

	// Create template with default mounts
	resp := doReq(t, ts, "POST", "/templates", map[string]any{
		"name":  "dev-template",
		"image": "alpine:latest",
		"mounts": []map[string]any{
			{"volume_name": sharedVol, "target": "/data", "auto_create": true},
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
		"name":        sbName,
	})
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d: %s", resp.StatusCode, body)
	}
	var sb store.Sandbox
	decodeJSON(t, resp, &sb)
	t.Cleanup(func() { doReq(t, ts, "DELETE", "/sandboxes/"+sb.ID, nil) })

	// Verify auto-created volumes exist in store
	resp = doReq(t, ts, "GET", "/volumes", nil)
	var vols []store.Volume
	decodeJSON(t, resp, &vols)
	volNames := map[string]bool{}
	for _, v := range vols {
		volNames[v.Name] = true
	}
	if !volNames[sharedVol] {
		t.Fatalf("expected %s volume to be auto-created, got %v", sharedVol, volNames)
	}
	if !volNames[autoVol] {
		t.Fatalf("expected %s volume, got %v", autoVol, volNames)
	}

	// Verify mounts actually work inside the container
	resp = doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/exec", map[string]any{
		"cmd": []string{"sh", "-c", "echo tmpl-mount > /data/test.txt && cat /data/test.txt"},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("exec on /data: expected 200, got %d", resp.StatusCode)
	}
	var dataResult engine.ExecResult
	decodeJSON(t, resp, &dataResult)
	if dataResult.ExitCode != 0 {
		t.Fatalf("/data mount write/read failed: exit=%d stderr=%s", dataResult.ExitCode, dataResult.Stderr)
	}
	if strings.TrimSpace(dataResult.Stdout) != "tmpl-mount" {
		t.Fatalf("expected 'tmpl-mount', got %q", dataResult.Stdout)
	}

	resp = doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/exec", map[string]any{
		"cmd": []string{"sh", "-c", "echo ws-mount > /workspace/test.txt && cat /workspace/test.txt"},
	})
	var wsResult engine.ExecResult
	decodeJSON(t, resp, &wsResult)
	if wsResult.ExitCode != 0 {
		t.Fatalf("/workspace mount write/read failed: exit=%d stderr=%s", wsResult.ExitCode, wsResult.Stderr)
	}
	if strings.TrimSpace(wsResult.Stdout) != "ws-mount" {
		t.Fatalf("expected 'ws-mount', got %q", wsResult.Stdout)
	}
}

func TestSandboxWithExistingVolume(t *testing.T) {
	_, ts := setup(t)

	volName := uniqueName(t, "existing")
	sbName1 := uniqueName(t, "volsb1")
	sbName2 := uniqueName(t, "volsb2")

	// Create a real Docker volume
	if out, err := exec.Command("docker", "volume", "create", volName).CombinedOutput(); err != nil {
		t.Fatalf("docker volume create failed: %v\n%s", err, out)
	}
	cleanupDockerVolume(t, volName)

	// Track it in bhatti store
	resp := doReq(t, ts, "POST", "/volumes", map[string]any{"name": volName})
	if resp.StatusCode != 201 {
		t.Fatalf("store volume: expected 201, got %d", resp.StatusCode)
	}

	// Create template
	resp = doReq(t, ts, "POST", "/templates", map[string]any{
		"name":  "alpine",
		"image": "alpine:latest",
	})
	var tmpl store.Template
	decodeJSON(t, resp, &tmpl)

	// --- First sandbox: write data to volume ---
	resp = doReq(t, ts, "POST", "/sandboxes", map[string]any{
		"template_id": tmpl.ID,
		"name":        sbName1,
		"volumes":     []map[string]any{{"name": volName, "target": "/data"}},
	})
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create sb1: expected 201, got %d: %s", resp.StatusCode, body)
	}
	var sb1 store.Sandbox
	decodeJSON(t, resp, &sb1)

	resp = doReq(t, ts, "POST", "/sandboxes/"+sb1.ID+"/exec", map[string]any{
		"cmd": []string{"sh", "-c", "echo persistent-data > /data/test.txt"},
	})
	var writeResult engine.ExecResult
	decodeJSON(t, resp, &writeResult)
	if writeResult.ExitCode != 0 {
		t.Fatalf("write to volume failed: exit=%d stderr=%s", writeResult.ExitCode, writeResult.Stderr)
	}

	// Verify data exists in first sandbox
	resp = doReq(t, ts, "POST", "/sandboxes/"+sb1.ID+"/exec", map[string]any{
		"cmd": []string{"cat", "/data/test.txt"},
	})
	var readResult engine.ExecResult
	decodeJSON(t, resp, &readResult)
	if strings.TrimSpace(readResult.Stdout) != "persistent-data" {
		t.Fatalf("read from volume: expected 'persistent-data', got %q", readResult.Stdout)
	}

	// Destroy first sandbox
	resp = doReq(t, ts, "DELETE", "/sandboxes/"+sb1.ID, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("destroy sb1: expected 200, got %d", resp.StatusCode)
	}

	// --- Second sandbox: verify data persisted ---
	resp = doReq(t, ts, "POST", "/sandboxes", map[string]any{
		"template_id": tmpl.ID,
		"name":        sbName2,
		"volumes":     []map[string]any{{"name": volName, "target": "/data"}},
	})
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create sb2: expected 201, got %d: %s", resp.StatusCode, body)
	}
	var sb2 store.Sandbox
	decodeJSON(t, resp, &sb2)
	t.Cleanup(func() { doReq(t, ts, "DELETE", "/sandboxes/"+sb2.ID, nil) })

	resp = doReq(t, ts, "POST", "/sandboxes/"+sb2.ID+"/exec", map[string]any{
		"cmd": []string{"cat", "/data/test.txt"},
	})
	var persistResult engine.ExecResult
	decodeJSON(t, resp, &persistResult)
	if strings.TrimSpace(persistResult.Stdout) != "persistent-data" {
		t.Fatalf("volume data did not persist: expected 'persistent-data', got %q", persistResult.Stdout)
	}
}

func TestSandboxVolumeReadOnly(t *testing.T) {
	_, ts := setup(t)

	volName := uniqueName(t, "ro")
	sbName := uniqueName(t, "rosb")
	cleanupDockerVolume(t, volName)

	// Create Docker volume
	if out, err := exec.Command("docker", "volume", "create", volName).CombinedOutput(); err != nil {
		t.Fatalf("docker volume create: %v\n%s", err, out)
	}

	_, sb := createTemplateAndSandbox(t, ts, sbName, []map[string]any{
		{"name": volName, "target": "/data", "readonly": true},
	})

	// Writing to a readonly mount should fail
	resp := doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/exec", map[string]any{
		"cmd": []string{"sh", "-c", "echo test > /data/file.txt"},
	})
	var result engine.ExecResult
	decodeJSON(t, resp, &result)
	if result.ExitCode == 0 {
		t.Fatal("expected write to readonly volume to fail, but it succeeded")
	}
}

func TestVolumeValidation(t *testing.T) {
	_, ts := setup(t)

	resp := doReq(t, ts, "POST", "/volumes", map[string]any{})
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestPortEndpoints(t *testing.T) {
	_, ts := setup(t)

	// Global ports — should be empty
	resp := doReq(t, ts, "GET", "/ports", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var ports []portInfo
	decodeJSON(t, resp, &ports)
	if len(ports) != 0 {
		t.Fatalf("expected 0 ports, got %d", len(ports))
	}

	// Create sandbox
	sbName := uniqueName(t, "ports")
	_, sb := createTemplateAndSandbox(t, ts, sbName, nil)

	// alpine with sleep infinity has no listening ports
	resp = doReq(t, ts, "GET", "/sandboxes/"+sb.ID+"/ports", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	decodeJSON(t, resp, &ports)
	if len(ports) != 0 {
		t.Fatalf("expected 0 ports, got %d", len(ports))
	}
}

func TestSecretValidation(t *testing.T) {
	_, ts := setup(t)

	resp := doReq(t, ts, "POST", "/secrets", map[string]any{
		"name": "test",
	})
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

// --- Part 21: WebSocket Auth Tests ---

func TestWSAuthRequired(t *testing.T) {
	_, ts := setup(t)

	// Create sandbox to have a valid ID
	name := uniqueName(t, "wsauth")
	_, sb := createTemplateAndSandbox(t, ts, name, nil)

	// Connect to WS without any token — should get 401, not upgrade
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/sandboxes/" + sb.ID + "/ws"
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		t.Fatal("expected WS dial to fail without auth, but it succeeded")
	}
	if resp != nil && resp.StatusCode != 401 {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestWSAuthQueryParam(t *testing.T) {
	_, ts := setup(t)

	name := uniqueName(t, "wsqp")
	_, sb := createTemplateAndSandbox(t, ts, name, nil)

	// Connect with correct token in query param
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/sandboxes/" + sb.ID + "/ws?token=test-token"
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("expected WS dial with query param token to succeed: %v", err)
	}
	ws.Close()
}

func TestWSAuthBearerHeader(t *testing.T) {
	_, ts := setup(t)

	name := uniqueName(t, "wshdr")
	_, sb := createTemplateAndSandbox(t, ts, name, nil)

	// Connect with correct token in Authorization header
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/sandboxes/" + sb.ID + "/ws"
	header := http.Header{}
	header.Set("Authorization", "Bearer test-token")
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("expected WS dial with bearer header to succeed: %v", err)
	}
	ws.Close()
}

func TestWSAuthWrongToken(t *testing.T) {
	_, ts := setup(t)

	name := uniqueName(t, "wsbad")
	_, sb := createTemplateAndSandbox(t, ts, name, nil)

	// Connect with wrong token
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/sandboxes/" + sb.ID + "/ws?token=wrong-token"
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		t.Fatal("expected WS dial with wrong token to fail")
	}
	if resp != nil && resp.StatusCode != 401 {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}