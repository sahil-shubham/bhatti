package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	_ "embed"
)

//go:embed shell.html
var shellHTML []byte

// handleWebShell routes /_shell/:id and /_shell/:id/ws.
func (s *Server) handleWebShell(w http.ResponseWriter, r *http.Request, cleanPath string) {
	// Handle bare /_shell (path.Clean strips trailing slash)
	if cleanPath == "/_shell" || cleanPath == "/_shell/" {
		errResp(w, 404, "not found")
		return
	}

	trimmed := strings.TrimPrefix(cleanPath, "/_shell/")
	parts := strings.SplitN(trimmed, "/", 2)
	sandboxID := parts[0]

	if sandboxID == "" {
		errResp(w, 404, "not found")
		return
	}

	if len(parts) == 1 || parts[1] == "" {
		s.serveShellHTML(w, r)
		return
	}
	if parts[1] == "ws" {
		s.handleShellWS(w, r, sandboxID)
		return
	}
	errResp(w, 404, "not found")
}

// serveShellHTML serves the embedded xterm.js terminal page.
// No token validation — the security boundary is the WebSocket.
func (s *Server) serveShellHTML(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("Content-Security-Policy", strings.Join([]string{
		"default-src 'none'",
		"script-src https://cdn.jsdelivr.net",                   // xterm.js (v5.x does not need unsafe-eval)
		"style-src https://cdn.jsdelivr.net 'unsafe-inline'",    // xterm.css
		"font-src https://cdn.jsdelivr.net",                     // FiraCode
		"connect-src 'self'",                                    // WebSocket + fetch
		"frame-ancestors 'none'",
	}, "; "))
	w.Write(shellHTML)
}

// handleShellWS upgrades to WebSocket, validates token via first message,
// then relays terminal I/O. Security invariant: the handler never leaks
// sandbox existence to an unauthenticated client.
func (s *Server) handleShellWS(w http.ResponseWriter, r *http.Request, sandboxID string) {
	// 1. Rate limit — per-IP cap on WebSocket connection attempts.
	ip := r.RemoteAddr
	if i := strings.LastIndex(ip, ":"); i >= 0 {
		ip = ip[:i] // strip port
	}
	if !s.shellLimiter.Allow(ip) {
		errResp(w, 429, "too many requests")
		return
	}

	// 2. Upgrade WebSocket FIRST — before any sandbox lookup.
	//    This prevents sandbox ID enumeration via HTTP status codes.
	conn, err := upgrader.Upgrade(w, r, http.Header{
		"Referrer-Policy": []string{"no-referrer"},
	})
	if err != nil {
		return
	}
	defer conn.Close()

	// 3. Read first message — must be auth within 10 seconds.
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		return
	}

	var auth struct {
		Type  string `json:"type"`
		Token string `json:"token"`
	}
	if json.Unmarshal(msg, &auth) != nil || auth.Type != "auth" || auth.Token == "" {
		conn.WriteJSON(map[string]string{"type": "error", "error": "unauthorized"})
		return
	}

	// 4. Lookup + validate TOGETHER — same error for all failure modes.
	sb, err := s.store.GetSandboxByID(sandboxID)
	tokenHash := sha256Hex(auth.Token)
	if err != nil || sb.Status == "destroyed" || sb.ShellTokenHash == "" ||
		subtle.ConstantTimeCompare([]byte(tokenHash), []byte(sb.ShellTokenHash)) != 1 {
		conn.WriteJSON(map[string]string{"type": "error", "error": "unauthorized"})
		return
	}

	// ---- authenticated ----

	// 5. Enforce concurrent session limit (cap per sandbox).
	if !s.shellSessions.Add(sandboxID) {
		conn.WriteJSON(map[string]string{"type": "error", "error": "too many sessions"})
		return
	}
	defer s.shellSessions.Remove(sandboxID)

	// 6. Register for revoke-disconnect.
	revoked := s.shellSessions.Done(sandboxID)

	slog.Info("shell_connect", "sandbox", sb.Name, "sandbox_id", sb.ID,
		"ip", r.RemoteAddr)
	start := time.Now()
	defer func() {
		slog.Info("shell_close", "sandbox", sb.Name, "sandbox_id", sb.ID,
			"duration", time.Since(start).Round(time.Second))
	}()

	// 7. Send sandbox info.
	conn.WriteJSON(map[string]any{
		"type":    "connected",
		"sandbox": sb.Name,
		"status":  sb.Status,
	})

	// 8. Wake sandbox (same ensureHot as CLI shell and public proxy).
	if err := s.ensureHot(r.Context(), sb.EngineID); err != nil {
		conn.WriteJSON(map[string]string{"type": "error", "error": "sandbox unavailable"})
		return
	}

	// 9. Create shell session — same Engine.Shell() as CLI path.
	conn.SetReadDeadline(time.Time{}) // clear the 10s auth deadline
	term, err := s.engine.Shell(context.Background(), sb.EngineID)
	if err != nil {
		conn.WriteJSON(map[string]string{"type": "error", "error": "shell unavailable"})
		return
	}
	defer term.Close()

	// 10. Bidirectional relay. Exits when either side closes OR on revoke.
	go func() {
		<-revoked
		conn.Close()
	}()
	wsRelay(conn, term)
}

