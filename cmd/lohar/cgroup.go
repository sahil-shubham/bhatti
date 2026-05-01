//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// systemSlice is the parent cgroup under which all unit cgroups live,
// matching systemd's convention. Created lazily on first unit start.
const systemSlice = "system.slice"

// The cgroup hierarchy root used to be a package-level var (cgroupRoot).
// It moved to Config.CgroupRoot during the audit-pass refactor that put
// all paths on Registry.Config to make them immutable post-construction
// and remove a -race finding around watcher goroutines. Production
// reads /sys/fs/cgroup from ProductionConfig(); tests construct a
// Registry with a tempdir CgroupRoot.

// CgroupPath returns the full filesystem path of this unit's cgroup,
// e.g. /sys/fs/cgroup/system.slice/ssh.service. The directory may or
// may not exist \u2014 CreateCgroup() materialises it.
//
// For instance units (postgresql@16-main.service), each instance gets
// its own cgroup. They don't share state with the template's cgroup.
func (u *Unit) CgroupPath() string {
	return filepath.Join(u.reg.Config.CgroupRoot, systemSlice, u.FullName())
}

// CreateCgroup creates the unit's cgroup directory under system.slice,
// then writes the resource limits declared in the unit's [Service]
// section into the control files. Idempotent.
//
// Writes are best-effort: a kernel without one of the controllers
// (e.g. some VPS guests don't have +memory) won't have the corresponding
// file, and Open will fail. We log and continue \u2014 the goal is "do what's
// possible" matching systemd's behaviour rather than refusing to start
// because a resource controller is missing.
func (u *Unit) CreateCgroup() error {
	// Ensure the parent slice exists. Idempotent mkdir.
	sliceDir := filepath.Join(u.reg.Config.CgroupRoot, systemSlice)
	if err := os.MkdirAll(sliceDir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", sliceDir, err)
	}

	cg := u.CgroupPath()
	if err := os.MkdirAll(cg, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", cg, err)
	}

	// Apply resource limits from the unit file. systemd's directives map
	// to cgroup v2 control files like this:
	//
	//   MemoryMax=512M       -> memory.max=536870912
	//   MemoryHigh=256M      -> memory.high=268435456
	//   MemoryMin=128M       -> memory.min=134217728
	//   TasksMax=512         -> pids.max=512
	//   CPUQuota=50%         -> cpu.max="50000 100000"
	//   IOWeight=100         -> io.weight=100
	//
	// Empty / unset directives are skipped \u2014 cgroup defaults are usually
	// "no limit" which is what an unconfigured unit wants anyway.
	svc := u.Sections
	writes := map[string]string{}

	if v := svc.get("Service", "MemoryMax"); v != "" {
		writes["memory.max"] = parseMemoryValue(v)
	}
	if v := svc.get("Service", "MemoryHigh"); v != "" {
		writes["memory.high"] = parseMemoryValue(v)
	}
	if v := svc.get("Service", "MemoryMin"); v != "" {
		writes["memory.min"] = parseMemoryValue(v)
	}
	if v := svc.get("Service", "MemoryLow"); v != "" {
		writes["memory.low"] = parseMemoryValue(v)
	}
	if v := svc.get("Service", "TasksMax"); v != "" {
		writes["pids.max"] = parseInfinityOrInt(v)
	}
	if v := svc.get("Service", "CPUQuota"); v != "" {
		writes["cpu.max"] = parseCPUQuota(v)
	}
	if v := svc.get("Service", "CPUWeight"); v != "" {
		writes["cpu.weight"] = strings.TrimSpace(v)
	}
	if v := svc.get("Service", "IOWeight"); v != "" {
		writes["io.weight"] = "default " + strings.TrimSpace(v)
	}

	for file, val := range writes {
		path := filepath.Join(cg, file)
		if err := os.WriteFile(path, []byte(val), 0644); err != nil {
			// A missing controller file isn't fatal \u2014 just log and continue.
			// Common case: kernel without +memory in the parent's
			// subtree_control (some restricted environments).
			fmt.Fprintf(os.Stderr, "lohar: cgroup write %s = %q: %v\n",
				path, val, err)
		}
	}
	return nil
}

// PlaceInCgroup moves pid into the unit's cgroup by writing the PID to
// the cgroup's cgroup.procs file. The kernel atomically migrates the
// process; subsequent forks of that process inherit the cgroup.
//
// There's a small race between fork+exec and this write \u2014 the daemon
// runs in the parent's cgroup for ~milliseconds before being moved.
// systemd avoids the race using clone3(CLONE_INTO_CGROUP) but that's not
// reachable from Go's exec without a custom syscall path; for our use
// case the race is benign (the daemon is doing exec setup, not consuming
// resources yet).
func (u *Unit) PlaceInCgroup(pid int) error {
	procs := filepath.Join(u.CgroupPath(), "cgroup.procs")
	return os.WriteFile(procs, []byte(strconv.Itoa(pid)), 0644)
}

// KillCgroup writes "1" to cgroup.kill, atomically delivering SIGKILL to
// every process in the cgroup. This is the kernel-supplied primitive that
// makes KillMode=control-group reliable: no race window where a forked
// child escapes, no process group manipulation, no missed grandchildren.
//
// Available on Linux >= 5.14. On older kernels the file doesn't exist
// and the write fails with ENOENT; svcStop falls back to the PGID-kill
// path in that case (caller's responsibility \u2014 we just return the error).
func (u *Unit) KillCgroup() error {
	path := filepath.Join(u.CgroupPath(), "cgroup.kill")
	return os.WriteFile(path, []byte("1"), 0644)
}

