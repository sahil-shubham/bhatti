#!/usr/bin/env bash
set -euo pipefail

# =============================================================================
# Bhatti Performance Benchmark Suite
# Measures real end-to-end latencies against a live bhatti instance.
# Covers: lifecycle (create/stop/start/destroy), exec, files, API, concurrency
# =============================================================================

SB="perf-bench"
ITERATIONS=${1:-30}
# Default to a results/ directory next to this script so the suite works
# from any machine. Override with RESULTS_DIR=... ./run.sh.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RESULTS_DIR="${RESULTS_DIR:-$SCRIPT_DIR/results}"
mkdir -p "$RESULTS_DIR"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

header()  { echo -e "\n${CYAN}${BOLD}━━━ $1 ━━━${NC}"; }
subhead() { echo -e "  ${YELLOW}$1${NC}"; }

percentiles() {
    local file="$1"
    sort -n "$file" | awk '
    {a[NR]=$1; sum+=$1}
    END {
        n=NR
        if (n==0) { print "  (no data)"; exit }
        printf "  n=%-3d  min=%8.1f  p50=%8.1f  p95=%8.1f  p99=%8.1f  max=%8.1f  mean=%8.1f ms\n",
            n, a[1],
            a[int(n*0.50)+1],
            a[int(n*0.95)+1],
            a[int(n*0.99)+1],
            a[n],
            sum/n
    }'
}

# Time a command in milliseconds
time_ms() {
    local start end
    start=$(python3 -c 'import time; print(time.monotonic_ns())')
    "$@" > /dev/null 2>&1
    end=$(python3 -c 'import time; print(time.monotonic_ns())')
    python3 -c "print(($end - $start) / 1_000_000)"
}

# Like time_ms but captures output too (for extracting sandbox IDs etc)
time_ms_out() {
    local outfile="$1"; shift
    local start end
    start=$(python3 -c 'import time; print(time.monotonic_ns())')
    "$@" > "$outfile" 2>&1 || true
    end=$(python3 -c 'import time; print(time.monotonic_ns())')
    python3 -c "print(($end - $start) / 1_000_000)"
}

echo -e "${BOLD}Bhatti Performance Benchmark${NC}"
echo -e "Target: $(bhatti version 2>&1 | head -1)"
echo -e "Iterations per test: $ITERATIONS"
echo -e "Timestamp: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo ""

# ╔═══════════════════════════════════════════════════════════════════════════╗
# ║  PART 1: SANDBOX LIFECYCLE                                               ║
# ╚═══════════════════════════════════════════════════════════════════════════╝

header "PART 1: SANDBOX LIFECYCLE"

# ---- 1a: Full create (cold boot) ----
subhead "1a. Create sandbox (full cold boot, 1 vCPU / 512MB)"
FILE="$RESULTS_DIR/create.txt"
> "$FILE"
LIFECYCLE_N=5  # fewer iterations — these are slow
for i in $(seq 1 "$LIFECYCLE_N"); do
    name="perf-create-$i"
    t=$(time_ms bhatti create --name "$name" --cpus 1 --memory 512)
    echo "$t" >> "$FILE"
    echo -e "    run $i: ${t}ms"
    # Destroy immediately to not leak sandboxes
    bhatti destroy "$name" > /dev/null 2>&1 &
done
wait
echo -e "${YELLOW}Create (cold boot, ms):${NC}"
percentiles "$FILE"

# ---- 1b: Stop (snapshot to disk) ----
subhead "1b. Stop sandbox (full snapshot, hot → cold)"
# Create a persistent sandbox for stop/start cycling
bhatti create --name perf-lifecycle --cpus 1 --memory 512 > /dev/null 2>&1 || true
# Make sure it's hot
bhatti exec perf-lifecycle -- true > /dev/null 2>&1

