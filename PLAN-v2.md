# Bhatti v2 — Implementation Plan

Continues from Parts 1–7 (protocol, lohar, client, images, FC engine,
Pi tests, persistence). See `ARCHITECTURE.md` for the full system map.

### Dependency graph

```
Part 8  (rename)          — no deps
     ↓
Part 9  (bridge)          — needs Part 8 (paths)
     ↓
Part 10 (config drive)    — needs Part 9 (bridge IPs)
     ↓
Part 11 (unified exec)    — needs Part 10 (auth, init)
     ↓
Part 12 (thermals)        — needs Part 11 (activity = attached sessions)
     ↓
Part 13 (files)           — needs Part 10 (auth)
     ↓
Part 14 (CLI)             — needs Parts 11-13 (all primitives)
     ↓
Part 15 (OCI images)      — independent
     ↓
Part 16 (deployment)      — needs Part 14 (CLI)
```

---

## Part 8 — Rename Guest Agent to Lohar

### 8.1 Renames

| Before | After |
|---|---|
| `cmd/bhatti-agent/` | `cmd/lohar/` |
| `bin/bhatti-agent-linux-arm64` | `bin/lohar-linux-arm64` |
| `BHATTI_AGENT_TEST` | `LOHAR_TEST` |
| `BHATTI_AGENT_SOCK` | `LOHAR_SOCK` |
| `BHATTI_AGENT_FWD_SOCK` | `LOHAR_FWD_SOCK` |
| `BHATTI_AGENT_BIN` | `LOHAR_BIN` |
| log prefix `bhatti-agent:` | `lohar:` |
| `init=/usr/local/bin/bhatti-agent` | `init=/usr/local/bin/lohar` |
| `scripts/build-rootfs.sh` agent refs | updated |
| `docs/pi-setup.md` | updated |

Module path `github.com/sahilshubham/bhatti` and `pkg/agent/` package
name stay. `bhatti` binary name stays.

### 8.2 Verification

```bash
grep -r "bhatti-agent\|bhatti_agent\|BHATTI_AGENT" --include='*.go' --include='*.sh' --include='*.md'
# Should return zero results after rename (excluding PLAN.md/git history)
go build ./...
go test ./... -count=1
```

---

## Part 9 — Bridge Networking

### 9.1 IP Pool

```go
// pkg/engine/firecracker/network.go

const (
    bridgeName = "brbhatti0"
    bridgeIP   = "192.168.137.1"
    bridgeCIDR = "192.168.137.1/24"
    subnetCIDR = "192.168.137.0/24"
)

type ipPool struct {
    mu   sync.Mutex
    used [256]bool // index = last octet; 0=network, 1=bridge, 255=broadcast
}

// Allocate returns the next free IP in the 192.168.137.0/24 range.
// Usable range: .2 through .254 (253 addresses).
func (p *ipPool) Allocate() (string, error) {
    p.mu.Lock()
    defer p.mu.Unlock()
    for i := 2; i < 255; i++ {
        if !p.used[i] {
            p.used[i] = true
            return fmt.Sprintf("192.168.137.%d", i), nil
        }
    }
    return "", fmt.Errorf("IP pool exhausted (253 sandboxes)")
}

// Release frees an IP back to the pool.
func (p *ipPool) Release(ip string) {
    var octet int
    fmt.Sscanf(ip, "192.168.137.%d", &octet)
    p.mu.Lock()
    p.used[octet] = false
    p.mu.Unlock()
}

// Mark reserves an IP (used during startup recovery).
func (p *ipPool) Mark(ip string) {
    var octet int
    fmt.Sscanf(ip, "192.168.137.%d", &octet)
    p.mu.Lock()
    p.used[octet] = true
    p.mu.Unlock()
}
```

### 9.2 Bridge Setup

```go
// ensureBridge creates the bridge and masquerade rule if they don't exist.
// Idempotent — safe to call on every engine startup.
func ensureBridge() error {
    // Create bridge (fails silently if exists)
    run("ip", "link", "add", bridgeName, "type", "bridge")
    run("ip", "addr", "add", bridgeCIDR, "dev", bridgeName)
    run("ip", "link", "set", bridgeName, "up")

    // Add masquerade rule if not present
    defaultIface := detectDefaultInterface()
    if err := run("iptables", "-t", "nat", "-C", "POSTROUTING",
        "-s", subnetCIDR, "-o", defaultIface, "-j", "MASQUERADE"); err != nil {
        run("iptables", "-t", "nat", "-A", "POSTROUTING",
            "-s", subnetCIDR, "-o", defaultIface, "-j", "MASQUERADE")
    }
    return nil
}
```

### 9.3 Per-VM TAP (simplified)

```go
func createTapDevice(sandboxID string) (tapName string, err error) {
    tapName = "tap" + sandboxID[:8]
    if err := run("ip", "tuntap", "add", tapName, "mode", "tap"); err != nil {
        return "", fmt.Errorf("create tap: %w", err)
    }
    if err := run("ip", "link", "set", tapName, "master", bridgeName); err != nil {
        run("ip", "link", "del", tapName)
        return "", fmt.Errorf("add to bridge: %w", err)
    }
    if err := run("ip", "link", "set", tapName, "up"); err != nil {
        run("ip", "link", "del", tapName)
        return "", fmt.Errorf("bring up tap: %w", err)
    }
    return tapName, nil
}

func destroyTapDevice(tapName string) {
    run("ip", "link", "del", tapName)
}
```

No per-VM iptables. No per-VM IP assignment on the host side.

### 9.4 Engine changes

`New()` calls `ensureBridge()` and initializes the IP pool. `Create()` uses
`pool.Allocate()`. `Destroy()` calls `pool.Release()`. Startup recovery
calls `pool.Mark()` for each existing sandbox's IP.

Boot args change:
```
ip=192.168.137.X::192.168.137.1:255.255.255.0::eth0:off:1.1.1.1:8.8.8.8:
```

### 9.5 Tests

