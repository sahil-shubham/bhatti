# Declarative Platform — YAML Sandboxes, Internal DNS, Cron, Volumes nx

The CLI is imperative: `create`, `publish`, `destroy`, `volume create`,
`secret set`. That works for one user typing one sandbox. It breaks the
moment someone wants:

- A repo with a `bhattifile.yaml` checked in, applied in CI, the same way
  you `kubectl apply` or `docker compose up`
- Sandbox A reachable from sandbox B by name, not IP, with the IP free
  to change after a snapshot/restore
- A scheduled job ("run `npm run sync` in `etl` every 10 minutes")
  without a babysitter VM
- Two sandboxes mounting one volume with sane semantics

These four asks share one thing: **declarative state plus a controller
that converges to it**. Today bhatti has a controller per noun
(thermal manager, publish rules, IP pool, backup scheduler) and
imperative CLI on top. We need (a) a single declarative surface and
(b) two new controllers (DNS, cron) plus volume work that the DNS work
unlocks.

This plan picks the order, fixes the surface, and leaves volumes nx
deliberately under-designed because it's the largest of the four and
the others don't block on it.

---

## First Principles

The platform's nouns, smallest set:

```
sandbox          a Linux VM with cpus/mem/disk/image, owned by a user
volume           a named ext4 file with bytes and a lifecycle
publish rule     (sandbox, port) → public alias
secret           a name → ciphertext, mounted as env or file
schedule         (sandbox, cron, command) → run history
network record   (user, name) → IP, served as DNS A
template         a saveable bag of sandbox spec defaults (already exists)
```

Two operations matter for every noun:

1. **Reconcile** — given a desired spec, make the world match. Idempotent.
2. **Observe** — given the world, report the state.

Everything user-facing is built on those two. The CLI today does step 1
imperatively, one verb per noun. A YAML manifest gives us step 1 across
all nouns at once, with diff-and-converge.

The four tracks in this plan are:

| Track | What it adds | Substrate? |
|-------|--------------|-----------|
| **A. Declarative manifest + apply** | YAML → reconciler → API calls | Yes — others plug into it |
| **B. Internal DNS responder** | `<name>.sb` resolves, survives restore | No, but unlocks C |
| **C. Cron / scheduled jobs** | Wake + exec on a schedule | No, depends on B for sane multi-sandbox jobs |
| **D. Volumes nx (multi-mount + sharing)** | One volume, two readers | No, deferred — see §8 |

Track A goes first. Without it, B/C/D each invent their own config
shape and we ship four flavors of YAML. With it, every later feature
is "add a field to the manifest, add a controller, done."

---

## Design at a Glance

```
                   ┌─────────────────────────────┐
                   │  bhattifile.yaml            │
                   │  (declarative spec)         │
                   └──────────────┬──────────────┘
                                  │ bhatti apply
                                  ▼
              ┌───────────────────────────────────┐
              │  Apply engine (pkg/manifest)      │
              │   • parse + validate              │
              │   • diff vs API GET state         │
              │   • build action plan (DAG)       │
              │   • execute via existing REST API │
              └──────────┬────────────────────────┘
                         │ HTTP (existing API)
                         ▼
        ┌────────────────────────────────────────────┐
        │  bhatti server (pkg/server)                │
        │                                            │
        │  ┌──────────┐  ┌─────────┐  ┌──────────┐   │
        │  │ Sandbox  │  │ Publish │  │ Secret   │   │
        │  │ handlers │  │ rules   │  │ store    │   │
        │  └────┬─────┘  └────┬────┘  └────┬─────┘   │
        │       │             │            │         │
        │  ┌────▼─────────────▼────────────▼──────┐  │
        │  │ Engine (Firecracker)                 │  │
        │  └────┬─────────────────────────────────┘  │
        │       │                                    │
        │  ┌────▼──────┐  ┌──────────┐  ┌─────────┐  │
        │  │ Thermal   │  │ Cron     │  │ DNS     │  │
        │  │ manager   │  │ scheduler│  │ resolver│  │
        │  │ (today)   │  │ (NEW)    │  │ (NEW)   │  │
        │  └───────────┘  └──────────┘  └─────────┘  │
        └────────────────────────────────────────────┘
                                ▲
                                │ port 53/udp on each
                                │ user bridge gateway
                          ┌─────┴─────┐
                          │ guest VMs │  resolve api.sb, etl.sb, …
                          └───────────┘
```

The apply engine never reaches into the engine or store directly. It
talks the same REST API a human typing `bhatti create` does. This
matters: it means the manifest path is a strict superset of the
imperative path, the same auth/scoping/rate limits apply, and we can
ship `bhatti apply` as pure CLI without any server changes if we want.

---

## Track A — Declarative Manifest and Apply

### A.1 The schema

`bhattifile.yaml`, single document, anchored to one user (the API key
the CLI is configured with).

```yaml
# bhattifile.yaml
apiVersion: bhatti/v1
kind: Project
name: hermes-stack          # logical group; used as a label, not a namespace

defaults:                    # applied to every sandbox unless overridden
  image: minimal
  cpus: 1
  memory: 1024
  keep_hot: false

volumes:
  - name: pg-data
    size: 4096               # MB
  - name: shared-cache
    size: 1024

secrets:                     # references — values live in `bhatti secret set`
  - DATABASE_URL
  - OPENAI_API_KEY

sandboxes:
  - name: api
    image: docker
    cpus: 2
    memory: 2048
    init: |
      cd /workspace && npm ci && npm run start
    env:
      NODE_ENV: production
      DB_HOST: pg.sb         # ← internal DNS, see Track B
    secrets: [DATABASE_URL, OPENAI_API_KEY]
    files:
      - source: ./config/api.toml
        target: /etc/api.toml
    volumes:
      - name: shared-cache
        mount: /var/cache
        readonly: false
    publish:
      - port: 3000
        alias: hermes-api    # → hermes-api.<proxy_zone>
    schedules:
      - name: nightly-vacuum
        cron: "0 3 * * *"
        command: ["psql", "-c", "VACUUM ANALYZE"]
    keep_hot: true

  - name: pg
    image: minimal
    init: |
      apt-get install -y postgresql && systemctl start postgresql
    volumes:
      - name: pg-data
        mount: /var/lib/postgresql
    # no publish — internal-only, reachable as pg.sb from api

  - name: etl
    image: minimal
    cpus: 1
    memory: 512
    schedules:
      - cron: "*/10 * * * *"
        command: ["python", "/workspace/sync.py"]
        timeout: 300         # seconds
        on_failure: continue # continue|retry|alert
```

