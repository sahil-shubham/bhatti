# Migration: agni-01 → v0.5.14 + btrfs

**Date:** April 2026
**Host:** agni-01 (Hetzner AX102, 24 cores, 128GB RAM, 2× NVMe RAID-1)
**From:** v0.5.10 on ext4
**To:** v0.5.14 on btrfs (loopback)
**Expected downtime:** ~15 minutes

---

## Pre-migration State

| Item | Value |
|------|-------|
| bhatti | v0.5.10 |
| Firecracker | v1.14.0 |
| Filesystem | ext4 on md RAID-1 (`/dev/md2`), 1.8TB total, 78GB used |
| Data dir | `/var/lib/bhatti/` — 69GB |
| Sandboxes | 10 (9 stopped, 1 running: rory) |
| Named snapshots | `rory-ready-v2` (7.9GB), `browser-ready` (3.0GB) |
| Volumes | `rory-data` (5GB, attached to rory) |
| User images | spc-agents-hermes, spc-golden, cli-alpine |
| Base images | browser (2GB), docker |
| Tools needed | btrfs-progs ✅, zstd ✅ (both already installed) |

### What's in v0.5.14 (since v0.5.10)

- **Snapshot safety:** SnapshotAll uses Full snapshots with retry, guest
  sync before snapshot, sanity checks on snapshot artifacts,
  has_base_snapshot reset on recovery
- **Thermal manager:** Logs + force-pauses after 10 consecutive agent
  failures, keep_hot sandboxes exempt
- **Restore resilience:** Circuit breaker on corrupt snapshots, FC stderr
  captured per-VM in 64KB ring buffer
- **FC hardening:** Serial console disabled, entropy device (virtio-rng),
  network_overrides for snapshot resume (eliminates TAP name races)
- **Operational:** Server-side name resolution (ID-first, name-fallback),
  FC per-VM logger + metrics
- **Code quality:** SIGTERM before SIGKILL, socket path validation,
  extracted startFC helper, process reaping in all error paths
- **Memory:** Balloon device on new VMs, hugepages opt-in
- **Storage:** reflink-auto on all block device copies (instant on btrfs)
- **Backup:** S3-compatible volume backup/restore (native, zero deps)
- **QoL:** Update notice only on `bhatti version`, install script guards
  major version crossings
- **Rate limiters:** Disabled by default (opt-in via config.yaml)

All changes are backward-compatible. No snapshot format changes. No
config schema changes. Existing sandboxes recover normally.

---

## Phase A: Upgrade bhatti to v0.5.14

The install script detects the existing server installation and updates
all components (binary, lohar, kernel, rootfs). It stops the systemd
service before updating and restarts after.

During shutdown, SnapshotAll snapshots all running VMs. rory (the only
running sandbox) will get a snapshot. The 9 already-stopped sandboxes
are no-ops.

```bash
curl -fsSL bhatti.sh/install | sudo bash
```

### Verify

```bash
# Check version
bhatti version
# Expected: bhatti v0.5.14

# Check all 10 sandboxes recovered
journalctl -u bhatti -n 50 --no-pager | grep -E 'recovered|recovery'
# Expected: "recovery complete" with count=10

# Check rory is accessible (as kowshik or admin)
bhatti list
# rory should appear with status=stopped

# Quick smoke test: start and exec on a sandbox
bhatti start sandbox-167074
bhatti exec sandbox-167074 -- echo hello
bhatti stop sandbox-167074
```

### If something goes wrong

The old binary is at `/usr/local/bin/bhatti.bak` (the install script
doesn't create this — if you want a safety net, copy it before running
the install):

```bash
# Before running install:
cp /usr/local/bin/bhatti /usr/local/bin/bhatti.v0.5.10
cp /var/lib/bhatti/lohar /var/lib/bhatti/lohar.v0.5.10

# To rollback:
systemctl stop bhatti
cp /usr/local/bin/bhatti.v0.5.10 /usr/local/bin/bhatti
cp /var/lib/bhatti/lohar.v0.5.10 /var/lib/bhatti/lohar
systemctl start bhatti
```

---

## Phase B: btrfs Migration

Converts `/var/lib/bhatti` from ext4 to btrfs-on-loopback. This gives
us instant copy-on-write clones (reflink) for sandbox creation and
snapshot resume, plus transparent zstd compression.

**Expected impact:**
- Disk usage: 69GB → ~23GB (zstd compression + reflink sharing)
- `bhatti create --image browser`: ~1.6s → ~0.01s
- Snapshot resume: 3-8s → <0.1s
- Named snapshot (Checkpoint): multi-second VM pause → near-zero

