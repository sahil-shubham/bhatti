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

	encrypted := []byte("fake-encrypted-data")

	if err := s.SetSecret("api-key", encrypted); err != nil {
		t.Fatal(err)
	}

	// Get value
	got, err := s.GetSecretValue("api-key")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(encrypted) {
		t.Fatalf("expected %q, got %q", encrypted, got)
	}

	// Get metadata (no value)
	meta, err := s.GetSecret("api-key")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Name != "api-key" {
		t.Fatalf("expected name 'api-key', got %q", meta.Name)
	}

	// Update
	updated := []byte("new-encrypted-data")
	if err := s.SetSecret("api-key", updated); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetSecretValue("api-key")
	if string(got) != string(updated) {
		t.Fatalf("after update: expected %q, got %q", updated, got)
	}

	// List (no values)
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

func TestVolumesCRUD(t *testing.T) {
	s := testStore(t)

	// Create
	if err := s.CreateVolume("my-vol"); err != nil {
		t.Fatal(err)
	}

	// Create is idempotent
	if err := s.CreateVolume("my-vol"); err != nil {
		t.Fatal(err)
	}

	// Get
	vol, err := s.GetVolume("my-vol")
	if err != nil {
		t.Fatal(err)
	}
	if vol.Name != "my-vol" {
		t.Fatalf("expected my-vol, got %s", vol.Name)
	}

	// List
	list, err := s.ListVolumes()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 volume, got %d", len(list))
	}

	// Delete
	if err := s.DeleteVolume("my-vol"); err != nil {
		t.Fatal(err)
	}
	list, _ = s.ListVolumes()
	if len(list) != 0 {
		t.Fatal("expected 0 volumes after delete")
	}

	// Delete non-existent
	if err := s.DeleteVolume("nope"); err == nil {
		t.Fatal("expected error deleting non-existent volume")
	}
}

func TestVolumeDeleteBlockedByAttachment(t *testing.T) {
	s := testStore(t)

	s.CreateVolume("shared-vol")

	// Create a sandbox so we can attach to it
	sb := Sandbox{
		ID:        "s-vol-test",
		Name:      "vol-sandbox",
		Status:    "running",
		CreatedAt: time.Now(),
	}
	s.CreateSandbox(sb)

	// Attach
	if err := s.AttachVolume("s-vol-test", "shared-vol", "/data", false); err != nil {
		t.Fatal(err)
	}

	// Delete should fail
	if err := s.DeleteVolume("shared-vol"); err == nil {
		t.Fatal("expected error deleting volume in use")
	}

	// Detach then delete
	s.DetachVolumes("s-vol-test")
	if err := s.DeleteVolume("shared-vol"); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxVolumes(t *testing.T) {
	s := testStore(t)

	sb := Sandbox{
		ID:        "s-sv-test",
		Name:      "sv-sandbox",
		Status:    "running",
		CreatedAt: time.Now(),
	}
	s.CreateSandbox(sb)

	// Attach volumes
	s.AttachVolume("s-sv-test", "vol-a", "/mnt/a", false)
	s.AttachVolume("s-sv-test", "vol-b", "/mnt/b", true)

	vols, err := s.GetSandboxVolumes("s-sv-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(vols) != 2 {
		t.Fatalf("expected 2 volumes, got %d", len(vols))
	}

	// Check readonly flag
	for _, v := range vols {
		if v.VolumeName == "vol-b" && !v.ReadOnly {
			t.Fatal("expected vol-b to be readonly")
		}
		if v.VolumeName == "vol-a" && v.ReadOnly {
			t.Fatal("expected vol-a to be read-write")
		}
	}

	// Detach
	s.DetachVolumes("s-sv-test")
	vols, _ = s.GetSandboxVolumes("s-sv-test")
	if len(vols) != 0 {
		t.Fatal("expected 0 volumes after detach")
	}
}

func TestTemplateMounts(t *testing.T) {
	s := testStore(t)

	tmpl := Template{
		ID:       "t-mounts",
		Name:     "with-mounts",
		Engine:   "docker",
		Image:    "ubuntu:22.04",
		CPUs:     1,
		MemoryMB: 512,
		Mounts: []TemplateMountSpec{
			{VolumeName: "shared-data", Target: "/data", ReadOnly: false, AutoCreate: true},
			{Target: "/workspace", AutoCreate: true}, // VolumeName empty = auto-generate
		},
		CreatedAt: time.Now().Truncate(time.Second),
	}

	if err := s.CreateTemplate(tmpl); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetTemplate("t-mounts")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Mounts) != 2 {
		t.Fatalf("expected 2 mounts, got %d", len(got.Mounts))
	}
	if got.Mounts[0].VolumeName != "shared-data" {
		t.Fatalf("expected shared-data, got %s", got.Mounts[0].VolumeName)
	}
	if got.Mounts[0].Target != "/data" {
		t.Fatalf("expected /data, got %s", got.Mounts[0].Target)
	}
	if got.Mounts[1].Target != "/workspace" {
		t.Fatalf("expected /workspace, got %s", got.Mounts[1].Target)
	}
	if !got.Mounts[1].AutoCreate {
		t.Fatal("expected auto_create to be true")
	}
}

func TestEnsureKeypair(t *testing.T) {
	// Import from parent package — tested separately
	_ = os.TempDir()
}

// --- Firecracker State Persistence ---

