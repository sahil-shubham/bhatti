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
	BalloonSet(ctx context.Context, id string, amountMiB int64) error
	MemSizeMib(id string) int64
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
	thermalDone     chan struct{}       // closed when thermal goroutine exits
	stopTaskCleanup context.CancelFunc
	startTime       time.Time
	lastActivity     sync.Map // engineID → time.Time — host-side activity cache
	snapshotFailures sync.Map // engineID → int — consecutive snapshot failure count
	thermalFails     sync.Map // engineID → int — consecutive Activity query failures

	// Task cancellation for async operations (image pull)
	pullCancelMu sync.Mutex
	pullCancels  map[string]context.CancelFunc // taskID → cancel

	// Request counters for /metrics
	requestTotal  atomic.Int64
	requestErrors atomic.Int64
	authFailures  atomic.Int64

	// Public proxy (set via options)
	proxyZone       string              // e.g. "bhatti.sh"
	apiHost         string              // e.g. "api.bhatti.sh" (must be under proxyZone)
	publicProxyAddr string              // e.g. "host:8443" (for URL generation)
	publicProxy     *PublicProxyHandler // nil until configured
	resumeSem       chan struct{}       // bounds concurrent cold resumes
}

// maxThermalFailures is the number of consecutive Activity query failures
// before a sandbox is force-paused. At 10-second thermal intervals this
// means ~100 seconds of agent silence. keep_hot sandboxes are exempt.
const maxThermalFailures = 10

// incrementThermalFails bumps and returns the consecutive failure count.
func (s *Server) incrementThermalFails(engineID string) int {
	val, _ := s.thermalFails.LoadOrStore(engineID, 0)
	count := val.(int) + 1
	s.thermalFails.Store(engineID, count)
	return count
}

// resetThermalFails clears the failure counter for an engine.
func (s *Server) resetThermalFails(engineID string) {
	s.thermalFails.Delete(engineID)
}

// touchActivity records that a sandbox was accessed via the API.
// The thermal manager checks this before querying the guest agent,
// avoiding a TCP connection per sandbox per thermal cycle.
func (s *Server) touchActivity(engineID string) {
	s.lastActivity.Store(engineID, time.Now())
	s.snapshotFailures.Delete(engineID) // reset retry counter on user activity
	s.resetThermalFails(engineID)        // reset thermal failure counter on user activity
}

