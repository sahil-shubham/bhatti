//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Config holds the filesystem locations the shim reads and writes. It's
// constructed once — by runAgent for PID-1 lohar, by runSystemctl for
// short-lived client invocations, by tests for their sandboxed view —
// and never mutated thereafter. That immutability is the structural
// reason watcher goroutines can read paths concurrently with everything
// else without races: there's no shared mutable state to race on.
//
// Before this struct existed, the same values lived as package-level
// vars that tests rewrote. -race flagged the resulting concurrency
// (test cleanup writing pidDir while a watcher goroutine read it).
// Moving paths onto Config closes that whole class of bug.
type Config struct {
	// ServiceDirs is the search path for unit fragment files,
	// highest-priority first. Production: /etc/systemd/system,
	// /usr/lib/systemd/system, /lib/systemd/system.
	ServiceDirs []string

	// EtcSystemdDir is where enable/disable creates wants/ symlinks and
	// where mask creates the /dev/null symlink. Production:
	// /etc/systemd/system.
	EtcSystemdDir string

	// PidDir holds runtime pidfiles + .failed markers. Production:
	// /run/bhatti/services.
	PidDir string

	// LogDir holds per-unit log files. Production: /var/log/bhatti.
	LogDir string

	// CgroupRoot is the cgroup v2 hierarchy root. Production:
	// /sys/fs/cgroup. Unit cgroups live under <CgroupRoot>/system.slice/.
	CgroupRoot string

	// DropInDirs is the search path for <unit>.service.d/*.conf
	// directories, in LOWEST-priority-first order so later loads
	// override earlier ones (matches systemd's unit_file_find_dropin_paths
	// in src/core/load-dropin.c).
	DropInDirs []string

	// SyslogSocketPath is the unix datagram socket the syslog receiver
	// binds. Production: /dev/log (libc's syslog(3) writes here). Tests
	// point at a tempdir socket to exercise the receiver without
	// clobbering the host's real /dev/log.
	SyslogSocketPath string

	// NotifySocketPath is the unix datagram socket the sd_notify
	// receiver binds and that we expose to daemons via the
	// NOTIFY_SOCKET env var when their unit declares Type=notify.
	// Production: /run/systemd/notify (systemd's convention). Tests
	// point at a tempdir socket.
	NotifySocketPath string
}

// ProductionConfig returns the Config that PID-1 lohar uses inside a
// real bhatti VM. The same values were package-level globals before
// the Config refactor; preserving the literal paths here matches
// existing on-disk state (pidfiles in /run/bhatti/services, etc.).
func ProductionConfig() Config {
	return Config{
		ServiceDirs: []string{
			"/etc/systemd/system",
			"/usr/lib/systemd/system",
			"/lib/systemd/system",
		},
		EtcSystemdDir: "/etc/systemd/system",
		PidDir:        "/run/bhatti/services",
		LogDir:        "/var/log/bhatti",
		CgroupRoot:    "/sys/fs/cgroup",
		DropInDirs: []string{
			"/usr/lib/systemd/system",
			"/lib/systemd/system",
			"/run/systemd/system",
			"/etc/systemd/system",
		},
		SyslogSocketPath: "/dev/log",
		NotifySocketPath: "/run/systemd/notify",
	}
}

// Unit is the resolved identity of a systemd-style unit.
//
// Two queries that resolve to the same physical fragment file produce
// pointer-identical *Unit values via the Registry. State (pidfile path,
// log path) is keyed by Unit.Canonical, never by the user-supplied
// query string — so `systemctl status sshd` and `systemctl status ssh`
// observe the same state when ssh.service has Alias=sshd.service.
//
// This mirrors systemd's data model in src/core/unit.h: every unit has
// one canonical id and a set of aliases, with the manager hashmap
// pre-populating every name as a key pointing at the same Unit*.
type Unit struct {
	// reg is the back-pointer to the Registry that owns this Unit. The
	// Registry holds the Config (paths) and watcher coordination state
	// (stopReqs, restartBurst, watcherWG). Methods that compute paths or
	// touch coordination state walk through reg; that's how Unit avoids
	// reading any package-level mutable global.
	reg *Registry

	// Canonical is the primary unit name without suffix.
	// For instance units, this includes the instance: "postgresql@16-main".
	Canonical string

	// Suffix is ".service", ".socket", ".target", or ".timer".
	Suffix string

	// Aliases is every other name this unit answers to, without suffix.
	// Includes both [Install] Alias= entries and inode-equivalent symlinks.
	Aliases map[string]struct{}

	// Path is the resolved real path of the fragment file (symlinks
	// followed). Empty for masked or not-found units.
	Path string

	// Instance is the part after @ for template-instance units; empty otherwise.
	// e.g. for "postgresql@16-main", Instance == "16-main".
	Instance string

	// Template is the template basename (without @ or suffix) for instance
	// units, e.g. "postgresql" for "postgresql@16-main"; empty otherwise.
	Template string

	// Sections is the parsed unit file with template specifiers expanded.
	// Drop-in directories are merged here in C2.
	Sections serviceFile

	// Masked is true if the unit is symlinked to /dev/null in any service dir.
	Masked bool
}

