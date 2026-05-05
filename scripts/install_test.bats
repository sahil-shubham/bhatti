#!/usr/bin/env bats
# scripts/install_test.bats — Tests for install.sh
#
# Run: bats scripts/install_test.bats
# Requires: bats-core (https://github.com/bats-core/bats-core)
#
# These tests source install.sh with BHATTI_TEST=1 to access functions
# without running main(). No root, no network, sub-second.

setup() {
    export BHATTI_TEST=1
    # Prevent ERR trap from interfering with bats
    trap - ERR
    source scripts/install.sh
}

# ── Tier consistency ──────────────────────────────────
# Every tier in scripts/tiers/ must appear in all of:
#   - scripts/build-tier.sh (SIZE_MB case statement)
#   - .github/workflows/release.yml (matrix)
#   - scripts/install.sh (interactive menu)
#   - scripts/install.sh (ALL_KNOWN_TIERS)

@test "tier consistency: all tiers in scripts/tiers/ are in build-tier.sh" {
    for tier_script in scripts/tiers/*.sh; do
        tier=$(basename "$tier_script" .sh)
        grep -q "${tier})" scripts/build-tier.sh || {
            echo "MISSING from build-tier.sh: $tier" >&2
            return 1
        }
    done
}

@test "tier consistency: all tiers in scripts/tiers/ are in release.yml matrix" {
    for tier_script in scripts/tiers/*.sh; do
        tier=$(basename "$tier_script" .sh)
        grep -q "$tier" .github/workflows/release.yml || {
            echo "MISSING from release.yml: $tier" >&2
            return 1
        }
    done
}

@test "tier consistency: all tiers in scripts/tiers/ are in install.sh menu" {
    for tier_script in scripts/tiers/*.sh; do
        tier=$(basename "$tier_script" .sh)
        grep -q "tier=\"${tier}\"" scripts/install.sh || {
            echo "MISSING from install.sh menu: $tier" >&2
            return 1
        }
    done
}

@test "tier consistency: all tiers in scripts/tiers/ are in ALL_KNOWN_TIERS" {
    local all_known
    all_known=$(grep 'ALL_KNOWN_TIERS=' scripts/install.sh | head -1 | sed 's/.*"\(.*\)".*/\1/')
    for tier_script in scripts/tiers/*.sh; do
        tier=$(basename "$tier_script" .sh)
        echo "$all_known" | grep -qw "$tier" || {
            echo "MISSING from ALL_KNOWN_TIERS: $tier" >&2
            return 1
        }
    done
}

@test "tier consistency: no phantom tiers in ALL_KNOWN_TIERS" {
    local all_known
    all_known=$(grep 'ALL_KNOWN_TIERS=' scripts/install.sh | head -1 | sed 's/.*"\(.*\)".*/\1/')
    for tier in $all_known; do
        [ -f "scripts/tiers/${tier}.sh" ] || {
            echo "PHANTOM tier in ALL_KNOWN_TIERS (no script): $tier" >&2
            return 1
        }
    done
}

# ── version_gt ────────────────────────────────────────

@test "version_gt: v1.6.3 > v1.6.2" {
    version_gt v1.6.3 v1.6.2
}

@test "version_gt: v2.0.0 > v1.99.99" {
    version_gt v2.0.0 v1.99.99
}

@test "version_gt: v1.6.3 = v1.6.3 (not greater)" {
    run version_gt v1.6.3 v1.6.3
    [ "$status" -ne 0 ]
}

@test "version_gt: v1.6.2 < v1.6.3 (not greater)" {
    run version_gt v1.6.2 v1.6.3
    [ "$status" -ne 0 ]
}

@test "version_gt: no v-prefix (1.6.3 > 1.6.2)" {
    version_gt 1.6.3 1.6.2
}

@test "version_gt: missing patch (v1.0 > v0.9)" {
    version_gt v1.0 v0.9
}

@test "version_gt: major bump (v1.0.0 > v0.99.99)" {
    version_gt v1.0.0 v0.99.99
}

@test "version_gt: patch only (v1.0.1 > v1.0.0)" {
    version_gt v1.0.1 v1.0.0
}

# ── crosses_major ─────────────────────────────────────

