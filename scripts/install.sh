#!/bin/bash
# scripts/install.sh — Unified bhatti installer.
#
# Detects platform and existing installation to do the right thing:
#   Linux / macOS (fresh)  → prompt: CLI or self-hosted server
#   CLI install            → update the CLI binary
#   server install         → update all server components
# Both Linux (KVM) and macOS (Apple Silicon/HVF) can run the full self-hosted
# stack; the only per-OS differences are the hypervisor preflight and the
# service manager (systemd on Linux, launchd on macOS).
#
# Usage:
#   curl -fsSL bhatti.sh/install | bash              # CLI or prompted
#   curl -fsSL bhatti.sh/install | sudo bash          # server (prompted for tier)
#
# Environment variables (for CI / non-interactive use):
#   BHATTI_MODE=cli|server     — skip install type prompt
#   BHATTI_TIER=minimal|browser|docker|computer — skip tier prompt (server only)
#   BHATTI_TIERS=all|tier1,tier2,...  — install additional tiers on update (server only)
#   BHATTI_VERSION=v2.x.y      — install/update to this exact tag instead of
#                                latest. Useful for validating an `-rc` prerelease
#                                before promoting it (the `releases/latest` API call
#                                that the script normally uses skips prereleases).
#                                The tag must exist as a public release/prerelease;
#                                draft releases require auth and are not reachable
#                                via the plain HTTPS asset URLs this script uses.
#   BHATTI_ALLOW_MAJOR_CUTOVER=1 — allow installing v2 over a v1 (Firecracker)
#                                server. v2 is a different VMM (krucible), so this
#                                is a fresh install, NOT an in-place upgrade: v1
#                                data/snapshots are not migrated. Refused by default.
#
# Platforms: Linux (KVM) and macOS (Apple Silicon, HVF) both install either the
# CLI or a full self-hosted server from one self-contained bundle (bhatti-vmm +
# bhatti-netd + libkrun + lean kernel). To use v1 (Firecracker), see the pinned
# install line at https://bhatti.sh/v1/docs/quickstart/.

# Skip script-mode hardening (set -e, traps) when sourced by the bats
# test suite — those flags clobber bats' own ERR trap and result
# tracking, causing failed assertions to be reported as "missing" tests
# instead of "not ok". When BHATTI_TEST=1, this file is purely a
# function library; the smoke test runs the script as a real process,
# where these protections DO apply.
if [ "${BHATTI_TEST:-}" != "1" ]; then
    set -euo pipefail
fi

GITHUB_REPO="sahil-shubham/bhatti"
DATA_DIR="/var/lib/bhatti"
# Order matters: drives the order in user-facing hints ("outdated on disk:
# computer, browser" follows ALL_KNOWN_TIERS order, not insertion order).
ALL_KNOWN_TIERS="minimal browser docker computer"

# ── Test overrides ──────────────────────────────────
# Set by scripts/install_smoke.bats to point the installer at a local
# fake-release tree instead of GitHub. Not user-facing; intentionally
# undocumented in --help. The smoke test is the keystone of "if CI
# passes, install actually works" — these are how it does its job.
#   BHATTI_TEST_VERSION       — skip the GitHub API call, use this version
#   BHATTI_TEST_RELEASE_URL   — base URL for asset downloads (file:// or http://)
#   BHATTI_TEST_BIN_DEST      — override /usr/local/bin/bhatti install path

# ── Formatting ────────────────────────────────────────

# ANSI-C quoting ($'\033...') so $RED is a real ESC byte sequence,
# letting us pass it through %s in printf instead of embedding it in
# format strings (which trips SC2059 and is genuinely unsafe if a value
# ever contains a percent sign).
BOLD=$'\033[1m'
DIM=$'\033[2m'
GREEN=$'\033[32m'
RED=$'\033[31m'
RESET=$'\033[0m'

info()    { [ "${QUIET:-}" = "1" ] && return; printf '  %s%s%s\n' "$DIM" "$*" "$RESET"; }
heading() { [ "${QUIET:-}" = "1" ] && return; printf '\n%s==> %s%s\n' "$BOLD" "$*" "$RESET"; }
success() { [ "${QUIET:-}" = "1" ] && return; printf '  %s✓%s %s\n' "$GREEN" "$RESET" "$*"; }

# ── Timing ────────────────────────────────────────────

_step_start=0
_total_start=0
step_start() { _step_start=$SECONDS; }
step_elapsed() {
    local dt=$(( SECONDS - _step_start ))
    if [ "$dt" -eq 0 ]; then echo "<1s"; else echo "${dt}s"; fi
}

