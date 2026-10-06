//go:build krucible

package krucible

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// TestKrucibleForward proves a real guest HTTP server can be reached via the
// engine's raw guest-port tunnel, without a server-local listening port.
func TestKrucibleForward(t *testing.T) {
	eng := newBlockRootEngine(t) // skips if libkrun/vmm/mke2fs unavailable

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "fwd", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := info.ID
	t.Cleanup(func() { eng.Destroy(context.Background(), id) })

	// High port: under TSI the guest shares the host's port namespace, so a
	// guest listen can't bind a port the host already uses (e.g. 8080 on a
	// dev/CI box). Pick one the host is unlikely to occupy.
	const guestPort = 18080
	de := eng.(engine.DetachedExecEngine)
	if _, _, err := de.ExecDetached(ctx, id, []string{"/bin/netcheck", "serve", fmt.Sprintf("%d", guestPort)}, "/tmp/serve.log"); err != nil {
		t.Fatalf("ExecDetached netcheck serve: %v", err)
	}

	body := guestHTTPRetry(t, eng, ctx, id, guestPort, "hello-from-guest", 25*time.Second)
	if !strings.Contains(body, "hello-from-guest") {
		t.Fatalf("tunnel response = %q, want hello-from-guest", body)
	}
}

// guestHTTPRetry connects through the same engine Tunnel used by the WebSocket
// handler and waits for the detached guest server to become available.
func guestHTTPRetry(t *testing.T, eng engine.Engine, ctx context.Context, id string, port int, want string, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	var last string
	for time.Now().Before(deadline) {
		tun, err := eng.Tunnel(ctx, id, port)
		if err == nil {
			if conn, ok := tun.(interface{ SetDeadline(time.Time) error }); ok {
				conn.SetDeadline(time.Now().Add(3 * time.Second))
			}
			_, err = io.WriteString(tun, "GET / HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")
			if err == nil {
				var resp *http.Response
				resp, err = http.ReadResponse(bufio.NewReader(tun), &http.Request{Method: http.MethodGet})
				if err == nil {
					data, readErr := io.ReadAll(resp.Body)
					resp.Body.Close()
					last = string(data)
					err = readErr
				}
			}
			tun.Close()
		}
		if err != nil {
			last = err.Error()
		} else if strings.Contains(last, want) {
			return last
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("guest tunnel %s:%d never returned %q (last: %q)", id, port, want, last)
	return ""
}
