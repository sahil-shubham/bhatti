# Bhatti — Multi-Tenant Production Hardening

Server: agni-01 (Ryzen 9 3900, 128GB ECC, 2×1.92TB NVMe RAID1, FSN1)
Endpoint: https://api.bhatti.sh (Cloudflare Tunnel)
Status: Running single-tenant. This plan takes it to secure multi-tenant.

Everything in this plan is a firm decision. No "nice to have" — each item
is either done before launch or explicitly deferred with a documented reason.

### Dependency graph

```
Part 1 (auth)      — per-user API keys, store scoping  ✅
     ↓
Part 1b (test infra) — remove Docker engine, mock engine for server tests, fix Part 1 bugs
     ↓
Part 2 (network)   — per-user bridge networks, iptables isolation
     ↓
Part 3 (proxy)     — remove TCP auto-forward, harden HTTP reverse proxy
     ↓
Part 4 (agent)     — guest hardening (exec as lohar, limits, unmount config drive)
     ↓
Part 5 (secrets)   — wire up age encryption, scope to users
     ↓
Part 6 (api)       — rate limiting, resource caps, exec timeout, output cap
     ↓
Part 7 (observability) — request logging, audit trail, metrics, error sanitization
     ↓
Part 8 (release)   — version tagging, CLI distribution, install script
```

Parts 1–3 are the security foundation. Part 4–5 harden the internals. Part
6–7 make it operable. Part 8 makes it distributable. Parts 1–7 must be done
before giving anyone an API key.

---

## Part 1 — Per-User Authentication

The single static auth token becomes per-user API keys with sandbox ownership.
This is the foundation for multi-tenancy — without it, nothing else matters.

### 1.1 Users Table

Add a `users` table to SQLite. API keys are hashed (SHA-256) so a DB leak
doesn't expose keys.

**File:** `pkg/store/store.go`

Add to the `schema` const:

```sql
CREATE TABLE IF NOT EXISTS users (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    api_key_hash TEXT NOT NULL UNIQUE,
    max_sandboxes INTEGER NOT NULL DEFAULT 5,
    max_cpus_per_sandbox INTEGER NOT NULL DEFAULT 4,
    max_memory_mb_per_sandbox INTEGER NOT NULL DEFAULT 4096,
    subnet_index INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
```

Store methods:

```go
func (s *Store) CreateUser(u User) error
func (s *Store) GetUserByKeyHash(hash string) (*User, error)
func (s *Store) GetUser(id string) (*User, error)
func (s *Store) ListUsers() ([]User, error)
func (s *Store) DeleteUser(id string) error
func (s *Store) NextSubnetIndex() (int, error)  // MAX(subnet_index)+1
```

The `User` struct:

```go
type User struct {
    ID                    string    `json:"id"`
    Name                  string    `json:"name"`
    APIKeyHash            string    `json:"-"`               // never serialized
    MaxSandboxes          int       `json:"max_sandboxes"`
    MaxCPUsPerSandbox     int       `json:"max_cpus_per_sandbox"`
    MaxMemoryMBPerSandbox int       `json:"max_memory_mb_per_sandbox"`
    SubnetIndex           int       `json:"subnet_index"`
    CreatedAt             time.Time `json:"created_at"`
}
```

### 1.2 Sandbox Ownership

Add `created_by` column to sandboxes, with a uniqueness constraint on
name within a user's namespace:

```sql
ALTER TABLE sandboxes ADD COLUMN created_by TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS idx_sandboxes_user_name
    ON sandboxes(created_by, name) WHERE status != 'destroyed';
```

The unique index prevents a user from creating two sandboxes named "dev".
Without this, `resolveID("dev")` in the CLI does a linear scan and returns
the first match — ambiguous names would silently target the wrong sandbox.
The `WHERE status != 'destroyed'` filter allows name reuse after deletion.

**Every store method that touches sandboxes takes a user ID and filters:**

```go
func (s *Store) ListSandboxes(userID string) ([]Sandbox, error)
// → SELECT ... WHERE created_by = ? ORDER BY created_at DESC

func (s *Store) GetSandbox(userID, sandboxID string) (*Sandbox, error)
// → SELECT ... WHERE id = ? AND created_by = ?

func (s *Store) DeleteSandbox(userID, sandboxID string) error
// → DELETE FROM sandboxes WHERE id = ? AND created_by = ?

func (s *Store) CountSandboxes(userID string) (int, error)
// → SELECT COUNT(*) FROM sandboxes WHERE created_by = ? AND status != 'destroyed'
```

The enforcement is at the store layer, not the handler layer. A handler that
doesn't pass a user ID cannot query sandboxes. This eliminates the class of
bugs where a new endpoint forgets to check ownership.

### 1.3 Auth Middleware

**File:** `pkg/server/server.go`

Replace the single-token check with user lookup:

```go
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    // Clean path before any checks
    cleanPath := path.Clean(r.URL.Path)

    // Unauthenticated endpoints (exact match only)
    if cleanPath == "/health" {
        s.mux.ServeHTTP(w, r)
        return
    }

    // Extract bearer token from Authorization header only.
    // No query parameter auth — eliminates token-in-URL leakage.
    token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
    if token == "" || token == r.Header.Get("Authorization") {
        errResp(w, 401, "authorization required")
        return
    }

    // Constant-time comparison via hash lookup.
    // Hash the incoming token, look up user by hash.
    hash := sha256Hex(token)
    user, err := s.store.GetUserByKeyHash(hash)
    if err != nil {
        errResp(w, 401, "invalid api key")
        return
    }

    // Attach user to request context
    ctx := context.WithValue(r.Context(), userContextKey, user)
    s.mux.ServeHTTP(w, r.WithContext(ctx))
}

func sha256Hex(s string) string {
    h := sha256.Sum256([]byte(s))
    return hex.EncodeToString(h[:])
}

func userFromContext(ctx context.Context) *store.User {
    u, _ := ctx.Value(userContextKey).(*store.User)
    return u
}
```

This solves three problems at once:
- **Constant-time comparison** — comparing hashes, not raw tokens. The
  DB lookup time is dominated by SQLite I/O, not string comparison.
- **No query parameter auth** — token only via Authorization header.
- **Path normalization** — `path.Clean` before routing.

### 1.4 Admin Bootstrap

The install script generates the first admin user:

```bash
# In scripts/install.sh, after generating config:
API_KEY=$(od -An -tx1 -N32 /dev/urandom | tr -d ' \n')
API_KEY_HASH=$(echo -n "$API_KEY" | sha256sum | cut -d' ' -f1)
sqlite3 "$DATA_DIR/state.db" "INSERT INTO users (id, name, api_key_hash, max_sandboxes, subnet_index)
  VALUES ('usr_admin', 'admin', '$API_KEY_HASH', 50, 1);"
echo "  API key: $API_KEY"
```

Future users are created via a CLI command:

```bash
bhatti user create --name alice --max-sandboxes 5
# → API key: bht_abc123...  (shown once, never stored plaintext)
```

