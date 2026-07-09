> [!WARNING]
> **DEPRECATED — do not edit.**
> The canonical, maintained version of this page is at
> <https://bhatti.sh/docs/contributing/adding-a-tier/>.
> This file is kept only for git history and may be removed in a future
> cleanup. See [`docs/README.md`](./README.md) for the redirect index.

---

# Rootfs Tiers

Tiers are pre-built ext4 root filesystem images that ship as base environments
for sandboxes. Each tier builds on `minimal` and adds specific tooling.

| Tier | Description | Approx Size |
|------|-------------|-------------|
| `minimal` | Bare Ubuntu 24.04 | ~200MB |
| `browser` | + Chromium/Playwright | ~600MB |
| `docker` | + Docker Engine | ~550MB |
| `computer` | + Full desktop (KasmVNC + XFCE + Chromium) | ~1.5GB |

## Computer tier (KasmVNC desktop)

A full graphical Linux desktop inside a Firecracker microVM: KasmVNC + XFCE +
Chromium, served over a single port (6080) as an HTTP/WebSocket web client.

### First-time use

```bash
bhatti create --name desktop --image computer --cpus 2 --memory 4096 --disk-size 8192
bhatti publish desktop -p 6080
bhatti exec desktop -- vnc-creds          # ← prints username + password
# Open the URL printed by `publish` in your browser, log in.
```

**Why `--cpus 2` minimum.** KasmVNC's encoder thread count is sized to the
guest's vCPUs (it leaves one for the desktop itself). On `--cpus 1` the encoder,
the X server, XFCE, and Chromium all share one core — expect <10 fps. Two cores
is the practical floor for a usable desktop; four is comfortable.

### Credentials

A random 16-character password is generated **on first boot of each sandbox**
(not bake-time). It is hashed into `/root/.kasmpasswd` for KasmVNC and stored
cleartext in `/root/.vnc/cleartext` (root-only) for the `vnc-creds` helper.

- Each sandbox you create gets its own password.
- The published rootfs image carries no shared secret.
- Snapshot/resume preserves the existing password.
- `bhatti image save` will bake the current password into the saved image —
  treat saved-from-running images like any other secret-bearing artifact.

Retrieve them anytime:

```bash
bhatti exec desktop -- vnc-creds          # human-readable
bhatti exec desktop -- vnc-creds --json   # for scripts/agents
```

### Tunables

Pass at create time with `--env`:

| Variable | Default | What it controls |
|---|---|---|
| `DISPLAY_WIDTH`  | `1280`         | X server geometry width |
| `DISPLAY_HEIGHT` | `720`          | X server geometry height |
| `DISPLAY_DEPTH`  | `24`           | colour depth (16/24/32) |
| `KASM_FRAMERATE` | `60`           | max frames/sec the encoder will emit (matches KasmVNC's upstream default) |
| `KASM_THREADS`   | `nproc - 1`    | encoder thread count; default leaves 1 vCPU for the desktop |

```bash
bhatti create --name desktop --image computer --cpus 4 --memory 4096 \
    --env DISPLAY_WIDTH=1920 --env DISPLAY_HEIGHT=1080 \
    --env KASM_FRAMERATE=30   # cap the encoder for low-bandwidth links
```

### Beyond the env knobs

KasmVNC has dozens of options bhatti deliberately does not surface (dynamic
quality bounds, video-mode thresholds, scaling algorithms, DLP/clipboard
policy, etc.). For those, edit `/etc/kasmvnc/kasmvnc.yaml` inside the sandbox
and reconnect. The upstream documentation is authoritative:

- Options reference: <https://github.com/kasmtech/KasmVNC/wiki/Video-Rendering-Options>
- Stats / control API: <https://github.com/kasmtech/KasmVNC/wiki/API>
- Browser-side tuning: <https://github.com/kasmtech/KasmVNC/wiki/Browser-Support>

### Agent helpers (run via `bhatti exec`)

| Helper | Purpose |
|---|---|
| `vnc-creds [--json]` | Print the username and password for this sandbox |
| `screenshot [--base64]` | Capture the current display as PNG |
| `screen-size` | Print the current X resolution |
| `active-window` / `list-windows` | Inspect window state |
| `xdotool ...` | Drive mouse/keyboard input |
| `chromium-browser <url>` | Launch Chromium with sane flags |

`DISPLAY=:99` is pre-set for `bhatti exec` (via `/run/bhatti/env`), so these
just work without any environment plumbing.

## How tiers are discovered

The server **auto-discovers** tiers at startup by globbing for
`rootfs-*-{arch}.ext4` in the images directory (`/var/lib/bhatti/images/`).
Any file matching the pattern is registered as a built-in admin image.
There is no hardcoded tier list in the server — drop a new rootfs file and
it appears in `bhatti image list` on next restart.

## Installing additional tiers on an existing server

By default, the install script only downloads the single tier configured in
`/etc/bhatti/config.yaml`. Pass `--tiers` to pull additional tiers:

```bash
# Install all available tiers
curl -fsSL bhatti.sh/install | sudo bash -s -- --tiers all

# Install specific tiers (comma-separated)
curl -fsSL bhatti.sh/install | sudo bash -s -- --tiers computer,browser
```

The `bash -s -- ...` syntax passes flags through the curl pipe. The
server discovers the new rootfs files on restart and registers them
automatically. No config changes needed — the config only controls which
tier is the default for `bhatti create` when no `--image` is specified.

## Adding a new tier

### 1. Create the tier script

Add `scripts/tiers/<name>.sh`. This runs inside a chroot during
`build-tier.sh`. It receives these env vars:

- `$MOUNT` — chroot mount point
- `$ARCH` / `$DEB_ARCH` — target architecture
- `$AGENT` — path to lohar binary
- `$SCRIPT_DIR` — path to `scripts/`

Most tiers source minimal first:

```bash
#!/bin/bash
set -euo pipefail
"$SCRIPT_DIR/tiers/minimal.sh"
# ... install your packages ...
```

### 2. Register in `scripts/build-tier.sh`

Add a size default to the `case` statement:

```bash
case "$TIER" in
    minimal)  SIZE_MB="${SIZE_MB:-512}" ;;
    browser)  SIZE_MB="${SIZE_MB:-2048}" ;;
    docker)   SIZE_MB="${SIZE_MB:-2048}" ;;
    computer) SIZE_MB="${SIZE_MB:-4096}" ;;
    new-tier) SIZE_MB="${SIZE_MB:-1024}" ;;  # ← add this
    *) echo "unknown tier: $TIER" >&2; exit 1 ;;
esac
```

### 3. Add to CI release matrix

In `.github/workflows/release.yml`, add the tier name to the rootfs job matrix:

```yaml
tier: [minimal, browser, docker, computer, new-tier]
```

Bump `timeout-minutes` if the tier has a heavy build (desktop packages, etc.).

### 4. Add to the install script menu

In `scripts/install.sh`, update the interactive tier selection prompt and its
`case` mapping so users can pick it during `install.sh`:

```bash
echo "    5) new-tier — description (~size)"
# ...
case "${tier_choice:-1}" in
    5) tier="new-tier" ;;
esac
```

Also update the `BHATTI_TIER` env var comment at the top of the file.

### 5. That's it

The server picks up the new rootfs automatically — no Go code changes needed.

## Checklist

```
[ ] scripts/tiers/<name>.sh          — tier build script
[ ] scripts/build-tier.sh            — SIZE_MB default in case statement
[ ] .github/workflows/release.yml    — add to matrix.tier
[ ] scripts/install.sh               — interactive menu + BHATTI_TIER comment
[ ] scripts/install.sh               — add to ALL_KNOWN_TIERS in do_server_update()
```
