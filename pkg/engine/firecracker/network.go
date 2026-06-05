//go:build linux

package firecracker

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/sahil-shubham/bhatti/pkg/dns"
)

// UserNetwork holds the network state for a single user.
// Each user gets their own bridge and /24 subnet for L2 isolation.
type UserNetwork struct {
	BridgeName string
	GatewayIP  string // e.g. "10.0.1.1"
	Subnet     string // e.g. "10.0.1.0/24"
	Pool       *ipPool

	// DNS is the per-user DNS responder bound to GatewayIP:53. Started
	// by Engine.bringUpUserNetwork after ensureUserBridge succeeds;
	// stopped by Engine.removeUserNetworkIfEmpty before destroyUserBridge.
	// nil when the bind failed (in which case L2/L3 still works, just
	// no name resolution for this user). G1.1 of PLAN-bhatti-v2.md.
	DNS *dns.Server
}

// subnetFromIndex converts a 1-based subnet index to network parameters.
//
//	index 1   → 10.0.1.0/24,   gateway 10.0.1.1,   bridge brbhatti-1
//	index 254 → 10.0.254.0/24, gateway 10.0.254.1, bridge brbhatti-254
//	index 255 → 10.1.0.0/24,   gateway 10.1.0.1,   bridge brbhatti-255
//	max: 10.255.254.0/24 → 65,024 users, 253 VMs each
func subnetFromIndex(index int) (gateway, subnet, bridge string) {
	// Maps 1-based index to 10.{hi}.{lo}.0/24 subnets.
	// Skips 10.0.0.0/24 (lo=0 when hi=0 would be ambiguous).
	//
	//   index 1   → hi=0, lo=1   → 10.0.1.0/24
	//   index 254 → hi=0, lo=254 → 10.0.254.0/24
	//   index 255 → hi=1, lo=0   → 10.1.0.0/24
	//   index 256 → hi=1, lo=1   → 10.1.1.0/24
	//   index 509 → hi=1, lo=254 → 10.1.254.0/24
	//   index 510 → hi=2, lo=0   → 10.2.0.0/24
	//
	// Each hi value covers 255 subnets (lo 0-254).
	// index 1 maps to (0,1), so we offset by 1 to skip (0,0).
	n := index - 1 // 0-based
	hi := (n + 1) / 255
	lo := (n + 1) % 255
	// index=1 → n=0 → (0+1)/255=0, (0+1)%255=1 → (0,1) ✓
	// index=254 → n=253 → (254)/255=0, 254%255=254 → (0,254) ✓
	// index=255 → n=254 → (255)/255=1, 255%255=0 → (1,0) ✓
	// index=510 → n=509 → (510)/255=2, 510%255=0 → (2,0) ✓
	gateway = fmt.Sprintf("10.%d.%d.1", hi, lo)
	subnet = fmt.Sprintf("10.%d.%d.0/24", hi, lo)
	bridge = fmt.Sprintf("brbhatti-%d", index)
	return
}

// ipPool manages IP allocation within a /24 subnet.
// Usable range: .2 through .254 (253 addresses).
// .0 = network, .1 = bridge/gateway, .255 = broadcast.
type ipPool struct {
	mu      sync.Mutex
	used    [256]bool
	gateway string // e.g. "10.0.1.1" — needed to format full IPs
	prefix  string // e.g. "10.0.1." — for formatting
}

func newIPPool(gateway string) *ipPool {
	// Extract prefix: "10.0.1.1" → "10.0.1."
	lastDot := strings.LastIndex(gateway, ".")
	prefix := gateway[:lastDot+1]

	p := &ipPool{gateway: gateway, prefix: prefix}
	p.used[0] = true   // network
	p.used[1] = true   // gateway
	p.used[255] = true // broadcast
	return p
}

// Allocate returns the next free IP in the subnet.
func (p *ipPool) Allocate() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := 2; i < 255; i++ {
		if !p.used[i] {
			p.used[i] = true
			return fmt.Sprintf("%s%d", p.prefix, i), nil
		}
	}
	return "", fmt.Errorf("IP pool exhausted (253 VMs per user)")
}

// Release frees an IP back to the pool.
func (p *ipPool) Release(ip string) {
	var octet int
	fmt.Sscanf(ip, p.prefix+"%d", &octet)
	if octet < 2 || octet > 254 {
		return
	}
	p.mu.Lock()
	p.used[octet] = false
	p.mu.Unlock()
}

