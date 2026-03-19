//go:build linux

package firecracker

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
)

const (
	bridgeName = "brbhatti0"
	bridgeIP   = "192.168.137.1"
	bridgeCIDR = "192.168.137.1/24"
	subnetCIDR = "192.168.137.0/24"
)

// ipPool manages IP allocation within the bridge subnet.
// Usable range: .2 through .254 (253 addresses).
// .0 = network, .1 = bridge, .255 = broadcast.
type ipPool struct {
	mu   sync.Mutex
	used [256]bool
}

func newIPPool() *ipPool {
	p := &ipPool{}
	p.used[0] = true   // network
	p.used[1] = true   // bridge
	p.used[255] = true // broadcast
	return p
}

// Allocate returns the next free IP in the 192.168.137.0/24 range.
func (p *ipPool) Allocate() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := 2; i < 255; i++ {
		if !p.used[i] {
			p.used[i] = true
			return fmt.Sprintf("192.168.137.%d", i), nil
		}
	}
	return "", fmt.Errorf("IP pool exhausted (253 sandboxes)")
}

// Release frees an IP back to the pool.
func (p *ipPool) Release(ip string) {
	var octet int
	fmt.Sscanf(ip, "192.168.137.%d", &octet)
	if octet < 2 || octet > 254 {
		return
	}
	p.mu.Lock()
	p.used[octet] = false
	p.mu.Unlock()
}

// Mark reserves an IP (used during startup recovery).
func (p *ipPool) Mark(ip string) {
	var octet int
	fmt.Sscanf(ip, "192.168.137.%d", &octet)
	if octet < 2 || octet > 254 {
		return
	}
	p.mu.Lock()
	p.used[octet] = true
	p.mu.Unlock()
}

// ensureBridge creates the bridge and masquerade rule if they don't exist.
// Idempotent — safe to call on every engine startup.
func ensureBridge() error {
	// Create bridge (ignore error if exists)
	runQuiet("ip", "link", "add", bridgeName, "type", "bridge")
	// Add address (ignore error if already set)
	runQuiet("ip", "addr", "add", bridgeCIDR, "dev", bridgeName)
	if err := run("ip", "link", "set", bridgeName, "up"); err != nil {
		return fmt.Errorf("bring up bridge: %w", err)
	}

	// Enable IP forwarding
	os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644)

	// Add masquerade rule if not present
	defaultIface := detectDefaultInterface()
	if err := runQuiet("iptables", "-t", "nat", "-C", "POSTROUTING",
		"-s", subnetCIDR, "-o", defaultIface, "-j", "MASQUERADE"); err != nil {
		if err := run("iptables", "-t", "nat", "-A", "POSTROUTING",
			"-s", subnetCIDR, "-o", defaultIface, "-j", "MASQUERADE"); err != nil {
			return fmt.Errorf("add masquerade rule: %w", err)
		}
	}

	// Add FORWARD rules for bridge traffic. Required when FORWARD policy is
	// DROP (e.g. Kubernetes sets this). Insert at top (-I) so they take
	// priority over any DROP rules added by kube-router, etc.
	forwardRules := [][5]string{
		{"-i", bridgeName, "-o", bridgeName, "-j"},  // bridge ↔ bridge (inter-VM)
		{"-i", bridgeName, "-o", defaultIface, "-j"}, // VM → internet
		{"-i", defaultIface, "-o", bridgeName, "-j"}, // internet → VM (return traffic)
	}
	for _, r := range forwardRules {
		if err := runQuiet("iptables", "-C", "FORWARD",
			r[0], r[1], r[2], r[3], r[4], "ACCEPT"); err != nil {
			runQuiet("iptables", "-I", "FORWARD", "1",
				r[0], r[1], r[2], r[3], r[4], "ACCEPT")
		}
	}

	return nil
}

func createTapDevice(sandboxID string) (tapName string, err error) {
	tapName = "tap" + sandboxID[:8]

	if err := run("ip", "tuntap", "add", tapName, "mode", "tap"); err != nil {
		return "", fmt.Errorf("create tap: %w", err)
	}
	if err := run("ip", "link", "set", tapName, "master", bridgeName); err != nil {
		run("ip", "link", "del", tapName)
		return "", fmt.Errorf("add to bridge: %w", err)
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
