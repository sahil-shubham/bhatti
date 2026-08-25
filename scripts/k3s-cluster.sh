#!/bin/bash
# k3s-cluster.sh — spin up a k3s HA cluster on bhatti sandboxes and time it.
#
# Usage:
#   ./scripts/k3s-cluster.sh prepare          # create k3s-bin volume + download binary (one-time)
#   ./scripts/k3s-cluster.sh up --servers 3
#   ./scripts/k3s-cluster.sh up --servers 3 --agents 2
#   ./scripts/k3s-cluster.sh down
#   ./scripts/k3s-cluster.sh status
#
# Nodes are created fresh (unique IPs) with a shared read-only volume
# containing the k3s binary — no per-node download.
# All nodes are keep-hot (cold-restore is wedged — never wake).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BHATTI="${BHATTI:-$SCRIPT_DIR/../bhatti}"
K3S_VERSION="v1.31.5+k3s1"
K3S_ARCH="k3s-arm64"
CLUSTER_TOKEN="bhatti-k3s-cluster"
VOL_NAME="k3s-bin"
SERVER_MEM=2048
AGENT_MEM=1024
SERVER_CPUS=2
K3S_FLAGS="--flannel-backend=host-gw --disable traefik,servicelb,metrics-server --disable-network-policy --disable-cloud-controller"

# --- timing ---
T0=0
now_ms() { python3 -c 'import time; print(int(time.time()*1000))'; }
timer_start() { T0=$(now_ms); }
timer_elapsed() { echo $(( $(now_ms) - T0 )); }
timer_log() { local label="$1"; local ms="$2"; printf "  %-32s %d.%03ds\n" "$label" $((ms/1000)) $((ms%1000)); }

# --- helpers ---
b() { "$BHATTI" "$@" 2>&1; }

get_ip() {
  b inspect "$1" 2>/dev/null | grep -E '^\s*IP:' | awk '{print $2}'
}

# Wait for exec to work (agent ready after fresh create)
wait_agent() {
  local name="$1"
  local timeout="${2:-30}"
  for _ in $(seq 1 "$timeout"); do
    if b exec "$name" -- echo ok 2>/dev/null | grep -q ok; then return 0; fi
    sleep 1
  done
  echo "  TIMEOUT: agent not ready in $name" >&2; return 1
}

wait_apiserver() {
  local name="$1" ip="$2" timeout="${3:-120}"
  for _ in $(seq 1 "$((timeout / 2))"); do
    if b exec "$name" -- sudo sh -c "curl -sf --max-time 3 https://${ip}:6443/readyz" 2>/dev/null | grep -q ok; then
      return 0
    fi
    sleep 2
  done
  echo "  TIMEOUT: apiserver not ready" >&2; return 1
}

wait_node_ready() {
  local name="$1" k3s_name="$2" timeout="${3:-120}"
  for _ in $(seq 1 "$((timeout / 2))"); do
    local status
    status=$(b exec "$name" -- sudo /mnt/k3s-bin/k3s kubectl get nodes 2>/dev/null \
      | grep "$k3s_name" | awk '{print $2}' || true)
    [ "$status" = "Ready" ] && return 0
    sleep 2
  done
  echo "  TIMEOUT: node $k3s_name not Ready" >&2; return 1
}

get_etcd_members() {
  b exec "$1" -- sudo /mnt/k3s-bin/k3s kubectl get nodes 2>/dev/null \
    | grep -c 'control-plane' || true
}

# --- prepare: create volume with k3s binary ---

