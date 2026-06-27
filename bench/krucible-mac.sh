#!/usr/bin/env bash
# macOS-native krucible benchmark. Stands up an ISOLATED block-root krucible
# daemon (cold-tier capable) over a real OCI rootfs and measures end-to-end CLI
# latencies — lifecycle, exec, files, cold-wake, and concurrent load. Unlike
# bench/run.sh it has no Linux deps (no timeout/flock/bash5): timing and
# percentiles use perl (Time::HiRes), present on stock macOS.
#
# Prereqs (build once): make krucible && make vmm && make build
#   plus a base image: ./krucible-mkimage alpine dist/krucible-base-alpine.img <lohar>
#
# Usage:
#   bench/krucible-mac.sh                 # default sample sizes
#   N_EXEC=50 N_LIFECYCLE=10 bench/krucible-mac.sh
#   BASE_IMAGE=dist/krucible-base-alpine.img bench/krucible-mac.sh
set -uo pipefail
cd "$(dirname "$0")/.."
REPO="$(pwd)"

PORT="${PORT:-8097}"
WORK="${KRUCIBLE_WORK:-/tmp/krucible-bench}"
BASE_IMAGE="${BASE_IMAGE:-$REPO/dist/krucible-base-alpine.img}"
RESULTS="${RESULTS_DIR:-$REPO/bench/results-krucible-mac}"
FORK_LIB="$REPO/libkrucible/_install/lib"

# Sample sizes. create is rate-limited to 30/min server-side, so N_LIFECYCLE is
# paced; exec/file are cheap (read bucket 1200/min).
N_EXEC="${N_EXEC:-30}"
N_FILE="${N_FILE:-20}"
N_LIFECYCLE="${N_LIFECYCLE:-10}"
N_COLD="${N_COLD:-10}"
CONCURRENCY_REPS="${CONCURRENCY_REPS:-15}"
SLEEP_PER_CREATE="${SLEEP_PER_CREATE:-2.1}"   # stay under 30 creates/min

# --- prereqs ---
for f in bhatti bhatti-vmm; do
  [ -x "$REPO/$f" ] || { echo "ERROR: $f not built — run: make build && make vmm"; exit 1; }
done
[ -f "$BASE_IMAGE" ] || { echo "ERROR: base image $BASE_IMAGE missing — build with ./krucible-mkimage"; exit 1; }
pkg-config --exists libkrun 2>/dev/null || { echo "ERROR: libkrun not installed"; exit 1; }

# --- timing + stats primitives (perl; no bash5/$EPOCHREALTIME needed) ---
# time_ms CMD... : run CMD (output discarded), print elapsed ms; exit = CMD's.
time_ms() {
  perl -MTime::HiRes=time -e '
    my $t0 = time;
    my $pid = fork();
    if ($pid == 0) { open(STDOUT, ">", "/dev/null"); open(STDERR, ">", "/dev/null"); exec @ARGV or exit 127; }
    waitpid($pid, 0);
    my $rc = $? >> 8;
    printf "%.1f\n", (time - $t0) * 1000;
    exit $rc;
  ' -- "$@"
}

# time_ms_in STDIN_FILE CMD... : like time_ms but feeds STDIN_FILE as the
# command's stdin (for `bhatti file write`, which reads content from stdin).
time_ms_in() {
  perl -MTime::HiRes=time -e '
    my $in = shift @ARGV;
    my $t0 = time;
    my $pid = fork();
    if ($pid == 0) {
      open(STDIN, "<", $in) or exit 126;
      open(STDOUT, ">", "/dev/null"); open(STDERR, ">", "/dev/null");
      exec @ARGV or exit 127;
    }
    waitpid($pid, 0); my $rc = $? >> 8;
    printf "%.1f\n", (time - $t0) * 1000; exit $rc;
  ' -- "$@"
}

