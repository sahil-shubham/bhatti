# Observability Plan

## Current State

**`/metrics`** — in-memory counters (`requestTotal`, `requestErrors`, `authFailures`),
point-in-time sandbox/user/thermal counts, host load + memory from `/proc`. Resets to
zero on every redeploy. Unauthenticated. Nothing consumes it — no Prometheus, no cron,
no external scraper.

**`PublicProxyHandler.Metrics()`** — in-memory counters for proxy traffic. Method exists
but nothing calls it. Not exposed anywhere.

**`/health`** — returns `{"status":"ok","sandboxes":12,"uptime":"4d"}`. Unauthenticated,
needed for monitoring probes. But `sandboxes` runs a full `ListAllSandboxes()` query on
every hit — a health probe shouldn't touch the database.

**slog JSON → stdout → journald** — good structured event names but no aggregation
beyond `journalctl`.

**Admin CLI** — `bhatti user create/list/delete/rotate-key`. No runtime visibility.

---

## Problem: `/metrics` Shouldn't Exist

`/metrics` is unauthenticated and leaks host infrastructure details, user counts,
auth failure counters, and sandbox topology to anyone who can reach the port. But the
fix isn't "move it behind auth" — the fix is to delete it. Nothing consumes it. The
in-memory counters it exposes reset on every restart. With `metrics_snapshots` persisting
to SQLite every 60 seconds and `bhatti admin` commands reading from there, the HTTP
endpoint is redundant. The SQLite data is strictly better — it survives restarts and
has history.

**Fix**: Delete the `/metrics` route, handler, unauthenticated bypass, and tests. The
in-memory `atomic.Int64` counters on `Server` and `PublicProxyHandler` stay — the
metrics snapshot goroutine reads them to compute deltas. They just stop being exposed
over HTTP.

Also slim `/health` down to `{"status":"ok","uptime":"4d"}` — drop the sandbox count.
A health probe that queries the database can fail for reasons unrelated to service
health. Sandbox counts belong in `bhatti admin status`.

---

## Design

Two things, no phases:

1. **Events table + metrics snapshots** — persist everything into SQLite
2. **`bhatti admin` CLI commands** — query it

All admin commands are **local-only** (direct SQLite, same as `bhatti user` today).
No remote admin API, no admin auth layer. The admin is always SSH'd into the server.

### What gets persisted

**Events table**: append-only log of every significant lifecycle action. One row per
event, JSON metadata blob.

**Metrics snapshots table**: gauge values (sandbox counts, host stats) + counter deltas
(requests in the last 60 seconds) dumped every 60 seconds. For time-series trend queries.

### What does NOT get its own event

Three categories that don't belong:

**`exec` events** — every exec call. The goal of observability is "how is bhatti being
used" — that's answered by lifecycle events (who creates sandboxes, what images, how long
they live, what gets published). Exec is the user's workload, not infrastructure behavior.
Knowing alice ran `npm install` 47 times doesn't change any operational decision. The
`metrics_snapshots` table already captures aggregate `api_requests` — counter deltas give
you req/min, which is the actual usage signal. At 170K rows/day, exec would be 97% of
write volume for data that doesn't matter. If ever needed, it's one `Record()` call to
add back.

**Generic `request` events** — every authenticated API request. This is what slog
already writes to journald, line by line, with method/path/status/duration/user. Storing
it again in SQLite doubles the write volume for data you can already `journalctl --grep`.

**`proxy.request` for every hit** — same reasoning. The metrics_snapshots table tracks
aggregate proxy request/error/wake counts. `proxy.error` gets its own event because
errors are rare and worth investigating individually. Logging every 200 OK proxy response
is noise.

### Storage math

Without exec/request/proxy events, the volume is small:

| Data | Rows/day | Bytes/row | MB/day |
|---|---|---|---|
| Lifecycle events | ~5K | ~300 | ~1.5 |
| metrics_snapshots | 1,440 | ~200 | ~0.3 |
| **Total** | | | **~2** |

Over 90 days retention: ~160 MB. Negligible on any hardware.

---

## Events Table

```sql
CREATE TABLE IF NOT EXISTS events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    ts TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    type TEXT NOT NULL,
    user_id TEXT NOT NULL DEFAULT '',
    sandbox_id TEXT NOT NULL DEFAULT '',
    meta TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_events_ts ON events(ts);
CREATE INDEX IF NOT EXISTS idx_events_type ON events(type, ts);
CREATE INDEX IF NOT EXISTS idx_events_user ON events(user_id, ts);
```

Compound index on `(type, ts)` for the common query pattern
`WHERE type = ? AND ts > ?`.

### Every event type

**Sandbox lifecycle:**