cmd_prepare() {
  echo "=== preparing k3s-bin volume (one-time setup) ==="

  # delete old volume if exists
  b volume delete "$VOL_NAME" --yes >/dev/null 2>&1 || true

  b volume create --name "$VOL_NAME" --size 128 2>&1 | head -1

  echo "--- downloading k3s into volume ---"
  timer_start
  # create a helper sandbox with the volume, download k3s, destroy
  b create --name k3s-helper --memory 512 --cpus 1 --keep-hot \
    --volume "${VOL_NAME}:/mnt/k3s-bin" >/dev/null 2>&1
  wait_agent k3s-helper 30
  b exec k3s-helper -- sudo sh -c "
    curl -sfL --max-time 120 https://github.com/k3s-io/k3s/releases/download/${K3S_VERSION}/${K3S_ARCH} \
      -o /mnt/k3s-bin/k3s && chmod +x /mnt/k3s-bin/k3s
  " 2>&1 | tail -1
  b destroy k3s-helper --yes >/dev/null 2>&1
  timer_log "k3s download (one-time)" "$(timer_elapsed)"

  echo; echo "k3s-bin volume ready. Run:"
  echo "  ./scripts/k3s-cluster.sh up --servers 3"
}

# --- up: spin up the cluster ---

cmd_up() {
  local servers=3 agents=0
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --servers) servers="$2"; shift 2 ;;
      --agents)  agents="$2";  shift 2 ;;
      *) echo "unknown: $1"; exit 1 ;;
    esac
  done

  # check volume exists
  if ! b volume list 2>/dev/null | grep -q "$VOL_NAME"; then
    echo "ERROR: $VOL_NAME volume not found. Run: ./scripts/k3s-cluster.sh prepare"
    exit 1
  fi

  echo "=== k3s HA cluster: $servers servers, $agents agents ==="
  echo "    volume: $VOL_NAME (shared k3s binary — no per-node download)"
  echo "    memory: ${SERVER_MEM}MB/server, ${AGENT_MEM}MB/agent"
  echo "    token:  $CLUSTER_TOKEN"
  echo "    flannel: host-gw (formation only — cross-node pods need Phase 2)"
  echo

  # --- create sandboxes in parallel (unique IPs, k3s from volume) ---
  echo "--- creating sandboxes ---"
  timer_start
  local pids=()
  for i in $(seq 1 $servers); do
    b create --name "k3s-server-$i" --memory "$SERVER_MEM" --cpus "$SERVER_CPUS" \
      --keep-hot --volume "${VOL_NAME}:/mnt/k3s-bin:ro" >/dev/null 2>&1 &
    pids+=($!)
  done
  for i in $(seq 1 $agents); do
    b create --name "k3s-agent-$i" --memory "$AGENT_MEM" --cpus 1 \
      --keep-hot --volume "${VOL_NAME}:/mnt/k3s-bin:ro" >/dev/null 2>&1 &
    pids+=($!)
  done
  for pid in "${pids[@]}"; do wait "$pid"; done
  local t_create
  t_create=$(timer_elapsed)
  timer_log "sandbox creation ($((servers + agents)) nodes)" "$t_create"

  # let agents settle
  sleep 2

  # --- start server 1 (cluster-init) ---
  echo; echo "--- starting server 1 (cluster-init) ---"
  timer_start
  b exec "k3s-server-1" --detach -- sudo /mnt/k3s-bin/k3s server \
    --cluster-init --token="$CLUSTER_TOKEN" \
    $K3S_FLAGS --data-dir /var/lib/rancher

  local server1_ip
  server1_ip=$(get_ip "k3s-server-1")
  echo "  server-1 IP: $server1_ip"

  wait_apiserver "k3s-server-1" "$server1_ip" 120
  local t_s1
  t_s1=$(timer_elapsed)
  timer_log "server 1 → apiserver ready" "$t_s1"

  # --- join servers 2..N ---
  echo; echo "--- joining servers 2..$servers ---"
  local join_total=0
  for i in $(seq 2 $servers); do
    local name="k3s-server-$i"
    timer_start
    b exec "$name" --detach -- sudo /mnt/k3s-bin/k3s server \
      --server "https://${server1_ip}:6443" --token="$CLUSTER_TOKEN" \
      $K3S_FLAGS --data-dir /var/lib/rancher

    wait_node_ready "k3s-server-1" "$name" 120
    local t_join
    t_join=$(timer_elapsed)
    join_total=$((join_total + t_join))
    timer_log "server $i → joined + Ready" "$t_join"
  done

  # --- start agents ---
  if [ "$agents" -gt 0 ]; then
    echo; echo "--- starting $agents agents ---"
    for i in $(seq 1 $agents); do
      b exec "k3s-agent-$i" --detach -- sudo /mnt/k3s-bin/k3s agent \
        --server "https://${server1_ip}:6443" --token="$CLUSTER_TOKEN"
    done
    for i in $(seq 1 $agents); do
      wait_node_ready "k3s-server-1" "k3s-agent-$i" 120
      echo "  agent $i → Ready"
    done
  fi

  # --- etcd quorum ---
  echo; echo "--- waiting for etcd quorum ---"
  timer_start
  local quorum_ok=0 members=0
  for _ in $(seq 1 60); do
    members=$(get_etcd_members "k3s-server-1")
    [ "$members" -ge "$servers" ] && { quorum_ok=1; break; }
    sleep 2
  done
  local t_quorum
  t_quorum=$(timer_elapsed)
  [ "$quorum_ok" = "1" ] \
    && timer_log "etcd quorum ($servers members)" "$t_quorum" \
    || echo "  WARNING: etcd $members/$servers"

  # --- CoreDNS ---
  echo; echo "--- waiting for CoreDNS ---"
  timer_start
  for _ in $(seq 1 60); do
    local coredns
    coredns=$(b exec "k3s-server-1" -- sudo /mnt/k3s-bin/k3s kubectl get pods -n kube-system 2>/dev/null \
      | grep coredns | awk '{print $2}' || true)
    [ "$coredns" = "1/1" ] && break
    sleep 2
  done
  local t_pods
  t_pods=$(timer_elapsed)
  timer_log "CoreDNS 1/1" "$t_pods"

  # --- summary ---
  local t_total=$((t_create + t_s1 + join_total + t_quorum + t_pods))
  echo; echo "=== CLUSTER READY ==="
  b exec "k3s-server-1" -- sudo /mnt/k3s-bin/k3s kubectl get nodes 2>&1 | head -10
  echo
  b exec "k3s-server-1" -- sudo /mnt/k3s-bin/k3s kubectl get pods -A 2>&1 | head -10
  echo; echo "=== TIMING ==="
  timer_log "sandbox creation" "$t_create"
  timer_log "server 1 → apiserver" "$t_s1"
  timer_log "server joins (total)" "$join_total"
  timer_log "etcd quorum" "$t_quorum"
  timer_log "CoreDNS" "$t_pods"
  echo "  ──────────────────────────────"
  timer_log "TOTAL" "$t_total"
  echo
  echo "  bhatti overhead: ${t_create}ms"
  echo "  k3s bootstrap:   $((t_total - t_create))ms"
}

