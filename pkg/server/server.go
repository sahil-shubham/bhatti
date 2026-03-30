package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

type contextKey string

const userContextKey contextKey = "user"
const requestIDKey contextKey = "request_id"

// ThermalEngine is optionally implemented by engines that support thermal management.
type ThermalEngine interface {
	EnsureHot(ctx context.Context, id string) error
	ThermalState(id string) string
	Pause(ctx context.Context, id string) error
	Activity(ctx context.Context, id string) (*proto.ActivityInfo, error)
}

// ThermalConfig controls automatic thermal transitions.
type ThermalConfig struct {
	WarmTimeout time.Duration // idle → warm (default 30s)
	ColdTimeout time.Duration // warm idle → cold (default 30min)
}

// Server is the HTTP API server.
type Server struct {
	engine       engine.Engine
	store        *store.Store
	dataDir      string // path to data directory (for age.key)
	mux          *http.ServeMux
	limiter      *rateLimiter
	stopThermal     context.CancelFunc
	stopTaskCleanup context.CancelFunc
	startTime       time.Time
	lastActivity sync.Map // engineID → time.Time — host-side activity cache

	// Task cancellation for async operations (image pull)
	pullCancelMu sync.Mutex
	pullCancels  map[string]context.CancelFunc // taskID → cancel

	// Request counters for /metrics
	requestTotal  atomic.Int64
	requestErrors atomic.Int64
	authFailures  atomic.Int64

	// Public proxy (set via options)
	proxyZone       string              // e.g. "deploy.bhatti.sh"
	apiHost         string              // e.g. "api.bhatti.sh"
	publicProxyAddr string              // e.g. "host:8443" (for URL generation)
	publicProxy     *PublicProxyHandler // nil until configured
	resumeSem       chan struct{}       // bounds concurrent cold resumes
}

// touchActivity records that a sandbox was accessed via the API.
// The thermal manager checks this before querying the guest agent,
// avoiding a TCP connection per sandbox per thermal cycle.
func (s *Server) touchActivity(engineID string) {
	s.lastActivity.Store(engineID, time.Now())
}

// ServerOption configures the server.
type ServerOption func(*Server)

// WithProxyZone sets the subdomain zone for published sandbox ports.
func WithProxyZone(zone string) ServerOption {
	return func(s *Server) { s.proxyZone = zone }
}

// WithAPIHost sets the hostname for the authenticated API.
func WithAPIHost(host string) ServerOption {
	return func(s *Server) { s.apiHost = host }
}

// WithPublicProxyAddr sets the address used for generating public URLs.
func WithPublicProxyAddr(addr string) ServerOption {
	return func(s *Server) { s.publicProxyAddr = addr }
}

// New creates a new API server. dataDir is the path to the data directory
// containing age.key for secret encryption.
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

// ResumeSem returns the resume semaphore for sharing with PublicProxyHandler.
func (s *Server) ResumeSem() chan struct{} { return s.resumeSem }

// SetPublicProxy sets the public proxy handler (called after server creation
// when the handler is created in main.go with shared resumeSem).
func (s *Server) SetPublicProxy(h *PublicProxyHandler) { s.publicProxy = h }

// HostPolicy returns nil for hosts that should be issued TLS certificates.
// Used by autocert.Manager during TLS handshake for uncached certs.
// Checks in-memory route cache first to avoid SQLite queries from
// unauthenticated TLS handshakes (DoS protection).
func (s *Server) HostPolicy(_ context.Context, host string) error {
	if host == s.apiHost {
		return nil
	}
	if s.proxyZone != "" && strings.HasSuffix(host, "."+s.proxyZone) {
		alias := strings.TrimSuffix(host, "."+s.proxyZone)
		// Fast path: check in-memory route cache
		if s.publicProxy != nil {
			if _, ok := s.publicProxy.routeCache.Get(alias); ok {
				return nil
			}
		}
		// Slow path: SQLite lookup
		if _, err := s.store.GetPublishRuleByAlias(alias); err != nil {
			return fmt.Errorf("no publish rule for %q", alias)
		}
		return nil
	}
	return fmt.Errorf("unknown host: %s", host)
}

// stripPort removes the port from a host:port string.
func stripPort(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// startTaskCleanup runs a background goroutine that deletes completed/failed
// tasks older than 24 hours. Never deletes running tasks.
func (s *Server) startTaskCleanup() {
	ctx, cancel := context.WithCancel(context.Background())
	s.stopTaskCleanup = cancel
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if n, err := s.store.CleanupOldTasks(24 * time.Hour); err != nil {
					slog.Warn("task cleanup failed", "error", err)
				} else if n > 0 {
					slog.Info("cleaned up old tasks", "count", n)
				}
			}
		}
	}()
}

// Close stops background goroutines (thermal manager, task cleanup).
func (s *Server) Close() {
	if s.stopThermal != nil {
		s.stopThermal()
	}
	if s.stopTaskCleanup != nil {
		s.stopTaskCleanup()
	}
}

// StartThermalManager starts the background goroutine that transitions idle
// sandboxes through thermal states: hot → warm → cold.
func (s *Server) StartThermalManager(cfg ThermalConfig) {
	te, ok := s.engine.(ThermalEngine)
	if !ok {
		return // engine doesn't support thermal management
	}
	if cfg.WarmTimeout == 0 {
		cfg.WarmTimeout = 30 * time.Second
	}
	if cfg.ColdTimeout == 0 {
		cfg.ColdTimeout = 30 * time.Minute
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.stopThermal = cancel

	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.runThermalCycle(te, cfg)
			}
		}
	}()
}

