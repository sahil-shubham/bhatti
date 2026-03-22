//go:build linux

package firecracker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// --- IP Pool unit tests (no root needed) ---

func TestIPPoolAllocRelease(t *testing.T) {
	p := newIPPool("10.0.99.1")

	ip, err := p.Allocate()
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if ip != "10.0.99.2" {
		t.Errorf("first alloc = %q, want 10.0.99.2", ip)
	}

	ip2, _ := p.Allocate()
	if ip2 != "10.0.99.3" {
		t.Errorf("second alloc = %q, want 10.0.99.3", ip2)
	}

	p.Release(ip)
	ip3, _ := p.Allocate()
	if ip3 != "10.0.99.2" {
		t.Errorf("after release got %q, want 10.0.99.2", ip3)
	}
}

func TestIPPoolExhaustion(t *testing.T) {
	p := newIPPool("10.0.99.1")

	for i := 0; i < 253; i++ {
		_, err := p.Allocate()
		if err != nil {
			t.Fatalf("Allocate %d: %v", i, err)
		}
	}

	_, err := p.Allocate()
	if err == nil {
		t.Error("expected exhaustion error")
	}

	// Release all and reallocate
	for i := 2; i <= 254; i++ {
		p.Release(fmt.Sprintf("10.0.99.%d", i))
	}
	for i := 0; i < 253; i++ {
		if _, err := p.Allocate(); err != nil {
			t.Fatalf("re-Allocate %d: %v", i, err)
		}
	}
}

func TestIPPoolMark(t *testing.T) {
	p := newIPPool("10.0.99.1")
	p.Mark("10.0.99.5")

	for i := 0; i < 10; i++ {
		ip, _ := p.Allocate()
		if ip == "10.0.99.5" {
			t.Fatal("allocated marked IP .5")
		}
	}
}

func TestIPPoolReleaseBoundary(t *testing.T) {
	p := newIPPool("10.0.99.1")

	// Releasing reserved/invalid addresses should be no-ops
	p.Release("10.0.99.0")
	p.Release("10.0.99.1")
	p.Release("10.0.99.255")
	p.Release("192.168.1.1") // wrong subnet
	p.Release("garbage")

	ip, _ := p.Allocate()
	if ip != "10.0.99.2" {
		t.Errorf("got %q, want 10.0.99.2", ip)
	}
}

func TestIPPoolConcurrent(t *testing.T) {
	p := newIPPool("10.0.99.1")
	const goroutines = 50

	var wg sync.WaitGroup
	results := make(chan string, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ip, err := p.Allocate()
			if err != nil {
				return
			}
			results <- ip
		}()
	}
	wg.Wait()
	close(results)

	seen := make(map[string]bool)
	for ip := range results {
		if seen[ip] {
			t.Errorf("duplicate: %s", ip)
		}
		seen[ip] = true
	}
	if len(seen) != goroutines {
		t.Errorf("expected %d unique IPs, got %d", goroutines, len(seen))
	}
}

// --- Integration tests (root + firecracker required) ---

func TestUserBridgeLifecycle(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("must run as root")
	}

	net := &UserNetwork{
		BridgeName: "brbhatti-test",
		GatewayIP:  "10.99.99.1",
		Subnet:     "10.99.99.0/24",
		Pool:       newIPPool("10.99.99.1"),
	}

	// Create
	if err := ensureUserBridge(net); err != nil {
		t.Fatalf("create bridge: %v", err)
	}
	defer destroyUserBridge(net.BridgeName)

	// Verify it exists
	out, err := exec.Command("ip", "addr", "show", net.BridgeName).Output()
	if err != nil {
		t.Fatalf("bridge not found: %v", err)
	}
	if !strings.Contains(string(out), "10.99.99.1/24") {
		t.Errorf("bridge missing IP: %s", out)
	}

	// Idempotent
	if err := ensureUserBridge(net); err != nil {
		t.Fatalf("second call: %v", err)
	}

	// Destroy
	destroyUserBridge(net.BridgeName)
	_, err = exec.Command("ip", "link", "show", net.BridgeName).CombinedOutput()
	if err == nil {
		t.Error("bridge should not exist after destroy")
	}
}

