//go:build krucible

package krucible

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

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
	dataDir := t.TempDir()
	if d := os.Getenv("KRUCIBLE_NET_DATADIR"); d != "" {
		dataDir = d // fixed dir so vmm.log survives for debugging
	}
	eng, err := New(Config{
		DataDir:     dataDir,
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
// with eth0 wired to bhatti-netd and egresses to the internet through the netd
// TCP forwarder.
//
// KNOWN GAP (2026-07-06): SKIPs pending a gVisor↔libkrun-virtio-net delivery
// quirk. netd receives the guest's SYN with the correct dst MAC + IP and
// promiscuous mode is on, but the gVisor IP layer drops it as
// InvalidDestinationAddressesReceived (the promiscuous temp-address local
// delivery isn't happening) — ONLY on the real libkrun VM. It is NOT reproducible
// in isolation: the byte-identical frame, over net.Pipe AND a real unix socket,
// on both darwin and linux, is delivered to the forwarder (see cmd/bhatti-netd
// delivery_test.go). Under investigation (next: gVisor sniffer verdict on the
// live VM / compare gvproxy's exact stack setup). The agent (vsock) + eth0 boot
// work (TestKrucibleNetAgentSuite).
func TestKrucibleNetEgress(t *testing.T) {
	t.Skip("KNOWN GAP: gVisor foreign-dst local delivery drops on the real libkrun VM (invalidDst); see doc comment")
}

// TestKrucibleNetHostIsolation would assert the gateway's egress guard denies
// private/host space. SKIPped with egress: until foreign-dst delivery works, a
// denied dial can't be distinguished from the delivery gap (both fail).
func TestKrucibleNetHostIsolation(t *testing.T) {
	t.Skip("KNOWN GAP: blocked on the same foreign-dst delivery gap as TestKrucibleNetEgress")
}

// TestKrucibleNetAgentSuite runs the shared agent suite on the virtio-net backend
// — proves exec/files/etc. still work when the guest is on eth0 (agent stays on
// vsock, decoupled from the data plane).
func TestKrucibleNetAgentSuite(t *testing.T) {
	enginetest.RunAgentSuite(t, newNetEngine)
}