// TryAllocate attempts to allocate a specific IP. Returns error if taken.
// Used by snapshot resume which must get the exact IP from the manifest.
func (p *ipPool) TryAllocate(ip string) error {
	var octet int
	fmt.Sscanf(ip, p.prefix+"%d", &octet)
	if octet < 2 || octet > 254 {
		return fmt.Errorf("IP %s out of range", ip)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.used[octet] {
		return fmt.Errorf("IP %s is in use by another sandbox", ip)
	}
	p.used[octet] = true
	return nil
}

// Mark reserves an IP (used during startup recovery).
func (p *ipPool) Mark(ip string) {
	var octet int
	fmt.Sscanf(ip, p.prefix+"%d", &octet)
	if octet < 2 || octet > 254 {
		return
	}
	p.mu.Lock()
	p.used[octet] = true
	p.mu.Unlock()
}

// ensureUserBridge creates a user's bridge if it doesn't exist and
// installs the per-bridge iptables exemption. Idempotent — safe to
// call on every sandbox creation.
func ensureUserBridge(net *UserNetwork) error {
	runQuiet("ip", "link", "add", net.BridgeName, "type", "bridge")
	runQuiet("ip", "addr", "add", net.GatewayIP+"/24", "dev", net.BridgeName)
	if err := run("ip", "link", "set", net.BridgeName, "up"); err != nil {
		return fmt.Errorf("bring up bridge %s: %w", net.BridgeName, err)
	}
	pred := intraBridgeAllowPredicate(net.BridgeName)
	check := append([]string{"-t", "filter", "-C", "FORWARD"}, pred...)
	if runQuiet("iptables", check...) != nil {
		// Insert at position 1 so the ACCEPT shadows the cross-bridge
		// DROP that setupGlobalFirewall installs (also at position 1,
		// pushed down to position 2 after this insert).
		insert := append([]string{"-t", "filter", "-I", "FORWARD", "1"}, pred...)
		if err := run("iptables", insert...); err != nil {
			return fmt.Errorf("install intra-bridge allow rule: %w", err)
		}
	}
	return nil
}

// destroyUserBridge removes a user's bridge device and its per-bridge
// iptables exemption.
func destroyUserBridge(bridgeName string) {
	pred := intraBridgeAllowPredicate(bridgeName)
	del := append([]string{"-t", "filter", "-D", "FORWARD"}, pred...)
	runQuiet("iptables", del...)
	run("ip", "link", "del", bridgeName)
}

// intraBridgeAllowPredicate returns the iptables predicate arguments
// (the "-i <bridge> -o <bridge> -j ACCEPT" portion) that match intra-
// bridge same-user-sandbox-to-same-user-sandbox traffic. Callers prepend
// "-t filter -I/-A/-C/-D FORWARD [pos]" depending on the operation.
//
// Why this exists: setupGlobalFirewall's DROP rule on
// 10.0.0.0/8 -> 10.0.0.0/8 assumes intra-bridge traffic stays at L2 and
// never enters the FORWARD chain. That assumption holds on a pure
// bhatti host. It BREAKS when the host has bridge-nf-call-iptables=1,
// which is the universal case for any host also running Kubernetes,
// Docker, or anything else that loads br_netfilter. With br_netfilter
// active, intra-bridge L2 traffic is routed through L3 iptables; same-
// user-sandbox-to-same-user-sandbox traffic hits the cross-bridge DROP
// and is silently lost.
//
// Surfaced during the G1.3 spike: k3s worker -> k3s control plane on
// the same bhatti bridge timed out because the host (asus-i5) was also
// a k3s node, which had loaded br_netfilter.
//
// The rule is harmless when br_netfilter is OFF: no traffic matches
// because intra-bridge traffic stays at L2 and never reaches FORWARD,
// so the rule sits at counter zero forever.
func intraBridgeAllowPredicate(bridgeName string) []string {
	return []string{"-i", bridgeName, "-o", bridgeName, "-j", "ACCEPT"}
}

// setupGlobalFirewall configures isolation rules for all VM traffic.
// Called once from Engine.New(). Idempotent. 8 rules total regardless
// of user/VM count.
func setupGlobalFirewall() error {
	defaultIface := detectDefaultInterface()

	rules := []struct {
		table string
		chain string
		args  []string
	}{
		// 1. Block cross-bridge routing (user A's VMs cannot reach user B's VMs).
		// Same-bridge traffic stays at L2 (switched, never enters FORWARD).
		{"filter", "FORWARD", []string{"-s", "10.0.0.0/8", "-d", "10.0.0.0/8", "-j", "DROP"}},

		// 2. Allow VM → internet
		{"filter", "FORWARD", []string{"-s", "10.0.0.0/8", "!", "-d", "10.0.0.0/8", "-j", "ACCEPT"}},

		// 3. Allow return traffic from internet → VM
		{"filter", "FORWARD", []string{"-d", "10.0.0.0/8", "-m", "state",
			"--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"}},

		// ORDERING (in this slice == in iptables top-down): the install
		// loop below iterates this slice in REVERSE and inserts each
		// rule at position 1. That means the LAST rule iterated (= the
		// FIRST rule in this slice) ends up at the TOP of the chain.
		// So this slice reads in iptables-evaluation order: top first.
		//
		// I had this backwards in 412d82a and put port-53 ACCEPTs at
		// the END of the slice thinking they'd land at the top. They
		// landed at the BOTTOM, below the DROP NEW. Diagnostic dump
		// from a raspi-5b reproduction showed the packet hitting the
		// DROP NEW (pkt count = 1) and the ACCEPTs at 0. Reverted to
		// the right order: port-53 ACCEPTs go BEFORE the DROP NEW in
		// the slice so they're evaluated first.

		// 4. Allow return traffic from VMs to host (agent TCP responses).
		// The bhatti daemon initiates TCP connections to VMs. The SYN-ACK
		// enters INPUT with source 10.0.0.0/8. Without this rule, rule 6
		// would kill all agent connections.
		{"filter", "INPUT", []string{"-s", "10.0.0.0/8", "-m", "state",
			"--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"}},

		// 5a/5b. Allow VM → host DNS (G1.1 of PLAN-bhatti-v2.md). The
		// per-user DNS responder binds to the bridge gateway IP
		// (10.0.N.1:53). A sandbox querying its resolver opens a NEW
		// connection from 10.0.0.0/8 — without these rules, rule 6
		// drops it. Port-scoped to 53 so this doesn't widen the attack
		// surface for non-DNS host services; destination-scoped to
		// 10.0.0.0/8 keeps it from accepting random DNS traffic.
		// MUST come before rule 6 (the NEW DROP).
		{"filter", "INPUT", []string{"-s", "10.0.0.0/8", "-d", "10.0.0.0/8",
			"-p", "udp", "--dport", "53", "-j", "ACCEPT"}},
		{"filter", "INPUT", []string{"-s", "10.0.0.0/8", "-d", "10.0.0.0/8",
			"-p", "tcp", "--dport", "53", "-j", "ACCEPT"}},

		// 6. Block VM-initiated connections to host (API, SSH, everything).
		// Only NEW connections are dropped. Prevents compromised VMs from
		// reaching the bhatti API or SSH. DNS escape hatch is rules 5a/5b
		// which iptables evaluates BEFORE this DROP.
		{"filter", "INPUT", []string{"-s", "10.0.0.0/8", "-m", "state",
			"--state", "NEW", "-j", "DROP"}},

		// 6. NAT for outbound
		{"nat", "POSTROUTING", []string{"-s", "10.0.0.0/8", "-o", defaultIface,
			"-j", "MASQUERADE"}},
	}

	// Insert rules at the TOP of each chain (-I, not -A) so they take
	// precedence over UFW's default rules. UFW's ufw-before-forward chain
	// accepts all ICMP and RELATED,ESTABLISHED traffic — our DROP rules
	// must come before UFW's chains.
	//
	// Rules are inserted in reverse order because each -I inserts at
	// position 1, pushing previous rules down.
	for i := len(rules) - 1; i >= 0; i-- {
		r := rules[i]
		checkArgs := append([]string{"-t", r.table, "-C", r.chain}, r.args...)
		if runQuiet("iptables", checkArgs...) != nil {
			insertArgs := append([]string{"-t", r.table, "-I", r.chain, "1"}, r.args...)
			if err := run("iptables", insertArgs...); err != nil {
				return fmt.Errorf("iptables rule %v: %w", r.args, err)
			}
		}
	}

	os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644)
	return nil
}

