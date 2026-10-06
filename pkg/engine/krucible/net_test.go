//go:build krucible

package krucible

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/engine/enginetest"
	"github.com/sahil-shubham/bhatti/pkg/gateway"
)

// newNetEngine builds a block-root engine with the virtio-net gateway backend
// (bhatti-netd) instead of TSI. Skips if bhatti-netd isn't built.
func newNetEngine(t *testing.T) engine.Engine {
	dataDir := vmmDir(t)
	if d := os.Getenv("KRUCIBLE_NET_DATADIR"); d != "" {
		dataDir = d // fixed dir so vmm.log survives for debugging
	}
	return newNetEngineAt(t, dataDir, shortSockDir(t))
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
		NetdBinary:  netd,
		KernelImage: requireLeanKernel(t, repo),
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

	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "netegress", CPUs: 1, MemoryMB: 512,
		NetPolicy: &gateway.NetPolicyWire{Default: "public"}})
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

// TestKrucibleNetNone: a sandbox with egress "none" boots with no network
// device and no netd, and the agent still serves it over vsock.
func TestKrucibleNetNone(t *testing.T) {
	eng := newNetEngine(t)
	ke := eng.(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "netnone", CPUs: 1, MemoryMB: 512, UserID: "nonet",
		NetPolicy: &gateway.NetPolicyWire{Default: gateway.PostureNone}})
	if err != nil {
		t.Fatalf("Create(none): %v", err)
	}
	id := info.ID
	t.Cleanup(func() { eng.Destroy(context.Background(), id) })

	// The test rootfs has no ls; /proc/net/dev lists every interface ("name:").
	r, err := eng.Exec(ctx, id, []string{"cat", "/proc/net/dev"})
	if err != nil || r.ExitCode != 0 {
		t.Fatalf("exec over vsock: err=%v exit=%d", err, r.ExitCode)
	}
	var ifs []string
	for _, l := range strings.Split(r.Stdout, "\n") {
		if name, _, ok := strings.Cut(strings.TrimSpace(l), ":"); ok {
			ifs = append(ifs, name)
		}
	}
	// lo and the kernel's own dummy0 are always there; eth0 is the virtio-net NIC.
	for _, name := range ifs {
		if name == "eth0" {
			t.Fatalf("interfaces = %q: a no-network sandbox has eth0", ifs)
		}
	}
	ke.netdMu.Lock()
	n := len(ke.netds)
	ke.netdMu.Unlock()
	if n != 0 {
		t.Fatalf("%d netd instances running for a no-network sandbox", n)
	}
	if r, _ := eng.Exec(ctx, id, []string{"netcheck", "tcp"}); r.ExitCode == 0 {
		t.Fatal("egress succeeded with no network device")
	}
}

// TestKrucibleNetDNS is the DNS-egress gate: name resolution in the guest must
// work through the gateway's UDP forwarder (netcheck dns → net.LookupHost →
// UDP:53 → netd → public resolver). Before the UDP forwarder existed this
// failed — the exact "Temporary failure resolving" an apt update hit — so this
// pins it. (Uses the baked netcheck helper; the minimal test rootfs has no
// getent.)
func TestKrucibleNetDNS(t *testing.T) {
	eng := newNetEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "netdns", CPUs: 1, MemoryMB: 512,
		NetPolicy: &gateway.NetPolicyWire{Default: "public"}})
	if err != nil {
		t.Fatalf("Create(net): %v", err)
	}
	id := info.ID
	t.Cleanup(func() { eng.Destroy(context.Background(), id) })

	r, err := eng.Exec(ctx, id, []string{"netcheck", "dns"})
	if err != nil || r.ExitCode != 0 {
		t.Fatalf("guest DNS resolve failed (no UDP/DNS egress?): err=%v exit=%d out=%q",
			err, r.ExitCode, strings.TrimSpace(r.Stdout))
	}
}

