# Fix: Published Vite dev-server apps hit 429 during initial page load

Issue: [#6](https://github.com/sahil-shubham/bhatti/issues/6)

---

## Summary

Published sandbox URLs return HTTP 429 during a Vite dev-server page
load, causing the app to render a blank screen. Vite serves each
source file as a separate native ESM module — a modest React app
generates 90-200+ HTTP requests on a single page load. The current
per-alias rate limit (burst 100, 200/min) is exhausted by one
browser's page load, and a reload seconds later gets 429s on every
request past the ~12 tokens that refilled.

Reproduced locally: 91-request Vite app loads fine on first visit,
then **18 out of 91 requests get 429** on immediate reload → blank page.
Production-built app (`vite build && vite preview`) serves 2 files
and never hits the limit — confirming this is dev-server specific.

---

## Root Cause

The public proxy rate limiter in `pkg/server/public_proxy.go` uses a
**per-alias** token bucket as its primary enforcement:

```go
pb = &publicBucket{
    bucket:     newTokenBucket(100, 200),  // 100 burst, 200/min (~3.3/sec refill)
}
```

This is the wrong dimension. Per-alias means every visitor to a
published URL shares one bucket. One developer's page load drains
the budget for all users of that alias — including themselves on
the next reload.

The fundamental problem: **abuse comes from a source (IP), not from
a destination (alias).** The standard pattern for a public-facing
reverse proxy is per-source-IP rate limiting — this is what nginx's
`limit_req_zone` does (`$binary_remote_addr`), what Cloudflare uses,
and what AWS WAF uses. The destination-level limit should exist as
an aggregate safety net against distributed attacks, not as the
primary enforcement.

No number works for a flat per-alias bucket. Too low → breaks Vite.
Too high → no abuse protection.

---

## The Fix

Re-key the primary rate limit to **per-IP**, keep per-alias as a
raised aggregate backstop. Three tiers:

| Tier | Key | Purpose | Bucket |
|---|---|---|---|
| Per-IP (new, primary) | client IP | One browser's burst doesn't affect others | `newTokenBucket(500, 1500)` — burst 500, sustained 25/sec |
| Per-alias (existing, raised) | alias | Distributed attack safety net | `newTokenBucket(2000, 5000)` — burst 2000, sustained 83/sec |
| Global (existing, unchanged) | — | Protects the whole proxy | `newTokenBucket(10000, 10000)` — burst 10k, sustained 166/sec |

The per-IP bucket of `(burst=500, sustained=1500/min)` means:
- A large Vite page load of 500 modules fits in one burst (enterprise
  React apps like Appsmith have 5000+ source files with minimal code
  splitting — a single route easily loads 300-500 modules)
- Sustained rate of 25/sec covers rapid navigation and HMR
- Full recovery after 20 seconds, allowing fast dev reload cycles
- An attacker from one IP can't do more than 500 req/burst

The per-alias bucket of `(burst=2000, sustained=5000/min)` means:
- 10 developers can each reload simultaneously without hitting it
- A botnet of 100 IPs each doing 20 req/s would hit it (~83/sec
  sustained vs the alias cap)

`r.RemoteAddr` is the real client IP — bhatti terminates TLS directly
on `:443` via `ListenAndServeTLS`, no load balancer in front.

### 1. Add `perIP` map to `publicRateLimiter`

**`pkg/server/public_proxy.go`:**

```go
type publicRateLimiter struct {
    mu       sync.Mutex
    perIP    map[string]*publicBucket  // primary: per source IP
    perAlias map[string]*publicBucket  // secondary: aggregate per alias
    global   *tokenBucket
}

func newPublicRateLimiter() *publicRateLimiter {
    return &publicRateLimiter{
        perIP:    make(map[string]*publicBucket),
        perAlias: make(map[string]*publicBucket),
        global:   newTokenBucket(10000, 10000),
    }
}
```

Same LRU-bounded map pattern already used for `perAlias`. Bounded
to 10K entries with oldest-eviction. Two maps share the same
`publicBucket` type and eviction logic — factor out the get-or-create
into a helper.

### 2. Change `Allow` signature to accept IP

```go
// Allow checks rate limits: global → per-IP → per-alias.
func (l *publicRateLimiter) Allow(alias, ip string) bool {
    l.mu.Lock()
    defer l.mu.Unlock()

    if !l.global.allow() {
        return false
    }

    // Primary: per-IP
    ipb := l.getOrCreate(l.perIP, ip, 500, 1500)
    if !ipb.allow() {
        return false
    }

    // Secondary: per-alias aggregate
    ab := l.getOrCreate(l.perAlias, alias, 2000, 5000)
    if !ab.allow() {
        return false
    }

    return true
}
```

The `getOrCreate` helper deduplicates the LRU-bounded map pattern:

```go
func (l *publicRateLimiter) getOrCreate(
    m map[string]*publicBucket, key string,
    burst, perMin float64,
) *tokenBucket {
    pb, ok := m[key]
    if !ok {
        if len(m) >= publicRateLimiterMaxSize {
            evictOldest(m)
        }
        pb = &publicBucket{
            bucket:     newTokenBucket(burst, perMin),
            lastAccess: time.Now(),
        }
        m[key] = pb
    }
    pb.lastAccess = time.Now()
    return pb.bucket
}
```

### 3. Extract client IP in `proxyToAlias` and pass to `Allow`

**`pkg/server/public_proxy.go` — `proxyToAlias`:**

```go
// Rate limit AFTER alias validation.
clientIP := extractIP(r.RemoteAddr)
if !h.limiter.Allow(alias, clientIP) {
```

Extract a small `extractIP` helper (strips port from `host:port`):

```go
func extractIP(remoteAddr string) string {
    ip, _, err := net.SplitHostPort(remoteAddr)
    if err != nil {
        return remoteAddr // bare IP, no port
    }
    return ip
}
```

This is the only call site change. `ServeHTTPPathBased` calls
`proxyToAlias` which already has `r`.

---

## Tests

### Unit tests in `pkg/server/public_proxy_test.go`

No mocking needed — `publicRateLimiter` is a standalone struct.

**`TestPublicRateLimiter_PerIPIsolation`** — two IPs don't interfere:
- IP-A sends 200 requests to alias "app" → all allowed
- IP-B sends 200 requests to alias "app" → all allowed (independent bucket)
- IP-A sends 1 more → rejected (its bucket is drained)

**`TestPublicRateLimiter_PerAliasAggregate`** — distributed attack:
- 20 different IPs each send 150 requests to alias "app"
  (each under their own per-IP burst of 200)
- Per-alias aggregate (burst 2000) is exhausted → requests start
  getting rejected even though individual IPs have budget left

**`TestPublicRateLimiter_ViteDevReload`** — the exact repro scenario:
- Single IP sends 91 requests (Vite page load) → all pass
- Wait 0ms (simulate immediate reload)
- Same IP sends 91 requests → most should fail (per-IP burst exhausted)
- Wait enough for ~91 tokens to refill → send 91 again → all pass

**`TestPublicRateLimiter_GlobalLimit`** — global cap still works:
- Exhaust the global bucket (10K requests from many IPs)
- Next request rejected regardless of IP or alias

**`TestPublicRateLimiter_LRUEviction`** — memory bounded:
- Insert > 10K unique IPs
- Map size stays at 10K

### Existing test updates

`TestPublicProxy429` in `pkg/server/server_test.go` (if it exists) —
update to account for per-IP keying. Currently may test per-alias
exhaustion; needs to send from the same simulated IP.

---

## Implementation Order

Single phase — all changes are in `pkg/server/public_proxy.go` plus
tests. No cross-package dependencies.

1. Add `perIP` field and `getOrCreate`/`evictOldest` helpers
2. Change `Allow(alias)` → `Allow(alias, ip string)`
3. Add `extractIP` helper
4. Update `proxyToAlias` call site
5. Raise per-alias bucket: `(100, 200)` → `(2000, 5000)`
6. Write unit tests
7. Verify existing tests pass

---

## What's Not in This Plan

**Per-IP-per-alias composite key.** Considered and rejected. A
developer typically works on one sandbox at a time. Per-IP globally
is simpler, uses less memory, and limits total per-source throughput
regardless of how many aliases they spray across. The per-alias
aggregate catches the multi-target case.

**Configurable rate limits in `config.yaml`.** Not worth it yet. The
new defaults handle the Vite use case and still protect against abuse.
If a specific deployment needs tuning, we can add config knobs later
without changing the architecture.

**`Retry-After` header with accurate backoff.** The current hardcoded
`Retry-After: 1` is fine. Browsers and Vite don't honor it for ESM
module loads anyway — the request simply fails and the module graph
breaks. The fix is to not 429 in the first place.

**X-Forwarded-For / X-Real-IP parsing.** Bhatti terminates TLS
directly (`ListenAndServeTLS` on `:443`). `r.RemoteAddr` is the real
client. If bhatti ever goes behind a load balancer, we'd need to
trust a forwarded header — but that's a separate change with its own
security considerations (header spoofing).

**WebSocket exemption.** WebSocket upgrades already pass through the
rate limiter as a single request (the upgrade handshake). The long-lived
connection itself isn't rate-limited per-message. No change needed.
