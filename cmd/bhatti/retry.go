package main

import (
	"context"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// retryPolicy is the one place the CLI decides whether to retry a rate-limited
// API call; every HTTP helper (via httpClient) and websocket dial uses it.
//
// Only 429 with an integer Retry-After is retried. The server sends that header
// for rate limits, which are rejected before any handler runs (so retrying is
// safe for every method). A 429 without it is a quota refusal: waiting cannot
// help, so it is returned at once.
type retryPolicy struct {
	maxRetries int
	maxWait    time.Duration // cap on a single Retry-After wait
	jitter     time.Duration // up to this much extra, so parallel scripts spread out
	sleep      func(ctx context.Context, d time.Duration) error
}

var rateLimitRetry = retryPolicy{
	maxRetries: 3,
	maxWait:    10 * time.Second,
	jitter:     250 * time.Millisecond,
	sleep:      sleepCtx,
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// delay reports how long to wait before retry number retries+1, or false when
// resp must be handed to the caller as is.
func (p retryPolicy) delay(resp *http.Response, retries int) (time.Duration, bool) {
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests || retries >= p.maxRetries {
		return 0, false
	}
	secs, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After")))
	if err != nil || secs < 0 {
		return 0, false
	}
	// Compared in seconds first so an absurd header cannot overflow Duration.
	d := p.maxWait
	if time.Duration(secs) <= p.maxWait/time.Second {
		d = time.Duration(secs) * time.Second
	}
	if p.jitter > 0 {
		d += rand.N(p.jitter)
	}
	return d, true
}

// rateLimitTransport retries rate-limited requests. A request whose body can't
// be replayed (a stream with no GetBody) is sent once: re-sending half-consumed
// bytes would corrupt the upload.
type rateLimitTransport struct {
	inner  http.RoundTripper
	policy retryPolicy
}

func (t *rateLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	replayable := req.Body == nil || req.Body == http.NoBody || req.GetBody != nil
	for retries := 0; ; retries++ {
		resp, err := t.inner.RoundTrip(req)
		if err != nil || !replayable {
			return resp, err
		}
		d, ok := t.policy.delay(resp, retries)
		if !ok {
			return resp, nil
		}
		next := req.Clone(req.Context())
		if req.GetBody != nil {
			if next.Body, err = req.GetBody(); err != nil {
				return resp, nil
			}
		}
		// Drain so the connection can be reused for the retry.
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if err := t.policy.sleep(req.Context(), d); err != nil {
			return nil, err
		}
		req = next
	}
}

// dialWS dials an API websocket, retrying a handshake rejected with a
// retryable 429. When retries run out the last response is returned exactly as
// a plain Dial would, so callers report the server's message as before.
func dialWS(ctx context.Context, d *websocket.Dialer, endpoint string, header http.Header) (*websocket.Conn, *http.Response, error) {
	for retries := 0; ; retries++ {
		conn, resp, err := d.DialContext(ctx, endpoint, header)
		if err == nil {
			return conn, resp, nil
		}
		wait, ok := rateLimitRetry.delay(resp, retries)
		if !ok {
			return nil, resp, err
		}
		resp.Body.Close()
		if err := rateLimitRetry.sleep(ctx, wait); err != nil {
			return nil, nil, err
		}
	}
}
