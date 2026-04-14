// Package ratelimit provides HTTP rate limiting middleware for EvalOps services.
package ratelimit

import (
	"context"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
)

// Policy describes the effective rate limit for a request.
type Policy struct {
	RequestsPerSecond float64
	Burst             int
	Scope             string
	Exempt            bool
}

// Config controls rate limiting behavior.
type Config struct {
	// RequestsPerSecond is the sustained rate of allowed requests per key.
	RequestsPerSecond float64
	// Burst is the maximum number of requests allowed in a single burst.
	Burst int
	// CleanupInterval controls how often stale in-memory limiters are evicted.
	CleanupInterval time.Duration
	// MaxAge is how long an idle in-memory or Redis-backed limiter is kept before eviction.
	MaxAge time.Duration
	// ExemptPaths bypass rate limiting entirely (for example /healthz or /metrics).
	ExemptPaths map[string]bool
	// KeyFunc resolves the caller identity. Defaults to the client IP.
	KeyFunc func(r *http.Request) string
	// ScopeFunc resolves the request scope used for metrics and bucket partitioning.
	ScopeFunc func(r *http.Request) string
	// PolicyFunc optionally overrides the effective policy per request.
	PolicyFunc func(r *http.Request) Policy
	// RedisClient enables distributed rate limiting when set.
	RedisClient redis.Scripter
	// ServiceName enables service-scoped Prometheus metrics when set.
	ServiceName string
	// Registerer controls where Prometheus metrics are registered.
	Registerer prometheus.Registerer
	// Now overrides the clock for tests.
	Now func() time.Time
	// OnLimited is called when a request is rate limited.
	OnLimited func(r *http.Request)
	// OnError is called when distributed rate limiting fails and the limiter falls back locally.
	OnError func(r *http.Request, err error)
}

// DefaultConfig returns sensible defaults for internal services.
func DefaultConfig() Config {
	return Config{
		RequestsPerSecond: 100,
		Burst:             200,
		CleanupInterval:   time.Minute,
		MaxAge:            5 * time.Minute,
		ExemptPaths: map[string]bool{
			"/healthz": true,
			"/readyz":  true,
			"/metrics": true,
		},
		KeyFunc:   clientIP,
		ScopeFunc: routePattern,
		Now:       time.Now,
	}
}

type entry struct {
	limiter  *tokenBucket
	lastSeen time.Time
}

// Limiter enforces request budgets by identity and scope.
type Limiter struct {
	cfg     Config
	mu      sync.Mutex
	entries map[string]*entry
	done    chan struct{}
	metrics *metrics
}

// New creates a Limiter and starts background cleanup.
func New(cfg Config) *Limiter {
	cfg = cfg.withDefaults()
	l := &Limiter{
		cfg:     cfg,
		entries: make(map[string]*entry),
		done:    make(chan struct{}),
		metrics: newMetrics(cfg),
	}
	go l.cleanup()
	return l
}

// Close stops the background cleanup goroutine.
func (l *Limiter) Close() {
	close(l.done)
}

// Allow checks if the given key is within the rate limit.
func (l *Limiter) Allow(key string) bool {
	allowed, _ := l.AllowContext(context.Background(), key)
	return allowed
}

// AllowContext checks if the given key is within the rate limit.
func (l *Limiter) AllowContext(ctx context.Context, key string) (bool, error) {
	policy := Policy{
		RequestsPerSecond: l.cfg.RequestsPerSecond,
		Burst:             l.cfg.Burst,
		Scope:             "global",
	}
	return l.allow(ctx, policy, key)
}

// Middleware returns HTTP middleware that rate limits requests by key and scope.
func (l *Limiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		policy := l.policyForRequest(r)
		if policy.Exempt {
			l.metrics.record(policy.Scope, "exempt")
			next.ServeHTTP(w, r)
			return
		}

		key := strings.TrimSpace(l.cfg.KeyFunc(r))
		if key == "" {
			key = "anonymous"
		}

		allowed, retryAfter, err := l.allowWithRetry(r.Context(), policy, key)
		if err != nil && l.cfg.OnError != nil {
			l.cfg.OnError(r, err)
		}
		if !allowed {
			l.metrics.record(policy.Scope, "limited")
			if l.cfg.OnLimited != nil {
				l.cfg.OnLimited(r)
			}
			retrySeconds := int(math.Ceil(retryAfter.Seconds()))
			if retrySeconds < 1 {
				retrySeconds = 1
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", strconv.Itoa(retrySeconds))
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate_limit_exceeded"}`))
			return
		}

		l.metrics.record(policy.Scope, "allowed")
		next.ServeHTTP(w, r)
	})
}

func (l *Limiter) allow(ctx context.Context, policy Policy, key string) (bool, error) {
	allowed, _, err := l.allowWithRetry(ctx, policy, key)
	return allowed, err
}

func (l *Limiter) allowWithRetry(ctx context.Context, policy Policy, key string) (bool, time.Duration, error) {
	now := l.cfg.Now()
	if l.cfg.RedisClient != nil {
		allowed, retryAfter, err := l.allowRedis(ctx, policy, key, now)
		if err == nil {
			return allowed, retryAfter, nil
		}
		allowed, retryAfter = l.allowLocal(policy, key, now)
		return allowed, retryAfter, err
	}
	allowed, retryAfter := l.allowLocal(policy, key, now)
	return allowed, retryAfter, nil
}

