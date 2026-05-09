> [!WARNING]
> **DEPRECATED — do not edit.**
> The canonical, maintained version of this page is at
> <https://bhatti.sh/docs/under-the-hood/networking/>.
> This file is kept only for git history and may be removed in a future
> cleanup. See [`docs/README.md`](./README.md) for the redirect index.

---

# Networking

Every VM gets its own network interface, IP address, and internet access through a shared bridge on the host. The network is configured at the kernel level before init runs — no DHCP, no NetworkManager, no race conditions.

## Bridge Architecture

```
                    ┌─────────────────────────────────┐
                    │  Host                            │
Internet ◄──NAT────┤                                  │
                    │  eth0 (or default interface)     │
                    │       │                          │
                    │  iptables MASQUERADE             │
                    │       │                          │
                    │  brbhatti0 (bridge)              │
                    │  192.168.137.1/24                │
                    │       │                          │
                    │  ┌────┴────┬────────┐           │
                    │  │         │        │           │
                    │  tap0001   tap0002  tap0003     │
                    │  │         │        │           │
                    └──┼─────────┼────────┼───────────┘
                       │         │        │
                    ┌──┴──┐  ┌──┴──┐  ┌──┴──┐
                    │ VM1 │  │ VM2 │  │ VM3 │
                    │.137.2│  │.137.3│  │.137.4│
                    └─────┘  └─────┘  └─────┘
```

All VMs share one bridge (`brbhatti0`) and one masquerade rule. VMs can reach the internet and each other.

## IP Pool

The pool manages 253 addresses in the `192.168.137.0/24` subnet:

- `.0` — network address (reserved)
- `.1` — bridge IP (reserved)
- `.2` through `.254` — available for VMs
- `.255` — broadcast (reserved)

