#!/usr/bin/env bats
# scripts/install_test.bats — Unit tests for scripts/install.sh
#
# Pairs with scripts/install_smoke.bats. The contract:
#   install_test.bats  — isolated helpers + temp paths, no network/system changes
#   install_smoke.bats — end-to-end against a fake release tree
# Together they're the suite that has to be green for "if CI passes,
# install + update works modulo GitHub being down".
#
# Every test below was kept because it traces back to a code path a
# user actually exercises. False-confidence tests (re-implementing the
# function under test, asserting properties of the test runner instead
# of the script) were deleted in this audit. If you find one of those
# creeping back in, the lesson is in the c31d997 / 4286d3a postmortem.

setup() {
    # BHATTI_TEST=1 tells install.sh to skip its script-mode hardening
    # (set -euo pipefail + ERR/EXIT traps). Those would clobber bats'
    # own bats_error_trap and silently turn failed assertions into
    # "missing tests" — see the matching block in scripts/install.sh.
    export BHATTI_TEST=1
    source "${BHATTI_TEST_INSTALL_SH:-scripts/install.sh}"
}

teardown() {
    if [ "${INSTALL_ROOTFS_FIXTURE:-}" = 1 ]; then
        rm -rf "$DATA_DIR"
    fi
    if [ -n "${SECURITY_TMP:-}" ]; then
        rm -rf "$SECURITY_TMP"
    fi
}

# Bats footgun fix: `[[ ]]` is a bash keyword, NOT a simple command, so
# set -e does not trip on it inside test bodies. A failing `[[ ]]` in
# the middle of a test is silently masked if any later command (e.g.
# trailing cleanup, or even the implicit `return 0` of the test body's
# last successful expression) returns 0. Use this helper for
# string-contains assertions — grep is a simple command, so set -e
# actually fails the test on a missed match. The cost: no glob/regex,
# fixed-string only. That's fine; we want strict matches anyway.
output_contains() {
    echo "$output" | grep -qF "$1"
}

# ── Tier consistency ──────────────────────────────────────────────
# These five guard the "all the lists agree" invariant across files.
# Every tier in scripts/tiers/ must appear in build-tier.sh, the
# release.yml matrix, install.sh's interactive menu, and ALL_KNOWN_TIERS.
# When this drifts, releases ship with broken/missing tiers — caught
# before merge instead of after a tag is cut.

@test "tier consistency: every scripts/tiers/*.sh is in build-tier.sh" {
    for tier_script in scripts/tiers/*.sh; do
        tier=$(basename "$tier_script" .sh)
        grep -q "${tier})" scripts/build-tier.sh || {
            echo "MISSING from build-tier.sh: $tier" >&2
            return 1
        }
    done
}

@test "tier consistency: every scripts/tiers/*.sh is in release.yml matrix" {
    for tier_script in scripts/tiers/*.sh; do
        tier=$(basename "$tier_script" .sh)
        grep -q "$tier" .github/workflows/release.yml || {
            echo "MISSING from release.yml: $tier" >&2
            return 1
        }
    done
}

@test "tier consistency: every scripts/tiers/*.sh is in install.sh menu" {
    for tier_script in scripts/tiers/*.sh; do
        tier=$(basename "$tier_script" .sh)
        grep -q "tier=\"${tier}\"" scripts/install.sh || {
            echo "MISSING from install.sh menu: $tier" >&2
            return 1
        }
    done
}

@test "tier consistency: every scripts/tiers/*.sh is in ALL_KNOWN_TIERS" {
    local all_known
    all_known=$(grep '^ALL_KNOWN_TIERS=' scripts/install.sh | head -1 | sed 's/.*"\(.*\)".*/\1/')
    for tier_script in scripts/tiers/*.sh; do
        tier=$(basename "$tier_script" .sh)
        echo "$all_known" | grep -qw "$tier" || {
            echo "MISSING from ALL_KNOWN_TIERS: $tier" >&2
            return 1
        }
    done
}

@test "tier consistency: no phantom tiers in ALL_KNOWN_TIERS without a build script" {
    local all_known
    all_known=$(grep '^ALL_KNOWN_TIERS=' scripts/install.sh | head -1 | sed 's/.*"\(.*\)".*/\1/')
    for tier in $all_known; do
        [ -f "scripts/tiers/${tier}.sh" ] || {
            echo "PHANTOM tier in ALL_KNOWN_TIERS (no script): $tier" >&2
            return 1
        }
    done
}

# ── version_gt ─────────────────────────────────────────────────────
# Used by install_firecracker to decide whether to upgrade FC. Tested
# with three cases — anything beyond is testing the IFS-split machinery
# of bash, not our code. The "missing patch" case is in here because
# v1.14 (FC) and v1.14.0 must compare equal, not as v1.14 < v1.14.0.

@test "version_gt: greater (1.6.3 > 1.6.2, with and without v-prefix)" {
    version_gt v1.6.3 v1.6.2
    version_gt 1.6.3 1.6.2
}

@test "version_gt: equal returns 1 (1.6.3 = 1.6.3 is NOT greater-than)" {
    run version_gt v1.6.3 v1.6.3
    [ "$status" -ne 0 ]
}

@test "version_gt: less returns 1 (1.6.2 < 1.6.3 is NOT greater-than)" {
    run version_gt v1.6.2 v1.6.3
    [ "$status" -ne 0 ]
}

@test "version_gt: missing patch component (v1.0 > v0.9, treats missing as 0)" {
    version_gt v1.0 v0.9
}

# ── resolve_latest_version: BHATTI_VERSION override ──────────────────
# Production env-var path that lets a caller pin a specific tag instead
# of letting GitHub's `releases/latest` decide. The two requirements:
#   1. VERSION is set verbatim from BHATTI_VERSION.
#   2. RELEASE_URL points at GitHub's release-download URL for that tag
#      (NOT influenced by BHATTI_TEST_RELEASE_URL — that override is
#      reserved for the smoke test rig).
# Without #2, a caller could end up downloading binaries from the
# previous test run's fake release tree, which would silently install
# the wrong artifacts.

@test "resolve_latest_version: BHATTI_VERSION pins VERSION and RELEASE_URL to the GitHub tag" {
    BHATTI_VERSION=v1.11.4-rc.1 resolve_latest_version
    [ "$VERSION" = "v1.11.4-rc.1" ]
    [ "$RELEASE_URL" = "https://github.com/sahil-shubham/bhatti/releases/download/v1.11.4-rc.1" ]
}