- `TestBridgeIdempotent` — call `ensureBridge()` twice, no error.
- `TestIPPoolAllocRelease` — allocate, release, allocate same IP.
- `TestIPPoolExhaustion` — allocate 253, 254th fails.
- Integration: two VMs, exec `ping -c1 192.168.137.X` from one to the other.

---

## Part 10 — Config Drive, Secrets, Auth, Volumes

### 10.1 Secrets Store

```go
// pkg/store/store.go — additions

// Secret stores an encrypted secret value.
type Secret struct {
    Name      string    `json:"name"`
    CreatedAt time.Time `json:"created_at"`
    UpdatedAt time.Time `json:"updated_at"`
    // value_encrypted is NOT exposed via JSON
}

func (s *Store) SetSecret(name string, encrypted []byte) error
func (s *Store) GetSecretValue(name string) ([]byte, error)  // returns encrypted bytes
func (s *Store) ListSecrets() ([]Secret, error)               // names + dates only
func (s *Store) DeleteSecret(name string) error
```

```sql
CREATE TABLE IF NOT EXISTS secrets (
    name TEXT PRIMARY KEY,
    value_encrypted BLOB NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
```

### 10.2 Age Encryption

```go
// pkg/secrets/age.go

import "filippo.io/age"

// EnsureKey generates an age identity at path if it doesn't exist.
// Returns the identity (for decryption) and recipient (for encryption).
func EnsureKey(path string) (*age.X25519Identity, *age.X25519Recipient, error)

// Encrypt encrypts plaintext with the recipient.
func Encrypt(plaintext []byte, recipient *age.X25519Recipient) ([]byte, error)

// Decrypt decrypts ciphertext with the identity.
func Decrypt(ciphertext []byte, identity *age.X25519Identity) ([]byte, error)
```

Key stored at `~/.bhatti/age.key`. Generated on first run alongside the
SSH keypair.

### 10.3 Config Drive Structure

```go
// pkg/engine/firecracker/configdrive.go

type SandboxConfig struct {
    SandboxID string            `json:"sandbox_id"`
    Hostname  string            `json:"hostname"`
    Token     string            `json:"token"`              // agent auth
    Env       map[string]string `json:"env"`                // env vars
    Files     map[string]ConfigFile `json:"files"`          // path → content
    Volumes   []VolumeMount     `json:"volumes"`            // device → mountpoint
    Init      string            `json:"init,omitempty"`     // boot script
    DNS       []string          `json:"dns"`
    User      string            `json:"user"`               // default: "lohar"
}

type ConfigFile struct {
    Content string `json:"content"` // base64-encoded
    Mode    string `json:"mode"`    // e.g. "0600"
}

type VolumeMount struct {
    Device string `json:"device"` // e.g. "/dev/vdc"
    Mount  string `json:"mount"`  // e.g. "/workspace"
    FS     string `json:"fs"`     // e.g. "ext4"
}
```

### 10.4 Config Drive Creation

```go
// createConfigDrive builds a 1MB ext4 image containing config.json.
func createConfigDrive(path string, cfg SandboxConfig) error {
    // 1. Create 1MB sparse file
    f, _ := os.Create(path)
    f.Truncate(1 << 20) // 1MB
    f.Close()

    // 2. Format as ext4
    if err := exec.Command("mkfs.ext4", "-F", "-q", path).Run(); err != nil {
        return fmt.Errorf("mkfs: %w", err)
    }

    // 3. Mount
    mountDir := path + ".mnt"
    os.MkdirAll(mountDir, 0700)
    if err := exec.Command("mount", path, mountDir).Run(); err != nil {
        return fmt.Errorf("mount: %w", err)
    }
    defer func() {
        exec.Command("umount", mountDir).Run()
        os.Remove(mountDir)
    }()

    // 4. Write config.json
    data, _ := json.MarshalIndent(cfg, "", "  ")
    if err := os.WriteFile(filepath.Join(mountDir, "config.json"), data, 0644); err != nil {
        return fmt.Errorf("write config: %w", err)
    }
    return nil
}
```

### 10.5 Volume Creation

```go
// createVolume creates an ext4 image of the specified size.
func createVolume(path string, sizeMB int) error {
    f, _ := os.Create(path)
    f.Truncate(int64(sizeMB) << 20)
    f.Close()
    return exec.Command("mkfs.ext4", "-F", "-q", path).Run()
}
```

Called during `Create()` for each `--volume NAME:SIZE:MOUNT` in the spec.
Attached as additional drives after the config drive:
- `/dev/vdb` = config drive (always)
- `/dev/vdc` = first volume
- `/dev/vdd` = second volume, etc.

### 10.6 Agent Auth

New constant:

```go
// pkg/agent/proto/constants.go
AUTH byte = 0x11 // host → guest: token bytes (first frame after connect)
```

**Lohar side** — in `handleControlConnection` and `handleForwardConnection`:

```go
var agentToken string // set during boot from config drive

func handleControlConnection(conn net.Conn) {
    defer conn.Close()

    if agentToken != "" {
        conn.SetReadDeadline(time.Now().Add(5 * time.Second))
        msgType, payload, err := proto.ReadFrame(conn)
        conn.SetReadDeadline(time.Time{})
        if err != nil || msgType != proto.AUTH || string(payload) != agentToken {
            proto.WriteFrame(conn, proto.ERROR, []byte("auth required"))
            return
        }
    }

    // existing dispatch continues...
    msgType, payload, err := proto.ReadFrame(conn)
    // ...
}
```

**Client side** — `AgentClient` gains a `token` field:

```go
type AgentClient struct {
    controlSock string
    forwardSock string
    isVsock     bool
    tcpAddr     string
    token       string // auth token, empty = no auth
}

func NewTCPClient(guestIP, token string) *AgentClient {
    return &AgentClient{tcpAddr: guestIP, token: token}
}

func (c *AgentClient) sendAuth(conn net.Conn) error {
    if c.token == "" { return nil }
    return proto.WriteFrame(conn, proto.AUTH, []byte(c.token))
}

// Called at the start of dialControl() and dialForward() after connecting:
//   conn, err := /* dial */
//   if err := c.sendAuth(conn); err != nil { conn.Close(); return ... }
```

