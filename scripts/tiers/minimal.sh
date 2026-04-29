#!/bin/bash
# Minimal tier: bare Ubuntu + lohar dependencies.
# Called by build-tier.sh with $MOUNT, $ARCH, $DEB_ARCH, $AGENT set.
#
# lohar runs as PID 1 and provides a systemctl shim (busybox pattern).
# No real systemd — package installs work via the shim.
set -euo pipefail

echo "==> Installing minimal tier packages..."
chroot "$MOUNT" /bin/bash -c '
set -eu
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y --no-install-recommends \
    iproute2 ca-certificates sudo curl locales fuse3

# Locale
sed -i "/en_US.UTF-8/s/^# //g" /etc/locale.gen
locale-gen

# Create lohar user (bash is the default shell — comes with minbase)
useradd -m -s /bin/bash -G sudo lohar
echo "lohar ALL=(ALL) NOPASSWD:ALL" >> /etc/sudoers

# FUSE: add lohar to fuse group (if group exists), enable user_allow_other
getent group fuse >/dev/null 2>&1 && usermod -aG fuse lohar || true
sed -i "s/^#[[:space:]]*user_allow_other$/user_allow_other/" /etc/fuse.conf

apt-get clean
rm -rf /var/lib/apt/lists/*
'

# Workspace
mkdir -p "$MOUNT/workspace"
chown 1000:1000 "$MOUNT/workspace"

# DNS
cat > "$MOUNT/etc/resolv.conf" << 'EOF'
nameserver 1.1.1.1
nameserver 8.8.8.8
EOF

# Install lohar
cp "$AGENT" "$MOUNT/usr/local/bin/lohar"
chmod 755 "$MOUNT/usr/local/bin/lohar"

# systemctl + journalctl shims — lohar handles these via busybox pattern.
# Package postinst scripts call systemctl enable/start/stop; these symlinks
# route those calls to lohar's built-in service manager.
ln -sf /usr/local/bin/lohar "$MOUNT/usr/bin/systemctl"
ln -sf /usr/local/bin/lohar "$MOUNT/usr/bin/journalctl"

# Target wants directory — deb-systemd-helper creates enable symlinks here.
mkdir -p "$MOUNT/etc/systemd/system/multi-user.target.wants"

# policy-rc.d — tells invoke-rc.d to allow all service actions.
# Without this, package postinst scripts skip starting services with
# "No init system and policy-rc.d missing! Defaulting to block."
cat > "$MOUNT/usr/sbin/policy-rc.d" << 'POLICY'
#!/bin/sh
exit 0
POLICY
chmod 755 "$MOUNT/usr/sbin/policy-rc.d"

# runlevel shim — invoke-rc.d calls /sbin/runlevel to determine the
# current runlevel. Without this, it prints "could not determine current
# runlevel" and may skip starting services during package install.
# Report runlevel 5 (multi-user with networking) which is the standard
# operational state.
cat > "$MOUNT/sbin/runlevel" << 'RUNLEVEL'
#!/bin/sh
echo "N 5"
RUNLEVEL
chmod 755 "$MOUNT/sbin/runlevel"

echo "==> Minimal tier done."