@test "resolve_latest_version: BHATTI_TEST_VERSION takes precedence over BHATTI_VERSION (test rig wins inside test rig)" {
    # If both are set, the test override must win so the smoke test
    # rig keeps working even if the test environment leaks a real
    # BHATTI_VERSION value.
    BHATTI_TEST_VERSION=v0.0.1-test \
    BHATTI_TEST_RELEASE_URL=file:///tmp/fake \
    BHATTI_VERSION=v1.11.4-rc.1 \
        resolve_latest_version
    [ "$VERSION" = "v0.0.1-test" ]
    [ "$RELEASE_URL" = "file:///tmp/fake" ]
}

# ── crosses_major / major_version ──────────────────────────────────
# Drives the "are you sure?" prompt for v0.x → v1.0.0 type upgrades.
# major_version is a one-liner; one test is enough.

@test "major_version: strips leading v and takes first component" {
    [ "$(major_version v1.6.3)" = "1" ]
    [ "$(major_version 12.0.0)" = "12" ]
    [ "$(major_version v0.5.14)" = "0" ]
}

@test "crosses_major: returns 0 when major differs (v0.5.0 → v1.0.0)" {
    crosses_major v0.5.0 v1.0.0
}

@test "crosses_major: returns 1 when major matches (v1.2.0 → v1.9.0)" {
    run crosses_major v1.2.0 v1.9.0
    [ "$status" -ne 0 ]
}

# ── map_arch ──────────────────────────────────────────────────────
# Every binary and rootfs URL in the script depends on this map. A
# silently-wrong arch means the user downloads the wrong binary and
# the post-download executable check is the only thing that catches it.
# The previous detect_platform tests just asserted "the test runner
# has an arch", which proved nothing — these drive the cases directly.

@test "map_arch: x86_64 (Linux) → amd64" {
    ARCH=""
    map_arch x86_64
    [ "$ARCH" = "amd64" ]
}

@test "map_arch: aarch64 (Linux) → arm64" {
    ARCH=""
    map_arch aarch64
    [ "$ARCH" = "arm64" ]
}

@test "map_arch: arm64 (macOS uname -m on Apple Silicon) → arm64" {
    ARCH=""
    map_arch arm64
    [ "$ARCH" = "arm64" ]
}

@test "map_arch: unknown arch dies with a clear message" {
    run map_arch riscv64
    [ "$status" -ne 0 ]
    output_contains "unsupported architecture"
    output_contains "riscv64"
}

# ── detect_install_type ───────────────────────────────────────────
# Drives the entire branch decision in main(). A regression here would
# silently route a server install through the CLI flow (or vice versa),
# which the user would not notice until they tried to start a sandbox.

@test "detect_install_type: 'none' on a fresh box (no config, no binary)" {
    DATA_DIR=$(mktemp -d)
    PATH=/usr/bin:/bin   # strip out anything that might shadow `bhatti`
    # Sanity: there's no /etc/bhatti/config.yaml on the test runner. If
    # there is, we're in a polluted environment and the test is a lie.
    [ ! -f /etc/bhatti/config.yaml ] || skip "polluted host: /etc/bhatti/config.yaml exists"

    result=$(detect_install_type)
    [ "$result" = "none" ]
    rm -rf "$DATA_DIR"
}

@test "detect_install_type: 'server' when /etc/bhatti/config.yaml exists" {
    [ ! -f /etc/bhatti/config.yaml ] || skip "polluted host: /etc/bhatti/config.yaml exists"
    # We can't write /etc/bhatti from the test, so verify the predicate
    # the function uses by exercising the same shape via the pre-v1.6
    # fallback path: /etc/bhatti missing AND $DATA_DIR/config.yaml present.
    DATA_DIR=$(mktemp -d)
    : > "$DATA_DIR/config.yaml"

    result=$(detect_install_type)
    [ "$result" = "server" ]
    rm -rf "$DATA_DIR"
}

@test "detect_install_type: 'cli' when bhatti is in PATH and no server config" {
    [ ! -f /etc/bhatti/config.yaml ] || skip "polluted host: /etc/bhatti/config.yaml exists"
    DATA_DIR=$(mktemp -d)
    local fakebin=$(mktemp -d)
    : > "$fakebin/bhatti"
    chmod +x "$fakebin/bhatti"
    PATH="$fakebin:$PATH"

    result=$(detect_install_type)
    [ "$result" = "cli" ]
    rm -rf "$DATA_DIR" "$fakebin"
}

# ── detect_tier ───────────────────────────────────────────────────
# Reads the configured tier from /etc/bhatti/config.yaml so update
# pulls the right rootfs. The previous tests were copy-paste of the
# parser inline — this set actually invokes the function via its
# config-path arg.

@test "detect_tier: parses tier name from firecracker_rootfs path" {
    local cfg=$(mktemp)
    ARCH=arm64
    cat > "$cfg" << EOF
firecracker_rootfs: /var/lib/bhatti/images/rootfs-browser-arm64.ext4
EOF
    [ "$(detect_tier "$cfg")" = "browser" ]
    rm -f "$cfg"
}

@test "detect_tier: parses tier from v2 krucible_base_image path" {
    local cfg=$(mktemp)
    ARCH=arm64
    cat > "$cfg" << EOF
engine: krucible
krucible_base_image: /var/lib/bhatti/images/rootfs-docker-arm64.ext4
EOF
    [ "$(detect_tier "$cfg")" = "docker" ]
    rm -f "$cfg"
}

@test "detect_tier: handles double- and single-quoted paths" {
    local cfg=$(mktemp)
    ARCH=amd64
    cat > "$cfg" << EOF
firecracker_rootfs: "/var/lib/bhatti/images/rootfs-docker-amd64.ext4"
EOF
    [ "$(detect_tier "$cfg")" = "docker" ]
    rm -f "$cfg"
}

@test "detect_tier: glob fallback prefers minimal when config absent" {
    DATA_DIR=$(mktemp -d)
    ARCH=arm64
    mkdir -p "$DATA_DIR/images"
    : > "$DATA_DIR/images/rootfs-minimal-arm64.ext4"
    : > "$DATA_DIR/images/rootfs-browser-arm64.ext4"

    # Pass a non-existent config path so the function falls through to glob
    [ "$(detect_tier "$DATA_DIR/no-such-config.yaml")" = "minimal" ]
    rm -rf "$DATA_DIR"
}

