> [!WARNING]
> **DEPRECATED — do not edit.**
> The canonical, maintained version of this page is at
> <https://bhatti.sh/docs/under-the-hood/decisions/>.
> This file is kept only for git history and may be removed in a future
> cleanup. See [`docs/README.md`](./README.md) for the redirect index.

---

# Design Decisions

Key architectural decisions with context, alternatives considered, and rationale. These are the decisions that shaped the system — the ones worth discussing in detail because they have non-obvious tradeoffs.

---

## 1. TCP over TAP instead of vsock after snapshot/restore

**Context:** Firecracker exposes vsock (virtio socket) as the primary host↔guest communication channel. It works perfectly during normal operation. After snapshot/restore, it breaks — the guest kernel's vsock state is stale, and connections complete the host-side handshake but never reach the guest agent.

**Discovery:** This was found during Part 5 (the first Firecracker engine implementation). Exec worked fine on a fresh VM but silently hung after restore. Debugging showed the `CONNECT/OK` handshake succeeded (Firecracker's proxy handled it) but the agent never received the connection. Tested with kernel 5.10 and 6.1, Firecracker 1.6.0. Other Firecracker orchestrators have encountered the same issue — vsock state after snapshot restore is a known limitation (see Firecracker issue tracker).

**Alternatives:**
- **Wait for Firecracker to fix it.** FC PR #5688 ("minimize local port collisions after snapshot restore") shipped in 1.15.0 but doesn't fully resolve the issue. Timeline unknown.
- **Restart the vsock listener inside the guest after restore.** Requires the host to signal the guest, but the host can't reach the guest (that's the problem).
- **Use TCP over virtio-net.** The virtual network card (virtio-net) survives snapshot/restore cleanly. The guest kernel's TCP stack works immediately.

**Decision:** Lohar listens on both vsock and TCP on the same ports (1024/1025). Cold boot uses vsock (slightly faster initial connection). After restore, a new `AgentClient` is created that uses TCP. The agent doesn't need to know — it accepts connections from either transport.

**Tradeoff:** TCP over TAP adds ~0.1ms latency compared to vsock. Negligible for the use case.

---

## 2. No Firecracker Go SDK

**Context:** The official `firecracker-go-sdk` provides a Go client for Firecracker's HTTP API. It's ~15,000 lines of generated code (Swagger models, client helpers, option types).

**Decision:** Talk directly to Firecracker's Unix socket HTTP API with ~20 lines of helpers:

```go
func fcPut(client *http.Client, path, body string) error {
    req, _ := http.NewRequest("PUT", "http://localhost"+path, strings.NewReader(body))
    req.Header.Set("Content-Type", "application/json")
    resp, err := client.Do(req)
    // check status, done
}
```

**Why:** The SDK abstracts away what's actually happening (HTTP PUTs to a Unix socket) behind layers of generated types. For debugging, you need to understand the raw API anyway. The SDK also pulls in heavy dependencies (Swagger runtime, go-openapi) that bloat the binary and complicate cross-compilation. And it doesn't help with the hard parts — snapshot/restore sequencing, TAP device management, network configuration, thermal state machines.

**Tradeoff:** We manually construct JSON strings for API calls (e.g., `fmt.Sprintf('{"vcpu_count":%d,"mem_size_mib":%d}', ...)`). This is less type-safe than the SDK's generated structs. In practice, the Firecracker API surface we use is small (~8 endpoints) and stable. Any typo shows up immediately in integration tests.

---

## 3. Lohar as PID 1 (no systemd)

**Context:** The rootfs is Ubuntu 24.04, which ships with systemd. The conventional approach is to let systemd start as PID 1 and run the agent as a systemd service.

**Decision:** Lohar *is* PID 1. The kernel boots directly into it via `init=/usr/local/bin/lohar`. No systemd, no initramfs, no init scripts.

**Why:** Determinism. Systemd's boot sequence is complex — it starts dozens of services, has dependency ordering, generates machine IDs, manages cgroups, handles device hotplug. All of this is unnecessary inside a microVM where we control the entire environment. Systemd adds 1-2 seconds to boot time and introduces failure modes we don't need.

With lohar as PID 1:
- Boot to agent-ready: ~3.5 seconds (kernel + mount + network + listen)
- Zero services to manage or debug
- Zero races between service startup and agent readiness
- Smaller rootfs (systemd and its dependencies are dead weight but still present — removing them from the base Ubuntu would break `apt`)

