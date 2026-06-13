#!/usr/bin/env bash
# krucible P1 smoke test: build the libkrun helper, build a minimal rootfs with
# lohar as PID 1, boot it, and prove the agent answers over the bridged vsock.
#
# Reproduces the S0/P1 validation end-to-end with one command. Requires libkrun
# + libkrunfw (macOS: `brew tap slp/krun && brew trust slp/krun && brew install
# libkrun`). Guest arch == host arch (HVF/KVM, no emulation).
#
# Usage: scripts/krucible-smoke.sh
set -euo pipefail

cd "$(dirname "$0")/.."
REPO="$(pwd)"
WORK="${KRUCIBLE_WORK:-/tmp/krucible-smoke}"
GUEST_ARCH="$(go env GOHOSTARCH)"
mkdir -p "$WORK"

# --- prereqs ---
if ! pkg-config --exists libkrun 2>/dev/null; then
  echo "ERROR: libkrun not found." >&2
  echo "  macOS: brew tap slp/krun && brew trust slp/krun && brew install libkrun" >&2
  exit 1
fi

# Where libkrunfw lives (libkrun dlopen()s it by name at runtime).
LIBDIR=""
for d in "$(brew --prefix 2>/dev/null || true)/lib" /opt/homebrew/lib /usr/local/lib /usr/lib; do
  if ls "$d"/libkrunfw* >/dev/null 2>&1; then LIBDIR="$d"; break; fi
done
[ -n "$LIBDIR" ] || { echo "ERROR: libkrunfw not found in any lib dir" >&2; exit 1; }
echo "libkrun OK; libkrunfw in $LIBDIR; guest arch=$GUEST_ARCH"

# --- build helper + probe ---
echo "==> make vmm"
make vmm >/dev/null
echo "==> build probe"
go build -o "$WORK/krucible-probe" ./cmd/krucible-probe

# --- minimal rootfs: lohar @ /init.krun (PID 1) + a tiny /bin/true for exec ---
echo "==> build rootfs (lohar @ /init.krun)"
ROOT="$WORK/rootfs"
rm -rf "$ROOT"
mkdir -p "$ROOT"/{usr/local/bin,bin,proc,sys,dev/pts,tmp,run,etc,root}
GOOS=linux GOARCH="$GUEST_ARCH" CGO_ENABLED=0 go build -o "$ROOT/init.krun" ./cmd/lohar
TD="$WORK/true-src"; mkdir -p "$TD"
printf 'package main\nfunc main(){}\n' > "$TD/main.go"
( cd "$TD" && [ -f go.mod ] || go mod init smoketrue >/dev/null 2>&1
  GOOS=linux GOARCH="$GUEST_ARCH" CGO_ENABLED=0 go build -o "$ROOT/bin/true" . )

# --- boot + probe ---
UDS="$WORK/vsock-1024.sock"; rm -f "$UDS"
cat > "$WORK/spec.json" <<EOF
{"rootfs_dir":"$ROOT","vcpus":1,"mem_mib":512,"pid1":true,"exec_path":"/init.krun","vsock_control_uds":"$UDS","log_level":2}
EOF

echo "==> boot bhatti-vmm"
DYLD_FALLBACK_LIBRARY_PATH="$LIBDIR" LD_LIBRARY_PATH="$LIBDIR" \
  ./bhatti-vmm "$WORK/spec.json" > "$WORK/vmm.log" 2>&1 &
VMM_PID=$!
cleanup() { kill "$VMM_PID" 2>/dev/null || true; }
trap cleanup EXIT

for _ in $(seq 1 50); do [ -S "$UDS" ] && break; sleep 0.1; done
sleep 1
if ! kill -0 "$VMM_PID" 2>/dev/null; then
  echo "ERROR: bhatti-vmm exited early; log:" >&2; tail -20 "$WORK/vmm.log" >&2; exit 1
fi

echo "==> probe agent"
if "$WORK/krucible-probe" "$UDS"; then
  echo ""
  echo "SMOKE PASS: lohar booted as PID 1 and the agent answered over vsock."
  echo "  (boot log: grep lohar $WORK/vmm.log)"
else
  echo "SMOKE FAIL — vmm log:" >&2; tail -20 "$WORK/vmm.log" >&2; exit 1
fi
