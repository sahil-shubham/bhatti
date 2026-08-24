# Investigation: Cold-wake page cache cost

*Measured on agni-01 (Hetzner AX102, Ryzen 9 3900X, 128 GB RAM, NVMe RAID-1), May 2026.*
*bhatti v1.11.9 production build. btrfs loopback at /var/lib/bhatti, compress=zstd:1, noatime.*

---

## Trigger

HN launch-post comment asked two pointed questions:

1. *"Have you measured how snapshot size scales with VM count and whether you're hitting storage bandwidth limits at scale?"*
2. *"Encrypted secrets add another serialization layer on wake — how much of that 360ms cold path is secret reconstruction versus block device replay?"*

While drafting a reply, a separate inconsistency surfaced: `thermal-states.mdx` said cold wake was `~42 ms p50` but the homepage benchmark says `360 ms p50`. Both numbers are real but they measure different things, and the docs page wasn't honest about that.

## Method

Enabled `BHATTI_LOG_LEVEL=debug` on agni-01 via a systemd override (`/etc/systemd/system/bhatti.service.d/debug.conf`, restart bhatti). Exercised the existing instrumentation in:

- `pkg/engine/firecracker/fc.go::startFCJailed` — FC process spawn + socket-ready timing
- `pkg/engine/firecracker/lifecycle.go::startVM` — `/snapshot/load` PUT timing
- `pkg/agent/client.go::WaitReady` — per-attempt TCP connect + AUTH + exec split

Controlled page cache state with `sync && echo 3 | sudo tee /proc/sys/vm/drop_caches` between runs. Timed end-to-end with `date +%s%N` deltas around `curl -X POST $URL/sandboxes/<id>/exec --data '{"argv":["true"]}'` against the public proxy.

Ten samples per scenario. Sandbox: 1024 MB RAM, computer tier base.

## Findings

| Scenario | Median exec | p99 | Notes |
|---|---|---|---|
| **Hot page cache** (`sync` only between runs) | **57–78 ms** | 84 ms | `mem.snap` still resident from the snapshot we just wrote |
| **Cold page cache** (`sync && drop_caches`) | **186–342 ms** | 380 ms | Full disk read of `mem.snap` working set |
| **Production-realistic** (concurrent traffic on the proxy during measurement) | **~400 ms** | ~430 ms | Matches the homepage 360 ms p50 / 430 ms p99 |
| **`keep_hot=true`** (no thermal management) | 13–14 ms (HOT) | — | Sandbox never goes cold; just a hot exec round-trip |

The gap between the two-digit and three-digit numbers is the disk read of `mem.snap`. With page cache hot, the kernel has the working set already; FC starts in tens of ms and resume is fast. With page cache cold, the read happens for real.

## Storage measurements (same host, same run)

| Metric | Value |
|---|---|
| Apparent (referenced) data | 571 GiB |
| Uncompressed (after reflink dedup) | 220 GiB |
| Physical (after zstd) | 95 GiB |
| Reflink savings | 2.6× |
| zstd savings | 2.3× |
| Combined | **6.0×** |
| One 1024 MB `mem.snap` on disk | **48 MiB** (21× compression) |
| One stopped sandbox dir (apparent 2.1 GiB) | **176 MiB** on disk |

Inspect with:

```bash
sudo compsize /var/lib/bhatti
sudo compsize /var/lib/bhatti/sandboxes/<id>/
```

## Measurement mistakes I made along the way

- **Timing `bhatti start` orchestration in a tight loop and getting 60 ms — mistaking it for cold-wake.** That's the host-side orchestration return path, which doesn't include the snapshot file read because the kernel still has `mem.snap` in page cache from the snapshot we just wrote 30 seconds ago. The `bench/run.sh` script at `bench/run.sh:330` flags this exact trap. Useful to record because the same trap was embedded in `thermal-states.mdx`.
- **Early measurements were noisy by ~300 ms** because external scanner traffic on the public proxy was auto-waking other sandboxes concurrently. Re-ran after putting the host on a different IP for measurement; numbers stabilised.

## What we don't (yet) know

1. Worst-case behavior on **ext4** host. The install script doesn't create a btrfs loopback by default; many self-hosters are on ext4. We have no production data on reflink-less hosts. The published perf numbers are btrfs-only.
2. `mem.snap` page cache residency under **sustained memory pressure** on a busy host. We've measured cold (`drop_caches`) and hot (just written), not "evicted by natural memory pressure 25 minutes later." The honest answer is that 30-minute idle on a busy host is enough to evict most of the working set — the production-realistic 400 ms number reflects this.
3. Whether multiple **concurrent cold wakes** contending for `mem.snap` reads (`resumeSem` capacity 10) actually saturate NVMe. The 400 ms production-realistic number is observational, not isolated.

## Possible follow-ups (not done in this investigation)

- **`posix_fadvise(POSIX_FADV_WILLNEED)` on `mem.snap` before `/snapshot/load`.** Hides part of the disk read behind FC startup. Bounded by demand. The post-create variant has multi-tenant fairness concerns and shouldn't be shipped without measurements showing real LRU eviction problems. Belongs in `pkg/engine/firecracker/lifecycle.go::startVM` immediately before the existing [`fcPut(... /snapshot/load ...)`](https://github.com/sahil-shubham/bhatti/blob/main/pkg/engine/firecracker/lifecycle.go#L395-L398) call.
- **Pin recently-snapshotted `mem.snap` files via `mlock` for the warm-to-cold window.** Would guarantee fast cold wake at the cost of host RAM. Not obviously the right trade-off on a multi-tenant host.

## Cleanup

```bash
# Revert the debug log level on agni-01.
sudo rm /etc/systemd/system/bhatti.service.d/debug.conf
sudo systemctl daemon-reload
sudo systemctl restart bhatti

# Verify back to INFO level.
sudo journalctl -u bhatti -n 20 | grep -i "level"
```

## What shipped from this investigation

- `thermal-states.mdx` updated to remove the false `~42 ms p50` claim and replace with the four-row cold-wake table above.
- `secrets.md` got a new "What happens on wake" section answering the second HN question directly: there is no decryption on wake.
- A new `storage.mdx` page consolidating the cloning/CoW/compression/page-cache story.
- Cross-links from `architecture.mdx`, `thermal-states.mdx`, `decisions.mdx` to the new storage page.
- Plan + reply: `docs/internal/PLAN-storage-and-cold-wake-docs.md` (internal), `docs/internal/DRAFT-discussion-17-reply.md` (queued).

Shipped in bhatti.sh commit c9c7907 (May 2026).
