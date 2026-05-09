> [!WARNING]
> **DEPRECATED — do not edit.**
> The canonical, maintained version of this page is at
> <https://bhatti.sh/docs/under-the-hood/lohar-the-blacksmith/>.
> This file is kept only for git history and may be removed in a future
> cleanup. See [`docs/README.md`](./README.md) for the redirect index.

---

# Guest Agent (Lohar)

Lohar is a single static Go binary that runs as PID 1 — the init process — inside every Firecracker microVM. It replaces systemd, handles all system initialization, and serves as the execution and file operations backend for the host.

No libc, no initramfs, no dynamic linking. Cross-compiled from macOS with `CGO_ENABLED=0`, it runs on any Linux kernel.

## Boot Sequence

The kernel boots with `init=/usr/local/bin/lohar` on the command line. Lohar is the first and only userspace process. Here's what it does:

```go
func main() {
    // 1. Mount essential filesystems
    //    The kernel provides a bare rootfs — no /proc, /dev, /sys, /tmp.
    //    Without /proc, tools like ps/ss/kill don't work.
    //    Without /dev/pts, PTY allocation fails.
    mount("proc",     "/proc",    "proc")
    mount("sysfs",    "/sys",     "sysfs")
    mount("devtmpfs", "/dev",     "devtmpfs")
    mount("devpts",   "/dev/pts", "devpts", "newinstance,ptmxmode=0666")
    mount("tmpfs",    "/tmp",     "tmpfs")
    mount("tmpfs",    "/run",     "tmpfs")

    // 2. Bring up loopback (lo)
    bringUpInterface("lo")    // raw ioctl: SIOCGIFFLAGS → set IFF_UP → SIOCSIFFLAGS

    // 3. Load config drive (/dev/vdb)
    //    1MB ext4 image with hostname, token, env vars, files, volumes, DNS, init script
    cfg := loadConfigDrive()
    applyHostname(cfg)
    applyDNS(cfg)
    writeConfigFiles(cfg)     // base64-decoded, chowned to uid 1000
    mountVolumes(cfg)         // ext4 images as /dev/vdc, /dev/vdd, ...

    // 4. Set up networking
    //    eth0 is already configured by kernel ip= cmdline parameter
    //    (e.g., ip=192.168.137.2::192.168.137.1:255.255.255.0::eth0:off:1.1.1.1:8.8.8.8:)
    //    Network is up before init runs. No DHCP, no chicken-and-egg.
    setupNetworking()

    // 5. Start listeners
    //    vsock: works on cold boot, broken after snapshot/restore
    //    TCP: works always (virtio-net survives snapshot/restore)
    listen(vsock, :1024, handleControlConnection)
    listen(vsock, :1025, handleForwardConnection)
    listen(tcp,   :1024, handleControlConnection)
    listen(tcp,   :1025, handleForwardConnection)

    // 6. Run init script (if configured) as attachable TTY session "init"
    if cfg.Init != "" {
        go runInitSession(cfg.Init)
    }

    // 7. Block forever. PID 1 must never exit.
    select {}
}
```

Boot to agent-ready takes ~3.5 seconds on a Pi 5. The host polls with `exec true` until it gets a response.

## Config Drive

A 1MB ext4 image attached as `/dev/vdb`, mounted read-only at `/run/bhatti/config`. Contains a single `config.json`:

```json
{
  "sandbox_id": "a1b2c3d4e5f6",
  "hostname": "dev",
  "token": "deadbeef...",
  "env": {"API_KEY": "sk-...", "NODE_ENV": "development"},
  "files": {
    "/workspace/.env": {"content": "base64...", "mode": "0600"}
  },
  "volumes": [
    {"device": "/dev/vdc", "mount": "/workspace", "fs": "ext4"}
  ],
  "init": "cd /workspace && npm install",
  "dns": ["1.1.1.1", "8.8.8.8"],
  "user": "lohar"
}
```

This is built on the host during `Create()` using `mkfs.ext4` + mount + write + umount. It's attached to Firecracker as a read-only virtio-blk drive before boot.

The config drive is how bhatti avoids the exec-after-boot pattern for configuration injection. Everything — hostname, environment variables, secrets, volumes, DNS, init scripts — is available before the agent starts listening. No race conditions, no retries.

## PTY Allocation

Lohar allocates PTYs using raw syscalls (no `creack/pty`, no cgo):

```go
func openPTY() (master, slave *os.File, err error) {
    // Open the PTY multiplexor
    master = os.OpenFile("/dev/ptmx", os.O_RDWR, 0)

    // Get the slave PTY number
    ioctl(master.Fd(), TIOCGPTN, &ptsNum)    // → e.g., 0

    // Unlock the slave
    ioctl(master.Fd(), TIOCSPTLCK, &zero)

    // Open the slave
    slave = os.OpenFile("/dev/pts/0", os.O_RDWR|O_NOCTTY, 0)

    return master, slave, nil
}
```