func TestFirecrackerStateRoundTrip(t *testing.T) {
	s := testStore(t)
	defer s.Close()

	// Create a sandbox first (SaveFirecrackerState updates by sandbox ID)
	sb := Sandbox{
		ID: "sb-fc-1", Name: "fc-test", EngineID: "eng-1",
		Status: "running", EngineMeta: json.RawMessage("{}"),
		CreatedAt: time.Now(),
	}
	if err := s.CreateSandbox(sb); err != nil {
		t.Fatal(err)
	}

	// Save FC state
	state := FirecrackerState{
		RootfsPath:  "/var/lib/bhatti/sandboxes/abc/rootfs.ext4",
		SnapMemPath: "/var/lib/bhatti/sandboxes/abc/mem.snap",
		SnapVMPath:  "/var/lib/bhatti/sandboxes/abc/vm.snap",
		VsockCID:    42,
		TapDevice:   "tap12345678",
		GuestIP:     "192.168.137.5",
		GuestMAC:    "02:ab:cd:ef:00:01",
		VcpuCount:   2,
		MemSizeMib:  1024,
		SocketPath:  "/var/lib/bhatti/sandboxes/abc/firecracker.sock",
		VsockPath:   "/var/lib/bhatti/sandboxes/abc/vsock.sock",
	}
	if err := s.SaveFirecrackerState("sb-fc-1", state); err != nil {
		t.Fatal(err)
	}

	// Load and verify
	got, err := s.LoadFirecrackerState("sb-fc-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.RootfsPath != state.RootfsPath {
		t.Errorf("RootfsPath: %q, want %q", got.RootfsPath, state.RootfsPath)
	}
	if got.SnapMemPath != state.SnapMemPath {
		t.Errorf("SnapMemPath: %q, want %q", got.SnapMemPath, state.SnapMemPath)
	}
	if got.SnapVMPath != state.SnapVMPath {
		t.Errorf("SnapVMPath: %q, want %q", got.SnapVMPath, state.SnapVMPath)
	}
	if got.VsockCID != state.VsockCID {
		t.Errorf("VsockCID: %d, want %d", got.VsockCID, state.VsockCID)
	}
	if got.TapDevice != state.TapDevice {
		t.Errorf("TapDevice: %q, want %q", got.TapDevice, state.TapDevice)
	}
	if got.GuestIP != state.GuestIP {
		t.Errorf("GuestIP: %q, want %q", got.GuestIP, state.GuestIP)
	}
	if got.GuestMAC != state.GuestMAC {
		t.Errorf("GuestMAC: %q, want %q", got.GuestMAC, state.GuestMAC)
	}
	if got.VcpuCount != state.VcpuCount {
		t.Errorf("VcpuCount: %v, want %v", got.VcpuCount, state.VcpuCount)
	}
	if got.MemSizeMib != state.MemSizeMib {
		t.Errorf("MemSizeMib: %d, want %d", got.MemSizeMib, state.MemSizeMib)
	}
	if got.SocketPath != state.SocketPath {
		t.Errorf("SocketPath: %q, want %q", got.SocketPath, state.SocketPath)
	}
	if got.VsockPath != state.VsockPath {
		t.Errorf("VsockPath: %q, want %q", got.VsockPath, state.VsockPath)
	}
}

func TestFirecrackerStateUpdate(t *testing.T) {
	s := testStore(t)
	defer s.Close()

	sb := Sandbox{
		ID: "sb-fc-2", Name: "fc-update", EngineID: "eng-2",
		Status: "running", EngineMeta: json.RawMessage("{}"),
		CreatedAt: time.Now(),
	}
	s.CreateSandbox(sb)

	// Save initial state
	s.SaveFirecrackerState("sb-fc-2", FirecrackerState{
		RootfsPath: "/old/rootfs.ext4",
		GuestIP:    "192.168.137.2",
		VsockCID:   10,
	})

	// Update with new state
	s.SaveFirecrackerState("sb-fc-2", FirecrackerState{
		RootfsPath:  "/new/rootfs.ext4",
		SnapMemPath: "/new/mem.snap",
		GuestIP:     "192.168.137.3",
		VsockCID:    20,
	})

	got, _ := s.LoadFirecrackerState("sb-fc-2")
	if got.RootfsPath != "/new/rootfs.ext4" {
		t.Errorf("RootfsPath not updated: %q", got.RootfsPath)
	}
	if got.SnapMemPath != "/new/mem.snap" {
		t.Errorf("SnapMemPath not updated: %q", got.SnapMemPath)
	}
	if got.GuestIP != "192.168.137.3" {
		t.Errorf("GuestIP not updated: %q", got.GuestIP)
	}
	if got.VsockCID != 20 {
		t.Errorf("VsockCID not updated: %d", got.VsockCID)
	}
}

func TestFirecrackerStateDefaults(t *testing.T) {
	s := testStore(t)
	defer s.Close()

	// Non-FC sandbox should return zero-value state
	sb := Sandbox{
		ID: "sb-docker", Name: "docker-test", EngineID: "dock-1",
		Status: "running", EngineMeta: json.RawMessage("{}"),
		CreatedAt: time.Now(),
	}
	s.CreateSandbox(sb)

	got, err := s.LoadFirecrackerState("sb-docker")
	if err != nil {
		t.Fatal(err)
	}
	if got.RootfsPath != "" || got.GuestIP != "" || got.VsockCID != 0 {
		t.Errorf("expected zero values for non-FC sandbox, got: %+v", got)
	}
}
