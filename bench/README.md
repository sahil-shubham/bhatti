# Bench

For the current v2 daemon and concurrent agent workloads, use the Go harness
in [`load/README.md`](load/README.md). It measures persistent-client API
latencies, concurrency ramps, throughput, public proxy fan-in, network egress,
and hot/warm VM density, recording raw samples and host metrics with provenance.
The `run.sh` results below are historical v1-era CLI-per-operation measurements;
do not use them as v2 capacity numbers.

`run.sh` measures end-to-end latency against a live bhatti daemon. Run it on
the same host as the daemon (loopback) so results don't include geographic
network latency.

```bash
# Full bench (~22 min):
./run.sh

# A subset:
SECTIONS=exec,files ./run.sh

# Smoke test:
ITERATIONS=10 LIFECYCLE_N=3 ./run.sh
```

Results land in `bench/results/<metric>.txt` — one millisecond value per
line, raw, no header. The script also prints a percentile summary
(`min / p50 / p95 / p99 / max / mean`) for each metric as it runs.

## What each metric measures

```
lifecycle/
  create.txt              `bhatti create` returning. Includes VM boot, lohar
                          becoming reachable. Excludes user init scripts.
  stop.txt                `bhatti stop` returning. FC writes the snapshot.
                          Synchronous — measures full memory-to-disk write.
  cold_resume.txt         `bhatti start` returning. ⚠ Misleading on its own
                          — see "the lazy-fault gotcha" below.
  cold_resume_exec.txt    `bhatti start` followed by `bhatti exec true`.
                          ✅ Realistic cold-start cost (includes page-in).
  destroy.txt             `bhatti destroy` returning. Just file deletion.

exec/                     `bhatti exec` against a warm sandbox. Sub-tests
                          for true / echo / cat / ls / sha256sum / env.
files/                    `bhatti file read/write/ls` at multiple sizes
                          (1k / 10k / 100k / 1M).
api/                      Direct API hits — list, inspect, /health, curl
                          /sandboxes. SQLite + HTTP routing only.
concurrent/               Wall-clock for N parallel exec calls finishing.
network/                  TCP connect time + first-byte time to the API.
publish-wake/
  publish_wake_hot.txt    Curl through publish proxy to a sandbox already
                          serving requests. Just the proxy roundtrip.
  publish_wake_cold.txt   Stop the sandbox, curl. The curl triggers wake +
                          page-in + workload. ✅ Realistic cold-wake.
  publish_wake_warm.txt   Wait 35s for the thermal manager to pause vCPUs,
                          then curl. Memory still in RAM, vCPUs paused.
                          ✅ Realistic warm-wake.
```

## Gotchas

These are findings that made us *not* trust earlier numbers. They're
documented here so future-us doesn't fall for them again.

### The lazy-fault gotcha — `cold_resume.txt` is misleading

bhatti restores snapshots with Firecracker's `backend_type: "File"`, which
mmap's the snapshot file. Memory pages fault in **lazily on first access**.
This means:

- `bhatti start` returns when FC has set up the mmap (~60ms).
- The first real workload triggers page faults that read from disk
  (~250ms more, depending on cache state).

So `cold_resume.txt` (timing `bhatti start` only) reports ~60ms — but
that's not a usable sandbox yet. The sandbox is "ready" only in the
sense that `bhatti list` shows it as running.

