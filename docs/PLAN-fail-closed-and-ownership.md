# Fail Closed, Own the Lifetime — paying down the v2 idiom and invariant debt

An adversarial read of the whole Go tree (~22k LOC non-test, post-Firecracker-removal),
calibrated against five codebases that solve bhatti's problems at scale: **tailscale**
(packet policy, `netip`, immutable shared state), **containerd** (daemon lifecycle,
optional subsystems, child reaping), **litestream** (SQLite correctness),
**GitHub CLI** (cobra without globals), **u-root** (PID 1). All five are cloned at
`~/ref/` and every pattern below was read, not recalled.

`go vet` is clean. `staticcheck` finds two things in 22k lines. Neither tool can see
any of what follows, because none of it is a bug in a line — it is a decision that was
never made.

---

## The diagnosis, in one paragraph

**bhatti guards against bad states in the places those five codebases arrange for them
to be inexpressible.** Every one of their best tricks is polarity, ownership, or naming:
decided once, free forever. Every one of bhatti's equivalents is a check at a use site:
paid repeatedly, and forgettable by the next person who adds a call site. That is also
why the code reads as machine-written — it is *locally plausible and globally unowned*.
Six of the seven items in Tranche 0 below are the same mistake wearing different clothes:
an absent value resolves to the permissive answer.

---

## Current State

### 1. Absence spells "permit" — seven sites, one root cause

`pkg/gateway/guard.go:76-79`:

```go
const (
	PosturePublic Posture = iota // allow the public internet (deny host/private) — the default
	PostureDeny                  // deny everything not explicitly allow-listed (locked-down agent box)
)
```

`EgressPolicy{}` — the zero value, what you get from a partially-decoded wire message,
a forgotten field, or a missing map key — means "reach the whole public internet."
tailscale orders the same enum the other way (`~/ref/tailscale/wgengine/filter/filter.go:114-119`):
`Drop Response = iota`, with the `noVerdict` sentinel deliberately **last** so that
"I forgot to set a verdict" is a drop, not a pass-through.

The tell that this was never a decision: thirty lines below, `Verdict{Allow bool}`
(`guard.go:131-134`) denies at zero. Same file, same author, opposite polarity. It is
the order the two constants happened to be typed in.

What that polarity costs, today:

| site | what an absent value does |
|---|---|
| `cmd/bhatti-netd/netstack.go:135` | `defPol := &gateway.EgressPolicy{Default: gateway.PosturePublic}` — the permissive policy, constructed anonymously |
| `cmd/bhatti-netd/netstack.go:162` | `if pol == nil { pol = &EgressPolicy{Default: PosturePublic} }` — second re-assertion |
| `cmd/bhatti-netd/netstack.go:180-186` | `stateFor` cache miss returns `g.defState`; an unregistered guest gets full public egress |
| `pkg/gateway/control.go:34-38` | `case "", "public":` — an **omitted** JSON `default` field maps to public egress |
| `pkg/gateway/control.go:110-113` | a policy that fails to parse is `continue`d; the guest keeps its previous (permissive) posture, logged nowhere |
| `pkg/store/user.go:97-104` | `NextSubnetIndex` discards its `Scan` error and returns `1` — signature returns an `error` it is structurally incapable of producing |
| `pkg/store/volume.go:251,253` | the RW-exclusivity guard's two `COUNT(*)` scans drop their errors; on failure both read 0 and two guests mount one ext4 read-write |