// TestKrucibleNetIPReported is the IP-visibility gate for the net backend: the
// engine must REPORT the guest's gateway IP (100.64.x.y) in SandboxInfo from
// Create, Status, AND List — the value the server persists and `bhatti list`
// shows. (That eth0 is actually up carrying it is covered by TestKrucibleNetEgress,
// where traffic flows.) On TSI this is empty; on the net backend it must be set
// and identical across all three surfaces.
func TestKrucibleNetIPReported(t *testing.T) {
	eng := newNetEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "netip", CPUs: 1, MemoryMB: 512,
		NetPolicy: &gateway.NetPolicyWire{Default: "public"}})
	if err != nil {
		t.Fatalf("Create(net): %v", err)
	}
	id := info.ID
	t.Cleanup(func() { eng.Destroy(context.Background(), id) })

	if info.IP == "" || !strings.HasPrefix(info.IP, "100.64.") {
		t.Fatalf("Create SandboxInfo.IP = %q, want a 100.64.x.y gateway IP", info.IP)
	}
	st, err := eng.Status(ctx, id)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.IP != info.IP {
		t.Fatalf("Status IP = %q, want %q (from Create)", st.IP, info.IP)
	}
	list, err := eng.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, s := range list {
		if s.ID == id {
			found = true
			if s.IP != info.IP {
				t.Fatalf("List IP = %q, want %q (from Create)", s.IP, info.IP)
			}
		}
	}
	if !found {
		t.Fatalf("sandbox %s missing from List", id)
	}
}

// TestKrucibleNetHostIsolation asserts the gateway's egress guard: a guest on the
// virtio-net backend cannot reach private/host space — a dial to an RFC-1918
// address is refused by the guard in netd's forwarder.
func TestKrucibleNetHostIsolation(t *testing.T) {
	eng := newNetEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "netiso", CPUs: 1, MemoryMB: 512,
		NetPolicy: &gateway.NetPolicyWire{Default: "public"}})
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

// TestKrucibleNetForward checks guest-port tunneling on the virtio-net backend:
// a real guest HTTP server answers through Tunnel, with no host-port fall-through
// (netd gives the guest its own loopback, unlike TSI).
func TestKrucibleNetForward(t *testing.T) {
	eng := newNetEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "netfwd", CPUs: 1, MemoryMB: 512,
		NetPolicy: &gateway.NetPolicyWire{Default: "public"}})
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

	body := guestHTTPRetry(t, eng, ctx, id, guestPort, "hello-from-guest", 25*time.Second)
	if !strings.Contains(body, "hello-from-guest") {
		t.Fatalf("tunnel response = %q, want hello-from-guest", body)
	}
}

