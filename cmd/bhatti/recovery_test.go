package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/store"
)

// mockVMStateProvider records RestoreVM calls for testing.
type mockVMStateProvider struct {
	restored []restoredVM
}

type restoredVM struct {
	ID, Name, Status string
	State            map[string]interface{}
}

func (m *mockVMStateProvider) VMState(id string) map[string]interface{} { return nil }
func (m *mockVMStateProvider) RestoreVM(id, name, status string, state map[string]interface{}) {
	m.restored = append(m.restored, restoredVM{id, name, status, state})
}

func setupRecoveryTest(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, dir
}

func createTestSandbox(t *testing.T, st *store.Store, id, name, engineID, status string) {
	t.Helper()
	st.CreateSandbox(store.Sandbox{
		ID: id, Name: name, EngineID: engineID,
		Status: status, EngineMeta: json.RawMessage("{}"),
		CreatedAt: time.Now(),
	})
}

func saveTestFCState(t *testing.T, st *store.Store, id string, snapMemPath string) {
	t.Helper()
	st.SaveFirecrackerState(id, store.FirecrackerState{
		RootfsPath:  "/var/lib/bhatti/sandboxes/test/rootfs.ext4",
		SnapMemPath: snapMemPath,
		SnapVMPath:  "/var/lib/bhatti/sandboxes/test/vm.snap",
		VsockCID:    10,
		TapDevice:   "tap12345678",
		GuestIP:     "192.168.137.2",
		GuestMAC:    "02:ab:cd:ef:00:01",
		VcpuCount:   1,
		MemSizeMib:  512,
		SocketPath:  "/var/lib/bhatti/sandboxes/test/firecracker.sock",
		VsockPath:   "/var/lib/bhatti/sandboxes/test/vsock.sock",
	})
}

func TestRecoverStoppedWithSnapshot(t *testing.T) {
	st, dir := setupRecoveryTest(t)
	mock := &mockVMStateProvider{}

	// Create a stopped sandbox with a snapshot file that exists
	createTestSandbox(t, st, "sb1", "stopped-vm", "eng1", "stopped")
	snapPath := filepath.Join(dir, "mem.snap")
	os.WriteFile(snapPath, []byte("fake"), 0644)
	saveTestFCState(t, st, "sb1", snapPath)

	recoverVMs(st, mock)

	if len(mock.restored) != 1 {
		t.Fatalf("expected 1 restored, got %d", len(mock.restored))
	}
	r := mock.restored[0]
	if r.ID != "eng1" || r.Name != "stopped-vm" || r.Status != "stopped" {
		t.Errorf("restored: %+v", r)
	}
	// Verify state map has correct values
	if r.State["guest_ip"] != "192.168.137.2" {
		t.Errorf("guest_ip: %v", r.State["guest_ip"])
	}
}

func TestRecoverStoppedMissingSnapshot(t *testing.T) {
	st, _ := setupRecoveryTest(t)
	mock := &mockVMStateProvider{}

	// Stopped sandbox but snapshot file doesn't exist
	createTestSandbox(t, st, "sb2", "missing-snap", "eng2", "stopped")
	saveTestFCState(t, st, "sb2", "/nonexistent/mem.snap")

	recoverVMs(st, mock)

	// Should NOT be restored
	if len(mock.restored) != 0 {
		t.Fatalf("expected 0 restored, got %d", len(mock.restored))
	}
	// Status should be updated to unknown
	sb, _ := st.GetSandbox("sb2")
	if sb.Status != "unknown" {
		t.Errorf("status: %q, want 'unknown'", sb.Status)
	}
}

func TestRecoverRunningWithSnapshot(t *testing.T) {
	st, dir := setupRecoveryTest(t)
	mock := &mockVMStateProvider{}

	// Was "running" when daemon died — has a snapshot from thermal cold
	createTestSandbox(t, st, "sb3", "was-running", "eng3", "running")
	snapPath := filepath.Join(dir, "mem.snap")
	os.WriteFile(snapPath, []byte("fake"), 0644)
	saveTestFCState(t, st, "sb3", snapPath)

	recoverVMs(st, mock)

	if len(mock.restored) != 1 {
		t.Fatalf("expected 1 restored, got %d", len(mock.restored))
	}
	// Should be restored as "stopped" (not "running" — FC process is dead)
	if mock.restored[0].Status != "stopped" {
		t.Errorf("status: %q, want 'stopped'", mock.restored[0].Status)
	}
	// Store should also reflect stopped
	sb, _ := st.GetSandbox("sb3")
	if sb.Status != "stopped" {
		t.Errorf("store status: %q, want 'stopped'", sb.Status)
	}
}