### 10.7 Lohar Boot Sequence (updated)

```go
func main() {
    if os.Getenv("LOHAR_TEST") == "1" { runTestMode(); return }

    // PID 1 init
    os.Setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
    os.Setenv("HOME", "/root")

    mustMount("proc", "/proc", "proc", 0, "")
    mustMount("sysfs", "/sys", "sysfs", 0, "")
    mustMount("devtmpfs", "/dev", "devtmpfs", 0, "")
    os.MkdirAll("/dev/pts", 0755)
    mustMount("devpts", "/dev/pts", "devpts", 0, "newinstance,ptmxmode=0666")
    mustMount("tmpfs", "/tmp", "tmpfs", 0, "")
    mustMount("tmpfs", "/run", "tmpfs", 0, "")

    bringUpInterface("lo")

    // Config drive
    cfg := loadConfigDrive() // mount /dev/vdb, parse config.json

    if cfg != nil {
        if cfg.Hostname != "" { syscall.Sethostname([]byte(cfg.Hostname)) }
        applyDNS(cfg.DNS)
        agentToken = cfg.Token
        applyEnv(cfg.Env)             // store for buildEnv()
        writeSecretFiles(cfg.Files)   // write files, set modes + ownership
        mountVolumes(cfg.Volumes)     // mount /dev/vdc etc.
    } else {
        syscall.Sethostname([]byte("bhatti"))
        ensureResolvConf()
    }

    installSignalHandlers()
    startListeners() // TCP + vsock on :1024 and :1025

    // Init script runs as a session
    if cfg != nil && cfg.Init != "" {
        go runInitSession(cfg.Init, cfg.User)
    }

    logf("ready")
    select {} // block forever
}

// loadConfigDrive mounts /dev/vdb and reads config.json.
// Returns nil if /dev/vdb doesn't exist (backward compatible).
func loadConfigDrive() *SandboxConfig {
    os.MkdirAll("/run/bhatti/config", 0755)
    if err := syscall.Mount("/dev/vdb", "/run/bhatti/config", "ext4",
        syscall.MS_RDONLY, ""); err != nil {
        return nil
    }
    data, err := os.ReadFile("/run/bhatti/config/config.json")
    if err != nil { return nil }
    var cfg SandboxConfig
    json.Unmarshal(data, &cfg)
    return &cfg
}

// writeSecretFiles writes files from the config drive to the filesystem.
func writeSecretFiles(files map[string]ConfigFile) {
    for path, cf := range files {
        content, _ := base64.StdEncoding.DecodeString(cf.Content)
        os.MkdirAll(filepath.Dir(path), 0755)
        mode, _ := strconv.ParseUint(cf.Mode, 8, 32)
        os.WriteFile(path, content, os.FileMode(mode))
        // chown to lohar (uid 1000)
        os.Chown(path, 1000, 1000)
        os.Chown(filepath.Dir(path), 1000, 1000)
    }
}

// mountVolumes mounts volume devices from the config drive.
func mountVolumes(volumes []VolumeMount) {
    for _, v := range volumes {
        os.MkdirAll(v.Mount, 0755)
        if err := syscall.Mount(v.Device, v.Mount, v.FS, 0, ""); err != nil {
            logf("mount %s → %s: %v", v.Device, v.Mount, err)
            continue
        }
        os.Chown(v.Mount, 1000, 1000)
    }
}
```

### 10.8 SandboxSpec Changes

```go
// pkg/engine/engine.go — additions to SandboxSpec

type SecretRef struct {
    Name string `json:"name"`       // secret name in bhatti's store
    Path string `json:"path"`       // file path OR env var name
    Mode string `json:"mode"`       // file mode (empty = env var)
}

type VolumeSpec struct {
    Name   string `json:"name"`
    SizeMB int    `json:"size_mb"`
    Mount  string `json:"mount"`
}

type SandboxSpec struct {
    // ... existing fields ...
    Secrets []SecretRef  `json:"secrets,omitempty"`
    Volumes []VolumeSpec `json:"volumes,omitempty"`
    Init    string       `json:"init,omitempty"`
}
```

### 10.9 Firecracker Create() Changes

After copying rootfs, before starting FC:

```go
// 1. Generate auth token
token := hex.EncodeToString(randomBytes(16))

// 2. Resolve secrets → build env map and files map
env := make(map[string]string)
files := make(map[string]ConfigFile)
for _, s := range spec.Secrets {
    encrypted, _ := store.GetSecretValue(s.Name)
    plaintext, _ := secrets.Decrypt(encrypted, ageIdentity)
    if s.Mode != "" {
        // File secret
        files[s.Path] = ConfigFile{
            Content: base64.StdEncoding.EncodeToString(plaintext),
            Mode:    s.Mode,
        }
    } else {
        // Env secret
        env[s.Path] = string(plaintext)
    }
}
// Merge spec.Env
for k, v := range spec.Env { env[k] = v }

// 3. Create volumes
var volumeMounts []VolumeMount
driveIndex := 'c' // vdb=config, vdc=first vol, vdd=second, ...
for _, vs := range spec.Volumes {
    volPath := filepath.Join(sandboxDir, fmt.Sprintf("vol-%s.ext4", vs.Name))
    createVolume(volPath, vs.SizeMB)
    device := fmt.Sprintf("/dev/vd%c", driveIndex)
    volumeMounts = append(volumeMounts, VolumeMount{
        Device: device, Mount: vs.Mount, FS: "ext4",
    })
    // Attach as drive in FC config
    fcPut(client, fmt.Sprintf("/drives/vol-%s", vs.Name), fmt.Sprintf(
        `{"drive_id":"vol-%s","path_on_host":%q,"is_root_device":false,"is_read_only":false}`,
        vs.Name, volPath))
    driveIndex++
}

// 4. Create config drive
configDrivePath := filepath.Join(sandboxDir, "config.ext4")
createConfigDrive(configDrivePath, SandboxConfig{
    SandboxID: id,
    Hostname:  name,
    Token:     token,
    Env:       env,
    Files:     files,
    Volumes:   volumeMounts,
    Init:      spec.Init,
    DNS:       []string{"1.1.1.1", "8.8.8.8"},
    User:      "lohar",
})

// 5. Attach config drive
fcPut(client, "/drives/config", fmt.Sprintf(
    `{"drive_id":"config","path_on_host":%q,"is_root_device":false,"is_read_only":true}`,
    configDrivePath))

// 6. Pass token to AgentClient
agentClient := agent.NewTCPClient(guestIP, token)
```