// FullName returns "<canonical><suffix>", e.g. "ssh.service".
func (u *Unit) FullName() string {
	return u.Canonical + u.Suffix
}

// PidPath returns <PidDir>/<canonical>.pid. All state-keyed paths use
// the canonical name regardless of which alias the caller queried by —
// that's the whole point of Unit identity. Reads u.reg.Config.PidDir;
// no package-level globals.
func (u *Unit) PidPath() string {
	return filepath.Join(u.reg.Config.PidDir, u.Canonical+".pid")
}

// LogPath returns <LogDir>/<canonical>.log.
func (u *Unit) LogPath() string {
	return filepath.Join(u.reg.Config.LogDir, u.Canonical+".log")
}

// WantsLink returns the path of the symlink in <target>.wants/ that
// would be created by `systemctl enable`, e.g.
// /etc/systemd/system/multi-user.target.wants/ssh.service.
func (u *Unit) WantsLink(target string) string {
	return filepath.Join(u.reg.Config.EtcSystemdDir, target+".wants", u.FullName())
}

// FailedMarkerPath returns <PidDir>/<canonical>.failed.
//
// The presence of this file means the unit's last run terminated with a
// non-zero exit code; the file's contents are the integer exit code
// (decimal). svcStart removes it; svcStop removes it (clean stop is not
// failure); the watcher writes it on crash. systemctl is-failed reads it.
//
// Lives on disk because failed-state needs to survive across systemctl
// client invocations — each one creates a fresh Registry, so in-memory
// state on a *Unit is only visible to the watcher that wrote it.
func (u *Unit) FailedMarkerPath() string {
	return filepath.Join(u.reg.Config.PidDir, u.Canonical+".failed")
}

// MarkFailed writes the failed marker with the given exit code.
func (u *Unit) MarkFailed(exitCode int) {
	os.MkdirAll(u.reg.Config.PidDir, 0755)
	os.WriteFile(u.FailedMarkerPath(), []byte(fmt.Sprintf("%d", exitCode)), 0644)
}

// ClearFailed removes the failed marker. Idempotent.
func (u *Unit) ClearFailed() {
	os.Remove(u.FailedMarkerPath())
}

// IsFailed returns true if the failed marker exists. Used by svcIsFailed.
func (u *Unit) IsFailed() bool {
	_, err := os.Stat(u.FailedMarkerPath())
	return err == nil
}

// LastExitCode returns the exit code stored in the failed marker, or 0
// if no marker exists or the file is malformed.
func (u *Unit) LastExitCode() int {
	data, err := os.ReadFile(u.FailedMarkerPath())
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return n
}

// ActivatingMarkerPath returns <PidDir>/<canonical>.activating.
//
// Created by svcStart for Type=notify units before the daemon spawns.
// The notify receiver removes it on READY=1. Existence of the marker
// means the daemon has been spawned but hasn't yet declared itself
// ready -- the systemd ActiveState=activating equivalent.
//
// Lives on disk for the same reason the failed marker does: a fresh
// systemctl client process needs to read the state without having
// participated in the spawning. Type=simple/forking/oneshot units
// never have an activating marker -- they're "active" the moment
// fork+exec succeeds.
func (u *Unit) ActivatingMarkerPath() string {
	return filepath.Join(u.reg.Config.PidDir, u.Canonical+".activating")
}

// MarkActivating writes an empty marker file. The contents don't matter;
// the existence does.
func (u *Unit) MarkActivating() error {
	os.MkdirAll(u.reg.Config.PidDir, 0755)
	return os.WriteFile(u.ActivatingMarkerPath(), []byte{}, 0644)
}

// ClearActivating removes the marker. Idempotent (no error if absent).
func (u *Unit) ClearActivating() {
	os.Remove(u.ActivatingMarkerPath())
}

