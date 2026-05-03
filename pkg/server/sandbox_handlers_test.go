package server

import (
	"encoding/base64"
	"io"
	"strings"
	"testing"
	"time"

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