### 1.5 Secret Scoping

Add `user_id` column to secrets:

```sql
ALTER TABLE secrets ADD COLUMN user_id TEXT NOT NULL DEFAULT '';
```

All secret store methods take `userID`:

```go
func (s *Store) SetSecret(userID, name string, encrypted []byte) error
func (s *Store) GetSecretValue(userID, name string) ([]byte, error)
func (s *Store) ListSecrets(userID string) ([]SecretRecord, error)
func (s *Store) DeleteSecret(userID, name string) error
```

### 1.6 Handler Updates

Every handler in `routes.go` extracts the user from context and passes it
through. Example for sandbox creation:

```go
func (s *Server) handleSandboxes(w http.ResponseWriter, r *http.Request) {
    user := userFromContext(r.Context())

    switch r.Method {
    case http.MethodGet:
        list, err := s.store.ListSandboxes(user.ID)
        // ...
    case http.MethodPost:
        // Check sandbox count against user limit
        count, _ := s.store.CountSandboxes(user.ID)
        if count >= user.MaxSandboxes {
            errResp(w, 429, "sandbox limit reached")
            return
        }
        // Check resource caps
        if spec.CPUs > float64(user.MaxCPUsPerSandbox) { ... }
        if spec.MemoryMB > user.MaxMemoryMBPerSandbox { ... }
        // ...
        sb.CreatedBy = user.ID
        // ...
    }
}
```

### 1.7 User Deletion

`DeleteUser` refuses if the user has active sandboxes or secrets. Same
pattern as `DeleteVolume` refusing when in use. Force explicit cleanup.

```go
func (s *Store) DeleteUser(id string) error {
    count, _ := s.CountSandboxes(id)
    if count > 0 {
        return fmt.Errorf("user has %d active sandbox(es) — destroy them first", count)
    }
    secrets, _ := s.ListSecrets(id)
    if len(secrets) > 0 {
        return fmt.Errorf("user has %d secret(s) — delete them first", len(secrets))
    }
    // Bridge network cleanup handled by caller (engine layer)
    _, err := s.db.Exec("DELETE FROM users WHERE id = ?", id)
    return err
}
```

An admin who wants to remove a user runs `bhatti destroy` for each sandbox,
`bhatti secret delete` for each secret, then `bhatti user delete`. Tedious
but safe. A `--force` flag that cascades can come later.

### 1.8 API Key Rotation

If a key is compromised, the user needs to rotate without losing their
sandboxes, secrets, subnet, or identity.

```bash
bhatti user rotate-key alice
# → New API key: bht_xyz789...  (shown once)
#   Old key is immediately invalidated.
```

Implementation: generate new key, hash it, update `api_key_hash` in the
`users` row. The user's ID, subnet_index, sandboxes, and secrets are
unchanged. All existing sessions using the old key fail auth on next request.

```go
func (s *Store) RotateUserKey(id, newKeyHash string) error {
    res, err := s.db.Exec("UPDATE users SET api_key_hash = ? WHERE id = ?",
        newKeyHash, id)
    if err != nil { return err }
    n, _ := res.RowsAffected()
    if n == 0 { return fmt.Errorf("user %q not found", id) }
    return nil
}
```

### 1.9 Testing

- `TestUserCRUD` — create user, get by key hash, list, delete
- `TestUserDeleteRefused` — create user with sandbox, delete returns error
- `TestKeyRotation` — create user, rotate key, old key returns 401, new
  key returns 200, sandboxes still accessible
- `TestSandboxScoping` — create 2 users, user A creates sandbox, user B
  cannot list/get/delete it
- `TestSandboxNameUniqueness` — create sandbox "dev", create another "dev"
  for same user → 409 conflict. Different user can use "dev".
- `TestSecretScoping` — user A's secrets invisible to user B
- `TestAuthMiddleware` — no token → 401, invalid token → 401, valid token → 200
- `TestSandboxLimit` — create up to max_sandboxes, next one returns 429
- `TestResourceCaps` — request CPUs > max → 400

---

## Part 1b — Remove Docker Engine, Mock Engine, Fix Bugs

The Docker engine served two purposes: macOS dev fallback and server
integration tests. Both are now obstacles. The Docker engine doesn't
support any Firecracker-specific features (thermal management, snapshots,
config drives, bridge networks). Server tests that use Docker are slow,
flaky, and test the wrong engine. Four of the server test "failures" from
Part 1 are Docker-specific limitations, not auth bugs.

The test architecture splits into two layers:

| Layer | What it tests | Engine | Runs on |
|-------|--------------|--------|---------|
| `pkg/engine/firecracker/` | VM lifecycle, exec, snapshot, files, thermal, network | Real Firecracker | agni-01 |
| `pkg/server/` | HTTP routing, auth, scoping, validation, error codes | Mock | Anywhere |

The Go compiler enforces the mock ↔ interface contract: if `engine.Engine`
changes a method signature, the mock won't compile. The mock is kept
minimal — a map of sandboxes with status tracking, canned exec results —
so there's nothing to go stale behaviorally.

### 1b.1 Delete Docker Engine

**Delete entirely:**
- `pkg/engine/docker/docker.go`
- `pkg/engine/docker/docker_test.go`
- `pkg/engine/docker/parse_test.go`

**Update:**
- `cmd/bhatti/main.go` — remove `docker.New()` case, remove import
- `cmd/bhatti/engine_other.go` — return clear error:
  `"bhatti requires Linux with KVM (firecracker engine only)"`
- `pkg/config.go` — change default engine from `"docker"` to `"firecracker"`
- `go.mod` — `go mod tidy` to remove `github.com/docker/docker` and its
  transitive dependencies
- `Dockerfile.sandbox` — move to `docs/archive/` (Docker sandbox image is
  replaced by rootfs built via `build-rootfs.sh`)
- `Makefile` — remove `sandbox` target

### 1b.2 Mock Engine for Server Tests

**File:** `pkg/server/mock_engine_test.go`

A minimal `engine.Engine` implementation for server tests. Not a
sophisticated fake — just enough to let the HTTP layer exercise its logic.

```go
type mockEngine struct {
    mu         sync.Mutex
    sandboxes  map[string]*engine.SandboxInfo
    execResult engine.ExecResult  // configurable per-test
}

func newMockEngine() *mockEngine {
    return &mockEngine{
        sandboxes:  make(map[string]*engine.SandboxInfo),
        execResult: engine.ExecResult{ExitCode: 0, Stdout: "mock\n"},
    }
}
```

Key behaviors:
- `Create` — stores sandbox in map, returns SandboxInfo with status "running"
- `Exec` — returns `m.execResult` (configurable per test)
- `Shell` — returns a `net.Pipe()` wrapped as TerminalConn
- `Destroy` — removes from map
- `Stop/Start` — toggles status
- `Status` — returns from map, error if not found
- `List` — returns all sandboxes
- `ListeningPorts` — returns empty slice
- `Tunnel` — returns a `net.Pipe()`