func TestSameUserVMsCommunicate(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("must run as root")
	}

	eng := testEngine(t)
	ctx := context.Background()

	// Both VMs get the same UserID → same bridge → can talk
	spec1 := engine.SandboxSpec{Name: "same-user-1", CPUs: 1, MemoryMB: 512, UserID: "usr_same", SubnetIndex: 98}
	spec2 := engine.SandboxSpec{Name: "same-user-2", CPUs: 1, MemoryMB: 512, UserID: "usr_same", SubnetIndex: 98}

	info1, err := eng.Create(ctx, spec1)
	if err != nil {
		t.Fatalf("Create vm1: %v", err)
	}
	defer eng.Destroy(ctx, info1.ID)

	info2, err := eng.Create(ctx, spec2)
	if err != nil {
		t.Fatalf("Create vm2: %v", err)
	}
	defer eng.Destroy(ctx, info2.ID)

	if info1.IP == info2.IP {
		t.Fatalf("same IP: %s", info1.IP)
	}
	t.Logf("vm1=%s vm2=%s (same bridge)", info1.IP, info2.IP)

	// VM1 pings VM2
	r, err := execWithTimeout(t, eng, info1.ID, []string{"ping", "-c", "2", "-W", "3", info2.IP})
	if err != nil || r.ExitCode != 0 {
		t.Errorf("same-user ping failed: err=%v exit=%d stderr=%q", err, r.ExitCode, r.Stderr)
	} else {
		t.Log("✓ same-user VMs can communicate")
	}
}

func TestCrossUserVMsIsolated(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("must run as root")
	}

	eng := testEngine(t)
	ctx := context.Background()

	// Different UserID + SubnetIndex → different bridges → cannot talk
	specA := engine.SandboxSpec{Name: "user-a-vm", CPUs: 1, MemoryMB: 512, UserID: "usr_a", SubnetIndex: 96}
	specB := engine.SandboxSpec{Name: "user-b-vm", CPUs: 1, MemoryMB: 512, UserID: "usr_b", SubnetIndex: 97}

	infoA, err := eng.Create(ctx, specA)
	if err != nil {
		t.Fatalf("Create vmA: %v", err)
	}
	defer eng.Destroy(ctx, infoA.ID)

	infoB, err := eng.Create(ctx, specB)
	if err != nil {
		t.Fatalf("Create vmB: %v", err)
	}
	defer eng.Destroy(ctx, infoB.ID)

	t.Logf("vmA=%s (subnet 96), vmB=%s (subnet 97)", infoA.IP, infoB.IP)

	// VM A tries to ping VM B — should fail (different bridges, iptables blocks cross-bridge)
	r, _ := execWithTimeout(t, eng, infoA.ID, []string{"ping", "-c", "1", "-W", "3", infoB.IP})
	if r.ExitCode == 0 {
		t.Error("cross-user ping should fail but succeeded")
	} else {
		t.Log("✓ cross-user VMs are isolated (ping failed as expected)")
	}

	// VM B tries to ping VM A — same
	r, _ = execWithTimeout(t, eng, infoB.ID, []string{"ping", "-c", "1", "-W", "3", infoA.IP})
	if r.ExitCode == 0 {
		t.Error("cross-user ping B→A should fail but succeeded")
	} else {
		t.Log("✓ cross-user VMs isolated in both directions")
	}
}

func TestVMCannotReachHostAPI(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("must run as root")
	}

	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("no-host"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Get the bridge gateway IP for this VM's subnet
	gateway, _, _ := subnetFromIndex(99) // testSpec uses SubnetIndex=99
	t.Logf("VM=%s, gateway=%s", info.IP, gateway)

	// VM tries to connect to the gateway on port 8080 (bhatti API)
	// Rule 5 (INPUT DROP NEW) should block this
	r, _ := execWithTimeout(t, eng, info.ID, []string{
		"sh", "-c", fmt.Sprintf("curl -sf --connect-timeout 3 http://%s:8080/health 2>&1 || echo BLOCKED", gateway),
	})
	if strings.Contains(r.Stdout, "\"status\":\"ok\"") {
		t.Error("VM reached host API — INPUT DROP rule not working!")
	} else {
		t.Logf("✓ VM cannot reach host API (output: %q)", strings.TrimSpace(r.Stdout))
	}
}

func TestVMCanReachInternet(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("must run as root")
	}

	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("internet"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Test DNS + HTTPS
	r, _ := execWithTimeout(t, eng, info.ID, []string{
		"sh", "-c", "curl -sf --max-time 10 https://httpbin.org/ip | head -c 100",
	})
	if r.ExitCode == 0 && strings.Contains(r.Stdout, "origin") {
		t.Log("✓ VM can reach internet")
	} else {
		t.Logf("⚠ VM internet access: exit=%d out=%q (may lack connectivity)", r.ExitCode, r.Stdout)
	}
}

