# CLI Migration Plan — Cobra + Agent-Ready

## Why

The current CLI has a config bug where env vars silently override `bhatti setup`,
762 lines of hand-rolled dispatch that's hard to extend, and no structured output
for agents. Before the agent starts using the CLI, fix the foundation.

---

## Part 1 — Cobra Skeleton + Config Precedence Fix

The actual bug that caused the 401: env vars override config file. And the
structural problem: hand-rolled `switch/case` + `flag.FlagSet` makes adding
flags, help text, and completions painful.

### File Layout

All client-side CLI logic in a top-level `cli/` package. Server-side daemon
code stays in `cmd/bhatti/` (it pulls Linux-specific engine deps).

```
cli/
  root.go           — root command, persistent flags, config loading
  create.go         — create
  list.go           — list / ls
  destroy.go        — destroy / rm
  exec.go           — exec
  shell.go          — shell / sh
  ps.go             — ps
  file.go           — file read / write / ls
  secret.go         — secret set / list / delete
  user.go           — user create / list / delete / rotate-key
  setup.go          — setup (interactive config)
  version.go        — version
  completion.go     — completion bash / zsh / fish
  output.go         — JSON + table helpers
  timing.go         — HTTP trace transport
  cli_test.go       — integration tests

cmd/bhatti/
  main.go           — registers serve command, calls cli.Execute()
  engine_linux.go   — newFirecrackerEngine (Linux + KVM only)
  engine_other.go   — stub for macOS builds
  recovery.go       — recoverVMs (server startup)
  serve.go          — serve command (daemon startup, wired into cli.RootCmd)
```

`main.go` is ~10 lines — it registers the `serve` command (which needs
Linux engine imports) onto `cli.RootCmd`, then calls `cli.Execute()`.
The serve command lives in `cmd/bhatti/` because it imports the firecracker
engine and recovery logic that shouldn't be in the client-side `cli/` package.

Old `cli.go` (762 lines) goes away.

### Config Precedence

```
--flag  →  config file  →  env var  →  default
```

`bhatti setup` writes the config, it just works. Env vars are the fallback
for CI. A flag is the one-off escape hatch.

```go
func loadConfig(cmd *cobra.Command) {
    cfg, _ := pkg.LoadConfig()

    // URL: flag wins, then config, then env, then default
    if v, _ := cmd.Flags().GetString("url"); v != "" {
        apiURL = v
    } else if cfg.APIURL != "" {
        apiURL = cfg.APIURL
    } else if v := os.Getenv("BHATTI_URL"); v != "" {
        apiURL = v
    }
    // default is already "http://localhost:8080"

    // Token: same order
    if v, _ := cmd.Flags().GetString("token"); v != "" {
        apiToken = v
    } else if cfg.AuthToken != "" {
        apiToken = cfg.AuthToken
    } else if v := os.Getenv("BHATTI_TOKEN"); v != "" {
        apiToken = v
    }
}
```

### Persistent Flags

```go
rootCmd.PersistentFlags().String("url", "", "API endpoint (overrides config)")
rootCmd.PersistentFlags().String("token", "", "API key (overrides config)")
rootCmd.PersistentFlags().Bool("json", false, "Output as JSON")
rootCmd.PersistentFlags().Bool("timing", false, "Show request timing breakdown")
```

---

## Part 2 — `--json` Flag

Explicit flag, no magic. When you want JSON, you say `--json`.

### On read commands (list, ps, version, file ls)

```bash
bhatti list              # table for humans
bhatti list --json       # JSON for agents
```

Table output stays exactly as-is. JSON output returns the raw API response:

```json
[
  {"id": "c4ae2df238261a6f", "name": "dev", "status": "running", "ip": "10.0.1.2"}
]
```

### On mutations (create, destroy)

```bash
bhatti create --name dev --json
# → {"id":"c4ae2df238261a6f","name":"dev","status":"running","ip":"10.0.1.2",...}

bhatti destroy dev --json
# → {"status":"destroyed"}
```

### On exec

Exec is special — stdout/stderr go to their normal file descriptors (so piping
works). `--json` wraps the metadata:

