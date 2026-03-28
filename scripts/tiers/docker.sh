#!/bin/bash
# Docker tier: minimal + Docker Engine.
# Sources minimal.sh first.
# Called by build-tier.sh with $MOUNT, $ARCH, $DEB_ARCH, $AGENT, $SCRIPT_DIR set.
#
# Kernel requirement: custom bhatti kernel with CONFIG_IP_NF_RAW=y,
# CONFIG_BRIDGE=y, CONFIG_VETH=y, CONFIG_OVERLAY_FS=y, CONFIG_NF_CONNTRACK=y.
# Without these, Docker's bridge networking won't work.
set -euo pipefail

# Build minimal base first
"$SCRIPT_DIR/tiers/minimal.sh"

echo "==> Installing docker tier packages..."
chroot "$MOUNT" /bin/bash -c '
set -eu
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq

# Docker prerequisites
apt-get install -y --no-install-recommends \
    ca-certificates curl gnupg iptables

# Docker repo
curl -fsSL https://download.docker.com/linux/ubuntu/gpg \
    | gpg --dearmor -o /usr/share/keyrings/docker-archive-keyring.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/docker-archive-keyring.gpg] \
    https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo $VERSION_CODENAME) stable" \
    > /etc/apt/sources.list.d/docker.list

apt-get update -qq
apt-get install -y --no-install-recommends docker-ce docker-ce-cli containerd.io

# Configure iptables-legacy (kernel has legacy iptables, not nftables)
update-alternatives --set iptables /usr/sbin/iptables-legacy
update-alternatives --set ip6tables /usr/sbin/ip6tables-legacy

# Add lohar user to docker group (standard Docker access control)
usermod -aG docker lohar

apt-get clean
rm -rf /var/lib/apt/lists/*
'

# Boot profile: start dockerd with readiness check + timeout
mkdir -p "$MOUNT/etc/bhatti"
cat > "$MOUNT/etc/bhatti/init.sh" << 'PROFILE'
#!/bin/sh
# iptables-legacy (kernel has legacy iptables, not nftables)
update-alternatives --set iptables /usr/sbin/iptables-legacy 2>/dev/null
update-alternatives --set ip6tables /usr/sbin/ip6tables-legacy 2>/dev/null

# Start Docker daemon
dockerd > /var/log/dockerd.log 2>&1 &

# Wait for socket (up to 10 seconds, then give up — non-fatal)
for i in $(seq 1 100); do
    [ -S /var/run/docker.sock ] && break
    sleep 0.1
done

if [ ! -S /var/run/docker.sock ]; then
    echo "bhatti: dockerd failed to start within 10s, check /var/log/dockerd.log" >&2
else
    # Allow non-root access. lohar exec runs as uid 1000 without
    # supplementary groups, so docker group membership doesn't work.
    # This is safe — the VM is an isolated single-user sandbox.
    chmod 666 /var/run/docker.sock
fi
PROFILE
chmod 755 "$MOUNT/etc/bhatti/init.sh"

echo "==> Docker tier done."
