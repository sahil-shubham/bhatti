//go:build linux

package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
)

// systemctl shim — reads .service files and manages processes directly.
// No real systemd. lohar (PID 1) handles zombie reaping.
//
// Covers the full surface used by Debian/Ubuntu package tooling
// (deb-systemd-helper, deb-systemd-invoke, invoke-rc.d) plus
// interactive use (start, stop, reload, status, logs).

// Path locations were package-level vars that tests rewrote (and that
// triggered the -race finding around watcher goroutines reading them
// while test cleanup wrote them). They now live on Registry.Config
// (see unit.go); production callers construct a Registry with
// ProductionConfig() and tests construct one with their tempdirs.
// Nothing in this file reads filesystem paths directly anymore.

// Well-known targets that are always "active" — invoke-rc.d checks
// sysinit.target to determine runlevel.
var alwaysActiveTargets = map[string]bool{
	"sysinit":        true,
	"multi-user":     true,
	"default":        true,
	"network":        true,
	"network-online": true,
	"sockets":        true,
	"basic":          true,
}

// runSystemctl is the entry point when invoked as /usr/bin/systemctl.
// A fresh Registry is created per invocation; for command-line use
// this is fine because each systemctl process resolves only the units
// it operates on. PID-1 lohar uses a long-lived Registry that's
// shared with the syslog receiver and journalctl (see C5).
func runSystemctl(args []string) {
	var command string
	var units []string
	var showProp string
	var showValue bool // --value: print just value, not Key=Value
	var quiet bool
	var nowFlag bool
	var signalName string
	noLegend := false
	stateFilter := ""
	typeFilter := ""

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--no-reload" || arg == "--no-block" ||
			arg == "--system" || arg == "--no-pager" ||
			arg == "--no-ask-password" || arg == "--full":
			continue
		case arg == "--quiet" || arg == "-q":
			quiet = true
		case arg == "--now":
			nowFlag = true
		case arg == "--value":
			showValue = true
		case arg == "--no-legend":
			noLegend = true
		case arg == "-p" || arg == "--property":
			if i+1 < len(args) {
				showProp = args[i+1]
				i++
			}
		case strings.HasPrefix(arg, "-p"):
			showProp = strings.TrimPrefix(arg, "-p")
		case strings.HasPrefix(arg, "--property="):
			showProp = strings.TrimPrefix(arg, "--property=")
		case strings.HasPrefix(arg, "--state="):
			stateFilter = strings.TrimPrefix(arg, "--state=")
		case strings.HasPrefix(arg, "--type="):
			typeFilter = strings.TrimPrefix(arg, "--type=")
		case strings.HasPrefix(arg, "--signal="):
			signalName = strings.TrimPrefix(arg, "--signal=")
		case strings.HasPrefix(arg, "--root=") || strings.HasPrefix(arg, "--preset-mode=") ||
			strings.HasPrefix(arg, "--global") || strings.HasPrefix(arg, "--kill-whom="):
			continue // flags we accept but ignore
		case strings.HasPrefix(arg, "-"):
			continue // skip unknown flags
		case command == "":
			command = arg
		default:
			units = append(units, arg)
		}
	}

	reg := NewRegistry(ProductionConfig())

	// Resolve .socket units to their associated .service for RUNTIME commands
	// (start/stop/restart/is-active/reload/kill). Socket activation is not
	// supported — we start the service directly.
	// Do NOT resolve for LIFECYCLE commands (enable/disable/is-enabled/preset/mask)
	// because those must create symlinks for the actual .socket file.
	runtimeCommands := map[string]bool{
		"start": true, "stop": true, "restart": true, "try-restart": true,
		"reload": true, "reload-or-restart": true, "is-active": true,
		"status": true, "kill": true,
	}
	if runtimeCommands[command] {
		for i, u := range units {
			if isSocketUnit(u) {
				units[i] = reg.resolveSocketToService(u) + ".service"
			}
		}
	}

	_ = quiet // TODO: suppress stdout when set

	// IPC dispatch for privileged ops: if we're not PID 1 and a daemon is
	// reachable on /run/bhatti/systemctl.sock, forward the request and
	// replay its output. This is what stops `bhatti exec dev -- systemctl
	// stop ssh` (running as the unprivileged lohar user) from silently
	// no-op'ing — PID 1 runs the kill as root, errors actually surface,
	// and a non-root caller gets a clean Access-denied response.
	//
	// If the daemon isn't there (test envs, dev hosts), we fall through to
	// the in-process path. Forward-compatible with v1.10.x flows.
	if requiresPrivilege(command) {
		ipcReq := proto.SystemctlRequest{
			Op:    command,
			Units: units,
			Flags: map[string]string{},
		}
		if nowFlag {
			ipcReq.Flags["now"] = "1"
		}
		if signalName != "" {
			ipcReq.Flags["signal"] = signalName
		}
		if resp, ok := tryDispatchViaIPC(ipcReq); ok {
			os.Stdout.Write([]byte(resp.Stdout))
			os.Stderr.Write([]byte(resp.Stderr))
			os.Exit(resp.ExitCode)
		}
		// Fallthrough: no daemon reachable, run in-process below.
	}

	// resolveOrFatal looks up a unit name through the registry. For not-found
	// units the behaviour matches what the old free-function dispatch did:
	// some commands (status, show, is-active, is-enabled) are tolerant of
	// missing units and report a sensible "inactive/not-found" line; others
	// (start, restart, reload) exit with the systemd convention of code 5.
	resolveOrTolerate := func(raw string) (*Unit, string) {
		u, err := reg.Resolve(raw)
		if err != nil {
			return nil, raw
		}
		return u, raw
	}
	resolveOrFatal := func(raw, op string) *Unit {
		u, err := reg.Resolve(raw)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to %s %s: Unit %s not found.\n", op, raw, raw)
			os.Exit(5)
		}
		return u
	}

	switch command {
	case "start":
		for _, raw := range units {
			u := resolveOrFatal(raw, "start")
			if err := svcStart(u); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to start %s: %v\n", raw, err)
				os.Exit(1)
			}
		}
	case "stop":
		for _, raw := range units {
			u, _ := resolveOrTolerate(raw)
			if u == nil {
				continue // stopping a nonexistent unit is not an error
			}
			if err := svcStop(u); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to stop %s: %v\n", raw, err)
				os.Exit(1)
			}
		}
	case "restart":
		for _, raw := range units {
			u := resolveOrFatal(raw, "restart")
			svcStop(u)
			if err := svcStart(u); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to restart %s: %v\n", raw, err)
				os.Exit(1)
			}
		}
	case "try-restart":
		for _, raw := range units {
			u, _ := resolveOrTolerate(raw)
			if u == nil || !svcIsActive(u) {
				continue
			}
			svcStop(u)
			if err := svcStart(u); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to restart %s: %v\n", raw, err)
				os.Exit(1)
			}
		}
	case "reload":
		for _, raw := range units {
			u := resolveOrFatal(raw, "reload")
			if err := svcReload(u); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to reload %s: %v\n", raw, err)
				os.Exit(1)
			}
		}
	case "reload-or-restart":
		for _, raw := range units {
			u := resolveOrFatal(raw, "reload-or-restart")
			if err := svcReload(u); err != nil {
				svcStop(u)
				if err := svcStart(u); err != nil {
					fmt.Fprintf(os.Stderr, "Failed to restart %s: %v\n", raw, err)
					os.Exit(1)
				}
			}
		}
	case "enable":
		for _, raw := range units {
			u, _ := resolveOrTolerate(raw)
			if u == nil {
				// enable on a missing unit is silently tolerated to match
				// the old behaviour — packages may enable services whose
				// files aren't installed yet.
				continue
			}
			if err := svcEnable(u); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to enable %s: %v\n", raw, err)
				os.Exit(1)
			}
			if nowFlag {
				svcStart(u)
			}
		}
	case "disable":
		for _, raw := range units {
			u, _ := resolveOrTolerate(raw)
			if u == nil {
				// Glob-based disable still works without a registry hit:
				// remove any wants/ symlinks matching the raw name.
				reg.svcDisableByName(normalizeName(raw), unitSuffix(raw))
				continue
			}
			if nowFlag {
				svcStop(u)
			}
			if err := svcDisable(u); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to disable %s: %v\n", raw, err)
				os.Exit(1)
			}
		}
	case "is-active":
		if len(units) == 0 {
			os.Exit(1)
		}
		active := false
		if isTarget(normalizeName(units[0])) {
			active = true
		} else if u, _ := resolveOrTolerate(units[0]); u != nil {
			active = svcIsActive(u)
		}
		if active {
			if !quiet {
				fmt.Println("active")
			}
		} else {
			if !quiet {
				fmt.Println("inactive")
			}
			os.Exit(3)
		}
	case "is-enabled":
		if len(units) == 0 {
			os.Exit(1)
		}
		enabled := false
		if u, _ := resolveOrTolerate(units[0]); u != nil {
			enabled = svcIsEnabled(u)
		} else {
			enabled = reg.svcIsEnabledByName(normalizeName(units[0]))
		}
		if enabled {
			if !quiet {
				fmt.Println("enabled")
			}
		} else {
			if !quiet {
				fmt.Println("disabled")
			}
			os.Exit(1)
		}
	case "is-failed":
		// Read-only; stays in-process. is-failed prints "failed" if the
		// .failed marker exists for the unit, otherwise prints the
		// current ActiveState approximation (active or inactive). Exit
		// 0 only when the unit IS failed — systemd's contract for
		// scripts that want to filter for crashed services.
		if len(units) == 0 {
			os.Exit(1)
		}
		u, _ := resolveOrTolerate(units[0])
		if u != nil && u.IsFailed() {
			if !quiet {
				fmt.Println("failed")
			}
			return // exit 0
		}
		state := "inactive"
		if u != nil && svcIsActive(u) {
			state = "active"
		}
		if !quiet {
			fmt.Println(state)
		}
		os.Exit(1)
	case "status":
		if len(units) == 0 {
			fmt.Println("running")
			return
		}
		u, raw := resolveOrTolerate(units[0])
		svcStatus(u, raw)
	case "show":
		if len(units) == 0 {
			return
		}
		u, raw := resolveOrTolerate(units[0])
		svcShow(u, raw, showProp, showValue)
	case "cat":
		if len(units) == 0 {
			os.Exit(1)
		}
		u, raw := resolveOrTolerate(units[0])
		svcCat(u, raw)
	case "mask":
		for _, raw := range units {
			if err := reg.svcMaskName(normalizeName(raw)); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to mask %s: %v\n", raw, err)
			}
		}
	case "unmask":
		for _, raw := range units {
			if err := reg.svcUnmaskName(normalizeName(raw)); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to unmask %s: %v\n", raw, err)
			}
		}
	case "kill":
		sig := syscall.SIGTERM
		if signalName == "KILL" || signalName == "SIGKILL" {
			sig = syscall.SIGKILL
		} else if signalName == "HUP" || signalName == "SIGHUP" {
			sig = syscall.SIGHUP
		}
		for _, raw := range units {
			u, _ := resolveOrTolerate(raw)
			if u == nil {
				continue
			}
			if pid, err := u.ReadPID(); err == nil {
				syscall.Kill(-pid, sig)
			}
		}
	case "daemon-reload", "daemon-reexec":
		// Drop both the negative cache (so newly-created unit files
		// become discoverable) and the positive cache (so edited unit
		// files have their parsed directives refreshed on next
		// Resolve). Runtime state lives in marker files on disk —
		// PIDs, .activating, .failed — so wiping byKey doesn't lose
		// it. See Registry.Reload's doc comment for the full story.
		reg.Reload()
	case "reset-failed":
		for _, raw := range units {
			if u, _ := resolveOrTolerate(raw); u != nil {
				u.ClearFailed()
			}
		}
	case "preset":
		for _, raw := range units {
			u, _ := resolveOrTolerate(raw)
			if u == nil {
				continue
			}
			svcEnable(u)
		}
	case "is-system-running":
		fmt.Println("running")
	case "list-units":
		svcListUnits(reg, stateFilter, typeFilter, noLegend)
	case "list-unit-files":
		svcListUnitFiles(reg, noLegend)
	default:
		// Unknown command — succeed silently. Package scripts sometimes
		// call obscure systemctl subcommands; failing breaks installs.
		if command != "" {
			fmt.Fprintf(os.Stderr, "systemctl: unknown command %q (ignored)\n", command)
		}
	}
}

