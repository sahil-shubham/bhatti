#!/usr/bin/env bash
# Bring up the krucible engine on a Linux/KVM host (the cluster: arm64 Pis +
# amd64 box) so the full VM-level krucible suite — agent, warm tier, and
# recovery adopt-live — runs there, not just on the Mac.
#
# What it does (idempotent; safe to re-run):
#   1. apt build deps + kernel-build deps + Go 1.25 + rustup
#   2. build + install libkrunfw (the bundled guest kernel → libkrunfw.so)
#   3. build libkrucible (our libkrun fork → libkrun.so) + install prefix
#   4. build bhatti-vmm (cgo, links libkrun/libkrunfw — no codesigning on Linux)
#
# This is the long pole: (2) is a Linux kernel build and (3) a Rust release
# build — tens of minutes on a 4-core Pi. Run under nohup/tmux and tail the log.
#
# Layout: this script lives in the bhatti repo; libkrucible is a git submodule
# at ./libkrucible (`git submodule update --init` after clone). libkrunfw is
# cloned from its public upstream and pinned to the version the guest kernel matches.
#
# Usage: scripts/krucible-linux-bringup.sh
set -euo pipefail
cd "$(dirname "$0")/.."
REPO="$(pwd)"
LIBKRUCIBLE="${LIBKRUCIBLE:-$REPO/libkrucible}"
LIBKRUNFW_REF="${LIBKRUNFW_REF:-v5.0.0}"            # match the guest kernel (linux-6.12.x)
PREFIX="${PREFIX:-/usr/local}"
ARCH="$(uname -m)"   # aarch64 | x86_64

log() { echo "==> $*"; }

# --- 1. toolchain + build deps ---
log "installing build deps (sudo apt)"
sudo apt-get update -y
sudo apt-get install -y \
  curl git build-essential pkg-config patchelf \
  python3-pyelftools bc kmod cpio flex libncurses5-dev libelf-dev libssl-dev dwarves bison

if ! command -v cargo >/dev/null; then
  log "installing rustup"
  curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y
fi
# shellcheck disable=SC1091
[ -f "$HOME/.cargo/env" ] && . "$HOME/.cargo/env"

# Go 1.25+ (the module requires it; distro Go is often older).
NEED_GO="$(awk '/^go /{print $2}' go.mod)"
if ! command -v go >/dev/null || [ "$(go env GOVERSION 2>/dev/null | sed 's/go//')" \< "$NEED_GO" ]; then
  GOTARBALL="go1.25.7.linux-${ARCH/aarch64/arm64}.tar.gz"; GOTARBALL="${GOTARBALL/x86_64/amd64}"
  log "installing Go ($GOTARBALL)"
  curl -fsSL "https://go.dev/dl/$GOTARBALL" -o "/tmp/$GOTARBALL"
  sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf "/tmp/$GOTARBALL"
  export PATH="/usr/local/go/bin:$PATH"
fi

# --- 2. libkrunfw (guest kernel blob) ---
if [ ! -e "$PREFIX/lib/libkrunfw.so" ] && [ ! -e "$PREFIX/lib64/libkrunfw.so" ]; then
  log "building libkrunfw $LIBKRUNFW_REF (kernel build — slow)"
  SRC=/tmp/libkrunfw
  [ -d "$SRC" ] || git clone --depth 1 --branch "$LIBKRUNFW_REF" https://github.com/containers/libkrunfw "$SRC"
  # libkrunfw pins a specific kernel patch (KERNEL_VERSION); cdn.kernel.org prunes
  # superseded patches (404 from CI egress once a newer patch ships), so override
  # KERNEL_REMOTE to the git.kernel.org origin, whose stable tags are permanent.
  # Its snapshots are .tar.gz, but libkrunfw's `tar xf` auto-detects compression,
  # so the .tar.xz tarball name is harmless.
  KVER_FULL="$(awk -F'= *' '/^KERNEL_VERSION/{print $2; exit}' "$SRC/Makefile")"
  KREMOTE="https://git.kernel.org/pub/scm/linux/kernel/git/stable/linux.git/snapshot/${KVER_FULL}.tar.gz"
  # Build via `cd && make` (NOT `make -C`): libkrunfw's Makefile passes
  # $(MAKEFLAGS) straight into the kernel sub-make, and `-C` auto-adds the `-w`
  # print-directory flag, which then becomes a bogus `make w` kernel target.
  ( cd "$SRC" && make -j"$(nproc)" KERNEL_REMOTE="$KREMOTE" )
  ( cd "$SRC" && sudo make install PREFIX="$PREFIX" )
  sudo ldconfig
else
  log "libkrunfw already installed — skipping"
fi

# --- 3. libkrucible (our libkrun fork) ---
log "building libkrucible (cargo release, --features blk)"
( cd "$LIBKRUCIBLE" && CC_LINUX=cc cargo build --release -p libkrun --no-default-features --features blk )
KPREFIX="$LIBKRUCIBLE/_install"
rm -rf "$KPREFIX"; mkdir -p "$KPREFIX/lib/pkgconfig" "$KPREFIX/include"
( cd "$LIBKRUCIBLE" && make libkrun.pc PREFIX="$KPREFIX" >/dev/null )
# libkrun.pc's libdir is lib64 on Linux, lib on macOS — install libkrun.so where
# the .pc (and thus the cgo linker) expects it.
LIBDIR="$(awk -F= '/^libdir=/{print $2}' "$LIBKRUCIBLE/libkrun.pc")"
mkdir -p "$LIBDIR"
cp "$LIBKRUCIBLE/include/libkrun.h" "$KPREFIX/include/"
cp "$LIBKRUCIBLE/libkrun.pc" "$KPREFIX/lib/pkgconfig/"
VER="$(grep -E '^FULL_VERSION' "$LIBKRUCIBLE/Makefile" | head -1 | sed 's/.*= *//')"
# soname major tracks the fork's ABI_VERSION (libkrun 2.0 -> libkrun.so.2); a
# hardcoded ".1" breaks the cgo/dlopen link once upstream bumps the major.
ABI="$(grep -E '^ABI_VERSION' "$LIBKRUCIBLE/Makefile" | head -1 | sed 's/.*= *//')"
cp "$LIBKRUCIBLE/target/release/libkrun.so" "$LIBDIR/libkrun.so.$VER"
( cd "$LIBDIR" && ln -sf "libkrun.so.$VER" "libkrun.so.$ABI" && ln -sf "libkrun.so.$ABI" libkrun.so )

# --- 4. bhatti-vmm (cgo helper; no codesigning on Linux) ---
log "building bhatti-vmm"
export PKG_CONFIG_PATH="$KPREFIX/lib/pkgconfig:${PKG_CONFIG_PATH:-}"
export LD_LIBRARY_PATH="$LIBDIR:$PREFIX/lib64:$PREFIX/lib:${LD_LIBRARY_PATH:-}"
CGO_ENABLED=1 go build -tags krucible -o "$REPO/bhatti-vmm" ./cmd/vmm

cat <<EOF

==> done. krucible engine built for linux/$ARCH.
    libkrunfw:   $PREFIX (libkrunfw.so)
    libkrucible: $KPREFIX/lib (libkrun.so.$VER)
    helper:      $REPO/bhatti-vmm

Run the krucible suite (warm + agent + recovery; cold tier is macOS-gated):
  export LD_LIBRARY_PATH=$LIBDIR:$PREFIX/lib64:$PREFIX/lib
  go test -tags krucible ./pkg/engine/krucible/ -run 'TestKrucibleAgentSuite|TestKrucibleThermalSuite|TestKrucibleRecovery' -v
EOF