# summarize FILE : print n / p50 / p95 / p99 / max / mean from a file of ms values.
summarize() {
  if [ ! -s "$1" ]; then echo "(no data)"; return; fi
  perl -e '
    my @a = sort { $a <=> $b } map { chomp; $_ } <>;
    my $n = @a; exit if !$n;
    my $sum = 0; $sum += $_ for @a;
    my $idx = sub { my $i = int($n * $_[0]); $i = $n-1 if $i >= $n; $a[$i] };
    printf "    n=%-3d  p50=%-7.1f p95=%-7.1f p99=%-7.1f max=%-7.1f mean=%-7.1f (ms)\n",
      $n, $idx->(0.50), $idx->(0.95), $idx->(0.99), $a[$n-1], $sum/$n;
  ' "$1"
}

# Fresh isolated state each run (new daemon DB + user); results dir is cleared
# but kept in-repo.
rm -rf "$WORK"
mkdir -p "$WORK/data" "$RESULTS"
rm -f "$RESULTS"/*.txt

# --- isolated daemon ---
CFG="$WORK/config.yaml"
cat > "$CFG" <<EOF
engine: krucible
listen: ":$PORT"
data_dir: $WORK/data
krucible_base_image: $BASE_IMAGE
krucible_block_root: true
krucible_vmm: $REPO/bhatti-vmm
krucible_libdir: $FORK_LIB:/opt/homebrew/lib
api_url: http://localhost:$PORT
EOF
export BHATTI_CONFIG="$CFG"

echo "==> minting bench user (high limits)"
KEY="$("$REPO/bhatti" user create --name bench --max-sandboxes 100 --max-cpus 8 --max-memory 8192 2>&1 | grep -oE 'bht_[A-Za-z0-9]+' | head -1)"
[ -n "$KEY" ] || { echo "ERROR: could not mint API key"; exit 1; }
export BHATTI_TOKEN="$KEY"

echo "==> starting isolated daemon on :$PORT (engine=krucible, block-root, $BASE_IMAGE)"
"$REPO/bhatti" serve > "$WORK/serve.log" 2>&1 &
SRV=$!
cleanup() {
  # destroy any leftover sandboxes, then stop the daemon
  for s in $("$REPO/bhatti" list 2>/dev/null | awk 'NR>1 {print $1}'); do
    "$REPO/bhatti" destroy "$s" --yes >/dev/null 2>&1 || true
  done
  kill "$SRV" 2>/dev/null || true
  sleep 0.5
  # backstop: kill any helper still bound to this run's data dir
  pkill -f "bhatti-vmm $WORK/data" 2>/dev/null || true
}
trap cleanup EXIT
for _ in $(seq 1 50); do curl -sf "http://localhost:$PORT/health" >/dev/null 2>&1 && break; sleep 0.2; done

bh() { "$REPO/bhatti" "$@"; }

echo
echo "===================================================================="
echo " krucible macOS bench — $(uname -m), $(sysctl -n hw.model 2>/dev/null)"
echo " image=$(basename "$BASE_IMAGE")  exec=$N_EXEC file=$N_FILE lifecycle=$N_LIFECYCLE cold=$N_COLD"
echo "===================================================================="

# ---- 1. create (full cold boot) ----
echo; echo "[1] create (cold boot, 1 vCPU / 512MB)"
f="$RESULTS/create.txt"
for i in $(seq 1 "$N_LIFECYCLE"); do
  (( i > 1 )) && sleep "$SLEEP_PER_CREATE"
  t=$(time_ms "$REPO/bhatti" create --name "b-create-$i" --cpus 1 --memory 512) && echo "$t" >> "$f"
done
summarize "$f"

# A persistent sandbox for the warm-path tests.
bh create --name bench-sb --cpus 1 --memory 512 >/dev/null 2>&1
bh exec bench-sb -- /bin/true >/dev/null 2>&1   # warm it

# ---- 2. exec (warm sandbox) ----
echo; echo "[2] exec (warm)"
for spec in "true:/bin/true" "echo:/bin/echo hi" "sha:sha256sum /etc/os-release"; do
  name="${spec%%:*}"; cmd="${spec#*:}"
  f="$RESULTS/exec_$name.txt"
  for i in $(seq 1 "$N_EXEC"); do
    t=$(time_ms "$REPO/bhatti" exec bench-sb -- $cmd) && echo "$t" >> "$f"
  done
  printf "  exec %-5s" "$name"; summarize "$f" | sed 's/^ *//'
