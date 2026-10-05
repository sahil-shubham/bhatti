package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/gateway"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

// ==========================================================================
// keep_hot
// ==========================================================================

func TestKeepHotOnCreate(t *testing.T) {
	_, ts := setup(t)

	// Create with keep_hot: true
	resp := doReq(t, ts, "POST", "/sandboxes", map[string]any{
		"name":     uniqueName(t, "hot"),
		"keep_hot": true,
	})
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d: %s", resp.StatusCode, body)
	}
	var sb store.Sandbox
	decodeJSON(t, resp, &sb)
	t.Cleanup(func() { doReq(t, ts, "DELETE", "/sandboxes/"+sb.ID, nil) })

	if !sb.KeepHot {
		t.Fatal("expected keep_hot=true on created sandbox")
	}

	// GET should reflect it
	resp = doReq(t, ts, "GET", "/sandboxes/"+sb.ID, nil)
	var sb2 store.Sandbox
	decodeJSON(t, resp, &sb2)
	if !sb2.KeepHot {
		t.Fatal("expected keep_hot=true on GET")
	}
}

func TestKeepHotDefaultFalse(t *testing.T) {
	_, ts := setup(t)

	resp := doReq(t, ts, "POST", "/sandboxes", map[string]any{
		"name": uniqueName(t, "cold"),
	})
	var sb store.Sandbox
	decodeJSON(t, resp, &sb)
	t.Cleanup(func() { doReq(t, ts, "DELETE", "/sandboxes/"+sb.ID, nil) })

	if sb.KeepHot {
		t.Fatal("expected keep_hot=false by default")
	}
}

func TestKeepHotPatch(t *testing.T) {
	_, ts := setup(t)

	// Create without keep_hot
	sb := createSandbox(t, ts, uniqueName(t, "patch"))
	if sb.KeepHot {
		t.Fatal("precondition: keep_hot should be false")
	}

	// PATCH to enable
	resp := doReq(t, ts, "PATCH", "/sandboxes/"+sb.ID, map[string]any{
		"keep_hot": true,
	})
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PATCH enable: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var updated store.Sandbox
	decodeJSON(t, resp, &updated)
	if !updated.KeepHot {
		t.Fatal("expected keep_hot=true after PATCH")
	}

	// PATCH to disable
	resp = doReq(t, ts, "PATCH", "/sandboxes/"+sb.ID, map[string]any{
		"keep_hot": false,
	})
	if resp.StatusCode != 200 {
		t.Fatalf("PATCH disable: expected 200, got %d", resp.StatusCode)
	}
	var disabled store.Sandbox
	decodeJSON(t, resp, &disabled)
	if disabled.KeepHot {
		t.Fatal("expected keep_hot=false after disable PATCH")
	}
}