// cleanupOldBridge removes the legacy single-bridge setup (192.168.137.0/24)
// from before per-user bridges. Called once during engine startup migration.
func cleanupOldBridge() {
	// Remove old bridge if it exists
	runQuiet("ip", "link", "del", "brbhatti0")

	// Remove old iptables rules (best effort, try multiple variations)
	defaultIface := detectDefaultInterface()
	runQuiet("iptables", "-t", "nat", "-D", "POSTROUTING",
		"-s", "192.168.137.0/24", "-o", defaultIface, "-j", "MASQUERADE")

	// Remove old per-bridge FORWARD rules that were inserted with -I
	// These reference brbhatti0 in -i/-o fields
	for i := 0; i < 5; i++ { // repeat to catch multiple copies
		runQuiet("iptables", "-D", "FORWARD",
			"-i", "brbhatti0", "-o", "brbhatti0", "-j", "ACCEPT")
		runQuiet("iptables", "-D", "FORWARD",
			"-i", "brbhatti0", "-o", defaultIface, "-j", "ACCEPT")
		runQuiet("iptables", "-D", "FORWARD",
			"-i", defaultIface, "-o", "brbhatti0", "-j", "ACCEPT")
	}

	// DELIBERATELY do NOT delete the 10.0.0.0/8 FORWARD/INPUT/NAT
	// rules here, even though earlier versions of this function did.
	// Those rules ARE the current rules that setupGlobalFirewall
	// installs — deleting them and re-inserting forces them to the
	// top of the chain on each Engine.New, which gets the relative
	// ordering between INPUT rules wrong on the 2nd-and-later call:
	//
	//   1st New: clean install → [RELATED, UDP53, TCP53, DROP] ✓
	//   2nd New: cleanup deletes RELATED + DROP, leaves UDP53/TCP53.
	//            setupGlobalFirewall's -C check finds UDP53/TCP53
	//            and skips them, then -I-1 inserts DROP and RELATED
	//            on top → [RELATED, DROP, UDP53, TCP53] ✗
	//
	// The 2nd-and-later order puts DROP NEW above the port-53
	// ACCEPTs, so NEW DNS queries from sandboxes match the DROP
	// first and get silently lost. setupGlobalFirewall is
	// idempotent on its own (via -C), so the delete-and-reinsert
	// dance buys nothing once we're past the 192.168.137.0/24 era
	// migration. Hunted down via raspi-5b reproducer + iptables
	// diagnostic dump on test failure.
}

