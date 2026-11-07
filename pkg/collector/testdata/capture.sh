#!/usr/bin/env bash
# Records real cgroup v2 and /proc accounting files, on a real systemd, as the
# collector's parser fixtures. Needs only Docker (Docker Desktop's Linux VM is
# fine; no KVM). Rebuilds a throwaway privileged container with systemd as
# PID 1 and the host's cgroup namespace, mimics bhatti's layout
#
#   bhatti.slice > bhatti-<owner>.slice > bhatti-<owner>-<id>.scope
#
# runs distinct workloads in distinct scopes, reads every file of every scope in
# one quick burst, and copies the result out untouched.
#
#   pkg/collector/testdata/capture.sh [target-dir]    default: testdata/linux next to this script
#   KEEP=1 pkg/collector/testdata/capture.sh          leave the container up for poking
#
# The container is removed on exit, also on failure. Nothing it records is
# edited by hand; the only generated prose is io-stat-devices.txt, derived from
# the recorded files.
set -euo pipefail
trap 'echo "capture.sh: failed at line $LINENO" >&2' ERR

HERE="$(cd "$(dirname "$0")" && pwd)"
DEST="${1:-$HERE/linux}"
NAME="${NAME:-bhatti-collector-fixtures}"
IMAGE="${IMAGE:-bhatti-collector-fixtures:trixie}"

cleanup() {
    if [ "${KEEP:-0}" = 1 ]; then
        echo "container $NAME left running"
        return
    fi
    docker rm -f "$NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "== image $IMAGE"
docker build -q -t "$IMAGE" - >/dev/null <<'DOCKERFILE'
FROM debian:trixie
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update -qq \
 && apt-get install -y -qq --no-install-recommends systemd systemd-sysv dbus dbus-daemon procps util-linux e2fsprogs ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && systemctl mask getty.target console-getty.service systemd-logind.service systemd-timesyncd.service
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
DOCKERFILE

echo "== container $NAME"
docker rm -f "$NAME" >/dev/null 2>&1 || true
docker run -d --name "$NAME" --privileged --cgroupns=host --tmpfs /run --tmpfs /run/lock "$IMAGE" >/dev/null
state=""
for _ in $(seq 1 60); do
    state="$(docker exec "$NAME" systemctl is-system-running 2>/dev/null || true)"
    case "$state" in running | degraded) break ;; esac
    sleep 1
done
case "$state" in running | degraded) ;; *) echo "systemd never came up (state: ${state:-none})" >&2; exit 1 ;; esac

echo "== capture (about a minute)"
# The script travels as an argument, not on stdin, so no workload can eat it.
read -r -d '' inner <<'INNER' || true
set -euo pipefail
export LC_ALL=C
OUT=/root/out
OWNER=a1b2c3d4e5f6
BUSY=0000000000000b01
IDLE=0000000000000a01
OOM=0000000000000c01
MULTI=0000000000000d01
ACCT=(-p CPUAccounting=yes -p MemoryAccounting=yes -p IOAccounting=yes)
rm -rf "$OUT" /root/w
mkdir -p "$OUT/cgroup" "$OUT/proc/pressure" /root/w
: >"$OUT/MISSING"

cleanup() {
    systemctl stop bhatti.slice >/dev/null 2>&1 || true
    umount /mnt/loop >/dev/null 2>&1 || true
    [ -z "${LOOP:-}" ] || losetup -d "$LOOP" >/dev/null 2>&1 || true
    rm -f /var/tmp/loop.img /var/tmp/busy.dat /var/tmp/md.dat
}
trap cleanup EXIT

# Workloads. Each scope also keeps a sleeping sentinel: a scope's cgroup
# vanishes the instant its last process exits.
cat >/root/w/busy.sh <<'EOF'
#!/bin/sh
# CPU burner, 96 MiB resident hog, and a direct-I/O write+read loop on the root
# filesystem; all bounded so nothing outlives the capture by much.
timeout 60 sh -c 'while :; do :; done' &
(dd if=/dev/zero bs=96M count=1 2>/dev/null | sleep 60) &
timeout 60 sh -c 'while :; do
  dd if=/dev/zero of=/var/tmp/busy.dat bs=1M count=64 oflag=direct 2>/dev/null
  dd if=/var/tmp/busy.dat of=/dev/null bs=1M iflag=direct 2>/dev/null
done' &
exec sleep 120
EOF
cat >/root/w/oom.sh <<'EOF'
#!/bin/sh
# MemoryMax=32M and no swap: the 64 MiB dd is the only thing big enough to be killed.
sleep 120 &
dd if=/dev/zero bs=64M count=1 2>/dev/null | cat >/dev/null || true
exec sleep 120
EOF
cat >/root/w/multidev.sh <<'EOF'
#!/bin/sh
# Touches every kind of block device this VM has: the root disk (direct write
# and read), a read-only raw read of every other whole disk, and a loop device
# whose backing file lives on the root disk.
dd if=/dev/zero of=/var/tmp/md.dat bs=1M count=64 oflag=direct 2>/dev/null
dd if=/var/tmp/md.dat of=/dev/null bs=1M iflag=direct 2>/dev/null
for d in /sys/block/*; do
    n=${d##*/}
    case $n in loop* | ram* | nbd* | zram* | dm-*) continue ;; esac
    dd if=/dev/"$n" of=/dev/null bs=1M count=16 iflag=direct 2>/dev/null || true