### 10.10 Tests

- `TestSecretSetGetDelete` — set, get (returns encrypted), decrypt, verify.
- `TestSecretListNoValues` — list returns names, not values.
- `TestConfigDriveRoundTrip` — create config drive, mount, read config.json,
  verify all fields.
- `TestAuthRequired` — start lohar with token, connect without AUTH, verify
  rejected.
- `TestAuthSuccess` — connect with correct AUTH, exec succeeds.
- `TestAuthBackwardCompat` — lohar with no token, connect without AUTH, works.
- `TestSecretFileInjection` — create sandbox with file secret, exec
  `cat /home/lohar/.ssh/id_ed25519`, verify content matches.
- `TestSecretEnvInjection` — create sandbox with env secret, exec
  `echo $NPM_TOKEN`, verify value.
- `TestVolumeMount` — create sandbox with volume, exec `df -h /workspace`,
  verify mount exists with correct size.
- `TestInitScript` — create sandbox with `--init "echo hello > /tmp/init-done"`,
  wait, exec `cat /tmp/init-done`, verify.

---

## Part 11 — Unified Exec (Session-Aware)

Every exec is a session. No separate session concept.

### 11.1 ExecRequest Changes

```go
// pkg/agent/proto/messages.go

type ExecRequest struct {
    Argv       []string          `json:"argv"`
    Env        map[string]string `json:"env,omitempty"`
    TTY        *bool             `json:"tty,omitempty"`
    Rows       *uint16           `json:"rows,omitempty"`
    Cols       *uint16           `json:"cols,omitempty"`
    Cwd        *string           `json:"cwd,omitempty"`
    SessionID  *string           `json:"session_id,omitempty"`   // nil = create new
    MaxIdleSec *int              `json:"max_idle_sec,omitempty"` // nil = default
}

type SessionInfo struct {
    SessionID string `json:"session_id"`
    Argv      string `json:"argv"`       // display string
    TTY       bool   `json:"tty"`
    Running   bool   `json:"running"`
    ExitCode  *int   `json:"exit_code,omitempty"`
    Attached  bool   `json:"attached"`
    CreatedAt int64  `json:"created_at"` // unix timestamp
}
```

### 11.2 New Frame Types

```go
// pkg/agent/proto/constants.go — additions

EXEC_LIST_REQ  byte = 0x30  // host → guest: empty payload
EXEC_LIST_RESP byte = 0x31  // guest → host: JSON []SessionInfo
EXEC_KILL      byte = 0x32  // host → guest: JSON {"session_id": "..."}
SESSION_INFO   byte = 0x33  // guest → host: JSON SessionInfo
                             //   sent once on create or attach, before STDOUT
```

### 11.3 Session Registry (lohar)

```go
// cmd/lohar/session.go

type Session struct {
    ID         string
    Argv       []string
    TTY        bool
    Master     *os.File         // PTY master fd (TTY only)
    Cmd        *exec.Cmd
    Scrollback *ringBuffer      // 64KB (TTY only)
    Attached   net.Conn         // currently attached connection (nil = detached)
    ExitCode   *int             // nil = still running
    MaxIdle    time.Duration    // 0 = forever
    CreatedAt  time.Time
    mu         sync.Mutex
    idleTimer  *time.Timer      // started on detach, cancelled on attach
}

var registry = struct {
    sync.Mutex
    sessions map[string]*Session
    counter  int
}{sessions: make(map[string]*Session)}

// newSession allocates a session ID and registers it.
func newSession(argv []string, tty bool, maxIdle time.Duration) *Session {
    registry.Lock()
    defer registry.Unlock()
    registry.counter++
    s := &Session{
        ID:        fmt.Sprintf("s%d", registry.counter),
        Argv:      argv,
        TTY:       tty,
        MaxIdle:   maxIdle,
        CreatedAt: time.Now(),
    }
    if tty {
        s.Scrollback = newRingBuffer(65536) // 64KB
    }
    registry.sessions[s.ID] = s
    return s
}

func getSession(id string) *Session { ... }
func listSessions() []SessionInfo { ... }
func removeSession(id string) { ... }
```

### 11.4 Ring Buffer

```go
type ringBuffer struct {
    buf  []byte
    size int
    w    int  // next write position
    full bool
}

func newRingBuffer(size int) *ringBuffer {
    return &ringBuffer{buf: make([]byte, size), size: size}
}

func (r *ringBuffer) Write(p []byte) (int, error) {
    for _, b := range p {
        r.buf[r.w] = b
        r.w = (r.w + 1) % r.size
        if r.w == 0 { r.full = true }
    }
    return len(p), nil
}

// Bytes returns the buffered content in order. Oldest bytes first.
func (r *ringBuffer) Bytes() []byte {
    if !r.full { return r.buf[:r.w] }
    out := make([]byte, r.size)
    n := copy(out, r.buf[r.w:])
    copy(out[n:], r.buf[:r.w])
    return out
}
```

### 11.5 Dispatch Changes (lohar handler.go)

