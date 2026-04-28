#!/bin/bash
# Minimal tier: Ubuntu + systemd + lohar agent.
# Called by build-tier.sh with $MOUNT, $ARCH, $DEB_ARCH, $AGENT set.
#
# All other tiers source this first. systemd is the only init —
# lohar runs as a systemd service, not PID 1.
set -euo pipefail

echo "==> Installing minimal tier packages..."
chroot "$MOUNT" /bin/bash -c '
set -eu
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y --no-install-recommends \
    iproute2 ca-certificates sudo curl locales fuse3 \
    systemd systemd-sysv dbus

# Locale
sed -i "/en_US.UTF-8/s/^# //g" /etc/locale.gen
locale-gen

# Create lohar user (bash is the default shell — comes with minbase)
useradd -m -s /bin/bash -G sudo lohar
echo "lohar ALL=(ALL) NOPASSWD:ALL" >> /etc/sudoers

# FUSE: add lohar to fuse group (if group exists), enable user_allow_other
getent group fuse >/dev/null 2>&1 && usermod -aG fuse lohar || true
sed -i "s/^#[[:space:]]*user_allow_other$/user_allow_other/" /etc/fuse.conf

# --- systemd configuration ---

# Pin systemd-resolved out of apt. Without this, apt-get install
# openssh-server pulls resolved back via the dependency chain
# (openssh-server -> libpam-systemd -> systemd meta-package).
cat > /etc/apt/preferences.d/no-resolved << APTPIN
Package: systemd-resolved
Pin: release *
Pin-Priority: -1
APTPIN

# Mask services that conflict with lohar, break snapshot/restore,
# or add overhead with zero value in a sandbox VM.
MASK_UNITS="
  systemd-resolved.service
  systemd-networkd.service
  systemd-networkd-wait-online.service
  systemd-timesyncd.service
  systemd-timedated.service
  systemd-logind.service
  systemd-user-sessions.service
  getty@.service
  serial-getty@.service
  console-setup.service
  keyboard-setup.service
  apt-daily.timer
  apt-daily-upgrade.timer
  e2scrub_all.timer
  fstrim.timer
  motd-news.timer
  man-db.timer
  systemd-tmpfiles-clean.timer
"
for unit in $MASK_UNITS; do
    systemctl mask "$unit" 2>/dev/null || true
done

# Journal: volatile (tmpfs), bounded at 8MB, no rate limiting so
# apt-get output and service logs are never suppressed.
mkdir -p /etc/systemd/journald.conf.d
cat > /etc/systemd/journald.conf.d/bhatti.conf << JCONF
[Journal]
Storage=volatile
RuntimeMaxUse=8M
RateLimitIntervalSec=0
JCONF

# System: disable watchdogs (interfere with snapshot/restore),
# fast timeouts so shutdown is snappy.
mkdir -p /etc/systemd/system.conf.d
cat > /etc/systemd/system.conf.d/bhatti.conf << SCONF
[Manager]
RuntimeWatchdogSec=0
ShutdownWatchdogSec=0
DefaultTimeoutStartSec=15s
DefaultTimeoutStopSec=10s
SCONF

# Default target — no graphical, no rescue.
systemctl set-default multi-user.target

apt-get clean
rm -rf /var/lib/apt/lists/*
'

# lohar.service — the agent that handles exec, files, sessions.
# Enabled so systemd starts it on boot.
cat > "$MOUNT/etc/systemd/system/lohar.service" << 'UNIT'
[Unit]
Description=Lohar sandbox agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/lohar
Restart=no
TimeoutStopSec=5
WatchdogSec=0
Delegate=yes

[Install]
WantedBy=multi-user.target
UNIT
chroot "$MOUNT" systemctl enable lohar.service

# Workspace
mkdir -p "$MOUNT/workspace"
chown 1000:1000 "$MOUNT/workspace"

# DNS — static resolv.conf, no resolved.
cat > "$MOUNT/etc/resolv.conf" << 'EOF'
nameserver 1.1.1.1
nameserver 8.8.8.8
EOF

# Install lohar
cp "$AGENT" "$MOUNT/usr/local/bin/lohar"
chmod 755 "$MOUNT/usr/local/bin/lohar"

echo "==> Minimal tier done."