```bash
bhatti exec dev --json -- npm test
# stdout: npm test output (goes to stdout as-is)
# stderr: npm test errors (goes to stderr as-is)
# After completion, prints to stdout:
# {"exit_code":0}
```

Actually, simpler: `--json` on exec returns the buffered API response as-is,
same as today's `apiJSON` call:

```json
{"exit_code": 0, "stdout": "...", "stderr": "..."}
```

No stdout/stderr splitting in JSON mode. The agent reads the JSON object.
Without `--json`, current behavior (stdout to stdout, stderr to stderr,
exit code forwarded).

### On errors

```bash
bhatti exec nonexistent -- echo hello
# table mode: "Error: 404 Not Found: not found" on stderr, exit 1
# --json mode: {"error": "not found"} on stdout, exit 1
```

### Implementation

```go
// output.go
func outputResult(cmd *cobra.Command, jsonData any, tableFn func()) {
    isJSON, _ := cmd.Flags().GetBool("json")
    if isJSON {
        enc := json.NewEncoder(os.Stdout)
        enc.SetIndent("", "  ")
        enc.Encode(jsonData)
    } else {
        tableFn()
    }
}
```

---

## Part 3 — `--timing` Flag

You asked for this specifically. Shows where time was spent in the request.
Goes to stderr so it never pollutes piped output.

### Output

```
$ bhatti exec dev --timing -- echo hello
hello
---
dns:       1ms
connect:   43ms
tls:       78ms
server:    3ms
transfer:  1ms
total:     126ms
```

Most of the time is TLS + connect (network to Hetzner). Server time is
the actual work. This tells you instantly whether latency is network or server.

For sandbox creation:

```
$ bhatti create --name test --timing
a1b2c3d4  test  10.0.1.4
---
dns:       1ms
connect:   44ms
tls:       79ms
server:    3412ms
transfer:  0ms
total:     3536ms
```

3.4s server = VM boot. 124ms network overhead. Clear.

### Combined with `--json`

When both are set, timing is a separate JSON object on stderr:

```
$ bhatti list --json --timing
[{"id":"abc","name":"dev",...}]              ← stdout
{"dns_ms":1,"connect_ms":43,...}             ← stderr
```

Keeps stdout clean for the agent to parse. Timing on stderr for the human
watching.

### Implementation

Use `net/http/httptrace` to hook into the Go HTTP client:

```go
// timing.go
type requestTiming struct {
    dnsStart, dnsDone       time.Time
    connectStart, connectDone time.Time
    tlsStart, tlsDone       time.Time
    gotFirstByte            time.Time
    start, end              time.Time
}

func (t *requestTiming) trace() *httptrace.ClientTrace {
    return &httptrace.ClientTrace{
        DNSStart:              func(_ httptrace.DNSStartInfo) { t.dnsStart = time.Now() },
        DNSDone:               func(_ httptrace.DNSDoneInfo) { t.dnsDone = time.Now() },
        ConnectStart:          func(_, _ string) { t.connectStart = time.Now() },
        ConnectDone:           func(_, _ string, _ error) { t.connectDone = time.Now() },
        TLSHandshakeStart:    func() { t.tlsStart = time.Now() },
        TLSHandshakeDone:     func(_ tls.ConnectionState, _ error) { t.tlsDone = time.Now() },
        GotFirstResponseByte: func() { t.gotFirstByte = time.Now() },
    }
}

func (t *requestTiming) print() {
    fmt.Fprintf(os.Stderr, "---\n")
    fmt.Fprintf(os.Stderr, "dns:       %s\n", t.dnsDone.Sub(t.dnsStart).Round(time.Millisecond))
    fmt.Fprintf(os.Stderr, "connect:   %s\n", t.connectDone.Sub(t.connectStart).Round(time.Millisecond))
    fmt.Fprintf(os.Stderr, "tls:       %s\n", t.tlsDone.Sub(t.tlsStart).Round(time.Millisecond))
    fmt.Fprintf(os.Stderr, "server:    %s\n", t.gotFirstByte.Sub(t.tlsDone).Round(time.Millisecond))
    fmt.Fprintf(os.Stderr, "transfer:  %s\n", t.end.Sub(t.gotFirstByte).Round(time.Millisecond))
    fmt.Fprintf(os.Stderr, "total:     %s\n", t.end.Sub(t.start).Round(time.Millisecond))
}
```