// IsActivating returns true if the .activating marker exists, i.e. the
// unit's daemon is running but hasn't sent READY=1 via sd_notify yet.
func (u *Unit) IsActivating() bool {
	_, err := os.Stat(u.ActivatingMarkerPath())
	return err == nil
}

// ReadPID returns the running PID for this unit, or an error if no
// pidfile exists. The pidfile is keyed by canonical name, so calling
// ReadPID through any alias returns the same PID.
func (u *Unit) ReadPID() (int, error) {
	data, err := os.ReadFile(u.PidPath())
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

// WritePID stores the PID for this unit in the canonical pidfile.
func (u *Unit) WritePID(pid int) error {
	os.MkdirAll(u.reg.Config.PidDir, 0755)
	return os.WriteFile(u.PidPath(), []byte(strconv.Itoa(pid)), 0644)
}

// RemovePID deletes the pidfile if present. Used after stop / on stale
// pidfile detection.
func (u *Unit) RemovePID() {
	os.Remove(u.PidPath())
}

// IsRunning returns true if the canonical pidfile points at a live process.
func (u *Unit) IsRunning() bool {
	pid, err := u.ReadPID()
	if err != nil {
		return false
	}
	return processAlive(pid)
}

// HasName returns true if name (with or without suffix) refers to this unit.
func (u *Unit) HasName(name string) bool {
	name = strings.TrimSuffix(name, u.Suffix)
	if name == u.Canonical {
		return true
	}
	_, ok := u.Aliases[name]
	return ok
}

// Registry is the n:1 map from any unit name (canonical, alias, symlinked
// alternate filename, template instance) to a *Unit. Lookups by any name
// the unit answers to return the same pointer. Holds the Config (paths,
// immutable after construction) and watcher coordination state
// (stopReqs, restartBurst, watcherWG) that used to live as package-level
// globals.
//
// A Registry is created per process invocation; for the systemctl shim
// it lives for the duration of one command, for PID-1 lohar it's
// long-lived and shared with the syslog receiver and journalctl
// invocations.
type Registry struct {
	Config Config // immutable after NewRegistry returns

	mu      sync.Mutex
	byKey   map[string]*Unit // every name (incl. suffix-stripped aliases) -> Unit
	byInode map[uint64]*Unit // dedupes inode-equivalent symlinks to one Unit
	// notFound caches names we already tried to resolve and couldn't find.
	// Without this, the syslog receiver would re-scan every unit file for
	// every kernel/cron/login message tagged with something that doesn't
	// match a unit.
	notFound map[string]struct{}

	// --- Watcher coordination state (was package-level pre-refactor) ---

	// coordMu protects stopReqs and restartBurst. Held briefly during
	// flag set/clear and burst-history append; never held while waiting.
	coordMu sync.Mutex

	// stopReqs marks units that an admin asked to stop, so the watcher
	// suppresses the auto-restart that Restart=on-failure would
	// otherwise trigger. Indexed by canonical name.
	stopReqs map[string]bool

	// restartBurst is the per-unit history of recent restart attempts,
	// used to enforce StartLimitBurst / StartLimitIntervalSec.
	restartBurst map[string][]time.Time

	// watcherWG tracks live watcher goroutines spawned by startDaemon.
	// Tests Wait on it before letting their cleanup run; production
	// never calls Wait because PID 1 lives forever.
	watcherWG sync.WaitGroup
}

// NewRegistry returns a fresh registry bound to the given Config. The
// Config is captured by value and never mutated by the Registry; that's
// the structural guarantee that makes Unit methods reading u.reg.Config.X
// safe to call from any goroutine without synchronisation.
func NewRegistry(cfg Config) *Registry {
	return &Registry{
		Config:       cfg,
		byKey:        make(map[string]*Unit),
		byInode:      make(map[uint64]*Unit),
		notFound:     make(map[string]struct{}),
		stopReqs:     make(map[string]bool),
		restartBurst: make(map[string][]time.Time),
	}
}

// markStopRequested, clearStopRequested, isStopRequested are called by
// svcStop and the watcher goroutine. They share state via coordMu.
func (r *Registry) markStopRequested(canonical string) {
	r.coordMu.Lock()
	r.stopReqs[canonical] = true
	r.coordMu.Unlock()
}
func (r *Registry) clearStopRequested(canonical string) {
	r.coordMu.Lock()
	delete(r.stopReqs, canonical)
	r.coordMu.Unlock()
}
func (r *Registry) isStopRequested(canonical string) bool {
	r.coordMu.Lock()
	defer r.coordMu.Unlock()
	return r.stopReqs[canonical]
}

// WaitForWatchers blocks until every watcher goroutine spawned via this
// Registry's startDaemon has returned. Test-only helper. Production
// code never calls this because PID-1 lohar lives forever.
func (r *Registry) WaitForWatchers() { r.watcherWG.Wait() }

// InvalidateNotFound clears the negative-resolution cache. Called by
// the systemctl shim's daemon-reload handler so a unit file written
// after a previous "is-active"/"status"/etc. lookup (which cached the
// miss) becomes visible to subsequent start/restart commands. Without
// this, installers that probe-then-write-then-start (k3s, docker, lots
// of distro packages) hit "Unit not found" on the start because the
// probe cached a miss that the write didn't invalidate.
func (r *Registry) InvalidateNotFound() {
	r.mu.Lock()
	r.notFound = make(map[string]struct{})
	r.mu.Unlock()
}

// globalRegistry is the long-lived Registry used by PID-1 lohar's syslog
// receiver, target-wants service activation, and IPC handler. Created in
// runAgent at boot via NewRegistry(ProductionConfig()).
//
// Short-lived systemctl client invocations construct their own local
// Registry with the same ProductionConfig() — see runSystemctl.
var globalRegistry *Registry

// ErrUnitNotFound is returned by Resolve when no unit file exists for the
// queried name and the name doesn't appear as an [Install] Alias= elsewhere.
// Masked units (symlinks to /dev/null) resolve successfully with Masked=true,
// because callers like svcShow need to report LoadState=masked rather than
// not-found.
var ErrUnitNotFound = errors.New("unit not found")

// Resolve returns the Unit identified by name. Every alias the unit answers
// to (canonical name, [Install] Alias= entries, symlinked alternate paths,
// template instance form) returns the same *Unit pointer.
//
// The resolution algorithm mirrors systemd's unit_file_build_name_map:
//  1. Memoise — if name was looked up before, return the cached Unit.
//  2. Mask check — if any service dir has <name><suffix> -> /dev/null,
//     return a Unit with Masked=true.
//  3. Template expansion — names with @ are resolved against <prefix>@<suffix>.
//  4. Direct file match — find <name><suffix> in serviceDirs, follow symlinks
//     to the real path, dedupe by inode against existing units.
//  5. [Install] Alias= scan — if no direct match, walk all unit files looking
//     for one that declares this name as an alias.
//  6. Register every name (canonical + every alias) in byKey.
func (r *Registry) Resolve(name string) (*Unit, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	base, suffix := splitSuffix(name)
	cacheKey := base + suffix
	if u, ok := r.byKey[cacheKey]; ok {
		return u, nil
	}
	if _, miss := r.notFound[cacheKey]; miss {
		return nil, fmt.Errorf("%w: %s", ErrUnitNotFound, cacheKey)
	}

	// Mask check: any service dir with <base><suffix> -> /dev/null.
	if r.checkMasked(base, suffix) {
		u := &Unit{
			reg:       r,
			Canonical: base,
			Suffix:    suffix,
			Aliases:   map[string]struct{}{},
			Masked:    true,
		}
		r.byKey[base+suffix] = u
		return u, nil
	}

	// Template instance: postgresql@16-main -> postgresql@.service template.
	if strings.Contains(base, "@") && !strings.HasSuffix(base, "@") {
		if u, err := r.resolveTemplateInstance(base, suffix); err == nil {
			return u, nil
		}
	}

	// Direct filesystem match.
	if u, err := r.resolveDirect(base, suffix); err == nil {
		return u, nil
	}

	// [Install] Alias= scan: maybe some other unit declares this name.
	if u, err := r.resolveByAliasScan(base, suffix); err == nil {
		return u, nil
	}

	// Cache the miss — keeps high-frequency unknown tags from the syslog
	// receiver (kernel, cron, login) from re-scanning every fragment file.
	r.notFound[cacheKey] = struct{}{}
	return nil, fmt.Errorf("%w: %s", ErrUnitNotFound, cacheKey)
}

// checkMasked returns true if base+suffix is a symlink to /dev/null in any
// service dir. This must precede direct-match because os.Stat would
// follow the symlink and fail.
func (r *Registry) checkMasked(base, suffix string) bool {
	for _, dir := range r.Config.ServiceDirs {
		path := filepath.Join(dir, base+suffix)
		if target, err := os.Readlink(path); err == nil && target == "/dev/null" {
			return true
		}
	}
	return false
}

// resolveDirect handles the common case: a file or symlink at <dir>/<base><suffix>.
// Symlinks are resolved to a real path, and the inode is checked against
// already-loaded units so that two names linking to the same fragment
// share one *Unit.
func (r *Registry) resolveDirect(base, suffix string) (*Unit, error) {
	for _, dir := range r.Config.ServiceDirs {
		path := filepath.Join(dir, base+suffix)
		st, err := os.Stat(path)
		if err != nil {
			continue
		}
		realPath, err := filepath.EvalSymlinks(path)
		if err != nil {
			realPath = path
		}

		// Inode dedup: if we've already loaded a Unit for this physical file
		// under another name, reuse it and add this name as an alias.
		inode := statInode(st)
		if inode != 0 {
			if existing, ok := r.byInode[inode]; ok {
				existing.Aliases[base] = struct{}{}
				r.byKey[base+suffix] = existing
				// Late-arriving alias: load drop-ins under the new name
				// so e.g. /etc/systemd/system/sshd.service.d/*.conf applies
				// even if Resolve("ssh") happened first.
				r.loadDropIns(existing, base, suffix)
				return existing, nil
			}
		}

		u := r.buildUnit(base, suffix, realPath)
		if inode != 0 {
			r.byInode[inode] = u
		}
		return u, nil
	}
	return nil, ErrUnitNotFound
}

// resolveByAliasScan walks all unit files looking for one that declares
// base+suffix in its [Install] Alias= directive. This handles the case
// where ssh.service has Alias=sshd.service but no sshd.service file
// (or symlink) exists on disk.
func (r *Registry) resolveByAliasScan(base, suffix string) (*Unit, error) {
	wantedAlias := base + suffix
	for _, dir := range r.Config.ServiceDirs {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), suffix) {
				continue
			}
			path := filepath.Join(dir, e.Name())
			realPath, err := filepath.EvalSymlinks(path)
			if err != nil {
				continue
			}
			sf := parseServiceFile(realPath)
			for _, alias := range sf.getAll("Install", "Alias") {
				if strings.TrimSpace(alias) == wantedAlias {
					// Found it. Build (or reuse) the Unit for this fragment
					// and register base as an alias.
					canonical := strings.TrimSuffix(e.Name(), suffix)
					if existing, ok := r.byKey[canonical+suffix]; ok {
						existing.Aliases[base] = struct{}{}
						r.byKey[base+suffix] = existing
						r.loadDropIns(existing, base, suffix)
						return existing, nil
					}
					u := r.buildUnit(canonical, suffix, realPath)
					u.Aliases[base] = struct{}{}
					r.byKey[base+suffix] = u
					r.loadDropIns(u, base, suffix)
					return u, nil
				}
			}
		}
	}
	return nil, ErrUnitNotFound
}

