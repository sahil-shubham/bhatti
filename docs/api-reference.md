> [!WARNING]
> **DEPRECATED — do not edit.**
> The canonical, maintained version of this page is at
> <https://bhatti.sh/docs/reference/api/>.
> This file is kept only for git history and may be removed in a future
> cleanup. See [`docs/README.md`](./README.md) for the redirect index.

---

# API Reference

All endpoints require `Authorization: Bearer <token>` except `/health` and static paths (`/`, `/index.html`, `/static/*`).

Base URL: `http://localhost:8080` (configurable via `listen` in config.yaml)

## Health

```
GET /health
```

No auth required. Returns status, sandbox count, and uptime.

```json
{"status": "ok", "sandboxes": 3, "uptime": "2h15m30s"}
```

## Sandboxes

### Create

```
POST /sandboxes
```

**Direct creation (no template):**

```json
{
  "name": "dev",
  "cpus": 2,
  "memory_mb": 1024,
  "env": {"API_KEY": "sk-..."},
  "init": "cd /workspace && npm install",
  "keep_hot": false,
  "new_volumes": [{"name": "work", "size_mb": 256, "mount": "/workspace"}]
}
```

All fields optional. Defaults: 1 CPU, 512MB RAM, auto-generated name.

`keep_hot` prevents the thermal manager from pausing or snapshotting this sandbox. Use for autonomous agents that maintain persistent external connections (e.g. Slack WebSocket). Can be toggled on existing sandboxes via PATCH.

**Template-based creation:**

```json
{
  "template_id": "tmpl-abc123",
  "name": "dev",
  "env": {"API_KEY": "sk-..."}
}
```

Response (201):

```json
{
  "id": "a1b2c3d4",
  "name": "dev",
  "status": "running",
  "ip": "192.168.137.2",
  "created_at": "2026-03-22T10:00:00Z"
}
```

### List

```
GET /sandboxes
```

Response: array of sandbox objects.

### Get

```
GET /sandboxes/:id
```

### Update

```
PATCH /sandboxes/:id
```

```json
{"keep_hot": true, "name": "new-name"}
```

Toggles mutable sandbox properties. All fields are optional; supply only the ones you want to change. Returns the updated sandbox object.

- `keep_hot` — prevent thermal transitions (see Thermal management).
- `name` — rename the sandbox. Must match `[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}` and be unique among the user's non-destroyed sandboxes; returns 409 on conflict. The in-guest hostname is set at create time and is *not* changed by rename. Public URLs from `bhatti publish` keep their original alias and remain stable. Active shells, exec sessions, and websockets continue uninterrupted.

### Destroy

```
DELETE /sandboxes/:id
```

### Stop (Snapshot)

```
POST /sandboxes/:id/stop
```

Snapshots the VM to disk. First stop creates a full snapshot; subsequent stops create diff snapshots (dirty pages only). Returns the updated sandbox object.

### Start (Resume)

```
POST /sandboxes/:id/start
```

Restores from snapshot. Returns the updated sandbox object.

## Exec

```
POST /sandboxes/:id/exec
```

```json
{"cmd": ["echo", "hello"]}
```

**Buffered response** (default):

```json
{"exit_code": 0, "stdout": "hello\n", "stderr": ""}
```

**Streaming response** (with `Accept: application/x-ndjson`):

```bash
curl -N -H "Accept: application/x-ndjson" \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"cmd":["npm","install"]}' \
  http://localhost:8080/sandboxes/$ID/exec
```

```json
{"type":"stdout","data":"Installing dependencies...\n"}
{"type":"stderr","data":"npm warn deprecated ...\n"}
{"type":"stdout","data":"added 847 packages in 12s\n"}
{"type":"exit","exit_code":0}
```

Each line is flushed immediately. Useful for long-running commands where the consumer wants real-time output.

## WebSocket Shell

```
GET /sandboxes/:id/ws
```

Upgrade to WebSocket. Auth via query param (`?token=...`) or `Authorization` header.

**Terminal → WebSocket:** binary messages containing raw terminal output.