bhatti must be stopped for this phase.

### Step 1: Stop bhatti

```bash
systemctl stop bhatti
```

Verify no firecracker processes are still running:

```bash
ps aux | grep firecracker | grep -v grep
```

Should be empty. If any remain (zombies show as `[firecracker] <defunct>`),
they're harmless and will disappear. If there's a live process, something
went wrong with SnapshotAll — check `journalctl -u bhatti -n 100` before
proceeding.

### Step 2: Backup critical files

These are small files that would be catastrophic to lose. The full ext4
data directory is preserved in step 4 as `/var/lib/bhatti-ext4-backup`.

```bash
cp /var/lib/bhatti/state.db /root/state.db.backup
cp -r /var/lib/bhatti/tls /root/tls.backup
cp /var/lib/bhatti/age.key /root/age.key.backup 2>/dev/null || true
```

### Step 3: Create and format btrfs image

500GB pre-allocated. The RAID-1 has 1.6TB free — plenty of room.
`fallocate` is instant (doesn't write data, just reserves space).

```bash
fallocate -l 500G /var/lib/bhatti-btrfs.img
mkfs.btrfs -f /var/lib/bhatti-btrfs.img
```

### Step 4: Copy data to btrfs

```bash
mkdir -p /mnt/bhatti-new
mount -o loop,noatime,compress=zstd:1 /var/lib/bhatti-btrfs.img /mnt/bhatti-new
rsync -aHAX --sparse --info=progress2 /var/lib/bhatti/ /mnt/bhatti-new/
```

This copies ~69GB. At NVMe RAID-1 read speeds, expect 2-4 minutes.
The `--sparse` flag preserves sparse files (important for rootfs images).
zstd compression happens transparently on write — the data on btrfs will
be smaller than the source.

After rsync completes, verify key files:

```bash
ls /mnt/bhatti-new/state.db /mnt/bhatti-new/config.yaml
ls /mnt/bhatti-new/images/vmlinux-amd64
ls /mnt/bhatti-new/sandboxes/ | wc -l   # should be 10
```

### Step 5: Swap mount points

```bash
umount /mnt/bhatti-new
rmdir /mnt/bhatti-new
mv /var/lib/bhatti /var/lib/bhatti-ext4-backup
mkdir -p /var/lib/bhatti
mount -o loop,noatime,compress=zstd:1 /var/lib/bhatti-btrfs.img /var/lib/bhatti
```

Verify:

```bash
df -T /var/lib/bhatti
# Filesystem     Type   Size  Used  Avail Use% Mounted on
# /dev/loopX     btrfs  500G  ~23G  ~477G   5% /var/lib/bhatti

ls /var/lib/bhatti/state.db /var/lib/bhatti/config.yaml
# Both should exist
```

### Step 6: Persist across reboots

```bash
echo '/var/lib/bhatti-btrfs.img /var/lib/bhatti btrfs loop,noatime,compress=zstd:1 0 0' >> /etc/fstab
```

### Step 7: Start bhatti

```bash
systemctl start bhatti
```

### Verify

```bash
# Version
bhatti version

# All sandboxes recovered
journalctl -u bhatti -n 50 --no-pager | grep -E 'recovered|recovery'

# Disk savings
du -sh /var/lib/bhatti/
# Expected: significantly less than 69GB

# Reflink works — create should be near-instant
time bhatti create --name reflink-test
bhatti destroy reflink-test
```

### Rollback

If anything is wrong, full rollback takes under a minute:

```bash
systemctl stop bhatti
umount /var/lib/bhatti
mv /var/lib/bhatti-ext4-backup /var/lib/bhatti
sed -i '/bhatti-btrfs/d' /etc/fstab
systemctl start bhatti
```

The ext4 backup can be deleted once btrfs is confirmed stable after a
few days of operation:

```bash
# Only after confirming everything works:
rm -rf /var/lib/bhatti-ext4-backup
```

The btrfs image file (`/var/lib/bhatti-btrfs.img`) can be resized later
if 500GB is insufficient:

```bash
systemctl stop bhatti
umount /var/lib/bhatti
truncate -s 800G /var/lib/bhatti-btrfs.img
mount -o loop,noatime,compress=zstd:1 /var/lib/bhatti-btrfs.img /var/lib/bhatti
btrfs filesystem resize max /var/lib/bhatti
systemctl start bhatti
```

---

## Phase C: Performance Benchmarks

Run after the migration is confirmed stable. Requires Go toolchain on
the host.

```bash
cd /path/to/bhatti   # or git clone
sudo go test ./pkg/engine/firecracker/ -v -count=1 -timeout=0 \
    -run 'TestPerf' 2>&1 | tee /tmp/perf-btrfs.txt
```

Key metrics to compare with the website (bhatti.sh):

| Operation | Website claim | Expected on btrfs |
|-----------|--------------|-------------------|
| Create sandbox (browser image) | 1.44s p50 | <0.1s (reflink) |
| Diff snapshot | 32ms p50 | Similar or faster |
| Cold resume exec | 40.6ms p50 | Similar or faster |
| Exec command | 1.22ms p50 | Same |
| Warm resume exec | 2.08ms p50 | Same |

The big improvement will be in Create (reflink vs full copy) and any
operation that copies block devices (snapshot resume, checkpoint).
Exec latency is agent-bound, not disk-bound, so it won't change.

---

## Post-Migration Issues (April 2, 2026)

The following issues were discovered during and after the migration.
None were caused by btrfs — all were pre-existing bugs exposed by
active usage during the migration window.

### 1. vm.snap sanity check false alarm

**Symptom:** Every snapshot logged `vm.snap is not valid JSON (truncated
or corrupt)` — the Phase 2.1 sanity check assumed FC stores vm.snap as
JSON.

**Cause:** Firecracker ≥1.14 uses a binary format for vm.snap, not JSON.

**Fix:** Replaced JSON validation with stat + non-empty check.
Commit: `04674bf`.

### 2. Stale vsock blocks restore after failure

**Symptom:** After a failed snapshot restore, subsequent attempts (via
`--force` or daemon restart) fail with `Address in use (os error 98)`.

**Cause:** The failed FC process created vsock.sock before crashing.
The `restoreFailed` cleanup killed FC but didn't remove the socket.
The circuit breaker then blocked retries, so the stale socket persisted.

**Fix:** `restoreFailed` now removes vsock.sock after killing FC.
Commit: `a5a74ca`.

### 3. Destroy/stop routes used name instead of resolved ID

**Symptom:** `bhatti destroy rory` returned 500. The engine destroy
succeeded (bridge cleaned up) but `DeleteSandbox("rory")` failed
because it matches by ID column, not name.

**Cause:** Phase 4.1 added name resolution to `GetSandbox`, but the
destroy and stop routes passed the raw URL parameter to subsequent
store operations instead of the resolved `sb.ID`.

**Fix:** All post-resolution operations now use `sb.ID`.
Commit: `8ffede6`.

**Manual remediation on agni-01:** Since the engine had already
destroyed rory's VM and bridge but the DB record remained, we
manually updated the DB:
```sql
UPDATE sandboxes SET status='destroyed' WHERE id='80ddac6a6acf2095';
DELETE FROM volume_attachments WHERE sandbox_id='80ddac6a6acf2095';
```

### 4. Rory's corrupt Diff snapshot

**Symptom:** `bhatti shell rory` → FC panics with
`The number of available virtio descriptors 34618 is greater than queue size: 256!`

**Cause:** This is the original April 1 rory incident. The thermal
snapshot in the sandbox directory was a Diff snapshot taken on a hot
VM by the old v0.5.10 SnapshotAll code. The v0.5.14 Phase 1.1 fix
(ForceFullSnapshot in SnapshotAll) prevents this for new snapshots
but cannot repair existing corrupt ones.

**Resolution:** Destroyed the old rory sandbox and resumed from the
named snapshot `rory-ready-v2` (taken as Full via Checkpoint, clean).
The `rory-data` volume was safe throughout.

### 5. keep_hot didn't wake sandbox

**Symptom:** `bhatti edit rory --keep-hot` updated the DB flag but
didn't bring the sandbox from cold to hot.

**Fix:** PATCH handler now calls `ensureHot()` when setting
`keep_hot=true`. Commit: `a6ad239`.

### 6. keep_hot sandboxes stay cold after daemon restart

**Symptom:** After `systemctl restart bhatti`, keep_hot sandboxes
remained cold until manually accessed.

**Fix:** Background goroutine auto-wakes all keep_hot sandboxes
after recovery. Commit: `0471a51`.

---

## Summary

| Phase | Action | Downtime | Risk |
|-------|--------|----------|------|
| A | `bhatti.sh/install` | ~2 min (service restart) | Near zero — backward-compatible |
| B | btrfs migration | ~10-15 min (rsync) | Low — full rollback in <1 min |
| C | Perf benchmarks | None (separate test) | None |