// resolveTemplateInstance handles foo@inst.service -> foo@.service.
// The instance string is recorded on the Unit and template specifiers
// (%i, %I, %n, %N) are expanded in the parsed sections.
func (r *Registry) resolveTemplateInstance(base, suffix string) (*Unit, error) {
	idx := strings.Index(base, "@")
	if idx < 0 {
		return nil, ErrUnitNotFound
	}
	prefix := base[:idx]
	instance := base[idx+1:]
	templateName := prefix + "@"

	for _, dir := range r.Config.ServiceDirs {
		path := filepath.Join(dir, templateName+suffix)
		st, err := os.Stat(path)
		if err != nil {
			continue
		}
		realPath, err := filepath.EvalSymlinks(path)
		if err != nil {
			realPath = path
		}
		_ = st

		// Each instance gets its own Unit (separate state, separate pidfile)
		// even though they share a template fragment. We don't dedupe by
		// inode here because postgresql@16-main and postgresql@17-main
		// resolve to the same template file but are independent units.
		u := &Unit{
			reg:       r,
			Canonical: base,
			Suffix:    suffix,
			Aliases:   map[string]struct{}{},
			Path:      realPath,
			Instance:  instance,
			Template:  prefix,
			Sections:  parseServiceFile(realPath),
		}
		expandTemplateSpecifiersInUnit(u)
		// Drop-ins for instance units come from two sources, both
		// supported by real systemd:
		//   - foo@.service.d/*.conf (template-wide; applies to every instance)
		//   - foo@bar.service.d/*.conf (instance-specific)
		r.loadDropIns(u, templateName, suffix)
		r.loadDropIns(u, base, suffix)
		r.byKey[base+suffix] = u
		return u, nil
	}
	return nil, ErrUnitNotFound
}

