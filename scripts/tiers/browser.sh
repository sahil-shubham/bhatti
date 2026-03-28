#!/bin/bash
# Browser tier: minimal + headless Chromium + Playwright.
# Sources minimal.sh first.
# Called by build-tier.sh with $MOUNT, $ARCH, $DEB_ARCH, $AGENT, $SCRIPT_DIR set.
#
# Node.js only — no Python runtime. Users who want Python Playwright
# can `pip install playwright` after boot. The rootfs ships the smallest
# path: Node.js (already needed for npx) + npx playwright install.
set -euo pipefail

PLAYWRIGHT_VERSION="1.50.0"
NODE_VERSION="22.16.0"

# Build minimal base first
"$SCRIPT_DIR/tiers/minimal.sh"

echo "==> Installing browser tier packages..."
chroot "$MOUNT" /bin/bash -c "
set -eu
export DEBIAN_FRONTEND=noninteractive
# Enable universe repo (needed by playwright install-deps for fonts, xvfb).
# debootstrap creates /etc/apt/sources.list with the correct mirror for the
# target arch. Don't replace it — just append universe to the existing line.
sed -i 's/ main$/ main universe/' /etc/apt/sources.list
apt-get update -qq

# xz-utils needed to decompress Node.js tarball (.tar.xz)
apt-get install -y --no-install-recommends xz-utils

# Node.js
case \$(dpkg --print-architecture) in
    amd64) NODE_ARCH=x64 ;;
    arm64) NODE_ARCH=arm64 ;;
esac
echo '==> Installing Node.js ${NODE_VERSION}...'
curl -fsSL \"https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-\${NODE_ARCH}.tar.xz\" \\
    | tar -xJ --strip-components=1 -C /usr/local

# Playwright (pinned version) — installs the JS API + CLI
echo '==> Installing Playwright ${PLAYWRIGHT_VERSION}...'
npm install -g playwright@${PLAYWRIGHT_VERSION}

# Install Playwright's headless shell + system deps.
# We use headless_shell (not the full chromium binary) because Chrome 133+
# new headless mode has broken CDP: commands sent to page sessions via
# Target.attachToTarget get no response. headless_shell is the dedicated
# headless binary that Playwright itself uses for launch().
echo '==> Installing Playwright headless shell + deps...'
npx playwright install chromium
npx playwright install-deps chromium

apt-get clean
rm -rf /var/lib/apt/lists/* /tmp/*
"

# Boot profile: start headless_shell with CDP + readiness wait
mkdir -p "$MOUNT/etc/bhatti"
cat > "$MOUNT/etc/bhatti/init.sh" << 'PROFILE'
#!/bin/sh
# Use Playwright's headless_shell binary (not the full chrome binary).
# headless_shell is the dedicated headless implementation with working CDP.
HEADLESS_SHELL=$(find /root/.cache/ms-playwright -path '*/chrome-linux/headless_shell' -type f 2>/dev/null | head -1)

if [ -z "$HEADLESS_SHELL" ]; then
    echo "bhatti: headless_shell not found in /root/.cache/ms-playwright" >&2
    exit 1
fi

"$HEADLESS_SHELL" \
    --no-sandbox \
    --disable-gpu \
    --disable-dev-shm-usage \
    --remote-debugging-port=9222 \
    --remote-debugging-address=0.0.0.0 &

# Wait for CDP to accept connections (up to 5 seconds)
for i in $(seq 1 50); do
    curl -sf http://127.0.0.1:9222/json/version >/dev/null 2>&1 && break
    sleep 0.1
done

if ! curl -sf http://127.0.0.1:9222/json/version >/dev/null 2>&1; then
    echo "bhatti: headless_shell CDP not ready after 5s" >&2
fi
PROFILE
chmod 755 "$MOUNT/etc/bhatti/init.sh"

echo "==> Browser tier done."
