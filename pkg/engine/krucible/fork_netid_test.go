//go:build krucible

package krucible

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/gateway"
)

// TestKrucibleForkNetIdentity is the fork network-identity gate.
//
// A memory fork clones the source's RAM, including its eth0 IP and MAC. The
// fork must join the owner's shared netd with fresh identities and carry
// traffic using them, not merely report a changed address in guest state.
func TestKrucibleForkNetIdentity(t *testing.T) {
	eng := newNetEngine(t)
	ke := eng.(*Engine)
	if !ke.caps.Checkpoint {
		t.Skip("bhatti-vmm build has no checkpoint support; skipping")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Second)
	defer cancel()

	src, err := eng.Create(ctx, engine.SandboxSpec{Name: "fork-netid-src", CPUs: 1, MemoryMB: 512, UserID: "forkowner",
		NetPolicy: &gateway.NetPolicyWire{Default: "deny", AllowCIDRs: []string{"1.1.1.1/32"}}})
	if err != nil {
		t.Fatalf("Create src: %v", err)
	}
	t.Cleanup(func() { eng.Destroy(context.Background(), src.ID) })

	fork, err := ke.Fork(ctx, src.ID, "fork-netid-fork")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	t.Cleanup(func() { eng.Destroy(context.Background(), fork.ID) })
	t.Logf("src.IP=%s fork.IP=%s", src.IP, fork.IP)

	// Host-side allocation must be distinct.
	if src.IP == "" || fork.IP == "" {
		t.Fatalf("net backend must report IPs: src=%q fork=%q", src.IP, fork.IP)
	}
	if fork.IP == src.IP {
		t.Fatalf("fork IP %q collides with source IP %q (host allocation not distinct)", fork.IP, src.IP)
	}

	// Guest-side: each box must present the IP the host allocated for it. Before
	// the reconcile the fork's guest still presented the source's IP (fork.IP
	// answered nothing); this is the direct proof of the fix.
	guestIP := func(id string) string {
		r, err := eng.Exec(ctx, id, []string{"netcheck", "localip"})
		if err != nil || r.ExitCode != 0 {
			t.Fatalf("localip in %s: err=%v exit=%d out=%q", id, err, r.ExitCode, strings.TrimSpace(r.Stdout))
		}
		return strings.TrimSpace(r.Stdout)
	}
	if got, want := guestIP(src.ID), src.IP; got != want {
		t.Fatalf("source guest IP = %q, want host-allocated %q", got, want)
	}
	if got, want := guestIP(fork.ID), fork.IP; got != want {
		t.Fatalf("fork guest IP = %q, want host-allocated %q — guest not reconciled to fresh identity", got, want)
	}
	srcVM, err := ke.getVM(src.ID)
	if err != nil {
		t.Fatal(err)
	}
	forkVM, err := ke.getVM(fork.ID)
	if err != nil {
		t.Fatal(err)
	}
	if srcVM.baseSpec.NetMAC == forkVM.baseSpec.NetMAC {
		t.Fatalf("fork retained source MAC %q", srcVM.baseSpec.NetMAC)
	}
	macResult, err := eng.Exec(ctx, fork.ID, []string{"netcheck", "localmac"})
	if err != nil || macResult.ExitCode != 0 || strings.TrimSpace(macResult.Stdout) != forkVM.baseSpec.NetMAC {
		t.Fatalf("fork eth0 MAC = %q, want allocated %q: err=%v exit=%d",
			strings.TrimSpace(macResult.Stdout), forkVM.baseSpec.NetMAC, err, macResult.ExitCode)
	}

	// The owner allocation must be shared, and an allowed TCP dial must
	// actually leave the fork via its own authenticated guest port.
	srcNet := src.IP[:strings.LastIndex(src.IP, ".")]
	forkNet := fork.IP[:strings.LastIndex(fork.IP, ".")]
	if srcNet != forkNet {
		t.Fatalf("fork IP %q not on the source's subnet %q — fork did not join the owner's shared netd", fork.IP, src.IP)
	}
	dial, err := eng.Exec(ctx, fork.ID, []string{"netcheck", "dial", "1.1.1.1:443"})
	if err != nil || dial.ExitCode != 0 {
		t.Fatalf("fork egress through shared netd failed: err=%v exit=%d out=%q",
			err, dial.ExitCode, strings.TrimSpace(dial.Stdout))
	}
}

