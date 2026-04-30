# Release B — CLI/UX overhaul

Tag: `v1.10.0`
Previous: `v1.9.0` (Release A — systemd-rc, shipped)

All CLI/host-side changes. No guest changes, no rootfs rebuilds.
Ordered by impact for the HN launch — first impression items first.

---

## B1 — Verbose create output + disk visibility

The first thing every user sees. Currently:
```
a1b2c3d4e5f6    dev    10.0.1.2
```

New:
```
sandbox/dev created (1 vCPU, 1024 MB, 1024 MB disk)
  IP:    10.0.1.2
  Shell: bhatti shell dev
```

Shows resources so the user knows what was allocated (the #12
complaint: "had to read docs to find the right flags"). Shows
disk size so they know how much space they have. Hints the next
command.

Idempotent create: `sandbox/dev unchanged (already exists)`.

## B2 — Streaming exec

Second thing every user hits. `bhatti exec dev -- sudo apt-get
install openssh-server` shows nothing for 30+ seconds.

When stdout is a terminal, send `Accept: application/x-ndjson`.
Server already supports it. Stream output line by line. When
piped, keep existing buffered behavior.

## B3 — Actionable error messages

Third thing every user hits — when something goes wrong.
Currently: `500 Internal Server Error: internal error`.

Pattern-match known errors, append recovery hints:
```
Error: sandbox "dev" is not running

  Resume it first:
    bhatti start dev
```

Also: confirm verbs on stop/start/destroy:
```
sandbox/dev stopped
sandbox/dev started
sandbox/dev destroyed
```

## B4 — Richer inspect + disk usage

kubectl `describe` style. The page you look at when something
isn't working.

```
Name:       dev
ID:         a1b2c3d4e5f6
Status:     running
Thermal:    hot
Image:      minimal
Created:    2026-04-30 10:00:00 (3 hours ago)

Resources:
  CPUs:     1
  Memory:   1024 MB
  Disk:     1024 MB (407 MB used, 518 MB free)

Network:
  IP:       10.0.1.2

Ports:
  22/tcp
  8080/tcp

Volumes:
  workspace → /workspace (rw)
```

Server: add cpus, memory_mb, disk_size_mb, image columns to
sandboxes table. Disk usage via live `df` exec (running VMs only).

## B5 — `bhatti ports`

CLI for existing `GET /ports` and `GET /sandboxes/:id/ports`.

```
$ bhatti ports dev
PORT    PROXY
22      /sandboxes/a1b2c3d4/proxy/22/
8080    /sandboxes/a1b2c3d4/proxy/8080/
```

~40 lines. Server endpoints exist.

## B6 — Cleaner list + wide mode

Drop ID from default columns (names are the primary key).
Add `-o wide` with resources and image.

```
$ bhatti ls
NAME         STATUS   THERMAL  IP
dev          running  hot      10.0.1.2

$ bhatti ls -o wide
NAME         STATUS   THERMAL  IP            CPUS  MEMORY  DISK   IMAGE
dev          running  hot      10.0.1.2      1     1024    1024   minimal
```

Needs the store columns from B4.

## B7 — Wire up `--force` on start

Error says `"use 'bhatti start --force' to retry"` but the flag
doesn't exist. Engine has `StartForce()`. Wire server + CLI.
~10 lines.

## B8 — Fix image pull Ctrl+C

Trap SIGINT, print "pull continues on server, check: bhatti
image list", exit cleanly. ~15 lines.

## B9 — `--detach` flag on exec

CLI for existing server `detach: true`. Fire-and-forget for
long-running commands.

```
$ bhatti exec dev --detach -- make build-all
pid: 4821
output: /tmp/bhatti-exec-4821.log
```

## B10 — `--hugepages` flag on create

CLI for existing server/engine support. 3 lines.

## B11 — `bhatti volume clone`

CLI for existing `POST /volumes/:name/snapshot`. ~20 lines.

## B12 — Delete dead code

