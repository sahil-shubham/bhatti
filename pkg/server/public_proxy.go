package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/store"
	"golang.org/x/sync/singleflight"
)

var errServerBusy = fmt.Errorf("server busy")

// PublicProxyHandler serves unauthenticated public traffic to published
// sandbox ports. It resolves aliases to sandbox routes, wakes cold
// sandboxes, and proxies HTTP/WebSocket traffic through engine tunnels.
type PublicProxyHandler struct {
	engine      engine.Engine
	store       *store.Store
	limiter     *publicRateLimiter
	resumeSem   chan struct{}
	resumeGroup singleflight.Group // coalesces concurrent resumes per sandbox
	routeCache  *routeCache        // in-memory alias → route mapping
	onActivity  func(engineID string) // called on every request to signal thermal manager
	onEnsureHot func(ctx context.Context, engineID string) error // canonical wake logic (delegates to Server.ensureHot)

	// Observability
	onRecordEvent func(store.Event) // optional callback to record events
	requestsTotal   atomic.Int64
	requestsError   atomic.Int64
	coldWakes       atomic.Int64
	rateLimited     atomic.Int64
	busy            atomic.Int64
	webSocketActive atomic.Int64
}

// NewPublicProxyHandler creates a new public proxy handler.
// onActivity is called on every proxied request to keep the thermal manager informed.
// onEnsureHot is the canonical wake function (typically Server.EnsureHot) — it handles
// touchActivity, store status updates, and VM state persistence. PublicProxyHandler
// wraps it with singleflight coalescing and bounded concurrency.
func NewPublicProxyHandler(eng engine.Engine, st *store.Store, resumeSem chan struct{}, onActivity func(engineID string), onEnsureHot func(ctx context.Context, engineID string) error) *PublicProxyHandler {
	return &PublicProxyHandler{
		engine:      eng,
		store:       st,
		limiter:     newPublicRateLimiter(),
		resumeSem:   resumeSem,
		routeCache:  newRouteCache(),
		onActivity:  onActivity,
		onEnsureHot: onEnsureHot,
	}
}

// SetRecordEvent sets the event recording callback.
func (h *PublicProxyHandler) SetRecordEvent(fn func(store.Event)) {
	h.onRecordEvent = fn
}

// --- Route Cache ---

// resolvedRoute is the cached result of an alias lookup.
type resolvedRoute struct {
	engineID  string
	sandboxID string
	port      int
}

// routeCache maps alias → resolvedRoute. Bounded to 10K entries (LRU eviction).
// Invalidated on unpublish and sandbox destroy.
type routeCache struct {
	mu      sync.RWMutex
	entries map[string]*routeCacheEntry
}

type routeCacheEntry struct {
	route      resolvedRoute
	lastAccess time.Time
}

const routeCacheMaxSize = 10_000

func newRouteCache() *routeCache {
	return &routeCache{
		entries: make(map[string]*routeCacheEntry),
	}
}

func (rc *routeCache) Get(alias string) (resolvedRoute, bool) {
	rc.mu.RLock()
	e, ok := rc.entries[alias]
	rc.mu.RUnlock()
	if !ok {
		return resolvedRoute{}, false
	}
	// Update access time under write lock (for LRU eviction)
	rc.mu.Lock()
	e.lastAccess = time.Now()
	rc.mu.Unlock()
	return e.route, true
}

func (rc *routeCache) Set(alias string, route resolvedRoute) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	// Evict oldest if at capacity
	if len(rc.entries) >= routeCacheMaxSize {
		var oldestKey string
		var oldestTime time.Time
		for k, v := range rc.entries {
			if oldestKey == "" || v.lastAccess.Before(oldestTime) {
				oldestKey = k
				oldestTime = v.lastAccess
			}
		}
		if oldestKey != "" {
			delete(rc.entries, oldestKey)
		}
	}
	rc.entries[alias] = &routeCacheEntry{
		route:      route,
		lastAccess: time.Now(),
	}
}