die() {
    printf '\n%serror: %s%s\n' "$RED" "$1" "$RESET" >&2
    shift
    for line in "$@"; do
        printf '  %s\n' "$line" >&2
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
    printf '\n%sinstall failed unexpectedly%s\n' "$RED" "$RESET" >&2
    printf '  line %d: %s\n' "$1" "$BASH_COMMAND" >&2
    printf '  exit code: %d\n' "$exit_code" >&2
    printf '\n  Please report this at:\n' >&2
    printf '  https://github.com/%s/issues\n\n' "$GITHUB_REPO" >&2
}
# Clean up temp files on any exit (including staged downloads)
_cleanup() {
    rm -f /tmp/bhatti.tmp
    rm -f "${BHATTI_STAGE_FILE:-}" 2>/dev/null || true
    rm -f "$DATA_DIR"/images/*.zst.tmp 2>/dev/null || true
    if [ -n "${SUDO_KEEPALIVE_PID:-}" ]; then
        kill "$SUDO_KEEPALIVE_PID" 2>/dev/null || true
    fi
}

# Same reasoning as set -euo pipefail above: don't install ERR/EXIT
# traps when sourced by tests — they fight with bats' bats_error_trap
# and obscure real test failures.
if [ "${BHATTI_TEST:-}" != "1" ]; then
    trap '_err_trap $LINENO' ERR
    trap '_cleanup' EXIT
fi

# ── Privilege escalation ──────────────────────────────
#
# Earlier versions tried `sudo touch "$tmp"` followed by an unprivileged
# `curl -o "$tmp"`, which fails with EACCES because curl runs as the
# invoking user but the file is now owned by root. The fix: always stage
# downloads into a user-writable tmp dir, then `sudo install` the
# finished file into place.
#
# need_sudo MSG — ensure we have a usable sudo session.
#
# Sets SUDO="sudo" so callers can do `$SUDO mv ...`. When already root,
# sets SUDO="" so the same callsites become no-ops.
#
# Prompts for the password ONCE and starts a keepalive so we don't
# re-prompt mid-install. Reads the password from /dev/tty so it works
# under `curl ... | bash` (where stdin is the script).

SUDO=""
SUDO_PRIMED=0
need_sudo() {
    local why="${1:-perform a privileged operation}"
    if [ "$(id -u)" -eq 0 ]; then
        SUDO=""
        return 0
    fi
    command -v sudo >/dev/null 2>&1 \
        || die "sudo is required to ${why}" \
               "Either install sudo, or re-run this script as root."
    SUDO="sudo"
    [ "$SUDO_PRIMED" -eq 1 ] && return 0

    if sudo -n true 2>/dev/null; then
        SUDO_PRIMED=1
        return 0
    fi

    info "Administrator password required to ${why}."
    # Read from /dev/tty so this works under `curl … | bash`, where
    # stdin is the piped script and not a terminal.
    if [ -r /dev/tty ]; then
        # shellcheck disable=SC2024
        # SC2024: sudo doesn't read stdin for the password (it goes
        # through tcsetattr on /dev/tty directly). Keeping the redirect
        # is intentional: it forces stdin to be the controlling terminal
        # in this scope, which matters under `curl ... | bash` where
        # stdin would otherwise be the piped script.
        sudo -v < /dev/tty || die "could not obtain sudo privileges"
    else
        sudo -v || die "could not obtain sudo privileges" \
                       "Re-run with: curl -fsSL bhatti.sh/install | sudo bash"
    fi
    SUDO_PRIMED=1
    # Keepalive: refresh the sudo timestamp every 50s while the script
    # runs. Cleanup trap kills this PID on exit.
    ( while true; do sudo -n true 2>/dev/null || exit; sleep 50; done ) &
    SUDO_KEEPALIVE_PID=$!
}

# ── Platform detection ────────────────────────────────

detect_platform() {
    OS=$(uname -s | tr '[:upper:]' '[:lower:]')
    HOST_ARCH=$(uname -m)
    map_arch "$HOST_ARCH"
}

# Map a host arch (uname -m output) to (ARCH, FC_ARCH) globals.
# Extracted from detect_platform so tests can drive every case without
# spawning a process — detect_platform itself is just "call uname,
# then map_arch", which doesn't need its own test.
map_arch() {
    case "$1" in
        x86_64)        ARCH="amd64" ;;
        aarch64|arm64) ARCH="arm64" ;;
        *) die "unsupported architecture: $1" ;;
    esac
}

# ── Download helpers ──────────────────────────────────

# download URL DEST — download a file and validate it's non-empty.
# On failure, prints what went wrong with actionable context.
download() {
    local url="$1" dest="$2"
    local http_code

    # Don't use -f (fail fast) — it causes curl to exit before writing
    # the -w output, so $http_code would be empty in the error path.
    # Silence rm errors: if $dest was created by a different user
    # (legacy bug), we don't want a confusing "Permission denied"
    # piling on top of the actual download failure.
    http_code=$(curl -sSL -w '%{http_code}' -o "$dest" "$url") || {
        rm -f "$dest" 2>/dev/null || true
        die "download failed: $url" \
            "curl error (network issue or invalid URL)" \
            "Check your network connection and try again."
    }

    if [ "$http_code" -ge 400 ] 2>/dev/null; then
        rm -f "$dest" 2>/dev/null || true
        die "download failed: $url" \
            "HTTP status: $http_code" \
            "Check your network connection and try again."
    fi

    if [ ! -s "$dest" ]; then
        rm -f "$dest" 2>/dev/null || true
        die "download produced an empty file: $url" \
            "This usually means the release asset is missing." \
            "Check: $url"
    fi
}

# download_large URL DEST — same as download() but with a progress bar.
# Use for files >50MB where multi-minute silence is bad UX.
download_large() {
    local url="$1" dest="$2"
    local http_code

    http_code=$(curl -SL --progress-bar -w '%{http_code}' -o "$dest" "$url") || {
        rm -f "$dest" 2>/dev/null || true
        die "download failed: $url" \
            "curl error (network issue or invalid URL)" \
            "Check your network connection and try again."
    }

    if [ "$http_code" -ge 400 ] 2>/dev/null; then
        rm -f "$dest" 2>/dev/null || true
        die "download failed: $url" \
            "HTTP status: $http_code" \
            "Check your network connection and try again."
    fi

    if [ ! -s "$dest" ]; then
        rm -f "$dest" 2>/dev/null || true
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

# ── Checksum verification ─────────────────────────────

CHECKSUMS=""

fetch_checksums() {
    CHECKSUMS=$(curl -fsSL "${RELEASE_URL}/checksums-sha256.txt" 2>/dev/null || true)
}

# Compute sha256 of a local file (empty string if file missing or no tool)
local_sha256() {
    local file="$1"
    [ -f "$file" ] || { echo ""; return 0; }
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$file" | awk '{print $1}'
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$file" | awk '{print $1}'
    else
        echo ""
    fi
}

# Get the expected checksum for a release asset name
remote_sha256() {
    local name="$1"
    [ -n "$CHECKSUMS" ] || { echo ""; return 0; }
    echo "$CHECKSUMS" | grep "$name" | awk '{print $1}'
}

# Check if a local file matches the release checksum. Returns 0 if up to date.
is_up_to_date() {
    local file="$1" asset_name="$2"
    [ -f "$file" ] || return 1
    local expected
    expected=$(remote_sha256 "$asset_name")
    [ -n "$expected" ] || return 1  # no checksum available, assume stale
    local actual
    actual=$(local_sha256 "$file")
    [ -n "$actual" ] || return 1
    [ "$actual" = "$expected" ]
}

# Returns 0 if every listed tier's rootfs is on disk AND its stored sidecar
# checksum matches the current release. Returns 1 if any tier is missing
# or stale (or if CHECKSUMS is empty — assume stale rather than wrongly skip).
#
# Why this exists separately from is_up_to_date():
# Rootfs is the one component where the local file (.ext4) is NOT what the
# release ships (.ext4.zst). install_rootfs() stores the *compressed*
# checksum in a .sha256 sidecar after a successful install, and uses that
# sidecar for its own skip-if-fresh check. The do_server_update() outer
# gate must use the same predicate, otherwise it can short-circuit before
# install_rootfs() ever gets a chance to look — which is exactly the bug
# that hid stale tier images behind `bhatti update --tiers <X>`.
all_rootfs_up_to_date() {
    local tier rootfs cks_file expected stored
    for tier in "$@"; do
        rootfs="$DATA_DIR/images/rootfs-${tier}-${ARCH}.ext4"
        cks_file="$DATA_DIR/images/.rootfs-${tier}-${ARCH}.sha256"
        [ -f "$rootfs" ] || return 1
        expected=$(remote_sha256 "rootfs-${tier}-${ARCH}.ext4.zst")
        [ -n "$expected" ] || return 1
        [ -f "$cks_file" ] || return 1
        stored=$(cat "$cks_file" 2>/dev/null || true)
        [ "$stored" = "$expected" ] || return 1
    done
    return 0
}

# Echo space-separated list of tiers whose rootfs is on disk but stale,
# excluding any tier in the args (caller passes the just-updated set).
# Used by the post-update hint so a user with a leftover stale tier
# image gets a visible nudge — the same UX gap that hid the original
# `bhatti update --tiers <X>` bug.
stale_rootfs_tiers() {
    local skip=" $* " result="" tier
    for tier in $ALL_KNOWN_TIERS; do
        case "$skip" in *" $tier "*) continue ;; esac
        [ -f "$DATA_DIR/images/rootfs-${tier}-${ARCH}.ext4" ] || continue
        all_rootfs_up_to_date "$tier" && continue
        result="${result:+$result }$tier"
    done
    echo "$result"
}

# Echo space-separated list of tiers whose rootfs is NOT on disk,
# excluding any tier in the args. Symmetric counterpart to
# stale_rootfs_tiers — together they classify every "other" tier into
# {fresh (skipped), stale-on-disk, not-installed}.
missing_rootfs_tiers() {
    local skip=" $* " result="" tier
    for tier in $ALL_KNOWN_TIERS; do
        case "$skip" in *" $tier "*) continue ;; esac
        [ -f "$DATA_DIR/images/rootfs-${tier}-${ARCH}.ext4" ] && continue
        result="${result:+$result }$tier"
    done
    echo "$result"
}

# Verify a downloaded file matches the expected checksum. Dies on mismatch.
verify_checksum() {
    local file="$1" expected_name="$2"
    local expected
    expected=$(remote_sha256 "$expected_name")
    [ -n "$expected" ] || return 0  # no checksum available, skip
    local actual
    actual=$(local_sha256 "$file")
    [ -n "$actual" ] || return 0
    [ "$actual" = "$expected" ] || die "checksum mismatch for $expected_name" \
        "expected: $expected" \
        "got:      $actual" \
        "The download may be corrupt. Try again."
}

# ── Disk space check ─────────────────────────────────

check_disk_space() {
    local required_mb="$1" path="$2"
    local available_mb
    # df -BM is Linux-only; use awk to parse portable df output
    available_mb=$(df -Pm "$path" 2>/dev/null | tail -1 | awk '{print $4}')
    if [ -n "$available_mb" ] && [ "$available_mb" -lt "$required_mb" ] 2>/dev/null; then
        die "insufficient disk space" \
            "Required: ${required_mb}MB" \
            "Available: ${available_mb}MB on $(df -P "$path" | tail -1 | awk '{print $6}')" \
            "Free up space and try again."
    fi
}

# ── Version helpers ───────────────────────────────────

resolve_latest_version() {
    # Test override: skip the GitHub API call, use whatever URL/version
    # the smoke test set up. See the BHATTI_TEST_* block at the top.
    if [ -n "${BHATTI_TEST_VERSION:-}" ]; then
        VERSION="$BHATTI_TEST_VERSION"
        RELEASE_URL="${BHATTI_TEST_RELEASE_URL:-https://github.com/${GITHUB_REPO}/releases/download/${VERSION}}"
        return 0
    fi

    # Production override: caller pinned a specific tag (e.g.
    #   sudo BHATTI_VERSION=v1.11.4-rc.1 bhatti update
    # to validate a prerelease on a single host before promoting it to
    # latest). Bypasses the `releases/latest` API call which only sees
    # non-prerelease tags. The asset URL pattern is the standard GitHub
    # release download URL; if the tag exists and is non-draft the
    # downloads will work, otherwise install_* helpers fail loudly with
    # the URL they tried.
    if [ -n "${BHATTI_VERSION:-}" ]; then
        VERSION="$BHATTI_VERSION"
        RELEASE_URL="https://github.com/${GITHUB_REPO}/releases/download/${VERSION}"
        return 0
    fi

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
#
# Optional arg $1: explicit config path. When unset, tries the canonical
# location and the pre-v1.6 fallback in $DATA_DIR. Tests pass an explicit
# path to drive the parser without touching real system files.
# shellcheck disable=SC2120
# SC2120: "function references arguments, but none are ever passed".
# Production callers (do_server_install, do_server_update) intentionally
# call this without args to use the canonical config path; the optional
# arg is for the bats unit tests in install_test.bats. Same reason for
# the SC2119 disable on the production callers below.
detect_tier() {
    # Primary: parse firecracker_rootfs from config.yaml
    local config_file="${1:-}"
    if [ -z "$config_file" ]; then
        config_file="/etc/bhatti/config.yaml"
        [ -f "$config_file" ] || config_file="$DATA_DIR/config.yaml"  # pre-v1.6 fallback
    fi
    if [ -f "$config_file" ]; then
        local rootfs_path
        # v2 uses krucible_base_image; keep firecracker_rootfs for migration.
        rootfs_path=$(grep -E '^(krucible_base_image|firecracker_rootfs):' "$config_file" | head -1 | awk '{print $2}' | tr -d '"' | tr -d "'") || true
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
    # Prefer "minimal" as the safest default if multiple tiers exist.
    local found_tier=""
    for f in "$DATA_DIR/images/rootfs-"*"-${ARCH}.ext4"; do
        [ -f "$f" ] || continue
        local t
        t=$(basename "$f" | sed "s/rootfs-//;s/-${ARCH}\.ext4//")
        [ "$t" = "minimal" ] && { echo "minimal"; return 0; }
        found_tier="$t"
    done
    # No minimal found — return first available, or default
    echo "${found_tier:-minimal}"
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

# is_firecracker_install returns 0 (true) if this host carries a v1 (Firecracker)
# server — the firecracker binary, or a config that predates the krucible engine.
# Used to hard-block an in-place v1→v2 crossing (a different VMM; not upgradeable).
is_firecracker_install() {
    command -v firecracker >/dev/null 2>&1 && return 0
    local cfg="/etc/bhatti/config.yaml"
    [ -f "$cfg" ] || cfg="$DATA_DIR/config.yaml"
    [ -f "$cfg" ] || return 1
    grep -q '^engine:[[:space:]]*krucible' "$cfg" && return 1
    grep -q '^firecracker_rootfs:' "$cfg"
}

# ── Install functions ─────────────────────────────────

# ensure_zstd installs zstd if missing (needed for the .tar.zst runtime bundle).
ensure_zstd() {
    command -v zstd >/dev/null 2>&1 && return 0
    info "Installing zstd..."
    if command -v apt-get >/dev/null 2>&1; then
        { apt-get update -qq && apt-get install -y -qq zstd >/dev/null; } || true
    elif command -v dnf >/dev/null 2>&1; then
        dnf install -y -q zstd >/dev/null || true
    elif command -v yum >/dev/null 2>&1; then
        yum install -y -q zstd >/dev/null || true
    elif command -v brew >/dev/null 2>&1; then
        brew install zstd >/dev/null 2>&1 || true
    fi
    command -v zstd >/dev/null 2>&1 \
        || die "failed to install zstd" "Install it manually and re-run."
}

# install_bundle downloads the per-platform v2 runtime bundle
# (bhatti-<ver>-<os>-<arch>.tar.zst = CLI + bhatti-vmm + bhatti-netd + libkrun +
# lean kernel), verifies it, installs the CLI to /usr/local/bin, and — when the
# first arg is 1 — lays the krucible runtime under $DATA_DIR/runtime (consumed by
# generate_config, and sets RUNTIME_DIR). One self-contained bundle IS the v2
# install; there are no separate vmm/libkrun/kernel assets to chase.
install_bundle() {
    local want_runtime="${1:-0}"
    local asset="bhatti-${VERSION}-${OS}-${ARCH}.tar.zst"
    ensure_zstd

    local tmp stage
    tmp=$(mktemp "${TMPDIR:-/tmp}/bhatti-bundle.XXXXXX") || die "could not create temp file"
    BHATTI_STAGE_FILE="$tmp"
    stage=$(mktemp -d "${TMPDIR:-/tmp}/bhatti-stage.XXXXXX") || die "could not create temp dir"

    download "${RELEASE_URL}/${asset}" "$tmp"
    verify_checksum "$tmp" "$asset"
    zstd -dc "$tmp" | tar -xf - -C "$stage" || die "failed to extract ${asset}"
    rm -f "$tmp"

    local root
    # -mindepth 1 so we don't match $stage itself (it's named bhatti-stage.XXXX,
    # which also matches bhatti-*) — we want the extracted bhatti-<ver>-... subdir.
    root=$(find "$stage" -mindepth 1 -maxdepth 1 -type d -name 'bhatti-*' | head -1)
    if [ -z "$root" ] || [ ! -x "$root/bin/bhatti" ]; then
        die "unexpected bundle layout (no bin/bhatti in ${asset})"
    fi

    if [ "$OS" = "darwin" ]; then
        xattr -dr com.apple.quarantine "$root" 2>/dev/null || true
    fi
    "$root/bin/bhatti" version >/dev/null 2>&1 \
        || die "downloaded bhatti failed to execute (wrong platform or corrupt download)"

    # Install the CLI.
    local dest="${BHATTI_TEST_BIN_DEST:-/usr/local/bin/bhatti}"
    local dest_dir; dest_dir=$(dirname "$dest")
    if [ ! -d "$dest_dir" ] || [ ! -w "$dest_dir" ] || { [ -e "$dest" ] && [ ! -w "$dest" ]; }; then
        need_sudo "install bhatti to ${dest}"
    else
        SUDO=""
    fi
    [ -d "$dest_dir" ] || $SUDO mkdir -p "$dest_dir"
    { [ -f "$dest" ] && $SUDO cp "$dest" "${dest}.old" 2>/dev/null; } || true
    $SUDO install -m 0755 "$root/bin/bhatti" "$dest" || die "failed to install bhatti to ${dest}"

    # Runtime (server / local daemon): vmm + netd + libkrun + lean kernel.
    if [ "$want_runtime" = "1" ]; then
        RUNTIME_DIR="$DATA_DIR/runtime"
        rm -rf "$RUNTIME_DIR"
        mkdir -p "$RUNTIME_DIR"
        cp -R "$root/bin" "$root/lib" "$root/kernel" "$RUNTIME_DIR/"
        chmod +x "$RUNTIME_DIR/bin/"* 2>/dev/null || true
        if [ "$OS" = "darwin" ]; then
            xattr -dr com.apple.quarantine "$RUNTIME_DIR" 2>/dev/null || true
        fi
    fi
    rm -rf "$stage"
}

install_bhatti_binary() {
    local binary="bhatti-${OS}-${ARCH}"
    # BHATTI_TEST_BIN_DEST lets the smoke test redirect the install away
    # from /usr/local/bin so it doesn't clobber a developer's real bhatti.
    local dest="${BHATTI_TEST_BIN_DEST:-/usr/local/bin/bhatti}"
    local dest_dir
    dest_dir=$(dirname "$dest")

    # Stage to a user-writable tmp file. mktemp picks $TMPDIR (per-user
    # on macOS, /tmp on Linux) — both are guaranteed writable by the
    # invoking user, which avoids the EACCES we'd hit if we tried to
    # download straight into a root-owned /usr/local/bin.
    local tmp
    tmp=$(mktemp "${TMPDIR:-/tmp}/bhatti.XXXXXX") \
        || die "could not create temp file"
    BHATTI_STAGE_FILE="$tmp"  # picked up by _cleanup on exit

    download "${RELEASE_URL}/${binary}" "$tmp"
    chmod +x "$tmp"

    verify_checksum "$tmp" "$binary"

    # Verify the binary actually executes (catches HTML error pages,
    # wrong-arch binaries, truncated downloads)
    if ! "$tmp" version >/dev/null 2>&1; then
        rm -f "$tmp"
        die "downloaded binary failed to execute" \
            "This usually means the download was corrupted or" \
            "the wrong platform binary was downloaded." \
            "Expected: ${OS}/${ARCH}"
    fi

    # macOS: remove quarantine attribute to prevent Gatekeeper dialog
    if [ "$OS" = "darwin" ]; then
        xattr -d com.apple.quarantine "$tmp" 2>/dev/null || true
    fi

    # Decide whether we need sudo for the install step. We may need it
    # for two reasons: dest_dir doesn't exist (Apple Silicon: no
    # /usr/local/bin by default) or it isn't writable by us.
    local need_priv=0
    if [ ! -d "$dest_dir" ]; then
        need_priv=1
    elif [ ! -w "$dest_dir" ]; then
        need_priv=1
    elif [ -e "$dest" ] && [ ! -w "$dest" ]; then
        need_priv=1
    fi
    if [ "$need_priv" -eq 1 ]; then
        need_sudo "install bhatti to ${dest}"
    else
        SUDO=""
    fi

    # Apple Silicon Macs don't ship /usr/local/bin; create it if missing.
    if [ ! -d "$dest_dir" ]; then
        $SUDO mkdir -p "$dest_dir"
    fi

    # Backup previous binary for manual rollback
    if [ -f "$dest" ]; then
        $SUDO cp "$dest" "${dest}.old" 2>/dev/null || true
    fi

    # `install(1)` does mode-set + atomic rename in a single call.
    # It exists on both macOS (BSD) and Linux (GNU coreutils) with
    # compatible -m semantics. Falls back to cp+mv if absent.
    if command -v install >/dev/null 2>&1; then
        $SUDO install -m 0755 "$tmp" "$dest" \
            || die "failed to install binary to ${dest}"
    else
        # cp+chmod fallback for the rare box without install(1).
        # `if !` instead of A&&B||C so chmod failure also triggers die
        # — the && || form was a SC2015 footgun even if it happened to
        # work here (writer's intent isn't obvious to the reader).
        if ! { $SUDO cp "$tmp" "$dest" && $SUDO chmod 0755 "$dest"; }; then
            die "failed to install binary to ${dest}"
        fi
    fi
    rm -f "$tmp"
    BHATTI_STAGE_FILE=""
}

install_rootfs() {
    local tier="$1"
    local asset="rootfs-${tier}-${ARCH}.ext4.zst"
    local rootfs_path="$DATA_DIR/images/rootfs-${tier}-${ARCH}.ext4"
    local checksum_file="$DATA_DIR/images/.rootfs-${tier}-${ARCH}.sha256"

    # Skip if local rootfs matches the release checksum.
    # We store the compressed (.zst) checksum after install because the
    # release checksum is for the compressed file, not the decompressed one.
    local expected
    expected=$(remote_sha256 "$asset")
    if [ -n "$expected" ] && [ -f "$rootfs_path" ] && [ -f "$checksum_file" ]; then
        local stored
        stored=$(cat "$checksum_file" 2>/dev/null || true)
        if [ "$stored" = "$expected" ]; then
            success "rootfs ${tier} (up to date)"
            return 0
        fi
    fi

    step_start
    heading "Installing ${tier} rootfs"

    # Disk space check: need room for compressed + decompressed
    case "$tier" in
        minimal)  check_disk_space 400 "$DATA_DIR" ;;
        browser)  check_disk_space 1000 "$DATA_DIR" ;;
        docker)   check_disk_space 900 "$DATA_DIR" ;;
        computer) check_disk_space 2500 "$DATA_DIR" ;;
    esac

    # Install zstd if needed. Each branch is a best-effort install — if
    # any step fails, we tolerate it and re-check `command -v zstd` below
    # to die with a clear message. The `{ ...; } || true` brace grouping
    # is the unambiguous form of "the whole compound was best-effort";
    # `A && B || true` would silently let A's failure look like a
    # success-path skip of B (SC2015).
    if ! command -v zstd >/dev/null 2>&1; then
        info "Installing zstd..."
        if command -v apt-get >/dev/null 2>&1; then
            { apt-get update -qq && apt-get install -y -qq zstd >/dev/null; } || true
        elif command -v dnf >/dev/null 2>&1; then
            dnf install -y -q zstd >/dev/null || true
        elif command -v yum >/dev/null 2>&1; then
            yum install -y -q zstd >/dev/null || true
        fi
        command -v zstd >/dev/null 2>&1 \
            || die "failed to install zstd" \
                   "Try installing it manually and re-run:" \
                   "  sudo apt-get install zstd   # Debian/Ubuntu" \
                   "  sudo dnf install zstd       # Fedora/RHEL" \
                   "  sudo yum install zstd       # CentOS"
    fi

    # Download compressed file (with progress bar), verify, then decompress
    local zst_tmp="${rootfs_path}.zst.tmp"
    download_large "${RELEASE_URL}/${asset}" "$zst_tmp"
    verify_checksum "$zst_tmp" "$asset"

    info "Decompressing..."
    zstd -d -q -f -o "$rootfs_path" "$zst_tmp"
    rm -f "$zst_tmp"

    [ -s "$rootfs_path" ] \
        || die "rootfs decompression produced an empty file" \
               "This may indicate insufficient disk space." \
               "Available space: $(df -h "$DATA_DIR" 2>/dev/null | tail -1 | awk '{print $4}')"

    # Store the compressed checksum for future skip checks
    if [ -n "$expected" ]; then
        echo "$expected" > "$checksum_file"
    fi

    success "rootfs ${tier} ($(du -h "$rootfs_path" | cut -f1), $(step_elapsed))"
}

generate_config() {
    local tier="$1"
    local rt="${RUNTIME_DIR:-$DATA_DIR/runtime}"
    # The lean kernel shipped in the bundle (Image-lean-* on arm64, vmlinux-lean-*
    # on x86_64). block-root + external lean kernel is the v2 boot path.
    local kernel
    kernel=$(find "$rt/kernel" -maxdepth 1 -type f \( -name 'Image-lean-*' -o -name 'vmlinux-lean-*' \) 2>/dev/null | head -1)
    mkdir -p /etc/bhatti
    cat > /etc/bhatti/config.yaml << EOF
engine: krucible
listen: :8080
data_dir: ${DATA_DIR}
# Secure per-owner network gateway (bhatti-netd) is ON by default: the guest is
# isolated from the host, egress is policed, and same-owner siblings are
# reachable. Set 'krucible_net_backend: false' for the legacy shared-netstack
# (TSI) path (a sandbox can then reach the host's loopback — not recommended).
krucible_vmm: ${rt}/bin/bhatti-vmm
krucible_netd: ${rt}/bin/bhatti-netd
krucible_libdir: ${rt}/lib
krucible_kernel_image: ${kernel}
krucible_base_image: ${DATA_DIR}/images/rootfs-${tier}-${ARCH}.ext4
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
            # Fallback for systems without getent (some minimal images).
            # `if`-form, not `&& ... || true`, to keep the failure semantics
            # explicit (SC2015): we want "set user_home from eval, ignore
            # eval errors", not "if test passes and assignment fails, run true".
            if [ -z "$user_home" ]; then
                user_home=$(eval echo "~$SUDO_USER" 2>/dev/null) || true
            fi

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

# write_launchd_daemon — the macOS equivalent of the systemd unit. A LaunchDaemon
# that runs `bhatti serve`. HVF works for the daemon; the notarized bhatti-vmm
# carries the com.apple.security.hypervisor entitlement. Logs to the data dir.
write_launchd_daemon() {
    local plist=/Library/LaunchDaemons/sh.bhatti.plist
    cat > "$plist" << EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>sh.bhatti</string>
    <key>ProgramArguments</key>
    <array>
        <string>/usr/local/bin/bhatti</string>
        <string>serve</string>
    </array>
    <key>RunAtLoad</key><true/>
    <key>KeepAlive</key><true/>
    <key>WorkingDirectory</key><string>${DATA_DIR}</string>
    <key>StandardOutPath</key><string>${DATA_DIR}/bhatti.log</string>
    <key>StandardErrorPath</key><string>${DATA_DIR}/bhatti.log</string>
</dict>
</plist>
EOF
    chmod 644 "$plist"
}

# start_service — (re)start the daemon after install/update and confirm health.
# OS-aware: systemd on Linux, launchd on macOS. Best-effort; a failure prints
# where to look rather than aborting a finished install.
start_service() {
    local plist=/Library/LaunchDaemons/sh.bhatti.plist
    if [ "$OS" = "darwin" ]; then
        launchctl bootout system "$plist" 2>/dev/null || true
        launchctl bootstrap system "$plist" 2>/dev/null \
            || launchctl load "$plist" 2>/dev/null || true
    elif command -v systemctl >/dev/null 2>&1; then
        systemctl enable --now bhatti 2>/dev/null || return 1
    else
        return 1
    fi
    local healthy=false
    for _ in 1 2 3 4 5; do
        if curl -sf http://localhost:8080/health >/dev/null 2>&1; then healthy=true; break; fi
        sleep 1
    done
    [ "$healthy" = true ]
}

# prompt_and_install_server — shared by the Linux and macOS fresh-install flows:
# prompt for a tier (unless BHATTI_TIER is set), then do a self-host install
# (handling the "all" tiers case). do_server_install is OS-aware.
prompt_and_install_server() {
    local tier="${BHATTI_TIER:-}"
    if [ -z "$tier" ]; then
        echo ""
        echo "  Rootfs tier:"
        echo "    1) minimal  — bare Ubuntu (~200MB)"
        echo "    2) browser  — + Chromium/Playwright (~600MB)"
        echo "    3) docker   — + Docker Engine (~550MB)"
        echo "    4) computer — + Full desktop with KasmVNC (~1.5GB)"
        echo "    5) all      — install all tiers (~2.8GB)"
        echo ""
        printf "  Choice [1]: "
        read -r tier_choice < /dev/tty 2>/dev/null || tier_choice="1"
        case "${tier_choice:-1}" in
            2) tier="browser" ;;
            3) tier="docker" ;;
            4) tier="computer" ;;
            5) tier="all" ;;
            *) tier="minimal" ;;
        esac
    fi
    if [ "$tier" = "all" ]; then
        do_server_install "minimal"
        for t in browser docker computer; do
            install_rootfs "$t"
        done
    else
        do_server_install "$tier"
    fi
}

# ── Top-level flows ────────────────────────────────────

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
            printf '  %s⚠  Major version upgrade: %s → %s%s\n' "$RED" "$current" "$VERSION" "$RESET"
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

    install_bundle    # v2 self-contained bundle; CLI only for a client install

    echo ""
    success "bhatti ${VERSION} → /usr/local/bin/bhatti"
    if [ -z "$current" ]; then
        echo ""
        echo "  Quick start:"
        echo "    bhatti setup     # configure API endpoint + key"
        # Shell completions hint
        local shell_name
        shell_name=$(basename "${SHELL:-}" 2>/dev/null || true)
        case "$shell_name" in
            zsh)  echo ""
                  echo "  Shell completions:"
                  echo "    echo 'source <(bhatti completion zsh)' >> ~/.zshrc" ;;
            bash) echo ""
                  echo "  Shell completions:"
                  echo "    echo 'source <(bhatti completion bash)' >> ~/.bashrc" ;;
            fish) echo ""
                  echo "  Shell completions:"
                  echo "    bhatti completion fish > ~/.config/fish/completions/bhatti.fish" ;;
        esac
    fi
}

do_server_install() {
    local tier="${1:-minimal}"

    [ "$(id -u)" -eq 0 ] || die "server installation requires root" \
                                "Re-run with:" \
                                "  sudo bhatti update" \
                                "  curl -fsSL bhatti.sh/install | sudo bash"

    # Preflight — hypervisor per OS. Linux: KVM (/dev/kvm). macOS: HVF on Apple
    # Silicon (no /dev/kvm; the notarized bhatti-vmm carries the hypervisor
    # entitlement, so it runs without any extra device).
    if [ "$OS" = "darwin" ]; then
        [ "$ARCH" = "arm64" ] || die "self-hosting on macOS requires Apple Silicon (arm64)" \
                                     "Intel Macs are not supported for the v2 runtime."
    else
        if [ ! -e /dev/kvm ]; then
            modprobe kvm 2>/dev/null || true
        fi
        [ -e /dev/kvm ] || die "/dev/kvm not available — KVM is required" \
                               "Enable virtualization in your BIOS/hypervisor settings," \
                               "or use a VM with nested virtualization enabled."
    fi
    command -v curl >/dev/null 2>&1 || die "curl is required"

    heading "Installing bhatti ${VERSION} (server, ${tier} tier) on $(hostname) (${HOST_ARCH})"

    mkdir -p "$DATA_DIR"/{images,sandboxes,volumes,snapshots}

    # v2 (krucible): one self-contained bundle brings the CLI + the whole runtime
    # (bhatti-vmm, bhatti-netd, libkrun, lean kernel). No Firecracker.
    heading "Installing bhatti ${VERSION} + runtime"
    install_bundle 1
    success "bhatti ${VERSION} + krucible runtime"

    install_rootfs "$tier"
    generate_config "$tier"
    create_admin_user

    # Install the service (OS-aware): systemd on Linux, launchd on macOS.
    if [ "$OS" = "darwin" ]; then
        write_launchd_daemon
    else
        write_systemd_unit
        if command -v systemctl >/dev/null 2>&1; then
            cp "$DATA_DIR/bhatti.service" /etc/systemd/system/bhatti.service
            systemctl daemon-reload
        fi
    fi

    local elapsed=$(( SECONDS - _total_start ))
    echo ""
    echo "============================================"
    echo "  bhatti ${VERSION} installed (${elapsed}s)"
    echo "  tier: ${tier}"
    echo ""
    echo "  Manage users:"
    echo "    sudo bhatti user create --name alice"
    echo "    sudo bhatti user list"
    echo ""
    if [ -f "$DATA_DIR/age.key" ]; then
        echo "  ⚠  BACK UP: $DATA_DIR/age.key"
        echo "     If lost, all encrypted secrets become unrecoverable."
    else
        echo "  ⚠  When you first use 'bhatti secret set', an encryption"
        echo "     key is created at $DATA_DIR/age.key — back it up."
    fi
    echo ""
    # Hint about other available tiers
    local other_tiers=""
    for t in minimal browser docker computer; do
        [ "$t" = "$tier" ] && continue
        other_tiers="${other_tiers:+$other_tiers, }$t"
    done
    if [ -n "$other_tiers" ]; then
        echo "  Other tiers available: ${other_tiers}"
        echo "  Install with: sudo bhatti update --tiers all"
    fi
    echo ""
    echo "  Uninstall:"
    echo "    curl -fsSL bhatti.sh/uninstall | sudo bash"
    echo "============================================"

    # Always start the service on fresh install — the user just installed
    # everything. OS-aware (systemd on Linux, launchd on macOS).
    echo ""
    if start_service; then
        success "bhatti service started and healthy"
    else
        printf '  %s⚠  Service not responding on :8080 yet%s\n' "$RED" "$RESET"
        if [ "$OS" = "darwin" ]; then
            echo "  Check logs:"
            echo "    tail -n 40 ${DATA_DIR}/bhatti.log"
            echo "  Or run it in the foreground:"
            echo "    bhatti serve"
        else
            echo "  Check logs:"
            echo "    sudo journalctl -u bhatti --no-pager -n 20"
            echo ""
            journalctl -u bhatti --no-pager -n 5 2>/dev/null || true
        fi
    fi

    # Print API key last so it's visible and not buried
    if [ -n "${ADMIN_KEY:-}" ]; then
        echo ""
        echo "  ┌────────────────────────────────────────────────┐"
        echo "  │  Admin API key (save this — shown only once):  │"
        echo "  │  ${ADMIN_KEY}  │"
        echo "  └────────────────────────────────────────────────┘"
    fi
}

do_server_update() {
    [ "$(id -u)" -eq 0 ] || die "server update requires root" \
                                "Re-run with:" \
                                "  sudo bhatti update" \
                                "  curl -fsSL bhatti.sh/install | sudo bash"

    local tier current
    # shellcheck disable=SC2119
    # SC2119 pairs with the SC2120 disable on detect_tier itself: the
    # function's $1 is an optional test hook, production calls use the
    # canonical config path.
    tier=$(detect_tier)
    current=$(installed_bhatti_version)

    # Determine which tiers to install.
    # Default: only the configured tier. BHATTI_TIERS overrides:
    #   all            → every known tier
    #   tier1,tier2    → specific list
    local tiers_to_install="$tier"
    if [ -n "${BHATTI_TIERS:-}" ]; then
        if [ "${BHATTI_TIERS}" = "all" ]; then
            tiers_to_install="$ALL_KNOWN_TIERS"
        else
            tiers_to_install=$(echo "$BHATTI_TIERS" | tr ',' ' ')
        fi
        # Always include the configured tier
        case "$tiers_to_install" in
            *"$tier"*) ;;
            *) tiers_to_install="$tier $tiers_to_install" ;;
        esac
    fi

    # Check if everything is already up to date.
    # Non-rootfs components only get a -f existence check here; each
    # install_*() function does its own checksum-based skip when called.
    # Rootfs files MUST go through a checksum check (all_rootfs_up_to_date)
    # because a stale .ext4 from a previous version can satisfy -f and
    # short-circuit `bhatti update --tiers <X>` for that tier — the gate
    # would skip install_rootfs() before its own skip-check ran.
    # v2 runtime closure (from `install_bundle 1`): the CLI, the per-VM vmm helper,
    # the per-owner net gateway, libkrun, and the lean kernel. lohar is baked into
    # the rootfs (/init.krun), not a standalone file. (The old FC checks —
    # firecracker/lohar/vmlinux — never matched on v2, so update never short-circuited.)
    local rt="$DATA_DIR/runtime"
    local all_present=true
    [ -f "/usr/local/bin/bhatti" ]              || all_present=false
    [ -f "$rt/bin/bhatti-vmm" ]                 || all_present=false
    [ -f "$rt/bin/bhatti-netd" ]                || all_present=false
    ls "$rt"/lib/libkrun.* >/dev/null 2>&1      || all_present=false
    ls "$rt"/kernel/*-lean-* >/dev/null 2>&1    || all_present=false

    local rootfs_fresh=true
    # shellcheck disable=SC2086
    # intentional word-splitting: $tiers_to_install is space-separated tokens
    all_rootfs_up_to_date $tiers_to_install || rootfs_fresh=false

    if [ -n "$current" ] && [ "v${current#v}" = "${VERSION}" ] \
       && [ "$all_present" = true ] && [ "$rootfs_fresh" = true ]; then
        success "bhatti ${VERSION} (server, ${tier} tier) is already up to date"
        return 0
    fi

    # Hard stop: a v1 (Firecracker) server cannot upgrade in place to v2
    # (krucible) — a different VMM, non-portable snapshots, a different on-disk
    # layout. This is a deliberate cutover, not an update.
    if is_firecracker_install && [ "$(major_version "$VERSION")" -ge 2 ]; then
        echo ""
        printf '  %s✗ Cannot upgrade a Firecracker (v1) server to %s in place.%s\n' "$RED" "$VERSION" "$RESET"
        echo "  v2 replaces Firecracker with krucible — a different VMM. Snapshots and the"
        echo "  on-disk layout do not carry over, so moving to v2 is a fresh install:"
        echo ""
        echo "    1. Drain + back up this server (sandboxes, volumes, secrets)."
        echo "    2. Install v2 fresh (on a clean host, or after removing v1):"
        echo "         curl -fsSL bhatti.sh/install | sudo bash"
        echo ""
        echo "  To keep running Firecracker instead, pin v1:"
        echo "    curl -fsSL https://raw.githubusercontent.com/${GITHUB_REPO}/firecracker/scripts/install.sh | sudo BHATTI_VERSION=v1.11.12 bash"
        echo ""
        if [ "${BHATTI_ALLOW_MAJOR_CUTOVER:-}" = "1" ]; then
            info "BHATTI_ALLOW_MAJOR_CUTOVER=1 set — proceeding with a fresh v2 install over v1 (v1 data is NOT migrated)"
        else
            die "refusing in-place v1→v2 upgrade (different VMM)" \
                "Set BHATTI_ALLOW_MAJOR_CUTOVER=1 to install v2 over this host anyway (v1 data is NOT migrated)."
        fi
    fi

    # Guard against other major version crossings (e.g. a future v2 → v3).
    # Major version bumps may include breaking changes (snapshot format,
    # config schema, etc.) that require manual migration steps.
    if [ -n "$current" ] && crosses_major "$current" "$VERSION"; then
        echo ""
        printf '  %s⚠  Major version upgrade: %s → %s%s\n' "$RED" "$current" "$VERSION" "$RESET"
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

    # Stop the service if running (restart after update). OS-aware.
    local was_running=false
    if [ "$OS" = "darwin" ]; then
        if launchctl print system/sh.bhatti >/dev/null 2>&1; then
            was_running=true
            heading "Stopping bhatti service"
            launchctl bootout system /Library/LaunchDaemons/sh.bhatti.plist 2>/dev/null || true
        fi
    elif command -v systemctl >/dev/null 2>&1 && systemctl is-active bhatti >/dev/null 2>&1; then
        was_running=true
        heading "Stopping bhatti service"
        systemctl stop bhatti
    fi

    heading "Installing bhatti ${VERSION} + runtime"
    install_bundle 1
    success "bhatti ${VERSION} + krucible runtime"

    for t in $tiers_to_install; do
        install_rootfs "$t"
    done
    if [ "$OS" = "darwin" ]; then
        write_launchd_daemon
    else
        write_systemd_unit
    fi

    # Migrate config from old location if needed (pre-v1.6, Linux only)
    if [ "$OS" != "darwin" ] && [ -f "$DATA_DIR/config.yaml" ] && [ ! -f "/etc/bhatti/config.yaml" ]; then
        mkdir -p /etc/bhatti
        mv "$DATA_DIR/config.yaml" /etc/bhatti/config.yaml
        info "Migrated config to /etc/bhatti/config.yaml"
    fi
    # INVARIANT: do_server_update NEVER overwrites /etc/bhatti/config.yaml.
    # The operator's config is preserved across updates. Only
    # do_server_install generates a fresh config. If the config schema
    # changes, handle it via migration logic, not regeneration.
    # admin user is PRESERVED

    # Always refresh the service definition + ensure it's enabled (OS-aware).
    if [ "$OS" = "darwin" ]; then
        if [ "$was_running" = true ]; then
            heading "Restarting bhatti service"
            launchctl bootstrap system /Library/LaunchDaemons/sh.bhatti.plist 2>/dev/null \
                || launchctl load /Library/LaunchDaemons/sh.bhatti.plist 2>/dev/null || true
        fi
    else
        cp "$DATA_DIR/bhatti.service" /etc/systemd/system/bhatti.service
        systemctl daemon-reload
        systemctl enable bhatti 2>/dev/null || true
        if [ "$was_running" = true ]; then
            heading "Restarting bhatti service"
            systemctl start bhatti
        fi
    fi

    echo ""
    echo "============================================"
    local elapsed=$(( SECONDS - _total_start ))
    echo "  bhatti updated to ${VERSION} (${elapsed}s)"
    echo "  tiers: $(echo "$tiers_to_install" | tr ' ' ', ')"
    # Status of every "other" tier (not in this update), in two buckets:
    #   stale on disk — user has a tier image from a previous release
    #   not installed — user never pulled this tier
    # One unified `--tiers all` suggestion follows because the gate fix
    # makes --tiers all reliable: it refreshes anything stale and no-ops
    # anything already fresh, so we don't need separate commands.
    local stale missing
    # shellcheck disable=SC2086
    # intentional word-splitting: $tiers_to_install is space-separated tokens
    stale=$(stale_rootfs_tiers $tiers_to_install)
    # shellcheck disable=SC2086
    missing=$(missing_rootfs_tiers $tiers_to_install)
    if [ -n "$stale" ] || [ -n "$missing" ]; then
        echo ""
        [ -n "$stale" ]   && echo "  outdated on disk: ${stale// /, }"
        [ -n "$missing" ] && echo "  not installed:    ${missing// /, }"
        echo "  update with:      sudo bhatti update --tiers all"
    fi
    if [ "$was_running" = true ]; then
        echo "  systemd service: restarted"
    else
        # Detect non-systemd bhatti serve process
        local serve_pid
        serve_pid=$(pgrep -x bhatti 2>/dev/null | head -1 || true)
        if [ -n "$serve_pid" ]; then
            echo ""
            echo "  ⚠  bhatti serve is running (PID ${serve_pid})"
            echo "     Restart it to use ${VERSION}"
        else
            echo ""
            echo "  Service enabled. Start with: sudo systemctl start bhatti"
        fi
    fi
    if [ -f /usr/local/bin/bhatti.old ]; then
        echo ""
        echo "  Rollback: sudo mv /usr/local/bin/bhatti.old /usr/local/bin/bhatti"
    fi
    echo "============================================"
}

# ── Main ──────────────────────────────────────────────

main() {
    _total_start=$SECONDS
    detect_platform
    resolve_latest_version
    fetch_checksums

    local install_type
    install_type=$(detect_install_type)

    # Both Linux (KVM) and macOS (Apple Silicon/HVF) can run the full stack —
    # CLI-only or a self-host server. The only per-OS differences (hypervisor
    # preflight, systemd vs launchd) live in do_server_install / do_server_update.
    case "$OS" in
        linux|darwin)
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
                        if [ "$OS" = "linux" ] && [ "$(id -u)" -eq 0 ] && [ -e /dev/kvm ]; then
                            # Root + KVM: they ran `curl | sudo bash` on a capable
                            # box — default to self-host.
                            echo ""
                            echo "  Install bhatti as:"
                            echo "    1) Self-host — run bhatti on this machine"
                            echo "    2) CLI only — connect to a remote bhatti server"
                            echo ""
                            printf "  Choice [1]: "
                            read -r mode_choice < /dev/tty 2>/dev/null || mode_choice="1"
                            case "${mode_choice:-1}" in
                                2) mode="cli" ;;
                                *) mode="server" ;;
                            esac
                        else
                            echo ""
                            echo "  Install bhatti as:"
                            echo "    1) CLI — connect to a remote bhatti server"
                            if [ "$OS" = "darwin" ]; then
                                echo "    2) Self-host — run sandboxes locally on this Mac (Apple Silicon)"
                            else
                                echo "    2) Self-host — run bhatti on this machine (requires root + KVM)"
                            fi
                            echo ""
                            printf "  Choice [1]: "
                            read -r mode_choice < /dev/tty 2>/dev/null || mode_choice="1"
                            case "${mode_choice:-1}" in
                                2) mode="server" ;;
                                *) mode="cli" ;;
                            esac
                        fi
                    fi

                    case "$mode" in
                        server)
                            # Self-host writes /etc/bhatti, /usr/local/bin, and the
                            # data dir — root on both OSes (macOS HVF itself needs no
                            # root, but these paths do).
                            [ "$(id -u)" -eq 0 ] || die "self-host installation requires root" \
                                                        "Re-run with:" \
                                                        "  curl -fsSL bhatti.sh/install | sudo bash"
                            prompt_and_install_server
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

# ── Flag parsing ──────────────────────────────────────
# Flags override env vars. Parsed before main() so they work
# both when run directly and via curl|bash -s -- --flags.

usage() {
    cat <<EOF
Usage: install.sh [flags]

Flags:
  --tier <name>       Tier for fresh install (minimal, browser, docker, computer)
  --tiers <list|all>  Additional tiers to install on update (comma-separated or "all")
  --mode <cli|server> Skip install type prompt
  --force             Skip major version upgrade confirmation
  --quiet             Suppress output (exit code only, for CI)
  --verbose           Enable debug output (set -x)
  -h, --help          Show this help

Environment variables (equivalent, for piped installs):
  BHATTI_TIER, BHATTI_TIERS, BHATTI_MODE, BHATTI_FORCE=1

Examples:
  curl -fsSL bhatti.sh/install | bash                             # CLI (auto-detected)
  curl -fsSL bhatti.sh/install | sudo bash                        # server (prompted)
  curl -fsSL bhatti.sh/install | sudo bash -s -- --tiers all      # flags via pipe
  sudo ./scripts/install.sh --tier computer                       # server, computer tier
  sudo ./scripts/install.sh --tiers all                           # update + pull all tiers
EOF
}

parse_flags() {
    while [ $# -gt 0 ]; do
        case "$1" in
            --tier)    BHATTI_TIER="$2"; shift 2 ;;
            --tier=*)  BHATTI_TIER="${1#--tier=}"; shift ;;
            --tiers)   BHATTI_TIERS="$2"; shift 2 ;;
            --tiers=*) BHATTI_TIERS="${1#--tiers=}"; shift ;;
            --mode)    BHATTI_MODE="$2"; shift 2 ;;
            --mode=*)  BHATTI_MODE="${1#--mode=}"; shift ;;
            --force)   BHATTI_FORCE=1; shift ;;
            --quiet)   QUIET=1; shift ;;
            --verbose) set -x; shift ;;
            --help|-h) usage; exit 0 ;;
            *) die "unknown flag: $1" ;;
        esac
    done
}

# Allow sourcing for tests without executing main
if [ "${BHATTI_TEST:-}" != "1" ]; then
    parse_flags "$@"
    main
fi
