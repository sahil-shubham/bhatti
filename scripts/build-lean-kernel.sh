#!/usr/bin/env bash
# Build bhatti's lean microVM kernel for the krucible external-kernel boot path
# (krun_set_kernel). A leaner kernel = faster cold-start: measured ~2x vs the
# stock bundled libkrunfw kernel (boot→agent ~312ms vs ~610ms on HVF).
#
# Reproducible via Docker so it runs identically on the macOS dev box and Linux
# CI: kernel.org source (pinned) + our in-repo config (scripts/lean-kernel/) ->
# olddefconfig -> Image (arm64) / vmlinux (x86). No libkrunfw involved.
#
# Usage:
#   scripts/build-lean-kernel.sh [aarch64|x86_64]   # default: host arch
#   KERNEL_VERSION=6.12.91 scripts/build-lean-kernel.sh aarch64
#
# Output: dist/kernel/<vmlinux-lean|Image-lean>-<version>-<arch>
set -euo pipefail
cd "$(dirname "$0")/.."
REPO="$(pwd)"

host_arch="$(uname -m)"; [ "$host_arch" = "arm64" ] && host_arch="aarch64"
ARCH="${1:-$host_arch}"
# Pinned for reproducibility. Override with KERNEL_VERSION=<x.y.z> (must be a
# linux-stable tag). Source is the official kernel.org git origin (git.kernel.org),
# whose tags are permanent — NOT the Fastly CDN (/pub/linux/kernel/*.tar.xz), whose
# tarball subtree is unreachable from CI egress here (returns 404 for every patch).
KERNEL_VERSION="${KERNEL_VERSION:-6.12.94}"

case "$ARCH" in
  aarch64) PLATFORM="linux/arm64"; MAKETARGET="Image"; KIMG="arch/arm64/boot/Image"; OUTBASE="Image-lean" ;;
  x86_64)  PLATFORM="linux/amd64"; MAKETARGET="vmlinux"; KIMG="vmlinux";             OUTBASE="vmlinux-lean" ;;
  *) echo "unsupported arch: $ARCH (use aarch64 or x86_64)" >&2; exit 1 ;;
esac

CONFIG="scripts/lean-kernel/config-lean_${ARCH}"
[ -f "$CONFIG" ] || { echo "ERROR: missing kernel config $CONFIG" >&2; exit 1; }
command -v docker >/dev/null 2>&1 || { echo "ERROR: docker required" >&2; exit 1; }

OUT="${OUTBASE}-${KERNEL_VERSION}-${ARCH}"
mkdir -p "$REPO/dist/kernel"
echo "==> building lean kernel: linux-$KERNEL_VERSION $ARCH ($PLATFORM) -> dist/kernel/$OUT"
[ "$ARCH" != "$host_arch" ] && echo "    (cross-arch via emulation — slow)"

docker run --rm --platform "$PLATFORM" \
  -v "$REPO/dist/kernel:/out" \
  -v "$REPO/$CONFIG:/lean.config:ro" \
  -w /build ubuntu:24.04 bash -c "
set -e
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq && apt-get install -y -qq build-essential flex bison libelf-dev libssl-dev bc curl xz-utils >/dev/null 2>&1
curl -fsSL https://git.kernel.org/pub/scm/linux/kernel/git/stable/linux.git/snapshot/linux-${KERNEL_VERSION}.tar.gz | tar xz
cd linux-${KERNEL_VERSION}
cp /lean.config .config
make olddefconfig >/dev/null
echo \"    config: \$(grep -c =y .config) =y\"
make -j\$(nproc) ${MAKETARGET} >/tmp/build.log 2>&1 || { tail -30 /tmp/build.log; exit 1; }
cp ${KIMG} /out/${OUT}
"

echo "==> done: dist/kernel/$OUT ($(du -h "$REPO/dist/kernel/$OUT" | cut -f1))"
echo "    point the daemon at it: krucible_kernel_image: $REPO/dist/kernel/$OUT"