func (rc *routeCache) Invalidate(alias string) {
	rc.mu.Lock()
	delete(rc.entries, alias)
	rc.mu.Unlock()
}

// InvalidateSandbox removes all cached routes for a sandbox.
func (rc *routeCache) InvalidateSandbox(sandboxID string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	for k, v := range rc.entries {
		if v.route.sandboxID == sandboxID {
			delete(rc.entries, k)
		}
	}
}

// --- Rate Limiter ---

// publicRateLimiter uses a three-tier token bucket scheme:
//   - Per-IP (primary): prevents one source from overwhelming the proxy.
//     Generous burst to accommodate Vite/webpack dev servers that serve
//     each source file as a separate ESM module request (100-300+ per page load).
//   - Per-alias (secondary): aggregate safety net against distributed attacks
//     where many IPs target a single published alias.
//   - Global: protects the proxy process from total overload.
//
// Both per-IP and per-alias maps are bounded with LRU eviction.
type publicRateLimiter struct {
	mu       sync.Mutex
	perIP    map[string]*publicBucket // primary: per source IP
	perAlias map[string]*publicBucket // secondary: aggregate per alias
	global   *tokenBucket
}

type publicBucket struct {
	bucket     *tokenBucket
	lastAccess time.Time
}

const publicRateLimiterMaxSize = 10_000

func newPublicRateLimiter() *publicRateLimiter {
	return &publicRateLimiter{
		perIP:    make(map[string]*publicBucket),
		perAlias: make(map[string]*publicBucket),
		global:   newTokenBucket(10000, 10000),
	}
}

// Allow checks rate limits: global → per-IP → per-alias.
// Only called for aliases known to exist.
func (l *publicRateLimiter) Allow(alias, ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.global.allow() {
		return false
	}

	// Primary: per source IP
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

// getOrCreate returns the token bucket for key, creating one if needed.
// Evicts the oldest entry if the map exceeds publicRateLimiterMaxSize.
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

// evictOldest removes the least-recently-accessed entry from the map.
func evictOldest(m map[string]*publicBucket) {
	var oldestKey string
	var oldestTime time.Time
	for k, v := range m {
		if oldestKey == "" || v.lastAccess.Before(oldestTime) {
			oldestKey = k
			oldestTime = v.lastAccess
		}
	}
	if oldestKey != "" {
		delete(m, oldestKey)
	}
}

// extractIP strips the port from a RemoteAddr "host:port" string.
func extractIP(remoteAddr string) string {
	ip, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr // bare IP, no port
	}
	return ip
}

// --- Path-Based Routing ---

// ServeHTTPPathBased handles requests with path-based alias routing:
// /<alias>/rest/of/path
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

// --- Core Proxy Logic ---