// --- Name resolution helpers ---
//
// Unit identity (canonical name, alias merge, drop-in loading) lives in
// unit.go. The helpers here are small utilities that operate on raw
// argv strings before resolution, plus the socket->service hop for
// runtime commands. Anything that touches *Unit state (pidfile, logfile,
// is-running) belongs on the Unit type.

// unitSuffix returns the suffix (.service, .socket, etc.) or defaults to .service.
func unitSuffix(name string) string {
	for _, s := range []string{".service", ".socket", ".target", ".timer"} {
		if strings.HasSuffix(name, s) {
			return s
		}
	}
	return ".service"
}

// normalizeName strips known suffixes. Returns the base name.
func normalizeName(name string) string {
	for _, s := range []string{".service", ".socket", ".target", ".timer"} {
		name = strings.TrimSuffix(name, s)
	}
	return name
}

// isSocketUnit returns true if the original argument was a .socket unit.
func isSocketUnit(name string) bool {
	return strings.HasSuffix(name, ".socket")
}

// isTarget returns true if the name refers to a .target unit.
func isTarget(name string) bool {
	return strings.HasSuffix(name, ".target") || alwaysActiveTargets[name]
}

// resolveSocketToService finds the .service associated with a .socket unit.
// Checks the Service= directive in the .socket file, falls back to name
// match. Runs before registry Resolve because the dispatch needs a
// .service name to look up. We don't (yet) implement socket activation —
// the .service is started directly.
func (r *Registry) resolveSocketToService(name string) string {
	base := normalizeName(name)
	for _, dir := range r.Config.ServiceDirs {
		path := filepath.Join(dir, base+".socket")
		if _, err := os.Stat(path); err == nil {
			svc := parseServiceFile(path)
			if s := svc.get("Socket", "Service"); s != "" {
				return normalizeName(s)
			}
		}
	}
	return base
}

// processAlive is the only PID helper that doesn't belong on Unit — it
// operates on a raw PID, not a unit identity.
func processAlive(pid int) bool {
	_, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	return err == nil
}