# ── is_up_to_date ─────────────────────────────────────────────────
# Used by install_lohar and install_kernel for the skip-if-fresh path.
# A regression here flips the script between "always re-downloads"
# (slow but correct) and "never re-downloads despite version change"
# (silent stale binaries — much worse).

@test "is_up_to_date: matching sha returns 0" {
    local f=$(mktemp)
    echo "hello" > "$f"
    local sha
    if command -v sha256sum >/dev/null; then sha=$(sha256sum "$f" | awk '{print $1}')
    else sha=$(shasum -a 256 "$f" | awk '{print $1}'); fi
    CHECKSUMS="${sha}  some-asset"$'\n'

    is_up_to_date "$f" "some-asset"
    rm -f "$f"
}

@test "is_up_to_date: mismatching sha returns 1" {
    local f=$(mktemp)
    echo "hello" > "$f"
    CHECKSUMS="0000deadbeef0000  some-asset"$'\n'

    run is_up_to_date "$f" "some-asset"
    [ "$status" -ne 0 ]
    rm -f "$f"
}

@test "is_up_to_date: empty CHECKSUMS returns 1 (assume stale, not silently skip)" {
    local f=$(mktemp)
    echo "hello" > "$f"
    CHECKSUMS=""

    run is_up_to_date "$f" "some-asset"
    [ "$status" -ne 0 ]
    rm -f "$f"
}

# ── need_sudo ─────────────────────────────────────────────────────
# This was the function class that had the macOS regression
# (c31d997). The end-to-end flow is in install_smoke.bats; these two
# guard the most basic invariants of the privilege gate.

@test "need_sudo: sets SUDO='' when already root" {
    # Mock id(-u) to simulate root. Function-level override so it
    # only affects this test.
    id() { case "$1" in -u) echo 0 ;; esac; }
    SUDO="should-be-cleared"
    need_sudo "test"
    [ "$SUDO" = "" ]
}

@test "need_sudo: dies with actionable message when sudo missing and not root" {
    id() { case "$1" in -u) echo 1000 ;; esac; }
    # Make `command -v sudo` fail by overriding the builtin
    command() {
        if [ "$1" = "-v" ] && [ "$2" = "sudo" ]; then
            return 1
        fi
        builtin command "$@"
    }
    run need_sudo "do something"
    [ "$status" -ne 0 ]
    output_contains "sudo is required to do something"
}

# ── all_rootfs_up_to_date ─────────────────────────────────────────
# Regression coverage for `bhatti update --tiers <X>` silently
# skipping a stale rootfs because do_server_update only checked file
# existence, not checksum. See 4286d3a for the postmortem.

_setup_rootfs_fixture() {
    DATA_DIR=$(mktemp -d)
    ARCH=arm64
    mkdir -p "$DATA_DIR/images"
}

_teardown_rootfs_fixture() {
    rm -rf "$DATA_DIR"
}

# Stage a rootfs .ext4 plus its sidecar .sha256 (sidecar omitted if $2 empty)
_stage_rootfs() {
    local tier="$1" stored_sha="$2"
    : > "$DATA_DIR/images/rootfs-${tier}-${ARCH}.ext4"
    if [ -n "$stored_sha" ]; then
        echo "$stored_sha" > "$DATA_DIR/images/.rootfs-${tier}-${ARCH}.sha256"
    fi
}

# Build a CHECKSUMS string in the same shape `sha256sum * > checksums-sha256.txt`
# produces. Args: pairs of `tier sha`.
_set_checksums() {
    CHECKSUMS=""
    while [ $# -gt 0 ]; do
        local tier="$1" sha="$2"; shift 2
        CHECKSUMS="${CHECKSUMS}${sha}  rootfs-${tier}-${ARCH}.ext4.zst"$'\n'
    done
}

# Exercise the actual installer against a tiny release image and a CLI with
# only the migration contract. No live data or privileged tools are involved.
_setup_install_rootfs() {
    _setup_rootfs_fixture
    INSTALL_ROOTFS_FIXTURE=1
    mkdir -p "$DATA_DIR/release" "$DATA_DIR/bin"
    RELEASE_URL="file://$DATA_DIR/release"
    BHATTI_TEST_BIN_DEST="$DATA_DIR/bin/bhatti"
    cat > "$BHATTI_TEST_BIN_DEST" <<'SH'
#!/bin/bash
[[ "$1" == admin && "$2" == migrate-images && "$3" == --data-dir ]] || exit 90
[[ "$4" == "$BHATTI_TEST_DATA_DIR" ]] || exit 91
case "${BHATTI_TEST_MIGRATE:-ok}" in
    fail) exit 92 ;;
    leave) exit 0 ;;
esac
for image in "$4"/images/rootfs-*.ext4; do
    [[ -f "$image" && ! -L "$image" ]] || continue
    name=$(basename "$image" .ext4)
    if command -v sha256sum >/dev/null; then
        sha=$(sha256sum "$image" | cut -c1-16)
    else
        sha=$(shasum -a 256 "$image" | cut -c1-16)
    fi
    mkdir -p "$4/images/bases"
    mv "$image" "$4/images/bases/${name}-${sha}.ext4"
    ln -s "bases/${name}-${sha}.ext4" "$image"
done
SH
    chmod +x "$BHATTI_TEST_BIN_DEST"
    export BHATTI_TEST_BIN_DEST BHATTI_TEST_DATA_DIR="$DATA_DIR"
    BHATTI_TEST_MIGRATE=ok
    export BHATTI_TEST_MIGRATE
    check_disk_space() { :; }
}

_release_rootfs() {
    local tier="$1" content="$2" asset="rootfs-${1}-${ARCH}.ext4.zst"
    printf '%s' "$content" > "$DATA_DIR/release/${asset%.zst}"
    zstd -q -f "$DATA_DIR/release/${asset%.zst}" -o "$DATA_DIR/release/$asset"
    CHECKSUMS="$(local_sha256 "$DATA_DIR/release/$asset")  $asset"
}

