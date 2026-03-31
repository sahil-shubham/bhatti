#!/bin/bash
# scripts/install.sh — Unified bhatti installer.
#
# Detects platform and existing installation to do the right thing:
#   macOS            → install/update CLI binary
#   Linux (fresh)    → prompt: CLI or self-hosted server
#   Linux (CLI)      → update CLI binary
#   Linux (server)   → update all server components
#
# Usage:
#   curl -fsSL bhatti.sh/install | bash              # CLI or prompted
#   curl -fsSL bhatti.sh/install | sudo bash          # server (prompted for tier)
#
# Environment variables (for CI / non-interactive use):
#   BHATTI_MODE=cli|server     — skip install type prompt
#   BHATTI_TIER=minimal|browser|docker — skip tier prompt (server only)
set -euo pipefail

GITHUB_REPO="sahil-shubham/bhatti"
FC_VERSION="1.14.0"
KERNEL_VERSION="6.1.155"
DATA_DIR="/var/lib/bhatti"

# ── Formatting ────────────────────────────────────────

BOLD='\033[1m'
DIM='\033[2m'
GREEN='\033[32m'
RED='\033[31m'
RESET='\033[0m'

info()    { printf "  ${DIM}%s${RESET}\n" "$*"; }
heading() { printf "\n${BOLD}==> %s${RESET}\n" "$*"; }
success() { printf "  ${GREEN}✓${RESET} %s\n" "$*"; }
die()     { printf "${RED}error: %s${RESET}\n" "$*" >&2; exit 1; }

# ── Platform detection ────────────────────────────────

detect_platform() {
    OS=$(uname -s | tr '[:upper:]' '[:lower:]')
    HOST_ARCH=$(uname -m)

    case "$HOST_ARCH" in
        x86_64)        ARCH="amd64"; FC_ARCH="x86_64" ;;
        aarch64|arm64) ARCH="arm64"; FC_ARCH="aarch64" ;;
        *) die "unsupported architecture: $HOST_ARCH" ;;
    esac
}

# ── Version helpers ───────────────────────────────────

resolve_latest_version() {
    VERSION=$(curl -fsSL "https://api.github.com/repos/${GITHUB_REPO}/releases/latest" \
        | grep '"tag_name"' | sed 's/.*"tag_name": "\(.*\)".*/\1/')
    [ -n "$VERSION" ] || die "could not resolve latest release version"
    RELEASE_URL="https://github.com/${GITHUB_REPO}/releases/download/${VERSION}"
}

# version_gt returns 0 (true) if $1 > $2 (semver, optional v prefix)
version_gt() {
    local a="${1#v}" b="${2#v}"
    local a1 a2 a3 b1 b2 b3
    IFS=. read -r a1 a2 a3 <<< "$a"
    IFS=. read -r b1 b2 b3 <<< "$b"
    a1=${a1:-0}; a2=${a2:-0}; a3=${a3:-0}
    b1=${b1:-0}; b2=${b2:-0}; b3=${b3:-0}

    # Use != guard then direct comparison to avoid set -e issues
    # with arithmetic expressions that evaluate to 0 (false).
    if (( a1 != b1 )); then (( a1 > b1 )); return; fi
    if (( a2 != b2 )); then (( a2 > b2 )); return; fi
    if (( a3 != b3 )); then (( a3 > b3 )); return; fi
    return 1
}

# ── Installation detection ────────────────────────────

# Returns: none | cli | server
detect_install_type() {
    if [ -d "$DATA_DIR" ] && [ -f "$DATA_DIR/config.yaml" ]; then
        echo "server"
    elif command -v bhatti >/dev/null 2>&1; then
        echo "cli"
    else
        echo "none"
    fi
}

# Detect existing rootfs tier from filename
detect_tier() {
    for f in "$DATA_DIR/images/rootfs-"*"-${ARCH}.ext4"; do
        [ -f "$f" ] || continue
        basename "$f" | sed "s/rootfs-//;s/-${ARCH}\.ext4//"
        return
    done
    echo "minimal"
}

# Get installed bhatti version (empty if not installed or dev build)
installed_bhatti_version() {
    command -v bhatti >/dev/null 2>&1 || return
    local ver
    ver=$(bhatti version 2>/dev/null | awk '/^bhatti/{print $2}') || true
    [ "$ver" = "dev" ] && return
    echo "$ver"
}

