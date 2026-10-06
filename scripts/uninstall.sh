#!/bin/bash
# scripts/uninstall.sh — Remove bhatti from a Linux or macOS host.
#
# Usage:
#   sudo ./scripts/uninstall.sh           # remove binaries + service, keep data and API group
#   sudo ./scripts/uninstall.sh --purge   # remove everything including data and API group
#
# Safe to run multiple times.
if [ "${BHATTI_TEST:-}" != "1" ]; then
    set -euo pipefail
fi

DATA_DIR="/var/lib/bhatti"
PURGE=false

linux_api_group_exists() {
    if command -v getent >/dev/null 2>&1; then
        getent group bhatti >/dev/null 2>&1
    elif [ -r /etc/group ]; then
        grep -q '^bhatti:' /etc/group
    fi
}

remove_api_group() {
    [ "$PURGE" = "true" ] || return 0
    if [ "$(uname -s)" = "Darwin" ]; then
        if dscl . -read /Groups/bhatti PrimaryGroupID >/dev/null 2>&1; then
            echo "==> Removing bhatti API group"
            dseditgroup -o delete bhatti
        fi
    elif linux_api_group_exists; then
        echo "==> Removing bhatti API group"
        groupdel bhatti
    fi
}

# Tests source the group helper without running the privileged teardown.
if [ "${BHATTI_TEST:-}" != "1" ]; then

for arg in "$@"; do
    case "$arg" in
        --purge) PURGE=true ;;
        --help|-h)
            echo "Usage: sudo $0 [--purge]"
            echo "  --purge   remove all data (sandboxes, images, volumes, secrets, config) and API group"
            echo "  (default) remove binaries + service only; preserve data and API group"
            exit 0
            ;;
    esac
done

if [[ $EUID -ne 0 ]]; then
    echo "error: must run as root" >&2
    exit 1
fi

echo "==> Uninstalling bhatti"
if [[ "$PURGE" == "true" ]]; then
    echo "    mode: PURGE (all data will be deleted)"
else
    echo "    mode: soft (data in $DATA_DIR preserved)"
fi
echo ""

# --- 1. Stop service ---

if [[ $(uname -s) == Darwin ]]; then
    PLIST=/Library/LaunchDaemons/sh.bhatti.plist
    if [[ -f "$PLIST" ]]; then
        echo "==> Stopping bhatti service..."
        launchctl bootout system "$PLIST" 2>/dev/null || true
        echo "==> Removing launchd unit..."
        rm -f "$PLIST"
    fi
else
    if systemctl is-active bhatti &>/dev/null; then
        echo "==> Stopping bhatti service..."
        systemctl stop bhatti
    fi

    if systemctl is-enabled bhatti &>/dev/null; then
        echo "==> Disabling bhatti service..."
        systemctl disable bhatti
    fi

    if [[ -f /etc/systemd/system/bhatti.service ]]; then
        echo "==> Removing systemd unit..."
        rm -f /etc/systemd/system/bhatti.service
        systemctl daemon-reload
    fi
fi

# --- 2. Kill any running sandbox VMs / gateways (v2 = krucible) ---
#
# v2 runs one bhatti-vmm helper per sandbox and one bhatti-netd gateway per
# owner. Both are plain host processes that outlive a daemon stop (the next
# daemon adopts them), so stopping the service above left them running.

KILLED=0
for pat in "bhatti-vmm" "bhatti-netd"; do
    for pid in $(pgrep -f "$pat" 2>/dev/null || true); do
        echo "  killing $pat pid $pid"
        kill "$pid" 2>/dev/null || true
        KILLED=$((KILLED + 1))
    done
done
if [[ $KILLED -gt 0 ]]; then
    echo "  killed $KILLED helper process(es), waiting for cleanup..."
    sleep 2
fi

# --- 2b. Legacy v1 (Firecracker) teardown — only if FC artifacts are present ---
#
# v2's network gateway (bhatti-netd) is a userspace gVisor netstack: there is NO
# host tap/bridge/iptables state to reap. This block only runs on a leftover v1
# host (per-sandbox TAP + per-user brbhatti bridge + FORWARD rules).