@test "install_rootfs preserves regular-tier bytes while switching to immutable relative base" {
    _setup_install_rootfs
    _release_rootfs minimal "new rootfs content"
    local tier="$DATA_DIR/images/rootfs-minimal-arm64.ext4"
    printf 'existing VM backing bytes' > "$tier"
    ln "$tier" "$DATA_DIR/old-inode"
    local old_sha new_sha
    old_sha=$(local_sha256 "$tier")
    new_sha=$(local_sha256 "$DATA_DIR/release/rootfs-minimal-arm64.ext4")

    install_rootfs minimal
    [ -L "$tier" ]
    [ "$(readlink "$tier")" = "bases/rootfs-minimal-arm64-${new_sha:0:16}.ext4" ]
    [ "$(local_sha256 "$DATA_DIR/old-inode")" = "$old_sha" ]
    [ "$(local_sha256 "$tier")" = "$new_sha" ]
    [ -f "$DATA_DIR/images/bases/rootfs-minimal-arm64-${old_sha:0:16}.ext4" ]
    [ "$(stat -c %a "$DATA_DIR/images/bases/rootfs-minimal-arm64-${new_sha:0:16}.ext4" 2>/dev/null || stat -f %Lp "$DATA_DIR/images/bases/rootfs-minimal-arm64-${new_sha:0:16}.ext4")" = 644 ]
}

@test "install_rootfs reuses identical base without changing inode or mtime" {
    _setup_install_rootfs
    _release_rootfs minimal "same content, new compression"
    local sha base inode mtime
    sha=$(local_sha256 "$DATA_DIR/release/rootfs-minimal-arm64.ext4")
    base="$DATA_DIR/images/bases/rootfs-minimal-arm64-${sha:0:16}.ext4"
    mkdir -p "$DATA_DIR/images/bases"
    cp "$DATA_DIR/release/rootfs-minimal-arm64.ext4" "$base"
    touch -t 202001010101 "$base"
    inode=$(stat -c %i "$base" 2>/dev/null || stat -f %i "$base")
    mtime=$(stat -c %Y "$base" 2>/dev/null || stat -f %m "$base")

    install_rootfs minimal
    [ "$(stat -c %i "$base" 2>/dev/null || stat -f %i "$base")" = "$inode" ]
    [ "$(stat -c %Y "$base" 2>/dev/null || stat -f %m "$base")" = "$mtime" ]
    [ "$(readlink "$DATA_DIR/images/rootfs-minimal-arm64.ext4")" = "bases/$(basename "$base")" ]
}

@test "install_rootfs refuses to flip tier when migrate-images fails" {
    _setup_install_rootfs
    _release_rootfs minimal "new rootfs content"
    local tier="$DATA_DIR/images/rootfs-minimal-arm64.ext4" old_sha
    printf 'old rootfs content' > "$tier"
    old_sha=$(local_sha256 "$tier")
    BHATTI_TEST_MIGRATE=fail
    ROOTFS_RESTART_ON_REFUSAL=true
    start_service() { printf 'restarted\n' > "$DATA_DIR/restart-marker"; }

    run install_rootfs minimal
    [ "$status" -ne 0 ]
    [ ! -L "$tier" ]
    [ "$(local_sha256 "$tier")" = "$old_sha" ]
    [ ! -f "$DATA_DIR/images/.rootfs-minimal-arm64.sha256" ]
    [ "$(cat "$DATA_DIR/restart-marker")" = restarted ]
    output_contains "previously running bhatti service was restarted"
}

@test "install_rootfs refuses to flip a regular tier left by migration" {
    _setup_install_rootfs
    _release_rootfs minimal "new rootfs content"
    local tier="$DATA_DIR/images/rootfs-minimal-arm64.ext4" old_sha
    printf 'old rootfs content' > "$tier"
    old_sha=$(local_sha256 "$tier")
    BHATTI_TEST_MIGRATE=leave

    run install_rootfs minimal
    [ "$status" -ne 0 ]
    [ ! -L "$tier" ]
    [ "$(local_sha256 "$tier")" = "$old_sha" ]
    [ ! -f "$DATA_DIR/images/.rootfs-minimal-arm64.sha256" ]
}

@test "rootfs helpers follow healthy symlinks and treat dangling symlinks as missing" {
    _setup_install_rootfs
    _set_checksums minimal aaaa1111 browser bbbb2222 docker cccc3333
    mkdir -p "$DATA_DIR/images/bases"
    printf 'healthy' > "$DATA_DIR/images/bases/rootfs-minimal-arm64-healthy.ext4"
    ln -s bases/rootfs-minimal-arm64-healthy.ext4 "$DATA_DIR/images/rootfs-minimal-arm64.ext4"
    printf '%s\n' aaaa1111 > "$DATA_DIR/images/.rootfs-minimal-arm64.sha256"
    ln -s bases/rootfs-browser-arm64-missing.ext4 "$DATA_DIR/images/rootfs-browser-arm64.ext4"
    printf '%s\n' bbbb2222 > "$DATA_DIR/images/.rootfs-browser-arm64.sha256"
    [ "$(detect_tier "$DATA_DIR/no-config.yaml")" = "minimal" ]
    all_rootfs_up_to_date minimal
    run all_rootfs_up_to_date minimal browser
    [ "$status" -ne 0 ]
    [ -z "$(stale_rootfs_tiers minimal)" ]
    [ "$(missing_rootfs_tiers minimal)" = "browser docker computer" ]
    printf '%s\n' stale > "$DATA_DIR/images/.rootfs-minimal-arm64.sha256"
    [ "$(stale_rootfs_tiers browser)" = "minimal" ]
    rm "$DATA_DIR/images/rootfs-minimal-arm64.ext4"
    [ "$(detect_tier "$DATA_DIR/no-config.yaml")" = "minimal" ]

    _release_rootfs minimal "new rootfs content"
    install_rootfs minimal
    [ -L "$DATA_DIR/images/rootfs-minimal-arm64.ext4" ]
    all_rootfs_up_to_date minimal
}

@test "all_rootfs_up_to_date: returns 0 when every requested tier matches release sha" {
    _setup_rootfs_fixture
    _set_checksums minimal aaaa1111 computer bbbb2222
    _stage_rootfs minimal aaaa1111
    _stage_rootfs computer bbbb2222
    all_rootfs_up_to_date minimal computer
    _teardown_rootfs_fixture
}

@test "all_rootfs_up_to_date: regression — stale tier with no sidecar returns 1" {
    # The bug: `bhatti update --tiers computer` left the stale computer
    # rootfs because the gate only checked -f. An .ext4 with no .sha256
    # sidecar is by definition not verified — must be treated as stale.
    _setup_rootfs_fixture
    _set_checksums minimal aaaa1111 computer bbbb2222
    _stage_rootfs minimal aaaa1111
    : > "$DATA_DIR/images/rootfs-computer-arm64.ext4"   # exists, no .sha256
    run all_rootfs_up_to_date minimal computer
    [ "$status" -ne 0 ]
    _teardown_rootfs_fixture
}