```go
func handleControlConnection(conn net.Conn) {
    defer conn.Close()

    // Auth (from Part 10)
    if agentToken != "" { /* ... auth check ... */ }

    msgType, payload, err := proto.ReadFrame(conn)
    if err != nil { return }

    switch msgType {
    case proto.EXEC_REQ:
        var req proto.ExecRequest
        json.Unmarshal(payload, &req)
        if req.SessionID != nil {
            // Attach to existing session
            handleSessionAttach(conn, *req.SessionID)
        } else if req.TTY != nil && *req.TTY {
            // Create new TTY session
            handleTTYSession(conn, req)
        } else {
            // Create new non-TTY session
            handlePipedSession(conn, req)
        }

    case proto.EXEC_LIST_REQ:
        sessions := listSessions()
        proto.SendJSON(conn, proto.EXEC_LIST_RESP, sessions)

    case proto.EXEC_KILL:
        var req struct{ SessionID string `json:"session_id"` }
        json.Unmarshal(payload, &req)
        s := getSession(req.SessionID)
        if s == nil {
            proto.WriteFrame(conn, proto.ERROR, []byte("session not found"))
            return
        }
        s.Cmd.Process.Signal(syscall.SIGTERM)
        proto.WriteFrame(conn, proto.EXIT, proto.ExitPayload(0)[:])

    // File operations (Part 13)
    case proto.FILE_READ_REQ:  // ...
    case proto.FILE_WRITE_REQ: // ...
    // ...
    }
}
```

### 11.6 TTY Session (replaces current handleTTYExec)

Key change: on host disconnect, **don't SIGHUP**. Keep PTY and process
alive. Start idle timer.

```go
func handleTTYSession(conn net.Conn, req proto.ExecRequest) {
    maxIdle := time.Duration(0) // forever by default
    if req.MaxIdleSec != nil { maxIdle = time.Duration(*req.MaxIdleSec) * time.Second }

    sess := newSession(req.Argv, true, maxIdle)
    master, slave := openPTY()
    sess.Master = master
    // ... set winsize, spawn process with slave as controlling terminal ...
    slave.Close()

    // Send session info to host
    proto.SendJSON(conn, proto.SESSION_INFO, SessionInfo{
        SessionID: sess.ID, Argv: strings.Join(req.Argv, " "),
        TTY: true, Running: true, CreatedAt: sess.CreatedAt.Unix(),
    })

    sess.mu.Lock()
    sess.Attached = conn
    sess.mu.Unlock()

    // Background goroutine: PTY master → scrollback + attached conn
    go func() {
        buf := make([]byte, 4096)
        for {
            n, err := master.Read(buf)
            if n > 0 {
                sess.Scrollback.Write(buf[:n])
                sess.mu.Lock()
                if sess.Attached != nil {
                    proto.WriteFrame(sess.Attached, proto.STDOUT, buf[:n])
                }
                sess.mu.Unlock()
            }
            if err != nil {
                // PTY closed — process exited
                exitCode := exitCodeFromErr(sess.Cmd.Wait())
                sess.mu.Lock()
                sess.ExitCode = &exitCode
                if sess.Attached != nil {
                    exit := proto.ExitPayload(int32(exitCode))
                    proto.WriteFrame(sess.Attached, proto.EXIT, exit[:])
                }
                sess.mu.Unlock()
                return
            }
        }
    }()

    // Host → PTY master (STDIN, RESIZE, KILL, disconnect)
    for {
        msgType, payload, err := proto.ReadFrame(conn)
        if err != nil {
            // Host disconnected — detach, don't kill
            sess.mu.Lock()
            sess.Attached = nil
            sess.mu.Unlock()
            sess.startIdleTimer()
            return
        }
        updateActivity()
        switch msgType {
        case proto.STDIN:
            master.Write(payload)
        case proto.RESIZE:
            if r, c, ok := proto.ParseResize(payload); ok {
                setWinsize(master, r, c)
            }
        case proto.KILL:
            sess.Cmd.Process.Signal(syscall.SIGTERM)
            return
        }
    }
}
```

### 11.7 Session Attach

```go
func handleSessionAttach(conn net.Conn, sessionID string) {
    sess := getSession(sessionID)
    if sess == nil {
        proto.WriteFrame(conn, proto.ERROR, []byte("session not found"))
        return
    }

    sess.mu.Lock()
    // Detach previous client if any
    if sess.Attached != nil {
        exit := proto.ExitPayload(0)
        proto.WriteFrame(sess.Attached, proto.EXIT, exit[:])
        sess.Attached = nil
    }
    // Cancel idle timer
    sess.cancelIdleTimer()
    sess.Attached = conn
    sess.mu.Unlock()

    // Send session info
    info := SessionInfo{
        SessionID: sess.ID, Argv: strings.Join(sess.Argv, " "),
        TTY: sess.TTY, Running: sess.ExitCode == nil,
        ExitCode: sess.ExitCode, Attached: true,
        CreatedAt: sess.CreatedAt.Unix(),
    }
    proto.SendJSON(conn, proto.SESSION_INFO, info)

    // Replay scrollback
    if sess.Scrollback != nil {
        scrollback := sess.Scrollback.Bytes()
        if len(scrollback) > 0 {
            proto.WriteFrame(conn, proto.STDOUT, scrollback)
        }
    }

    // If process already exited, send exit and return
    if sess.ExitCode != nil {
        exit := proto.ExitPayload(int32(*sess.ExitCode))
        proto.WriteFrame(conn, proto.EXIT, exit[:])
        removeSession(sess.ID)
        return
    }

    // Read host input until disconnect
    for {
        msgType, payload, err := proto.ReadFrame(conn)
        if err != nil {
            sess.mu.Lock()
            sess.Attached = nil
            sess.mu.Unlock()
            sess.startIdleTimer()
            return
        }
        updateActivity()
        switch msgType {
        case proto.STDIN:
            sess.Master.Write(payload)
        case proto.RESIZE:
            if r, c, ok := proto.ParseResize(payload); ok {
                setWinsize(sess.Master, r, c)
            }
        case proto.KILL:
            sess.Cmd.Process.Signal(syscall.SIGTERM)
            return
        }
    }
}
```

### 11.8 Idle Timer

```go
func (s *Session) startIdleTimer() {
    if s.MaxIdle <= 0 { return } // 0 = run forever
    s.idleTimer = time.AfterFunc(s.MaxIdle, func() {
        s.Cmd.Process.Signal(syscall.SIGTERM)
    })
}

func (s *Session) cancelIdleTimer() {
    if s.idleTimer != nil {
        s.idleTimer.Stop()
        s.idleTimer = nil
    }
}
```

