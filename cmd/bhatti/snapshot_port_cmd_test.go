package main

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// serveCLI points the CLI at handler over a unix socket, the transport it uses
// against a local daemon.
func serveCLI(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	// AF_UNIX paths are short (104 bytes on macOS); t.TempDir() can be longer.
	dir, err := os.MkdirTemp("/tmp", "bt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "api.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler}
	go srv.Serve(ln)
	prevSock, prevURL, prevToken := unixSocketPath, apiURL, apiToken
	unixSocketPath, apiURL, apiToken = sock, "http://unix", "tok"
	t.Cleanup(func() {
		srv.Close()
		unixSocketPath, apiURL, apiToken = prevSock, prevURL, prevToken
	})
}

// TestSnapshotExportLeavesNoPartialArchive: an archive only appears under its
// name once all of it arrived; a download that breaks off or is refused
// leaves no file that could be imported as a whole one.
func TestSnapshotExportLeavesNoPartialArchive(t *testing.T) {
	archive := bytes.Repeat([]byte("archive"), 200_000)
	var breakOff bool
	serveCLI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/snapshots/dev-ready/export" || r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(404)
			return
		}
		if r.URL.Query().Get("include_base") == "true" {
			w.WriteHeader(422)
			io.WriteString(w, `{"error":"engine says no"}`)
			return
		}
		w.Write(archive[:len(archive)/2])
		if breakOff {
			panic(http.ErrAbortHandler)
		}
		w.Write(archive[len(archive)/2:])
	})
	out := filepath.Join(t.TempDir(), "dev-ready.tar.zst")
	noFile := func() {
		t.Helper()
		for _, p := range []string{out, out + ".partial"} {
			if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s exists after a failed export", p)
			}
		}
	}

	breakOff = true
	if _, err := exportSnapshot("dev-ready", out, false); err == nil || !strings.Contains(err.Error(), "broke off") {
		t.Fatalf("export that broke off: %v", err)
	}
	noFile()
	if _, err := exportSnapshot("dev-ready", out, true); err == nil || !strings.Contains(err.Error(), "422") || !strings.Contains(err.Error(), "engine says no") {
		t.Fatalf("refused export: %v", err)
	}
	noFile()

	breakOff = false
	if n, err := exportSnapshot("dev-ready", out, false); err != nil || n != int64(len(archive)) {
		t.Fatalf("export: %d bytes, %v", n, err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, archive) {
		t.Fatal("the exported file isn't the archive")
	}
}

// TestSnapshotImportShowsTheRefusal: a host that refuses the archive early
// (another CPU, a missing base image) answers while the CLI is still
// uploading; the user sees that answer, not a broken pipe.
func TestSnapshotImportShowsTheRefusal(t *testing.T) {
	const why = "snapshot can't run on this host: CPU vendor differs: the checkpoint was taken on GenuineIntel"
	serveCLI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/snapshots/import" || r.URL.Query().Get("name") != "dev" {
			w.WriteHeader(404)
			return
		}
		io.CopyN(io.Discard, r.Body, 1<<20)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(422)
		io.WriteString(w, `{"error":"`+why+`"}`)
	})
	path := filepath.Join(t.TempDir(), "big.tar.zst")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Truncate(1 << 30) // sparse: cheap to make, a gigabyte to upload
	f.Close()
	_, err = importSnapshot(path, "dev")
	if err == nil || err.Error() != "422 Unprocessable Entity: "+why {
		t.Fatalf("import: %v", err)
	}
}
