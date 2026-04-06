# Code Reorganization — File Splitting & Cleanup

Four files contain 75% of the production code. Splitting them improves
navigability, makes bug hunts faster, and surfaces dead code. No
refactoring — just mechanical moves with tests between each step.

---

## Current State

| File | Lines | Responsibility count |
|------|-------|---------------------|
| `pkg/engine/firecracker/engine.go` | 1908 | 14+ (create, stop, start, destroy, pause, resume, exec, shell, tunnel, files, sessions, ports, FC process mgmt, helpers) |
| `pkg/server/routes.go` | 3064 | 16+ (sandbox CRUD, exec, files, proxy, secrets, templates, volumes, images, snapshots, tasks, publish, sessions, checkpoints, metrics, health) |
| `pkg/store/store.go` | 1683 | 12+ (users, sandboxes, templates, secrets, volumes, persistent volumes, images, snapshots, tasks, publish rules, backups, FC state) |
| `cmd/bhatti/cli.go` | 2683 | 10+ (sandbox ops, user mgmt, file ops, publish, setup, serve, volumes, images, snapshots, backup) |

Target: each file covers one domain concept. No hard line-count ceiling —
a 600-line file that owns one concept is better than two 300-line files
that split a concept arbitrarily.

---

## Why Now

The reliability audit (docs/archive/RELIABILITY-AUDIT.md) traced the
`rory` corruption to code in `engine.go` that spans snapshot creation,
thermal transitions, and FC process management — three unrelated domains
in the same 1908-line file. The snapshot recovery fix
(docs/archive/PLAN-snapshot-recovery.md) required touching `Stop()`,
`fcPut()`, `fcPatch()`, and `ensureHot()` — all in `engine.go`, all in
different conceptual regions. Navigating that file during an incident is
active drag on debugging.

Similarly, `routes.go` at 3064 lines is a single file where a change to
the proxy handler can accidentally impact the publish handler during a
merge. The v0.5 and v0.6 plans both added features to `routes.go` that
were conceptually unrelated but adjacent in the file.

This is not speculative cleanup. Every feature plan from v0.3 onward has
added code to these same four files. Each addition increases the cost of
the next change.

---

## Principles

1. **Move, don't rewrite — this is the top priority.** Cut and paste
   functions as-is. No edits to function bodies. No renaming. No
   "while I'm here" fixes. Before committing every file, diff the
   moved functions line-by-line against the original to verify nothing
   was modified. A single accidental edit turns a risk-free move into
   a potential regression with no test coverage for the change. The
   verification step is:
   ```bash
   # After creating the new file, before committing:
   # For each function moved, extract it from the old file (git show)
   # and diff against the new file. Zero diff = safe to commit.
   git diff HEAD -- pkg/engine/firecracker/engine.go | grep '^-' | grep -v '^---' > /tmp/removed.txt
   git diff HEAD -- pkg/engine/firecracker/fc.go | grep '^+' | grep -v '^+++' > /tmp/added.txt
   # Strip the +/- prefixes and diff the content:
   sed 's/^-//' /tmp/removed.txt > /tmp/old.txt
   sed 's/^+//' /tmp/added.txt > /tmp/new.txt
   diff /tmp/old.txt /tmp/new.txt
   # If this produces ANY output, something was changed during the move.
   ```
   This is not optional. Do it for every commit in every phase.

2. **One file per commit.** Extract one domain into a new file, run
   `go build ./...` and `go test ./...`, commit. If tests break, the
   diff is trivial to review.

3. **Import order catches mistakes.** After moving functions, `go build`
   will fail if you forgot to move a helper that only the moved functions
   use. Fix by moving the helper too, or by keeping it in the original
   file if it's shared.

4. **Test files follow source files — but in a separate phase.** If
   `exec.go` is extracted from `engine.go`, functions tested in
   `engine_test.go` that only test exec behavior should eventually move
   to `exec_test.go`. But don't split test files in the same commit as
   source files. Test splitting is Phase 6, after all source moves are
   complete and green.

5. **Types stay with their primary consumer.** When a struct is used by
   multiple files in the same package, it stays in the file that defines
   the concept. E.g. `fcProcess` and `startFCOpts` move with the FC
   process management functions, not with the callers.

6. **`init()` functions are the landmine.** `cmd/bhatti/cli.go` has 11
   `init()` functions that register cobra commands and flags. Each one
   must move with its associated command variable. If an `init()` is
   missed, the command silently disappears from the CLI with zero
   compile errors. Verification: `bhatti --help` after every commit.

7. **Test on the Pi cluster, not agni-01.** agni-01 runs the production
   workflow. Do not run test builds or integration tests there during
   this refactor — a broken intermediate state would take down prod.
   Use the `integration.yml` workflow (`workflow_dispatch` trigger) which
   runs on the `arc-runner-set` Pi cluster. After each phase:
   ```bash
   gh workflow run integration.yml
   gh run watch  # wait for green
   ```
   Local verification (`go build ./...`, `go test ./...`) catches
   compile errors and unit test failures. The integration workflow
   catches runtime issues (Firecracker boot, snapshot, agent comms)
   on real hardware without risking prod.

---

## Phase 1 — `pkg/engine/firecracker/engine.go` (1908 → ~300)

This is where snapshot bugs live. Split first.

### Step 1.1: `fc.go` — Firecracker process management

**Functions to move:**

```
startFC(socketPath, opts)           line 1649   (method on *Engine)
startFCBare(socketPath)             line 1657   (method on *Engine)
startFCJailed(socketPath, opts)     line 1676   (method on *Engine)
waitForSocket(path)                 line 1791   (package-level)
validateSocketPath(path)            line 1802   (package-level)
killFC(cmd, timeout)                line 1813   (package-level)
copyBlock(src, dst)                 line 1831   (package-level)
copyRootfs(src, dst)                line 1836   (package-level, alias for copyBlock)
fcAPIClient(socketPath)             line 1841   (package-level)
fcPut(ctx, client, path, body)      line 1856   (package-level)
fcPatch(ctx, client, path, body)    line 1871   (package-level)
```

**Types to move:**

```
fcProcess struct                    line 1631
startFCOpts struct                  line 1640
```

**Why this is safe:** These are standalone utilities. They don't read
or write VM state. They take explicit parameters and return results.
The method receivers on Engine (`startFC`/`startFCBare`/`startFCJailed`)
only access `e.cfg` and `e.jailCfg` — no `e.vms` map, no `e.stateMu`.

**Issue to watch:** `copyBlock` is called from `engine.go` (Create at
line 319 via `copyRootfs`, and Stop at line 760 directly), `snapshot.go`
(Checkpoint/ResumeSnapshot), and `startFCJailed` (line 1695). All callers
are in the same package — moving to `fc.go` is fine. `copyRootfs` is a
one-line alias for `copyBlock` (line 1835-1837); move both together.

**Issue to watch:** `fcPut` and `fcPatch` already take `ctx` as their
first parameter (fixed in the snapshot-recovery work). The plan in
`PLAN-snapshot-recovery.md` threaded context through these functions.
Verify this is still the current signature before moving. Confirmed:
line 1856 shows `func fcPut(ctx context.Context, client *http.Client,
path, body string) error`.