Thread it through `apiRequest` — if `--timing` is set, attach the trace
context and print after the response.

---

## Part 4 — Shell Completions

### Static completions (subcommands + flags)

Cobra gives this for free:

```bash
bhatti completion zsh > "${fpath[1]}/_bhatti"
bhatti completion bash > /etc/bash_completion.d/bhatti
bhatti completion fish > ~/.config/fish/completions/bhatti.fish
```

This completes `bhatti cr<tab>` → `create`, `bhatti create --cp<tab>` → `--cpus`,
etc. Zero network calls.

### Dynamic sandbox name completion — local cache, never blocks

The problem you raised: if the server is in Germany, hitting the API on every
tab keypress adds 100-200ms+ latency. That's unacceptable.

Solution: **opportunistic local cache**. Every time `bhatti list` runs
successfully, it writes sandbox names to a temp file. Completions read that
file — instant, offline, never blocks.

```go
// On every successful `bhatti list`:
func cacheSandboxNames(names []string) {
    path := filepath.Join(os.TempDir(), fmt.Sprintf("bhatti-completions-%d", os.Getuid()))
    os.WriteFile(path, []byte(strings.Join(names, "\n")), 0600)
}

// Completion function reads the cache, never hits network:
func completeSandboxNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
    path := filepath.Join(os.TempDir(), fmt.Sprintf("bhatti-completions-%d", os.Getuid()))
    data, err := os.ReadFile(path)
    if err != nil {
        return nil, cobra.ShellCompDirectiveNoFileComp
    }
    names := strings.Split(strings.TrimSpace(string(data)), "\n")
    return names, cobra.ShellCompDirectiveNoFileComp
}
```

If you haven't run `list` yet, no completions — that's fine. After one `list`,
tab works instantly. Cache is per-user, in `/tmp`, stale is better than slow.

Register on every command that takes a sandbox name:

```go
destroyCmd.ValidArgsFunction = completeSandboxNames
execCmd.ValidArgsFunction = completeSandboxNames
shellCmd.ValidArgsFunction = completeSandboxNames
psCmd.ValidArgsFunction = completeSandboxNames
```

---

## Part 5 — Exec `--timeout`

One flag, maps to the existing `timeout_sec` API field:

```bash
bhatti exec dev --timeout 30 -- npm test    # 30s timeout
bhatti exec dev -- echo hello               # default 300s
```

Agent should always set this. A hung process shouldn't block the agent forever.

---

## Migration Order

Each step is one PR. Tests pass after each.

### PR 1: Cobra skeleton + config fix
- `go get github.com/spf13/cobra`
- Create `root.go`, move all commands to individual files
- Fix config precedence (flag → config → env → default)
- Delete `cli.go`
- All existing behavior unchanged
- All existing tests pass

### PR 2: `--json` flag
- Add `--json` persistent flag
- Add `output.go` with JSON/table helpers
- Apply to: list, create, destroy, exec, ps, file ls, secret list, version

### PR 3: `--timing` flag
- Add `timing.go` with httptrace transport
- Add `--timing` persistent flag
- Output to stderr

### PR 4: Completions
- Add `cmd_completion.go`
- Add local cache written on `list`
- Register `ValidArgsFunction` on sandbox commands

### PR 5: Exec `--timeout`
- One flag, one line in the API request

---

## Not Now

- **TTY auto-detection for output format** — explicit `--json` is clearer.
- **Structured exit codes** — exit 0/1 is fine. Agent reads the error JSON.
- **Idempotent mutations** (`--if-not-exists`) — good for later, not blocking.
- **`bhatti run` compound command** — nice shortcut, build it when the agent
  actually needs it. Might not — the agent might prefer explicit create + exec
  for visibility.
- **Multi-profile support** — design the config to allow it later, don't build now.
- **Background exec / session logs** — needs protocol changes, separate effort.
