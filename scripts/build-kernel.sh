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
    NETFILTER_XT_CONNTRACK IP_NF_SECURITY IP6_NF_SECURITY \
    NET_CLS_CGROUP NETFILTER_XT_MARK; do
    grep "CONFIG_${flag}[= ]" .config 2>/dev/null || echo "# CONFIG_${flag} is not set"
done

# Apply bhatti additions (idempotent — safe if already =y)
echo "==> Applying bhatti kernel config (13 flags)..."

# Docker bridge networking (hard blockers)
scripts/config --enable CONFIG_IP_NF_RAW
scripts/config --enable CONFIG_IP6_NF_RAW

# Docker container plumbing
scripts/config --enable CONFIG_BRIDGE
scripts/config --enable CONFIG_VETH
scripts/config --enable CONFIG_OVERLAY_FS
scripts/config --enable CONFIG_NF_CONNTRACK
scripts/config --enable CONFIG_NETFILTER_XT_CONNTRACK

# Docker security tables
scripts/config --enable CONFIG_IP_NF_SECURITY
scripts/config --enable CONFIG_IP6_NF_SECURITY

# Docker traffic shaping
scripts/config --enable CONFIG_NET_CLS_CGROUP
scripts/config --enable CONFIG_NETFILTER_XT_MARK

# Resolve dependencies (turns on transitive deps, answers new prompts with defaults)
# shellcheck disable=SC2086
make $CROSS olddefconfig

# Post-build verification: ensure critical flags survived olddefconfig
echo "==> Verifying critical flags in final config..."
MISSING=0
for flag in IP_NF_RAW IP6_NF_RAW BRIDGE VETH OVERLAY_FS NF_CONNTRACK NETFILTER_XT_CONNTRACK; do
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
echo "==> Building vmlinux ($(nproc) cores)..."
# shellcheck disable=SC2086
make $CROSS -j"$(nproc)" vmlinux

mkdir -p ../dist
cp vmlinux "../dist/vmlinux-${KERNEL_VERSION}-${ARCH}"
echo "==> Built: dist/vmlinux-${KERNEL_VERSION}-${ARCH} ($(du -h "../dist/vmlinux-${KERNEL_VERSION}-${ARCH}" | cut -f1))"