// TestKrucibleNetSiblings exercises default-denied and explicitly granted
// same-owner sibling access; another owner remains unreachable with an opt-in.
func TestKrucibleNetSiblings(t *testing.T) {
	eng := newNetEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	mk := func(name, owner, posture, sibling string) string {
		info, err := eng.Create(ctx, engine.SandboxSpec{
			Name: name, CPUs: 1, MemoryMB: 512, UserID: owner,
			NetPolicy: &gateway.NetPolicyWire{Default: posture, Siblings: sibling},
		})
		if err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
		t.Cleanup(func() { eng.Destroy(context.Background(), info.ID) })
		return info.ID
	}
	// owner1 gets two sandboxes → 100.64.0.2 (a) and 100.64.0.3 (b).
	a := mk("sib-a", "owner1", "public", "")
	b := mk("sib-b", "owner1", "deny", "")

	const port = 18090
	const bAddr = "100.64.0.3:18090"
	de := eng.(engine.DetachedExecEngine)
	if _, _, err := de.ExecDetached(ctx, b, []string{"/bin/netcheck", "serve", fmt.Sprintf("%d", port)}, "/tmp/serve.log"); err != nil {
		t.Fatalf("serve in B: %v", err)
	}

	// Deny is independent of the public/deny internet posture.
	dialOK := func(id, addr string) bool {
		for i := 0; i < 40; i++ {
			if r, err := eng.Exec(ctx, id, []string{"netcheck", "dial", addr}); err == nil && r.ExitCode == 0 {
				return true
			}
			time.Sleep(500 * time.Millisecond)
		}
		return false
	}
	r, err := eng.Exec(ctx, a, []string{"netcheck", "dial", bAddr})
	if err != nil || r.ExitCode == 0 {
		t.Fatalf("public without sibling grant reached B: err=%v exit=%d", err, r.ExitCode)
	}
	locked := mk("sib-locked", "owner1", "deny", "")
	r, err = eng.Exec(ctx, locked, []string{"netcheck", "dial", bAddr})
	if err != nil || r.ExitCode == 0 {
		t.Fatalf("deny without sibling grant reached B: err=%v exit=%d", err, r.ExitCode)
	}
	granted := mk("sib-granted", "owner1", "deny", "allow")
	if !dialOK(granted, bAddr) {
		t.Fatalf("granted sibling could not reach B at %s", bAddr)
	}

	// A different owner remains isolated even when it asks for siblings.
	c := mk("other", "owner2", "public", "allow")
	r, err = eng.Exec(ctx, c, []string{"netcheck", "dial", bAddr})
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
	dataDir := vmmDir(t)
	sockDir := shortSockDir(t)
	eng1 := newNetEngineAt(t, dataDir, sockDir)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	info, err := eng1.Create(ctx, engine.SandboxSpec{Name: "rec", CPUs: 1, MemoryMB: 512, UserID: "recowner",
		NetPolicy: &gateway.NetPolicyWire{Default: "public"}})
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

// TestKrucibleNetEgressPolicy is the per-sandbox network-rules gate: a policy
// pushed via spec.NetPolicy (deny-by-default, allow only 1.1.1.1/32) travels
// create -> engine -> the netd control channel -> the guest's forwarder, so the
// guest reaches the allow-listed address and is denied everything else.
func TestKrucibleNetEgressPolicy(t *testing.T) {
	eng := newNetEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	info, err := eng.Create(ctx, engine.SandboxSpec{
		Name: "netpol", CPUs: 1, MemoryMB: 512,
		NetPolicy: &gateway.NetPolicyWire{Default: "deny", AllowCIDRs: []string{"1.1.1.1/32"}},
	})
	if err != nil {
		t.Fatalf("Create(net+policy): %v", err)
	}
	id := info.ID
	t.Cleanup(func() { eng.Destroy(context.Background(), id) })

	// The allow-listed address is reachable.
	if r, err := eng.Exec(ctx, id, []string{"netcheck", "dial", "1.1.1.1:443"}); err != nil || r.ExitCode != 0 {
		t.Fatalf("allow-listed 1.1.1.1 should be reachable under deny+allow: err=%v exit=%d out=%q",
			err, r.ExitCode, strings.TrimSpace(r.Stdout))
	}
	// Everything else is denied by the default posture — proving the pushed
	// policy is actually enforced per-guest (not the global public default).
	r, err := eng.Exec(ctx, id, []string{"netcheck", "dial", "8.8.8.8:443"})
	if err != nil {
		t.Fatalf("exec dial 8.8.8.8: %v", err)
	}
	if r.ExitCode == 0 {
		t.Fatalf("8.8.8.8 must be denied under deny + allow-only-1.1.1.1 (egress policy not enforced): %q",
			strings.TrimSpace(r.Stdout))
	}
}

// TestKrucibleNetAllowHostByName is the F18 regression: under deny with an
// allow-host rule, the guest resolves the allowed name through netd and can
// connect to it, while a name that isn't allowed doesn't even resolve. Before
// netd tracked which name an IP came from, the allowed host was blocked too.
func TestKrucibleNetAllowHostByName(t *testing.T) {
	eng := newNetEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	info, err := eng.Create(ctx, engine.SandboxSpec{
		Name: "allowhost", CPUs: 1, MemoryMB: 512,
		NetPolicy: &gateway.NetPolicyWire{Default: "deny", AllowHosts: []string{"example.com"}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := info.ID
	t.Cleanup(func() { eng.Destroy(context.Background(), id) })

	if r, err := eng.Exec(ctx, id, []string{"netcheck", "dial", "example.com:443"}); err != nil || r.ExitCode != 0 {
		t.Fatalf("allow-listed example.com unreachable: err=%v exit=%d out=%q", err, r.ExitCode, strings.TrimSpace(r.Stdout))
	}
	r, err := eng.Exec(ctx, id, []string{"netcheck", "dial", "github.com:443"})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if r.ExitCode == 0 {
		t.Fatalf("github.com reachable under deny + allow-only-example.com: %q", strings.TrimSpace(r.Stdout))
	}
}

// TestKrucibleNetdRespawnsCtllessAdopt guards the daemon-upgrade case: a live
// netd that serves traffic but predates the control channel (has n.sock, no
// ctl.sock) must be replaced, not adopted — otherwise per-sandbox egress policy
// is silently dropped and guests run on the open default.
func TestKrucibleNetdRespawnsCtllessAdopt(t *testing.T) {
	e := newNetEngine(t).(*Engine)

	dir, err := os.MkdirTemp("/tmp", "kr-ctlless")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "n.sock")
	ctlSock := filepath.Join(dir, "ctl.sock")

	// Stand in for the old netd: a live process + an n.sock (serving traffic),
	// but no ctl.sock (no control channel).
	dummy := exec.Command("sleep", "30")
	dummy.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := dummy.Start(); err != nil {
		t.Fatal(err)
	}
	oldPid := dummy.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-oldPid, syscall.SIGKILL) })
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	owner := "ctlless-owner"
	e.netdMu.Lock()
	e.netds[owner] = &netdInstance{owner: owner, sock: sock, ctlSock: ctlSock, dir: dir, subnetIdx: 7, pid: oldPid, refs: 1}
	e.netdMu.Unlock()

	if err := e.ensureNetd(owner); err != nil {
		t.Fatalf("ensureNetd: %v", err)
	}

	// ensureNetd SIGKILLed the stale netd; reap our child so it isn't left a
	// zombie (in production it's reparented to init, which reaps it).
	_ = dummy.Wait()

	e.netdMu.Lock()
	inst := e.netds[owner]
	e.netdMu.Unlock()
	t.Cleanup(func() {
		if inst.pid > 0 {
			_ = syscall.Kill(-inst.pid, syscall.SIGKILL)
		}
	})

	if pidAlive(oldPid) {
		t.Error("stale ctl-less netd was not killed")
	}
	if inst.pid == oldPid || !pidAlive(inst.pid) {
		t.Errorf("netd was not respawned: pid=%d (old=%d)", inst.pid, oldPid)
	}
	if _, serr := os.Stat(ctlSock); serr != nil {
		t.Errorf("respawned netd has no control socket: %v", serr)
	}
}

