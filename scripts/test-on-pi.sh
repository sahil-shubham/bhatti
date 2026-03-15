#!/usr/bin/env bash
#
# Run bhatti tests on Pi via SSH.
#
# Usage:
#   ./scripts/test-on-pi.sh                          # all agent + client tests
#   ./scripts/test-on-pi.sh agent                     # Part 2: agent tests only
#   ./scripts/test-on-pi.sh client                    # Part 3: client tests only
#   ./scripts/test-on-pi.sh agent TestAgentTTY        # single test
#   PI_HOST=user@10.0.0.5 ./scripts/test-on-pi.sh    # different Pi
#
set -euo pipefail

PI_HOST="${PI_HOST:-user@192.168.1.201}"
PI_DIR="/tmp/bhatti-test"

SUITE="${1:-all}"
TEST_FILTER="${2:-}"

echo "==> Building agent binary..."
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build \
    -ldflags='-s -w' \
    -o bin/bhatti-agent-linux-arm64 \
    ./cmd/bhatti-agent

run_agent_tests() {
    echo "==> Compiling agent test binary..."
    GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go test -c \
        -o bin/bhatti-agent-test-linux-arm64 ./cmd/bhatti-agent

    echo "==> Uploading to $PI_HOST..."
    ssh "$PI_HOST" "mkdir -p $PI_DIR"
    scp -q bin/bhatti-agent-test-linux-arm64 "$PI_HOST:$PI_DIR/"

    EXTRA=""
    if [[ -n "$TEST_FILTER" ]]; then EXTRA="-test.run=$TEST_FILTER"; fi

    echo "==> Running agent tests on Pi..."
    ssh "$PI_HOST" "cd $PI_DIR && ./bhatti-agent-test-linux-arm64 -test.v -test.timeout=60s $EXTRA"
}

run_client_tests() {
    echo "==> Compiling client test binary..."
    GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go test -c \
        -o bin/bhatti-client-test-linux-arm64 ./pkg/agent

    echo "==> Uploading to $PI_HOST..."
    ssh "$PI_HOST" "mkdir -p $PI_DIR"
    scp -q bin/bhatti-agent-linux-arm64 bin/bhatti-client-test-linux-arm64 "$PI_HOST:$PI_DIR/"

    EXTRA=""
    if [[ -n "$TEST_FILTER" ]]; then EXTRA="-test.run=$TEST_FILTER"; fi

    echo "==> Running client tests on Pi..."
    ssh "$PI_HOST" "cd $PI_DIR && BHATTI_AGENT_BIN=$PI_DIR/bhatti-agent-linux-arm64 ./bhatti-client-test-linux-arm64 -test.v -test.timeout=60s $EXTRA"
}

case "$SUITE" in
    agent)
        run_agent_tests
        ;;
    client)
        run_client_tests
        ;;
    all)
        run_agent_tests
        echo ""
        echo "========================================="
        echo ""
        TEST_FILTER=""  # reset for client tests
        run_client_tests
        ;;
    *)
        echo "Usage: $0 [agent|client|all] [TestFilter]"
        exit 1
        ;;
esac