The mock does NOT implement `ThermalEngine`, `FileEngine`,
`StreamExecEngine`, or `VMStateProvider`. Server tests that need those
interfaces test against real Firecracker on agni-01.

### 1b.3 Update Server Test Setup

Replace `dockerengine.New()` with `newMockEngine()` in `setup()` and
`setupTwoUsers()`. Remove `skipIfNoDocker()`, `ensureAlpinePulled()`,
and all Docker cleanup helpers. Server tests become:
- Fast (~milliseconds per test, not seconds)
- Deterministic (no container startup races)
- Runnable anywhere (CI, Mac, Linux, no Docker needed)

### 1b.4 Fix Bug: Secrets Table Primary Key

The `secrets` table has `name TEXT PRIMARY KEY`. Two users can't have a
secret with the same name. `SetSecret` silently loses data when names
collide across users.

**Fix:** Recreate the table with composite primary key `(user_id, name)`:

```sql
-- In store.New(), after running migrations:
CREATE TABLE IF NOT EXISTS secrets_v2 (
    user_id TEXT NOT NULL DEFAULT '',
    name TEXT NOT NULL,
    path TEXT NOT NULL DEFAULT '',
    value_encrypted BLOB DEFAULT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, name)
);
INSERT OR IGNORE INTO secrets_v2 (user_id, name, path, value_encrypted, created_at, updated_at)
    SELECT COALESCE(user_id, ''), name, path, value_encrypted,
           created_at, COALESCE(updated_at, created_at) FROM secrets;
DROP TABLE IF EXISTS secrets;
ALTER TABLE secrets_v2 RENAME TO secrets;
```

Update `SetSecret` upsert to conflict on `(user_id, name)`:

```go
INSERT INTO secrets (user_id, name, path, value_encrypted, created_at, updated_at)
VALUES (?, ?, '', ?, ?, ?)
ON CONFLICT(user_id, name) DO UPDATE SET
    value_encrypted = excluded.value_encrypted,
    updated_at = excluded.updated_at
```

### 1b.5 Fix Bug: Sandbox Name Collision Wastes VM Boot

Check for name conflicts BEFORE calling `engine.Create()`. Return 409
with a clean message instead of booting a VM and then destroying it.

```go
// In handleSandboxes POST, after name validation, before engine.Create():
if spec.Name != "" {
    existing, _ := s.store.ListSandboxes(user.ID)
    for _, sb := range existing {
        if sb.Name == spec.Name && sb.Status != "destroyed" {
            errResp(w, 409, fmt.Sprintf("sandbox %q already exists", spec.Name))
            return
        }
    }
}
```

### 1b.6 Testing

All existing Part 1 tests must pass plus:

Store:
- `TestTwoUsersCreateSameSecretName` — Alice and Bob both create "API_KEY",
  each sees their own value (was failing, now passes)
- `TestDeleteSandboxByID` — unscoped delete works
- `TestUserDuplicateNameRejected` — UNIQUE constraint on name
- `TestUserDuplicateKeyHashRejected` — UNIQUE constraint on hash

Server (all using mock engine, no Docker):
- `TestCrossUserSandboxIsolation` — user B gets 404 on user A's sandbox
- `TestCrossUserExecIsolation` — user B gets 404 on exec/stop
- `TestSandboxResourceCaps` — CPU/memory over limit → 400
- `TestSandboxCountLimit` — create up to max, next → 429
- `TestSandboxNameValidationHTTP` — invalid names → 400
- `TestDuplicateSandboxNameHTTP` — same name twice → 409 (not 500)
- `TestPathCleanAuthBypass` — path tricks don't bypass auth
- `TestCrossUserSecretIsolationHTTP` — user B can't see/delete user A's secrets

---

## Part 2 — Per-User Bridge Networks

Each user gets their own bridge device and /24 subnet. VMs from different
users are on physically separate L2 segments. Combined with 5 global
iptables rules, this provides complete network isolation.

### 2.1 Subnet Allocation

**Scheme:** `10.{hi}.{lo}.0/24`

```
User subnet_index=1:   10.0.1.0/24   bridge brbhatti-1   gateway 10.0.1.1
User subnet_index=2:   10.0.2.0/24   bridge brbhatti-2   gateway 10.0.2.1
...
User subnet_index=254: 10.0.254.0/24 bridge brbhatti-254 gateway 10.0.254.1
User subnet_index=255: 10.1.0.0/24   bridge brbhatti-255 gateway 10.1.0.1
...
Max: 10.255.254.0/24 → 65,024 users, 253 VMs each
```

**File:** `pkg/engine/firecracker/network.go`

```go
// UserNetwork holds the network state for a single user.
type UserNetwork struct {
    BridgeName string
    Subnet     string   // "10.X.Y.0/24"
    GatewayIP  string   // "10.X.Y.1"
    Pool       *ipPool  // per-user IP allocation (.2-.254)
}

// subnetFromIndex converts a 1-based subnet index to network parameters.
func subnetFromIndex(index int) (gateway, subnet, bridge string) {
    // index 1 → 10.0.1.0/24
    // index 254 → 10.0.254.0/24
    // index 255 → 10.1.0.0/24
    hi := (index - 1) / 254
    lo := ((index - 1) % 254) + 1
    gateway = fmt.Sprintf("10.%d.%d.1", hi, lo)
    subnet = fmt.Sprintf("10.%d.%d.0/24", hi, lo)
    bridge = fmt.Sprintf("brbhatti-%d", index)
    return
}
```

### 2.2 Bridge Lifecycle

Bridges are created lazily (first sandbox for a user) and destroyed when
the user's last sandbox is destroyed. "Last sandbox" means last sandbox
in **any** state — running, stopped, or cold. A cold sandbox still has
files on disk (rootfs, snapshots) and its TAP device is referenced in the
snapshot state. The bridge must persist until the user has zero sandboxes.

```go
// ensureUserBridge creates the bridge if it doesn't exist.
// Idempotent — safe to call on every sandbox creation.
func ensureUserBridge(net *UserNetwork) error {
    // Create bridge (ignore "already exists" error)
    runQuiet("ip", "link", "add", net.BridgeName, "type", "bridge")
    runQuiet("ip", "addr", "add", net.GatewayIP+"/24", "dev", net.BridgeName)
    if err := run("ip", "link", "set", net.BridgeName, "up"); err != nil {
        return fmt.Errorf("bring up bridge %s: %w", net.BridgeName, err)
    }
    return nil
}

// destroyUserBridge removes the bridge device.
// Called from Destroy() after confirming zero remaining sandboxes for the user.
func destroyUserBridge(bridgeName string) {
    run("ip", "link", "del", bridgeName)
}
```

The check in `Destroy()`:

```go
// After destroying the sandbox, check if user has any remaining sandboxes
remaining, _ := s.store.CountSandboxes(user.ID)
if remaining == 0 {
    destroyUserBridge(userNet.BridgeName)
    delete(e.userNetworks, user.ID)
}
```

