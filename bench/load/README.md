# bhatti v2 concurrent load bench

`bench/load` is a standalone Go program (standard library only) that measures the
**current daemon HTTP API** with a shared keep-alive client. Run it on the daemon
host over its Unix control socket for API and VM measurements; only the `proxy`
scenario deliberately traverses the public TLS hostname. This is not a
replacement for the historical Firecracker/CLI-per-call `bench/run.sh` output.
Never compare numbers from different engines, machines, images, or API modes
without the accompanying `meta.json`.

## Build and run safely

From the repository root:

```sh
GOOS=linux GOARCH=amd64 go build \
  -ldflags "-X main.buildSHA=$(git rev-parse HEAD)" \
  -o /var/tmp/bench-load ./bench/load
scp /var/tmp/bench-load root@agni-02:/var/tmp/bench-load
```

On the server, use a *dedicated* account, **not** an administrator token. Create
it only once, on that same server (admin commands read its local SQLite database):

```sh
bhatti user create --name bench --max-sandboxes 120 --max-cpus 2 --max-memory 2048
# Store the one-time API key in /var/tmp/bench-key; chmod 0600 /var/tmp/bench-key.
bhatti user list --json   # note bench's user ID for optional -owner-id
```

The key must not be included in a script or a results file. Use `-token-file
/var/tmp/bench-key` rather than `-token`; `BHATTI_TOKEN` is also accepted. All
examples below assume the following run prefix **on agni-02**, using that bench
account:

```sh
B='/var/tmp/bench-load'
COMMON='-api unix:///var/lib/bhatti/api.sock -token-file /var/tmp/bench-key -out /var/tmp/bench-results'
$B baseline $COMMON -cli /usr/local/bin/bhatti
```

The default result parent is `results/` relative to the working directory;
**on agni-02 always pass** `-out /var/tmp/bench-results` so all scratch files
stay under `/var/tmp/bench-*`. `-api https://agni-02.karkhana.dev` also works,
but then API TLS/network RTT is included. Never supply the published alias as
`-api`; the daemon API and public proxy route by hostname. The bearer key is
required even over the owner-only socket. A snapshot does not require a
`/version` endpoint: the program reads `X-Bhatti-Version` from `/health`.

The process only destroys the bench account's sandboxes and snapshots whose
names start with its **fresh random run prefix**. On SIGINT/SIGTERM it cancels
load and cleans up under a new context; cleanup failures make the run fail and
print the prefix. Check with the bench account after each run:

```sh
BHATTI_TOKEN=\"$(cat /var/tmp/bench-key)\" bhatti ls --json
BHATTI_TOKEN=\"$(cat /var/tmp/bench-key)\" bhatti snapshot list --json
```

Avoid running multiple large scenarios at once. The current server's sandbox
quota is 120 and its guest resources are limited to 2 vCPU / 2048 MiB each;
the examples below use 1 vCPU / 512 MiB. No run touches existing sandboxes from
another owner; do not restart the daemon or kill the per-owner gateway.

## Scenarios

All commands below run **on the server**, after setting `B` and `COMMON` above.
For short acceptance runs use `-n`/`-levels 1`/`-aliases 1`/`-fan-in 2`,
`-iterations 1 -lifecycle 1`, and short `-duration` as appropriate.

| Subcommand | Full-size command | Measurement |
| --- | --- | --- |
| `baseline` | `$B baseline $COMMON -cli /usr/local/bin/bhatti` | 30 sequential exec and file 1 KiB/100 KiB/1 MiB write+read samples; 15 sequential create/destroy, stop/start/first exec, fork and memory checkpoint/resume samples; one browser-tier create; CLI exec/create separately when `-cli` set. Warm first exec is measured **only after** `GET /sandboxes` reports `thermal=warm` (up to 2 min wait each); explicit stop is a v2 power-off, not a warm pause. |
| `ramp` | `for op in create fork resume wake; do $B ramp $COMMON -op "$op" -levels 1,5,10,25,50; done` | A barrier starts all L calls together. Each level has individual call samples and batch-wall elapsed + successes and ops/s. Forks share one source; restores share one checkpoint; wake uses L stopped sandboxes and executes `/bin/true`. Wake **setup** creates are paced to stay below the owner's create limit; timed wake calls are not paced. Clean up between levels. Each `-op` is a separate run. |
| `exec` | `$B exec $COMMON -n 10 -c 4 -duration 60s` | N hot sandboxes × C independent persistent-client exec loops; successful and attempted ops/s plus latency/error distribution during the active window. |
| `swarm` | `$B swarm $COMMON -n 10 -duration 600s -k 5 -think 200ms` | Each agent repeats create, k execs, 100 KiB file write and verified read, every third **successful create** loop fork+exec, then destroys the fork and parent. Attempted vs successful loop throughput, failures, randomized think delay up to `-think`. |
| `proxy` | `$B proxy $COMMON -domain agni-02.karkhana.dev -levels 1,10,50 -duration 15s -fan-in 100 -aliases 10` | Browser-tier Node HTTP server published on port 3000: sustained public TLS keep-alive load with C clients **for each level**, then 100 simultaneous requests to one stopped alias, then one simultaneous request to each of 10 stopped aliases. Record HTTP errors/429s as failures, not completed requests. |
| `net` | `$B net $COMMON -levels 1,5,10 -connect-samples 30 -net-timeout 30s` | 100 MiB public download from each guest at each N, guest-measured `curl` bytes/s and aggregate MB/s; guest-side TCP connect latency samples to `1.1.1.1:443`; one sibling-only browser pair streams data over their guest IPs. Guest **setup** creates are paced to stay below the owner's create limit; one bounded download per guest/level, **not** a `-duration` steady loop. |
| `density` | `$B density $COMMON -n 30 -step 10` | Creates minimal VMs with quota-paced setup and refreshes them with `/bin/true` before each all-hot capture at 10/20/30 (earlier VMs would otherwise warm while the later ones are created). Then leaves **all** idle until they report thermal warm (max 2 min), and captures warm PSS, available RAM, per-VM distribution and per-step creation latency. |