# Get installed firecracker version (empty if not installed)
installed_fc_version() {
    command -v firecracker >/dev/null 2>&1 || return
    firecracker --version 2>&1 | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1 || true
}

# ── Install functions ─────────────────────────────────

install_bhatti_binary() {
    local binary="bhatti-${OS}-${ARCH}"
    curl -fsSL "${RELEASE_URL}/${binary}" -o /tmp/bhatti
    chmod +x /tmp/bhatti

    if [ -w "/usr/local/bin" ]; then
        mv /tmp/bhatti /usr/local/bin/bhatti
    else
        sudo mv /tmp/bhatti /usr/local/bin/bhatti
    fi
}

install_firecracker() {
    local installed_fc
    installed_fc=$(installed_fc_version)

    if [ -n "$installed_fc" ] && ! version_gt "$FC_VERSION" "$installed_fc"; then
        success "Firecracker ${installed_fc} (up to date)"
        return
    fi

    if [ -n "$installed_fc" ]; then
        heading "Updating Firecracker ${installed_fc} → ${FC_VERSION}"
    else
        heading "Installing Firecracker ${FC_VERSION}"
    fi

    local tmpdir
    tmpdir=$(mktemp -d)
    curl -fsSL \
        "https://github.com/firecracker-microvm/firecracker/releases/download/v${FC_VERSION}/firecracker-v${FC_VERSION}-${FC_ARCH}.tgz" \
        | tar xz -C "$tmpdir"
    mv "$tmpdir/release-v${FC_VERSION}-${FC_ARCH}/firecracker-v${FC_VERSION}-${FC_ARCH}" \
        /usr/local/bin/firecracker
    chmod +x /usr/local/bin/firecracker
    rm -rf "$tmpdir"
    success "Firecracker ${FC_VERSION}"
}

install_lohar() {
    heading "Installing lohar"
    curl -fsSL "${RELEASE_URL}/lohar-linux-${ARCH}" -o "$DATA_DIR/lohar"
    chmod +x "$DATA_DIR/lohar"
    success "lohar ($(du -h "$DATA_DIR/lohar" | cut -f1))"
}

install_kernel() {
    heading "Installing kernel"
    local kernel_path="$DATA_DIR/images/vmlinux-${ARCH}"
    curl -fsSL "${RELEASE_URL}/vmlinux-${KERNEL_VERSION}-${FC_ARCH}" -o "$kernel_path"
    success "kernel ($(du -h "$kernel_path" | cut -f1))"
}

install_rootfs() {
    local tier="$1"
    local rootfs_path="$DATA_DIR/images/rootfs-${tier}-${ARCH}.ext4"

    heading "Installing ${tier} rootfs"

    # Install zstd if needed
    if ! command -v zstd >/dev/null 2>&1; then
        info "Installing zstd..."
        if command -v apt-get >/dev/null 2>&1; then
            apt-get update -qq && apt-get install -y -qq zstd >/dev/null
        elif command -v dnf >/dev/null 2>&1; then
            dnf install -y -q zstd >/dev/null
        elif command -v yum >/dev/null 2>&1; then
            yum install -y -q zstd >/dev/null
        else
            die "zstd is required but not installed — install it manually and re-run"
        fi
    fi

    curl -fsSL "${RELEASE_URL}/rootfs-${tier}-${ARCH}.ext4.zst" \
        | zstd -d -f -o "$rootfs_path"
    success "rootfs ${tier} ($(du -h "$rootfs_path" | cut -f1))"
}

generate_config() {
    local tier="$1"
    cat > "$DATA_DIR/config.yaml" << EOF
engine: firecracker
listen: :8080
data_dir: ${DATA_DIR}
firecracker_bin: /usr/local/bin/firecracker
firecracker_kernel: ${DATA_DIR}/images/vmlinux-${ARCH}
firecracker_rootfs: ${DATA_DIR}/images/rootfs-${tier}-${ARCH}.ext4
EOF
}

