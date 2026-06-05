#!/bin/bash
# Build the bhatti kernel from Firecracker CI config + Docker flags.
# Usage: ./scripts/build-kernel.sh [arch]
#   arch: x86_64 (default) or aarch64
#
# Requirements: build-essential flex bison libelf-dev libssl-dev bc
# For aarch64 cross-compile: gcc-aarch64-linux-gnu
#
# Output: dist/vmlinux-<version>-<arch>
set -euo pipefail

ARCH="${1:-x86_64}"
KERNEL_VERSION="6.1.155"
FC_CI_VERSION="v1.15"

case "$ARCH" in
    x86_64)  KARCH="x86_64"; CROSS="" ;;
    aarch64) KARCH="arm64";  CROSS="ARCH=arm64 CROSS_COMPILE=aarch64-linux-gnu-" ;;
    *) echo "unsupported arch: $ARCH" >&2; exit 1 ;;
esac

# Download kernel source if not cached
if [ ! -d "linux-${KERNEL_VERSION}" ]; then
    echo "==> Downloading kernel ${KERNEL_VERSION} source..."
    curl -fsSL "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-${KERNEL_VERSION}.tar.xz" | tar xJ
fi
cd "linux-${KERNEL_VERSION}"

# Start from Firecracker CI config
echo "==> Downloading Firecracker CI config (${FC_CI_VERSION}/${ARCH})..."
curl -fsSL "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/${FC_CI_VERSION}/${ARCH}/vmlinux-${KERNEL_VERSION}.config" -o .config

# Audit: show current state of our flags before modification
echo "==> Current state of bhatti flags in CI config:"
for flag in IP_NF_RAW IP6_NF_RAW BRIDGE VETH OVERLAY_FS NF_CONNTRACK \
    NETFILTER_XT_MATCH_CONNTRACK IP_NF_SECURITY IP6_NF_SECURITY \
    NET_CLS_CGROUP NETFILTER_XT_MARK FUSE_FS; do
    grep "CONFIG_${flag}[= ]" .config 2>/dev/null || echo "# CONFIG_${flag} is not set"
done

# Strip hardware that doesn't exist in Firecracker VMs.
# The FC CI config inherits i8042 (PS/2 keyboard controller) from x86
# defconfig. The driver probes via ACPI, finds PNP0303, registers a serio
# port, then atkbd tries to talk to it. Since there's no real PS/2 hw,
# i8042_wait_read() spins for I8042_CTL_TIMEOUT * udelay(50) = 500ms
# before giving up. This delays every VM boot by ~530ms on x86_64.
# arm64 doesn't have i8042 at all, which is why Pi boots are faster.
echo "==> Stripping non-existent hardware drivers..."
scripts/config --disable CONFIG_SERIO_I8042
scripts/config --disable CONFIG_KEYBOARD_ATKBD

# Apply bhatti additions (idempotent — safe if already =y)
echo "==> Applying bhatti kernel config (22 flags)..."

# Docker bridge networking (hard blockers)
scripts/config --enable CONFIG_IP_NF_RAW
scripts/config --enable CONFIG_IP6_NF_RAW

# Docker container plumbing
scripts/config --enable CONFIG_BRIDGE
scripts/config --enable CONFIG_VETH
scripts/config --enable CONFIG_OVERLAY_FS
scripts/config --enable CONFIG_NF_CONNTRACK
scripts/config --enable CONFIG_NETFILTER_XT_MATCH_CONNTRACK

# Docker security tables
scripts/config --enable CONFIG_IP_NF_SECURITY
scripts/config --enable CONFIG_IP6_NF_SECURITY

# Docker traffic shaping / kube-proxy iptables rules
scripts/config --enable CONFIG_NET_CLS_CGROUP
scripts/config --enable CONFIG_NETFILTER_XT_MARK
scripts/config --enable CONFIG_NETFILTER_XT_MATCH_COMMENT  # kube-proxy requires comment match

# Docker advanced networking (VXLAN for overlay networks, k3s flannel default)
scripts/config --enable CONFIG_VXLAN
scripts/config --enable CONFIG_DUMMY
scripts/config --enable CONFIG_MACVLAN
scripts/config --enable CONFIG_IPVLAN

# TUN/TAP (VPNs: Tailscale, WireGuard-go, OpenVPN, cloudflared)
scripts/config --enable CONFIG_TUN

# FUSE (sshfs, rclone, s3fs, Mesa, AppImage, fuse-overlayfs)
scripts/config --enable CONFIG_FUSE_FS

# WireGuard VPN (in-kernel, faster than wireguard-go)
scripts/config --enable CONFIG_WIREGUARD

# Kernel TLS offload (nginx, HAProxy)
scripts/config --enable CONFIG_TLS

# Security: AppArmor + Landlock (Docker MAC, unprivileged sandboxing)
scripts/config --enable CONFIG_SECURITY_APPARMOR
scripts/config --enable CONFIG_SECURITY_LANDLOCK

# Resolve dependencies (turns on transitive deps, answers new prompts with defaults)
# shellcheck disable=SC2086
make $CROSS olddefconfig

# Post-build verification: ensure critical flags survived olddefconfig
echo "==> Verifying critical flags in final config..."
MISSING=0
for flag in IP_NF_RAW IP6_NF_RAW BRIDGE VETH OVERLAY_FS NF_CONNTRACK NETFILTER_XT_MATCH_CONNTRACK \
    NETFILTER_XT_MATCH_COMMENT VXLAN TUN FUSE_FS WIREGUARD TLS; do
    if ! grep -q "CONFIG_${flag}=y" .config; then
        echo "FATAL: CONFIG_${flag} not set to =y after olddefconfig" >&2
        MISSING=1
    fi
done
if [ "$MISSING" -eq 1 ]; then
    echo "Aborting: critical kernel flags missing. Check dependency conflicts." >&2
    exit 1
fi

# Build
# On x86_64, Firecracker loads the vmlinux ELF directly.
# On aarch64, Firecracker requires arch/arm64/boot/Image (PE/COFF format).
if [ "$ARCH" = "aarch64" ]; then
    echo "==> Building Image ($(nproc) cores)..."
    # shellcheck disable=SC2086
    make $CROSS -j"$(nproc)" Image
    mkdir -p ../dist
    cp arch/arm64/boot/Image "../dist/vmlinux-${KERNEL_VERSION}-${ARCH}"
else
    echo "==> Building vmlinux ($(nproc) cores)..."
    # shellcheck disable=SC2086
    make $CROSS -j"$(nproc)" vmlinux
    mkdir -p ../dist
    cp vmlinux "../dist/vmlinux-${KERNEL_VERSION}-${ARCH}"
fi
echo "==> Built: dist/vmlinux-${KERNEL_VERSION}-${ARCH} ($(du -h "../dist/vmlinux-${KERNEL_VERSION}-${ARCH}" | cut -f1))"