FILE="$RESULTS_DIR/stop.txt"
> "$FILE"
for i in $(seq 1 "$LIFECYCLE_N"); do
    # Ensure hot first
    bhatti start perf-lifecycle > /dev/null 2>&1 || true
    bhatti exec perf-lifecycle -- true > /dev/null 2>&1
    sleep 1
    t=$(time_ms bhatti stop perf-lifecycle)
    echo "$t" >> "$FILE"
    echo -e "    run $i: ${t}ms"
done
echo -e "${YELLOW}Stop / snapshot (ms):${NC}"
percentiles "$FILE"

# ---- 1c: Start from cold (resume from snapshot) ----
subhead "1c. Start from cold (snapshot resume)"
FILE="$RESULTS_DIR/cold_resume.txt"
> "$FILE"
for i in $(seq 1 "$LIFECYCLE_N"); do
    # Make sure it's stopped
    bhatti stop perf-lifecycle > /dev/null 2>&1 || true
    sleep 0.5
    t=$(time_ms bhatti start perf-lifecycle)
    echo "$t" >> "$FILE"
    echo -e "    run $i: ${t}ms"
done
echo -e "${YELLOW}Cold resume (ms):${NC}"
percentiles "$FILE"

# ---- 1d: Warm resume (exec on warm sandbox — triggers transparent wake) ----
subhead "1d. Warm resume (exec triggers transparent hot←warm wake)"
# We need to let the sandbox go warm (30s idle). Instead, we'll measure
# the exec-on-stopped sandbox which triggers cold resume transparently.
# For warm, we'd need to wait 30s — let's do it properly with a few samples.
FILE="$RESULTS_DIR/warm_resume_exec.txt"
> "$FILE"
# Make sure sandbox is hot first
bhatti start perf-lifecycle > /dev/null 2>&1 || true
bhatti exec perf-lifecycle -- true > /dev/null 2>&1
WARM_N=3
echo "    (waiting for sandbox to go warm — 35s idle each...)"
for i in $(seq 1 "$WARM_N"); do
    # Wait for thermal manager to pause it (30s + buffer)
    sleep 35
    # Now exec — this triggers warm→hot resume transparently
    t=$(time_ms bhatti exec perf-lifecycle -- true)
    echo "$t" >> "$FILE"
    echo -e "    run $i: ${t}ms (includes warm→hot + exec)"
done
echo -e "${YELLOW}Warm resume + exec (ms):${NC}"
percentiles "$FILE"

# ---- 1e: Cold resume via transparent exec ----
subhead "1e. Cold resume via transparent exec (exec on stopped sandbox)"
FILE="$RESULTS_DIR/cold_resume_exec.txt"
> "$FILE"
for i in $(seq 1 "$LIFECYCLE_N"); do
    bhatti stop perf-lifecycle > /dev/null 2>&1 || true
    sleep 0.5
    t=$(time_ms bhatti exec perf-lifecycle -- true)
    echo "$t" >> "$FILE"
    echo -e "    run $i: ${t}ms (cold resume + exec)"
done
echo -e "${YELLOW}Cold resume + exec (ms):${NC}"
percentiles "$FILE"

# ---- 1f: Destroy ----
subhead "1f. Destroy sandbox"
FILE="$RESULTS_DIR/destroy.txt"
> "$FILE"
for i in $(seq 1 "$LIFECYCLE_N"); do
    bhatti create --name "perf-destroy-$i" --cpus 1 --memory 512 > /dev/null 2>&1
    t=$(time_ms bhatti destroy "perf-destroy-$i")
    echo "$t" >> "$FILE"
    echo -e "    run $i: ${t}ms"
done
echo -e "${YELLOW}Destroy (ms):${NC}"
percentiles "$FILE"

# Clean up lifecycle sandbox
bhatti destroy perf-lifecycle > /dev/null 2>&1 || true

# ╔═══════════════════════════════════════════════════════════════════════════╗
# ║  PART 2: EXEC OPERATIONS (HOT SANDBOX)                                  ║
# ╚═══════════════════════════════════════════════════════════════════════════╝

header "PART 2: EXEC OPERATIONS (hot sandbox)"