`CountSandboxes` counts all non-destroyed sandboxes regardless of status
(running, stopped, cold). This ensures the bridge is never pulled out from
under a cold sandbox whose snapshot references a TAP on that bridge.
```

### 2.3 Global Iptables Rules

Set once at engine startup. 6 rules total, regardless of user/VM count:

```go
// setupGlobalFirewall configures isolation rules for all VM traffic.
// Called once from Engine.New(). Idempotent.
func setupGlobalFirewall() error {
    defaultIface := detectDefaultInterface()

    rules := []struct {
        table string
        chain string
        args  []string
    }{
        // 1. Block cross-bridge routing (user A's VMs cannot reach user B's VMs)
        {"filter", "FORWARD", []string{"-s", "10.0.0.0/8", "-d", "10.0.0.0/8", "-j", "DROP"}},

        // 2. Allow VM → internet
        {"filter", "FORWARD", []string{"-s", "10.0.0.0/8", "!", "-d", "10.0.0.0/8", "-j", "ACCEPT"}},

        // 3. Allow return traffic from internet → VM
        {"filter", "FORWARD", []string{"-d", "10.0.0.0/8", "-m", "state",
            "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"}},

        // 4. Allow return traffic from VMs to host (agent TCP responses).
        //
        // The bhatti daemon initiates TCP connections to VMs (net.Dial to
        // 10.X.Y.Z:1024). The kernel picks the bridge IP as source. The
        // SYN-ACK from the VM enters the INPUT chain with source 10.0.0.0/8.
        // Without this ACCEPT rule, a blanket DROP would kill every agent
        // connection — exec, file ops, sessions, health checks all fail.
        //
        // This MUST come before rule 5 (the DROP). conntrack ensures only
        // response packets on host-initiated connections are accepted.
        {"filter", "INPUT", []string{"-s", "10.0.0.0/8", "-m", "state",
            "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"}},

        // 5. Block VM-initiated connections to host (API, SSH, everything).
        //
        // Only NEW connections from VMs are dropped. This prevents a
        // compromised VM from connecting to the bhatti API (port 8080),
        // SSH (port 22), or any other host service. Combined with rule 4,
        // the host can maintain agent connections while VMs cannot initiate
        // new connections to the host.
        {"filter", "INPUT", []string{"-s", "10.0.0.0/8", "-m", "state",
            "--state", "NEW", "-j", "DROP"}},

        // 6. NAT for outbound
        {"nat", "POSTROUTING", []string{"-s", "10.0.0.0/8", "-o", defaultIface,
            "-j", "MASQUERADE"}},
    }

    for _, r := range rules {
        // Check if rule exists first (-C), add if not (-A or -I)
        checkArgs := append([]string{"-t", r.table, "-C", r.chain}, r.args...)
        if runQuiet("iptables", checkArgs...) != nil {
            addArgs := append([]string{"-t", r.table, "-A", r.chain}, r.args...)
            if err := run("iptables", addArgs...); err != nil {
                return fmt.Errorf("iptables rule %v: %w", r.args, err)
            }
        }
    }

    // Enable IP forwarding
    os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644)
    return nil
}
```

**Rule ordering matters for INPUT.** Rule 4 (ACCEPT ESTABLISHED) must be
evaluated before rule 5 (DROP NEW). Since both are appended with `-A`, they
are evaluated in insertion order. If the chain already has rules from a
previous run, the idempotency check (`-C`) ensures we don't duplicate them.

**Why same-user VMs can talk to each other:** Traffic between VMs on the
same bridge (e.g., `10.0.1.2` → `10.0.1.3`) stays at L2 — it's switched by
the bridge, never enters the FORWARD chain. Rule 1 only blocks cross-bridge
traffic that would go through the host's routing table. This is a feature:
a user can run a frontend sandbox and a backend sandbox that communicate.

**Why VMs can't initiate connections to the host:** Rule 5 drops NEW
connections from `10.0.0.0/8` in the INPUT chain. VMs use `1.1.1.1` and
`8.8.8.8` for DNS (injected via kernel cmdline). They have no legitimate
reason to initiate connections to the host. This prevents a leaked API key
inside a sandbox from being used to control the host API. Rule 4 allows the
return path for host-initiated agent connections (exec, file ops, etc.).

### 2.4 TAP Device Creation Changes

TAP devices join the user's bridge instead of a global bridge:

```go
func createTapDevice(sandboxID string, bridge string) (tapName string, err error) {
    tapName = "tap" + sandboxID[:8]
    if err := run("ip", "tuntap", "add", tapName, "mode", "tap"); err != nil {
        return "", fmt.Errorf("create tap: %w", err)
    }
    if err := run("ip", "link", "set", tapName, "master", bridge); err != nil {
        run("ip", "link", "del", tapName)
        return "", fmt.Errorf("add to bridge %s: %w", bridge, err)
    }
    if err := run("ip", "link", "set", tapName, "up"); err != nil {
        run("ip", "link", "del", tapName)
        return "", fmt.Errorf("bring up tap: %w", err)
    }
    return tapName, nil
}
```

### 2.5 Engine Changes

The engine needs to know which user a sandbox belongs to, so it can allocate
from the correct network.

```go
// Engine.userNetworks maps user ID → UserNetwork
type Engine struct {
    mu           sync.RWMutex
    vms          map[string]*VM
    cfg          Config
    nextCID      uint32
    userNetworks map[string]*UserNetwork  // userID → network
}
```

`Create()` accepts the user's subnet index (passed from the server layer):

```go
func (e *Engine) Create(ctx context.Context, spec engine.SandboxSpec) (engine.SandboxInfo, error) {
    // ... existing code ...

    // Get or create user's network
    net := e.getOrCreateUserNetwork(spec.UserID, spec.SubnetIndex)
    ensureUserBridge(net)
    guestIP, err := net.Pool.Allocate()
    tapName, err := createTapDevice(id, net.BridgeName)

    // Boot args use user's gateway instead of hardcoded bridge IP
    bootArgs := fmt.Sprintf(
        "... ip=%s::%s:255.255.255.0::eth0:off:1.1.1.1:8.8.8.8:",
        guestIP, net.GatewayIP)
    // ...
}
```

`SandboxSpec` gets two new fields:

```go
type SandboxSpec struct {
    // ... existing fields ...
    UserID      string `json:"-"`  // set by server, not by API client
    SubnetIndex int    `json:"-"`  // set by server from user record
}
```

### 2.6 Kernel Boot Args Update

The kernel `ip=` parameter uses the user's gateway:

```
ip=10.0.1.2::10.0.1.1:255.255.255.0::eth0:off:1.1.1.1:8.8.8.8:
```

### 2.7 Recovery

On startup, reconstruct `userNetworks` from stored sandbox state:

```go
func (e *Engine) recoverUserNetworks(st *store.Store) {
    sandboxes, _ := st.ListSandboxes("") // admin query for recovery
    for _, sb := range sandboxes {
        user, _ := st.GetUser(sb.CreatedBy)
        if user == nil { continue }
        e.getOrCreateUserNetwork(user.ID, user.SubnetIndex)
        // Mark IPs from existing sandboxes as used
        if sb.IP != "" {
            net := e.userNetworks[user.ID]
            net.Pool.Mark(sb.IP)
        }
    }
}
```

### 2.8 Testing

- `TestSubnetFromIndex` — verify index→subnet mapping for indices
  1, 254, 255, 508, 65024
- `TestUserBridgeLifecycle` — create bridge, verify it exists, destroy,
  verify it's gone
- `TestCrossBridgeIsolation` — create 2 users, create a sandbox each,
  exec `ping -c1 -W1 <other_vm_ip>` from each, verify failure (ICMP
  unreachable or timeout)
- `TestSameBridgeCommunication` — create 2 sandboxes for same user, exec
  `ping -c1 -W1 <other_vm_ip>`, verify success
- `TestVMCannotReachHost` — exec `curl -s --connect-timeout 2
  http://10.0.1.1:8080/health` from a VM, verify failure (connection refused
  or timeout, not a 200)