func (l *Limiter) allowLocal(policy Policy, key string, now time.Time) (bool, time.Duration) {
	bucketKey := localBucketKey(policy.Scope, key)

	l.mu.Lock()
	bucketEntry, ok := l.entries[bucketKey]
	if !ok || bucketEntry.limiter.rate != policy.RequestsPerSecond || bucketEntry.limiter.burst != policy.Burst {
		bucketEntry = &entry{limiter: newTokenBucket(policy.RequestsPerSecond, policy.Burst, now)}
		l.entries[bucketKey] = bucketEntry
	}
	bucketEntry.lastSeen = now
	l.mu.Unlock()

	return bucketEntry.limiter.AllowAt(now)
}

func (l *Limiter) policyForRequest(r *http.Request) Policy {
	scope := strings.TrimSpace(l.cfg.ScopeFunc(r))
	if scope == "" {
		scope = r.URL.Path
	}

	policy := Policy{
		RequestsPerSecond: l.cfg.RequestsPerSecond,
		Burst:             l.cfg.Burst,
		Scope:             scope,
		Exempt:            l.cfg.ExemptPaths[r.URL.Path],
	}

	if l.cfg.PolicyFunc != nil {
		override := l.cfg.PolicyFunc(r)
		if override.RequestsPerSecond > 0 {
			policy.RequestsPerSecond = override.RequestsPerSecond
		}
		if override.Burst > 0 {
			policy.Burst = override.Burst
		}
		if scoped := strings.TrimSpace(override.Scope); scoped != "" {
			policy.Scope = scoped
		}
		if override.Exempt {
			policy.Exempt = true
		}
	}

	if policy.Scope == "" {
		policy.Scope = "unknown"
	}
	return policy
}

func (l *Limiter) cleanup() {
	ticker := time.NewTicker(l.cfg.CleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-l.done:
			return
		case <-ticker.C:
			cutoff := l.cfg.Now().Add(-l.cfg.MaxAge)
			l.mu.Lock()
			for key, entry := range l.entries {
				if entry.lastSeen.Before(cutoff) {
					delete(l.entries, key)
				}
			}
			l.mu.Unlock()
		}
	}
}

// Len returns the number of tracked in-memory buckets.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

func (cfg Config) withDefaults() Config {
	defaults := DefaultConfig()

	if cfg.RequestsPerSecond <= 0 {
		cfg.RequestsPerSecond = defaults.RequestsPerSecond
	}
	if cfg.Burst <= 0 {
		cfg.Burst = defaults.Burst
	}
	if cfg.CleanupInterval <= 0 {
		cfg.CleanupInterval = defaults.CleanupInterval
	}
	if cfg.MaxAge <= 0 {
		cfg.MaxAge = defaults.MaxAge
	}
	if cfg.ExemptPaths == nil {
		cfg.ExemptPaths = defaults.ExemptPaths
	}
	if cfg.KeyFunc == nil {
		cfg.KeyFunc = defaults.KeyFunc
	}
	if cfg.ScopeFunc == nil {
		cfg.ScopeFunc = defaults.ScopeFunc
	}
	if cfg.Now == nil {
		cfg.Now = defaults.Now
	}
	return cfg
}

func localBucketKey(scope, key string) string {
	return scope + "\n" + key
}

func routePattern(r *http.Request) string {
	route := r.URL.Path
	if routeContext := chi.RouteContext(r.Context()); routeContext != nil {
		if pattern := routeContext.RoutePattern(); pattern != "" {
			route = pattern
		}
	}
	return route
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			if candidate := strings.TrimSpace(parts[i]); candidate != "" {
				return candidate
			}
		}
	}
	if xri := strings.TrimSpace(r.Header.Get("X-Real-Ip")); xri != "" {
		return xri
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	return r.RemoteAddr
}

// tokenBucket is a simple token bucket rate limiter.
type tokenBucket struct {
	mu       sync.Mutex
	rate     float64
	burst    int
	tokens   float64
	lastTime time.Time
}

func newTokenBucket(rate float64, burst int, now time.Time) *tokenBucket {
	return &tokenBucket{
		rate:     rate,
		burst:    burst,
		tokens:   float64(burst),
		lastTime: now,
	}
}

func (tb *tokenBucket) Allow() bool {
	allowed, _ := tb.AllowAt(time.Now())
	return allowed
}

func (tb *tokenBucket) AllowAt(now time.Time) (bool, time.Duration) {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	if now.Before(tb.lastTime) {
		now = tb.lastTime
	}

	elapsed := now.Sub(tb.lastTime).Seconds()
	tb.lastTime = now
	tb.tokens += elapsed * tb.rate
	if tb.tokens > float64(tb.burst) {
		tb.tokens = float64(tb.burst)
	}

	if tb.tokens >= 1 {
		tb.tokens--
		return true, 0
	}
	if tb.rate <= 0 {
		return false, time.Second
	}

	deficit := 1 - tb.tokens
	retryAfter := time.Duration(math.Ceil((deficit / tb.rate) * float64(time.Second)))
	if retryAfter < time.Millisecond {
		retryAfter = time.Millisecond
	}
	return false, retryAfter
}
