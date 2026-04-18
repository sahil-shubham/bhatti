package server

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPublicRateLimiter_PerIPIsolation(t *testing.T) {
	rl := newPublicRateLimiter()

	// IP-A sends 500 requests to alias "app" — should all pass (burst=500)
	for i := 0; i < 500; i++ {
		if !rl.Allow("app", "1.2.3.4") {
			t.Fatalf("IP-A request %d rejected, expected all 500 to pass", i+1)
		}
	}

	// IP-B sends 500 requests to same alias — should all pass (independent bucket)
	for i := 0; i < 500; i++ {
		if !rl.Allow("app", "5.6.7.8") {
			t.Fatalf("IP-B request %d rejected, expected all 500 to pass", i+1)
		}
	}

	// IP-A's bucket is now drained — next request should be rejected
	if rl.Allow("app", "1.2.3.4") {
		t.Fatal("IP-A request 501 allowed, expected rejection after burst exhausted")
	}

	// IP-B's bucket is also drained
	if rl.Allow("app", "5.6.7.8") {
		t.Fatal("IP-B request 501 allowed, expected rejection after burst exhausted")
	}
}

func TestPublicRateLimiter_PerAliasAggregate(t *testing.T) {
	rl := newPublicRateLimiter()

	// Per-alias aggregate burst is 2000. Send 150 requests each from
	// 14 different IPs = 2100 total. Each IP is under its own per-IP
	// burst (200), but the alias aggregate (2000) should be exhausted.
	allowed := 0
	for ip := 0; ip < 14; ip++ {
		addr := fmt.Sprintf("10.0.0.%d", ip)
		for j := 0; j < 150; j++ {
			if rl.Allow("app", addr) {
				allowed++
			}
		}
	}

	if allowed > 2001 {
		t.Fatalf("expected ~2000 allowed (alias aggregate), got %d", allowed)
	}
	if allowed < 1900 {
		t.Fatalf("expected ~2000 allowed, got %d (too few)", allowed)
	}
	t.Logf("allowed %d / 2100 (alias aggregate burst=2000)", allowed)
}

func TestPublicRateLimiter_ViteDevReload(t *testing.T) {
	rl := newPublicRateLimiter()
	ip := "192.168.1.1"
	pageSize := 150 // realistic mid-size Vite app

	// First page load — all should pass
	for i := 0; i < pageSize; i++ {
		if !rl.Allow("vite-app", ip) {
			t.Fatalf("first load: request %d rejected", i+1)
		}
	}

	// Immediate reload: burst=500, used 150, so 350 remain — all pass.
	for i := 0; i < pageSize; i++ {
		if !rl.Allow("vite-app", ip) {
			t.Fatalf("second load: request %d rejected (should have ~350 tokens left)", i+1)
		}
	}

	// Third immediate reload: used 300 of 500, ~200 remain — all pass.
	for i := 0; i < pageSize; i++ {
		if !rl.Allow("vite-app", ip) {
			t.Fatalf("third load: request %d rejected (should have ~200 tokens left)", i+1)
		}
	}

	// Fourth immediate reload: used 450 of 500, only ~50 left — partial.
	fourthAllowed := 0
	for i := 0; i < pageSize; i++ {
		if rl.Allow("vite-app", ip) {
			fourthAllowed++
		}
	}
	if fourthAllowed > 60 {
		t.Fatalf("fourth load: expected most rejected (~50 tokens left), got %d allowed", fourthAllowed)
	}
	if fourthAllowed == 0 {
		t.Fatal("fourth load: expected some requests to pass from remaining tokens")
	}
	t.Logf("fourth load: %d/%d allowed (expected ~50)", fourthAllowed, pageSize)
}

