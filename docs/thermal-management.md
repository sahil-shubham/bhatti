> [!WARNING]
> **DEPRECATED — do not edit.**
> The canonical, maintained version of this page is at
> <https://bhatti.sh/docs/under-the-hood/thermal-states/>.
> This file is kept only for git history and may be removed in a future
> cleanup. See [`docs/README.md`](./README.md) for the redirect index.

---

# Thermal Management

Bhatti manages VM resources automatically through three thermal states. The consumer never sees this — from the API's perspective, every sandbox is always "running." Behind the scenes, idle VMs progressively release resources and transparently restore when needed.

## The Three States

```
             idle 30s              idle 30min
    Hot ──────────────► Warm ──────────────► Cold
     ▲     ~400µs        ▲     ~50ms          │
     │                    │                    │
     └────────────────────┴────────────────────┘
              any API request (ensureHot)
```

| State | Firecracker process | vCPUs | Host RAM | Resume latency | When |
|-------|-------------------|-------|----------|----------------|------|
| **Hot** | alive | running | allocated | — | actively used |
| **Warm** | alive | paused | allocated | ~400µs | idle < 30 min |
| **Cold** | dead | — | freed | ~50ms | idle > 30 min |

### Hot → Warm

When no sessions are attached and the VM has been idle for 30 seconds, the thermal manager sends `PATCH /vm {"state":"Paused"}` to Firecracker's API. This freezes all vCPUs. The FC process stays alive, memory stays allocated, but the VM consumes zero CPU cycles.

Resume is a single `PATCH /vm {"state":"Resumed"}` — under 400 microseconds.

### Warm → Cold

When a warm VM has been idle for 30 minutes, the thermal manager:

1. Creates a memory snapshot (full or diff — see below)
2. Kills the Firecracker process
3. Host RAM is freed

The TAP device is *not* destroyed — the snapshot contains virtio-net state that references it. Destroying and recreating the TAP would break networking after restore.

### Cold → Hot

When any API request targets a cold VM:

1. A new Firecracker process is started
2. The snapshot is loaded with `PUT /snapshot/load`
3. The VM resumes with all processes, memory, and network state intact
4. A new TCP `AgentClient` is created (the old vsock connection is dead)
5. The host waits for the agent to respond

Total time: ~50ms. The API caller sees nothing — `ensureHot()` runs before every operation.

## ensureHot()

Every API operation that touches a VM calls `ensureHot()` first. It reads the thermal state, and if the VM isn't hot, it resumes or restores it:

```go
func (e *Engine) EnsureHot(ctx context.Context, id string) error {
    vm.stateMu.Lock()
    thermal := vm.Thermal
    vm.stateMu.Unlock()

    switch thermal {
    case "hot":  return nil
    case "warm": return e.Resume(ctx, id)   // ~400µs
    case "cold": return e.Start(ctx, id)    // ~50ms
    }
    return nil
}
```

Note the lock discipline: read state under lock, release, then call Resume/Start (which acquire their own lock). No nested locking.

The server layer also touches a host-side activity cache when calling `ensureHot()`, recording that this sandbox was recently accessed.

## keep_hot: Opting Out of Thermal Management

Sandboxes with `keep_hot: true` are skipped entirely by the thermal cycle. The VM stays hot regardless of idle time. This is for autonomous agents that maintain persistent external connections (WebSocket to Slack, Discord gateway, etc.) that would die if the VM's vCPUs were paused.

```bash
# At creation time
bhatti create --name agent --init "hermes gateway" --keep-hot

# Toggle on an existing sandbox
bhatti edit agent --keep-hot
bhatti edit agent --allow-cold
```

```
PATCH /sandboxes/:id
{"keep_hot": true}
```

The flag is stored in SQLite and checked before any thermal evaluation. Hot VMs with `keep_hot` consume their allocated memory and CPU continuously.

## The Thermal Cycle

A background goroutine ticks every 10 seconds and evaluates each sandbox:

```go
func (s *Server) runThermalCycle(te ThermalEngine, cfg ThermalConfig) {
    for _, sb := range sandboxes {
        thermal := te.ThermalState(sb.EngineID)

        // Fast path: check host-side activity cache
        if lastAPIActivity < warmTimeout {
            continue  // definitely active, skip agent query
        }

        // Slow path: ask the guest agent
        activity := te.Activity(sb.EngineID)
        idle := time.Since(activity.LastActivityUnix)

        if thermal == "hot" && idle > 30s && noAttachedSessions {
            te.Pause(sb.EngineID)    // hot → warm
        }
        if thermal == "warm" && idle > 30min {
            engine.Stop(sb.EngineID) // warm → cold (snapshot + kill)
        }
    }
}
```

