# ⚒ Bhatti

Sandbox orchestrator — spin up, manage, and shell into Docker-based dev environments from a single API and web UI.

## What it does

- **Template-based sandboxes** — define blueprints (image, CPU, memory, mounts), create sandboxes from them
- **Web terminal** — full xterm.js shell into any running sandbox via WebSocket
- **Port forwarding** — auto-detects listening ports inside sandboxes, reverse-proxies them to the host (HTTP + WebSocket)
- **Persistent volumes** — attach named Docker volumes to sandboxes for workspace persistence across destroys
- **Secrets management** — store and inject secrets into sandbox environments
- **REST API** — full CRUD for templates, sandboxes, volumes, and secrets
- **Bearer auth** — optional token-based authentication
- **SSH keypair** — auto-generates ed25519 keys per install (`~/.bhatti/id_ed25519`)
- **Pre-built sandbox image** — Ubuntu 24.04 with zsh, Node.js, tmux, starship, claude-code, and your dotfiles baked in

## What it doesn't do

- No Firecracker/microVM support yet (engine interface exists, only Docker is implemented)
- No secret encryption (age envelope planned, currently metadata-only)
- No multi-node / remote host orchestration — single Docker daemon only
- No built-in HTTPS — put it behind a reverse proxy
- No user/RBAC system — single shared token or open

## Setup

```bash
# Prerequisites
go 1.25+, Docker running

# Build the server
make build

# Build the sandbox Docker image (pulls your dotfiles + Claude creds automatically)
make sandbox

# Run
./bhatti                      # listens on :8080 by default

# Config (optional) — ~/.bhatti/config.yaml
# engine: docker
# listen: :8080
# auth_token: your-secret-token
```

## API at a glance

```
GET/POST   /templates          — list or create templates
GET/DELETE /templates/:id      — get or delete a template

GET/POST   /sandboxes          — list or create sandboxes
GET/DELETE /sandboxes/:id      — get or destroy a sandbox
POST       /sandboxes/:id/start|stop  — start or stop
POST       /sandboxes/:id/exec — run a command
WS         /sandboxes/:id/ws   — interactive shell
GET        /sandboxes/:id/ports — detected listening ports
ANY        /sandboxes/:id/proxy/:port/* — reverse proxy into sandbox

GET/POST   /volumes            — list or create volumes
GET/DELETE /volumes/:name      — get or delete a volume

GET/POST   /secrets            — list or create secrets
DELETE     /secrets/:name      — delete a secret

GET        /ports              — all open ports across all running sandboxes
```

## Project structure

```
cmd/bhatti/       — entrypoint
pkg/
  config.go       — config loading + SSH key generation
  engine/         — sandbox lifecycle interface
  engine/docker/  — Docker implementation
  server/         — HTTP server, routes, WebSocket, reverse proxy
  store/          — SQLite persistence (templates, sandboxes, volumes, secrets)
sandbox/          — zshrc, tmux.conf for the sandbox image
web/              — single-page web UI
Dockerfile.sandbox — sandbox image definition
Makefile          — build, sandbox, test, clean
```

## Commands

```bash
make build      # compile ./bhatti
make sandbox    # build bhatti-sandbox Docker image
make test       # run all tests
make clean      # remove build artifacts
```

## License

Unlicensed — private project.
