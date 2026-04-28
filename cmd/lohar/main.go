//go:build linux

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
)

// configEnv holds environment variables from the config drive, merged into
// every exec request's environment.
var configEnv map[string]string

// TCP port constants reuse the vsock port numbers for simplicity.
// The agent listens on both vsock AND TCP on the same port numbers.

func main() {
	if os.Getenv("LOHAR_TEST") == "1" {
		runTestMode()
		return
	}

	runAgent()
}

// runAgent runs lohar as a systemd service. systemd has already mounted
// filesystems, brought up loopback, and started the network. We handle:
// config drive, listeners, boot profile, init session.
func runAgent() {
	bootStart := time.Now()
	var bootLog strings.Builder
	bp := func(name string) {
		line := fmt.Sprintf("+%dms %s\n", time.Since(bootStart).Milliseconds(), name)
		fmt.Fprint(os.Stderr, "lohar: "+line)
		bootLog.WriteString(line)
	}
	bp("start")

	os.Setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")

	// /dev/fuse: devtmpfs creates it as 0600. Without udev (masked),
	// nobody chmods it. FUSE clients running as uid 1000 need rw access.
	os.Chmod("/dev/fuse", 0666)

	// Load config drive (/dev/vdb) — hostname, DNS, env, files, volumes.
	cfg := loadConfigDrive()
	if cfg != nil {
		hostname := "bhatti"
		if cfg.Hostname != "" {
			hostname = cfg.Hostname
		}
		syscall.Sethostname([]byte(hostname))
		os.WriteFile("/etc/hosts", []byte(
			"127.0.0.1 localhost "+hostname+"\n::1 localhost "+hostname+"\n"), 0644)
		if len(cfg.DNS) > 0 {
			applyDNS(cfg.DNS)
		} else {
			ensureResolvConf()
		}
		agentToken = cfg.Token
		configEnv = cfg.Env
		writeConfigFiles(cfg.Files)
		mountVolumes(cfg.Volumes)
		syscall.Unmount("/run/bhatti/config", 0)
		os.RemoveAll("/run/bhatti/config")
		bp("config_applied")
	} else {
		syscall.Sethostname([]byte("bhatti"))
		os.WriteFile("/etc/hosts", []byte(
			"127.0.0.1 localhost bhatti\n::1 localhost bhatti\n"), 0644)
		ensureResolvConf()
	}

	setupNetworking()
	bp("network_done")

	// Vsock listeners (work for cold boot, broken after snapshot/restore).
	lnControl, err := listenVsock(proto.VsockPortControl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lohar: vsock control: %v\n", err)
	} else {
		go acceptLoop(lnControl, handleControlConnection)
	}
	lnForward, err := listenVsock(proto.VsockPortForward)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lohar: vsock forward: %v\n", err)
	} else {
		go acceptLoop(lnForward, handleForwardConnection)
	}

	// TCP listeners (survive snapshot/restore — virtio-net is reliable).
	tcpControl, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", proto.VsockPortControl))
	if err != nil {
		fmt.Fprintf(os.Stderr, "lohar: tcp control: %v\n", err)
	} else {
		go acceptLoop(tcpControl, handleControlConnection)
	}
	tcpForward, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", proto.VsockPortForward))
	if err != nil {
		fmt.Fprintf(os.Stderr, "lohar: tcp forward: %v\n", err)
	} else {
		go acceptLoop(tcpForward, handleForwardConnection)
	}
	bp("tcp_listen")

	// Write boot timing to /run/bhatti/ (not /tmp/ — avoids tmpfiles-clean race).
	os.MkdirAll("/run/bhatti", 0755)
	os.WriteFile("/run/bhatti/boot-timing.txt", []byte(bootLog.String()), 0644)

	fmt.Fprintln(os.Stderr, "lohar: ready")

	// Run boot profile if present. Runs AFTER listeners so the VM is
	// reachable via bhatti exec even if the boot profile hangs.
	if _, err := os.Stat("/etc/bhatti/init.sh"); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cmd := exec.CommandContext(ctx, "/bin/sh", "/etc/bhatti/init.sh")
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		cmd.Env = buildEnv(map[string]string{"HOME": "/root"})
		if err := cmd.Run(); err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				fmt.Fprintf(os.Stderr, "lohar: boot profile timed out after 30s\n")
			} else {
				fmt.Fprintf(os.Stderr, "lohar: boot profile failed: %v\n", err)
			}
		}
		cancel()
	}

	// Read supplementary env from /run/bhatti/env.
	// Tier boot profiles write runtime env vars here (e.g., DISPLAY=:99
	// for the computer tier). Merged into configEnv so every subsequent
	// exec inherits them without requiring --env flags.
	if data, err := os.ReadFile("/run/bhatti/env"); err == nil {
		if configEnv == nil {
			configEnv = make(map[string]string)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if k, v, ok := strings.Cut(line, "="); ok && k != "" {
				configEnv[k] = v
			}
		}
	}

	// Init script runs as a TTY session (attachable via session ID "init").
	if cfg != nil && cfg.Init != "" {
		go runInitSession(cfg.Init, cfg.User)
	}

	// Block forever. systemd manages our lifecycle.
	select {}
}

