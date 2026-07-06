//go:build krucible

package krucible

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/engine/enginetest"
)

// newNetEngine builds a block-root engine with the virtio-net gateway backend
// (bhatti-netd) instead of TSI. Skips if bhatti-netd isn't built.
func newNetEngine(t *testing.T) engine.Engine {
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
		DataDir:     t.TempDir(),
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
// address is refused by the guard in netd's forwarder, while public egress works.
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

// TestKrucibleNetAgentSuite runs the shared agent suite on the virtio-net backend
// — proves exec/files/etc. still work when the guest is on eth0 (agent stays on
// vsock, decoupled from the data plane).
func TestKrucibleNetAgentSuite(t *testing.T) {
	enginetest.RunAgentSuite(t, newNetEngine)
}