// humanBytes formats a byte count the way systemd's status does:
// "124.5M", "1.2G". Picks the largest unit where the value is >= 1.
func humanBytes(n uint64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)
	switch {
	case n >= GB:
		return fmt.Sprintf("%.1fG", float64(n)/float64(GB))
	case n >= MB:
		return fmt.Sprintf("%.1fM", float64(n)/float64(MB))
	case n >= KB:
		return fmt.Sprintf("%.1fK", float64(n)/float64(KB))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// Watcher coordination state — stopRequested, restart burst history, and
// the WaitGroup tracking live watchers — lives on *Registry now (see
// unit.go). It used to be package-level; the audit-pass move into
// Registry was driven by a -race finding where the watcher goroutine
// read pidDir while a test was concurrently writing it during cleanup.
// With state on Registry, paths and coordination are all reachable from
// the same construction-time-immutable config, removing the race by
// construction.

// --- Service operations ---
//
// Every operation takes a *Unit instead of a name string. State (pidfile,
// logfile, enabled-marker glob) is keyed by Unit.Canonical, so any name
// the unit answers to (canonical or alias) sees the same state. This is
// the heart of C1: it's why `systemctl status sshd` and `systemctl status
// ssh` can no longer disagree.

func svcStart(u *Unit) error {
	if u.Masked {
		return fmt.Errorf("unit %s is masked", u.FullName())
	}
	if u.IsRunning() {
		return nil // already running
	}

	// Conditions: if any [Unit] Condition*= directive evaluates false,
	// the unit is silently skipped without an error and without leaving
	// a failed marker. Matches systemd's semantics: a failed condition
	// is the admin saying 'don't run this right now,' not a service
	// failure. (F4)
	if ok, reason := evaluateConditions(u); !ok {
		fmt.Fprintf(os.Stderr, "lohar: %s skipped: %s\n", u.Canonical, reason)
		return nil
	}

	svc := u.Sections

	// State / Cache / Logs / Configuration directories: created with
	// the right ownership and mode before ExecStartPre runs, since
	// ExecStartPre often expects them to exist. (F5)
	if err := u.ApplyStateDirectories(); err != nil {
		return fmt.Errorf("state directories: %w", err)
	}

	// RuntimeDirectory
	if rd := svc.get("Service", "RuntimeDirectory"); rd != "" {
		dir := "/run/" + rd
		mode := os.FileMode(0755)
		if m := svc.get("Service", "RuntimeDirectoryMode"); m != "" {
			if parsed, err := strconv.ParseUint(m, 8, 32); err == nil {
				mode = os.FileMode(parsed)
			}
		}
		os.MkdirAll(dir, mode)
		os.Chmod(dir, mode)
		if user := svc.get("Service", "User"); user != "" {
			exec.Command("chown", user, dir).Run()
		}
	}

	// ExecStartPre
	for _, cmdLine := range svc.getAll("Service", "ExecStartPre") {
		ignoreErr := strings.HasPrefix(cmdLine, "-")
		if err := runServiceCommand(cmdLine, svc); err != nil {
			if ignoreErr {
				fmt.Fprintf(os.Stderr, "ExecStartPre (ignored): %v\n", err)
			} else {
				return fmt.Errorf("ExecStartPre failed: %w", err)
			}
		}
	}

	execStart := svc.get("Service", "ExecStart")
	if execStart == "" {
		return fmt.Errorf("no ExecStart in %s", u.Path)
	}

	svcType := svc.get("Service", "Type")
	if svcType == "" {
		svcType = "simple"
	}

	// Type=notify: write the activating marker BEFORE spawning so the
	// notify receiver can clear it as soon as READY=1 arrives. The
	// receiver is a separate goroutine; if the daemon is fast enough
	// to send READY=1 before svcStart's wait loop starts, the marker
	// is already gone and the wait returns immediately.
	if svcType == "notify" {
		if err := u.MarkActivating(); err != nil {
			return fmt.Errorf("mark activating: %w", err)
		}
	}

	var startErr error
	switch svcType {
	case "oneshot":
		startErr = runServiceCommand(execStart, svc)
	case "forking":
		startErr = startForking(u, execStart, svc)
	default:
		// simple, exec, dbus -- treated as simple daemons (active on
		// fork+exec). notify is also handled by startDaemon but blocks
		// at the end waiting for READY=1.
		if err := startDaemon(u, execStart, svc); err != nil {
			u.ClearActivating()
			return err
		}
		if svcType == "notify" {
			startErr = waitForNotifyReady(u, svc)
		}
	}
	if startErr != nil {
		return startErr
	}

	// ExecStartPost: runs after the unit is considered active. For
	// Type=oneshot/forking this is after ExecStart returns; for
	// Type=simple it's after fork+exec; for Type=notify it's after
	// READY=1 from sd_notify. Mirrors systemd's semantics: leading '-'
	// makes failure non-fatal; otherwise a failing post stops the unit.
	// (PLAN-tiers-systemd.md depends on this for chmod-the-socket
	// hooks; arrived as part of v1.11.3.)
	for _, cmdLine := range svc.getAll("Service", "ExecStartPost") {
		ignoreErr := strings.HasPrefix(cmdLine, "-")
		if err := runServiceCommand(cmdLine, svc); err != nil {
			if ignoreErr {
				fmt.Fprintf(os.Stderr, "ExecStartPost (ignored): %v\n", err)
			} else {
				return fmt.Errorf("ExecStartPost failed: %w", err)
			}
		}
	}

	return nil
}

// waitForNotifyReady blocks until the unit's .activating marker is
// removed (the notify receiver clears it on READY=1) or TimeoutStartSec
// elapses. Mirrors systemd's behaviour: "systemctl start postgres"
// returns only after postgres has actually opened its socket, so the
// next command in a script doesn't race against half-started state.
//
// If the timeout fires, we return an error AND mark the unit failed so
// the watcher's restart policy applies (the daemon may also still be
// running and consuming resources -- the caller can choose to stop it
// if needed).
func waitForNotifyReady(u *Unit, svc serviceFile) error {
	timeout := parseRestartSec(svc.get("Service", "TimeoutStartSec"))
	if timeout < time.Second {
		timeout = 90 * time.Second // systemd's default
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !u.IsActivating() {
			return nil // READY=1 received
		}
		if !u.IsRunning() {
			// Daemon died before signalling ready. The watcher will
			// observe the exit and write the failed marker via the
			// usual path; we just bail out of the wait.
			u.ClearActivating()
			return fmt.Errorf("%s exited before sending READY=1", u.Canonical)
		}
		time.Sleep(50 * time.Millisecond)
	}
	u.ClearActivating()
	u.MarkFailed(1)
	return fmt.Errorf("%s did not send READY=1 within %v", u.Canonical, timeout)
}

// spawnHelperPath is the binary invoked by startDaemon to perform the
// race-free cgroup placement before execve into the daemon. Defaults to
// /proc/self/exe — the kernel-maintained symlink to the running binary,
// which in production resolves to lohar itself (started by the kernel
// as PID 1 from /usr/local/bin/lohar). The argv[1]-verb dispatch in
// main.go then routes the subprocess to runSpawn.
//
// Tests override this (and spawnHelperPrefix / spawnHelperEnv below) so
// that startDaemon's subprocess is the test binary itself, re-routed
// through TestMain to runSpawn. /proc/self/exe in a test binary is the
// test binary, which has its own main (testing.M.Run) and would not
// hit our argv-verb dispatch — hence the redirection.
var (
	spawnHelperPath   = "/proc/self/exe"
	spawnHelperPrefix []string // argv prepended before "spawn ..."
	spawnHelperEnv    []string // extra env vars set on cmd.Env
)

func startDaemon(u *Unit, execStart string, svc serviceFile) error {
	execStart = strings.TrimLeft(execStart, "-!+:@")
	if execStart == "" {
		return fmt.Errorf("empty ExecStart")
	}

	// Create the cgroup BEFORE spawning so resource limits are in effect
	// from the moment the process is moved into it. Errors are non-fatal:
	// if cgroup setup fails (kernel without v2, missing controllers,
	// running outside PID 1 in a test) we still start the daemon — it
	// just won't have isolation. The KillMode=control-group path detects
	// the missing cgroup and falls back to PGID-kill.
	if err := u.CreateCgroup(); err != nil {
		fmt.Fprintf(os.Stderr, "lohar: cgroup setup for %s: %v (proceeding without isolation)\n", u.Canonical, err)
	}

	// Spawn via the `lohar spawn` helper instead of /bin/sh directly.
	// The helper does one thing before exec'ing into the daemon: writes
	// its own PID into <cgroup>/cgroup.procs. Because the write happens
	// before any fork by the daemon (X-server detach, dbus pre-fork, etc.)
	// and execve preserves cgroup membership, every descendant of the
	// daemon lands in the unit's cgroup. Stop-time cgroup.kill then
	// catches all of them.
	//
	// This replaces the previous post-cmd.Start() PlaceInCgroup call,
	// which had a race window where the daemon could fork before being
	// moved (e.g. Xkasmvnc's daemon(3) fork escaping to 0::/, leaving
	// an orphan that systemctl stop couldn't kill). See spawn.go and
	// docs/internal/PLAN-spawn-helper.md for the full story.
	//
	// spawnHelperPath defaults to /proc/self/exe — the kernel-maintained
	// symlink to lohar's binary. Cheap (no syscall on Linux: kernel-side
	// resolve at execve time), test-friendly, and standard practice for
	// re-exec patterns (gosu, su-exec, runc all use it). The Prefix and
	// Env slices are empty in production and only populated by tests.
	spawnArgs := append([]string{}, spawnHelperPrefix...)
	spawnArgs = append(spawnArgs,
		"spawn",
		"--cgroup", u.CgroupPath(),
		"--", "/bin/sh", "-c", "exec "+execStart,
	)
	cmd := exec.Command(spawnHelperPath, spawnArgs...)
	cmd.Dir = svc.get("Service", "WorkingDirectory")
	cmd.Env = buildServiceEnv(svc)
	// For Type=notify daemons we expose NOTIFY_SOCKET so libsystemd's
	// sd_notify(3) (or any reimplementation) can find our receiver.
	// Setting it unconditionally is harmless for non-notify daemons --
	// they just won't connect to it. Inherited through the spawn helper
	// across both execves (lohar spawn → /bin/sh → daemon).
	cmd.Env = append(cmd.Env, "NOTIFY_SOCKET="+u.reg.Config.NotifySocketPath)
	cmd.Env = append(cmd.Env, spawnHelperEnv...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil

	// Capture stdout/stderr to log file for debugging. The log fd is
	// inherited by lohar spawn and then by the daemon across both
	// execves — close-on-exec is not set by exec.Command, so the fd
	// survives unless syscall.Exec explicitly closes it (it doesn't).
	os.MkdirAll(u.reg.Config.LogDir, 0755)
	logFile, err := os.OpenFile(u.LogPath(),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}

	if err := cmd.Start(); err != nil {
		if logFile != nil {
			logFile.Close()
		}
		return fmt.Errorf("start %s: %w", execStart, err)
	}

	// Close log file in the parent — the child has its own fd.
	if logFile != nil {
		logFile.Close()
	}

	// cmd.Process.Pid is the PID of the forked child, which is the same
	// PID running lohar spawn, /bin/sh, and ultimately the daemon —
	// execve preserves PID across all three transitions. The cgroup
	// placement happens inside lohar spawn before its execve into
	// /bin/sh; no PlaceInCgroup call is needed here.
	u.WritePID(cmd.Process.Pid)
	u.ClearFailed() // a new run starts with a clean slate

	// Spawn a watcher goroutine that observes the daemon's exit and
	// applies the Restart= policy. cmd.Process.Release() is NOT called
	// before this — the watcher needs the cmd handle for cmd.Wait().
	//
	// Tracked via the Registry's watcherWG so tests can wait for
	// completion before tearing down. Production code never reads it.
	//
	// Snapshot/restore caveat: this goroutine is bound to *os.Process,
	// which doesn't survive a process restart. In a Firecracker microVM
	// that snapshot/restores the entire VM atomically, goroutines pause/
	// resume cleanly so this caveat doesn't apply to the snapshot
	// lifecycle — only to a hypothetical lohar-only restart.
	u.reg.watcherWG.Add(1)
	go func() {
		defer u.reg.watcherWG.Done()
		watchAndMaybeRestart(u, cmd)
	}()
	return nil
}

// watchAndMaybeRestart is the per-daemon supervisor goroutine. It blocks
// on cmd.Wait() (which also reaps the zombie), then:
//
//   - Updates the failed-state marker based on the exit code.
//   - Removes the pidfile.
//   - Consults Restart= policy and invokes svcStart again if appropriate,
//     subject to a burst limit so a flapping service can't pin a CPU.
//
// If an admin called systemctl stop (markStopRequested), the restart is
// suppressed and the stop-marker cleared.
func watchAndMaybeRestart(u *Unit, cmd *exec.Cmd) {
	err := cmd.Wait()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	u.RemovePID()
	if exitCode == 0 {
		u.ClearFailed()
	} else {
		u.MarkFailed(exitCode)
	}

	if u.reg.isStopRequested(u.Canonical) {
		u.reg.clearStopRequested(u.Canonical)
		return
	}

	policy := u.Sections.get("Service", "Restart")
	if !shouldRestart(policy, exitCode) {
		return
	}
	if !u.reg.restartBurstAllowed(u) {
		fmt.Fprintf(os.Stderr, "lohar: %s flapping, giving up after start-limit-burst\n", u.Canonical)
		return
	}

	restartDelay := parseRestartSec(u.Sections.get("Service", "RestartSec"))
	time.Sleep(restartDelay)

	if err := svcStart(u); err != nil {
		fmt.Fprintf(os.Stderr, "lohar: auto-restart of %s failed: %v\n", u.Canonical, err)
	}
}

// shouldRestart maps the Restart= directive to a yes/no for the given
// exit code. Mirrors systemd's policy:
//
//	no            never
//	always        always (also after clean exits)
//	on-success    only after exit 0
//	on-failure    after non-zero exit (the common case)
//	on-abnormal   after signals (exit > 128, by convention)
//	"" (unset)    treated as no — services explicitly opt in
func shouldRestart(policy string, exitCode int) bool {
	failed := exitCode != 0
	switch policy {
	case "always":
		return true
	case "on-success":
		return !failed
	case "on-failure":
		return failed
	case "on-abnormal":
		return exitCode > 128
	case "no", "":
		return false
	}
	return false
}

// parseRestartSec returns the RestartSec= value as a time.Duration.
// Empty or unparseable input defaults to 100ms (matches systemd's default).
func parseRestartSec(v string) time.Duration {
	if v == "" {
		return 100 * time.Millisecond
	}
	// Bare integers in unit files are seconds (systemd convention).
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
		return time.Duration(n) * time.Second
	}
	if d, err := time.ParseDuration(strings.TrimSpace(v)); err == nil {
		return d
	}
	return 100 * time.Millisecond
}

// restartBurstAllowed enforces StartLimitBurst / StartLimitIntervalSec
// (matching systemd's defaults: 5 attempts in 10 seconds). Per-unit
// history is kept on the Registry so it's shared between PID-1 lohar's
// watcher goroutines but isolated per test Registry. Protected by
// coordMu.
func (r *Registry) restartBurstAllowed(u *Unit) bool {
	burst := 5
	if v := u.Sections.get("Service", "StartLimitBurst"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			burst = n
		}
	}
	interval := 10 * time.Second
	if v := u.Sections.get("Service", "StartLimitIntervalSec"); v != "" {
		interval = parseRestartSec(v)
	}

	r.coordMu.Lock()
	defer r.coordMu.Unlock()
	now := time.Now()
	cutoff := now.Add(-interval)
	history := r.restartBurst[u.Canonical]
	// Drop attempts older than the interval.
	fresh := history[:0]
	for _, t := range history {
		if t.After(cutoff) {
			fresh = append(fresh, t)
		}
	}
	if len(fresh) >= burst {
		r.restartBurst[u.Canonical] = fresh
		return false
	}
	fresh = append(fresh, now)
	r.restartBurst[u.Canonical] = fresh
	return true
}

