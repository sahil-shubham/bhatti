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

die() {
    printf "\n${RED}error: %s${RESET}\n" "$1" >&2
    shift
    for line in "$@"; do
        printf "  %s\n" "$line" >&2
    done
    exit 1
}

# ── Error trap ────────────────────────────────────────
# Safety net: if any command fails unexpectedly (set -e), we
# print exactly what failed instead of exiting silently.
# Every KNOWN failure path uses die() with a descriptive message.
# This trap catches everything else — the "should never happen" cases.

_err_trap() {
    local exit_code=$?
    printf "\n${RED}install failed unexpectedly${RESET}\n" >&2
    printf "  line %d: %s\n" "$1" "$BASH_COMMAND" >&2
    printf "  exit code: %d\n" "$exit_code" >&2
    printf "\n  Please report this at:\n" >&2
    printf "  https://github.com/%s/issues\n\n" "$GITHUB_REPO" >&2
}
trap '_err_trap $LINENO' ERR

# Clean up temp files on any exit
_cleanup() { rm -f /tmp/bhatti.tmp; }
trap '_cleanup' EXIT

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

# ── Download helpers ──────────────────────────────────

# download URL DEST — download a file and validate it's non-empty.
# On failure, prints what went wrong with actionable context.
download() {
    local url="$1" dest="$2"
    local http_code

    http_code=$(curl -fsSL -w '%{http_code}' -o "$dest" "$url") || {
        rm -f "$dest"
        die "download failed: $url" \
            "HTTP status: ${http_code:-unknown}" \
            "Check your network connection and try again."
    }

    if [ ! -s "$dest" ]; then
        rm -f "$dest"
        die "download produced an empty file: $url" \
            "This usually means the release asset is missing." \
            "Check: $url"
    fi
}

# download_pipe URL CMD... — stream a download into a command (e.g. tar, zstd).
# Validates that curl succeeds and the pipeline produces output.
download_pipe() {
    local url="$1"; shift

    # Use a temp file for curl errors since we're in a pipeline
    local err_file
    err_file=$(mktemp)

    if ! curl -fsSL "$url" 2>"$err_file" | "$@"; then
        local curl_err
        curl_err=$(cat "$err_file")
        rm -f "$err_file"
        die "download + extract failed: $url" \
            "${curl_err:-pipeline failed}" \
            "Check your network connection and disk space."
    fi
    rm -f "$err_file"
}

# ── Version helpers ───────────────────────────────────

