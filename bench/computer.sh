#!/usr/bin/env bash
# =============================================================================
# bench/computer.sh — KasmVNC + agent-loop perf measurement.
#
# Measures the computer tier on two axes that have *disjoint* pixel-delivery
# pipelines:
#
#   - human:  the VNC encoder pipeline. Server-side stats from
#             /api/get_bottleneck_stats and /api/get_frame_stats while a real
#             Chromium renders a fixed page on :99 and the user's browser
#             (the actual VNC client) drives encoder pressure.
#
#   - agent:  the X11 + screenshot path. `screenshot` reads /dev/x11 directly,
#             never touching KasmVNC's encoder. Agents care about screenshot
#             capture latency, xdotool input latency, and the time between
#             an input event and the next screenshot showing its effect.
#
# These are different optimisation targets — knobs that help one don't always
# help the other.
#
# Designed to run on the same host as the bhatti daemon (loopback to
# 127.0.0.1:8080). Bench output joins bench/results/ alongside run.sh.
#
# Usage:
#   ./bench/computer.sh                                       # both phases
#   SECTIONS=human ./bench/computer.sh                        # encoder only
#   SECTIONS=agent ./bench/computer.sh                        # in-VM loop only
#   REUSE=existing-sandbox-name ./bench/computer.sh           # don't create
#   HUMAN_DURATION=120 AGENT_ITERATIONS=50 ./bench/computer.sh
#   TARGET_URL=https://example.com ./bench/computer.sh
#
# Output:
#   bench/results/computer_human.jsonl    — one JSON per second of sampling
#   bench/results/computer_agent.csv      — one row per iteration
# =============================================================================

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────────────────

SECTIONS="${SECTIONS:-human,agent}"
CPUS="${CPUS:-4}"
MEMORY_MB="${MEMORY_MB:-4096}"
TARGET_URL="${TARGET_URL:-https://bhatti.sh}"
HUMAN_DURATION="${HUMAN_DURATION:-60}"
AGENT_ITERATIONS="${AGENT_ITERATIONS:-30}"
RESULTS_DIR="${RESULTS_DIR:-bench/results}"
REUSE="${REUSE:-}"
# SKIP_PROMPT=1 → don't pause for the human to open a browser. Useful when
# iterating kasmvnc config: you keep one browser tab open across runs and
# just want the script to sample without re-prompting every time.
SKIP_PROMPT="${SKIP_PROMPT:-0}"

# Naming: reuse caller-supplied name, else generate one. The $$ suffix means
# parallel runs don't collide on the create call.
NAME="${REUSE:-bench-computer-$$}"

mkdir -p "$RESULTS_DIR"

HUMAN_OUT="$RESULTS_DIR/computer_human.jsonl"
AGENT_OUT="$RESULTS_DIR/computer_agent.csv"
RUN_META="$RESULTS_DIR/computer_run.txt"

# ── Tooling check ─────────────────────────────────────────────────────────────

for cmd in bhatti jq awk; do
    command -v "$cmd" >/dev/null 2>&1 || {
        echo "error: $cmd not found in PATH" >&2
        exit 1
    }
done

# ── Cleanup ───────────────────────────────────────────────────────────────────

# Re-entrancy guard: once cleanup has run, don't run it again. Without this,
# Ctrl+C during the prompt fires the INT trap (cleanup destroys the VM), the
# script keeps going (set -e isn't on), every subsequent bvm call fails, and
# each fresh Ctrl+C re-fires the trap and re-prints "Destroying ...". The
# guard fixes that, and the explicit `exit 130` in on_signal stops the script
# from running past the prompt after a SIGINT.
_cleaned_up=0
cleanup() {
    [ "$_cleaned_up" = "1" ] && return
    _cleaned_up=1
    set +e
    if [ -z "$REUSE" ] && [ -n "${NAME:-}" ]; then
        echo "==> Destroying $NAME..."
        bhatti destroy "$NAME" -y >/dev/null 2>&1 || true
    fi
}
on_signal() {
    cleanup
    exit 130
}
trap cleanup EXIT
trap on_signal INT TERM

# ── Helpers ───────────────────────────────────────────────────────────────────