# Ensure perf-bench exists and is hot
bhatti create --name "$SB" --cpus 2 --memory 2048 > /dev/null 2>&1 || true
bhatti exec "$SB" -- true > /dev/null 2>&1

# ---- 2a: Minimal exec ----
subhead "2a. Exec 'true' (minimal — no output)"
FILE="$RESULTS_DIR/exec_true.txt"
> "$FILE"
for i in $(seq 1 3); do bhatti exec "$SB" -- true > /dev/null 2>&1; done  # warmup
for i in $(seq 1 "$ITERATIONS"); do
    t=$(time_ms bhatti exec "$SB" -- true)
    echo "$t" >> "$FILE"
done
echo -e "${YELLOW}Exec 'true' (ms):${NC}"
percentiles "$FILE"

# ---- 2b: Echo ----
subhead "2b. Exec 'echo hello' (tiny output)"
FILE="$RESULTS_DIR/exec_echo.txt"
> "$FILE"
for i in $(seq 1 3); do bhatti exec "$SB" -- echo hello > /dev/null 2>&1; done
for i in $(seq 1 "$ITERATIONS"); do
    t=$(time_ms bhatti exec "$SB" -- echo hello)
    echo "$t" >> "$FILE"
done
echo -e "${YELLOW}Exec 'echo hello' (ms):${NC}"
percentiles "$FILE"

# ---- 2c: Medium output ----
subhead "2c. Exec 'cat /etc/os-release' (~400B output)"
FILE="$RESULTS_DIR/exec_cat.txt"
> "$FILE"
for i in $(seq 1 3); do bhatti exec "$SB" -- cat /etc/os-release > /dev/null 2>&1; done
for i in $(seq 1 "$ITERATIONS"); do
    t=$(time_ms bhatti exec "$SB" -- cat /etc/os-release)
    echo "$t" >> "$FILE"
done
echo -e "${YELLOW}Exec 'cat /etc/os-release' (ms):${NC}"
percentiles "$FILE"

# ---- 2d: Larger output ----
subhead "2d. Exec 'ls -laR /usr/bin' (~50KB output)"
FILE="$RESULTS_DIR/exec_ls.txt"
> "$FILE"
for i in $(seq 1 3); do bhatti exec "$SB" -- ls -laR /usr/bin > /dev/null 2>&1; done
for i in $(seq 1 "$ITERATIONS"); do
    t=$(time_ms bhatti exec "$SB" -- ls -laR /usr/bin)
    echo "$t" >> "$FILE"
done
echo -e "${YELLOW}Exec 'ls -laR /usr/bin' (ms):${NC}"
percentiles "$FILE"

# ---- 2e: CPU-bound command ----
subhead "2e. Exec 'sha256sum /usr/bin/bash' (CPU-bound)"
FILE="$RESULTS_DIR/exec_sha.txt"
> "$FILE"
for i in $(seq 1 3); do bhatti exec "$SB" -- sha256sum /usr/bin/bash > /dev/null 2>&1; done
for i in $(seq 1 "$ITERATIONS"); do
    t=$(time_ms bhatti exec "$SB" -- sha256sum /usr/bin/bash)
    echo "$t" >> "$FILE"
done
echo -e "${YELLOW}Exec 'sha256sum' (ms):${NC}"
percentiles "$FILE"

# ---- 2f: Exec with env ----
subhead "2f. Exec with env var"
FILE="$RESULTS_DIR/exec_env.txt"
> "$FILE"
for i in $(seq 1 3); do bhatti exec "$SB" -- sh -c 'echo $FOO' > /dev/null 2>&1; done
for i in $(seq 1 "$ITERATIONS"); do
    t=$(time_ms bhatti exec "$SB" -- sh -c 'echo $HOME')
    echo "$t" >> "$FILE"
done
echo -e "${YELLOW}Exec 'sh -c echo' (ms):${NC}"
percentiles "$FILE"

# ╔═══════════════════════════════════════════════════════════════════════════╗
# ║  PART 3: FILE OPERATIONS                                                ║
# ╚═══════════════════════════════════════════════════════════════════════════╝

