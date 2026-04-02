package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	st, err := New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestVolumeBackupCRUD(t *testing.T) {
	st := newTestStore(t)

	b := VolumeBackup{
		ID:         "bk_001",
		VolumeName: "workspace",
		UserID:     "usr_test",
		S3Key:      "volumes/usr_test/workspace/2026-04-02T03:00:00Z.ext4.zst",
		SizeBytes:  1024 * 1024 * 50,
		SHA256:     "abc123",
		CreatedAt:  time.Now().UTC().Truncate(time.Second),
	}

	// Create
	if err := st.CreateVolumeBackup(b); err != nil {
		t.Fatal("create:", err)
	}

	// Get
	got, err := st.GetVolumeBackup("usr_test", "bk_001")
	if err != nil {
		t.Fatal("get:", err)
	}
	if got.VolumeName != "workspace" || got.S3Key != b.S3Key || got.SizeBytes != b.SizeBytes {
		t.Errorf("got %+v, want %+v", got, b)
	}

	// List
	list, err := st.ListVolumeBackups("usr_test", "workspace")
	if err != nil {
		t.Fatal("list:", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 backup, got %d", len(list))
	}

	// Delete
	if err := st.DeleteVolumeBackup("usr_test", "bk_001"); err != nil {
		t.Fatal("delete:", err)
	}
	list, _ = st.ListVolumeBackups("usr_test", "workspace")
	if len(list) != 0 {
		t.Fatalf("expected 0 backups after delete, got %d", len(list))
	}
}

func TestVolumeBackupListOrder(t *testing.T) {
	st := newTestStore(t)

	// Create 3 backups with different timestamps
	for i, ts := range []string{"2026-04-01T01:00:00Z", "2026-04-02T01:00:00Z", "2026-04-03T01:00:00Z"} {
		created, _ := time.Parse(time.RFC3339, ts)
		st.CreateVolumeBackup(VolumeBackup{
			ID:         "bk_" + ts,
			VolumeName: "data",
			UserID:     "usr_test",
			S3Key:      "key_" + ts,
			SizeBytes:  int64(i * 100),
			CreatedAt:  created,
		})
	}

	list, err := st.ListVolumeBackups("usr_test", "data")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("expected 3, got %d", len(list))
	}
	// Newest first
	if list[0].ID != "bk_2026-04-03T01:00:00Z" {
		t.Errorf("expected newest first, got %s", list[0].ID)
	}
	if list[2].ID != "bk_2026-04-01T01:00:00Z" {
		t.Errorf("expected oldest last, got %s", list[2].ID)
	}
}

func TestVolumeBackupRetention(t *testing.T) {
	st := newTestStore(t)

	// Create 5 backups
	for i := 0; i < 5; i++ {
		created := time.Date(2026, 4, i+1, 3, 0, 0, 0, time.UTC)
		st.CreateVolumeBackup(VolumeBackup{
			ID:         fmt.Sprintf("bk_%d", i),
			VolumeName: "workspace",
			UserID:     "usr_test",
			S3Key:      fmt.Sprintf("key_%d", i),
			SizeBytes:  100,
			CreatedAt:  created,
		})
	}

	// Keep 3 → should return 2 oldest
	old, err := st.OldestVolumeBackups("usr_test", "workspace", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(old) != 2 {
		t.Fatalf("expected 2 old backups, got %d", len(old))
	}
	// Should be the oldest ones (bk_0, bk_1)
	ids := map[string]bool{}
	for _, b := range old {
		ids[b.ID] = true
	}
	if !ids["bk_0"] || !ids["bk_1"] {
		t.Errorf("expected bk_0 and bk_1, got %v", ids)
	}
}

func TestVolumeBackupUserIsolation(t *testing.T) {
	st := newTestStore(t)

	st.CreateVolumeBackup(VolumeBackup{
		ID: "bk_a", VolumeName: "vol", UserID: "user_a",
		S3Key: "key_a", SizeBytes: 100, CreatedAt: time.Now().UTC(),
	})
	st.CreateVolumeBackup(VolumeBackup{
		ID: "bk_b", VolumeName: "vol", UserID: "user_b",
		S3Key: "key_b", SizeBytes: 100, CreatedAt: time.Now().UTC(),
	})

	// user_a should only see their backup
	list, _ := st.ListVolumeBackups("user_a", "vol")
	if len(list) != 1 || list[0].ID != "bk_a" {
		t.Errorf("user_a: expected [bk_a], got %v", list)
	}

	// user_b should only see their backup
	list, _ = st.ListVolumeBackups("user_b", "vol")
	if len(list) != 1 || list[0].ID != "bk_b" {
		t.Errorf("user_b: expected [bk_b], got %v", list)
	}

	// user_a can't get user_b's backup
	_, err := st.GetVolumeBackup("user_a", "bk_b")
	if err == nil {
		t.Error("expected error getting other user's backup")
	}
}

// Ensure we didn't break existing store functionality
func TestStoreOpenClose(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	st, err := New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	// Reopen should work
	st2, err := New(dbPath)
	if err != nil {
		t.Fatal("reopen:", err)
	}
	st2.Close()

	if _, err := os.Stat(dbPath); err != nil {
		t.Fatal("db file missing:", err)
	}
}