create_admin_user() {
    heading "Creating admin user"
    ADMIN_KEY=$(bhatti user create --name admin --max-sandboxes 50 2>&1 \
        | grep "API key:" | awk '{print $NF}') || true

    if [ -n "${ADMIN_KEY:-}" ]; then
        # Write CLI config for the user who ran sudo
        if [ -n "${SUDO_USER:-}" ] && [ "$SUDO_USER" != "root" ]; then
            local user_home user_group
            user_home=$(getent passwd "$SUDO_USER" 2>/dev/null | cut -d: -f6) || true
            user_group=$(id -gn "$SUDO_USER" 2>/dev/null) || user_group="$SUDO_USER"
            [ -z "$user_home" ] && user_home=$(eval echo "~$SUDO_USER" 2>/dev/null) || true

            if [ -n "$user_home" ] && [ -d "$user_home" ]; then
                mkdir -p "$user_home/.bhatti"
                cat > "$user_home/.bhatti/config.yaml" << EOF
auth_token: ${ADMIN_KEY}
listen: :8080
EOF
                chown -R "$SUDO_USER:$user_group" "$user_home/.bhatti"
            fi
        fi

        mkdir -p /root/.bhatti
        cat > /root/.bhatti/config.yaml << EOF
auth_token: ${ADMIN_KEY}
listen: :8080
EOF
        success "Admin user created"
    else
        info "Admin user may already exist, skipping"
    fi
}

write_systemd_unit() {
    cat > "$DATA_DIR/bhatti.service" << 'UNIT'
[Unit]
Description=Bhatti Sandbox Infrastructure
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/bhatti serve
WorkingDirectory=/var/lib/bhatti
Environment=HOME=/root
Restart=always
RestartSec=5
KillMode=process
TimeoutStopSec=120
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
UNIT
}

# ── Top-level flows ───────────────────────────────────

do_cli_install() {
    local current
    current=$(installed_bhatti_version)

    if [ -n "$current" ] && [ "v${current#v}" = "${VERSION}" ]; then
        success "bhatti ${VERSION} is already installed"
        return
    fi

    if [ -n "$current" ]; then
        heading "Updating bhatti ${current} → ${VERSION}"
    else
        heading "Installing bhatti ${VERSION} (${OS}/${ARCH})"
    fi

    install_bhatti_binary

    echo ""
    success "bhatti ${VERSION} → /usr/local/bin/bhatti"
    if [ -z "$current" ]; then
        echo ""
        echo "  Quick start:"
        echo "    bhatti setup     # configure API endpoint + key"
    fi
}

do_server_install() {
    local tier="${1:-minimal}"

    [ "$(id -u)" -eq 0 ] || die "server installation requires root — re-run with:\n  curl -fsSL bhatti.sh/install | sudo bash"

    # Preflight
    if [ ! -e /dev/kvm ]; then
        modprobe kvm 2>/dev/null || true
    fi
    [ -e /dev/kvm ] || die "/dev/kvm not available — KVM is required"
    command -v curl >/dev/null 2>&1 || die "curl is required"

    heading "Installing bhatti ${VERSION} (server, ${tier} tier) on $(hostname) (${HOST_ARCH})"

    mkdir -p "$DATA_DIR"/{images,sandboxes,volumes,snapshots}

    install_firecracker

    heading "Installing bhatti ${VERSION}"
    install_bhatti_binary
    success "bhatti ${VERSION}"

    install_lohar
    install_kernel
    install_rootfs "$tier"
    generate_config "$tier"
    create_admin_user
    write_systemd_unit

    echo ""
    echo "============================================"
    echo "  bhatti ${VERSION} installed"
    echo "  tier: ${tier}"
    echo ""
    if [ -n "${ADMIN_KEY:-}" ]; then
        echo "  Admin API key: ${ADMIN_KEY}"
        echo "  (saved to ~/.bhatti/config.yaml)"
        echo ""
    fi
    echo "  Start the daemon:"
    echo "    cd $DATA_DIR && sudo bhatti serve"
    echo ""
    echo "  Run as a systemd service:"
    echo "    sudo cp $DATA_DIR/bhatti.service /etc/systemd/system/"
    echo "    sudo systemctl enable --now bhatti"
    echo ""
    echo "  Manage users:"
    echo "    sudo bhatti user create --name alice"
    echo "    sudo bhatti user list"
    echo ""
    echo "  ⚠  BACK UP: $DATA_DIR/age.key"
    echo "     If lost, all encrypted secrets become unrecoverable."
    echo "============================================"
}