### Step 1.2: `lifecycle.go` — State transitions

**Functions to move:**

```
Stop(ctx, id)                       line 690    (method on *Engine)
Pause(ctx, id)                      line 792    (method on *Engine)
Resume(ctx, id)                     line 813    (method on *Engine)
BalloonSet(ctx, id, amountMiB)      line 836    (method on *Engine)
MemSizeMib(id)                      line 853    (method on *Engine)
ThermalState(id)                    line 862    (method on *Engine)
Activity(ctx, id)                   line 873    (method on *Engine)
EnsureHot(ctx, id)                  line 889    (method on *Engine)
Start(ctx, id)                      line 919    (method on *Engine)
StartForce(ctx, id)                 line 924    (method on *Engine)
startVM(ctx, id, force)             line 928    (method on *Engine)
Destroy(ctx, id)                    line 1083   (method on *Engine)
```

**Why together:** These are all VM state transitions. When debugging
snapshot resume, you open `lifecycle.go` and `snapshot.go`. That's it.

**Issue to watch:** `Stop()` calls `verifySnapshotArtifacts()` at
line ~760 area. That helper is defined at line 1601. It's also called
from `snapshot.go` (Checkpoint). Move `verifySnapshotArtifacts` to
`fc.go` (it's a pure validation function on file paths, no VM state)
rather than `lifecycle.go`, so both `lifecycle.go` and `snapshot.go`
can call it without a conceptual dependency.

**Issue to watch:** `Stop()` calls `copyBlock()` (line 760) and
`fcPatch()`/`fcPut()` (for pause and snapshot creation). Both will be
in `fc.go` after Step 1.1. `Stop()` also calls `killFC()` (line 780
area). All cross-file references are within the same package — no
import issues.

**Issue to watch:** `startVM()` (line 928) is 155 lines. It handles
snapshot resume, agent reconnection, and status transitions. It calls
into `snapshot.go` for `ResumeSnapshot`. This is the most complex
function being moved — test it carefully after the move. It also
references `injectLoharIntoRootfs` (line 1577), which should be in
`fc.go` or `helpers.go`.

**Issue to watch:** `Destroy()` calls `killFC()`, `removeUserNetworkIfEmpty()`,
and accesses `e.vms` map under `e.stateMu`. The map access pattern is
`e.stateMu.Lock(); delete(e.vms, id); e.stateMu.Unlock()` — same as
other lifecycle methods. No special consideration needed.

### Step 1.3: `exec.go` — Command execution

**Functions to move:**

```
Exec(ctx, id, cmd)                  line 1261   (method on *Engine)
ExecStream(ctx, id, cmd, onEvent)   line 1280   (method on *Engine)
Shell(ctx, id)                      line 1331   (method on *Engine)
ShellSession(ctx, id)               line 1338   (method on *Engine)
ShellAttach(ctx, id, sessionID...)  line 1364   (method on *Engine)
ListeningPorts(ctx, id)             line 1381   (method on *Engine)
SessionList(ctx, id)                line 1422   (method on *Engine)
parseSSOutput(output)               line 1887   (package-level)
```

**Why together:** All agent-facing operations that run commands or
manage TTY sessions. `ListeningPorts` is here because it calls
the agent's `Exec` internally with `ss -tln` and then parses the
output with `parseSSOutput`. It's a query disguised as an exec.

**Issue to watch:** All of these call `e.getVM(id)` then access
`vm.Agent`. `getVM` stays in `engine.go`. No issue — same package.

**Issue to watch:** `parseSSOutput` is a pure function (string → []int)
with no dependencies. It's tested in `engine_test.go`. When we split
tests in Phase 6, the test moves to `exec_test.go`.

### Step 1.4: `files.go` — File operations + tunneling

**Functions to move:**

```
FileRead(ctx, id, path, w, opts)    line 1441   (method on *Engine)
FileWrite(ctx, id, path, mode...)   line 1458   (method on *Engine)
FileStat(ctx, id, path)             line 1475   (method on *Engine)
FileList(ctx, id, path)             line 1492   (method on *Engine)
Tunnel(ctx, id, port)               line 1402   (method on *Engine)
```

**Why together:** All agent-facing data transfer operations. Tunnel is
here rather than in exec.go because it's a data pipe (returns
`io.ReadWriteCloser`), not a command execution.

**Issue to watch:** These are the simplest methods in the file. Each
one calls `e.getVM(id)` → `vm.Agent.Method()`. No cross-dependencies
with other functions being moved. Safest step.

Note: `files.go` already exists as a test file
(`pkg/engine/firecracker/files_test.go` at 634 lines). The source file
`files.go` does not exist yet — this step creates it. No collision.

### Step 1.5: `helpers.go` — Recovery, state serialization, utilities

**Functions to move:**

```
VMState(id)                         line 1151   (method on *Engine)
RestoreVM(id, name, status, state)  line 1183   (method on *Engine)
List(ctx)                           line 1246   (method on *Engine)
Status(ctx, id)                     line 1136   (method on *Engine)
SaveImage(ctx, sandboxID, destPath) line 635    (method on *Engine)

stateStr(m, key)                    line 1511   (package-level)
stateInt64(m, key)                  line 1518   (package-level)
stateUint32(m, key)                 line 1532   (package-level)
stateBool(m, key)                   line 1546   (package-level)

generateID()                        line 1560   (package-level)
generateMAC()                       line 1566   (package-level)
injectLoharIntoRootfs(...)          line 1577   (package-level)
verifySnapshotArtifacts(...)        line 1601   (package-level)
```

**Why this naming:** The original plan called this `restore.go`. But
this file contains four distinct things: crash recovery
(`VMState`/`RestoreVM`), state serialization (`stateStr`/`stateInt64`),
ID generation (`generateID`/`generateMAC`), and rootfs preparation
(`injectLoharIntoRootfs`/`verifySnapshotArtifacts`). None of these are
purely about "restore." `helpers.go` is honest about what this file is:
the grab-bag of utilities that don't belong to a specific domain.

**Alternative:** Split further into `state.go` (VMState, RestoreVM,
Status, List, state* helpers) and `util.go` (generateID, generateMAC,
injectLohar, verifySnapshot). But this creates two 100-line files
where one 200-line file is fine. Split if either grows past 400 lines.

**Issue to watch:** `SaveImage` (line 635) is 55 lines and calls
`copyRootfs` (in `fc.go` after Step 1.1), accesses `vm.Agent.Exec`
(for `sync`), and calls `fcPatch` (for pause/resume). It's conceptually
an image operation, but it needs VM access (`e.getVM`, `e.stateMu`).
It fits in `helpers.go` alongside VMState and RestoreVM — they're all
"persistence-adjacent" operations on VM state.

**Issue to watch:** `RestoreVM` (line 1183) populates `e.vms` map by
deserializing state from a `map[string]interface{}`. It calls
`stateStr`, `stateInt64`, `stateUint32`, `stateBool` (all in this same
file). It also creates a `VM` struct — the `VM` type definition stays
in `engine.go`. No issue — same package.

### Step 1.6: `create.go` — Sandbox creation

**Functions to move:**