The pool is a simple `[256]bool` array protected by a mutex. `Allocate()` scans linearly for the first free slot. `Release()` marks it free. `Mark()` reserves an IP during recovery (so restored VMs don't get their IP re-allocated).

This limits a single host to 253 concurrent VMs. For the target deployment (a Pi 5 or single bare-metal box), this is more than sufficient — memory is the real bottleneck.

## TAP Devices

Each VM gets a dedicated TAP device:

```go
func createTapDevice(sandboxID string) (string, error) {
    tapName := "tap" + sandboxID[:8]
    run("ip", "tuntap", "add", tapName, "mode", "tap")
    run("ip", "link", "set", tapName, "master", bridgeName)
    run("ip", "link", "set", tapName, "up")
    return tapName, nil
}
```

The TAP is attached to the bridge, not given its own IP. Firecracker binds the VM's virtio-net device to this TAP. From the guest's perspective, it has an ethernet interface (`eth0`) connected to a LAN.

### TAP Lifecycle

TAP devices are created during `Create()` and destroyed during `Destroy()`. They are **not** destroyed during `Stop()` (snapshot to disk). The Firecracker snapshot contains virtio-net state that references the TAP. If the TAP were destroyed and recreated, the restored guest's network stack would find a different TAP than what it remembers, breaking connectivity.

On engine startup, orphaned TAP devices (from previous crashes) are cleaned up:

```go
func cleanupOrphanedTapDevices(knownTaps map[string]bool) {
    // List all "tun" type devices
    // Delete any "tap*" device not in the known set
}
```

At startup, no VMs are loaded yet, so all TAP devices are orphans.

## Kernel-Level Network Configuration

The guest IP is configured via the kernel `ip=` command-line parameter, passed through Firecracker's boot args:

```
ip=192.168.137.2::192.168.137.1:255.255.255.0::eth0:off:1.1.1.1:8.8.8.8:
```

This tells the kernel to configure `eth0` with the given IP, gateway, netmask, and DNS *before init runs*. By the time lohar starts as PID 1, the network is already up.

This is a standard Linux kernel feature (`Documentation/admin-guide/kernel-parameters.txt`). It solves the chicken-and-egg problem: if the agent configures networking, how does the host talk to the agent to tell it what IP to use?

Alternatives considered:
- **DHCP**: adds a DHCP server on the bridge, adds a DHCP client in the guest, adds latency, adds a failure mode. Rejected.
- **Config drive + agent**: the agent reads its IP from the config drive and runs `ip addr add`. Requires the agent to start before networking is available. The host can't poll the agent until networking is up. Creates a race condition. Rejected.
- **Kernel `ip=`**: zero dependencies, zero latency, zero failure modes. Adopted.

## Bridge Setup

`ensureBridge()` runs on every engine startup and is idempotent:

1. Create bridge `brbhatti0` (ignore "already exists")
2. Assign `192.168.137.1/24` (ignore "already set")
3. Bring bridge up
4. Enable IP forwarding (`/proc/sys/net/ipv4/ip_forward`)
5. Add masquerade rule for the subnet (if not already present)
6. Add FORWARD rules for bridge traffic (needed when the default FORWARD policy is DROP, e.g., Kubernetes sets this)

The FORWARD rules are inserted at position 1 (`iptables -I FORWARD 1`) so they take priority over any DROP rules added by other software.

The default outbound interface is auto-detected:

```go
func detectDefaultInterface() string {
    out := exec.Command("ip", "route", "show", "default").Output()
    // parse "default via X.X.X.X dev eth0" → "eth0"
}
```

## MAC Address Generation

Each VM gets a random locally-administered unicast MAC address:

```go
func generateMAC() string {
    b := make([]byte, 6)
    rand.Read(b)
    b[0] = (b[0] & 0xfe) | 0x02  // locally administered, unicast
    return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3], b[4], b[5])
}
```

The `0x02` bit marks it as locally administered (won't conflict with real hardware MACs). The `0xfe` mask clears the multicast bit.

## Post-Snapshot Networking

After snapshot/restore:

- **Virtio-net works.** The guest kernel's TCP stack, the TAP device, and the bridge are all intact. Host-initiated TCP connections (the agent client dialing port 1024) succeed immediately.
- **Vsock does not work.** Guest-side vsock listeners never receive connections after restore. This is a known Firecracker limitation. See [Thermal Management](thermal-management.md#why-tcp-not-vsock-after-restore) for details.
- **Guest-initiated TCP has stale conntrack.** The host's iptables conntrack table has stale entries for the guest IP. Guest-initiated SYN packets get stuck in kernel retransmit backoff for 30+ seconds. This doesn't affect normal operation (the host always initiates connections to the agent), but it means `ping` and outbound TCP from the guest may be slow immediately after restore. An ARP flush helps:

```bash
ip neigh flush dev brbhatti0
```

## Reverse Proxy

Two proxy paths exist:

**Authenticated proxy** (API users):
```
Browser → bhatti :8080 → /sandboxes/:id/proxy/:port/path → Engine.Tunnel() → lohar → localhost:port
```

**Public proxy** (published ports, no auth):
```
Browser → Cloudflare → bhatti :443 → Host: my-app.bhatti.sh → alias lookup → EnsureHot() → Tunnel() → lohar → localhost:port
```

Both use `httputil.ReverseProxy` with a custom `tunnelTransport` that wraps `Engine.Tunnel()` as an `http.RoundTripper`. This gives proper hop-by-hop header removal, chunked transfer encoding, and streaming support (`FlushInterval: -1` flushes every chunk for SSE). A `context.AfterFunc` guard ensures tunnel FDs are cleaned up on client disconnect.

WebSocket connections are hijacked and relayed bidirectionally with a 10-minute idle timeout.

The public proxy adds:
- In-memory route cache (LRU, 10K entries) — zero SQLite on hot path
- `singleflight.Group` for resume coalescing — 50 concurrent requests to a cold sandbox share one wake
- Per-alias + global rate limiting
- 5-minute per-request deadline, 50MB body limit

No direct network access to the VM is required — everything goes through the engine's tunnel abstraction. This works identically whether the VM's IP is reachable from the client or not (important for Docker Desktop on macOS, where container IPs are unreachable from the host).

See [API Reference](api-reference.md#publish-public-preview-urls) and [CLI Reference](cli-reference.md#publish) for usage.
