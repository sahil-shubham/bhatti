package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

// portingEngine is the mock engine plus the snapshot porter and resume
// capabilities, each answering as the test sets it.
type portingEngine struct {
	*mockEngine
	export  func(w io.Writer) error
	imp     func(r io.Reader, destDir string) (engine.SnapshotImport, error)
	resumed struct {
		snapDir  string
		manifest string
	}
}

func (p *portingEngine) ExportSnapshot(_ context.Context, w io.Writer, _ string, _ []byte, _ engine.SnapshotExport) error {
	return p.export(w)
}

func (p *portingEngine) ImportSnapshot(_ context.Context, r io.Reader, destDir string) (engine.SnapshotImport, error) {
	return p.imp(r, destDir)
}

func (p *portingEngine) ResumeFromManifestJSON(ctx context.Context, snapDir string, manifestJSON []byte, newName, _ string) (engine.SandboxInfo, error) {
	p.resumed.snapDir, p.resumed.manifest = snapDir, string(manifestJSON)
	return p.mockEngine.Create(ctx, engine.SandboxSpec{Name: newName})
}

func setupPorting(t *testing.T) (*httptest.Server, *portingEngine, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	st.CreateUser(store.User{
		ID: "usr_test", Name: "test-user", APIKeyHash: sha256Hex(testAPIKey),
		MaxSandboxes: 50, MaxCPUsPerSandbox: 4, MaxMemoryMBPerSandbox: 4096,
		SubnetIndex: 1, CreatedAt: time.Now(),
	})
	eng := &portingEngine{mockEngine: newMockEngine()}
	srv := New(eng, st, dir)
	ts := httptest.NewServer(srv)
	t.Cleanup(func() { srv.Close(); ts.Close(); st.Close() })
	return ts, eng, st, dir
}

func postArchive(t *testing.T, ts *httptest.Server, query string, body io.Reader) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", ts.URL+"/snapshots/import"+query, body)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	resp, err := testHTTPClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func errorBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	decodeJSON(t, resp, &body)
	return body.Error
}