done
dd if=/dev/zero of=/mnt/loop/md.dat bs=1M count=32 oflag=direct 2>/dev/null
touch /root/w/multidev.done
exec sleep 120
EOF
chmod +x /root/w/*.sh

truncate -s 192M /var/tmp/loop.img
mkfs.ext4 -q -F /var/tmp/loop.img
LOOP="$(losetup -f --show /var/tmp/loop.img)"
mkdir -p /mnt/loop
mount "$LOOP" /mnt/loop

# bhatti's slice chain. A slice's parent comes from its dash-separated name, so
# neither gets Slice=; accounting is on, limits are not set. bhatti.slice's
# CPUWeight (the default value, 100) enables the cpu controller at the top; the
# netd scope's CPUWeight (below) enables it inside the owner slice, so every
# scope there has the full cpu.stat. Without those, cpu.stat is just the four
# usage lines.
for s in bhatti.slice "bhatti-$OWNER.slice"; do
    systemctl start "$s"
    systemctl set-property --runtime "$s" CPUAccounting=yes MemoryAccounting=yes IOAccounting=yes
done
systemctl set-property --runtime bhatti.slice CPUWeight=100

ROOT="/sys/fs/cgroup$(cut -d: -f3 /proc/1/cgroup | sed 's#/init.scope##')"
SLICE="$ROOT/bhatti.slice/bhatti-$OWNER.slice"

scope() { # id [extra systemd-run args] -- command...
    local id=$1
    shift
    systemd-run --scope --quiet --unit="bhatti-$OWNER-$id" --slice="bhatti-$OWNER.slice" "${ACCT[@]}" "$@" \
        </dev/null >/dev/null 2>&1 &
    for _ in $(seq 1 100); do
        grep -qs . "$SLICE/bhatti-$OWNER-$id.scope/cgroup.procs" && return 0
        sleep 0.1
    done
    echo "scope $id never appeared" >&2
    return 1
}
waitfor() { # seconds description command...
    local n=$(($1 * 10))
    shift
    local what=$1
    shift
    for _ in $(seq 1 "$n"); do
        "$@" && return 0
        sleep 0.1
    done
    echo "timed out waiting for $what" >&2
    return 1
}

scope netd -p CPUWeight=1000 sleep 120
scope "$IDLE" sleep 120
scope "$OOM" -p MemoryMax=32M -p MemorySwapMax=0 -p OOMPolicy=continue /root/w/oom.sh
waitfor 30 "oom_kill 1" grep -q '^oom_kill 1$' "$SLICE/bhatti-$OWNER-$OOM.scope/memory.events"
scope "$MULTI" /root/w/multidev.sh
waitfor 60 "multidev workload" test -e /root/w/multidev.done
scope "$BUSY" /root/w/busy.sh
sleep 6

FILES="cpu.stat memory.current memory.peak memory.stat memory.events memory.events.local io.stat cpu.pressure memory.pressure io.pressure pids.current cgroup.procs"
# One cat per file, bytes straight into the fixture. A file this kernel does not
# have, or refuses to read, is not written; its errno text goes to MISSING.
capture() { # name cgroup-dir
    local name=$1 cg=$2 dest="$OUT/cgroup/$1" f err
    mkdir -p "$dest"
    for f in $FILES; do
        if ! err=$(cat "$cg/$f" 2>&1 >"$dest/$f"); then
            rm -f "$dest/$f"
            echo "cgroup/$name/$f: ${err##*: }" >>"$OUT/MISSING"
        fi
    done
}
capture_proc() {
    local f err
    for f in stat meminfo loadavg pressure/cpu pressure/memory pressure/io; do
        if ! err=$(cat "/proc/$f" 2>&1 >"$OUT/proc/$f"); then
            rm -f "$OUT/proc/$f"
            echo "proc/$f: ${err##*: }" >>"$OUT/MISSING"
        fi
    done
}

# Burst 1: everything, busy first. The second busy sample is due 5 s after the
# first one's start, however long the rest of this burst took.
t1=$EPOCHREALTIME
capture scope-busy "$SLICE/bhatti-$OWNER-$BUSY.scope"
capture scope-idle "$SLICE/bhatti-$OWNER-$IDLE.scope"
capture scope-netd "$SLICE/bhatti-$OWNER-netd.scope"
capture scope-oom "$SLICE/bhatti-$OWNER-$OOM.scope"
capture scope-busy-multidev "$SLICE/bhatti-$OWNER-$MULTI.scope"
capture slice-owner "$SLICE"
capture_proc
echo "burst 1 took $((10#${EPOCHREALTIME/./} - 10#${t1/./})) us" >&2
due=$((10#${t1/./} + 5000000))
while [ $((10#${EPOCHREALTIME/./})) -lt $due ]; do sleep 0.05; done
t2=$EPOCHREALTIME
capture scope-busy-later "$SLICE/bhatti-$OWNER-$BUSY.scope"
# cpu.stat is the first file read, so these stamps are when each sample's
# counters were read, to within one cat.
us=$((10#${t2/./} - 10#${t1/./}))
printf '%d.%06d\n' $((us / 1000000)) $((us % 1000000)) >"$OUT/cgroup/scope-busy-later/ELAPSED"

# The VM's root cgroup lists devices that have no I/O yet as empty rows.
mkdir -p "$OUT/cgroup/root-host"
cp /sys/fs/cgroup/io.stat "$OUT/cgroup/root-host/io.stat"

{
    echo "kernel: $(uname -r)"
    echo "machine: $(uname -m)"
    echo "systemd: $(systemctl --version | sed -n 1p)"
    if [ -d /proc/pressure ]; then echo "proc_pressure: present"; else echo "proc_pressure: absent"; fi
    echo "cgroup_fs: $(stat -fc %T /sys/fs/cgroup)"
    echo "userland: $(sed -n 's/^PRETTY_NAME=//p' /etc/os-release)"
} >"$OUT/KERNEL"

for d in /sys/dev/block/*; do
    echo "$d -> $(readlink -f "$d")  slaves=[$(ls "$d/slaves" 2>/dev/null | tr '\n' ' ')] holders=[$(ls "$d/holders" 2>/dev/null | tr '\n' ' ')]"
done >"$OUT/block-topology.txt"

# Generated from the files above: what each io.stat row is.
describe() { # io.stat file
    local row mm topo slaves holders backing kind
    while read -r row; do
        mm=${row%% *}
        topo=$(grep "^/sys/dev/block/$mm -> " "$OUT/block-topology.txt" || true)
        slaves=$(sed -n 's/.*slaves=\[\(.*\)\] holders=.*/\1/p' <<<"$topo")
        holders=$(sed -n 's/.*holders=\[\(.*\)\].*/\1/p' <<<"$topo")
        kind="whole disk"
        [ ! -e "/sys/dev/block/$mm/partition" ] || kind="partition"
        backing=""
        if [ -e "/sys/dev/block/$mm/loop/backing_file" ]; then
            kind="loop device"
            backing="  backing_file=$(cat "/sys/dev/block/$mm/loop/backing_file") (a file on this container's root filesystem)"
        fi
        echo "$row"
        echo "    -> ${topo#* -> }"
        echo "    kind=$kind${backing}"
        if [ -n "${slaves// /}" ]; then
            echo "    STACKED: sits on [${slaves% }]; its I/O is also counted on those rows (double count if summed)"
        elif [ -n "${holders// /}" ]; then
            echo "    HOLDERS: [${holders% }] sit on top of this device and are counted on their own rows"
        else
            echo "    not stacked (no slaves, no holders)"
        fi
    done <"$1"
}
{
    echo "scope-busy/io.stat rows (root-filesystem direct I/O only):"
    describe "$OUT/cgroup/scope-busy/io.stat"
    echo
    echo "scope-busy-multidev/io.stat rows (root disk + every other whole disk read + a loop device on the root disk):"
    describe "$OUT/cgroup/scope-busy-multidev/io.stat"
    echo
    echo "block devices in this VM that can do I/O: $(for d in /sys/block/*; do n=${d##*/}; case $n in loop* | ram* | nbd* | zram*) ;; *) printf '%s ' "$n" ;; esac; done)"
    echo "root-host/io.stat (the VM's root cgroup) rows: $(wc -l <"$OUT/cgroup/root-host/io.stat"); rows with no counters at all: $(grep -c '^[0-9]*:[0-9]* *$' "$OUT/cgroup/root-host/io.stat" || true)"
    # multidev.sh wrote 64 MiB straight to the root disk and 32 MiB to the loop
    # device. Whole-disk rows summing to 96 MiB would mean the loop's backing
    # writes were charged to the scope a second time.
    awk '
        { split($1, id, ":"); for (i = 2; i <= NF; i++) if ($i ~ /^wbytes=/) w = substr($i, 8) + 0 }
        id[1] == 7 { loop += w; next }
        { disk += w }
        END {
            printf "scope-busy-multidev wbytes: loop rows %d, other rows %d (the workload wrote 33554432 to loop, 67108864 direct to the root disk)\n", loop, disk
            if (disk >= 67108864 + loop && loop > 0) print "loop writes ARE charged again to the disk row: summing rows double counts"
            else print "loop writes are NOT charged again to the disk row: summing rows does not double count here"
        }' "$OUT/cgroup/scope-busy-multidev/io.stat"
} >"$OUT/io-stat-devices.txt"
INNER
docker exec "$NAME" bash -c "$inner"

echo "== copy out to $DEST"
mkdir -p "$DEST"
rm -rf "$DEST/cgroup" "$DEST/proc" "$DEST/MISSING" "$DEST/KERNEL" "$DEST/block-topology.txt" "$DEST/io-stat-devices.txt"
docker cp "$NAME:/root/out/." "$DEST"
echo "== recorded:"
cat "$DEST/KERNEL"
echo "-- MISSING:"
cat "$DEST/MISSING"