func startForking(u *Unit, execStart string, svc serviceFile) error {
	execStart = strings.TrimLeft(execStart, "-!+:@")
	if execStart == "" {
		return fmt.Errorf("empty ExecStart")
	}

	cmd := exec.Command("/bin/sh", "-c", execStart)
	cmd.Dir = svc.get("Service", "WorkingDirectory")
	cmd.Env = buildServiceEnv(svc)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("start %s: %w", execStart, err)
	}

	if pf := svc.get("Service", "PIDFile"); pf != "" {
		if data, err := os.ReadFile(pf); err == nil {
			os.MkdirAll(u.reg.Config.PidDir, 0755)
			os.WriteFile(u.PidPath(), data, 0644)
		}
	}
	return nil
}

func svcStop(u *Unit) error {
	// Tell the watcher this is intentional so the Restart= policy doesn't
	// fire after we kill the daemon. Cleared by the watcher when it sees
	// the flag, or by the deferred clear below if there was nothing to
	// stop.
	u.reg.markStopRequested(u.Canonical)
	defer u.ClearFailed() // a clean stop is not a failure

	pid, err := u.ReadPID()
	if err != nil {
		u.reg.clearStopRequested(u.Canonical) // no watcher to consume the flag
		u.RemoveCgroup()                      // best-effort cleanup of empty cgroup
		return nil                            // not running, nothing to do
	}
	// Defensive guard: never signal PID ≤ 1.
	//
	//   pid == 0  → kill(-0, ...) broadcasts to the caller's process group
	//   pid == 1  → kill(-1, ...) is the POSIX "send to ALL processes I'm
	//                allowed to signal" sentinel — catastrophic from PID 1
	//
	// Neither is a legitimate state (svcStart never writes 1 or 0), but a
	// corrupt pidfile or a manual-edit accident must not be allowed to
	// turn `systemctl stop` into a system-wide kill.
	if pid <= 1 {
		u.RemovePID()
		return fmt.Errorf("refusing to signal pid %d (corrupt pidfile)", pid)
	}
	if !processAlive(pid) {
		u.RemovePID()
		u.RemoveCgroup()
		u.reg.clearStopRequested(u.Canonical)
		return nil // pidfile stale
	}

	svc := u.Sections
	if execStop := svc.get("Service", "ExecStop"); execStop != "" {
		runServiceCommand(execStop, svc)
		time.Sleep(500 * time.Millisecond)
		if !processAlive(pid) {
			u.RemovePID()
			u.RemoveCgroup()
			return nil
		}
	}

	// KillMode=control-group (the default in systemd) uses the kernel's
	// cgroup.kill primitive: one write delivers SIGKILL to every process
	// in the cgroup, including double-forked children, daemons that
	// setsid'd themselves out of the parent's PGID, and dbus-activated
	// helpers — all the things PGID-kill misses. This is the architectural
	// improvement F1 brings.
	//
	// KillMode=process keeps the legacy PGID-only behaviour for services
	// that explicitly opt out (rare; mostly historical Type=forking
	// daemons that manage their own children).
	//
	// Fallback: cgroup.kill is Linux >=5.14. On older kernels the write
	// fails with ENOENT and we fall back to PGID-kill, preserving
	// behaviour at the cost of the extra-children guarantee.
	switch killModeFor(u) {
	case "control-group":
		if err := u.KillCgroup(); err == nil {
			u.WaitCgroupDrain(5 * time.Second)
			u.RemoveCgroup()
			u.RemovePID()
			return nil
		}
		// fallthrough to PGID-kill
		fallthrough
	case "process":
		// SIGTERM to the process group. Errors are surfaced; ESRCH means
		// the process died between processAlive() and Kill() — treated as
		// success.
		if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && !errIsESRCH(err) {
			return fmt.Errorf("kill -TERM %d: %w", pid, err)
		}
		for i := 0; i < 50; i++ {
			if !processAlive(pid) {
				u.RemovePID()
				u.RemoveCgroup()
				return nil
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errIsESRCH(err) {
			return fmt.Errorf("kill -KILL %d: %w", pid, err)
		}
		u.RemovePID()
		u.RemoveCgroup()
	}
	return nil
}

// errIsESRCH reports whether err is or wraps syscall.ESRCH ("no such
// process"). Used to treat a race between processAlive() and Kill() as
// success rather than an error.
func errIsESRCH(err error) bool {
	return err == syscall.ESRCH
}

func svcReload(u *Unit) error {
	pid, err := u.ReadPID()
	if err != nil || !processAlive(pid) {
		return fmt.Errorf("%s is not running", u.Canonical)
	}

	svc := u.Sections
	if execReload := svc.get("Service", "ExecReload"); execReload != "" {
		return runServiceCommand(execReload, svc)
	}
	if err := syscall.Kill(pid, syscall.SIGHUP); err != nil {
		return fmt.Errorf("kill -HUP %d: %w", pid, err)
	}
	return nil
}

// svcEnable creates the enable-symlinks for a unit. C3 will extend this
// to honour [Install] Alias= directives by creating alias symlinks; for
// C1 we keep the existing WantedBy/RequiredBy behaviour unchanged.
func svcEnable(u *Unit) error {
	if u.Masked {
		return fmt.Errorf("unit %s is masked", u.FullName())
	}
	if u.Path == "" {
		return nil // package may not have shipped the file yet
	}
	svc := u.Sections

	wantedBy := svc.get("Install", "WantedBy")
	if wantedBy == "" {
		wantedBy = "multi-user.target"
	}
	wantsDir := filepath.Join(u.reg.Config.EtcSystemdDir, wantedBy+".wants")
	os.MkdirAll(wantsDir, 0755)
	link := filepath.Join(wantsDir, u.FullName())
	os.Remove(link)
	if err := os.Symlink(u.Path, link); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Created symlink %s → %s.\n", link, u.Path)

	for _, reqBy := range svc.getAll("Install", "RequiredBy") {
		reqDir := filepath.Join(u.reg.Config.EtcSystemdDir, reqBy+".requires")
		os.MkdirAll(reqDir, 0755)
		reqLink := filepath.Join(reqDir, u.FullName())
		os.Remove(reqLink)
		os.Symlink(u.Path, reqLink)
		fmt.Fprintf(os.Stderr, "Created symlink %s → %s.\n", reqLink, u.Path)
	}

	// [Install] Alias= entries become symlinks at the top level of
	// /etc/systemd/system/, e.g. ssh.service with Alias=sshd.service
	// produces /etc/systemd/system/sshd.service -> <fragment path>.
	// Real systemd does this in install.c. The reason: the alias name
	// becomes a real on-disk filename, so anything globbing the dir
	// (including our own svcIsEnabled, distro tooling, dependency
	// resolvers in other unit files) sees both names.
	os.MkdirAll(u.reg.Config.EtcSystemdDir, 0755)
	for _, alias := range svc.getAll("Install", "Alias") {
		aliasName := strings.TrimSpace(alias)
		if aliasName == "" {
			continue
		}
		aliasLink := filepath.Join(u.reg.Config.EtcSystemdDir, aliasName)
		os.Remove(aliasLink)
		if err := os.Symlink(u.Path, aliasLink); err != nil {
			fmt.Fprintf(os.Stderr, "warning: alias symlink %s: %v\n", aliasLink, err)
			continue
		}
		fmt.Fprintf(os.Stderr, "Created symlink %s → %s.\n", aliasLink, u.Path)
	}

	return nil
}

// svcDisable removes every enable-time artefact for a unit: the wants/
// symlink (under any target), the requires/ symlinks, and the alias
// symlinks at the top of /etc/systemd/system/. Mirrors what real systemd
// does when undoing what svcEnable created.
func svcDisable(u *Unit) error {
	if err := u.reg.svcDisableByName(u.Canonical, u.Suffix); err != nil {
		return err
	}
	// Remove [Install] Alias= symlinks created by svcEnable. Each alias
	// is recorded both in u.Sections (the source of truth from the file
	// + drop-ins) and u.Aliases (the resolution-time set, which may
	// include inode-discovered aliases that we should NOT remove because
	// we didn't create them). So we trust [Install] Alias= here.
	for _, alias := range u.Sections.getAll("Install", "Alias") {
		aliasName := strings.TrimSpace(alias)
		if aliasName == "" {
			continue
		}
		aliasLink := filepath.Join(u.reg.Config.EtcSystemdDir, aliasName)
		// Only remove if it's a symlink we plausibly created (points back
		// to the unit's fragment path). Don't blindly delete a regular
		// file an admin might have placed there.
		if target, err := os.Readlink(aliasLink); err == nil && target == u.Path {
			os.Remove(aliasLink)
			fmt.Fprintf(os.Stderr, "Removed %s.\n", aliasLink)
		}
	}
	return nil
}

// svcDisableByName is the no-Unit fallback used when disable is called
// for a name that doesn't resolve through the registry. Method on
// Registry so it has access to Config.EtcSystemdDir without touching
// any package-level globals. Covers the wants/ and requires/ symlinks;
// alias symlinks at the top of /etc/systemd/system/ are left intact in
// this path because we don't know which they are without a Unit to
// consult.
func (r *Registry) svcDisableByName(name, suffix string) error {
	pattern := filepath.Join(r.Config.EtcSystemdDir, "*.wants", name+suffix)
	matches, _ := filepath.Glob(pattern)
	for _, m := range matches {
		os.Remove(m)
		fmt.Fprintf(os.Stderr, "Removed %s.\n", m)
	}
	reqPattern := filepath.Join(r.Config.EtcSystemdDir, "*.requires", name+suffix)
	reqMatches, _ := filepath.Glob(reqPattern)
	for _, m := range reqMatches {
		os.Remove(m)
		fmt.Fprintf(os.Stderr, "Removed %s.\n", m)
	}
	return nil
}

// svcIsActive returns true only when the unit is running AND has
// finished activating. For Type=simple/forking/oneshot units the
// activating marker is never written, so this reduces to IsRunning.
// For Type=notify units, the marker is present until READY=1 arrives;
// during that window the unit is "activating", not "active". Matches
// systemd's distinction — scripts polling is-active won't see a
// half-started Type=notify daemon as ready.
func svcIsActive(u *Unit) bool {
	return u.IsRunning() && !u.IsActivating()
}

func svcIsEnabled(u *Unit) bool {
	// Glob across both .service and .socket: when a unit answers to both
	// (e.g. ssh.service + ssh.socket), enabling either is "enabled".
	names := []string{u.Canonical}
	for alias := range u.Aliases {
		names = append(names, alias)
	}
	for _, suffix := range []string{".service", ".socket"} {
		for _, name := range names {
			pattern := filepath.Join(u.reg.Config.EtcSystemdDir, "*.wants", name+suffix)
			matches, _ := filepath.Glob(pattern)
			if len(matches) > 0 {
				return true
			}
		}
	}
	return false
}

// svcIsEnabledByName is the no-Unit fallback used when Resolve failed,
// so `is-enabled` can still report disabled. Method on Registry for
// access to Config.EtcSystemdDir.
func (r *Registry) svcIsEnabledByName(name string) bool {
	for _, suffix := range []string{".service", ".socket"} {
		matches, _ := filepath.Glob(filepath.Join(r.Config.EtcSystemdDir, "*.wants", name+suffix))
		if len(matches) > 0 {
			return true
		}
	}
	return false
}

// svcMaskName creates the /dev/null mask symlink. Naming-by-string is
// correct here — mask is a filesystem operation that doesn't need a
// resolved unit (you can mask a unit that doesn't exist yet). Method on
// Registry for access to Config.EtcSystemdDir.
func (r *Registry) svcMaskName(name string) error {
	target := filepath.Join(r.Config.EtcSystemdDir, name+".service")
	os.MkdirAll(filepath.Dir(target), 0755)
	os.Remove(target)
	return os.Symlink("/dev/null", target)
}

func (r *Registry) svcUnmaskName(name string) error {
	target := filepath.Join(r.Config.EtcSystemdDir, name+".service")
	link, err := os.Readlink(target)
	if err == nil && link == "/dev/null" {
		return os.Remove(target)
	}
	return nil
}

// svcStatus prints the kubectl-describe-style status block. The header
// line uses the user-supplied query name (matches systemd's behaviour),
// but the underlying state (pidfile, logfile) is read from the resolved
// Unit so an alias query and a canonical query agree.
func svcStatus(u *Unit, displayName string) {
	displayName = normalizeName(displayName)
	active := "inactive"
	pid := 0
	failedExit := 0
	if u != nil {
		switch {
		case u.IsFailed():
			active = "failed"
			failedExit = u.LastExitCode()
		case u.IsActivating():
			// Running but hasn't sent READY=1 yet. systemd shows this
			// as "activating (start)".
			active = "activating"
			if p, err := u.ReadPID(); err == nil && processAlive(p) {
				pid = p
			}
		default:
			if p, err := u.ReadPID(); err == nil && processAlive(p) {
				active = "active"
				pid = p
			}
		}
	}
	desc := displayName
	if u != nil {
		if d := u.Sections.get("Unit", "Description"); d != "" {
			desc = d
		}
	}
	fmt.Printf("● %s.service - %s\n", displayName, desc)
	fmt.Printf("     Active: %s", active)
	if pid > 0 {
		fmt.Printf(" (running, PID %d)", pid)
	} else if failedExit != 0 {
		fmt.Printf(" (Result: exit-code, code=%d)", failedExit)
	}
	fmt.Println()

	// Cgroup accounting (systemd's status format). Only printed when the
	// service is running and the controllers report non-zero values —
	// don't clutter the output for inactive units or kernels without the
	// memory/pids controllers.
	if u != nil && pid > 0 {
		if mem := u.CgroupMemoryCurrent(); mem > 0 {
			fmt.Printf("     Memory: %s\n", humanBytes(mem))
		}
		if tasks := u.CgroupTasksCurrent(); tasks > 0 {
			fmt.Printf("     Tasks: %d\n", tasks)
		}
	}

	// Show last few lines of log if available.
	if u != nil {
		if logPath := u.LogPath(); fileExists(logPath) {
			fmt.Println()
			showLastLines(logPath, 5)
		}
	}
}

func svcShow(u *Unit, displayName string, prop string, valueOnly bool) {
	synth := map[string]string{}
	if u != nil && !u.Masked {
		synth["SourcePath"] = u.Path
		synth["LoadState"] = "loaded"
		if u.Sections.get("Service", "ExecReload") != "" {
			synth["CanReload"] = "yes"
		} else {
			synth["CanReload"] = "no"
		}
		for _, section := range []string{"Unit", "Service", "Install"} {
			for _, kv := range u.Sections.sections[section] {
				if _, exists := synth[kv.key]; !exists {
					synth[kv.key] = kv.value
				}
			}
		}
	} else if u != nil && u.Masked {
		synth["LoadState"] = "masked"
	} else {
		synth["LoadState"] = "not-found"
	}
	_ = displayName

	if prop != "" {
		val := synth[prop]
		if valueOnly {
			fmt.Println(val)
		} else {
			fmt.Printf("%s=%s\n", prop, val)
		}
		return
	}
	for k, v := range synth {
		fmt.Printf("%s=%s\n", k, v)
	}
}

func svcCat(u *Unit, displayName string) {
	if u == nil || u.Path == "" {
		fmt.Fprintf(os.Stderr, "No files found for %s.service.\n", normalizeName(displayName))
		os.Exit(1)
	}
	data, err := os.ReadFile(u.Path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read %s: %v\n", u.Path, err)
		os.Exit(1)
	}
	fmt.Printf("# %s\n", u.Path)
	os.Stdout.Write(data)
}

func svcListUnits(reg *Registry, stateFilter, typeFilter string, noLegend bool) {
	if typeFilter != "" && typeFilter != "service" {
		return
	}
	if !noLegend {
		fmt.Printf("%-40s %-10s %-10s %s\n", "UNIT", "LOAD", "ACTIVE", "DESCRIPTION")
	}
	seen := map[string]bool{}
	for _, dir := range reg.Config.ServiceDirs {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".service") || seen[e.Name()] {
				continue
			}
			seen[e.Name()] = true
			name := strings.TrimSuffix(e.Name(), ".service")
			u, err := reg.Resolve(name)
			if err != nil {
				continue
			}
			active := "inactive"
			if svcIsActive(u) {
				active = "active"
			}
			if stateFilter == "running" && active != "active" {
				continue
			}
			if stateFilter == "failed" {
				continue
			}
			desc := name
			if d := u.Sections.get("Unit", "Description"); d != "" {
				desc = d
			}
			fmt.Printf("%-40s %-10s %-10s %s\n",
				e.Name(), "loaded", active, desc)
		}
	}
}

