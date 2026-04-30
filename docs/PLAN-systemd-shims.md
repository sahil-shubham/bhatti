# systemctl/journalctl shim fidelity — completing the v1.9.0 shim

Patch series: `v1.10.2`, `v1.10.3`, ... (each item ships as its own patch)
Previous: `v1.10.1` (CLI/UX fixes, shipped)
Triggered by: GitHub issue #12 follow-up — `systemctl status sshd` reports
"inactive" while `systemctl status ssh` reports "active" on the same daemon.

lohar today reimplements roughly nine pieces of systemd's userspace at varying
fidelity: PID 1 init, systemctl, journalctl, /dev/log syslog, /etc/resolv.conf
management, hostname, /run tmpfile creation, target-wants service activation,
and (via shell) policy-rc.d + runlevel. The systemctl bug Fastidious found is
not a one-off — it is the visible edge of a larger pattern where the shims are
name-keyed and ad-hoc where their parents are identity-keyed and structured.

This plan completes the v1.9.0 shim — fills in the gaps, fixes the bugs, and
aligns the data model with what real systemd does. Each item is a self-contained
patch shipped on its own. The bigger architectural pieces that make systemd
*systemd* (cgroup-per-unit, Type=notify, dependency ordering) are documented in
[Future architectural work](#future-architectural-work) at the bottom — they're
too large to fit in a patch and they aren't needed to fix the bugs in this plan,
but they're the difference between "a process supervisor that reads .service
files" and "a thing you can reason about with a systemd mental model".

**Target machine:** `ssh user@192.168.1.201` (raspi-5a, aarch64, Pi 5)

---

## Why this plan now

The user-visible bug is small: status reports the wrong thing for one alias.
But the debug ([github.com/sahil-shubham/bhatti/issues/12](https://github.com/sahil-shubham/bhatti/issues/12))
showed:

1. `systemctl status sshd` says inactive while `systemctl status ssh` says
   active on the same PID. Reproduced 1:1 on a fresh VM.
2. `systemctl stop sshd` is a **silent no-op** while the daemon keeps running.
   Demonstrated on the same VM as root: pidfile keyed by canonical name, lookup
   keyed by argument string, the two never meet.
3. `systemctl stop ssh` from `bhatti exec` (non-root) silently exits 0 without
   stopping anything because `svcStop` ignores `kill()` errors.
4. `parseServiceFile` reads exactly one file. Drop-in directories
   (`<unit>.service.d/*.conf`) are silently ignored — that's where most of the
   distro-shipped behaviour customisation lives.
5. `svcEnableUnit` ignores `[Install] Alias=` directives. Real systemd creates
   an alias symlink during enable; lohar doesn't, which is why `sshd.service`
   doesn't exist as a filesystem entity at all in our test VM, breaking any
   tool that scans `/etc/systemd/system/`.

Every one of those is a symptom of the same root cause: **the shim treats unit
names as opaque strings, not as identities**. Real systemd, by contrast, keeps
exactly one `Unit` object per service and routes every name (canonical and
every alias) at lookup time to the same pointer. State lives on the Unit, never
keyed by name. The data model makes the "two names disagree" bug class
unrepresentable.

A second realisation came from surveying the rest of lohar: most of the other
systemd-shaped surfaces have the same architectural shape problem. They each
make sense in isolation; they're inconsistent as a set; they share enough
state that bugs in one bleed into another. This plan addresses them together.

This is not a rewrite. The on-disk format is unchanged. The CLI surface is
unchanged. The pidfile path is unchanged. What changes is that everything that
used to take `name string` takes `*Unit`, the privileged boundary moves where
it should be, and the gaps in the smaller shims are closed.

---

## What we read

To make sure this plan isn't a tactical patch, I read the implementations of
the things lohar is shimming:

- **systemd itself** (`github.com/systemd/systemd`,
  `src/core/{unit,manager,load-fragment}.{c,h}` and `src/basic/unit-name.{c,h}`).
  The reference architecture: `Manager.units` is `Hashmap<name → Unit*>`,
  `n:1`. Each `Unit` owns `id` (canonical name) plus `aliases` (Set of all other
  names). All state — pidfile reference, exec status, cgroup, last restart,
  journal cursor — lives on the Unit object. `merge_by_names()` (load-fragment.c
  line 6017) merges two stub units that turn out to be the same physical file.
- **gdraheim/docker-systemctl-replacement** (`files/docker/systemctl3.py`,
  cited as inspiration in the original PLAN-systemd-rc.md). Their `_file_for_unit`
  is keyed by filename only. They escape the alias problem by leaning on
  `PIDFile=` from the unit file, not on the queried name. For services without
  `PIDFile=` — exactly our `Type=notify` ssh.service case — they fall back to
  `get_StatusFile`, which is `"%s.status" % conf.name()`, and `conf.name()`
  prefers the queried module name. **They have the same bug we have, just
  hidden whenever `PIDFile=` happens to be declared.** The lesson: we do not
  want to copy this; we want to copy systemd.

What this means for our fix shape:

- The **right architectural pattern** is systemd's: identity-typed `Unit`,
  string→pointer registry that pre-populates aliases, all state on the Unit.
- The **wrong-but-tempting** patch is what docker-systemctl-replacement does
  and what we proposed first: canonicalise the string at the top of every
  function. This works for one bug; it doesn't generalise to drop-ins, doesn't
  generalise to symlink-as-alias, and keeps string-keying spread through the
  code.

---

## The shim inventory

| # | Shim | Real systemd counterpart | Current fidelity | Severity of gaps |
|---|------|--------------------------|------------------|------------------|
| 1 | `runAgent()` (PID 1) | systemd PID 1 | Mounts, signals, zombie reap, `/run/{systemd,bhatti}` dirs, target-wants scan. **No tmpfiles.d, no sysusers.d, no generators, no targets/dependencies, no socket activation, no cgroup unit assignment.** | Medium. Most things work because postinst handles their own users/dirs. Snapshot/restore lifecycle is unique and not systemd-equivalent — that's by design. |
| 2 | `/usr/bin/systemctl` shim | systemd's `systemctl` D-Bus client | start/stop/restart/reload/enable/disable/status/show/cat/mask/unmask/list-units/list-unit-files/preset/kill/is-active/is-enabled. **Name-keyed state, no alias merge, no drop-ins, no privilege boundary, silent kill failures, no failed-state tracking, no `Restart=` policy, no `Type=notify` sd_notify protocol, no socket activation runtime.** | **High.** This is the one Fastidious tripped on. See the "Why this plan now" list above. |
| 3 | `/usr/bin/journalctl` shim | systemd-journald + journalctl | Reads `/var/log/bhatti/<unit>.log`. Supports `-u`, `-f`, `-n`. **No metadata indexing, no cursors, no `--since`/`--until`, no priority filtering, no boot rotation, no JSON output, no kernel ringbuffer, splits per-name not per-Unit (so alias bug bleeds in here too).** | Medium. Users mostly want `tail -f`; that works. The alias-split is the same bug class as #2. |
| 4 | `/dev/log` syslog receiver | systemd-journald (or rsyslog) | Listens on unix datagram, parses `<priority>tag[pid]: msg`, appends to `/var/log/bhatti/<tag>.log`. **No structured fields, no priority filtering, no rate limiting, no rotation, no `_TRANSPORT=stdout` capture (only syslog).** | Low. Works for the "I want to see sshd.log" case. Tag-keying is independent of the systemctl Unit identity, so logs from a service started under one name go to a different file than logs sent to syslog by the same daemon — same identity-fragmentation bug as #2 and #3. |
| 5 | `/etc/resolv.conf` management (`applyDNS`, `ensureResolvConf`) | systemd-resolved + nss-resolve | Writes a static resolv.conf at boot from config drive or fallback. **No 127.0.0.53 stub resolver, no per-link DNS, no DNSSEC, no caching, no LLMNR/mDNS.** | Low. We deliberately keep this minimal; the rootfs pins out `systemd-resolved`. The gap is acceptable. |
| 6 | Hostname (`Sethostname` + `/etc/hosts`) | systemd-hostnamed | Set once at boot from config. No D-Bus, no transient/static distinction, no `hostnamectl`. | Negligible. |
| 7 | `/run/systemd/system` marker | systemd's runtime dir convention | Empty directory created at boot. Causes `deb-systemd-helper` to take the systemctl path instead of the no-op path. | Low. Works as intended. The dir being empty means we don't honour drop-ins placed at runtime — see C2. |
| 8 | Target-wants activation (`startEnabledServices`) | systemd's target dependency graph | Scans `/etc/systemd/system/multi-user.target.wants/`, calls `svcStart` on each. **No ordering (`After=`/`Before=`), no parallel start with barriers, no failure handling for required deps, no targets other than `multi-user.target`.** | Medium. Order matters for some services (`postgres` then `pgbouncer`). We mostly get away with it because most services tolerate restart racing. |
| 9 | `/usr/sbin/policy-rc.d` + `/sbin/runlevel` | sysvinit invoke-rc.d / runlevel | 3-line shell scripts that say "yes, start" and "5". | Negligible. They exist solely to unblock Debian package postinsts, which is the only thing that calls them. |

The systemctl shim (#2) is by far the highest-leverage problem — it's where the
alias bug lives, and the architectural fix there enables fixing #3, #4 and #8
as side effects (because once Unit identity exists, journalctl reads logs by
canonical name, syslog tagging can be reconciled to the canonical name,
target-wants activation iterates Units instead of filenames).

---

## The architectural principle

Three rules, taken from `src/core/unit.h` and `manager.h`. These are the parts
of systemd we want to be faithful to, even at the cost of more code:

1. **One `Unit` object per service** — canonical identity is structural, not
   string-typed.
2. **The lookup map is `n:1`** — every name (canonical and every alias) is a
   key, all pointing at the same `*Unit`. Merging happens once at load time.
3. **All runtime state lives on the Unit** — pidfile path, log path, enabled
   marker, last-failure flag, cgroup id. Nothing is keyed by name string after
   resolution.

A fourth rule comes from systemd's process model:

4. **Privileged operations run in PID 1; the CLI is a thin client.** systemd's
   `systemctl` IPCs into PID 1 over D-Bus; polkit checks the caller's identity
   at the bus boundary. We will use lohar's existing UDS instead of D-Bus, but
   the topology is the same: the systemctl shim becomes a client that asks
   PID-1 lohar to do the privileged thing, with caller-uid propagated for
   authorisation.

```
   ┌─────────────────────────────────────────────────────────┐
   │ /usr/bin/systemctl  (thin client, runs as caller)        │
   │ — argument parsing, output formatting                    │
   │ — IPCs the requested op to PID-1 lohar via UDS           │
   └────────────────────┬────────────────────────────────────┘
                        │  privileged boundary (uid check + audit)
   ┌────────────────────▼────────────────────────────────────┐
   │ lohar agent (PID 1, runs as root)                        │
   │                                                          │
   │  ┌────────────────────────────────────────────────────┐ │
   │  │ Unit registry (n:1 hashmap)                        │ │
   │  │   map[string]*Unit    every name → same *Unit      │ │
   │  └────────────────────────────────────────────────────┘ │
   │                                                          │
   │  ┌────────────────────────────────────────────────────┐ │
   │  │ Unit { Canonical, Aliases, Path, Instance,         │ │
   │  │        Sections (fragment + drop-ins merged),       │ │
   │  │        Pid, LogPath, EnabledAt, LastExitCode, ... }│ │
   │  └────────────────────────────────────────────────────┘ │
   │                                                          │
   │  ┌──────────────────┐  ┌────────────────────────────┐   │
   │  │ Loader           │  │ Operations                 │   │
   │  │ - filename scan  │  │ start/stop/status/enable/  │   │
   │  │ - Alias= merge   │  │ restart/reload/kill/...    │   │
   │  │ - symlink merge  │  │ all take *Unit             │   │
   │  │   by inode       │  │                            │   │
   │  │ - drop-in load   │  │                            │   │
   │  └──────────────────┘  └────────────────────────────┘   │
   └─────────────────────────────────────────────────────────┘
```

The same Unit registry powers journalctl (`runJournalctl` resolves the queried
unit to a Unit and reads `Unit.LogPath`) and `startEnabledServices` (iterates
the registry instead of globbing wants/).

---

# The patch series

Each Cn is a self-contained patch on the v1.10.x line. C1 must land first
because everything else depends on it; after that, C2–C6 are independent and
can ship in any order as separate patches.

| # | Patch | What ships | Why this can land alone |
|---|-------|------------|-------------------------|
| C1 | v1.10.2 | Unit registry + identity-keyed state | Fixes Fastidious's bug. Every later item builds on this type. |
| C2 | v1.10.3 | Drop-in directory loading | Independent extension to the loader. |
| C3 | v1.10.4 | `[Install] Alias=` symlinks on enable | Independent change to `svcEnable`. |
| C4 | v1.10.5 | Privilege boundary (systemctl as IPC client) | Independent change in dispatch + handler. |
| C4b | (folded into C4) | journalctl on Unit identity | Trivial follow-up using the registry. |
| C5 | v1.10.6 | Syslog tag → Unit canonical reconciliation | Independent change in syslog receiver. |
| C6 | v1.10.7 | `Restart=` policy + failed-state tracking | Independent extension of Unit type. |

If a Cn breaks something, only that Cn rolls back — earlier patches are
self-contained.

Lifts unit identity into a type, fixes the alias and drop-in classes of bug,
moves the privilege boundary, and closes the smaller shim gaps that compound
the same root cause.

---

## C1 — Unit identity + registry (the architectural core)

New file: `cmd/lohar/unit.go`

This is the change that everything else builds on. The systemctl shim today is
a kitchen of `func(name string)` that each call `findServiceFile`, `pidFile`,
`serviceLogPath`, `parseServiceFile` independently — the same lookup work
repeated, each function free to disagree about what `name` means. C1 collapses
that into a single resolution step that produces a `*Unit`, and rewrites every
operation to take `*Unit`.

### Types

```go
// Unit is the resolved identity of a systemd-style unit.
// Two queries that resolve to the same physical fragment file
// produce pointer-identical *Unit values via the registry.
type Unit struct {
    Canonical string                // "ssh"
    Suffix    string                // ".service" / ".socket" / ".target" / ".timer"
    Aliases   map[string]struct{}   // {"sshd"}
    Path      string                // resolved fragment, real path (not symlink)
    Instance  string                // "" or e.g. "16-main" for postgresql@16-main
    Sections  serviceFile           // fragment merged with drop-ins
    Masked    bool
    Template  string                // for instance units, the template basename
}

// Registry resolves any name (canonical, alias, symlink, template
// instance) to the same *Unit pointer. Memoised for the lifetime of
// a systemctl/journalctl/agent operation.
type Registry struct {
    mu    sync.Mutex
    byKey map[string]*Unit         // every name (canonical + every alias)
}

func (r *Registry) Resolve(name string) (*Unit, error)
```

### Resolution algorithm (mirrors systemd's `unit_file_build_name_map`)

For each query name `q`:

1. **Strip suffix** if present, remember it (`.service` default).
2. **Template expansion**: if `q` contains `@`, split into `prefix@instance`,
   load the template `prefix@.service`, set `Unit.Instance = instance`, expand
   `%i`/`%I`/`%n`/`%N` specifiers in the loaded sections.
3. **Filesystem direct match**: walk `serviceDirs` looking for `q.suffix`.
   Resolve symlinks to a real path; record the real path.
4. **Alias merge by inode**: for each candidate file, `os.Stat` to get inode.
   If two names (the queried one and a previously-loaded one) point at the
   same inode, they're aliases of the same Unit. Reuse the existing `*Unit`
   and add this name to its `Aliases` set.
5. **`[Install] Alias=` merge**: scan the chosen unit file for `Alias=`
   directives; add each alias name to the registry as a key pointing at the
   same `*Unit`. (Real systemd does this in `merge_by_names`.)
6. **Drop-in load**: see C2. Merge `<dir>/<canonical>.service.d/*.conf`
   contents into `Unit.Sections` for every alias the unit answers to, in
   precedence order: `/etc/systemd/system/` overrides `/run/systemd/system/`
   overrides `/usr/lib/systemd/system/`.
7. **Mask check**: if any candidate path is a symlink to `/dev/null`, set
   `Masked = true`.
8. **Memoise**: insert one `*Unit` value under every name it answers to in
   `byKey`.

The result: `Resolve("ssh")` and `Resolve("sshd")` return the same pointer.

### State lives on the Unit, not on strings

```go
// before
pidFile(name)            → /run/bhatti/services/<name>.pid
serviceLogPath(name)     → /var/log/bhatti/<name>.log

// after
(u *Unit) PidPath()      → /run/bhatti/services/<u.Canonical>.pid
(u *Unit) LogPath()      → /var/log/bhatti/<u.Canonical>.log
(u *Unit) WantsLink(target string) string  // /etc/systemd/system/<target>.wants/<u.Canonical>.service
```

Every operation function changes signature:

```go
// before                    →  after
svcStart(name string)         svcStart(u *Unit)
svcStop(name string)          svcStop(u *Unit)
svcStatus(name string)        svcStatus(u *Unit, displayName string)  // displayName = what the user typed
svcShow(name string, ...)     svcShow(u *Unit, ...)
svcEnable(name string)        svcEnable(u *Unit)
svcIsActive(name string)      svcIsActive(u *Unit)
svcIsEnabled(name string)     svcIsEnabled(u *Unit)
svcKill(name string, sig)     svcKill(u *Unit, sig)
svcReload(name string)        svcReload(u *Unit)
svcMask(name string)          svcMask(u *Unit)
```

`svcStatus` keeps the queried name for the header line (real systemd does the
same — it prints whichever name you asked, then resolves underlying state to
the unified Unit).

### Dispatch in `runSystemctl`

```go
reg := newRegistry()
for _, raw := range units {
    u, err := reg.Resolve(raw)
    if err != nil { /* not-found behaviour as today */ }
    switch command {
    case "start":  svcStart(u)
    case "stop":   svcStop(u)
    case "status": svcStatus(u, raw)        // pass raw for display
    ...
    }
}
```

### What goes away

- `findServiceFile`, `findUnitFile`, `resolveTemplateName`,
  `resolveSocketToService`, `isMasked`, `pidFile`, `serviceLogPath`,
  `normalizeName` — all collapse into `Registry.Resolve` + Unit methods.
- `parseServiceFile` becomes an internal loader detail; nothing outside the
  registry calls it directly.
- The duplicate alias-walk in `findServiceFile` (lines 386-401 today) is
  done once at registry-load time, not on every operation.

### What stays the same

- Pidfile path: `/run/bhatti/services/<canonical>.pid`. The on-disk format is
  unchanged; in practice the only files lohar wrote on the test VM were
  already keyed by canonical name.
- Logfile path: `/var/log/bhatti/<canonical>.log`. Same reasoning.
- Output format of `status`, `show`, `cat`, `list-units`, `list-unit-files`.

### Changes

- `cmd/lohar/unit.go`: new file, ~250 lines. `Unit` type, `Registry`,
  `Resolve`, `Unit` methods.
- `cmd/lohar/systemctl.go`: rewrite operation functions to take `*Unit`. The
  file shrinks because dedup goes away (~1100 → ~800 LOC).
- `cmd/lohar/systemctl_test.go`: update test signatures; add the regression
  tests in C7.

---

## C2 — Drop-in directory loading

Real systemd loads `<unit>.service.d/*.conf` for every name a unit answers to,
from `/etc/systemd/system/`, `/run/systemd/system/`, `/usr/lib/systemd/system/`,
in defined precedence. The shim today reads exactly one file. This is invisible
until something doesn't work, and it usually doesn't error — the missing
directive just isn't applied, and the failure mode is "behaves wrong, no
explanation".

### Implementation

In `Registry.Resolve`, after the fragment is loaded, for each name in
`Unit.Aliases ∪ {Unit.Canonical}`:

```go
for _, dir := range []string{
    "/etc/systemd/system",       // highest priority
    "/run/systemd/system",
    "/usr/lib/systemd/system",   // lowest priority
} {
    overlay := filepath.Join(dir, name+".service.d")
    entries, _ := os.ReadDir(overlay)
    sort.Slice(entries, ...)              // alphabetical
    for _, e := range entries {
        if !strings.HasSuffix(e.Name(), ".conf") { continue }
        sf := parseServiceFile(filepath.Join(overlay, e.Name()))
        u.Sections.merge(sf)              // later directives override
    }
}
```

`serviceFile.merge(other)` appends `other`'s key/value pairs section by
section. Multi-value keys (`ExecStartPre`, `Environment`, `EnvironmentFile`)
accumulate; single-value keys overwrite. Empty value means reset (e.g.
`ExecStart=` on its own clears prior `ExecStart=` entries — systemd's
list-reset semantics).

### What this fixes

- `/etc/systemd/system/ssh.service.d/00-no-wait-online.conf` → respected.
- `/lib/systemd/system/postgresql@.service.d/00-defaults.conf` → respected.
- Distro patches that ship as drop-ins instead of replacement units.

### Changes

- `cmd/lohar/unit.go`: drop-in walk inside `Resolve`, ~40 lines.
- `cmd/lohar/systemctl.go`: `serviceFile.merge`, ~25 lines.
- Test: drop-in overrides `ExecStart`; multiple drop-ins merge in alphabetical
  order; `ExecStart=` (empty) resets prior values.

---

## C3 — `[Install] Alias=` symlink creation in enable

When `systemctl enable ssh` runs on real systemd, three kinds of symlinks are
created from the `[Install]` section:

- `WantedBy=multi-user.target` → `multi-user.target.wants/ssh.service`
- `RequiredBy=foo.service`     → `foo.service.requires/ssh.service`
- **`Alias=sshd.service`       → `/etc/systemd/system/sshd.service`** ← lohar misses this

The third one matters because it makes the alias visible to anything globbing
`/etc/systemd/system/`, including:

- lohar's own `svcIsEnabled` (which globs `*.wants/<name>.service`)
- Other unit files that say `After=sshd.service` instead of `After=ssh.service`
- Distro tooling that probes for unit files by alias name
- Cold-restart loader: with the symlink in place, the inode-merge in C1's
  `Resolve` picks up the alias relationship even on a freshly-rebuilt registry,
  without re-parsing every unit's `[Install]` section.

### Implementation

In `svcEnable`, after creating wants/requires links:

```go
for _, alias := range u.Sections.getAll("Install", "Alias") {
    aliasName := strings.TrimSuffix(alias, ".service")
    aliasLink := filepath.Join("/etc/systemd/system", aliasName+u.Suffix)
    os.Remove(aliasLink)
    if err := os.Symlink(u.Path, aliasLink); err != nil { ... }
    fmt.Fprintf(os.Stderr, "Created symlink %s → %s.\n", aliasLink, u.Path)
}
```

Disable removes the alias symlink too. Mask handles aliases (mask of `sshd`
masks `ssh` because they resolve to the same Unit; the user-facing symlink at
`/etc/systemd/system/<masked>.service` is created at whichever name was passed,
which matches systemd's behaviour).

### Changes

- `cmd/lohar/systemctl.go`: alias-symlink creation in `svcEnableUnit`, removal
  in `svcDisableUnit`, ~30 lines.
- Test: enable creates the alias symlink; disable removes it; cold-load via
  `Registry.Resolve` finds the alias through the symlink-by-inode path.

---

## C4 — Privilege boundary: systemctl as IPC client

This closes the second-most-dangerous bug found in the issue #12 debug:
`bhatti exec <vm> -- systemctl stop ssh` (default unprivileged user) silently
exits 0 because `svcStop`'s `kill()` returns `EPERM` and lohar drops the error.
The same pattern hides in `svcStart`/`svcReload`/`svcKill`/`svcRestart`.

The fix is to invert the privilege model so it matches systemd: the
`/usr/bin/systemctl` binary becomes a thin client that asks PID-1 lohar to
perform the operation. PID 1 runs as root and has the privilege; the client
just formats output and returns the exit code.

### Wire format

A new vsock/UDS endpoint on PID 1 lohar, alongside the existing exec/forward
listeners. The protocol is JSON over the existing control connection — same
auth, same observability, same connection multiplexing.

```go
type SystemctlReq struct {
    Op       string   `json:"op"`        // "start" | "stop" | "status" | ...
    Units    []string `json:"units"`
    Flags    map[string]string `json:"flags,omitempty"`
    CallerUID uint32  `json:"caller_uid"`
    CallerGID uint32  `json:"caller_gid"`
}

type SystemctlResp struct {
    ExitCode int    `json:"exit_code"`
    Stdout   string `json:"stdout"`
    Stderr   string `json:"stderr"`
}
```

For `status` and `show` and `list-units`, the server formats the output as the
client would have. For interactive pagination (`-f` on journalctl) we keep the
existing in-process path because nothing privileged is needed for read-only
log access — see C4b.

### Authorisation

A simple uid-based policy for now (we can graduate to a polkit-shaped rule
file later if anyone asks):

```go
// Privileged ops require uid 0 OR group "systemd-journal" for read-only
// query ops. Read-only ops (status, show, cat, is-active, is-enabled,
// list-units) are allowed for any uid.
var readOnlyOps = map[string]bool{
    "status": true, "show": true, "cat": true,
    "is-active": true, "is-enabled": true, "is-failed": true,
    "list-units": true, "list-unit-files": true,
}
```

If the caller is non-root and the op is privileged, return:
```
Failed to <op> <unit>.service: Access denied
```
and exit non-zero. Matches systemd's polkit-rejection message exactly so
existing scripts and parsing tools work.

### Where to draw the line

Read-only ops (status, show, cat, is-active, is-enabled, list-units,
list-unit-files, journalctl read) keep their current in-process path. They
don't need privilege, the IPC round-trip would slow down `bhatti exec` calls
that just want to check status, and the agent is already PID 1 reading the
same files.

Privileged ops (start, stop, restart, reload, kill, enable, disable, mask,
unmask, daemon-reload, preset, reset-failed) go through the IPC.

### How the dispatch works

`runSystemctl` checks the op:

```go
if requiresPrivilege(command) {
    resp, err := callDaemon(SystemctlReq{
        Op: command, Units: units, Flags: flags,
        CallerUID: uint32(os.Getuid()), CallerGID: uint32(os.Getgid()),
    })
    os.Stdout.Write([]byte(resp.Stdout))
    os.Stderr.Write([]byte(resp.Stderr))
    os.Exit(resp.ExitCode)
}
// otherwise, in-process as today
```

PID 1 lohar handles the request by running the same `svc*` operation
functions in-process, against the same Registry. No process fork, no extra
goroutine pool — these ops are fast.

### Snapshot/restore considerations

The IPC endpoint listens on UDS at `/run/bhatti/control.sock` (already
exists); no changes to the existing exec/forward vsock listeners. Adding a
new code path on the existing socket is structurally identical to adding a
new exec verb, which already survives snapshot/restore correctly.

### What this also fixes

`svcStop`'s `kill()` now actually runs as root, so `EPERM` doesn't happen for
unprivileged callers — they hit the access-denied check up front and get a
clear error. `kill()` errors that *do* happen (`ESRCH`, `EINVAL`) are now
propagated and surfaced via the `ExitCode` field in `SystemctlResp`, instead
of being silently swallowed. The whole `svcStop` becomes much shorter:

```go
func svcStop(u *Unit) error {
    pid, err := readPID(u)
    if err != nil { return nil }       // already stopped
    if !processAlive(pid) {            // pidfile stale
        os.Remove(u.PidPath())
        return nil
    }
    if execStop := u.Sections.get("Service", "ExecStop"); execStop != "" { ... }
    if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
        return fmt.Errorf("kill: %w", err)  // surfaced now
    }
    // wait + SIGKILL fallback as today
}
```

### Changes

- `cmd/lohar/systemctl_ipc.go`: new file, ~200 lines. Request/response types,
  client dispatch, server handler that calls into the same `svc*` functions.
- `cmd/lohar/handler.go`: new control-connection verb `systemctl` routed to
  the handler.
- `cmd/lohar/systemctl.go`: error propagation in `svcStop`/`svcKill`/
  `svcReload` (~30 lines of `if err != nil { return err }` discipline).
- Test: non-root caller gets access-denied; root caller succeeds; ESRCH on a
  dead pidfile returns 0 (already-stopped is success); `kill -EPERM` is no
  longer reachable from the unprivileged path.

---

## C4b — journalctl rebuilt on Unit identity

While the registry is being added, `runJournalctl` joins the same model:

```go
func runJournalctl(args []string) {
    ...
    if unit != "" {
        u, err := reg.Resolve(unit)
        if err != nil {
            fmt.Fprintf(os.Stderr, "No journal files found for %s.\n", unit)
            os.Exit(1)
        }
        logPath := u.LogPath()           // canonical, regardless of alias
        ...
    }
}
```

This collapses the same alias-split bug that affects the systemctl shim:
`journalctl -u sshd` and `journalctl -u ssh` now read the same file.

The standalone `/var/log/bhatti/<tag>.log` files written by syslog (#4 in the
inventory) are also reconciled — see C5.

### Changes

- `cmd/lohar/systemctl.go` (the `runJournalctl` function): use registry,
  ~10 LOC change.
- Test: `journalctl -u sshd` and `journalctl -u ssh` both return the same
  output after a service started under either name.

---

## C5 — Syslog tag → Unit canonical reconciliation

Today, `startSyslogReceiver` writes to `/var/log/bhatti/<tag>.log` where
`<tag>` is whatever syslog message tag the daemon used (typically the binary
name — `sshd`, not `ssh`). lohar's `svcStart` writes to
`/var/log/bhatti/<service-name>.log`. So a single daemon ends up with logs
split across two files: one captured from stdout/stderr (named after the
service), one received over /dev/log (named after the binary).

Fix: in the syslog receiver, look up the tag in the Unit registry. If the
tag matches a Unit's canonical name OR any alias, write to the canonical
log path. If it doesn't match any known unit (kernel, login, custom daemons
not managed by lohar), fall back to `/var/log/bhatti/<tag>.log` as today.

```go
logPath := filepath.Join("/var/log/bhatti", tag+".log")  // fallback
if u, err := reg.Resolve(tag); err == nil {
    logPath = u.LogPath()
}
```

### Changes

- `cmd/lohar/main.go`: registry lookup in `startSyslogReceiver`,
  ~5 LOC change. Registry must be a process-wide singleton initialised in
  `runAgent` for this to work — small refactor in C1.
- Test: write a syslog message tagged `sshd`; assert it lands in
  `<logDir>/ssh.log`, not `<logDir>/sshd.log`.

---

## C6 — Failed-state tracking + `Restart=` policy

Real systemd tracks per-unit `ActiveState` in `{active, reloading, inactive,
failed, activating, deactivating}` and supports `Restart=on-failure` /
`Restart=always` / `Restart=on-success` / etc. The shim today tracks only
"pidfile exists and PID is alive". So:

- `systemctl is-failed ssh` returns "active" or "inactive"; never "failed".
- A daemon that crashes is reported as inactive; admins have no way to
  distinguish a clean stop from a crash.
- `Restart=on-failure` from the unit file is silently ignored.

This is in scope because it's the same data-model lift as C1 — the failure
state belongs on the `*Unit`, not in a parallel string-keyed file. The
restart loop is the smallest possible scope: a goroutine per running Unit,
spawned by `svcStart`, watching the child PID and acting on `Restart=`.

### Implementation

`Unit` gains:

```go
ActiveState  string                 // "active" | "inactive" | "failed" | "activating"
LastExit     int                    // exit code of last terminated process
RestartCount int
```

`startDaemon` spawns a watcher goroutine:

```go
go func() {
    state, err := proc.Wait()
    if err == nil && state.ExitCode() == 0 {
        u.ActiveState = "inactive"
    } else {
        u.ActiveState = "failed"
        u.LastExit = state.ExitCode()
    }
    if shouldRestart(u) {
        time.Sleep(restartDelay(u))
        svcStart(u)
    }
}()

func shouldRestart(u *Unit) bool {
    policy := u.Sections.get("Service", "Restart")
    failed := u.ActiveState == "failed"
    switch policy {
    case "always":              return true
    case "on-failure":          return failed
    case "on-abnormal":         return failed && u.LastExit > 128
    case "on-success":          return !failed
    case "no", "":              return false
    }
    return false
}
```

Bound restart attempts via `StartLimitBurst` / `StartLimitIntervalSec`
(default 5 in 10s — same as systemd) so a flapping service doesn't pin a
core.

### `is-failed`

```go
case "is-failed":
    u, _ := reg.Resolve(units[0])
    if u.ActiveState == "failed" {
        fmt.Println("failed")
    } else {
        fmt.Println(u.ActiveState)
        os.Exit(1)
    }
```

### `reset-failed`

Was a no-op; now actually resets `u.ActiveState` from "failed" to "inactive"
and zeroes `RestartCount`.

### What this does NOT do

- Doesn't implement `Type=notify` (sd_notify) — orthogonal, openssh works
  fine without it on `Type=simple`. Tracked separately if anyone needs it.
- Doesn't implement socket activation (we already start the service
  directly).
- Doesn't track the full ActiveState transition graph (`activating` →
  `active` → `deactivating` → `inactive`). We use only `active`,
  `inactive`, `failed`, which is what 99% of consumers (`is-active`,
  `is-failed`, `status`) need.

### Changes

- `cmd/lohar/unit.go`: ActiveState + watcher goroutine, ~80 LOC.
- `cmd/lohar/systemctl.go`: `Restart=` parsing, `is-failed` op, `reset-failed`
  op, ~50 LOC.
- Test: `Restart=on-failure` actually restarts a crashing service;
  `is-failed` returns "failed" after a crash; `reset-failed` clears it;
  `StartLimitBurst` stops infinite restart.

---

## C7 — Tests

New file: `cmd/lohar/unit_test.go` (registry + resolution).
Extends: `cmd/lohar/systemctl_test.go` (operation tests with new signatures).
Extends: `pkg/engine/firecracker/systemctl_test.go` (integration on Pi).

### Unit tests (in lohar package)

```go
func TestRegistryAliasResolution(t *testing.T)
    // Write ssh.service with [Install] Alias=sshd.service.
    // reg.Resolve("ssh") == reg.Resolve("sshd") (pointer-equal).
    // Both have Canonical == "ssh", Aliases contains "sshd".

func TestRegistrySymlinkAlias(t *testing.T)
    // Write /lib/.../foo.service. Symlink /etc/.../bar.service -> foo.service.
    // reg.Resolve("foo") == reg.Resolve("bar") (inode merge).

func TestRegistryDropInLoad(t *testing.T)
    // Write fragment with ExecStart=/usr/bin/a.
    // Drop-in: 10-override.conf with ExecStart=/usr/bin/b.
    // u.Sections.get("Service", "ExecStart") == "/usr/bin/b".

func TestRegistryDropInListReset(t *testing.T)
    // Fragment: ExecStartPre=/a; ExecStartPre=/b
    // Drop-in:  ExecStartPre= ; ExecStartPre=/c
    // Result: getAll(ExecStartPre) == ["/c"]   (reset semantics)

func TestRegistryDropInForAlias(t *testing.T)
    // Fragment ssh.service with Alias=sshd.service.
    // Drop-in at sshd.service.d/00-foo.conf.
    // Loaded into the merged Unit (because sshd is an alias).

func TestRegistryTemplateInstance(t *testing.T)
    // postgresql@.service. reg.Resolve("postgresql@16-main").
    // Instance == "16-main", %i specifiers expanded.

func TestUnitStateUnification(t *testing.T)
    // svcStart on Resolve("ssh") writes pidfile at canonical path.
    // svcIsActive on Resolve("sshd") returns true.
    // svcStop on Resolve("sshd") actually kills the daemon.
    // ← This is Fastidious's bug; this test is the regression guard.

func TestSvcStopErrorPropagation(t *testing.T)
    // Mock kill returning EPERM. svcStop returns non-nil error.
    // (Not silent success.)

func TestRestartOnFailure(t *testing.T)
    // Fake service that exits 1. Restart=on-failure.
    // After 1s, watcher restarts it. ActiveState transitions
    // failed → activating → active.

func TestStartLimitBurst(t *testing.T)
    // Service that always exits 1, StartLimitBurst=2,
    // StartLimitIntervalSec=10. Watcher gives up after 2 attempts.

func TestIsFailed(t *testing.T)
    // Crash a service. systemctl is-failed → "failed", exit 0.
    // reset-failed. is-failed → "inactive", exit 1.

func TestEnableCreatesAliasSymlink(t *testing.T)
    // Service with Alias=sshd.service. svcEnable creates
    // /etc/systemd/system/sshd.service → /lib/.../ssh.service.

func TestSyslogReconciledToUnit(t *testing.T)
    // Register Unit "ssh" with alias "sshd".
    // Send syslog message tagged "sshd[123]: hello".
    // Assert /var/log/bhatti/ssh.log contains "hello".
```

### Integration tests (engine package, on Pi)

```go
func TestSystemctlAliasNotRegression(t *testing.T)
    // Issue #12 follow-up. apt install openssh-server.
    // systemctl is-active ssh && systemctl is-active sshd → both "active".
    // systemctl stop sshd → daemon actually stops (port 22 not listening).
    // systemctl start ssh → starts. systemctl status sshd → active.

func TestSystemctlPrivilegeBoundary(t *testing.T)
    // bhatti exec (default uid 1000) systemctl stop ssh → exit non-zero,
    // stderr contains "Access denied". Daemon still running.
    // bhatti exec --user root systemctl stop ssh → exit 0, daemon stopped.

func TestRestartPolicyOnFailure(t *testing.T)
    // Install a custom unit with Restart=on-failure that exits 1.
    // After 5s, observe two start log entries (auto-restart fired).

func TestServiceLogsByAlias(t *testing.T)
    // openssh-server installed. journalctl -u ssh and journalctl -u sshd
    // produce identical output.

func TestDropInOverride(t *testing.T)
    // Write /etc/systemd/system/ssh.service.d/00-port.conf with
    // ExecStart= and ExecStart=/usr/sbin/sshd -p 2222 -D.
    // systemctl restart ssh. Assert sshd listening on 2222, not 22.

func TestSnapshotRestoreUnitState(t *testing.T)
    // Install nginx. systemctl is-active nginx → active.
    // Stop (snapshot), Start (restore).
    // systemctl is-active nginx → still active. is-failed → no.
    // The Registry should rebuild from disk on cold load and unify state
    // with the running pidfile from the resumed memory.
```

### What's removed

- `TestFindServiceFileAlias` (in current `systemctl_test.go`): obsoleted by
  `TestRegistryAliasResolution` which tests the property at the registry
  level rather than the obsolete `findServiceFile` helper.

### Test coverage table

| Component | Before C7 | After C7 |
|-----------|-----------|----------|
| Unit identity / registry | 0 | 6 tests |
| Operation correctness with alias | 1 (lookup-only) | 5 (lookup + state) |
| Drop-in handling | 0 | 3 |
| Restart policy | 0 | 3 |
| Privilege boundary | 0 | 2 |
| Failed-state tracking | 0 | 2 |
| Syslog reconciliation | 0 | 1 |
| Snapshot/restore unit state | 0 | 1 |

---

## C8 — Documentation

Two documents land with this release.

### `docs/guest-agent.md` — update

Replace the systemctl section with the architectural model: Unit registry,
n:1 lookup, drop-ins, alias merge, privilege boundary. State the explicit
fidelity boundaries (what we implement, what we don't, what the equivalent
in real systemd is). Reference issue #12 as the originating bug for the
architecture.

### `docs/architecture.md` — new section "Userspace shims"

The shim inventory table from this plan, with each row pointing at the
relevant code path. So a future contributor reading "what is lohar's
relationship to systemd" gets one place that answers it.

### Changes

- `docs/guest-agent.md`: rewrite systemctl/journalctl section, ~150 LOC.
- `docs/architecture.md`: add "Userspace shims" section, ~80 LOC.

---

## Decision gates per patch

Each Cn ships when:
- Its corresponding tests in C7 pass locally and in CI.
- Integration test `TestSystemctlAliasNotRegression` (or the test specific to
  that patch) passes on the Pi.
- The patch applies cleanly on top of the previous v1.10.x release.

The series is "done" when a real `apt install openssh-server && apt install
nginx && apt install redis-server && apt install postgresql` on a fresh VM
produces: `systemctl is-active` returning `active` for every service, by every
name the package's postinst registered, with `bhatti stop && bhatti start`
preserving state. After that, future-architectural-work items become the next
batch.

---

## Dependency graph

```
C1 (Unit + Registry)  ─┬─→ C2 (drop-ins, lives inside Resolve)
   v1.10.2             ├─→ C3 (alias symlinks, uses Unit.Sections)
                       ├─→ C4 (IPC, calls into svc* with *Unit)
                       ├─→ C4b (journalctl, uses Unit.LogPath)
                       ├─→ C5 (syslog, uses Registry.Resolve)
                       └─→ C6 (failed-state, lives on Unit)

C7 (tests)       grows with each Cn — each patch ships its own tests
C8 (docs)        landed alongside C1, updated incrementally

Patch order (rough, can re-order C2–C6 freely):
  v1.10.2 — C1 Unit registry (Fastidious's bug fixed)
  v1.10.3 — C2 drop-in directory loading
  v1.10.4 — C3 alias symlinks on enable
  v1.10.5 — C4 + C4b privilege boundary + journalctl unification
  v1.10.6 — C5 syslog tag reconciliation
  v1.10.7 — C6 Restart= policy + failed-state
```

---

## Future architectural work

The Cn patches above fix bugs and align the data model. They do not, on their
own, make lohar's shim *behave* like systemd in the ways that distinguish
systemd from older inits. There are six pieces of real systemd that we don't
yet implement and whose absence will eventually cost a bhatti user (or me)
hours of debugging the wrong thing. They're documented here in priority order
so they don't get lost — each is bigger than a patch, none is needed for the Cn
patches to land, all are honest candidates for follow-up minor releases.

For each item: what systemd does, what we do today, the concrete "hours of
debugging" failure mode if we ship without it, and rough effort. The point of
this section is to keep what makes systemd actually systemd in our line of
sight, not to commit to a timeline.

### F1. Cgroup-per-unit — the architectural difference between systemd and sysvinit

**What systemd does.** Every unit gets its own cgroup at
`/sys/fs/cgroup/system.slice/<name>.service/`. `MemoryMax=`, `CPUQuota=`,
`TasksMax=`, `IOWeight=` from the unit file are written into that cgroup's
control files. `KillMode=control-group` (the default) makes stop equivalent to
`echo 1 > cgroup.kill`, which the kernel atomically applies to every process
in the group.

**What lohar does today.** Every service runs as a bare process with `Setsid`,
sharing the VM's memory pool and CPU. We `kill(-pid, SIGTERM)` to the PGID,
which catches the daemon and its direct children but not anything that
`setsid()`s itself out, double-forks, or escapes via dbus-activated helpers.

**Failure modes you'll spend hours on.**
- A misbehaving redis OOMs nginx in the same VM because there's no memory
  limit. The user reads nginx logs ("killed by OOM"), checks dmesg, blames
  their app, eventually realises another service ate the memory.
- `systemctl stop postgres` returns 0, the pidfile is gone, but a stray
  postgres worker is still holding the data directory. Next start fails with
  "another instance is already running". The user hunts orphans with `ps -ef
  | grep postgres` for 40 minutes.
- A service that `setsid()`s in `ExecStartPre=` is invisible to `systemctl
  stop` entirely. It runs forever, consuming resources, until `bhatti destroy`.

**Why this is what makes systemd *systemd*.** The cgroup-per-unit decision is
the original Lennart-era choice that distinguished systemd from upstart,
sysvinit, OpenRC. Resource accounting, reliable termination, process
containment all flow from this. A systemctl shim without cgroup-per-unit is a
service launcher that happens to read .service files — not a process
supervisor. We already mount cgroup v2 in `runAgent` for Docker; the substrate
is there, we just don't use it.

**Effort.** ~250 LOC in lohar plus ~50 LOC of cgroup plumbing. Approach: on
`svcStart`, mkdir the slice and write the PID into `cgroup.procs` after fork;
parse `MemoryMax=`/`CPUQuota=`/`TasksMax=` into the corresponding cgroup
files; on `svcStop`, write `1` to `cgroup.kill` (kernel ≥5.14, fine on Pi 5).

### F2. `Type=notify` / sd_notify protocol

**What systemd does.** Unit declares `Type=notify`. Service `connect()`s to
`$NOTIFY_SOCKET` (`/run/systemd/notify`) and sends `READY=1\n` when it has
finished initialising. PID 1 transitions ActiveState from `activating` to
`active` only on receipt. `STATUS=loading config...` updates the status line
shown by `systemctl status`. `WATCHDOG=1` pings keep an `WatchdogSec=` timer
alive.

**What lohar does today.** `Type=notify` is treated identically to
`Type=simple`. We report `active` the moment fork+exec succeeds.

**Failure modes you'll spend hours on.**
- `bhatti exec dev -- 'systemctl start postgres && psql -c "select 1"'`
  fails with "connection refused". The user verifies postgres is `active`,
  re-reads their script, blames psql, blames networking, eventually realises
  postgres wasn't actually ready when we said it was.
- A modern Go service that uses sd_notify never reports ready, so a wrapper
  script that polls `systemctl is-active` thinks startup hung. Real cause:
  the service IS up, we just don't speak the protocol it's using to tell us.
- This will get worse over time. Modern daemons default to `Type=notify`:
  Postgres 14+, NetworkManager, podman, systemd-resolved, almost any Go web
  server with a startup procedure.

**Effort.** ~120 LOC inside the registry built in C1. PID 1 lohar listens on
unix dgram at `/run/systemd/notify`, attributes incoming messages to Units
(via cgroup membership lookup if F1 has landed, or via PID-to-Unit map
otherwise), parses `READY=1`/`STATUS=`/`MAINPID=`/`WATCHDOG=1`, transitions
state.

### F3. Honest dependency ordering (`After=`, `Before=`, `Requires=`, `Wants=`)

**What systemd does.** At boot, builds a DAG from these directives across all
enabled units, runs a topological sort, starts units in waves with barrier
synchronisation. `After=postgresql.service` actually waits for postgres to
reach `active` before pgbouncer starts.

**What lohar does today.** Parses these directives into `Unit.Sections` and
ignores them. `startEnabledServices` iterates `multi-user.target.wants/` in
directory-listing order, starts everything in parallel.

**Failure modes you'll spend hours on.**
- User installs postgres + pgbouncer + their app on one VM. pgbouncer's
  `After=postgresql.service` is silently ignored; pgbouncer starts before
  postgres has its socket open, crashes; once F6 (Restart=) lands, it
  thrashes. The user reads pgbouncer logs, postgres logs, network configs,
  spends two hours convinced their stack is broken before discovering boot
  order is racy on bhatti specifically.
- Custom service depends on a mount unit (`After=workspace.mount`); the mount
  isn't ready when the service starts; service fails on missing files. User
  blames the volume code.

**Effort.** ~100 LOC of DAG construction + topological sort. Synchronisation
barrier between waves depends on F2 to know when a unit is genuinely active
(not just fork-exec'd) — so this is most useful after F2 lands.

### F4. `Condition*=` directives

**What systemd does.** Evaluates `ConditionPathExists=`,
`ConditionDirectoryNotEmpty=`, `ConditionFileNotEmpty=`,
`ConditionVirtualization=`, and ~8 others at start time. If any condition
fails, the unit is skipped without an error — the conventional admin escape
hatch.

**What lohar does today.** Ignored entirely. Touching the file an admin
thinks will disable a service has no effect.

**Failure modes you'll spend hours on.**
- Admin reads `ssh.service`, sees
  `ConditionPathExists=!/etc/ssh/sshd_not_to_be_run`, runs
  `touch /etc/ssh/sshd_not_to_be_run` to disable sshd temporarily, restarts.
  sshd is still running. They verify the file exists, re-read the man page,
  google `ConditionPathExists`, eventually find the shim ignores conditions.
  20–40 minutes lost.

**Effort.** ~60 LOC. Evaluate the common four (`ConditionPathExists`,
`ConditionDirectoryNotEmpty`, `ConditionFileNotEmpty`,
`ConditionVirtualization`) at the top of `svcStart`, log
`Condition X failed, skipping` and return success. The other 8 are exotic
enough to defer until someone asks.

### F5. `StateDirectory=`, `CacheDirectory=`, `LogsDirectory=`, `ConfigurationDirectory=`

**What systemd does.** Auto-creates these directories at start time with the
unit's `User=`/`Group=` ownership. `systemctl clean` removes them.

**What lohar does today.** Ignored. The unit assumes the directory exists.

**Failure modes you'll spend hours on.**
- A unit declares `StateDirectory=foo` without an explicit `mkdir` in
  `ExecStartPre=` (because on real systemd it's not needed). Service fails to
  start: `Permission denied` opening `/var/lib/foo`. User blames the daemon's
  config, checks AppArmor, checks selinux (we don't have either), checks fs
  permissions, checks user/group. Eventually finds the missing directory.
- Common in newer services and in custom units users write themselves.

**Effort.** ~50 LOC inside `svcStart` before `ExecStartPre=`. Parse the four
directives, mkdir each as `/var/lib/<dir>`, `/var/cache/<dir>`,
`/var/log/<dir>`, `/etc/<dir>` respectively, chown to `User=`/`Group=`.

### F6. tmpfiles.d minimal handler

**What systemd does.** `systemd-tmpfiles --create` runs at boot and reads
`/usr/lib/tmpfiles.d/*.conf` + `/etc/tmpfiles.d/*.conf` to create runtime
directories declared by packages.

**What lohar does today.** Hardcodes `/run/bhatti/services` and
`/run/systemd/system` and that's it. Packages that depend on `/run/sshd`
happen to work because their postinst does its own `mkdir`; packages that
rely on tmpfiles.d alone (some NixOS-derived units, some custom daemons)
silently break.

**Failure modes you'll spend hours on.**
- A service ships only a tmpfiles.d entry for its runtime dir, no postinst
  mkdir. After install, `systemctl start foo` fails with a permission/path
  error. User stares at the unit file looking for what's wrong. The
  *missing* runtime dir is invisible until someone says "oh, tmpfiles.d
  isn't honoured".

**Effort.** ~120 LOC. A 60-line subset of `systemd-tmpfiles` covering `d`
(create dir), `D` (create dir, empty if exists), `f` (create file), `F`
(create file, truncate), `L+` (replace symlink), `R` (recursive remove of
contents). Run during `runAgent` after mounts and before
`startEnabledServices`. Reads `/usr/lib/tmpfiles.d/*.conf` and
`/etc/tmpfiles.d/*.conf` (later overrides earlier), parses each line with
`%`-specifier expansion (`%h`, `%t`, `%u`).

---

### What's intentionally still out, even long-term

We deliberately stop at F1–F6. Beyond that line is over-engineering for a
microVM sandbox — things real systemd has but where the cost outweighs the
benefit for our use case:

- **Real systemd as PID 1.** The reasoning from PLAN-systemd-rc.md still
  applies — under Firecracker snapshot/restore on ARM64, child processes of
  systemd lose their network poller state. lohar stays as PID 1.
- **D-Bus / sd-bus.** The C4 IPC uses our own UDS protocol, not D-Bus. We
  don't run dbus-daemon and don't intend to. `libpam-systemd` is pinned out
  of the rootfs.
- **logind / `loginctl`.** Single-user sandbox; sessions are meaningless.
  `tmux new-session` and similar work today because they don't depend on
  logind.
- **journald binary log format.** Plain-text per-unit logs in
  `/var/log/bhatti/` are easier to operate, snapshot, and grep. We keep
  journalctl for compatibility, not for the format.
- **PrivateTmp / ProtectSystem / sandboxing directives.** The VM is the
  sandbox; process-level hardening inside it is decoration. We honour
  `User=` and `Group=`, which actually matter, and ignore the rest.
- **Timer units.** cron exists in the rootfs and works.
- **Generators.** `init.sh` is our equivalent for the cases generators
  exist for.
- **systemd-networkd / DHCP client / timesyncd.** Static IP from config
  drive; host kernel handles clock.
- **polkit.** Replaced by the simpler uid check in C4.
- **udev rules processing.** devtmpfs handles basic device nodes; rule-based
  naming isn't needed for a microVM with known fixed devices.
- **Socket activation runtime.** Our `resolveSocketToService` already maps
  `.socket` to `.service` and starts eagerly. True socket activation (lohar
  listens, hands the fd to the service on first connection) is a lot of
  code for marginal benefit — services start fast in our VMs anyway.
- **`OnFailure=`** (start unit X when unit Y fails). Niche, easy to add
  later if anyone asks.
- **Scope and slice units beyond `system.slice`.** Once F1 lands, the
  flat `system.slice/<unit>.service` layout is enough. Hierarchical slices
  (`user.slice`, `machine.slice`) only matter if we add multi-user or
  nested-VM scenarios.

---

## Audit reference

Each row: gap → fix → release item → test that proves it.

| # | Gap | Severity | Fix | Item | Test |
|---|-----|----------|-----|------|------|
| 1 | `systemctl status sshd` reports inactive when service is active | High | Unit registry, alias-merge | C1 | `TestUnitStateUnification`, `TestSystemctlAliasNotRegression` |
| 2 | `systemctl stop <alias>` is a silent no-op | High | Unit registry | C1 | `TestUnitStateUnification` |
| 3 | `svcStop` ignores kill() errors → silent success for non-root | High | Error propagation | C4 | `TestSvcStopErrorPropagation`, `TestSystemctlPrivilegeBoundary` |
| 4 | Drop-in directories silently ignored | High | Drop-in loader | C2 | `TestRegistryDropInLoad`, `TestDropInOverride` |
| 5 | `[Install] Alias=` symlinks not created on enable | Medium | Symlink creation | C3 | `TestEnableCreatesAliasSymlink` |
| 6 | `bhatti exec` (non-root) systemctl ops silently succeed | High | Privilege boundary IPC | C4 | `TestSystemctlPrivilegeBoundary` |
| 7 | `journalctl -u sshd` and `-u ssh` read different files | Medium | Unit registry in journalctl | C4b | `TestServiceLogsByAlias` |
| 8 | Syslog logs split between `<canonical>.log` and `<binary>.log` | Medium | Tag→Unit reconcile | C5 | `TestSyslogReconciledToUnit` |
| 9 | `Restart=on-failure` ignored → crashed services stay dead | Medium | Watcher goroutine | C6 | `TestRestartOnFailure` |
| 10 | `is-failed` always returns inactive/active, never failed | Medium | ActiveState on Unit | C6 | `TestIsFailed` |
| 11 | `reset-failed` is a no-op | Low | ActiveState reset | C6 | `TestIsFailed` |
| 12 | Inode-based alias merge (symlinks) absent | Medium | Loader inode dedup | C1 | `TestRegistrySymlinkAlias` |
| 13 | Template `%i`/`%I` expansion lives in caller, not loader | Low | Move into Resolve | C1 | `TestRegistryTemplateInstance` |
| 14 | `findServiceFile`'s alias scan walks every unit on every call | Low | Done once at registry build | C1 | (covered by perf test if added) |
| 15 | `parseServiceFile` doesn't handle line continuation (`\`) | Low | Loader update | C1 | (covered by drop-in test) |
| 16 | Two pid files for the same service when alias and canonical both touched | High | Unit-keyed pidfile path | C1 | `TestUnitStateUnification` |
| 17 | Snapshot-resume doesn't validate pidfile against running PID | Low | Loader sanity check on cold load | C1 | `TestSnapshotRestoreUnitState` |

### Future-architectural-work items (separate releases, see F-section)

| # | Gap | User-visible cost | Item |
|---|-----|-------------------|------|
| F1 | No cgroup-per-unit — OOM contagion, unreliable stop, orphan workers | High ("why is my VM sluggish" / "can't restart postgres") | F1 |
| F2 | `Type=notify` ignored — false `active` reports | High ("connection refused" right after start) | F2 |
| F3 | Dependency ordering ignored — racy multi-service boot | High (multi-service stacks flake at restore) | F3 |
| F4 | `Condition*=` ignored — admin escape hatches don't work | Medium (admin disable knob silently no-ops) | F4 |
| F5 | `StateDirectory=` etc. ignored — missing runtime dirs | Medium (cryptic startup failures) | F5 |
| F6 | tmpfiles.d ignored — packages that rely on it break silently | Low–Medium | F6 |
| F7 | sysusers.d ignored | Low (postinst handles it) | (deferred) |
| F8 | Templated `.socket` units | Low | (add when needed) |

---

## Why this is worth doing now

Four reasons.

First, the alias bug Fastidious found is one report; the architectural shape
that produced it produces the same class of bug everywhere lohar shims
something. Fixing the shape once eliminates a class instead of an instance.

Second, the cost is modest — ~600 LOC of new code spread across the Cn
patches, ~400 LOC of net change in existing files, ~12 new tests. The on-disk
format doesn't change. The CLI doesn't change. There's no rootfs rebuild
required (the shim is the lohar
binary; rebuilding lohar is `make build` + the existing release flow).

Third, the systemctl shim is increasingly the surface bhatti users touch
when they "really use" their VM (apt install something, expect it to work).
Each interaction either reinforces "this is a real Linux machine that happens
to be a sandbox" or breaks the illusion. The bugs in this plan all live on
the wrong side of that line.

Fourth, building this shim is one of the better learning surfaces in bhatti.
The Cn patches teach us systemd's data model (Unit identity, n:1 lookup,
state-on-identity); the F-items teach us *why* systemd made the choices it
did (cgroup-per-unit, sd_notify, dependency DAG). We get those lessons by
actually implementing the parts that matter, not by depending on the whole
thing. Keeping what makes systemd actually systemd in our line of sight —
even the parts we haven't built yet — is the point.