resolve_latest_version() {
    local response
    response=$(curl -fsSL "https://api.github.com/repos/${GITHUB_REPO}/releases/latest") \
        || die "could not reach GitHub API" \
               "Check your network connection." \
               "You can also download directly from:" \
               "  https://github.com/${GITHUB_REPO}/releases"

    VERSION=$(echo "$response" | grep '"tag_name"' | sed 's/.*"tag_name": "\(.*\)".*/\1/') || true
    [ -n "$VERSION" ] || die "could not determine latest release version" \
                              "GitHub API returned unexpected response." \
                              "Try again, or download manually from:" \
                              "  https://github.com/${GITHUB_REPO}/releases"

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

# major_version extracts the major version number from a semver string
major_version() {
    local v="${1#v}"
    echo "${v%%.*}"
}

# crosses_major returns 0 (true) if upgrading from $1 to $2 crosses a major version
crosses_major() {
    local from_major to_major
    from_major=$(major_version "$1")
    to_major=$(major_version "$2")
    [ "$from_major" != "$to_major" ]
}

# ── Installation detection ────────────────────────────

# Returns: none | cli | server
detect_install_type() {
    if [ -f "/etc/bhatti/config.yaml" ]; then
        echo "server"
    elif [ -d "$DATA_DIR" ] && [ -f "$DATA_DIR/config.yaml" ]; then
        # Pre-v1.6 installs kept config in the data dir
        echo "server"
    elif command -v bhatti >/dev/null 2>&1; then
        echo "cli"
    else
        echo "none"
    fi
}

# Detect existing rootfs tier from config.yaml, falling back to filename glob.
# Reading config.yaml is authoritative — it's what the server actually uses.
# The glob fallback handles edge cases (missing config, manual installs).
detect_tier() {
    # Primary: parse firecracker_rootfs from config.yaml
    local config_file="/etc/bhatti/config.yaml"
    [ -f "$config_file" ] || config_file="$DATA_DIR/config.yaml"  # pre-v1.6 fallback
    if [ -f "$config_file" ]; then
        local rootfs_path
        rootfs_path=$(grep '^firecracker_rootfs:' "$config_file" | awk '{print $2}' | tr -d '"' | tr -d "'") || true
        if [ -n "$rootfs_path" ]; then
            local tier
            tier=$(basename "$rootfs_path" | sed "s/rootfs-//;s/-${ARCH}\.ext4//")
            if [ -n "$tier" ]; then
                echo "$tier"
                return 0
            fi
        fi
    fi
    # Fallback: glob (for fresh installs where config doesn't exist yet)
    for f in "$DATA_DIR/images/rootfs-"*"-${ARCH}.ext4"; do
        [ -f "$f" ] || continue
        basename "$f" | sed "s/rootfs-//;s/-${ARCH}\.ext4//"
        return 0
    done
    echo "minimal"
}

# ── Version queries ───────────────────────────────────
# These functions return empty string (not error) when the
# tool isn't installed. They must always exit 0 — a missing
# binary is expected state, not an error.

# Get installed bhatti version (empty if not installed or dev build)
installed_bhatti_version() {
    command -v bhatti >/dev/null 2>&1 || { echo ""; return 0; }
    local ver
    ver=$(bhatti version 2>/dev/null | awk '/^bhatti/{print $2}') || true
    [ "$ver" = "dev" ] && { echo ""; return 0; }
    echo "$ver"
}

# Get installed firecracker version (empty if not installed)
installed_fc_version() {
    command -v firecracker >/dev/null 2>&1 || { echo ""; return 0; }
    firecracker --version 2>&1 | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1 || true
}

# ── Install functions ─────────────────────────────────

install_bhatti_binary() {
    local binary="bhatti-${OS}-${ARCH}"
    download "${RELEASE_URL}/${binary}" /tmp/bhatti.tmp
    chmod +x /tmp/bhatti.tmp

    if [ -w "/usr/local/bin" ]; then
        mv /tmp/bhatti.tmp /usr/local/bin/bhatti
    else
        sudo mv /tmp/bhatti.tmp /usr/local/bin/bhatti
    fi
}

install_firecracker() {
    local installed_fc
    installed_fc=$(installed_fc_version)

    if [ -n "$installed_fc" ] && ! version_gt "$FC_VERSION" "$installed_fc"; then
        # FC is up to date, but jailer might be missing
        if [ -x /usr/local/bin/jailer ]; then
            success "Firecracker ${installed_fc} + jailer (up to date)"
            return 0
        fi
        info "Firecracker ${installed_fc} up to date, installing missing jailer"
    elif [ -n "$installed_fc" ]; then
        heading "Updating Firecracker ${installed_fc} → ${FC_VERSION}"
    else
        heading "Installing Firecracker ${FC_VERSION}"
    fi

    local tmpdir
    tmpdir=$(mktemp -d)

    download_pipe \
        "https://github.com/firecracker-microvm/firecracker/releases/download/v${FC_VERSION}/firecracker-v${FC_VERSION}-${FC_ARCH}.tgz" \
        tar xz -C "$tmpdir"

    local release_dir="$tmpdir/release-v${FC_VERSION}-${FC_ARCH}"

    [ -f "$release_dir/firecracker-v${FC_VERSION}-${FC_ARCH}" ] \
        || die "firecracker binary not found in release archive" \
               "Expected: $release_dir/firecracker-v${FC_VERSION}-${FC_ARCH}" \
               "The release archive may be corrupt. Try again."

    [ -f "$release_dir/jailer-v${FC_VERSION}-${FC_ARCH}" ] \
        || die "jailer binary not found in release archive" \
               "Expected: $release_dir/jailer-v${FC_VERSION}-${FC_ARCH}" \
               "The release archive may be corrupt. Try again."

    mv "$release_dir/firecracker-v${FC_VERSION}-${FC_ARCH}" /usr/local/bin/firecracker
    mv "$release_dir/jailer-v${FC_VERSION}-${FC_ARCH}" /usr/local/bin/jailer
    chmod +x /usr/local/bin/firecracker /usr/local/bin/jailer
    rm -rf "$tmpdir"
    success "Firecracker ${FC_VERSION} + jailer"
}

install_lohar() {
    heading "Installing lohar"
    download "${RELEASE_URL}/lohar-linux-${ARCH}" "$DATA_DIR/lohar"
    chmod +x "$DATA_DIR/lohar"
    success "lohar ($(du -h "$DATA_DIR/lohar" | cut -f1))"
}

install_kernel() {
    heading "Installing kernel"
    local kernel_path="$DATA_DIR/images/vmlinux-${ARCH}"
    download "${RELEASE_URL}/vmlinux-${KERNEL_VERSION}-${FC_ARCH}" "$kernel_path"
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
            apt-get update -qq && apt-get install -y -qq zstd >/dev/null || true
        elif command -v dnf >/dev/null 2>&1; then
            dnf install -y -q zstd >/dev/null || true
        elif command -v yum >/dev/null 2>&1; then
            yum install -y -q zstd >/dev/null || true
        fi

        # Verify it actually installed — catches permission errors, broken repos, etc.
        command -v zstd >/dev/null 2>&1 \
            || die "failed to install zstd" \
                   "Try installing it manually and re-run:" \
                   "  sudo apt-get install zstd   # Debian/Ubuntu" \
                   "  sudo dnf install zstd       # Fedora/RHEL" \
                   "  sudo yum install zstd       # CentOS"
    fi

    download_pipe \
        "${RELEASE_URL}/rootfs-${tier}-${ARCH}.ext4.zst" \
        zstd -d -f -o "$rootfs_path"

    [ -s "$rootfs_path" ] \
        || die "rootfs decompression produced an empty file" \
               "This may indicate insufficient disk space." \
               "Available space: $(df -h "$DATA_DIR" 2>/dev/null | tail -1 | awk '{print $4}')"

    success "rootfs ${tier} ($(du -h "$rootfs_path" | cut -f1))"
}

setup_jail_user() {
    if ! id -u bhatti-vm >/dev/null 2>&1; then
        useradd -r -s /usr/sbin/nologin -u 10000 bhatti-vm 2>/dev/null || true
        success "Created bhatti-vm user (uid 10000)"
    fi
}

generate_config() {
    local tier="$1"
    mkdir -p /etc/bhatti
    cat > /etc/bhatti/config.yaml << EOF
engine: firecracker
listen: :8080
data_dir: ${DATA_DIR}
firecracker_bin: /usr/local/bin/firecracker
firecracker_jailer: /usr/local/bin/jailer
jail_uid: 10000
jail_gid: 10000
firecracker_kernel: ${DATA_DIR}/images/vmlinux-${ARCH}
firecracker_rootfs: ${DATA_DIR}/images/rootfs-${tier}-${ARCH}.ext4
EOF

    # Clean up pre-v1.6 config location
    if [ -f "$DATA_DIR/config.yaml" ]; then
        rm -f "$DATA_DIR/config.yaml"
        info "Migrated config to /etc/bhatti/config.yaml"
    fi
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
api_url: http://localhost:8080
auth_token: ${ADMIN_KEY}
EOF
                chown -R "$SUDO_USER:$user_group" "$user_home/.bhatti"
            fi
        fi

        mkdir -p /root/.bhatti
        cat > /root/.bhatti/config.yaml << EOF
api_url: http://localhost:8080
auth_token: ${ADMIN_KEY}
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
        return 0
    fi

    if [ -n "$current" ]; then
        # Guard against major version crossings for CLI updates too
        if crosses_major "$current" "$VERSION"; then
            echo ""
            printf "  ${RED}⚠  Major version upgrade: ${current} → ${VERSION}${RESET}\n"
            echo "  Review release notes: https://github.com/${GITHUB_REPO}/releases/tag/${VERSION}"
            echo ""
            if [ "${BHATTI_FORCE:-}" != "1" ]; then
                printf "  Continue? [y/N]: "
                read -r confirm < /dev/tty 2>/dev/null || confirm="n"
                case "$confirm" in
                    y|Y|yes|YES) ;;
                    *) echo "  Aborted."; exit 0 ;;
                esac
            fi
        fi
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

    [ "$(id -u)" -eq 0 ] || die "server installation requires root" \
                                "Re-run with:" \
                                "  curl -fsSL bhatti.sh/install | sudo bash"

    # Preflight
    if [ ! -e /dev/kvm ]; then
        modprobe kvm 2>/dev/null || true
    fi
    [ -e /dev/kvm ] || die "/dev/kvm not available — KVM is required" \
                           "Enable virtualization in your BIOS/hypervisor settings," \
                           "or use a VM with nested virtualization enabled."
    command -v curl >/dev/null 2>&1 || die "curl is required" \
                                          "Install it with: apt-get install curl"

    heading "Installing bhatti ${VERSION} (server, ${tier} tier) on $(hostname) (${HOST_ARCH})"

    mkdir -p "$DATA_DIR"/{images,sandboxes,volumes,snapshots}

    install_firecracker

    heading "Installing bhatti ${VERSION}"
    install_bhatti_binary
    success "bhatti ${VERSION}"

    install_lohar
    install_kernel
    install_rootfs "$tier"
    setup_jail_user
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
    [ "$(id -u)" -eq 0 ] || die "server update requires root" \
                                "Re-run with:" \
                                "  curl -fsSL bhatti.sh/install | sudo bash"

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
        return 0
    fi

    # Guard against major version crossings (e.g. v0.5.x → v1.0.0).
    # Major version bumps may include breaking changes (snapshot format,
    # config schema, etc.) that require manual migration steps.
    if [ -n "$current" ] && crosses_major "$current" "$VERSION"; then
        echo ""
        printf "  ${RED}⚠  Major version upgrade: ${current} → ${VERSION}${RESET}\n"
        echo "  This may include breaking changes. Review the release notes:"
        echo "    https://github.com/${GITHUB_REPO}/releases/tag/${VERSION}"
        echo ""
        if [ "${BHATTI_FORCE:-}" = "1" ]; then
            info "BHATTI_FORCE=1 set, proceeding"
        else
            printf "  Continue? [y/N]: "
            read -r confirm < /dev/tty 2>/dev/null || confirm="n"
            case "$confirm" in
                y|Y|yes|YES) ;;
                *) echo "  Aborted."; exit 0 ;;
            esac
        fi
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
    setup_jail_user
    write_systemd_unit

    # Migrate config from old location if needed (pre-v1.6)
    if [ -f "$DATA_DIR/config.yaml" ] && [ ! -f "/etc/bhatti/config.yaml" ]; then
        mkdir -p /etc/bhatti
        mv "$DATA_DIR/config.yaml" /etc/bhatti/config.yaml
        info "Migrated config to /etc/bhatti/config.yaml"
    fi
    # admin user is PRESERVED

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