// TestPublicRateLimiter_LargeViteApp tests a realistic large Vite app.
// Enterprise React apps (Appsmith: 5628 source files, Twenty CRM: 6629)
// with minimal code splitting can load 300-500+ modules on a single route.
// Verifies the burst handles this, and refill allows reload after a wait.
func TestPublicRateLimiter_LargeViteApp(t *testing.T) {
	rl := newPublicRateLimiter()
	ip := "10.0.0.1"

	// First load: 500 requests (large enterprise app). All should pass.
	for i := 0; i < 500; i++ {
		if !rl.Allow("big-app", ip) {
			t.Fatalf("first load: request %d rejected (burst should be 500)", i+1)
		}
	}

	// Immediate second load with 0 tokens: rejected
	if rl.Allow("big-app", ip) {
		t.Fatal("expected rejection immediately after draining burst")
	}

	// Simulate 20 seconds of refill: 1500/min = 25/sec → 500 tokens.
	// A full page load should pass again.
	rl.mu.Lock()
	rl.perIP[ip].bucket.lastRefill = time.Now().Add(-20 * time.Second)
	rl.mu.Unlock()

	allowed := 0
	for i := 0; i < 500; i++ {
		if rl.Allow("big-app", ip) {
			allowed++
		}
	}
	// 20s × 25/sec = 500, capped at maxTokens=500
	if allowed < 490 {
		t.Fatalf("after 20s refill: expected ~500 allowed, got %d", allowed)
	}
	t.Logf("after 20s refill: %d/500 allowed", allowed)
}

// TestPublicRateLimiter_CrossAliasIPBudget verifies that per-IP budget
// is shared across aliases. A developer hitting two published URLs from
// the same IP draws from one IP bucket.
func TestPublicRateLimiter_CrossAliasIPBudget(t *testing.T) {
	rl := newPublicRateLimiter()
	ip := "10.0.0.1"

	// Use 400 tokens on alias "frontend"
	for i := 0; i < 400; i++ {
		if !rl.Allow("frontend", ip) {
			t.Fatalf("frontend request %d rejected", i+1)
		}
	}

	// Only 100 tokens left — hitting alias "backend" from same IP
	allowed := 0
	for i := 0; i < 200; i++ {
		if rl.Allow("backend", ip) {
			allowed++
		}
	}
	if allowed > 110 || allowed < 90 {
		t.Fatalf("expected ~100 allowed on second alias (shared IP budget), got %d", allowed)
	}
	t.Logf("cross-alias: %d/200 allowed on second alias (100 tokens remaining)", allowed)
}

// TestPublicRateLimiter_TokenLeakOnPartialReject verifies that when an
// outer tier allows but an inner tier rejects, the leaked tokens from
// the outer tier are negligible. Specifically: global tokens consumed
// when per-IP rejects should not materially drain the global bucket.
func TestPublicRateLimiter_TokenLeakOnPartialReject(t *testing.T) {
	rl := newPublicRateLimiter()

	// Drain one IP's budget completely
	for i := 0; i < 500; i++ {
		rl.Allow("app", "10.0.0.1")
	}

	// Now send 500 more requests from the drained IP.
	// Per-IP rejects, but each call leaks 1 global token.
	for i := 0; i < 500; i++ {
		rl.Allow("app", "10.0.0.1")
	}

	// Global started at 10000, spent 500 (passed) + 500 (leaked) = 1000.
	// A fresh IP should still have ~9000 global tokens available.
	allowed := 0
	for i := 0; i < 500; i++ {
		if rl.Allow("app", "99.99.99.99") {
			allowed++
		}
	}
	if allowed != 500 {
		t.Fatalf("fresh IP after global leak: expected 500 allowed, got %d", allowed)
	}
}

// TestPublicRateLimiter_Concurrent verifies no data races under
// concurrent access from many goroutines. Run with -race.
func TestPublicRateLimiter_Concurrent(t *testing.T) {
	rl := newPublicRateLimiter()

	var wg sync.WaitGroup
	var totalAllowed atomic.Int64

	// 50 goroutines, each sending 100 requests from a unique IP
	for g := 0; g < 50; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			ip := fmt.Sprintf("10.0.%d.%d", id/256, id%256)
			for i := 0; i < 100; i++ {
				if rl.Allow("app", ip) {
					totalAllowed.Add(1)
				}
			}
		}(g)
	}

	wg.Wait()

	// 50 IPs × 100 req = 5000 total. Each IP is under burst (200),
	// alias aggregate is 2000 burst. So we expect ~2000 allowed
	// (alias aggregate is the bottleneck).
	total := totalAllowed.Load()
	if total < 1800 || total > 2200 {
		t.Fatalf("expected ~2000 allowed (alias aggregate), got %d", total)
	}
	t.Logf("concurrent: %d / 5000 allowed", total)
}

