//go:build linux

package firecracker

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// IP allocation: each sandbox gets a /30 subnet from 172.16.0.0/16.
//   CID 3 → 172.16.0.0/30:  host=172.16.0.1,  guest=172.16.0.2
//   CID 4 → 172.16.0.4/30:  host=172.16.0.5,  guest=172.16.0.6
//   CID N → base = (N-3)*4

type subnet struct {
	HostIP  string
	GuestIP string
}

func subnetForCID(cid uint32) subnet {
	offset := (cid - 3) * 4
	b3 := byte(offset >> 8)
	b4 := byte(offset & 0xFF)
	return subnet{
		HostIP:  fmt.Sprintf("172.16.%d.%d", b3, b4+1),
		GuestIP: fmt.Sprintf("172.16.%d.%d", b3, b4+2),
	}
}

func createTapDevice(sandboxID string, cid uint32) (tapName, guestIP string, err error) {
	// Max 15 chars for Linux interface name: "tap" + 8 hex chars from ID
	tapName = "tap" + sandboxID[:8]
	s := subnetForCID(cid)

	if err := run("ip", "tuntap", "add", tapName, "mode", "tap"); err != nil {
		return "", "", fmt.Errorf("create tap: %w", err)
	}
	if err := run("ip", "addr", "add", s.HostIP+"/30", "dev", tapName); err != nil {
		run("ip", "link", "del", tapName)
		return "", "", fmt.Errorf("set tap ip: %w", err)
	}
	if err := run("ip", "link", "set", tapName, "up"); err != nil {
		run("ip", "link", "del", tapName)
		return "", "", fmt.Errorf("bring up tap: %w", err)
	}

	// NAT: masquerade guest traffic through the host's default interface.
	defaultIface := detectDefaultInterface()
	if err := run("iptables", "-t", "nat", "-A", "POSTROUTING",
		"-s", s.GuestIP+"/32", "-o", defaultIface,
		"-j", "MASQUERADE"); err != nil {
		fmt.Fprintf(os.Stderr, "warning: iptables NAT failed: %v\n", err)
	}

	return tapName, s.GuestIP, nil
}

func destroyTapDevice(tapName string, cid uint32) {
	run("ip", "link", "del", tapName)

	s := subnetForCID(cid)
	defaultIface := detectDefaultInterface()
	run("iptables", "-t", "nat", "-D", "POSTROUTING",
		"-s", s.GuestIP+"/32", "-o", defaultIface,
		"-j", "MASQUERADE")
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
		// Format: "N: tapXXXXXXXX: <FLAGS>..."
		name := strings.TrimSuffix(fields[1], ":")
		if !strings.HasPrefix(name, "tap") {
			continue
		}
		if knownTaps[name] {
			continue
		}
		fmt.Fprintf(os.Stderr, "bhatti: cleaning orphaned TAP: %s\n", name)
		run("ip", "link", "del", name)
	}
}

// cleanupAllTapDevices removes all bhatti-created TAP devices.
// Called on engine shutdown.
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
