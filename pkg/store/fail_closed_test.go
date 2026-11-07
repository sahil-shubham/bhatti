package store

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConcurrentSandboxLabelMerges(t *testing.T) {
	s := testStore(t)
	labelTestSandbox(t, s, "sb1", "worker", nil)

	for i := range 100 {
		if _, err := s.db.Exec(`UPDATE sandboxes SET labels = '{}' WHERE id = 'sb1'`); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		var errs [2]error
		for j, key := range []string{"first", "second"} {
			wg.Add(1)
			go func(j int, key string) {
				defer wg.Done()
				<-start
				errs[j] = s.UpdateSandboxLabels(context.Background(), "usr_test", "sb1", map[string]string{key: "yes"}, nil)
			}(j, key)
		}
		close(start)
		wg.Wait()
		if errs[0] != nil || errs[1] != nil {
			t.Fatalf("iteration %d: updates failed: %v, %v", i, errs[0], errs[1])
		}
		sb, err := s.GetSandbox(t.Context(), "usr_test", "sb1")
		if err != nil {
			t.Fatal(err)
		}
		if sb.Labels["first"] != "yes" || sb.Labels["second"] != "yes" {
			t.Fatalf("iteration %d: lost concurrent label update: %v", i, sb.Labels)
		}
	}
}

func TestAttachPersistentVolumeRejectsFailedAttachmentCount(t *testing.T) {
	s := testStore(t)
	if err := s.CreatePersistentVolume(t.Context(), PersistentVolume{
		ID: "vol", UserID: "usr_test", Name: "data", SizeMB: 10,
		Status: "ready", FilePath: "/tmp/data", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TABLE volume_attachments`); err != nil {
		t.Fatal(err)
	}
	if err := s.AttachPersistentVolume(t.Context(), "usr_test", "data", "sb2", "/data", false); err == nil || !strings.Contains(err.Error(), "count read-write attachments") {
		t.Fatalf("expected RW count failure, got %v", err)
	}
}

func TestNextSubnetIndexRejectsFailedScanAndInvalidIndex(t *testing.T) {
	s := testStore(t)
	if _, err := s.db.Exec(`INSERT INTO users (id, name, api_key_hash, subnet_index) VALUES ('u', 'user', 'key', -1)`); err != nil {
		t.Fatal(err)
	}
	if idx, err := s.NextSubnetIndex(t.Context()); err == nil || idx != 0 {
		t.Fatalf("invalid subnet index: got %d, %v", idx, err)
	}
	if _, err := s.db.Exec(`DROP TABLE users`); err != nil {
		t.Fatal(err)
	}
	if idx, err := s.NextSubnetIndex(t.Context()); err == nil || idx != 0 || !strings.Contains(err.Error(), "next subnet index") {
		t.Fatalf("failed subnet scan: got %d, %v", idx, err)
	}
}

// TestNewRejectsNonDuplicateMigrationError: a column the database lacks and
// cannot take fails the open, naming the step and the column, and leaves the
// database at the version it had.
func TestNewRejectsNonDuplicateMigrationError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migration.db")
	s, err := New(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TABLE sandboxes`); err != nil {
		t.Fatal(err)
	}
	// SQLite cannot ALTER virtual tables; CREATE TABLE IF NOT EXISTS leaves
	// this existing name alone, isolating the additive migration failure.
	if _, err := s.db.Exec(`CREATE VIRTUAL TABLE sandboxes USING fts5(content)`); err != nil {
		t.Fatal(err)
	}
	// Reopened as a database from before the ladder, so the step runs again.
	setUserVersion(t, s.db, 0)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	opened, err := New(t.Context(), path)
	if err == nil {
		opened.Close()
		t.Fatal("opened a database whose sandboxes table cannot take its columns")
	}
	if msg := err.Error(); !strings.Contains(msg, "migration 1") || !strings.Contains(msg, "sandboxes.rootfs_path") {
		t.Fatalf("error does not name migration 1 and the column it could not add: %v", err)
	}
	if v := userVersion(t, rawOpen(t, path)); v != 0 {
		t.Fatalf("version after the failed open = %d, want 0", v)
	}
}

func TestDeleteUserRejectsFailedSandboxCount(t *testing.T) {
	s := testStore(t)
	if err := s.CreateUser(t.Context(), User{ID: "usr", Name: "test", APIKeyHash: "key", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TABLE sandboxes`); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser(t.Context(), "usr"); err == nil {
		t.Fatal("deleted user despite failed sandbox count")
	}
	if _, err := s.GetUser(t.Context(), "usr"); err != nil {
		t.Fatalf("failed count must leave user intact: %v", err)
	}
}

func TestVolumeStorageRejectsFailedSum(t *testing.T) {
	s := testStore(t)
	if _, err := s.db.Exec(`DROP TABLE volumes_v2`); err != nil {
		t.Fatal(err)
	}
	if n, err := s.UserVolumeStorageUsed(t.Context(), "usr"); err == nil || n != 0 {
		t.Fatalf("expected quota query error, got %d, %v", n, err)
	}
}

func TestGetPersistentVolumeRejectsFailedAttachmentLookup(t *testing.T) {
	s := testStore(t)
	if err := s.CreatePersistentVolume(t.Context(), PersistentVolume{
		ID: "vol", UserID: "usr", Name: "data", SizeMB: 10,
		Status: "ready", FilePath: "/tmp/data", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TABLE volume_attachments`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetPersistentVolume(t.Context(), "usr", "data"); err == nil {
		t.Fatal("returned empty attachment list after lookup failed")
	}
}

func TestDeletePersistentVolumeRejectsFailedAttachmentCount(t *testing.T) {
	s := testStore(t)
	if err := s.CreatePersistentVolume(t.Context(), PersistentVolume{
		ID: "vol", UserID: "usr", Name: "data", SizeMB: 10,
		Status: "ready", FilePath: "/tmp/data", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TABLE volume_attachments`); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePersistentVolume(t.Context(), "usr", "data"); err == nil {
		t.Fatal("deleted volume despite failed attachment count")
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM volumes_v2 WHERE id = 'vol'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("volume must remain after failed count: %d, %v", count, err)
	}
}

func TestDeleteVolumeRejectsFailedMountCount(t *testing.T) {
	s := testStore(t)
	if err := s.CreateVolume(t.Context(), "data"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TABLE sandbox_volumes`); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteVolume(t.Context(), "data"); err == nil {
		t.Fatal("deleted volume despite failed mount count")
	}
	if _, err := s.GetVolume(t.Context(), "data"); err != nil {
		t.Fatalf("volume must remain after failed count: %v", err)
	}
}

func TestListImageSharesRejectsUnreadableRow(t *testing.T) {
	s := testStore(t)
	if _, err := s.db.Exec(`DROP TABLE image_shares; CREATE TABLE image_shares (image_id TEXT, user_id TEXT);
		INSERT INTO image_shares VALUES ('img', NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListImageShares(t.Context(), "img"); err == nil {
		t.Fatal("returned an empty user ID after its row failed to scan")
	}
}