header "PART 3: FILE OPERATIONS"

# Prep test files in the sandbox
bhatti exec "$SB" -- sh -c 'head -c 1024 /dev/urandom | base64 > /tmp/bench1k.txt' > /dev/null 2>&1
bhatti exec "$SB" -- sh -c 'head -c 10240 /dev/urandom | base64 > /tmp/bench10k.txt' > /dev/null 2>&1
bhatti exec "$SB" -- sh -c 'head -c 102400 /dev/urandom | base64 > /tmp/bench100k.txt' > /dev/null 2>&1
bhatti exec "$SB" -- sh -c 'head -c 1048576 /dev/urandom | base64 > /tmp/bench1m.txt' > /dev/null 2>&1

# Prep local write files
head -c 1024 /dev/urandom | base64 > /tmp/bhatti_w1k.txt
head -c 10240 /dev/urandom | base64 > /tmp/bhatti_w10k.txt
head -c 102400 /dev/urandom | base64 > /tmp/bhatti_w100k.txt

# ---- 3a-d: File reads ----
for size_label in "1k:1KB" "10k:10KB" "100k:100KB" "1m:1MB"; do
    IFS=':' read -r tag label <<< "$size_label"
    subhead "3. File read $label"
    FILE="$RESULTS_DIR/file_read_${tag}.txt"
    > "$FILE"
    for i in $(seq 1 3); do bhatti file read "$SB" "/tmp/bench${tag}.txt" > /dev/null 2>&1; done
    for i in $(seq 1 "$ITERATIONS"); do
        t=$(time_ms bhatti file read "$SB" "/tmp/bench${tag}.txt")
        echo "$t" >> "$FILE"
    done
    echo -e "${YELLOW}File read $label (ms):${NC}"
    percentiles "$FILE"
done

# ---- 3e-g: File writes ----
for size_label in "1k:1KB" "10k:10KB" "100k:100KB"; do
    IFS=':' read -r tag label <<< "$size_label"
    subhead "3. File write $label"
    FILE="$RESULTS_DIR/file_write_${tag}.txt"
    > "$FILE"
    for i in $(seq 1 3); do cat "/tmp/bhatti_w${tag}.txt" | bhatti file write "$SB" "/tmp/benchw${tag}.txt" > /dev/null 2>&1; done
    for i in $(seq 1 "$ITERATIONS"); do
        t=$(time_ms bash -c "cat /tmp/bhatti_w${tag}.txt | bhatti file write $SB /tmp/benchw${tag}.txt")
        echo "$t" >> "$FILE"
    done
    echo -e "${YELLOW}File write $label (ms):${NC}"
    percentiles "$FILE"
done

# ---- 3h: File ls ----
subhead "3h. File ls /usr/bin"
FILE="$RESULTS_DIR/file_ls.txt"
> "$FILE"
for i in $(seq 1 3); do bhatti file ls "$SB" /usr/bin > /dev/null 2>&1; done
for i in $(seq 1 "$ITERATIONS"); do
    t=$(time_ms bhatti file ls "$SB" /usr/bin)
    echo "$t" >> "$FILE"
done
echo -e "${YELLOW}File ls /usr/bin (ms):${NC}"
percentiles "$FILE"

# ╔═══════════════════════════════════════════════════════════════════════════╗
# ║  PART 4: API / CONTROL PLANE                                            ║
# ╚═══════════════════════════════════════════════════════════════════════════╝

header "PART 4: API / CONTROL PLANE"

subhead "4a. List sandboxes"
FILE="$RESULTS_DIR/api_list.txt"
> "$FILE"
for i in $(seq 1 3); do bhatti list --json > /dev/null 2>&1; done
for i in $(seq 1 "$ITERATIONS"); do
    t=$(time_ms bhatti list --json)
    echo "$t" >> "$FILE"
done
echo -e "${YELLOW}List sandboxes (ms):${NC}"
percentiles "$FILE"

