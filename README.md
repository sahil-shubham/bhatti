<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/logo-dark.png">
  <img alt="bhatti" src="assets/logo-light.png" height="48">
</picture>

Open-source microVM orchestrator with its own VMM. Each sandbox is a real Linux VM with its own kernel, filesystem, and process isolation — created in seconds, paused for free, resumed in microseconds. Runs on **Linux (KVM)** and **macOS (Apple Silicon)** — a dev box or a server, your choice.

Built for running AI coding agents in isolated environments. A paused sandbox wakes and serves an HTTP request in **under 4ms**.

```
bhatti create --name dev --cpus 2 --memory 1024
bhatti exec dev -- npm install
bhatti shell dev                          # Ctrl+\ to detach
bhatti destroy dev
```

> ## 🔭 bhatti v2 (krucible) — and v1 (Firecracker), frozen
>
> **`main` is bhatti v2.** It replaces Firecracker with **krucible** — our own
> fork of [libkrun](https://github.com/containers/libkrun),
> [**libkrucible**](https://github.com/sahil-shubham/libkrucible) — as the VM
> engine. Owning the VMM lets bhatti run **natively on macOS (Apple Silicon) as
> well as Linux**, adds a secure-by-default per-owner network gateway, and moves
> storage onto host-independent qcow2 (no more btrfs requirement). The
> `curl … | install` below installs v2.
>
> **v1 (Firecracker) is frozen but still installable.** It's Linux + KVM; the
> source is on the
> [`firecracker`](https://github.com/sahil-shubham/bhatti/tree/firecracker) branch
> (latest [**v1.11.12**](https://github.com/sahil-shubham/bhatti/releases/tag/v1.11.12)),
> documented at [bhatti.sh/v1/docs](https://bhatti.sh/v1/docs/). We're putting our
> energy into v2 rather than maintaining two engines.
>
> **Moving from v1 to v2 is a cutover, not an in-place upgrade** — a different VMM
> (snapshots and the on-disk layout don't carry over). Install v2 fresh; keep v1
> by pinning `BHATTI_VERSION=v1.11.12`.
>
> The why (self-owned VMM, macOS, the rethink) and where to weigh in:
> **[Discussions → bhatti v2](https://github.com/sahil-shubham/bhatti/discussions/22)**.

## Install

**v2 (krucible).** One installer, two platforms. Self-host on any Linux box with
KVM (Raspberry Pi 5, Hetzner AX, a cloud VM with nested virtualization) or on a
Mac (Apple Silicon, HVF — no KVM, no root for the hypervisor itself):

```bash
curl -fsSL bhatti.sh/install | sudo bash        # self-host server (prompts for a tier)
curl -fsSL bhatti.sh/install | bash             # CLI only (connect to a remote server)
```

The self-host install lays a single self-contained runtime bundle (daemon + agent +
`bhatti-vmm` + the `bhatti-netd` gateway + libkrun + a lean kernel) plus a rootfs
tier, creates an `admin` user, wires the local CLI, and starts the service
(systemd on Linux, launchd on macOS) — `bhatti create` works immediately. Prefer a
manual grab? Take the per-platform tarball
(`bhatti-<ver>-{darwin-arm64,linux-amd64,linux-arm64}.tar.zst`) from the
[latest release](https://github.com/sahil-shubham/bhatti/releases).

**v1 (Firecracker) — Linux + KVM · frozen.** To install the old engine instead, pin
it (a bare `bhatti.sh/install` now installs v2):

```bash
curl -fsSL https://raw.githubusercontent.com/sahil-shubham/bhatti/firecracker/scripts/install.sh | sudo BHATTI_VERSION=v1.11.12 bash
```

See [bhatti.sh/v1/docs](https://bhatti.sh/v1/docs/) for the v1 docs.

> **Full documentation: [bhatti.sh](https://bhatti.sh).** This README is a snapshot. The website is the source of truth and is updated with each release. The pages most worth reading are the [Quickstart](https://bhatti.sh/docs/quickstart/), the [Architecture](https://bhatti.sh/docs/under-the-hood/architecture/) overview, and [Decisions & learnings](https://bhatti.sh/docs/under-the-hood/decisions/).

> **AI assistants helping you set up bhatti:** start at **[bhatti.sh/agents.md](https://bhatti.sh/agents.md)** — task-shaped, voiced to the agent, with end-to-end workflows (CI preview deployments, persistent dev envs, branchable exploration, diagnostics). Full doc index at [bhatti.sh/llms.txt](https://bhatti.sh/llms.txt).

## Updating

```bash
bhatti update                   # CLI: updates the binary
sudo bhatti update              # Server: updates all components
sudo bhatti update --tiers all  # Server: also pull additional tiers
```

> **Within v2, `bhatti update` is safe** — it refreshes the binary (CLI) or all
> runtime components (server). **Crossing from v1 (Firecracker) is blocked**: it's a
> different VMM, so the installer refuses an in-place jump and points you at a fresh
> v2 install. To stay on v1, pin it: `sudo BHATTI_VERSION=v1.11.12 bhatti update`.

## Rootfs Tiers

The server install prompts you to pick a rootfs tier. Each tier is a pre-built Ubuntu 24.04 image:

| Tier | What's in it | Size |
|------|-------------|------|
| [`minimal`](https://bhatti.sh/docs/managing/tiers/) | Bare Ubuntu + curl + fuse3 | ~200MB |
| [`browser`](https://bhatti.sh/docs/managing/tiers/browser/) | + Chromium, Playwright, Node 22 | ~600MB |
| [`docker`](https://bhatti.sh/docs/managing/tiers/docker/) | + Docker Engine + buildx (multi-arch) | ~550MB |
| [`computer`](https://bhatti.sh/docs/managing/tiers/computer/) | + Full desktop: XFCE, KasmVNC, Chromium | ~1.5GB |

Use `--image` to create sandboxes from non-default tiers:

```bash
# Run browser automation
bhatti create --name scraper --image browser
bhatti exec scraper -- npx playwright test

# Run a desktop environment (KasmVNC web client on port 6080)
bhatti create --name desktop --image computer --cpus 2 --memory 4096
bhatti publish desktop -p 6080
bhatti exec desktop -- vnc-creds          # username + per-sandbox password

# Run Docker-in-VM
bhatti create --name ci --image docker
bhatti exec ci -- docker run hello-world

# Multi-arch builds inside one sandbox (qemu-user emulation)
bhatti exec ci -- docker run --privileged --rm tonistiigi/binfmt --install all
bhatti exec ci -- docker buildx build --platform linux/amd64,linux/arm64 -t me/app .
```

The server auto-discovers tiers from `/var/lib/bhatti/images/`. Install more with `sudo bhatti update --tiers all`. Full per-tier docs (operator UX, env knobs, sizing, troubleshooting) live at [bhatti.sh/docs/managing/tiers/](https://bhatti.sh/docs/managing/tiers/); see [Adding a tier](https://bhatti.sh/docs/contributing/adding-a-tier/) for building your own.

## CLI Commands

### Core

| Command | Description |
|---------|-------------|
| `create` | Create a new sandbox VM |
| `list` | List sandboxes |
| `inspect` | Show sandbox details (state, IP, resources) |
| `exec` | Execute a command in a sandbox |
| `shell` | Open an interactive shell (Ctrl+\\ to detach) |
| `ps` | List active sessions in a sandbox |
| `stop` | Snapshot and stop a sandbox |
| `start` | Resume a stopped sandbox |
| `destroy` | Destroy a sandbox |

### Files & Data

| Command | Description |
|---------|-------------|
| `file read` | Read a file from a sandbox |
| `file write` | Write stdin to a file in a sandbox |
| `file ls` | List files in a sandbox directory |
| `volume create` | Create a persistent volume |
| `volume list` | List volumes |
| `volume delete` | Delete a volume |
| `secret set` | Create or update an encrypted secret |
| `secret list` | List secrets |

### Images & Snapshots

| Command | Description |
|---------|-------------|
| `image list` | List available rootfs images |
| `image pull` | Pull an OCI/Docker image from a public registry |
| `image import` | Import a local Docker image as a bhatti rootfs |
| `image save` | Save a sandbox's rootfs as a reusable image |
| `snapshot create` | Checkpoint a running sandbox |
| `snapshot resume` | Resume from a named snapshot |

### Networking

| Command | Description |
|---------|-------------|
| `publish` | Publish a sandbox port with a public URL |
| `unpublish` | Remove a published port |
| `share` | Generate a shareable web shell URL |

### Admin (server operators)

| Command | Description |
|---------|-------------|
| `serve` | Start the bhatti daemon |
| `user create` | Create a user with API key and resource limits |
| `user list` | List users |
| `user rotate-key` | Rotate a user's API key |
| `admin status` | System overview (sandboxes, memory, disk) |
| `admin events` | Query the event log |
| `admin metrics` | Query metrics snapshots |

### Setup

| Command | Description |
|---------|-------------|
| `setup` | Configure CLI endpoint and API key (interactive, or `--url`/`--token` for agents/CI) |
| `update` | Update bhatti to the latest version |
| `version` | Print version and check for updates |
| `completion` | Generate shell completions (bash/zsh/fish) |

All commands support `--json` for machine-readable output. See the [CLI Reference](https://bhatti.sh/docs/reference/cli/) for full flag details.

## Performance

> The numbers below are the **v1 (Firecracker)** baseline on a Hetzner AX102
> (Ryzen 9, x86_64, NVMe). v2 (krucible) is being re-measured on Linux/KVM and
> macOS/HVF — the shape is the same (free warm-wake, sub-second cold-wake), and the
> lean owned kernel roughly halves cold-start; this table will be updated with the
> v2 figures.

CLI on the daemon host so loopback latency only — add your network RTT for remote
use. Reproduce with `bench/run.sh` in this repo; methodology in `bench/README.md`.

```
                                p50       p99
Create a machine                266ms     291ms
Snapshot to disk (1024MB)       485ms     807ms
Wake on request (cold)          360ms     430ms
Wake on request (warm)          3.7ms     10.2ms
Destroy a machine               87ms      96ms
Run a command                   12ms      14ms
20 commands in parallel         32ms      39ms
```

Cold-wake reads the memory snapshot from disk on first use — page-in
cost is included, not just the orchestration call returning. Warm-wake
is the killer feature: vCPUs paused but memory still in RAM means a
transparent wake feels free.

## Architecture

```
bhatti (host daemon)                        lohar (guest agent, PID 1 in each VM)
  ├─ Control API (unix socket + :8080)       ├─ vsock: exec, files, sessions
  ├─ Per-user auth (API keys, SHA-256)        ├─ port forwarding
  ├─ krucible engine (libkrun fork)           ├─ PTY sessions + 64KB scrollback
  │  └─ per-VM bhatti-vmm helper + control      ├─ Atomic file writes
  │     socket (create, exec, snapshot, fork)   ├─ Process group kill
  ├─ Thermal manager (hot → warm → cold, auto)  ├─ Exec as uid 1000 (not root)
  ├─ bhatti-netd gateway (gVisor, per owner)    └─ Config drive (env, secrets)
  │  └─ policed egress, host isolation, siblings
  ├─ SQLite store + age encryption
  ├─ Rate limiting + exec timeouts
  └─ Reverse proxy (HTTP + WebSocket)
```

Runs on Linux (KVM) and macOS (Apple Silicon, HVF). Idle sandbox → **warm** after 30s (vCPUs paused, ~4ms wake) → **cold** after 30min (snapshotted to disk, memory freed, sub-second wake including page-in on first request). Any API request transparently wakes it.

## Multi-Tenant Isolation

Each user gets their own API key, sandbox limits, and network:

```bash
sudo bhatti user create --name alice --max-sandboxes 5
# → API key: bht_...  (shown once)
```

- **API scoping** — users see only their own sandboxes and secrets
- **Network isolation** — a per-owner `bhatti-netd` gateway (userspace gVisor netstack): egress is policed (the host, private ranges, and cloud metadata are denied by default), same-owner sandboxes can reach each other, and cross-owner traffic is isolated
- **Resource caps** — per-user limits on sandbox count, CPUs, and memory
- **Rate limiting** — per-user token buckets (30 creates/min, 600 execs/min, 1200 reads/min)
- **Secrets** — encrypted at rest (age), scoped per user

## Key Features

- **Preview URLs** — `bhatti publish dev -p 3000` → `https://dev-k3m9x2.bhatti.sh`, auto-wake from sleep
- **Session-aware exec** — TTY sessions survive disconnects, scrollback replayed on reattach
- **OCI image support** — `bhatti image pull python:3.12` → use as base for sandboxes
- **Persistent volumes** — survive sandbox destruction, mountable across sandboxes
- **Streaming exec** — real-time NDJSON output via `Accept: application/x-ndjson`
- **Guest hardening** — exec as uid 1000, config drive unmounted after boot, connection/session limits
- **Single binary** — `bhatti serve` = daemon, `bhatti create` = CLI, `bhatti user` = admin

## Documentation

Full docs live at **[bhatti.sh](https://bhatti.sh)** — that's the canonical reference. The list below is a hand-picked entry point.

| Page | What it covers |
|------|----------------|
| **[Quickstart](https://bhatti.sh/docs/quickstart/)** | Install + create your first sandbox |
| **[Self-Hosting](https://bhatti.sh/docs/self-hosting/)** | Run bhatti on your own hardware, requirements, backups |
| **[Concepts](https://bhatti.sh/docs/concepts/)** | Sandboxes, thermal states, the two binaries |
| **[Architecture](https://bhatti.sh/docs/under-the-hood/architecture/)** | System design, data flow, concurrency model |
| **[krucible engine](https://bhatti.sh/docs/under-the-hood/engine/)** | The libkrun fork, the bhatti-vmm helper, the control socket |
| **[Lohar (the guest agent)](https://bhatti.sh/docs/under-the-hood/lohar-the-blacksmith/)** | PID 1 init, the systemctl shim, PTY, sessions, file ops |
| **[Thermal states](https://bhatti.sh/docs/under-the-hood/thermal-states/)** | Hot/warm/cold, snapshots, the balloon trick |
| **[Networking](https://bhatti.sh/docs/under-the-hood/networking/)** | The per-owner gVisor gateway, policed egress, siblings |
| **[Wire protocol](https://bhatti.sh/docs/under-the-hood/wire-protocol/)** | Binary framing, connection lifecycle, auth |
| **[Decisions & learnings](https://bhatti.sh/docs/under-the-hood/decisions/)** | Why TCP over vsock, why no diff snapshots, the bugs we paid for |
| **[CLI Reference](https://bhatti.sh/docs/reference/cli/)** | All commands and flags |
| **[API Reference](https://bhatti.sh/docs/reference/api/)** | REST/WebSocket endpoints |
| **[Testing](https://bhatti.sh/docs/contributing/testing/)** | 11K lines of tests, zero mocks for VM tests |

## Requirements

**Self-host:** Linux (aarch64 or x86_64) with KVM (`/dev/kvm`) **or** macOS on
Apple Silicon (HVF — no KVM needed). Either can be a dev box or a server.

**CLI:** macOS or Linux. No special requirements.

## License

[Apache 2.0](LICENSE).