Design notes:

- **One file, one user.** Multi-user manifests are a server-side admin
  concern, not a project concern. A user with 5 different stacks runs
  `bhatti apply -f stack-a/bhattifile.yaml` and `... stack-b/...`
  separately. Cross-stack references via `bhatti.sh/depends-on` labels
  later if needed; not in v1.
- **Names are the identity.** `sandboxes[].name` is the unique key
  scoped to the user. Renaming changes identity (= destroy + create).
  This matches how `kubectl` treats names and how `bhatti edit --name`
  works today.
- **No status fields in the input.** No "running" / "stopped". The
  manifest is desired state; lifecycle is reconciled separately
  (`bhatti apply --stop name=etl`) — see A.4.
- **References, not values.** `secrets: [NAME]` references the secret
  store. The manifest is safe to commit. Same trick `docker-compose`
  uses with `secrets:`.
- **`init` is the only imperative bit.** Everything else is a fact.

The schema lives in `pkg/manifest/schema.go`:

```go
package manifest

type Manifest struct {
    APIVersion string                 `yaml:"apiVersion"` // must be "bhatti/v1"
    Kind       string                 `yaml:"kind"`       // must be "Project"
    Name       string                 `yaml:"name"`
    Labels     map[string]string      `yaml:"labels,omitempty"`
    Defaults   *SandboxSpec           `yaml:"defaults,omitempty"`
    Volumes    []VolumeSpec           `yaml:"volumes,omitempty"`
    Secrets    []string               `yaml:"secrets,omitempty"` // names only
    Sandboxes  []SandboxSpec          `yaml:"sandboxes"`
}

type SandboxSpec struct {
    Name       string            `yaml:"name"`
    Image      string            `yaml:"image,omitempty"`
    CPUs       float64           `yaml:"cpus,omitempty"`
    Memory     int               `yaml:"memory,omitempty"`     // MB
    DiskSize   int               `yaml:"disk_size,omitempty"`  // MB
    Init       string            `yaml:"init,omitempty"`
    Env        map[string]string `yaml:"env,omitempty"`
    Secrets    []string          `yaml:"secrets,omitempty"`
    Files      []FileInjection   `yaml:"files,omitempty"`
    Volumes    []VolumeMount     `yaml:"volumes,omitempty"`
    Publish    []PublishSpec     `yaml:"publish,omitempty"`
    Schedules  []ScheduleSpec    `yaml:"schedules,omitempty"`
    KeepHot    *bool             `yaml:"keep_hot,omitempty"`   // pointer = tri-state
    Hugepages  *bool             `yaml:"hugepages,omitempty"`
    Labels     map[string]string `yaml:"labels,omitempty"`
}

type VolumeSpec struct {
    Name string `yaml:"name"`
    Size int    `yaml:"size"` // MB
}

type VolumeMount struct {
    Name     string `yaml:"name"`
    Mount    string `yaml:"mount"`
    ReadOnly bool   `yaml:"readonly,omitempty"`
}

type PublishSpec struct {
    Port  int    `yaml:"port"`
    Alias string `yaml:"alias,omitempty"`
}

type FileInjection struct {
    Source string `yaml:"source"` // path on apply host
    Target string `yaml:"target"` // path inside guest
    Mode   string `yaml:"mode,omitempty"`
}

type ScheduleSpec struct {
    Name      string   `yaml:"name,omitempty"`
    Cron      string   `yaml:"cron"`
    Command   []string `yaml:"command"`
    Timeout   int      `yaml:"timeout,omitempty"`    // seconds
    OnFailure string   `yaml:"on_failure,omitempty"` // continue|retry|alert
}
```

`*bool` for `KeepHot` and `Hugepages` is deliberate — `nil` means
"inherit from defaults", `false` means "explicitly off". Without the
pointer we can't distinguish "user omitted" from "user set false."

### A.2 Validation

`pkg/manifest/validate.go`. Hard fails before any HTTP call:

- `apiVersion == "bhatti/v1"` and `kind == "Project"` (forward-compat)
- Sandbox names: `^[a-z0-9][a-z0-9-]{0,62}$` (matches existing rules)
- Volume names referenced in mounts exist in `volumes:`
- Secret names referenced in `secrets` exist in `manifest.Secrets`
  *and* in the secret store (this requires one `GET /secrets` call)
- Cron expressions parse (use `github.com/robfig/cron/v3` parser, no
  scheduler — just the parser, ~zero deps)
- Publish ports are 1–65535
- Aliases either match the alias regex or are empty (auto-generated
  server-side)
- File `source` paths exist and are readable; total inlined size <= 16MB
  (the `Files` field on the create API is base64-encoded inline; bigger
  payloads should be sent post-create via `file write`)
- After merging defaults, every sandbox has cpus > 0 and memory > 0

The error type carries a path so the CLI prints
`bhattifile.yaml: sandboxes[2].publish[0].alias: "Foo" is not a valid alias`.

