package server

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// rateLimiter implements per-user token bucket rate limiting.
// Different operation classes have different limits.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*userBuckets
}

type userBuckets struct {
	create *tokenBucket // sandbox creation: 10/min
	exec   *tokenBucket // exec, file ops: 120/min
	read   *tokenBucket // list, get, ports: 600/min
}

type tokenBucket struct {
	tokens     float64
	maxTokens  float64
	refillRate float64 // tokens per second
	lastRefill time.Time
}

func newTokenBucket(maxTokens, perMinute float64) *tokenBucket {
	return &tokenBucket{
		tokens:     maxTokens,
		maxTokens:  maxTokens,
		refillRate: perMinute / 60.0,
		lastRefill: time.Now(),
	}
}

func (b *tokenBucket) allow() bool {
	now := time.Now()
	elapsed := now.Sub(b.lastRefill).Seconds()
	b.tokens += elapsed * b.refillRate
	if b.tokens > b.maxTokens {
		b.tokens = b.maxTokens
	}
	b.lastRefill = now

	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{
		buckets: make(map[string]*userBuckets),
	}
}

func (rl *rateLimiter) getUserBuckets(userID string) *userBuckets {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	if ub, ok := rl.buckets[userID]; ok {
		return ub
	}
	ub := &userBuckets{
		create: newTokenBucket(10, 10),   // 10/min, burst of 10
		exec:   newTokenBucket(30, 120),  // 120/min, burst of 30
		read:   newTokenBucket(60, 600),  // 600/min, burst of 60
	}
	rl.buckets[userID] = ub
	return ub
}

// Allow checks if a request is allowed under rate limits.
func (rl *rateLimiter) Allow(userID string, r *http.Request) bool {
	ub := rl.getUserBuckets(userID)

	class := classifyRequest(r)
	rl.mu.Lock()
	defer rl.mu.Unlock()

	switch class {
	case "create":
		return ub.create.allow()
	case "exec":
		return ub.exec.allow()
	default:
		return ub.read.allow()
	}
}

// classifyRequest determines the rate limit class for a request.
func classifyRequest(r *http.Request) string {
	path := r.URL.Path

	// Sandbox creation
	if r.Method == http.MethodPost && path == "/sandboxes" {
		return "create"
	}

	// Exec and write operations
	if r.Method == http.MethodPost && strings.Contains(path, "/exec") {
		return "exec"
	}
	if r.Method == http.MethodPut && strings.Contains(path, "/files") {
		return "exec"
	}
	if strings.Contains(path, "/ws") {
		return "exec"
	}

	// Everything else is a read
	return "read"
}
