#!/usr/bin/env bats
# scripts/install_test.bats — Unit tests for scripts/install.sh
#
# Pairs with scripts/install_smoke.bats. The contract:
#   install_test.bats  — pure helpers, sub-second, no network/disk
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
    source scripts/install.sh
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

@test "map_arch: x86_64 (Linux) → amd64 / x86_64" {
    ARCH=""; FC_ARCH=""
    map_arch x86_64
    [ "$ARCH" = "amd64" ]
    [ "$FC_ARCH" = "x86_64" ]
}

@test "map_arch: aarch64 (Linux) → arm64 / aarch64" {
    ARCH=""; FC_ARCH=""
    map_arch aarch64
    [ "$ARCH" = "arm64" ]
    [ "$FC_ARCH" = "aarch64" ]
}

@test "map_arch: arm64 (macOS uname -m on Apple Silicon) → arm64 / aarch64" {
    ARCH=""; FC_ARCH=""
    map_arch arm64
    [ "$ARCH" = "arm64" ]
    [ "$FC_ARCH" = "aarch64" ]
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
