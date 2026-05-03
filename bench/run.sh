#!/usr/bin/env bash
# =============================================================================
# Bhatti performance benchmarks.
#
# Measures real end-to-end latencies against a live bhatti daemon. Designed
# to run on the same host as the daemon (loopback) so results don't include
# geographic network latency.
#
# Sections:
#   lifecycle  — create / stop / start / cold-resume-via-exec / destroy
#   warm       — warm-resume + exec (35s sleep per sample, opt-in)
#   exec       — exec ops (true, echo, cat, ls, sha256, env)
#   files      — file read/write/ls at multiple sizes
#   api        — list / inspect / curl /health / curl /sandboxes
#   concurrent — 5/10/20 parallel exec + 30 sequential
#   network    — TCP connect / TTFB to the API
#
# Usage:
#   ./run.sh [iterations]                # default iterations: 20
#   SECTIONS=exec,files ./run.sh 30      # subset
#   TIMEOUT_PER_CALL=10s ./run.sh        # per-bhatti-call timeout
#   RESULTS_DIR=/tmp/r ./run.sh          # override output directory
#
# Robustness:
#   - Single-instance lock (flock /tmp/bhatti-bench.lock).
#   - Per-call `timeout` so a hung bhatti invocation doesn't block the suite.
#   - All created sandboxes tracked and destroyed on EXIT (success, failure,
#     SIGINT, SIGTERM).
#   - Unique sandbox names per run (no collisions across simultaneous or
#     leftover runs).
#   - Failed iterations logged with n=X (skipped: Y) instead of recording
#     bogus near-zero values.
# =============================================================================

set -uo pipefail   # NOT -e: a failing iteration shouldn't kill the suite.

# ── Config ────────────────────────────────────────────────────────────────────

# Default sample sizes are sized so p99 is statistically meaningful where
# the cost permits, and as large as we can afford where it doesn't. Tune
# down via env vars for quick smoke-tests during development.
#
#   ITERATIONS              — cheap-per-call tests (exec/file/api/network).
#                             n=100 means p99 = index 99/100, a real
#                             99th-percentile estimate, not just "max - 1".
#                             Cost: ~12s per sub-test.
#   LIFECYCLE_N             — lifecycle ops (create/stop/start/destroy) where
#                             each iteration is 1–3s and creates hit the
#                             30/min rate limit. n=15 fits in ~4 min while
#                             tightening p50 vs the previous n=5.
#   WARM_N                  — warm-resume tests, bound by 35s sleep per
#                             sample. n=10 = ~6 min of sleep per row;
#                             not cheap, but n=3 had outliers dominating.
#   PUBLISH_WAKE_COLD_N     — publish-wake-cold via curl. ~1.5s per sample
#                             (stop + curl), so n=100 fits in ~2.5 min.
#   CONCURRENT_REPS         — number of repeated wall-clock measurements
#                             at each parallelism level (5/10/20).
ITERATIONS="${1:-100}"
LIFECYCLE_N="${LIFECYCLE_N:-15}"
WARM_N="${WARM_N:-10}"
PUBLISH_WAKE_COLD_N="${PUBLISH_WAKE_COLD_N:-100}"
CONCURRENT_REPS="${CONCURRENT_REPS:-30}"
TIMEOUT_PER_CALL="${TIMEOUT_PER_CALL:-30s}"
SECTIONS="${SECTIONS:-lifecycle,exec,files,api,concurrent,network,publish-wake}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RESULTS_DIR="${RESULTS_DIR:-$SCRIPT_DIR/results}"

# Per-user rate limits in pkg/server/ratelimit.go (defaults):
#   create: 30/min, burst 10
#   exec/file: 600/min (10/sec), burst 30
#   read (list/inspect/ports): 1200/min (20/sec), burst 60
# Each iteration of an exec/file test consumes one token, so bursts deplete
# in <1s of bench wall-clock. We pace iterations to stay under sustained
# limits with a small safety margin. Set to 0 to disable (only safe if
# you've raised the daemon's rate limits).
SLEEP_PER_EXEC="${SLEEP_PER_EXEC:-0.11}"  # 1/10/sec + 10% margin
SLEEP_PER_READ="${SLEEP_PER_READ:-0.06}"  # 1/20/sec + 20% margin
SLEEP_PER_CREATE="${SLEEP_PER_CREATE:-2.1}"  # 1/(30/60s) + margin
SLEEP_PER_CONCURRENT="${SLEEP_PER_CONCURRENT:-3}"  # rep gap, lets bucket refill