### A.3 The reconciler

`pkg/manifest/apply.go`. Pure function: `(Manifest, RemoteState) → Plan`.
Plan is a list of typed actions:

```go
type Action interface{ Describe() string }

type CreateVolume struct{ Name string; SizeMB int }
type DeleteVolume struct{ Name string }
type CreateSandbox struct{ Spec SandboxSpec }
type UpdateSandbox struct{ Name string; Patch SandboxPatch }
type DestroySandbox struct{ Name string }
type StopSandbox struct{ Name string }
type StartSandbox struct{ Name string }
type CreatePublish struct{ Sandbox string; Port int; Alias string }
type DeletePublish struct{ Sandbox string; Port int }
type AttachVolume struct{ Sandbox, Volume, Mount string; ReadOnly bool }
type DetachVolume struct{ Sandbox, Volume string }
type WriteFile struct{ Sandbox, Target string; Content []byte; Mode os.FileMode }
type UpsertSchedule struct{ Sandbox string; Spec ScheduleSpec }
type DeleteSchedule struct{ Sandbox, Name string }
```

The plan has hard ordering:

```
1. Volumes      — create new, before sandboxes that mount them
2. Sandboxes    — create new (bringing them up running)
3. Files        — inject into running sandboxes
4. Volumes      — attach to existing sandboxes (already-running case)
5. Publish      — after sandboxes are up, so EnsureHot has a target
6. Schedules    — last, so DNS records (Track B) and ports already exist
7. Detach/destroy — reverse order (publish → schedules → mounts → sb → vol)
```

Within a phase, actions are independent and can run in parallel
(bounded by an `errgroup` of 4 — same bound the apply engine uses for
HTTP calls today). Across phases, hard barrier.

Diff is field-by-field, not blob-equality. Two reasons:

- The remote state has fields the manifest can't express (engine_id,
  IP, created_at). Blob-equality always says "drift."
