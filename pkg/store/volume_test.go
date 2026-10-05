package store

import (
	"encoding/json"
	"testing"
	"time"
)

func TestPersistentVolumeCreateAndGet(t *testing.T) {
	s := testStore(t)
	createTestUser(t, s, "usr_a", "alice")
	v := createTestVolume(t, s, "usr_a", "workspace", 5120)

	got, err := s.GetPersistentVolume("usr_a", "workspace")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != v.ID || got.Name != "workspace" || got.SizeMB != 5120 || got.Status != "ready" {
		t.Fatalf("unexpected volume: %+v", got)
	}
	if len(got.Attachments) != 0 {
		t.Fatalf("expected 0 attachments, got %d", len(got.Attachments))
	}
}

func TestPersistentVolumeUserScoped(t *testing.T) {
	s := testStore(t)
	createTestUser(t, s, "usr_a", "alice")
	createTestUser(t, s, "usr_b", "bob")
	createTestVolume(t, s, "usr_a", "ws", 1024)

	// Bob can't see Alice's volume
	if _, err := s.GetPersistentVolume("usr_b", "ws"); err == nil {
		t.Fatal("expected error: bob should not see alice's volume")
	}

	// Bob can have his own with the same name
	createTestVolume(t, s, "usr_b", "ws", 2048)
	got, err := s.GetPersistentVolume("usr_b", "ws")
	if err != nil {
		t.Fatal(err)
	}
	if got.SizeMB != 2048 {
		t.Fatalf("expected bob's 2048MB volume, got %dMB", got.SizeMB)
	}

	// Alice can't delete Bob's volume
	if err := s.DeletePersistentVolume("usr_a", "ws"); err != nil {
		// Should delete alice's, not bob's
	}
	if _, err := s.GetPersistentVolume("usr_b", "ws"); err != nil {
		t.Fatal("bob's volume should still exist")
	}
}

func TestPersistentVolumeAttachDetach(t *testing.T) {
	s := testStore(t)
	createTestUser(t, s, "usr_a", "alice")
	createTestVolume(t, s, "usr_a", "ws", 1024)
	s.CreateSandbox(Sandbox{ID: "sb1", Name: "sb1", Status: "running", CreatedBy: "usr_a", CreatedAt: time.Now()})

	if err := s.AttachPersistentVolume("usr_a", "ws", "sb1", "/workspace", false); err != nil {
		t.Fatal(err)
	}

	got, _ := s.GetPersistentVolume("usr_a", "ws")
	if len(got.Attachments) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(got.Attachments))
	}
	if got.Attachments[0].SandboxID != "sb1" || got.Attachments[0].Mount != "/workspace" {
		t.Fatalf("unexpected attachment: %+v", got.Attachments[0])
	}

	// Detach
	s.DetachPersistentVolume("usr_a", "ws", "sb1")
	got, _ = s.GetPersistentVolume("usr_a", "ws")
	if len(got.Attachments) != 0 {
		t.Fatalf("expected 0 attachments after detach, got %d", len(got.Attachments))
	}
}

func TestPersistentVolumeDoubleRWAttachRejected(t *testing.T) {
	s := testStore(t)
	createTestUser(t, s, "usr_a", "alice")
	createTestVolume(t, s, "usr_a", "ws", 1024)
	s.CreateSandbox(Sandbox{ID: "sb1", Name: "sb1", Status: "running", CreatedBy: "usr_a", CreatedAt: time.Now()})
	s.CreateSandbox(Sandbox{ID: "sb2", Name: "sb2", Status: "running", CreatedBy: "usr_a", CreatedAt: time.Now()})

	if err := s.AttachPersistentVolume("usr_a", "ws", "sb1", "/ws", false); err != nil {
		t.Fatal(err)
	}
	if err := s.AttachPersistentVolume("usr_a", "ws", "sb2", "/ws", false); err == nil {
		t.Fatal("expected error: RW double attach should be rejected")
	}
}

func TestPersistentVolumeROMultiAttach(t *testing.T) {
	s := testStore(t)
	createTestUser(t, s, "usr_a", "alice")
	createTestVolume(t, s, "usr_a", "data", 1024)
	for i := 0; i < 3; i++ {
		id := "sb" + string(rune('1'+i))
		s.CreateSandbox(Sandbox{ID: id, Name: id, Status: "running", CreatedBy: "usr_a", CreatedAt: time.Now()})
	}

	for i := 0; i < 3; i++ {
		id := "sb" + string(rune('1'+i))
		if err := s.AttachPersistentVolume("usr_a", "data", id, "/data", true); err != nil {
			t.Fatalf("RO attach %d failed: %v", i, err)
		}
	}

	got, _ := s.GetPersistentVolume("usr_a", "data")
	if len(got.Attachments) != 3 {
		t.Fatalf("expected 3 RO attachments, got %d", len(got.Attachments))
	}
}

