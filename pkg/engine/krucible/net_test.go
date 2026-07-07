//go:build krucible

package krucible

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/engine/enginetest"
	"github.com/sahil-shubham/bhatti/pkg/forward"
)

// newNetEngine builds a block-root engine with the virtio-net gateway backend
// (bhatti-netd) instead of TSI. Skips if bhatti-netd isn't built.
func newNetEngine(t *testing.T) engine.Engine {
	dataDir := t.TempDir()
	if d := os.Getenv("KRUCIBLE_NET_DATADIR"); d != "" {
		dataDir = d // fixed dir so vmm.log survives for debugging
	}
	return newNetEngineAt(t, dataDir, t.TempDir())
}

// newNetEngineAt builds a net-backend engine on explicit dataDir + sockDir, so a
// recovery test can spin up a SECOND engine over the same state (simulating a
// daemon restart). Skips if the net backend prerequisites aren't present.
func newNetEngineAt(t *testing.T, dataDir, sockDir string) engine.Engine {
	repo := repoRoot(t)
	if !hasLibkrun() {
		t.Skip("libkrun not installed; skipping")
	}
	if !hasHypervisor() {
		t.Skip("no hypervisor (/dev/kvm or HVF); skipping")
	}
	if _, err := exec.LookPath("mke2fs"); err != nil {
		t.Skip("mke2fs not found; skipping")
	}
	vmm := filepath.Join(repo, "bhatti-vmm")
	if _, err := os.Stat(vmm); err != nil {
		t.Skip("bhatti-vmm not built — run `make vmm`; skipping")
	}
	netd := filepath.Join(repo, "bhatti-netd")
	if _, err := os.Stat(netd); err != nil {
		t.Skip("bhatti-netd not built (go build ./cmd/bhatti-netd); skipping")
	}
	ensureVMMSigned(t, vmm)
	eng, err := New(Config{
		DataDir:     dataDir,
		SocketDir:   sockDir,
		BaseRootfs:  buildBaseRootfs(t, repo),
		VMMBinary:   vmm,
		LibDir:      libDir(),
		BlockRoot:   true,
		NetBackend:  true,
		NetdBinary:  netd,
		KernelImage: os.Getenv("KRUCIBLE_LEAN_KERNEL"),
	})
	if err != nil {
		t.Fatalf("New(net): %v", err)
	}
	return eng
}

// TestKrucibleNetEgress is the virtio-net gateway end-to-end gate: a guest boots
// with eth0 wired to bhatti-netd, and its TCP egress to the public internet flows
// guest → eth0 → netstack → TCP forwarder → guard dialer → upstream.
func TestKrucibleNetEgress(t *testing.T) {
	eng := newNetEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "netegress", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("Create(net): %v", err)
	}
	id := info.ID
	t.Cleanup(func() { eng.Destroy(context.Background(), id) })

	// Public egress works through the gateway (eth0 up + netstack + forwarder).
	if r, err := eng.Exec(ctx, id, []string{"netcheck", "tcp"}); err != nil || r.ExitCode != 0 {
		t.Fatalf("guest egress via netd failed: err=%v exit=%d out=%q", err, r.ExitCode, strings.TrimSpace(r.Stdout))
	}
}

// TestKrucibleNetHostIsolation asserts the gateway's egress guard: a guest on the
// virtio-net backend cannot reach private/host space — a dial to an RFC-1918
// address is refused by the guard in netd's forwarder.
func TestKrucibleNetHostIsolation(t *testing.T) {
	eng := newNetEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "netiso", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("Create(net): %v", err)
	}
	id := info.ID
	t.Cleanup(func() { eng.Destroy(context.Background(), id) })

	// A private/host destination must be denied by the gateway guard.
	r, err := eng.Exec(ctx, id, []string{"netcheck", "dial", "10.0.0.1:80"})
	if err != nil {
		t.Fatalf("exec netcheck dial: %v", err)
	}
	if r.ExitCode == 0 {
		t.Fatalf("isolation breach: guest reached private 10.0.0.1 through the gateway: %q", strings.TrimSpace(r.Stdout))
	}
}

// TestKrucibleNetForward is the inbound port-forward (dev-loop) gate on the
// virtio-net backend: a real guest HTTP server is reached from the host through
// forward.Serve over the vsock Tunnel. Under netd the guest has its OWN loopback
// (unlike TSI's shared host stack), so there is no host-port fall-through — the
// forwarded response can only come from inside the guest.
func TestKrucibleNetForward(t *testing.T) {
	eng := newNetEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "netfwd", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("Create(net): %v", err)
	}
	id := info.ID
	t.Cleanup(func() { eng.Destroy(context.Background(), id) })

	const guestPort = 18080
	de := eng.(engine.DetachedExecEngine)
	if _, _, err := de.ExecDetached(ctx, id, []string{"/bin/netcheck", "serve", fmt.Sprintf("%d", guestPort)}, "/tmp/serve.log"); err != nil {
		t.Fatalf("ExecDetached netcheck serve: %v", err)
	}

	ln, err := forward.Serve(eng, id, guestPort, "127.0.0.1:0", nil)
	if err != nil {
		t.Fatalf("forward.Serve: %v", err)
	}
	defer ln.Close()

	url := "http://" + ln.Addr().String() + "/"
	if body := httpGetRetry(t, url, "hello-from-guest", 25*time.Second); !strings.Contains(body, "hello-from-guest") {
		t.Fatalf("forwarded response = %q, want hello-from-guest", body)
	}
}