**WebSocket → Terminal:** binary messages forwarded as keystrokes. Text messages with JSON `{"type":"resize","rows":N,"cols":N}` trigger terminal resize.

## Files

### Read

```
GET /sandboxes/:id/files?path=/workspace/app.js
```

Returns raw file content with `Content-Type: application/octet-stream`.

Headers:
- `Content-Length` — file size (omitted for truncated reads)
- `X-File-Size` — total file size (always present, lets client detect truncation)

**Server-side truncation:**

```
GET /sandboxes/:id/files?path=/app.log&offset=1&limit=2000&max_bytes=51200
```

- `offset` — 1-indexed line number to start from
- `limit` — max lines to return
- `max_bytes` — max bytes to return

Whichever limit hits first stops the read.

### Write

```
PUT /sandboxes/:id/files?path=/workspace/app.js
```

Body: raw file content. `Content-Length` header required (rejects chunked/unknown).

Optional query: `mode=0644` (default: `0644`).

Writes are atomic (temp file + rename). Concurrent readers never see partial content.

### Stat

```
HEAD /sandboxes/:id/files?path=/workspace/app.js
```

Response headers:
- `X-File-Size` — file size in bytes
- `X-File-Mode` — permissions (e.g., `0644`)
- `X-File-IsDir` — `true` or `false`

### List Directory

```
GET /sandboxes/:id/files?path=/workspace&ls=true
```

```json
[
  {"name": "app.js", "size": 1234, "mode": "0644", "is_dir": false, "mtime": 1711100000},
  {"name": "node_modules", "size": 4096, "mode": "0755", "is_dir": true, "mtime": 1711100000}
]
```

Capped at 10,000 entries. If truncated, a sentinel entry indicates the total count.

## Sessions

```
GET /sandboxes/:id/sessions
```

```json
[
  {"session_id": "init", "argv": "npm install", "tty": true, "running": true, "attached": false, "created_at": 1711100000},
  {"session_id": "s1", "argv": "/bin/zsh -li", "tty": true, "running": true, "attached": true, "created_at": 1711100100}
]
```

## Ports

```
GET /sandboxes/:id/ports
```

```json
[
  {"container_port": 3000, "proxy_url": "/sandboxes/abc123/proxy/3000/", "host_port": 49152}
]
```

```
GET /ports
```

All listening ports across all sandboxes.

## Reverse Proxy

```
ANY /sandboxes/:id/proxy/:port/*path
```

HTTP requests and WebSocket connections are tunneled through the engine into the sandbox. The request is rewritten to target `localhost:<port>` inside the VM.

## Publish (Public Preview URLs)

### Publish a Port

```
POST /sandboxes/:id/publish
```

```json
{"port": 3000, "alias": "my-app"}
```

`alias` is optional. If omitted, an alias is auto-generated from the sandbox name with a random suffix (e.g. `dev-k3m9x2`).

Response (201):

```json
{
  "id": "pub_a1b2c3d4",
  "sandbox_id": "a1b2c3d4",
  "port": 3000,
  "alias": "my-app",
  "url": "https://my-app.bhatti.sh",
  "created_at": "2026-03-30T17:00:00Z"
}
```

The URL is publicly accessible without authentication. The sandbox wakes automatically from any thermal state when a request arrives.

### List Published Ports

```
GET /sandboxes/:id/publish
```

Response: array of publish rules with URLs.

### Unpublish a Port

```
DELETE /sandboxes/:id/publish/:port
```

Response: 204 No Content.

Publish rules are automatically cleaned up when a sandbox is destroyed.

## Templates

```
POST   /templates              Create template
GET    /templates              List templates
GET    /templates/:id          Get template
DELETE /templates/:id          Delete template
```

## Secrets

```
POST   /secrets                Create/update secret {"name": "...", "value": "..."}
GET    /secrets                List secrets (names only, no values)
DELETE /secrets/:name          Delete secret
```

## Volumes

```
POST   /volumes                Create volume {"name": "..."}
GET    /volumes                List volumes
GET    /volumes/:name          Get volume
DELETE /volumes/:name          Delete volume (fails if in use)
```
