package server

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

// --- Snapshot export/import: moving a named snapshot to another host ---

// snapshotLimitError is why userID can't have another snapshot, "" if it can.
func (s *Server) snapshotLimitError(userID string) string {
	existing, _ := s.store.ListSnapshots(userID)
	maxSnaps := 5
	if u, _ := s.store.GetUser(userID); u != nil && u.MaxSnapshots > 0 {
		maxSnaps = u.MaxSnapshots
	}
	if len(existing) >= maxSnaps {
		return fmt.Sprintf("snapshot limit reached (%d/%d)", len(existing), maxSnaps)
	}
	return ""
}

// newSnapshotRecord describes the snapshot directory dir. The resume handler
// finds the directory as MemPath's parent; the other paths are the
// Firecracker layout.
func newSnapshotRecord(userID, name, sourceSandbox, dir string, manifestJSON []byte) store.SnapshotRecord {
	var total int64
	filepath.Walk(dir, func(_ string, fi os.FileInfo, _ error) error {
		if fi != nil && !fi.IsDir() {
			total += fi.Size()
		}
		return nil
	})
	return store.SnapshotRecord{
		ID: genID(), UserID: userID, Name: name,
		SourceSandbox: sourceSandbox,
		MemPath:       filepath.Join(dir, "mem.snap"),
		VMPath:        filepath.Join(dir, "vm.snap"),
		RootfsPath:    filepath.Join(dir, "rootfs.ext4"),
		ConfigPath:    filepath.Join(dir, "config.ext4"),
		ManifestJSON:  string(manifestJSON),
		SizeMB:        int(total / 1024 / 1024),
		CreatedAt:     time.Now(),
	}
}

// snapshotPortError reports an export or import failure: a damaged archive is
// the client's (400), a snapshot this host can't run is refused with the
// engine's reason (422), anything else is internal.
func snapshotPortError(w http.ResponseWriter, r *http.Request, logMsg string, err error) {
	switch {
	case errors.Is(err, engine.ErrBadSnapshotArchive):
		errResp(w, 400, err.Error())
	case errors.Is(err, engine.ErrSnapshotIncompatible):
		errResp(w, 422, err.Error())
	default:
		errRespInternal(w, r, logMsg, err)
	}
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// handleSnapshotExport streams a named snapshot as an archive another host's
// POST /snapshots/import takes in. GET /snapshots/:name/export[?include_base=true]
func (s *Server) handleSnapshotExport(w http.ResponseWriter, r *http.Request, user *store.User, name string) {
	if r.Method != http.MethodGet {
		errResp(w, 405, "method not allowed")
		return
	}
	var includeBase bool
	if v := r.URL.Query().Get("include_base"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			errResp(w, 400, "include_base must be true or false")
			return
		}
		includeBase = b
	}
	snap, err := s.store.GetSnapshot(user.ID, name)
	if err != nil {
		errResp(w, 404, err.Error())
		return
	}
	sp, ok := s.engine.(engine.SnapshotPorter)
	if !ok {
		errResp(w, 501, "engine does not support snapshot export")
		return
	}

	w.Header().Set("Content-Type", "application/zstd")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name+".tar.zst"))
	cw := &countingWriter{w: w}
	err = sp.ExportSnapshot(r.Context(), cw, filepath.Dir(snap.MemPath), []byte(snap.ManifestJSON),
		engine.SnapshotExport{Name: name, IncludeBase: includeBase})
	if err != nil {
		if cw.n == 0 {
			w.Header().Del("Content-Disposition")
			snapshotPortError(w, r, "export snapshot failed", err)
			return
		}
		// Part of the archive is out with a 200: only a broken connection
		// tells the client it didn't get all of it.
		slog.Error("snapshot.export_failed", "request_id", RequestIDFromContext(r.Context()),
			"name", name, "user", user.Name, "sent_bytes", cw.n, "error", err)
		panic(http.ErrAbortHandler)
	}
	slog.Info("snapshot.exported", "name", name, "user", user.Name,
		"include_base", includeBase, "bytes", cw.n)
	s.RecordEvent(store.Event{
		Type: "snapshot.exported", UserID: user.ID,
		Meta: map[string]any{"name": name, "include_base": includeBase, "bytes": cw.n},
	})
}

// handleSnapshotImport registers a snapshot from an archive an export wrote,
// under ?name= or the name it was exported with. POST /snapshots/import
func (s *Server) handleSnapshotImport(w http.ResponseWriter, r *http.Request, user *store.User) {
	name := r.URL.Query().Get("name")
	if name != "" && !isValidName(name) {
		errResp(w, 400, "invalid snapshot name")
		return
	}
	sp, ok := s.engine.(engine.SnapshotPorter)
	if !ok {
		errResp(w, 501, "engine does not support snapshot import")
		return
	}
	// Checked before the upload when the name is known; again after it.
	exists := func(name string) bool {
		_, err := s.store.GetSnapshot(user.ID, name)
		return err == nil
	}
	if name != "" && exists(name) {
		errResp(w, 409, fmt.Sprintf("snapshot %q already exists", name))
		return
	}
	if msg := s.snapshotLimitError(user.ID); msg != "" {
		errResp(w, 429, msg)
		return
	}

	snapRoot := filepath.Join(s.dataDir, "snapshots", user.ID)
	if err := os.MkdirAll(snapRoot, 0o700); err != nil {
		errRespInternal(w, r, "import snapshot failed", err)
		return
	}
	// A .tmp directory: daemon startup removes one an interrupted import left.
	staging := filepath.Join(snapRoot, "import-"+genID()+".tmp")
	imp, err := sp.ImportSnapshot(r.Context(), r.Body, staging)
	if err != nil {
		snapshotPortError(w, r, "import snapshot failed", err)
		return
	}
	defer os.RemoveAll(staging) // a no-op once it is renamed into place

	if name == "" {
		name = imp.Name
		if !isValidName(name) {
			errResp(w, 400, fmt.Sprintf("the archive's snapshot name %q isn't valid; pass ?name=", name))
			return
		}
	}
	finalDir := filepath.Join(snapRoot, name)
	if _, err := os.Lstat(finalDir); exists(name) || err == nil {
		errResp(w, 409, fmt.Sprintf("snapshot %q already exists", name))
		return
	}
	if err := os.Rename(staging, finalDir); err != nil {
		errRespInternal(w, r, "import snapshot failed", err)
		return
	}
	snap := newSnapshotRecord(user.ID, name, "", finalDir, imp.ManifestJSON)
	if err := s.store.CreateSnapshot(snap); err != nil {
		os.RemoveAll(finalDir)
		if strings.Contains(err.Error(), "UNIQUE") {
			errResp(w, 409, fmt.Sprintf("snapshot %q already exists", name))
		} else {
			errRespInternal(w, r, "store snapshot record failed", err)
		}
		return
	}
	slog.Info("snapshot.imported", "name", name, "type", imp.Type, "user", user.Name, "size_mb", snap.SizeMB)
	s.RecordEvent(store.Event{
		Type: "snapshot.imported", UserID: user.ID,
		Meta: map[string]any{"name": name, "type": imp.Type, "size_mb": snap.SizeMB},
	})
	writeJSON(w, 201, snap)
}