@test "all_rootfs_up_to_date: stale tier with mismatching sidecar returns 1" {
    _setup_rootfs_fixture
    _set_checksums minimal aaaa1111 computer bbbb2222
    _stage_rootfs minimal aaaa1111
    _stage_rootfs computer oldoldold
    run all_rootfs_up_to_date minimal computer
    [ "$status" -ne 0 ]
    _teardown_rootfs_fixture
}

@test "all_rootfs_up_to_date: missing tier .ext4 returns 1" {
    _setup_rootfs_fixture
    _set_checksums minimal aaaa1111 computer bbbb2222
    _stage_rootfs minimal aaaa1111
    run all_rootfs_up_to_date minimal computer
    [ "$status" -ne 0 ]
    _teardown_rootfs_fixture
}

@test "all_rootfs_up_to_date: empty CHECKSUMS returns 1 (assume stale)" {
    _setup_rootfs_fixture
    CHECKSUMS=""
    _stage_rootfs minimal aaaa1111
    run all_rootfs_up_to_date minimal
    [ "$status" -ne 0 ]
    _teardown_rootfs_fixture
}

@test "all_rootfs_up_to_date: empty tier list returns 0 (vacuously fresh)" {
    all_rootfs_up_to_date
}

@test "all_rootfs_up_to_date: one fresh + one stale returns 1 (no partial pass)" {
    # Exact shape of the user-reported bug: configured tier (minimal)
    # is fresh, the additional --tiers tier (computer) is stale.
    _setup_rootfs_fixture
    _set_checksums minimal aaaa1111 computer bbbb2222
    _stage_rootfs minimal aaaa1111
    _stage_rootfs computer staleoldsha
    run all_rootfs_up_to_date minimal computer
    [ "$status" -ne 0 ]
    _teardown_rootfs_fixture
}

# ── stale_rootfs_tiers / missing_rootfs_tiers ─────────────────────
# Drive the post-update hint that classifies every "other" tier into
# {stale-on-disk, not-installed}. The user-facing strings depend on
# these returning the right tiers in the right order.

@test "stale_rootfs_tiers: empty when nothing is stale" {
    _setup_rootfs_fixture
    _set_checksums minimal aaaa1111
    _stage_rootfs minimal aaaa1111
    [ -z "$(stale_rootfs_tiers minimal)" ]
    _teardown_rootfs_fixture
}

@test "stale_rootfs_tiers: lists stale-on-disk tier outside the skip list" {
    _setup_rootfs_fixture
    _set_checksums minimal aaaa1111 computer bbbb2222
    _stage_rootfs minimal aaaa1111
    _stage_rootfs computer staleoldsha
    [ "$(stale_rootfs_tiers minimal)" = "computer" ]
    _teardown_rootfs_fixture
}

@test "stale_rootfs_tiers: skip list suppresses just-updated tiers even if sidecar is stale" {
    # Defensive: install_rootfs is the source of truth for tiers we
    # just touched. The hint must trust the skip list, not re-verify.
    _setup_rootfs_fixture
    _set_checksums minimal aaaa1111 computer bbbb2222
    _stage_rootfs minimal staleoldsha
    _stage_rootfs computer staleoldsha
    [ -z "$(stale_rootfs_tiers minimal computer)" ]
    _teardown_rootfs_fixture
}

@test "stale_rootfs_tiers: ordering follows ALL_KNOWN_TIERS, not insertion order" {
    # User-facing output ("outdated on disk: browser, docker") needs
    # deterministic ordering.
    _setup_rootfs_fixture
    _set_checksums minimal a browser b docker c computer d
    _stage_rootfs minimal a
    _stage_rootfs browser stale1
    _stage_rootfs docker  stale2
    [ "$(stale_rootfs_tiers minimal)" = "browser docker" ]
    _teardown_rootfs_fixture
}

@test "missing_rootfs_tiers: lists tiers not on disk, excluding skip list" {
    _setup_rootfs_fixture
    _set_checksums minimal a
    _stage_rootfs minimal a
    [ "$(missing_rootfs_tiers minimal)" = "browser docker computer" ]
    _teardown_rootfs_fixture
}

@test "missing_rootfs_tiers: skip list excludes tiers we just installed" {
    _setup_rootfs_fixture
    _set_checksums minimal a
    _stage_rootfs minimal a
    [ "$(missing_rootfs_tiers minimal browser)" = "docker computer" ]
    _teardown_rootfs_fixture
}

@test "missing_rootfs_tiers: empty when every tier is on disk" {
    _setup_rootfs_fixture
    _set_checksums minimal a browser b docker c computer d
    _stage_rootfs minimal a
    _stage_rootfs browser b
    _stage_rootfs docker c
    _stage_rootfs computer d
    [ -z "$(missing_rootfs_tiers minimal)" ]
    _teardown_rootfs_fixture
}

@test "stale + missing: bucketed UX scenario the hint was added for" {
    # The exact strings the user sees on `bhatti update` after having
    # pulled `computer` previously and never pulled browser/docker.
    _setup_rootfs_fixture
    _set_checksums minimal a computer d
    _stage_rootfs minimal a
    _stage_rootfs computer staleoldsha
    [ "$(stale_rootfs_tiers minimal)"   = "computer" ]
    [ "$(missing_rootfs_tiers minimal)" = "browser docker" ]
    _teardown_rootfs_fixture
}

# ── parse_flags ───────────────────────────────────────────────────
# Each flag is a real shell path the user can hit. Cheap to test, and
# regressions silently send the script down the wrong branch.

@test "parse_flags: --tier browser sets BHATTI_TIER" {
    BHATTI_TIER=""
    parse_flags --tier browser
    [ "$BHATTI_TIER" = "browser" ]
}

@test "parse_flags: --tier=browser (equals syntax)" {
    BHATTI_TIER=""
    parse_flags --tier=browser
    [ "$BHATTI_TIER" = "browser" ]
}

@test "parse_flags: --tiers all sets BHATTI_TIERS" {
    BHATTI_TIERS=""
    parse_flags --tiers all
    [ "$BHATTI_TIERS" = "all" ]
}

@test "parse_flags: --tiers computer,browser (comma list)" {
    BHATTI_TIERS=""
    parse_flags --tiers computer,browser
    [ "$BHATTI_TIERS" = "computer,browser" ]
}