// RemoveCgroup rmdir's the cgroup directory. Caller must ensure no
// processes remain (CgroupHasProcs() == false) or the rmdir will fail.
// Failure is non-fatal \u2014 a leftover empty cgroup costs nothing and the
// next start will reuse it.
func (u *Unit) RemoveCgroup() error {
	return os.Remove(u.CgroupPath())
}

// CgroupHasProcs returns true if the cgroup.procs file is non-empty,
// i.e. at least one process is currently a member of the cgroup.
//
// Used by svcStop after KillCgroup to wait for the cgroup to drain
// before attempting RemoveCgroup. The kernel updates cgroup.procs
// asynchronously as processes exit, so a brief polling loop is
// expected.
func (u *Unit) CgroupHasProcs() bool {
	data, err := os.ReadFile(filepath.Join(u.CgroupPath(), "cgroup.procs"))
	if err != nil {
		return false
	}
	return len(strings.TrimSpace(string(data))) > 0
}

// WaitCgroupDrain polls cgroup.procs every 50ms until it's empty or the
// timeout expires. Returns whether drain succeeded. svcStop calls this
// after KillCgroup; under normal conditions the SIGKILL takes effect in
// well under a second.
func (u *Unit) WaitCgroupDrain(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !u.CgroupHasProcs() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return !u.CgroupHasProcs()
}

// --- Resource value parsers ---

// parseMemoryValue translates a systemd memory directive value into the
// integer-bytes form cgroup v2 expects.
//
//	"infinity" / "max" -> "max"        (cgroup v2 sentinel)
//	"512M" / "512MB"   -> "536870912"
//	"1G"               -> "1073741824"
//	"1024"             -> "1024"       (bare bytes)
//	"50%"              -> percentage of host RAM (computed)
//
// Unparseable input returns "max" (no limit) rather than failing the
// service start \u2014 matches systemd's permissive parsing of unit files.
func parseMemoryValue(v string) string {
	v = strings.TrimSpace(v)
	switch strings.ToLower(v) {
	case "infinity", "max", "":
		return "max"
	}
	// Trim a trailing 'B' (systemd accepts "512MB" and "512M" identically).
	if strings.HasSuffix(strings.ToUpper(v), "B") && len(v) > 1 {
		ch := v[len(v)-2]
		if ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' {
			v = v[:len(v)-1]
		}
	}
	mult := uint64(1)
	switch strings.ToUpper(v[len(v)-1:]) {
	case "K":
		mult = 1024
	case "M":
		mult = 1024 * 1024
	case "G":
		mult = 1024 * 1024 * 1024
	case "T":
		mult = 1024 * 1024 * 1024 * 1024
	}
	if mult != 1 {
		v = v[:len(v)-1]
	}
	n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return "max"
	}
	return strconv.FormatUint(n*mult, 10)
}

// parseInfinityOrInt handles the directives that take either an integer
// or systemd's "infinity" sentinel: TasksMax, etc. cgroup v2 uses "max".
func parseInfinityOrInt(v string) string {
	v = strings.TrimSpace(v)
	switch strings.ToLower(v) {
	case "infinity", "max", "":
		return "max"
	}
	if _, err := strconv.ParseUint(v, 10, 64); err != nil {
		return "max"
	}
	return v
}

// parseCPUQuota translates CPUQuota=50% to cgroup v2's cpu.max format
// "<quota_us> <period_us>". The period is fixed at 100ms (systemd's
// default CPUQuotaPeriodSec); the quota is computed from the percentage.
//
//	"50%"  -> "50000 100000"     (half a CPU)
//	"100%" -> "100000 100000"    (one whole CPU)
//	"200%" -> "200000 100000"    (two CPUs)
//	""     -> ""                 (caller skips the write)
func parseCPUQuota(v string) string {
	v = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "%"))
	pct, err := strconv.ParseFloat(v, 64)
	if err != nil || pct <= 0 {
		return "max 100000"
	}
	const period = 100000 // microseconds
	quota := int64(pct * float64(period) / 100.0)
	return fmt.Sprintf("%d %d", quota, period)
}

// killModeFor returns the effective KillMode= for the unit. systemd's
// default is "control-group"; an explicit value in the unit file wins.
// Recognised values: "control-group", "process", "mixed", "none".
// Unrecognised values fall back to control-group, matching systemd's
// permissive parser.
func killModeFor(u *Unit) string {
	v := strings.TrimSpace(u.Sections.get("Service", "KillMode"))
	switch v {
	case "control-group", "process", "mixed", "none":
		return v
	}
	return "control-group"
}

// CgroupMemoryCurrent returns memory.current (bytes used) or 0 if the
// cgroup doesn't exist or the controller isn't enabled. Used by status
// output — systemd shows "Memory: 124M" in `systemctl status` and we
// now have the raw value to do the same.
func (u *Unit) CgroupMemoryCurrent() uint64 {
	return readCgroupUint(u.CgroupPath(), "memory.current")
}

// CgroupTasksCurrent returns pids.current (active tasks in the cgroup),
// the count systemd shows as "Tasks: 31" in status output.
func (u *Unit) CgroupTasksCurrent() uint64 {
	return readCgroupUint(u.CgroupPath(), "pids.current")
}

func readCgroupUint(cg, file string) uint64 {
	data, err := os.ReadFile(filepath.Join(cg, file))
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	return n
}