func TestPersistentVolumeRWBlocksRO(t *testing.T) {
	s := testStore(t)
	createTestUser(t, s, "usr_a", "alice")
	createTestVolume(t, s, "usr_a", "ws", 1024)
	s.CreateSandbox(Sandbox{ID: "sb1", Name: "sb1", Status: "running", CreatedBy: "usr_a", CreatedAt: time.Now()})
	s.CreateSandbox(Sandbox{ID: "sb2", Name: "sb2", Status: "running", CreatedBy: "usr_a", CreatedAt: time.Now()})

	s.AttachPersistentVolume("usr_a", "ws", "sb1", "/ws", false) // RW
	if err := s.AttachPersistentVolume("usr_a", "ws", "sb2", "/ws", true); err == nil {
		t.Fatal("expected error: RO attach should be blocked by existing RW")
	}
}

func TestPersistentVolumeROBlocksRW(t *testing.T) {
	s := testStore(t)
	createTestUser(t, s, "usr_a", "alice")
	createTestVolume(t, s, "usr_a", "ws", 1024)
	s.CreateSandbox(Sandbox{ID: "sb1", Name: "sb1", Status: "running", CreatedBy: "usr_a", CreatedAt: time.Now()})
	s.CreateSandbox(Sandbox{ID: "sb2", Name: "sb2", Status: "running", CreatedBy: "usr_a", CreatedAt: time.Now()})

	s.AttachPersistentVolume("usr_a", "ws", "sb1", "/ws", true) // RO
	if err := s.AttachPersistentVolume("usr_a", "ws", "sb2", "/ws", false); err == nil {
		t.Fatal("expected error: RW attach should be blocked by existing RO")
	}
}

func TestPersistentVolumeDeleteWhileAttached(t *testing.T) {
	s := testStore(t)
	createTestUser(t, s, "usr_a", "alice")
	createTestVolume(t, s, "usr_a", "ws", 1024)
	s.CreateSandbox(Sandbox{ID: "sb1", Name: "sb1", Status: "running", CreatedBy: "usr_a", CreatedAt: time.Now()})
	s.AttachPersistentVolume("usr_a", "ws", "sb1", "/ws", false)

	if err := s.DeletePersistentVolume("usr_a", "ws"); err == nil {
		t.Fatal("expected error: delete while attached should fail")
	}
}

func TestDetachAllPersistentVolumesForSandbox(t *testing.T) {
	s := testStore(t)
	createTestUser(t, s, "usr_a", "alice")
	createTestVolume(t, s, "usr_a", "vol1", 512)
	createTestVolume(t, s, "usr_a", "vol2", 512)
	createTestVolume(t, s, "usr_a", "vol3", 512)
	s.CreateSandbox(Sandbox{ID: "sb1", Name: "sb1", Status: "running", CreatedBy: "usr_a", CreatedAt: time.Now()})
	s.CreateSandbox(Sandbox{ID: "sb2", Name: "sb2", Status: "running", CreatedBy: "usr_a", CreatedAt: time.Now()})

	s.AttachPersistentVolume("usr_a", "vol1", "sb1", "/v1", false)
	s.AttachPersistentVolume("usr_a", "vol2", "sb1", "/v2", false)
	s.AttachPersistentVolume("usr_a", "vol3", "sb2", "/v3", false)

	s.DetachAllPersistentVolumesForSandbox("sb1")

	// sb1's volumes should be free
	got1, _ := s.GetPersistentVolume("usr_a", "vol1")
	got2, _ := s.GetPersistentVolume("usr_a", "vol2")
	got3, _ := s.GetPersistentVolume("usr_a", "vol3")
	if len(got1.Attachments) != 0 || len(got2.Attachments) != 0 {
		t.Fatal("sb1's volumes should be detached")
	}
	if len(got3.Attachments) != 1 {
		t.Fatal("sb2's volume should still be attached")
	}
}

