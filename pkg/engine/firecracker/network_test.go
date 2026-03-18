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

	"github.com/sahilshubham/bhatti/pkg/engine"
)

// --- IP Pool unit tests (no root needed) ---

func TestIPPoolAllocRelease(t *testing.T) {
	p := newIPPool()

	// First alloc should be .2
	ip, err := p.Allocate()
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if ip != "192.168.137.2" {
		t.Errorf("first alloc = %q, want 192.168.137.2", ip)
	}

	// Second alloc should be .3
	ip2, _ := p.Allocate()
	if ip2 != "192.168.137.3" {
		t.Errorf("second alloc = %q, want 192.168.137.3", ip2)
	}

	// Release .2, next alloc should reuse it
	p.Release(ip)
	ip3, _ := p.Allocate()
	if ip3 != "192.168.137.2" {
		t.Errorf("after release got %q, want 192.168.137.2", ip3)
	}
}

func TestIPPoolExhaustion(t *testing.T) {
	p := newIPPool()

	// Allocate all 253 usable addresses (.2 through .254)
	ips := make([]string, 0, 253)
	for i := 0; i < 253; i++ {
		ip, err := p.Allocate()
		if err != nil {
			t.Fatalf("Allocate %d: %v", i, err)
		}
		ips = append(ips, ip)
	}

	// 254th should fail
	_, err := p.Allocate()
	if err == nil {
		t.Error("expected exhaustion error")
	}

	// Release all, then reallocate all — full cycle
	for _, ip := range ips {
		p.Release(ip)
	}
	for i := 0; i < 253; i++ {
		_, err := p.Allocate()
		if err != nil {
			t.Fatalf("re-Allocate %d after full release: %v", i, err)
		}
	}
	_, err = p.Allocate()
	if err == nil {
		t.Error("expected exhaustion after re-allocation")
	}
}

func TestIPPoolMark(t *testing.T) {
	p := newIPPool()

	// Mark .5 as used (simulates recovery)
	p.Mark("192.168.137.5")

	// Allocations should skip .5
	allocated := make(map[string]bool)
	for i := 0; i < 10; i++ {
		ip, _ := p.Allocate()
		if ip == "192.168.137.5" {
			t.Fatal("allocated marked IP .5")
		}
		allocated[ip] = true
	}

	// .2, .3, .4, .6, .7, .8, .9, .10, .11, .12 should be allocated
	if allocated["192.168.137.2"] != true {
		t.Error(".2 should be allocated")
	}
	if allocated["192.168.137.6"] != true {
		t.Error(".6 should be allocated")
	}
}

func TestIPPoolReleaseBoundary(t *testing.T) {
	p := newIPPool()

	// Releasing reserved addresses should be no-ops (not corrupt the pool)
	p.Release("192.168.137.0")   // network
	p.Release("192.168.137.1")   // bridge
	p.Release("192.168.137.255") // broadcast
	p.Release("10.0.0.1")        // wrong subnet — octet parse = 0 → guarded
	p.Release("garbage")         // unparseable

	// Pool should still work normally
	ip, _ := p.Allocate()
	if ip != "192.168.137.2" {
		t.Errorf("got %q after boundary releases, want .2", ip)
	}
}

func TestIPPoolDoubleRelease(t *testing.T) {
	p := newIPPool()
	ip, _ := p.Allocate()

	// Double release should not corrupt state
	p.Release(ip)
	p.Release(ip)

	// Should still get .2 back (it was released), then .3
	ip1, _ := p.Allocate()
	ip2, _ := p.Allocate()
	if ip1 != "192.168.137.2" {
		t.Errorf("after double release: got %q, want .2", ip1)
	}
	if ip2 != "192.168.137.3" {
		t.Errorf("second alloc: got %q, want .3", ip2)
	}
}

