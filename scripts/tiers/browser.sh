#!/bin/bash
# Browser tier: minimal + headless Chromium + Playwright.
# Sources minimal.sh first.
# Called by build-tier.sh with $MOUNT, $ARCH, $DEB_ARCH, $AGENT, $SCRIPT_DIR set.
set -euo pipefail

PLAYWRIGHT_VERSION="1.50.0"
NODE_VERSION="22.16.0"

# Build minimal base first
"$SCRIPT_DIR/tiers/minimal.sh"

echo "==> Installing browser tier packages..."
chroot "$MOUNT" /bin/bash -c "
set -eu
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq

# Node.js (for Playwright CLI)
case \$(dpkg --print-architecture) in
    amd64) NODE_ARCH=x64 ;;
    arm64) NODE_ARCH=arm64 ;;
esac
echo '==> Installing Node.js ${NODE_VERSION}...'
curl -fsSL \"https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-\${NODE_ARCH}.tar.xz\" \\
    | tar -xJ --strip-components=1 -C /usr/local

# Python 3 + Playwright (pinned version)
echo '==> Installing Python 3 + Playwright ${PLAYWRIGHT_VERSION}...'
apt-get install -y --no-install-recommends python3 python3-pip
pip3 install --break-system-packages playwright==${PLAYWRIGHT_VERSION}
npm install -g playwright@${PLAYWRIGHT_VERSION}

# Install Playwright's bundled Chromium + its system dependencies.
# This avoids Ubuntu's chromium-browser package which is a snap redirect
# in noble and broken in minbase chroots without snapd.
echo '==> Installing Playwright bundled Chromium...'
npx playwright install chromium
npx playwright install-deps chromium

apt-get clean
rm -rf /var/lib/apt/lists/* /tmp/*
"

# Boot profile: start Chromium with CDP + readiness wait
mkdir -p "$MOUNT/etc/bhatti"
cat > "$MOUNT/etc/bhatti/init.sh" << 'PROFILE'
#!/bin/sh
# Resolve Playwright's bundled Chromium path.
# Use a specific glob to avoid matching test binaries or other files.
CHROMIUM=$(find /root/.cache/ms-playwright -path '*/chrome-linux/chrome' -type f 2>/dev/null | head -1)

if [ -z "$CHROMIUM" ]; then
    echo "bhatti: chromium not found in /root/.cache/ms-playwright" >&2
    exit 1
fi

"$CHROMIUM" \
    --headless \
    --no-sandbox \
    --disable-gpu \
    --remote-debugging-port=9222 \
    --remote-debugging-address=0.0.0.0 &

# Wait for CDP to accept connections (up to 5 seconds)
for i in $(seq 1 50); do
    curl -sf http://localhost:9222/json/version >/dev/null 2>&1 && break
    sleep 0.1
done

if ! curl -sf http://localhost:9222/json/version >/dev/null 2>&1; then
    echo "bhatti: chromium CDP not ready after 5s" >&2
fi
PROFILE
chmod 755 "$MOUNT/etc/bhatti/init.sh"

echo "==> Browser tier done."