cmd_down() {
  echo "=== destroying k3s cluster ==="
  for name in $(b ls 2>/dev/null | grep -E 'k3s-server-|k3s-agent-' | awk '{print $1}'); do
    echo "  $name"
    b destroy "$name" --yes >/dev/null 2>&1 || true
  done
  echo "done"
  echo "(k3s-bin volume preserved — run 'down --all' to remove it too)"
}

cmd_down_all() {
  cmd_down
  echo "  k3s-bin volume"
  b volume delete "$VOL_NAME" --yes >/dev/null 2>&1 || true
}

cmd_status() {
  echo "=== cluster status ==="
  b exec "k3s-server-1" -- sudo /mnt/k3s-bin/k3s kubectl get nodes 2>&1 | head -10
  echo
  b exec "k3s-server-1" -- sudo /mnt/k3s-bin/k3s kubectl get pods -A 2>&1 | head -10
}

case "${1:-}" in
  prepare)  shift; cmd_prepare "$@" ;;
  up)       shift; cmd_up "$@" ;;
  down)     [[ "${2:-}" == "--all" ]] && cmd_down_all || cmd_down ;;
  status)   cmd_status ;;
  *) echo "usage: $0 {prepare | up --servers N [--agents N] | down [--all] | status}"; exit 1 ;;
esac