func TestIPPoolConcurrent(t *testing.T) {
	p := newIPPool()
	const goroutines = 50

	var wg sync.WaitGroup
	results := make(chan string, goroutines)

	// Allocate concurrently
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ip, err := p.Allocate()
			if err != nil {
				t.Errorf("concurrent Allocate: %v", err)
				return
			}
			results <- ip
		}()
	}
	wg.Wait()
	close(results)

	// Every IP must be unique
	seen := make(map[string]bool)
	for ip := range results {
		if seen[ip] {
			t.Errorf("duplicate IP allocated: %s", ip)
		}
		seen[ip] = true
	}
	if len(seen) != goroutines {
		t.Errorf("expected %d unique IPs, got %d", goroutines, len(seen))
	}

	// Release all concurrently
	var wg2 sync.WaitGroup
	for ip := range seen {
		wg2.Add(1)
		go func(ip string) {
			defer wg2.Done()
			p.Release(ip)
		}(ip)
	}
	wg2.Wait()

	// Should be able to reallocate all
	for i := 0; i < goroutines; i++ {
		_, err := p.Allocate()
		if err != nil {
			t.Fatalf("re-Allocate %d after concurrent release: %v", i, err)
		}
	}
}

func TestIPPoolAllocatedRangeCorrect(t *testing.T) {
	p := newIPPool()

	for i := 0; i < 253; i++ {
		ip, err := p.Allocate()
		if err != nil {
			t.Fatalf("Allocate %d: %v", i, err)
		}
		if !strings.HasPrefix(ip, "192.168.137.") {
			t.Fatalf("IP %q not in expected subnet", ip)
		}
		var octet int
		fmt.Sscanf(ip, "192.168.137.%d", &octet)
		if octet < 2 || octet > 254 {
			t.Fatalf("IP %q has octet %d outside usable range [2,254]", ip, octet)
		}
	}
}

// --- Integration tests (root + firecracker required) ---

func TestBridgeIdempotent(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("must run as root")
	}

	if err := ensureBridge(); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := ensureBridge(); err != nil {
		t.Fatalf("second call: %v", err)
	}

	// Verify bridge actually exists and has the right IP
	out, err := exec.Command("ip", "addr", "show", bridgeName).Output()
	if err != nil {
		t.Fatalf("bridge not found: %v", err)
	}
	if !strings.Contains(string(out), bridgeCIDR) {
		t.Errorf("bridge missing expected CIDR %s in:\n%s", bridgeCIDR, out)
	}
	if !strings.Contains(string(out), "UP") {
		t.Error("bridge is not UP")
	}
}

