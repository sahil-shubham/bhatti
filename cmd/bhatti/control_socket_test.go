package main

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg"
)

// Unix socket paths have a short OS limit (104 bytes on macOS), while Go's
// t.TempDir() can be much longer on macOS and under nested test names.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bhs-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// TestControlSocketTransport verifies the CLI talks to the daemon over a unix
// control socket (the #2 hole-closer): apiRequest routes through the socket, not
// TCP, when unixSocketPath is set.
func TestControlSocketTransport(t *testing.T) {
	sock := filepath.Join(shortSocketDir(t), "api.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("pong"))
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()

	// Point the CLI transport at the socket (restore after).
	prevSock, prevURL := unixSocketPath, apiURL
	unixSocketPath, apiURL = sock, "http://unix"
	defer func() { unixSocketPath, apiURL = prevSock, prevURL }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err := apiRequest("GET", "/ping", nil)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(body) != "pong" {
				t.Fatalf("body = %q, want pong (wrong endpoint reached)", body)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("apiRequest over unix socket failed: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestControlSocketOwnerOnlyByDefault(t *testing.T) {
	for _, tc := range []struct {
		name string
		gid  int
	}{
		{name: "unset"},
		{name: "explicit zero", gid: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &pkg.Config{DataDir: shortSocketDir(t), APISocketGID: tc.gid}
			srv := serveControlSocket(cfg, nil)
			t.Cleanup(func() { _ = srv.Close() })

			info, err := os.Stat(cfg.APISocketPath())
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != 0600 {
				t.Fatalf("socket mode = %04o, want 0600", got)
			}
		})
	}
}

func TestControlSocketGroupPermissions(t *testing.T) {
	gid := os.Getgid()
	if gid == 0 {
		// Zero means owner-only in the config; root uses a positive test group.
		gid = 1
	}
	sock := filepath.Join(shortSocketDir(t), "api.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	called, groupUpdated := false, false
	chown := func(path string, uid, group int) error {
		called = true
		if path != sock || uid != 0 || group != gid {
			t.Errorf("chown(%q, %d, %d), want (%q, 0, %d)", path, uid, group, sock, gid)
		}
		if os.Geteuid() == 0 {
			if err := os.Chown(path, uid, group); err != nil {
				return err
			}
			groupUpdated = true
			return nil
		}
		// Production requires root. Non-root tests can still set their own
		// primary group on systems that permit owner-initiated group changes.
		if err := os.Chown(path, -1, group); err != nil {
			t.Logf("cannot chgrp socket without privilege: %v (checking chown args and mode)", err)
			return nil
		}
		groupUpdated = true
		return nil
	}
	if err := setControlSocketPermissions(sock, gid, chown); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("group socket must be chowned")
	}
	info, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0660 {
		t.Fatalf("socket mode = %04o, want 0660", got)
	}
	if groupUpdated {
		if got := int(info.Sys().(*syscall.Stat_t).Gid); got != gid {
			t.Fatalf("socket gid = %d, want %d", got, gid)
		}
	}
}
