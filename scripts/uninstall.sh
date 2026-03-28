#!/bin/bash
# scripts/uninstall.sh — Remove bhatti from a Linux host.
#
# Usage:
#   sudo ./scripts/uninstall.sh           # remove binaries + service, keep data
#   sudo ./scripts/uninstall.sh --purge   # remove everything including data
#
# Safe to run multiple times.
set -euo pipefail

DATA_DIR="/var/lib/bhatti"
PURGE=false

for arg in "$@"; do
    case "$arg" in
        --purge) PURGE=true ;;
        --help|-h)
            echo "Usage: sudo $0 [--purge]"
            echo "  --purge   remove all data (sandboxes, images, volumes, secrets, config)"
            echo "  (default) remove binaries + service only, keep data for reinstall"
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

# --- 2. Kill any running Firecracker VMs ---

KILLED=0
for pid in $(pgrep -f "firecracker --api-sock" 2>/dev/null || true); do
    echo "  killing firecracker pid $pid"
    kill "$pid" 2>/dev/null || true
    KILLED=$((KILLED + 1))
done
if [[ $KILLED -gt 0 ]]; then
    echo "  killed $KILLED firecracker process(es), waiting for cleanup..."
    sleep 2
fi

# --- 3. Clean up network devices ---

# TAP devices (created per-sandbox)
for tap in $(ip -o link show type tun 2>/dev/null | grep "tap" | awk -F': ' '{print $2}' | cut -d@ -f1); do
    echo "  removing tap device: $tap"
    ip link del "$tap" 2>/dev/null || true
done

# Bridge devices (created per-user)
for br in $(ip -o link show type bridge 2>/dev/null | grep "brbhatti" | awk -F': ' '{print $2}' | cut -d@ -f1); do
    echo "  removing bridge: $br"
    ip link del "$br" 2>/dev/null || true
done

# iptables rules (bhatti adds FORWARD rules per-bridge)
for chain in FORWARD; do
    iptables -S "$chain" 2>/dev/null | grep -i "brbhatti" | while read -r rule; do
        echo "  removing iptables rule: $rule"
        iptables $(echo "$rule" | sed 's/^-A/-D/') 2>/dev/null || true
    done
done

# --- 4. Remove binaries ---

for bin in /usr/local/bin/bhatti /usr/local/bin/firecracker; do
    if [[ -f "$bin" ]]; then
        echo "==> Removing $bin"
        rm -f "$bin"
    fi
done

# --- 5. Purge data (only with --purge) ---

if [[ "$PURGE" == "true" ]]; then
    # Unmount any leftover loop mounts from sandboxes
    for mnt in $(mount | grep "$DATA_DIR" | awk '{print $3}'); do
        echo "  unmounting: $mnt"
        umount -l "$mnt" 2>/dev/null || true
    done

    if [[ -d "$DATA_DIR" ]]; then
        echo "==> Removing $DATA_DIR"
        rm -rf "$DATA_DIR"
    fi

    # Remove CLI configs
    for cfg in /root/.bhatti; do
        if [[ -d "$cfg" ]]; then
            echo "==> Removing $cfg"
            rm -rf "$cfg"
        fi
    done

    # Try to find the sudo user's config too
    if [[ -n "${SUDO_USER:-}" && "$SUDO_USER" != "root" ]]; then
        USER_CFG="$(eval echo "~$SUDO_USER")/.bhatti"
        if [[ -d "$USER_CFG" ]]; then
            echo "==> Removing $USER_CFG"
            rm -rf "$USER_CFG"
        fi
    fi
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
    echo "  Binaries + service removed."
    echo "  Data preserved: $DATA_DIR"
    echo "    (config, kernel, rootfs, volumes, secrets)"
    echo ""
    echo "  To reinstall:"
    echo "    sudo ./scripts/install.sh --systemd"
fi
echo "============================================"