func svcListUnitFiles(reg *Registry, noLegend bool) {
	if !noLegend {
		fmt.Printf("%-50s %s\n", "UNIT FILE", "STATE")
	}
	seen := map[string]bool{}
	for _, dir := range reg.Config.ServiceDirs {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".service") || seen[e.Name()] {
				continue
			}
			seen[e.Name()] = true
			name := strings.TrimSuffix(e.Name(), ".service")
			state := "disabled"
			u, err := reg.Resolve(name)
			if err == nil {
				if u.Masked {
					state = "masked"
				} else if svcIsEnabled(u) {
					state = "enabled"
				}
			}
			fmt.Printf("%-50s %s\n", e.Name(), state)
		}
	}
}

// startEnabledServices starts all services in multi-user.target.wants.
// Called at boot by lohar (PID 1). Uses globalRegistry so the same Unit
// objects (and their watcher coordination) are visible to syslog,
// journalctl, and the IPC handler that follow.
// startEnabledServices boots the units in multi-user.target.wants/
// honouring After=/Before= ordering. Activation happens in waves: each
// wave's units start in parallel, and the next wave waits until all
// units in the current wave reach 'active' (per F2: for Type=notify
// this means READY=1; for Type=simple/forking it's fork+exec; for
// Type=oneshot it's clean exit). This is what makes multi-service
// stacks like postgres -> pgbouncer -> webapp boot in the right order
// instead of racing.
//
// Failures within a wave don't abort the wave -- other units in the
// same wave still try to start. They DO advance the next wave (we
// don't gate on a previous wave's failures here; failure propagation
// for Requires= is F3.5). Matches systemd's behaviour where the
// dependency graph is best-effort under failures.
func startEnabledServices() {
	reg := globalRegistry
	if reg == nil {
		return
	}
	wantsDir := filepath.Join(reg.Config.EtcSystemdDir, "multi-user.target.wants")
	entries, err := os.ReadDir(wantsDir)
	if err != nil {
		return
	}

	// Resolve every enabled unit. Drop any we can't find (the wants/
	// symlink might point at a unit whose file got removed).
	var enabled []*Unit
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".service") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".service")
		u, err := reg.Resolve(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "lohar: cannot resolve %s: %v\n", name, err)
			continue
		}
		enabled = append(enabled, u)
	}

	// Topologically sort by After=/Before= and activate wave by wave.
	groups := buildStartGraph(enabled)
	for groupIdx, group := range groups {
		var wg sync.WaitGroup
		for _, u := range group {
			wg.Add(1)
			go func(u *Unit) {
				defer wg.Done()
				if err := svcStart(u); err != nil {
					fmt.Fprintf(os.Stderr, "lohar: failed to start %s: %v\n", u.Canonical, err)
					return
				}
				fmt.Fprintf(os.Stderr, "lohar: started %s (wave %d/%d)\n",
					u.Canonical, groupIdx+1, len(groups))
			}(u)
		}
		wg.Wait()
	}
}