- `TestVMCanReachInternet` — exec `curl -s --connect-timeout 5
  https://httpbin.org/ip` from a VM, verify 200

---

## Part 3 — Proxy Cleanup

Remove the TCP auto-forward system. Keep the authenticated HTTP reverse proxy.

### 3.1 Remove TCP Auto-Forward

**Delete entirely:**
- `startPortScanner()` goroutine in `server.go`
- `ProxyManager.Forward()` method
- `ForwardEntry` struct's `listener`, `cancel` fields
- `ProxyManager.accept()` method
- The `stopScan` field and its cleanup in `Server.Close()`

**Keep:**
- `GET /sandboxes/:id/ports` — queries the VM for listening ports as metadata.
  Returns the list but does NOT create host listeners. Remove the `host_port`
  field from the response.
- `/sandboxes/:id/proxy/:port/...` — authenticated HTTP/WS reverse proxy.
  This is the correct way for external clients to reach sandbox services.
- `ProxyManager` can be reduced to just tracking metadata (which ports are
  known) or removed entirely if the port list endpoint just queries the VM
  directly on each call.

**File changes:**

`pkg/server/server.go`:
- Remove `startPortScanner`, `scanPorts`, `stopScan` field
- Remove the `s.startPortScanner(3 * time.Second)` call from `New()`

`pkg/server/proxy.go`:
- Remove `Forward`, `accept`, `StopForward`, `StopAll`, `AllForwards`
- Or delete the entire file if port metadata tracking is moved inline

`pkg/server/routes.go`:
- `handleSandboxPorts` — remove `HostPort` from response, remove forward
  map lookup. Just return `[{container_port, proxy_url}]`.
- Remove `handleAllPorts` or simplify to just list known ports per sandbox.
- Remove all `s.proxy.StopAll(id)` calls from destroy/stop handlers.

### 3.2 Port Response Simplification

The ports endpoint becomes:

```json
GET /sandboxes/:id/ports

[
  {"container_port": 3000, "proxy_url": "/sandboxes/abc123/proxy/3000/"},
  {"container_port": 8080, "proxy_url": "/sandboxes/abc123/proxy/8080/"}
]
```

No `host_port`. The only way to reach a sandbox service from outside is
through the authenticated proxy URL.

### 3.3 Testing

- `TestProxyHTTP` — start a web server in sandbox, access via
  `/sandboxes/:id/proxy/:port/`, verify response
- `TestProxyWS` — start a WS server in sandbox, connect via proxy, verify
  bidirectional communication
- `TestNoDirectAccess` — verify no host ports are opened for sandbox services.
  After creating a sandbox and starting a server inside it, scan host ports
  to confirm nothing new is listening.

---

## Part 4 — Guest Agent Hardening

### 4.1 Exec as lohar (uid 1000), Not Root

**File:** `cmd/lohar/exec.go`

Add `Credential` to `SysProcAttr` in `handlePipedExec`:

```go
cmd.SysProcAttr = &syscall.SysProcAttr{
    Setpgid: true,
    Credential: &syscall.Credential{
        Uid: 1000,
        Gid: 1000,
    },
}
```

**File:** `cmd/lohar/tty.go`

Same for `handleTTYSession`:

```go
cmd.SysProcAttr = &syscall.SysProcAttr{
    Setsid:  true,
    Setctty: true,
    Ctty:    0,
    Credential: &syscall.Credential{
        Uid: 1000,
        Gid: 1000,
    },
}
```

The user `lohar` already exists in the rootfs with uid 1000, has sudo
with NOPASSWD, and owns `/workspace`. This is the correct default. If a
user needs root, they use `sudo` — the sudoers entry already allows it.

The init session already handles this via the `User` field in config. No
change needed there.

### 4.2 Unmount Config Drive After Boot

**File:** `cmd/lohar/main.go`

After `loadConfigDrive()` returns, unmount and remove:

```go
cfg := loadConfigDrive()
if cfg != nil {
    // ... apply config ...

    // Unmount config drive — it contains the agent token and env vars
    // in plaintext JSON. No reason to keep it accessible.
    syscall.Unmount("/run/bhatti/config", 0)
    os.RemoveAll("/run/bhatti/config")
}
```

### 4.3 Connection and Session Limits

**File:** `cmd/lohar/handler.go`

Add limits at the top of `handleControlConnection`:

```go
const maxConcurrentConns = 50
const maxActiveSessions = 20

var activeConns atomic.Int32

func handleControlConnection(conn net.Conn) {
    if activeConns.Add(1) > maxConcurrentConns {
        activeConns.Add(-1)
        proto.WriteFrame(conn, proto.ERROR, []byte("connection limit exceeded"))
        conn.Close()
        return
    }
    defer activeConns.Add(-1)
    defer conn.Close()

    // ... existing auth + dispatch ...
}
```

In session creation (`newSession` in `session.go`):

```go
func newSession(argv []string, tty bool, maxIdle time.Duration) *Session {
    registry.Lock()
    defer registry.Unlock()
    if len(registry.sessions) >= maxActiveSessions {
        return nil // caller checks for nil and returns error
    }
    // ...
}
```

### 4.4 File Write Size Limit

**File:** `cmd/lohar/files.go`

```go
const maxWriteSize = 100 << 20 // 100 MB

func handleFileWrite(conn net.Conn, payload []byte) {
    // ...
    if req.Size > maxWriteSize {
        proto.WriteFrame(conn, proto.ERROR,
            []byte(fmt.Sprintf("file too large: %d bytes (max %d)", req.Size, maxWriteSize)))
        return
    }
    // ...
}
```

**Note on disk storage limits:** The 100MB cap applies to the file API path
only. A user can bypass this via exec (`dd if=/dev/zero of=/bigfile ...`).
The real disk limit is the **ext4 image size** — `build-rootfs.sh` creates
a 2048MB image, and `copyRootfs` (whether `cp --reflink=always` or plain
`cp`) preserves the filesystem size boundary. Writes inside the VM get
ENOSPC when the 2GB rootfs is full. Volumes have their own size limits set
at creation time via `size_mb`.

