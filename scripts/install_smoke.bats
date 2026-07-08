#!/usr/bin/env bats
# scripts/install_smoke.bats — end-to-end smoke for scripts/install.sh
#
# Stands up a local fake-release tree (a directory the script can read
# via file://), runs install.sh as a subprocess, and asserts the
# binary lands, executes, and the right thing happens on the failure
# paths users will actually hit (corrupt asset, missing asset).
#
# This file is the keystone of the user's claim:
#   "If CI passes, install + update works modulo GitHub being down."
# install_test.bats proves the helpers in isolation; this file proves
# they compose into a working installer when wired together.
#
# Why file:// and not a Python http.server: simpler (no port juggling,
# no race conditions on startup), and install.sh's download() uses curl
# which speaks file:// transparently — the path under test is identical
# regardless of scheme. If we ever need HTTP-specific coverage (e.g.
# real HTTP 4xx/5xx codes, redirects), that's a separate test.

setup_file() {
    export FAKE_ROOT
    FAKE_ROOT=$(mktemp -d)
    export RELEASE="$FAKE_ROOT/release"
    mkdir -p "$RELEASE"

    # PATH for tests that must NOT find an existing bhatti. Critically
    # excludes /usr/local/bin (and anywhere else bhatti might live) so
    # the script's installed_bhatti_version() probe returns empty and
    # we exercise the fresh-install path instead of the major-version
    # upgrade prompt against whatever real bhatti the dev has.
    export TEST_PATH="/usr/bin:/bin:/usr/sbin:/sbin"
    # install_bundle needs zstd/tar/find; add their dirs (e.g. Homebrew's
    # /opt/homebrew/bin on macOS) so the restricted PATH can reach them without
    # pulling in wherever a real bhatti might live.
    for _tool in zstd tar find; do
        _tp=$(command -v "$_tool" 2>/dev/null) || continue
        _td=$(dirname "$_tp")
        case ":$TEST_PATH:" in *":$_td:"*) ;; *) TEST_PATH="$TEST_PATH:$_td" ;; esac
    done
    export TEST_PATH

    # Detect platform the same way install.sh does, so the asset name
    # matches what the script will request.
    local os arch
    os=$(uname -s | tr '[:upper:]' '[:lower:]')
    case "$(uname -m)" in
        x86_64)        arch=amd64 ;;
        aarch64|arm64) arch=arm64 ;;
        *) echo "unsupported test runner arch: $(uname -m)" >&2; exit 1 ;;
    esac
    export OS="$os" ARCH="$arch"
    export VERSION_SMOKE="v0.0.0-smoke"
    export BUNDLE_ASSET="bhatti-${VERSION_SMOKE}-${os}-${arch}.tar.zst"

    # Sanity: every external command install.sh needs in CLI mode must
    # be reachable via TEST_PATH. If a runner ships them somewhere
    # else, we want to know now, not via a confusing test failure.
    local missing=""
    for cmd in curl awk sed grep basename mktemp chmod cp install tar zstd; do
        PATH="$TEST_PATH" command -v "$cmd" >/dev/null 2>&1 || missing="$missing $cmd"
    done
    if [ -n "$missing" ]; then
        echo "setup_file: TEST_PATH missing required commands:$missing" >&2
        exit 1
    fi

    # Build a fake v2 bundle matching the release layout: a top-level
    # bhatti-<ver>-<os>-<arch>/ dir with bin/bhatti (+ lib/ + kernel/), tar.zst'd.
    # install_bundle downloads + extracts this and runs `bin/bhatti version` as
    # its self-check, so bin/bhatti must quack like real bhatti.
    local dir="bhatti-${VERSION_SMOKE}-${os}-${arch}"
    local btree="$RELEASE/.build/$dir"
    mkdir -p "$btree/bin" "$btree/lib" "$btree/kernel"
    cat > "$btree/bin/bhatti" <<'EOF'
#!/bin/bash
case "$1" in
    version) echo "bhatti v0.0.0-smoke" ;;