Also `volume.go:223` (attachment count before `DELETE`, then the caller `os.Remove`s the
file), `volume.go:228` (the `DELETE`'s own error dropped before `Commit()`), and
`volume.go:382` (storage quota `SUM` → 0 → quota bypass).

### 2. Two egress holes that follow from the same design gap

`ExtraHardDeny` (`guard.go:127`) is documented as covering "host addrs, daemon API,
other tenants' vnets." It has exactly three references in the tree: its declaration,
one read at `:163`, and `guard_test.go:71`. **Nothing in production writes it.**

`Check`'s documented and implemented order (`guard.go:122`, `:163-186`) is
`hard-deny → allow-cidr → allow-host → soft-deny → default`. Because `AllowCIDRs`
(`:168`) is tested *before* `classSoftDeny` (`:179`), and hard-deny is empty,
`--allow-cidr 100.64.0.0/10` returns `allow("allow-cidr")` for the host gateway and
every other tenant on the box. Today only CGNAT soft-deny keeps them out, and
allow-cidr is precisely the flag that overrides it.

Second hole: `cmd/bhatti-netd/forward.go:33-43` dials siblings via
`gonet.DialContextTCP` and **never calls `Check`**. Under `PostureDeny` — the
"locked-down agent box" — a guest still has unrestricted TCP to every sibling of the
same owner. UDP goes the other way (`forward.go:101` has no sibling branch, so siblings
hit CGNAT soft-deny and are dropped). TCP-to-sibling always allowed, UDP-to-sibling
always denied, and the asymmetry exists only as the shape of two `if`s in two functions.
`EgressPolicy` cannot express "deny siblings," so no reader of `guard.go` can discover
they are reachable. The comment at `forward.go:34-35` says siblings are "mediated +
observable by netd" — that branch produces no `Verdict`, so nothing is observable.

### 3. Nobody owns a child process, and nobody owns a goroutine

**`cmd.Wait()` is called zero times in `pkg/engine`.** Five sites call
`cmd.Process.Wait()` instead (`krucible/engine.go:176, 239, 244, 746, 772`), which reaps
the pid but leaves `exec.Cmd`'s pipes and the `watchCtx` goroutine that
`exec.CommandContext` spawns. `WaitDelay` and `Cmd.Cancel` are unused repo-wide, so
cancelling the context signals the group leader only, despite `Setpgid: true`. Nothing
observes helper exit, so `vm.Status` — written only by `Stop`/`Destroy`
(`engine.go:812`, `:867`) — reports `"running"` for a dead VM indefinitely, and
`agentFor` hands out a client for a dead socket.

In the guest, both wait-consumers exist and neither knows about the other.
`cmd/lohar/main.go:361-370` is `syscall.Wait4(-1, &status, WNOHANG, nil)` on a
one-second sleep, with the reaped `status` discarded entirely — and its own doc comment
says *"Go's runtime handles SIGCHLD for processes started via exec.Command,"* which is
exactly the invariant a blanket `wait4(-1)` breaks. When the reaper wins the race,
`cmd.Wait()` at `systemctl.go:807` returns `*os.SyscallError{ECHILD}` instead of
`*exec.ExitError`, so `:813` forces exit code 1 and `:821` marks a cleanly-exited unit
`failed`.

containerd names this exact bug as the reason a type exists
(`~/ref/containerd/vendor/github.com/containerd/go-runc/monitor.go:38-49`):
*"It allows daemons using go-runc to have a SIGCHLD handler to handle exits without
introducing races between the handler and go's exec.Cmd."*

On the host side, `Server` holds five independent stop-handles — `stopThermal` +
`thermalDone` (`server.go:55-56`), `stopTaskCleanup` (`:57`), `stopMetrics` /
`stopRetention` (`:80-81`), `stopBackup` (`:92`) — and `Close()` (`:318-343`) cancels
all five and waits for exactly one. Five features added by five commits with nobody
owning shutdown.

The reachable failure lives in that gap: `Close` cancels `stopBackup` without waiting,
then calls `s.events.Close()` → `close(r.ch)` (`event_recorder.go:188`), while `Record`
does an unguarded `case r.ch <- e:` (`:104`). A backup failing on SIGTERM calls
`RecordEvent`; so does every hijacked websocket's deferred `RecordEvent`
(`exec_handlers.go:244`, `:453`, `shell_handlers.go:148`) — and `http.Server.Shutdown`
explicitly does not wait for hijacked connections. Send on a closed channel panics; the
`default:` arm only covers *would block*. With **16 `go func()` and zero `recover()`**
in `pkg/server`, that is a core dump during shutdown.

### 4. The store cannot be cancelled, and its writers are not serialized

| property | measured | consequence |
|---|---|---|
| `context.Context` on any store method | **0** of 133 query calls | no request deadline, no shutdown cancellation |
| `SetMaxOpenConns` | **never called** anywhere in the repo | unbounded concurrent connections to one SQLite file |
| DSN (`store.go:111`) | `?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)`, no `_txlock` | see below |
| `journal_mode` verified after set | no | a silent failure to enter WAL is undetectable |
| sentinel/typed errors in `pkg/store` | **zero** | 14 `strings.Contains(err.Error(), …)` control-flow sites in `pkg/server` |
| migration error handling (`store.go:118-124`) | every error discarded | a daemon can boot on an incomplete schema |

`busy_timeout` does not cover the case this store actually hits. Verified in the driver
already vendored — `modernc.org/sqlite@v1.46.1/lib/sqlite_darwin_arm64.go`, inside
`sqlite3BtreeBeginTrans`:

```go
if rc == SQLITE_BUSY|2<<8 && pBt.FinTransaction == TRANS_NONE {
	/* if there was no transaction opened when this function was
	** called and SQLITE_BUSY_SNAPSHOT is returned, change the error
	** code to SQLITE_BUSY. */
	rc = int32(SQLITE_BUSY)
}
```

`SQLITE_BUSY_SNAPSHOT` (517) is downgraded to a retryable `SQLITE_BUSY` **only if no
transaction was already open** — i.e. only for `BEGIN IMMEDIATE` at the top. A DEFERRED
transaction that reads and *then* writes takes the other branch, the busy handler is
never invoked, and `busy_timeout(5000)` is irrelevant.

Three read-then-write DEFERRED transactions sit on exactly the paths where two API
callers collide: `AttachPersistentVolume` (`volume.go:234-276`),
`DeletePersistentVolume` (`:209-229`), and `UpdateSandboxLabels` (`sandbox.go:234-277`).
That last one carries this doc comment (`sandbox.go:228-229`):

> *"Runs in a transaction so concurrent updates from another writer don't lose entries."*

The body is `Begin()` → `SELECT COALESCE(labels,'{}')` → unmarshal → merge in Go →
`UPDATE` → `Commit()`. Two concurrent label edits both read `{}`, both merge their own
key, both UPDATE. One is lost, and the loser gets `nil` back — not `SQLITE_BUSY`,
because the second UPDATE is a legal write against a snapshot SQLite still allows. The
comment asserts precisely the property the code cannot have.

### 5. Load-bearing no-ops: abstractions kept alive for an engine that was deleted

| cluster | evidence | LOC |
|---|---|---|
| `VMStateProvider` → `saveVMState` → `FirecrackerState` | `cmd/bhatti/main.go:122-124` states in prose that krucible *"does not implement VMStateProvider"*; `routes.go:19-22` therefore returns early every time; **8 live call sites** carry comments like `// persist updated state` and `// persist snapshot paths` | ~120 + the 4 untyped-map helpers at `routes.go:45-86` |
| `recoverVMs` | `main.go:499`; **zero** production callers (20 call sites, all in `recovery_test.go` / `recovery_reliability_test.go`); reads a Firecracker-era table | 103 |
| `StartForce` | 2 refs total, both in `sandbox_handlers.go` (`:871` declaration, `:875` call). No implementation anywhere → the `if` is permanently false and `--force` silently degrades to a plain `Start` | 9 |
| `BalloonSet` | `krucible/thermal.go:119` returns nil; on `server.ThermalEngine` (`server.go:38`); the thermal manager calls it at `server.go:675` and `:703` expecting memory reclamation | 5 + 2 call sites |
| `pkg/dns` | **zero** importers among tracked non-test files; krucible sets neither `DNS` nor `DNSInternal`, so lohar's `applyDNS` is unreachable in v2 | 935 |
| dead agent transports | `NewVsockClient`, `NewTCPClient`, `NewTCPClientWithAuth` — refs only inside `client.go` | ~60 |

Roughly **1,300 LOC** whose deletion cannot change behaviour, and which currently makes
six subsystems look implemented.

containerd's answer to the same problem (`plugins/services/tasks/local.go:120-127`) is
worth stating because it is three decisions bhatti does not make: absence arrives as a
*named error* (`plugin.ErrPluginNotFound`), not a `false`; a **null object**
(`runtime.NewNoopMonitor()`) is substituted so downstream code has no optionality left
in it; and the decision happens once at startup, where `config.RequiredPlugins`
(`cmd/containerd/server/server.go:157-159`, `:201-229`) lets the operator make an
absent subsystem fatal.

The operationally dangerous instance is `server.go:528-531`:
`te, ok := s.engine.(ThermalEngine); if !ok { return }`. If a future engine drops one of
those six methods, the daemon boots clean, the CLI works, and sandboxes simply never
cool. No log line, no config key, no error.

### 6. The prose is where the machine-written origin shows

Comment *volume* is normal (~20% of lines) and the best comments here are as good as
anything in the exemplars. The problem is three specific, mechanically-detectable
failures.

**Banners as a substitute for files, which then become the cut line.**
`pkg/server/admin_handlers.go` carries ten topic banners (`:166`, `:241`, `:311`,
`:357`, `:368`, `:479`, `:618`, `:742`, `:814`, `:930`) — a directory's worth of subject
matter in one file, saying so out loud. `pkg/server/routes.go:113-115` is the fossil:
two headings, no content. Across tailscale's `ipn/`, `wgengine/`, `net/`, all of
containerd's `core/` and `pkg/`, and litestream's non-test source there are no topic
banners; the only banner-shaped construct is a lock scope attached to its mutex
(`~/ref/tailscale/net/dns/manager.go:77`: `mu sync.Mutex // guards following`).

`pkg/store/sandbox.go` **ends** like this:

```go
405: }
406:
407: // ==========================================================================
408: // v0.3 Persistent Volumes
409: // ==========================================================================
410:
411: // CreatePersistentVolume inserts a new persistent volume record.
```

End of file. The function is in `volume.go`, wearing the second half of its own doc
comment. The banner was the cut line for a file split and the cut went one line too
late. The same mechanism struck across `pkg/store`: `store.go:325-333` strands six doc
comments (CreateUser, SetSecret, CreateImage, CreateSnapshot, CreateTask,
CreateVolumeBackup); `secret.go:16` holds `User`'s doc while the type is in `user.go:9`;
`secret.go:107` holds `FirecrackerState`'s while the type is in `sandbox.go:349`;
`task.go:21` holds `Sandbox`'s; `proxy_handlers.go:381` is a `FileEngine` doc comment as
the last line of the file, with the type in `admin_handlers.go:361`.

This is not tidiness. `go doc` is wrong right now:

```
$ go doc ./pkg/store CreatePersistentVolume
func (s *Store) CreatePersistentVolume(v PersistentVolume) error
    Returns error on UNIQUE violation (not idempotent — for race coordination).

$ go doc ./pkg/store User
type User struct { ... }          ← no documentation

$ go doc ./pkg/server FileEngine
type FileEngine interface { ... } ← no documentation
```

`CreatePersistentVolume` renders a sentence fragment. `User` — the auth principal — is
documented with silence. Every editor hover and every pkgsite render shows this.

**Undated universal claims, six of which are false today.**

| claim | reality |
|---|---|
| `routes.go:17` "saveVMState persists Firecracker VM state to the store if the engine supports it" | no engine supports it; returns at `:21` every time |
| `main.go:128` "MUST run after recoverVMs" | `recoverVMs` has no production caller — and `:122-125`, four lines above, explains why it was removed |
| `krucible/thermal.go:35-36` "serialized … by launchMu (acquired below)" | `launchMu` is acquired at `:25`, *above* |
| `guard.go:127` `ExtraHardDeny // host addrs, daemon API, other tenants' vnets` | written only by `guard_test.go:73` |
| `systemctl.go:28-33` "Nothing in this file reads filesystem paths directly anymore" | `:684-688` declares three package-level mutable path vars, read from the production path at `:725-740`, written by tests — the exact shape the header says was eliminated |
| `sandbox.go:228-229` "concurrent updates from another writer don't lose entries" | DEFERRED read-modify-write; loses updates silently |

tailscale's convention is to timestamp the decay
(`~/ref/tailscale/ipn/prefs.go:114`: *"As of 2025-07-02, the only supported value is
[AnyExitNode]"*); litestream's is to cite the incident
(`~/ref/litestream/db.go:64`: *"The RESTART checkpoint mode was permanently removed due
to production issues with indefinite write blocking (issue #724)"*). Either makes the
claim re-checkable. A universally-quantified undated claim can only stay true if every
future edit re-verifies it, and no future edit will.

**Re-derivation instead of citation.** `startDaemon` (`systemctl.go:690-794`) is 105
lines of which ~54 re-explain what `spawn.go`'s own 50-line header already says. Two
copies must be kept in sync; they won't be. Relatedly, bhatti's provenance citations
point at perishable artifacts — *"Tranche 0a item #6 of PLAN-bhatti-v2.md"*
(`store.go:279`), *"G1.6 of PLAN-bhatti-v2.md"* (`sandbox.go:31`). The instinct is right
and it is the same instinct as `issue #724`, but a plan doc's section numbering shifts
as the plan is edited and the doc moves to `docs/archive/` when it lands. **Cite the
durable artifact** — a GitHub issue number resolves forever and carries the argument.

### 7. Idiom debt that is purely mechanical

`go.mod` says `go 1.26.3`. Zero uses of `slices`, `maps`, `cmp`, `min`/`max`,
`errgroup`, `os.Root`, `sync.OnceValue`, `binary.Append`, `errors.Join`. One sentinel
error in the entire tree (`cmd/lohar/unit.go:418`); `errors.Is`/`As` in two files.
`golang.org/x/sync` is a **direct** dependency used only for `singleflight`.
`gofmt -l` is dirty on 36 tracked files, 15 of them non-test production. `modernize`
reports 163 findings. Routing is 15 `HandleFunc` registrations with no method patterns
and no wildcards (`routes.go:88-104`), which is why `handleSandbox` is a hand-rolled
sub-router and the package carries 34 hand-written `errResp(w, 405, …)` branches.

And one whole class of bug follows from three package-level strings: `bhatti setup`
assigns `apiURL = endpoint` (`setup_cmd.go:121-122`) but never clears
`unixSocketPath`, and `baseTransport()` (`cli.go:467-476`) prefers the socket whenever
it is non-empty — so "Testing connection…" hits the **local** daemon over a unix
socket, gets 200 because the token is valid locally, and prints `✓ authenticated` for a
remote endpoint it never contacted. On a server box, that is the default path. gh's
`Factory` (`~/ref/cli/pkg/cmdutil/factory.go:36-46`) makes this unrepresentable by
holding *funcs* rather than values, and by naming its three clients after their auth
posture (`HttpClient`, `PlainHttpClient`, `ExternalHttpClient`).

---

## Scope

### In

- Polarity, ownership, and naming changes that make the bad state inexpressible.
- Deleting the load-bearing no-ops (~1,300 LOC) **before** any refactor, so nothing
  migrates a dead abstraction.
- Store: context plumbing, writer serialization, a migration ladder, an error vocabulary.
- Repairing `go doc` and the six false comments.
- The mechanical idiom sweep (`gofmt`, `modernize`), gated in CI so it stays fixed.

### Out (deliberately deferred)

- **The Go 1.22 routing rewrite and the middleware chain.** Real (it deletes the
  sub-router, the 34 `405` branches, and every `switch r.Method`), but it touches every
  handler signature and wants to land after the no-op deletion and the error vocabulary,
  or it merges with them badly. Its own plan, after T3.
- **A CLI `Factory`.** T0 fixes the *bug* (`setup` clearing `unixSocketPath`) in one
  line. Replacing nine package vars with an injected value is a separate, mostly
  mechanical PR that should not ride along with correctness work.
- **`engine.Caps()`** replacing the 15 runtime capability assertions. Right answer, but
  its value appears at the moment a second engine exists. T1 deletes the two capabilities
  implemented by nothing, which is the part that pays now.
- **A `[]Creator`-table boot sequence for lohar** (u-root's shape,
  `~/ref/u-root/pkg/libinit/root_linux.go:106-186`). `runAgent` being a linear boot
  script is legitimately linear; the table earns its keep when the error path is
  duplicated, which it currently is not.
- **`pkg/dns` resurrection.** Delete it. If sibling DNS returns, the answer is
  `golang.org/x/net/dns/dnsmessage`, already in the module graph.
- Splitting `systemctl.go` (1807 lines). Its function-length distribution is barely
  worse than gh CLI's; the file is large but the functions are not the problem. Revisit
  only if T2's reaper work makes the supervisor harder to read, not before.

---

## Tranches

Ordered so that each one makes the next cheaper, and so nothing lands on top of code
that is about to be deleted.

### T0 — Flip the polarity and close the reads that fail open

One sitting. No new abstractions, no signature churn. Every item is a decision that
propagates.

1. **`PostureDeny Posture = iota`** (`guard.go:76-79`). Changes no decision logic —
   `:183` still reads `if p.Default == PosturePublic`. What changes is that the three
   re-assertion sites become explicit rather than accidental, `control.go:34-38`'s
   empty-string case becomes an error instead of a permission, and every future
   `EgressPolicy` literal in a test or a new subsystem fails closed.
2. **Name the permissive constructor.** `func AllowAllEgress() *EgressPolicy` with a
   doc comment stating what a guest can reach, per
   `~/ref/tailscale/wgengine/filter/filter.go:157-160` (`NewAllowAllForTest`, whose
   comment names the spoofing attack it enables). Collapses the three anonymous struct
   literals into one greppable call site.
3. **Route siblings through `Check`.** Give `EgressPolicy` a `Siblings Posture` (deny at
   zero, after item 1) and have `forward.go:33` ask the policy instead of asking the
   subnet. Fixes the TCP/UDP asymmetry and makes the policy object the complete answer
   to "what can this guest reach," which is what makes a manifest diff reviewable.
4. **Populate `ExtraHardDeny` at netd startup**, from the host's own interfaces plus the
   daemon's bind address, merged inside `SetSandbox` so no wire message can omit it.
   Closes the `--allow-cidr 100.64.0.0/10` widening.
5. **The six fail-open reads.** `volume.go:223, 228, 251, 253, 382`; `user.go:99`.
   Shape: `if err := …Scan(&x); err != nil { return fmt.Errorf("…: %w", err) }`, and for
   `NextSubnetIndex` an actual error return. litestream's habit is stronger and worth
   copying — `~/ref/litestream/db.go:1102-1106` checks the scan *and* rejects a value
   that cannot be right (`else if db.pageSize <= 0`).
6. **`_txlock=immediate` + `SetMaxOpenConns`.** The driver accepts `_txlock`
   (`modernc.org/sqlite@v1.46.1/sqlite.go:187-193`). One DSN parameter turns
   `UpdateSandboxLabels`/`AttachPersistentVolume`/`DeletePersistentVolume` from a
   silent lost-update into a retryable `SQLITE_BUSY` that `busy_timeout` actually
   handles. Also read back `PRAGMA journal_mode` and fail if it isn't `wal`
   (`~/ref/litestream/db.go:1077-1081`).
7. **`setup` clears `unixSocketPath`** (`setup_cmd.go:121`), and returns an error on a
   failed connection test instead of `return nil`.
8. **`pid <= 1` moves into `ReadPID`** (`unit.go:252-258`), so `systemctl kill`
   (`systemctl.go:379-381`) and the IPC path (`systemctl_ipc.go:412-419`) inherit the
   guard that `svcStop` already has at `:964-976`, and the guard at `:973-976` gets
   deleted as redundant.

### T1 — Delete the load-bearing no-ops

Before anything else, so no refactor migrates a dead abstraction. Deletion order matters
only in that `recoverVMs` and `VMStateProvider` go together.

- `VMStateProvider` + `saveVMState` + the 8 call sites + `FirecrackerState` +
  `SaveFirecrackerState`/`LoadFirecrackerState` + the four untyped-map helpers at
  `routes.go:45-86`.
- `recoverVMs` (`main.go:499-600`) and the two stale comments referencing it
  (`main.go:128`, `:284`).
- `forceStarter`/`StartForce` (`sandbox_handlers.go:870-878`) — call `s.engine.Start`
  directly.
- `BalloonSet` from `server.ThermalEngine` and its two call sites (`server.go:675`,
  `:703`), plus the krucible no-op.
- `pkg/dns` (935 non-test LOC).
- The three dead agent constructors and the `isVsock`/`tcpAddr` branches in
  `pkg/agent/client.go`.

Two things this buys beyond the line count: the thermal manager stops calling a
function that pretends to reclaim memory, and the `ThermalEngine` assertion at
`server.go:528` becomes the only remaining silent-degradation path, which makes it worth
guarding with a startup check.

### T2 — Own the lifetimes

The tranche that stops the next five features from each adding their own `stopX` field.

1. **One `ctx`/`cancel`/`wg` per type, `Close()` cancels then waits.** litestream's shape
   (`~/ref/litestream/db.go:102-105`, `:802-805`, `:818-820`) repeated on every type
   that runs loops. Collapses the five `stopX` fields into one pair and makes `Close`
   provably blocking. Two details worth copying: `cancel: func() {}` at construction so
   `Close` before `Start` is a no-op rather than a nil deref (`replica.go:75`), and
   `context.WithoutCancel` for teardown work that must always run (`db.go:827`).
2. **Then the `event_recorder` panic becomes unrepresentable** — with no producer alive
   after `wg.Wait()`, `close(r.ch)` is safe. Do it in this order; a guard on `Record` is
   the wrong fix.
3. **A wait-owner for `bhatti-vmm`.** `cmd.WaitDelay = 5*time.Second`,
   `cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }`
   (you already set `Setpgid`), and one reaper goroutine per VM that calls `cmd.Wait()`
   and flips `vm.Status`/`vm.Agent` under `vm.mu`.
4. **A wait-owner for lohar.** containerd's design, not a mutex: SIGCHLD-driven, peek
   with `waitid(P_ALL, WEXITED|WNOHANG|WNOWAIT)` before reaping, and skip any pid the
   supervisor owns (the pid set already exists at `systemctl.go:773`, `:817`). Read
   `~/ref/containerd/pkg/sys/reaper/reaper_unix.go:95-127` — note that `Monitor.Start`
   subscribes *before* `c.Start()` so no exit lands in the gap, and `Monitor.Wait` calls
   `c.Wait()` only for its IO-flush side effect with the `ECHILD` error deliberately
   unchecked, returning the status from the reaper.
5. **While in there:** `exitCode := exitCodeFromErr(err)` at `systemctl.go:808-815`. The
   correct helper already exists 600 lines away at `exec.go:194-206` and converts
   `status.Signaled()` to `128+signal`, which is what makes `Restart=on-abnormal`
   (`:865`, testing `exitCode > 128`) able to fire at all.
6. **`recover()` on spawned goroutines** in `pkg/server`, or a tracked spawner
   (`~/ref/tailscale/util/goroutines/tracker.go:14-25`) so "is this leaking" is an
   answerable question.
7. **Release `vm.mu` before every blocking round-trip.** `krucible/thermal.go:27-39`
   (`Pause`/`Resume` hold it across a UDS round-trip) and `engine.go:768-772`
   (`kill` holds it across `Process.Kill`+`Wait`). `Stop` already does this correctly at
   `engine.go:845-847` — copy `ctlUDS` under the lock, release, then call. This matters
   because `List` takes `vm.mu` under `e.mu.RLock()`, and `RWMutex` is writer-preferring.
8. **`Status()` asks the control socket** instead of reading a cached string, mapping a
   closed socket to "gone." containerd's rule
   (`~/ref/containerd/core/runtime/v2/shim.go:856-870`): no cached status field
   anywhere, every `State()` round-trips, and `ttrpc.ErrClosed` maps to
   `errdefs.ErrNotFound` — so "the helper is gone" and "no such task" are the same
   answer, which is the correct answer. You already have
   `controlCmd(ctx, uds, "STATUS")` at `krucible/control.go:14-17`.

### T3 — The store's vocabulary and its schema

1. **Four sentinels + one classifier.** `ErrNotFound`, `ErrConflict`, `ErrInUse`,
   `ErrVolumeAttached` in `pkg/store`, plus one `classify()` using `errors.As` on
   `*sqlite.Error` (`modernc.org/sqlite@v1.46.1/error.go:12-21`;
   `SQLITE_CONSTRAINT_UNIQUE = 2067` is a stable extended result code). Deletes all 14
   `strings.Contains` sites in `pkg/server`, and removes driver message text from the
   test contract (`store_test.go:700` currently pins it). litestream's split is the model
   (`~/ref/litestream/litestream.go:31-37` for sentinels, `:41-58` for `LTXError` with
   `Op`/`Path`/`Hint` + `Unwrap` where the caller needs data).
   - Fixes a live collision on the way: `isValidName` permits a volume named
     `attachment`; `DeletePersistentVolume` returns `volume "attachment" not found`;
     `volume_handlers.go:152` matches `"attachment"` first → **409 Conflict with a body
     saying "not found."**
2. **`context.Context` on every store method**, in one mechanical pass to the
   `…Context` variants. Not a creeping half-migration.
3. **A `PRAGMA user_version` ladder**, generalised from `migrateSecretsToV2`
   (`store.go:281-318`) — which is already a correct migration: it detects the current
   shape by querying `pragma_table_info` rather than guessing, returns `(bool, error)`,
   and checks and wraps all four steps. `[]func(*sql.Tx) error` indexed by version, each
   step in its own transaction that bumps the version on commit. A failed boot then names
   step *N* instead of producing a daemon on an unknown schema.
4. **Close-to-broadcast for the event fan-out.** Replace the subscriber registry
   (`subs` map, `nextID`, `subscriberBuffer`, `closed atomic.Bool`, `disconnect`, the
   `fanOut` mutex on the request path, and both duplicated close loops) with litestream's
   `notify` pattern (`~/ref/litestream/db.go:76`, `:652-657`, `:1975-1977`): close the
   channel and install a fresh one, and let each subscriber read from the `events` table
   with its own cursor. bhatti's own comment already says this is the right substrate
   (`event_recorder.go:14-18`), and it *upgrades* delivery from lossy-with-disconnects to
   the at-least-once semantics that comment currently apologises for.

### T4 — Make the prose worth reading, and keep it that way

1. **Delete every topic banner**, split the files they were standing in for, and run
   `go doc` on every moved symbol. `pkg/server/admin_handlers.go` (10 banners) and
   `pkg/store` are the two that matter.
2. **Reattach the nine orphaned doc comments** listed in §6, verified with `go doc`.
3. **Fix the six false claims**, or fix the code they describe. Where the code can't be
   fixed now, downgrade the comment — `// TODO: not yet populated; CGNAT soft-deny is
   what currently keeps siblings unreachable` is worth more than a false description.
4. **`gofmt -w` and `modernize -fix`**, then add both to CI so it stays fixed.
5. **Adopt the house rules** below, and re-point the plan-doc citations at issue numbers.

**House rules** (each earned by something read in the exemplars):

1. No topic banners. A banner means the file wants splitting; split it and check `go doc`.
2. Put the invariant on the data, not in the function that currently enforces it —
   tailscale writes default-deny on `matches4` (`filter.go:51-55`) because the
   enforcement site is the thing most likely to move.
3. Date or cite every claim about the present. No un-scoped "nothing / always / the only".
4. A comment must not assert behaviour the code does not have.
5. Cite, don't re-derive. One home per fact; doc links elsewhere.
6. Name the dangerous constructor, and say what it enables.
7. Record the constraint, not the mechanism.

---

## What is already right, and should not be "improved"

Worth writing down so a future sweep doesn't flatten it.

- **`pkg/gateway/guard.go` is strong work.** Fixed evaluation order with the reason
  stated (`:120-122`); a hard-deny class no allow rule can re-open including `0.0.0.0/0`
  (`:163-166`, proven at `guard_test.go:149-153`); `canonical` (`:39-49`) closes both the
  `::ffff:` and NAT64 v4-smuggling paths and is applied in `classify` *and*
  `cidrsContain`, so an allow-CIDR cannot be evaded by re-encoding; `HostPattern` refuses
  regex on purpose and its wildcard is boundary-anchored with the two attack strings
  named (`:110-113`); `DeniedError` (`:195-204`) lets a caller distinguish policy denial
  from a network error. The `Dialer` resolves, vets **every** returned address, and dials
  only vetted ones, per-dial rather than pinning name→IP — closing rebinding TOCTOU
  without breaking CDNs, with the trade stated at `:206-210`.
- **`dialAddr` is unexported on purpose** (`guard.go:217-219`): *"Unexported so the
  vetting can never be bypassed by a caller."* That is behaviour-as-func-field plus a
  real reachability decision — the same pattern tailscale uses for `pathForTest`
  (`net/ipset/ipset.go:51`).
- **`allow(reason)` / `deny(reason)`** (`guard.go:136-137`) already is tailscale's
  `why`-carrying verdict. T0 item 3 is "use your own pattern consistently," not "adopt a
  new one."
- **`link.go`'s framing.** The single `[len‖frame]` buffer under a mutex is the correct
  way to make length-prefixed framing atomic; `maxFrameLen` rejects a desync rather than
  allocating from it; `ReadFrame` returns `io.EOF` verbatim. The allocation is a cost, not
  a bug, and ownership is documented (`:40-42`).
- **`ShellTokenHash string \`json:"-"\`** (`sandbox.go:22`) — a leak made
  unrepresentable at the type level. Same instinct as `sandboxCols` (`:37`): one const so
  the SELECT list and every Scan cannot drift.
- **`depgraph.go:96-135`** is a real Kahn topological sort with in-degree draining and
  cycle detection, consumed as parallel waves. No sleep-and-retry anywhere. (Two gaps
  worth an issue, not a tranche: `Wants=`/`Requires=` are never read, and the doc cites
  an `expandStartSet` that does not exist in the tree.)
- **Shutdown *ordering* in `main.go:274-296`** is deliberate and mostly right — drain
  HTTP, then `SnapshotAll()`, then `Close()`, with `:282` explaining "Done before Close()
  so the event recorder is still alive." What is missing is the waiting, not the thinking.
- **`thermalDone`** (`server.go:56`, `:324-326`) is the correct pattern applied once.
  Somebody hit the race and fixed it properly. T2 generalises it; it does not replace it.
- **The best comments** — `systemctl.go:964-976` (the `pid <= 1` reasoning),
  `netstack.go:105-112` (RX checksum offload: what libkrun strips, why gVisor would
  otherwise fail, why TX is deliberately not set), `config.go:41` + `:294-300` (the
  `*bool` tri-state, with the reason), `store.go:267-280` (`migrateSecretsToV2`'s
  crash-window note), `event_recorder.go:145-152` (a mutex-across-loop justified with an
  actual bound). T4's goal is *more of this*, uniformly — not "write like tailscale."

---

## Risk + mitigations

| Risk | Mitigation |
|---|---|
| Flipping `Posture`'s zero value breaks a live sandbox whose policy was never pushed | This is the point, and it is why T0 item 4 lands in the same PR: `SetSandbox` merges the deployment hard-deny and the daemon pushes policy *before* `krun_add_net_unixstream`, so there is no window in which a guest exists without a policy. The create/push race the current fallback was written for (`netstack.go:176-179`) becomes a narrow exception with an explicit name rather than the type's default. Verify with a create-under-load loop on agni-01 before tagging: N=50 concurrent creates, assert every guest gets a `SetSandbox` before its first packet. |
| `_txlock=immediate` turns silent lost-updates into visible `SQLITE_BUSY` errors, and some caller does not retry | Correct, and better than losing writes. `busy_timeout(5000)` handles the retry inside the driver for the ordinary case — the whole point of the change is that IMMEDIATE is the shape where the busy handler *is* invoked (verified in the driver source, see Pre-flight #4). The three affected methods are `UpdateSandboxLabels`, `AttachPersistentVolume`, `DeletePersistentVolume`; all three are already `error`-returning and all three callers already surface it. |
| `SetMaxOpenConns(1)` serializes reads and hurts list-heavy request paths | Do not set it to 1 globally. litestream's global `semaphore.NewWeighted(1)` is right for a single-purpose replicator and wrong for an HTTP API with many independent readers (`ListSandboxes`, `GetUserByKeyHash` on every authenticated request). Bound the pool to something sane (start at `runtime.GOMAXPROCS(0)`) and let IMMEDIATE serialize the writers. Measure `GET /sandboxes` p99 before and after on the local restore. |
| Deleting `recoverVMs` deletes recovery, and a future engine needs it | It is not recovery today — it has no production caller, and `main.go:122-125` records why: krucible rehydrates from each sandbox's `state.json` in `New()`, and there are no host TAP devices to reclaim under TSI. If a future engine needs store-based recovery it will need a different shape anyway (this one reads a Firecracker-only table). Deleting it also deletes the `VMStateProvider` interface whose only implementor is a test double, which is the actual reason the code survived. |
| Deleting `pkg/dns` removes sibling name resolution someone is relying on | Nothing imports it. The v2 path actively blackholes it: a guest querying its gateway `100.64.N.1:53` hits netd's UDP forwarder, gets `classSoftDeny` from CGNAT, and is dropped with no ICMP and no log. So sibling DNS is already not working; deleting the package makes that visible instead of implied. If it returns, `golang.org/x/net/dns/dnsmessage` is already in the module graph. |
| The T2 reaper change breaks `Restart=` behaviour in a way tier smoke tests miss | The reaper change and the `exitCodeFromErr` change (T2 item 5) are the two halves of the same bug and must land together — today a clean exit can be recorded as code 1 and a signal death as -1. Smoke both directions on agni-01: a unit that exits 0 must not be marked failed, and a unit killed with SIGSEGV must trigger `Restart=on-abnormal`. Both are currently broken, so the test is a fix-verification, not a regression guard. |
| Reattaching doc comments produces a huge diff that hides real changes in review | Land T4 items 1-2 as their own commit, comment-only, with `git diff --stat` showing zero non-comment lines. `go doc` before/after on the nine symbols is the review artifact. Do it *after* T1's deletions so the moved symbols are the final set. |
| `gofmt -w` across 36 files collides with in-flight branches | Run it as the last commit of T4, immediately before adding the CI gate, and rebase open branches onto it the same day. 15 non-test files are affected; the other 21 are tests. |
| Tranches drift and T0's polarity flip ships without T0's hard-deny population | Do not split T0. It is one PR, eight items, one tag. Items 1-4 are a single security change and reviewing them separately makes each look either pointless or dangerous. |

---

## Phasing

Five PRs, five tags. T0 and T1 can be same-day; T2 is the one that needs care.

1. **T0 — `v2.3.0`.** One PR, eight items. Includes the create-under-load verification
   on agni-01 and a `--allow-cidr 100.64.0.0/10` negative test proving the host gateway
   is still unreachable after item 4.
2. **T1 — `v2.3.1`.** Pure deletion, one commit per cluster so each is independently
   revertable. `go build ./... && go vet ./...` is the whole gate; there is no behaviour
   to smoke because there is no behaviour.
3. **T2 — `v2.4.0`.** The largest. Commit order: the `ctx`/`cancel`/`wg` shape on
   `Server` → the `event_recorder` simplification that becomes safe once it lands → the
   `bhatti-vmm` wait-owner → the lohar reaper + `exitCodeFromErr` (together) → the
   `vm.mu` releases → `Status()` over the control socket. Smoke each of the last four on
   agni-01 separately; a bad reaper change is a guest-visible regression.
4. **T3 — `v2.5.0`.** Sentinels first (they are additive and delete the `strings.Contains`
   sites), then the `ctx` pass, then the migration ladder, then the notify broadcast.
   The migration ladder needs a real prod-restore run: apply on a copy of production
   `state.db`, assert `user_version` lands on N, and assert a deliberately-broken step
   fails the boot with the step name.
5. **T4 — `v2.5.1`.** Banners + doc reattachment as one comment-only commit, false-claim
   fixes as a second, `gofmt`/`modernize` as a third, CI gate as a fourth.
6. **Move this PLAN to `docs/archive/`** once T4 ships.

Deferred work gets issues, not paragraphs here: Go 1.22 routing + middleware, the CLI
`Factory`, `engine.Caps()`, `os.Root` at the two snapshot path-join sites
(`krucible/snapshot.go:76`, `:190-196`).

---

## Pre-flight verifications (done while writing this plan)

1. **`cmd.Wait()` really is absent from `pkg/engine`.** `git grep` over the package finds
   five `cmd.Process.Wait()` sites (`krucible/engine.go:176, 239, 244, 746, 772`) and no
   `cmd.Wait()`. `WaitDelay` and `Cmd.Cancel` return zero matches repo-wide. So the
   `watchCtx` goroutine that `exec.CommandContext` spawns is never released and the
   `Cmd`'s pipes are never closed.
2. **`Posture` and `Verdict` really do have opposite polarity in the same file.**
   `guard.go:76` is `PosturePublic Posture = iota`; `guard.go:131-134` is
   `Verdict{Allow bool}`, which denies at zero. Read both.
3. **`ExtraHardDeny` has no production writer.** Three references in the whole tree: the
   declaration (`guard.go:127`), one read (`:163`), and `guard_test.go:71`.
4. **`busy_timeout` does not cover a DEFERRED read-then-write.** Confirmed in the vendored
   driver's amalgamation: `SQLITE_BUSY_SNAPSHOT` is `517` (`= 5 | 2<<8`), and inside
   `sqlite3BtreeBeginTrans` the downgrade to plain `SQLITE_BUSY` is guarded by
   `&& pBt.FinTransaction == TRANS_NONE` — i.e. only when no transaction was already
   open. The busy handler is only consulted after that downgrade.
5. **`_txlock` is supported by the driver we already vendor.**
   `modernc.org/sqlite@v1.46.1/sqlite.go:187-193` validates
   `deferred|immediate|exclusive` and sets `c.beginMode`. So T0 item 6 is a DSN string
   change, not a driver swap.
6. **`SetMaxOpenConns` is called nowhere.** Zero matches for
   `SetMaxOpenConns|SetMaxIdleConns|SetConnMaxLifetime` across the tree.
7. **The store takes no context at all.** 133 `Query`/`Exec`/`QueryRow` calls in
   `pkg/store`, zero `…Context` variants, and no method signature carries a
   `context.Context`.
8. **`saveVMState` is an unconditional no-op.** `routes.go:19-22` asserts
   `engine.VMStateProvider` and returns when it fails; `cmd/bhatti/main.go:122-124`
   states in prose that krucible does not implement it; the only implementor in the tree
   is `mockVMStateProvider` (`cmd/bhatti/recovery_test.go:23`). Eight production call
   sites.
9. **`recoverVMs` has no production caller.** 20 call sites, all in
   `recovery_test.go`/`recovery_reliability_test.go`.
10. **`StartForce` has no implementation.** Two references total, both inside
    `sandbox_handlers.go` — the interface declaration at `:871` and the call at `:875`.
11. **`go doc` is currently wrong for three symbols.** Ran it:
    `./pkg/store CreatePersistentVolume` renders only *"Returns error on UNIQUE violation
    (not idempotent — for race coordination)"* — a fragment, because the first half of its
    doc comment is stranded as the last line of `sandbox.go:411`. `./pkg/store User` and
    `./pkg/server FileEngine` render **no documentation at all**. (Note: the banner does
    not misattribute the comment as I first assumed — it severs the association entirely.)
12. **`pkg/store/sandbox.go` ends with a banner and an orphaned doc comment.** Lines
    407-411 are a `// ===` banner followed by
    `// CreatePersistentVolume inserts a new persistent volume record.` and then EOF.
    That is the file-split cut line, one line too late.
13. **`reapZombies` discards the status it reaps.** `cmd/lohar/main.go:361-370` declares
    `var status syscall.WaitStatus`, passes `&status` to `Wait4(-1, …)`, and the loop body
    never reads it. The sleep is one second on any non-reap, so it is a 1 Hz poll, not
    SIGCHLD-driven.
14. **`User=`/`Group=` are not honoured.** `syscall.Credential` appears in `cmd/lohar`
    only as hardcoded `Uid: 1000, Gid: 1000` on the interactive paths (`exec.go:41`,
    `:89`, `tty.go:78`, `piped_session.go:59`). `startDaemon` (`systemctl.go:741`),
    `startForking` (`:935`) and `runServiceCommand` (`:1589`) set none. Not in this plan's
    scope — filed as its own issue, because the fix has a real constraint (the drop must
    happen inside `lohar spawn`, after the `cgroup.procs` write, `Setgid` before
    `Setuid`).
15. **`systemctl kill` lacks the guard `svcStop` has.** `systemctl.go:379-381` is
    `if pid, err := u.ReadPID(); err == nil { syscall.Kill(-pid, sig) }` with no range
    check, while `:964-976` carries a ten-line comment explaining why `kill(-1, …)` from
    PID 1 is catastrophic. `ReadPID` (`unit.go:252-258`) is a bare `strconv.Atoi`, so
    `0`, `1` and `-1` all parse.
16. **`pkg/server` has 16 `go func()` and zero `recover()`.** `http.Server` recovers
    panics in the handler goroutine only; a panic in any spawned one kills the daemon.
17. **The exemplar claims are read, not recalled.** `filter.go:114-119` (`Drop = iota`,
    `noVerdict` last) and `:157-160` (`NewAllowAllForTest` with its spoofing warning);
    `litestream/db.go:1077-1081` (journal_mode read back and verified) and
    `litestream.go:31-37` (the five sentinels); `cli/pkg/cmdutil/factory.go:36-46` (func
    fields, three clients named by auth posture) and `pkg/cmdutil/errors.go` (the whole
    70-line error taxonomy for a 90k-LOC CLI); `containerd/core/runtime/v2/shim.go:856-870`
    (`ttrpc.ErrClosed` → `errdefs.ErrNotFound`) and `pkg/sys/reaper/reaper_unix.go:95-127`
    plus `go-runc/monitor.go:38-49` (the three-method `ProcessMonitor` that makes
    `cmd.Wait()` unreachable as a source of truth).
18. **`go vet` is clean and `staticcheck` finds two things in production code**
    (`cmd/bhatti/main.go:720` S1025, `pkg/server/public_proxy.go:224` S1008). Nothing in
    this plan is visible to either tool, which is the argument for writing it down.

---

## Open questions

1. **Should `Siblings` be a `Posture` or a `bool` on `EgressPolicy`?** A `Posture` (deny
   at zero, after T0 item 1), so a future third state — "siblings on an explicit port
   list" — does not require a schema change to the wire type. Closed: `Posture`.
2. **Does T0 item 4's hard-deny list belong on the wire or on the netd process?** On the
   process. It is a property of the deployment, not of a sandbox, and putting it on the
   wire means every control message can omit it — which is how it ended up empty in the
   first place. Computed once at netd startup from the host's interfaces plus the daemon's
   bind address, merged inside `SetSandbox`. Closed.
3. **Should `stateFor`'s permissive fallback survive as a narrow exception?** The
   create/push race it was written for (`netstack.go:176-179`) is real, and choosing
   availability there was a legitimate call. But it should be a named, logged, bounded
   exception — a guest whose `SetSandbox` has not landed within N ms — not the type's
   default answer for every future forgotten field. Open: decide N, and whether the
   bounded window is even needed once the daemon pushes policy before the VMM connects.
4. **Does the `notify` broadcast (T3 item 4) move too much cost onto SQLite?** Each
   subscriber wakes and runs one indexed `SELECT … WHERE id > cursor`. That is a local
   WAL-mode read and it is already how `/events?since=` works. Probably fine, but measure
   with 20 concurrent SSE subscribers on the local restore before deleting the registry.
5. **Should the `ThermalEngine` assertion become a startup check?** After T1, it is the
   only remaining silent-degradation path — an engine missing one of six methods boots
   clean and never cools a sandbox. containerd's answer is `RequiredPlugins` +
   `RegisterReadiness`. bhatti's cheaper version: a `var _ ThermalEngine =
   (*krucible.Engine)(nil)` compile-time assertion, plus a single startup log line naming
   the capabilities the engine actually satisfies. Open: is the compile-time assertion
   enough, or does the operator need to be able to make it fatal?
6. **What replaces the perishable plan-doc citations?** `store.go:279` and
   `sandbox.go:31` cite `PLAN-bhatti-v2.md` section numbers. Once this plan moves to
   `docs/archive/`, so does that reference. Proposal: open a tracking issue per invariant
   worth citing and reference the issue number, per litestream's `issue #724` habit.
   Open: whether that is worth the issue-tracker churn for ~6 sites.

---

## Alternatives considered (one paragraph each)

**Guard the permissive default instead of flipping the polarity.** Add a check at the
three `PosturePublic` construction sites and at `PolicyFromWire`. Rejected: that is four
places that must each stay correct, and the failure mode of forgetting one is silent
public egress. Flipping `iota` is one line that covers every site including the ones
nobody has written. This is the whole thesis of the plan and it would be strange to
reject it in its own first tranche.

**Fix the `event_recorder` panic with a guard on `Record`.** A `select { case <-r.quit:
return; case r.ch <- e: }` plus a `sync.Once` on `Close` would work and is smaller than
T2. Rejected as the *primary* fix because it treats the symptom: the panic is possible
only because five goroutines have no owner, and the same gap also loses the final metrics
interval and any partially-applied purge on every shutdown. Do the ownership work; the
guard becomes unnecessary. (If T2 slips, take the guard as a stopgap and say so in the
commit.)

**Keep `VMStateProvider` for the next engine.** Rejected: it is shaped for
Firecracker's state (`vsock_cid`, `tap_device`, `snap_mem_path`) and returns
`map[string]interface{}` that the server re-parses field by field through four untyped
helpers. A future engine needs a different type anyway, and keeping this one costs eight
call sites that read as live persistence. Delete it; write the new one when there is a
second engine to write it against.

**Split `systemctl.go` in this plan.** Tempting at 1807 lines, and the ten-file split
practically writes itself. Rejected for now: the measured function-length distribution is
barely worse than gh CLI's, so the file is large but the functions are not the problem,
and a 1800-line move would collide with T2's reaper and exit-code changes in the same
functions. Revisit after T2, when the supervisor's shape has settled.

**Adopt `versionize`-style schema versioning for the SQLite migrations.** Rejected as
over-scoped. `PRAGMA user_version` plus a slice of `func(*sql.Tx) error` is ~40 lines,
uses a mechanism SQLite provides, and generalises code that already exists in the file
(`migrateSecretsToV2`). A framework would be more machinery than the problem has.

---

## Reference reading

Cloned at `~/ref/`, and worth keeping for the next design question rather than the next
review:

| exemplar | what to read it for |
|---|---|
| `tailscale/wgengine/filter/` | a policy engine whose zero value denies; behaviour as func fields instead of optional interfaces; `PacketMatch`'s `(match bool, why string)`; `net/ipset` choosing one of seven implementations behind one closure |
| `tailscale/net/tstun/wrap.go` | `atomic.Pointer` over an immutable policy on a datapath; a `sync.Pool` whose comment explains why escape analysis needs it |
| `containerd/pkg/sys/reaper/` + `go-runc/monitor.go` | the only correct answer to "I reap orphans and I use `os/exec`" |
| `containerd/core/runtime/v2/shim.go` | never cache a fact you don't own; map a dead transport to "not found" |
| `containerd/plugins/services/tasks/local.go` + `cmd/containerd/server/server.go` | absence as a named value, null-object substitution, and letting the operator make it fatal |
| `litestream/db.go` | SQLite done properly: verified pragmas, a serialized writer, `defer`-on-failure init, and field comments that carry incident numbers |
| `litestream/db.go:76,652-657,1975-1977` | close-to-broadcast instead of a subscriber registry |
| `cli/pkg/cmdutil/` | a CLI with no ambient state, and a 70-line error taxonomy that serves 200 commands |
| `u-root/pkg/libinit/` | PID 1 duties, and a mount sequence as a table of values rather than a script |