### 11.9 Init Script as Session

```go
func runInitSession(script, user string) {
    sess := newSession([]string{"sh", "-c", script}, true, 0)
    master, slave := openPTY()
    sess.Master = master
    sess.ID = "init" // well-known session ID

    cmd := exec.Command("sh", "-c", script)
    cmd.Env = buildEnv(nil)
    cmd.Stdin = slave
    cmd.Stdout = slave
    cmd.Stderr = slave
    cmd.SysProcAttr = &syscall.SysProcAttr{
        Setsid: true, Setctty: true, Ctty: 0,
        Credential: &syscall.Credential{Uid: 1000, Gid: 1000},
    }
    cmd.Dir = "/workspace"
    cmd.Start()
    slave.Close()
    sess.Cmd = cmd

    // Background reader: PTY master → scrollback
    go func() {
        buf := make([]byte, 4096)
        for {
            n, err := master.Read(buf)
            if n > 0 {
                sess.Scrollback.Write(buf[:n])
                sess.mu.Lock()
                if sess.Attached != nil {
                    proto.WriteFrame(sess.Attached, proto.STDOUT, buf[:n])
                }
                sess.mu.Unlock()
            }
            if err != nil {
                exitCode := exitCodeFromErr(cmd.Wait())
                sess.mu.Lock()
                sess.ExitCode = &exitCode
                sess.mu.Unlock()
                return
            }
        }
    }()
}
```

Consumer watches init:
```
bhatti exec attach my-sandbox init
```

### 11.10 Engine Interface Changes

```go
type ExecOpts struct {
    Cmd        []string
    Env        map[string]string
    Cwd        string
    TTY        bool
    Rows, Cols uint16
    SessionID  string // empty = create new
    MaxIdleSec int    // 0=forever, -1=die on disconnect
}

// Replace old Exec + Shell with:
Exec(ctx, id, opts ExecOpts)       → ExecResult, error   // non-TTY, blocks
ExecStream(ctx, id, opts ExecOpts) → TerminalConn, error  // TTY or streaming
ExecList(ctx, id)                  → []SessionInfo, error
ExecKill(ctx, id, sessionID)       → error
```

### 11.11 API Endpoints

```
POST   /sandboxes/:id/exec              — non-TTY (HTTP, blocks until done)
WS     /sandboxes/:id/exec?tty=true     — new TTY session
WS     /sandboxes/:id/exec?id=SID       — attach to existing
GET    /sandboxes/:id/exec              — list sessions
DELETE /sandboxes/:id/exec/:sid         — kill
```

### 11.12 Tests

- `TestExecOneShot` — exec `echo hello`, verify stdout and exit code.
- `TestTTYSessionCreateDetachReattach` — create TTY `cat`, write `hello\n`,
  detach (close conn), reconnect with session_id, verify scrollback contains
  `hello`, write `exit\n`, verify EXIT.
- `TestProcessSurvivesDetach` — create `sh -c "sleep 1 && echo done"`,
  detach, wait 2s, reattach, verify scrollback contains `done`.
- `TestMaxIdle` — create with max_idle=2, detach, wait 3s, verify process
  killed (reattach gets EXIT).
- `TestMaxIdleZero` — create with max_idle=0 (default TTY), detach, wait
  5s, reattach, process still alive.
- `TestSessionList` — create two sessions, EXEC_LIST_REQ, verify both.
- `TestSessionKill` — create `sleep 3600`, EXEC_KILL, verify dead.
- `TestInitSession` — boot lohar with config drive containing init script,
  attach to session "init", verify output.
- `TestSnapshotSurvival` — create TTY session, write data, snapshot VM,
  restore VM, reattach to session, verify scrollback and process alive.
- `TestScrollbackOverflow` — write >64KB to a TTY session, reattach,
  verify last 64KB is returned.

---

## Part 12 — Thermal Management

### 12.1 New Engine Methods

```go
// Pause freezes vCPUs. FC process stays alive. Memory stays allocated.
// Hot → Warm. ~1ms.
func (e *Engine) Pause(ctx context.Context, id string) error {
    vm, _ := e.getVM(id)
    client := fcAPIClient(vm.SocketPath)
    return fcPatch(client, "/vm", `{"state":"Paused"}`)
}

// Resume unfreezes vCPUs. Warm → Hot. ~1ms.
func (e *Engine) Resume(ctx context.Context, id string) error {
    vm, _ := e.getVM(id)
    client := fcAPIClient(vm.SocketPath)
    return fcPatch(client, "/vm", `{"state":"Resumed"}`)
}

// Snapshot is the existing Stop() renamed. Warm → Cold.
// Pauses, writes memory to disk, kills FC process.
func (e *Engine) Snapshot(ctx context.Context, id string) error {
    // existing Stop() code
}

// Restore is the existing Start() renamed. Cold → Hot.
// New FC process, loads snapshot via mmap, reconnects agent.
func (e *Engine) Restore(ctx context.Context, id string) error {
    // existing Start() code
}
```

### 12.2 Activity Tracking

```go
// pkg/agent/proto/constants.go
ACTIVITY_REQ  byte = 0x40
ACTIVITY_RESP byte = 0x41

// pkg/agent/proto/messages.go
type ActivityInfo struct {
    LastActivityUnix int64 `json:"last_activity_unix"`
    ActiveSessions   int   `json:"active_sessions"`   // running processes
    AttachedSessions int   `json:"attached_sessions"`  // connected clients
}
```

In lohar: `var lastActivity int64` updated atomically on every EXEC_REQ,
STDIN, FWD_REQ. `handleControlConnection` for ACTIVITY_REQ returns the
current timestamp and session counts.

### 12.3 Thermal State Tracking

```go
// pkg/engine/firecracker/engine.go

// VM gains a thermal field:
type VM struct {
    // ... existing fields ...
    Thermal string // "hot", "warm", "cold"
}
```

### 12.4 ensureHot

