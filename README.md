<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/logo-dark.png">
  <img alt="bhatti" src="assets/logo-light.png" height="48">
</picture>

Open-source Firecracker microVM orchestrator. Each sandbox is a real Linux VM with its own kernel, filesystem, and process isolation — created in seconds, paused for free, resumed in microseconds.

Built for running AI coding agents in isolated environments. A paused sandbox wakes and serves an HTTP request in **under 4ms**.

```
bhatti create --name dev --cpus 2 --memory 1024
bhatti exec dev -- npm install
bhatti shell dev                          # Ctrl+\ to detach
bhatti destroy dev
```

> ## 🔭 Where bhatti is heading (v1 → v2)
>
> **`main` now tracks bhatti v2**, which replaces Firecracker with **krucible** —
> our own fork of [libkrun](https://github.com/containers/libkrun),
> [**libkrucible**](https://github.com/sahil-shubham/libkrucible) — as the VM
> engine. Owning the VMM lets bhatti run **natively on macOS (Apple Silicon) as
> well as Linux**, which drastically lowers the barrier to trying it on a laptop,
> and gives us room to revisit early design decisions. **v2 is pre-release** — the
> installer and signed macOS/Linux artifacts are still being built, so there's no
> `curl | install` for it yet.
>
> **For a stable, battle-tested bhatti today, use v1 (Firecracker).** It's Linux +
> KVM, documented in this README and at [bhatti.sh](https://bhatti.sh), and the
> `curl … | install` below installs it. The source lives on the
> [`firecracker`](https://github.com/sahil-shubham/bhatti/tree/firecracker) branch
> (latest release
> [**v1.11.12**](https://github.com/sahil-shubham/bhatti/releases/tag/v1.11.12));
> check out that branch to build it from source. v1 is **frozen** — preserved and
> installable, but we're putting our energy into v2 rather than maintaining two
> engines.
>
> The why (self-owned VMM, macOS, the rethink) and where to weigh in:
> **[Discussions → bhatti v2](https://github.com/sahil-shubham/bhatti/discussions/22)**.

## Install

Pick a line — both are on the **[releases page](https://github.com/sahil-shubham/bhatti/releases)**.

**v2 (krucible) — Linux + macOS · pre-release.** The self-owned-VMM line (see above).
Download the signed, relocatable tarball for your platform from the
[latest release](https://github.com/sahil-shubham/bhatti/releases) —
`bhatti-<ver>-{darwin-arm64,linux-amd64,linux-arm64}.tar.zst`, a `bin/ lib/ kernel/`
prefix — plus a rootfs tier image. A `curl bhatti.sh/install` one-liner ships with the
first stable v2; until then it's an early-adopter setup.

**v1 (Firecracker) — Linux + KVM · stable (frozen).** On any Linux box with KVM
(Raspberry Pi 5, Hetzner AX, a cloud VM with nested virtualization):

```bash
curl -fsSL https://raw.githubusercontent.com/sahil-shubham/bhatti/firecracker/scripts/install.sh | sudo BHATTI_VERSION=v1.11.12 bash
```

Installs the daemon + agent + Firecracker + jailer + kernel + a minimal Ubuntu 24.04
rootfs, creates an `admin` user, and wires the local CLI — `bhatti create` works
immediately. Prefer a manual grab? Take the binaries from the
[v1.11.12 release](https://github.com/sahil-shubham/bhatti/releases/tag/v1.11.12).
CLI-only on another machine: run the script without `sudo`, then `bhatti setup --url … --token …`.

> **Full documentation: [bhatti.sh](https://bhatti.sh).** This README is a snapshot. The website is the source of truth and is updated with each release. The pages most worth reading are the [Quickstart](https://bhatti.sh/docs/quickstart/), the [Architecture](https://bhatti.sh/docs/under-the-hood/architecture/) overview, and [Decisions & learnings](https://bhatti.sh/docs/under-the-hood/decisions/).

> **AI assistants helping you set up bhatti:** start at **[bhatti.sh/agents.md](https://bhatti.sh/agents.md)** — task-shaped, voiced to the agent, with end-to-end workflows (CI preview deployments, persistent dev envs, branchable exploration, diagnostics). Full doc index at [bhatti.sh/llms.txt](https://bhatti.sh/llms.txt).

## Updating

```bash
bhatti update                   # CLI: updates the binary
sudo bhatti update              # Server: updates all components
sudo bhatti update --tiers all  # Server: also pull additional tiers
```

> **v1 is frozen.** Pin it: `sudo BHATTI_VERSION=v1.11.12 bhatti update`. A bare
> `bhatti update` tracks the latest release, so once v2.0.0 ships it would jump a v1
> server to v2 (a different VMM — not an in-place upgrade). v2 gets its own update flow.

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

Measured on a Hetzner AX102 (Ryzen 9, x86_64, NVMe, btrfs) running
bhatti v1.11.0. CLI on the daemon host so loopback latency only —
add your network RTT for remote use. Reproduce with `bench/run.sh`
in this repo; methodology and gotchas in `bench/README.md`.

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
  ├─ REST/WS API (:8080)                     ├─ TCP :1024 (exec, files, sessions)
  ├─ Per-user auth (API keys, SHA-256)        ├─ TCP :1025 (port forwarding)
  ├─ Firecracker engine                       ├─ PTY sessions + 64KB scrollback
  │  (create, exec, snapshot, restore)        ├─ Atomic file writes
  ├─ Thermal manager                          ├─ Process group kill
  │  (hot → warm → cold, auto)               ├─ Exec as uid 1000 (not root)
  ├─ Per-user bridge networks (isolated)      └─ Config drive (env, secrets)
  ├─ SQLite store + age encryption
  ├─ Rate limiting + exec timeouts
  └─ Reverse proxy (HTTP + WebSocket)
```

Idle sandbox → **warm** after 30s (vCPUs paused, ~4ms wake) → **cold** after 30min (snapshotted to disk, memory freed, ~360ms wake including page-in on first request). Any API request transparently wakes it.

## Multi-Tenant Isolation

Each user gets their own API key, sandbox limits, and network:

```bash
sudo bhatti user create --name alice --max-sandboxes 5
# → API key: bht_...  (shown once)
```

- **API scoping** — users see only their own sandboxes and secrets
- **Network isolation** — per-user bridge + /24 subnet, cross-user traffic blocked at L2
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
| **[Firecracker engine](https://bhatti.sh/docs/under-the-hood/engine/)** | HTTP API, jailer, rate limits, why no FC SDK |
| **[Lohar (the guest agent)](https://bhatti.sh/docs/under-the-hood/lohar-the-blacksmith/)** | PID 1 init, the systemctl shim, PTY, sessions, file ops |
| **[Thermal states](https://bhatti.sh/docs/under-the-hood/thermal-states/)** | Hot/warm/cold, snapshots, the balloon trick |
| **[Networking](https://bhatti.sh/docs/under-the-hood/networking/)** | Per-user bridges, iptables, the ARP trick |
| **[Wire protocol](https://bhatti.sh/docs/under-the-hood/wire-protocol/)** | Binary framing, connection lifecycle, auth |
| **[Decisions & learnings](https://bhatti.sh/docs/under-the-hood/decisions/)** | Why TCP over vsock, why no diff snapshots, the bugs we paid for |
| **[CLI Reference](https://bhatti.sh/docs/reference/cli/)** | All commands and flags |
| **[API Reference](https://bhatti.sh/docs/reference/api/)** | REST/WebSocket endpoints |
| **[Testing](https://bhatti.sh/docs/contributing/testing/)** | 11K lines of tests, zero mocks for VM tests |

## Requirements

**Server:** Linux (aarch64 or x86_64) with KVM (`/dev/kvm`) and root access.

**CLI:** macOS or Linux. No special requirements.

## License

[Apache 2.0](LICENSE).