@test "parse_flags: --force sets BHATTI_FORCE=1" {
    BHATTI_FORCE=""
    parse_flags --force
    [ "$BHATTI_FORCE" = "1" ]
}

@test "parse_flags: --quiet sets QUIET=1" {
    QUIET=""
    parse_flags --quiet
    [ "$QUIET" = "1" ]
}

@test "parse_flags: unknown flag exits non-zero" {
    run parse_flags --bogus
    [ "$status" -ne 0 ]
}

@test "parse_flags: explicit flags override pre-set env vars" {
    BHATTI_TIER="minimal"
    parse_flags --tier browser
    [ "$BHATTI_TIER" = "browser" ]
}

# ── output_contains helper self-test ─────────────────────────────
# This guards the helper from a particular regression: someone
# changes it to use `[[ ]]` for ergonomics, re-introducing the
# masking footgun the helper was created to avoid.

@test "output_contains: matches fixed string" {
    output="alpha beta gamma"
    output_contains "beta"
}

@test "output_contains: returns 1 on miss (so set -e fails the test)" {
    output="alpha beta gamma"
    run output_contains "delta"
    [ "$status" -ne 0 ]
}

@test "parse_flags: --help exits 0 and prints Usage" {
    run parse_flags --help
    [ "$status" -eq 0 ]
    output_contains "Usage:"
}

# ── Local API security ────────────────────────────────────────────
# Source the script and exercise its real config/update helpers with temporary
# paths. Never run the full installer or mutate system groups/service files.
_security_fixture() {
    SECURITY_TMP=$(mktemp -d)
    CONFIG_DIR="$SECURITY_TMP/etc"
    DATA_DIR="$SECURITY_TMP/data"
    RUNTIME_DIR="$DATA_DIR/runtime"
    mkdir -p "$CONFIG_DIR" "$RUNTIME_DIR/kernel"
    chmod 0755 "$DATA_DIR"
    : > "$RUNTIME_DIR/kernel/Image-lean-test"
    API_GROUP_GID=4242
    OS=linux
    ARCH=arm64
    SUDO_USER=""
}

@test "generate_config uses a group-owned Unix API socket, no TCP listen, and accurate sibling policy" {
    _security_fixture
    generate_config minimal
    local cfg="$CONFIG_DIR/config.yaml"
    grep -qx "data_dir: $DATA_DIR" "$cfg"
    grep -qx "api_socket: $DATA_DIR/api.sock" "$cfg"
    grep -qx "api_socket_gid: 4242" "$cfg"
    grep -q "denied by default and requires an explicit network-policy opt-in" "$cfg"
    run grep -qE '^[[:space:]]*listen:' "$cfg"
    [ "$status" -ne 0 ]
}

@test "create_admin_user writes token-only, private CLI configs for sudo user and root" {
    _security_fixture
    mkdir -p "$SECURITY_TMP/user" "$SECURITY_TMP/root"
    SUDO_USER=operator
    bhatti() { echo "API key: bht_test_secret"; }
    getent() { [ "$1" = passwd ] && printf 'operator:x:1000:1000:Operator:%s/user:/bin/bash\n' "$SECURITY_TMP"; }
    id() { [ "$1" = -gn ] && echo staff; }
    eval() { printf '%s\n' "$SECURITY_TMP/root"; }
    chown() { :; }

    create_admin_user
    [ "$(cat "$SECURITY_TMP/user/.bhatti/config.yaml")" = 'auth_token: bht_test_secret' ]
    [ "$(cat "$SECURITY_TMP/root/.bhatti/config.yaml")" = 'auth_token: bht_test_secret' ]
    [ "$(stat -c %a "$SECURITY_TMP/user/.bhatti/config.yaml" 2>/dev/null || stat -f %Lp "$SECURITY_TMP/user/.bhatti/config.yaml")" = 600 ]
    [ "$(stat -c %a "$SECURITY_TMP/root/.bhatti/config.yaml" 2>/dev/null || stat -f %Lp "$SECURITY_TMP/root/.bhatti/config.yaml")" = 600 ]
}

@test "start_service probes the local socket even when domain mode has no TCP listener" {
    _security_fixture
    systemctl() { [ "$*" = "enable --now bhatti" ]; }
    curl() {
        printf '%s\n' "$*" > "$SECURITY_TMP/curl-args"
        [ "$*" = "--unix-socket $DATA_DIR/api.sock -sf http://localhost/health" ]
    }

    start_service
    [ "$(cat "$SECURITY_TMP/curl-args")" = "--unix-socket $DATA_DIR/api.sock -sf http://localhost/health" ]
}

@test "ensure_api_group on Linux creates group and sudo membership only once" {
    _security_fixture
    SUDO_USER=operator
    getent() {
        [ "$1" = group ] && [ "$2" = bhatti ] && [ -f "$SECURITY_TMP/group" ] &&
            echo 'bhatti:x:3131:'
    }
    groupadd() {
        [ "$*" = "--system bhatti" ] || return 1
        echo groupadd >> "$SECURITY_TMP/calls"
        : > "$SECURITY_TMP/group"
    }
    id() {
        [ "$1" = -nG ] || return 1
        if [ -f "$SECURITY_TMP/member" ]; then echo 'staff bhatti'; else echo staff; fi
    }
    usermod() {
        [ "$*" = "-aG bhatti operator" ] || return 1
        echo usermod >> "$SECURITY_TMP/calls"
        : > "$SECURITY_TMP/member"
    }

    ensure_api_group
    [ "$API_GROUP_GID" = 3131 ]
    [ "$API_GROUP_USER_ADDED" = true ]
    ensure_api_group
    [ "$API_GROUP_USER_ADDED" = false ]
    [ "$(grep -c '^groupadd$' "$SECURITY_TMP/calls")" -eq 1 ]
    [ "$(grep -c '^usermod$' "$SECURITY_TMP/calls")" -eq 1 ]
}

@test "ensure_api_group Linux tells new members to start a new login session" {
    _security_fixture
    SUDO_USER=operator
    linux_api_group_gid() { printf '3131\n'; }
    id() { printf 'staff\n'; }
    usermod() { :; }

    run ensure_api_group
    [ "$status" -eq 0 ]
    output_contains "log out and back in"
}


