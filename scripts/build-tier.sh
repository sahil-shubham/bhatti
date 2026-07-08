#!/bin/bash
# Build a rootfs tier image.
# Usage: sudo ./scripts/build-tier.sh <tier> <arch> <lohar-binary>
#   tier: minimal, browser, docker, computer
#   arch: amd64, arm64
#
# Output: dist/rootfs-<tier>-<arch>.ext4
# Environment:
#   SIZE_MB — image size (default: auto per tier)
#
# For cross-arch builds (e.g., arm64 on amd64 host):
#   sudo apt-get install qemu-user-static  # registers binfmt_misc handlers
#   sudo ./scripts/build-tier.sh minimal arm64 ./lohar-arm64
set -euo pipefail

TIER="${1:?usage: build-tier.sh <tier> <arch> <lohar-binary>}"
ARCH="${2:?}"
AGENT="${3:?}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [[ $EUID -ne 0 ]]; then
    echo "error: must run as root (need mount/chroot)" >&2
    exit 1
fi

if [[ ! -f "$AGENT" ]]; then
    echo "error: lohar binary not found: $AGENT" >&2
    exit 1
fi

# Tier-specific defaults
case "$TIER" in
    minimal)  SIZE_MB="${SIZE_MB:-1024}" ;;
    browser)  SIZE_MB="${SIZE_MB:-2048}" ;;
    docker)   SIZE_MB="${SIZE_MB:-2048}" ;;
    computer) SIZE_MB="${SIZE_MB:-4096}" ;;
    *) echo "unknown tier: $TIER" >&2; exit 1 ;;
esac

case "$ARCH" in
    amd64) DEB_ARCH="amd64"; MIRROR="http://archive.ubuntu.com/ubuntu" ;;
    arm64) DEB_ARCH="arm64"; MIRROR="http://ports.ubuntu.com/ubuntu-ports" ;;
    *) echo "unsupported arch: $ARCH" >&2; exit 1 ;;
esac

IMG="${IMG:-dist/rootfs-${TIER}-${ARCH}.ext4}"
MOUNT="/mnt/bhatti-${TIER}-$$"

mkdir -p dist

# Robust cleanup: kill leaked chroot processes, lazy-unmount everything.
# Lazy unmount (-l) is essential for CI runners where stale mounts from
# a failed previous build would block the next job.
cleanup() {
    set +e
    echo "==> Cleaning up..."
    fuser -km "$MOUNT" 2>/dev/null
    sleep 1
    umount -l "$MOUNT/dev/pts" 2>/dev/null
    umount -l "$MOUNT/dev"     2>/dev/null
    umount -l "$MOUNT/sys"     2>/dev/null
    umount -l "$MOUNT/proc"    2>/dev/null
    umount -l "$MOUNT"         2>/dev/null
    rmdir "$MOUNT"             2>/dev/null
}
trap cleanup EXIT

# Create ext4 image
echo "==> Creating ${SIZE_MB}MB ext4 image for ${TIER}-${ARCH}..."
dd if=/dev/zero of="$IMG" bs=1M count="$SIZE_MB" status=progress
mkfs.ext4 -F -q "$IMG"
mkdir -p "$MOUNT"
mount "$IMG" "$MOUNT"

# Bootstrap minimal Ubuntu
echo "==> Bootstrapping Ubuntu 24.04 (noble) ${DEB_ARCH}..."
debootstrap --variant=minbase --arch="$DEB_ARCH" noble "$MOUNT" "$MIRROR"

# Set up chroot
cp /etc/resolv.conf "$MOUNT/etc/resolv.conf"
mount --bind /proc    "$MOUNT/proc"
mount --bind /sys     "$MOUNT/sys"
mount --bind /dev     "$MOUNT/dev"
mount --bind /dev/pts "$MOUNT/dev/pts"

# Run tier script
export MOUNT ARCH DEB_ARCH AGENT SCRIPT_DIR
echo "==> Running tier script: ${TIER}.sh"
"$SCRIPT_DIR/tiers/${TIER}.sh"

# The krucible engine boots /init.krun as PID 1 (engine ExecPath, cmd/vmm PID-1
# mode). Tier scripts install lohar at /usr/local/bin/lohar (for the systemctl/
# journalctl shims); also place it at /init.krun so the release image is
# bootable on krucible — without this the guest panics with
# "Requested init /init.krun failed (error -2)". (Integration CI builds its own
# rootfs with /init.krun, so this only surfaced on a real release image.)
cp "$AGENT" "$MOUNT/init.krun"
chmod 755 "$MOUNT/init.krun"
echo "==> installed /init.krun (lohar PID 1)"

echo "==> Built: $IMG ($(du -h "$IMG" | cut -f1))"