Remove `UserData` and `Labels` from SandboxSpec and Template.
Leave DB columns (harmless, risky to migrate).

## B13 — Update `scripts/build-rootfs.sh`

Standalone dev rootfs gets the systemctl/journalctl symlinks,
policy-rc.d, runlevel shim, universe repo, systemd pin.

## B14 — Rewrite cli-reference.md

Cover all 55+ commands. Group by category:
```
Core:       create, list, inspect, destroy, stop, start, edit
Exec:       exec, shell, ps, ports
Files:      file read, file write, file ls
Resources:  image, volume, snapshot, secret
Publish:    publish, unpublish, share
Admin:      admin status, admin events, admin metrics, user, setup, update
```

Document volume backup (fully built, zero docs). Document the
systemctl shim behavior and limitations.

## B15 — `--secret` and `--file` on create

### Problem

Secrets are a dead end for CLI users. The documented flow is:

```bash
bhatti secret set API_KEY sk-abc123
# ...then what? No way to get it into a sandbox.
```

The only code path that decrypts and injects secrets is the
template path (`sandbox_handlers.go:153`). There is no template
CLI. So `secret set/list/delete` exist but are unusable from the
CLI.

`--file` has no boot-time equivalent either. The workaround
(`bhatti file write` after create) isn't atomic — the sandbox
boots, init runs, and the file isn't there yet.

### Design: kubectl model

Two mechanisms, different source, same destination (env var or
file in the guest):

- `--env KEY=VALUE` — inline plaintext, for non-sensitive config
  (`NODE_ENV=production`). Already works.
- `--secret NAME` — resolve from encrypted store server-side.
  Plaintext never on the command line after initial `secret set`.
  Injected as env var with the secret name as the key.
- `--file local_path:guest_path` — read local file, inject via
  config drive. For `.env` files, JSON configs, SSH keys,
  certificates. Content base64-encoded into config drive JSON.

```bash
# Inline env (existing)
bhatti create --name api --env NODE_ENV=production

# Secret from store (new)
bhatti secret set API_KEY sk-abc123        # once
bhatti create --name api --secret API_KEY  # every create

# File injection (new)
bhatti create --name api --file .env:/app/.env
bhatti create --name api --file id_rsa:/home/lohar/.ssh/id_rsa
```

### Server changes

Add to `createSandboxReq`:

```go
type createSandboxReq struct {
    // ... existing fields ...
    Secrets []string `json:"secrets,omitempty"` // secret names to resolve
    Files   []struct {
        GuestPath string `json:"guest_path"`
        Content   string `json:"content"`       // base64-encoded
        Mode      string `json:"mode,omitempty"` // default "0644"
    } `json:"files,omitempty"`
}
```

In the direct-creation path (no template), add secret resolution
— same 15-line decrypt loop that already exists in the template
path:

```go
// Resolve secrets from store (same logic as template path)
if len(req.Secrets) > 0 {
    for _, name := range req.Secrets {
        ciphertext, err := s.store.GetSecretValue(user.ID, name)
        if err != nil {
            errResp(w, 400, fmt.Sprintf("secret %q not found", name))
            return
        }
        plaintext, err := s.decryptSecret(ciphertext)
        if err != nil {
            errResp(w, 500, fmt.Sprintf("decrypt secret %q failed", name))
            return
        }
        spec.Env[name] = string(plaintext)
    }
}

// Resolve files
if len(req.Files) > 0 {
    if spec.Files == nil {
        spec.Files = make(map[string]engine.FileSpec)
    }
    for _, f := range req.Files {
        spec.Files[f.GuestPath] = engine.FileSpec{
            Content: decodeBase64(f.Content),
            Mode:    f.Mode,
        }
    }
}
```

### CLI changes

```go
// In createCmd flags:
createCmd.Flags().StringSlice("secret", nil, "Secret name from store (repeatable)")
createCmd.Flags().StringSlice("file", nil, "Inject file (local_path:guest_path, repeatable)")
```

Flag parsing for `--file`:

```go
for _, f := range fileFlags {
    parts := strings.SplitN(f, ":", 2)
    if len(parts) != 2 {
        return fmt.Errorf("invalid --file format %q (expected local:guest)", f)
    }
    data, err := os.ReadFile(parts[0])
    if err != nil {
        return fmt.Errorf("read %s: %w", parts[0], err)
    }
    files = append(files, map[string]string{
        "guest_path": parts[1],
        "content":    base64.StdEncoding.EncodeToString(data),
    })
}
```

### What we explicitly DON'T do

- **`--env-file`** — read a `.env` file as key-value pairs into
  env vars. Use `--file .env:/app/.env` and read it in your init
  script. Avoids parsing ambiguity (quotes, multiline, comments).
- **`--secret` with custom path/mode** — secrets always inject as
  env vars with the secret name as key. File-mode secrets
  (writing to a path with a mode) is a follow-up. The engine's
  `SecretRef` struct supports it but the CLI doesn't need it yet.
- **Directory injection** — `--file` takes a single file. For
  directories, use a volume or multiple `--file` flags.
- **Stdin as file source** — `--file -:/app/config` to read from
  stdin. Nice to have, follow-up.

### Changes

- `pkg/server/sandbox_handlers.go`: Add `Secrets` and `Files` to
  `createSandboxReq`, add resolution in direct-creation path.
  ~30 lines.
- `cmd/bhatti/sandbox_cmd.go`: Add `--secret` and `--file` flags,
  parse and build request. ~40 lines.

## B16 — Integration tests

New file: `cmd/bhatti/cli_ux_test.go`

This is the HN launch gate. Every B item gets verified by at least
one test. Tests are organized in three tiers: must-have for launch,
important polish, and edge cases for long-term safety.

All tests use the existing `cliTest` harness (builds the binary
from source, talks to a real daemon). Tests that modify output
formats use exact string matching (golden-file style) — not
`strings.Contains` — so formatting regressions are caught.

### Tier 1 — Must-have for launch (first-impression path)

The exact path every HN user will walk: create → see output →
exec something → hit an error → check inspect → list → stop/start
→ check ports. 11 tests.

```go
// B1: Verbose create output
func TestCLICreateVerboseOutput(t *testing.T)
    // Create sandbox, verify multi-line format:
    //   sandbox/<name> created (1 vCPU, 1024 MB, 1024 MB disk)
    //     IP:    10.x.x.x
    //     Shell: bhatti shell <name>
    // Match exact format — not strings.Contains.

func TestCLICreateIdempotent(t *testing.T)
    // Create same name twice → second prints:
    //   sandbox/<name> unchanged (already exists)
    // Exit code 0 (not an error).

// B2: Streaming exec
func TestCLIStreamingExecNDJSON(t *testing.T)
    // Run slow command (echo line; sleep 0.1; echo line).
    // Verify Accept: application/x-ndjson was sent.
    // Verify stdout lines arrive incrementally.
    // Use BHATTI_FORCE_STREAM=1 env to bypass TTY check in tests.

// B3: Actionable error messages
func TestCLIErrorExecOnStopped(t *testing.T)
    // Create → stop → exec. Verify stderr contains:
    //   sandbox "<name>" is not running
    //   Resume it first:
    //     bhatti start <name>

func TestCLIStopStartConfirmVerbs(t *testing.T)
    // Stop → verify output: "sandbox/<name> stopped"
    // Stop again → should not error (idempotency)
    // Start → verify output: "sandbox/<name> started"
    // Start again → should succeed or print "already running"
    // Destroy → verify output: "sandbox/<name> destroyed"
    // Exact format, not substring.

// B3 + lifecycle: Stop/start round-trip
func TestCLIStopStartRoundTrip(t *testing.T)
    // Create → exec (write marker) → stop → start → exec (read marker).
    // Verifies the snapshot/restore path through the CLI.

// B4: Richer inspect
func TestCLIInspectRichOutput(t *testing.T)
    // Create with --cpus 2 --memory 2048 --disk-size 4096.
    // Write a 10MB file to show disk usage.
    // Inspect → verify kubectl-describe-style fields present:
    //   Name, ID, Status, Thermal, Image, Created,
    //   Resources: (CPUs, Memory, Disk with used/free), Network: (IP)
    // Inspect --json → parse → verify cpus, memory_mb,
    //   disk_size_mb, image fields present and correct types.

// B5: Ports
func TestCLIPorts(t *testing.T)
    // Create → exec (python3 -m http.server 9090 &) → ports.
    // Verify output table includes 9090.
    // Verify --json returns parseable array with port + proxy path.

// B6: Cleaner list + wide mode
func TestCLIListCleanDefault(t *testing.T)
    // Create → list. Verify default columns are:
    //   NAME  STATUS  THERMAL  IP
    // Verify ID column is NOT present.

func TestCLIListWideMode(t *testing.T)
    // Create with --cpus 2 --memory 2048 → list -o wide.
    // Verify columns include:
    //   NAME  STATUS  THERMAL  IP  CPUS  MEMORY  DISK  IMAGE
    // Verify CPUS=2, MEMORY=2048 in the output row.

// B7: Force start
func TestCLIForceStart(t *testing.T)
    // Create → stop → start --force → exec succeeds.
    // (If no restore failure to trigger, at minimum verify
    // the flag is accepted and start succeeds.)
```