func TestPublicRateLimiter_RefillAfterWait(t *testing.T) {
	rl := newPublicRateLimiter()
	ip := "10.0.0.1"

	// Drain the per-IP burst
	for i := 0; i < 500; i++ {
		rl.Allow("app", ip)
	}

	// Should be rejected now
	if rl.Allow("app", ip) {
		t.Fatal("expected rejection after draining burst")
	}

	// Simulate time passing by directly manipulating the bucket.
	// Per-IP refill is 1500/min = 25/sec. After 1 second, ~25 tokens.
	rl.mu.Lock()
	ipb := rl.perIP[ip]
	ipb.bucket.lastRefill = time.Now().Add(-1 * time.Second)
	rl.mu.Unlock()

	// Should allow some requests now
	allowed := 0
	for i := 0; i < 35; i++ {
		if rl.Allow("app", ip) {
			allowed++
		}
	}
	if allowed < 22 || allowed > 28 {
		t.Fatalf("expected ~25 allowed after 1s refill (1500/min), got %d", allowed)
	}
	t.Logf("allowed %d after 1s refill", allowed)
}

func TestPublicRateLimiter_GlobalLimit(t *testing.T) {
	rl := newPublicRateLimiter()

	// Global burst is 10000. Exhaust it from many different IPs
	// so we don't hit per-IP or per-alias limits first.
	allowed := 0
	for i := 0; i < 10200; i++ {
		// Unique IP and alias per request to avoid per-IP/per-alias limits
		ip := fmt.Sprintf("10.%d.%d.%d", (i>>16)&0xFF, (i>>8)&0xFF, i&0xFF)
		alias := fmt.Sprintf("alias-%d", i)
		if rl.Allow(alias, ip) {
			allowed++
		}
	}

	// Slight over-count is expected: tokens refill while the loop runs.
	// 10200 iterations at ~50ms total → ~8 tokens refilled (10000/min = 166/sec).
	if allowed > 10050 {
		t.Fatalf("expected ~10000 allowed (global burst), got %d", allowed)
	}
	if allowed < 9900 {
		t.Fatalf("expected ~10000 allowed, got %d (too few)", allowed)
	}
	t.Logf("global: allowed %d / 10200", allowed)
}

func TestPublicRateLimiter_LRUEviction(t *testing.T) {
	rl := newPublicRateLimiter()

	// Insert more than publicRateLimiterMaxSize unique IPs
	for i := 0; i < publicRateLimiterMaxSize+500; i++ {
		ip := fmt.Sprintf("10.%d.%d.%d", (i>>16)&0xFF, (i>>8)&0xFF, i&0xFF)
		rl.Allow("app", ip)
	}

	rl.mu.Lock()
	ipCount := len(rl.perIP)
	rl.mu.Unlock()

	if ipCount > publicRateLimiterMaxSize {
		t.Fatalf("per-IP map exceeded max size: %d > %d", ipCount, publicRateLimiterMaxSize)
	}
	t.Logf("per-IP map size: %d (max %d)", ipCount, publicRateLimiterMaxSize)
}

func TestExtractIP(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"1.2.3.4:5678", "1.2.3.4"},
		{"[::1]:8080", "::1"},
		{"[2001:db8::1]:443", "2001:db8::1"},
		{"192.168.1.1:443", "192.168.1.1"},
		{"1.2.3.4", "1.2.3.4"},   // bare IP, no port
		{"::1", "::1"},            // bare IPv6, no brackets/port
	}
	for _, tt := range tests {
		got := extractIP(tt.input)
		if got != tt.want {
			t.Errorf("extractIP(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