Only `net`'s guests use public network egress; the sibling pair explicitly opts
into same-owner sibling access. The proxy's Node server is a **guest init
session**, relaunched on cold boot rather than lost with a one-shot exec;
the sibling server uses detached exec to outlive its initial piped request.
The minimal tier supplies `/bin/sh` and `curl` (not Python/Node). For `net`, the
default `-url` is `https://download.thinkbroadband.com/100MB.zip` (HTTP HEAD
200 with `Content-Length: 104857600` from agni-02 on 2026-10-05). Hetzner's
suggested speed-test URLs returned TLS EOF from that host when checked; use
`-url` to choose a reachable local/other mirror. `-net-timeout` bounds each
guest download and the curl result records failures as failures, not zero MB/s.
Public paths also depend on DNS/TLS and the guest proxy; document any external
CDN/throttling when comparing net runs.

## Flags and measurement semantics

Every scenario accepts `-api` (default `unix:///var/lib/bhatti/api.sock`),
`-token` / `-token-file` / `BHATTI_TOKEN`, `-out` (default `results`),
`-timeout` (default `2m` per daemon call), `-data-dir` (default
`/var/lib/bhatti` for host filesystem metadata), `-sample` (defaults true
when `/proc/stat` exists), `-owner-id` (optional exact daemon user ID),
`-tier` (default `minimal`), `-image` (override tier image), `-cpus` (1),
`-memory` (512 MiB), and `-network` (default daemon posture; also `none`,
`deny`, `public`, `allow-siblings`, `public-siblings`). A fork inherits its
source network policy, so no new policy can be specified for that request.
Scenario flags are in the table; `-n`, `-c`, `-k`, `-iterations`, `-lifecycle`,
`-step`, `-levels`, `-think`, `-fan-in`, `-aliases`, `-domain`, `-url`,
`-connect-samples`, `-net-timeout`, `-op`, and baseline-only `-cli` are exposed
by `<scenario> -h`. `-duration` defaults to 60s for `exec`, 600s for `swarm`,
and 15s **per hot concurrency level** for `proxy`.

Each invocation creates a directory `results/<UTC-timestamp>-<scenario>/` (or
under `-out`) with:

- `meta.json`: UTC start, scenario and flag values **excluding the key**,
  hostname, CPU model and logical cores, RAM, kernel, filesystem of the data
  directory, server version header, git SHA (build-time `-ldflags` when run
  outside a checkout), image/tier and API mode.
- `samples.jsonl`: one JSON line **per attempted operation**, wall-clock UTC
  `started_at`, floating-point `latency_ms`, `op`, outcome `ok|err|429|timeout`,
  diagnostic `error` on failure and optional numeric `values` (guest-side
  throughput/connect metrics and batch walls). There is **no retry** hidden
  inside measured operations. A guest `exec` with HTTP 200 but nonzero exit is
  an error. File reads verify bytes rather than trusting a possibly partial
  HTTP 200 stream.
- `summary.json`: per-op counts (`ok`, ordinary `errors`, `rate_limited`,
  `timeouts`), p50/p90/p95/p99/max/mean **milliseconds**, error fraction and
  operations/s; scenario details include batch elapsed/throughput, loop
  throughput, density points and the last host sample before teardown. Human
  sorted summary table prints to stdout. Percentiles are **nearest-rank**:
  sort only successful latencies, take rank `ceil(p*n)` (one-based). Failed
  attempts stay in count and error rate, not the latency percentile; if no
  successes, percentile values are zero and the success count is zero.
- `host.jsonl`: Linux `/proc` samples every 1 second: host CPU busy %, load1,
  MemAvailable MiB; host-wide daemon and netd process CPU%/PSS MiB, VMM count,
  summed CPU%/PSS and per-VMM PSS. Process CPU% is on a **one-core=100%** basis;
  host CPU% is total-busy fraction of all cores. With `-owner-id` (or once an
  API create returns `created_by`), VMMs are counted **bench-only** only if
  every observed VMM can be attributed by matching its exact vmspec path,
  state.json owner and PID. Otherwise `scope:host_total` and `scope_reason`
  say explicitly that other users' VMMs (including the production sandbox)
  may be included. The daemon/netd process totals always cover the host;
  `pss_complete:false` / `pss_unavailable_count` means permission prevented
  some PSS reads, **not** that PSS was zero. On non-Linux/`-sample=false`, the
  file records unsupported/disabled and density requires sampling.

Throughput in sustained scenarios divides **attempted** operation count by
the measured active window (also report successful throughput separately when
available). Ramp batch ops/s uses successful completions divided by the batch
wall elapsed, including the slowest call. For sequential operations, summary
ops/s is per full run elapsed time and is **not** a server capacity figure.
Default daemon rate limits include create 30/min burst 10 and exec/file-write
600/min burst 30; 429s are expected under some ramps and must not be omitted.
In steady `exec` and `proxy` hot loops, a worker waits 200ms **after recording**
a 429 before its **next new attempt**. In `swarm`, a rejected create pauses
that agent for 2s before a new loop. Rejected requests are never silently
retried. Cold `start` alone is not a user-visible wake; report
`cold_start_first_exec` or published-path first response too.