The realistic cold-start cost is in `cold_resume_exec.txt`
(~310ms p50) or `publish_wake_cold.txt` (~360ms p50, includes the
in-VM HTTP server's working set). **Use those for any user-facing
"cold start" claim**; treat `cold_resume.txt` as an
internal-orchestration-cost measurement only.

### The page-cache gradient — disk-reading metrics drift over a long run

Linux's OS page cache makes the first 5–10 iterations of any
disk-reading metric artificially fast. We saw this on
`publish_wake_cold` at n=100:

```
samples 1-10:   mean 58ms   (snapshot still in OS page cache from the recent stop)
sample 11:      277ms       (page cache evicts under memory pressure)
samples 11-90:  mean 356ms  (steady state, snapshot read from NVMe)
```

The transition is sharp at sample 11 because by that point ~5 GB of
fresh writes have accumulated and Linux starts evicting older
pages. Below n=10, you only ever see the cache-hot regime — the
small-n "60ms cold wake" we used to publish was wholly an artifact
of this.

**Implication for sample sizes**: lifecycle metrics at n=15 may also
sit in the cache-hot regime if the snapshot is small enough to fit.
Don't trust `cold_resume*` numbers under ~n=20 as steady-state.

### Rate limits and the cleanup trap

Per-user rate limits (30/min create, 1200/min read) constrain how
fast you can spin up sandboxes. The bench paces creates at
`SLEEP_PER_CREATE=2.1s` to stay under. Cleanup is sequential with
retries because parallel destroy of 30+ sandboxes can race the read
bucket via `resolveID`'s 2-call name lookup.

### Outliers happen, especially at p99

Stop p99 of 800ms with p50 of 485ms is a real ratio: snapshot writes
contend for the same NVMe bandwidth as everything else on the host.
`publish_wake_cold` showed a 600–876ms run of outliers around
sample 92 in our isolated run — system blip, single-event. p95
(~430ms) is more representative for "what users typically feel"
than p99 in cases like this.

## Sample sizes — what each `n` represents

```
ITERATIONS=100         exec / file / api / network / publish_wake_hot
LIFECYCLE_N=15         create / stop / cold_resume / destroy
WARM_N=10              publish_wake_warm (35s sleep per sample)
PUBLISH_WAKE_COLD_N=100  publish_wake_cold
CONCURRENT_REPS=30     concurrent_5 / concurrent_10 / concurrent_20
```

`LIFECYCLE_N=15` is rate-limit-bound: 30 creates/min means you can
do ~25 lifecycle iterations per minute, and we want the bench under
20 minutes total. Bumping it past 20 mostly buys tighter p99
estimates and doesn't change p50.

`WARM_N=10` is sleep-bound: each sample needs 35s of idle time to
let the thermal manager pause the VM's vCPUs. n=10 = ~6 minutes of
sleep, which dominates the bench duration for that section.

`PUBLISH_WAKE_COLD_N=100` is what catches the page-cache gradient
above. Under ~50 you can't see the steady-state.

## Reading the output

Each `*.txt` file contains raw millisecond values, one per line.
Useful one-liners:

```bash
# Quick percentile summary:
awk 'BEGIN{} {a[NR]=$1} END {asort(a); n=NR;
  printf "n=%d p50=%.1f p95=%.1f p99=%.1f max=%.1f\n",
  n, a[int(n*0.5)+1], a[int(n*0.95)+1], a[int(n*0.99)+1], a[n] }' \
  results/exec_true.txt

# First-N vs last-N mean (catch cache effects):
awk 'NR<=10 {f+=$1; fc++} NR>=NR-9 {l+=$1; lc++}
  END {printf "first10=%.1f last10=%.1f\n", f/fc, l/lc}' \
  results/publish_wake_cold.txt
```

## Reproducing the homepage numbers

The numbers on `bhatti.sh` come from a clean run on a Hetzner AX102
(Ryzen 9, NVMe, btrfs) with default daemon configuration. To
reproduce:

```bash
ssh your-bhatti-server
cd /path/to/bhatti
./bench/run.sh                   # full bench, ~22 min
# then read results/*.txt
```

The headline numbers we publish use:
- `cold_resume_exec.txt` or `publish_wake_cold.txt` for **cold wake**
  (NOT `cold_resume.txt` — see lazy-fault gotcha)
- `publish_wake_warm.txt` for **warm wake**
- `create.txt`, `stop.txt`, `destroy.txt` for **lifecycle**
- `exec_true.txt` for **run a command**
- `concurrent_20.txt` for **20 commands in parallel**
