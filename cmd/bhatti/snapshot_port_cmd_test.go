package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
	// An earlier archive under that name outlives a failed export.
	if err := os.WriteFile(out, []byte("earlier archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := exportSnapshot("dev-ready", out, false); err == nil {
		t.Fatal("export that broke off succeeded")
	}
	if got, _ := os.ReadFile(out); string(got) != "earlier archive" {
		t.Fatalf("a failed export left %d bytes where the earlier archive was", len(got))
	}
	os.Remove(out)
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

func TestCreateAllowSiblingsAndInspectPolicy(t *testing.T) {
	var sent map[string]any
	serveCLI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes":
			if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
				t.Errorf("decode create: %v", err)
			}
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"name":"","ip":"100.64.0.2"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/sandboxes/workers":
			io.WriteString(w, `{"id":"workers","name":"workers","status":"stopped","created_at":"now","ip":"100.64.0.2","net_policy":{"default":"deny","siblings":"allow"}}`)
		default:
			http.NotFound(w, r)
		}
	})

	flags := createCmd.Flags()
	oldName, _ := flags.GetString("name")
	oldAllow, _ := flags.GetBool("allow-siblings")
	nameChanged := flags.Lookup("name").Changed
	allowChanged := flags.Lookup("allow-siblings").Changed
	t.Cleanup(func() {
		flags.Set("name", oldName)
		flags.Set("allow-siblings", fmt.Sprint(oldAllow))
		flags.Lookup("name").Changed = nameChanged
		flags.Lookup("allow-siblings").Changed = allowChanged
	})
	flags.Set("name", "workers")
	flags.Set("allow-siblings", "true")
	if err := createCmd.RunE(createCmd, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	np, ok := sent["net_policy"].(map[string]any)
	if !ok || np["siblings"] != "allow" || np["default"] != nil {
		t.Fatalf("--allow-siblings request = %+v", sent)
	}

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prior := os.Stdout
	os.Stdout = write
	err = inspectCmd.RunE(inspectCmd, []string{"workers"})
	write.Close()
	os.Stdout = prior
	defer read.Close()
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	out, err := io.ReadAll(read)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "Egress:   deny") || !strings.Contains(string(out), "Siblings: allow") {
		t.Fatalf("inspect policy missing: %s", out)
	}
}
