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

	_ = quiet // TODO: suppress stdout when set

	switch command {
	case "start":
		for _, u := range units {
			if err := svcStart(normalizeName(u)); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to start %s: %v\n", u, err)
				os.Exit(1)
			}
		}
	case "stop":
		for _, u := range units {
			if err := svcStop(normalizeName(u)); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to stop %s: %v\n", u, err)
				os.Exit(1)
			}
		}
	case "restart":
		for _, u := range units {
			name := normalizeName(u)
			svcStop(name)
			if err := svcStart(name); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to restart %s: %v\n", u, err)
				os.Exit(1)
			}
		}
	case "try-restart":
		// Restart only if already running.
		for _, u := range units {
			name := normalizeName(u)
			if svcIsActive(name) {
				svcStop(name)
				if err := svcStart(name); err != nil {
					fmt.Fprintf(os.Stderr, "Failed to restart %s: %v\n", u, err)
					os.Exit(1)
				}
			}
		}
	case "reload":
		for _, u := range units {
			if err := svcReload(normalizeName(u)); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to reload %s: %v\n", u, err)
				os.Exit(1)
			}
		}
	case "reload-or-restart":
		for _, u := range units {
			name := normalizeName(u)
			if err := svcReload(name); err != nil {
				svcStop(name)
				if err := svcStart(name); err != nil {
					fmt.Fprintf(os.Stderr, "Failed to restart %s: %v\n", u, err)
					os.Exit(1)
				}
			}
		}
	case "enable":
		for _, u := range units {
			name := normalizeName(u)
			if err := svcEnable(name); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to enable %s: %v\n", u, err)
				os.Exit(1)
			}
			if nowFlag {
				svcStart(name)
			}
		}
	case "disable":
		for _, u := range units {
			name := normalizeName(u)
			if nowFlag {
				svcStop(name)
			}
			if err := svcDisable(name); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to disable %s: %v\n", u, err)
				os.Exit(1)
			}
		}
	case "is-active":
		if len(units) == 0 {
			os.Exit(1)
		}
		if svcIsActive(normalizeName(units[0])) {
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
		if svcIsEnabled(normalizeName(units[0])) {
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
		svcStatus(normalizeName(units[0]))
	case "show":
		if len(units) == 0 {
			return
		}
		svcShow(normalizeName(units[0]), showProp, showValue)
	case "cat":
		if len(units) == 0 {
			os.Exit(1)
		}
		svcCat(normalizeName(units[0]))
	case "mask":
		for _, u := range units {
			if err := svcMask(normalizeName(u)); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to mask %s: %v\n", u, err)
			}
		}
	case "unmask":
		for _, u := range units {
			if err := svcUnmask(normalizeName(u)); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to unmask %s: %v\n", u, err)
			}
		}
	case "kill":
		sig := syscall.SIGTERM
		if signalName == "KILL" || signalName == "SIGKILL" {
			sig = syscall.SIGKILL
		} else if signalName == "HUP" || signalName == "SIGHUP" {
			sig = syscall.SIGHUP
		}
		for _, u := range units {
			if pid, err := readPID(normalizeName(u)); err == nil {
				syscall.Kill(-pid, sig)
			}
		}
	case "daemon-reload", "daemon-reexec", "reset-failed":
		// no-op
	case "preset":
		for _, u := range units {
			svcEnable(normalizeName(u))
		}
	case "is-system-running":
		fmt.Println("running")
	case "list-units":
		svcListUnits(stateFilter, typeFilter, noLegend)
	case "list-unit-files":
		svcListUnitFiles(noLegend)
	default:
		// Unknown command — succeed silently. Package scripts sometimes
		// call obscure systemctl subcommands; failing breaks installs.
		if command != "" {
			fmt.Fprintf(os.Stderr, "systemctl: unknown command %q (ignored)\n", command)
		}
	}
}

// --- Name resolution ---

// normalizeName strips the .service suffix if present.
func normalizeName(name string) string {
	return strings.TrimSuffix(name, ".service")
}

// isTarget returns true if the name refers to a .target unit.
func isTarget(name string) bool {
	return strings.HasSuffix(name, ".target") || alwaysActiveTargets[name]
}

// findServiceFile locates the .service file for a unit.
// Also resolves aliases (e.g. sshd -> ssh via Alias=sshd.service).
func findServiceFile(name string) string {
	for _, dir := range serviceDirs {
		path := filepath.Join(dir, name+".service")
		if target, err := os.Readlink(path); err == nil {
			if target == "/dev/null" {
				return "" // masked
			}
		}
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	// Check aliases.
	for _, dir := range serviceDirs {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".service") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			svc := parseServiceFile(path)
			for _, alias := range svc.getAll("Install", "Alias") {
				if strings.TrimSuffix(alias, ".service") == name {
					return path
				}
			}
		}
	}
	return ""
}

