//go:build linux

package main

import (
	"fmt"
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
// Implements enough of systemctl for Debian/Ubuntu package postinst
// scripts: start, stop, restart, enable, disable, is-active, is-enabled,
// status, daemon-reload, is-system-running, mask, unmask, show, cat,
// list-units.

var serviceDirs = []string{
	"/etc/systemd/system",
	"/usr/lib/systemd/system",
	"/lib/systemd/system",
}

const pidDir = "/run/bhatti/services"

// runSystemctl is the entry point when invoked as /usr/bin/systemctl.
func runSystemctl(args []string) {
	// Strip common flags that package scripts pass.
	var command string
	var units []string
	var showProp string
	noLegend := false
	stateFilter := ""
	typeFilter := ""

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--no-reload" || arg == "--no-block" || arg == "--quiet" ||
			arg == "-q" || arg == "--system" || arg == "--no-pager" ||
			arg == "--no-ask-password" || arg == "--now":
			continue
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
		case strings.HasPrefix(arg, "-"):
			continue // skip unknown flags
		case command == "":
			command = arg
		default:
			units = append(units, arg)
		}
	}

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
			svcStop(name) // best-effort
			if err := svcStart(name); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to restart %s: %v\n", u, err)
				os.Exit(1)
			}
		}
	case "enable":
		for _, u := range units {
			if err := svcEnable(normalizeName(u)); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to enable %s: %v\n", u, err)
				os.Exit(1)
			}
		}
	case "disable":
		for _, u := range units {
			if err := svcDisable(normalizeName(u)); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to disable %s: %v\n", u, err)
				os.Exit(1)
			}
		}
	case "is-active":
		if len(units) == 0 {
			os.Exit(1)
		}
		if svcIsActive(normalizeName(units[0])) {
			fmt.Println("active")
		} else {
			fmt.Println("inactive")
			os.Exit(3) // systemctl convention: exit 3 = inactive
		}
	case "is-enabled":
		if len(units) == 0 {
			os.Exit(1)
		}
		if svcIsEnabled(normalizeName(units[0])) {
			fmt.Println("enabled")
		} else {
			fmt.Println("disabled")
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
		svcShow(normalizeName(units[0]), showProp)
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
	case "daemon-reload", "daemon-reexec", "reset-failed":
		// no-op
	case "preset":
		// deb-systemd-helper calls 'systemctl preset <unit>' after install.
		// Preset reads preset policy files to decide enable/disable.
		// We default to enable (most packages want their service enabled).
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

// normalizeName ensures the name has a .service suffix.
func normalizeName(name string) string {
	name = strings.TrimSuffix(name, ".service")
	return name
}

// findServiceFile locates the .service file for a unit.
// Also resolves aliases (e.g. sshd -> ssh via Alias=sshd.service).
func findServiceFile(name string) string {
	// Direct match first.
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
	// Check aliases — scan service files for Alias=<name>.service.
	for _, dir := range serviceDirs {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".service") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			svc := parseServiceFile(path)
			for _, alias := range svc.getAll("Install", "Alias") {
				alias = strings.TrimSuffix(alias, ".service")
				if alias == name {
					return path
				}
			}
		}
	}
	return ""
}

// pidFile returns the path to a service's PID file.
func pidFile(name string) string {
	return filepath.Join(pidDir, name+".pid")
}

// readPID reads the PID from a service's PID file.
func readPID(name string) (int, error) {
	data, err := os.ReadFile(pidFile(name))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

// processAlive checks if a PID is alive by reading /proc/<pid>/stat.
// This works regardless of permissions (unlike kill -0 which requires
// same UID or root).
func processAlive(pid int) bool {
	_, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	return err == nil
}

// --- Service operations ---

func svcStart(name string) error {
	// Already running?
	if pid, err := readPID(name); err == nil && processAlive(pid) {
		return nil
	}

	path := findServiceFile(name)
	if path == "" {
		return fmt.Errorf("unit %s.service not found or masked", name)
	}

	svc := parseServiceFile(path)

	// Create RuntimeDirectory with optional mode.
	if rd := svc.get("Service", "RuntimeDirectory"); rd != "" {
		dir := "/run/" + rd
		mode := os.FileMode(0755)
		if m := svc.get("Service", "RuntimeDirectoryMode"); m != "" {
			if parsed, err := strconv.ParseUint(m, 8, 32); err == nil {
				mode = os.FileMode(parsed)
			}
		}
		os.MkdirAll(dir, mode)
		os.Chmod(dir, mode) // MkdirAll doesn't set mode on existing dirs
		if u := svc.get("Service", "User"); u != "" {
			exec.Command("chown", u, dir).Run()
		}
	}

	// ExecStartPre — leading '-' means ignore failure.
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

	// ExecStart
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
	case "simple", "exec":
		return startDaemon(name, execStart, svc)
	case "forking":
		return startForking(name, execStart, svc)
	default:
		// Try simple as fallback
		return startDaemon(name, execStart, svc)
	}
}