```
Create(ctx, spec)                   line 273    (method on *Engine)
```

**Why separate:** `Create()` is 362 lines — the longest single function
in the codebase. It handles rootfs copying, config drive creation,
network setup, volume attachment, FC boot sequence, and agent handshake.
Moving it to `create.go` leaves `engine.go` at ~180 lines: just type
definitions, constructor, and the handful of methods that define what
an Engine *is* rather than what it *does*.

**Issue to watch:** `Create` calls functions that will be in three
different files after the split:
- `fc.go`: `copyRootfs`, `injectLoharIntoRootfs` (wait —
  `injectLoharIntoRootfs` is in `helpers.go`, not `fc.go`), `startFC`,
  `waitForSocket`, `fcPut` ×10, `fcAPIClient`, `generateID`, `generateMAC`
- `helpers.go`: `generateID`, `generateMAC`, `injectLoharIntoRootfs`
- `engine.go`: `getOrCreateUserNetwork`
- `configdrive.go`: `buildConfigDrive` (already separate)
- `network.go`: `setupTapDevice` (already separate)

All same-package calls. No import issues. But `injectLoharIntoRootfs`
and `generateID`/`generateMAC` are called from both `Create` (in
`create.go`) and `RestoreVM`/`startVM` (in `lifecycle.go`/`helpers.go`).
Keep them in `helpers.go` — the shared utility file.

**Decision point resolved:** The original plan asked "Keep Create() in
engine.go or move to create.go?" — move it. 362 lines of procedural
setup logic does not belong in the file that defines the Engine type.

### What remains in `engine.go` (~180 lines):

```
RateLimitConfig struct + methods    lines 33-46
Config struct + jailed()            lines 48-63
Engine struct                       lines 65-126
New(cfg)                            lines 128-167
CleanupOrphanedTaps()               lines 169-181
Shutdown()                          lines 183-216
getVM(id)                           lines 218-228
getOrCreateUserNetwork(...)         lines 230-248
removeUserNetworkIfEmpty(...)       lines 250-270
```

These are the identity of the Engine: types, constructor, resource
management, and the `getVM` accessor that every other method calls.

### Final file map for `pkg/engine/firecracker/`:

```
engine.go       ~180   Engine struct, Config, VM, New(), getVM, network helpers, Shutdown
create.go       ~362   Create()
lifecycle.go    ~400   Stop, Start, Pause, Resume, EnsureHot, Destroy, Balloon, Thermal
exec.go         ~170   Exec, ExecStream, Shell*, SessionList, ListeningPorts
files.go        ~100   FileRead, FileWrite, FileStat, FileList, Tunnel
fc.go           ~250   startFC*, killFC, copyBlock, copyRootfs, fcAPI*, waitForSocket, validateSocketPath, verifySnapshotArtifacts
helpers.go      ~250   VMState, RestoreVM, Status, List, SaveImage, state* helpers, generateID/MAC, injectLohar
snapshot.go     ~458   Checkpoint, ResumeSnapshot (already separate)
network.go      ~346   (already separate)
configdrive.go  ~100   (already separate)
jail.go         ~40    (already separate)
ringbuffer.go   ~50    (already separate)
```

### Verification checklist for Phase 1:

After each step:
1. **Line-by-line diff** — verify moved functions are byte-identical to originals (Principle #1)
2. `go build ./...` — catches missing functions, type references
3. `go vet ./...` — catches signature mismatches
4. `go test ./pkg/engine/firecracker/...` — unit tests pass
5. Spot-check: `grep -rn "func.*Engine.*{" pkg/engine/firecracker/engine.go` — verify only identity methods remain

After Phase 1 is complete (all 6 steps committed):
6. `gh workflow run integration.yml` — integration tests on Pi cluster (not agni-01)

---

## Phase 2 — `pkg/server/routes.go` (3064 → ~200)

### Step 2.1: `sandbox_handlers.go` — Sandbox CRUD handlers

**Functions to move:**

```
handleSandboxes(w, r)               line 305    POST /sandboxes, GET /sandboxes
handleSandbox(w, r)                 line 673    GET/DELETE /sandboxes/:id, sub-routing switch
handleSandboxStop(w, r, id)         line 807
handleSandboxStart(w, r, id)        line 826
```

**Types to move:**

None. The only type used here is `store.Sandbox` (in store package).

**Issue to watch:** `handleSandbox` (line 673) is a 134-line function
that acts as a manual router — it parses the URL path and dispatches to
sub-handlers (exec, files, stop, start, ws, proxy, publish, sessions,
checkpoint, save-image). After the split, this function dispatches to
handlers in *other files* (`exec_handlers.go`, `file_handlers.go`,
etc.). This works fine in Go (same package), but the function itself
is the central dispatch point. It must stay wherever the route table
lives, OR the sub-routes should be registered directly in `routes()`.

**Recommendation:** Keep `handleSandbox` in `sandbox_handlers.go`. It's
the logical entry point for `/sandboxes/:id/*` and the switch statement
is a routing concern, not a handler concern. The alternative (registering
each sub-route in `routes()`) would require Go 1.22+ `net/http` routing
or a third-party router, neither of which we use.

**Issue to watch:** `handleSandboxes` POST (the create handler, starting
at line 305) is 367 lines — the longest handler. It validates the
request, resolves images/templates/volumes, builds the engine spec, calls
`engine.Create`, creates store records, and attaches volumes. Moving it
intact is correct (principle #1: don't rewrite). But flag it for future
extraction into `handleSandboxCreate` as a follow-up.

### Step 2.2: `exec_handlers.go` — Exec and shell handlers

**Functions to move:**

```
handleSandboxExec(w, r, id)         line 857
handleSandboxExecStream(w, r, sb, req) line 905
handleSandboxWS(w, r, id)           line 957    WebSocket shell
handleSandboxSessions(w, r, id)     line 2307
```

**Types to move:**

```
execReq struct                      line 852
```

**Issue to watch:** `handleSandboxWS` uses the `upgrader` variable
(line 947: `var upgrader = websocket.Upgrader{...}`). This is a
package-level variable shared with `proxyWebSocket` (which also
upgrades WebSocket connections for the proxy handler). Keep `upgrader`
in `routes.go` (the shared helpers file) since it's used by both
`exec_handlers.go` and `proxy_handlers.go`.

**Issue to watch:** `handleSandboxExecStream` uses Server-Sent Events
(SSE). It calls `w.(http.Flusher)` and writes raw bytes. No type
dependencies beyond the standard library.

### Step 2.3: `file_handlers.go` — File operation handlers

**Functions to move:**

```
handleSandboxFiles(w, r, id)        line 2208
```

**Why its own file:** This is only one function (~100 lines), but it
handles GET (read), PUT (write), and DELETE (not implemented, returns
405) for file operations. It interacts with the engine's FileRead,
FileWrite, FileStat, and FileList methods. Clean separation from exec
and proxy concerns.

**Issue to watch:** `handleSandboxFiles` GET reads the `path` query
parameter and streams file content. It sets `Content-Type` based on
file extension. No shared state with other handlers.

### Step 2.4: `proxy_handlers.go` — Per-sandbox proxy handlers

**Functions to move:**

```
handleSandboxProxyRoute(...)        line 1940
handleProxyHTTP(...)                line 2040
handleProxyWS(...)                  line 2192
proxyWebSocket(...)                 line 2109   (package-level)
idleCopyWithDeadline(...)           line 2089   (package-level)
schemeOf(r)                         line 2068   (package-level)
handleSandboxPorts(w, r, id)        line 1227
handleAllPorts(w, r)                line 1256
```

**Types to move:**

```
tunnelTransport struct              line 1980
tunnelBody struct                   line 2019
deadlineConn interface              line 2081
```

**Issue to watch:** `proxyWebSocket` (line 2109) uses the same
`upgrader` variable as `handleSandboxWS`. As noted in Step 2.2, keep
`upgrader` in `routes.go`.

**Issue to watch:** `public_proxy.go` already exists at 386 lines. It
handles alias-based unauthenticated proxying. The proxy handlers being
moved here handle per-sandbox authenticated proxying. These are distinct:
different auth models, different URL patterns, different request flow.
Keep them separate.

**Issue to watch:** `tunnelTransport` and `tunnelBody` implement
`http.RoundTripper` and `io.ReadCloser` respectively, creating an HTTP
transport over the engine's `Tunnel()` method. These types are only used
by `handleProxyHTTP`. They move together as a unit.

### Step 2.5: `publish_handlers.go` — Publish/preview URL handlers

**Functions to move:**

```
handleSandboxPublish(w, r, id, sub) line 2888
handlePublish(w, r, sandboxID)      line 2915
handleListPublishRules(w, r, sandboxID) line 2976
handleUnpublish(w, r, sandboxID, port) line 2997
validateAlias(alias)                line 2845   (package-level)
generateAlias(sandboxName)          line 2857   (package-level)
generateUniqueAlias(st, sandboxName) line 2877  (package-level)
```

**Variables to move:**

```
aliasRegex                          line 2837
reservedAliases                     line 2839
```

**Issue to watch:** `generateUniqueAlias` takes a `*store.Store`
parameter, not a `*Server` receiver. It's a pure function that queries
the store. No dependency on server state beyond the store.

### Step 2.6: `admin_handlers.go` — Templates, secrets, images, snapshots, tasks

**Functions to move:**

```
handleTemplates(w, r)               line 218
handleTemplate(w, r)                line 259
handleSecrets(w, r)                 line 1159
handleSecret(w, r)                  line 1200
handleImages(w, r)                  line 1755
handleImage(w, r)                   line 1774
handleImagePull(w, r, user)         line 2541
handleImageImport(w, r, user)       line 2657
handleSandboxSaveImage(w, r, id)    line 2725
handleSnapshots(w, r)               line 1826
handleSnapshot(w, r)                line 1845
handleSnapshotResume(w, r, user, snapName) line 2432
handleSandboxCheckpoint(w, r, id)   line 2339
handleTask(w, r)                    line 1892
encryptSecret(plaintext)            line 2808   (method on *Server)
decryptSecret(ciphertext)           line 2822   (method on *Server)
```

**Issue to watch:** This is a large collection (16 functions). The
alternative is splitting further: `template_handlers.go`,
`secret_handlers.go`, `image_handlers.go`, `snapshot_handlers.go`,
`task_handlers.go`. But most of these are 30-50 line handlers with no
interdependencies. Five files of 40 lines each provides no navigability
benefit over one file of 500 lines with clear section comments.

**Exception:** If image handling grows (it already has `handleImagePull`
at 116 lines and `handleImageImport` at 68 lines plus
`handleSandboxSaveImage` at 163 lines), extract `image_handlers.go` as a
follow-up. For now, keep them grouped.

**Issue to watch:** `encryptSecret` and `decryptSecret` are methods on
`*Server` (they access `s.dataDir` for the age key path). They move
with the secret handlers that call them. If other handlers ever need
encryption, they'd import from the same package — no issue.

**Issue to watch:** `handleSnapshotResume` (line 2432) is 109 lines
and calls `engine.Create` indirectly (it creates a new sandbox from a
snapshot). It also creates store records, attaches volumes, and
launches a background goroutine for the resume. This is the most complex
handler being moved — verify all store and engine calls compile after
the move.

### Step 2.7: `volume_handlers.go` — Volume handlers

**Functions to move:**

```
handlePersistentVolumes(w, r)       line 1293
handlePersistentVolume(w, r)        line 1370
handleVolumeBackups(w, r, user, ...) line 1440
handleVolumeResize(w, r, user, name) line 1616
handleVolumeSnapshot(w, r, user, srcName) line 1666
volumeIsClean(path)                 line 1726   (package-level)
createVolumeFile(path, sizeMB)      line 1735   (package-level)
```

**Issue to watch:** `handlePersistentVolume` (line 1370) is another
manual sub-router (like `handleSandbox`). It parses the URL to dispatch
to `handleVolumeResize`, `handleVolumeSnapshot`, and
`handleVolumeBackups`. This dispatch logic moves with the volume
handlers.

**Issue to watch:** `volumeIsClean` (line 1726) shells out to
`e2fsck -fn <path>` to check filesystem integrity. `createVolumeFile`
shells out to `dd` and `mkfs.ext4`. These are host-level operations
that only volume handlers use. Move them together.

### What remains in `routes.go` (~200 lines):

```
routes()                            line 105    the route table
handleMetrics(w, r)                 line 124    metrics endpoint
handleHealth(w, r)                  line 207    health check
saveVMState(sandboxID, engineID)    line 35     shared helper (called by sandbox, thermal, snapshot handlers)
errRespInternal(...)                line 2798   shared error helper
strOrEmpty, intOrZero, etc.         lines 62-100 JSON parsing helpers
genID()                             line 3020   shared ID generator
isValidName(name)                   line 3028   shared validation
isValidMountPath(mount)             line 3034   shared validation
upgrader (websocket.Upgrader)       line 947    shared by exec + proxy handlers
```

**Issue to watch:** `saveVMState` is called from `handleSandboxStop`,
`handleSandboxCheckpoint`, and `server.go`'s thermal cycle. It reads
engine state via `engine.VMState()` and writes to the store via
`store.SaveFirecrackerState()`. It must stay in a shared location.
`routes.go` is correct — it's the shared helpers file.

**Issue to watch:** `errRespInternal` logs the error with `slog.Error`
and writes an HTTP 500 response. It's called from virtually every
handler. Must stay in a shared location.

### Final file map for `pkg/server/`:

```
server.go           ~800   Server struct, middleware, ServeHTTP, thermal, backup (already exists)
routes.go           ~200   Route table, health, metrics, shared helpers (saveVMState, errRespInternal, JSON helpers, upgrader)
sandbox_handlers.go ~500   handleSandboxes, handleSandbox, handleSandboxStop, handleSandboxStart
exec_handlers.go    ~300   handleSandboxExec, handleSandboxExecStream, handleSandboxWS, handleSandboxSessions
file_handlers.go    ~100   handleSandboxFiles
proxy_handlers.go   ~350   handleSandboxProxyRoute, handleProxyHTTP, handleProxyWS, tunnel types, port handlers
publish_handlers.go ~200   handleSandboxPublish, handlePublish, handleListPublishRules, handleUnpublish, alias helpers
admin_handlers.go   ~500   Templates, secrets, images, snapshots, tasks, encrypt/decrypt
volume_handlers.go  ~400   Volume CRUD, backup, restore, resize, snapshot, volumeIsClean, createVolumeFile
public_proxy.go     ~386   (already exists — alias-based public proxy)
ratelimit.go        ~exists (already separate)
```

### Verification checklist for Phase 2:

After each step:
1. **Line-by-line diff** — verify moved functions are byte-identical to originals (Principle #1)
2. `go build ./...`
3. `go test ./pkg/server/...`

After Phase 2 is complete (all 7 steps committed):
4. `gh workflow run integration.yml` — integration tests on Pi cluster

---

## Phase 3 — `pkg/store/store.go` (1683 → ~300)

Split by entity. Each file has the type definition and all SQL methods
for that entity.

### Shared infrastructure that stays in `store.go`:

```
Store struct                        line 161
const schema (all migrations)       lines 165-252  (~88 lines of DDL)
New(dbPath)                         lines 253-387  (constructor + all ALTER TABLE migrations)
Close()                             line 388
scanner interface                   line 542
```

All migration SQL stays in `store.go`. It's the one place that defines
the schema. Entity files only contain runtime queries. This is deliberate
— when debugging a schema issue, you look at one file.

**Issue to watch:** The `scanner` interface (line 542) is used by
`scanTemplate`, `scanUser`, and `scanSandbox`. It must stay in `store.go`
or move to a new `scan.go` since multiple entity files need it. Keep it in
`store.go` — it's 4 lines and conceptually part of the store's
infrastructure.

### Step 3.1: `user.go` (~100 lines)

**Types to move:**
```
User struct                         line 146
```

**Functions to move:**
```
CreateUser(u User)                  line 393
scanUser(s scanner)                 line 404    (package-level, uses scanner interface)
GetUserByKeyHash(hash)              line 416
GetUser(id)                         line 422
GetUserByName(name)                 line 1339
ListUsers()                         line 428
DeleteUser(id)                      line 446
NextSubnetIndex()                   line 467
RotateUserKey(id, newKeyHash)       line 477
```

**Issue to watch:** `scanUser` takes a `scanner` interface parameter.
The `scanner` interface stays in `store.go`. This works because
`scanner` is unexported but `user.go` is in the same package. Go's
visibility rules are per-package, not per-file.

### Step 3.2: `sandbox.go` (~150 lines)

**Types to move:**
```
Sandbox struct                      line 123
```

**Functions to move:**
```
CreateSandbox(sb Sandbox)           line 569
GetSandbox(userID, idOrName)        line 586
GetSandboxByID(id)                  line 598
GetSandboxByEngineID(engineID)      line 604
ListSandboxes(userID)               line 610
ListAllSandboxes()                  line 628
CountUserSandboxes(userID)          line 646
UpdateSandboxStatus(id, status)     line 652
UpdateSandboxEngine(id, engineID, ip) line 657
UpdateSandboxKeepHot(id, keepHot)   line 663
StopSandbox(id)                     line 672
DeleteSandbox(userID, id)           line 679
DeleteSandboxByID(id)               line 692
scanSandbox(s scanner)              line 704    (package-level, uses scanner interface)
SaveFirecrackerState(id, st)        line 928
LoadFirecrackerState(id)            line 948
```

**Types to also move:**
```
FirecrackerState struct             line 910
```

**Issue to watch:** `FirecrackerState` is conceptually sandbox-specific
(it stores per-sandbox engine state). Grouping it with the Sandbox
entity is correct.

### Step 3.3: `template.go` (~80 lines)

**Types to move:**
```
TemplateMountSpec struct             line 14
Template struct                      line 22
```

**Functions to move:**
```
CreateTemplate(t Template)          line 491
GetTemplate(id)                     line 507
ListTemplates()                     line 512
DeleteTemplate(id)                  line 529
scanTemplate(s scanner)             line 546    (package-level)
```

### Step 3.4: `secret.go` (~80 lines)

**Types to move:**
```
SecretRecord struct                  line 138
```

**Functions to move:**
```
SetSecret(userID, name, encrypted)  line 724
GetSecretValue(userID, name)        line 737
ListUserSecrets(userID)             line 747
ListAllSecrets()                    line 757
scanSecretRecords(rows)             line 766    (package-level)
GetSecret(userID, name)             line 782
DeleteSecret(userID, name)          line 895
```

**Issue to watch:** `DeleteSecret` (line 895) is physically located
after the volume methods in the file (lines 798-890). It's not adjacent
to the other secret methods. This is a sign that `store.go` grew
organically without grouping. The move corrects this.

### Step 3.5: `volume.go` (~300 lines)

**Types to move:**
```
Volume struct                        line 38
SandboxVolume struct                 line 44
PersistentVolume struct              line 52
VolumeBackup struct                  line 64
VolumeAttachment struct              line 75
```

**Functions to move:**
```
CreateVolume(name)                  line 798
GetVolume(name)                     line 807
ListVolumes()                       line 818
DeleteVolume(name)                  line 836
AttachVolume(sandboxID, volumeName, target, readonly) line 854
GetSandboxVolumes(sandboxID)        line 867
DetachVolumes(sandboxID)            line 890
CreatePersistentVolume(v)           line 974
GetPersistentVolume(userID, name)   line 984
ListPersistentVolumes(userID)       line 1012
DeletePersistentVolume(userID, name) line 1032
AttachPersistentVolume(userID, name, sandboxID, mount, readOnly) line 1057
DetachPersistentVolume(userID, name, sandboxID) line 1104
DetachAllPersistentVolumesForSandbox(sandboxID) line 1118
AttachedPersistentVolumesForSandbox(sandboxID) line 1126
DetachOrphanedPersistentVolumes()   line 1166
UpdatePersistentVolumeSize(userID, name, sizeMB) line 1182
UpdatePersistentVolumeStatus(userID, name, status) line 1196
UserVolumeStorageUsed(userID)       line 1203
CreateVolumeBackup(b)               line 1616
ListVolumeBackups(userID, volumeName) line 1625
GetVolumeBackup(userID, backupID)   line 1646
DeleteVolumeBackup(userID, backupID) line 1659
OldestVolumeBackups(userID, volumeName, keepCount) line 1665
```

This is the largest entity file at ~300 lines. It contains two volume
systems: the old v0.2 `Volume`/`SandboxVolume` system and the v0.3
`PersistentVolume` system (see Issues section below for analysis).

### Step 3.6: `image.go` (~120 lines)

**Types to move:**
```
ImageRecord struct                   line 82
```

**Functions to move:**
```
CreateImage(img)                    line 1217
GetImage(userID, name)              line 1228
ListImages(userID)                  line 1255
DeleteImage(userID, name)           line 1280
ShareImage(imageID, userID)         line 1295
UnshareImage(imageID, userID)       line 1301
ListImageShares(imageID)            line 1307
GetImageByName(name)                line 1325
```

### Step 3.7: `snapshot.go` (~60 lines)

**Types to move:**
```
SnapshotRecord struct                line 95
```

**Functions to move:**
```
CreateSnapshot(snap)                line 1358
GetSnapshot(userID, name)           line 1371
ListSnapshots(userID)               line 1388
DeleteSnapshot(userID, name)        line 1411
```

**Issue to watch:** `snapshot.go` already exists in
`pkg/engine/firecracker/snapshot.go`. This is a different package
(`pkg/store/snapshot.go`). No naming collision — Go resolves by
package path, not filename.

### Step 3.8: `task.go` (~60 lines)

**Types to move:**
```
TaskRecord struct                    line 110
```

**Functions to move:**
```
CreateTask(t)                       line 1428
GetTask(id)                         line 1438
UpdateTaskProgress(id, progress)    line 1457
CompleteTask(id, resultJSON)        line 1463
FailTask(id, errMsg)                line 1471
CleanupOldTasks(maxAge)             line 1479
```

### Step 3.9: `publish.go` (~100 lines)

**Types to move:**
```
PublishRule struct                    line 1494
```

**Functions to move:**
```
CreatePublishRule(rule)             line 1505
GetPublishRuleByAlias(alias)        line 1524
ListPublishRules(sandboxID)         line 1537
ListUserPublishRules(userID)        line 1558
DeletePublishRule(userID, sandboxID, port) line 1578
DeletePublishRulesForSandbox(sandboxID) line 1593
CleanupOrphanedPublishRules()       line 1602
```

### Verification checklist for Phase 3:

After each step:
1. **Line-by-line diff** — verify moved functions are byte-identical to originals (Principle #1)
2. `go build ./...`
3. `go test ./pkg/store/...`
4. Verify: every type referenced in `pkg/server/` and
   `pkg/engine/firecracker/` still resolves (e.g. `store.Sandbox`,
   `store.User`, `store.PublishRule`)

---

## Phase 4 — `cmd/bhatti/cli.go` (2683 → ~300)

### The `init()` problem

`cli.go` has 11 `init()` functions that register commands and flags.
In Go, multiple `init()` functions in the same package execute in
source-file order (alphabetical by filename), then top-to-bottom within
each file. The registration order doesn't matter for Cobra — commands
are added to the tree, not to a sequential list. But each `init()` must
move with the command variable it registers.

**Verification after every step:** Run `go run ./cmd/bhatti/ --help`
and count the commands. If a command disappeared, an `init()` was missed.

### Step 4.1: `sandbox_cmd.go` (~400 lines)

**Variables/functions to move:**
```
createCmd                           line 549
editCmd                             line 671
stopCmd                             line 727
startCmd                            line 758
inspectCmd                          line 787
listCmd                             line 827
destroyCmd                          line 896
init() at line 653                  (registers createCmd flags)
init() at line 931                  (registers destroyCmd)
```

**Issue to watch:** `createCmd` (line 549) is 104 lines and references
`parseEnvFlag`, `resolveID`, `apiJSON`, `addToCompletionCache`. These
are shared helpers that stay in `cli.go`. Same package — no import issue.

**Issue to watch:** `editCmd` (line 671) uses `resolveID` and `apiJSON`.
Both shared.

### Step 4.2: `exec_cmd.go` (~200 lines)

**Variables/functions to move:**
```
execCmd                             line 937
shellCmd                            line 999
psCmd                               line 1182
init() at line 993                  (registers execCmd flags)
```

**Issue to watch:** `shellCmd` (line 999) is 183 lines. It handles
WebSocket connections, terminal raw mode, signal handling (SIGWINCH),
and session management. It references `apiRequest`, `setupTiming`,
`printTiming`, and the `gorilla/websocket` library. All of these are
either shared helpers (stay in `cli.go`) or direct imports.

### Step 4.3: `file_cmd.go` (~120 lines)

**Variables/functions to move:**
```
fileCmd                             line 1222
fileReadCmd                         line 1230
fileWriteCmd                        line 1258
fileLSCmd                           line 1295
init() at line 1334                 (registers file subcommands)
```

### Step 4.4: `secret_cmd.go` (~80 lines)

**Variables/functions to move:**
```
secretCmd                           line 1342
secretSetCmd                        line 1352
secretListCmd                       line 1370
secretDeleteCmd                     line 1394
init() at line 1410                 (registers secret subcommands)
```

### Step 4.5: `user_cmd.go` (~200 lines)

**Variables/functions to move:**
```
userCmd                             line 1418
userCreateCmd                       line 1429
userListCmd                         line 1490
userDeleteCmd                       line 1517
userRotateKeyCmd                    line 1548
init() at line 1584                 (registers user subcommands)
openLocalStore()                    line 1609   (only called by user commands)
generateAPIKey()                    line 1642   (only called by user commands)
sha256HexCLI(s)                     line 1648   (only called by user commands)
```

**Issue to watch:** `openLocalStore()`, `generateAPIKey()`, and
`sha256HexCLI()` are only called from user commands (they open the
SQLite DB directly on the server). Verify with:
```bash
grep -n "openLocalStore\|generateAPIKey\|sha256HexCLI" cmd/bhatti/cli.go
```
If any other command calls them, keep them in `cli.go`.

### Step 4.6: `setup_cmd.go` (~100 lines)

**Variables/functions to move:**
```
setupCmd                            line 1655
```

The `init()` for setupCmd is inside the function definition (not a
separate `init()`). It's registered by the root `init()` at line 59.
Check whether `rootCmd.AddCommand(setupCmd)` is in the root `init()`
or in a separate one.

### Step 4.7: `volume_cmd.go` (~200 lines)

**Variables/functions to move:**
```
volumeCmd                           line 1732
volumeCreateCmd                     line 1743
volumeListCmd                       line 1778
volumeDeleteCmd                     line 1806
volumeResizeCmd                     line 1825
volumeBackupCmd                     line 1865
volumeBackupListCmd                 line 1892
volumeRestoreCmd                    line 1925
volumeBackupDeleteCmd               line 1952
init() at line 1848                 (registers volume subcommands)
init() at line 1971                 (registers backup subcommands)
```

### Step 4.8: `image_cmd.go` (~200 lines)

**Variables/functions to move:**
```
imageCmd                            line 1977
imageListCmd                        line 1992
imageDeleteCmd                      line 2026
imagePullCmd                        line 2045
imageSaveCmd                        line 2129
imageImportCmd                      line 2166
imageShareCmd                       line 2323
imageUnshareCmd                     line 2370
deriveImageName(ref)                line 2297   (only called by image commands)
init() at line 2303                 (registers image subcommands)
```

### Step 4.9: `snapshot_cmd.go` (~130 lines)

**Variables/functions to move:**
```
snapshotCmd                         line 2405
snapshotCreateCmd                   line 2415
snapshotListCmd                     line 2451
snapshotResumeCmd                   line 2479
snapshotDeleteCmd                   line 2510
init() at line 2529                 (registers snapshot subcommands)
```

### Step 4.10: `misc_cmd.go` (~140 lines)

**Variables/functions to move:**
```
updateCmd                           line 2542
versionCmd                          line 2565
publishCmd                          line 2602
unpublishCmd                        line 2635
completionCmd                       line 2666
```

These don't have their own `init()` — they're registered in the root
`init()` at line 59. Verify which commands are registered there and
create a new `init()` in `misc_cmd.go` for these.

### What remains in `cli.go` (~300 lines):

```
rootCmd                             line 41     root command definition
init() at line 59                   root-level command registration + global flags
runCLI()                            line 128    entry point
loadConfig(cmd)                     line 140    config loading
apiRequest(method, path, body)      line 164    HTTP client helper
apiJSON(method, path, body, result) line 181    JSON API helper
checkServerVersion(resp)            line 213    version compatibility check
compareVersions(a, b)               line 236    semver comparison
confirmAction(cmd, msg)             line 263    destructive op confirmation
isJSON(cmd)                         line 280    --json flag check
outputJSON(v)                       line 285    JSON output helper
requestTiming struct + methods      lines 296-405 timing infrastructure
httpClient()                        line 407    HTTP client factory
setupTiming(cmd)                    line 419    timing setup
printTiming()                       line 427    timing output
resolveID(nameOrID)                 line 436    name→ID resolution
parseEnvFlag(s)                     line 463    --env flag parser
addToCompletionCache(name)          line 481    completion cache writer
removeFromCompletionCache(name)     line 502    completion cache reader
completeSandboxNames(...)           line 523    tab-completion function
completionCachePath()               line 539    cache file path
```

**Issue to watch:** The root `init()` at line 59 registers many
commands via `rootCmd.AddCommand(...)`. After the split, each new file
should have its own `init()` that registers its commands. The root
`init()` should shrink to only global flag registration and command
groups. Verify with `diff` that every `AddCommand` call has a home.

### Verification checklist for Phase 4:

After each step:
1. **Line-by-line diff** — verify moved functions are byte-identical to originals (Principle #1)
2. `go build ./cmd/bhatti/`
3. `go run ./cmd/bhatti/ --help` — count commands, compare with before
4. `go run ./cmd/bhatti/ <moved-command> --help` — verify flags present
5. `go test ./cmd/bhatti/...`

---

## Phase 5 — Cleanup pass

After all moves are done and tests pass, one final pass.

### 5.1 Dead code removal

```bash
go vet ./...
# If staticcheck is installed:
staticcheck ./...
# Manual hunt for unexported functions never called:
grep -rn "^func [a-z]" pkg/store/*.go | while read line; do
  func=$(echo "$line" | grep -o 'func [a-z][a-zA-Z0-9]*' | awk '{print $2}')
  file=$(echo "$line" | cut -d: -f1)
  count=$(grep -rn "$func" pkg/store/*.go | grep -v "_test.go" | wc -l)
  if [ "$count" -le 1 ]; then
    echo "UNUSED: $func in $file"
  fi
done
```

Look for:
- Functions that were duplicated during the Docker→Firecracker transition
- Helpers that were used by deleted features
- Commented-out code blocks
- The old `Volume`/`SandboxVolume` system (see Issues #4 below)

### 5.2 Import cleanup

Each new file will have its own import block. Run `goimports` on
every new file to clean up unused imports and sort them.

```bash
goimports -w pkg/engine/firecracker/*.go
goimports -w pkg/server/*.go
goimports -w pkg/store/*.go
goimports -w cmd/bhatti/*.go
```

### 5.3 Verify no behavior change

```bash
go build ./...
go test ./...
# Integration tests on the Pi cluster (not agni-01 — that's prod):
gh workflow run integration.yml
gh run watch
```

---

## Phase 6 — Test file splitting

Deferred from the source moves. Now that the source files are stable,
split test files to match.

### 6.1 `pkg/engine/firecracker/engine_test.go` (552 lines)

Examine each test function. If it only tests behavior from `exec.go`
(e.g. `TestParseSSOutput`), move to `exec_test.go`. If it tests
Create behavior, move to `create_test.go`. Tests that span multiple
domains (e.g. create + exec + destroy) stay in `engine_test.go` or
move to `integration_test.go`.

**Issue to watch:** Test helper functions (`testEngine()`, `testSpec()`,
`execWithTimeout()`) are used by many test files. Keep them in
`engine_test.go` or extract to `test_helpers_test.go`. In Go, test
helpers in `*_test.go` files are visible to all test files in the
same package.

### 6.2 `pkg/server/server_test.go`

Same approach. Tests for sandbox handlers → `sandbox_handlers_test.go`.
Tests for exec handlers → `exec_handlers_test.go`. Shared test setup
(mock engine, test server creation) stays in `server_test.go` or
`test_helpers_test.go`.

### 6.3 `pkg/store/store_test.go`

Split by entity, matching the source split.

### 6.4 `cmd/bhatti/cli_test.go`

Split by command group, matching the source split.

---

## Issues found during audit

### 1. `routes.go` has two separate JSON helper sets

Lines 62-100 define `strOrEmpty`, `intOrZero`, `floatOrZero`,
`boolOrFalse`. `engine.go` lines 1511-1546 define `stateStr`,
`stateInt64`, `stateUint32`, `stateBool`. These do the same thing
(extract typed values from `map[string]interface{}`) with different
names and slightly different signatures.

**After the split:** `engine.go`'s helpers land in `helpers.go` (Phase 1
Step 1.5). `routes.go`'s helpers stay in `routes.go`. The duplication
is now visible across two files instead of hidden in two 2000-line files.

**Recommendation:** Do NOT deduplicate during the split (principle #1).
File a follow-up to unify them into a `pkg/maputil` package or pick one
naming convention. The engine helpers work on `map[string]interface{}`
with typed returns; the routes helpers work on `map[string]interface{}`
with typed returns. They're functionally identical.

### 2. `handleSandbox` is a 134-line routing function

`handleSandbox` (routes.go:673) parses the URL path to figure out which
sub-handler to call. After splitting into domain files, this function
dispatches across file boundaries. This is fine in Go (same package) but
it means a reader of `exec_handlers.go` won't find the entry point for
exec handling — it's the `case "exec":` inside `handleSandbox` in
`sandbox_handlers.go`.

**Recommendation:** Add a comment at the top of each handler file:
```go
// exec_handlers.go — Exec and shell handlers.
// Entry point: handleSandbox() in sandbox_handlers.go dispatches here.
```
Do not refactor the routing mechanism during this split.

### 3. `SaveImage` is in engine.go but is conceptually an image operation

`SaveImage` (engine.go:635) copies the rootfs of a running sandbox to a
new path. It accesses VM state (needs the rootfs path), calls the agent
(to `sync`), and calls `fcPatch` (to pause/resume). It's called only
from `handleSandboxSaveImage` in routes.go.

**Resolution:** Move to `helpers.go` alongside VMState and RestoreVM.
These are all "reach into VM state for persistence purposes" operations.
It's not a perfect fit, but it's better than leaving it in `engine.go`
between `Create` and `Stop`, which is where it currently sits.

### 4. Store has Volume AND PersistentVolume — both are live

The plan originally said "Check if the old Volume system is still used
anywhere." I checked:

**Old system (`Volume`, `SandboxVolume`):**
- `CreateVolume` called from `routes.go:405` (sandbox create handler)
- `AttachVolume` called from `routes.go:658` (sandbox create handler)
- `DetachVolumes` called from `routes.go:754` (sandbox destroy handler)
- `GetSandboxVolumes` called from sandbox enrichment paths

**New system (`PersistentVolume`):**
- Full CRUD via `handlePersistentVolumes` / `handlePersistentVolume`
- Backup/restore via `handleVolumeBackups`

Both are actively used. The old `Volume` system tracks named volumes
that are ephemeral (created per-sandbox, destroyed with sandbox). The
new `PersistentVolume` system tracks user-owned volumes that survive
sandbox destruction.

**The naming is the problem.** `Volume` vs `PersistentVolume` makes it
seem like two implementations of the same concept. They're not — they're
two different features. After the split, both land in `volume.go` in
the store. Add a comment block explaining the distinction:

```go
// volume.go — Two volume systems coexist:
//
// 1. Volume / SandboxVolume (v0.2): Ephemeral named volumes. Created
//    during sandbox creation, attached to one sandbox, destroyed with it.
//    Used for --volume flags that aren't persistent volumes.
//
// 2. PersistentVolume / VolumeBackup (v0.3): User-owned volumes that
//    survive sandbox destruction. Created via `bhatti volume create`,
//    attached/detached independently, support backup/restore.
```

**Do not merge or deprecate during this split.** That's a behavioral
change, not a mechanical move.

### 5. `copyBlock` silently discards stderr

`copyBlock` (engine.go:1831) runs `cp --reflink=auto --sparse=always`
but only checks the error return, not stderr:

```go
func copyBlock(src, dst string) error {
    return exec.Command("cp", "--reflink=auto", "--sparse=always", src, dst).Run()
}
```

When `cp` fails (permissions, disk full, ENOSPC), the error is
`exit status 1` with no context. The reliability audit
(docs/archive/RELIABILITY-AUDIT.md, issue #5) flagged this.

**Recommendation:** Do NOT fix during the split (principle #1). File a
follow-up. The fix is trivial (switch `.Run()` to `.CombinedOutput()`
and wrap the error) but it's a code change, not a move.

### 6. Multiple `init()` functions in cli.go risk silent command loss

`cli.go` has 11 `init()` functions. When splitting into files, each
`init()` must move with its command. If an `init()` is accidentally
left in `cli.go` after its command variable has moved to another file,
the code will still compile (the `init()` references a variable that
exists in the package) but the registration may happen in wrong order
or the variable may be zero-valued if it depends on another `init()`.

**Mitigation:** After each Phase 4 step, run `go run ./cmd/bhatti/ --help`
and manually verify the moved commands appear. Automate this:

```bash
BEFORE=$(go run ./cmd/bhatti/ --help 2>&1 | grep -c "^  ")
# ... do the move ...
AFTER=$(go run ./cmd/bhatti/ --help 2>&1 | grep -c "^  ")
if [ "$BEFORE" != "$AFTER" ]; then
    echo "COMMAND COUNT CHANGED: $BEFORE → $AFTER"
    exit 1
fi
```

### 7. `handlePersistentVolume` has nested sub-routing (same pattern as `handleSandbox`)

`handlePersistentVolume` (routes.go:1370) parses the URL to dispatch to
resize, snapshot, and backup sub-handlers. After moving to
`volume_handlers.go`, this becomes the volume-domain equivalent of
`handleSandbox` — a local router. This is fine, but document it:

```go
// volume_handlers.go — Volume management handlers.
// Entry point: routes() registers /volumes and /volumes/ which dispatch here.
// handlePersistentVolume sub-routes to resize, snapshot, backup handlers.
```

### 8. `server.go` is 800 lines and not being split

`server.go` contains the Server struct, middleware, thermal manager,
backup scheduler, and `ensureHot`. It's not in the "four big files" list
because it's already reasonably organized (one concept: server lifecycle
and background tasks). But it's worth noting that `runThermalCycle`
(line 454, ~144 lines) and `SnapshotAll` (line 348, ~75 lines) are
substantial. If `server.go` grows past 1000 lines, split thermal
management into `thermal.go`.

### 9. Store migrations (~225 lines) will dominate `store.go` after the split

After moving all entity methods out, `store.go` will be ~300 lines:
~225 of schema/migrations + ~75 of Store struct, New(), Close(). That's
75% migrations. This is fine — it makes `store.go` the single source of
truth for the database schema. But if more migrations are added (likely
in every version), consider extracting to `migrations.go`. Not now.

---

## Execution order

```
Phase 1: engine.go split — 6 steps, 6 commits
  Step 1.1: fc.go
  Step 1.2: lifecycle.go
  Step 1.3: exec.go
  Step 1.4: files.go
  Step 1.5: helpers.go
  Step 1.6: create.go
  → Run integration tests on agni-01

Phase 2: routes.go split — 7 steps, 7 commits
  Step 2.1: sandbox_handlers.go
  Step 2.2: exec_handlers.go
  Step 2.3: file_handlers.go
  Step 2.4: proxy_handlers.go
  Step 2.5: publish_handlers.go
  Step 2.6: admin_handlers.go
  Step 2.7: volume_handlers.go
  → Run server tests

Phase 3: store.go split — 9 steps, 9 commits
  Steps 3.1-3.9: one per entity
  → Run store tests

Phase 4: cli.go split — 10 steps, 10 commits
  Steps 4.1-4.10: one per command group
  → Run CLI tests, verify --help output

Phase 5: Cleanup — 1-2 commits
  Dead code, imports, final test run

Phase 6: Test file splitting — 4 commits
  One per package (engine, server, store, cmd)
```

Total: ~37 commits.

### Dependency graph

```
Phase 1 (engine.go) ─── independent ─── Phase 3 (store.go)
     │                                        │
     ↓                                        ↓
Phase 2 (routes.go) ─── depends on ──── Phase 4 (cli.go)
                    neither Phase 1             │
                    nor Phase 3                 ↓
                                         Phase 5 (cleanup)
                                               │
                                               ↓
                                         Phase 6 (test split)
```

Phases 1 and 3 can run in parallel (different packages, no overlap).
Phase 2 and Phase 4 can also run in parallel. Phase 5 requires all
moves complete. Phase 6 requires Phase 5.

In practice, do them sequentially — parallel file moves in the same
repo create merge conflicts.

---

## What's NOT in this plan

**No function body edits.** The `copyBlock` stderr fix, the
`handleSandbox` routing cleanup, the JSON helper deduplication — all
flagged as follow-ups, not done here.

**No new abstractions.** No interfaces extracted, no helper packages
created, no generics introduced. Moving code reveals what abstractions
*could* help, but introducing them simultaneously defeats the purpose
of a mechanical split.

**No test coverage improvements.** The split may reveal untested code
paths (functions that were "tested" only by being near tested functions
in the same file). That's a signal for future work, not this plan.

**No file renames.** Existing files (`snapshot.go`, `network.go`,
`configdrive.go`, `public_proxy.go`, `ratelimit.go`, `server.go`) keep
their current names. Even if `server.go` would be better named
`lifecycle.go` after routes move out — don't rename. One change at a time.

**No `go:generate` or build tag changes.** The `engine_linux.go` /
`engine_other.go` files in `cmd/bhatti/` use build tags. Don't touch them.