The child process is started with `Setsid: true` and `Setctty: true` to create a new session and make the slave PTY its controlling terminal. The master side is used by lohar for I/O relay and scrollback capture.

Window size is set via `TIOCSWINSZ` ioctl on the master — the host sends `RESIZE` frames when the terminal changes size, and lohar applies them immediately.

## Session Model

Every TTY exec creates a *session* — a persistent handle to a running process with scrollback.

```
┌──────────────┐     ┌──────────────┐     ┌──────────────┐
│ Host Client  │────►│   Session    │────►│  PTY Master  │────► child process
│ (attached)   │◄────│ s1           │◄────│              │◄────  /bin/zsh
└──────────────┘     │ scrollback:  │     └──────────────┘
                     │  64KB ring   │
                     │ idle timer   │
                     └──────────────┘
```

**Disconnect doesn't kill.** When the host client disconnects (network drop, `Ctrl+\`), the session detaches. The child process keeps running. The PTY master stays open. Output continues flowing into the 64KB scrollback ring buffer. An idle timer starts (if configured).

**Reattach replays scrollback.** When a client reconnects via `SessionAttach`, it receives:
1. A `SESSION_INFO` frame with the session's current state
2. The scrollback buffer contents as a `STDOUT` frame (up to 64KB of recent output)
3. Live I/O from that point forward

The previous client (if still connected) gets an `EXIT` frame and is disconnected.

**Init scripts are sessions.** The `init` field from the config drive runs as a TTY session with the well-known ID `"init"`. The host can attach to it to monitor progress: `bhatti ps dev` shows it, and it appears in `SessionList` responses.

### Ring Buffer

The scrollback buffer is a fixed-size ring buffer (64KB). Writes wrap around, overwriting the oldest data. This bounds memory usage per session regardless of how much output the child produces.

```go
type ringBuffer struct {
    buf  []byte
    size int
    w    int     // next write position
    full bool    // true once we've wrapped
}
```

`Bytes()` returns the contents in order — oldest first. If the buffer hasn't wrapped yet, it returns `buf[:w]`. If it has, it returns `buf[w:] + buf[:w]`.

## Piped Exec (Non-TTY)

For one-shot commands (`bhatti exec dev -- npm install`):

1. Create `exec.Command` with `Setpgid: true` (own process group)
2. Create stdin/stdout/stderr pipes
3. Start the child
4. Fan out: stdout → `STDOUT` frames, stderr → `STDERR` frames (via channel to serialize writes)
5. Fan in: `STDIN`/`KILL` frames from connection → child stdin / process group kill
6. Wait for stdout+stderr goroutines to drain, *then* `cmd.Wait()`
7. `syscall.Sync()` — flush filesystem writes before host might snapshot
8. Send `EXIT` frame

The ordering matters: we wait for I/O goroutines before `Wait()` to ensure all output is sent before the exit code. And `Sync()` before `EXIT` ensures that any files the command wrote are on the virtual disk, not just in the page cache — critical because the host might snapshot the VM immediately after receiving EXIT.

The stdout/stderr goroutines send through a channel to a single writer goroutine. This serializes frame writes to the connection and prevents interleaving.

## Process Group Kill

Piped exec runs children with `Setpgid: true`, which puts the child and all its descendants in a new process group. When the host sends `KILL` (or the connection drops for non-TTY exec), lohar sends `SIGKILL` to the negative PID:

```go
syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
```

This kills the entire process tree. Without it, `npm install` (which spawns node, which spawns dozens of child processes) would leave orphans running after the shell was killed.

TTY sessions use `SIGTERM` instead — allowing the shell to clean up and preserving the reattach model.

## Signal Handling

Lohar does *not* install a `SIGCHLD` handler. Go's runtime manages `SIGCHLD` for processes started via `exec.Command`. A manual `Wait4(-1)` reaper would race with `cmd.Wait()` and corrupt exit codes. Orphan zombies from grandchild processes are acceptable — they're cleaned up when the VM is destroyed.

`SIGTERM`/`SIGINT` triggers a clean shutdown: `syscall.Sync()` followed by `syscall.Reboot(LINUX_REBOOT_CMD_POWER_OFF)`.

## Environment Variables

Every exec inherits a merged environment:

```
defaults (PATH, TERM, HOME, LANG)
    ↓ overridden by
config drive env (secrets, API keys)
    ↓ overridden by
per-request env (from EXEC_REQ)
```

This means secrets from the config drive are available in every command without the host needing to pass them explicitly, but a per-request env can override anything.

## Testing Without VMs

Lohar has a test mode (`LOHAR_TEST=1`) that listens on Unix sockets instead of vsock/TCP. The agent test suite (40+ tests) starts lohar as a subprocess, connects via Unix socket, and exercises every protocol handler — exec, TTY, sessions, files, port forwarding. All tests run on macOS with `go test ./cmd/lohar/`, no VM or root required.
