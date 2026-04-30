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
	"syscall"
	"time"
)

// systemctl shim — reads .service files and manages processes directly.
// No real systemd. lohar (PID 1) handles zombie reaping.
//
// Covers the full surface used by Debian/Ubuntu package tooling
// (deb-systemd-helper, deb-systemd-invoke, invoke-rc.d) plus
// interactive use (start, stop, reload, status, logs).

var serviceDirs = []string{
	"/etc/systemd/system",
	"/usr/lib/systemd/system",
	"/lib/systemd/system",
}

// etcSystemdDir is where enable/disable creates wants/ symlinks and where
// mask creates the /dev/null symlink. Always /etc/systemd/system in production;
// overridable in tests so we don't have to run as root or clobber the host's
// real systemd state.
var etcSystemdDir = "/etc/systemd/system"

const (
	pidDir = "/run/bhatti/services"
	logDir = "/var/log/bhatti"
)

// Well-known targets that are always "active" — invoke-rc.d checks
// sysinit.target to determine runlevel.
var alwaysActiveTargets = map[string]bool{
	"sysinit":      true,
	"multi-user":   true,
	"default":      true,
	"network":      true,
	"network-online": true,
	"sockets":      true,
	"basic":        true,
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

	reg := NewRegistry()

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
				units[i] = resolveSocketToService(u) + ".service"
			}
		}
	}

	_ = quiet // TODO: suppress stdout when set

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
				svcDisableByName(normalizeName(raw), unitSuffix(raw))
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
			enabled = svcIsEnabledByName(normalizeName(units[0]))
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
			if err := svcMaskName(normalizeName(raw)); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to mask %s: %v\n", raw, err)
			}
		}
	case "unmask":
		for _, raw := range units {
			if err := svcUnmaskName(normalizeName(raw)); err != nil {
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
	case "daemon-reload", "daemon-reexec", "reset-failed":
		// no-op
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
// Checks the Service= directive in the .socket file, falls back to name match.
// This runs before registry resolution because the dispatch needs a .service
// name to look up. We don't (yet) implement socket activation — the .service
// is started directly.
func resolveSocketToService(name string) string {
	base := normalizeName(name)
	for _, dir := range serviceDirs {
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

	svc := u.Sections

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

	switch svcType {
	case "oneshot":
		return runServiceCommand(execStart, svc)
	case "forking":
		return startForking(u, execStart, svc)
	default:
		// simple, exec, notify, dbus — all treated as simple daemons.
		return startDaemon(u, execStart, svc)
	}
}

func startDaemon(u *Unit, execStart string, svc serviceFile) error {
	execStart = strings.TrimLeft(execStart, "-!+:@")
	if execStart == "" {
		return fmt.Errorf("empty ExecStart")
	}

	cmd := exec.Command("/bin/sh", "-c", "exec "+execStart)
	cmd.Dir = svc.get("Service", "WorkingDirectory")
	cmd.Env = buildServiceEnv(svc)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil

	// Capture stdout/stderr to log file for debugging.
	os.MkdirAll(logDir, 0755)
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

	u.WritePID(cmd.Process.Pid)
	cmd.Process.Release()
	return nil
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
			os.MkdirAll(pidDir, 0755)
			os.WriteFile(u.PidPath(), data, 0644)
		}
	}
	return nil
}

func svcStop(u *Unit) error {
	pid, err := u.ReadPID()
	if err != nil || !processAlive(pid) {
		u.RemovePID()
		return nil
	}

	svc := u.Sections
	if execStop := svc.get("Service", "ExecStop"); execStop != "" {
		runServiceCommand(execStop, svc)
		time.Sleep(500 * time.Millisecond)
		if !processAlive(pid) {
			u.RemovePID()
			return nil
		}
	}

	syscall.Kill(-pid, syscall.SIGTERM)
	for i := 0; i < 50; i++ {
		if !processAlive(pid) {
			u.RemovePID()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	syscall.Kill(-pid, syscall.SIGKILL)
	u.RemovePID()
	return nil
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
	return syscall.Kill(pid, syscall.SIGHUP)
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
	wantsDir := filepath.Join(etcSystemdDir, wantedBy+".wants")
	os.MkdirAll(wantsDir, 0755)
	link := filepath.Join(wantsDir, u.FullName())
	os.Remove(link)
	if err := os.Symlink(u.Path, link); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Created symlink %s → %s.\n", link, u.Path)

	for _, reqBy := range svc.getAll("Install", "RequiredBy") {
		reqDir := filepath.Join(etcSystemdDir, reqBy+".requires")
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
	os.MkdirAll(etcSystemdDir, 0755)
	for _, alias := range svc.getAll("Install", "Alias") {
		aliasName := strings.TrimSpace(alias)
		if aliasName == "" {
			continue
		}
		aliasLink := filepath.Join(etcSystemdDir, aliasName)
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
	if err := svcDisableByName(u.Canonical, u.Suffix); err != nil {
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
		aliasLink := filepath.Join(etcSystemdDir, aliasName)
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
// for a name that doesn't resolve through the registry. Same glob logic
// as before — covers the wants/ and requires/ symlinks; alias symlinks
// at the top of /etc/systemd/system/ are left intact in this path because
// we don't know which they are without a Unit to consult.
func svcDisableByName(name, suffix string) error {
	pattern := filepath.Join(etcSystemdDir, "*.wants", name+suffix)
	matches, _ := filepath.Glob(pattern)
	for _, m := range matches {
		os.Remove(m)
		fmt.Fprintf(os.Stderr, "Removed %s.\n", m)
	}
	reqPattern := filepath.Join(etcSystemdDir, "*.requires", name+suffix)
	reqMatches, _ := filepath.Glob(reqPattern)
	for _, m := range reqMatches {
		os.Remove(m)
		fmt.Fprintf(os.Stderr, "Removed %s.\n", m)
	}
	return nil
}

func svcIsActive(u *Unit) bool {
	return u.IsRunning()
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
			pattern := filepath.Join(etcSystemdDir, "*.wants", name+suffix)
			matches, _ := filepath.Glob(pattern)
			if len(matches) > 0 {
				return true
			}
		}
	}
	return false
}

// svcIsEnabledByName is the no-Unit fallback. Used for unresolved names
// (where Resolve failed) so `is-enabled` can still report disabled.
func svcIsEnabledByName(name string) bool {
	for _, suffix := range []string{".service", ".socket"} {
		matches, _ := filepath.Glob(filepath.Join(etcSystemdDir, "*.wants", name+suffix))
		if len(matches) > 0 {
			return true
		}
	}
	return false
}

// svcMaskName creates the /dev/null mask symlink. Naming-by-string is
// correct here — mask is a filesystem operation that doesn't need a
// resolved unit (you can mask a unit that doesn't exist yet).
func svcMaskName(name string) error {
	target := filepath.Join(etcSystemdDir, name+".service")
	os.MkdirAll(filepath.Dir(target), 0755)
	os.Remove(target)
	return os.Symlink("/dev/null", target)
}

func svcUnmaskName(name string) error {
	target := filepath.Join(etcSystemdDir, name+".service")
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
	if u != nil {
		if p, err := u.ReadPID(); err == nil && processAlive(p) {
			active = "active"
			pid = p
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
	}
	fmt.Println()

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
	for _, dir := range serviceDirs {
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
	for _, dir := range serviceDirs {
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
// Called at boot by lohar (PID 1). Each name is resolved through the
// registry so aliases share state with their canonical units.
func startEnabledServices() {
	wantsDir := "/etc/systemd/system/multi-user.target.wants"
	entries, err := os.ReadDir(wantsDir)
	if err != nil {
		return
	}
	reg := NewRegistry()
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
		if err := svcStart(u); err != nil {
			fmt.Fprintf(os.Stderr, "lohar: failed to start %s: %v\n", name, err)
		} else {
			fmt.Fprintf(os.Stderr, "lohar: started %s\n", name)
		}
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

	if unit == "" {
		// No unit specified — list available logs.
		entries, err := os.ReadDir(logDir)
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
	reg := NewRegistry()
	u, _ := reg.Resolve(unit)
	var logPath string
	if u != nil {
		logPath = u.LogPath()
	} else {
		// Fallback: file might exist for a unit we couldn't resolve
		// (e.g. a custom log written by the syslog receiver under an
		// arbitrary tag).
		logPath = filepath.Join(logDir, strings.TrimSuffix(unit, ".service")+".log")
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
	for _, line := range strings.Split(string(data), "\n") {
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
// list-typed directives like ExecStartPre, Environment, EnvironmentFile.
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
//   - List-valued directives (ExecStartPre, Environment, etc.) accumulate —
//     a later directive is appended to the list returned by getAll().
//   - Scalar directives (ExecStart, Type, User) effectively override because
//     get() returns the LAST value.
//   - **Reset semantics**: an empty assignment (e.g. `ExecStart=`) clears
//     every prior entry for that key in that section. This is the
//     conventional way drop-ins replace a fragment's directive cleanly:
//
//         [Service]
//         ExecStart=
//         ExecStart=/usr/bin/foo --new-args
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