This is the correct design: the ext4 image is the disk quota. The API file
write cap is defense-in-depth preventing a single API call from filling the
filesystem. Document for users: "each sandbox has a 2GB root filesystem."

### 4.5 Zombie Reaping — Explicitly Not Done

**Decision:** Keep the current no-reaper design. Do not add a `Wait4(-1)`
reaper.

The existing code comment explains the reasoning correctly:

```
// Note: we do NOT install a SIGCHLD handler. Go's runtime manages
// SIGCHLD for processes started via exec.Command. A manual Wait4(-1)
// reaper would race with cmd.Wait() and corrupt exit codes.
// Orphan zombies (from grandchild processes) are acceptable for now.
```

**Why a generic reaper breaks piped exec:** For piped exec (the primary
code path agents use), the exit code comes from `cmd.Wait()`:

```go
exitCode := exitCodeFromErr(cmd.Wait())
```

If a reaper calls `Wait4(-1, WNOHANG)` and reaps the child PID before
`cmd.Wait()` does, `cmd.Wait()` gets ECHILD. `exitCodeFromErr` treats
this as exit code 1. A process that exited successfully (code 0) gets
reported as code 1. This silently corrupts exit codes — an `npm test`
that passed gets reported as failed.

This race is **not** harmless for piped exec. It only appears harmless
for TTY sessions, where the exit code is captured by the PTY reader
goroutine independently of `cmd.Wait()`.

**Why zombies are acceptable:** Orphan zombies only accumulate from
grandchild processes that outlive their parent and get reparented to
PID 1. In the coding agent workload (short-lived sequential commands),
these are rare. The VM is ephemeral — zombies are cleaned up on destroy.

**Future option:** If zombie accumulation becomes observable on long-lived
VMs (check via `ps aux | grep defunct`), the correct fix is a PID-tracking
reaper that only reaps PIDs not managed by `cmd.Wait()`:

```go
var trackedPIDs sync.Map // pid → bool
// Register in handlePipedExec after cmd.Start():
trackedPIDs.Store(cmd.Process.Pid, true)
defer trackedPIDs.Delete(cmd.Process.Pid)
// Reaper skips tracked PIDs:
if _, tracked := trackedPIDs.Load(pid); !tracked { /* safe to reap */ }
```

This adds complexity for marginal benefit. Defer until there's evidence
of the problem in production.

### 4.6 Constant-Time Token Comparison in Agent

**File:** `cmd/lohar/handler.go`

```go
import "crypto/subtle"

// In handleControlConnection:
if agentToken != "" {
    conn.SetReadDeadline(time.Now().Add(5 * time.Second))
    msgType, payload, err := proto.ReadFrame(conn)
    conn.SetReadDeadline(time.Time{})
    if err != nil || msgType != proto.AUTH ||
        subtle.ConstantTimeCompare([]byte(payload), []byte(agentToken)) != 1 {
        proto.WriteFrame(conn, proto.ERROR, []byte("auth required"))
        return
    }
}
```

Same change in `cmd/lohar/forward.go`.

### 4.7 Testing

- `TestExecRunsAsLohar` — exec `whoami`, verify output is `lohar`, not `root`
- `TestExecCanSudo` — exec `sudo whoami`, verify output is `root`
- `TestConfigDriveUnmounted` — exec `cat /run/bhatti/config/config.json`,
  verify error (no such file or directory)
- `TestSessionLimit` — create 20 sessions, verify 21st returns error
- `TestFileWriteSizeLimit` — attempt to write 200MB file, verify error
- `TestDiskLimit` — exec `dd if=/dev/zero of=/bigfile bs=1M count=3000`,
  verify it fails with ENOSPC (rootfs is 2GB)

---

## Part 5 — Secrets Encryption

Wire up the existing `pkg/secrets/age.go` package. The infrastructure exists
but is not connected.

### 5.1 Encrypt on Write, Decrypt on Read

**File:** `pkg/server/routes.go`

In `handleSecrets` POST:

```go
case http.MethodPost:
    // ...
    identity, recipient, err := secrets.EnsureKey(
        filepath.Join(s.dataDir, "age.key"))
    if err != nil { ... }

    encrypted, err := secrets.Encrypt([]byte(req.Value), recipient)
    if err != nil { ... }

    if err := s.store.SetSecret(user.ID, req.Name, encrypted); err != nil { ... }
```

In sandbox creation, when resolving secrets:

```go
encrypted, err := s.store.GetSecretValue(user.ID, secretName)
identity, _, _ := secrets.EnsureKey(filepath.Join(s.dataDir, "age.key"))
plaintext, err := secrets.Decrypt(encrypted, identity)
envMap[secretName] = string(plaintext)
```

### 5.2 Age Key Backup Documentation

The `age.key` file at `/var/lib/bhatti/age.key` is the master decryption
key. If lost, all encrypted secrets are unrecoverable.

Add to install script output:

```
⚠  BACK UP THIS FILE: /var/lib/bhatti/age.key
   If lost, all encrypted secrets become unrecoverable.
```

### 5.3 Testing

- `TestSecretEncryptDecrypt` — set a secret, verify stored bytes are not
  plaintext, retrieve and verify decrypted value matches original
- `TestSecretWithMissingScopeKey` — user A cannot read user B's secrets

---

## Part 6 — API Hardening

### 6.1 Rate Limiting

**File:** `pkg/server/server.go`

Per-user token bucket. Different limits for different operation classes:

```go
type rateLimiter struct {
    mu      sync.Mutex
    buckets map[string]*tokenBucket // userID → bucket
}

type tokenBucket struct {
    create  *bucket  // 10/min  (sandbox creation)
    exec    *bucket  // 120/min (exec, file ops)
    read    *bucket  // 600/min (list, get, ports)
}
```

Check in `ServeHTTP` after user lookup, before dispatching:

```go
if !s.limiter.Allow(user.ID, classifyRequest(r)) {
    errResp(w, 429, "rate limit exceeded")
    return
}
```

`classifyRequest` returns `"create"`, `"exec"`, or `"read"` based on
method and path.

### 6.2 Sandbox Name Validation

**File:** `pkg/server/routes.go`

In `handleSandboxes` POST:

```go
var validName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)

if spec.Name != "" && !validName.MatchString(spec.Name) {
    errResp(w, 400, "invalid sandbox name: must match [a-zA-Z0-9][a-zA-Z0-9._-]{0,62}")
    return
}
```

### 6.3 Exec Timeout

**File:** `pkg/server/routes.go`

Default 300 seconds, overridable per request:

```go
type execReq struct {
    Cmd        []string `json:"cmd"`
    TimeoutSec int      `json:"timeout_sec,omitempty"` // default 300, max 3600
}

func (s *Server) handleSandboxExec(w http.ResponseWriter, r *http.Request, id string) {
    // ...
    timeout := 300 * time.Second
    if req.TimeoutSec > 0 && req.TimeoutSec <= 3600 {
        timeout = time.Duration(req.TimeoutSec) * time.Second
    }
    execCtx, cancel := context.WithTimeout(r.Context(), timeout)
    defer cancel()

    result, err := s.engine.Exec(execCtx, sb.EngineID, req.Cmd)
    // ...
}
```

### 6.4 Buffered Exec Output Cap

**File:** `pkg/agent/client.go`

Cap total buffered output at 10MB:

```go
const maxBufferedOutput = 10 << 20 // 10 MB

func (c *AgentClient) Exec(ctx context.Context, argv []string, ...) (engine.ExecResult, error) {
    // ...
    var stdout, stderr bytes.Buffer
    var totalBytes int

    for {
        msgType, payload, err := proto.ReadFrame(conn)
        // ...
        switch msgType {
        case proto.STDOUT:
            if totalBytes+len(payload) <= maxBufferedOutput {
                stdout.Write(payload)
                totalBytes += len(payload)
            }
        case proto.STDERR:
            if totalBytes+len(payload) <= maxBufferedOutput {
                stderr.Write(payload)
                totalBytes += len(payload)
            }
        case proto.EXIT:
            result := engine.ExecResult{
                ExitCode: int(exitCode),
                Stdout:   stdout.String(),
                Stderr:   stderr.String(),
            }
            if totalBytes >= maxBufferedOutput {
                result.Stderr += "\n[output truncated at 10MB]"
            }
            return result, nil
        }
    }
}
```

The streaming NDJSON path (`ExecStream`) is unaffected — it forwards frames
as they arrive without buffering.

### 6.5 Testing

- `TestRateLimiting` — send 15 sandbox creation requests in 1 second,
  verify the last few return 429
- `TestNameValidation` — valid names pass, names with newlines/spaces/
  special chars return 400
- `TestExecTimeout` — exec `sleep 600` with `timeout_sec: 2`, verify
  it returns within ~2 seconds with a timeout error
- `TestExecOutputCap` — exec `dd if=/dev/zero bs=1M count=20 | base64`,
  verify output is truncated at ~10MB

---

## Part 7 — Observability

### 7.1 JSON Structured Logging

**File:** `cmd/bhatti/main.go`

At the top of `runDaemon()`:

```go
logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
    Level: slog.LevelInfo,
}))
slog.SetDefault(logger)
```

### 7.2 Request Logging Middleware

**File:** `pkg/server/server.go`

Wrap the mux dispatch in `ServeHTTP`:

```go
start := time.Now()
reqID := generateRequestID()

wrapped := &statusWriter{ResponseWriter: w, status: 200}
s.mux.ServeHTTP(wrapped, r.WithContext(
    context.WithValue(ctx, requestIDKey, reqID)))

slog.Info("request",
    "request_id", reqID,
    "method", r.Method,
    "path", cleanPath,
    "status", wrapped.status,
    "duration_ms", time.Since(start).Milliseconds(),
    "user", user.Name,
    "user_id", user.ID,
)
```

### 7.3 Lifecycle Logging

**File:** `pkg/server/routes.go`

After successful operations:

```go
// Sandbox created
slog.Info("sandbox.created",
    "sandbox_id", sb.ID, "name", sb.Name,
    "user", user.Name, "cpus", spec.CPUs, "memory_mb", spec.MemoryMB)

// Sandbox destroyed
slog.Info("sandbox.destroyed", "sandbox_id", sb.ID, "name", sb.Name, "user", user.Name)

// Auth failure
slog.Warn("auth.failed", "ip", r.RemoteAddr, "reason", "invalid api key")
```

### 7.4 Error Sanitization

**File:** `pkg/server/routes.go`

Replace raw error messages with generic responses + request IDs:

```go
func errRespInternal(w http.ResponseWriter, reqID string, logMsg string, err error) {
    slog.Error(logMsg, "request_id", reqID, "error", err)
    writeJSON(w, 500, map[string]string{
        "error":      "internal error",
        "request_id": reqID,
    })
}
```

Client-facing errors remain specific for 400-level (bad input):

```go
errResp(w, 400, "invalid sandbox name")     // this is fine — user input error
errRespInternal(w, reqID, "create failed", err)  // this hides internals
```

### 7.5 Metrics Endpoint

**File:** `pkg/server/routes.go`

`GET /metrics` (no auth, like `/health`):

```json
{
  "uptime": "4h23m",
  "sandboxes": {"total": 47, "hot": 5, "warm": 12, "cold": 30},
  "users": {"total": 8, "active": 3},
  "host": {
    "cpu_cores": 24,
    "memory_total_mb": 128000,
    "memory_available_mb": 94000,
    "disk_total_gb": 1800,
    "disk_used_gb": 120,
    "load_1m": 2.4
  },
  "requests": {
    "total": 12847,
    "errors_5xx": 3,
    "auth_failures": 17
  }
}
```

Read from `/proc/meminfo`, `/proc/loadavg`, `df`. Request counters from
atomic int64s in the logging middleware.

### 7.6 Testing

- `TestRequestLogging` — make a request, verify structured JSON log output
  contains method, path, status, user, request_id
- `TestErrorSanitization` — trigger a 500 error, verify response contains
  `request_id` but not internal paths or stack traces
- `TestMetricsEndpoint` — verify `/metrics` returns valid JSON with
  expected fields, requires no auth

---

## Part 8 — Release

### 8.1 Version Injection

**File:** `cmd/bhatti/main.go`

```go
var version = "dev"

// In runCLI():
case "version":
    fmt.Printf("bhatti %s\n", version)
    fmt.Printf("api: %s\n", apiURL)
```

**File:** `Makefile`

```makefile
VERSION ?= $(shell git describe --tags --always --dirty)

build:
	go build -ldflags="-s -w -X main.version=$(VERSION)" -o bhatti ./cmd/bhatti/
```

### 8.2 Cross-Compile Matrix

```makefile
release:
	@mkdir -p dist
	GOOS=darwin GOARCH=arm64 go build -ldflags="-s -w -X main.version=$(VERSION)" \
		-o dist/bhatti-darwin-arm64 ./cmd/bhatti/
	GOOS=darwin GOARCH=amd64 go build -ldflags="-s -w -X main.version=$(VERSION)" \
		-o dist/bhatti-darwin-amd64 ./cmd/bhatti/
	GOOS=linux GOARCH=amd64 go build -ldflags="-s -w -X main.version=$(VERSION)" \
		-o dist/bhatti-linux-amd64 ./cmd/bhatti/
	GOOS=linux GOARCH=arm64 go build -ldflags="-s -w -X main.version=$(VERSION)" \
		-o dist/bhatti-linux-arm64 ./cmd/bhatti/
```

### 8.3 Git Tag

```bash
git tag v0.1.0
git push --tags
# Create GitHub Release, upload the 4 binaries from dist/
```

