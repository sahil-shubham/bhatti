> [!WARNING]
> **DEPRECATED — do not edit.**
> The canonical, maintained docs landing page is at
> <https://bhatti.sh/docs/>.
> This file is kept only for git history and may be removed in a future
> cleanup. See [`docs/README.md`](./README.md) for the redirect index.

---

# Bhatti

**Firecracker microVM orchestrator for AI coding agents.**

Bhatti gives every coding agent its own Linux VM — full kernel, full filesystem, full process isolation — with sub-millisecond pause/resume and transparent resource management. A paused VM resumes and executes a command in under 3ms.

```
bhatti create --name dev --cpus 2 --memory 1024
bhatti exec dev -- npm install           # runs inside an isolated VM
bhatti shell dev                          # interactive shell (Ctrl+\ to detach)
bhatti destroy dev
```

---

## Why This Exists

AI coding agents execute arbitrary code. They run `npm install`, modify filesystems, spawn long-running servers, and pipe commands together. They need a sandbox that feels like a real machine but can be created in seconds, paused for free, and resumed instantly.

The existing options:

- **Containers** (Docker) — shared kernel, weak isolation, no true pause/resume (SIGSTOP is not memory snapshots). Process state is lost on restart.
- **Cloud VMs** (EC2, GCE) — real isolation but 30-60 second boot, $0.01+/hr minimum, no memory snapshots.
- **Serverless sandboxes** (E2B, Modal) — purpose-built but opaque, no self-hosting, per-minute billing, vendor lock-in.
- **Fly Machines / Sprites** — closest in spirit, but filesystem-only persistence (processes die on hibernate), fixed resource tiers, no self-hosting.

Bhatti is the self-hosted answer: real VMs on commodity hardware (a Raspberry Pi 5 or a Hetzner bare-metal box), with memory snapshots that preserve running processes across pause/resume, and a three-tier thermal system that automatically manages resources without the agent knowing.

## How It's Different

**Memory snapshots, not just filesystem persistence.** When Bhatti snapshots a VM, it captures everything: running processes, open file descriptors, TCP connections, in-memory state. Resume picks up exactly where it left off. An `npm install` running when the VM was paused continues running after resume.

**Three-tier thermal management, invisible to the consumer.** VMs transition automatically between hot (running, ~400µs resume), warm (vCPUs paused, memory allocated, ~400µs resume), and cold (snapshotted to disk, memory freed, ~50ms resume). The API layer transparently wakes VMs on any request. From the outside, every sandbox is always "running."

**No SDK, no runtime dependency.** The Firecracker engine talks directly to Firecracker's HTTP API over a Unix socket — ~20 lines of helpers replace thousands of SDK lines. The guest agent (lohar) runs as PID 1 with zero dependencies — no systemd, no initramfs, no libc. The entire system cross-compiles from a Mac with `CGO_ENABLED=0`.

**Built for the agent workload.** Server-side file truncation (agents always truncate to 2000 lines/50KB — doing it guest-side avoids transferring megabytes). Streaming exec via NDJSON. Process group kill for reliable abort. `ripgrep` and `fd` pre-installed. Parallel file operations. Every design choice is informed by how coding agents actually use sandboxes.

## Performance

Measured on Raspberry Pi 5 (ARM64, NVMe):

```
                                p50       p95       p99
Exec `true`:                    1.0ms     1.2ms     1.3ms
1KB file write:                 809µs     1.1ms     1.4ms
1KB file read:                  472µs     826µs     881µs
5 parallel file reads:          1.9ms     2.3ms     2.3ms
Warm→exec (resume + exec):     2.5ms     2.6ms     2.6ms
10 concurrent execs:            18ms      19ms      19ms

VM boot (create → ready):      ~3.5s
Full snapshot (512MB):          ~4.4s
Diff snapshot (idle VM):        ~52ms
Restore from snapshot:          ~50ms
Pause/Resume (vCPU only):      ~400µs
```

## The Name

**Bhatti** (भट्टी) is Hindi for *furnace* — the system that manages fire, provides the environment where work happens.

**Lohar** (लोहार) means *blacksmith* — the one who works inside the bhatti. The guest agent that runs as PID 1 inside every microVM.

## Documentation

| Document | What's in it |
|----------|-------------|
| [Quickstart](quickstart.md) | Install, create a sandbox, run commands, tear down — 2 minutes |
| [Architecture](architecture.md) | System design, component map, lifecycle, concurrency model |
| [Wire Protocol](wire-protocol.md) | Binary framing, frame types, connection lifecycle, auth |
| [Guest Agent](guest-agent.md) | PID 1 init, PTY allocation, sessions, process management |
| [Thermal Management](thermal-management.md) | Hot/warm/cold states, diff snapshots, activity caching |
| [Networking](networking.md) | Bridge, TAP devices, IP pool, kernel-level config, post-snapshot TCP |
| [API Reference](api-reference.md) | REST/WebSocket endpoints, request/response formats |
| [CLI Reference](cli-reference.md) | All commands with examples |
| [Testing](testing.md) | Test philosophy, categories, how to run, what's covered |
| [Design Decisions](decisions.md) | Key decisions with context, alternatives, and rationale |

## Project Stats

- **~8,000 lines** of Go (host daemon + guest agent + CLI)
- **~11,000 lines** of tests across 25 test files
- **Zero mocks** for VM tests — all integration tests run on real Firecracker VMs
- **Single binary** — `bhatti serve` starts the daemon, everything else is CLI
- **Two binaries total** — `bhatti` (host) and `lohar` (guest agent, baked into rootfs)
- **5 external dependencies** — Docker client, gorilla/websocket, x/crypto, yaml, pure-Go SQLite

## Requirements

- Linux (aarch64 or x86_64) with KVM (`/dev/kvm`)
- Root access (for Firecracker, TAP devices, bridge networking)
- Tested on: Raspberry Pi 5 (Ubuntu, NVMe), AWS Graviton bare metal, Hetzner Ryzen 9

## License

[Apache 2.0](../LICENSE).
