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
( cd "$SRC" && CC_LINUX=cc cargo build --release -p libkrun --no-default-features )

echo "==> assembling install prefix at $PREFIX"
rm -rf "$PREFIX"
mkdir -p "$PREFIX/lib/pkgconfig" "$PREFIX/include"
( cd "$SRC" && make libkrun.pc PREFIX="$PREFIX" >/dev/null )
cp "$SRC/include/libkrun.h" "$PREFIX/include/"
cp "$SRC/libkrun.pc" "$PREFIX/lib/pkgconfig/"

VER="$(grep -E '^FULL_VERSION' "$SRC/Makefile" | head -1 | sed 's/.*= *//')"
if [ "$OS" = "Darwin" ]; then
  cp "$SRC/target/release/libkrun.dylib" "$PREFIX/lib/libkrun.$VER.dylib"
  ( cd "$PREFIX/lib" && ln -sf "libkrun.$VER.dylib" libkrun.1.dylib && ln -sf libkrun.1.dylib libkrun.dylib )
else
  cp "$SRC/target/release/libkrun.so" "$PREFIX/lib/libkrun.so.$VER"
  ( cd "$PREFIX/lib" && ln -sf "libkrun.so.$VER" libkrun.so.1 && ln -sf libkrun.so.1 libkrun.so )
fi

echo "==> done. libkrucible libkrun: $PREFIX/lib"
echo "    (runtime also needs libkrunfw on the dyld path — Homebrew provides it)"
