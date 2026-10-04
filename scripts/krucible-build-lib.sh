#!/usr/bin/env bash
# Build libkrucible (upstream libkrun, tracked on our krucible-v2 branch) and
# assemble a local install prefix that `make vmm` links against via
# PKG_CONFIG_PATH. bhatti always boots its own lean kernel and lohar is
# /init.krun, so neither libkrunfw nor libkrun's bundled init is needed.
#
# Usage: scripts/krucible-build-lib.sh [LIBKRUCIBLE_SRC] [PREFIX]
set -euo pipefail
SRC="$(cd "${1:-libkrucible}" && pwd)"
PREFIX="${2:-$SRC/_install}"
# libkrun.pc embeds the prefix, and cgo resolves it from wherever `make vmm`
# runs, so it must be absolute.
mkdir -p "$PREFIX" && PREFIX="$(cd "$PREFIX" && pwd)"
OS="$(uname -s)"

echo "==> building libkrucible at $SRC (release)"
# blk: virtio-block (qcow2/raw root + volumes). net: virtio-net over a
# unixstream socket (the bhatti-netd gateway). ffi: the C API itself; upstream's
# builder/handle API is only exported with it.
( cd "$SRC" && CC_LINUX=cc cargo build --release -p libkrun --no-default-features --features blk,net,ffi )

echo "==> assembling install prefix at $PREFIX"
rm -rf "$PREFIX"
mkdir -p "$PREFIX/lib/pkgconfig" "$PREFIX/include"
( cd "$SRC" && make libkrun.pc PREFIX="$PREFIX" >/dev/null )
cp "$SRC/include/libkrun.h" "$PREFIX/include/"
cp "$SRC/libkrun.pc" "$PREFIX/lib/pkgconfig/"

# libkrun.pc's libdir is lib64 on Linux but lib on macOS (libkrucible's Makefile:
# LIBDIR_Linux=lib64). Install the lib where the .pc — and thus the cgo linker
# via pkg-config — expects it, derived from the .pc itself rather than hardcoding
# `lib` (which silently breaks the Linux link with `cannot find -lkrun`).
# Mirrors scripts/krucible-linux-bringup.sh.
LIBDIR="$(awk -F= '/^libdir=/{print $2}' "$SRC/libkrun.pc")"
mkdir -p "$LIBDIR"
VER="$(grep -E '^FULL_VERSION' "$SRC/Makefile" | head -1 | sed 's/.*= *//')"
# The soname/install-name major must track the fork's ABI_VERSION (libkrun 2.0
# bumped it to 2 -> libkrun.2.dylib / libkrun.so.2). Hardcoding ".1" silently
# breaks the dlopen once upstream bumps the major, so derive it.
ABI="$(grep -E '^ABI_VERSION' "$SRC/Makefile" | head -1 | sed 's/.*= *//')"
if [ "$OS" = "Darwin" ]; then
  cp "$SRC/target/release/libkrun.dylib" "$LIBDIR/libkrun.$VER.dylib"
  ( cd "$LIBDIR" && ln -sf "libkrun.$VER.dylib" "libkrun.$ABI.dylib" && ln -sf "libkrun.$ABI.dylib" libkrun.dylib )
else
  cp "$SRC/target/release/libkrun.so" "$LIBDIR/libkrun.so.$VER"
  ( cd "$LIBDIR" && ln -sf "libkrun.so.$VER" "libkrun.so.$ABI" && ln -sf "libkrun.so.$ABI" libkrun.so )
fi

echo "==> done. libkrucible libkrun: $LIBDIR"
