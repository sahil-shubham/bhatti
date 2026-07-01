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
	case "journalctl":
		runJournalctl(os.Args[1:])
		return
	}

	// Verb dispatch: argv[1] subcommand. This is a different axis than
	// the busybox-style argv[0] dispatch above — `lohar spawn` is a
	// private supervisor primitive (called by startDaemon to fix the
	// cgroup-placement race for forking daemons), not a user-facing
	// verb. Deliberately not symlinked into PATH. See cmd/lohar/spawn.go.
	if len(os.Args) > 1 && os.Args[1] == "spawn" {
		runSpawn(os.Args[2:])
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
	// SAFETY GUARD: lohar's runAgent does PID-1 things (mounts /proc,
	// installs a SIGTERM handler that calls reboot(POWER_OFF), brings
	// up loopback, starts every enabled service). All of that is only
	// safe inside a Firecracker microVM where lohar IS PID 1. Running
	// it on a real host as root has powered off two Pi5 machines
	// in this project's history. Refuse to proceed unless we really
	// are PID 1.
	if os.Getpid() != 1 {
		fmt.Fprintf(os.Stderr, "lohar: refusing to runAgent: not PID 1 (PID=%d). "+
			"This binary's agent path is for use inside a Firecracker VM as init. "+
			"For systemctl/journalctl invocations, the busybox dispatch routes "+
			"based on argv[0]; symlink /usr/bin/systemctl -> lohar to use those.\n",
			os.Getpid())
		os.Exit(2)
	}

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

	// binfmt_misc — kernel API filesystem that lets the kernel hand foreign-arch
	// ELFs to a userspace interpreter (qemu-user). Needed for `docker buildx`
	// cross-arch builds inside the sandbox: `tonistiigi/binfmt --install all`
	// writes its handler registrations through /proc/sys/fs/binfmt_misc/register.
	// Normally systemd-binfmt mounts this; with the shim, lohar does it.
	// Best-effort: only fails if the kernel was built without CONFIG_BINFMT_MISC.
	if err := syscall.Mount("binfmt_misc", "/proc/sys/fs/binfmt_misc", "binfmt_misc", 0, ""); err != nil {
		fmt.Fprintf(os.Stderr, "lohar: mount binfmt_misc (non-fatal): %v\n", err)
	}

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
		applyHostname(hostname)
		if cfg.DNSInternal != "" || len(cfg.DNS) > 0 {
			applyDNS(cfg.DNSInternal, cfg.DNS)
		} else {
			ensureResolvConf()
		}
		agentToken = cfg.Token
		configEnv = cfg.Env
		writeConfigFiles(cfg.Files)
		mountVolumes(cfg.Volumes)
		mountFsMounts(cfg.Mounts)
		syscall.Unmount("/run/bhatti/config", 0)
		os.RemoveAll("/run/bhatti/config")
		bp("config_applied")
	} else {
		applyHostname("bhatti")
		ensureResolvConf()
	}

	setupNetworking()
	bp("network_done")

	// --- Signal handlers + zombie reaping + syslog ---

	installSignalHandlers()
	go reapZombies()

	// Build the long-lived Unit registry shared by the syslog receiver,
	// service activation, and the IPC handler. Bound to ProductionConfig()
	// (the real /etc, /run, /var/log paths). Once constructed the Config
	// is immutable, so watchers and other goroutines can read paths
	// off it without synchronisation.
	//
	// A syslog message tagged "sshd" gets reconciled to the canonical
	// Unit (ssh.service) on first lookup here and cached thereafter —
	// logs land in the same file regardless of which name the daemon
	// uses internally.
	globalRegistry = NewRegistry(ProductionConfig())

	go startSyslogReceiver(globalRegistry)

	// sd_notify receiver: daemons declaring Type=notify connect to
	// $NOTIFY_SOCKET (which we set in their env at spawn time) and send
	// READY=1 when initialised. The receiver attributes via cgroup-per-
	// unit and clears the .activating marker so svcStart can return.
	go startNotifyReceiver(globalRegistry)

	// Privileged systemctl operations from in-guest non-root callers go
	// through this Unix socket. PID 1 lohar runs the op as root and sends
	// the formatted output back. See cmd/lohar/systemctl_ipc.go.
	startSystemctlListener()

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

	fmt.Fprintln(os.Stderr, "lohar: ready")

	// --- Bridge user --env into unit-file environment ---
	// configEnv comes from the config drive (populated by `bhatti create
	// --env KEY=VALUE`). Today it only reaches `bhatti exec` invocations
	// via the env-merge in exec.go. Units spawned by
	// startEnabledServices() can't see it unless we materialise it as a
	// file they can EnvironmentFile= from.
	//
	// Convention (per PLAN-tiers-systemd.md): write canonical KEY=VALUE
	// lines to /run/bhatti/config-env. Tier units opt in with
	//   EnvironmentFile=-/run/bhatti/config-env
	// The leading '-' makes the file optional, so minimal sandboxes
	// without any --env flags still boot cleanly.
	os.MkdirAll("/run/bhatti", 0755)
	if len(configEnv) > 0 {
		var b strings.Builder
		for k, v := range configEnv {
			fmt.Fprintf(&b, "%s=%s\n", k, v)
		}
		if err := os.WriteFile("/run/bhatti/config-env", []byte(b.String()), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "lohar: write config-env: %v\n", err)
		}
	}

	// --- Start enabled services ---
	// Read /etc/systemd/system/multi-user.target.wants/ and start each
	// service. This replaces systemd's multi-user.target activation.

	// systemd-tmpfiles equivalent: process /etc/tmpfiles.d/*.conf,
	// /run/tmpfiles.d/*.conf, and /usr/lib/tmpfiles.d/*.conf to
	// materialise runtime directories that packages declare
	// (e.g., openssh-server's /run/sshd). Must run before
	// startEnabledServices so daemons that depend on these dirs
	// don't fail to start. (F6)
	applyTmpfiles([]string{
		"/usr/lib/tmpfiles.d",
		"/lib/tmpfiles.d",
		"/run/tmpfiles.d",
		"/etc/tmpfiles.d",
	})
	bp("tmpfiles_applied")

	startEnabledServices()
	bp("services_started")

	// --- Boot timing ---
	// Written AFTER tmpfiles_applied / services_started so the
	// trace actually reflects what happened. Previously written
	// right after tcp_listen, which truncated the visible boot to
	// the first ~7ms and hid the slow phases.
	os.WriteFile("/run/bhatti/boot-timing.txt", []byte(bootLog.String()), 0644)

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

