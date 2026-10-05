#!/usr/bin/env bash
# Bring up the krucible engine on a Linux/KVM host (the cluster: arm64 Pis +
# amd64 box) so the full VM-level krucible suite — agent, warm tier, and
# recovery adopt-live — runs there, not just on the Mac.
#
# What it does (idempotent; safe to re-run):
#   1. apt build deps + Go + rustup
#   2. fetch the lean guest kernel from the latest release into dist/kernel/
#      (the VM tests and a dev daemon pick it up from there)
#   3. build libkrucible (upstream libkrun → libkrun.so) + install prefix
#   4. build bhatti-vmm (cgo, links libkrun — no codesigning on Linux)
#
# Layout: this script lives in the bhatti repo; libkrucible is a git submodule
# at ./libkrucible (`git submodule update --init` after clone).
#
# Usage: scripts/krucible-linux-bringup.sh
set -euo pipefail
cd "$(dirname "$0")/.."
REPO="$(pwd)"
LIBKRUCIBLE="${LIBKRUCIBLE:-$REPO/libkrucible}"
ARCH="$(uname -m)"   # aarch64 | x86_64

log() { echo "==> $*"; }

# --- 1. toolchain + build deps ---
log "installing build deps (sudo apt)"
sudo apt-get update -y
sudo apt-get install -y \
  curl git build-essential pkg-config zstd

if ! command -v cargo >/dev/null; then
  log "installing rustup"
  curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y
fi
# shellcheck disable=SC1091
[ -f "$HOME/.cargo/env" ] && . "$HOME/.cargo/env"

# Go (the module requires it; distro Go is often older).
NEED_GO="$(awk '/^go /{print $2}' go.mod)"
if ! command -v go >/dev/null || [ "$(go env GOVERSION 2>/dev/null | sed 's/go//')" \< "$NEED_GO" ]; then
  GOTARBALL="go${NEED_GO}.linux-${ARCH/aarch64/arm64}.tar.gz"; GOTARBALL="${GOTARBALL/x86_64/amd64}"
  log "installing Go ($GOTARBALL)"
  curl -fsSL "https://go.dev/dl/$GOTARBALL" -o "/tmp/$GOTARBALL"
  sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf "/tmp/$GOTARBALL"
  export PATH="/usr/local/go/bin:$PATH"
fi

# --- 2. lean guest kernel (bhatti-vmm boots nothing else) ---
GOARCH="${ARCH/aarch64/arm64}"; GOARCH="${GOARCH/x86_64/amd64}"
if ! ls "$REPO"/dist/kernel/*-lean-*-"$ARCH" >/dev/null 2>&1; then
  log "fetching the lean kernel from the latest release"
  URL="$(curl -fsSL https://api.github.com/repos/sahil-shubham/bhatti/releases/latest |
    grep -oE "https://[^\"]*-linux-${GOARCH}\.tar\.zst" | head -1)"
  mkdir -p "$REPO/dist/kernel"
  curl -fsSL "$URL" | zstd -dc | tar -x -C "$REPO/dist/kernel" --strip-components=2 --wildcards '*/kernel/*-lean-*'
else
  log "lean kernel already in dist/kernel — skipping"
fi

# --- 3. libkrucible (libkrun) — the same build `make krucible` and release use ---
log "building libkrucible"
KPREFIX="$LIBKRUCIBLE/_install"
"$REPO/scripts/krucible-build-lib.sh" "$LIBKRUCIBLE" "$KPREFIX"
LIBDIR="$(awk -F= '/^libdir=/{print $2}' "$KPREFIX/lib/pkgconfig/libkrun.pc")"

# --- 4. bhatti-vmm (cgo helper; no codesigning on Linux) ---
log "building bhatti-vmm"
export PKG_CONFIG_PATH="$KPREFIX/lib/pkgconfig:${PKG_CONFIG_PATH:-}"
export LD_LIBRARY_PATH="$LIBDIR:${LD_LIBRARY_PATH:-}"
CGO_ENABLED=1 go build -tags krucible -o "$REPO/bhatti-vmm" ./cmd/vmm

cat <<EOF

==> done. krucible engine built for linux/$ARCH.
    kernel:      $(ls "$REPO"/dist/kernel/*-lean-*-"$ARCH")
    libkrucible: $LIBDIR (libkrun.so)
    helper:      $REPO/bhatti-vmm

Run the krucible suite (also needs: CGO_ENABLED=0 go build -o bhatti-netd ./cmd/bhatti-netd):
  export PKG_CONFIG_PATH=$KPREFIX/lib/pkgconfig
  go test -tags krucible ./pkg/engine/krucible/ -v
EOF