// TestSnapshotExportFailureReachesTheClient: a failure before the archive
// starts is an ordinary error reply; one after part of it went out must
// break the download, or the client keeps a truncated file as a whole one.
func TestSnapshotExportFailureReachesTheClient(t *testing.T) {
	ts, eng, st, _ := setupPorting(t)
	if err := st.CreateSnapshot(store.SnapshotRecord{ID: "s1", UserID: "usr_test", Name: "dev-ready",
		MemPath: "/x/dev-ready/mem.snap", ManifestJSON: "{}", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	eng.export = func(io.Writer) error {
		return fmt.Errorf("%w: the snapshot binds host directories", engine.ErrNotSupported)
	}
	resp := doReq(t, ts, "GET", "/snapshots/dev-ready/export", nil)
	if resp.StatusCode != 501 || !strings.Contains(errorBody(t, resp), "binds host directories") {
		t.Fatalf("refusal before streaming: status %d", resp.StatusCode)
	}

	eng.export = func(w io.Writer) error {
		w.Write(bytes.Repeat([]byte("archive"), 100_000))
		return errors.New("disk read failed")
	}
	resp = doReq(t, ts, "GET", "/snapshots/dev-ready/export", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if n, err := io.Copy(io.Discard, resp.Body); err == nil {
		t.Fatalf("a download that broke off after %d bytes read as complete", n)
	}

	eng.export = func(w io.Writer) error { _, err := w.Write([]byte("whole archive")); return err }
	resp = doReq(t, ts, "GET", "/snapshots/dev-ready/export?include_base=true", nil)
	defer resp.Body.Close()
	if body, err := io.ReadAll(resp.Body); err != nil || string(body) != "whole archive" {
		t.Fatalf("body %q, err %v", body, err)
	}
}

// TestSnapshotImportRegistersAResumableSnapshot: the imported directory ends
// up where resume looks for it, under the archive's name, with nothing left
// in staging.
func TestSnapshotImportRegistersAResumableSnapshot(t *testing.T) {
	ts, eng, _, dir := setupPorting(t)
	const manifest = `{"type":"memory","vcpus":1}`
	var staging string
	eng.imp = func(r io.Reader, destDir string) (engine.SnapshotImport, error) {
		staging = destDir
		io.Copy(io.Discard, r)
		if err := os.Mkdir(destDir, 0o700); err != nil {
			return engine.SnapshotImport{}, err
		}
		os.WriteFile(filepath.Join(destDir, "memory.bin"), make([]byte, 3<<20), 0o600)
		return engine.SnapshotImport{Name: "dev-ready", Type: "memory", ManifestJSON: []byte(manifest)}, nil
	}
	resp := postArchive(t, ts, "", strings.NewReader("archive"))
	if resp.StatusCode != 201 {
		t.Fatalf("import: %d %s", resp.StatusCode, errorBody(t, resp))
	}
	var snap store.SnapshotRecord
	decodeJSON(t, resp, &snap)
	final := filepath.Join(dir, "snapshots", "usr_test", "dev-ready")
	if snap.Name != "dev-ready" || snap.SizeMB != 3 {
		t.Fatalf("imported %+v", snap)
	}
	if !strings.HasSuffix(staging, ".tmp") || filepath.Dir(staging) != filepath.Dir(final) {
		t.Fatalf("staged in %s: startup cleanup wouldn't find an interrupted import", staging)
	}
	mustBeGone(t, staging)
	if _, err := os.Stat(filepath.Join(final, "memory.bin")); err != nil {
		t.Fatal(err)
	}

	resp = doReq(t, ts, "POST", "/snapshots/dev-ready/resume", map[string]any{"name": "dev"})
	if resp.StatusCode != 201 {
		t.Fatalf("resume: %d %s", resp.StatusCode, errorBody(t, resp))
	}
	resp.Body.Close()
	if eng.resumed.snapDir != final || eng.resumed.manifest != manifest {
		t.Fatalf("resume used %+v; want %s with the imported manifest", eng.resumed, final)
	}
}

func mustBeGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s is still there (%v)", path, err)
	}
}

// TestSnapshotImportRefusals: the engine's refusals reach the client with
// their reasons, and a name that's taken is refused without keeping anything.
func TestSnapshotImportRefusals(t *testing.T) {
	ts, eng, st, dir := setupPorting(t)
	userDir := filepath.Join(dir, "snapshots", "usr_test")
	stage := func(name string) func(io.Reader, string) (engine.SnapshotImport, error) {
		return func(_ io.Reader, destDir string) (engine.SnapshotImport, error) {
			if err := os.Mkdir(destDir, 0o700); err != nil {
				return engine.SnapshotImport{}, err
			}
			return engine.SnapshotImport{Name: name, ManifestJSON: []byte("{}")}, nil
		}
	}

	for status, refusal := range map[int]error{
		400: fmt.Errorf("%w: memory.bin: unexpected EOF", engine.ErrBadSnapshotArchive),
		422: fmt.Errorf("%w: CPU vendor differs: the checkpoint was taken on GenuineIntel", engine.ErrSnapshotIncompatible),
		501: fmt.Errorf("%w: no checkpoint support", engine.ErrNotSupported),
	} {
		eng.imp = func(io.Reader, string) (engine.SnapshotImport, error) { return engine.SnapshotImport{}, refusal }
		resp := postArchive(t, ts, "", strings.NewReader("archive"))
		if got := errorBody(t, resp); resp.StatusCode != status || got != refusal.Error() {
			t.Fatalf("refusal %q: %d %q", refusal, resp.StatusCode, got)
		}
	}

	if err := st.CreateSnapshot(store.SnapshotRecord{ID: "s1", UserID: "usr_test", Name: "taken",
		MemPath: filepath.Join(userDir, "taken", "mem.snap"), ManifestJSON: "{}", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	called := false
	eng.imp = func(io.Reader, string) (engine.SnapshotImport, error) {
		called = true
		return engine.SnapshotImport{}, errors.New("unreachable")
	}
	if resp := postArchive(t, ts, "?name=taken", strings.NewReader("x")); resp.StatusCode != 409 || called {
		t.Fatalf("explicit taken name: %d (engine called: %v)", resp.StatusCode, called)
	}
	if resp := postArchive(t, ts, "?name=../escape", strings.NewReader("x")); resp.StatusCode != 400 || called {
		t.Fatalf("bad name: %d (engine called: %v)", resp.StatusCode, called)
	}

	// The archive's own name is only known once it is unpacked.
	eng.imp = stage("taken")
	if resp := postArchive(t, ts, "", strings.NewReader("x")); resp.StatusCode != 409 {
		t.Fatalf("archive named like an existing snapshot: %d", resp.StatusCode)
	}
	eng.imp = stage("no/such name")
	if resp := postArchive(t, ts, "", strings.NewReader("x")); resp.StatusCode != 400 {
		t.Fatalf("archive with an invalid name: %d", resp.StatusCode)
	}
	entries, _ := os.ReadDir(userDir)
	if len(entries) != 0 {
		t.Fatalf("refused imports left %v behind", entries)
	}
	if snaps, _ := st.ListSnapshots("usr_test"); len(snaps) != 1 {
		t.Fatalf("snapshots after refusals: %+v", snaps)
	}

	// At the limit (5 by default), before reading the upload.
	for _, n := range []string{"a", "b", "c", "d"} {
		st.CreateSnapshot(store.SnapshotRecord{ID: n, UserID: "usr_test", Name: n, ManifestJSON: "{}", CreatedAt: time.Now()})
	}
	called = false
	eng.imp = func(io.Reader, string) (engine.SnapshotImport, error) {
		called = true
		return engine.SnapshotImport{}, nil
	}
	if resp := postArchive(t, ts, "", strings.NewReader("x")); resp.StatusCode != 429 || called {
		t.Fatalf("over the limit: %d (engine called: %v)", resp.StatusCode, called)
	}
}

// TestSnapshotImportEarlyRefusalReachesTheUploader: an import refused after
// the archive's first megabyte (another CPU, a missing base) answers while
// the client is still sending gigabytes; the client must get that answer, not
// a broken pipe.
func TestSnapshotImportEarlyRefusalReachesTheUploader(t *testing.T) {
	ts, eng, _, _ := setupPorting(t)
	refusal := fmt.Errorf("%w: missing base image sha256:00", engine.ErrSnapshotIncompatible)
	eng.imp = func(r io.Reader, _ string) (engine.SnapshotImport, error) {
		io.CopyN(io.Discard, r, 1<<20)
		return engine.SnapshotImport{}, refusal
	}
	resp := postArchive(t, ts, "", io.LimitReader(zeroReader{}, 2<<30))
	if got := errorBody(t, resp); resp.StatusCode != 422 || got != refusal.Error() {
		t.Fatalf("got %d %q", resp.StatusCode, got)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