// TestKrucibleForkKeepsEgressPolicy: a fork is its source's guest resumed in a
// new sandbox, so its egress must stay as narrow as the source's instead of
// coming up on the public default. It also proves the fork's reconciled NIC
// carries traffic through netd under the copied egress policy.
func TestKrucibleForkKeepsEgressPolicy(t *testing.T) {
	eng := newNetEngine(t)
	ke := eng.(*Engine)
	if !ke.caps.Checkpoint {
		t.Skip("bhatti-vmm build has no checkpoint support; skipping")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Second)
	defer cancel()

	src, err := eng.Create(ctx, engine.SandboxSpec{
		Name: "fork-policy-src", CPUs: 1, MemoryMB: 512, UserID: "policyowner",
		NetPolicy: &gateway.NetPolicyWire{Default: "deny", AllowCIDRs: []string{"1.1.1.1/32"}},
	})
	if err != nil {
		t.Fatalf("Create src: %v", err)
	}
	t.Cleanup(func() { eng.Destroy(context.Background(), src.ID) })
	fork, err := ke.Fork(ctx, src.ID, "fork-policy-fork")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	t.Cleanup(func() { eng.Destroy(context.Background(), fork.ID) })

	dial := func(addr string) (bool, string) {
		r, err := eng.Exec(ctx, fork.ID, []string{"netcheck", "dial", addr})
		if err != nil {
			t.Fatalf("exec dial %s in fork: %v", addr, err)
		}
		return r.ExitCode == 0, strings.TrimSpace(r.Stdout)
	}
	if ok, out := dial("1.1.1.1:443"); !ok {
		t.Fatalf("fork can't reach the allow-listed 1.1.1.1 (no traffic through netd): %q", out)
	}
	if ok, out := dial("8.8.8.8:443"); ok {
		t.Fatalf("fork reached 8.8.8.8: it came up without its source's deny policy: %q", out)
	}
}

// Memory snapshot resume follows the same identity reconciliation as Fork,
// but enters through the durable snapshot API rather than Fork's temp image.
func TestKrucibleNetSnapshotResumeEgress(t *testing.T) {
	eng := newNetEngine(t)
	ke := eng.(*Engine)
	if !ke.caps.Checkpoint {
		t.Skip("bhatti-vmm build has no checkpoint support; skipping")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Second)
	defer cancel()
	src, err := eng.Create(ctx, engine.SandboxSpec{
		Name: "snapshot-net-src", CPUs: 1, MemoryMB: 512, UserID: "snapshot-owner",
		NetPolicy: &gateway.NetPolicyWire{Default: "deny", AllowCIDRs: []string{"1.1.1.1/32"}},
	})
	if err != nil {
		t.Fatalf("Create source: %v", err)
	}
	t.Cleanup(func() { eng.Destroy(context.Background(), src.ID) })
	parent := t.TempDir()
	manifest, err := ke.Checkpoint(ctx, src.ID, "snapshot-owner", 0, "network", parent)
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := ke.ResumeFromManifestJSON(ctx, filepath.Join(parent, "network"), data, "snapshot-net-restore", "snapshot-owner")
	if err != nil {
		t.Fatalf("ResumeFromManifestJSON: %v", err)
	}
	t.Cleanup(func() { eng.Destroy(context.Background(), restored.ID) })
	if restored.IP == src.IP || restored.IP == "" {
		t.Fatalf("restored IP %q must be distinct from source %q", restored.IP, src.IP)
	}
	for _, check := range []struct{ mode, want string }{
		{"localip", restored.IP},
		{"localmac", netGuestMACFor(1)},
	} {
		r, err := eng.Exec(ctx, restored.ID, []string{"netcheck", check.mode})
		if err != nil || r.ExitCode != 0 || strings.TrimSpace(r.Stdout) != check.want {
			t.Fatalf("restored %s = %q, want %q: err=%v exit=%d",
				check.mode, strings.TrimSpace(r.Stdout), check.want, err, r.ExitCode)
		}
	}
	r, err := eng.Exec(ctx, restored.ID, []string{"netcheck", "dial", "1.1.1.1:443"})
	if err != nil || r.ExitCode != 0 {
		t.Fatalf("restored network egress: err=%v exit=%d out=%q",
			err, r.ExitCode, strings.TrimSpace(r.Stdout))
	}
}