@test "crosses_major: v0.5.0 to v1.0.0" {
    crosses_major v0.5.0 v1.0.0
}

@test "crosses_major: same major (v1.2.0 to v1.9.0) returns false" {
    run crosses_major v1.2.0 v1.9.0
    [ "$status" -ne 0 ]
}

# ── all_rootfs_up_to_date ─────────────────────────────────────────
# Regression coverage for: `bhatti update --tiers <X>` silently skipping
# a stale rootfs because do_server_update only checked file existence,
# not checksum, before short-circuiting on "already up to date".
#
# These tests drive all_rootfs_up_to_date() against a fake $DATA_DIR
# tree. They exercise the predicate the outer gate now uses, so a future
# refactor that re-introduces the existence-only check will go red here.

_setup_rootfs_fixture() {
    DATA_DIR=$(mktemp -d)
    ARCH=arm64
    mkdir -p "$DATA_DIR/images"
}

_teardown_rootfs_fixture() {
    rm -rf "$DATA_DIR"
}

# Stage a rootfs .ext4 plus its sidecar .sha256 (sidecar omitted if $2 is empty)
_stage_rootfs() {
    local tier="$1" stored_sha="$2"
    : > "$DATA_DIR/images/rootfs-${tier}-${ARCH}.ext4"
    if [ -n "$stored_sha" ]; then
        echo "$stored_sha" > "$DATA_DIR/images/.rootfs-${tier}-${ARCH}.sha256"
    fi
}

# Build a CHECKSUMS string in the same shape `sha256sum * > checksums-sha256.txt`
# produces (used by remote_sha256). Pairs of: tier sha
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
    # The bug: `bhatti update` then `bhatti update --tiers computer` left
    # the stale computer rootfs because the gate only checked -f.
    # An .ext4 with no .sha256 sidecar is by definition not verified —
    # treat it as stale so install_rootfs gets a chance to refresh it.
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
    _stage_rootfs computer oldoldold     # stored sha != remote bbbb2222
    run all_rootfs_up_to_date minimal computer
    [ "$status" -ne 0 ]
    _teardown_rootfs_fixture
}

@test "all_rootfs_up_to_date: missing tier .ext4 returns 1" {
    _setup_rootfs_fixture
    _set_checksums minimal aaaa1111 computer bbbb2222
    _stage_rootfs minimal aaaa1111
    # computer .ext4 not staged at all
    run all_rootfs_up_to_date minimal computer
    [ "$status" -ne 0 ]
    _teardown_rootfs_fixture
}