### Tier 2 — Important polish (12 tests)

Commands that exist but have zero test coverage, plus the
remaining B items that wire CLI flags to existing server features.

```go
// B9: Detached exec
func TestCLIDetachedExec(t *testing.T)
    // exec --detach -- sleep 300
    // Verify output contains "pid:" and "output:".
    // Verify exit code 0 (command launched, not waited on).
    // Verify the process is running: exec -- kill -0 <pid>

// B10: Hugepages flag
func TestCLIHugepagesFlag(t *testing.T)
    // create --hugepages --name hp-test
    // Verify sandbox created successfully.
    // Inspect → verify hugepages field is true.

// B11: Volume clone
func TestCLIVolumeClone(t *testing.T)
    // volume create → create sandbox → write data → destroy.
    // volume clone src → dst.
    // create sandbox with dst → verify data present.
    // Cleanup both volumes.

// B15: Secret and file injection
func TestCLICreateWithSecret(t *testing.T)
    // secret set TEST_SECRET test-value-123
    // create --secret TEST_SECRET --name sec-test
    // exec -- printenv TEST_SECRET → "test-value-123"
    // Verify: secret value never in CLI output or process args.

func TestCLICreateWithFile(t *testing.T)
    // Write a local temp file with known content.
    // create --file /tmp/test.conf:/app/config.conf --name file-test
    // exec -- cat /app/config.conf → matches local file content.
    // Verify file is available before init script runs:
    //   create --file /tmp/test.conf:/app/config.conf
    //          --init "cp /app/config.conf /tmp/proof"
    //   exec -- cat /tmp/proof → matches.

// Exit code contract (kubectl/docker standard)
func TestCLIExitCodeContract(t *testing.T)
    // exec -- true → exit 0
    // exec -- false → exit 1
    // exec -- sh -c "exit 42" → exit 42
    // exec nonexistent-sandbox → exit non-zero (not 0)
    // destroy nonexistent → exit non-zero

// --json on every major command
func TestCLIJSONCreateInspectListPorts(t *testing.T)
    // Create --json → valid JSON with id, name, ip, cpus, memory_mb
    // Inspect --json → valid JSON with all B4 fields
    // List --json → array, sandbox present
    // Ports --json → array
    // Each parse must succeed (json.Unmarshal, not strings.Contains).

// Commands with zero test coverage today
func TestCLIEditKeepHot(t *testing.T)
    // Create → edit --keep-hot → inspect → keep_hot: true.
    // edit --allow-cold → inspect → keep_hot: false.
    // edit --keep-hot --allow-cold → error.

func TestCLIPublishUnpublish(t *testing.T)
    // Create → exec (start http server on 9090).
    // Publish -p 9090 → verify URL in output.
    // Unpublish -p 9090 → verify success message.

func TestCLIShareRevoke(t *testing.T)
    // Create → share → verify URL in output.
    // share --revoke → verify revoked message.

func TestCLIAdminStatus(t *testing.T)
    // admin status → verify output contains version/uptime/sandboxes.
    // admin status --json → parse valid JSON.
    // No VM needed.

func TestCLITimingFlag(t *testing.T)
    // exec --timing -- echo hi
    // Verify stderr contains "server:" and "total:" lines.
    // Verify stdout still has "hi".
```

