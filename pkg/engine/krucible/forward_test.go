//go:build krucible

package krucible

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/forward"
)

// TestKrucibleForward proves the host↔guest forward end to end with no mocking:
// a real guest HTTP server (netcheck serve) is reached from the host through a
// forward.Serve listener that bridges over the vsock Tunnel primitive.
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
	// Start a real HTTP server inside the guest (detached, keeps running).
	// (The toy rootfs has no `ss`, so we can't poll ListeningPorts; instead we
	// retry the forwarded GET, which fails fast until the guest server is up.)
	de := eng.(engine.DetachedExecEngine)
	if _, _, err := de.ExecDetached(ctx, id, []string{"/bin/netcheck", "serve", fmt.Sprintf("%d", guestPort)}, "/tmp/serve.log"); err != nil {
		t.Fatalf("ExecDetached netcheck serve: %v", err)
	}

	// Bridge a host port to the guest port over the vsock tunnel.
	ln, err := forward.Serve(eng, id, guestPort, "127.0.0.1:0", nil)
	if err != nil {
		t.Fatalf("forward.Serve: %v", err)
	}
	defer ln.Close()

	// Hit the host port — the response must come from inside the guest. Retry
	// until the guest's server answers: TSI shares the host's network stack, so a
	// guest connect to :8080 before netcheck has bound it can transiently fall
	// through to a host process on the same port — wait for the real guest body.
	url := "http://" + ln.Addr().String() + "/"
	body := httpGetRetry(t, url, "hello-from-guest", 25*time.Second)
	if !strings.Contains(body, "hello-from-guest") {
		t.Fatalf("forwarded response = %q, want hello-from-guest", body)
	}
}

// httpGetRetry GETs url until the response body contains want (or it times
// out). Retrying until the EXPECTED body — not just any non-empty body — makes
// the forward tests robust to the TSI host-stack fall-through described above
// and to the guest server still coming up.
func httpGetRetry(t *testing.T, url, want string, within time.Duration) string {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(within)
	var last string
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err != nil {
			last = err.Error()
			time.Sleep(300 * time.Millisecond)
			continue
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		last = string(b)
		if strings.Contains(last, want) {
			return last
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("GET %s never returned %q (last: %q)", url, want, last)
	return ""
}