// --- journalctl shim ---

// runJournalctl handles /usr/bin/journalctl invocations.
// Reads service log files from /var/log/bhatti/<service>.log.
func runJournalctl(args []string) {
	var unit string
	var follow bool
	var lines int

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-f" || arg == "--follow":
			follow = true
		case arg == "-u" || arg == "--unit":
			if i+1 < len(args) {
				unit = args[i+1]
				i++
			}
		case strings.HasPrefix(arg, "-u"):
			unit = strings.TrimPrefix(arg, "-u")
		case strings.HasPrefix(arg, "--unit="):
			unit = strings.TrimPrefix(arg, "--unit=")
		case arg == "-n" || arg == "--lines":
			if i+1 < len(args) {
				lines, _ = strconv.Atoi(args[i+1])
				i++
			}
		case strings.HasPrefix(arg, "-n"):
			lines, _ = strconv.Atoi(strings.TrimPrefix(arg, "-n"))
		case strings.HasPrefix(arg, "--lines="):
			lines, _ = strconv.Atoi(strings.TrimPrefix(arg, "--lines="))
		}
		// Ignore: --no-pager, -p/--priority, -q, --boot, etc.
	}

	// Build a Registry with production paths. journalctl is a short-
	// lived client process so this Registry just lives for the
	// invocation; it doesn't need to be globalRegistry. The Config
	// is what matters — it tells the resolver where unit files live
	// and where logs are kept.
	reg := NewRegistry(ProductionConfig())

	if unit == "" {
		// No unit specified — list available logs.
		entries, err := os.ReadDir(reg.Config.LogDir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "No journal files found.")
			return
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".log") {
				fmt.Println(strings.TrimSuffix(e.Name(), ".log"))
			}
		}
		return
	}

	// Resolve the unit through the registry so journalctl -u sshd and
	// journalctl -u ssh read the same file when ssh has Alias=sshd.
	u, _ := reg.Resolve(unit)
	var logPath string
	if u != nil {
		logPath = u.LogPath()
	} else {
		// Fallback: file might exist for a unit we couldn't resolve
		// (e.g. a custom log written by the syslog receiver under an
		// arbitrary tag).
		logPath = filepath.Join(reg.Config.LogDir, strings.TrimSuffix(unit, ".service")+".log")
	}

	if follow {
		// tail -f equivalent
		tailFollow(logPath)
		return
	}

	if lines > 0 {
		showLastLines(logPath, lines)
		return
	}

	// Default: dump entire log.
	f, err := os.Open(logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "No logs for %s.\n", unit)
		os.Exit(1)
	}
	defer f.Close()
	io.Copy(os.Stdout, f)
}