# bvm: run a command inside the bench VM. Trims output trailing newline.
bvm() { bhatti exec "$NAME" -- "$@"; }

# wait_for_port: poll inside the VM until a TCP port is listening.
wait_for_port() {
    local port="$1" timeout="${2:-30}"
    for _ in $(seq 1 "$timeout"); do
        if bvm ss -tln 2>/dev/null | grep -q ":$port "; then
            return 0
        fi
        sleep 1
    done
    return 1
}

# now_ms: monotonic-ish milliseconds. Good enough for 10ms-resolution timing.
now_ms() { echo $(($(date +%s%N) / 1000000)); }

# stat_or_empty: curl an /api endpoint with creds, return jq-compacted JSON,
# falling back to "null" on any error or non-JSON response.
stat_or_empty() {
    local path="$1"
    local body
    body=$(bvm curl -s -m 2 -u "$KUSER:$KPW" "http://127.0.0.1:6080$path" 2>/dev/null)
    echo "$body" | jq -c . 2>/dev/null || echo "null"
}

# ── Setup ─────────────────────────────────────────────────────────────────────

if [ -z "$REUSE" ]; then
    echo "==> Creating sandbox $NAME (${CPUS} vCPU, ${MEMORY_MB} MB)"
    bhatti create --name "$NAME" --image computer \
        --cpus "$CPUS" --memory "$MEMORY_MB" >/dev/null

    echo "==> Waiting for KasmVNC to bind 6080..."
    if ! wait_for_port 6080 30; then
        echo "FAIL: KasmVNC didn't come up. Check init.sh."
        echo "  bhatti exec $NAME -- ps auxf"
        echo "  bhatti exec $NAME -- ls -la /root/.kasmpasswd /tmp/kasm.log"
        exit 1
    fi
else
    echo "==> Reusing sandbox $NAME"
    if ! bhatti list 2>/dev/null | grep -q "^$NAME "; then
        echo "FAIL: sandbox '$NAME' not found"
        exit 1
    fi
    wait_for_port 6080 5 || {
        echo "FAIL: KasmVNC not listening on 6080 in $NAME"
        exit 1
    }
fi

# Capture creds for the VNC management API.
CREDS=$(bvm vnc-creds --json)
KUSER=$(echo "$CREDS" | jq -r .username)
KPW=$(echo "$CREDS" | jq -r .password)
[ -n "$KUSER" ] && [ -n "$KPW" ] || { echo "FAIL: vnc-creds returned empty"; exit 1; }

# ── Launch the workload (Chromium → TARGET_URL) ───────────────────────────────

if [ -z "$REUSE" ]; then
    echo "==> Launching Chromium → $TARGET_URL on :99"
    # --detach returns once the command has been spawned; the chromium process
    # keeps running inside the VM until the desktop session ends. Logs go to
    # /tmp/chromium.log inside the VM in case we need to debug a render hang.
    bhatti exec "$NAME" --detach -- \
        sh -c "DISPLAY=:99 chromium-browser --kiosk --no-first-run \
                --disable-features=TranslateUI '$TARGET_URL' \
                >/tmp/chromium.log 2>&1" >/dev/null

    echo "==> Waiting for Chromium to render (10s)..."
    sleep 10
fi

# ── Publish 6080 + capture a Mac-reachable URL ────────────────────────────────

# `bhatti publish` is the right call here: it registers the port mapping and
# emits a public URL when a custom domain is configured on the server, or the
# localhost proxy URL otherwise. We constructed the proxy URL ourselves in an
# earlier rev, which was wrong for any operator running the script over SSH
# from a different machine.
#
# The output format isn't structured (no --json on publish today) so we grep
# for an http(s):// token and prefer one that isn't localhost.
echo "==> Publishing 6080..."
PUBLISH_OUT=$(bhatti publish "$NAME" -p 6080 2>&1 || true)
PUBLIC_URL=$(echo "$PUBLISH_OUT" | grep -oE 'https?://[^[:space:]<>]+' | grep -vE '127\.0\.0\.1|localhost' | head -1)
if [ -z "$PUBLIC_URL" ]; then
    PUBLIC_URL=$(echo "$PUBLISH_OUT" | grep -oE 'https?://[^[:space:]<>]+' | head -1)
    echo "    note: no custom-domain URL configured \u2014 falling back to localhost"
    echo "          (SSH-tunnel localhost:8080 to your laptop to reach it,"
    echo "           or set up a custom domain: bhatti.sh/docs/managing/custom-domain)"
