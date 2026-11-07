package server

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

// removingEngine is the mock engine with an engine's own say over a snapshot's
// files: it deletes the directory, or refuses as it is told to.
type removingEngine struct {
	*mockEngine
	refuse  error
	removed []string
}

func (r *removingEngine) RemoveSnapshot(_ context.Context, snapDir string) error {
	r.removed = append(r.removed, snapDir)
	if r.refuse != nil {
		return r.refuse
	}
	return os.RemoveAll(snapDir)
}

// snapshotOnDisk makes a snapshot of user usr_test: its record, and a
// directory with a file in it where the record says it is.
func snapshotOnDisk(t *testing.T, st *store.Store, dir, name string) string {
	t.Helper()
	snapDir := filepath.Join(dir, "snapshots", "usr_test", name)
	if err := os.MkdirAll(snapDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapDir, "rootfs.qcow2"), []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSnapshot(t.Context(), store.SnapshotRecord{ID: "id-" + name, UserID: "usr_test", Name: name,
		MemPath: filepath.Join(snapDir, "mem.snap"), ManifestJSON: "{}", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return snapDir
}

func setupRemoving(t *testing.T, eng engine.Engine) (*httptest.Server, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(t.Context(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	st.CreateUser(t.Context(), store.User{
		ID: "usr_test", Name: "test-user", APIKeyHash: sha256Hex(testAPIKey),
		MaxSandboxes: 50, MaxCPUsPerSandbox: 4, MaxMemoryMBPerSandbox: 4096,
		SubnetIndex: 1, CreatedAt: time.Now(),
	})
	srv := New(eng, st, dir)
	ts := httptest.NewServer(srv)
	t.Cleanup(func() { srv.Close(); ts.Close(); st.Close() })
	return ts, st, dir
}

// TestDeleteSnapshotRefusedWhileDisksAreLayeredOverIt: the engine that knows
// what is layered over a snapshot's disks has the say over deleting it. Its
// refusal is a 409 carrying what it counted, and leaves the snapshot whole
// (record and files), so the delete can be tried again once the sandboxes are
// destroyed; the retry that succeeds takes the record too.
func TestDeleteSnapshotRefusedWhileDisksAreLayeredOverIt(t *testing.T) {
	eng := &removingEngine{mockEngine: newMockEngine()}
	ts, st, dir := setupRemoving(t, eng)
	snapDir := snapshotOnDisk(t, st, dir, "golden")

	eng.refuse = fmt.Errorf("%w by 3 sandboxes, whose disks are layered over this snapshot's; destroy them first", engine.ErrSnapshotInUse)
	resp := doReq(t, ts, "DELETE", "/snapshots/golden", nil)
	if resp.StatusCode != 409 || !strings.Contains(errorBody(t, resp), "3 sandboxes") {
		t.Fatalf("a snapshot with clones: status %d", resp.StatusCode)
	}
	if _, err := st.GetSnapshot(t.Context(), "usr_test", "golden"); err != nil {
		t.Fatalf("a refused delete took the record: %v", err)
	}
	if _, err := os.Stat(filepath.Join(snapDir, "rootfs.qcow2")); err != nil {
		t.Fatalf("a refused delete took the files: %v", err)
	}

	eng.refuse = nil
	expect(t, doReq(t, ts, "DELETE", "/snapshots/golden", nil), 200, nil)
	if _, err := st.GetSnapshot(t.Context(), "usr_test", "golden"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("record after a delete: %v", err)
	}
	if _, err := os.Stat(snapDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("files after a delete: %v", err)
	}
	if len(eng.removed) != 2 || eng.removed[1] != snapDir {
		t.Fatalf("the engine was asked to remove %v, want the snapshot's directory", eng.removed)
	}
}

// TestDeleteSnapshotKeepsTheRecordWhenTheEngineFails: an engine that can't
// remove the files for any other reason leaves the record too: a snapshot
// that is listed but gone can't be told from one that is whole.
func TestDeleteSnapshotKeepsTheRecordWhenTheEngineFails(t *testing.T) {
	eng := &removingEngine{mockEngine: newMockEngine(), refuse: errors.New("disk unplugged")}
	ts, st, dir := setupRemoving(t, eng)
	snapshotOnDisk(t, st, dir, "golden")
	if resp := doReq(t, ts, "DELETE", "/snapshots/golden", nil); resp.StatusCode != 500 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if _, err := st.GetSnapshot(t.Context(), "usr_test", "golden"); err != nil {
		t.Fatalf("record after a failed delete: %v", err)
	}
}

// TestDeleteSnapshotOfAnEngineWithoutTheSayRemovesTheDirectory: engines whose
// snapshots are self-contained copies are deleted as they always were.
func TestDeleteSnapshotOfAnEngineWithoutTheSayRemovesTheDirectory(t *testing.T) {
	ts, st, dir := setupRemoving(t, newMockEngine())
	snapDir := snapshotOnDisk(t, st, dir, "plain")
	expect(t, doReq(t, ts, "DELETE", "/snapshots/plain", nil), 200, nil)
	if _, err := os.Stat(snapDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("directory after a delete: %v", err)
	}
}