### Tier 3 — Edge cases for long-term safety (7 tests)

```go
// Full lifecycle
func TestCLILifecycleFullCycle(t *testing.T)
    // create → exec (write) → stop → start → exec (read) →
    // stop → start → exec (read again) → destroy.
    // Data survives two thermal cycles.

func TestCLIConcurrentExec(t *testing.T)
    // Launch 5 concurrent execs (different commands) on same sandbox.
    // All return correct results (no cross-contamination).
    // Use sync.WaitGroup + goroutines.

func TestCLILargeOutput(t *testing.T)
    // exec -- seq 1 100000 (outputs ~600KB).
    // Verify first line is "1" and last line is "100000".
    // Verify no truncation.

func TestCLIExecTimeout(t *testing.T)
    // exec --timeout 2 -- sleep 30
    // Verify exit code non-zero.
    // Verify completes in < 10s (not 300s default).

func TestCLICreateAllFlags(t *testing.T)
    // create --cpus 2 --memory 2048 --disk-size 4096
    //        --volume vol:/data --init "echo ok > /tmp/init"
    //        --env FOO=bar --secret MY_SECRET
    //        --file ./test.conf:/app/config.conf --keep-hot
    // Verify: inspect shows all resources.
    // Verify: exec -- cat /tmp/init → "ok".
    // Verify: exec -- printenv FOO → "bar".
    // Verify: exec -- printenv MY_SECRET → stored value.
    // Verify: exec -- cat /app/config.conf → file content.
    // Verify: exec -- ls /data → empty dir (volume mounted).

func TestCLIStopDestroyShortcut(t *testing.T)
    // Create → stop → destroy.
    // Verify stopped sandbox can be destroyed without start first.

func TestCLIInspectStoppedSandbox(t *testing.T)
    // Create → stop → inspect.
    // Verify status: stopped, stopped_at present.
    // Verify disk fields show "—" or "stopped" (no live df).
```

### What was cut and why

| Cut | Reason |
|-----|--------|
| `TestCLIExecBufferedWhenPiped` | Existing `TestCLIExec` already runs piped. Tests a negative. |
| `TestCLIErrorNotFound` | Existing `TestCLIExecNonexistentSandbox` + `TestCLIDestroyNonexistentSandbox` already cover. |
| `TestCLIHelpGroupHeaders` | Tests cobra's group rendering, not our code. |
| `TestCLICompletionScripts` | Tests cobra's `GenBashCompletion`. No bhatti logic. |
| `TestCLIVersionCheck` | Existing `TestCLIVersion` already covers; `--json` proven by `TestCLIJSONOutput`. |
| `TestCLIThermalCycleWithExec` | `TestCLILifecycleFullCycle` does 2 cycles; A6 engine tests do 5 with nginx. 30s+ of apt-get for no new signal. |
| `TestCLICreateDuplicateName` | Identical to `TestCLICreateIdempotent`. |
| `TestCLIInspectDiskUsage` | Merged into `TestCLIInspectRichOutput` — same VM, same inspect call. |
| `TestCLIInspectJSON` | Merged into `TestCLIInspectRichOutput` — text + JSON in one test. |
| `TestCLIStopStopIdempotent` | Merged into `TestCLIStopStartConfirmVerbs` — same lifecycle. |
| `TestCLIStartRunningIdempotent` | Merged into `TestCLIStopStartConfirmVerbs` — same lifecycle. |
| `TestCLIExecWithEnv` | Subsumed by `TestCLICreateAllFlags` which tests --env + --init + --volume + --secret + --file together. |