fi

# Capture run parameters for later result-diffing. Anything that affects the
# numbers (cpus, memory, target page, bhatti+git versions) goes here so a
# diff between two runs can call out the relevant deltas.
{
    echo "ts=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "name=$NAME"
    echo "reuse=${REUSE:-no}"
    echo "cpus=$CPUS"
    echo "memory_mb=$MEMORY_MB"
    echo "target_url=$TARGET_URL"
    echo "sections=$SECTIONS"
    echo "human_duration=$HUMAN_DURATION"
    echo "agent_iterations=$AGENT_ITERATIONS"
    echo "bhatti=$(bhatti version 2>&1 | awk '/^bhatti/{print $2}')"
    echo "git=$(git -C "$(dirname "$0")/.." rev-parse --short HEAD 2>/dev/null || echo unknown)"
    echo "host=$(uname -n)"
    echo "kernel=$(uname -r)"
} > "$RUN_META"

cat <<EOF

──────────────────────────────────────────────────
  Sandbox:      $NAME
  URL:          $PUBLIC_URL
  KasmVNC user: $KUSER
  KasmVNC pass: $KPW
  Run meta:     $RUN_META
──────────────────────────────────────────────────

EOF

# ── Phase: human (VNC encoder stats) ──────────────────────────────────────────

run_human() {
    cat <<EOF
==> [human] Open the proxy URL above in Chrome or Firefox.
    Log in with the KasmVNC creds (basic auth dialog).
    Once the desktop is rendered and you see Chromium showing $TARGET_URL,
    interact with it normally — scroll, click links — for the duration of
    the measurement.

    Without a connected client, KasmVNC encodes nothing, so /api stats
    return empty objects. Any browser will do; for agni-served stats
    Chrome on macOS is the standard reference.

EOF
    if [ "$SKIP_PROMPT" != "1" ]; then
        read -p "Press Enter when ready to start ${HUMAN_DURATION}s sampling... " _
    fi

    echo "==> [human] Sampling /api/get_bottleneck_stats + /api/get_frame_stats..."
    : > "$HUMAN_OUT"

    local end=$(( $(date +%s) + HUMAN_DURATION ))
    local n=0
    while [ "$(date +%s)" -lt "$end" ]; do
        local ts bn fs
        ts=$(date +%s)
        bn=$(stat_or_empty "/api/get_bottleneck_stats")
        fs=$(stat_or_empty "/api/get_frame_stats?client=all")
        printf '{"ts":%s,"bottleneck":%s,"frames":%s}\n' "$ts" "$bn" "$fs" >> "$HUMAN_OUT"
        n=$(( n + 1 ))
        # Rate-limit to ~1Hz; account for the round-trip time.
        sleep 0.7
    done
    echo "    $n samples → $HUMAN_OUT"
}

# ── Phase: agent (in-VM screenshot/xdotool latency) ───────────────────────────

