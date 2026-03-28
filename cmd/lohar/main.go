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
	"os/signal"
	"path/filepath"
	"strconv"
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

	// --- PID 1 init ---

	os.Setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	os.Setenv("HOME", "/root")

	// Mount essential filesystems.
	mustMount("proc", "/proc", "proc", 0, "")
	mustMount("sysfs", "/sys", "sysfs", 0, "")
	mustMount("devtmpfs", "/dev", "devtmpfs", 0, "")
	os.MkdirAll("/dev/pts", 0755)
	mustMount("devpts", "/dev/pts", "devpts", 0, "newinstance,ptmxmode=0666")
	mustMount("tmpfs", "/tmp", "tmpfs", 0, "")
	mustMount("tmpfs", "/run", "tmpfs", 0, "")

	// /dev/shm — required by Chromium (shared memory), Docker containers,
	// and any process using shm_open(3). Harmless when unused.
	os.MkdirAll("/dev/shm", 0755)
	mustMount("tmpfs", "/dev/shm", "tmpfs", 0, "")

	// cgroups v2 — required by Docker for resource isolation.
	// Mount unconditionally: zero overhead when unused, avoids
	// tier-specific logic in lohar.
	os.MkdirAll("/sys/fs/cgroup", 0755)
	if err := syscall.Mount("cgroup2", "/sys/fs/cgroup", "cgroup2", 0, ""); err != nil {
		fmt.Fprintf(os.Stderr, "lohar: mount cgroup2: %v\n", err)
	}
	// Enable cgroup controllers for Docker.
	os.WriteFile("/sys/fs/cgroup/cgroup.subtree_control",
		[]byte("+cpu +memory +io +pids"), 0644)

	bringUpInterface("lo")

	// Try to load config drive (/dev/vdb)
	cfg := loadConfigDrive()
	if cfg != nil {
		hostname := "bhatti"
		if cfg.Hostname != "" {
			hostname = cfg.Hostname
		}
		syscall.Sethostname([]byte(hostname))
		// Write /etc/hosts so sudo and other tools can resolve the hostname.
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

		// Unmount config drive — it contains the agent token and env vars
		// in plaintext JSON. No reason to keep it accessible after boot.
		syscall.Unmount("/run/bhatti/config", 0)
		os.RemoveAll("/run/bhatti/config")
	} else {
		syscall.Sethostname([]byte("bhatti"))
		os.WriteFile("/etc/hosts", []byte(
			"127.0.0.1 localhost bhatti\n::1 localhost bhatti\n"), 0644)
		ensureResolvConf()
	}

	setupNetworking()
	installSignalHandlers()

	// Listen on vsock (works for cold boot, broken after snapshot/restore).
	lnControl, err := listenVsock(proto.VsockPortControl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lohar: vsock control: %v\n", err)
		// Non-fatal: TCP listeners below are the primary channel.
	} else {
		go acceptLoop(lnControl, handleControlConnection)
	}
	lnForward, err := listenVsock(proto.VsockPortForward)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lohar: vsock forward: %v\n", err)
	} else {
		go acceptLoop(lnForward, handleForwardConnection)
	}

	// Listen on TCP (survives snapshot/restore since virtio-net is reliable).
	// The guest IP is configured by the kernel's ip= cmdline parameter
	// before init runs, so the interface is already up.
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

	fmt.Fprintln(os.Stderr, "lohar: ready")

	// Run boot profile if present. Runs AFTER listeners so the VM is
	// reachable via bhatti exec even if the boot profile hangs.
	// 30-second hard timeout — if dockerd or chromium can't start in 30s,
	// something is broken. Don't block forever.
	if _, err := os.Stat("/etc/bhatti/init.sh"); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cmd := exec.CommandContext(ctx, "/bin/sh", "/etc/bhatti/init.sh")
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		cmd.Env = buildEnv(nil)
		if err := cmd.Run(); err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				fmt.Fprintf(os.Stderr, "lohar: boot profile timed out after 30s\n")
			} else {
				fmt.Fprintf(os.Stderr, "lohar: boot profile failed: %v\n", err)
			}
		}
		cancel()
	}

	// Init script runs as a TTY session (can be attached to via session ID "init")
	if cfg != nil && cfg.Init != "" {
		go runInitSession(cfg.Init, cfg.User)
	}

	// PID 1 must never exit.
	select {}
}

func mustMount(source, target, fstype string, flags uintptr, data string) {
	os.MkdirAll(target, 0755)
	if err := syscall.Mount(source, target, fstype, flags, data); err != nil {
		fmt.Fprintf(os.Stderr, "lohar: mount %s on %s: %v\n", source, target, err)
	}
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

func installSignalHandlers() {
	// Note: we do NOT install a SIGCHLD handler. Go's runtime manages
	// SIGCHLD for processes started via exec.Command. A manual Wait4(-1)
	// reaper would race with cmd.Wait() and corrupt exit codes.
	// Orphan zombies (from grandchild processes) are acceptable for now.

	// SIGTERM/SIGINT: clean shutdown.
	sigterm := make(chan os.Signal, 1)
	signal.Notify(sigterm, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigterm
		syscall.Sync()
		syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF)
	}()
}