func TestKeepHotPatchNonexistent(t *testing.T) {
	_, ts := setup(t)
	resp := doReq(t, ts, "PATCH", "/sandboxes/nonexistent", map[string]any{
		"keep_hot": true,
	})
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// ==========================================================================
// rename (PATCH name)
// ==========================================================================

func TestPatchSandbox_Rename(t *testing.T) {
	_, ts := setup(t)
	oldName := uniqueName(t, "rn")
	sb := createSandbox(t, ts, oldName)

	newName := uniqueName(t, "rn-new")
	resp := doReq(t, ts, "PATCH", "/sandboxes/"+sb.ID, map[string]any{"name": newName})
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PATCH rename: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var updated store.Sandbox
	decodeJSON(t, resp, &updated)
	if updated.Name != newName {
		t.Fatalf("response name = %q, want %q", updated.Name, newName)
	}

	// GET by new name works
	resp = doReq(t, ts, "GET", "/sandboxes/"+newName, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET by new name: expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// GET by old name returns 404
	resp = doReq(t, ts, "GET", "/sandboxes/"+oldName, nil)
	if resp.StatusCode != 404 {
		t.Fatalf("GET by old name: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestPatchSandbox_RenameInvalidName(t *testing.T) {
	_, ts := setup(t)
	sb := createSandbox(t, ts, uniqueName(t, "badrn"))

	resp := doReq(t, ts, "PATCH", "/sandboxes/"+sb.ID, map[string]any{
		"name": "has spaces",
	})
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400 for invalid name, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestPatchSandbox_RenameConflict(t *testing.T) {
	_, ts := setup(t)
	a := createSandbox(t, ts, uniqueName(t, "a"))
	b := createSandbox(t, ts, uniqueName(t, "b"))

	// Rename a → b.Name should 409.
	resp := doReq(t, ts, "PATCH", "/sandboxes/"+a.ID, map[string]any{"name": b.Name})
	if resp.StatusCode != 409 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 409, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

func TestPatchSandbox_RenameSameName(t *testing.T) {
	_, ts := setup(t)
	sb := createSandbox(t, ts, uniqueName(t, "same"))

	// PATCH with the current name is a no-op: 200 and the row is unchanged.
	resp := doReq(t, ts, "PATCH", "/sandboxes/"+sb.ID, map[string]any{"name": sb.Name})
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var updated store.Sandbox
	decodeJSON(t, resp, &updated)
	if updated.Name != sb.Name {
		t.Fatalf("same-name PATCH changed name: %q -> %q", sb.Name, updated.Name)
	}
}

func TestPatchSandbox_RenameAndKeepHot(t *testing.T) {
	srv, ts := setup(t)
	sb := createSandbox(t, ts, uniqueName(t, "both"))

	newName := uniqueName(t, "both-new")
	resp := doReq(t, ts, "PATCH", "/sandboxes/"+sb.ID, map[string]any{
		"name":     newName,
		"keep_hot": true,
	})
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	var updated store.Sandbox
	decodeJSON(t, resp, &updated)
	if updated.Name != newName || !updated.KeepHot {
		t.Fatalf("both fields not applied: name=%q keep_hot=%v", updated.Name, updated.KeepHot)
	}

	// Persistence check
	stored, _ := srv.store.GetSandboxByID(sb.ID)
	if stored.Name != newName || !stored.KeepHot {
		t.Fatalf("both fields not persisted: name=%q keep_hot=%v", stored.Name, stored.KeepHot)
	}
}

// An engine that can't perform an operation in its current build (krucible
// without checkpoint support) must answer 501 with the engine's reason, not a
// generic 500 that sends the operator to the logs.
func TestStopNotSupportedIs501(t *testing.T) {
	srv, ts := setup(t)
	name := uniqueName(t, "nostop")
	createSandbox(t, ts, name)
	srv.engine.(*mockEngine).StopErr = fmt.Errorf("%w: no checkpoint support in this build", engine.ErrNotSupported)

	resp := doReq(t, ts, "POST", "/sandboxes/"+name+"/stop", nil)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 501 || !strings.Contains(string(body), "no checkpoint support") {
		t.Fatalf("stop: got %d %s, want 501 with the engine's reason", resp.StatusCode, body)
	}
}

// A durability warning is not a failed power-off: the client must see the
// stopped sandbox, and operators must receive a persisted unclean event.
func TestManualWarmStopUncleanResultPersistsEvent(t *testing.T) {
	srv, ts := setup(t)
	sb := createSandbox(t, ts, uniqueName(t, "unclean-manual"))
	eng := srv.engine.(*mockEngine)
	eng.mu.Lock()
	eng.thermal[sb.EngineID] = "warm"
	eng.mu.Unlock()
	srv.engine = &uncleanStopTestEngine{mockEngine: eng, reason: "guest sync timed out"}
	srv.events = NewEventRecorder(srv.store)

	resp := doReq(t, ts, "POST", "/sandboxes/"+sb.Name+"/stop", nil)
	var stopped store.Sandbox
	decodeJSON(t, resp, &stopped)
	if resp.StatusCode != 200 || stopped.Status != "stopped" || stopped.ID != sb.ID {
		t.Fatalf("manual stop replied %d %+v, want this sandbox stopped", resp.StatusCode, stopped)
	}
	stored, err := srv.store.GetSandboxByID(sb.ID)
	if err != nil || stored.Status != "stopped" || eng.ThermalState(sb.EngineID) != "cold" {
		t.Fatalf("manual power-off diverged from store: %+v, err=%v", stored, err)
	}
	srv.events.Close()
	srv.events = nil
	events, err := srv.store.QueryEvents(store.EventFilter{SandboxID: sb.ID})
	if err != nil {
		t.Fatal(err)
	}
	var gotUnclean, gotStopped bool
	for _, event := range events {
		switch event.Type {
		case "sandbox.unclean_stop":
			gotUnclean = event.UserID == sb.CreatedBy && event.Meta["reason"] == "guest sync timed out"
		case "sandbox.stopped":
			gotStopped = event.Meta["reason"] == "api"
		}
	}
	if !gotUnclean || !gotStopped {
		t.Fatalf("manual stop did not persist unclean reason and stopped event: %+v", events)
	}
}

// ==========================================================================
// state-sync after name-based stop/start (regression for #16)
// ==========================================================================
//
// resolveID in the CLI returns names as-is when the name lookup hits, so
// /sandboxes/<name>/start has the name in the URL. Before this fix,
// handleSandboxStart and handleSandbox GET passed that URL parameter to
// store methods that key on the primary key (UpdateSandboxStatus,
// UpdateSandboxEngine, GetSandboxByID). With a name as the
// key, the UPDATE matches zero rows, returns no error, and silently leaves
// the store out of sync with the engine — surfacing as `bhatti list`
// showing a running VM as stopped.

func TestStartByName_PersistsStatus(t *testing.T) {
	srv, ts := setup(t)
	name := uniqueName(t, "startname")
	sb := createSandbox(t, ts, name)

	// Stop via name (this path uses sb.ID internally and was always correct).
	resp := doReq(t, ts, "POST", "/sandboxes/"+name+"/stop", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("stop by name: expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	stored, _ := srv.store.GetSandboxByID(sb.ID)
	if stored.Status != "stopped" {
		t.Fatalf("after stop: store status = %q, want stopped", stored.Status)
	}

	// Start via name. Pre-fix: silent no-op on the persistence side.
	resp = doReq(t, ts, "POST", "/sandboxes/"+name+"/start", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("start by name: expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	stored, _ = srv.store.GetSandboxByID(sb.ID)
	if stored.Status != "running" {
		t.Fatalf("after start: store status = %q, want running (regression: #16)",
			stored.Status)
	}
}

func TestGetByName_PersistsStatusRefresh(t *testing.T) {
	srv, ts := setup(t)
	name := uniqueName(t, "getname")
	sb := createSandbox(t, ts, name)

	// Tamper with the store to simulate drift between store and engine
	// (e.g. a missed update due to an earlier crash).
	if err := srv.store.UpdateSandboxStatus(sb.ID, "stopped"); err != nil {
		t.Fatal(err)
	}

	// GET by name should refresh status from the engine AND persist it.
	resp := doReq(t, ts, "GET", "/sandboxes/"+name, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get by name: expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	stored, _ := srv.store.GetSandboxByID(sb.ID)
	if stored.Status != "running" {
		t.Fatalf("after GET-by-name refresh: store status = %q, want running (regression: #16)",
			stored.Status)
	}
}

func TestStartByName_RenameThenStartStop(t *testing.T) {
	// End-to-end repro of issue #16: rename, stop, start — all by the new
	// name — and verify the store stays in sync with the engine.
	srv, ts := setup(t)
	oldName := uniqueName(t, "old")
	sb := createSandbox(t, ts, oldName)

	newName := uniqueName(t, "new")
	resp := doReq(t, ts, "PATCH", "/sandboxes/"+sb.ID, map[string]any{"name": newName})
	resp.Body.Close()

	// Stop by new name
	resp = doReq(t, ts, "POST", "/sandboxes/"+newName+"/stop", nil)
	resp.Body.Close()
	stored, _ := srv.store.GetSandboxByID(sb.ID)
	if stored.Status != "stopped" {
		t.Fatalf("after stop: %q, want stopped", stored.Status)
	}

	// Start by new name
	resp = doReq(t, ts, "POST", "/sandboxes/"+newName+"/start", nil)
	resp.Body.Close()
	stored, _ = srv.store.GetSandboxByID(sb.ID)
	if stored.Status != "running" {
		t.Fatalf("after start: %q, want running", stored.Status)
	}
}

func TestKeepHotThermalCycleSkip(t *testing.T) {
	srv, ts := setup(t)

	// Create two sandboxes
	sb1 := createSandbox(t, ts, uniqueName(t, "hot"))
	sb2 := createSandbox(t, ts, uniqueName(t, "cold"))

	// Enable keep_hot on sb1
	resp := doReq(t, ts, "PATCH", "/sandboxes/"+sb1.ID, map[string]any{"keep_hot": true})
	resp.Body.Close()

	// Verify keep_hot is persisted in store
	sbStored, _ := srv.store.GetSandboxByID(sb1.ID)
	if !sbStored.KeepHot {
		t.Fatal("keep_hot not persisted in store")
	}

	// Verify sb2 is NOT keep_hot
	sb2Stored, _ := srv.store.GetSandboxByID(sb2.ID)
	if sb2Stored.KeepHot {
		t.Fatal("sb2 should not be keep_hot")
	}
}

// ==========================================================================
// B2: template + request-side secrets/files merge
// ==========================================================================
//
// Before B2 was fixed, the template-based creation branch in
// sandbox_handlers.go silently ignored req.Secrets and req.Files,
// only honouring tmpl.Secrets. These tests verify the union
// semantics: request adds to template defaults, and bogus values
// are rejected with 400 (no longer silently dropped).

// addTemplate inserts a template directly via the store. Used by tests
// that don't want to construct a full POST /admin/templates request.
func addTemplate(t *testing.T, srv *Server, name string, secrets []string) string {
	t.Helper()
	tmpl := store.Template{
		ID:        "tmpl_" + name,
		Name:      name,
		Engine:    "firecracker",
		Image:     "alpine",
		CPUs:      1,
		MemoryMB:  512,
		Secrets:   secrets,
		CreatedAt: time.Now(),
	}
	if err := srv.store.CreateTemplate(tmpl); err != nil {
		t.Fatalf("create template: %v", err)
	}
	return tmpl.ID
}

// addSecret stores a user secret using the server's encryption helper.
func addSecret(t *testing.T, srv *Server, userID, name, value string) {
	t.Helper()
	ct, err := srv.encryptSecret([]byte(value))
	if err != nil {
		t.Fatalf("encrypt %q: %v", name, err)
	}
	if err := srv.store.SetSecret(userID, name, ct); err != nil {
		t.Fatalf("set secret %q: %v", name, err)
	}
}

func TestB2_TemplateMergesRequestSecrets(t *testing.T) {
	srv, ts := setup(t)
	eng := srv.engine.(*mockEngine)

	// Template references DB_URL; request adds API_KEY (not in template).
	addSecret(t, srv, "usr_test", "DB_URL", "postgres://x")
	addSecret(t, srv, "usr_test", "API_KEY", "sk-abc")
	tmplID := addTemplate(t, srv, "merge", []string{"DB_URL"})

	resp := doReq(t, ts, "POST", "/sandboxes", map[string]any{
		"name":        uniqueName(t, "merge"),
		"template_id": tmplID,
		"secrets":     []string{"API_KEY"},
	})
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d: %s", resp.StatusCode, body)
	}
	var sb store.Sandbox
	decodeJSON(t, resp, &sb)
	t.Cleanup(func() { doReq(t, ts, "DELETE", "/sandboxes/"+sb.ID, nil) })

	// Spec passed to the engine should include BOTH secrets in env.
	if eng.LastCreateSpec.Env["DB_URL"] != "postgres://x" {
		t.Errorf("template secret DB_URL not in env: %v", eng.LastCreateSpec.Env)
	}
	if eng.LastCreateSpec.Env["API_KEY"] != "sk-abc" {
		t.Errorf("request secret API_KEY missing from env (B2 regression): %v", eng.LastCreateSpec.Env)
	}
}

func TestB2_TemplateRejectsMissingRequestSecret(t *testing.T) {
	srv, ts := setup(t)
	tmplID := addTemplate(t, srv, "strict", nil)

	resp := doReq(t, ts, "POST", "/sandboxes", map[string]any{
		"name":        uniqueName(t, "strict"),
		"template_id": tmplID,
		"secrets":     []string{"NOPE"},
	})
	defer resp.Body.Close()

	// Before B2 fix: req.Secrets silently dropped — returned 201.
	// After B2 fix: bogus secret name surfaces as 400.
	if resp.StatusCode != 400 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400 for missing secret, got %d: %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "NOPE") {
		t.Errorf("error body should mention the missing secret name, got: %s", body)
	}
}

func TestB2_TemplateProcessesRequestFiles(t *testing.T) {
	srv, ts := setup(t)
	eng := srv.engine.(*mockEngine)
	tmplID := addTemplate(t, srv, "files", nil)

	content := "hello-from-test"
	encoded := base64.StdEncoding.EncodeToString([]byte(content))

	resp := doReq(t, ts, "POST", "/sandboxes", map[string]any{
		"name":        uniqueName(t, "files"),
		"template_id": tmplID,
		"files": []map[string]any{
			{"guest_path": "/etc/cfg", "content": encoded, "mode": "0600"},
		},
	})
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d: %s", resp.StatusCode, body)
	}
	var sb store.Sandbox
	decodeJSON(t, resp, &sb)
	t.Cleanup(func() { doReq(t, ts, "DELETE", "/sandboxes/"+sb.ID, nil) })

	f, ok := eng.LastCreateSpec.Files["/etc/cfg"]
	if !ok {
		t.Fatalf("file /etc/cfg not in spec.Files (B2 regression): %v", eng.LastCreateSpec.Files)
	}
	if string(f.Content) != content {
		t.Errorf("file content = %q, want %q", f.Content, content)
	}
	if f.Mode != "0600" {
		t.Errorf("file mode = %q, want 0600", f.Mode)
	}
}

func TestB2_TemplateRejectsInvalidRequestFile(t *testing.T) {
	srv, ts := setup(t)
	tmplID := addTemplate(t, srv, "badfile", nil)

	// Empty guest_path is rejected by the file validator.
	resp := doReq(t, ts, "POST", "/sandboxes", map[string]any{
		"name":        uniqueName(t, "badfile"),
		"template_id": tmplID,
		"files": []map[string]any{
			{"guest_path": "", "content": "aGVsbG8="},
		},
	})
	defer resp.Body.Close()

	if resp.StatusCode != 400 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400 for empty guest_path, got %d: %s", resp.StatusCode, body)
	}
}

// ==========================================================================
// net_policy (per-sandbox egress rules)
// ==========================================================================

func TestCreateSandbox_NetPolicy(t *testing.T) {
	srv, ts := setup(t)
	eng := srv.engine.(*mockEngine)

	resp := doReq(t, ts, "POST", "/sandboxes", map[string]any{
		"name": uniqueName(t, "netpol"),
		"net_policy": map[string]any{
			"default":     "deny",
			"allow_hosts": []string{"api.openai.com", "*.github.com"},
			"allow_cidrs": []string{"1.1.1.1/32"},
		},
	})
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d: %s", resp.StatusCode, body)
	}
	var sb store.Sandbox
	decodeJSON(t, resp, &sb)
	t.Cleanup(func() { doReq(t, ts, "DELETE", "/sandboxes/"+sb.ID, nil).Body.Close() })

	np := eng.LastCreateSpec.NetPolicy
	if np == nil {
		t.Fatal("net_policy did not reach the engine spec")
	}
	if np.Default != "deny" {
		t.Errorf("spec default = %q, want deny", np.Default)
	}
	if len(np.AllowHosts) != 2 || np.AllowHosts[0] != "api.openai.com" {
		t.Errorf("spec allow_hosts = %v, want [api.openai.com *.github.com]", np.AllowHosts)
	}
	if len(np.AllowCIDRs) != 1 || np.AllowCIDRs[0] != "1.1.1.1/32" {
		t.Errorf("spec allow_cidrs = %v, want [1.1.1.1/32]", np.AllowCIDRs)
	}
}

// TestCreateSandbox_NetPolicyInvalid rejects an unparseable policy at the API
// (fail-fast 400) instead of deferring to a deep engine error.
func TestCreateSandbox_NetPolicyInvalid(t *testing.T) {
	_, ts := setup(t)
	resp := doReq(t, ts, "POST", "/sandboxes", map[string]any{
		"name": uniqueName(t, "badnetpol"),
		"net_policy": map[string]any{
			"default": "sometimes", // not public|deny
		},
	})
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400 for bad egress posture, got %d", resp.StatusCode)
	}
}

// TestCreateSandbox_NetPostureResolution pins explicit and implicit posture:
// a sibling-only opt-in creates a NIC but never grants public internet access.
func TestCreateSandbox_NetPostureResolution(t *testing.T) {
	cases := []struct {
		name          string
		serverDefault string
		policy        map[string]any // nil = no net_policy in the request
		wantStatus    int
		wantPosture   string
	}{
		{"unset default is none", "", nil, 201, "none"},
		{"configured default applies", "public", nil, 201, "public"},
		{"allow rules imply deny", "public", map[string]any{"allow_hosts": []string{"api.example.com"}}, 201, "deny"},
		{"sibling-only implies deny even with public server default", "public", map[string]any{"siblings": "allow"}, 201, "deny"},
		{"explicit beats default", "none", map[string]any{"default": "public"}, 201, "public"},
		{"none with allow rules rejected", "", map[string]any{"default": "none", "allow_hosts": []string{"x.com"}}, 400, ""},
		{"none with sibling access rejected", "", map[string]any{"default": "none", "siblings": "allow"}, 400, ""},
		{"invalid sibling posture rejected", "", map[string]any{"default": "deny", "siblings": "public"}, 400, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, ts := setup(t)
			srv.defaultEgress = tc.serverDefault
			eng := srv.engine.(*mockEngine)
			req := map[string]any{"name": uniqueName(t, "posture")}
			if tc.policy != nil {
				req["net_policy"] = tc.policy
			}
			resp := doReq(t, ts, "POST", "/sandboxes", req)
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tc.wantStatus, body)
			}
			if tc.wantStatus != 201 {
				return
			}
			if np := eng.LastCreateSpec.NetPolicy; np == nil || np.Default != tc.wantPosture {
				t.Fatalf("engine posture = %+v, want %q", np, tc.wantPosture)
			}
		})
	}
}

func TestCreateSandbox_SiblingPolicyPersistsThroughInspect(t *testing.T) {
	srv, ts := setup(t)
	eng := srv.engine.(*mockEngine)
	resp := doReq(t, ts, "POST", "/sandboxes", map[string]any{
		"name":       uniqueName(t, "siblings"),
		"net_policy": map[string]any{"siblings": "allow"},
	})
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create = %d: %s", resp.StatusCode, body)
	}
	var created store.Sandbox
	decodeJSON(t, resp, &created)
	t.Cleanup(func() { doReq(t, ts, "DELETE", "/sandboxes/"+created.ID, nil).Body.Close() })
	if p := eng.LastCreateSpec.NetPolicy; p == nil || p.Default != "deny" || p.Siblings != "allow" {
		t.Fatalf("engine policy = %+v", p)
	}
	inspect := doReq(t, ts, "GET", "/sandboxes/"+created.ID, nil)
	if inspect.StatusCode != 200 {
		t.Fatalf("inspect status = %d", inspect.StatusCode)
	}
	var sb store.Sandbox
	decodeJSON(t, inspect, &sb)
	var policy gateway.NetPolicyWire
	if err := json.Unmarshal(sb.NetPolicy, &policy); err != nil {
		t.Fatal(err)
	}
	if policy.Default != "deny" || policy.Siblings != "allow" {
		t.Fatalf("stored inspect policy = %+v", policy)
	}
}

// Journal repair on the first RO attach must hold the same per-volume gate as
// RW attach and backup cloning. A RW guest must not mount while e2fsck runs.
func TestReadOnlyAttachJournalRepairBlocksReadWriteAttach(t *testing.T) {
	srv, ts := setup(t)
	commands := t.TempDir()
	started := filepath.Join(commands, "repair-started")
	release := filepath.Join(commands, "repair-release")
	t.Setenv("BHATTI_RO_REPAIR_STARTED", started)
	t.Setenv("BHATTI_RO_REPAIR_RELEASE", release)
	t.Setenv("PATH", commands+string(os.PathListSeparator)+os.Getenv("PATH"))
	for name, script := range map[string]string{
		"tune2fs": "#!/bin/sh\nprintf 'Filesystem state: dirty\\n'\n",
		"e2fsck":  "#!/bin/sh\n: > \"$BHATTI_RO_REPAIR_STARTED\"\nwhile [ ! -e \"$BHATTI_RO_REPAIR_RELEASE\" ]; do /bin/sleep 0.01; done\n",
	} {
		if err := os.WriteFile(filepath.Join(commands, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	// Always release the waiting HTTP request if this test detects a failure.
	defer os.WriteFile(release, nil, 0600)

	volName := uniqueName(t, "repair")
	vol := store.PersistentVolume{
		ID: genID(), UserID: "usr_test", Name: volName, SizeMB: 64,
		Status: "ready", FilePath: filepath.Join(commands, "volume.ext4"),
		CreatedAt: time.Now(),
	}
	if err := srv.store.CreatePersistentVolume(vol); err != nil {
		t.Fatal(err)
	}
	sbName := uniqueName(t, "ro-repair")
	body, err := json.Marshal(map[string]any{
		"name": sbName,
		"persistent_volumes": []map[string]any{
			{"name": volName, "mount": "/workspace", "read_only": true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/sandboxes", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	type httpResult struct {
		status int
		err    error
	}
	roDone := make(chan httpResult, 1)
	go func() {
		resp, err := testHTTPClient.Do(req)
		if err != nil {
			roDone <- httpResult{err: err}
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		roDone <- httpResult{status: resp.StatusCode}
	}()

	waitUntil := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(waitUntil) {
			t.Fatal("RO attach never entered e2fsck on a dirty, detached volume")
		}
		time.Sleep(10 * time.Millisecond)
	}

	rwStarted := make(chan struct{})
	rwDone := make(chan error, 1)
	go func() {
		close(rwStarted)
		rwDone <- srv.store.AttachPersistentVolume("usr_test", volName, "competing-rw", "/data", false)
	}()
	<-rwStarted
	premature := false
	select {
	case <-rwDone:
		premature = true // a live RW mount overlapped e2fsck
	case <-time.After(250 * time.Millisecond):
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	var ro httpResult
	select {
	case ro = <-roDone:
	case <-time.After(3 * time.Second):
		t.Fatal("RO attach did not complete after releasing e2fsck")
	}
	var rwErr error
	if !premature {
		select {
		case rwErr = <-rwDone:
		case <-time.After(3 * time.Second):
			t.Fatal("RW attachment remained blocked after journal repair")
		}
	}
	if premature {
		t.Fatal("RW attachment completed while e2fsck was running")
	}
	if ro.err != nil {
		t.Fatal(ro.err)
	}
	if (ro.status == http.StatusCreated) == (rwErr == nil) {
		t.Fatalf("RO HTTP attach status=%d, RW attach error=%v: exactly one must win", ro.status, rwErr)
	}
	if ro.status != http.StatusCreated && ro.status != http.StatusConflict {
		t.Fatalf("RO HTTP attach returned unexpected status %d", ro.status)
	}
	updated, err := srv.store.GetPersistentVolume("usr_test", volName)
	if err != nil || len(updated.Attachments) != 1 {
		t.Fatalf("competing attaches left unexpected volume attachments: %+v, %v", updated, err)
	}
}
