package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/store"
)

func TestReconcileOrphanedVolumeFilesKeepsRecordedVolumeOnAttachmentError(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	st, err := store.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	volDir := filepath.Join(dir, "volumes", "owner")
	if err := os.MkdirAll(volDir, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(volDir, "live.ext4")
	if err := os.WriteFile(file, []byte("live volume"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := st.CreatePersistentVolume(store.PersistentVolume{
		ID: "vol", UserID: "owner", Name: "live", SizeMB: 10,
		FilePath: file, Status: "ready", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// SQLite accepts TEXT in the integer-affinity read_only column. An
	// attachment scan fails even though the volume's own row is present.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO volume_attachments(volume_id, sandbox_id, mount, read_only) VALUES ('vol','sb','/data','broken')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetPersistentVolume("owner", "live"); err == nil {
		t.Fatal("fixture: attachment scan unexpectedly succeeded")
	}
	orphan := filepath.Join(volDir, "orphan.ext4")
	if err := os.WriteFile(orphan, []byte("orphan"), 0600); err != nil {
		t.Fatal(err)
	}
	reconcileOrphanedVolumeFiles(dir, st)
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("recorded volume deleted after attachment scan failure: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan file not deleted: %v", err)
	}
}

func TestReconcileOrphanedVolumeFilesKeepsFilesOnDatabaseError(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	volDir := filepath.Join(dir, "volumes", "owner")
	if err := os.MkdirAll(volDir, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(volDir, "unknown.ext4")
	if err := os.WriteFile(file, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`DROP TABLE volumes_v2`); err != nil {
		t.Fatal(err)
	}
	reconcileOrphanedVolumeFiles(dir, st)
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("volume deleted when database query failed: %v", err)
	}
}