if [[ $(uname -s) == Linux ]] && { command -v firecracker >/dev/null 2>&1 \
   || ip -o link show type bridge 2>/dev/null | grep -q "brbhatti"; }; then
    echo "==> Firecracker (v1) artifacts detected — cleaning up host network state"

    for pid in $(pgrep -f "firecracker --api-sock" 2>/dev/null || true); do
        echo "  killing firecracker pid $pid"
        kill "$pid" 2>/dev/null || true
    done
    sleep 1

    for tap in $(ip -o link show type tun 2>/dev/null | grep "tap" | awk -F': ' '{print $2}' | cut -d@ -f1); do
        echo "  removing tap device: $tap"
        ip link del "$tap" 2>/dev/null || true
    done
    for br in $(ip -o link show type bridge 2>/dev/null | grep "brbhatti" | awk -F': ' '{print $2}' | cut -d@ -f1); do
        echo "  removing bridge: $br"
        ip link del "$br" 2>/dev/null || true
    done
    RULES=$(iptables -S FORWARD 2>/dev/null | grep -i "brbhatti" || true)
    if [[ -n "$RULES" ]]; then
        echo "$RULES" | while read -r rule; do
            echo "  removing iptables rule: $rule"
            # shellcheck disable=SC2086
            iptables $(echo "$rule" | sed 's/^-A/-D/') 2>/dev/null || true
        done
    fi
fi

# --- 3. Remove binaries + the v2 runtime prefix ---

for bin in /usr/local/bin/bhatti /usr/local/bin/firecracker /usr/local/bin/jailer; do
    if [[ -f "$bin" ]]; then
        echo "==> Removing $bin"
        rm -f "$bin"
    fi
done

# The v2 runtime closure (bhatti-vmm, bhatti-netd, libkrun, lean kernel) lives
# under $DATA_DIR/runtime and is re-laid on every install, so remove it even in
# soft mode (it is not user data).
if [[ -d "$DATA_DIR/runtime" ]]; then
    echo "==> Removing $DATA_DIR/runtime"
    rm -rf "$DATA_DIR/runtime"
fi

# --- 5. Purge data (only with --purge) ---

if [[ "$PURGE" == "true" ]]; then
    # Unmount any leftover loop mounts from sandboxes (Linux only).
    if [[ $(uname -s) == Linux ]]; then
        for mnt in $(mount | grep "$DATA_DIR" | awk '{print $3}'); do
            echo "  unmounting: $mnt"
            umount -l "$mnt" 2>/dev/null || true
        done
    fi

    if [[ -d "$DATA_DIR" ]]; then
        echo "==> Removing $DATA_DIR"
        rm -rf "$DATA_DIR"
    fi

    if [[ -d /etc/bhatti ]]; then
        echo "==> Removing /etc/bhatti"
        rm -rf /etc/bhatti
    fi

    # Remove CLI configs
    ROOT_HOME=$(eval echo ~root)
    if [[ -d "$ROOT_HOME/.bhatti" ]]; then
        echo "==> Removing $ROOT_HOME/.bhatti"
        rm -rf "$ROOT_HOME/.bhatti"
    fi

    # Try to find the sudo user's config too
    if [[ -n "${SUDO_USER:-}" && "$SUDO_USER" != "root" ]]; then
        USER_CFG="$(eval echo "~$SUDO_USER")/.bhatti"
        if [[ -d "$USER_CFG" ]]; then
            echo "==> Removing $USER_CFG"
            rm -rf "$USER_CFG"
        fi
    fi

    remove_api_group
else
    echo ""
    echo "  Data preserved in $DATA_DIR"
    echo "  To remove everything: sudo $0 --purge"
fi

# --- Summary ---

echo ""
echo "============================================"
echo "  bhatti uninstalled"
echo ""
if [[ "$PURGE" == "true" ]]; then
    echo "  All data removed."
else
    echo "  Binaries + runtime + service removed."
    echo "  Data preserved: $DATA_DIR"
    echo "    (rootfs images, volumes, secrets, sandboxes)"
    echo "  Config preserved: /etc/bhatti/config.yaml; bhatti API group preserved."
    echo "    (the krucible runtime under $DATA_DIR/runtime is re-installed on update)"
    echo ""
    echo "  To reinstall:"
    echo "    curl -fsSL bhatti.sh/install | sudo bash"
fi
echo "============================================"
fi