### 8.4 CLI Install Script

Host at `https://bhatti.sh/install.sh` (Cloudflare Pages or R2):

```bash
#!/bin/bash
set -euo pipefail
VERSION="${BHATTI_VERSION:-latest}"
REPO="sahil-shubham/bhatti"
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
    x86_64)        ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *) echo "unsupported: $ARCH" >&2; exit 1 ;;
esac
if [ "$VERSION" = "latest" ]; then
    VERSION=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
        | grep tag_name | cut -d'"' -f4)
fi
URL="https://github.com/$REPO/releases/download/${VERSION}/bhatti-${OS}-${ARCH}"
echo "Installing bhatti $VERSION ($OS/$ARCH)..."
curl -fsSL "$URL" -o /tmp/bhatti && chmod +x /tmp/bhatti
INSTALL_DIR="/usr/local/bin"
if [ -w "$INSTALL_DIR" ]; then mv /tmp/bhatti "$INSTALL_DIR/bhatti"
else sudo mv /tmp/bhatti "$INSTALL_DIR/bhatti"; fi
echo "bhatti $VERSION installed"
```

### 8.5 CLI User Setup Command

```bash
$ bhatti setup
API endpoint [https://api.bhatti.sh]:
API key: ****
Saved to ~/.bhatti/config.yaml
Testing connection... ✓ connected (user: alice, 2 sandboxes)
```

---

## Deployment Verification

Run on agni-01 after all parts are implemented:

```bash
# 1. Create two test users
bhatti user create --name alice --max-sandboxes 5
bhatti user create --name bob --max-sandboxes 5

# 2. As alice: create sandbox, verify isolation
BHATTI_TOKEN=$ALICE_KEY bhatti create --name alice-dev
BHATTI_TOKEN=$ALICE_KEY bhatti exec alice-dev -- whoami
# → lohar (not root)

BHATTI_TOKEN=$ALICE_KEY bhatti exec alice-dev -- cat /run/bhatti/config/config.json
# → error: no such file (config drive unmounted)

# 3. As bob: verify cannot see alice's sandbox
BHATTI_TOKEN=$BOB_KEY bhatti list
# → empty

# 4. Network isolation
ALICE_IP=$(BHATTI_TOKEN=$ALICE_KEY bhatti exec alice-dev -- hostname -I | tr -d ' ')
BHATTI_TOKEN=$BOB_KEY bhatti create --name bob-dev
BHATTI_TOKEN=$BOB_KEY bhatti exec bob-dev -- ping -c1 -W1 $ALICE_IP
# → 100% packet loss (different bridges)

# 5. VM cannot reach host API
BHATTI_TOKEN=$ALICE_KEY bhatti exec alice-dev -- \
  curl -s --connect-timeout 2 http://10.0.1.1:8080/health
# → connection refused or timeout

# 6. VM can reach internet
BHATTI_TOKEN=$ALICE_KEY bhatti exec alice-dev -- \
  curl -s https://httpbin.org/ip
# → {"origin": "..."}

# 7. No TCP auto-forward ports exposed
ss -tlnp | grep bhatti
# → only :8080 (the API), nothing else

# 8. Rate limiting
for i in $(seq 20); do
  BHATTI_TOKEN=$ALICE_KEY bhatti create --name "spam-$i" 2>&1
done
# → last few return "rate limit exceeded" or "sandbox limit reached"

# 9. Exec timeout
BHATTI_TOKEN=$ALICE_KEY bhatti exec alice-dev -- sleep 600
# → times out after 300s (default)

# 10. Cleanup
BHATTI_TOKEN=$ALICE_KEY bhatti destroy alice-dev
BHATTI_TOKEN=$BOB_KEY bhatti destroy bob-dev
```

---

## Documented Non-Decisions

These items were evaluated and explicitly not done, with reasoning:

**TLS between host and guest agent.** The transport is a virtual TAP device
inside the host kernel. No physical wire to tap. An attacker with host root
access is outside the threat model. Adding TLS would add latency to every
operation for no meaningful security gain.

**Symlink restrictions in file API.** The file API operates with the same
privileges as processes inside the sandbox. It is not a security boundary
within the VM. The security boundary is between VMs (network isolation) and
between VMs and the host (Firecracker hypervisor). If path restrictions are
added later (e.g., only `/workspace`), symlink following must be revisited.

**Running the daemon as non-root.** Firecracker requires `/dev/kvm`, TAP
devices require `CAP_NET_ADMIN`, bridge management requires `CAP_NET_ADMIN`,
iptables requires `CAP_NET_ADMIN` + `CAP_NET_RAW`, mounting ext4 images
requires `CAP_SYS_ADMIN`. This is the same posture as Docker/containerd.
The attack surface is the HTTP API running as root. Mitigation: small API
surface, stdlib HTTP server, validated inputs.

**LD_PRELOAD / env injection restrictions.** Per-request `env` overrides are
a feature, not a bug. The sandbox is the user's environment — they should be
able to set any environment variable. The security boundary is between users
(API scoping + network isolation), not within a single user's sandbox.

**WebSocket auth via query parameter.** Removed. The bhatti API is
machine-to-machine. The CLI and all programmatic WebSocket clients (Go,
Python, Node, Rust) support custom headers on upgrade — they send
`Authorization: Bearer` in the header. The only client that can't set
headers on WebSocket upgrade is the browser's native `new WebSocket(url)`.
Since the web UI is a separate project, it should solve its own auth:
authenticate to its backend, get a short-lived single-use WebSocket ticket,
connect with `ws://...?ticket=xyz`. The ticket is validated once and
discarded. This is the standard pattern (Slack, Discord, etc.). Bhatti's
API should not weaken its auth model to accommodate browser limitations
that belong to a different project.

**Generic zombie reaper in guest agent.** A `Wait4(-1, WNOHANG)` reaper
races with `cmd.Wait()` in piped exec, corrupting exit codes (successful
processes reported as exit code 1). The existing no-reaper design is
correct. Orphan zombies from grandchild processes are rare in the coding
agent workload and cleaned up on sandbox destroy. See Part 4.5 for full
analysis and a PID-tracking reaper design to use if the problem manifests
in production.

---

## Capacity Reference

```
Server: Ryzen 9 3900 (12c/24t), 128GB ECC, 2×1.92TB NVMe RAID1

Default sandbox: 1 vCPU, 512MB RAM

Per-user allocation: 5 sandboxes, 1 vCPU each, 512MB each = 2.5GB/user
Max users (all warm):  ~50 (128GB / 2.5GB)
Max users (mix):       ~200 (most sandboxes cold, using only disk)
Disk per sandbox:      ~2GB rootfs + snapshots
Total disk capacity:   ~1.8TB usable → ~500 sandboxes with snapshots

Warning thresholds:
  RAM > 80% (>100GB used)     → alert
  Disk > 70% (>1.2TB used)    → alert
  Sandboxes > 400             → time for second node
```