### Server-side prerequisites for B tests

Tests should be written first (TDD-style) and will fail until the
corresponding B item lands:

| Test | Blocks on |
|------|-----------|
| `TestCLICreateVerboseOutput` | B1 (new output format) |
| `TestCLICreateIdempotent` | B1 (idempotent create) |
| `TestCLIStreamingExecNDJSON` | B2 (CLI sends Accept header) |
| `TestCLIErrorExecOnStopped` | B3 (pattern-matched errors) |
| `TestCLIStopStartConfirmVerbs` | B3 (confirm verb format) |
| `TestCLIInspectRichOutput` | B4 (store columns + Sandbox struct + live df) |
| `TestCLIPorts` | B5 (ports CLI command) |
| `TestCLIListCleanDefault` | B6 (drop ID column) |
| `TestCLIListWideMode` | B6 (-o wide) + B4 (store columns) |
| `TestCLIForceStart` | B7 (server reads force param + CLI flag) |
| `TestCLIDetachedExec` | B9 (--detach flag) |
| `TestCLIHugepagesFlag` | B10 (--hugepages flag) |
| `TestCLIVolumeClone` | B11 (volume clone command) |
| `TestCLICreateWithSecret` | B15 (--secret flag + server resolution) |
| `TestCLICreateWithFile` | B15 (--file flag + server injection) |
| `TestCLICreateAllFlags` | B15 (--secret + --file in combo test) |

Tests not in this table can pass against the existing codebase
and should be written and merged first.

### Implementation order

1. Write all tests in `cli_ux_test.go`. Tests that depend on
   unshipped B items use `t.Skip("requires B<N>")`.
2. Ship each B item. Remove the corresponding `t.Skip`.
3. All tests green → tag v1.10.0.

### Coverage summary

| Category | Before | After |
|----------|--------|-------|
| Total CLI test functions | 54 | 84 |
| Lifecycle (stop/start) | 0 | 5 |
| Output format verification | 0 | 5 |
| Error message contracts | 0 | 2 |
| Streaming exec | 0 | 1 |
| New B commands (ports, vol clone, detach, hugepages, force) | 0 | 5 |
| Secret/file injection | 0 | 3 |
| Untested commands (inspect, edit, publish, share, admin) | 0 | 5 |
| Exit code contracts | 1 (partial) | 2 |
| Flag composition (--json, --timing, --force, -o wide) | 1 (partial) | 4 |
| Concurrent/stress | 0 | 2 |
| Full lifecycle round-trip | 0 | 2 |

---

## Dependency graph

```
Server-side prerequisites (must land before dependent B items):
  Sandbox struct: add CPUs, MemoryMB, DiskSizeMB, Image, Thermal
  fields to store.Sandbox + sandboxCols query + JSON response.
  Needed by: B1, B4, B6.

  handleSandboxStart: read "force" param from request body,
  call engine.StartForce() when true. Needed by: B7.

  createSandboxReq: add Secrets + Files fields, add resolution
  in direct-creation path. Needed by: B15.

Within B, dependencies:
  B4 (store columns) ← B6 (list -o wide needs resource columns)
  B4 (store columns) ← B1 (create output shows allocated resources)
  B4 (store columns) ← B16 tests that verify resource fields
  B1-B15 all land   ← B14 (docs rewrite)
  All B items        ← B16 (integration tests remove t.Skip gates)
  Everything else is independent.

Implementation order for HN:
  Phase 1 — Write all B16 tests (with t.Skip gates)       [day 1]
  Phase 2 — Server prereqs (Sandbox struct, force, secrets) [day 1]
  Phase 3 — B1 (create output)                             [day 2]
  Phase 4 — B3 (error hints + confirm verbs)               [day 2]
  Phase 5 — B2 (streaming exec)                            [day 2]
  Phase 6 — B4 (richer inspect)                            [day 3]
  Phase 7 — B6 (list -o wide)                              [day 3]
  Phase 8 — B5 (ports), B7 (--force), B9 (--detach)       [day 3]
  Phase 9 — B15 (--secret + --file on create)              [day 4]
  Phase 10 — B10 (hugepages), B11 (vol clone), B8 (Ctrl+C) [day 4]
  Phase 11 — B12 (dead code), B13 (build-rootfs.sh)        [day 4]
  Phase 12 — B14 (docs rewrite)                            [day 5]
  Phase 13 — Remove all t.Skip gates, all green → tag      [day 5]
```

