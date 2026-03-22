# Quickstart

There are two ways to use Bhatti: **install the CLI** to use someone else's server, or **run the full server** on your own hardware.

---

## Option A: Use the CLI (remote user)

If someone gave you an API key, you just need the CLI binary. No KVM, no root, no Go — works on macOS and Linux.

### Install

```bash
curl -fsSL https://raw.githubusercontent.com/sahil-shubham/bhatti/main/scripts/install-cli.sh | bash
```

This downloads a ~11MB binary for your OS and architecture and puts it in `/usr/local/bin`.

### Configure

```bash
bhatti setup
```

```
API endpoint [http://localhost:8080]: https://api.bhatti.sh
API key: ****
Saved to ~/.bhatti/config.yaml
Testing connection... ✓ connected (sandboxes: 0, uptime: 4h23m)
```

Or set environment variables directly:

```bash
export BHATTI_URL=https://api.bhatti.sh
export BHATTI_TOKEN=bht_your_api_key_here
```

### Use

```bash
bhatti create --name dev
bhatti exec dev -- uname -a               # Linux VM, full isolation
bhatti exec dev -- node --version          # Node 22 pre-installed
bhatti shell dev                           # interactive shell (Ctrl+\ to detach)
echo 'console.log("hi")' | bhatti file write dev /workspace/app.js
bhatti file read dev /workspace/app.js
bhatti destroy dev
```

That's it. Each sandbox is a real Firecracker microVM with its own kernel, filesystem, and network. Idle sandboxes pause automatically and resume transparently on the next command.

---

## Option B: Run the Server (self-hosted)

Run Bhatti on your own Linux box with KVM — Raspberry Pi 5, AWS Graviton, Hetzner bare metal, or any x86_64/arm64 machine with `/dev/kvm`.

### Install

```bash
git clone https://github.com/sahil-shubham/bhatti.git
cd bhatti
sudo ./scripts/install.sh
```

This builds everything from source (~10 min on first run):
- Installs Go and Firecracker if not present
- Builds the host daemon (`bhatti`) and guest agent (`lohar`)
- Downloads a Linux kernel and builds an Ubuntu 24.04 rootfs
- Creates an admin user and saves the API key to `~/.bhatti/config.yaml`

```
==> Installing bhatti on myhost (aarch64)
==> Building bhatti and lohar from source...
==> Downloading kernel...
==> Building rootfs (this takes ~10 minutes on first install)...
==> Creating admin user...

============================================
  bhatti installed on myhost (aarch64)

  Admin API key: bht_abc123...
  (saved to ~/.bhatti/config.yaml)

  To start the daemon:
    cd /var/lib/bhatti && sudo bhatti serve

  ⚠  BACK UP: /var/lib/bhatti/age.key
     If lost, all encrypted secrets become unrecoverable.
============================================
```

For a systemd service that starts on boot:

```bash
sudo ./scripts/install.sh --systemd
```

### Start the daemon

```bash
cd /var/lib/bhatti && sudo bhatti serve
```

### Create users

Each user gets their own API key, sandbox limit, resource caps, and isolated network:

```bash
sudo bhatti user create --name alice --max-sandboxes 5
# → API key: bht_...  (shown once, save it now)

sudo bhatti user create --name bob --max-sandboxes 10 --max-cpus 4 --max-memory 4096

sudo bhatti user list
# ID           NAME                 SANDBOXES  CPUS   MEM    SUBNET
# usr_admin    admin                0/50       4      4096   1
# usr_a1b2     alice                0/5        4      4096   2
# usr_c3d4     bob                  0/10       4      4096   3
```

Users are isolated at every layer:
- **API**: each user sees only their own sandboxes and secrets
- **Network**: each user gets a dedicated bridge and `/24` subnet — VMs from different users cannot communicate
- **Resources**: per-user sandbox count limits and CPU/memory caps
- **Rate limits**: per-user token buckets (10 creates/min, 120 execs/min)

Give alice her API key. She installs the CLI ([Option A](#option-a-use-the-cli-remote-user)), runs `bhatti setup`, and she's in.

### Key rotation

```bash
sudo bhatti user rotate-key alice
# → New key: bht_...  (old key immediately invalidated)
```

### Secrets

```bash
bhatti secret set API_KEY sk-abc123
bhatti secret list
bhatti secret delete API_KEY
```

Secrets are encrypted at rest with [age](https://age-encryption.org/) and scoped per user.

---

## What Just Happened

When you ran `bhatti create --name dev`:

1. The server authenticated your API key (SHA-256 hash lookup), checked your sandbox limit, and validated the name
2. It copied the base rootfs (CoW clone if filesystem supports it), created a config drive with hostname/DNS/auth token, allocated a TAP device on your user's bridge network, started a Firecracker process, configured it over the Unix socket API, booted the kernel, and waited for lohar (the guest agent) to respond
3. The sandbox is now running with its own kernel, its own filesystem, and its own network — isolated from other users' sandboxes by separate L2 bridge segments and iptables rules

When you left the sandbox idle for 30 seconds, it automatically transitioned to *warm* (vCPUs paused, ~400µs resume). After 30 minutes idle, it would be snapshotted to disk (*cold*, ~50ms resume). The next `exec` transparently restores it. See [Thermal Management](thermal-management.md).

---

## Next Steps

- [Architecture](architecture.md) — how the system fits together
- [API Reference](api-reference.md) — REST and WebSocket endpoints
- [CLI Reference](cli-reference.md) — all commands
- [Design Decisions](decisions.md) — why things are the way they are
