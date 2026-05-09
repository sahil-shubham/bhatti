# Feature: Rename an existing sandbox

Discussion: [#14](https://github.com/sahil-shubham/bhatti/discussions/14)

---

## Summary

A user asked whether sandbox renaming is feasible. It is, and it's
about as small as a feature gets. The sandbox name is a label in one
SQLite column; nothing in the engine, network, snapshot, volume,
publish, or recovery paths treats it as identity. The only invariant
to preserve is per-user uniqueness among non-destroyed sandboxes,
and the index that enforces it (`idx_sandboxes_user_name`) already
exists. The change slots into the existing `PATCH /sandboxes/:id`
handler — the same endpoint that toggles `keep_hot` — so there is
no new route, no migration, no new event type, no new engine
capability.

Half-day change end-to-end. ~130 LOC including tests.

---

## What "name" actually is

| Where | Use | Affected by rename? |
|---|---|---|
| `sandboxes.name` (SQLite) | source of truth, returned by API | **yes — this is the rename** |
| `engine_id` | opaque ID assigned by the engine | no, separate field |
| `pkg/engine/firecracker.VM.Name` | held in memory, used only for slog labels and the (unused) `engine.List()` return | stale until next daemon restart — see below |
| Config drive `Hostname` | becomes `/etc/hostname` and `/etc/hosts` in the guest at boot | **deliberately not touched** |
| `publish_rules.alias` | derived from name *at publish time*, then frozen | **not touched** — public URLs stay stable |
| `bhatti-<name>-workspace` default volume name | generated once at create time | not touched, volume is its own object |
| `RestoreVM(id, name, …)` on daemon restart | reads `sandboxes.name` from the store | picks up the new name automatically |
| `~/.cache/bhatti/sandboxes` (CLI completion) | newline-separated list | updated by CLI on rename, also rebuilt on next `bhatti ls` |
| Active shell / exec / websocket sessions | keyed on `sb.ID` and the shell token, never `sb.Name` | **not affected — live sessions keep working** |
| `snapshots.source_sandbox`, `events.sandbox_id` | both reference sandbox ID | not affected — historical records stay linked |

### Things deliberately left alone

**The guest hostname.** Set in `pkg/engine/firecracker/create.go:213`
via the config drive, read by `lohar` at PID-1 boot
(`cmd/lohar/main.go:115-121`). Changing it post-boot would mean a
new wire-protocol op or shelling out to `hostname` + a `sed -i
/etc/hosts`. The hostname is visible only inside the sandbox's own
shell prompt; the user picked it when they created the sandbox.
Document it: *the in-guest hostname is fixed at create time*. Same
behaviour as `docker rename`.

**Published aliases.** `publish_rules.alias` is generated as
`<name>-<6 random chars>` and is the public URL the user has
already shared externally. Rewriting it on rename would silently
break links. Keep it frozen — the user can `unpublish && publish`
to get a fresh one if they want it.

**Auto-generated volume names.** `bhatti-<name>-workspace` was a
one-shot default at create time and is now the volume's actual
identifier. Volumes are independent objects with their own
lifecycle; renaming the sandbox does not retroactively rename
volumes.

**The engine's in-memory `vm.Name`.** Used only for slog labels.
After rename, log lines from the thermal manager etc. print the old
name until the daemon restarts (at which point `RestoreVM` reads the
new name from the store). This is a cosmetic-only inconsistency
limited to log files — not worth a new engine capability interface,
a new mutex acquisition, and a new mock surface in every engine
test. Revisit only if it actually causes confusion in practice.

---

## API surface

Extend the existing `PATCH /sandboxes/:id` to accept `name`. No new
route, no new event type.

**`pkg/server/sandbox_handlers.go`** — extend the inline struct in
the PATCH branch:

```go
var req struct {
    KeepHot *bool   `json:"keep_hot"`
    Name    *string `json:"name"`
}
```

When `req.Name != nil`:

```go
newName := *req.Name
if newName != sb.Name {
    if !isValidName(newName) {
        errResp(w, 400, "invalid sandbox name: must match [a-zA-Z0-9][a-zA-Z0-9._-]{0,62}")
        return
    }
    if err := s.store.RenameSandbox(user.ID, sb.ID, newName); err != nil {
        // Match the create-path convention (sandbox_handlers.go:499)
        if strings.Contains(err.Error(), "UNIQUE") {
            errResp(w, 409, fmt.Sprintf("name %q is already in use", newName))
            return
        }
        errRespInternal(w, r, "rename sandbox failed", err)
        return
    }
    oldName := sb.Name
    sb.Name = newName
    slog.Info("sandbox.updated",
        "sandbox_id", sb.ID, "old_name", oldName, "new_name", newName,
        "user", user.Name)
    s.RecordEvent(store.Event{
        Type: "sandbox.updated", UserID: user.ID, SandboxID: sb.ID,
        Meta: map[string]any{"old_name": oldName, "new_name": newName},
    })
}
```

Apply the rename block **before** the existing `keep_hot` block so
the keep_hot log line (and a possible `ensureHot` wake) reflect the
new name. The two blocks remain independently optional and run as
two sequential DB writes — not in a single transaction. If a PATCH
sets both fields and `ensureHot` fails after a successful rename,
the rename has committed; the user just retries the keep_hot. That's
acceptable: each field has its own meaning and there's no consistency
invariant between them.

### Why PATCH and not a dedicated `/rename` endpoint

PATCH is already the "mutate a property of an existing sandbox"
endpoint, already returns the updated sandbox, already does
ownership checking. Adding a second mutator there is one struct
field. A `POST /sandboxes/:id/rename` would duplicate the lookup,
ownership check, event emission, and 200-response shape with no
benefit.

### Why `sandbox.updated` and not a new `sandbox.renamed` type

The existing event taxonomy is
`sandbox.{created,destroyed,started,stopped,updated}`. `keep_hot`
toggles already emit `sandbox.updated`. A rename is the same shape —
"a mutable property changed" — and putting `old_name`/`new_name` in
`meta` preserves the audit trail without sprawling the event types.

### Idempotent / no-op semantics

`newName == sb.Name` short-circuits silently (no DB write, no event).
Matches the idempotent-create behaviour. An empty string name fails
`isValidName` and returns 400; we don't try to be cute about
"unset" semantics — there's no such state.

### Resolution by old name

`PATCH /sandboxes/:id` already accepts either a UUID or a name in
the path (`s.store.GetSandbox` falls back to name lookup). So
`bhatti edit old-name --name new-name` works without the CLI doing a
pre-resolve.

---

## Store layer

**`pkg/store/sandbox.go`** — one method:

```go
// RenameSandbox updates the user-visible name of a sandbox, scoped
// to the owning user. Returns an error containing "UNIQUE" on name
// conflict (caught by the handler), and a "not found" error if the
// sandbox doesn't exist or belongs to another user.
func (s *Store) RenameSandbox(userID, id, newName string) error {
    res, err := s.db.Exec(
        `UPDATE sandboxes SET name = ? WHERE id = ? AND created_by = ?`,
        newName, id, userID,
    )
    if err != nil {
        return err
    }
    n, _ := res.RowsAffected()
    if n == 0 {
        return fmt.Errorf("sandbox %q not found", id)
    }
    return nil
}
```

The `idx_sandboxes_user_name` partial unique index in
`pkg/store/store.go:215` —

```sql
CREATE UNIQUE INDEX IF NOT EXISTS idx_sandboxes_user_name
    ON sandboxes(created_by, name) WHERE status != 'destroyed'
```

— gives us all the constraint we need:

- Renaming to a name owned by a *destroyed* sandbox of the same user → **succeeds** (the index excludes destroyed rows). Same semantics as create.
- Renaming to a name owned by another user → **succeeds**. Names are namespaced per user.
- Renaming to a name owned by another live sandbox of the same user → **fails** with a `UNIQUE` constraint error, surfaced as 409.

After `foo → bar`, the name "foo" is immediately available for a new
sandbox — same as destroy-then-create. We do *not* honour "you can
never reuse a name that once existed." That's intentional and
matches the existing model.

A concurrent `PATCH name=foo` and `POST name=foo` are serialised by
the unique index — same mechanism that makes idempotent create work
today. Whichever loses gets a clean 409 (rename) or the existing
UNIQUE-rollback in the create handler.

---

## CLI

**`cmd/bhatti/sandbox_cmd.go`** — add `--name` to the existing `edit`
command. No new top-level command in v1.

```go
editCmd.Flags().String("name", "", "Rename sandbox")
```

In `RunE`, after reading the flag:

```go
newName, _ := cmd.Flags().GetString("name")
if newName != "" {
    req["name"] = newName
}
```

After a successful PATCH that included a rename, update the local
completion cache (`addToCompletionCache` and
`removeFromCompletionCache` already exist in `cmd/bhatti/cli.go`):

```go
if newName != "" && newName != args[0] {
    removeFromCompletionCache(args[0])
    addToCompletionCache(newName)
}
```

The cache is also rebuilt by every `bhatti ls`, so this is mostly
for the user who renames and immediately tab-completes. Two lines.

Add an example to the `editCmd.Long`:

```
  # Rename a sandbox
  bhatti edit dev --name dev-old
```

### CLI ergonomics — honest assessment

`bhatti edit foo --name bar` reads slightly oddly: the positional
argument is the *old* name and the flag is the *new* name. Compare
to `mv old new` / `git mv old new` / `docker rename old new`, which
all use two positionals. We're picking the inconsistency in exchange
for a one-line flag addition versus a fresh top-level cobra command
with its own `setupTiming`, resolver, JSON shape, and tests. If
discoverability turns out to matter, adding `bhatti rename` later as
a thin alias that calls into the same code path is two lines and
non-breaking. Defer.

---

## Tests

### Store — `pkg/store/store_test.go`

**`TestRenameSandbox_HappyPath`** — create "foo", rename to "bar",
verify `GetSandbox(user, "bar")` returns it and `GetSandbox(user,
"foo")` returns `sql.ErrNoRows`.

**`TestRenameSandbox_Conflict`** — two live sandboxes "foo" and
"bar"; rename "foo" → "bar" returns an error whose `.Error()`
contains "UNIQUE"; `foo` row is unchanged.

**`TestRenameSandbox_DestroyedNameReusable`** — destroy "foo",
rename a separate sandbox to "foo", succeeds.

**`TestRenameSandbox_OtherUserCannotRename`** — user A owns the
sandbox; `RenameSandbox(B, ...)` returns "not found" (rows-affected
zero).

### Handler — `pkg/server/sandbox_handlers_test.go`

**`TestPatchSandbox_Rename`** — PATCH `{"name":"new"}` returns 200
with the updated body; subsequent GET by new name works, GET by old
name returns 404.

**`TestPatchSandbox_RenameInvalidName`** — `{"name":"has spaces"}` → 400.

**`TestPatchSandbox_RenameConflict`** — two sandboxes; rename one to
the other's name → 409.

**`TestPatchSandbox_RenameAndKeepHot`** — PATCH
`{"name":"new","keep_hot":true}` applies both (note: two sequential
DB writes, not a transaction); returned body has both updated; one
`sandbox.updated` event with rename meta + one with keep_hot meta.

**`TestPatchSandbox_RenameSameName`** — PATCH with current name → 200,
no DB write, no event.

### CLI — `cmd/bhatti/cli_test.go`

**`TestEdit_Rename`** — `bhatti create --name a`, `bhatti edit a
--name b`, then `bhatti inspect b` succeeds and `bhatti inspect a`
errors. Completion cache contains `b`, not `a`.

---

## Docs

**`docs/api-reference.md`** — extend the PATCH section near line 84:

```markdown
PATCH /sandboxes/:id

Body: {"keep_hot": true, "name": "new-name"}

Toggles mutable sandbox properties. All fields are optional; supply
only the ones you want to change.

- `keep_hot` — prevent thermal transitions (see below).
- `name` — rename the sandbox. Must match `[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}`
  and be unique among the user's non-destroyed sandboxes; returns 409
  on conflict. The in-guest hostname is set at create time and is
  *not* changed by rename. Public URLs from `bhatti publish` keep
  their original alias and remain stable. Active shells, exec
  sessions, and websockets continue uninterrupted.
```

**`docs/cli-reference.md`** — add a `bhatti edit --name` example
under the existing `edit` entry.

---

## Implementation order

One PR, four commits for review clarity:

1. `pkg/store/sandbox.go`: `RenameSandbox` + store tests.
2. `pkg/server/sandbox_handlers.go`: PATCH extension + handler tests.
3. `cmd/bhatti/sandbox_cmd.go`: `--name` flag + completion cache + CLI test.
4. `docs/api-reference.md`, `docs/cli-reference.md`: docs.

---

## What's not in this plan

**Renaming the in-guest hostname.** Documented as a known limitation,
matches `docker rename`. Adding it later means a new agent op
(`pkg/agent/proto/constants.go`), a `lohar` handler that calls
`syscall.Sethostname` and rewrites `/etc/hosts`, and a client
wrapper. Half a day. No user has asked for it.

**Reflowing publish-rule aliases.** Already-published URLs stay
stable on rename. If the user wants a new alias they republish.

**A `SandboxRenamer` engine capability interface to keep the
in-memory `vm.Name` in sync.** Cosmetic only — limited to slog log
lines, self-heals on next daemon restart via `RestoreVM`. The cost
is a new interface in `pkg/engine`, an implementation in firecracker
with mutex handling, and a fresh surface that every engine
fake/mock has to think about. Revisit only if log confusion becomes
a real complaint.

**A typed `ErrNameTaken` sentinel error in the store.** The handler
matches the create-path convention of `strings.Contains(err.Error(),
"UNIQUE")`. Adding a sentinel for one call site is either
inconsistent with create or scope creep (refactor both).

**A separate `bhatti rename` command.** `bhatti edit --name` covers
the request; a verb alias is a non-breaking 2-line addition if
discoverability proves to matter.

**History of past names.** Not stored. The `sandbox.updated` event
with `old_name`/`new_name` in meta is the audit trail, queryable from
the events table.

**Renaming volumes, secrets, images, snapshots, templates.** Out of
scope. Each has its own object lifecycle and nobody has asked. The
same pattern would apply if and when they do.
