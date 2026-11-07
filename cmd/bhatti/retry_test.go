package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// rateLimitedServer answers the first limited requests with 429 (plus
// Retry-After when retryAfter != ""), then 200. It records every body received.
type rateLimitedServer struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []string
}

func newRateLimitedServer(t *testing.T, limited int, retryAfter string) *rateLimitedServer {
	t.Helper()
	s := &rateLimitedServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.bodies = append(s.bodies, string(b))
		n := len(s.bodies)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n <= limited {
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":"slow down","code":"rate_limited"}`)
			return
		}
		io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *rateLimitedServer) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bodies...)
}

// useTestAPI points the CLI at url with a recording, instant sleeper.
func useTestAPI(t *testing.T, url string) *[]time.Duration {
	t.Helper()
	prevURL, prevToken, prevSock, prevPolicy := apiURL, apiToken, unixSocketPath, rateLimitRetry
	t.Cleanup(func() { apiURL, apiToken, unixSocketPath, rateLimitRetry = prevURL, prevToken, prevSock, prevPolicy })
	apiURL, apiToken, unixSocketPath = url, "", ""
	var slept []time.Duration
	rateLimitRetry.jitter = 0
	rateLimitRetry.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}
	return &slept
}

func TestAPIJSONRetriesRateLimitWithBodyIntact(t *testing.T) {
	srv := newRateLimitedServer(t, 2, "1")
	slept := useTestAPI(t, srv.URL)

	var out struct{ OK bool }
	if err := apiJSON("POST", "/sandboxes", map[string]string{"name": "dev"}, &out); err != nil {
		t.Fatalf("apiJSON: %v", err)
	}
	if !out.OK {
		t.Fatal("result not decoded from the successful attempt")
	}
	reqs := srv.requests()
	if len(reqs) != 3 {
		t.Fatalf("server saw %d requests, want 3", len(reqs))
	}
	for i, b := range reqs {
		if b != `{"name":"dev"}` {
			t.Errorf("request %d body = %q, want it re-sent intact", i, b)
		}
	}
	if len(*slept) != 2 || (*slept)[0] != time.Second {
		t.Errorf("slept %v, want two waits of the Retry-After value", *slept)
	}
}

func TestAPIJSONQuotaRefusalNotRetried(t *testing.T) {
	srv := newRateLimitedServer(t, 100, "")
	slept := useTestAPI(t, srv.URL)

	err := apiJSON("POST", "/sandboxes", map[string]string{"name": "dev"}, nil)
	if err == nil || !strings.Contains(err.Error(), "slow down") {
		t.Fatalf("err = %v, want the server's message", err)
	}
	if n := len(srv.requests()); n != 1 {
		t.Fatalf("server saw %d requests, want exactly 1", n)
	}
	if len(*slept) != 0 {
		t.Errorf("slept %v, want no waits", *slept)
	}
}

func TestAPIJSONGivesUpAfterThreeRetries(t *testing.T) {
	srv := newRateLimitedServer(t, 100, "1")
	useTestAPI(t, srv.URL)

	err := apiJSON("GET", "/sandboxes", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "slow down") {
		t.Fatalf("err = %v, want the server's message once retries run out", err)
	}
	if n := len(srv.requests()); n != 4 {
		t.Fatalf("server saw %d requests, want 1 + 3 retries", n)
	}
}

func TestRetryAfterWaitIsCapped(t *testing.T) {
	srv := newRateLimitedServer(t, 1, "3600")
	slept := useTestAPI(t, srv.URL)

	if err := apiJSON("GET", "/sandboxes", nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(*slept) != 1 || (*slept)[0] != rateLimitRetry.maxWait {
		t.Errorf("slept %v, want one wait capped at %v", *slept, rateLimitRetry.maxWait)
	}
}

func TestRetrySleepHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
		cancel() // the client is now waiting out Retry-After
	}))
	defer srv.Close()
	useTestAPI(t, srv.URL)
	rateLimitRetry.sleep = sleepCtx

	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	start := time.Now()
	if _, err := httpClient().Do(req); err == nil {
		t.Fatal("want an error once the context is cancelled mid-wait")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("cancellation did not interrupt the wait")
	}
}

func TestUnparseableRetryAfterNotRetried(t *testing.T) {
	// HTTP-date form is not an integer number of seconds.
	srv := newRateLimitedServer(t, 100, "Wed, 21 Oct 2026 07:28:00 GMT")
	useTestAPI(t, srv.URL)

	if err := apiJSON("GET", "/sandboxes", nil, nil); err == nil {
		t.Fatal("want the 429 as an error")
	}
	if n := len(srv.requests()); n != 1 {
		t.Fatalf("server saw %d requests, want 1", n)
	}
}

func TestPutFileRetriesWithFullBody(t *testing.T) {
	srv := newRateLimitedServer(t, 1, "1")
	useTestAPI(t, srv.URL)

	// A regular file is streamed; the retry must re-read it from the same
	// offset, not from wherever the first attempt left the cursor.
	path := filepath.Join(t.TempDir(), "in")
	if err := os.WriteFile(path, []byte("skipped-payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Seek(8, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if err := putFile("sb1", "/x", f); err != nil {
		t.Fatalf("putFile: %v", err)
	}
	reqs := srv.requests()
	if len(reqs) != 2 || reqs[0] != "payload" || reqs[1] != "payload" {
		t.Fatalf("bodies = %q, want the full payload twice", reqs)
	}
}

func TestStreamingBodyWithoutReplayNotRetried(t *testing.T) {
	srv := newRateLimitedServer(t, 1, "1")
	useTestAPI(t, srv.URL)

	req, _ := http.NewRequest("POST", srv.URL, io.NopCloser(strings.NewReader("stream")))
	resp, err := httpClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the 429 passed through", resp.StatusCode)
	}
	if n := len(srv.requests()); n != 1 {
		t.Fatalf("server saw %d requests, want 1 (stream can't be replayed)", n)
	}
}

func TestDialWSRetriesRateLimitedHandshake(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n <= 2 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		c, err := up.Upgrade(w, r, nil)
		if err == nil {
			c.Close()
		}
	}))
	defer srv.Close()
	slept := useTestAPI(t, srv.URL)

	conn, _, err := dialWS(t.Context(), websocket.DefaultDialer, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dialWS: %v", err)
	}
	conn.Close()
	if attempts != 3 || len(*slept) != 2 {
		t.Fatalf("attempts=%d sleeps=%v, want 3 attempts after 2 waits", attempts, *slept)
	}
}

func TestDialWSQuotaRefusalReturnsResponse(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":"quota","code":"quota_exceeded"}`)
	}))
	defer srv.Close()
	useTestAPI(t, srv.URL)

	_, resp, err := dialWS(t.Context(), websocket.DefaultDialer, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("err=%v resp=%v, want the 429 handed back", err, resp)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}