- Some fields are immutable post-create (cpus, memory, image — bhatti
  can't resize a running VM). The diff has to either flag them as
  "destroy + recreate" or "warning, ignored." We pick **warn-and-skip**
  by default and `--recreate-on-immutable-drift` to opt into the
  destructive path. This matches `terraform plan` semantics and avoids
  the worst class of "I changed `memory: 1024` → `memory: 2048` and
  apply destroyed my database" surprises.

The mutable set (changeable on a running sandbox via `bhatti edit` or
new endpoints) is small and explicit:

```
keep_hot          → existing /sandboxes/:id PATCH
publish[]         → POST/DELETE /sandboxes/:id/publish
schedules[]       → NEW, see Track C
volumes[].mount   → detach + attach (requires sandbox running)
secrets[]         → requires recreate (inserted at boot via config drive)
env[]             → requires recreate (same)
init              → requires recreate (only runs at boot)
files[]           → live-write via /sandboxes/:id/files (no recreate)
```

### A.4 Lifecycle commands

`bhatti apply` brings declared sandboxes up to **running**. Operators
need to stop/start without editing the file:

```
bhatti apply [-f bhattifile.yaml]      # converge to running
bhatti apply --stop NAME [...]         # converge, but stop these
bhatti apply --start NAME [...]        # converge, then start these
bhatti apply --plan                    # print plan, don't execute
bhatti apply --diff                    # print plan as a diff
bhatti apply --prune                   # also destroy sandboxes not in manifest
bhatti destroy [-f bhattifile.yaml]    # delete everything in the manifest
```

Without `--prune`, `apply` is **additive-by-default**. A sandbox that
exists in the API but not in the manifest is left alone. This is the
single most-requested guardrail in every compose-style tool that
shipped without it. `--prune` opts into destructive convergence.

State that lets `apply` work without round-tripping the entire account:

- A `bhatti.sh/managed-by=<project-name>` label written into
  `sandbox.labels`, `volume.labels`, etc. on every create
- `--prune` only considers resources with that label

This matches the `app.kubernetes.io/managed-by` convention and is the
right boundary: bhatti users will mix declarative and ad-hoc sandboxes
on the same account.

### A.5 Server changes for A

The reconciler is mostly client-side, but a few API gaps need filling
to make it round-trip cleanly:

| Gap | Add |
|-----|-----|
| Labels on sandboxes | `labels TEXT` JSON column on `sandboxes` table; accepted in `POST /sandboxes`, returned in `GET` |
| Labels on volumes | same on `volumes_v2` |
| Bulk read | `GET /sandboxes?label=managed-by=foo` filter (already partially supported, verify) |
| Detach without destroy | `DELETE /sandboxes/:id/volumes/:name` (verify; if missing, add) |
| Mutable port re-publish | already works via existing publish/unpublish |
| Manifest endpoint (optional) | `POST /apply` with the manifest body, server-side reconcile — defer; client-side is enough for v1 |

Schema migration in `pkg/store/store.go`:

```sql
ALTER TABLE sandboxes ADD COLUMN labels_json TEXT NOT NULL DEFAULT '{}';
ALTER TABLE volumes_v2 ADD COLUMN labels_json TEXT NOT NULL DEFAULT '{}';
CREATE INDEX IF NOT EXISTS idx_sandboxes_labels ON sandboxes(labels_json);
```

The label query goes through SQLite's `json_extract`:
```sql
SELECT … FROM sandboxes WHERE json_extract(labels_json, '$."managed-by"') = ?
```

Existing rows pick up `'{}'` from the default. No data migration.

### A.6 CLI

`cmd/bhatti/apply_cmd.go`:

```go
var applyCmd = &cobra.Command{
    Use:   "apply",
    Short: "Reconcile resources to match a declarative manifest",
    Example: `  bhatti apply -f bhattifile.yaml
  bhatti apply --plan
  bhatti apply --diff
  bhatti apply --prune
  bhatti destroy -f bhattifile.yaml`,
    RunE: runApply,
}

func init() {
    applyCmd.Flags().StringP("file", "f", "bhattifile.yaml", "Manifest file")
    applyCmd.Flags().Bool("plan", false, "Print the action plan and exit")
    applyCmd.Flags().Bool("diff", false, "Print a diff of declared vs actual and exit")
    applyCmd.Flags().Bool("prune", false, "Destroy managed resources not in manifest")
    applyCmd.Flags().StringSlice("stop", nil, "Stop these sandboxes after reconcile")
    applyCmd.Flags().StringSlice("start", nil, "Start these sandboxes after reconcile")
    applyCmd.Flags().Int("parallelism", 4, "Max concurrent API calls")
    rootCmd.AddCommand(applyCmd)
}
```

Output is intentionally plain:

```
$ bhatti apply --plan -f bhattifile.yaml
Plan for project hermes-stack (3 sandboxes, 2 volumes):

  + create volume pg-data (4096 MB)
  + create volume shared-cache (1024 MB)
  + create sandbox api (2 vCPU, 2048 MB, image=docker)
  + create sandbox pg (1 vCPU, 1024 MB, image=minimal)
  + create sandbox etl (1 vCPU, 512 MB, image=minimal)
  + publish api:3000 → hermes-api
  + schedule etl every 10m: python /workspace/sync.py
  + schedule api 0 3 * * *: psql -c VACUUM ANALYZE

5 to add, 0 to change, 0 to destroy.
```

`--diff` shows old/new field-by-field for `change` actions.

### A.7 Tests for A

`pkg/manifest/`:

- `TestParseValid` — golden manifest → expected struct
- `TestParseInvalid` — table of bad inputs, each fails with a path-bearing error
- `TestValidate*` — one per rule (cron, alias, secret-not-found, etc.)
- `TestDiffEmpty` — same manifest, empty plan
- `TestDiffAdd` — sandbox absent → CreateSandbox + child actions, ordered
- `TestDiffRemoveWithoutPrune` — sandbox in remote, not in manifest, no prune → no-op
- `TestDiffRemoveWithPrune` — same but `--prune` → DestroySandbox
- `TestDiffImmutableField` — cpus changed → warn-and-skip by default, recreate with flag
- `TestDiffMutableFieldKeepHot` — flips → `UpdateSandbox{Patch: {KeepHot: ...}}`
- `TestPlanOrdering` — synthetic "create everything" plan, assert volumes < sandboxes < publish < schedules
- `TestApplyEndToEndDocker` — uses Docker engine, full cycle: apply → list → modify → apply → destroy
- `TestApplyParallelism` — 8 sandboxes, parallelism=2, observe ≤ 2 in-flight

CLI tests in `cmd/bhatti/apply_cmd_test.go` mirror the existing
`cli_ux_test.go` structure (mock server, snapshot stdout).

---

## Track B — Internal DNS Responder

### B.1 The need, sharply

After Track A, `bhattifile.yaml` says:

```yaml
env:
  DB_HOST: pg.sb
```

Without DNS, the only way `api` reaches `pg` is by IP. IPs are
allocated by `pkg/engine/firecracker/`'s per-user pool, change across
restore (sometimes — see `lifecycle.go:287` "TAP is gone"), and aren't
even visible to the user without an `inspect` call. We need a stable
name → live IP resolver, owned by bhatti, scoped per user.

### B.2 Why not /etc/hosts?

We already write `/etc/hosts` in the systemd-rc work. It's static —
written once at boot from the config drive. To add a new sandbox after
boot, every existing sandbox's `/etc/hosts` would need updating. That's
a fan-out write to N VMs over the agent on every create, with retry
semantics, partial-failure handling, and a race against in-flight
DNS lookups in long-running processes. DNS is the right mechanism;
that's why DNS exists.

### B.3 Architecture

One DNS server **per user bridge gateway**, listening on
`<gateway>:53/udp` (and `/tcp` for AXFR-class queries we won't support
but should at least RST cleanly). `pkg/engine/firecracker/engine.go`
already has a `UserNetwork` struct keyed per user; the DNS server is
a child of that.

```go
// pkg/dns/resolver.go
type Resolver struct {
    store    *store.Store     // for sandbox lookups
    user     *store.User
    gateway  net.IP           // 192.168.x.1
    bridge   string           // brbhatti-uX
    cache    *cache.Cache     // name → record, refreshed on store changes
    server   *dns.Server      // miekg/dns
}

func (r *Resolver) Start() error {
    r.server = &dns.Server{
        Addr:    net.JoinHostPort(r.gateway.String(), "53"),
        Net:     "udp",
        Handler: dns.HandlerFunc(r.handle),
    }
    go r.server.ListenAndServe()
    // tcp variant on the same port for fallback
    return nil
}

func (r *Resolver) handle(w dns.ResponseWriter, req *dns.Msg) {
    if len(req.Question) != 1 {
        // refuse multi-question (none of the resolvers we care about send these)
        m := new(dns.Msg).SetRcode(req, dns.RcodeRefused)
        w.WriteMsg(m); return
    }
    q := req.Question[0]
    name := strings.ToLower(strings.TrimSuffix(q.Name, "."))

    // .sb (the chosen TLD; see §B.4)
    if strings.HasSuffix(name, ".sb") {
        sbName := strings.TrimSuffix(name, ".sb")
        if ip, ok := r.cache.Get(sbName); ok {
            r.replyA(w, req, q, ip); return
        }
        sb, err := r.store.GetSandboxByUserAndName(r.user.ID, sbName)
        if err != nil || sb.IP == "" {
            r.replyNXDomain(w, req); return
        }
        r.cache.Set(sbName, sb.IP, 30*time.Second)
        r.replyA(w, req, q, sb.IP); return
    }

    // Anything else → forward to upstream (1.1.1.1, 8.8.8.8 — same
    // as the kernel ip= line uses today).
    r.forward(w, req)
}
```

Dependencies: `github.com/miekg/dns` (the standard Go DNS library,
mature, single dependency tree, used everywhere from CoreDNS down).
The `cache` is `patrickmn/go-cache` or a 60-line homegrown one — the
working set is "live sandboxes for this user," tens to low hundreds.

### B.4 Why `.sb`, not `.local` or `.bhatti`

- `.local` is mDNS (RFC 6762). Don't conflict.
- `.internal` is reserved by ICANN as of 2024 for private use — fine
  but verbose.
- `.bhatti` looks branded on every guest's resolv.conf, ugly.
- `.sb` is two characters, unambiguous, isn't a public TLD, and reads
  well in env vars (`DB_HOST=pg.sb`). The risk: it could become a
  public TLD some year. If that happens, change the constant in one
  place; resolution short-circuits at the server before forwarding.

Configurable via `Config.InternalSuffix string` for users who want
`.internal` or `.bhatti` instead. Default `.sb`.

### B.5 Wiring DNS into the guest

The guest already gets DNS via the kernel `ip=` line:
```
ip=192.168.137.2::192.168.137.1:255.255.255.0::eth0:off:1.1.1.1:8.8.8.8:
```

This puts `1.1.1.1` and `8.8.8.8` into `/etc/resolv.conf`. We change
the line to put **the gateway first**:

```
ip=192.168.137.2::192.168.137.1:255.255.255.0::eth0:off:192.168.137.1:1.1.1.1:
```

The gateway is the bridge IP, which is where our resolver listens.
The kernel writes the resolv.conf in order, so `<gateway>` is queried
first; on NXDOMAIN/timeout it falls through to `1.1.1.1`. This is
exactly how Docker's embedded DNS works.

For systemd-resolved guests (the future systemd path), we set the same
list via the config drive's resolv.conf, which lohar copies to
`/etc/resolv.conf` and (where systemd-resolved is active) gets picked
up via the `DNS=` directive in `/etc/systemd/resolved.conf.d/bhatti.conf`.

### B.6 Updating the cache

Two mechanisms, layered:

1. **TTL-based** — every entry has 30s TTL; under load, normal pull
   model with bounded staleness. Fine for "I just created a sandbox
   and want to curl it from another."
2. **Push-on-change** — `pkg/store/sandbox.go` `Create`, `Destroy`,
   IP-changing path in `lifecycle.go:287` all emit on a fan-out chan.
   The resolver subscribes and invalidates the affected name. Removes
   the 30s window.

The push channel is best-effort. If the resolver misses an event
(restart, channel buffer overflow), the TTL refresh closes the gap.
**Never block the create path on the push.**

### B.7 The hard part: wake-on-connect

If `api` calls `pg.sb` and `pg` is **cold** (memory snapshotted to disk),
DNS will resolve to `pg`'s last IP, but the TCP SYN to that IP goes
nowhere — Firecracker isn't running, the TAP is down, the ARP entry is
stale.

The reverse-proxy has the same problem and solves it because every
request goes through `Engine.Tunnel()` which calls `EnsureHot` first.
Sandbox-to-sandbox traffic doesn't go through any host code path —
it's pure L2 between guests on the same bridge. There's no place to
inject a wake.

Three options, ordered by effort:

**Option 1: Wake on resolve.** Cheapest. The DNS resolver, before
returning the A record, calls `EnsureHot(targetSandbox)` if the target
is not hot. The caller's TCP SYN arrives a few hundred ms later,
finding the sandbox awake and listening. This doesn't help repeated
connects to a sandbox that *re-cools* between them — the second
connect's DNS is cached in the client (TTL up to 30s) and skips the
resolver. Mitigation: short TTL (5s) for cold→hot transitions.

**Option 2: ARP-trap on the bridge.** Run an ARP monitor on the bridge
that observes "ARP request for 192.168.x.7 on bridge brbhatti-uX, no
reply." Look up which sandbox owns .7, call `EnsureHot`, then let the
ARP retransmit succeed. This is invasive (raw socket, BPF filter) but
correct. Closest analog is libvirt's approach for paused VMs.

**Option 3: Short-circuit through the host.** Don't use DNS A records
at all — return `127.0.0.1` and run a per-user proxy on the gateway
that NATs to the right sandbox after `EnsureHot`. Simple, slow (every
packet through the host), defeats the per-user-bridge L2 isolation.

**Pick Option 1 for v1, with an explicit `keep_hot: true` in the
manifest for sandboxes that other sandboxes call.** Ship Option 2 as
a follow-up if real users hit the cold-loop. Document it.

This is the kind of decision that should live in `docs/decisions.md`
once made; the trade is real and worth being honest about.

### B.8 Server changes for B

```
pkg/dns/resolver.go      — the resolver
pkg/dns/cache.go         — TTL cache + invalidation channel
pkg/engine/firecracker/  — start/stop resolver per UserNetwork
                           (in getOrCreateUserNetwork / removeUserNetworkIfEmpty)
pkg/server/server.go     — config: InternalSuffix, DNSEnabled
config.go                — same fields
```

The boot args change is in `pkg/engine/firecracker/create.go` where
the `ip=` line is built. Backward compat: existing snapshots embed
the old resolv.conf; on first resume they keep working (1.1.1.1 only,
no `.sb` resolution). Documented.

Iptables: bridge traffic to/from `<gateway>:53` must be allowed by the
FORWARD rules (already true — the bridge accepts all internal traffic).
Cross-user blocking is already enforced at L2 by the per-user bridge —
each user's resolver is unreachable from other users.

### B.9 Tests for B

- `TestResolveSandboxByName` — create a, b in same user, b resolves a.sb to a's IP
- `TestResolveCrossUser` — alice's bridge, bob's sandbox name → NXDOMAIN
- `TestResolveCacheInvalidationOnDestroy` — destroy a, next query → NXDOMAIN
- `TestResolveCacheInvalidationOnRecreate` — destroy + recreate same name → new IP
- `TestForwardUpstream` — query example.com → forwarded to 1.1.1.1
- `TestWakeOnResolve` — resolve cold sandbox → EnsureHot called, sandbox hot before reply
- `TestDNSAfterRestore` — snapshot user network, restart bhatti, resolver back up, queries work
- `TestDNSRefuseUnsupported` — multi-question, AXFR → REFUSED, no panic
- Integration: real guest sandbox runs `getent hosts pg.sb` → returns pg's IP

---

## Track C — Cron / Scheduled Jobs

### C.1 Schema

`pkg/store/schedule.go`, new table:

```sql
CREATE TABLE IF NOT EXISTS schedules (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,
    sandbox_id TEXT NOT NULL,
    name TEXT NOT NULL,
    cron TEXT NOT NULL,
    command_json TEXT NOT NULL,           -- JSON array of args
    timeout_seconds INTEGER NOT NULL DEFAULT 300,
    on_failure TEXT NOT NULL DEFAULT 'continue',  -- continue|retry|alert
    enabled INTEGER NOT NULL DEFAULT 1,
    last_run_at DATETIME,
    next_run_at DATETIME,                 -- denormalized for fast scan
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(sandbox_id, name)
);
CREATE INDEX IF NOT EXISTS idx_schedules_next_run ON schedules(enabled, next_run_at);
CREATE INDEX IF NOT EXISTS idx_schedules_sandbox ON schedules(sandbox_id);

CREATE TABLE IF NOT EXISTS schedule_runs (
    id TEXT PRIMARY KEY,
    schedule_id TEXT NOT NULL,
    started_at DATETIME NOT NULL,
    finished_at DATETIME,
    exit_code INTEGER,
    output BLOB,                          -- last 64KB stdout+stderr, gzipped
    triggered_by TEXT NOT NULL DEFAULT 'cron'  -- cron|manual
);
CREATE INDEX IF NOT EXISTS idx_schedule_runs_schedule ON schedule_runs(schedule_id, started_at DESC);
```

64KB capped output mirrors lohar's scrollback. Anything bigger should
go into a volume; we don't want the metadata DB to hold logs.

### C.2 The scheduler loop

`pkg/server/scheduler.go`. One goroutine per server (not per user —
the scan is cheap, ~one indexed query every 10s):

```go
type Scheduler struct {
    store  *store.Store
    engine engine.Engine
    server *Server  // for EnsureHot + exec
    tick   time.Duration  // 10s default
    sem    chan struct{}  // bound concurrent runs (e.g. 32)
}

func (s *Scheduler) Run(ctx context.Context) {
    t := time.NewTicker(s.tick)
    defer t.Stop()
    for {
        select {
        case <-ctx.Done(): return
        case now := <-t.C:
            due, _ := s.store.DueSchedules(now)
            for _, sched := range due {
                go s.runOne(ctx, sched)
            }
        }
    }
}
```

`DueSchedules` returns `enabled = 1 AND next_run_at <= now()`,
LIMIT 200 to bound a backlog. Each `runOne` acquires a slot in `sem`,
calls `EnsureHot` on the target sandbox, opens an exec session, runs
the command with a deadline = `timeout_seconds`, persists a
`schedule_runs` row, computes the next `next_run_at` from the cron
expression, and updates the schedule row in a single transaction.

Key behaviors:

- **At-least-once.** If the server crashes between exec start and DB
  update, `next_run_at` is unchanged and the job runs again on
  restart. Document this; advise idempotent jobs.
- **Catch-up policy: don't.** If a server was down for 3 hours and a
  job was scheduled every 10 minutes, we don't fire 18 missed runs.
  Set `next_run_at` to the next *future* tick. This is what `cron`
  does; `anacron` is the opposite. Bhatti is `cron`.
- **Concurrency per schedule = 1.** A second run is suppressed if the
  previous one is still going. Implemented via a `running INTEGER`
  flag on the schedule row, set in the same transaction that picks it
  up (`UPDATE … WHERE running = 0 AND next_run_at <= ? RETURNING …`).
- **Failure policy.** `continue` (default) records the failure, advances
  `next_run_at`, moves on. `retry` re-queues with exponential backoff
  up to 3 attempts. `alert` writes an event row consumable by `bhatti
  admin events` and (later) a webhook.

### C.3 Why the scheduler runs in the daemon, not the guest

Three reasons we don't put `cron` inside each VM:

1. **Cold sandboxes.** A cold-snapshotted VM with cron jobs would never
   fire — its kernel is paused. The whole point is that cron *wakes
   it*.
2. **Observability.** Run history, retries, alerts are bhatti's job;
   piping cron output back to a central log is a per-image headache
   we'd hit on every rootfs tier.
3. **Resource accounting.** Per-user limits (max parallel jobs) belong
   in the daemon.

### C.4 API + CLI

```
POST   /sandboxes/:id/schedules        body: ScheduleSpec
GET    /sandboxes/:id/schedules
DELETE /sandboxes/:id/schedules/:name
POST   /sandboxes/:id/schedules/:name/run     # manual trigger
GET    /sandboxes/:id/schedules/:name/runs    # history (last 100)
```

CLI:

```
bhatti schedule add api --name vacuum --cron "0 3 * * *" -- psql -c VACUUM
bhatti schedule list api
bhatti schedule run api vacuum
bhatti schedule logs api vacuum --tail
bhatti schedule rm api vacuum
```

`schedules:` in the manifest reconciles to these endpoints exactly.

### C.5 Wake semantics

Same `EnsureHot` path as the public proxy. The scheduler holds an
inline `singleflight.Group` so two near-simultaneous runs of two
different schedules in the same sandbox share one wake. Wake counts
toward `resumeSem`. Document that cron wakes are charged like proxy
wakes.

### C.6 Tests for C

- `TestScheduleCreate` / list / delete / unique
- `TestScheduleNextRunCalculation` — `*/10 * * * *`, now=12:01, next=12:10
- `TestScheduleFireOnce` — 1s cron, in-memory engine, observe one exec
- `TestScheduleSuppressesOverlap` — long-running job, second tick: skipped, run count = 1
- `TestScheduleRetryBackoff` — `on_failure: retry`, command exits 1, three runs with rising delay
- `TestScheduleCatchupNo` — clock jumps forward 1h, only one run fires, next is in the future
- `TestScheduleWakesCold` — cold sandbox, fire, observe EnsureHot called
- `TestScheduleManualTrigger` — POST /run, observe immediate run, schedule unaffected
- `TestScheduleRunsRetention` — 200 runs, list returns last 100

---

## Track D — Volumes nx (deliberately under-designed)

The fourth track is volumes 2.0: shared mounts (one volume, multiple
sandboxes), better lifecycle (resize, clone), and the snapshot story
(today a volume backup is `dd | gzip | s3`; we want differential).

I am going to **not design this here.** Three reasons:

1. **It's the largest of the four.** Multi-mount needs a coherent
   lock/visibility story (NFSv4 lease semantics? virtio-fs DAX?
   read-only-fanout-only?). Each option has a months-long blast
   radius on the engine, snapshots, and migrations. Doing it well is
   one full plan document, not a section.
2. **The other three tracks don't need it.** Track A's manifest
   already supports volumes; declaring `readonly: false` to two
   sandboxes will fail at the existing API ("volume in use"). That's
   a fine error to keep until D ships.
3. **The right design comes from real usage.** Once Track A lands,
   we'll have a corpus of `bhattifile.yaml`s in the wild that tell us
   what people actually want — read-many for static assets? RWX for
   shared scratch? Database-on-shared-volume (almost certainly no)?
   Designing in advance is a guess.

Forward-compat hooks we **do** add now, in Track A:

- `volumes[].access_mode: rwx | rwo | rox` field, validated, defaulting
  to `rwo` (the only one supported today). RWX is rejected with
  "not yet implemented" until Track D.
- The volume schema in the store already has `volume_attachments`
  one-to-many, so the data model is fine.

Track D becomes its own plan: `PLAN-volumes-nx.md`. It depends on
nothing, but is sequenced last because B and C give us more user
signal.

---

## Dependency Graph and Sequencing

```
Track A (manifest + apply)
   │
   ├── A.1 schema + parser           ──┐
   ├── A.2 validator                   │   independent
   ├── A.3 reconciler/diff             │
   ├── A.4 lifecycle commands          │
   ├── A.5 server: labels             ──┘
   ├── A.6 CLI
   └── A.7 tests
                                       ↓
Track B (internal DNS)        — depends on nothing in A code-wise,
   │                            but A's manifest is the consumer:
   │                            shipping B without A means it's a
   │                            CLI-only feature with no declarative
   │                            config and weak test surface. Order
   │                            B *after* A.
   ├── B.1 resolver
   ├── B.2 cache + push channel
   ├── B.3 wake-on-resolve hook
   ├── B.4 boot args change
   └── B.5 tests
                                       ↓
Track C (cron / scheduled jobs) — strongly benefits from B:
   │                              schedules whose command hits
   │                              "pg.sb" need DNS to mean what
   │                              it says.
   ├── C.1 schema (schedules + runs)
   ├── C.2 scheduler loop
   ├── C.3 API + handlers
   ├── C.4 CLI
   └── C.5 tests
                                       ↓
Track D (volumes nx)            — own plan; uses A's reservation
                                  fields (access_mode) but no other
                                  coupling.
```

### Recommended ship order, with rough sizing

| # | Track | Slice | Effort | Ships as |
|---|-------|-------|--------|----------|
| 1 | A | Schema + parser + validator + diff (no apply) | 1 wk | `bhatti apply --plan` |
| 2 | A | Apply executor + labels + prune + tests | 1 wk | `bhatti apply` GA |
| 3 | B | Resolver + cache + boot-args + tests (no wake) | 1 wk | `name.sb` works for hot sandboxes |
| 4 | B | Wake-on-resolve, push invalidation, restore | 4 d | `name.sb` works for cold |
| 5 | C | Schema + scheduler loop + handlers (manual trigger only) | 4 d | `bhatti schedule run` |
| 6 | C | Cron parser + due loop + retry + manifest integration | 1 wk | `schedules:` in YAML works end-to-end |
| 7 | D | Separate plan document | TBD | TBD |

**Total to "fuller serverless platform" (1–6): ~5 weeks of focused work.**
The slicing is deliberate: each row leaves the system in a shippable
state with documented behavior, even if later rows never land.

### Why A first, not B

It's tempting to do internal DNS first because it's a small, contained,
high-leverage feature. But:

1. **Without A, B's main use case (env=`DB_HOST: pg.sb`) is `bhatti
   create --env DB_HOST=pg.sb` — typed at a shell, on every
   create.** The value of `pg.sb` over a hardcoded IP only shows up
   when something *else* is doing the wiring, and that something is A.
2. **A surfaces the weak parts of the API (labels, partial updates)
   that we'd rather find before adding two more controllers (DNS,
   cron) that depend on it.** Building C on a shaky API means
   reworking it twice.
3. **A is the most-asked-for feature externally** (it's what every
   compose-shaped tool gives, and bhatti users are coming from
   docker-compose / fly.toml / railway).

---

## Cross-cutting Concerns

### Multi-tenancy and per-user limits

Every track has a per-user resource cost we need caps for:

| Resource | Existing cap | Cap to add |
|----------|--------------|-----------|
| Sandboxes | `max_sandboxes` | — |
| Volume MB | `max_volume_storage_mb` | — |
| Images | `max_images` | — |
| Snapshots | `max_snapshots` | — |
| Schedules | — | `max_schedules` (default 50) |
| Schedule runs/min | — | `max_schedule_runs_per_min` (default 100) |
| DNS QPS | — | per-user rate limit on resolver, default 1000 qps |

Add to `pkg/store/user.go` `User` struct, default values in
`CreateUser`, surface in `bhatti user create`/`update`.

### Observability

Each new controller publishes metrics through the existing
`pkg/server/observability.go` mechanism:

```
bhatti_apply_actions_total{action,result}
bhatti_apply_duration_seconds{phase}
bhatti_dns_queries_total{user,result}    # result: hit|miss|forward|nxdomain
bhatti_dns_query_duration_seconds
bhatti_schedule_runs_total{result}       # result: ok|fail|skipped|retry
bhatti_schedule_pending                  # gauge, schedules with next_run_at < now
```

And events (via `pkg/server/event_recorder.go`):

```
schedule.run.started        sandbox=etl schedule=sync
schedule.run.completed      sandbox=etl schedule=sync exit=0 duration=42s
schedule.run.failed         sandbox=etl schedule=sync exit=1 stderr_tail=...
apply.plan.executed         project=hermes-stack actions=12
dns.wake_triggered          sandbox=pg from=api
```

These show up in `bhatti admin events` for free.

### Backward compat

- All YAML fields are additive; missing fields use `defaults:` then
  hardcoded defaults
- Existing imperative CLI keeps working unchanged
- Sandboxes created before A get `labels = {}` and are visible to
  `apply --prune` only if their name matches; `--prune` won't touch
  unlabeled sandboxes by default (`--prune-unlabeled` opt-in)
- Old snapshots resume without DNS resolution working until the next
  reboot — documented; users can `bhatti edit --restart` to refresh
- Schedules survive sandbox snapshot/restore (they're in the daemon,
  not the guest)

### Failure modes I want flagged early

1. **Apply mid-failure.** Half the plan succeeds, then the network
   blips. Today's CLI errors and exits. With a plan, partial state is
   visible and recoverable: re-run apply, the diff is now smaller,
   converge again. This is correct behavior but counter-intuitive
   for users used to atomic Terraform-style applies. Document it.
2. **DNS poisoning across users via shared bridge name?** No — each
   user has their own bridge. But a future "shared bridge" feature
   (cross-user collaboration) would need the resolver to scope by
   user, not bridge. Keep the API user-keyed even though today the
   bridge is sufficient.
3. **Cron storm on restart.** Server down 1h, 60 schedules due. Even
   with no-catchup, every schedule has its `next_run_at` in the past
   and fires on first tick. Bound the per-user concurrency, stagger
   wakes (small jitter on `next_run_at` calculation: `+ rand(30s)`
   for any "in the past" recovery).
4. **Wake-on-resolve loop.** A misbehaving sandbox calls itself
   (`localhost.sb`?) and triggers wake on its own DNS. Don't allow
   self-resolution to wake — the resolver compares the source IP
   against the target sandbox's IP and short-circuits.

---

## Open Questions

These are the decisions I'd defer until v1 of each track is in users'
hands, but flag now so we can think about them:

1. **Server-side apply?** A `POST /apply` endpoint that takes the
   manifest and returns the plan/result — required for a future web UI,
   convenient for CI without a full CLI. Defer; client-side apply
   is sufficient and the server endpoint is a thin shell over the same
   reconciler.

2. **Cross-project references?** Do we want `from-project: other-stack`
   in the manifest? Almost certainly no for v1 — it's a kubectl-namespace
   trap. Keep manifests self-contained.

3. **Where do init scripts live?** Inline in YAML feels wrong for
   anything past 5 lines. We already have `--file` injection. Add a
   `script: ./bootstrap.sh` field that's sugar for "inject this file
   and run it as init."

4. **DNS over the proxy for external clients?** Could publish
   `pg.api.bhatti.sh` resolving to the public proxy. Probably yes, but
   that's the public proxy plan's territory, not internal DNS.

5. **Schedule output streaming.** Should `bhatti schedule logs --tail`
   stream a *running* job's stdout, or only finished runs? Streaming
   is plumbing we already have (NDJSON exec); adds 100 LoC. Add it.

6. **Manifest schema versioning past v1.** Adopt the Kubernetes
   convention: `apiVersion: bhatti/v1` is stable; future breaking
   changes go to `bhatti/v2` and the parser dispatches. `v1alpha1`
   is the wrong move — we have no users yet, ship v1 and live with it.

---

## Summary

The four asks reduce to one system: a desired-state surface
(`bhattifile.yaml`) and three controllers (the existing CRUD path,
plus DNS and cron) that converge it. Volumes nx is its own beast and
doesn't block any of this.

Order of operations:

```
1. Track A — declarative manifest + apply (substrate)
2. Track B — internal DNS (small, high leverage, unlocks C)
3. Track C — scheduled jobs (the "serverless" claim)
4. Track D — volumes nx (separate plan, real-world signal first)
```

Build A so that B and C are "add a controller, add a manifest field,"
not "add a feature, retrofit a YAML schema." That's the whole point
of doing A first, even though B and C are individually shorter.