**Tradeoff:** Lohar must handle everything PID 1 is responsible for: mounting filesystems, signal handling, and (in theory) reaping orphan zombies. The zombie reaping is intentionally omitted — Go's runtime manages `SIGCHLD` for `exec.Command` processes, and a manual `Wait4(-1)` reaper would race with `cmd.Wait()`. Orphan zombies are acceptable because the VM is short-lived.

---

## 4. Exec is sessions

**Context:** Most sandbox systems have two separate concepts: "exec" (run a command, get output) and "shell" (interactive terminal). They're different code paths with different APIs.

**Decision:** Every TTY exec is a session. Sessions have IDs, scrollback buffers, and survive host disconnects. There's no separate "shell" concept — a shell is just a TTY exec of `/bin/zsh`. Non-TTY exec (piped) is the only path that doesn't create a session.

**Why:** This fell out of a real problem. During development, SSH connections to the Pi would drop (Wi-Fi, laptop sleep). A running `npm install` inside a shell would get killed (SIGHUP from PTY close). With sessions:

1. The host disconnects → session detaches (no SIGHUP)
2. The process keeps running, output goes to the 64KB scrollback ring buffer
3. The host reconnects → scrollback is replayed, live I/O resumes

The init script from the config drive is also a session (ID: `"init"`), so the host can attach to it and watch progress.

**Tradeoff:** Every TTY session allocates a 64KB ring buffer. With 100 concurrent sessions per VM, that's 6.4MB. Acceptable for the use case.

---

## 5. Atomic file writes (temp + fsync + rename)

**Context:** The host writes files into the VM via the agent. Multiple operations can be concurrent (e.g., an AI agent sending 5 parallel file writes). Readers should never see partial content.

**Decision:** Lohar writes to a temp file (`path.bhatti-tmp`), fsyncs it, then renames it over the target atomically:

```go
f := os.Create(tmpPath)
// ... write content ...
f.Sync()
f.Close()
os.Rename(tmpPath, path)
```

**Why:** `rename()` is atomic on POSIX filesystems. A concurrent reader sees either the old file or the new file, never a half-written state. Without this, a reader during a write could get truncated content, corrupted JSON, or empty files.

The `fsync()` before rename ensures data is on disk, not just in the page cache. This matters because the host might snapshot the VM immediately after the write completes — without fsync, the renamed file could exist (metadata committed) but contain zeroes (data still in page cache).

---

## 6. Server-side file truncation

**Context:** AI coding agents always truncate file reads — typically to 2000 lines or 50KB, whichever comes first. Without server-side truncation, a 100MB log file transfers 100MB through the wire protocol, then the agent throws away 99.95% of it.

**Decision:** Added `offset`, `limit`, and `max_bytes` parameters to `FILE_READ_REQ`. The guest agent reads line-by-line with a `bufio.Scanner`, applies the constraints, and stops reading when any limit is hit. The response includes the total file size so the consumer knows if content was truncated.

**Performance impact:** On a 10K-line file, truncated reads (limit=100) are 4.5x faster at p50 than full reads. For larger files, the improvement is proportionally greater.

**Why not just `head -n` via exec?** That works but has overhead: fork/exec of `head`, pipe setup, shell argument parsing. The file protocol path is a single connection with no process spawning — ~472µs for a 1KB read vs ~1ms for an exec.

---

## 7. Content-negotiated streaming exec

**Context:** The exec endpoint needs to support two modes: buffered (for simple API consumers) and streaming (for UIs and agent frameworks that want real-time output).

**Decision:** Content negotiation on `Accept: application/x-ndjson`. Without the header, the existing buffered JSON response is returned. With it, output streams as newline-delimited JSON events, each flushed immediately.

**Alternatives:**
- **WebSocket for streaming.** More complex client-side (upgrade handshake, frame handling). NDJSON over HTTP works with `curl -N` and any HTTP client that supports streaming bodies.
- **Server-Sent Events (SSE).** Similar to NDJSON but with the `text/event-stream` content type and `data: ` prefixes. SSE is designed for unidirectional server→client, which fits exec output. NDJSON was chosen for simplicity — no event type prefixes, no reconnection protocol, just JSON per line.
- **Separate endpoint** (`/exec-stream`). Duplicates routing logic. Content negotiation keeps one endpoint for both modes.

**Tradeoff:** The Firecracker engine implements `StreamExecEngine` natively (reads agent frames, emits events). The Docker engine falls back to buffering then emitting as NDJSON events — you get all the output at once, just formatted as NDJSON. This is semantically correct but doesn't provide real-time streaming for Docker. Since Docker is only used for macOS development, this is acceptable.

