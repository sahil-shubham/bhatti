#!/usr/bin/env bash
#
# Build the base rootfs ext4 image for Firecracker VMs.
# Supports aarch64 and x86_64.
#
# Usage:
#   sudo ./build-rootfs.sh /path/to/lohar-binary
#
# Environment:
#   IMG — output path (default: auto-detected by arch)
#
# Requires: debootstrap, mkfs.ext4 (apt-get install debootstrap e2fsprogs)
#
set -euo pipefail

SIZE_MB=2048
MOUNT="/mnt/bhatti-rootfs"
AGENT="${1:-}"
SANDBOX_DIR="${SANDBOX_DIR:-}"

# Detect architecture
HOST_ARCH=$(uname -m)
case "$HOST_ARCH" in
    aarch64)
        DEB_ARCH="arm64"
        MIRROR="http://ports.ubuntu.com/ubuntu-ports"
        ;;
    x86_64)
        DEB_ARCH="amd64"
        MIRROR="http://archive.ubuntu.com/ubuntu"
        ;;
    *)
        echo "error: unsupported architecture $HOST_ARCH" >&2
        exit 1
        ;;
esac

IMG="${IMG:-/var/lib/bhatti/images/rootfs-base-${DEB_ARCH}.ext4}"

if [[ $EUID -ne 0 ]]; then
    echo "error: must run as root (need mount/chroot)" >&2
    exit 1
fi

if [[ -z "$AGENT" || ! -f "$AGENT" ]]; then
    echo "error: agent binary not found: $AGENT" >&2
    echo "usage: sudo $0 /path/to/lohar" >&2
    exit 1
fi

# --- Cleanup on exit ---
cleanup() {
    echo "==> Cleaning up mounts..."
    umount "$MOUNT/dev/pts" 2>/dev/null || true
    umount "$MOUNT/dev"     2>/dev/null || true
    umount "$MOUNT/sys"     2>/dev/null || true
    umount "$MOUNT/proc"    2>/dev/null || true
    umount "$MOUNT"         2>/dev/null || true
    rmdir "$MOUNT"          2>/dev/null || true
}
trap cleanup EXIT

# --- Create ext4 image ---
echo "==> Creating ${SIZE_MB}MB ext4 image at $IMG..."
mkdir -p "$(dirname "$IMG")"
dd if=/dev/zero of="$IMG" bs=1M count="$SIZE_MB" status=progress
mkfs.ext4 -F "$IMG"

mkdir -p "$MOUNT"
mount "$IMG" "$MOUNT"

# --- Bootstrap Ubuntu ---
echo "==> Bootstrapping Ubuntu 24.04 (noble) ${DEB_ARCH}..."
which debootstrap >/dev/null || apt-get install -y debootstrap
debootstrap --arch="${DEB_ARCH}" noble "$MOUNT" "${MIRROR}"

# DNS for chroot network access.
cp /etc/resolv.conf "$MOUNT/etc/resolv.conf"

# Enable universe repo (needed for ripgrep, fd-find)
echo "deb ${MIRROR} noble main universe" > "$MOUNT/etc/apt/sources.list"

# Bind-mount for chroot.
mount --bind /proc    "$MOUNT/proc"
mount --bind /sys     "$MOUNT/sys"
mount --bind /dev     "$MOUNT/dev"
mount --bind /dev/pts "$MOUNT/dev/pts"

# --- Install packages ---
echo "==> Installing packages..."
chroot "$MOUNT" /bin/bash -c '
set -eu
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y --no-install-recommends \
    zsh git curl wget ca-certificates gnupg \
    tmux vim-tiny htop jq unzip xz-utils \
    locales sudo socat iproute2 \
    ripgrep fd-find

# fd-find installs as fdfind on Ubuntu, symlink to fd
ln -sf /usr/bin/fdfind /usr/local/bin/fd

sed -i "/en_US.UTF-8/s/^# //g" /etc/locale.gen
locale-gen

# Starship prompt
curl -fsSL https://starship.rs/install.sh | sh -s -- -y

# Create user
useradd -m -s /bin/zsh -G sudo lohar
echo "lohar ALL=(ALL) NOPASSWD:ALL" >> /etc/sudoers

# Node.js
NODE_VERSION=22.16.0
case $(dpkg --print-architecture) in
    amd64) NODE_ARCH=x64 ;;
    arm64) NODE_ARCH=arm64 ;;
    *) echo "unsupported arch"; exit 1 ;;
esac
curl -fsSL "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${NODE_ARCH}.tar.xz" \
    | tar -xJ --strip-components=1 -C /usr/local

# Claude Code
npm install -g @anthropic-ai/claude-code

apt-get clean
rm -rf /var/lib/apt/lists/*
'

# --- Shell configs ---
echo "==> Installing shell configs..."
# Try to find sandbox/ configs relative to this script, or via SANDBOX_DIR.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SRC="${SANDBOX_DIR:-$(dirname "$SCRIPT_DIR")/sandbox}"

if [[ -f "$SRC/zshrc" ]]; then
    cp "$SRC/zshrc" "$MOUNT/home/lohar/.zshrc"
    chown 1000:1000 "$MOUNT/home/lohar/.zshrc"
    echo "  copied zshrc"
else
    echo "  warning: $SRC/zshrc not found, skipping"
fi

if [[ -f "$SRC/tmux.conf" ]]; then
    cp "$SRC/tmux.conf" "$MOUNT/home/lohar/.tmux.conf"
    chown 1000:1000 "$MOUNT/home/lohar/.tmux.conf"
    echo "  copied tmux.conf"
else
    echo "  warning: $SRC/tmux.conf not found, skipping"
fi

# --- Shell plugins ---
echo "==> Installing shell plugins..."
chroot "$MOUNT" su - lohar -c '
# tmux plugins
mkdir -p ~/.tmux/plugins
git clone --depth 1 https://github.com/tmux-plugins/tmux-sensible ~/.tmux/plugins/tmux-sensible
git clone --depth 1 https://github.com/dracula/tmux ~/.tmux/plugins/tmux
git clone --depth 1 https://github.com/tmux-plugins/tmux-cpu ~/.tmux/plugins/tmux-cpu

# zsh via zinit
git clone --depth 1 https://github.com/zdharma-continuum/zinit.git ~/.local/share/zinit/zinit.git
mkdir -p ~/.local/share/zinit/plugins
git clone --depth 1 https://github.com/zsh-users/zsh-syntax-highlighting ~/.local/share/zinit/plugins/zsh-users---zsh-syntax-highlighting
git clone --depth 1 https://github.com/zsh-users/zsh-autosuggestions ~/.local/share/zinit/plugins/zsh-users---zsh-autosuggestions
git clone --depth 1 https://github.com/agkozak/zsh-z ~/.local/share/zinit/plugins/agkozak---zsh-z
'

# --- Agent + workspace ---
echo "==> Installing agent and workspace..."
mkdir -p "$MOUNT/workspace"
chown 1000:1000 "$MOUNT/workspace"

cp "$AGENT" "$MOUNT/usr/local/bin/lohar"
chmod 755 "$MOUNT/usr/local/bin/lohar"

# Static DNS for the guest VM (no NetworkManager).
cat > "$MOUNT/etc/resolv.conf" << 'DNSEOF'
nameserver 1.1.1.1
nameserver 8.8.8.8
DNSEOF

echo ""
echo "==> Done! Rootfs: $IMG ($(du -h "$IMG" | cut -f1))"