run_agent() {
    echo "==> [agent] Measuring screenshot + xdotool + click→change loop ($AGENT_ITERATIONS iter)"
    {
        echo "iteration,screenshot_ms,xdotool_click_ms,click_to_change_ms,polls"
    } > "$AGENT_OUT"

    local i ss_t0 ss_t1 ss_ms cl_t0 cl_t1 cl_ms prev_hash new_hash poll change_t0 change_ms

    for i in $(seq 1 "$AGENT_ITERATIONS"); do
        # Time a screenshot capture + base64 encode (the full agent path).
        ss_t0=$(now_ms)
        bvm screenshot /tmp/sa.png >/dev/null
        ss_t1=$(now_ms)
        ss_ms=$(( ss_t1 - ss_t0 ))

        prev_hash=$(bvm md5sum /tmp/sa.png | awk '{print $1}')

        # Time a click. Different coords each iteration so we don't keep
        # double-clicking the same link.
        local x=$(( 200 + (i * 47) % 800 ))
        local y=$(( 200 + (i * 31) % 400 ))
        cl_t0=$(now_ms)
        bvm xdotool mousemove "$x" "$y" click 1 >/dev/null
        cl_t1=$(now_ms)
        cl_ms=$(( cl_t1 - cl_t0 ))

        # Poll for the next screenshot to differ. We use md5 for cheap diffing;
        # a structural-similarity check would be more accurate but adds deps.
        # Cap at 30 polls × 50ms = 1.5s — beyond that we count it as "no change".
        change_t0=$(now_ms)
        change_ms=-1
        for poll in $(seq 1 30); do
            bvm screenshot /tmp/sb.png >/dev/null
            new_hash=$(bvm md5sum /tmp/sb.png | awk '{print $1}')
            if [ "$new_hash" != "$prev_hash" ]; then
                change_ms=$(( $(now_ms) - change_t0 ))
                break
            fi
            sleep 0.05
        done

        echo "$i,$ss_ms,$cl_ms,$change_ms,$poll" >> "$AGENT_OUT"
        printf "    %2d/%-2d  ss=%4dms  click=%4dms  →change=%5sms\n" \
            "$i" "$AGENT_ITERATIONS" "$ss_ms" "$cl_ms" \
            "$([ "$change_ms" -ge 0 ] && echo "$change_ms" || echo "—")"
    done
    echo "    → $AGENT_OUT"
}

# ── Run requested phases ──────────────────────────────────────────────────────

case ",$SECTIONS," in
    *,human,*) run_human ;;
esac
case ",$SECTIONS," in
    *,agent,*) run_agent ;;
esac

# ── Summary ───────────────────────────────────────────────────────────────────

echo ""
echo "═══ Summary ═══"

if [ -s "$HUMAN_OUT" ]; then
    echo ""
    echo "[human] $(wc -l < "$HUMAN_OUT" | tr -d ' ') samples over ${HUMAN_DURATION}s → $HUMAN_OUT"
    echo "  bottleneck-stats keys observed:"
    jq -r '[.bottleneck | objects | keys[]?] | unique | join(", ")' < "$HUMAN_OUT" 2>/dev/null \
        | head -3 | sed 's/^/    /'
    echo "  frame-stats sample (first non-empty):"
    jq -r 'select(.frames != null and .frames != {}) | .frames' < "$HUMAN_OUT" 2>/dev/null \
        | head -1 | head -c 300 | sed 's/^/    /'
    echo ""
fi

if [ -s "$AGENT_OUT" ]; then
    echo "[agent] → $AGENT_OUT"
    awk -F, 'NR>1 {
        ss[NR-1]=$2; cl[NR-1]=$3
        if ($4>=0) { ch[++n_ch]=$4 }
        n=NR-1
    }
    function pct(arr, count, p,    s, k, i, j, t) {
        # arr is 1-indexed. Sort in place via a copy. i/j/t declared as awk
        # locals (extra args) so concurrent pct calls in a single printf
        # do not clobber each other.  (avoid apostrophes; this is in single quotes)
        for (k=1; k<=count; k++) s[k]=arr[k]
        for (i=1; i<=count; i++) for (j=i+1; j<=count; j++) if (s[i]>s[j]) { t=s[i]; s[i]=s[j]; s[j]=t }
        k = int(count * p / 100)
        if (k < 1) k = 1
        return s[k]
    }
    END {
        printf "  screenshot:       p50=%4dms  p95=%4dms  (n=%d)\n", pct(ss,n,50), pct(ss,n,95), n
        printf "  xdotool click:    p50=%4dms  p95=%4dms  (n=%d)\n", pct(cl,n,50), pct(cl,n,95), n
        if (n_ch > 0) {
            printf "  click → change:  p50=%4dms  p95=%4dms  (n=%d, %d/%d had no observable change)\n", \
                pct(ch,n_ch,50), pct(ch,n_ch,95), n_ch, n - n_ch, n
        } else {
            printf "  click → change:  no observable changes in any iteration\n"
        }
    }' "$AGENT_OUT"
fi

echo ""
