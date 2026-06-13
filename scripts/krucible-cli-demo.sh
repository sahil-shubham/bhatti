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

echo "==> build (bhatti daemon/CLI + vmm helper + base rootfs)"
go build -o bhatti ./cmd/bhatti/
make vmm >/dev/null
[ -x dist/krucible-rootfs/init.krun ] || ./scripts/krucible-rootfs.sh >/dev/null

# Isolated config — never read/written by the system bhatti.
cat > "$CFG" <<EOF
engine: krucible
listen: ":$PORT"
data_dir: $WORK/data
krucible_rootfs: $REPO/dist/krucible-rootfs
krucible_vmm: $REPO/bhatti-vmm
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

run() { echo; echo "\$ ./bhatti $*"; ./bhatti "$@"; }

run create --name s1 --cpus 1 --memory 512
run list
run exec s1 -- echo hello-from-cli
run exec s1 -- netcheck http
run inspect s1
run destroy s1 --yes

echo; echo "SMOKE PASS: created, exec'd, and destroyed a sandbox via the local CLI."
echo "  (daemon log: $WORK/serve.log)"
