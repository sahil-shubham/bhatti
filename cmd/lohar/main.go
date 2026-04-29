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
	"strings"
	"syscall"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
)

// configEnv holds environment variables from the config drive, merged into
// every exec request's environment.
var configEnv map[string]string

func main() {
	// Busybox pattern: check how we were invoked.
	switch filepath.Base(os.Args[0]) {
	case "systemctl":
		runSystemctl(os.Args[1:])
		return
	}

	if os.Getenv("LOHAR_TEST") == "1" {
		runTestMode()
		return
	}

	runAgent()
}

// runAgent is the main init + agent loop. lohar runs as PID 1:
// mounts filesystems, configures the system, starts listeners,
// starts enabled services, then handles exec/shell/file requests.
func runAgent() {
	bootStart := time.Now()
	var bootLog strings.Builder
	bp := func(name string) {
		line := fmt.Sprintf("+%dms %s\n", time.Since(bootStart).Milliseconds(), name)
		fmt.Fprint(os.Stderr, "lohar: boot "+line)
		bootLog.WriteString(line)
	}
	bp("start")

	os.Setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	os.Setenv("HOME", "/root")

	// --- PID 1 init duties ---

	mustMount("proc", "/proc", "proc", 0, "")
	mustMount("sysfs", "/sys", "sysfs", 0, "")
	mustMount("devtmpfs", "/dev", "devtmpfs", 0, "")
	os.Chmod("/dev/fuse", 0666)
	os.MkdirAll("/dev/pts", 0755)
	mustMount("devpts", "/dev/pts", "devpts", 0, "newinstance,ptmxmode=0666")
	mustMount("tmpfs", "/tmp", "tmpfs", 0, "")
	mustMount("tmpfs", "/run", "tmpfs", 0, "")
	os.MkdirAll("/dev/shm", 0755)
	mustMount("tmpfs", "/dev/shm", "tmpfs", 0, "")
	bp("mounts_done")

	// cgroups v2 — required by Docker for resource isolation.
	os.MkdirAll("/sys/fs/cgroup", 0755)
	if err := syscall.Mount("cgroup2", "/sys/fs/cgroup", "cgroup2", 0, ""); err != nil {
		fmt.Fprintf(os.Stderr, "lohar: mount cgroup2: %v\n", err)
	}
	os.WriteFile("/sys/fs/cgroup/cgroup.subtree_control",
		[]byte("+cpu +memory +io +pids"), 0644)

	bringUpInterface("lo")
	bp("lo_up")

	// Create runtime directories.
	// /run/systemd/system: deb-systemd-helper checks for this to decide
	// whether to use the systemctl enable/disable path. Without it,
	// package installs silently skip service enablement.
	// /run/bhatti/services: PID files for services managed by the shim.
	os.MkdirAll("/run/systemd/system", 0755)
	os.MkdirAll("/run/bhatti/services", 0755)

	// --- Config drive ---

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

	// --- Signal handlers + zombie reaping ---

	installSignalHandlers()
	go reapZombies()

	// --- Listeners ---

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

	// --- Boot timing ---

	os.WriteFile("/run/bhatti/boot-timing.txt", []byte(bootLog.String()), 0644)
	fmt.Fprintln(os.Stderr, "lohar: ready")

	// --- Start enabled services ---
	// Read /etc/systemd/system/multi-user.target.wants/ and start each
	// service. This replaces systemd's multi-user.target activation.

	startEnabledServices()
	bp("services_started")

	// --- Boot profile ---

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

	// --- Supplementary env ---

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

	// --- Init session ---

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

func installSignalHandlers() {
	sigterm := make(chan os.Signal, 1)
	signal.Notify(sigterm, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigterm
		syscall.Sync()
		syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF)
	}()
}

// reapZombies reaps orphaned child processes. As PID 1, lohar is
// responsible for waiting on all orphans to prevent zombie accumulation.
// Go's runtime handles SIGCHLD for processes started via exec.Command,
// but grandchild processes (e.g. services started by the systemctl shim,
// daemons that double-fork) need explicit reaping.
func reapZombies() {
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		if err != nil || pid <= 0 {
			time.Sleep(1 * time.Second)
			continue
		}
	}
}

// --- Config drive ---

type VolumeMountConfig struct {
	Device   string `json:"device"`
	Mount    string `json:"mount"`
	FS       string `json:"fs"`
	ReadOnly bool   `json:"read_only"`
}

type SandboxConfig struct {
	SandboxID string            `json:"sandbox_id"`
	Hostname  string            `json:"hostname"`
	Token     string            `json:"token"`
	Env       map[string]string `json:"env"`
	Files     map[string]struct {
		Content string `json:"content"`
		Mode    string `json:"mode"`
	} `json:"files"`
	Volumes []VolumeMountConfig `json:"volumes"`
	Init    string              `json:"init,omitempty"`
	DNS     []string            `json:"dns"`
	User    string              `json:"user"`
}

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