```go
// pkg/server/server.go

func (s *Server) ensureHot(ctx context.Context, engineID string) error {
    thermal := s.engine.(*fc.Engine).ThermalState(engineID)
    switch thermal {
    case "hot":
        return nil
    case "warm":
        return s.engine.Resume(ctx, engineID)
    case "cold":
        return s.engine.Restore(ctx, engineID)
    }
    return nil
}
```

Called at the top of: exec handlers, file handlers, tunnel/proxy handlers,
port listing. NOT called for: sandbox get, sandbox list, session list
(returns cached data).

### 12.5 Thermal Manager Goroutine

```go
func (s *Server) runThermalManager(warmTimeout, coldTimeout time.Duration) {
    ticker := time.NewTicker(10 * time.Second)
    go func() {
        for range ticker.C {
            sandboxes, _ := s.store.ListSandboxes()
            for _, sb := range sandboxes {
                if sb.Status != "running" { continue }

                eng := s.engine.(*fc.Engine)
                thermal := eng.ThermalState(sb.EngineID)
                if thermal == "cold" { continue } // already cold

                activity, err := eng.Activity(context.Background(), sb.EngineID)
                if err != nil { continue }

                idle := time.Since(time.Unix(activity.LastActivityUnix, 0))

                if thermal == "hot" && idle > warmTimeout && activity.AttachedSessions == 0 {
                    eng.Pause(context.Background(), sb.EngineID)
                    logf("sandbox %s → warm (idle %v)", sb.Name, idle)
                }
                if thermal == "warm" && idle > coldTimeout {
                    eng.Snapshot(context.Background(), sb.EngineID)
                    s.saveVMState(sb.ID, sb.EngineID)
                    logf("sandbox %s → cold (idle %v)", sb.Name, idle)
                }
            }
        }
    }()
}
```

Defaults: `warmTimeout = 30s`, `coldTimeout = 30min`. Configurable in
config.yaml.

### 12.6 Consumer-Facing Status

`GET /sandboxes/:id` always returns `"status": "running"` regardless of
thermal state. Thermal state is only visible via `--verbose` in the CLI.

Remove `POST /sandboxes/:id/stop` and `POST /sandboxes/:id/start`.
Add CLI operator commands `bhatti sandbox suspend` / `bhatti sandbox resume`.

### 12.7 Tests

- `TestPauseResume` — create, exec to verify running, pause, resume, exec
  again to verify still works. Measure resume latency (<50ms).
- `TestSnapshotRestore` — create, write file, start background process,
  snapshot, restore, verify file + process alive. Measure restore latency.
- `TestAutoWarm` — set warmTimeout=2s, create sandbox, wait 5s, verify
  thermal=warm. Exec triggers resume, verify thermal=hot.
- `TestAutoCold` — set warmTimeout=1s, coldTimeout=3s, wait 5s, verify
  thermal=cold. Exec triggers restore, verify works.
- `TestAttachedSessionPreventsWarm` — create sandbox, attach a TTY session,
  wait past warmTimeout, verify still hot.
- `TestEnsureHotOnProxy` — sandbox goes cold, HTTP request to proxy/:port,
  verify ensureHot() fires, request succeeds.

---

## Part 13 — Filesystem API

### 13.1 Frame Types

```go
FILE_READ_REQ   byte = 0x50  // host → guest: JSON {"path": "/workspace/f.txt"}
FILE_READ_RESP  byte = 0x51  // guest → host: JSON {"size": N, "mode": "0644"}
                              //   followed by STDOUT frames with content,
                              //   then EXIT with code 0
FILE_WRITE_REQ  byte = 0x52  // host → guest: JSON {"path": "...", "mode": "0644", "size": N}
                              //   followed by STDIN frames with content
FILE_WRITE_RESP byte = 0x53  // guest → host: JSON {"status": "ok"} after all STDIN received
FILE_STAT_REQ   byte = 0x54  // host → guest: JSON {"path": "..."}
FILE_STAT_RESP  byte = 0x55  // guest → host: JSON {"size", "mode", "mtime", "is_dir"}
FILE_LS_REQ     byte = 0x56  // host → guest: JSON {"path": "..."}
FILE_LS_RESP    byte = 0x57  // guest → host: JSON [{"name", "size", "mode", "is_dir"}]
```

### 13.2 Lohar Handlers

```go
// cmd/lohar/files.go

func handleFileRead(conn net.Conn, payload []byte) {
    var req struct{ Path string `json:"path"` }
    json.Unmarshal(payload, &req)

    f, err := os.Open(req.Path)
    if err != nil {
        proto.WriteFrame(conn, proto.ERROR, []byte(err.Error()))
        return
    }
    defer f.Close()

    info, _ := f.Stat()
    proto.SendJSON(conn, proto.FILE_READ_RESP, map[string]any{
        "size": info.Size(),
        "mode": fmt.Sprintf("%04o", info.Mode().Perm()),
    })

    buf := make([]byte, 32768)
    for {
        n, err := f.Read(buf)
        if n > 0 { proto.WriteFrame(conn, proto.STDOUT, buf[:n]) }
        if err != nil { break }
    }
    exit := proto.ExitPayload(0)
    proto.WriteFrame(conn, proto.EXIT, exit[:])
}

func handleFileWrite(conn net.Conn, payload []byte) {
    var req struct {
        Path string `json:"path"`
        Mode string `json:"mode"`
        Size int64  `json:"size"`
    }
    json.Unmarshal(payload, &req)

    mode, _ := strconv.ParseUint(req.Mode, 8, 32)
    if mode == 0 { mode = 0644 }

    os.MkdirAll(filepath.Dir(req.Path), 0755)
    f, err := os.OpenFile(req.Path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(mode))
    if err != nil {
        proto.WriteFrame(conn, proto.ERROR, []byte(err.Error()))
        return
    }
    defer f.Close()

    var written int64
    for written < req.Size {
        msgType, data, err := proto.ReadFrame(conn)
        if err != nil { break }
        if msgType == proto.STDIN {
            f.Write(data)
            written += int64(len(data))
        }
    }
    proto.SendJSON(conn, proto.FILE_WRITE_RESP, map[string]string{"status": "ok"})
}

func handleFileStat(conn net.Conn, payload []byte) { /* os.Stat → JSON */ }
func handleFileList(conn net.Conn, payload []byte) { /* os.ReadDir → JSON */ }
```