func TestTwoVMsPing(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("must run as root")
	}

	eng := testEngine(t)
	ctx := context.Background()

	info1, err := eng.Create(ctx, testSpec("ping-1"))
	if err != nil {
		t.Fatalf("Create vm1: %v", err)
	}
	defer eng.Destroy(ctx, info1.ID)

	info2, err := eng.Create(ctx, testSpec("ping-2"))
	if err != nil {
		t.Fatalf("Create vm2: %v", err)
	}
	defer eng.Destroy(ctx, info2.ID)

	// Verify IPs are different and in the right subnet
	if info1.IP == info2.IP {
		t.Fatalf("both VMs got same IP: %s", info1.IP)
	}
	if !strings.HasPrefix(info1.IP, "192.168.137.") || !strings.HasPrefix(info2.IP, "192.168.137.") {
		t.Fatalf("IPs not in bridge subnet: vm1=%s vm2=%s", info1.IP, info2.IP)
	}
	t.Logf("vm1=%s vm2=%s", info1.IP, info2.IP)

	// VM1 pings VM2
	r, err := execWithTimeout(t, eng, info1.ID, []string{"ping", "-c", "2", "-W", "3", info2.IP})
	if err != nil {
		t.Fatalf("ping from vm1→vm2: %v", err)
	}
	if r.ExitCode != 0 {
		t.Errorf("ping vm1→vm2 failed: exit=%d stdout=%q stderr=%q", r.ExitCode, r.Stdout, r.Stderr)
	} else {
		t.Log("✓ vm1→vm2 ping")
	}

	// VM2 pings VM1
	r, err = execWithTimeout(t, eng, info2.ID, []string{"ping", "-c", "2", "-W", "3", info1.IP})
	if err != nil {
		t.Fatalf("ping from vm2→vm1: %v", err)
	}
	if r.ExitCode != 0 {
		t.Errorf("ping vm2→vm1 failed: exit=%d stdout=%q stderr=%q", r.ExitCode, r.Stdout, r.Stderr)
	} else {
		t.Log("✓ vm2→vm1 ping")
	}

	// VM1 pings bridge gateway
	r, err = execWithTimeout(t, eng, info1.ID, []string{"ping", "-c", "2", "-W", "3", bridgeIP})
	if err != nil {
		t.Fatalf("ping from vm1→bridge: %v", err)
	}
	if r.ExitCode != 0 {
		t.Errorf("ping vm1→bridge failed: exit=%d stderr=%q", r.ExitCode, r.Stderr)
	} else {
		t.Log("✓ vm1→bridge ping")
	}

	// VM1→internet via masquerade (DNS + HTTPS) — not fatal if fails
	r, err = execWithTimeout(t, eng, info1.ID, []string{"sh", "-c", "curl -sf --max-time 5 https://example.com | head -c 100"})
	if err != nil {
		t.Logf("⚠ vm1→internet timed out (host may lack NAT): %v", err)
	} else if r.ExitCode != 0 {
		t.Logf("⚠ vm1→internet failed: exit=%d stderr=%q", r.ExitCode, r.Stderr)
	} else {
		t.Log("✓ vm1→internet (curl example.com)")
	}

	// Cross-VM TCP: start a python HTTP server on VM2, curl from VM1
	execWithTimeout(t, eng, info2.ID, []string{"sh", "-c",
		"echo cross-vm-ok > /tmp/index.html && cd /tmp && python3 -m http.server 7777 </dev/null >/dev/null 2>&1 &"})
	time.Sleep(1 * time.Second)
	r, _ = execWithTimeout(t, eng, info1.ID, []string{"sh", "-c", "curl -sf --max-time 5 http://" + info2.IP + ":7777/index.html"})
	if strings.Contains(r.Stdout, "cross-vm-ok") {
		t.Log("✓ cross-VM TCP works (vm1→vm2:7777)")
	} else {
		t.Errorf("cross-VM TCP: expected 'cross-vm-ok', got exit=%d out=%q err=%q", r.ExitCode, r.Stdout, r.Stderr)
	}

	// Each VM sees correct IP on eth0
	r, _ = execWithTimeout(t, eng, info1.ID, []string{"sh", "-c", "ip addr show eth0 | grep 'inet '"})
	if !strings.Contains(r.Stdout, info1.IP) {
		t.Errorf("vm1 eth0 doesn't show assigned IP %s: %s", info1.IP, r.Stdout)
	} else {
		t.Logf("✓ vm1 eth0 has %s", info1.IP)
	}

	r, _ = execWithTimeout(t, eng, info2.ID, []string{"sh", "-c", "ip addr show eth0 | grep 'inet '"})
	if !strings.Contains(r.Stdout, info2.IP) {
		t.Errorf("vm2 eth0 doesn't show assigned IP %s: %s", info2.IP, r.Stdout)
	} else {
		t.Logf("✓ vm2 eth0 has %s", info2.IP)
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

	// Verify network works before snapshot (TCP-based: curl external)
	r, _ := execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "curl -sf --max-time 5 https://example.com | head -c 50"})
	if r.ExitCode != 0 {
		t.Logf("⚠ pre-snapshot curl failed (may lack internet), testing TCP to host instead")
		r, _ = execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "echo | nc -w 2 " + bridgeIP + " 22 2>&1 | head -1"})
		if !strings.Contains(r.Stdout, "SSH") {
			t.Fatalf("pre-snapshot TCP to host:22 failed: %q", r.Stdout)
		}
	}
	t.Log("✓ pre-snapshot network works")

	// Snapshot and resume
	if err := eng.Stop(ctx, info.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := eng.Start(ctx, info.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// 1. Basic exec (no network)
	r, err = execWithTimeout(t, eng, info.ID, []string{"echo", "post-resume-ok"})
	if err != nil {
		t.Fatalf("post-resume echo failed: %v", err)
	}
	if !strings.Contains(r.Stdout, "post-resume-ok") {
		t.Fatalf("post-resume echo unexpected: %q", r.Stdout)
	}
	t.Log("✓ post-resume exec works")

	// 2. Guest IP still correct
	r, _ = execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "ip addr show eth0 | grep 'inet '"})
	if !strings.Contains(r.Stdout, info.IP) {
		t.Errorf("post-resume IP mismatch: expected %s in %q", info.IP, r.Stdout)
	} else {
		t.Logf("✓ post-resume IP still %s", info.IP)
	}

	// 3. TCP outbound works after resume (guest-initiated connection)
	// Connect to the host's SSH server through the bridge — this tests
	// the full guest→TAP→bridge→host path with a real TCP handshake.
	r, err = execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "echo | nc -w 3 " + bridgeIP + " 22 2>&1 | head -1"})
	if err != nil {
		t.Fatalf("post-resume TCP to host:22 failed: %v", err)
	}
	if !strings.Contains(r.Stdout, "SSH") {
		t.Errorf("post-resume TCP: expected SSH banner, got %q", r.Stdout)
	} else {
		t.Log("✓ post-resume TCP outbound works (guest→host:22)")
	}

	// 4. DNS resolution works after resume (tests masquerade + outbound UDP)
	r, _ = execWithTimeout(t, eng, info.ID, []string{"sh", "-c", "host example.com 2>&1 | head -3"})
	if r.ExitCode == 0 && strings.Contains(r.Stdout, "address") {
		t.Log("✓ post-resume DNS works")
	} else {
		t.Logf("⚠ post-resume DNS: exit=%d out=%q (may need internet)", r.ExitCode, r.Stdout)
	}

	// NOTE: We don't test ping after snapshot/resume. The `ping` command uses
	// raw ICMP sockets which can hang after VM resume due to stale kernel
	// timestamp state. TCP and UDP networking works correctly.
}