func (s *Server) runThermalCycle(te ThermalEngine, cfg ThermalConfig) {
	sandboxes, err := s.store.ListAllSandboxes()
	if err != nil {
		return
	}
	for _, sb := range sandboxes {
		if sb.Status != "running" {
			continue
		}

		thermal := te.ThermalState(sb.EngineID)
		if thermal == "cold" || thermal == "" {
			continue
		}

		// Fast path: check host-side activity cache. If the sandbox had
		// API activity within warmTimeout, skip the agent query entirely.
		// This avoids opening a TCP connection per sandbox per cycle.
		if ts, ok := s.lastActivity.Load(sb.EngineID); ok {
			if time.Since(ts.(time.Time)) < cfg.WarmTimeout {
				continue // definitely active, skip agent query
			}
		}

		// Slow path: ask the agent for authoritative activity info.
		actCtx, actCancel := context.WithTimeout(context.Background(), 5*time.Second)
		activity, err := te.Activity(actCtx, sb.EngineID)
		actCancel()
		if err != nil {
			continue
		}

		idle := time.Since(time.Unix(activity.LastActivityUnix, 0))

		if thermal == "hot" && idle > cfg.WarmTimeout && activity.AttachedSessions == 0 {
			if err := te.Pause(context.Background(), sb.EngineID); err != nil {
				slog.Warn("thermal pause failed", "sandbox", sb.Name, "error", err)
				continue
			}
			slog.Info("thermal transition", "sandbox", sb.Name, "from", "hot", "to", "warm", "idle", idle.Round(time.Second))
		}

		if thermal == "warm" && idle > cfg.ColdTimeout {
			if err := s.engine.Stop(context.Background(), sb.EngineID); err != nil {
				slog.Warn("thermal snapshot failed", "sandbox", sb.Name, "error", err)
				continue
			}
			s.saveVMState(sb.ID, sb.EngineID)
			slog.Info("thermal transition", "sandbox", sb.Name, "from", "warm", "to", "cold", "idle", idle.Round(time.Second))
		}
	}
}

// ensureHot transparently wakes a sandbox from warm or cold state.
// Also touches the host-side activity cache so the thermal manager
// knows this sandbox was recently accessed without querying the agent.
// Returns nil if the engine doesn't support thermal management.
func (s *Server) ensureHot(ctx context.Context, engineID string) error {
	s.touchActivity(engineID)
	te, ok := s.engine.(ThermalEngine)
	if !ok {
		return nil
	}
	return te.EnsureHot(ctx, engineID)
}


// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Domain mode: route by Host header BEFORE auth.
	// Proxy zone subdomains are served unauthenticated.
	// API host falls through to normal auth flow.
	if s.proxyZone != "" {
		host := stripPort(r.Host)
		if strings.HasSuffix(host, "."+s.proxyZone) {
			alias := strings.TrimSuffix(host, "."+s.proxyZone)
			if s.publicProxy != nil {
				s.publicProxy.proxyToAlias(w, r, alias, r.URL.Path)
			} else {
				errResp(w, 503, "public proxy not configured")
			}
			return
		}
		if s.apiHost != "" && host != s.apiHost && host != "localhost" && host != "127.0.0.1" {
			errResp(w, 404, "unknown host")
			return
		}
		// host == apiHost or localhost → fall through to auth
	}

	// Normalize path before any checks to prevent path confusion attacks
	cleanPath := path.Clean(r.URL.Path)

	// Unauthenticated endpoints (exact match only)
	if cleanPath == "/health" || cleanPath == "/metrics" {
		s.mux.ServeHTTP(w, r)
		return
	}

	// Extract bearer token from Authorization header only.
	// No query parameter auth — eliminates token-in-URL leakage.
	authHeader := r.Header.Get("Authorization")
	token := strings.TrimPrefix(authHeader, "Bearer ")
	if token == "" || token == authHeader {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authorization required"})
		return
	}

	// Hash the incoming token and look up user by hash.
	hash := sha256Hex(token)
	user, err := s.store.GetUserByKeyHash(hash)
	if err != nil {
		s.authFailures.Add(1)
		slog.Warn("auth.failed", "ip", r.RemoteAddr)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid api key"})
		return
	}

	// Rate limiting per user
	if !s.limiter.Allow(user.ID, r) {
		errResp(w, 429, "rate limit exceeded")
		return
	}

	// Generate request ID and attach user + request ID to context
	reqID := generateRequestID()
	ctx := context.WithValue(r.Context(), requestIDKey, reqID)
	ctx = context.WithValue(ctx, userContextKey, user)

	// Request logging
	start := time.Now()
	wrapped := &statusWriter{ResponseWriter: w, status: 200}
	s.mux.ServeHTTP(wrapped, r.WithContext(ctx))

	s.requestTotal.Add(1)
	if wrapped.status >= 500 {
		s.requestErrors.Add(1)
	}

	slog.Info("request",
		"request_id", reqID,
		"method", r.Method,
		"path", cleanPath,
		"status", wrapped.status,
		"duration_ms", time.Since(start).Milliseconds(),
		"user", user.Name,
		"user_id", user.ID,
	)
}

// statusWriter wraps http.ResponseWriter to capture the status code.
// It preserves Flusher and Hijacker interfaces for streaming and WebSocket.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("hijack not supported")
}

func generateRequestID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "req_" + hex.EncodeToString(b)
}

// RequestIDFromContext extracts the request ID from context.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// UserFromContext extracts the authenticated user from the request context.
func UserFromContext(ctx context.Context) *store.User {
	u, _ := ctx.Value(userContextKey).(*store.User)
	return u
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("json encode failed", "error", err)
	}
}

func readJSON(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20) // 1MB limit
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

// errResp is a helper for error responses.
func errResp(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