@test "all_rootfs_up_to_date: empty CHECKSUMS returns 1 (assume stale, never silently skip)" {
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

@test "all_rootfs_up_to_date: single tier happy path" {
    _setup_rootfs_fixture
    _set_checksums minimal aaaa1111
    _stage_rootfs minimal aaaa1111
    all_rootfs_up_to_date minimal
    _teardown_rootfs_fixture
}

@test "all_rootfs_up_to_date: one fresh + one stale returns 1 (no partial pass)" {
    # The exact shape of the user-reported bug: configured tier (minimal)
    # is fresh, the additional --tiers tier (computer) is stale. Gate
    # must NOT pass.
    _setup_rootfs_fixture
    _set_checksums minimal aaaa1111 computer bbbb2222
    _stage_rootfs minimal aaaa1111      # fresh
    _stage_rootfs computer staleoldsha  # stale
    run all_rootfs_up_to_date minimal computer
    [ "$status" -ne 0 ]
    _teardown_rootfs_fixture
}

# ── stale_rootfs_tiers / missing_rootfs_tiers ───────────────────────────────
# Drives the post-update hint that classifies every "other" tier (not
# in this update's $tiers_to_install) into:
#   stale-on-disk — user has a tier image from a previous release
#   not-installed — user never pulled this tier
# Empty buckets => no hint shown. The exact UX scenario covered by
# 'lists stale on disk excluded from skip list' is the one a user hits
# when they run a plain `bhatti update` after having pulled `computer`
# in a previous release — prior to this hint, they'd get no signal.

@test "stale_rootfs_tiers: empty when nothing is stale" {
    _setup_rootfs_fixture
    _set_checksums minimal aaaa1111
    _stage_rootfs minimal aaaa1111
    result=$(stale_rootfs_tiers minimal)
    [ -z "$result" ]
    _teardown_rootfs_fixture
}

@test "stale_rootfs_tiers: lists stale-on-disk tier outside the skip list" {
    # Post-update UX scenario: minimal just updated (skip), computer on
    # disk but stale, browser/docker not on disk — expect just "computer".
    _setup_rootfs_fixture
    _set_checksums minimal aaaa1111 computer bbbb2222
    _stage_rootfs minimal aaaa1111
    _stage_rootfs computer staleoldsha
    result=$(stale_rootfs_tiers minimal)
    [ "$result" = "computer" ]
    _teardown_rootfs_fixture
}

@test "stale_rootfs_tiers: skip list suppresses just-updated tiers even when sidecar is stale" {
    # Defensive: install_rootfs is the source of truth for freshness of
    # tiers we just touched. The hint must trust the skip list and not
    # second-guess in-flight installs by re-checking sidecars.
    _setup_rootfs_fixture
    _set_checksums minimal aaaa1111 computer bbbb2222
    _stage_rootfs minimal staleoldsha    # would be stale by sidecar
    _stage_rootfs computer staleoldsha   # would be stale by sidecar
    result=$(stale_rootfs_tiers minimal computer)
    [ -z "$result" ]
    _teardown_rootfs_fixture
}

@test "stale_rootfs_tiers: returns multiple tiers in ALL_KNOWN_TIERS order" {
    # Output ordering is user-facing (becomes 'outdated on disk: ...'),
    # so it must be deterministic.
    _setup_rootfs_fixture
    _set_checksums minimal a browser b docker c computer d
    _stage_rootfs minimal a              # fresh, in skip list anyway
    _stage_rootfs browser  stale1
    _stage_rootfs docker   stale2
    # computer not on disk
    result=$(stale_rootfs_tiers minimal)
    [ "$result" = "browser docker" ]
    _teardown_rootfs_fixture
}

@test "missing_rootfs_tiers: lists tiers not on disk, excluding skip list" {
    _setup_rootfs_fixture
    _set_checksums minimal a
    _stage_rootfs minimal a
    result=$(missing_rootfs_tiers minimal)
    [ "$result" = "browser docker computer" ]
    _teardown_rootfs_fixture
}

@test "missing_rootfs_tiers: skip list excludes tiers we just installed" {
    # If --tiers browser was just run, browser ends up in the skip list.
    # It should not show up as "not installed" even if for some reason
    # the .ext4 isn't on disk yet (e.g. mid-install hint inspection).
    _setup_rootfs_fixture
    _set_checksums minimal a
    _stage_rootfs minimal a
    result=$(missing_rootfs_tiers minimal browser)
    [ "$result" = "docker computer" ]
    _teardown_rootfs_fixture
}

@test "missing_rootfs_tiers: empty when every tier is on disk" {
    _setup_rootfs_fixture
    _set_checksums minimal a browser b docker c computer d
    _stage_rootfs minimal a
    _stage_rootfs browser b
    _stage_rootfs docker c
    _stage_rootfs computer d
    result=$(missing_rootfs_tiers minimal)
    [ -z "$result" ]
    _teardown_rootfs_fixture
}

@test "stale + missing: bucketed UX scenario the hint was added for" {
    # End-to-end check on the user-reported flow: minimal just updated,
    # computer left over and stale, browser/docker never installed.
    # The two helpers together drive the hint block; verifying both at
    # once locks in the exact strings the user sees.
    _setup_rootfs_fixture
    _set_checksums minimal a computer d
    _stage_rootfs minimal a              # fresh
    _stage_rootfs computer staleoldsha   # stale
    # browser, docker absent
    local stale missing
    stale=$(stale_rootfs_tiers minimal)
    missing=$(missing_rootfs_tiers minimal)
    [ "$stale" = "computer" ]
    [ "$missing" = "browser docker" ]
    _teardown_rootfs_fixture
}

# ── detect_tier ────────────────────────────────────

@test "detect_tier: parses browser from config" {
    local tmpdir
    tmpdir=$(mktemp -d)
    mkdir -p "$tmpdir/etc/bhatti" "$tmpdir/images"
    ARCH=arm64  # set explicitly for parsing

    cat > "$tmpdir/etc/bhatti/config.yaml" << EOF
firecracker_rootfs: /var/lib/bhatti/images/rootfs-browser-arm64.ext4
EOF

    # detect_tier reads /etc/bhatti/config.yaml which we can't override,
    # so test the parsing logic directly (same code as detect_tier)
    local rootfs_path tier
    rootfs_path=$(grep '^firecracker_rootfs:' "$tmpdir/etc/bhatti/config.yaml" | awk '{print $2}' | tr -d '"' | tr -d "'")
    tier=$(basename "$rootfs_path" | sed "s/rootfs-//;s/-${ARCH}\.ext4//")
    rm -rf "$tmpdir"
    [ "$tier" = "browser" ]
}

@test "detect_tier: parses computer from config" {
    local tmpdir
    tmpdir=$(mktemp -d)
    ARCH=amd64

    cat > "$tmpdir/config.yaml" << EOF
firecracker_rootfs: /var/lib/bhatti/images/rootfs-computer-amd64.ext4
EOF

    local rootfs_path tier
    rootfs_path=$(grep '^firecracker_rootfs:' "$tmpdir/config.yaml" | awk '{print $2}' | tr -d '"' | tr -d "'")
    tier=$(basename "$rootfs_path" | sed "s/rootfs-//;s/-${ARCH}\.ext4//")
    rm -rf "$tmpdir"
    [ "$tier" = "computer" ]
}

@test "detect_tier: handles quoted path" {
    local tmpdir
    tmpdir=$(mktemp -d)
    ARCH=arm64

    cat > "$tmpdir/config.yaml" << EOF
firecracker_rootfs: "/var/lib/bhatti/images/rootfs-docker-arm64.ext4"
EOF

    local rootfs_path tier
    rootfs_path=$(grep '^firecracker_rootfs:' "$tmpdir/config.yaml" | awk '{print $2}' | tr -d '"' | tr -d "'")
    tier=$(basename "$rootfs_path" | sed "s/rootfs-//;s/-${ARCH}\.ext4//")
    rm -rf "$tmpdir"
    [ "$tier" = "docker" ]
}

# ── detect_platform ───────────────────────────────────

@test "detect_platform: sets OS and ARCH" {
    detect_platform
    [ -n "$OS" ]
    [ -n "$ARCH" ]
    [ -n "$HOST_ARCH" ]
}

@test "detect_platform: ARCH is amd64 or arm64" {
    detect_platform
    [[ "$ARCH" = "amd64" || "$ARCH" = "arm64" ]]
}

# ── major_version ─────────────────────────────────────

@test "major_version: v1.6.3 → 1" {
    result=$(major_version v1.6.3)
    [ "$result" = "1" ]
}

@test "major_version: v0.5.14 → 0" {
    result=$(major_version v0.5.14)
    [ "$result" = "0" ]
}

@test "major_version: 12.0.0 → 12" {
    result=$(major_version 12.0.0)
    [ "$result" = "12" ]
}

# ── Flag parsing ─────────────────────────────────────

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

@test "parse_flags: --tiers computer,browser" {
    BHATTI_TIERS=""
    parse_flags --tiers computer,browser
    [ "$BHATTI_TIERS" = "computer,browser" ]
}

@test "parse_flags: --force sets BHATTI_FORCE" {
    BHATTI_FORCE=""
    parse_flags --force
    [ "$BHATTI_FORCE" = "1" ]
}

@test "parse_flags: --quiet sets QUIET" {
    QUIET=""
    parse_flags --quiet
    [ "$QUIET" = "1" ]
}

@test "parse_flags: unknown flag exits non-zero" {
    run parse_flags --bogus
    [ "$status" -ne 0 ]
}

@test "parse_flags: flags override env vars" {
    BHATTI_TIER="minimal"
    parse_flags --tier browser
    [ "$BHATTI_TIER" = "browser" ]
}

@test "parse_flags: --help exits 0" {
    run parse_flags --help
    [ "$status" -eq 0 ]
    [[ "$output" == *"Usage:"* ]]
}