// buildUnit constructs a fresh Unit from a fragment file. Caller is
// responsible for inode bookkeeping; this just parses + registers.
// Aliases declared in [Install] Alias= are added to the Unit and indexed
// in byKey so subsequent lookups by alias name return this same pointer.
//
// Drop-in directories are loaded after the fragment so their directives
// override (scalar) or extend (list) the fragment's. Drop-ins for every
// known alias are loaded too — an admin who places a config under either
// name (canonical or alias) sees it applied.
func (r *Registry) buildUnit(canonical, suffix, realPath string) *Unit {
	u := &Unit{
		reg:       r,
		Canonical: canonical,
		Suffix:    suffix,
		Aliases:   map[string]struct{}{},
		Path:      realPath,
		Sections:  parseServiceFile(realPath),
	}
	r.byKey[canonical+suffix] = u

	// [Install] Alias= entries become alias keys pointing at the same
	// Unit. Aliases share the source unit's suffix — you can't have
	// Alias=sshd.socket on a .service unit.
	for _, alias := range u.Sections.getAll("Install", "Alias") {
		aliasBase, _ := splitSuffix(strings.TrimSpace(alias))
		if aliasBase == "" || aliasBase == canonical {
			continue
		}
		u.Aliases[aliasBase] = struct{}{}
		r.byKey[aliasBase+suffix] = u
	}

	r.loadDropIns(u, canonical, suffix)
	for alias := range u.Aliases {
		r.loadDropIns(u, alias, suffix)
	}
	return u
}