// handleShellToken handles POST (generate) and DELETE (revoke) for shell tokens.
// Authenticated endpoint — sits behind the auth middleware via handleSandbox.
func (s *Server) handleShellToken(w http.ResponseWriter, r *http.Request, id string) {
	sb := s.getUserSandbox(w, r, id)
	if sb == nil {
		return
	}

	switch r.Method {
	case "POST":
		token := randomHex(32)
		hash := sha256Hex(token)
		if err := s.store.SetShellToken(sb.ID, hash); err != nil {
			errResp(w, 500, "failed to set shell token")
			return
		}
		url := fmt.Sprintf("https://%s/_shell/%s#token=%s", s.apiHost, sb.ID, token)
		writeJSON(w, 200, map[string]string{
			"token": token,
			"url":   url,
		})

	case "DELETE":
		if err := s.store.ClearShellToken(sb.ID); err != nil {
			errResp(w, 500, "failed to revoke shell token")
			return
		}
		s.shellSessions.DisconnectAll(sb.ID)
		w.WriteHeader(204)

	default:
		errResp(w, 405, "method not allowed")
	}
}

// randomHex returns n random bytes as a hex string (2n chars).
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return fmt.Sprintf("%x", b)
}

// --- shellSessionTracker ---

// shellSessionTracker manages active web shell sessions per sandbox.
// Enforces a concurrent session cap and supports revoke-disconnect.
type shellSessionTracker struct {
	mu       sync.Mutex
	counts   map[string]int
	channels map[string][]chan struct{}
	limit    int
}

func newShellSessionTracker(limit int) *shellSessionTracker {
	return &shellSessionTracker{
		counts:   make(map[string]int),
		channels: make(map[string][]chan struct{}),
		limit:    limit,
	}
}

// Add increments the count. Returns false if limit reached.
func (t *shellSessionTracker) Add(sandboxID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.counts[sandboxID] >= t.limit {
		return false
	}
	t.counts[sandboxID]++
	return true
}

// Remove decrements the count.
func (t *shellSessionTracker) Remove(sandboxID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.counts[sandboxID]--
	if t.counts[sandboxID] <= 0 {
		delete(t.counts, sandboxID)
	}
}

// Done returns a channel that is closed when DisconnectAll is called.
func (t *shellSessionTracker) Done(sandboxID string) <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	ch := make(chan struct{})
	t.channels[sandboxID] = append(t.channels[sandboxID], ch)
	return ch
}

// DisconnectAll closes all done channels for a sandbox (called on revoke).
func (t *shellSessionTracker) DisconnectAll(sandboxID string) {
	t.mu.Lock()
	chs := t.channels[sandboxID]
	delete(t.channels, sandboxID)
	t.mu.Unlock()
	for _, ch := range chs {
		close(ch)
	}
}

// --- shellRateLimiter ---

// shellRateLimiter is a simple per-key rate limiter for WebSocket connection
// attempts. Allows `rate` requests per second per key. Uses a token bucket
// with 1-second resolution.
type shellRateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*shellBucket
	rate    int
}

type shellBucket struct {
	tokens int
	last   time.Time
}

func newShellRateLimiter(rate int) *shellRateLimiter {
	return &shellRateLimiter{
		buckets: make(map[string]*shellBucket),
		rate:    rate,
	}
}

// Allow returns true if the request is allowed.
func (l *shellRateLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	b, ok := l.buckets[key]
	if !ok {
		l.buckets[key] = &shellBucket{tokens: l.rate - 1, last: now}
		return true
	}

	// Refill tokens based on elapsed time.
	elapsed := now.Sub(b.last).Seconds()
	b.tokens += int(elapsed * float64(l.rate))
	if b.tokens > l.rate {
		b.tokens = l.rate
	}
	b.last = now

	if b.tokens <= 0 {
		return false
	}
	b.tokens--
	return true
}