do_server_update() {
    [ "$(id -u)" -eq 0 ] || die "server update requires root — re-run with:\n  curl -fsSL bhatti.sh/install | sudo bash"

    local tier current
    tier=$(detect_tier)
    current=$(installed_bhatti_version)

    # Check if everything is already up to date
    local all_present=true
    [ -f "/usr/local/bin/bhatti" ]                          || all_present=false
    [ -f "/usr/local/bin/firecracker" ]                     || all_present=false
    [ -f "$DATA_DIR/lohar" ]                                || all_present=false
    [ -f "$DATA_DIR/images/vmlinux-${ARCH}" ]               || all_present=false
    [ -f "$DATA_DIR/images/rootfs-${tier}-${ARCH}.ext4" ]   || all_present=false

    if [ -n "$current" ] && [ "v${current#v}" = "${VERSION}" ] && [ "$all_present" = true ]; then
        success "bhatti ${VERSION} (server, ${tier} tier) is already up to date"
        return
    fi

    heading "Updating bhatti server (${tier} tier)"
    if [ -n "$current" ]; then
        info "${current} → ${VERSION}"
    fi

    # Stop systemd service if running (restart after update)
    local was_running=false
    if command -v systemctl >/dev/null 2>&1 && systemctl is-active bhatti >/dev/null 2>&1; then
        was_running=true
        heading "Stopping bhatti service"
        systemctl stop bhatti
    fi

    install_firecracker

    heading "Installing bhatti ${VERSION}"
    install_bhatti_binary
    success "bhatti ${VERSION}"

    install_lohar
    install_kernel
    install_rootfs "$tier"
    write_systemd_unit
    # config.yaml and admin user are PRESERVED

    if [ "$was_running" = true ]; then
        heading "Restarting bhatti service"
        cp "$DATA_DIR/bhatti.service" /etc/systemd/system/bhatti.service
        systemctl daemon-reload
        systemctl start bhatti
    fi

    echo ""
    echo "============================================"
    echo "  bhatti updated to ${VERSION} (${tier} tier)"
    if [ "$was_running" = true ]; then
        echo "  systemd service: restarted"
    else
        echo ""
        echo "  Restart the daemon to use the new version."
    fi
    echo "============================================"
}

# ── Main ──────────────────────────────────────────────

main() {
    detect_platform
    resolve_latest_version

    local install_type
    install_type=$(detect_install_type)

    case "$OS" in
        darwin)
            do_cli_install
            ;;
        linux)
            case "$install_type" in
                server)
                    do_server_update
                    ;;
                cli)
                    do_cli_install
                    ;;
                none)
                    local mode="${BHATTI_MODE:-}"

                    if [ -z "$mode" ]; then
                        echo ""
                        echo "  Install bhatti as:"
                        echo "    1) CLI — connect to a remote bhatti server"
                        echo "    2) Self-host — run bhatti on this machine (requires root + KVM)"
                        echo ""
                        printf "  Choice [1]: "
                        read -r mode_choice < /dev/tty 2>/dev/null || mode_choice="1"
                        case "${mode_choice:-1}" in
                            2) mode="server" ;;
                            *) mode="cli" ;;
                        esac
                    fi

                    case "$mode" in
                        server)
                            local tier="${BHATTI_TIER:-}"

                            if [ -z "$tier" ]; then
                                echo ""
                                echo "  Rootfs tier:"
                                echo "    1) minimal — bare Ubuntu (~200MB)"
                                echo "    2) browser — + Chromium/Playwright (~600MB)"
                                echo "    3) docker  — + Docker Engine (~550MB)"
                                echo ""
                                printf "  Choice [1]: "
                                read -r tier_choice < /dev/tty 2>/dev/null || tier_choice="1"
                                case "${tier_choice:-1}" in
                                    2) tier="browser" ;;
                                    3) tier="docker" ;;
                                    *) tier="minimal" ;;
                                esac
                            fi

                            do_server_install "$tier"
                            ;;
                        *)
                            do_cli_install
                            ;;
                    esac
                    ;;
            esac
            ;;
        *)
            die "unsupported OS: $OS"
            ;;
    esac
}

main