// TestKrucibleNetdConfined: the netd the engine spawns (as root here) gives up
// root once its sockets are open: not uid 0, no capabilities, no_new_privs, and
// no filesystem writes — while still carrying the guest's traffic.
func TestKrucibleNetdConfined(t *testing.T) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("netd confinement is Linux-only and needs the engine to start it as root")
	}
	eng := newNetEngine(t)
	ke := eng.(*Engine)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	info, err := eng.Create(ctx, engine.SandboxSpec{Name: "netdconf", CPUs: 1, MemoryMB: 512, UserID: "confined",
		NetPolicy: &gateway.NetPolicyWire{Default: "public"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { eng.Destroy(context.Background(), info.ID) })
	if r, err := eng.Exec(ctx, info.ID, []string{"netcheck", "dns"}); err != nil || r.ExitCode != 0 {
		t.Fatalf("guest DNS through a confined netd: err=%v exit=%d", err, r.ExitCode)
	}

	ke.netdMu.Lock()
	var pid int
	for _, n := range ke.netds {
		n.mu.Lock()
		pid = n.pid
		n.mu.Unlock()
	}
	ke.netdMu.Unlock()
	if pid == 0 {
		t.Fatal("no netd running")
	}
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatal(err)
	}
	field := func(name string) string {
		for _, l := range strings.Split(string(status), "\n") {
			if v, ok := strings.CutPrefix(l, name+":"); ok {
				return strings.Fields(v)[0]
			}
		}
		return ""
	}
	if field("Uid") == "0" {
		t.Fatal("netd still runs as root")
	}
	if got := field("CapEff"); strings.Trim(got, "0") != "" {
		t.Fatalf("netd has effective capabilities %s", got)
	}
	if field("NoNewPrivs") != "1" {
		t.Fatal("netd lacks no_new_privs")
	}
}