func startDaemon(name, execStart string, svc serviceFile) error {
	execStart = strings.TrimLeft(execStart, "-!+:@") // strip systemd exec prefixes
	if execStart == "" {
		return fmt.Errorf("empty ExecStart")
	}

	// Run through shell to handle $VAR expansion (e.g. $SSHD_OPTS).
	cmd := exec.Command("/bin/sh", "-c", "exec "+execStart)
	cmd.Dir = svc.get("Service", "WorkingDirectory")
	cmd.Env = buildServiceEnv(svc)
	// Setsid: create a new session so the daemon doesn't get SIGHUP
	// when this (short-lived) systemctl shim process exits.
	// Setpgid: new process group for clean signal handling.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// Detach stdio so the daemon doesn't get broken pipes when
	// the shim process exits.
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", execStart, err)
	}

	os.MkdirAll(pidDir, 0755)
	os.WriteFile(pidFile(name), []byte(strconv.Itoa(cmd.Process.Pid)), 0644)

	// Release the process so it's not tied to this (short-lived) shim.
	// PID 1 (lohar) will reap it via the zombie reaper.
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

	// For forking services, check PIDFile from the .service
	if pf := svc.get("Service", "PIDFile"); pf != "" {
		if data, err := os.ReadFile(pf); err == nil {
			// Copy PID to our tracking directory
			os.WriteFile(pidFile(name), data, 0644)
		}
	}

	return nil
}

func svcStop(name string) error {
	pid, err := readPID(name)
	if err != nil {
		return nil // not running
	}
	if !processAlive(pid) {
		os.Remove(pidFile(name))
		return nil
	}

	// Try ExecStop first if defined
	path := findServiceFile(name)
	if path != "" {
		svc := parseServiceFile(path)
		if execStop := svc.get("Service", "ExecStop"); execStop != "" {
			runServiceCommand(execStop, svc)
			// Give it a moment
			time.Sleep(500 * time.Millisecond)
			if !processAlive(pid) {
				os.Remove(pidFile(name))
				return nil
			}
		}
	}

	// SIGTERM → wait → SIGKILL
	syscall.Kill(-pid, syscall.SIGTERM) // negative PID = process group
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
	os.Remove(link) // remove stale
	if err := os.Symlink(path, link); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Created symlink %s → %s.\n", link, path)
	return nil
}

func svcDisable(name string) error {
	// Remove from all target wants directories
	pattern := "/etc/systemd/system/*.wants/" + name + ".service"
	matches, _ := filepath.Glob(pattern)
	for _, m := range matches {
		os.Remove(m)
		fmt.Fprintf(os.Stderr, "Removed %s.\n", m)
	}
	return nil
}

func svcIsActive(name string) bool {
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
	path := findServiceFile(name)
	desc := name
	if path != "" {
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
}

func svcShow(name string, prop string) {
	path := findServiceFile(name)
	if path == "" {
		return
	}
	svc := parseServiceFile(path)
	if prop != "" {
		// Return specific property
		for _, section := range []string{"Unit", "Service", "Install"} {
			if v := svc.get(section, prop); v != "" {
				fmt.Printf("%s=%s\n", prop, v)
				return
			}
		}
		fmt.Printf("%s=\n", prop)
		return
	}
	// Dump all properties
	for _, section := range []string{"Unit", "Service", "Install"} {
		if m, ok := svc.sections[section]; ok {
			for _, kv := range m {
				fmt.Printf("%s=%s\n", kv.key, kv.value)
			}
		}
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
		return // only services
	}
	if !noLegend {
		fmt.Printf("%-40s %-10s %-10s %s\n", "UNIT", "LOAD", "ACTIVE", "DESCRIPTION")
	}
	seen := map[string]bool{}
	for _, dir := range serviceDirs {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			name := strings.TrimSuffix(e.Name(), ".service")
			if !strings.HasSuffix(e.Name(), ".service") || seen[name] {
				continue
			}
			seen[name] = true
			active := "inactive"
			if svcIsActive(name) {
				active = "active"
			}
			if stateFilter != "" {
				if stateFilter == "running" && active != "active" {
					continue
				}
				if stateFilter == "failed" {
					continue // we don't track failures
				}
			}
			path := findServiceFile(name)
			desc := name
			if path != "" {
				svc := parseServiceFile(path)
				if d := svc.get("Unit", "Description"); d != "" {
					desc = d
				}
			}
			fmt.Printf("%-40s %-10s %-10s %s\n",
				name+".service", "loaded", active, desc)
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
			// Check masked
			link, err := os.Readlink(filepath.Join(dir, e.Name()))
			if err == nil && link == "/dev/null" {
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

// --- Helpers ---

func runServiceCommand(cmdLine string, svc serviceFile) error {
	cmdLine = strings.TrimLeft(cmdLine, "-!+:@")
	if cmdLine == "" {
		return nil
	}
	// Run through shell for $VAR expansion.
	cmd := exec.Command("/bin/sh", "-c", cmdLine)
	cmd.Env = buildServiceEnv(svc)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func buildServiceEnv(svc serviceFile) []string {
	env := os.Environ()
	// Add Environment= lines
	for _, e := range svc.getAll("Service", "Environment") {
		e = strings.Trim(e, "\"'")
		env = append(env, e)
	}
	// Add EnvironmentFile= lines
	for _, f := range svc.getAll("Service", "EnvironmentFile") {
		f = strings.TrimLeft(f, "-") // leading - means ignore if missing
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

// --- Minimal .service file parser ---

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
