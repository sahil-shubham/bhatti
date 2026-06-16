//go:build krucible

package krucible

import (
	"context"
	"fmt"
	"io"
	"net/http"
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

	const guestPort = 8080
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

	// Hit the host port — the response must come from inside the guest.
	url := "http://" + ln.Addr().String() + "/"
	body := httpGetRetry(t, url, 25*time.Second)
	if body != "hello-from-guest\n" {
		t.Fatalf("forwarded response = %q, want %q", body, "hello-from-guest\n")
	}
}

func httpGetRetry(t *testing.T, url string, within time.Duration) string {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(within)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err != nil {
			lastErr = err
			time.Sleep(300 * time.Millisecond)
			continue
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if len(b) == 0 {
			lastErr = fmt.Errorf("empty body")
			time.Sleep(300 * time.Millisecond)
			continue
		}
		return string(b)
	}
	t.Fatalf("GET %s never succeeded: %v", url, lastErr)
	return ""
}