### Host-Side Activity Cache

The thermal cycle needs to know if a sandbox is idle. The authoritative source is the guest agent (it tracks `lastActivity` as an atomic int64, updated on every exec and stdin). But querying the agent means opening a TCP connection — one per sandbox per cycle.

With 50 sandboxes, that's 50 TCP connections every 10 seconds, most of which return "yes, still idle."

The optimization: the server maintains a `sync.Map` of `engineID → time.Time` recording the last API-level activity. Before querying the agent, the thermal cycle checks this cache. If the sandbox had API activity within the warm timeout, the agent query is skipped entirely.

```
Before: 50 sandboxes = 50 TCP connections every 10 seconds
After:  only idle sandboxes get queried
```

This is a host-side heuristic, not a replacement for the guest-side truth. If the sandbox is doing work that doesn't go through the API (e.g., a cron job inside the VM), the agent query catches it on the slow path.

## Diff Snapshots

The first `Stop()` creates a **full snapshot** — every memory page is written to disk. For a 512MB VM on a Pi 5 with NVMe, this takes ~4.4 seconds.

Subsequent `Stop()` calls create **diff snapshots** — only pages modified since the last snapshot are written. For an idle VM, this is 10-50MB instead of 512MB, taking ~52ms.

This requires two Firecracker features:
- `track_dirty_pages: true` in the machine config (enables the dirty page bitmap)
- `enable_diff_snapshots: true` on snapshot load (re-enables tracking after restore)

### Fallback

If the base snapshot file is missing (deleted, disk corruption), `Stop()` logs a warning and falls back to a full snapshot. The `hasBaseSnapshot` flag tracks whether diff is available:

```go
snapshotType := "Full"
if vm.hasBaseSnapshot {
    if _, err := os.Stat(vm.SnapMemPath); err != nil {
        slog.Warn("base snapshot missing, falling back to full")
        vm.hasBaseSnapshot = false
    } else {
        snapshotType = "Diff"
    }
}
```

After a full snapshot, `hasBaseSnapshot` is set to true, and subsequent stops use diff. This flag is persisted through `VMState`/`RestoreVM` so it survives daemon restarts.

### Performance Impact

```
Full snapshot (512MB VM):     ~4.4s, writes 512MB
Diff snapshot (idle VM):      ~52ms, writes ~10-50MB
Diff snapshot (active VM):    varies with dirty pages
```

The thermal manager's warm → cold transition uses diff snapshots. Since the VM has been paused (warm) for 30 minutes with no activity, the diff is minimal — almost all pages are clean.

## Why TCP, Not Vsock, After Restore

Firecracker exposes vsock as a Unix domain socket on the host. During cold boot, the host connects through this socket with a `CONNECT <port>\n` / `OK <port>\n` handshake. This works perfectly.

After snapshot/restore, it breaks. The guest kernel's vsock state is stale — connections complete the Firecracker-side handshake but never reach the guest agent. This was confirmed independently:

- Tested with kernel 5.10 and 6.1
- Tested with Firecracker 1.6.0
- SlicerVM (a production Firecracker orchestrator) has the same issue — their suspend/restore was unshipped as of v0.1.108
- Firecracker PR #5688 ("minimize local port collisions after snapshot restore") doesn't fully resolve it

The fix: after restore, create a new `AgentClient` that uses TCP over the TAP network instead of vsock. Virtio-net (the virtual network card) survives snapshot/restore cleanly — the guest kernel's TCP stack re-establishes connections through the existing TAP device and bridge.

Lohar listens on both vsock *and* TCP on the same ports (1024/1025). Cold boot uses whichever connects first (vsock is slightly faster). Post-restore always uses TCP.

## Port Scanning

A background goroutine polls running sandboxes every 3 seconds for listening ports (`ss -tln`). New ports get automatic TCP forwards through the ProxyManager. Stale forwards (port no longer listening) are cleaned up.

Port scanning skips non-hot VMs — it won't wake a warm or cold sandbox just to check ports. This respects the thermal model.
