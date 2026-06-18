#!/usr/bin/env bash
# Build libkrucible (our libkrun fork) and assemble a local install prefix that
# `make vmm` links against via PKG_CONFIG_PATH. Built with --no-default-features
# (no bundled init — lohar is /init.krun), which skips the init cross-compile so
# no lld/Debian-sysroot is needed. libkrunfw is taken from Homebrew at runtime.
#
# Usage: scripts/krucible-build-lib.sh [LIBKRUCIBLE_SRC] [PREFIX]
set -euo pipefail
SRC="$(cd "${1:-../libkrucible}" && pwd)"
PREFIX="${2:-$SRC/_install}"
OS="$(uname -s)"

echo "==> building libkrucible at $SRC (release, no-default-features)"
# --no-default-features drops the bundled init (lohar is /init.krun) so we don't
# need the init cross-compile toolchain. --features blk enables the virtio-block
# device + krun_set_root_disk/add_disk2 — the block/qcow2 root the cold tier
# snapshots (see docs/PLAN-krucible-cold-tier.md §1).
( cd "$SRC" && CC_LINUX=cc cargo build --release -p libkrun --no-default-features --features blk )

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
if [ "$OS" = "Darwin" ]; then
  cp "$SRC/target/release/libkrun.dylib" "$LIBDIR/libkrun.$VER.dylib"
  ( cd "$LIBDIR" && ln -sf "libkrun.$VER.dylib" libkrun.1.dylib && ln -sf libkrun.1.dylib libkrun.dylib )
else
  cp "$SRC/target/release/libkrun.so" "$LIBDIR/libkrun.so.$VER"
  ( cd "$LIBDIR" && ln -sf "libkrun.so.$VER" libkrun.so.1 && ln -sf libkrun.so.1 libkrun.so )
fi

echo "==> done. libkrucible libkrun: $LIBDIR"
echo "    (runtime also needs libkrunfw on the dyld path — Homebrew provides it)"
