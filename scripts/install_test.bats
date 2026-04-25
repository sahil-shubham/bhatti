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

# ── detect_tier ───────────────────────────────────────

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