// loadDropIns merges every <name><suffix>.d/*.conf file from the search
// path into u.Sections. Directories are walked in lowest-priority-first
// order; within a directory, files are loaded alphabetically by basename.
// The end result is that high-priority dirs and lexically-later files
// override / extend lower-priority earlier ones — matching what real
// systemd does in src/core/load-dropin.c.
//
// Called by buildUnit for the canonical name and every alias known at the
// time. Also called when a new alias is discovered later (inode dedup,
// Alias= scan hits) so the late-arriving alias's drop-ins are picked up too.
func (r *Registry) loadDropIns(u *Unit, name, suffix string) {
	for _, dir := range r.Config.DropInDirs {
		overlay := filepath.Join(dir, name+suffix+".d")
		entries, err := os.ReadDir(overlay)
		if err != nil {
			continue
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".conf") {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, n := range names {
			u.Sections.merge(parseServiceFile(filepath.Join(overlay, n)))
		}
	}
}

// splitSuffix separates a unit name into its base and suffix.
// "ssh.service"          -> ("ssh", ".service")
// "ssh"                  -> ("ssh", ".service")  (default suffix)
// "postgresql@16.service"-> ("postgresql@16", ".service")
// "ssh.socket"           -> ("ssh", ".socket")
func splitSuffix(name string) (base, suffix string) {
	for _, s := range []string{".service", ".socket", ".target", ".timer", ".mount", ".path"} {
		if strings.HasSuffix(name, s) {
			return strings.TrimSuffix(name, s), s
		}
	}
	return name, ".service"
}

// expandTemplateSpecifiersInUnit replaces %i, %I, %n, %N in the unit's
// parsed sections with the instance string. Called after parsing a template
// fragment so that subsequent reads see expanded values.
//
//	%i -> instance string as-is
//	%I -> instance string with - replaced by /
//	%n -> full unit name (canonical + suffix)
//	%N -> full unit name without suffix
func expandTemplateSpecifiersInUnit(u *Unit) {
	if u.Instance == "" {
		return
	}
	repl := strings.NewReplacer(
		"%i", u.Instance,
		"%I", strings.ReplaceAll(u.Instance, "-", "/"),
		"%n", u.FullName(),
		"%N", u.Canonical,
	)
	for section, kvs := range u.Sections.sections {
		for i, kv := range kvs {
			u.Sections.sections[section][i].value = repl.Replace(kv.value)
		}
	}
}

// statInode returns the inode number for a FileInfo on Linux.
// Returns 0 if the underlying syscall.Stat_t isn't available, which
// disables inode dedup for that unit but doesn't break correctness —
// alias merge still works through the [Install] Alias= scan path.
func statInode(st os.FileInfo) uint64 {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return s.Ino
	}
	return 0
}