// startSyslogReceiver creates the syslog socket and routes datagrams to
// <LogDir>/<canonical>.log via the Unit registry: a daemon that logs as
// "sshd[123]: ..." lands in ssh.log when ssh.service has
// Alias=sshd.service — unifying with the stdout/stderr capture in
// svcStart, which already keys by canonical name.
//
// Tags that don't match any known Unit (kernel, cron, login, custom
// daemons not managed by the shim) fall back to <LogDir>/<tag>.log so
// they're still captured and greppable.
//
// Services like sshd, postgres, and nginx write to syslog (via libc's
// openlog/syslog) instead of stdout when daemonised. Without this
// receiver their logs are lost.
//
// All paths come from reg.Config; the receiver doesn't read any
// package-level globals.
func startSyslogReceiver(reg *Registry) {
	sockPath := reg.Config.SyslogSocketPath
	os.Remove(sockPath)
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sockPath, Net: "unixgram"})
	if err != nil {
		fmt.Fprintf(os.Stderr, "lohar: syslog receiver: %v\n", err)
		return
	}
	os.Chmod(sockPath, 0666)
	os.MkdirAll(reg.Config.LogDir, 0755)

	buf := make([]byte, 8192)
	for {
		n, err := conn.Read(buf)
		if err != nil || n == 0 {
			continue
		}
		tag, msg := parseSyslogMessage(string(buf[:n]))
		if tag == "" {
			tag = "syslog"
		}
		logPath := filepath.Join(reg.Config.LogDir, tag+".log")
		if u, err := reg.Resolve(tag); err == nil && !u.Masked {
			logPath = u.LogPath()
		}
		if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644); err == nil {
			f.WriteString(msg + "\n")
			f.Close()
		}
	}
}

// parseSyslogMessage extracts the tag (service name) and message from a
// syslog datagram. Handles common formats:
//
//	"<priority>Mon DD HH:MM:SS hostname tag[pid]: message"
//	"<priority>tag: message"
func parseSyslogMessage(raw string) (tag, msg string) {
	s := raw
	if len(s) > 0 && s[0] == '<' {
		if idx := strings.IndexByte(s, '>'); idx > 0 {
			s = s[idx+1:]
		}
	}
	fields := strings.Fields(s)
	for i, f := range fields {
		if strings.HasSuffix(f, ":") || strings.Contains(f, "[") {
			tag = strings.TrimRight(f, ":")
			if idx := strings.IndexByte(tag, '['); idx > 0 {
				tag = tag[:idx]
			}
			// Skip timestamp-like fields (month names, digits)
			if len(tag) <= 3 || (tag[0] >= '0' && tag[0] <= '9') {
				continue
			}
			msg = strings.TrimSpace(strings.Join(fields[i:], " "))
			return tag, msg
		}
	}
	return "syslog", strings.TrimSpace(s)
}

// --- Config drive ---

