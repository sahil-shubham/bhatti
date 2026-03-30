# Reverse Proxy — Public Ingress for Bhatti

## Problem

Today, every sandbox port is only accessible through the authenticated API:

```
Browser → Cloudflare Tunnel → bhatti :8080 → /sandboxes/:id/proxy/:port/path → Engine.Tunnel() → lohar → localhost:port
```

This requires a valid API key. If a user runs a web app inside a sandbox
and wants to share it — hand someone a URL — they can't. The only path
is through the Cloudflare tunnel pointing at `:8080`, which demands
`Authorization: Bearer bht_...` on every request.

Beyond the proxy problem, the Cloudflare tunnel is itself an imposition.
Self-hosters shouldn't need a Cloudflare account, a tunnel daemon, or
any external service to expose bhatti to the internet. If you have a
domain and a public IP, bhatti should handle the rest.

**Goal:** Give bhatti a subdomain zone (e.g. `deploy.bhatti.sh`), and it
serves the API and public sandbox proxying directly. Published sandbox
ports get subdomains under the zone (`my-app.deploy.bhatti.sh`). TLS
certificates are automatic. No Cloudflare tunnel, no external reverse
proxy, no DNS-provider-specific configuration. The self-hoster's only
job is pointing DNS at the bhatti host.

The killer use case is **preview environments**. Deploy an app to a
bhatti sandbox, share the URL, and pay zero resources when nobody's
looking at it. Bhatti's thermal management snapshots the sandbox to
disk when idle, restores it in ~50ms when a request arrives, serves
the response, then sleeps again. Persistent deployments that cost
nothing at rest.

### Why Not Caddy / Nginx / Traefik in Front?