@test "ensure_api_group on macOS creates group and sudo membership only once" {
    _security_fixture
    OS=darwin
    SUDO_USER=operator
    dscl() {
        [ "$*" = ". -read /Groups/bhatti PrimaryGroupID" ] || return 1
        [ -f "$SECURITY_TMP/group" ] && echo 'PrimaryGroupID: 4040'
    }
    dseditgroup() {
        case "$*" in
            "-o create bhatti") echo create >> "$SECURITY_TMP/calls"; : > "$SECURITY_TMP/group" ;;
            "-o edit -a operator -t user bhatti") echo edit >> "$SECURITY_TMP/calls"; : > "$SECURITY_TMP/member" ;;
            *) return 1 ;;
        esac
    }
    id() {
        [ "$1" = -nG ] || return 1
        if [ -f "$SECURITY_TMP/member" ]; then echo 'staff bhatti'; else echo staff; fi
    }

    ensure_api_group
    [ "$API_GROUP_GID" = 4040 ]
    [ "$API_GROUP_USER_ADDED" = true ]
    ensure_api_group
    [ "$API_GROUP_USER_ADDED" = false ]
    [ "$(grep -c '^create$' "$SECURITY_TMP/calls")" -eq 1 ]
    [ "$(grep -c '^edit$' "$SECURITY_TMP/calls")" -eq 1 ]
}

@test "ensure_api_group macOS does not tell new members to log out" {
    _security_fixture
    OS=darwin
    SUDO_USER=operator
    dscl() { printf 'PrimaryGroupID: 4040\n'; }
    id() { printf 'staff\n'; }
    dseditgroup() { :; }

    run ensure_api_group
    [ "$status" -eq 0 ]
    output_contains "added to bhatti group"
    if echo "$output" | grep -qF "log out"; then
        echo "macOS group instructions incorrectly require a new login: $output" >&2
        return 1
    fi
}

@test "purge removes only a token-only invoking user CLI config" {
    _security_fixture
    export HOME="$SECURITY_TMP/user"
    mkdir -p "$HOME/.bhatti"
    printf 'auth_token: local-key\n' > "$HOME/.bhatti/config.yaml"
    printf 'remote credential\n' > "$HOME/.bhatti/other-server"
    SUDO_USER=operator
    source scripts/uninstall.sh
    eval() { printf '%s\n' "$HOME"; }

    run purge_invoking_user_cli_config
    [ "$status" -eq 0 ]
    [ ! -e "$HOME/.bhatti/config.yaml" ]
    [ "$(cat "$HOME/.bhatti/other-server")" = "remote credential" ]
    output_contains "Keeping $HOME/.bhatti"
}

@test "purge preserves invoking user remote CLI config and other files" {
    _security_fixture
    export HOME="$SECURITY_TMP/user"
    mkdir -p "$HOME/.bhatti"
    printf 'api_url: https://other.example.test\nauth_token: remote-key\n' > "$HOME/.bhatti/config.yaml"
    printf 'remote credential\n' > "$HOME/.bhatti/other-server"
    SUDO_USER=operator
    source scripts/uninstall.sh
    eval() { printf '%s\n' "$HOME"; }

    run purge_invoking_user_cli_config
    [ "$status" -eq 0 ]
    grep -qx 'api_url: https://other.example.test' "$HOME/.bhatti/config.yaml"
    [ "$(cat "$HOME/.bhatti/other-server")" = "remote credential" ]
    output_contains "Keeping $HOME/.bhatti/config.yaml"
    output_contains "api_url"
}


@test "update_api_security appends gid once and warns about untouched plaintext listen" {
    _security_fixture
    printf 'engine: krucible\nlisten: :8080\n# api_socket_gid: 5' > "$CONFIG_DIR/config.yaml"

    run update_api_security
    [ "$status" -eq 0 ]
    output_contains "Existing listen: :8080 exposes a plaintext API; configuration left unchanged."
    [ "$(grep -c '^api_socket_gid:' "$CONFIG_DIR/config.yaml")" -eq 1 ]
    grep -qx 'listen: :8080' "$CONFIG_DIR/config.yaml"
    grep -qx '# api_socket_gid: 5' "$CONFIG_DIR/config.yaml"
    grep -qx 'api_socket_gid: 4242' "$CONFIG_DIR/config.yaml"
    run update_api_security
    [ "$status" -eq 0 ]
    [ "$(grep -c '^api_socket_gid:' "$CONFIG_DIR/config.yaml")" -eq 1 ]
}

@test "update_api_security preserves existing gid and ignores commented or empty listen" {
    _security_fixture
    printf 'api_socket_gid: 999\n# listen: :8080\nlisten: ""\n' > "$CONFIG_DIR/config.yaml"

    run update_api_security
    [ "$status" -eq 0 ]
    [ -z "$output" ]
    [ "$(grep -c '^api_socket_gid:' "$CONFIG_DIR/config.yaml")" -eq 1 ]
    grep -qx 'api_socket_gid: 999' "$CONFIG_DIR/config.yaml"
}

@test "update_api_security appends to deprecated data-dir config without replacing it" {
    _security_fixture
    printf 'engine: krucible\nlisten: ":8080" # retained\n' > "$DATA_DIR/config.yaml"

    run update_api_security
    [ "$status" -eq 0 ]
    output_contains "Existing listen: :8080 exposes a plaintext API"
    [ ! -f "$CONFIG_DIR/config.yaml" ]
    grep -qx 'listen: ":8080" # retained' "$DATA_DIR/config.yaml"
    grep -qx 'api_socket_gid: 4242' "$DATA_DIR/config.yaml"
}

@test "update_api_security preserves loopback listeners without warning" {
    _security_fixture
    local listen
    for listen in 'localhost:8080' '127.42.0.1:8080' "'[::1]:8080'" '"::1:8080"'; do
        printf 'listen: %s\n' "$listen" > "$CONFIG_DIR/config.yaml"
        run update_api_security
        [ "$status" -eq 0 ]
        [ -z "$output" ]
        grep -qxF "listen: $listen" "$CONFIG_DIR/config.yaml"
        grep -qx 'api_socket_gid: 4242' "$CONFIG_DIR/config.yaml"
    done
}

