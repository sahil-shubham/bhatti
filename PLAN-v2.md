# Bhatti v2 — Infrastructure Improvements + New Features

Informed by research into Slicer (production Firecracker orchestrator),
E2B (cloud sandbox platform), and Sprites (fly.io persistent VMs).

---

## Phase 1: Operational Reliability

These are blocking daily use. Must be solid before anything else.

### 1.1 TAP Device Cleanup

**Problem**: Failed VM creations or unclean shutdowns leak TAP devices.
Subsequent creates fail due to IP conflicts. Manual cleanup required.

**Solution**:
- Engine tracks all TAP devices in memory and SQLite
- On Destroy: always clean up TAP (even if FC process is already dead)
- On server startup: scan for orphaned TAP devices (prefixed `tap*` with
  no matching running VM) and delete them
- On engine shutdown (SIGTERM): clean up all TAPs
- Add systemd `ExecStopPost` to clean up if bhatti crashes

**Scope**: ~50 lines in `network.go` + `engine.go` + systemd unit update.

### 1.2 Agent Auth Token

**Problem**: Anyone who can reach the VM's TCP port (1024) can exec arbitrary
commands. No authentication between host and agent.

**Solution**:
- Generate a random 32-byte token per sandbox at creation time
- Pass the token to the VM via kernel cmdline (`bhatti.token=<hex>`)
  or via the config drive (1.3)
- Agent reads the token at boot, requires it as the first frame after
  connection (new `AUTH` frame type, 0x11)
- Client sends AUTH frame immediately after connecting, before EXEC_REQ
- If auth fails, agent closes the connection

**Wire change**: New frame type AUTH (0x11). Backward compatible — old agents
ignore unknown frame types, old clients won't send AUTH. New agents require it.

**Scope**: ~30 lines in agent, ~15 in client, token generation in engine.

### 1.3 Config Drive (Second virtio-blk)

**Problem**: Per-sandbox configuration (env vars, auth token, DNS, hostname)
is injected via exec-after-boot which is fragile and slow.