esac
EOF
    chmod +x "$btree/bin/bhatti"
    : > "$btree/lib/libkrun.so"
    local karch=x86_64; [ "$arch" = "arm64" ] && karch=aarch64
    : > "$btree/kernel/Image-lean-6.12.0-${karch}"
    ( cd "$RELEASE/.build" && tar -cf - "$dir" | zstd -q -o "$RELEASE/${BUNDLE_ASSET}" )

    # Generate checksums.txt in the same shape the release pipeline does (bare
    # filename), which the script's grep against $CHECKSUMS matches.
    (
        cd "$RELEASE"
        if command -v sha256sum >/dev/null 2>&1; then
            sha256sum "$BUNDLE_ASSET" > checksums-sha256.txt
        else
            shasum -a 256 "$BUNDLE_ASSET" > checksums-sha256.txt
        fi
    )
}

teardown_file() {
    rm -rf "$FAKE_ROOT"
}

setup() {
    # Every test gets its own writable dest so they don't clobber each
    # other and so install.sh never needs sudo. The sudo path is
    # exercised by the macOS CI runner via the unit suite (need_sudo
    # tests) and by the c31d997 postmortem — running the real flow on
    # a system-owned dir would require passwordless sudo here, which
    # is fragile across local dev environments.
    BIN_DEST_DIR=$(mktemp -d)
    BIN_DEST="$BIN_DEST_DIR/bhatti"
}

# Bats footgun fix — see the longer explanation in install_test.bats.
# tl;dr: `[[ ]]` is a bash keyword, set -e does not trip on it, so a
# failing `[[ "$output" == *X* ]]` in the middle of a test gets masked
# by any 0-returning command after it. Use this helper instead.
output_contains() {
    echo "$output" | grep -qF "$1"
}

teardown() {
    rm -rf "$BIN_DEST_DIR"
}

# ── 1. Fresh CLI install ──────────────────────────────────────────
# The single most important test in the file: prove an unprivileged
# user can run install.sh and get a working binary. If only one test
# in this file is allowed to pass, it has to be this one.

@test "smoke: fresh CLI install lands a working binary" {
    run env -i HOME="$HOME" PATH="$TEST_PATH" \
        BHATTI_MODE=cli \
        BHATTI_TEST_VERSION="v0.0.0-smoke" \
        BHATTI_TEST_RELEASE_URL="file://$RELEASE" \
        BHATTI_TEST_BIN_DEST="$BIN_DEST" \
        QUIET=1 \
        bash scripts/install.sh
    [ "$status" -eq 0 ] || { echo "install.sh failed: $output"; return 1; }

    [ -x "$BIN_DEST" ]
    [ "$("$BIN_DEST" version)" = "bhatti v0.0.0-smoke" ]
}

# ── 2. Idempotent re-install ──────────────────────────────────────
# `bhatti update` should be cheap when there's nothing to update. This
# test puts $BIN_DEST on PATH so installed_bhatti_version() can find
# it; in real life the user has /usr/local/bin in PATH already.

@test "smoke: re-running install.sh at the same version is a fast no-op" {
    # First install (fresh path: bhatti not on PATH)
    env -i HOME="$HOME" PATH="$TEST_PATH" \
        BHATTI_MODE=cli BHATTI_TEST_VERSION="v0.0.0-smoke" \
        BHATTI_TEST_RELEASE_URL="file://$RELEASE" \
        BHATTI_TEST_BIN_DEST="$BIN_DEST" \
        QUIET=1 \
        bash scripts/install.sh

    # Second run: PATH starts with our just-installed binary so
    # installed_bhatti_version() finds it and the script reports the
    # "already installed" short-circuit instead of re-downloading.
    run env -i HOME="$HOME" PATH="$BIN_DEST_DIR:$TEST_PATH" \
        BHATTI_MODE=cli BHATTI_TEST_VERSION="v0.0.0-smoke" \
        BHATTI_TEST_RELEASE_URL="file://$RELEASE" \
        BHATTI_TEST_BIN_DEST="$BIN_DEST" \
        bash scripts/install.sh
    [ "$status" -eq 0 ] || { echo "second run failed: $output"; return 1; }
    output_contains "already installed"
}

