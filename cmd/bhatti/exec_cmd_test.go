package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// streamServer fakes the daemon's exec endpoint. For an NDJSON request it sends
// "line1", then holds the rest of the stream until the test closes release, so a
// client that buffers until the command ends can never show line1 in time.
// Anything else gets the buffered JSON result.
type streamServer struct {
	*httptest.Server
	release chan struct{}
	mu      sync.Mutex
	accepts []string // Accept header of each exec request
	bodies  []map[string]any
}

func newStreamServer(t *testing.T, exitCode int) *streamServer {
	t.Helper()
	s := &streamServer{release: make(chan struct{})}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.accepts = append(s.accepts, r.Header.Get("Accept"))
		s.bodies = append(s.bodies, body)
		s.mu.Unlock()

		if r.Header.Get("Accept") != "application/x-ndjson" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"exit_code":%d,"stdout":"line1\nline2\n","stderr":"warn\n"}`, exitCode)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		fmt.Fprintln(w, `{"type":"stdout","data":"line1\n"}`)
		fl.Flush()
		select {
		case <-s.release:
		case <-time.After(10 * time.Second):
			return // the client never showed line1: the test reports it
		}
		fmt.Fprintln(w, `{"type":"stderr","data":"warn\n"}`)
		fmt.Fprintln(w, `{"type":"stdout","data":"line2\n"}`)
		fmt.Fprintf(w, `{"type":"exit","exit_code":%d}`+"\n", exitCode)
		fl.Flush()
	}))
	t.Cleanup(s.Close)
	return s
}

// notifyWriter reports when what was written so far contains marker.
type notifyWriter struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	marker string
	seen   chan struct{}
	once   sync.Once
}

func (w *notifyWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	if strings.Contains(w.buf.String(), w.marker) {
		w.once.Do(func() { close(w.seen) })
	}
	return len(p), nil
}

func (w *notifyWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func useEndpoint(t *testing.T, url string) {
	t.Helper()
	prevURL, prevToken, prevSock := apiURL, apiToken, unixSocketPath
	t.Cleanup(func() { apiURL, apiToken, unixSocketPath = prevURL, prevToken, prevSock })
	apiURL, apiToken, unixSocketPath = url, "", ""
}

// Output reaches the writer while the command still runs, stdout and stderr
// stay apart, and the command's own exit code comes back.
func TestExecStreamDeliversOutputBeforeTheCommandEnds(t *testing.T) {
	srv := newStreamServer(t, 3)
	useEndpoint(t, srv.URL)

	stdout := &notifyWriter{marker: "line1\n", seen: make(chan struct{})}
	var stderr bytes.Buffer
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := execStream("sb", map[string]any{"cmd": []string{"build"}}, stdout, &stderr)
		done <- result{code, err}
	}()

	select {
	case <-stdout.seen:
	case <-time.After(5 * time.Second):
		t.Fatal("line1 not delivered while the command was still running: output is buffered until exit")
	}
	close(srv.release)
	res := <-done
	if res.err != nil || res.code != 3 {
		t.Fatalf("execStream = %d, %v; want exit code 3", res.code, res.err)
	}
	if got := stdout.String(); got != "line1\nline2\n" {
		t.Fatalf("stdout = %q", got)
	}
	if got := stderr.String(); got != "warn\n" {
		t.Fatalf("stderr = %q; want only the command's stderr", got)
	}
}

func TestExecStreamReportsFailuresInsteadOfExitingZero(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		code   int
		err    string // substring of the returned error
		stderr string // substring of stderr
	}{
		{"timeout before output", 504, `{"error":"exec timed out after 300s (use --timeout, up to 86400s)"}`, 0, "exec timed out after 300s (use --timeout, up to 86400s)", ""},
		{"timeout mid-stream", 200, `{"type":"stdout","data":"building\n"}` + "\n" + `{"type":"error","data":"exec timed out after 300s (use --timeout, up to 86400s)"}` + "\n", 1, "", "exec timed out after 300s (use --timeout, up to 86400s)"},
		{"stream cut off", 200, `{"type":"stdout","data":"building\n"}` + "\n", 1, "", "without an exit status"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			useEndpoint(t, srv.URL)

			var stdout, stderr bytes.Buffer
			code, err := execStream("sb", map[string]any{"cmd": []string{"build"}}, &stdout, &stderr)
			if code != tc.code {
				t.Fatalf("exit code = %d, want %d", code, tc.code)
			}
			if tc.err == "" && err != nil || tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
				t.Fatalf("err = %v; want containing %q", err, tc.err)
			}
			if !strings.Contains(stderr.String(), tc.stderr) {
				t.Fatalf("stderr = %q; want containing %q", stderr.String(), tc.stderr)
			}
		})
	}
}

func buildBhatti(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "bhatti")
	build := exec.Command("go", "build", "-o", bin, "./cmd/bhatti/")
	build.Dir = projectRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build bhatti: %s\n%v", out, err)
	}
	return bin
}

// bhattiCmd runs the CLI against url with a clean environment: no inherited
// config (a developer's points at a live server), no terminal.
func bhattiCmd(t *testing.T, bin, url string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"BHATTI_URL=" + url,
		"BHATTI_TOKEN=test",
	}
	return cmd
}

// The real command, with stdout a pipe (what scripts, agents and CI have): the
// first line shows up while the server is still holding the rest, and the exit
// status and stderr come through unchanged.
func TestExecCommandStreamsWhenStdoutIsNotATerminal(t *testing.T) {
	bin := buildBhatti(t)

	t.Run("streams", func(t *testing.T) {
		srv := newStreamServer(t, 3)
		cmd := bhattiCmd(t, bin, srv.URL, "exec", "sb", "--", "build")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.StdoutPipe() // a pipe, not a TTY
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer cmd.Process.Kill()

		br := bufio.NewReader(out)
		first := make(chan string, 1)
		go func() {
			line, _ := br.ReadString('\n')
			first <- line
		}()
		select {
		case line := <-first:
			if line != "line1\n" {
				t.Fatalf("first line = %q", line)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no output while the command was still running: exec buffers when stdout isn't a terminal")
		}
		close(srv.release)
		rest, _ := io.ReadAll(br) // must drain before Wait closes the pipe
		if got := "line1\n" + string(rest); got != "line1\nline2\n" || strings.Contains(got, "\x1b") {
			t.Fatalf("stdout = %q; want the command's bytes only, no terminal control sequences", got)
		}

		var exitErr *exec.ExitError
		if err := cmd.Wait(); !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
			t.Fatalf("exit = %v; want status 3", err)
		}
		if got := stderr.String(); got != "warn\n" {
			t.Fatalf("stderr = %q; want only the command's stderr", got)
		}
		srv.mu.Lock()
		defer srv.mu.Unlock()
		if len(srv.accepts) != 1 || srv.accepts[0] != "application/x-ndjson" {
			t.Fatalf("Accept headers = %q; want one NDJSON request", srv.accepts)
		}
	})

	// --json needs the whole result, so it stays a single buffered response.
	t.Run("json stays buffered", func(t *testing.T) {
		srv := newStreamServer(t, 0)
		cmd := bhattiCmd(t, bin, srv.URL, "--json", "exec", "sb", "--", "build")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("bhatti --json exec: %v", err)
		}
		var res struct {
			ExitCode int    `json:"exit_code"`
			Stdout   string `json:"stdout"`
			Stderr   string `json:"stderr"`
		}
		if err := json.Unmarshal(out, &res); err != nil || res.Stdout != "line1\nline2\n" || res.Stderr != "warn\n" {
			t.Fatalf("output %q (%v); want the buffered result", out, err)
		}
		srv.mu.Lock()
		defer srv.mu.Unlock()
		if len(srv.accepts) != 1 || srv.accepts[0] == "application/x-ndjson" {
			t.Fatalf("Accept headers = %q; --json must not ask for a stream", srv.accepts)
		}
	})
}