### 13.3 API + CLI

```
GET    /sandboxes/:id/files?path=/workspace/package.json
PUT    /sandboxes/:id/files?path=/workspace/config.yaml    (body = content)
DELETE /sandboxes/:id/files?path=/workspace/temp.txt
GET    /sandboxes/:id/files?path=/workspace/&ls=true
HEAD   /sandboxes/:id/files?path=/workspace/package.json

bhatti file read my-sandbox /workspace/package.json
bhatti file write my-sandbox /workspace/config.yaml < config.yaml
bhatti file ls my-sandbox /workspace/
```

### 13.4 Tests

- `TestFileWriteRead` — write "hello", read back, verify.
- `TestFileReadNotFound` — read nonexistent, verify ERROR.
- `TestFileStat` — write file, stat, verify size and mode.
- `TestFileList` — create several files, list directory, verify names.
- `TestFileLargeRoundTrip` — write 10MB file, read back, verify checksum.

---

## Part 14 — CLI

### 14.1 Architecture

Same binary as the daemon. Mode detection:

```go
// cmd/bhatti/main.go
func main() {
    if len(os.Args) > 1 && os.Args[1] != "serve" {
        runCLI(os.Args[1:])
        return
    }
    runDaemon()
}
```

CLI is a thin HTTP/WebSocket client. Config via `BHATTI_URL` (default
`http://localhost:8080`) and `BHATTI_TOKEN`.

### 14.2 Output

- `--format json` on all list/get commands. Default: human-readable table.
- `bhatti exec` exit code = process exit code.
- `bhatti exec --tty` passes raw terminal I/O. `Ctrl+\` detaches.
- `SIGWINCH` on host → RESIZE frame to lohar.
- Pipe-friendly: `echo hello | bhatti exec ID -- cat` works.

### 14.3 Files

```
cmd/bhatti/cli.go            — subcommand router, HTTP helpers, table formatter
cmd/bhatti/cli_sandbox.go    — create, list, get, destroy, suspend, resume
cmd/bhatti/cli_exec.go       — exec, exec list, exec attach, exec kill, shell
cmd/bhatti/cli_file.go       — read, write, ls
cmd/bhatti/cli_secret.go     — set, list, delete
cmd/bhatti/cli_image.go      — pull, list, delete
cmd/bhatti/cli_user.go       — create, list (Part 16)
```

Each file is ~50-80 lines. The CLI calls `http.NewRequest` / `json.Decode`
or upgrades to WebSocket for streaming.

---

## Part 15 — OCI Image Distribution

### 15.1 Implementation

Use `github.com/google/go-containerregistry` to push/pull ext4 images as
OCI artifacts.

```go
// pkg/images/oci.go

import "github.com/google/go-containerregistry/pkg/crane"

func Pull(ref, destDir string) (string, error) {
    // crane.Pull → extract single-layer blob → write to destDir
}

func Push(localPath, ref string) error {
    // Read ext4 file → crane.Push as single-layer OCI artifact
}

func List(storeDir string) ([]ImageInfo, error) { /* scan oci/ dir */ }
func Delete(storeDir, ref string) error { /* remove local file */ }
```

### 15.2 Storage

```
/var/lib/bhatti/images/oci/
  ghcr.io/sahilshubham/bhatti-sandbox/
    latest.ext4
    node-dev.ext4
```

### 15.3 Store

```sql
CREATE TABLE IF NOT EXISTS images (
    ref TEXT PRIMARY KEY,
    local_path TEXT NOT NULL,
    size_bytes INTEGER NOT NULL,
    pulled_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
```

### 15.4 Template/Create Integration

Templates and `--image` flag reference images by ref. Engine resolves ref
to local path via the store. Error if not pulled.

---

## Part 16 — Deployment

### 16.1 Systemd

```ini
[Unit]
Description=Bhatti Sandbox Infrastructure
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/bhatti serve
WorkingDirectory=/var/lib/bhatti
Restart=always
RestartSec=5
ExecStopPost=/bin/sh -c 'ip -o link show type tun | grep tap | cut -d: -f2 | xargs -r -n1 ip link del'

[Install]
WantedBy=multi-user.target
```

### 16.2 Multi-User

```sql
CREATE TABLE IF NOT EXISTS users (
    id TEXT PRIMARY KEY,
    name TEXT UNIQUE NOT NULL,
    api_key TEXT UNIQUE NOT NULL,
    is_admin BOOLEAN DEFAULT 0,
    max_sandboxes INTEGER DEFAULT 3,
    max_memory_mb INTEGER DEFAULT 2048,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

ALTER TABLE sandboxes ADD COLUMN user_id TEXT DEFAULT '';
```

Auth middleware: extract user from API key, filter queries by user_id.
Admin sees all.

### 16.3 Remote Access + Per-Sandbox URLs

Cloudflare Tunnel for HTTPS. Wildcard DNS for per-sandbox URLs. Bhatti
resolves `<port>-<id>.bhatti.domain` in its HTTP handler → rewrite to
`/sandboxes/:id/proxy/:port/` → `ensureHot()` → proxy.

### 16.4 Install Script

Downloads bhatti + lohar + firecracker, creates dirs, generates config +
age key, pulls default image, installs systemd service.

---

## Implementation Order

```
Week 1:
  Part 8   Rename lohar                   2 hours
  Part 9   Bridge networking              1 day
  Part 10  Config drive + secrets + auth  2 days

Week 2:
  Part 11  Unified exec (sessions)        2.5 days
  Part 12  Thermal management             1.5 days

Week 3:
  Part 13  Filesystem API                 1 day
  Part 14  CLI                            2 days
  Integration testing                     2 days

Week 4:
  Part 15  OCI images                     2 days
  Part 16  Deployment                     2 days
```