# ── 3. Update across versions ─────────────────────────────────────
# Different from idempotent: pre-stage an OLD binary at $BIN_DEST,
# then run install.sh with the NEW version. Exercises the update path
# (current != VERSION) and proves the binary actually gets replaced
# rather than the script no-op-ing.

@test "smoke: install.sh replaces an older binary on update" {
    # Pre-stage a v0.0.0-old binary so installed_bhatti_version() reads "v0.0.0-old"
    cat > "$BIN_DEST" <<'EOF'
#!/bin/bash
case "$1" in version) echo "bhatti v0.0.0-old" ;; esac
EOF
    chmod +x "$BIN_DEST"

    run env -i HOME="$HOME" PATH="$BIN_DEST_DIR:$TEST_PATH" \
        BHATTI_MODE=cli BHATTI_TEST_VERSION="v0.0.0-smoke" \
        BHATTI_TEST_RELEASE_URL="file://$RELEASE" \
        BHATTI_TEST_BIN_DEST="$BIN_DEST" \
        QUIET=1 \
        bash scripts/install.sh
    [ "$status" -eq 0 ] || { echo "install.sh failed: $output"; return 1; }

    # The v0.0.0-old binary must have been replaced with v0.0.0-smoke.
    [ "$("$BIN_DEST" version)" = "bhatti v0.0.0-smoke" ]
    # And install.sh's rollback hint left a .old behind for the user.
    [ -f "${BIN_DEST}.old" ]
}

# ── 4. Checksum mismatch ──────────────────────────────────────────
# Tamper with the binary AFTER the checksums file was generated. This
# exercises verify_checksum's die path — the failure mode that matters
# is "loud failure with no half-installed artifact", because a silent
# install of a tampered binary is a supply-chain incident.

@test "smoke: checksum mismatch dies loudly and leaves no installed binary" {
    local tampered_root tampered_release
    tampered_root=$(mktemp -d)
    tampered_release="$tampered_root/release"
    cp -R "$RELEASE" "$tampered_release"

    # Replace the bundle with different content. checksums.txt still has
    # the OLD sha — verify_checksum should die when the new content
    # hashes to something different.
    echo "tampered with" > "$tampered_release/$BUNDLE_ASSET"

    run env -i HOME="$HOME" PATH="$TEST_PATH" \
        BHATTI_MODE=cli BHATTI_TEST_VERSION="v0.0.0-smoke" \
        BHATTI_TEST_RELEASE_URL="file://$tampered_release" \
        BHATTI_TEST_BIN_DEST="$BIN_DEST" \
        QUIET=1 \
        bash scripts/install.sh

    [ "$status" -ne 0 ]
    output_contains "checksum mismatch"
    [ ! -e "$BIN_DEST" ]   # critical: no half-installed artifact

    rm -rf "$tampered_root"
}

# ── 5. Missing release asset ──────────────────────────────────────
# A release tree with checksums.txt but no actual binary. This is
# what a half-failed release upload looks like (asset missing, manifest
# present). Script must die at download, not silently install nothing
# or fall through to a misleading later error.

@test "smoke: missing release asset dies with download error, no binary installed" {
    local empty_root empty_release
    empty_root=$(mktemp -d)
    empty_release="$empty_root/release"
    mkdir -p "$empty_release"
    : > "$empty_release/checksums-sha256.txt"   # manifest present, asset missing

    run env -i HOME="$HOME" PATH="$TEST_PATH" \
        BHATTI_MODE=cli BHATTI_TEST_VERSION="v0.0.0-smoke" \
        BHATTI_TEST_RELEASE_URL="file://$empty_release" \
        BHATTI_TEST_BIN_DEST="$BIN_DEST" \
        QUIET=1 \
        bash scripts/install.sh

    [ "$status" -ne 0 ]
    # Either curl returns non-zero (file:// 404 → "curl error") or
    # the empty-file guard fires. Both are acceptable; the contract is
    # "die before reaching install" with a download-shaped message.
    output_contains "download failed" || output_contains "download produced an empty file"
    [ ! -e "$BIN_DEST" ]

    rm -rf "$empty_root"
}
