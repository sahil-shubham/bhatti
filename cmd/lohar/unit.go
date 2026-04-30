//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

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

// PidPath returns /run/bhatti/services/<canonical>.pid.
// All state-keyed paths use the canonical name regardless of which
// alias the caller queried by — that's the whole point of Unit identity.
func (u *Unit) PidPath() string {
	return filepath.Join(pidDir, u.Canonical+".pid")
}

// LogPath returns /var/log/bhatti/<canonical>.log.
func (u *Unit) LogPath() string {
	return filepath.Join(logDir, u.Canonical+".log")
}

// WantsLink returns the path of the symlink in <target>.wants/ that
// would be created by `systemctl enable`, e.g.
// /etc/systemd/system/multi-user.target.wants/ssh.service.
func (u *Unit) WantsLink(target string) string {
	return filepath.Join("/etc/systemd/system", target+".wants", u.FullName())
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
// Caller is responsible for ensuring pidDir exists (cheap, idempotent).
func (u *Unit) WritePID(pid int) error {
	os.MkdirAll(pidDir, 0755)
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
// the unit answers to return the same pointer.
//
// A Registry is created per process invocation; for the systemctl shim it
// lives for the duration of one command, for PID-1 lohar it's long-lived
// and shared with the syslog receiver and journalctl invocations.
type Registry struct {
	mu    sync.Mutex
	byKey map[string]*Unit // every name (incl. suffix-stripped aliases) -> Unit
	// byInode dedupes units reachable through symlinks: the same physical
	// file resolved through two paths returns the same *Unit.
	byInode map[uint64]*Unit
}

// NewRegistry returns a fresh, empty registry.
func NewRegistry() *Registry {
	return &Registry{
		byKey:   make(map[string]*Unit),
		byInode: make(map[uint64]*Unit),
	}
}

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
	if u, ok := r.byKey[base]; ok && u.Suffix == suffix {
		return u, nil
	}
	if u, ok := r.byKey[base]; ok && suffix == ".service" {
		// Cached without explicit .service suffix (the common case).
		return u, nil
	}

	// Mask check: any service dir with <base><suffix> -> /dev/null.
	if r.checkMasked(base, suffix) {
		u := &Unit{
			Canonical: base,
			Suffix:    suffix,
			Aliases:   map[string]struct{}{},
			Masked:    true,
		}
		r.byKey[base] = u
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

	return nil, fmt.Errorf("%w: %s%s", ErrUnitNotFound, base, suffix)
}

// checkMasked returns true if base+suffix is a symlink to /dev/null in any
// service dir. This must precede direct-match because os.Stat would
// follow the symlink and fail.
func (r *Registry) checkMasked(base, suffix string) bool {
	for _, dir := range serviceDirs {
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
	for _, dir := range serviceDirs {
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
				r.byKey[base] = existing
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
	for _, dir := range serviceDirs {
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
					if existing, ok := r.byKey[canonical]; ok && existing.Suffix == suffix {
						existing.Aliases[base] = struct{}{}
						r.byKey[base] = existing
						return existing, nil
					}
					u := r.buildUnit(canonical, suffix, realPath)
					u.Aliases[base] = struct{}{}
					r.byKey[base] = u
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

	for _, dir := range serviceDirs {
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
			Canonical: base,
			Suffix:    suffix,
			Aliases:   map[string]struct{}{},
			Path:      realPath,
			Instance:  instance,
			Template:  prefix,
			Sections:  parseServiceFile(realPath),
		}
		expandTemplateSpecifiersInUnit(u)
		r.byKey[base] = u
		return u, nil
	}
	return nil, ErrUnitNotFound
}

// buildUnit constructs a fresh Unit from a fragment file. Caller is
// responsible for inode bookkeeping; this just parses + registers.
// Aliases declared in [Install] Alias= are added to the Unit and indexed
// in byKey so subsequent lookups by alias name return this same pointer.
func (r *Registry) buildUnit(canonical, suffix, realPath string) *Unit {
	u := &Unit{
		Canonical: canonical,
		Suffix:    suffix,
		Aliases:   map[string]struct{}{},
		Path:      realPath,
		Sections:  parseServiceFile(realPath),
	}
	r.byKey[canonical] = u

	// [Install] Alias= entries become alias keys pointing at the same Unit.
	for _, alias := range u.Sections.getAll("Install", "Alias") {
		aliasBase, _ := splitSuffix(strings.TrimSpace(alias))
		if aliasBase == "" || aliasBase == canonical {
			continue
		}
		u.Aliases[aliasBase] = struct{}{}
		r.byKey[aliasBase] = u
	}
	return u
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