subhead "4b. Inspect sandbox"
FILE="$RESULTS_DIR/api_inspect.txt"
> "$FILE"
for i in $(seq 1 3); do bhatti inspect "$SB" --json > /dev/null 2>&1; done
for i in $(seq 1 "$ITERATIONS"); do
    t=$(time_ms bhatti inspect "$SB" --json)
    echo "$t" >> "$FILE"
done
echo -e "${YELLOW}Inspect sandbox (ms):${NC}"
percentiles "$FILE"

subhead "4c. Raw HTTP (curl to /health — no auth)"
FILE="$RESULTS_DIR/api_health.txt"
> "$FILE"
API_URL=$(grep api_url ~/.config/bhatti/config.yaml 2>/dev/null | awk '{print $2}' || echo "https://api.bhatti.sh")
for i in $(seq 1 3); do curl -sf "$API_URL/health" > /dev/null 2>&1; done
for i in $(seq 1 "$ITERATIONS"); do
    t=$(time_ms curl -sf "$API_URL/health")
    echo "$t" >> "$FILE"
done
echo -e "${YELLOW}GET /health (ms):${NC}"
percentiles "$FILE"

subhead "4d. Raw HTTP (curl to /sandboxes — with auth)"
FILE="$RESULTS_DIR/api_sandboxes_curl.txt"
> "$FILE"
TOKEN=$(grep auth_token ~/.config/bhatti/config.yaml 2>/dev/null | awk '{print $2}')
for i in $(seq 1 3); do curl -sf -H "Authorization: Bearer $TOKEN" "$API_URL/sandboxes" > /dev/null 2>&1; done
for i in $(seq 1 "$ITERATIONS"); do
    t=$(time_ms curl -sf -H "Authorization: Bearer $TOKEN" "$API_URL/sandboxes")
    echo "$t" >> "$FILE"
done
echo -e "${YELLOW}GET /sandboxes (curl, ms):${NC}"
percentiles "$FILE"

# ╔═══════════════════════════════════════════════════════════════════════════╗
# ║  PART 5: CONCURRENCY                                                    ║
# ╚═══════════════════════════════════════════════════════════════════════════╝

header "PART 5: CONCURRENCY"

subhead "5a. 5 concurrent execs"
FILE="$RESULTS_DIR/concurrent_5.txt"
> "$FILE"
for run in $(seq 1 10); do
    start=$(python3 -c 'import time; print(time.monotonic_ns())')
    for i in $(seq 1 5); do
        bhatti exec "$SB" -- true > /dev/null 2>&1 &
    done
    wait
    end=$(python3 -c 'import time; print(time.monotonic_ns())')
    python3 -c "print(($end - $start) / 1_000_000)" >> "$FILE"
done
echo -e "${YELLOW}5 concurrent execs, wall time (ms):${NC}"
percentiles "$FILE"

subhead "5b. 10 concurrent execs"
FILE="$RESULTS_DIR/concurrent_10.txt"
> "$FILE"
for run in $(seq 1 10); do
    start=$(python3 -c 'import time; print(time.monotonic_ns())')
    for i in $(seq 1 10); do
        bhatti exec "$SB" -- true > /dev/null 2>&1 &
    done
    wait
    end=$(python3 -c 'import time; print(time.monotonic_ns())')
    python3 -c "print(($end - $start) / 1_000_000)" >> "$FILE"
done
echo -e "${YELLOW}10 concurrent execs, wall time (ms):${NC}"
percentiles "$FILE"

subhead "5c. 20 concurrent execs"
FILE="$RESULTS_DIR/concurrent_20.txt"
> "$FILE"
for run in $(seq 1 10); do
    start=$(python3 -c 'import time; print(time.monotonic_ns())')
    for i in $(seq 1 20); do
        bhatti exec "$SB" -- true > /dev/null 2>&1 &
    done
    wait
    end=$(python3 -c 'import time; print(time.monotonic_ns())')
    python3 -c "print(($end - $start) / 1_000_000)" >> "$FILE"
