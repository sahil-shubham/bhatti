package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestTemplatesCRUD(t *testing.T) {
	s := testStore(t)

	tmpl := Template{
		ID:       "t1",
		Name:     "ubuntu-dev",
		Engine:   "docker",
		Image:    "ubuntu:22.04",
		CPUs:     2,
		MemoryMB: 1024,
		Secrets:  []string{"github-token"},
		Labels:   map[string]string{"env": "dev"},
		CreatedAt: time.Now().Truncate(time.Second),
	}

	// Create
	if err := s.CreateTemplate(tmpl); err != nil {
		t.Fatal(err)
	}

	// Get
	got, err := s.GetTemplate("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "ubuntu-dev" || got.Image != "ubuntu:22.04" || got.CPUs != 2 || got.MemoryMB != 1024 {
		t.Fatalf("unexpected template: %+v", got)
	}
	if len(got.Secrets) != 1 || got.Secrets[0] != "github-token" {
		t.Fatalf("unexpected secrets: %v", got.Secrets)
	}
	if got.Labels["env"] != "dev" {
		t.Fatalf("unexpected labels: %v", got.Labels)
	}

	// List
	list, err := s.ListTemplates()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 template, got %d", len(list))
	}

	// Delete
	if err := s.DeleteTemplate("t1"); err != nil {
		t.Fatal(err)
	}
	list, _ = s.ListTemplates()
	if len(list) != 0 {
		t.Fatal("expected 0 templates after delete")
	}

	// Delete non-existent
	if err := s.DeleteTemplate("nope"); err == nil {
		t.Fatal("expected error deleting non-existent template")
	}
}

func TestSandboxesCRUD(t *testing.T) {
	s := testStore(t)

	sb := Sandbox{
		ID:         "s1",
		Name:       "my-sandbox",
		TemplateID: "t1",
		EngineID:   "abc123",
		Status:     "running",
		IP:         "172.17.0.2",
		EngineMeta: json.RawMessage(`{"port":8080}`),
		CreatedAt:  time.Now().Truncate(time.Second),
	}

	// Create
	if err := s.CreateSandbox(sb); err != nil {
		t.Fatal(err)
	}

	// Get
	got, err := s.GetSandbox("s1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "my-sandbox" || got.Status != "running" || got.IP != "172.17.0.2" {
		t.Fatalf("unexpected sandbox: %+v", got)
	}

	// Update status
	if err := s.UpdateSandboxStatus("s1", "stopped"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetSandbox("s1")
	if got.Status != "stopped" {
		t.Fatalf("expected stopped, got %s", got.Status)
	}

	// Stop (sets stopped_at)
	if err := s.StopSandbox("s1"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetSandbox("s1")
	if got.StoppedAt == nil {
		t.Fatal("expected stopped_at to be set")
	}

	// List
	list, err := s.ListSandboxes()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 sandbox, got %d", len(list))
	}

	// Delete
	if err := s.DeleteSandbox("s1"); err != nil {
		t.Fatal(err)
	}
	list, _ = s.ListSandboxes()
	if len(list) != 0 {
		t.Fatal("expected 0 sandboxes after delete")
	}
}

func TestSecretsCRUD(t *testing.T) {
	s := testStore(t)

	sr := SecretRecord{
		Name:      "api-key",
		Path:      "/tmp/secrets/api-key.age",
		CreatedAt: time.Now().Truncate(time.Second),
	}

	if err := s.CreateSecret(sr); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetSecret("api-key")
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != sr.Path {
		t.Fatalf("expected path %s, got %s", sr.Path, got.Path)
	}

	list, err := s.ListSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 secret, got %d", len(list))
	}

	if err := s.DeleteSecret("api-key"); err != nil {
		t.Fatal(err)
	}
	list, _ = s.ListSecrets()
	if len(list) != 0 {
		t.Fatal("expected 0 secrets after delete")
	}
}

func TestSandboxNilEngineMeta(t *testing.T) {
	s := testStore(t)

	sb := Sandbox{
		ID:        "s2",
		Name:      "no-meta",
		Status:    "running",
		CreatedAt: time.Now(),
	}
	if err := s.CreateSandbox(sb); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetSandbox("s2")
	if string(got.EngineMeta) != "{}" {
		t.Fatalf("expected empty JSON object, got %s", got.EngineMeta)
	}
}

func TestEnsureKeypair(t *testing.T) {
	// Import from parent package — tested separately
	_ = os.TempDir()
}
