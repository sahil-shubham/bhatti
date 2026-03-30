# bhatti-routes

Preview subdomain routing for bhatti sandboxes. Expose sandbox services as HTTPS URLs like `https://myapp.dev2.example.com`.

## How it works

```
Browser → Caddy (:443, auto-TLS) → VM bridge IP directly → sandbox service
```

- **Direct-to-VM routing** — Caddy proxies straight to the Firecracker VM's bridge IP. No double-proxy, no buffering issues.
- **Auto-TLS** — Let's Encrypt certificates via Cloudflare DNS-01 challenge.
- **Auto-wake** — A route-gen script runs every 15s, keeping routed sandboxes warm by pinging bhatti's API. Cold VMs restore in ~50ms on next cycle.
- **Multi-port** — Expose multiple ports from the same sandbox on different subdomains.

## Prerequisites

- bhatti installed and running
- [Caddy](https://caddyserver.com/) with the [cloudflare DNS plugin](https://github.com/caddy-dns/cloudflare) (for wildcard TLS)
- [uv](https://docs.astral.sh/uv/) (for the CLI script)
- Cloudflare-managed domain (optional, for auto DNS record management)

## Quick start

### 1. Install Caddy with Cloudflare plugin

```bash
go install github.com/caddyserver/xcaddy/cmd/xcaddy@latest
xcaddy build --with github.com/caddy-dns/cloudflare --output /usr/local/bin/caddy
```

### 2. Set up config directory

```bash
sudo mkdir -p /opt/bhatti-caddy
sudo touch /opt/bhatti-caddy/routes.yml /opt/bhatti-caddy/sites.caddy

sudo tee /opt/bhatti-caddy/Caddyfile > /dev/null <<EOF
{
    acme_dns cloudflare {env.CF_DNS_API_TOKEN}
    email admin@example.com
}

import /opt/bhatti-caddy/sites.caddy
EOF
```

### 3. Install scripts

```bash
sudo cp bhatti-routes /usr/local/bin/bhatti-routes
sudo cp route-gen.sh /opt/bhatti-caddy/route-gen.sh
```

### 4. Configure environment

Set these in your shell profile or systemd unit:

```bash
export BHATTI_DOMAIN="dev2.example.com"       # subdomain suffix
export BHATTI_PUBLIC_IP="1.2.3.4"             # IP for DNS A records
export CF_DNS_API_TOKEN="your_token"          # Cloudflare API token
export BHATTI_CF_ZONE_ID="your_zone_id"       # Cloudflare zone ID (optional)
```

### 5. Start Caddy + route timer

```bash
# Caddy service
sudo systemctl enable --now caddy-bhatti

# Route-gen timer (runs every 15s)
sudo systemctl enable --now bhatti-routes.timer
```

See [systemd/](systemd/) for example unit files, or create them:

```ini
# /etc/systemd/system/caddy-bhatti.service
[Unit]
Description=Caddy for bhatti preview subdomains
After=network.target bhatti.service

[Service]
Type=simple
ExecStart=/usr/local/bin/caddy run --config /opt/bhatti-caddy/Caddyfile
ExecReload=/usr/local/bin/caddy reload --config /opt/bhatti-caddy/Caddyfile
Environment=CF_DNS_API_TOKEN=your_token
Restart=always
LimitNOFILE=65536
AmbientCapabilities=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
```

```ini
# /etc/systemd/system/bhatti-routes.service
[Unit]
Description=Generate Caddy routes from bhatti sandboxes
After=bhatti.service

[Service]
Type=oneshot
ExecStart=/opt/bhatti-caddy/route-gen.sh
Environment=CF_DNS_API_TOKEN=your_token
Environment=BHATTI_DOMAIN=dev2.example.com
Environment=BHATTI_PUBLIC_IP=1.2.3.4
Environment=BHATTI_CF_ZONE_ID=your_zone_id
```

```ini
# /etc/systemd/system/bhatti-routes.timer
[Unit]
Description=Refresh bhatti Caddy routes every 15s

[Timer]
OnBootSec=10s
OnUnitActiveSec=15s
AccuracySec=5s

[Install]
WantedBy=timers.target
```

## Usage

```bash
# Expose a sandbox port
bhatti-routes add myapp 3000
# → https://myapp.dev2.example.com

# Expose another port on a custom subdomain
bhatti-routes add myapp 8080 myapp-api
# → https://myapp-api.dev2.example.com

# Interactive port picker (scans sandbox, shows process info)
bhatti-routes pick myapp

# List all active routes
bhatti-routes ls
# ┏━━━━━━━━━━━━┳━━━━━━━━━┳━━━━━━┳━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━┓
# ┃ Subdomain  ┃ Sandbox ┃ Port ┃ URL                                 ┃
# ┡━━━━━━━━━━━━╇━━━━━━━━━╇━━━━━━╇━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━┩
# │ myapp      │ myapp   │ 3000 │ https://myapp.dev2.example.com      │
# │ myapp-api  │ myapp   │ 8080 │ https://myapp-api.dev2.example.com  │
# └────────────┴─────────┴──────┴─────────────────────────────────────┘

# Show listening ports with process/container details
bhatti-routes ports myapp
# ┏━━━┳━━━━━━┳━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━┓
# ┃ # ┃ Port ┃ Process                                       ┃
# ┡━━━╇━━━━━━╇━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━┩
# │ 1 │ 3000 │ workspace-ui-1 (workspace-ui)                 │
# │ 2 │ 8080 │ workspace-api-1 (workspace-api)               │
# │ 3 │ 5432 │ workspace-postgres-1 (pgvector)               │
# └───┴──────┴───────────────────────────────────────────────┘

# Remove a route
bhatti-routes rm myapp-api

# Force-sync Caddy config + DNS
bhatti-routes sync
```

## Architecture

```
bhatti-routes CLI ──writes──→ routes.yml
                                  │
route-gen.sh (15s timer) ─reads───┘
    │
    ├── Looks up sandbox bridge IPs via bhatti API
    ├── Pings GET /sandboxes/:id to keep VMs warm
    ├── Creates Cloudflare DNS A records (if configured)
    ├── Generates Caddy site blocks → sites.caddy
    └── Reloads Caddy
```

### Why direct-to-VM instead of bhatti's proxy?

Bhatti's built-in reverse proxy (`/sandboxes/:id/proxy/:port/`) routes through a raw TCP tunnel. When an upstream HTTP proxy (Caddy/Traefik) is the client, responses larger than ~2KB can hang indefinitely due to a buffering interaction between `httputil.ReverseProxy` and the tunnel's `io.Copy`. Direct-to-VM-bridge-IP routing avoids this entirely.

### Why a timer instead of on-demand wake?

When a routed VM is cold (snapshotted to disk after 30min idle), its bridge IP isn't reachable. The 15s timer keeps routed VMs warm by calling bhatti's API, which triggers `ensureHot()`. This is simpler than implementing a Caddy middleware for on-demand wake, and the worst-case latency is 15s + 50ms (cold restore time).

## Routes file format

```yaml
# subdomain: sandbox:port
myapp: myapp:3000
myapp-api: myapp:8080

# shorthand (subdomain = sandbox name)
demo: 8000
```