func TestBridgeCleanupOnLastDestroy(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("must run as root")
	}

	eng := testEngine(t)
	ctx := context.Background()

	spec := engine.SandboxSpec{Name: "cleanup-vm", CPUs: 1, MemoryMB: 512, UserID: "usr_cleanup", SubnetIndex: 95}
	info, err := eng.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, bridge, _ := subnetFromIndex(95)
	_ = bridge

	// Bridge should exist
	bridgeName := "brbhatti-95"
	out, err := exec.Command("ip", "link", "show", bridgeName).CombinedOutput()
	if err != nil {
		t.Fatalf("bridge should exist: %s", out)
	}
	t.Log("✓ bridge exists while VM is running")

	// Destroy VM
	eng.Destroy(ctx, info.ID)

	// Bridge should be gone (last VM for this user)
	time.Sleep(100 * time.Millisecond)
	out, err = exec.Command("ip", "link", "show", bridgeName).CombinedOutput()
	if err == nil && !strings.Contains(string(out), "does not exist") {
		t.Errorf("bridge should be destroyed after last VM: %s", out)
	} else {
		t.Log("✓ bridge destroyed after last VM")
	}
}

func TestNetworkSurvivesSnapshot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("must run as root")
	}

	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("net-snap"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	// Exec works before snapshot
	r, _ := execWithTimeout(t, eng, info.ID, []string{"echo", "pre-snap"})
	if !strings.Contains(r.Stdout, "pre-snap") {
		t.Fatalf("pre-snapshot exec failed: %q", r.Stdout)
	}
	t.Log("✓ pre-snapshot exec works")

	// Snapshot and resume
	if err := eng.Stop(ctx, info.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := eng.Start(ctx, info.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Exec works after resume
	r, err = execWithTimeout(t, eng, info.ID, []string{"echo", "post-resume"})
	if err != nil || !strings.Contains(r.Stdout, "post-resume") {
		t.Fatalf("post-resume exec: err=%v out=%q", err, r.Stdout)
	}
	t.Log("✓ post-resume exec works")

	// IP is still correct
	r, _ = execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "ip addr show eth0 | grep 'inet '"})
	if !strings.Contains(r.Stdout, info.IP) {
		t.Errorf("post-resume IP mismatch: expected %s in %q", info.IP, r.Stdout)
	} else {
		t.Logf("✓ post-resume IP intact: %s", info.IP)
	}
}

func TestIPReuseAfterDestroy(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("must run as root")
	}

	eng := testEngine(t)
	ctx := context.Background()

	info1, err := eng.Create(ctx, testSpec("reuse-1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	firstIP := info1.IP

	eng.Destroy(ctx, info1.ID)

	// Second VM should get the same IP (pool recycled)
	info2, err := eng.Create(ctx, testSpec("reuse-2"))
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}
	defer eng.Destroy(ctx, info2.ID)

	if info2.IP != firstIP {
		t.Errorf("IP not reused: first=%s second=%s", firstIP, info2.IP)
	} else {
		t.Logf("✓ IP %s reused", firstIP)
	}

	r, _ := execWithTimeout(t, eng, info2.ID, []string{"echo", "reuse-ok"})
	if !strings.Contains(r.Stdout, "reuse-ok") {
		t.Errorf("exec on reused IP failed: %q", r.Stdout)
	}
}

func TestConcurrentCreates(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("must run as root")
	}

	eng := testEngine(t)
	ctx := context.Background()
	const count = 3

	type result struct {
		info engine.SandboxInfo
		err  error
	}
	results := make(chan result, count)
	for i := 0; i < count; i++ {
		go func(i int) {
			info, err := eng.Create(ctx, testSpec(fmt.Sprintf("concurrent-%d", i)))
			results <- result{info, err}
		}(i)
	}

	var infos []engine.SandboxInfo
	for i := 0; i < count; i++ {
		r := <-results
		if r.err != nil {
			t.Errorf("Create %d: %v", i, r.err)
			continue
		}
		infos = append(infos, r.info)
	}
	defer func() {
		for _, info := range infos {
			eng.Destroy(ctx, info.ID)
		}
	}()

	// All IPs unique
	ips := make(map[string]bool)
	for _, info := range infos {
		if ips[info.IP] {
			t.Errorf("duplicate: %s", info.IP)
		}
		ips[info.IP] = true
	}
	t.Logf("✓ %d VMs with unique IPs", len(infos))

	// All can exec
	for _, info := range infos {
		r, err := execWithTimeout(t, eng, info.ID, []string{"hostname"})
		if err != nil || r.ExitCode != 0 {
			t.Errorf("exec %s: err=%v exit=%d", info.ID, err, r.ExitCode)
		}
	}
	t.Logf("✓ all %d VMs respond", len(infos))
}
