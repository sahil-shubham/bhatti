#!/usr/bin/env bash
# End-to-end local CLI demo on the krucible engine — runs an ISOLATED daemon +
# CLI on this Mac without touching your system ~/.bhatti config (which points at
# the remote server). Everything lives under $KRUCIBLE_WORK + a throwaway config.
#
# Usage: scripts/krucible-cli-demo.sh
set -euo pipefail
cd "$(dirname "$0")/.."
REPO="$(pwd)"
WORK="${KRUCIBLE_WORK:-/tmp/krucible-cli}"
PORT="${PORT:-8099}"
CFG="$WORK/config.yaml"
rm -rf "$WORK"          # fresh isolated state each run
mkdir -p "$WORK/data"

if ! pkg-config --exists libkrun 2>/dev/null; then
  echo "ERROR: libkrun not installed (brew install libkrun)"; exit 1
fi

echo "==> build (libkrucible + bhatti daemon/CLI + vmm helper + base rootfs)"
make krucible >/dev/null
go build -o bhatti ./cmd/bhatti/
make vmm >/dev/null
[ -x dist/krucible-rootfs/init.krun ] || ./scripts/krucible-rootfs.sh >/dev/null

# libkrun comes from the libkrucible prefix; libkrunfw from Homebrew. The daemon
# passes krucible_libdir to bhatti-vmm as its dyld search path.
FORK_LIB="$REPO/libkrucible/_install/lib"

# Isolated config — never read/written by the system bhatti.
cat > "$CFG" <<EOF
engine: krucible
listen: ":$PORT"
data_dir: $WORK/data
krucible_rootfs: $REPO/dist/krucible-rootfs
krucible_vmm: $REPO/bhatti-vmm
krucible_libdir: $FORK_LIB:/opt/homebrew/lib
api_url: http://localhost:$PORT
EOF
export BHATTI_CONFIG="$CFG"

echo "==> create local user (direct store write)"
KEY="$(./bhatti user create --name dev 2>&1 | grep -oE 'bht_[A-Za-z0-9]+' | head -1)"
[ -n "$KEY" ] || { echo "ERROR: could not mint API key"; exit 1; }
export BHATTI_TOKEN="$KEY"
echo "    token: ${KEY:0:12}…"

echo "==> start daemon (./bhatti serve, engine=krucible) on :$PORT"
./bhatti serve > "$WORK/serve.log" 2>&1 &
SRV=$!
cleanup() { kill "$SRV" 2>/dev/null || true; }
trap cleanup EXIT
for _ in $(seq 1 50); do
  curl -sf "http://localhost:$PORT/health" >/dev/null 2>&1 && break
  sleep 0.2
done

# check "<expected substring>" ./bhatti <args...> — runs, prints, and asserts.
FAILED=0
check() {
  want="$1"; shift
  echo; echo "\$ $*"
  out="$("$@" 2>&1)" || true   # don't let set -e abort before we assert
  echo "$out"
  if printf '%s' "$out" | grep -qF -- "$want"; then echo "  [ok] matched: $want"; else echo "  [MISS] expected: $want"; FAILED=1; fi
}

check "created"        ./bhatti create --name s1 --cpus 1 --memory 512
check "s1"             ./bhatti list
check "hello-from-cli" ./bhatti exec s1 -- echo hello-from-cli
check "OK http 200"    ./bhatti exec s1 -- netcheck http   # real egress from the sandbox
check "running"        ./bhatti inspect s1
check "destroyed"      ./bhatti destroy s1 --yes

echo
if [ "$FAILED" = 0 ]; then
  echo "SMOKE PASS: create/list/exec/egress/inspect/destroy via the local CLI all verified."
  echo "  (this is a convenience e2e check; the primary gate is 'go test ./pkg/engine/krucible/')"
else
  echo "SMOKE FAIL — see output above and $WORK/serve.log"; exit 1
fi