func TestIPReuseAfterDestroy(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("must run as root")
	}

	eng := testEngine(t)
	ctx := context.Background()

	// Create and destroy a VM, note its IP
	info1, err := eng.Create(ctx, testSpec("reuse-1"))
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	firstIP := info1.IP
	tapName := "tap" + info1.ID[:8]
	t.Logf("first VM: id=%s ip=%s tap=%s", info1.ID, firstIP, tapName)

	if err := eng.Destroy(ctx, info1.ID); err != nil {
		t.Fatalf("Destroy first: %v", err)
	}

	// Verify TAP is cleaned up
	out, _ := exec.Command("ip", "link", "show", tapName).CombinedOutput()
	if !strings.Contains(string(out), "does not exist") {
		t.Errorf("TAP %s still exists after destroy: %s", tapName, out)
	} else {
		t.Logf("✓ TAP %s cleaned up", tapName)
	}

	// Create a new VM — should get the same IP back (pool recycled it)
	info2, err := eng.Create(ctx, testSpec("reuse-2"))
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}
	defer eng.Destroy(ctx, info2.ID)

	if info2.IP != firstIP {
		t.Errorf("IP not reused: first=%s second=%s", firstIP, info2.IP)
	} else {
		t.Logf("✓ IP %s reused after destroy", firstIP)
	}

	// New VM works
	r, err := execWithTimeout(t, eng, info2.ID, []string{"echo", "reuse-works"})
	if err != nil || r.ExitCode != 0 || !strings.Contains(r.Stdout, "reuse-works") {
		t.Errorf("exec on reused IP VM failed: err=%v exit=%d out=%q", err, r.ExitCode, r.Stdout)
	} else {
		t.Log("✓ exec works on VM with reused IP")
	}
}

