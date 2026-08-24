# Adding bhatti as a Hermes Agent terminal backend — implementation plan

> Scoping/planning doc. Target: [NousResearch/hermes-agent](https://github.com/NousResearch/hermes-agent).
> bhatti exposes a clean REST API (`docs/reference/api`) that maps 1:1 onto
> Hermes' `BaseEnvironment` contract, so this is a small, well-trodden change.

## 1. Landscape — what already exists upstream

**Built-in backends** (`tools/environments/`): `local`, `docker`, `ssh`,
`singularity`, `modal` (direct + Nous-managed), `daytona`.

**Community backends in flight** (open PRs, none merged — this is an actively
contested area, so a clean PR has a good shot):

| PR | Backend | Note |
|----|---------|------|
| #23466 | FastVM | microVM, snapshot-backed — closest analog, "wire everything" PR (2489 +/45 files), changes requested |
| #14993 | microsandbox | minimal 5-file PR (532 lines) — the lean template |
| #18348 | E2B | cloud sandbox |
| #20809 | Blaxel | cloud sandbox |
| #8902  | Novita | cloud sandbox |
| #30112 | Sprites | stateful sandbox |
| #14191 | nsjail | local sandbox |
| #24087 | Alibaba AgentRun, #15034 Koyeb, #20247 boxd, #6867 Apple Container, #2019 Morph | misc |

**No existing bhatti/firecracker reference** in the hermes tree — clean slate.

**Relevant design issues to reference in the PR:**
- #1855 — multi-backend terminal (local + N named remotes). Our backend slots
  into the single-backend model today; design with the future multi-backend
  config in mind.
- #28807 — no startup reconciliation: orphaned sandboxes never reaped. bhatti's
  `keep_hot=false` + thermal auto-pause and `DELETE` semantics let us avoid this
  class of bug; document the cleanup story.
- #28299 / #30336 — Daytona/Docker concurrency bugs from shared sandbox names.
  We key sandboxes by `task_id` (`hermes-<task_id>`) to avoid this.

## 2. How a backend plugs in (the contract)

`tools/environments/base.py :: BaseEnvironment` (ABC). The base class already
implements `execute()`, session snapshotting, CWD tracking via stdout markers,
interrupt handling, timeout enforcement, and output draining. A backend only
implements:

- `_run_bash(cmd_string, *, login, timeout, stdin_data) -> ProcessHandle`
- `cleanup()`
- optional `_before_execute()` (file sync hook for remote backends)
- class attr `_stdin_mode = "heredoc"` for SDK/REST backends (no real pipe)

For non-subprocess backends there is a ready-made adapter,
`base._ThreadedProcessHandle(exec_fn, cancel_fn)`, that wraps a blocking
`exec_fn() -> (output_str, exit_code)` in a thread and exposes the
`ProcessHandle` protocol (`poll`/`kill`/`wait`/`stdout`/`returncode`).
`cancel_fn` is invoked on interrupt.

**Daytona is the ideal template** (`tools/environments/daytona.py`, 270 lines)
because it is a remote provider with: persistent filesystem via stop/resume,
`_ThreadedProcessHandle` exec, `cancel_fn -> stop()`, and `FileSyncManager` for
syncing `~/.hermes` into the sandbox. bhatti matches all of these.

## 3. Why bhatti is a near-perfect fit

| Hermes need | Daytona SDK | bhatti REST |
|-------------|-------------|-------------|
| create env | `daytona.create(params)` | `POST /sandboxes` `{name,cpus,memory_mb,env,new_volumes}` |
| run command | `sandbox.process.exec(cmd, timeout)` → `{result, exit_code}` | `POST /sandboxes/:id/exec` `{"cmd":[...]}` → `{exit_code,stdout,stderr}` |
| interrupt | `sandbox.stop()` | `POST /sandboxes/:id/stop` |
| persistent FS | `sandbox.stop()` / `.start()` | `POST /sandboxes/:id/stop` (snapshot) / `/start` (resume) |
| ephemeral | `daytona.delete()` | `DELETE /sandboxes/:id` |
| upload file | `sandbox.fs.upload_file()` | `PUT /sandboxes/:id/files?path=` |
| download file | `sandbox.fs.download_file()` | `GET /sandboxes/:id/files?path=` |
| list/stat | n/a | `GET ...&ls=true` / `HEAD ...` |
| resume sandbox by name | `daytona.get(name)` | `GET /sandboxes` then match `name == hermes-<task_id>` |

bhatti's stop=snapshot / start=resume is exactly Daytona's `_persistent` model —
microsecond resume is a bonus (paused sandbox serves an HTTP request in <4ms).

**Key difference:** bhatti has no Python SDK; it's a Go binary + REST daemon.
The backend talks REST directly with a ~80-line `httpx` (or stdlib `urllib`)
client embedded in the backend module. No new heavy dependency — prefer stdlib
`urllib`/`http.client` so there's nothing to add to lazy-deps, or a thin
`httpx` pin if we want streaming exec.

## 4. File-by-file work breakdown

Mirrors the merged backend pattern (see microsandbox #14993 for the minimal set,
FastVM #23466 for the full surface). Recommend the **lean set first**, then a
follow-up for the long-tail surfaces.

### Tier 1 — minimum viable backend (~400–550 LOC, the PR core)

| File | Change | Est. |
|------|--------|------|
| `tools/environments/bhatti.py` | **New.** `BhattiEnvironment(BaseEnvironment)` + tiny REST client. Model on `daytona.py`. | ~230 LOC |
| `tools/environments/__init__.py` | Add docstring mention (no import — backends are lazy). | 1 line |
| `tools/terminal_tool.py` | `_create_environment`: add `elif env_type == "bhatti"`. `_check_requirements`: bhatti branch (server reachable + token). `_get_env_config`: add `bhatti_image`/url defaults + include `bhatti` in the container-cwd-sanitize set and in the unknown-backend error strings (4 spots: ~570, ~1266, ~1831, ~2376, ~2470). | ~60 LOC |
| `hermes_cli/config.py` | Add `bhatti_*` defaults (~892), the `_config_to_env_sync` map entries (~5760), and a `hermes status` branch (~5561). | ~25 LOC |
| `gateway/run.py` | Add `bhatti_*` keys to `_terminal_env_map` (~907) so gateway sessions bridge config→env. | ~5 LOC |
| `cli-config.yaml.example` | Commented `terminal.backend: "bhatti"` block (url, token env, image, resources). | ~20 lines |
| `tests/integration/test_bhatti_terminal.py` | **New.** Mock the REST client; cover launch/exec/cwd-persist/interrupt/cleanup/persistent-resume/requirement-gating. Model on existing daytona tests. | ~250 LOC |

### Tier 2 — full surface parity (follow-up PR, optional)

| File | Change |
|------|--------|
| `pyproject.toml` + `tools/lazy_deps.py` | Only if we choose `httpx` over stdlib. `terminal.bhatti` group + extra. |
| `hermes doctor` (`hermes_cli/`) | bhatti reachability/token check (mirror SSH/daytona doctor checks; see #29509). |
| execute_code / file tools | Route through the backend (FastVM did this); usually inherited free via `BaseEnvironment`. Audit `vision_analyze` image path (#32709) and skills paths (#27491). |
| `website/docs/.../terminal-backends` | A bhatti backend doc page. |
| `.env.example` | `TERMINAL_BHATTI_URL`, `TERMINAL_BHATTI_TOKEN`, `TERMINAL_BHATTI_IMAGE`. |
| dashboard/setup wizard | Backend picker entry (`setup-hermes.sh`, web UI). |

## 5. Config surface (proposed)

```yaml
terminal:
  backend: "bhatti"
  bhatti_url: "https://your-server:8080"   # TERMINAL_BHATTI_URL
  bhatti_image: "browser"                   # tier/image; TERMINAL_BHATTI_IMAGE
  container_cpu: 2
  container_memory: 1024                    # MB (bhatti memory_mb)
  container_persistent: true                # stop(snapshot) on cleanup vs destroy
# secret via env: BHATTI_TOKEN=bht_...      (never written to .env by default)
```

Reuses the existing shared `container_cpu/memory/disk/persistent` keys (already
consumed by docker/singularity/modal/daytona) — only `bhatti_url`/`bhatti_image`
are new, plus the `BHATTI_TOKEN` secret read from env.

## 6. Effort estimate

- **Tier 1 (mergeable MVP):** ~1–1.5 focused days. ~450–550 LOC incl. tests.
  The hard parts are already solved by `BaseEnvironment` + `_ThreadedProcessHandle`
  + `FileSyncManager`; the bhatti REST surface is small and synchronous.
- **Tier 2 (parity + docs):** ~1 day, can land as a follow-up.

Risk is low: clean slate, strong template, well-matched API. Main review-bait
(per FastVM #23466 "changes requested"): keep the PR scoped, include real tests,
follow Conventional Commits, update `cli-config.yaml.example`, and don't add a
heavy dep (prefer stdlib HTTP).

## 7. Open questions to resolve before coding

1. **HTTP client:** stdlib `urllib` (zero new deps, blocking — fine via
   `_ThreadedProcessHandle`) vs `httpx` (streaming NDJSON exec, but new
   lazy-dep). Recommend stdlib for v1.
2. **Sandbox identity:** `name = f"hermes-{task_id}"`; resume by listing
   `GET /sandboxes` and matching name (bhatti has no get-by-name yet → or use
   the create-then-409 path). Confirm rename/uniqueness semantics.
3. **Home dir & file sync base:** bhatti rootfs uses `/root`; sync target
   `/root/.hermes` (matches `iter_sync_files` default). Confirm per-tier.
4. **Default image/tier:** `minimal` is too bare (no python/node). Default to
   `browser` (Node 22 + Chromium) or document that users pick a tier with the
   toolchain their agent needs.
5. **Cold-start timeout:** set `_snapshot_timeout` generously for first create;
   resume is sub-ms so steady-state is fast.
</content>
</invoke>