type VolumeMountConfig struct {
	Device   string `json:"device"`
	Mount    string `json:"mount"`
	FS       string `json:"fs"`
	ReadOnly bool   `json:"read_only"`
}

type FsMountConfig struct {
	Tag      string `json:"tag"`
	Mount    string `json:"mount"`
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
	Mounts  []FsMountConfig     `json:"mounts,omitempty"`
	Init    string              `json:"init,omitempty"`
	DNS     []string            `json:"dns"`
	// DNSInternal is the per-user bridge gateway IP hosting the
	// in-cluster DNS responder. Prepended to /etc/resolv.conf so
	// sandbox-name lookups resolve locally before the public DNS
	// fallbacks. Empty string skips the install — backwards-compatible
	// with hosts running an older bhatti daemon. G1.1 of
	// PLAN-bhatti-v2.md.
	DNSInternal string `json:"dns_internal,omitempty"`
	User        string `json:"user"`
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

// applyDNS writes /etc/resolv.conf. Two mutually-exclusive shapes,
// chosen by the host (engine) based on whether the per-user DNS
// responder bound successfully:
//
//  1. Responder up (the normal case): internal != "", public empty.
//     We write ONLY the in-cluster responder. It is authoritative for
//     sibling sandbox names AND forwards everything else upstream
//     itself (see pkg/dns Server.Upstreams), so it's the only resolver
//     the sandbox needs.
//
//  2. Responder bind failed (degraded): internal == "", public set.
//     We write the public resolvers directly so the sandbox still has
//     working name resolution — just without sibling names.
//
// IMPORTANT: we do NOT list internal AND public together. An earlier
// version did, on the assumption that a non-sandbox name would "fall
// through" to 1.1.1.1 after the responder returned NXDOMAIN. That is
// false — glibc treats NXDOMAIN as authoritative and never tries the
// next nameserver (only a TIMEOUT does). Listing public servers
// alongside the responder would just let glibc round-robin away from
// our responder and miss sibling names. Forwarding (case 1) is what
// makes both kinds of name resolve from a single nameserver line.
// G1.1 of PLAN-bhatti-v2.md.
func applyDNS(internal string, public []string) {
	content := buildResolvConf(internal, public)
	if content == "" {
		return
	}
	os.Remove("/etc/resolv.conf")
	os.WriteFile("/etc/resolv.conf", []byte(content), 0644)
}

// buildResolvConf renders the resolv.conf contents. Pure function for
// testability: the ordering (internal first, public second) is the
// load-bearing property a future refactor must preserve, and a
// regression test on the string output catches accidental swaps.
//
// Returns "" when both inputs are empty; callers skip writing the
// file in that case so an existing system-installed resolv.conf isn't
// clobbered.
func buildResolvConf(internal string, public []string) string {
	if internal == "" && len(public) == 0 {
		return ""
	}
	var content string
	if internal != "" {
		// Comment line so a human reading the file knows what 10.0.N.1
		// is. Some tools strip comments; that's fine — the nameserver
		// line is what actually matters.
		content += "# bhatti in-cluster DNS (per-user): resolves sibling sandbox\n"
		content += "# names (<sandbox>, <sandbox>.sb) and forwards everything else\n"
		content += "# upstream, so it's the only resolver the sandbox needs.\n"
		content += "nameserver " + internal + "\n"
		// timeout:2 attempts:1 — if the responder is unreachable, fail
		// fast (2s) rather than stalling on glibc's default 5s × 2
		// attempts = 10s before the sandbox gives up on DNS entirely.
		content += "options timeout:2 attempts:1\n"
	}
	for _, s := range public {
		content += "nameserver " + s + "\n"
	}
	return content
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

// mountFsMounts mounts each virtio-fs bind (create --mount) by tag at its guest
// path. Non-fatal (log + continue) so a bad/absent mount never blocks boot —
// e.g. a snapshot restored without its host dirs still comes up.
func mountFsMounts(mounts []FsMountConfig) {
	for _, m := range mounts {
		if m.Tag == "" || m.Mount == "" {
			continue
		}
		os.MkdirAll(m.Mount, 0755)
		var flags uintptr
		if m.ReadOnly {
			flags |= syscall.MS_RDONLY
		}
		if err := syscall.Mount(m.Tag, m.Mount, "virtiofs", flags, ""); err != nil {
			fmt.Fprintf(os.Stderr, "lohar: mount virtiofs %s → %s: %v\n", m.Tag, m.Mount, err)
			continue
		}
		if !m.ReadOnly {
			os.Chown(m.Mount, 1000, 1000)
		}
		fmt.Fprintf(os.Stderr, "lohar: mounted virtiofs %s → %s (ro=%v)\n", m.Tag, m.Mount, m.ReadOnly)
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