| Type | Trigger | Meta |
|---|---|---|
| `sandbox.created` | POST /sandboxes success | `name, cpus, memory_mb, image, template_id, keep_hot, volumes[]` |
| `sandbox.destroyed` | DELETE /sandboxes/:id | `name, lifetime_s` |
| `sandbox.stopped` | POST /sandboxes/:id/stop + thermal cold | `name, reason` ("api" / "thermal") |
| `sandbox.started` | POST /sandboxes/:id/start + cold→hot wake | `name, reason` ("api" / "thermal" / "proxy"), `wake_ms` |
| `sandbox.updated` | PATCH /sandboxes/:id | `name, keep_hot` |

Four code paths must be instrumented for started/stopped:
- `handleSandboxStop` — API stop, `reason: "api"`
- `runThermalCycle` warm→cold — thermal stop, `reason: "thermal"`
- `handleSandboxStart` — API start, `reason: "api"`
- `ensureHot` — thermal/proxy wake, `reason: "thermal"` or `"proxy"`

**Shell sessions:**

Record a single event at disconnect time (not connect + disconnect). At disconnect
you know both sides — the session existed and how long it lasted. Halves the row count
vs separate connect/disconnect events.

| Type | Trigger | Meta |
|---|---|---|
| `shell.session` | handleSandboxWS defer (after close) | `sandbox, session_id, reattach, duration_s` |
| `shell.web_session` | handleShellWS defer (after close) | `sandbox, ip, duration_s` |

**Thermal:**

| Type | Trigger | Meta |
|---|---|---|
| `thermal.pause` | runThermalCycle hot→warm | `sandbox, idle_s` |
| `thermal.snapshot` | runThermalCycle warm→cold success | `sandbox, idle_s` |
| `thermal.snapshot_failed` | runThermalCycle warm→cold failure | `sandbox, error, attempt, max_attempts` |
| `thermal.force_pause` | runThermalCycle agent unresponsive | `sandbox, consecutive_failures` |
| `thermal.wake` | ensureHot succeeds from cold/warm | `sandbox, from_state, wake_ms` |

**Snapshots:**

| Type | Trigger | Meta |
|---|---|---|
| `snapshot.created` | handleSandboxCheckpoint success | `name, sandbox, size_mb, duration_ms` |
| `snapshot.resumed` | handleSnapshotResume success | `name, new_sandbox, duration_ms` |
| `snapshot.deleted` | DELETE /snapshots/:name | `name` |

**Images:**

| Type | Trigger | Meta |
|---|---|---|
| `image.pulled` | Pull goroutine success | `ref, name, size_mb, digest` |
| `image.pull_failed` | Pull goroutine failure | `ref, error` |
| `image.imported` | handleImageImport success | `name, size_mb` |
| `image.saved` | handleSandboxSaveImage success | `name, source_sandbox, size_mb` |
| `image.deleted` | DELETE /images/:name | `name` |

**Volumes:**

| Type | Trigger | Meta |
|---|---|---|
| `volume.created` | POST /volumes success | `name, size_mb` |
| `volume.deleted` | DELETE /volumes/:name | `name` |
| `volume.resized` | POST /volumes/:name/resize | `name, old_mb, new_mb` |
| `volume.snapshot` | POST /volumes/:name/snapshot | `src, dst, size_mb` |
| `volume.backup_created` | performVolumeBackup success | `name, backup_id, size_bytes, s3_key` |
| `volume.backup_failed` | performVolumeBackup error | `name, error` |
| `volume.restored` | performVolumeRestore success | `name, backup_id` |

**Publishing:**

| Type | Trigger | Meta |
|---|---|---|
| `publish.created` | handlePublish success | `sandbox, port, alias, url` |
| `publish.deleted` | handleUnpublish success | `sandbox, port, alias` |

**Proxy (errors only):**

| Type | Trigger | Meta |
|---|---|---|
| `proxy.error` | proxyToAlias 5xx / error handler | `alias, status, error, duration_ms` |

**Auth:**

| Type | Trigger | Meta |
|---|---|---|
| `auth.failed` | ServeHTTP invalid key | `ip` |

**User management:**

| Type | Trigger | Meta |
|---|---|---|
| `user.created` | bhatti user create | `name, max_sandboxes, subnet_index` |
| `user.deleted` | bhatti user delete | `name` |
| `user.key_rotated` | bhatti user rotate-key | `name` |

**Daemon:**

| Type | Trigger | Meta |
|---|---|---|
| `daemon.started` | runDaemon after recovery completes | `version, recovered_vms, listen` |
| `daemon.shutdown` | SnapshotAll completes | `snapshotted, failed, signal` |