func (h *PublicProxyHandler) proxyToAlias(w http.ResponseWriter, r *http.Request, alias, path string) {
	start := time.Now()
	h.requestsTotal.Add(1)

	// Hard per-request deadline: 5 minutes.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	r = r.WithContext(ctx)

	// Resolve alias → route. Cache first, then SQLite.
	// Alias lookup BEFORE rate limiting — unknown aliases get fast 404.
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

	// Rate limit AFTER alias validation.
	clientIP := extractIP(r.RemoteAddr)
	if !h.limiter.Allow(alias, clientIP) {
		h.rateLimited.Add(1)
		w.Header().Set("Retry-After", "1")
		errResp(w, 429, "rate limit exceeded")
		return
	}

	// Limit request body for non-GET/HEAD (public internet).
	if r.Method != "GET" && r.Method != "HEAD" {
		r.Body = http.MaxBytesReader(w, r.Body, 50<<20) // 50MB
	}

	// Wake sandbox with bounded concurrency + singleflight coalescing.
	wasCold := false
	if te, ok := h.engine.(ThermalEngine); ok {
		if err := h.ensureHotBounded(ctx, te, route.engineID); err != nil {
			if err == errServerBusy {
				h.busy.Add(1)
				w.Header().Set("Retry-After", "2")
				errResp(w, 503, "server busy, retry shortly")
			} else {
				h.requestsError.Add(1)
				h.routeCache.Invalidate(alias)
				errResp(w, 502, "sandbox unavailable")
			}
			return
		}
		wasCold = true // conservative: we called ensureHotBounded
	}

	// Signal activity on every request (HTTP and WebSocket) so the
	// thermal manager knows this sandbox has public traffic.
	if h.onActivity != nil {
		h.onActivity(route.engineID)
	}

	// WebSocket
	if websocket.IsWebSocketUpgrade(r) {
		h.webSocketActive.Add(1)
		var activityFn func()
		if h.onActivity != nil {
			eid := route.engineID
			activityFn = func() { h.onActivity(eid) }
		}
		proxyWebSocket(w, r, h.engine, route.engineID, route.port, path, activityFn)
		h.webSocketActive.Add(-1)
		h.logRequest(alias, r.Method, path, 101, time.Since(start), wasCold, cached)
		return
	}

	// HTTP — wrap response to capture status
	sw := &statusWriter{ResponseWriter: w, status: 200}
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
			req.Header.Set("X-Forwarded-Proto", schemeOf(r))
			req.Header.Set("X-Forwarded-Host", r.Host)
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			h.requestsError.Add(1)
			slog.Warn("public proxy error", "alias", alias, "error", err)
			if h.onRecordEvent != nil {
				h.onRecordEvent(store.Event{
					Type: "proxy.error",
					Meta: map[string]any{"alias": alias, "status": 502, "error": err.Error(), "duration_ms": time.Since(start).Milliseconds()},
				})
			}
			errResp(w, 502, "bad gateway")
		},
	}
	proxy.ServeHTTP(sw, r)

	if sw.status >= 400 {
		h.requestsError.Add(1)
	}
	h.logRequest(alias, r.Method, path, sw.status, time.Since(start), wasCold, cached)
}

// ensureHotBounded wraps the canonical ensureHot callback with singleflight
// coalescing and semaphore-based concurrency limiting. The actual wake logic
// (touchActivity, store update, saveVMState) lives in Server.ensureHot —
// we only add public-proxy-specific concerns here.
func (h *PublicProxyHandler) ensureHotBounded(ctx context.Context, te ThermalEngine, engineID string) error {
	thermal := te.ThermalState(engineID)
	if thermal == "hot" {
		return nil
	}

	h.coldWakes.Add(1)
	_, err, _ := h.resumeGroup.Do(engineID, func() (interface{}, error) {
		select {
		case h.resumeSem <- struct{}{}:
			defer func() { <-h.resumeSem }()
		default:
			return nil, errServerBusy
		}
		return nil, h.onEnsureHot(ctx, engineID)
	})
	return err
}

func (h *PublicProxyHandler) logRequest(alias, method, path string, status int, elapsed time.Duration, coldWake, cached bool) {
	slog.Info("public_proxy",
		"alias", alias,
		"method", method,
		"path", path,
		"status", status,
		"duration_ms", elapsed.Milliseconds(),
		"cold_wake", coldWake,
		"cached_route", cached,
	)
}

// Metrics returns a snapshot of public proxy metrics.
func (h *PublicProxyHandler) Metrics() map[string]interface{} {
	return map[string]interface{}{
		"requests_total":    h.requestsTotal.Load(),
		"requests_error":    h.requestsError.Load(),
		"cold_wakes":        h.coldWakes.Load(),
		"rate_limited":      h.rateLimited.Load(),
		"busy":              h.busy.Load(),
		"websocket_active":  h.webSocketActive.Load(),
	}
}

// --- helpers (reused from server.go, avoid import cycle) ---

// publishedURL generates the public URL for an alias.
func publishedURL(alias, proxyZone, publicProxyAddr string) string {
	if proxyZone != "" {
		return "https://" + alias + "." + proxyZone
	}
	if publicProxyAddr != "" {
		return "http://" + publicProxyAddr + "/" + alias + "/"
	}
	return "(no public proxy configured) alias: " + alias
}
