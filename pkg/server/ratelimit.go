package server

import (
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sahil-shubham/bhatti/pkg"
)

// defaultRateLimits are the per-user budgets when the config sets none.
var defaultRateLimits = pkg.RateLimits{
	Create: pkg.RateBucket{PerMinute: 30, Burst: 10},
	Exec:   pkg.RateBucket{PerMinute: 600, Burst: 30},
	Read:   pkg.RateBucket{PerMinute: 1200, Burst: 60},
}

// rateLimiter implements per-user token bucket rate limiting.
// Each user has their own mutex and bucket set, so rate limit checks
// for different users never contend. The global mutex is only held
// briefly during user lookup/creation.
//
// Entries are evicted when the map exceeds maxRateLimitEntries to
// prevent unbounded growth from deleted users.
type rateLimiter struct {
	mu      sync.Mutex
	limits  pkg.RateLimits // every field set: defaults filled in
	buckets map[string]*userBuckets
}

const maxRateLimitEntries = 10_000

// Rate limit classes, as named in rate_limits config and 429 messages.
const (
	bucketCreate = "create"
	bucketExec   = "exec"
	bucketRead   = "read"
)

type userBuckets struct {
	mu         sync.Mutex // per-user lock — no global contention
	create     *tokenBucket
	exec       *tokenBucket
	read       *tokenBucket
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

// wait is how long until the bucket holds a whole token again, as of its
// last refill. Only meaningful right after allow refused.
func (b *tokenBucket) wait() time.Duration {
	return time.Duration((1 - b.tokens) / b.refillRate * float64(time.Second))
}

// newRateLimiter builds a limiter with the given budgets; zero fields take the
// defaults. Callers validate limits first (pkg.RateLimits.Validate).
func newRateLimiter(limits pkg.RateLimits) *rateLimiter {
	fill := func(b *pkg.RateBucket, def pkg.RateBucket) {
		if b.PerMinute == 0 {
			b.PerMinute = def.PerMinute
		}
		if b.Burst == 0 {
			b.Burst = def.Burst
		}
	}
	fill(&limits.Create, defaultRateLimits.Create)
	fill(&limits.Exec, defaultRateLimits.Exec)
	fill(&limits.Read, defaultRateLimits.Read)
	return &rateLimiter{
		limits:  limits,
		buckets: make(map[string]*userBuckets),
	}
}

// bucketLimits returns the configured budget of a class.
func (rl *rateLimiter) bucketLimits(class string) pkg.RateBucket {
	switch class {
	case bucketCreate:
		return rl.limits.Create
	case bucketExec:
		return rl.limits.Exec
	default:
		return rl.limits.Read
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

	l := rl.limits
	ub := &userBuckets{
		create:     newTokenBucket(l.Create.Burst, l.Create.PerMinute),
		exec:       newTokenBucket(l.Exec.Burst, l.Exec.PerMinute),
		read:       newTokenBucket(l.Read.Burst, l.Read.PerMinute),
		lastAccess: time.Now(),
	}
	rl.buckets[userID] = ub
	return ub
}

// Allow spends a token from the request's bucket. When it refuses, it names
// the bucket and how long until that bucket has a token, which is when a retry
// can succeed. Uses per-user locking — user A's check never blocks user B.
func (rl *rateLimiter) Allow(userID string, r *http.Request) (ok bool, class string, retryAfter time.Duration) {
	ub := rl.getUserBuckets(userID)
	class = classifyRequest(r)

	ub.mu.Lock()
	defer ub.mu.Unlock()
	ub.lastAccess = time.Now()

	var b *tokenBucket
	switch class {
	case bucketCreate:
		b = ub.create
	case bucketExec:
		b = ub.exec
	default:
		b = ub.read
	}
	if b.allow() {
		return true, class, 0
	}
	return false, class, b.wait()
}

// retryAfterSeconds rounds a wait up to the whole seconds Retry-After carries;
// rounding down would invite a retry that's refused again.
func retryAfterSeconds(d time.Duration) int {
	return max(1, int(math.Ceil(d.Seconds())))
}

// classifyRequest determines the rate limit class for a request.
// Tunnel upgrades use the read bucket (60 burst, 1200/min). A browser can
// connect tens of local ports at once without exhausting the smaller exec
// bucket, while upgrades still count against the user's general request limit.
// Destroy is a read: tearing down what you own isn't what the create budget
// guards, and sharing it made cleanup of a burst of sandboxes fail.
func classifyRequest(r *http.Request) string {
	path := r.URL.Path
	if r.Method == http.MethodGet && strings.HasPrefix(path, "/sandboxes/") && strings.HasSuffix(path, "/tunnel") {
		return bucketRead
	}

	// Sandbox creation
	if r.Method == http.MethodPost && path == "/sandboxes" {
		return bucketCreate
	}

	// Exec and write operations
	if r.Method == http.MethodPost && strings.Contains(path, "/exec") {
		return bucketExec
	}
	if r.Method == http.MethodPut && strings.Contains(path, "/files") {
		return bucketExec
	}
	if strings.Contains(path, "/ws") {
		return bucketExec
	}

	// Everything else is a read
	return bucketRead
}