**Solution** (learned from Slicer's `/runner/` drive):
- Create a tiny ext4 image (1MB) per sandbox at creation time
- Contents: `config.json` with token, env vars, hostname, DNS servers
- Attach as second drive (`/dev/vdb`) in Firecracker config
- Agent mounts it at `/run/bhatti/config` on boot, reads config
- Replaces: kernel cmdline IP config (move to config), env injection via exec,
  resolv.conf creation, hostname setting

**Config drive contents**:
```json
{
  "hostname": "sandbox-abc123",
  "token": "a1b2c3d4...",
  "env": {"ANTHROPIC_API_KEY": "sk-...", "GITHUB_TOKEN": "ghp_..."},
  "dns": ["1.1.1.1", "8.8.8.8"],
  "ip": "172.16.0.6",
  "gateway": "172.16.0.5",
  "netmask": "255.255.255.252"
}
```

**Scope**: ~100 lines. New file `configdrive.go` in engine package. Agent
changes: mount /dev/vdb, read JSON, apply config.

### 1.4 Bridge Networking

**Problem**: Per-VM TAP + iptables NAT is fragile, verbose, and leaks.
Each sandbox creates 1 TAP + 2 iptables rules. Cleanup is error-prone.

**Solution** (learned from Slicer's bridge mode):
- Create ONE bridge device (`brbhatti0`) at engine startup
- Assign bridge IP (e.g., `192.168.137.1/24`) — acts as gateway for all VMs
- ONE masquerade rule for the bridge subnet
- Per-VM: create TAP, add it to the bridge. No per-VM iptables.
- Guest IPs allocated from the bridge's /24 pool (252 sandboxes max)
- Cleanup: delete TAP from bridge. Bridge and masquerade rule stay.

**Benefits**:
- VMs can talk to each other (bridge provides L2 connectivity)
- One iptables rule total (not 2 per VM)
- Simpler cleanup (just delete TAP, no iptables to track)
- Bridge survives server restart (no need to recreate iptables)

**Scope**: Rewrite `network.go` (~150 lines). Engine startup creates bridge.
Per-VM: create TAP + add to bridge. Destroy: remove TAP.

---

## Phase 2: Golden Snapshots (Snapshot as Template)

**Problem**: Every new sandbox cold-boots from the base rootfs. Installing
tools (npm install, pip install) takes minutes and repeats every time.

**Solution** (learned from E2B's snapshots and Sprites' checkpoints):

### 2.1 Create Snapshot from Running Sandbox

- New API: `POST /sandboxes/:id/snapshot` → creates a named snapshot
- Implementation: pause VM, create Firecracker snapshot (mem + VM state),
  copy rootfs, resume VM. The original sandbox keeps running.
- Store: new `snapshots` table with id, name, sandbox_id, mem_path,
  vm_path, rootfs_path, created_at

### 2.2 Create Sandbox from Snapshot

- Modified `POST /sandboxes` accepts `snapshot_id` instead of `template_id`
- Implementation: copy snapshot's rootfs + mem + vm files, create new FC
  process, load snapshot. Instant boot with all state from the snapshot.
- This is E2B's one-to-many: one snapshot → many sandboxes

### 2.3 Workflow

```bash
# 1. Create a sandbox, install everything
POST /sandboxes {"name":"setup","template_id":"dev"}
POST /sandboxes/:id/exec {"cmd":["npm","install","-g","typescript"]}
POST /sandboxes/:id/exec {"cmd":["pip","install","requests","flask"]}

# 2. Snapshot it as a "golden image"
POST /sandboxes/:id/snapshot {"name":"dev-ready"}

# 3. Future sandboxes start from the snapshot — instant, pre-configured
POST /sandboxes {"name":"agent-1","snapshot_id":"<snapshot-id>"}
POST /sandboxes {"name":"agent-2","snapshot_id":"<snapshot-id>"}
```

**Scope**: ~200 lines. New snapshot store methods, new API route, engine
method for snapshot creation and restore.

---

## Phase 3: Auto-Idle / Auto-Resume

**Problem**: Sandboxes consume RAM and CPU even when idle. Manual stop/start
is friction. Users forget to stop sandboxes.

**Solution** (learned from Sprites' auto-hibernation and E2B's AutoResume):

### 3.1 Activity Tracking

- Agent tracks last activity timestamp (exec, shell, stdin, TCP connection)
- Host polls activity via a lightweight API call (or agent reports via metrics)
- Configurable idle timeout (default: 5 minutes)

### 3.2 Auto-Pause on Idle

- When idle timeout expires, server automatically calls Stop (snapshot)
- Sandbox status changes to "sleeping" (distinct from manual "stopped")
- VM process is killed, RAM freed, only disk used

### 3.3 Auto-Resume on Access

- Any API call to a sleeping sandbox triggers automatic Start (resume)
- The caller blocks until resume completes (~2-3 seconds)
- From the caller's perspective, the sandbox was always running — just slow
- Terminal WebSocket reconnects automatically after resume

### 3.4 Transparent to the User

The user creates a sandbox and uses it. They don't think about pause/resume.
The sandbox manages its own lifecycle:

```
Create → Active → [idle 5min] → Sleeping → [API call] → Active → ...
```

**Scope**: Activity tracking in agent (~30 lines), auto-pause goroutine in
server (~50 lines), auto-resume middleware in route handlers (~30 lines),
UI WebSocket reconnect (~20 lines JS).

---

## Phase 4: UX Polish

### 4.1 Terminal WebSocket Reconnect

**Problem**: After stop/start, the terminal shows "running" but the WebSocket
is dead. Manual page reload required.

**Solution**: UI detects WebSocket close, shows "reconnecting..." overlay,
auto-reconnects when sandbox status returns to "running". Exponential backoff.

### 4.2 Real-time Port Detection

**Problem**: Port detection polls via `ss -tln` every few seconds. Slow, not
real-time.

**Solution** (learned from Sprites): Agent watches `/proc/net/tcp` or uses
inotify/netlink for port change events. Sends port notifications to the host
via a persistent connection. UI updates instantly when a port opens.

### 4.3 Filesystem API

**Problem**: Reading/writing files requires exec + cat/echo. Clunky for
programmatic use.

**Solution** (learned from Sprites and E2B):
- `GET /sandboxes/:id/files?path=/foo` → file contents
- `PUT /sandboxes/:id/files?path=/foo` → write file (body = content)
- `DELETE /sandboxes/:id/files?path=/foo` → delete
- Implemented via agent: new frame types FILE_READ, FILE_WRITE, etc.

### 4.4 UI Improvements

- Real-time status updates via WebSocket (not polling)
- Sandbox metrics (CPU, memory, disk) in the detail panel
- Terminal reconnect on resume
- Port pill links that actually work from outside the Pi
- Better create flow with snapshot selection

---

## Phase 5: Access & Distribution

### 5.1 Remote Access (Cloudflare Tunnel)

**Problem**: bhatti only accessible from local network.

**Solution**: Cloudflare Tunnel (free tier). One command:
`cloudflared tunnel --url http://localhost:8080`
Gives a public HTTPS URL. Add to systemd for persistence.

### 5.2 Per-Sandbox Public URLs

**Problem**: Accessing services inside VMs requires going through bhatti's
proxy endpoint.

**Solution** (learned from Sprites): Each sandbox gets a URL pattern:
`<port>-<sandbox-id>.bhatti.yourdomain.com`. Caddy or nginx reverse proxy
with wildcard DNS. Requires Cloudflare Tunnel or similar for external access.

### 5.3 CLI Tool

A `bhatti` CLI that talks to the REST API:
```bash
bhatti create --template dev --name my-agent
bhatti exec my-agent -- npm install
bhatti shell my-agent
bhatti snapshot my-agent --name "ready"
bhatti create --snapshot ready --name agent-2
bhatti list
bhatti destroy my-agent
```

### 5.4 OCI Images

Replace raw ext4 copy with containerd-based image management. Enables
versioned images, delta updates, and instant clones via snapshotter.

---

## Implementation Order

```
Phase 1 (reliability):
  1.1 TAP cleanup        ← essential, do first
  1.4 Bridge networking   ← simplifies everything after
  1.2 Agent auth token    ← security, before multi-user
  1.3 Config drive        ← clean config injection

Phase 2 (golden snapshots):
  2.1 Create snapshot     ← enables template workflow
  2.2 Create from snapshot
  2.3 API + UI integration

Phase 3 (auto-idle):
  3.1 Activity tracking
  3.2 Auto-pause
  3.3 Auto-resume
  3.4 UI integration

Phase 4 (UX):
  4.1 Terminal reconnect  ← can do anytime
  4.2 Port notifications
  4.3 Filesystem API
  4.4 UI improvements

Phase 5 (access):
  5.1 Cloudflare Tunnel   ← can do anytime
  5.2 Per-sandbox URLs
  5.3 CLI tool
  5.4 OCI images
```

Phase 1 is ~1-2 days of focused work. Phase 2 is ~1 day.
Phase 3 is ~1 day. These three phases get bhatti to feature
parity with the core of what E2B and Sprites offer, while
retaining the self-hosted + full memory snapshot advantages.