done
echo -e "${YELLOW}20 concurrent execs, wall time (ms):${NC}"
percentiles "$FILE"

subhead "5d. Sequential exec throughput (30 execs back-to-back)"
FILE="$RESULTS_DIR/sequential_throughput.txt"
> "$FILE"
start=$(python3 -c 'import time; print(time.monotonic_ns())')
for i in $(seq 1 30); do
    bhatti exec "$SB" -- true > /dev/null 2>&1
done
end=$(python3 -c 'import time; print(time.monotonic_ns())')
total_ms=$(python3 -c "print(($end - $start) / 1_000_000)")
per_exec=$(python3 -c "print($total_ms / 30)")
echo -e "  30 sequential execs: ${total_ms}ms total, ${per_exec}ms/exec"

# ╔═══════════════════════════════════════════════════════════════════════════╗
# ║  PART 6: NETWORK LATENCY BASELINE                                       ║
# ╚═══════════════════════════════════════════════════════════════════════════╝

header "PART 6: NETWORK BASELINE (client → server RTT)"

subhead "6a. TLS handshake + TCP RTT"
FILE="$RESULTS_DIR/network_rtt.txt"
> "$FILE"
for i in $(seq 1 "$ITERATIONS"); do
    # Use curl with timing to measure just the TLS connection setup
    t=$(curl -sf -o /dev/null -w '%{time_connect}' "$API_URL/health" 2>/dev/null)
    ms=$(python3 -c "print(float('$t') * 1000)")
    echo "$ms" >> "$FILE"
done
echo -e "${YELLOW}TCP connect time (ms):${NC}"
percentiles "$FILE"

FILE="$RESULTS_DIR/network_ttfb.txt"
> "$FILE"
for i in $(seq 1 "$ITERATIONS"); do
    t=$(curl -sf -o /dev/null -w '%{time_starttransfer}' "$API_URL/health" 2>/dev/null)
    ms=$(python3 -c "print(float('$t') * 1000)")
    echo "$ms" >> "$FILE"
done
echo -e "${YELLOW}TTFB /health (ms):${NC}"
percentiles "$FILE"

# ╔═══════════════════════════════════════════════════════════════════════════╗
# ║  SUMMARY                                                                ║
# ╚═══════════════════════════════════════════════════════════════════════════╝

header "DONE — Results in $RESULTS_DIR"
echo ""
echo -e "${BOLD}Quick summary:${NC}"
echo -e "  Exec 'true':        $(sort -n $RESULTS_DIR/exec_true.txt | awk '{a[NR]=$1} END {printf "p50=%.0fms p99=%.0fms", a[int(NR*0.5)+1], a[int(NR*0.99)+1]}')"
echo -e "  File read 1KB:      $(sort -n $RESULTS_DIR/file_read_1k.txt | awk '{a[NR]=$1} END {printf "p50=%.0fms p99=%.0fms", a[int(NR*0.5)+1], a[int(NR*0.99)+1]}')"
echo -e "  File write 1KB:     $(sort -n $RESULTS_DIR/file_write_1k.txt | awk '{a[NR]=$1} END {printf "p50=%.0fms p99=%.0fms", a[int(NR*0.5)+1], a[int(NR*0.99)+1]}')"
echo -e "  Cold resume+exec:   $(sort -n $RESULTS_DIR/cold_resume_exec.txt | awk '{a[NR]=$1} END {printf "p50=%.0fms p99=%.0fms", a[int(NR*0.5)+1], a[int(NR*0.99)+1]}')"
echo -e "  Create (cold boot): $(sort -n $RESULTS_DIR/create.txt | awk '{a[NR]=$1} END {printf "p50=%.0fms p99=%.0fms", a[int(NR*0.5)+1], a[int(NR*0.99)+1]}')"
echo -e "  Network RTT:        $(sort -n $RESULTS_DIR/network_rtt.txt | awk '{a[NR]=$1} END {printf "p50=%.0fms p99=%.0fms", a[int(NR*0.5)+1], a[int(NR*0.99)+1]}')"