---

## Metrics Snapshots Table

```sql
CREATE TABLE IF NOT EXISTS metrics_snapshots (
    ts TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    api_requests INTEGER NOT NULL DEFAULT 0,
    api_errors INTEGER NOT NULL DEFAULT 0,
    api_auth_failures INTEGER NOT NULL DEFAULT 0,
    api_rate_limited INTEGER NOT NULL DEFAULT 0,
    proxy_requests INTEGER NOT NULL DEFAULT 0,
    proxy_errors INTEGER NOT NULL DEFAULT 0,
    proxy_cold_wakes INTEGER NOT NULL DEFAULT 0,
    proxy_rate_limited INTEGER NOT NULL DEFAULT 0,
    events_dropped INTEGER NOT NULL DEFAULT 0,
    sandboxes_total INTEGER NOT NULL DEFAULT 0,
    sandboxes_hot INTEGER NOT NULL DEFAULT 0,
    sandboxes_warm INTEGER NOT NULL DEFAULT 0,
    sandboxes_cold INTEGER NOT NULL DEFAULT 0,
    users_total INTEGER NOT NULL DEFAULT 0,
    users_active INTEGER NOT NULL DEFAULT 0,
    websockets_active INTEGER NOT NULL DEFAULT 0,
    host_load_1m REAL NOT NULL DEFAULT 0,
    host_mem_total_mb INTEGER NOT NULL DEFAULT 0,
    host_mem_avail_mb INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_ms_ts ON metrics_snapshots(ts);
```

Counter columns (`api_requests`, `proxy_requests`, etc.) store **deltas since the
previous snapshot**, not absolute values. Each row says "in the last 60 seconds, there
were N requests." This eliminates the restart problem — no negative deltas to handle,
no lost data at restart boundaries. The snapshot goroutine reads the current atomic
counter, computes `current - previous`, stores the delta, and saves `current` as the
new previous. On startup, `previous` initializes to the current counter values (which
are 0), so the first snapshot is correct.

Gauge columns (`sandboxes_total`, `host_load_1m`, etc.) are point-in-time values as
before.

60-second snapshot interval. 1,440 rows/day, 43,200 rows over 30 days. Aggregate
queries over this dataset are sub-millisecond with the timestamp index.

---

## Retention

One background goroutine, runs hourly:

```
DELETE FROM events WHERE ts < datetime('now', '-90 days');
DELETE FROM metrics_snapshots WHERE ts < datetime('now', '-30 days');
```