func TestDestroyStoppedVM(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("must run as root")
	}

	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("destroy-stopped"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	sandboxDir := eng.cfg.DataDir + "/sandboxes/" + info.ID
	tapName := "tap" + info.ID[:8]

	// Snapshot it (stopped state)
	if err := eng.Stop(ctx, info.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Verify snapshot files exist
	vm, _ := eng.getVM(info.ID)
	if _, err := os.Stat(vm.SnapMemPath); err != nil {
		t.Fatalf("snapshot mem file missing: %v", err)
	}

	// Destroy while stopped
	if err := eng.Destroy(ctx, info.ID); err != nil {
		t.Fatalf("Destroy stopped VM: %v", err)
	}

	// Verify everything is cleaned up
	if _, err := os.Stat(sandboxDir); !os.IsNotExist(err) {
		t.Errorf("sandbox dir still exists: %s", sandboxDir)
	} else {
		t.Log("✓ sandbox dir cleaned up")
	}

	out, _ := exec.Command("ip", "link", "show", tapName).CombinedOutput()
	if !strings.Contains(string(out), "does not exist") {
		t.Errorf("TAP %s still exists: %s", tapName, out)
	} else {
		t.Logf("✓ TAP %s cleaned up", tapName)
	}

	if _, err := eng.Status(ctx, info.ID); err == nil {
		t.Error("Status should fail after destroy")
	} else {
		t.Log("✓ Status returns error after destroy")
	}
}

func TestExecOnStoppedVM(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("must run as root")
	}

	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("exec-stopped"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer eng.Destroy(ctx, info.ID)

	if err := eng.Stop(ctx, info.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Exec on stopped VM should fail with a clear error
	_, err = execWithTimeout(t, eng, info.ID, []string{"echo", "hello"})
	if err == nil {
		t.Error("expected error when exec on stopped VM")
	} else {
		t.Logf("✓ exec on stopped VM fails: %v", err)
	}
}

func TestDoubleDestroy(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("must run as root")
	}

	eng := testEngine(t)
	ctx := context.Background()

	info, err := eng.Create(ctx, testSpec("double-destroy"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := eng.Destroy(ctx, info.ID); err != nil {
		t.Fatalf("first Destroy: %v", err)
	}

	// Second destroy should return error, not panic
	err = eng.Destroy(ctx, info.ID)
	if err == nil {
		t.Error("expected error on second destroy")
	} else {
		t.Logf("✓ second destroy returns error: %v", err)
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
			t.Errorf("concurrent Create %d: %v", i, r.err)
			continue
		}
		infos = append(infos, r.info)
	}
	defer func() {
		for _, info := range infos {
			eng.Destroy(ctx, info.ID)
		}
	}()

	// All IPs must be unique and in the right subnet
	ips := make(map[string]bool)
	for _, info := range infos {
		if !strings.HasPrefix(info.IP, "192.168.137.") {
			t.Errorf("IP %s not in bridge subnet", info.IP)
		}
		if ips[info.IP] {
			t.Errorf("duplicate IP: %s", info.IP)
		}
		ips[info.IP] = true
	}
	t.Logf("✓ %d VMs created concurrently with unique IPs: %v", len(infos), ips)

	// All VMs can exec
	for _, info := range infos {
		r, err := execWithTimeout(t, eng, info.ID, []string{"hostname"})
		if err != nil || r.ExitCode != 0 {
			t.Errorf("exec on %s failed: err=%v exit=%d", info.ID, err, r.ExitCode)
		}
	}
	t.Logf("✓ all %d VMs respond to exec", len(infos))

	// Cross-VM ping: first VM pings all others
	if len(infos) >= 2 {
		for i := 1; i < len(infos); i++ {
			r, _ := execWithTimeout(t, eng, infos[0].ID, []string{"ping", "-c", "1", "-W", "3", infos[i].IP})
			if r.ExitCode != 0 {
				t.Errorf("ping %s → %s failed", infos[0].IP, infos[i].IP)
			}
		}
		t.Logf("✓ cross-VM ping works across %d VMs", len(infos))
	}
}