// TouchActivity is the exported version of touchActivity for use by
// PublicProxyHandler's WebSocket activity callback (wired in main.go).
func (s *Server) TouchActivity(engineID string) {
	s.touchActivity(engineID)
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
// It waits for the thermal manager to finish its current cycle before
// returning, preventing races between a mid-cycle Stop() and SnapshotAll().
func (s *Server) Close() {
	if s.stopThermal != nil {
		s.stopThermal()
		// Wait for the thermal goroutine to finish its current cycle.
		// Without this, SnapshotAll() can race with a mid-cycle thermal
		// Stop(), causing double-stop or missed saveVMState.
		if s.thermalDone != nil {
			<-s.thermalDone
		}
	}
	if s.stopTaskCleanup != nil {
		s.stopTaskCleanup()
	}
}

// SnapshotAll stops every hot or warm sandbox so it has a snapshot on disk.
// Called during graceful shutdown so that recoverVMs can restore them on
// the next startup.
//
// Sandboxes are snapshotted in parallel (bounded to 10 concurrent) to avoid
// a 25-minute sequential shutdown with 50 VMs. On failure, the VM is left
// as-is and marked in the store so recovery can detect it.
func (s *Server) SnapshotAll() {
	slog.Info("snapshotting all running VMs before shutdown")
	sandboxes, err := s.store.ListAllSandboxes()
	if err != nil {
		slog.Warn("snapshot-all: list sandboxes", "error", err)
		return
	}

	// Collect running sandboxes
	var running []store.Sandbox
	for _, sb := range sandboxes {
		if sb.Status == "running" {
			running = append(running, sb)
		}
	}
	if len(running) == 0 {
		slog.Info("snapshot-all: no running VMs")
		return
	}

	slog.Info("snapshot-all: starting", "count", len(running))

	// Parallel with bounded concurrency. 10 concurrent snapshots means
	// 50 VMs finish in ~5 batches × 30s = 2.5min instead of 25min.
	const maxParallel = 10
	sem := make(chan struct{}, maxParallel)
	var snapped, failed atomic.Int32
	var wg sync.WaitGroup

	for _, sb := range running {
		wg.Add(1)
		go func(sb store.Sandbox) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			err := s.engine.Stop(ctx, sb.EngineID, engine.StopOpts{ForceFullSnapshot: true})
			if err != nil {
				// Retry once with a fresh context — transient failures
				// (FC API timeout, disk hiccup) often succeed on retry.
				slog.Warn("snapshot-all: first attempt failed, retrying",
					"sandbox", sb.Name, "id", sb.ID, "error", err)
				retryCtx, retryCancel := context.WithTimeout(context.Background(), 60*time.Second)
				err = s.engine.Stop(retryCtx, sb.EngineID, engine.StopOpts{ForceFullSnapshot: true})
				retryCancel()
			}
			if err != nil {
				failed.Add(1)
				slog.Error("snapshot-all: retry failed, leaving VM running",
					"sandbox", sb.Name, "id", sb.ID, "error", err)
				// Do NOT kill the FC process — an unsnapshotted live VM is
				// better than an unrecoverable dead sandbox.
				return
			}

			s.saveVMState(sb.ID, sb.EngineID)
			s.store.UpdateSandboxStatus(sb.ID, "stopped")
			snapped.Add(1)
			slog.Info("snapshot-all: stopped",
				"sandbox", sb.Name, "id", sb.ID)
		}(sb)
	}
	wg.Wait()

	slog.Info("snapshot-all complete",
		"snapshotted", snapped.Load(),
		"failed", failed.Load(),
		"total", len(running))
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
	s.thermalDone = make(chan struct{})

	go func() {
		defer close(s.thermalDone)
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

		// Sandboxes with keep_hot skip thermal management entirely.
		// Used for autonomous agents that maintain persistent external
		// connections (e.g. Slack WebSocket) that would die if paused.
		if sb.KeepHot {
			continue
		}

		thermal := te.ThermalState(sb.EngineID)

		// --- Warm → Cold: host-side timing only, no agent query ---
		// vCPUs are paused — the agent can't respond. Querying it either
		// times out (skipping the cold check) or wakes the VM via TCP.
		// Use lastActivity timestamp instead, set when hot→warm fired.
		if thermal == "warm" {
			ts, ok := s.lastActivity.Load(sb.EngineID)
			if !ok {
				continue
			}
			idle := time.Since(ts.(time.Time))
			if idle > cfg.ColdTimeout {
				stopCtx, stopCancel := context.WithTimeout(context.Background(), 60*time.Second)
				err := s.engine.Stop(stopCtx, sb.EngineID, engine.StopOpts{})
				stopCancel()

				if err != nil {
					// Track consecutive failures. The VM is still warm
					// (alive, vCPUs paused) — retry on next cycle.
					// Only mark unknown after 3 consecutive failures.
					count := 0
					if v, ok := s.snapshotFailures.Load(sb.EngineID); ok {
						count = v.(int)
					}
					count++
					s.snapshotFailures.Store(sb.EngineID, count)

					if count >= 3 {
						slog.Error("thermal snapshot failed 3 times — marking unknown",
							"sandbox", sb.Name, "id", sb.ID, "error", err,
							"attempts", count)
						s.store.UpdateSandboxStatus(sb.ID, "unknown")
						s.snapshotFailures.Delete(sb.EngineID)
					} else {
						slog.Warn("thermal snapshot failed — will retry",
							"sandbox", sb.Name, "id", sb.ID, "error", err,
							"attempt", count, "max_attempts", 3)
					}
					continue
				}

				// Success — clear failure counter
				s.snapshotFailures.Delete(sb.EngineID)
				s.store.StopSandbox(sb.ID)
				s.saveVMState(sb.ID, sb.EngineID)
				slog.Info("thermal transition", "sandbox", sb.Name,
					"from", "warm", "to", "cold", "idle", idle.Round(time.Second))
			}
			continue
		}

		if thermal != "hot" {
			continue
		}

		// --- Hot → Warm: query agent (vCPUs running, agent can respond) ---

		// Fast path: host-side activity cache.
		if ts, ok := s.lastActivity.Load(sb.EngineID); ok {
			if time.Since(ts.(time.Time)) < cfg.WarmTimeout {
				continue
			}
		}

		// Slow path: ask the agent for authoritative activity info.
		actCtx, actCancel := context.WithTimeout(context.Background(), 5*time.Second)
		activity, err := te.Activity(actCtx, sb.EngineID)
		actCancel()
		if err != nil {
			count := s.incrementThermalFails(sb.EngineID)
			slog.Warn("thermal activity query failed",
				"sandbox", sb.Name, "error", err,
				"consecutive_failures", count)
			if count >= maxThermalFailures {
				slog.Error("thermal force-pause: agent unresponsive",
					"sandbox", sb.Name, "failures", count)
				// Pause is a Firecracker API call — doesn't need the agent
				if err := te.Pause(context.Background(), sb.EngineID); err != nil {
					slog.Warn("thermal force-pause failed", "sandbox", sb.Name, "error", err)
				} else {
					// Inflate balloon on force-paused VMs too
					if memMiB := te.MemSizeMib(sb.EngineID); memMiB > 0 {
						bCtx, bCancel := context.WithTimeout(context.Background(), 5*time.Second)
						te.BalloonSet(bCtx, sb.EngineID, memMiB/2)
						bCancel()
					}
					s.lastActivity.Store(sb.EngineID, time.Now())
					slog.Info("thermal transition", "sandbox", sb.Name,
						"from", "hot", "to", "warm", "reason", "force-pause")
				}
				s.resetThermalFails(sb.EngineID)
			}
			continue
		}
		s.resetThermalFails(sb.EngineID) // success — reset counter

		idle := time.Since(time.Unix(activity.LastActivityUnix, 0))

		if idle > cfg.WarmTimeout && activity.AttachedSessions == 0 {
			if err := te.Pause(context.Background(), sb.EngineID); err != nil {
				slog.Warn("thermal pause failed", "sandbox", sb.Name, "error", err)
				continue
			}
			// Inflate balloon to reclaim ~50% of guest memory while paused.
			// deflate_on_oom ensures the guest reclaims it when resumed.
			if memMiB := te.MemSizeMib(sb.EngineID); memMiB > 0 {
				balloonCtx, balloonCancel := context.WithTimeout(context.Background(), 5*time.Second)
				if err := te.BalloonSet(balloonCtx, sb.EngineID, memMiB/2); err != nil {
					slog.Debug("balloon inflate failed", "sandbox", sb.Name, "error", err)
				}
				balloonCancel()
			}
			// Record pause time so warm→cold timer starts from now,
			// not from the last user interaction.
			s.lastActivity.Store(sb.EngineID, time.Now())
			slog.Info("thermal transition", "sandbox", sb.Name,
				"from", "hot", "to", "warm", "idle", idle.Round(time.Second))
		} else if activity.AttachedSessions > 0 {
			slog.Debug("thermal skip: active sessions",
				"sandbox", sb.Name, "sessions", activity.AttachedSessions)
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

	if err := te.EnsureHot(ctx, engineID); err != nil {
		return err
	}

	// After a successful wake, update store status if it was stale.
	// This handles: cold→hot (store said "stopped"),
	// and unknown→hot (store said "unknown" but VM was still alive).
	if sb, err := s.store.GetSandboxByEngineID(engineID); err == nil {
		if sb.Status != "running" {
			slog.Info("sandbox recovered",
				"sandbox", sb.Name, "from_status", sb.Status)
			s.store.UpdateSandboxStatus(sb.ID, "running")
			s.saveVMState(sb.ID, engineID)
		}
	}

	return nil
}


// ServerVersion is set by the main package at startup from the build-time
// version string. Advertised to CLI clients via the X-Bhatti-Version header
// so they can detect when an update is available (push, not pull).
var ServerVersion = "dev"

// MinCLIVersion is the minimum CLI version the server requires. CLI clients
// older than this receive an X-Bhatti-Min-CLI header and should warn the user.
// Bump this when making breaking API changes that old CLIs can't handle.
var MinCLIVersion = ""

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Domain mode: route by Host header BEFORE auth.
	// Proxy zone subdomains are served unauthenticated.
	// API host falls through to normal auth flow.
	if s.proxyZone != "" {
		host := stripPort(r.Host)

		// API host and localhost always fall through to auth.
		// Check BEFORE proxy zone match because api.bhatti.sh also
		// matches *.bhatti.sh when proxy zone is bhatti.sh.
		if host == s.apiHost || host == "localhost" || host == "127.0.0.1" {
			// fall through to auth
		} else if strings.HasSuffix(host, "."+s.proxyZone) {
			alias := strings.TrimSuffix(host, "."+s.proxyZone)
			if s.publicProxy != nil {
				s.publicProxy.proxyToAlias(w, r, alias, r.URL.Path)
			} else {
				errResp(w, 503, "public proxy not configured")
			}
			return
		} else {
			errResp(w, 404, "unknown host")
			return
		}
	}

	// Advertise server version on every API response so CLI clients can
	// detect when an update is available (push, not pull).
	w.Header().Set("X-Bhatti-Version", ServerVersion)
	if MinCLIVersion != "" {
		w.Header().Set("X-Bhatti-Min-CLI", MinCLIVersion)
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