# Run-scoped sandbox names so concurrent runs don't collide and leftover
# sandboxes from a crashed prior run don't shadow this one.
RUN_ID="bench-$$-$(date +%s)"
SB="${RUN_ID}-main"
LIFECYCLE_SB="${RUN_ID}-lifecycle"

CREATED_SANDBOXES=()

# ── Output ────────────────────────────────────────────────────────────────────

if [[ -t 1 ]]; then
    RED=$'\033[0;31m'; GREEN=$'\033[0;32m'; YELLOW=$'\033[1;33m'
    CYAN=$'\033[0;36m'; BOLD=$'\033[1m'; NC=$'\033[0m'
else
    RED=''; GREEN=''; YELLOW=''; CYAN=''; BOLD=''; NC=''
fi

header()  { echo "${CYAN}${BOLD}━━━ $* ━━━${NC}"; }
subhead() { echo "  ${YELLOW}$*${NC}"; }
warn()    { echo "${YELLOW}WARN:${NC} $*" >&2; }
die()     { echo "${RED}ERROR:${NC} $*" >&2; exit 1; }

# ── Single-instance lock ──────────────────────────────────────────────────────

LOCK="/tmp/bhatti-bench.lock"
exec 200>"$LOCK" || die "cannot open lock file $LOCK"
flock -n 200 || die "another bench is already running (lock: $LOCK)"

# ── Cleanup trap ──────────────────────────────────────────────────────────────