func createTapDevice(sandboxID string, bridge string) (tapName string, err error) {
	tapName = "tap" + sandboxID[:8]
	if err := run("ip", "tuntap", "add", tapName, "mode", "tap"); err != nil {
		return "", fmt.Errorf("create tap: %w", err)
	}
	if err := run("ip", "link", "set", tapName, "master", bridge); err != nil {
		run("ip", "link", "del", tapName)
		return "", fmt.Errorf("add to bridge %s: %w", bridge, err)
	}
	if err := run("ip", "link", "set", tapName, "up"); err != nil {
		run("ip", "link", "del", tapName)
		return "", fmt.Errorf("bring up tap: %w", err)
	}
	return tapName, nil
}

func destroyTapDevice(tapName string) {
	run("ip", "link", "del", tapName)
}

func detectDefaultInterface() string {
	out, err := exec.Command("ip", "route", "show", "default").Output()
	if err != nil {
		return "eth0"
	}
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "dev" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return "eth0"
}

// cleanupOrphanedTapDevices removes any TAP devices prefixed with "tap" that
// don't belong to a known VM. Called on engine startup to recover from crashes.
func cleanupOrphanedTapDevices(knownTaps map[string]bool) {
	out, err := exec.Command("ip", "-o", "link", "show", "type", "tun").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimSuffix(fields[1], ":")
		if !strings.HasPrefix(name, "tap") {
			continue
		}
		if knownTaps[name] {
			continue
		}
		slog.Info("cleaning orphaned TAP", "device", name)
		run("ip", "link", "del", name)
	}
}

// cleanupAllTapDevices removes all bhatti-created TAP devices.
func cleanupAllTapDevices() {
	out, err := exec.Command("ip", "-o", "link", "show", "type", "tun").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimSuffix(fields[1], ":")
		if strings.HasPrefix(name, "tap") {
			run("ip", "link", "del", name)
		}
	}
}

// cleanupAllUserBridges removes all bhatti user bridges (brbhatti-*).
func cleanupAllUserBridges() {
	out, err := exec.Command("ip", "-o", "link", "show", "type", "bridge").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimSuffix(fields[1], ":")
		if strings.HasPrefix(name, "brbhatti-") {
			run("ip", "link", "del", name)
		}
	}
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// runQuiet runs a command suppressing stderr. Used for idempotent operations
// where "already exists" errors are expected and not useful to log.
func runQuiet(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}
