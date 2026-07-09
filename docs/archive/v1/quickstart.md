> [!WARNING]
> **DEPRECATED — do not edit.**
> The canonical, maintained version of this page is at
> <https://bhatti.sh/docs/quickstart/>.
> This file is kept only for git history and may be removed in a future
> cleanup. See [`docs/README.md`](./README.md) for the redirect index.

---

# Quickstart

The whole site documentation lives at <https://bhatti.sh/docs> — this
file is the short version, kept in the repo for offline readers and
people browsing GitHub.

There are two entry points:

1. **Self-host on a Linux box you own** (the recommended path). Install
   the daemon, get a sandbox in about five minutes, including download.
2. **Drive a remote bhatti from your laptop** (useful when someone
   else runs the server, or when you've installed bhatti on a server
   and want to use it from your laptop).

---

## Self-hosting

You need:

- Linux with KVM (`/dev/kvm` exists)
- Root access (the install script uses `sudo`)
- ~1 GB of disk for the minimal tier

```bash
curl -fsSL bhatti.sh/install | sudo bash
```

The script downloads `bhatti`, `lohar`, Firecracker, the jailer, the
kernel, and an Ubuntu 24.04 rootfs. It then:

- Installs a systemd unit (`bhatti.service`) and starts the daemon.
- Creates an `admin` user.
- Writes the admin API key to `/root/.bhatti/config.yaml` **and** to
  the invoking user's `~/.bhatti/config.yaml`. So if you ran the
  install with `sudo` from your normal user, your CLI is already
  wired up.

That last point matters: **you don't need to run `bhatti setup` after
installing.** Go straight to creating a sandbox:

```bash
bhatti create --name dev
bhatti exec dev -- uname -a
bhatti shell dev                    # Ctrl+\ to detach, scrollback preserved
echo 'console.log("hi")' | bhatti file write dev /workspace/app.js
bhatti file read dev /workspace/app.js
bhatti destroy dev
```

Each sandbox is a real Firecracker microVM. Idle sandboxes pause
themselves automatically and resume on the next request. See
[`docs/thermal-management.md`](thermal-management.md).

### Adding a teammate

```bash
sudo bhatti user create --name alice --max-sandboxes 5
# → API key: bht_...  (shown once)
```

Send Alice the key over a secure channel. On her machine she follows
the remote-CLI flow below.

### Key rotation, deletion

```bash
sudo bhatti user rotate-key alice    # invalidates old key, prints new one
sudo bhatti user delete alice         # fails if alice has running sandboxes
```

### Secrets

```bash
bhatti secret set API_KEY sk-abc123
bhatti secret list
bhatti secret delete API_KEY
```

Secrets are encrypted at rest with [age](https://age-encryption.org/)
under `/var/lib/bhatti/age.key`. **Back that key up.** If you lose it,
every encrypted secret on the server is unrecoverable.

---

## Driving a remote bhatti from your laptop

This is the path for someone using a bhatti server they don't run, or
for using bhatti from your laptop after installing it on a separate
host.

```bash
# 1. Install the CLI binary (no sudo)
curl -fsSL bhatti.sh/install | bash

# 2. Configure
bhatti setup --url https://your-server:8080 --token bht_...
# or interactively:
bhatti setup
```

`bhatti setup` accepts `--url` and `--token` for non-interactive use
(agents, CI, provisioning scripts). Without flags it prompts and masks
the key on input. The auth check always runs and the command exits
non-zero on failure, so a bad key surfaces immediately.

Or set environment variables, which take precedence over the saved
config at runtime:

```bash
export BHATTI_URL=https://your-server:8080
export BHATTI_TOKEN=bht_your_api_key_here
```

Once configured, every other command is the same as the self-hosted
case (`bhatti create`, `bhatti exec`, etc.). You see only the
sandboxes that belong to your user — bhatti is multi-tenant by
default.

---

## What just happened

When you ran `bhatti create --name dev`:

1. The daemon authenticated your API key (SHA-256 hash lookup),
   checked your sandbox cap, and validated the name.
2. It copied the base rootfs (CoW reflink on btrfs/XFS), built a 1 MB
   ext4 config drive with hostname / DNS / auth token, allocated a
   TAP device on your user's bridge, started a Firecracker process,
   configured it via Firecracker's Unix-socket HTTP API, booted the
   kernel, and waited for `lohar` (the guest agent) to respond on
   TCP :1024.
3. The sandbox is now running — its own kernel, its own filesystem,
   its own L2 segment, isolated from other users by per-user bridges
   and iptables rules.

When you leave the sandbox idle for 30 seconds, the thermal manager
pauses its vCPUs and inflates a virtio-balloon to take ~50% of its
RAM back. After 30 minutes idle, it snapshots to disk and frees the
host RAM entirely. The next request transparently restores it. See
[`docs/thermal-management.md`](thermal-management.md).

---

## Next

- [Architecture](architecture.md) — how the system fits together
- [API Reference](api-reference.md) — REST and WebSocket endpoints
- [CLI Reference](cli-reference.md) — every command and flag
- [Site docs](https://bhatti.sh/docs) — long-form, with the most
  recent voice and structure