An external reverse proxy can't call `EnsureHot()`. Bhatti's first
external user (PR #2) built an external Caddy setup that routes
directly to VM bridge IPs, bypassing bhatti's tunnel. To handle wake,
they abused Caddy's `forward_auth` directive to hit `GET /sandboxes/:id`
as a side effect — which triggers `EnsureHot()` — before Caddy proxies
the real request. This has two problems:

1. **Race between wake and proxy.** `forward_auth` and `reverse_proxy`
   are separate TCP connections. Between them, the sandbox could be
   paused by the thermal manager, or the app might not be listening yet.
   The user gets a 502.

2. **Leaks the API key.** The bhatti API key must be baked into the Caddy
   config. Every public request triggers an authenticated API call.

With a built-in proxy, wake and proxy are a single code path in the same
goroutine — `EnsureHot()` returns, then `Tunnel()` connects, atomically.
No race, no leaked credentials, no external dependency.

The same user also discovered a proxy buffering bug: responses larger
than ~2KB hang indefinitely when an upstream HTTP proxy is the client.
This is caused by the raw `outReq.Write(tunnel)` + `io.Copy(w, resp.Body)`
approach in the current proxy handler, which doesn't flush. Part 0
of this plan fixes it.

---

## Current State

```
Internet → Cloudflare Tunnel → bhatti :8080 (authenticated API, plain HTTP)
                                  ├─ /health, /metrics         (unauthenticated)
                                  ├─ /sandboxes/...            (authenticated)
                                  └─ /sandboxes/:id/proxy/:port (authenticated, reverse proxy)
```

The proxy handler in `routes.go` (`handleSandboxProxyRoute`, line 1530)
opens an `Engine.Tunnel()` connection, rewrites the HTTP request to
`localhost:port`, and relays the response. WebSocket upgrades are handled
via hijack + bidirectional relay.

Problems:
1. No unauthenticated path to sandbox ports (can't share URLs publicly)
2. Depends on an external tunnel/proxy for TLS and public reachability
3. The proxy doesn't flush responses (streaming/SSE broken)

---

## What Bhatti Manages vs. What It Doesn't

Bhatti takes a **subdomain zone**, not the whole domain.

```
bhatti.sh                          → project website (NOT bhatti's concern)
www.bhatti.sh                      → project website (NOT bhatti's concern)
mail.bhatti.sh                     → email (NOT bhatti's concern)
api.bhatti.sh                      → authenticated API (managed by bhatti)
my-app.deploy.bhatti.sh            → public proxy to sandbox port (NEW)
dashboard.deploy.bhatti.sh         → public proxy to different sandbox (NEW)
```

The config gives bhatti two things:
1. **API host** — where the authenticated API is served (e.g. `api.bhatti.sh`)
2. **Proxy zone** — the subdomain zone for published sandbox ports (e.g. `deploy.bhatti.sh`)

The self-hoster's responsibilities are limited to DNS:
1. Point `api.bhatti.sh` at the bhatti host's public IP
2. Point `*.deploy.bhatti.sh` at the bhatti host's public IP
3. That's it

---

## Part 0: Fix the Proxy

The existing `handleProxyHTTP` (line 1565) does `outReq.Write(tunnel)` +
`http.ReadResponse()` + `io.Copy(w, resp.Body)`. This doesn't flush
chunks (breaks SSE, streaming APIs), doesn't strip hop-by-hop headers,
and doesn't inject `X-Forwarded-*` headers. A real user confirmed this
causes responses >2KB to hang when an upstream proxy is the client (PR #2).

This must be fixed before any public proxy work. The fix benefits both
the existing authenticated proxy and the future public proxy.

### 0.1 `tunnelTransport` — Engine.Tunnel as http.RoundTripper

**File:** `pkg/server/routes.go`

```go
// tunnelTransport wraps Engine.Tunnel() as an http.RoundTripper.
// Each RoundTrip opens a new tunnel connection to the sandbox.
type tunnelTransport struct {
    engine   engine.Engine
    engineID string
    port     int
}

func (t *tunnelTransport) RoundTrip(req *http.Request) (*http.Response, error) {
    tunnel, err := t.engine.Tunnel(req.Context(), t.engineID, t.port)
    if err != nil {
        return nil, err
    }

    // Guard against context cancellation leaking the tunnel FD.
    // If the client disconnects mid-response, ReverseProxy may cancel
    // the context without closing resp.Body. This AfterFunc ensures
    // the tunnel is always cleaned up.
    ctx := req.Context()
    stop := context.AfterFunc(ctx, func() {
        tunnel.Close()
    })

    if err := req.Write(tunnel); err != nil {
        stop()         // cancel the AfterFunc
        tunnel.Close()
        return nil, err
    }
    resp, err := http.ReadResponse(bufio.NewReader(tunnel), req)
    if err != nil {
        stop()
        tunnel.Close()
        return nil, err
    }
    // Closing the body also closes the tunnel and cancels the AfterFunc.
    resp.Body = &tunnelBody{ReadCloser: resp.Body, tunnel: tunnel, stopGuard: stop}
    return resp, nil
}

type tunnelBody struct {
    io.ReadCloser
    tunnel    io.Closer
    stopGuard func() bool // cancels the context.AfterFunc
    once      sync.Once
}

func (tb *tunnelBody) Close() error {
    var err error
    tb.once.Do(func() {
        tb.stopGuard()          // cancel AfterFunc — we're closing ourselves
        tb.ReadCloser.Close()
        err = tb.tunnel.Close()
    })
    return err
}
```

### 0.2 Replace `handleProxyHTTP`

**File:** `pkg/server/routes.go` — replace lines 1565-1604

```go
func (s *Server) handleProxyHTTP(w http.ResponseWriter, r *http.Request, engineID string, port int, path string) {
    proxy := &httputil.ReverseProxy{
        Transport: &tunnelTransport{
            engine:   s.engine,
            engineID: engineID,
            port:     port,
        },
        Director: func(req *http.Request) {
            req.URL.Scheme = "http"
            req.URL.Host = fmt.Sprintf("localhost:%d", port)
            req.URL.Path = path
            req.URL.RawQuery = r.URL.RawQuery
            req.Host = fmt.Sprintf("localhost:%d", port)
            // Proxy headers
            if clientIP, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
                req.Header.Set("X-Forwarded-For", clientIP)
            }
            req.Header.Set("X-Forwarded-Proto", "https")
            req.Header.Set("X-Forwarded-Host", r.Host)
        },
        FlushInterval: -1, // flush immediately (streaming)
        ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
            errResp(w, 502, "bad gateway: "+err.Error())
        },
    }
    proxy.ServeHTTP(w, r)
}
```

`httputil.ReverseProxy` handles hop-by-hop header removal, chunked
transfer encoding, trailer forwarding, and response flushing. Go's
`net/http` serves HTTP/2 automatically when TLS is configured — no
extra code needed.

**`handleProxyWS` is NOT replaced.** WebSocket uses hijack + bidirectional
relay, which is correct and not handled by `ReverseProxy`.

**Why a new tunnel per request?** Each `Engine.Tunnel()` opens a fresh
TCP stream to `localhost:port` inside the sandbox via lohar. There's no
connection pool — the tunnel is point-to-point. This matches existing
behavior.

### 0.3 Extract Shared WebSocket Relay

The existing `handleProxyWS` (line 1609) will be needed by both the
authenticated proxy and the public proxy. Extract it into a standalone
function.

**Idle timeout:** The relay sets a read deadline on both sides. Every
successful read resets the deadline. If neither side sends data for
`idleTimeout`, the connection closes. This prevents FD exhaustion from
abandoned WebSocket connections (critical for the public proxy, where
untrusted clients can open connections and walk away).

```go
const wsIdleTimeout = 10 * time.Minute

// idleCopyWithDeadline copies src → dst, resetting the read deadline
// on src after every successful read. Returns when src hits the idle
// timeout or either side errors/closes.
func idleCopyWithDeadline(dst io.Writer, src deadlineConn, timeout time.Duration) {
    buf := make([]byte, 32*1024)
    for {
        src.SetReadDeadline(time.Now().Add(timeout))
        n, err := src.Read(buf)
        if n > 0 {
            if _, wErr := dst.Write(buf[:n]); wErr != nil {
                return
            }
        }
        if err != nil {
            return
        }
    }
}

// deadlineConn is satisfied by net.Conn and by tunnels that wrap net.Conn.
type deadlineConn interface {
    io.Reader
    SetReadDeadline(t time.Time) error
}

// proxyWebSocket hijacks the client connection and relays WS frames
// through an engine tunnel. Used by both authenticated and public proxy.
func proxyWebSocket(w http.ResponseWriter, r *http.Request, eng engine.Engine, engineID string, port int, path string) {
    tunnel, err := eng.Tunnel(r.Context(), engineID, port)
    if err != nil {
        errResp(w, 502, "tunnel failed: "+err.Error())
        return
    }
    defer tunnel.Close()

    hj, ok := w.(http.Hijacker)
    if !ok {
        errResp(w, 500, "server doesn't support hijacking")
        return
    }
    clientConn, clientBuf, err := hj.Hijack()
    if err != nil {
        errResp(w, 500, "hijack failed")
        return
    }
    defer clientConn.Close()

    outReq := r.Clone(r.Context())
    outReq.URL.Scheme = "http"
    outReq.URL.Host = fmt.Sprintf("localhost:%d", port)
    outReq.URL.Path = path
    outReq.URL.RawQuery = r.URL.RawQuery
    outReq.RequestURI = ""
    outReq.Host = fmt.Sprintf("localhost:%d", port)
    outReq.Write(tunnel)

    resp, err := http.ReadResponse(bufio.NewReader(tunnel), outReq)
    if err != nil {
        return
    }
    resp.Write(clientBuf)
    clientBuf.Flush()

    // Bidirectional relay with idle timeout.
    // If the tunnel doesn't support SetReadDeadline (e.g., a pipe),
    // fall back to plain io.Copy (authenticated proxy on Docker engine).
    done := make(chan struct{})
    if tunnelDC, ok := tunnel.(deadlineConn); ok {
        go func() {
            idleCopyWithDeadline(tunnel, clientConn, wsIdleTimeout)
            close(done)
        }()
        idleCopyWithDeadline(clientConn, tunnelDC, wsIdleTimeout)
    } else {
        go func() {
            io.Copy(tunnel, clientConn)
            close(done)
        }()
        io.Copy(clientConn, tunnel)
    }
    <-done
}
```

Then `handleProxyWS` becomes a one-liner:
```go
func (s *Server) handleProxyWS(w http.ResponseWriter, r *http.Request, engineID string, port int, path string) {
    proxyWebSocket(w, r, s.engine, engineID, port, path)
}
```

### 0.4 Tests

- `TestReverseProxyStreaming` — sandbox runs SSE endpoint, proxy client
  receives events in real-time (not buffered until close)
- `TestReverseProxyLargeResponse` — sandbox returns 50KB response body,
  proxy delivers it completely (the PR #2 bug — >2KB hang)
- `TestReverseProxyHopByHop` — sandbox returns `Connection: close`,
  proxy strips it from client-facing response
- `TestReverseProxyXForwardedFor` — proxy request includes
  `X-Forwarded-For` visible inside the sandbox
- `TestReverseProxyClientDisconnect` — client cancels mid-response,
  tunnel FD is cleaned up (context.AfterFunc fires, no leak)
- `TestWebSocketIdleTimeout` — WS connection with no traffic closes
  after idle timeout
- `TestExistingAuthProxyRegression` — all existing proxy tests still pass

---

## Part 1: Publish API + Data Model

### 1.1 Store Schema

**File:** `pkg/store/store.go` — add to `New()` table creation block

```sql
CREATE TABLE IF NOT EXISTS publish_rules (
    id TEXT PRIMARY KEY,
    sandbox_id TEXT NOT NULL,
    user_id TEXT NOT NULL,
    port INTEGER NOT NULL,
    alias TEXT NOT NULL UNIQUE,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(sandbox_id, port)
);
CREATE INDEX IF NOT EXISTS idx_publish_rules_sandbox ON publish_rules(sandbox_id);
```

**Why two UNIQUE constraints?**
- `UNIQUE(alias)` — globally unique subdomain label. Also creates the
  index for the hot-path `SELECT ... WHERE alias = ?` in every public
  proxy request.
- `UNIQUE(sandbox_id, port)` — one alias per (sandbox, port). Publishing
  the same port twice is a mistake.

### 1.2 Store Types and Methods

**File:** `pkg/store/store.go`

```go
type PublishRule struct {
    ID        string    `json:"id"`
    SandboxID string    `json:"sandbox_id"`
    UserID    string    `json:"user_id"`
    Port      int       `json:"port"`
    Alias     string    `json:"alias"`
    CreatedAt time.Time `json:"created_at"`
}

func (s *Store) CreatePublishRule(rule PublishRule) error {
    _, err := s.db.Exec(
        `INSERT INTO publish_rules (id, sandbox_id, user_id, port, alias)
         VALUES (?, ?, ?, ?, ?)`,
        rule.ID, rule.SandboxID, rule.UserID, rule.Port, rule.Alias,
    )
    if err != nil {
        if strings.Contains(err.Error(), "UNIQUE constraint") {
            if strings.Contains(err.Error(), "alias") {
                return fmt.Errorf("alias %q is already taken", rule.Alias)
            }
            return fmt.Errorf("port %d is already published for this sandbox", rule.Port)
        }
        return err
    }
    return nil
}

func (s *Store) GetPublishRuleByAlias(alias string) (*PublishRule, error) {
    var r PublishRule
    err := s.db.QueryRow(
        `SELECT id, sandbox_id, user_id, port, alias, created_at
         FROM publish_rules WHERE alias = ?`, alias,
    ).Scan(&r.ID, &r.SandboxID, &r.UserID, &r.Port, &r.Alias, &r.CreatedAt)
    if err == sql.ErrNoRows {
        return nil, fmt.Errorf("no publish rule for alias %q", alias)
    }
    return &r, err
}

func (s *Store) ListPublishRules(sandboxID string) ([]PublishRule, error) {
    rows, err := s.db.Query(
        `SELECT id, sandbox_id, user_id, port, alias, created_at
         FROM publish_rules WHERE sandbox_id = ?`, sandboxID,
    )
    if err != nil {
        return nil, err
    }
    defer rows.Close()
    var rules []PublishRule
    for rows.Next() {
        var r PublishRule
        rows.Scan(&r.ID, &r.SandboxID, &r.UserID, &r.Port, &r.Alias, &r.CreatedAt)
        rules = append(rules, r)
    }
    return rules, rows.Err()
}

func (s *Store) DeletePublishRule(userID, sandboxID string, port int) error {
    res, err := s.db.Exec(
        `DELETE FROM publish_rules WHERE user_id = ? AND sandbox_id = ? AND port = ?`,
        userID, sandboxID, port,
    )
    if err != nil {
        return err
    }
    if n, _ := res.RowsAffected(); n == 0 {
        return fmt.Errorf("no publish rule for port %d on this sandbox", port)
    }
    return nil
}

func (s *Store) DeletePublishRulesForSandbox(sandboxID string) (int64, error) {
    res, err := s.db.Exec(`DELETE FROM publish_rules WHERE sandbox_id = ?`, sandboxID)
    if err != nil {
        return 0, err
    }
    return res.RowsAffected()
}

func (s *Store) CleanupOrphanedPublishRules() (int64, error) {
    // Clean up rules for sandboxes that are destroyed, unknown (unrecoverable),
    // or missing entirely (deleted from DB but rules survived a crash).
    res, err := s.db.Exec(`
        DELETE FROM publish_rules WHERE sandbox_id NOT IN (
            SELECT id FROM sandboxes WHERE status NOT IN ('destroyed', 'unknown')
        )`)
    if err != nil {
        return 0, err
    }
    return res.RowsAffected()
}
```

### 1.3 Alias Validation

**File:** `pkg/server/routes.go`

```go
var aliasRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

var reservedAliases map[string]bool // populated by initReservedAliases

func initReservedAliases(proxyZone string) {
    reservedAliases = map[string]bool{
        "www": true, "mail": true, "admin": true, "status": true,
        "ns1": true, "ns2": true, "api": true, "app": true,
        "_acme-challenge": true,
    }
    // Reserve the proxy zone's own label to prevent confusion.
    // e.g., if proxyZone is "deploy.bhatti.sh", reserve "deploy".
    if proxyZone != "" {
        parts := strings.SplitN(proxyZone, ".", 2)
        if len(parts) > 0 {
            reservedAliases[parts[0]] = true
        }
    }
}

func validateAlias(alias string) error {
    if !aliasRegex.MatchString(alias) {
        return fmt.Errorf("alias must be lowercase alphanumeric with hyphens, 1-63 chars")
    }
    if reservedAliases[alias] {
        return fmt.Errorf("alias %q is reserved", alias)
    }
    return nil
}
```

### 1.4 Auto-Generated Aliases

When `--alias` is omitted, generate from `<sandbox_name>` (without port
suffix when only one port is published, which is the common case):

```go
func generateAlias(sandboxName string, port int, existingCount int) string {
    alias := strings.ToLower(sandboxName)
    alias = regexp.MustCompile(`[^a-z0-9-]`).ReplaceAllString(alias, "-")
    alias = strings.Trim(alias, "-")
    if alias == "" {
        alias = "sandbox"
    }
    // Only append -p<port> when the sandbox already has other published ports.
    // "my-app.deploy.bhatti.sh" is much nicer than "my-app-p3000.deploy.bhatti.sh".
    if existingCount > 0 {
        suffix := fmt.Sprintf("-p%d", port)
        maxBase := 63 - len(suffix)
        if len(alias) > maxBase {
            alias = alias[:maxBase]
        }
        alias += suffix
    } else if len(alias) > 63 {
        alias = alias[:63]
    }
    return alias
}

func generateUniqueAlias(st *store.Store, sandboxID, sandboxName string, port int) (string, error) {
    existing, _ := st.ListPublishRules(sandboxID)
    alias := generateAlias(sandboxName, port, len(existing))

    // Retry loop with increasing random suffix length.
    // 8 hex chars = 4 billion possibilities — sufficient for any realistic
    // collision rate. 3 attempts covers the TOCTOU race where two
    // concurrent publishes both see "not taken" simultaneously.
    candidates := []string{alias}
    for i := 0; i < 3; i++ {
        b := make([]byte, 4)
        rand.Read(b)
        candidates = append(candidates, alias + "-" + hex.EncodeToString(b))
    }

    for _, candidate := range candidates {
        if len(candidate) > 63 {
            candidate = candidate[:63]
        }
        if _, err := st.GetPublishRuleByAlias(candidate); err != nil {
            return candidate, nil // not taken
        }
    }
    return "", fmt.Errorf("failed to generate unique alias after %d attempts", len(candidates))
}
```

### 1.5 Server Handlers

**File:** `pkg/server/routes.go`

The `handleSandbox` function (line 635) dispatches sub-routes. The
existing dispatch uses `switch sub` for exact matches and `HasPrefix`
for `proxy/`. Publish needs `HasPrefix` too because unpublish has a
port suffix (`publish/3000`):

```go
// In handleSandbox, add before the switch statement, after the proxy HasPrefix block:
if strings.HasPrefix(sub, "publish") {
    s.handleSandboxPublish(w, r, id, strings.TrimPrefix(sub, "publish"))
    return
}
```

**Handlers:**

```go
func (s *Server) handleSandboxPublish(w http.ResponseWriter, r *http.Request, id, sub string) {
    user := UserFromContext(r.Context())

    switch r.Method {
    case "POST":
        if sub != "" && sub != "/" {
            errResp(w, 404, "not found")
            return
        }
        s.handlePublish(w, r, user, id)
    case "GET":
        if sub != "" && sub != "/" {
            errResp(w, 404, "not found")
            return
        }
        s.handleListPublishRules(w, r, user, id)
    case "DELETE":
        // DELETE /sandboxes/:id/publish/3000
        portStr := strings.TrimPrefix(sub, "/")
        port, err := strconv.Atoi(portStr)
        if err != nil || port < 1 || port > 65535 {
            errResp(w, 400, "invalid port in path")
            return
        }
        s.handleUnpublish(w, r, user, id, port)
    default:
        errResp(w, 405, "method not allowed")
    }
}

func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request, user *store.User, sandboxID string) {
    sb := s.getUserSandbox(w, r, sandboxID)
    if sb == nil {
        return
    }

    var req struct {
        Port  int    `json:"port"`
        Alias string `json:"alias,omitempty"`
    }
    if err := readJSON(r, &req); err != nil {
        errResp(w, 400, "invalid request body")
        return
    }
    if req.Port < 1 || req.Port > 65535 {
        errResp(w, 400, "port must be 1-65535")
        return
    }

    alias := req.Alias
    if alias == "" {
        var err error
        alias, err = generateUniqueAlias(s.store, sb.ID, sb.Name, req.Port)
        if err != nil {
            errResp(w, 500, "alias generation failed")
            return
        }
    }
    if err := validateAlias(alias); err != nil {
        errResp(w, 400, err.Error())
        return
    }

    rule := store.PublishRule{
        ID:        "pub_" + genID(),
        SandboxID: sb.ID,
        UserID:    user.ID,
        Port:      req.Port,
        Alias:     alias,
    }
    if err := s.store.CreatePublishRule(rule); err != nil {
        if strings.Contains(err.Error(), "already taken") ||
            strings.Contains(err.Error(), "already published") {
            errResp(w, 409, err.Error())
        } else {
            errResp(w, 500, err.Error())
        }
        return
    }

    writeJSON(w, 201, map[string]interface{}{
        "id":         rule.ID,
        "sandbox_id": sb.ID,
        "port":       rule.Port,
        "alias":      alias,
        "url":        s.publishedURL(alias),
        "created_at": rule.CreatedAt,
    })
}

func (s *Server) publishedURL(alias string) string {
    if s.proxyZone != "" {
        return "https://" + alias + "." + s.proxyZone
    }
    if s.publicProxyAddr != "" {
        return "http://" + s.publicProxyAddr + "/" + alias + "/"
    }
    // No public proxy configured — return a placeholder that makes
    // the situation clear instead of a bare alias that looks broken.
    return "(no public proxy configured) alias: " + alias
}

func (s *Server) handleListPublishRules(w http.ResponseWriter, r *http.Request, user *store.User, sandboxID string) {
    sb := s.getUserSandbox(w, r, sandboxID)
    if sb == nil {
        return
    }
    rules, err := s.store.ListPublishRules(sb.ID)
    if err != nil {
        errResp(w, 500, err.Error())
        return
    }
    type ruleResp struct {
        store.PublishRule
        URL string `json:"url"`
    }
    resp := make([]ruleResp, len(rules))
    for i, r := range rules {
        resp[i] = ruleResp{PublishRule: r, URL: s.publishedURL(r.Alias)}
    }
    writeJSON(w, 200, resp)
}

func (s *Server) handleUnpublish(w http.ResponseWriter, r *http.Request, user *store.User, sandboxID string, port int) {
    sb := s.getUserSandbox(w, r, sandboxID)
    if sb == nil {
        return
    }
    // Look up alias before deleting so we can invalidate the route cache.
    rules, _ := s.store.ListPublishRules(sb.ID)
    if err := s.store.DeletePublishRule(user.ID, sb.ID, port); err != nil {
        errResp(w, 404, err.Error())
        return
    }
    // Invalidate route cache for the deleted alias.
    for _, r := range rules {
        if r.Port == port {
            if s.publicProxy != nil {
                s.publicProxy.routeCache.Invalidate(r.Alias)
            }
            break
        }
    }
    w.WriteHeader(204)
}
```

**Cleanup on sandbox destroy.** In `handleSandbox` DELETE (line 710),
after `s.store.DetachAllPersistentVolumesForSandbox(id)`:

```go
if n, err := s.store.DeletePublishRulesForSandbox(sb.ID); err != nil {
    slog.Warn("cleanup publish rules", "sandbox", sb.ID, "error", err)
} else if n > 0 {
    slog.Info("cleaned up publish rules", "sandbox", sb.ID, "count", n)
}
// Invalidate all cached routes for this sandbox.
if s.publicProxy != nil {
    s.publicProxy.routeCache.InvalidateSandbox(sb.ID)
}
```

**In-flight requests during destroy:** The tunnel's TCP connection drops
when Firecracker is killed. `httputil.ReverseProxy` returns 502. Correct
behavior, no special handling needed.

### 1.6 Server Struct Changes

**File:** `pkg/server/server.go`

Add fields to `Server`:

```go
type Server struct {
    // ... existing fields ...

    // Public proxy (set via options before first request)
    proxyZone       string              // e.g. "deploy.bhatti.sh"
    apiHost         string              // e.g. "api.bhatti.sh"
    publicProxyAddr string              // e.g. "host:8443" (for URL generation)
    publicProxy     *PublicProxyHandler  // nil until configured
    resumeSem       chan struct{}        // bounds concurrent cold resumes
}
```

Change the `New` signature. The current signature is
`New(eng, st, dataDir ...string)`. Change `dataDir` from variadic to
required and add options:

```go
type ServerOption func(*Server)

func WithProxyZone(zone string) ServerOption {
    return func(s *Server) { s.proxyZone = zone }
}
func WithAPIHost(host string) ServerOption {
    return func(s *Server) { s.apiHost = host }
}
func WithPublicProxyAddr(addr string) ServerOption {
    return func(s *Server) { s.publicProxyAddr = addr }
}

func New(eng engine.Engine, st *store.Store, dataDir string, opts ...ServerOption) *Server {
    s := &Server{
        engine:      eng,
        store:       st,
        dataDir:     dataDir,
        mux:         http.NewServeMux(),
        limiter:     newRateLimiter(),
        startTime:   time.Now(),
        pullCancels: make(map[string]context.CancelFunc),
        resumeSem:   make(chan struct{}, 10),
    }
    for _, opt := range opts {
        opt(s)
    }
    s.routes()
    s.startTaskCleanup()
    return s
}
```

**Update call sites.** Current callers pass `dataDir` as variadic:
- `cmd/bhatti/main.go`: `server.New(eng, st, cfg.DataDir)` — unchanged
- `pkg/server/server_test.go`: `server.New(mockEngine, st)` → `server.New(mockEngine, st, "")`
- Any other test files — add `""` as third arg

### 1.7 Tests

**Store tests:**
- `TestCreatePublishRule` — create, read back by alias, verify fields
- `TestPublishRuleAliasUnique` — same alias twice → error
- `TestPublishRuleSandboxPortUnique` — same (sandbox, port) → error
- `TestPublishRuleDifferentSandboxSamePort` — different sandbox, same port → OK
- `TestGetPublishRuleByAlias` — exists → rule; missing → error
- `TestListPublishRules` — 3 rules on sandbox, list → 3
- `TestDeletePublishRule` — delete, then get → error
- `TestDeletePublishRulesForSandbox` — 3 rules, delete all → returns 3
- `TestDeletePreservesOtherSandbox` — A has rules, B has rules, delete A → B intact
- `TestCleanupOrphanedPublishRules` — rule for destroyed sandbox, cleanup → deleted

**Server tests (mock engine):**
- `TestPublishHTTP` — POST, verify 201 + alias + url in response
- `TestPublishAutoAlias` — POST without alias, verify generated alias
- `TestPublishDuplicateAlias` → 409
- `TestPublishDuplicatePort` → 409
- `TestUnpublishHTTP` — DELETE, verify 204
- `TestListPublishHTTP` — publish 2, list → 2
- `TestPublishUserScoping` — user A publishes, user B can't unpublish
- `TestDestroyCleanupPublish` — destroy sandbox → rules gone
- `TestAliasValidation` — invalid chars, reserved → 400
- `TestPublishNonexistentSandbox` → 404

---

## Part 2: Public Proxy Handler

### 2.1 The Handler

**File:** `pkg/server/public_proxy.go` (new)

```go
type PublicProxyHandler struct {
    engine      engine.Engine
    store       *store.Store
    limiter     *publicRateLimiter
    resumeSem   chan struct{}
    resumeGroup singleflight.Group             // coalesces concurrent resumes per sandbox
    routeCache  *routeCache                    // in-memory alias → route mapping
}

var errServerBusy = fmt.Errorf("server busy")

func NewPublicProxyHandler(eng engine.Engine, st *store.Store, resumeSem chan struct{}) *PublicProxyHandler {
    return &PublicProxyHandler{
        engine:    eng,
        store:     st,
        limiter:   newPublicRateLimiter(),
        resumeSem: resumeSem,
        routeCache: newRouteCache(),
    }
}
```

**Path-based routing** (`/<alias>/rest/of/path`):

```go
func (h *PublicProxyHandler) ServeHTTPPathBased(w http.ResponseWriter, r *http.Request) {
    path := strings.TrimPrefix(r.URL.Path, "/")
    if path == "" {
        errResp(w, 404, "not found")
        return
    }
    alias := path
    rest := "/"
    if idx := strings.IndexByte(path, '/'); idx >= 0 {
        alias = path[:idx]
        rest = path[idx:]
    }
    h.proxyToAlias(w, r, alias, rest)
}
```

**Core proxy logic** (shared between path-based and host-based):

```go
// resolvedRoute is the cached result of an alias lookup.
// Stored in routeCache to avoid SQLite queries on every request.
type resolvedRoute struct {
    engineID  string
    sandboxID string
    port      int
}

// routeCache maps alias → resolvedRoute. Entries are invalidated on
// unpublish and sandbox destroy. Bounded to 10K entries (LRU eviction).
type routeCache struct {
    mu    sync.RWMutex
    cache *lru.Cache[string, resolvedRoute]
}

func newRouteCache() *routeCache {
    c, _ := lru.New[string, resolvedRoute](10_000)
    return &routeCache{cache: c}
}

func (rc *routeCache) Get(alias string) (resolvedRoute, bool) {
    rc.mu.RLock()
    defer rc.mu.RUnlock()
    return rc.cache.Get(alias)
}

func (rc *routeCache) Set(alias string, route resolvedRoute) {
    rc.mu.Lock()
    defer rc.mu.Unlock()
    rc.cache.Add(alias, route)
}

func (rc *routeCache) Invalidate(alias string) {
    rc.mu.Lock()
    defer rc.mu.Unlock()
    rc.cache.Remove(alias)
}

// InvalidateSandbox removes all cached routes for a sandbox.
// Called on sandbox destroy.
func (rc *routeCache) InvalidateSandbox(sandboxID string) {
    rc.mu.Lock()
    defer rc.mu.Unlock()
    for _, key := range rc.cache.Keys() {
        if route, ok := rc.cache.Peek(key); ok && route.sandboxID == sandboxID {
            rc.cache.Remove(key)
        }
    }
}
```

```go
func (h *PublicProxyHandler) proxyToAlias(w http.ResponseWriter, r *http.Request, alias, path string) {
    // Hard per-request deadline: 5 minutes. Prevents slow-drip attacks
    // that hold tunnels open indefinitely. WriteTimeout on the http.Server
    // resets on each write, so it doesn't catch slow-drip responses.
    ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
    defer cancel()
    r = r.WithContext(ctx)

    // Resolve alias → route. Check cache first, fall back to SQLite.
    // NOTE: alias lookup happens BEFORE rate limiting. Unknown aliases
    // get a fast 404 with zero state allocation. This prevents attackers
    // from creating unbounded rate limiter entries by probing random aliases.
    route, cached := h.routeCache.Get(alias)
    if !cached {
        rule, err := h.store.GetPublishRuleByAlias(alias)
        if err != nil {
            errResp(w, 404, "not found")
            return
        }
        sb, err := h.store.GetSandboxByID(rule.SandboxID)
        if err != nil || sb.Status == "destroyed" {
            errResp(w, 404, "not found")
            return
        }
        route = resolvedRoute{
            engineID:  sb.EngineID,
            sandboxID: sb.ID,
            port:      rule.Port,
        }
        h.routeCache.Set(alias, route)
    }

    // Rate limit AFTER alias validation — only known aliases get buckets.
    if !h.limiter.Allow(alias) {
        w.Header().Set("Retry-After", "1")
        errResp(w, 429, "rate limit exceeded")
        return
    }

    // Limit request body size for non-GET/HEAD methods (public internet).
    if r.Method != "GET" && r.Method != "HEAD" {
        r.Body = http.MaxBytesReader(w, r.Body, 50<<20) // 50MB
    }

    // Wake sandbox with bounded concurrency + singleflight coalescing.
    // See §2.1 "Resume semaphore + singleflight behavior" for details.
    if te, ok := h.engine.(ThermalEngine); ok {
        if err := h.ensureHotBounded(ctx, te, route.engineID); err != nil {
            if err == errServerBusy {
                w.Header().Set("Retry-After", "2")
                errResp(w, 503, "server busy, retry shortly")
            } else {
                // Sandbox may have been destroyed since cache was populated.
                // Invalidate cache so next request re-checks SQLite.
                h.routeCache.Invalidate(alias)
                errResp(w, 502, "sandbox unavailable")
            }
            return
        }
    }

    // WebSocket
    if websocket.IsWebSocketUpgrade(r) {
        proxyWebSocket(w, r, h.engine, route.engineID, route.port, path)
        return
    }

    // HTTP
    proxy := &httputil.ReverseProxy{
        Transport: &tunnelTransport{
            engine:   h.engine,
            engineID: route.engineID,
            port:     route.port,
        },
        Director: func(req *http.Request) {
            req.URL.Scheme = "http"
            req.URL.Host = fmt.Sprintf("localhost:%d", route.port)
            req.URL.Path = path
            req.URL.RawQuery = r.URL.RawQuery
            req.Host = fmt.Sprintf("localhost:%d", route.port)
            if clientIP, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
                req.Header.Set("X-Forwarded-For", clientIP)
            }
            req.Header.Set("X-Forwarded-Host", r.Host)
        },
        FlushInterval: -1,
        ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
            slog.Warn("public proxy error", "alias", alias, "error", err)
            errResp(w, 502, "bad gateway")
        },
    }
    proxy.ServeHTTP(w, r)
}
```

**Hot-path cost:** For a hot sandbox with a cached route, the per-request
work is: LRU cache lookup (in-memory, ~50ns) → rate limiter check
(in-memory, ~100ns) → `ThermalState` (read `vm.Thermal` under lock,
~200ns) → tunnel + proxy. Zero SQLite queries on the hot path.

**Resume semaphore + singleflight behavior.** The resume semaphore
bounds concurrent cold wakeups to 10 **distinct sandboxes**, not 10
requests. `EnsureHot` is wrapped in a `singleflight.Group` keyed by
`engineID`, so concurrent requests to the same sandbox coalesce into
one resume and share one semaphore slot:

```go
type PublicProxyHandler struct {
    // ...
    resumeGroup singleflight.Group
}
```

```go
func (h *PublicProxyHandler) ensureHotBounded(ctx context.Context, te ThermalEngine, engineID string) error {
    thermal := te.ThermalState(engineID)
    if thermal == "hot" {
        return nil
    }

    // singleflight: all concurrent requests for the same sandbox
    // share one resume call and one semaphore slot.
    _, err, _ := h.resumeGroup.Do(engineID, func() (interface{}, error) {
        select {
        case h.resumeSem <- struct{}{}:
            defer func() { <-h.resumeSem }()
        default:
            return nil, errServerBusy
        }
        return nil, te.EnsureHot(ctx, engineID)
    })
    return err
}
```

When 50 requests hit a cold sandbox:
1. First request acquires the semaphore, calls `EnsureHot()` (~50ms)
2. Remaining 49 coalesce via singleflight — they block on the first
   call, consuming zero semaphore slots
3. First completes, all 50 proceed to tunnel
4. Only 1 semaphore slot was used — the other 9 are free for
   different sandboxes resuming concurrently

Without singleflight, 10 requests to the same sandbox would consume
all 10 semaphore slots while 9 just block on `vm.stateMu`, starving
resumes for other sandboxes.

### 2.2 Public Rate Limiter

**File:** `pkg/server/public_proxy.go`

Two key design decisions from review:

1. **Rate limit AFTER alias lookup, not before.** Unknown aliases get
   a fast 404 with zero state allocation. Otherwise an attacker probing
   millions of random aliases creates unbounded token bucket entries.

2. **Per-alias buckets are mutex-protected for the entire Allow() call.**
   The bucket's `tokens` and `lastRefill` fields are mutated on every
   call. Dropping the lock before `tb.Allow()` would race under
   concurrent requests for the same alias.

3. **Fixed-size LRU, not unbounded map.** Cap at 10K entries. When full,
   evict least-recently-accessed alias. This bounds memory to ~640KB
   regardless of traffic pattern.

```go
type publicRateLimiter struct {
    mu       sync.Mutex
    perAlias *lru.Cache[string, *tokenBucket] // max 10K entries
    global   *tokenBucket
}

func newPublicRateLimiter() *publicRateLimiter {
    cache, _ := lru.New[string, *tokenBucket](10_000)
    return &publicRateLimiter{
        perAlias: cache,
        global:   newTokenBucket(10000, 10000),
    }
}

// Allow checks rate limits. Only called for aliases that are known to
// exist (checked by the caller after GetPublishRuleByAlias succeeds).
func (l *publicRateLimiter) Allow(alias string) bool {
    l.mu.Lock()
    defer l.mu.Unlock()

    if !l.global.allow() {
        return false
    }
    tb, ok := l.perAlias.Get(alias)
    if !ok {
        tb = newTokenBucket(100, 200)
        l.perAlias.Add(alias, tb)
    }
    return tb.allow()
}
```

**Dependency:** `github.com/hashicorp/golang-lru/v2` — tiny, zero
transitive deps, widely used. Alternatively, hand-roll a linked-list
LRU in ~60 lines.

### 2.3 Config

**File:** `pkg/config.go`

```go
type Config struct {
    Engine    string `yaml:"engine"`
    Listen    string `yaml:"listen"`
    APIURL    string `yaml:"api_url"`
    AuthToken string `yaml:"auth_token"`
    DataDir   string `yaml:"data_dir"`

    PublicProxyListen string        `yaml:"public_proxy_listen,omitempty"`
    Domain            *DomainConfig `yaml:"domain,omitempty"`

    FirecrackerBin    string `yaml:"firecracker_bin"`
    FirecrackerKernel string `yaml:"firecracker_kernel"`
    FirecrackerRootfs string `yaml:"firecracker_rootfs"`
}

type DomainConfig struct {
    APIHost   string `yaml:"api_host"`
    ProxyZone string `yaml:"proxy_zone"`
    ACMEEmail string `yaml:"acme_email"`
    TLSCert   string `yaml:"tls_cert"`
    TLSKey    string `yaml:"tls_key"`
}
```

### 2.4 Starting the Listener

**File:** `cmd/bhatti/main.go` — after the API server starts

```go
if cfg.PublicProxyListen != "" {
    pubHandler := server.NewPublicProxyHandler(eng, st, srv.ResumeSem())
    pubServer := &http.Server{
        Addr:         cfg.PublicProxyListen,
        Handler:      http.HandlerFunc(pubHandler.ServeHTTPPathBased),
        ReadTimeout:  30 * time.Second,
        WriteTimeout: 60 * time.Second,
        IdleTimeout:  120 * time.Second,
    }
    go func() {
        slog.Info("public proxy listening", "addr", cfg.PublicProxyListen)
        if err := pubServer.ListenAndServe(); err != http.ErrServerClosed {
            slog.Error("public proxy failed", "error", err)
        }
    }()
}
```

Add `ResumeSem()` accessor to `Server`:
```go
func (s *Server) ResumeSem() chan struct{} { return s.resumeSem }
```

### 2.5 CLI Commands

**File:** `cmd/bhatti/cli.go`

```go
var publishCmd = &cobra.Command{
    Use:   "publish <sandbox> --port <port> [--alias <alias>]",
    Short: "Publish a sandbox port with a public URL",
    Args:  cobra.ExactArgs(1),
    Run: func(cmd *cobra.Command, args []string) {
        port, _ := cmd.Flags().GetInt("port")
        alias, _ := cmd.Flags().GetString("alias")

        body := map[string]interface{}{"port": port}
        if alias != "" {
            body["alias"] = alias
        }
        sandboxID := resolveSandboxID(args[0])
        resp := apiPost(fmt.Sprintf("/sandboxes/%s/publish", sandboxID), body)
        if jsonOutput {
            fmt.Println(resp.raw)
            return
        }
        fmt.Printf("Published: %s\n", resp.json["url"])
    },
}

var unpublishCmd = &cobra.Command{
    Use:   "unpublish <sandbox> --port <port>",
    Short: "Unpublish a sandbox port",
    Args:  cobra.ExactArgs(1),
    Run: func(cmd *cobra.Command, args []string) {
        port, _ := cmd.Flags().GetInt("port")
        sandboxID := resolveSandboxID(args[0])
        apiDelete(fmt.Sprintf("/sandboxes/%s/publish/%d", sandboxID, port))
        if !jsonOutput {
            fmt.Printf("Unpublished port %d\n", port)
        }
    },
}
```

Register in `init()`:
```go
publishCmd.Flags().IntP("port", "p", 0, "Port to publish (required)")
publishCmd.MarkFlagRequired("port")
publishCmd.Flags().StringP("alias", "a", "", "Custom alias (auto-generated if omitted)")
unpublishCmd.Flags().IntP("port", "p", 0, "Port to unpublish (required)")
unpublishCmd.MarkFlagRequired("port")
rootCmd.AddCommand(publishCmd, unpublishCmd)
```

### 2.6 Orphan Cleanup on Startup

**File:** `cmd/bhatti/main.go` — after `recoverVMs` and volume cleanup

```go
if n, err := st.CleanupOrphanedPublishRules(); err != nil {
    slog.Warn("orphaned publish rule cleanup failed", "error", err)
} else if n > 0 {
    slog.Info("cleaned up orphaned publish rules", "count", n)
}
```

### 2.7 Phase 1 Tests

**Integration tests (agni-01):**
- `TestPublishAndAccess` — create sandbox, run HTTP server, publish port,
  `curl http://localhost:8443/test-app/` → 200
- `TestPublishWebSocket` — WebSocket through public proxy works
- `TestPublishEnsureHot` — publish, stop sandbox, hit URL → wakes, 200
- `TestPublishDestroyCleanup` — destroy sandbox, hit URL → 404
- `TestPublishRateLimit` — 300 rapid requests → 429 after ~200
- `TestPublishResumeBound` — 20 cold sandboxes, hit all → max 10 resume,
  rest get 503
- `TestPublishSingleflight` — 50 concurrent requests to same cold sandbox,
  only 1 semaphore slot used, all 50 succeed
- `TestPublishRouteCache` — first request populates cache, second request
  skips SQLite (verify with query counter or mock)
- `TestPublishRouteCacheInvalidation` — unpublish invalidates cache,
  next request returns 404
- `TestPublishRequestTimeout` — sandbox app that sleeps 10 minutes,
  proxy returns error within 5-minute deadline
- `TestPublishRequestBodyLimit` — POST with >50MB body, returns 413
- `TestPublishUnknownAliasNoState` — probe 1000 random aliases, verify
  rate limiter map doesn't grow
- `TestPublishCLI` — `bhatti publish dev -p 3000 -a my-app` → URL,
  `bhatti unpublish dev -p 3000` → removed

---

## Part 3: Domain Mode

### 3.1 Host-Based Routing

**File:** `pkg/server/server.go` — at the top of `ServeHTTP`

When `proxyZone` is set, route by `Host` header BEFORE auth:

```go
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    host := stripPort(r.Host)

    if s.proxyZone != "" {
        if strings.HasSuffix(host, "."+s.proxyZone) {
            alias := strings.TrimSuffix(host, "."+s.proxyZone)
            s.publicProxy.proxyToAlias(w, r, alias, r.URL.Path)
            return
        }
        if host != s.apiHost {
            errResp(w, 404, "unknown host")
            return
        }
        // host == apiHost → fall through to auth
    }

    // === existing auth middleware (unchanged) ===
    cleanPath := path.Clean(r.URL.Path)
    // ...
}

func stripPort(host string) string {
    if i := strings.LastIndex(host, ":"); i > 0 {
        return host[:i]
    }
    return host
}
```

When `proxyZone` is empty, this block is skipped — existing behavior unchanged.

### 3.2 TLS with autocert

**File:** `cmd/bhatti/main.go`

When `cfg.Domain` is set, start `:443` + `:80` instead of `:8080`:

```go
if cfg.Domain != nil {
    dom := cfg.Domain
    if dom.APIHost == "" || dom.ProxyZone == "" {
        slog.Error("domain.api_host and domain.proxy_zone are required")
        os.Exit(1)
    }
    if dom.ACMEEmail == "" && dom.TLSCert == "" {
        slog.Error("domain.acme_email required (or provide tls_cert/tls_key)")
        os.Exit(1)
    }

    srv := server.New(eng, st, cfg.DataDir,
        server.WithProxyZone(dom.ProxyZone),
        server.WithAPIHost(dom.APIHost),
    )
    srv.StartThermalManager(thermalCfg)

    var tlsConfig *tls.Config
    if dom.TLSCert != "" && dom.TLSKey != "" {
        cert, err := tls.LoadX509KeyPair(dom.TLSCert, dom.TLSKey)
        if err != nil {
            slog.Error("load TLS cert", "error", err)
            os.Exit(1)
        }
        tlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
        go http.ListenAndServe(":80", http.HandlerFunc(redirectHTTPS))
    } else {
        certDir := filepath.Join(cfg.DataDir, "certs")
        os.MkdirAll(certDir, 0700)
        cm := &autocert.Manager{
            Prompt:     autocert.AcceptTOS,
            HostPolicy: srv.HostPolicy,
            Cache:      autocert.DirCache(certDir),
            Email:      dom.ACMEEmail,
        }
        tlsConfig = cm.TLSConfig()
        go http.ListenAndServe(":80", cm.HTTPHandler(http.HandlerFunc(redirectHTTPS)))
    }

    httpsServer := &http.Server{
        Addr:              ":443",
        Handler:           srv,
        TLSConfig:         tlsConfig,
        ReadHeaderTimeout: 10 * time.Second,
        IdleTimeout:       120 * time.Second,
    }
    go func() {
        slog.Info("bhatti listening",
            "api", "https://"+dom.APIHost,
            "proxy", "https://*."+dom.ProxyZone,
        )
        if err := httpsServer.ListenAndServeTLS("", ""); err != http.ErrServerClosed {
            slog.Error("HTTPS server failed", "error", err)
            os.Exit(1)
        }
    }()

    // ... signal handling, shutdown ...
}
```

**Domain mode keeps a localhost-only API listener.** In domain mode,
the external-facing listener is `:443` + `:80`. But `:8080` continues
listening on `127.0.0.1` only (loopback). This is critical for:

- **Health checks and monitoring** (`curl localhost:8080/health`)
- **Local development and debugging** (no TLS cert needed)
- **Graceful migration** (internal tools don't break when domain mode
  is enabled)

The loopback listener is not exposed to the network, so API keys
cannot leak over plain HTTP to external clients. This is the standard
pattern used by Grafana, Prometheus, and every production service that
has both internal and external listeners.

```go
// In domain mode, also start localhost-only API listener
go func() {
    slog.Info("internal API listening", "addr", "127.0.0.1:8080")
    loopback := &http.Server{
        Addr:    "127.0.0.1:8080",
        Handler: srv,
    }
    if err := loopback.ListenAndServe(); err != http.ErrServerClosed {
        slog.Error("loopback server failed", "error", err)
    }
}()
```

### 3.3 HostPolicy

**File:** `pkg/server/server.go`

`HostPolicy` is called during TLS handshake for uncached certs. This
is an **unauthenticated** code path — an attacker can force invocations
by connecting with arbitrary SNI values. To prevent this from becoming
a DoS vector against SQLite, check the in-memory route cache first.
Only fall through to SQLite if the cache misses.

```go
func (s *Server) HostPolicy(_ context.Context, host string) error {
    if host == s.apiHost {
        return nil
    }
    if s.proxyZone != "" && strings.HasSuffix(host, "."+s.proxyZone) {
        alias := strings.TrimSuffix(host, "."+s.proxyZone)
        // Fast path: check in-memory route cache (populated by proxy requests)
        if _, ok := s.publicProxy.routeCache.Get(alias); ok {
            return nil
        }
        // Slow path: SQLite lookup (only for first TLS handshake per alias)
        if _, err := s.store.GetPublishRuleByAlias(alias); err != nil {
            return fmt.Errorf("no publish rule for %q", alias)
        }
        return nil
    }
    return fmt.Errorf("unknown host: %s", host)
}
```

Only called during TLS handshake when cert is NOT cached. Cached certs
(common case) bypass this entirely. With the route cache, even uncached
certs for known aliases avoid SQLite.

**Stale cert after unpublish:** Cert stays cached (valid 90 days), TLS
handshake succeeds, but `proxyToAlias` returns 404. Correct behavior.

### 3.4 TLS Strategy: Wildcard First, Per-Alias Fallback

Let's Encrypt limits: 50 certs per registered domain per week. For the
"preview environments" use case (the killer feature), CI/CD can easily
create 50+ aliases per day. **Per-alias autocert will hit rate limits
within hours.**

The recommended TLS strategy is a **wildcard cert** for `*.deploy.bhatti.sh`:

**Option A: Bring your own wildcard cert (recommended, zero dependencies).**
Get a wildcard cert from any CA (Let's Encrypt via DNS-01, Cloudflare
Origin CA, etc.) and provide `tls_cert`/`tls_key` in config. One cert
covers all aliases. No per-alias issuance, no rate limits, no 2-5s
first-request latency.

```yaml
domain:
  api_host: "api.bhatti.sh"
  proxy_zone: "deploy.bhatti.sh"
  tls_cert: "/etc/bhatti/wildcard.pem"     # covers *.deploy.bhatti.sh + api.bhatti.sh
  tls_key: "/etc/bhatti/wildcard-key.pem"
```

**Option B: Per-alias autocert (fallback for simple setups).**
For self-hosters with few aliases (< 50/week), `acme_email` enables
automatic per-alias certificate issuance via Let's Encrypt HTTP-01.
First request to a new alias takes 2-5s (ACME challenge). All
subsequent requests use the cached cert.

```yaml
domain:
  api_host: "api.bhatti.sh"
  proxy_zone: "deploy.bhatti.sh"
  acme_email: "admin@bhatti.sh"  # per-alias certs, 50/week limit
```

- Cert cache at `<data_dir>/certs/`. Survives restart.
- **Warning logged on startup** when `acme_email` is set without
  `tls_cert`: "per-alias TLS is rate-limited to 50 new aliases/week.
  For preview environments, use a wildcard cert."

**Option C (future): Automated wildcard via DNS-01.** Bhatti could
do the DNS-01 challenge itself if given DNS provider credentials.
Out of scope for v1 — too many DNS providers to support.

### 3.5 Tests

- `TestHostPolicyAllowsAPIHost` → nil
- `TestHostPolicyAllowsPublishedAlias` → nil
- `TestHostPolicyUsesCache` — cached alias doesn't hit SQLite
- `TestHostPolicyRejectsUnknown` → error (no SQLite query)
- `TestHostBasedRouting` — request with `Host: my-app.deploy.bhatti.sh` hits proxy
- `TestAPIHostRouting` — request with `Host: api.bhatti.sh` goes through auth
- `TestUnknownHostReturns404`
- `TestHTTPSRedirect` — `:80` → 301 to HTTPS
- `TestLocalhostListenerInDomainMode` — `curl 127.0.0.1:8080/health` works
  when domain mode is active
- `TestWildcardCertServesBothAPIAndProxy` — single wildcard cert serves
  `api.bhatti.sh` and `*.deploy.bhatti.sh`

---

## Part 4: Lifecycle

**Sandbox destroy:** Publish rules deleted (§1.5). In-flight requests get 502.

**Sandbox stop (thermal):** Publish rules persist. Requests wake the sandbox
via `EnsureHot()`. This is the preview environment use case.

**Daemon restart:** Publish rules in SQLite, cert cache on disk — both persist.
No special recovery. Orphaned rules cleaned on startup (§2.6).

---

## Part 5: Security

**Public proxy is unauthenticated by design.** The publish action is
authenticated. Accessing the URL is not. Same as Vercel/Railway/Fly.

**Publish is opt-in.** No port is public by default.

**Rate limiting:** Per-alias (100 req/s) + global (10k req/s). Rate
limiter state is only allocated for aliases that exist in the publish
rules table. Unknown aliases get a fast 404 with zero allocation,
preventing memory exhaustion from alias probing.

**Resume bound:** Semaphore (capacity 10) bounds concurrent sandbox
resumes from public traffic. `singleflight.Group` coalesces concurrent
requests to the same sandbox, so 10 slots = 10 distinct sandbox wakeups,
not 10 requests. Excess requests get 503 + Retry-After.

**Request limits:** Per-request deadline of 5 minutes. Request body
limited to 50MB for non-GET/HEAD methods. Prevents slow-drip attacks
that hold tunnel FDs open indefinitely.

**WebSocket idle timeout:** 10-minute idle timeout on the bidirectional
relay. If neither side sends data for 10 minutes, both sides are closed.
Prevents FD exhaustion from abandoned connections.

**Tunnel FD safety:** `tunnelTransport` registers a `context.AfterFunc`
that closes the tunnel if the request context is cancelled (client
disconnect). The `tunnelBody.Close()` is idempotent via `sync.Once`.
No tunnel FD leak on any code path.

**Alias namespace:** Aliases are globally unique. On a multi-tenant
system, user A can squat on alias "google". This is acceptable for
bhatti's target deployment (single-tenant / small-team self-hosted).
If multi-tenancy becomes important, scope aliases per-user with URL
structure `<alias>.<user>.deploy.bhatti.sh`.

---

## Part 6: Observability

The public proxy **must** have request logging and metrics. Without
them, you can't debug issues, capacity plan, or understand usage.

### 6.1 Request Logging

Every public proxy request is logged with structured fields:

```go
slog.Info("public_proxy",
    "alias", alias,
    "method", r.Method,
    "path", path,
    "status", status,
    "duration_ms", elapsed.Milliseconds(),
    "client_ip", clientIP,
    "cold_wake", wasCold,     // true if EnsureHot was needed
    "cached_route", cached,  // true if route came from cache
)
```

Use `statusWriter` wrapper (already exists in server.go) to capture
the response status code.

### 6.2 Metrics

Add to the existing `/metrics` endpoint:

```go
// Public proxy counters
proxyRequestsTotal   atomic.Int64  // all public proxy requests
proxyRequestsError   atomic.Int64  // 4xx + 5xx responses
proxyColdWakes       atomic.Int64  // requests that triggered EnsureHot
proxyRateLimited     atomic.Int64  // 429 responses
proxyBusy            atomic.Int64  // 503 responses (semaphore full)
proxyWebSocketActive atomic.Int64  // currently open WS connections
```

Exposed as JSON in `/metrics` response alongside existing counters.

### 6.3 Tests

- `TestPublicProxyRequestLogging` — make proxy request, verify log output
  contains alias, status, duration
- `TestPublicProxyMetrics` — make proxy requests, check `/metrics`
  counters increment

---

## Implementation Phases

### Phase 0: Proxy Fix (ship independently)

The proxy streaming/flush bug (PR #2) is a standalone fix that benefits
existing users immediately. Ship it as its own commit/release before
starting the publish feature. No schema changes, no new endpoints.

1. `tunnelTransport` with context-aware FD cleanup (§0.1)
2. Replace `handleProxyHTTP` with `ReverseProxy` (§0.2)
3. Extract `proxyWebSocket` with idle timeout (§0.3)
4. Tests (§0.4)

### Phase 1: Publish API + Public Proxy

1. Add `publish_rules` table (§1.1)
2. Add store CRUD (§1.2)
3. Add publish handlers + destroy cleanup (§1.5)
4. Add orphan cleanup (§2.6)
5. Add `PublicProxyHandler` with route cache + singleflight (§2.1)
6. Add public rate limiter with LRU (§2.2)
7. Add `PublicProxyListen` config + listener (§2.3, §2.4)
8. Add `Server` options + struct fields (§1.6)
9. Add `publish` / `unpublish` CLI (§2.5)
10. Add observability (§6)
11. Tests

**Path-based routing (`/<alias>/path`) is for development/testing only.**
It is documented as such and ships in Phase 1 as a convenience for
testing without DNS. Relative URLs break with path-based proxying
(browser resolves `<a href="/about">` to `/about`, not `/<alias>/about`).
Host-based routing (Phase 2) does not have this problem. Users should
not build production workflows around path-based URLs.

```yaml
listen: ":8080"
public_proxy_listen: ":8443"  # development/testing only
```

### Phase 2: Domain Mode + TLS

1. Add `DomainConfig` (§2.3)
2. Host routing in `ServeHTTP` (§3.1)
3. `HostPolicy` with route cache (§3.3)
4. Wildcard cert support (§3.2, §3.4)
5. Per-alias `autocert` fallback (§3.2)
6. Localhost-only `:8080` in domain mode (§3.2)
7. Tests

```yaml
# Recommended: wildcard cert
domain:
  api_host: "api.bhatti.sh"
  proxy_zone: "deploy.bhatti.sh"
  tls_cert: "/etc/bhatti/wildcard.pem"
  tls_key: "/etc/bhatti/wildcard-key.pem"

# Fallback: per-alias autocert (< 50 aliases/week)
domain:
  api_host: "api.bhatti.sh"
  proxy_zone: "deploy.bhatti.sh"
  acme_email: "admin@bhatti.sh"
```

### Dependency Graph

```
Phase 0 (proxy fix) — standalone, ship first
     ↓
Phase 1 (publish + public proxy)
     ↓
Phase 2 (domain + TLS) — reuses 100% of Phase 1 code
```

---

## Migration: Dropping the Cloudflare Tunnel

1. Deploy Phase 2 code (domain not yet in config, `:8080` still works)
2. Add domain config, open ports 80/443 on firewall
3. Point `api.bhatti.sh` A record to agni-01 IP
4. Point `*.deploy.bhatti.sh` A record to agni-01 IP
5. Restart bhatti → starts on `:443` + `:80` + `127.0.0.1:8080`
6. Verify `curl https://api.bhatti.sh/health`
7. Verify `curl localhost:8080/health` (internal listener still works)
8. Kill `cloudflared`
9. CLI configs unchanged (`api_url: https://api.bhatti.sh`)
10. Internal monitoring unchanged (still hits `localhost:8080`)

---

## Dependencies

**New:**
- `golang.org/x/crypto/acme/autocert` — already transitively in `go.mod`
  via `golang.org/x/crypto` (v0.48.0).
- `golang.org/x/sync/singleflight` — already transitively in `go.mod`
  via `golang.org/x/sync`.
- `github.com/hashicorp/golang-lru/v2` — for bounded route cache and
  rate limiter. Tiny, zero transitive deps. Alternative: hand-roll LRU
  in ~60 lines.

**Existing reused:** `Engine.Tunnel()`, `store.Store`, `httputil.ReverseProxy`
(stdlib), `rateLimiter` patterns.

---

## File Impact

| File | Change | Phase |
|------|--------|-------|
| `pkg/server/routes.go` | `tunnelTransport` with context guard, `handleProxyHTTP` rewrite, `proxyWebSocket` with idle timeout | 0 |
| `pkg/config.go` | `PublicProxyListen`, `DomainConfig` | 1+2 |
| `pkg/store/store.go` | `publish_rules` table, CRUD, cleanup (incl. `unknown` status) | 1 |
| `pkg/server/server.go` | `ServerOption`, proxy fields, `HostPolicy` (cache-aware), Host routing, `initReservedAliases` | 1+2 |
| `pkg/server/routes.go` | Publish handlers, destroy cleanup + cache invalidation | 1 |
| `pkg/server/public_proxy.go` | **New.** `PublicProxyHandler`, `routeCache`, `singleflight` resume, LRU rate limiter, observability counters, request timeouts | 1+2 |
| `cmd/bhatti/main.go` | Path listener, domain listener, `autocert`, localhost listener in domain mode, orphan cleanup | 1+2 |
| `cmd/bhatti/cli.go` | `publish`, `unpublish` | 1 |

---

## Settled Decisions

1. **Built-in, not Caddy.** The tight `EnsureHot()` → `Tunnel()` coupling
   in the same goroutine is the feature. External proxies can't do this
   without races or leaked credentials. (Validated by PR #2.)

2. **`httputil.ReverseProxy` for HTTP.** Fixes streaming, hop-by-hop
   headers, flushing. Upgrades both authenticated and public proxy.

3. **Domain mode keeps `127.0.0.1:8080`.** External traffic goes to
   `:443`. Internal tools use localhost. No API keys over plain HTTP
   to external clients.

4. **One alias = one port.** Multiple aliases per sandbox (one per port).

5. **Resume semaphore = 10 + singleflight.** Bounds concurrent sandbox
   resumes (not requests). Concurrent requests to the same sandbox
   coalesce into one resume via `singleflight.Group`.

6. **Auto-alias: `<name>` (clean) or `<name>-p<port>` (multi-port).**
   First published port gets the clean alias. Collision retries with
   8-char random suffix, up to 3 attempts.

7. **Wildcard cert is recommended TLS path.** Per-alias autocert is
   the fallback. Preview environment use case hits LE rate limits fast.

8. **Route cache on hot path.** `alias → (engineID, sandboxID, port)`
   cached in-memory (LRU, 10K entries). Zero SQLite queries for hot
   sandboxes with cached routes. Cache invalidated on unpublish/destroy.

9. **Rate limit after alias lookup.** Unknown aliases get fast 404,
   no rate limiter state allocated. Prevents memory exhaustion from
   alias probing attacks.

## Open Questions

1. **Custom domains.** `app.example.com` → sandbox. Requires per-domain
   TLS + DNS verification. Out of scope.

2. **Auth on published URLs.** App's job. Could add optional basic auth
   to publish rules later.

3. **CORS headers.** Published sandbox URLs will be accessed from browsers
   on different origins. If the user's app doesn't set CORS headers,
   browser-based API calls fail. Consider adding optional
   `Access-Control-Allow-Origin: *` to publish rules for preview
   environments. Low priority — this is the app's concern.
