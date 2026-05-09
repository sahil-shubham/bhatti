> [!WARNING]
> **DEPRECATED — do not edit.**
> The canonical, maintained version of this page is at
> <https://bhatti.sh/docs/reference/cli/>.
> This file is kept only for git history and may be removed in a future
> cleanup. See [`docs/README.md`](./README.md) for the redirect index.

---

# CLI Reference

`bhatti` is a single binary — `bhatti serve` starts the daemon, everything else is a CLI command that talks to the daemon's HTTP API.

All sandbox commands accept sandbox name or ID interchangeably.

## Installation

Server (Linux + KVM, recommended):

```bash
curl -fsSL bhatti.sh/install | sudo bash
```

This also wires the CLI on the same box — admin user is auto-created and its API key is written to your `~/.bhatti/config.yaml`. No `bhatti setup` needed for local use.

CLI only (driving a remote server from your laptop):

```bash
curl -fsSL bhatti.sh/install | bash
```

Re-running updates an existing installation.

## Configuration

### Setup

Interactive (prompts for endpoint and key):

```bash
bhatti setup
```

Non-interactive (agents, CI, provisioning scripts):

```bash
bhatti setup --url https://api.bhatti.sh --token bht_your_key_here
```

Writes to `~/.bhatti/config.yaml` and tests the connection by listing sandboxes. Exits non-zero on auth failure so scripts can detect it. Key input is masked when typed at the prompt.

### Config file

`~/.bhatti/config.yaml`:

```yaml
api_url: https://api.bhatti.sh
auth_token: bht_your_key_here
```

### Environment variables (override for CI/scripts)

```bash
export BHATTI_URL=https://api.bhatti.sh    # API endpoint
export BHATTI_TOKEN=bht_your_key_here      # API key
```

Priority: `--flag` > config file > environment variable > default.

The config file is the primary source — `bhatti setup` writes it and it just works. Environment variables are a fallback for CI pipelines and scripts, not the default.

## Commands

### version

```bash
bhatti version
# → bhatti v0.1.0
# → api: https://api.bhatti.sh
```

### serve

Start the daemon. Requires root, KVM, and a config at `/var/lib/bhatti/config.yaml`.

```bash
sudo bhatti serve
```

### create

```bash
bhatti create --name dev --cpus 2 --memory 1024
bhatti create --name worker --env API_KEY=sk-abc,NODE_ENV=prod
bhatti create --name builder --init "npm install && npm run build"
```

| Flag | Default | Description |
|------|---------|-------------|
| `--name` | auto-generated | Sandbox name (must match `[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}`) |
| `--cpus` | 1 | Number of vCPUs (capped by user's per-sandbox limit) |
| `--memory` | 512 | Memory in MB (capped by user's per-sandbox limit) |
| `--env` | — | Comma-separated KEY=VALUE pairs |
| `--init` | — | Init script (runs as attachable TTY session "init") |

### list / ls

```bash
bhatti list
```

Shows only your sandboxes (scoped to your API key).

### destroy / rm

```bash
bhatti destroy dev
```

### edit

Update mutable settings on an existing sandbox.

```bash
# Rename a sandbox
bhatti edit dev --name dev-old

# Pin to hot tier (autonomous agents that hold persistent connections)
bhatti edit my-agent --keep-hot

# Re-enable thermal transitions
bhatti edit my-agent --allow-cold
```

Rename notes:

- The in-guest hostname is fixed at create time and is **not** changed by rename. Same behaviour as `docker rename`.
- Published URLs from `bhatti publish` keep their original alias and remain stable.
- Active shells, exec sessions, and websockets keep working through the rename — they're keyed on the sandbox ID, not the name.
- A renamed sandbox's old name is immediately available for a new sandbox — same as destroy-then-create.

### exec

```bash
bhatti exec dev -- echo hello
bhatti exec dev -- npm install
bhatti exec dev -- sh -c 'echo $API_KEY'
```

Everything after `--` is the command. Exit code is forwarded. Stdout goes to stdout, stderr goes to stderr:

```bash
bhatti exec dev -- cat /workspace/data.json | jq .name
```

Commands run as user `lohar` (uid 1000), not root. Use `sudo` inside the sandbox for root access.

### shell / sh

```bash
bhatti shell dev
```

Interactive terminal inside the sandbox. `Ctrl+\` to detach — the shell keeps running, scrollback is preserved. Reconnect with `bhatti shell dev` again.

### ps

```bash
bhatti ps dev
```

### file

```bash
bhatti file read dev /workspace/app.js
echo 'console.log("hello")' | bhatti file write dev /workspace/app.js
bhatti file ls dev /workspace/
```

File writes are capped at 100MB per operation. Writes are atomic (concurrent readers never see partial content).

### secret

```bash
bhatti secret set API_KEY sk-abc123def
bhatti secret list
bhatti secret delete API_KEY
```

Secrets are encrypted at rest (age) and scoped to your user.

### user (server operator only)

User management operates directly on the local SQLite database. Requires access to the data directory.

```bash
# Create a user
sudo bhatti user create --name alice --max-sandboxes 5 --max-cpus 4 --max-memory 4096
# → API key: bht_...  (shown once)

# List users
sudo bhatti user list

# Rotate API key (old key immediately invalidated)
sudo bhatti user rotate-key alice

# Delete user (fails if user has active sandboxes)
sudo bhatti user delete alice
```

| Flag | Default | Description |
|------|---------|-------------|
| `--name` | required | User name (must be unique) |
| `--max-sandboxes` | 5 | Maximum concurrent sandboxes |
| `--max-cpus` | 4 | Maximum vCPUs per sandbox |
| `--max-memory` | 4096 | Maximum memory (MB) per sandbox |

### publish

```bash
bhatti publish dev -p 3000                  # auto-generated alias
bhatti publish dev -p 3000 -a my-app        # explicit alias
```

Publishes a sandbox port with a public URL. The URL is accessible without authentication.

With `-a`, the alias is used directly (`my-app.bhatti.sh`). Without `-a`, an alias is generated from the sandbox name with a random suffix (`dev-k3m9x2.bhatti.sh`) to prevent URL guessing.

| Flag | Description |
|------|-------------|
| `-p, --port` | Port to publish (required) |
| `-a, --alias` | Custom alias (optional, auto-generated if omitted) |

### unpublish

```bash
bhatti unpublish dev -p 3000
```

Removes a published port. The URL stops working immediately.

| Flag | Description |
|------|-------------|
| `-p, --port` | Port to unpublish (required) |

### setup

```bash
bhatti setup
```

Interactive configuration for remote CLI users. Prompts for endpoint and API key, saves config, tests the connection.