// --- Helpers ---

func runServiceCommand(cmdLine string, svc serviceFile) error {
	cmdLine = strings.TrimLeft(cmdLine, "-!+:@")
	if cmdLine == "" {
		return nil
	}
	cmd := exec.Command("/bin/sh", "-c", cmdLine)
	cmd.Env = buildServiceEnv(svc)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func buildServiceEnv(svc serviceFile) []string {
	env := os.Environ()
	for _, e := range svc.getAll("Service", "Environment") {
		env = append(env, strings.Trim(e, "\"'"))
	}
	for _, f := range svc.getAll("Service", "EnvironmentFile") {
		f = strings.TrimLeft(f, "-")
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") {
				env = append(env, line)
			}
		}
	}
	return env
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func showLastLines(path string, n int) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	start := len(lines) - n
	if start < 0 {
		start = 0
	}
	for _, l := range lines[start:] {
		fmt.Println(l)
	}
}

func tailFollow(path string) {
	// Open or wait for the file to appear.
	var f *os.File
	for i := 0; i < 50; i++ {
		var err error
		f, err = os.Open(path)
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if f == nil {
		fmt.Fprintf(os.Stderr, "No logs for unit.\n")
		os.Exit(1)
	}
	defer f.Close()

	// Seek to end, then poll for new data.
	f.Seek(0, io.SeekEnd)
	buf := make([]byte, 4096)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			os.Stdout.Write(buf[:n])
		}
		if err != nil {
			time.Sleep(200 * time.Millisecond)
		}
	}
}

