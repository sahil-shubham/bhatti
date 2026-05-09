> [!WARNING]
> **DEPRECATED — do not edit.**
> The canonical, maintained version of this page is at
> <https://bhatti.sh/docs/contributing/testing/>.
> This file is kept only for git history and may be removed in a future
> cleanup. See [`docs/README.md`](./README.md) for the redirect index.

---

# Testing

~11,000 lines of tests across 25 test files. Zero mocks for VM tests — all Firecracker integration tests run on real microVMs.

## Philosophy

**Test at the real boundary.** Early in development, the server tests used a mock engine that silently accepted any input. Tests passed even when volumes, exec, and state transitions were broken. The mock was replaced with real Docker integration tests, and later with real Firecracker VMs. The mock now only exists in `proxy_test.go` where it's at the right abstraction level (testing TCP relay logic, not VM behavior).

**Test the protocol without VMs.** The guest agent (lohar) has a test mode that listens on Unix sockets instead of vsock/TCP. The entire protocol handler test suite — 40+ tests covering exec, TTY sessions, file operations, scrollback, idle timers — runs on macOS in under 2 seconds without root or KVM.

**Test performance with percentiles.** Performance tests report p50/p95/p99, not averages. A p50 of 1ms with a p99 of 50ms is a very different system than a p50 of 1ms with a p99 of 2ms. The tests assert on p99 to catch regressions.

## Test Categories

### Protocol Tests (`pkg/agent/proto/`)

Pure Go, no dependencies, runs everywhere. Tests the binary framing layer:

- Round-trip: write frame → read frame → verify type + payload
- EOF handling: empty reader → `io.EOF`, partial header → `io.ErrUnexpectedEOF`
- Max frame size: reject frames > 1MB on both write and read
- `TryParse`: partial buffer → `ok=false`, complete frame → correct offsets
- `SendJSON`: encode → frame → decode → verify fields
- Resize/Exit payloads: encode/decode edge cases (0, max uint16, negative exit codes)
- Concurrent writes: 2 goroutines × 1000 frames → read all 2000 back, none corrupt

### Agent Tests (`cmd/lohar/`)

Start lohar as a subprocess in test mode, connect via Unix socket, exercise every handler. Runs on macOS, no VM needed.

**Exec:**
- Basic exec, exit codes, command not found
- Stderr separation, env vars, working directory
- Large output (1MB), kill during exec
- Process group kill (child processes die with parent)

**TTY Sessions:**
- Create session, I/O relay, resize, exit
- Detach/reattach with scrollback replay
- Session listing, session kill
- Idle timer (session killed after timeout)
- Init session (well-known ID "init")

**Files:**
- Read: normal, empty, binary data, unicode filenames
- Read truncation: offset, limit, max_bytes, combinations
- Write: normal, zero-byte, permissions, atomic (concurrent readers)
- Stat: regular file, directory, missing file
- List: normal, empty dir, non-directory, large dir (10k+ entries capped)
- Error cases: read directory, write negative size, stat nonexistent

**Sessions:**
- Ring buffer: write, wrap, ordered bytes
- Scrollback replay on reattach
- Multiple sessions, concurrent access

### Client Tests (`pkg/agent/`)

Test the host-side client against lohar in test mode. Validates the full stack: client → Unix socket → agent → child process → frames → client.

### Engine Integration Tests (`pkg/engine/firecracker/`)

Run on real Firecracker VMs. Require Linux with KVM and root access.

**Lifecycle:**
- Create, exec, shell, destroy
- Snapshot/resume (data persists across stop/start)
- Diff snapshots (full → diff → diff, data accumulation verified)
- Template-free creation (direct CPU/memory/env/init/volumes)

**Thermal:**
- Pause/resume (hot ↔ warm)
- EnsureHot from warm and cold states
- Activity tracking

**Networking:**
- Bridge setup (idempotent)
- Cross-VM communication (two VMs ping each other)
- IP allocation/release/reuse
- TAP cleanup after destroy
- Network survives snapshot/restore

**Files:**
- All file operations through the engine layer
- Server-side truncation through the engine layer

**Sessions:**
- Session list, attach, kill through the engine layer

**Performance (percentile-based):**
- Exec latency: 100 sequential `true` commands
- File I/O: 50 cycles of 1KB write + read
- Parallel reads: 5 concurrent 1KB file reads
- Warm→exec: resume from warm + exec (5 cycles)
- Diff vs full snapshot: timing and size comparison
- Streaming exec: time-to-first-byte and total latency
- Concurrent exec: 10 simultaneous commands
- Truncated read: full vs truncated on 10K-line file

**Proxy:**
- HTTP GET through reverse proxy
- 404 handling
- Header forwarding
- Invalid port, sandbox not found

### Server Tests (`pkg/server/`)

Test the HTTP layer against real Docker (macOS) or real Firecracker (Linux).

- Sandbox CRUD lifecycle
- Exec (buffered and streaming)
- WebSocket auth (query param, bearer header, rejection)
- Proxy routing
- Template CRUD
- Secret CRUD
- Volume CRUD

### Daemon Recovery Tests (`cmd/bhatti/`)

Test `recoverVMs()` without any actual VMs. Use a mock `VMStateProvider` to verify recovery logic:

- Stopped sandbox with snapshot → restored as stopped
- Stopped sandbox without snapshot → marked unknown
- Running sandbox with snapshot → restored as stopped
- Running sandbox without snapshot → marked unknown
- Destroyed sandbox → skipped
- Docker sandbox (no FC state) → skipped
- Type coercion: `float64` from JSON vs `int` from SQLite
- Multiple sandboxes recovered in one pass

### CLI Tests (`cmd/bhatti/`)

Integration tests against a running Firecracker daemon:

- Create, list, exec, destroy
- Name-to-ID resolution
- File write/read/ls
- Session listing
- Secret set/list/delete

### Benchmarks

```bash
go test -bench=. ./pkg/agent/proto/
go test -bench=. ./cmd/lohar/
```

- Frame write throughput
- Frame read throughput
- Ring buffer write throughput
- JSON encoding for ExecRequest

## Running Tests

```bash
# Protocol + agent tests (macOS, no root, no VM)
go test -v -timeout=120s ./pkg/agent/proto/ ./cmd/lohar/ ./pkg/agent/

# Server tests (requires Docker on macOS, or Firecracker on Linux)
go test -v -timeout=120s ./pkg/server/

# Store tests
go test -v -timeout=30s ./pkg/store/

# Recovery + CLI tests
go test -v -timeout=30s ./cmd/bhatti/

# Firecracker integration tests (Linux, root, KVM required)
sudo go test -v -timeout=600s ./pkg/engine/firecracker/

# Cross-compile tests for remote Pi
GOOS=linux GOARCH=arm64 go test -c -o bin/fc-test ./pkg/engine/firecracker
scp bin/fc-test pi:/tmp/ && ssh pi "sudo /tmp/fc-test -test.v"

# Everything
sudo go test -v -timeout=600s ./...
```

## Test Flakiness Notes

**`TestNetworkSurvivesSnapshot`** was originally flaky because it tested guest→host TCP after snapshot/restore. After restore, the host's iptables conntrack has stale entries — guest-initiated SYN packets get stuck in kernel retransmit backoff for 30+ seconds. The test was rewritten to verify only host→guest connectivity (which is what matters for the agent protocol) and passes reliably.

**Performance tests** use 5 warm→exec cycles instead of 100 to avoid hitting Firecracker's Unix socket connection limit under rapid pause/resume.