// isMasked checks if a unit is masked (symlinked to /dev/null).
func isMasked(name string) bool {
	for _, dir := range serviceDirs {
		path := filepath.Join(dir, name+".service")
		if target, err := os.Readlink(path); err == nil && target == "/dev/null" {
			return true
		}
	}
	return false
}

// --- PID + process management ---

func pidFile(name string) string {
	return filepath.Join(pidDir, name+".pid")
}

func readPID(name string) (int, error) {
	data, err := os.ReadFile(pidFile(name))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

func processAlive(pid int) bool {
	_, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	return err == nil
}

func serviceLogPath(name string) string {
	return filepath.Join(logDir, name+".log")
}

// --- Service operations ---

func svcStart(name string) error {
	if pid, err := readPID(name); err == nil && processAlive(pid) {
		return nil // already running
	}

	path := findServiceFile(name)
	if path == "" {
		return fmt.Errorf("unit %s.service not found or masked", name)
	}

	svc := parseServiceFile(path)

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
		if u := svc.get("Service", "User"); u != "" {
			exec.Command("chown", u, dir).Run()
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
		return fmt.Errorf("no ExecStart in %s", path)
	}

	svcType := svc.get("Service", "Type")
	if svcType == "" {
		svcType = "simple"
	}

	switch svcType {
	case "oneshot":
		return runServiceCommand(execStart, svc)
	case "forking":
		return startForking(name, execStart, svc)
	default:
		// simple, exec, notify, dbus — all treated as simple daemons.
		return startDaemon(name, execStart, svc)
	}
}

func startDaemon(name, execStart string, svc serviceFile) error {
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
	logFile, err := os.OpenFile(serviceLogPath(name),
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

	os.MkdirAll(pidDir, 0755)
	os.WriteFile(pidFile(name), []byte(strconv.Itoa(cmd.Process.Pid)), 0644)
	cmd.Process.Release()
	return nil
}

func startForking(name, execStart string, svc serviceFile) error {
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
			os.WriteFile(pidFile(name), data, 0644)
		}
	}
	return nil
}

