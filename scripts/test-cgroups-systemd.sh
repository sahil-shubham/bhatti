#!/usr/bin/env bash
# Runs bhatti's cgroup placement tests against a real systemd, in a throwaway
# privileged container with systemd as PID 1 and the host's cgroup namespace.
# Needs only Docker (Docker Desktop's Linux VM works: no KVM involved; the tests
# place stand-in helpers, not VMs).
#
#   scripts/test-cgroups-systemd.sh                     the whole krucible package + pkg/launchgate
#   scripts/test-cgroups-systemd.sh -test.run Systemd   only what matches, as `go test -run` would
#   KEEP=1 scripts/test-cgroups-systemd.sh              leave the container up for poking
#
# The container is where the destructive test runs: it stops D-Bus, which a real
# host must never have done to it.
set -euo pipefail
trap 'echo "test-cgroups-systemd.sh: failed at line $LINENO" >&2' ERR

cd "$(dirname "$0")/.."
NAME="${NAME:-bhatti-cgroups-test}"
IMAGE="${IMAGE:-bhatti-cgroups-test:trixie}"
GOARCH="$(docker version --format '{{.Server.Arch}}')"
WORK="$(mktemp -d)"

cleanup() {
    if [ "${KEEP:-0}" = 1 ]; then
        echo "container $NAME left running; test binaries in $WORK"
        return
    fi
    docker rm -f "$NAME" >/dev/null 2>&1 || true
    rm -rf "$WORK"
}
trap cleanup EXIT

echo "== image $IMAGE"
docker build -q -t "$IMAGE" - >/dev/null <<'DOCKERFILE'
FROM debian:trixie
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update -qq \
 && apt-get install -y -qq --no-install-recommends systemd systemd-sysv dbus dbus-daemon procps util-linux ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && systemctl mask getty.target console-getty.service systemd-logind.service systemd-timesyncd.service
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
DOCKERFILE

echo "== test binaries for linux/$GOARCH"
for pkg in pkg/engine/krucible pkg/launchgate; do
    GOOS=linux GOARCH="$GOARCH" CGO_ENABLED=0 go test -c -o "$WORK/$(basename "$pkg").test" "./$pkg"
done

echo "== container $NAME"
docker rm -f "$NAME" >/dev/null 2>&1 || true
docker run -d --name "$NAME" --privileged --cgroupns=host --tmpfs /run --tmpfs /run/lock -v "$WORK:/work:ro" "$IMAGE" >/dev/null
for _ in $(seq 1 60); do
    state="$(docker exec "$NAME" systemctl is-system-running 2>/dev/null || true)"
    case "$state" in running | degraded) break ;; esac
    sleep 1
done
echo "systemd: $(docker exec "$NAME" systemctl --version | head -1); system state: $state"

# What the placement has to turn on. Accounting flags alone don't enable the cpu
# controller on cgroup v2; bhatti.slice's CPUWeight does.
root='C=$(cut -d: -f3 /proc/1/cgroup | sed "s#/init.scope##"); cat /sys/fs/cgroup$C/cgroup.subtree_control'
echo "controllers enabled at systemd's root before: $(docker exec "$NAME" bash -c "$root")"

args=("$@")
[ ${#args[@]} -gt 0 ] || args=(-test.run .)
status=0
docker exec -e BHATTI_TEST_SYSTEMD=1 -e BHATTI_TEST_SYSTEMD_DESTRUCTIVE=1 "$NAME" \
    /work/krucible.test -test.v -test.timeout 600s "${args[@]}" || status=$?
if [ ${#args[@]} -eq 2 ] && [ "${args[1]}" = . ]; then
    docker exec "$NAME" /work/launchgate.test -test.v -test.timeout 120s || status=$?
fi
echo "controllers enabled at systemd's root after:  $(docker exec "$NAME" bash -c "$root")"
exit "$status"
