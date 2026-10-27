package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const noSpaceBody = `{"error":"no space: need 68719476736 bytes (64.0G), 1288490188 bytes (1.2G) available"}`

// The daemon rejects a write the guest cannot hold and answers without draining
// the body; the CLI must show that answer, not the write error from the
// connection the server closed under it.
func TestFileWriteShowsRejectionWhileStreamingHugeBody(t *testing.T) {
	huge := filepath.Join(t.TempDir(), "huge.img")
	f, err := os.Create(huge)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(64 << 30); err != nil { // sparse: no disk or RAM needed
		t.Skipf("filesystem cannot make a sparse 64 GiB file: %v", err)
	}

	reject := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength != 64<<30 {
			t.Errorf("Content-Length = %d; want the file size streamed, not buffered", r.ContentLength)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInsufficientStorage)
		io.WriteString(w, noSpaceBody)
	})

	for _, transport := range []string{"tcp", "unix"} {
		t.Run(transport, func(t *testing.T) {
			prevURL, prevToken, prevSock := apiURL, apiToken, unixSocketPath
			t.Cleanup(func() { apiURL, apiToken, unixSocketPath = prevURL, prevToken, prevSock })
			apiToken = ""
			if transport == "tcp" {
				srv := httptest.NewServer(reject)
				t.Cleanup(srv.Close)
				apiURL, unixSocketPath = srv.URL, ""
			} else {
				dir, err := os.MkdirTemp("/tmp", "bh-cli-") // sockaddr_un length limit
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.RemoveAll(dir) })
				sock := filepath.Join(dir, "api.sock")
				ln, err := net.Listen("unix", sock)
				if err != nil {
					t.Fatal(err)
				}
				srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: reject}}
				srv.Start()
				t.Cleanup(srv.Close)
				apiURL, unixSocketPath = "http://unix", sock
			}

			// A race between the early response and the body write shows up as a
			// transport error only some of the time, so try repeatedly.
			for i := range 10 {
				if _, err := f.Seek(0, io.SeekStart); err != nil {
					t.Fatal(err)
				}
				err := putFile("sb", "/home/lohar/huge", f)
				if err == nil || !strings.Contains(err.Error(), "507") || !strings.Contains(err.Error(), "no space: need 68719476736 bytes") {
					t.Fatalf("attempt %d: putFile = %v; want the server's 507 message", i, err)
				}
				if strings.Contains(err.Error(), `{"error"`) {
					t.Fatalf("error shows the raw JSON body: %v", err)
				}
			}
		})
	}
}

// A regular-file stdin is streamed from its current offset with the matching
// Content-Length; a pipe is buffered. Both deliver the same bytes.
func TestPutFileSendsRemainingBytesOfFileAndPipe(t *testing.T) {
	var got string
	var length int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got, length = string(b), r.ContentLength
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()
	prevURL, prevToken, prevSock := apiURL, apiToken, unixSocketPath
	defer func() { apiURL, apiToken, unixSocketPath = prevURL, prevToken, prevSock }()
	apiURL, apiToken, unixSocketPath = srv.URL, "", ""

	file, err := os.CreateTemp(t.TempDir(), "in")
	if err != nil {
		t.Fatal(err)
	}
	file.WriteString("0123456789")
	file.Seek(4, io.SeekStart)
	if err := putFile("sb", "/x", file); err != nil || got != "456789" || length != 6 {
		t.Fatalf("file: err=%v body=%q length=%d; want the 6 bytes after the offset", err, got, length)
	}

	r, w, _ := os.Pipe()
	go func() { w.WriteString("from a pipe"); w.Close() }()
	if err := putFile("sb", "/x", r); err != nil || got != "from a pipe" || length != 11 {
		t.Fatalf("pipe: err=%v body=%q length=%d", err, got, length)
	}

	empty, _ := os.CreateTemp(t.TempDir(), "empty")
	if err := putFile("sb", "/x", empty); err != nil || got != "" || length != 0 {
		t.Fatalf("empty: err=%v body=%q length=%d", err, got, length)
	}
}