func TestRecoverRunningNoSnapshot(t *testing.T) {
	st, _ := setupRecoveryTest(t)
	mock := &mockVMStateProvider{}

	// Was "running" with no snapshot — unrecoverable
	createTestSandbox(t, st, "sb4", "no-snap", "eng4", "running")
	saveTestFCState(t, st, "sb4", "") // empty snap path

	recoverVMs(st, mock)

	if len(mock.restored) != 0 {
		t.Fatalf("expected 0 restored, got %d", len(mock.restored))
	}
	sb, _ := st.GetSandbox("sb4")
	if sb.Status != "unknown" {
		t.Errorf("status: %q, want 'unknown'", sb.Status)
	}
}

func TestRecoverSkipsNonFirecracker(t *testing.T) {
	st, _ := setupRecoveryTest(t)
	mock := &mockVMStateProvider{}

	// Docker sandbox — no FC state saved
	createTestSandbox(t, st, "sb5", "docker-vm", "dock1", "running")

	recoverVMs(st, mock)

	// Should not be restored (no FC state)
	if len(mock.restored) != 0 {
		t.Fatalf("expected 0 restored, got %d", len(mock.restored))
	}
}

func TestRecoverSkipsDestroyed(t *testing.T) {
	st, dir := setupRecoveryTest(t)
	mock := &mockVMStateProvider{}

	// Destroyed sandbox — should be skipped entirely
	createTestSandbox(t, st, "sb6", "dead-vm", "eng6", "destroyed")
	snapPath := filepath.Join(dir, "mem.snap")
	os.WriteFile(snapPath, []byte("fake"), 0644)
	saveTestFCState(t, st, "sb6", snapPath)

	recoverVMs(st, mock)

	if len(mock.restored) != 0 {
		t.Fatalf("expected 0 restored for destroyed sandbox, got %d", len(mock.restored))
	}
}

func TestRecoverMultipleSandboxes(t *testing.T) {
	st, dir := setupRecoveryTest(t)
	mock := &mockVMStateProvider{}

	// Mix of recoverable and non-recoverable
	snap1 := filepath.Join(dir, "snap1.mem")
	snap2 := filepath.Join(dir, "snap2.mem")
	os.WriteFile(snap1, []byte("fake"), 0644)
	os.WriteFile(snap2, []byte("fake"), 0644)

	createTestSandbox(t, st, "sb-a", "vm-a", "eng-a", "stopped")
	saveTestFCState(t, st, "sb-a", snap1)

	createTestSandbox(t, st, "sb-b", "vm-b", "eng-b", "running")
	saveTestFCState(t, st, "sb-b", snap2)

	createTestSandbox(t, st, "sb-c", "vm-c", "eng-c", "running")
	saveTestFCState(t, st, "sb-c", "") // no snapshot

	createTestSandbox(t, st, "sb-d", "vm-d", "dock-d", "running")
	// no FC state

	recoverVMs(st, mock)

	// sb-a (stopped + snap) and sb-b (running + snap) should be restored
	if len(mock.restored) != 2 {
		t.Fatalf("expected 2 restored, got %d", len(mock.restored))
	}

	// sb-c and sb-d should not be restored
	names := map[string]bool{}
	for _, r := range mock.restored {
		names[r.Name] = true
	}
	if !names["vm-a"] || !names["vm-b"] {
		t.Errorf("expected vm-a and vm-b restored, got %v", names)
	}
}

func TestRecoverStateTypeCoercion(t *testing.T) {
	st, dir := setupRecoveryTest(t)
	mock := &mockVMStateProvider{}

	createTestSandbox(t, st, "sb-tc", "type-test", "eng-tc", "stopped")
	snapPath := filepath.Join(dir, "mem.snap")
	os.WriteFile(snapPath, []byte("fake"), 0644)
	saveTestFCState(t, st, "sb-tc", snapPath)

	recoverVMs(st, mock)

	if len(mock.restored) != 1 {
		t.Fatalf("expected 1 restored, got %d", len(mock.restored))
	}

	// The state map values come from store.FirecrackerState fields.
	// VsockCID is int, VcpuCount is float64, MemSizeMib is int.
	// RestoreVM (with Part 41 helpers) should handle all of these.
	state := mock.restored[0].State
	if _, ok := state["vsock_cid"]; !ok {
		t.Error("vsock_cid missing from state")
	}
	if _, ok := state["vcpu_count"]; !ok {
		t.Error("vcpu_count missing from state")
	}
	if _, ok := state["guest_ip"]; !ok {
		t.Error("guest_ip missing from state")
	}
}
