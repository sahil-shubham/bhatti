package server

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// rateLimiter implements per-user token bucket rate limiting.
// Each user has their own mutex and bucket set, so rate limit checks
// for different users never contend. The global mutex is only held
// briefly during user lookup/creation.
//
// Entries are evicted when the map exceeds maxRateLimitEntries to
// prevent unbounded growth from deleted users.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*userBuckets
}

const maxRateLimitEntries = 10_000

type userBuckets struct {
	mu         sync.Mutex  // per-user lock — no global contention
	create     *tokenBucket // sandbox creation: 30/min
	exec       *tokenBucket // exec, file ops: 600/min
	read       *tokenBucket // list, get, ports: 1200/min
	lastAccess time.Time
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

// getUserBuckets returns the bucket set for a user, creating one if needed.
// The global lock is held only for map lookup/insertion. Evicts the oldest
// entry if the map exceeds maxRateLimitEntries.
func (rl *rateLimiter) getUserBuckets(userID string) *userBuckets {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	if ub, ok := rl.buckets[userID]; ok {
		return ub
	}

	// Evict oldest entry if at capacity
	if len(rl.buckets) >= maxRateLimitEntries {
		var oldestKey string
		var oldestTime time.Time
		for k, v := range rl.buckets {
			if oldestKey == "" || v.lastAccess.Before(oldestTime) {
				oldestKey = k
				oldestTime = v.lastAccess
			}
		}
		if oldestKey != "" {
			delete(rl.buckets, oldestKey)
		}
	}

	ub := &userBuckets{
		create:     newTokenBucket(10, 30),   // 30/min, burst of 10
		exec:       newTokenBucket(30, 600),  // 600/min, burst of 30
		read:       newTokenBucket(60, 1200), // 1200/min, burst of 60
		lastAccess: time.Now(),
	}
	rl.buckets[userID] = ub
	return ub
}

// Allow checks if a request is allowed under rate limits.
// Uses per-user locking — user A's rate limit check never blocks user B.
func (rl *rateLimiter) Allow(userID string, r *http.Request) bool {
	ub := rl.getUserBuckets(userID)
	class := classifyRequest(r)

	ub.mu.Lock()
	defer ub.mu.Unlock()
	ub.lastAccess = time.Now()

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