No vacuum needed. At ~2 MB/day write volume the database stabilizes well under 200 MB.
SQLite reuses freed pages internally — the file won't shrink but it won't grow past
steady state either. If file size ever becomes a concern (it won't at this volume),
add `auto_vacuum=INCREMENTAL` and a one-time `VACUUM` then.

Started in `runDaemon` alongside the thermal manager and task cleanup.

---

## EventRecorder

Buffered, non-blocking, batched:

```go
type EventRecorder struct {
    store   *store.Store
    ch      chan store.Event
    done    chan struct{}
    dropped atomic.Int64 // exposed to metrics snapshot goroutine
}

func (r *EventRecorder) Record(e store.Event) {
    select {
    case r.ch <- e:
    default:
        // Buffer full — drop rather than block the API request.
        r.dropped.Add(1)
    }
}
```

Channel buffer: 1,000. Background goroutine drains into batched transactions
(one `BEGIN`/`COMMIT` per batch). Flushes every 500ms or at 100 events, whichever
first.

Writes go through WAL mode, same as all other store operations. A batch of 100
INSERT statements in one transaction takes <1ms on SQLite with WAL. No contention
with the main request path.

**Meta JSON validation**: In the flush loop, `json.Marshal` the meta map. If marshal
fails (shouldn't happen with `map[string]any`, but defensively), store
`{"error":"marshal_failed"}` rather than dropping the event.

The recorder lives on `Server` and gets wired into `runDaemon`:

```go
srv.events = NewEventRecorder(st)
defer srv.events.Close()  // drains remaining events on shutdown
```

Each instrumentation site is one call:

```go
s.events.Record(store.Event{
    Type: "sandbox.created", UserID: user.ID, SandboxID: sb.ID,
    Meta: map[string]any{"name": sb.Name, "cpus": spec.CPUs, "memory_mb": spec.MemoryMB},
})
```

The `dropped` counter is read by the metrics snapshot goroutine and stored as the
`events_dropped` delta in each snapshot row. If this is ever non-zero, the channel
buffer should be increased.

---

## `/metrics` and `/health` Changes

### Delete `/metrics`

Remove:
- `handleMetrics` handler in `routes.go`
- The route registration `s.mux.HandleFunc("/metrics", s.handleMetrics)`
- The unauthenticated bypass: change `if cleanPath == "/health" || cleanPath == "/metrics"`
  to just `if cleanPath == "/health"`
- Tests asserting `/metrics` works without auth
- The OpenAPI spec entry for `/metrics`

Keep the in-memory counters on `Server` (`requestTotal`, `requestErrors`, `authFailures`)
and on `PublicProxyHandler` (`requestsTotal`, `requestsError`, `coldWakes`, etc.). The
metrics snapshot goroutine reads these. They just stop being exposed over HTTP.

### Slim `/health`

Remove the `ListAllSandboxes()` call. Health probes should not touch the database.

```go
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
    writeJSON(w, 200, map[string]any{
        "status": "ok",
        "uptime": time.Since(s.startTime).Round(time.Second).String(),
    })
}
```

Sandbox counts belong in `bhatti admin status`.

---

## CLI: `bhatti admin`

All commands are **local-only** — they open SQLite directly using `openLocalStore()`,
same as `bhatti user list`. Run on the server, not remotely. No API endpoints, no
admin auth.

### `bhatti admin status`

One-shot overview. Reads the latest metrics snapshot + recent events from SQLite.

```
$ bhatti admin status

Bhatti v1.2.0 — up 4d 12h 30m
Host: 0.45 load, 5.1 / 8.0 GB memory

Sandboxes  12 total (3 hot, 4 warm, 5 cold)
Users      4 total, 3 active
API        84,521 requests, 12 errors, 3 auth failures
Proxy      45,000 requests, 120 errors, 340 cold wakes

USER         SANDBOXES  STATUS
alice        3/5        2 hot, 1 cold
bob          2/5        1 warm, 1 cold
ci-runner    0/10       -

RECENT EVENTS
14:30  sandbox.created      alice     dev-feature-x
14:28  thermal.wake         bob       api-server (52ms cold→hot)
14:15  thermal.snapshot_failed         dev-main (timeout, attempt 2)
```

`--json` for machine consumption.

The "API 84,521 requests" line sums the `api_requests` deltas across all
metrics_snapshots rows. Since each row is already a delta, `SUM(api_requests)` gives
the total since retention began. Same for errors, proxy counts, etc.

### `bhatti admin events`

```
bhatti admin events                                  # last 50
bhatti admin events --type sandbox.created           # by type
bhatti admin events --user alice                     # by user
bhatti admin events --sandbox dev                    # by sandbox name or id
bhatti admin events --since 24h                      # relative time
bhatti admin events --since 2026-04-01               # absolute date
bhatti admin events --type thermal --since 7d        # combine
bhatti admin events --limit 200                      # more
bhatti admin events --json                           # raw JSON
bhatti admin events --type thermal --since 1h --count   # just the count
```

Human-readable default:

```
TS                   TYPE                 USER        SANDBOX         DETAILS
2026-04-09 14:30:12  sandbox.created      alice       dev-feature-x   cpus=2 mem=1024 image=python-312
2026-04-09 14:31:00  thermal.pause        -           dev-feature-x   idle=30s
2026-04-09 14:35:00  shell.session        alice       dev-feature-x   reattach=true 12m30s
```

`DETAILS` column is a type-specific one-line summary from the meta JSON.

`--type` supports prefix matching: `--type thermal` matches all `thermal.*` events,
`--type sandbox` matches all `sandbox.*` events.

### `bhatti admin metrics`

```
bhatti admin metrics                    # last hour, 1-min buckets
bhatti admin metrics --since 24h        # 15-min buckets
bhatti admin metrics --since 7d         # 1-hr buckets
bhatti admin metrics --json             # raw snapshots
```

```
TIME              REQ/min  ERR  PROXY/min  WAKES  HOT  WARM  COLD  LOAD  MEM_AVAIL
14:00             12       0    85         2      3    4     5     0.4   5100 MB
14:15             8        0    42         0      3    4     5     0.3   5200 MB
14:30             15       1    120        5      4    3     5     0.5   4900 MB
```

Auto-selects bucket size from time range. Rates computed by summing deltas within each
bucket and dividing by bucket duration.

---

## Implementation Order

**First: persistence + cleanup**
- Events table + EventRecorder + batched flush goroutine
- Metrics snapshots table + 60s snapshot goroutine (delta-based counters)
- Retention goroutine (hourly)
- Instrument all event sites above
- Delete `/metrics` route, handler, bypass, and tests
- Slim `/health` (remove `ListAllSandboxes()`)

**Second: CLI**
- `bhatti admin status`
- `bhatti admin events`
- `bhatti admin metrics`