---

## 8. Per-VM mutex with capture-and-release

**Context:** Multiple goroutines access the same VM concurrently: the thermal manager checks state, the API serves exec requests, the port scanner queries listening ports. A global lock would serialize everything.

**Decision:** Each VM has its own `stateMu sync.Mutex`. The engine-level `sync.RWMutex` protects only the VM map. Operations follow a capture-and-release pattern:

```go
vm.stateMu.Lock()
if vm.Thermal != "hot" { vm.stateMu.Unlock(); return error }
ag := vm.Agent        // capture the reference
vm.stateMu.Unlock()   // release before the slow call

return ag.Exec(ctx, cmd)  // safe: Agent pointer doesn't change until Start()
```

**Why:** A Shell or Tunnel call can last hours. Holding the VM lock for the duration would block the thermal manager from pausing *any* VM, block exec on the same VM, and block status queries. Capture-and-release lets long-lived operations proceed without holding any lock.

The `Agent` pointer is safe to use after release because it's only replaced during `Start()` (snapshot restore), which holds `stateMu`. If `Start()` is called while a Shell is active, the Shell continues using the old Agent reference — the old connection is still valid until the Firecracker process is killed, which only happens during `Stop()` or `Destroy()`.

---

## 9. Host-side activity cache for thermal management

**Context:** The thermal manager ticks every 10 seconds and needs to know which sandboxes are idle. The authoritative source is the guest agent (it tracks the last activity timestamp). But querying the agent means opening a TCP connection per sandbox per cycle.

**Decision:** The server maintains a `sync.Map` of `engineID → time.Time`. `ensureHot()` — which is called on every API request — updates this timestamp. The thermal cycle checks the cache first. If a sandbox had API activity within the warm timeout, the agent query is skipped.

**Why:** With 50 sandboxes, the naive approach opens 50 TCP connections every 10 seconds — most returning "yes, still idle." The cache eliminates these for active sandboxes. Only truly idle sandboxes (no API activity within 30s) get queried.

**Tradeoff:** The cache is a heuristic. If a sandbox is doing work internally (e.g., a background process writing files) without any API activity, the cache says "idle" and the agent query catches it. This is correct behavior — the slow path handles it. The cache only skips queries when it can prove the sandbox is active, never when it might be idle.

---

## 10. Pure-Go SQLite

**Context:** The store needs a lightweight embedded database. SQLite is the obvious choice, but the standard Go driver (`mattn/go-sqlite3`) requires CGO — a C compiler, dynamic linking, and platform-specific builds.

**Decision:** Use `modernc.org/sqlite` — a pure-Go translation of SQLite's C code. Zero CGO, cross-compiles from macOS to Linux ARM64 with `CGO_ENABLED=0`.

**Why:** The bhatti binary is cross-compiled on a Mac and deployed to a Pi. With CGO, this requires a cross-compiler toolchain (arm64 gcc), careful library management, and different build commands per platform. With pure-Go SQLite, `GOOS=linux GOARCH=arm64 go build` produces a static binary that just works.

**Tradeoff:** `modernc.org/sqlite` is ~10% slower than the C-based driver. For bhatti's workload (metadata CRUD, not analytics), this is irrelevant. The binary size increases by ~3MB (the translated C code is large). Worth it for the build simplicity.

---

## 11. Bridge networking with kernel ip=

**Context:** VMs need network access (internet + inter-VM). Two main approaches: per-VM NAT with iptables rules, or a shared bridge.

**Decision:** Shared bridge (`brbhatti0`) on `192.168.137.0/24` with a single masquerade rule for the subnet. Guest IP configured via kernel `ip=` command-line parameter.

**Why per-VM NAT was rejected:** Each VM would need its own iptables rules (DNAT for inbound, SNAT for outbound). With 50 VMs, that's 100+ iptables rules to manage, debug, and clean up on crash. A bridge with one masquerade rule is simpler and scales better.

**Why kernel `ip=`:** The guest network must be up before lohar starts, because the host polls lohar via TCP to detect readiness. If lohar configures networking, the host can't reach lohar to tell it what IP to use. The kernel `ip=` parameter is processed during early boot — the interface is configured before init runs. Zero DHCP, zero latency, zero failure modes.

The kernel `ip=` parameter is documented in `Documentation/admin-guide/kernel-parameters.txt` and is widely used in Firecracker deployments.