@test "migrate_local_cli_configs removes legacy URL but preserves tokens and remote URLs" {
    _security_fixture
    mkdir -p "$SECURITY_TMP/root/.bhatti" "$SECURITY_TMP/user/.bhatti"
    SUDO_USER=operator
    eval() { printf '%s\n' "$SECURITY_TMP/root"; }
    getent() { [ "$1" = passwd ] && printf 'operator:x:1000:1000:Operator:%s/user:/bin/bash\n' "$SECURITY_TMP"; }
    printf 'api_url: https://api.example.test\nauth_token: root-token\n' > "$SECURITY_TMP/root/.bhatti/config.yaml"
    printf 'api_url: http://localhost:8080\nauth_token: user-token\n' > "$SECURITY_TMP/user/.bhatti/config.yaml"

    migrate_local_cli_configs
    [ "$(cat "$SECURITY_TMP/root/.bhatti/config.yaml")" = $'api_url: https://api.example.test\nauth_token: root-token' ]
    [ "$(cat "$SECURITY_TMP/user/.bhatti/config.yaml")" = 'auth_token: user-token' ]
}

@test "migrate_local_cli_configs keeps the invoking user's localhost URL while the server listens on TCP" {
    _security_fixture
    mkdir -p "$SECURITY_TMP/root/.bhatti" "$SECURITY_TMP/user/.bhatti"
    SUDO_USER=operator
    eval() { printf '%s\n' "$SECURITY_TMP/root"; }
    getent() { [ "$1" = passwd ] && printf 'operator:x:1000:1000:Operator:%s/user:/bin/bash\n' "$SECURITY_TMP"; }
    printf 'engine: krucible\nlisten: :8080\n' > "$CONFIG_DIR/config.yaml"
    printf 'api_url: http://localhost:8080\nauth_token: root-token\n' > "$SECURITY_TMP/root/.bhatti/config.yaml"
    printf 'api_url: http://localhost:8080\nauth_token: user-token\n' > "$SECURITY_TMP/user/.bhatti/config.yaml"

    migrate_local_cli_configs
    # root reaches the socket regardless; the user would need a fresh login first.
    [ "$(cat "$SECURITY_TMP/root/.bhatti/config.yaml")" = 'auth_token: root-token' ]
    [ "$(cat "$SECURITY_TMP/user/.bhatti/config.yaml")" = $'api_url: http://localhost:8080\nauth_token: user-token' ]
}

@test "same-version server update adds gid and keeps loopback config without full install" {
    _security_fixture
    cat > "$CONFIG_DIR/config.yaml" <<EOF
engine: krucible
listen: 127.0.0.1:8080
krucible_base_image: $DATA_DIR/images/rootfs-minimal-arm64.ext4
EOF
    mkdir -p "$SECURITY_TMP/root/.bhatti"
    printf 'api_url: http://localhost:8080\nauth_token: local-token\n' > "$SECURITY_TMP/root/.bhatti/config.yaml"
    eval() { printf '%s\n' "$SECURITY_TMP/root"; }
    mkdir -p "$RUNTIME_DIR/bin" "$RUNTIME_DIR/lib" "$DATA_DIR/images"
    : > "$RUNTIME_DIR/bin/bhatti-vmm"
    : > "$RUNTIME_DIR/bin/bhatti-netd"
    : > "$RUNTIME_DIR/lib/libkrun.so"
    : > "$DATA_DIR/images/rootfs-minimal-arm64.ext4"
    printf 'expected\n' > "$DATA_DIR/images/.rootfs-minimal-arm64.sha256"
    CHECKSUMS='expected  rootfs-minimal-arm64.ext4.zst'
    BHATTI_TEST_BIN_DEST="$SECURITY_TMP/bhatti"
    : > "$BHATTI_TEST_BIN_DEST"
    VERSION=v2.0.0
    unset BHATTI_TIERS
    id() { [ "$1" = -u ] && echo 0; }
    installed_bhatti_version() { echo v2.0.0; }
    is_firecracker_install() { return 1; }
    ensure_api_group() { API_GROUP_GID=4242; }
    install_bundle() { echo "unexpected install_bundle" >&2; return 1; }
    systemctl() { echo "unexpected systemctl" >&2; return 1; }

    run do_server_update
    [ "$status" -eq 0 ] || { echo "$output" >&2; return 1; }
    output_contains "is already up to date"
    grep -qx 'api_socket_gid: 4242' "$CONFIG_DIR/config.yaml"
    grep -qx 'listen: 127.0.0.1:8080' "$CONFIG_DIR/config.yaml"
    [ "$(cat "$SECURITY_TMP/root/.bhatti/config.yaml")" = 'auth_token: local-token' ]
    if echo "$output" | grep -qF "Existing listen:"; then
        echo "loopback listener was incorrectly treated as network-bound: $output" >&2
        return 1
    fi
}

@test "private existing data dir warns instead of changing permissions" {
    _security_fixture
    OS=$(uname -s | tr '[:upper:]' '[:lower:]')
    chmod 0700 "$DATA_DIR"
    run warn_if_socket_dir_private
    [ "$status" -eq 0 ]
    output_contains "not traversable by the bhatti group"
    [ "$(stat -c %a "$DATA_DIR" 2>/dev/null || stat -f %Lp "$DATA_DIR")" = 700 ]
    chmod 0755 "$DATA_DIR"
    run warn_if_socket_dir_private
    [ "$status" -eq 0 ]
    [ -z "$output" ]
}

@test "purge group helper removes Linux API group idempotently" {
    _security_fixture
    source scripts/uninstall.sh
    uname() { echo Linux; }
    getent() { [ "$*" = "group bhatti" ] && [ -f "$SECURITY_TMP/group" ]; }
    groupdel() { [ "$*" = bhatti ] && rm -f "$SECURITY_TMP/group" && echo groupdel >> "$SECURITY_TMP/calls"; }
    : > "$SECURITY_TMP/group"
    remove_api_group
    [ -f "$SECURITY_TMP/group" ]
    PURGE=true
    remove_api_group
    remove_api_group
    [ "$(grep -c '^groupdel$' "$SECURITY_TMP/calls")" -eq 1 ]
}

@test "purge group helper removes macOS API group idempotently" {
    _security_fixture
    source scripts/uninstall.sh
    uname() { echo Darwin; }
    dscl() { [ "$*" = ". -read /Groups/bhatti PrimaryGroupID" ] && [ -f "$SECURITY_TMP/group" ]; }
    dseditgroup() { [ "$*" = "-o delete bhatti" ] && rm -f "$SECURITY_TMP/group" && echo delete >> "$SECURITY_TMP/calls"; }
    : > "$SECURITY_TMP/group"
    remove_api_group
    [ -f "$SECURITY_TMP/group" ]
    PURGE=true
    remove_api_group
    remove_api_group
    [ "$(grep -c '^delete$' "$SECURITY_TMP/calls")" -eq 1 ]
}