done

# ---- 3. files ----
echo; echo "[3] file write/read"
for sz in 1024:1k 102400:100k 1048576:1m; do
  bytes="${sz%%:*}"; label="${sz#*:}"
  tmp="$WORK/payload-$label"; head -c "$bytes" /dev/urandom > "$tmp"
  fw="$RESULTS/file_write_$label.txt"; fr="$RESULTS/file_read_$label.txt"
  for i in $(seq 1 "$N_FILE"); do
    t=$(time_ms_in "$tmp" "$REPO/bhatti" file write bench-sb "/tmp/f-$label") && echo "$t" >> "$fw"
    t=$(time_ms "$REPO/bhatti" file read bench-sb "/tmp/f-$label") && echo "$t" >> "$fr"
  done
  printf "  write %-4s" "$label"; summarize "$fw" | sed 's/^ *//'
  printf "  read  %-4s" "$label"; summarize "$fr" | sed 's/^ *//'
done

# ---- 4. cold tier (stop / start / cold-wake-via-exec) ----
echo; echo "[4] cold tier (stop = snapshot+free RAM; start = restore)"
fs="$RESULTS/stop.txt"; fr="$RESULTS/cold_resume.txt"; fe="$RESULTS/cold_resume_exec.txt"
for i in $(seq 1 "$N_COLD"); do
  bh exec bench-sb -- /bin/true >/dev/null 2>&1   # ensure hot
  t=$(time_ms "$REPO/bhatti" stop bench-sb) && echo "$t" >> "$fs"
  t=$(time_ms "$REPO/bhatti" start bench-sb) && echo "$t" >> "$fr"
  bh stop bench-sb >/dev/null 2>&1
  # realistic cold wake: start + first exec (forces page-in + workload).
  # Both children run with output suppressed; only the ms total hits stdout.
  t=$(perl -MTime::HiRes=time -e '
    sub run { my $p=fork(); if($p==0){ open(STDOUT,">","/dev/null"); open(STDERR,">","/dev/null"); exec(@_) or exit 127 } waitpid($p,0); }
    my @a=@ARGV; my $t0=time; run(@a[0..2]); run(@a[3..$#a]); printf "%.1f\n",(time-$t0)*1000;
  ' -- "$REPO/bhatti" start bench-sb "$REPO/bhatti" exec bench-sb -- /bin/true) && echo "$t" >> "$fe"
done
printf "  stop          "; summarize "$fs" | sed 's/^ *//'
printf "  start (orch)  "; summarize "$fr" | sed 's/^ *//'
printf "  cold-wake+exec"; summarize "$fe" | sed 's/^ *//'

# ---- 5. concurrent exec (surfaces wake/lifecycle races under load) ----
echo; echo "[5] concurrent exec (wall-clock for N parallel execs to finish)"
bh exec bench-sb -- /bin/true >/dev/null 2>&1
for c in 5 10 20; do
  f="$RESULTS/concurrent_$c.txt"
  for r in $(seq 1 "$CONCURRENCY_REPS"); do
    t=$(perl -MTime::HiRes=time -e '
      my $t0=time; my @pids;
      for (1..$ARGV[0]) { my $p=fork(); if($p==0){ open(STDOUT,">","/dev/null"); open(STDERR,">","/dev/null"); exec(@ARGV[1..$#ARGV]); exit 127 } push @pids,$p; }
      waitpid($_,0) for @pids; printf "%.1f\n",(time-$t0)*1000;
    ' "$c" "$REPO/bhatti" exec bench-sb -- /bin/true) && echo "$t" >> "$f"
  done
  printf "  parallel x%-2d " "$c"; summarize "$f" | sed 's/^ *//'
done

# ---- 6. destroy ----
echo; echo "[6] destroy"
f="$RESULTS/destroy.txt"
for i in $(seq 1 "$N_LIFECYCLE"); do
  t=$(time_ms "$REPO/bhatti" destroy "b-create-$i" --yes) && echo "$t" >> "$f"
done
summarize "$f"

echo
echo "==> results in $RESULTS/  (raw ms, one per line). daemon log: $WORK/serve.log"