cleanup() {
    local exit_code=$?
    trap '' INT TERM   # ignore signals during cleanup
    echo
    if (( ${#CREATED_SANDBOXES[@]} > 0 )); then
        echo "Cleaning up ${#CREATED_SANDBOXES[@]} sandbox(es)..."
        local s
        for s in "${CREATED_SANDBOXES[@]}"; do
            bhatti destroy "$s" -y >/dev/null 2>&1 &
        done
        wait
    fi
    if (( exit_code != 0 )); then
        echo "${RED}bench exited with status $exit_code${NC}" >&2
    else
        echo "${GREEN}bench complete${NC}"
    fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# ── Sandbox helpers ───────────────────────────────────────────────────────────

create_sandbox() {
    local name="$1"; shift
    if ! timeout 30s bhatti create --name "$name" "$@" >/dev/null 2>&1; then
        die "create $name failed"
    fi
    CREATED_SANDBOXES+=("$name")
}

# ── Pre-flight checks ─────────────────────────────────────────────────────────

command -v bhatti >/dev/null 2>&1 || die "bhatti not in PATH"
command -v flock  >/dev/null 2>&1 || die "flock not installed (required for single-instance lock)"
command -v timeout >/dev/null 2>&1 || die "timeout not installed"
command -v awk    >/dev/null 2>&1 || die "awk not installed"
command -v curl   >/dev/null 2>&1 || die "curl not installed"

# Bash 5+ for $EPOCHREALTIME (microsecond precision, zero fork).
if (( BASH_VERSINFO[0] < 5 )); then
    die "bash 5+ required (need \$EPOCHREALTIME); current: $BASH_VERSION"
fi

# Daemon reachable?
if ! timeout 5s bhatti list >/dev/null 2>&1; then
    die "cannot reach bhatti daemon (run 'bhatti list' to debug)"
fi

mkdir -p "$RESULTS_DIR"

# ── Timing primitive ──────────────────────────────────────────────────────────

# time_ms: run a command, print elapsed ms to stdout. On failure or timeout,
# return non-zero so the caller can skip the iteration.
#
# Uses bash 5's $EPOCHREALTIME (microseconds since epoch as "secs.usecs"). We
# strip the dot and treat the value as integer microseconds to avoid awk/python
# forks per measurement. Earlier versions of this script forked python3 three
# times per call, adding 150-300ms of overhead and making sub-100ms operations
# unmeasurable.
#
# Set BENCH_DEBUG=1 to log stderr from failed iterations to /tmp/bhatti-bench-fails.log.
time_ms() {
    local start_us="${EPOCHREALTIME//.}"
    local err
    if [[ -n "${BENCH_DEBUG:-}" ]]; then
        err=$(timeout "$TIMEOUT_PER_CALL" "$@" 2>&1 >/dev/null)
        local rc=$?
        if (( rc != 0 )); then
            echo "[$(date +%H:%M:%S)] rc=$rc cmd: $* | err: $err" >> /tmp/bhatti-bench-fails.log
            return 1
        fi
    else
        if ! timeout "$TIMEOUT_PER_CALL" "$@" >/dev/null 2>&1; then
            return 1
        fi
    fi
    local end_us="${EPOCHREALTIME//.}"
    local diff=$((end_us - start_us))
    printf '%d.%03d\n' $((diff / 1000)) $((diff % 1000))
}

# Run a timed iteration N times. Failures are counted, not crashes.
# Args: <results-file> <n> <inter-iteration-sleep> <command...>
collect() {
    local file="$1" n="$2" sleep_s="$3"; shift 3
    local fails=0 t i
    : > "$file"
    for ((i = 1; i <= n; i++)); do
        (( i > 1 )) && sleep "$sleep_s"
        if t=$(time_ms "$@"); then
            echo "$t" >> "$file"
        else
            ((fails++))
        fi
    done
    if (( fails > 0 )); then
        warn "$(basename "$file"): $fails/$n iteration(s) failed or timed out"
    fi
}

percentiles() {
    local file="$1"
    if [[ ! -s "$file" ]]; then
        echo "  (no data)"
        return
    fi
    sort -n "$file" | awk '
    {a[NR]=$1; sum+=$1}
    END {
        n = NR
        printf "  n=%-3d  min=%8.1f  p50=%8.1f  p95=%8.1f  p99=%8.1f  max=%8.1f  mean=%8.1f ms\n",
            n, a[1],
            a[int(n*0.50)+1],
            a[int(n*0.95)+1],
            a[int(n*0.99)+1],
            a[n],
            sum/n
    }'
}

want_section() {
    [[ ",$SECTIONS," == *",$1,"* ]]
}

elapsed_since() {
    local start_us="$1"
    local now_us="${EPOCHREALTIME//.}"
    local diff=$((now_us - start_us))
    awk -v d="$diff" 'BEGIN { printf "%.1fs", d / 1000000 }'
}

# ── Banner ────────────────────────────────────────────────────────────────────

START_US="${EPOCHREALTIME//.}"

echo "${BOLD}Bhatti performance benchmark${NC}"
echo "  Target:        $(bhatti version 2>&1 | head -1)"
echo "  Sections:      $SECTIONS"
echo "  Sample sizes:  exec/file/api/network/publish-wake-hot=$ITERATIONS"
echo "                 lifecycle=$LIFECYCLE_N  warm=$WARM_N (35s sleep each)"
echo "                 publish-wake-cold=$PUBLISH_WAKE_COLD_N  concurrent=$CONCURRENT_REPS reps"
echo "  Timeout/call:  $TIMEOUT_PER_CALL"
echo "  Results dir:   $RESULTS_DIR"
echo "  Run ID:        $RUN_ID"
echo "  Timestamp:     $(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo

# =============================================================================
# 1. LIFECYCLE
# =============================================================================

run_lifecycle() {
    header "LIFECYCLE"

    # 1a. Create (cold boot)
    subhead "1a. Create sandbox (full cold boot, 1 vCPU / 512MB)"
    local file="$RESULTS_DIR/create.txt"
    : > "$file"
    local i name t
    for ((i = 1; i <= LIFECYCLE_N; i++)); do
        (( i > 1 )) && sleep "$SLEEP_PER_CREATE"
        name="$RUN_ID-create-$i"
        if t=$(time_ms bhatti create --name "$name" --cpus 1 --memory 512); then
            echo "$t" >> "$file"
            CREATED_SANDBOXES+=("$name")
            echo "    run $i: ${t}ms"
        else
            warn "create $name failed"
        fi
    done
    echo "${YELLOW}Create (cold boot, ms):${NC}"
    percentiles "$file"

    # Reuse one of the created sandboxes for the rest of the lifecycle tests.
    # Renaming via stop+start cycle keeps state warm.
    create_sandbox "$LIFECYCLE_SB" --cpus 1 --memory 512
    timeout 10s bhatti exec "$LIFECYCLE_SB" -- true >/dev/null 2>&1

    # 1b. Stop (snapshot)
    subhead "1b. Stop sandbox (snapshot to disk)"
    file="$RESULTS_DIR/stop.txt"
    : > "$file"
    for ((i = 1; i <= LIFECYCLE_N; i++)); do
        timeout 10s bhatti start "$LIFECYCLE_SB" >/dev/null 2>&1
        timeout 10s bhatti exec  "$LIFECYCLE_SB" -- true >/dev/null 2>&1
        sleep 1
        if t=$(time_ms bhatti stop "$LIFECYCLE_SB"); then
            echo "$t" >> "$file"
            echo "    run $i: ${t}ms"
        else
            warn "stop iteration $i failed"
        fi
    done
    echo "${YELLOW}Stop / snapshot (ms):${NC}"
    percentiles "$file"

    # 1c. Cold resume (start)
    subhead "1c. Start from cold (snapshot resume)"
    file="$RESULTS_DIR/cold_resume.txt"
    : > "$file"
    for ((i = 1; i <= LIFECYCLE_N; i++)); do
        timeout 10s bhatti stop "$LIFECYCLE_SB" >/dev/null 2>&1
        sleep 0.5
        if t=$(time_ms bhatti start "$LIFECYCLE_SB"); then
            echo "$t" >> "$file"
            echo "    run $i: ${t}ms"
        else
            warn "start iteration $i failed"
        fi
    done
    echo "${YELLOW}Cold resume (ms):${NC}"
    percentiles "$file"

    # 1d. Cold resume + exec (transparent wake on exec)
    subhead "1d. Cold resume + exec (transparent wake)"
    file="$RESULTS_DIR/cold_resume_exec.txt"
    : > "$file"
    for ((i = 1; i <= LIFECYCLE_N; i++)); do
        timeout 10s bhatti stop "$LIFECYCLE_SB" >/dev/null 2>&1
        sleep 0.5
        if t=$(time_ms bhatti exec "$LIFECYCLE_SB" -- true); then
            echo "$t" >> "$file"
            echo "    run $i: ${t}ms"
        else
            warn "cold-resume-exec iteration $i failed"
        fi
    done
    echo "${YELLOW}Cold resume + exec (ms):${NC}"
    percentiles "$file"

    # 1e. Destroy
    subhead "1e. Destroy sandbox"
    file="$RESULTS_DIR/destroy.txt"
    : > "$file"
    for ((i = 1; i <= LIFECYCLE_N; i++)); do
        (( i > 1 )) && sleep "$SLEEP_PER_CREATE"
        name="$RUN_ID-destroy-$i"
        timeout 30s bhatti create --name "$name" --cpus 1 --memory 512 >/dev/null 2>&1 || {
            warn "destroy iteration $i: create failed"; continue
        }
        CREATED_SANDBOXES+=("$name")
        if t=$(time_ms bhatti destroy "$name" -y); then
            echo "$t" >> "$file"
            echo "    run $i: ${t}ms"
            # Already destroyed; remove from cleanup list.
            CREATED_SANDBOXES=("${CREATED_SANDBOXES[@]/$name}")
        else
            warn "destroy iteration $i failed"
        fi
    done
    echo "${YELLOW}Destroy (ms):${NC}"
    percentiles "$file"
}

# =============================================================================
# WARM RESUME — opt-in (35s sleep per sample)
# =============================================================================

run_warm() {
    header "WARM RESUME (slow — $((WARM_N * 35))s of waits)"

    # Make sure we have a hot sandbox.
    create_sandbox "$RUN_ID-warm" --cpus 1 --memory 512
    timeout 10s bhatti exec "$RUN_ID-warm" -- true >/dev/null 2>&1

    subhead "Warm resume + exec ($WARM_N samples)"
    local file="$RESULTS_DIR/warm_resume_exec.txt"
    : > "$file"
    local i t
    for ((i = 1; i <= WARM_N; i++)); do
        echo "    sample $i: waiting 35s for thermal manager to pause sandbox..."
        sleep 35
        if t=$(time_ms bhatti exec "$RUN_ID-warm" -- true); then
            echo "$t" >> "$file"
            echo "    sample $i: ${t}ms (warm→hot + exec)"
        else
            warn "warm-resume sample $i failed"
        fi
    done
    echo "${YELLOW}Warm resume + exec (ms):${NC}"
    percentiles "$file"
}

# =============================================================================
# 2. EXEC OPS (hot sandbox)
# =============================================================================

ensure_main_sandbox() {
    if ! timeout 5s bhatti inspect "$SB" >/dev/null 2>&1; then
        create_sandbox "$SB" --cpus 2 --memory 2048
    fi
    timeout 10s bhatti exec "$SB" -- true >/dev/null 2>&1
}

# Helper: warmup + collect for an exec/file-style test (rate-limited at 600/min).
# Args: <name-tag> <result-file> <iterations> <command...>
exec_test() {
    local label="$1" file="$2" n="$3"; shift 3
    subhead "$label"
    # 3 warmup calls (also rate-limited; pace them)
    local w
    for w in 1 2 3; do
        timeout "$TIMEOUT_PER_CALL" "$@" >/dev/null 2>&1 || true
        sleep "$SLEEP_PER_EXEC"
    done
    collect "$file" "$n" "$SLEEP_PER_EXEC" "$@"
    echo "${YELLOW}$(basename "$file" .txt) (ms):${NC}"
    percentiles "$file"
}

# Helper: warmup + collect for a read-style test (rate-limited at 1200/min).
# Args: <name-tag> <result-file> <iterations> <command...>
read_test() {
    local label="$1" file="$2" n="$3"; shift 3
    subhead "$label"
    local w
    for w in 1 2 3; do
        timeout "$TIMEOUT_PER_CALL" "$@" >/dev/null 2>&1 || true
        sleep "$SLEEP_PER_READ"
    done
    collect "$file" "$n" "$SLEEP_PER_READ" "$@"
    echo "${YELLOW}$(basename "$file" .txt) (ms):${NC}"
    percentiles "$file"
}

run_exec() {
    header "EXEC OPS (hot sandbox)"
    ensure_main_sandbox

    exec_test "2a. Exec 'true' (no output)"          "$RESULTS_DIR/exec_true.txt"  "$ITERATIONS" bhatti exec "$SB" -- true
    exec_test "2b. Exec 'echo hello' (tiny output)"  "$RESULTS_DIR/exec_echo.txt"  "$ITERATIONS" bhatti exec "$SB" -- echo hello
    exec_test "2c. Exec 'cat /etc/os-release'"       "$RESULTS_DIR/exec_cat.txt"   "$ITERATIONS" bhatti exec "$SB" -- cat /etc/os-release
    exec_test "2d. Exec 'ls -laR /usr/bin' (~50KB)"  "$RESULTS_DIR/exec_ls.txt"    "$ITERATIONS" bhatti exec "$SB" -- ls -laR /usr/bin
    exec_test "2e. Exec 'sha256sum /usr/bin/bash'"   "$RESULTS_DIR/exec_sha.txt"   "$ITERATIONS" bhatti exec "$SB" -- sha256sum /usr/bin/bash
    exec_test "2f. Exec 'sh -c echo \$HOME'"         "$RESULTS_DIR/exec_env.txt"   "$ITERATIONS" bhatti exec "$SB" -- sh -c 'echo $HOME'
}

# =============================================================================
# 3. FILE OPS
# =============================================================================

run_files() {
    header "FILE OPS"
    ensure_main_sandbox

    # Prep test files inside the guest (idempotent).
    local sizes_kb=(1 10 100 1024)
    local s
    for s in "${sizes_kb[@]}"; do
        timeout 30s bhatti exec "$SB" -- sh -c "head -c $((s*1024)) /dev/urandom | base64 > /tmp/bench${s}k.txt" >/dev/null 2>&1
    done

    # Prep local write payloads in /tmp.
    local local_writes=(1 10 100)
    for s in "${local_writes[@]}"; do
        head -c $((s*1024)) /dev/urandom | base64 > "/tmp/${RUN_ID}.w${s}k.txt"
    done

    for s in 1 10 100 1024; do
        local label="${s}KB"
        [[ $s -eq 1024 ]] && label="1MB"
        local out="$RESULTS_DIR/file_read_${s}k.txt"
        [[ $s -eq 1024 ]] && out="$RESULTS_DIR/file_read_1m.txt"
        # File read goes through /sandboxes/:id/files which is the exec
        # rate-limit class (writes data through the engine).
        exec_test "3. File read $label" "$out" "$ITERATIONS" \
            bhatti file read "$SB" "/tmp/bench${s}k.txt"
    done

    for s in "${local_writes[@]}"; do
        local label="${s}KB"
        subhead "3. File write $label"
        local out="$RESULTS_DIR/file_write_${s}k.txt"
        local fails=0 t i
        : > "$out"
        # 3 warmup calls (rate-limited)
        for w in 1 2 3; do
            timeout "$TIMEOUT_PER_CALL" sh -c "bhatti file write $SB /tmp/benchw${s}k.txt < /tmp/${RUN_ID}.w${s}k.txt" >/dev/null 2>&1 || true
            sleep "$SLEEP_PER_EXEC"
        done
        for ((i = 1; i <= ITERATIONS; i++)); do
            (( i > 1 )) && sleep "$SLEEP_PER_EXEC"
            local start_us="${EPOCHREALTIME//.}"
            if timeout "$TIMEOUT_PER_CALL" sh -c "bhatti file write $SB /tmp/benchw${s}k.txt < /tmp/${RUN_ID}.w${s}k.txt" >/dev/null 2>&1; then
                local end_us="${EPOCHREALTIME//.}"
                local diff=$((end_us - start_us))
                printf '%d.%03d\n' $((diff / 1000)) $((diff % 1000)) >> "$out"
            else
                ((fails++))
            fi
        done
        (( fails > 0 )) && warn "file write ${s}k: $fails/$ITERATIONS failed"
        echo "${YELLOW}File write $label (ms):${NC}"
        percentiles "$out"
    done

    # `file ls` is a read op (no engine I/O — just a dir listing).
    read_test "3. File ls /usr/bin" "$RESULTS_DIR/file_ls.txt" "$ITERATIONS" \
        bhatti file ls "$SB" /usr/bin

    # Cleanup local payloads
    rm -f "/tmp/${RUN_ID}.w"*.txt
}

# =============================================================================
# 4. API
# =============================================================================

run_api() {
    header "API / CONTROL PLANE"
    ensure_main_sandbox

    # `list` and `inspect` are read-class endpoints (1200/min limit).
    read_test "4a. List sandboxes"   "$RESULTS_DIR/api_list.txt"     "$ITERATIONS" bhatti list --json
    read_test "4b. Inspect sandbox"  "$RESULTS_DIR/api_inspect.txt"  "$ITERATIONS" bhatti inspect "$SB" --json

    # curl tests
    local api_url="${BHATTI_URL:-$(grep -h '^api_url:' ~/.bhatti/config.yaml /etc/bhatti/config.yaml 2>/dev/null | head -1 | awk '{print $2}')}"
    api_url="${api_url:-http://localhost:8080}"
    local token="${BHATTI_TOKEN:-$(grep -h '^auth_token:' ~/.bhatti/config.yaml 2>/dev/null | awk '{print $2}')}"

    subhead "4c. GET /health (curl, no auth)"
    local out="$RESULTS_DIR/api_health.txt"
    : > "$out"
    local i t
    for ((i = 1; i <= ITERATIONS; i++)); do
        t=$(timeout "$TIMEOUT_PER_CALL" curl -sf -o /dev/null -w '%{time_total}' "$api_url/health" 2>/dev/null) || continue
        awk -v s="$t" 'BEGIN { printf "%.3f\n", s * 1000 }' >> "$out"
    done
    echo "${YELLOW}GET /health (ms):${NC}"
    percentiles "$out"

    if [[ -n "$token" ]]; then
        subhead "4d. GET /sandboxes (curl, with auth)"
        out="$RESULTS_DIR/api_sandboxes_curl.txt"
        : > "$out"
        for ((i = 1; i <= ITERATIONS; i++)); do
            t=$(timeout "$TIMEOUT_PER_CALL" curl -sf -o /dev/null -w '%{time_total}' \
                -H "Authorization: Bearer $token" "$api_url/sandboxes" 2>/dev/null) || continue
            awk -v s="$t" 'BEGIN { printf "%.3f\n", s * 1000 }' >> "$out"
        done
        echo "${YELLOW}GET /sandboxes (curl, ms):${NC}"
        percentiles "$out"
    else
        warn "no auth token in ~/.bhatti/config.yaml or BHATTI_TOKEN — skipping 4d"
    fi
}

# =============================================================================
# 5. CONCURRENCY
# =============================================================================

# Time N parallel execs and report wall-clock.
concurrent_run() {
    local count="$1" reps="$2" out="$3"
    : > "$out"
    local r i fails=0
    for ((r = 1; r <= reps; r++)); do
        # Pace reps so the rate-limit bucket has time to refill between bursts.
        # With burst=30 and 10/sec refill, 3s between reps gives back ~30 tokens.
        (( r > 1 )) && sleep "$SLEEP_PER_CONCURRENT"
        local start_us="${EPOCHREALTIME//.}"
        local pids=()
        for ((i = 1; i <= count; i++)); do
            ( timeout "$TIMEOUT_PER_CALL" bhatti exec "$SB" -- true >/dev/null 2>&1 ) &
            pids+=("$!")
        done
        local rep_ok=1
        for p in "${pids[@]}"; do
            wait "$p" || rep_ok=0
        done
        if (( rep_ok )); then
            local end_us="${EPOCHREALTIME//.}"
            local diff=$((end_us - start_us))
            printf '%d.%03d\n' $((diff / 1000)) $((diff % 1000)) >> "$out"
        else
            ((fails++))
        fi
    done
    (( fails > 0 )) && warn "$(basename "$out"): $fails/$reps reps had a failed exec"
}

run_concurrent() {
    header "CONCURRENCY"
    ensure_main_sandbox

    local n
    for n in 5 10 20; do
        subhead "5. $n concurrent execs ($CONCURRENT_REPS reps)"
        concurrent_run "$n" "$CONCURRENT_REPS" "$RESULTS_DIR/concurrent_${n}.txt"
        echo "${YELLOW}$n concurrent execs, wall time (ms):${NC}"
        percentiles "$RESULTS_DIR/concurrent_${n}.txt"
    done

    subhead "5d. Sequential exec throughput (30 execs, paced)"
    local start_us="${EPOCHREALTIME//.}"
    local i fails=0
    for ((i = 1; i <= 30; i++)); do
        (( i > 1 )) && sleep "$SLEEP_PER_EXEC"
        timeout "$TIMEOUT_PER_CALL" bhatti exec "$SB" -- true >/dev/null 2>&1 || ((fails++))
    done
    local end_us="${EPOCHREALTIME//.}"
    local diff=$((end_us - start_us))
    local total_ms
    total_ms=$(printf '%d.%03d' $((diff / 1000)) $((diff % 1000)))
    local per_exec
    per_exec=$(awk -v t="$diff" 'BEGIN { printf "%.3f", t / 1000 / 30 }')
    echo "  30 sequential execs: ${total_ms}ms total ($per_exec ms/exec including ${SLEEP_PER_EXEC}s pacing; $fails failed)"
}

# =============================================================================
# 7. PUBLISH WAKE — HTTP request triggers wake on a published port.
#
# Models the real user-facing scenario: someone hits a published URL on a
# sandbox that has been idle. The path-based proxy (/sandboxes/<id>/proxy/<port>/)
# uses the same `ensureHot` machinery as the public proxy at <alias>.<zone>,
# so this measurement is faithful to that path even on hosts not in domain mode.
#
# Three states:
#   hot   — sandbox already running, baseline overhead.
#   warm  — vCPUs paused (35s idle, thermal manager has paused).
#   cold  — fully snapshotted to disk (`bhatti stop`).
# =============================================================================

run_publish_wake() {
    header "PUBLISH WAKE (HTTP request triggers wake)"

    local pub="$RUN_ID-pub"
    # Use browser tier since the minimal tier has no HTTP server pre-installed.
    create_sandbox "$pub" --cpus 1 --memory 512 --image browser

    # Tiny in-VM HTTP server (Node, single-line). --detach returns the PID
    # immediately so the script doesn't block on `exec`.
    if ! timeout 30s bhatti exec --detach "$pub" -- \
        node -e 'require("http").createServer((q,r)=>r.end("ok")).listen(3000)' \
        >/dev/null 2>&1; then
        warn "failed to start in-VM http server"
        return
    fi
    sleep 2  # let it bind

    local api_url="${BHATTI_URL:-$(grep -h '^api_url:' ~/.bhatti/config.yaml /etc/bhatti/config.yaml 2>/dev/null | head -1 | awk '{print $2}')}"
    api_url="${api_url:-http://localhost:8080}"
    local token="${BHATTI_TOKEN:-$(grep -h '^auth_token:' ~/.bhatti/config.yaml 2>/dev/null | awk '{print $2}')}"
    local proxy_url="$api_url/sandboxes/$pub/proxy/3000/"

    # Smoke test the proxy URL
    if ! timeout 10s curl -sf -H "Authorization: Bearer $token" "$proxy_url" >/dev/null 2>&1; then
        warn "proxy URL not reachable ($proxy_url) — skipping publish-wake"
        return
    fi

    local i t out

    # 7a. Hot — sandbox already running, no wake.
    subhead "7a. Hot (sandbox running, no wake)"
    out="$RESULTS_DIR/publish_wake_hot.txt"
    : > "$out"
    # Warmup
    for i in 1 2 3; do
        timeout "$TIMEOUT_PER_CALL" curl -sf -o /dev/null -H "Authorization: Bearer $token" "$proxy_url" 2>/dev/null || true
        sleep "$SLEEP_PER_READ"
    done
    for ((i = 1; i <= ITERATIONS; i++)); do
        (( i > 1 )) && sleep "$SLEEP_PER_READ"
        t=$(timeout "$TIMEOUT_PER_CALL" curl -sf -o /dev/null -w '%{time_total}' \
            -H "Authorization: Bearer $token" "$proxy_url" 2>/dev/null) || continue
        awk -v s="$t" 'BEGIN { printf "%.3f\n", s * 1000 }' >> "$out"
    done
    echo "${YELLOW}Hot publish-path TTFB (ms):${NC}"
    percentiles "$out"

    # 7b. Cold wake — stop, then request triggers full snapshot resume.
    subhead "7c. Cold wake on request ($PUBLISH_WAKE_COLD_N samples + 1 warmup)"
    out="$RESULTS_DIR/publish_wake_cold.txt"
    : > "$out"
    # Warmup cycle — first stop+wake pays setup costs we don't want to measure.
    timeout 30s bhatti stop "$pub" >/dev/null 2>&1 || true
    sleep 0.5
    timeout "$TIMEOUT_PER_CALL" curl -sf -o /dev/null \
        -H "Authorization: Bearer $token" "$proxy_url" 2>/dev/null || true
    sleep 1
    for ((i = 1; i <= PUBLISH_WAKE_COLD_N; i++)); do
        timeout 30s bhatti stop "$pub" >/dev/null 2>&1 || true
        sleep 0.5
        t=$(timeout "$TIMEOUT_PER_CALL" curl -sf -o /dev/null -w '%{time_total}' \
            -H "Authorization: Bearer $token" "$proxy_url" 2>/dev/null) || { warn "sample $i failed"; continue; }
        awk -v s="$t" 'BEGIN { printf "%.3f\n", s * 1000 }' >> "$out"
        local ms
        ms=$(awk -v s="$t" 'BEGIN { printf "%.1f", s * 1000 }')
        echo "    sample $i: ${ms}ms (cold wake + proxy + in-VM server)"
    done
    echo "${YELLOW}Cold wake on request (ms):${NC}"
    percentiles "$out"

    # 7c. Warm wake — idle 35s so thermal manager pauses vCPUs, then request.
    subhead "7b. Warm wake on request ($WARM_N samples × 35s idle each)"
    out="$RESULTS_DIR/publish_wake_warm.txt"
    : > "$out"
    # Make sure it's hot first.
    timeout "$TIMEOUT_PER_CALL" curl -sf -o /dev/null \
        -H "Authorization: Bearer $token" "$proxy_url" 2>/dev/null || true
    for ((i = 1; i <= WARM_N; i++)); do
        echo "    sample $i: waiting 35s for thermal manager to pause sandbox..."
        sleep 35
        t=$(timeout "$TIMEOUT_PER_CALL" curl -sf -o /dev/null -w '%{time_total}' \
            -H "Authorization: Bearer $token" "$proxy_url" 2>/dev/null) || { warn "sample $i failed"; continue; }
        awk -v s="$t" 'BEGIN { printf "%.3f\n", s * 1000 }' >> "$out"
        local ms
        ms=$(awk -v s="$t" 'BEGIN { printf "%.1f", s * 1000 }')
        echo "    sample $i: ${ms}ms (warm wake + proxy + in-VM server)"
    done
    echo "${YELLOW}Warm wake on request (ms):${NC}"
    percentiles "$out"
}

# =============================================================================
# 6. NETWORK BASELINE
# =============================================================================

run_network() {
    header "NETWORK BASELINE (client → server)"
    local api_url="${BHATTI_URL:-$(grep -h '^api_url:' ~/.bhatti/config.yaml /etc/bhatti/config.yaml 2>/dev/null | head -1 | awk '{print $2}')}"
    api_url="${api_url:-http://localhost:8080}"

    subhead "6a. TCP connect time"
    local out="$RESULTS_DIR/network_rtt.txt"
    : > "$out"
    local i t
    for ((i = 1; i <= ITERATIONS; i++)); do
        t=$(timeout "$TIMEOUT_PER_CALL" curl -sf -o /dev/null -w '%{time_connect}' "$api_url/health" 2>/dev/null) || continue
        awk -v s="$t" 'BEGIN { printf "%.3f\n", s * 1000 }' >> "$out"
    done
    echo "${YELLOW}TCP connect (ms):${NC}"
    percentiles "$out"

    subhead "6b. TTFB to /health"
    out="$RESULTS_DIR/network_ttfb.txt"
    : > "$out"
    for ((i = 1; i <= ITERATIONS; i++)); do
        t=$(timeout "$TIMEOUT_PER_CALL" curl -sf -o /dev/null -w '%{time_starttransfer}' "$api_url/health" 2>/dev/null) || continue
        awk -v s="$t" 'BEGIN { printf "%.3f\n", s * 1000 }' >> "$out"
    done
    echo "${YELLOW}TTFB /health (ms):${NC}"
    percentiles "$out"
}

# =============================================================================
# Driver
# =============================================================================

want_section lifecycle    && run_lifecycle
want_section warm         && run_warm
want_section exec         && run_exec
want_section files        && run_files
want_section api          && run_api
want_section concurrent   && run_concurrent
want_section network      && run_network
want_section publish-wake && run_publish_wake

echo
header "DONE in $(elapsed_since "$START_US")"
echo "Results: $RESULTS_DIR/"