func ensureResolvConf() {
	const path = "/etc/resolv.conf"
	// Remove any broken symlink (e.g. from systemd-resolved stub)
	os.Remove(path)
	if err := os.WriteFile(path, []byte("nameserver 1.1.1.1\nnameserver 8.8.8.8\n"), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "lohar: write resolv.conf: %v\n", err)
	}
}

func acceptLoop(ln net.Listener, handler func(net.Conn)) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		go handler(conn)
	}
}

// --- Config drive ---

// VolumeMountConfig maps a block device to a mount point inside the guest.
// This type mirrors the engine-side VolumeMountConfig — the config drive
// JSON is the contract between them. The ReadOnly field controls MS_RDONLY.
type VolumeMountConfig struct {
	Device   string `json:"device"`    // e.g. "/dev/vdc"
	Mount    string `json:"mount"`     // e.g. "/workspace"
	FS       string `json:"fs"`        // e.g. "ext4"
	ReadOnly bool   `json:"read_only"` // mount with MS_RDONLY
}

// SandboxConfig mirrors the config drive JSON structure.
type SandboxConfig struct {
	SandboxID string            `json:"sandbox_id"`
	Hostname  string            `json:"hostname"`
	Token     string            `json:"token"`
	Env       map[string]string `json:"env"`
	Files     map[string]struct {
		Content string `json:"content"` // base64-encoded
		Mode    string `json:"mode"`
	} `json:"files"`
	Volumes []VolumeMountConfig `json:"volumes"`
	Init    string              `json:"init,omitempty"`
	DNS     []string            `json:"dns"`
	User    string              `json:"user"`
}

// loadConfigDrive mounts /dev/vdb and reads config.json.
// Returns nil if /dev/vdb doesn't exist (backward compatible).
func loadConfigDrive() *SandboxConfig {
	if _, err := os.Stat("/dev/vdb"); err != nil {
		return nil
	}
	os.MkdirAll("/run/bhatti/config", 0755)
	if err := syscall.Mount("/dev/vdb", "/run/bhatti/config", "ext4",
		syscall.MS_RDONLY, ""); err != nil {
		fmt.Fprintf(os.Stderr, "lohar: mount config drive: %v\n", err)
		return nil
	}
	data, err := os.ReadFile("/run/bhatti/config/config.json")
	if err != nil {
		fmt.Fprintf(os.Stderr, "lohar: read config.json: %v\n", err)
		return nil
	}
	var cfg SandboxConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "lohar: parse config.json: %v\n", err)
		return nil
	}
	fmt.Fprintf(os.Stderr, "lohar: loaded config drive for %s\n", cfg.SandboxID)
	return &cfg
}

func applyDNS(servers []string) {
	os.Remove("/etc/resolv.conf")
	var content string
	for _, s := range servers {
		content += "nameserver " + s + "\n"
	}
	os.WriteFile("/etc/resolv.conf", []byte(content), 0644)
}

func writeConfigFiles(files map[string]struct {
	Content string `json:"content"`
	Mode    string `json:"mode"`
}) {
	for path, cf := range files {
		content, err := base64.StdEncoding.DecodeString(cf.Content)
		if err != nil {
			fmt.Fprintf(os.Stderr, "lohar: decode file %s: %v\n", path, err)
			continue
		}
		os.MkdirAll(filepath.Dir(path), 0755)
		mode, _ := strconv.ParseUint(cf.Mode, 8, 32)
		if mode == 0 {
			mode = 0644
		}
		if err := os.WriteFile(path, content, os.FileMode(mode)); err != nil {
			fmt.Fprintf(os.Stderr, "lohar: write file %s: %v\n", path, err)
			continue
		}
		// chown to lohar user (uid 1000)
		os.Chown(path, 1000, 1000)
		os.Chown(filepath.Dir(path), 1000, 1000)
	}
}

func mountVolumes(volumes []VolumeMountConfig) {
	for _, v := range volumes {
		os.MkdirAll(v.Mount, 0755)
		var flags uintptr
		if v.ReadOnly {
			flags |= syscall.MS_RDONLY
		}
		if err := syscall.Mount(v.Device, v.Mount, v.FS, flags, ""); err != nil {
			fmt.Fprintf(os.Stderr, "lohar: mount %s → %s: %v\n", v.Device, v.Mount, err)
			continue
		}
		if !v.ReadOnly {
			os.Chown(v.Mount, 1000, 1000)
		}
		fmt.Fprintf(os.Stderr, "lohar: mounted %s → %s (ro=%v)\n", v.Device, v.Mount, v.ReadOnly)
	}
}