---

## Audit reference

| # | Gap | Fix | Item | Test |
|---|-----|-----|------|------|
| 1 | `--force` flag referenced, doesn't exist | Wire server + CLI | B7 | `TestCLIForceStart` |
| 2 | Port discovery: server exists, no CLI | `bhatti ports` | B5 | `TestCLIPorts` |
| 3 | Detached exec: server supports, no CLI | `--detach` flag | B9 | `TestCLIDetachedExec` |
| 4 | Streaming exec: server has NDJSON, CLI never requests | Stream when TTY | B2 | `TestCLIStreamingExecNDJSON` |
| 5 | Hugepages: API field, no CLI flag | `--hugepages` flag | B10 | `TestCLIHugepagesFlag` |
| 6 | Labels: dead field | Delete | B12 | — |
| 7 | UserData: dead field | Delete | B12 | — |
| 8 | Secrets on create: store works, no CLI consumption path | `--secret` flag + server resolution | B15 | `TestCLICreateWithSecret` |
| 9 | Files on create: engine supports, no CLI flag | `--file` flag + server injection | B15 | `TestCLICreateWithFile` |
| 10 | Volume clone: server works, no CLI | `bhatti volume clone` | B11 | `TestCLIVolumeClone` |
| 11 | Template CRUD: server works, CLI only consumes | Deferred | — | — |
| 12 | Task status: server works, no CLI | Deferred | — | — |
| 13 | Health endpoint: no CLI | Not needed | — | — |
| 14 | cli-reference.md: 16/55+ commands documented | Rewrite | B14 | — |
| 15 | Image pull: no Ctrl+C handling | Signal trap | B8 | — (manual) |
| 16 | build-rootfs.sh: needs systemctl symlink | Update | B13 | — |
| 17 | Disk space not visible in create/inspect/list | Show in output | B1/B4/B6 | `TestCLIInspectRichOutput`, `TestCLIListWideMode` |
| 18 | `Sandbox` struct missing CPUs/Memory/Disk/Image/Thermal | Add to struct + query | B4 prereq | `TestCLIInspectRichOutput` |
| 19 | `handleSandboxStart` ignores force param | Read param, call `StartForce()` | B7 prereq | `TestCLIForceStart` |
| 20 | stop/start lifecycle: zero CLI test coverage | Add tests | B16 | `TestCLIStopStartRoundTrip`, `TestCLILifecycleFullCycle` |
| 21 | inspect command: zero test coverage | Add tests | B16 | `TestCLIInspectRichOutput`, `TestCLIInspectStoppedSandbox` |
| 22 | edit command: zero test coverage | Add tests | B16 | `TestCLIEditKeepHot` |
| 23 | publish/unpublish: zero test coverage | Add tests | B16 | `TestCLIPublishUnpublish` |
| 24 | share: zero test coverage | Add tests | B16 | `TestCLIShareRevoke` |
| 25 | admin commands: zero test coverage | Add tests | B16 | `TestCLIAdminStatus` |
| 26 | Exit code contract not verified | Add tests | B16 | `TestCLIExitCodeContract` |
| 27 | Create with all flags: untested combo | Add test | B16 | `TestCLICreateAllFlags` |