// TestKrucibleNetSiblings is the sibling-reachability gate: two sandboxes of the
// SAME owner share one bhatti-netd and reach each other across the L2 switch,
// while a sandbox of a DIFFERENT owner (separate netd) cannot.
func TestKrucibleNetSiblings(t *testing.T) {
	eng := newNetEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	mk := func(name, owner string) string {
		info, err := eng.Create(ctx, engine.SandboxSpec{Name: name, CPUs: 1, MemoryMB: 512, UserID: owner})
		if err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
		t.Cleanup(func() { eng.Destroy(context.Background(), info.ID) })
		return info.ID
	}
	// owner1 gets two sandboxes → 100.64.0.2 (a) and 100.64.0.3 (b).
	a := mk("sib-a", "owner1")
	b := mk("sib-b", "owner1")

	const port = 18090
	const bAddr = "100.64.0.3:18090"
	de := eng.(engine.DetachedExecEngine)
	if _, _, err := de.ExecDetached(ctx, b, []string{"/bin/netcheck", "serve", fmt.Sprintf("%d", port)}, "/tmp/serve.log"); err != nil {
		t.Fatalf("serve in B: %v", err)
	}

	// A reaches B across the sibling link (retry until B's server is up).
	dialOK := func(id, addr string) bool {
		for i := 0; i < 40; i++ {
			if r, err := eng.Exec(ctx, id, []string{"netcheck", "dial", addr}); err == nil && r.ExitCode == 0 {
				return true
			}
			time.Sleep(500 * time.Millisecond)
		}
		return false
	}
	if !dialOK(a, bAddr) {
		t.Fatalf("sibling A could not reach B at %s", bAddr)
	}

	// A different owner is isolated: separate netd, must NOT reach B's address.
	c := mk("other", "owner2")
	r, err := eng.Exec(ctx, c, []string{"netcheck", "dial", bAddr})
	if err != nil {
		t.Fatalf("exec dial from C: %v", err)
	}
	if r.ExitCode == 0 {
		t.Fatalf("isolation breach: non-sibling C reached B at %s", bAddr)
	}
}

// readNetdPid reads the pid from the (single) per-owner netd record under sockDir.
func readNetdPid(t *testing.T, sockDir string) int {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(sockDir, "netd-*", "netd.json"))
	if len(m) == 0 {
		return 0
	}
	data, err := os.ReadFile(m[0])
	if err != nil {
		return 0
	}
	var rec netdRecord
	if json.Unmarshal(data, &rec) != nil {
		return 0
	}
	return rec.Pid
}

// TestKrucibleNetRecovery is the daemon-restart gate for the net backend: the
// per-owner bhatti-netd survives a restart and is RE-ADOPTED (not respawned onto
// the socket it still holds), so a recovered sandbox keeps its networking; and
// it's still reference-counted, so destroying the owner's last sandbox tears it
// down.
func TestKrucibleNetRecovery(t *testing.T) {
	dataDir := t.TempDir()
	sockDir := t.TempDir()
	eng1 := newNetEngineAt(t, dataDir, sockDir)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	info, err := eng1.Create(ctx, engine.SandboxSpec{Name: "rec", CPUs: 1, MemoryMB: 512, UserID: "recowner"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := info.ID

	pid := readNetdPid(t, sockDir)
	if pid == 0 || !pidAlive(pid) {
		t.Fatalf("netd not running after create (pid=%d)", pid)
	}
	if r, err := eng1.Exec(ctx, id, []string{"netcheck", "tcp"}); err != nil || r.ExitCode != 0 {
		t.Fatalf("pre-restart egress: err=%v exit=%d", err, r.ExitCode)
	}

	// Simulate a daemon restart: a fresh engine over the same DataDir + SocketDir.
	eng2 := newNetEngineAt(t, dataDir, sockDir)
	t.Cleanup(func() { eng2.Destroy(context.Background(), id) })

	if _, err := eng2.Status(ctx, id); err != nil {
		t.Fatalf("sandbox not recovered: %v", err)
	}
	if p2 := readNetdPid(t, sockDir); p2 != pid || !pidAlive(pid) {
		t.Fatalf("netd not adopted across restart (pid %d -> %d, alive=%v)", pid, p2, pidAlive(pid))
	}
	if r, err := eng2.Exec(ctx, id, []string{"netcheck", "tcp"}); err != nil || r.ExitCode != 0 {
		t.Fatalf("post-restart egress failed — recovered guest lost networking: err=%v exit=%d out=%q", err, r.ExitCode, strings.TrimSpace(r.Stdout))
	}

	if err := eng2.Destroy(context.Background(), id); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if pidAlive(pid) {
		t.Fatalf("netd not torn down after the owner's last sandbox was destroyed (pid %d)", pid)
	}
}

// TestKrucibleNetAgentSuite runs the shared agent suite on the virtio-net backend
// — proves exec/files/etc. still work when the guest is on eth0 (agent stays on
// vsock, decoupled from the data plane).
func TestKrucibleNetAgentSuite(t *testing.T) {
	enginetest.RunAgentSuite(t, newNetEngine)
}