func TestDetachOrphanedPersistentVolumes(t *testing.T) {
	s := testStore(t)
	createTestUser(t, s, "usr_a", "alice")
	createTestVolume(t, s, "usr_a", "ws", 1024)

	// Create a sandbox, attach, then delete the sandbox (simulating crash)
	s.CreateSandbox(Sandbox{ID: "sb1", Name: "sb1", Status: "running", CreatedBy: "usr_a", CreatedAt: time.Now()})
	s.AttachPersistentVolume("usr_a", "ws", "sb1", "/ws", false)
	s.DeleteSandboxByID("sb1") // sandbox row gone, attachment row remains

	// Also create a valid attachment
	s.CreateSandbox(Sandbox{ID: "sb2", Name: "sb2", Status: "running", CreatedBy: "usr_a", CreatedAt: time.Now()})
	createTestVolume(t, s, "usr_a", "ws2", 1024)
	s.AttachPersistentVolume("usr_a", "ws2", "sb2", "/ws2", false)

	n, err := s.DetachOrphanedPersistentVolumes()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 orphan detached, got %d", n)
	}

	// Valid attachment should survive
	got, _ := s.GetPersistentVolume("usr_a", "ws2")
	if len(got.Attachments) != 1 {
		t.Fatal("valid attachment should survive orphan cleanup")
	}
}

func TestPersistentVolumeCreatingStatusBlocksAttach(t *testing.T) {
	s := testStore(t)
	createTestUser(t, s, "usr_a", "alice")

	v := PersistentVolume{
		ID: "vol_creating", UserID: "usr_a", Name: "pending",
		SizeMB: 1024, FilePath: "/tmp/pending.ext4",
		Status: "creating", CreatedAt: time.Now(),
	}
	s.CreatePersistentVolume(v)

	s.CreateSandbox(Sandbox{ID: "sb1", Name: "sb1", Status: "running", CreatedBy: "usr_a", CreatedAt: time.Now()})
	if err := s.AttachPersistentVolume("usr_a", "pending", "sb1", "/ws", false); err == nil {
		t.Fatal("expected error: attaching to 'creating' volume should be blocked")
	}
}

func TestPersistentVolumeResizeAndQuota(t *testing.T) {
	s := testStore(t)
	createTestUser(t, s, "usr_a", "alice")
	createTestVolume(t, s, "usr_a", "ws", 1024)

	s.UpdatePersistentVolumeSize("usr_a", "ws", 2048)
	got, _ := s.GetPersistentVolume("usr_a", "ws")
	if got.SizeMB != 2048 {
		t.Fatalf("expected 2048, got %d", got.SizeMB)
	}

	used, _ := s.UserVolumeStorageUsed("usr_a")
	if used != 2048 {
		t.Fatalf("expected 2048 used, got %d", used)
	}
}

func TestPersistentVolumeUniqueConstraint(t *testing.T) {
	s := testStore(t)
	createTestUser(t, s, "usr_a", "alice")
	createTestVolume(t, s, "usr_a", "ws", 1024)

	dup := PersistentVolume{
		ID: "vol_dup", UserID: "usr_a", Name: "ws",
		SizeMB: 2048, FilePath: "/tmp/dup.ext4",
		Status: "ready", CreatedAt: time.Now(),
	}
	if err := s.CreatePersistentVolume(dup); err == nil {
		t.Fatal("expected UNIQUE constraint error")
	}
}

func TestVolumeAttachmentGateBlocksSameVolumeOnly(t *testing.T) {
	s := testStore(t)
	createTestUser(t, s, "usr_a", "alice")
	createTestVolume(t, s, "usr_a", "data", 1024)
	createTestVolume(t, s, "usr_a", "other", 1024)
	createTestSandbox(t, s, "usr_a", "sb1", "first")
	createTestSandbox(t, s, "usr_a", "sb2", "second")

	unlock := s.LockPersistentVolumeAttachments("usr_a", "data")
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()
	started := make(chan struct{})
	attached := make(chan error, 1)
	go func() {
		close(started)
		attached <- s.AttachPersistentVolume("usr_a", "data", "sb1", "/data", true)
	}()
	<-started
	select {
	case err := <-attached:
		t.Fatalf("attach entered locked volume before clone finished: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := s.AttachPersistentVolume("usr_a", "other", "sb2", "/other", true); err != nil {
		t.Fatalf("unrelated volume blocked by per-volume gate: %v", err)
	}
	unlock()
	locked = false
	select {
	case err := <-attached:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("attach did not resume after backup released volume gate")
	}
	got, err := s.GetPersistentVolume("usr_a", "data")
	if err != nil || len(got.Attachments) != 1 || got.Attachments[0].SandboxID != "sb1" {
		t.Fatalf("racing attachment lost after gate release: volume=%+v err=%v", got, err)
	}
}

// ==========================================================================
// Publish Rules
// ==========================================================================

func createTestSandbox(t *testing.T, s *Store, userID, sbID, name string) {
	t.Helper()
	err := s.CreateSandbox(Sandbox{
		ID: sbID, Name: name, EngineID: "eng_" + sbID,
		Status: "running", CreatedBy: userID,
		EngineMeta: json.RawMessage("{}"), CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
}