func svcStop(name string) error {
	pid, err := readPID(name)
	if err != nil || !processAlive(pid) {
		os.Remove(pidFile(name))
		return nil
	}

	// ExecStop if defined
	if path := findServiceFile(name); path != "" {
		svc := parseServiceFile(path)
		if execStop := svc.get("Service", "ExecStop"); execStop != "" {
			runServiceCommand(execStop, svc)
			time.Sleep(500 * time.Millisecond)
			if !processAlive(pid) {
				os.Remove(pidFile(name))
				return nil
			}
		}
	}

	syscall.Kill(-pid, syscall.SIGTERM)
	for i := 0; i < 50; i++ {
		if !processAlive(pid) {
			os.Remove(pidFile(name))
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	syscall.Kill(-pid, syscall.SIGKILL)
	os.Remove(pidFile(name))
	return nil
}

func svcReload(name string) error {
	pid, err := readPID(name)
	if err != nil || !processAlive(pid) {
		return fmt.Errorf("%s is not running", name)
	}

	// Use ExecReload if defined, otherwise SIGHUP.
	if path := findServiceFile(name); path != "" {
		svc := parseServiceFile(path)
		if execReload := svc.get("Service", "ExecReload"); execReload != "" {
			return runServiceCommand(execReload, svc)
		}
	}
	return syscall.Kill(pid, syscall.SIGHUP)
}

func svcEnable(name string) error {
	path := findServiceFile(name)
	if path == "" {
		return fmt.Errorf("unit %s.service not found", name)
	}

	svc := parseServiceFile(path)
	wantedBy := svc.get("Install", "WantedBy")
	if wantedBy == "" {
		wantedBy = "multi-user.target"
	}

	wantsDir := filepath.Join("/etc/systemd/system", wantedBy+".wants")
	os.MkdirAll(wantsDir, 0755)

	link := filepath.Join(wantsDir, name+".service")
	os.Remove(link)
	if err := os.Symlink(path, link); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Created symlink %s → %s.\n", link, path)
	return nil
}

func svcDisable(name string) error {
	pattern := "/etc/systemd/system/*.wants/" + name + ".service"
	matches, _ := filepath.Glob(pattern)
	for _, m := range matches {
		os.Remove(m)
		fmt.Fprintf(os.Stderr, "Removed %s.\n", m)
	}
	return nil
}

func svcIsActive(name string) bool {
	// Well-known targets are always active.
	if isTarget(name) {
		return true
	}
	pid, err := readPID(name)
	if err != nil {
		return false
	}
	return processAlive(pid)
}

func svcIsEnabled(name string) bool {
	pattern := "/etc/systemd/system/*.wants/" + name + ".service"
	matches, _ := filepath.Glob(pattern)
	return len(matches) > 0
}

func svcMask(name string) error {
	target := filepath.Join("/etc/systemd/system", name+".service")
	os.Remove(target)
	return os.Symlink("/dev/null", target)
}

func svcUnmask(name string) error {
	target := filepath.Join("/etc/systemd/system", name+".service")
	link, err := os.Readlink(target)
	if err == nil && link == "/dev/null" {
		return os.Remove(target)
	}
	return nil
}

func svcStatus(name string) {
	active := "inactive"
	pid := 0
	if p, err := readPID(name); err == nil && processAlive(p) {
		active = "active"
		pid = p
	}
	desc := name
	if path := findServiceFile(name); path != "" {
		svc := parseServiceFile(path)
		if d := svc.get("Unit", "Description"); d != "" {
			desc = d
		}
	}
	fmt.Printf("● %s.service - %s\n", name, desc)
	fmt.Printf("     Active: %s", active)
	if pid > 0 {
		fmt.Printf(" (running, PID %d)", pid)
	}
	fmt.Println()

	// Show last few lines of log if available.
	if logPath := serviceLogPath(name); fileExists(logPath) {
		fmt.Println()
		showLastLines(logPath, 5)
	}
}

func svcShow(name string, prop string, valueOnly bool) {
	// Synthesized properties that invoke-rc.d checks.
	synth := map[string]string{}

	path := findServiceFile(name)
	if path != "" {
		synth["SourcePath"] = path
		synth["LoadState"] = "loaded"
		svc := parseServiceFile(path)
		if svc.get("Service", "ExecReload") != "" {
			synth["CanReload"] = "yes"
		} else {
			synth["CanReload"] = "no"
		}
		// Include all file properties.
		for _, section := range []string{"Unit", "Service", "Install"} {
			for _, kv := range svc.sections[section] {
				if _, exists := synth[kv.key]; !exists {
					synth[kv.key] = kv.value
				}
			}
		}
	} else if isMasked(name) {
		synth["LoadState"] = "masked"
	} else {
		synth["LoadState"] = "not-found"
	}

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

func svcCat(name string) {
	path := findServiceFile(name)
	if path == "" {
		fmt.Fprintf(os.Stderr, "No files found for %s.service.\n", name)
		os.Exit(1)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read %s: %v\n", path, err)
		os.Exit(1)
	}
	fmt.Printf("# %s\n", path)
	os.Stdout.Write(data)
}

func svcListUnits(stateFilter, typeFilter string, noLegend bool) {
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
			active := "inactive"
			if svcIsActive(name) {
				active = "active"
			}
			if stateFilter == "running" && active != "active" {
				continue
			}
			if stateFilter == "failed" {
				continue
			}
			desc := name
			if path := findServiceFile(name); path != "" {
				svc := parseServiceFile(path)
				if d := svc.get("Unit", "Description"); d != "" {
					desc = d
				}
			}
			fmt.Printf("%-40s %-10s %-10s %s\n",
				e.Name(), "loaded", active, desc)
		}
	}
}

func svcListUnitFiles(noLegend bool) {
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
			if svcIsEnabled(name) {
				state = "enabled"
			}
			if isMasked(name) {
				state = "masked"
			}
			fmt.Printf("%-50s %s\n", e.Name(), state)
		}
	}
}

// startEnabledServices starts all services in multi-user.target.wants.
// Called at boot by lohar (PID 1).
func startEnabledServices() {
	wantsDir := "/etc/systemd/system/multi-user.target.wants"
	entries, err := os.ReadDir(wantsDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".service") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".service")
		if err := svcStart(name); err != nil {
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

	unit = strings.TrimSuffix(unit, ".service")
	logPath := serviceLogPath(unit)

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

func (sf serviceFile) get(section, key string) string {
	for _, kv := range sf.sections[section] {
		if kv.key == key {
			return kv.value
		}
	}
	return ""
}

func (sf serviceFile) getAll(section, key string) []string {
	var vals []string
	for _, kv := range sf.sections[section] {
		if kv.key == key {
			vals = append(vals, kv.value)
		}
	}
	return vals
}