// --- .service file parser ---

type kvPair struct {
	key   string
	value string
}

type serviceFile struct {
	sections map[string][]kvPair
}

func parseServiceFile(path string) serviceFile {
	sf := serviceFile{sections: make(map[string][]kvPair)}
	data, err := os.ReadFile(path)
	if err != nil {
		return sf
	}
	section := ""

	// First pass: glue backslash-continuation lines together so multi-line
	// directives parse as one logical line. systemd does this in
	// `extract_first_word` (`src/basic/extract-word.c`); a backslash
	// immediately before the newline joins this line with the next, with
	// the backslash and the newline dropped. Common in upstream unit files
	// for long ExecStart/Environment directives; without it those land as
	// truncated values plus a stream of unparseable orphan lines.
	rawLines := strings.Split(string(data), "\n")
	lines := make([]string, 0, len(rawLines))
	var joined strings.Builder
	for _, l := range rawLines {
		if strings.HasSuffix(l, "\\") {
			joined.WriteString(l[:len(l)-1])
			joined.WriteByte(' ')
			continue
		}
		if joined.Len() > 0 {
			joined.WriteString(l)
			lines = append(lines, joined.String())
			joined.Reset()
			continue
		}
		lines = append(lines, l)
	}
	// Trailing backslash with no following line: keep what we have.
	if joined.Len() > 0 {
		lines = append(lines, joined.String())
	}

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line[1 : len(line)-1]
			continue
		}
		if idx := strings.IndexByte(line, '='); idx >= 0 && section != "" {
			key := strings.TrimSpace(line[:idx])
			value := strings.TrimSpace(line[idx+1:])
			sf.sections[section] = append(sf.sections[section], kvPair{key, value})
		}
	}
	return sf
}

// get returns the LAST value for the given section/key. systemd's behaviour:
// for scalar directives, a later assignment overrides an earlier one. This is
// what makes drop-in overrides work — the fragment supplies the initial value
// and the drop-in supplies the override; the drop-in loads after, so its
// value is the one returned here.
func (sf serviceFile) get(section, key string) string {
	var val string
	for _, kv := range sf.sections[section] {
		if kv.key == key {
			val = kv.value
		}
	}
	return val
}

// getAll returns every value for the given section/key in order. Used for
// list-typed directives like ExecStartPre, ExecStartPost, Environment, EnvironmentFile.
// Reset markers (empty values) are filtered out by merge() before this is
// called, so callers don't need to handle them.
func (sf serviceFile) getAll(section, key string) []string {
	var vals []string
	for _, kv := range sf.sections[section] {
		if kv.key == key {
			vals = append(vals, kv.value)
		}
	}
	return vals
}

// merge appends another serviceFile's directives onto this one, implementing
// systemd's drop-in merge semantics:
//
//   - List-valued directives (ExecStartPre, ExecStartPost, Environment, etc.) accumulate —
//     a later directive is appended to the list returned by getAll().
//
//   - Scalar directives (ExecStart, Type, User) effectively override because
//     get() returns the LAST value.
//
//   - **Reset semantics**: an empty assignment (e.g. `ExecStart=`) clears
//     every prior entry for that key in that section. This is the
//     conventional way drop-ins replace a fragment's directive cleanly:
//
//     [Service]
//     ExecStart=
//     ExecStart=/usr/bin/foo --new-args
//
//     The empty marker itself is dropped after the reset; only the
//     subsequent assignment(s) remain.
func (sf *serviceFile) merge(other serviceFile) {
	if sf.sections == nil {
		sf.sections = map[string][]kvPair{}
	}
	for section, kvs := range other.sections {
		for _, kv := range kvs {
			if kv.value == "" {
				// Reset: drop every prior entry for this key in this section.
				filtered := sf.sections[section][:0]
				for _, existing := range sf.sections[section] {
					if existing.key != kv.key {
						filtered = append(filtered, existing)
					}
				}
				sf.sections[section] = filtered
				continue // don't keep the reset marker itself
			}
			sf.sections[section] = append(sf.sections[section], kv)
		}
	}
}
