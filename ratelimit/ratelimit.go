// Package ratelimit provides HTTP rate limiting middleware for EvalOps services.
//
// Supports per-IP token bucket limiting using golang.org/x/time/rate.
// Designed to be added to the httpkit middleware chain.
package ratelimit

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// Config controls rate limiting behavior.
type Config struct {
	// RequestsPerSecond is the sustained rate of allowed requests per IP.
	RequestsPerSecond float64
	// Burst is the maximum number of requests allowed in a single burst.
	Burst int
	// CleanupInterval controls how often stale limiters are evicted.
	CleanupInterval time.Duration
	// MaxAge is how long an idle limiter is kept before eviction.
	MaxAge time.Duration
	// ExemptPaths are paths that bypass rate limiting (e.g., /healthz, /readyz).
	ExemptPaths map[string]bool
	// OnLimited is called when a request is rate limited. Optional.
	OnLimited func(r *http.Request)
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
	}
}

type entry struct {
	limiter  *tokenBucket
	lastSeen time.Time
}

// Limiter is a per-IP rate limiter with automatic cleanup.
type Limiter struct {
	cfg     Config
	mu      sync.Mutex
	entries map[string]*entry
	done    chan struct{}
}

// New creates a Limiter and starts background cleanup.
func New(cfg Config) *Limiter {
	if cfg.CleanupInterval == 0 {
		cfg.CleanupInterval = time.Minute
	}
	if cfg.MaxAge == 0 {
		cfg.MaxAge = 5 * time.Minute
	}
	l := &Limiter{
		cfg:     cfg,
		entries: make(map[string]*entry),
		done:    make(chan struct{}),
	}
	go l.cleanup()
	return l
}

// Close stops the background cleanup goroutine.
func (l *Limiter) Close() {
	close(l.done)
}

// Allow checks if the given key (typically an IP) is within the rate limit.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	e, ok := l.entries[key]
	if !ok {
		e = &entry{limiter: newTokenBucket(l.cfg.RequestsPerSecond, l.cfg.Burst)}
		l.entries[key] = e
	}
	e.lastSeen = time.Now()
	l.mu.Unlock()
	return e.limiter.Allow()
}

// Middleware returns an HTTP middleware that rate limits by client IP.
func (l *Limiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if l.cfg.ExemptPaths[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}

		ip := clientIP(r)
		if !l.Allow(ip) {
			if l.cfg.OnLimited != nil {
				l.cfg.OnLimited(r)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate_limit_exceeded"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (l *Limiter) cleanup() {
	ticker := time.NewTicker(l.cfg.CleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-l.done:
			return
		case <-ticker.C:
			l.mu.Lock()
			cutoff := time.Now().Add(-l.cfg.MaxAge)
			for key, e := range l.entries {
				if e.lastSeen.Before(cutoff) {
					delete(l.entries, key)
				}
			}
			l.mu.Unlock()
		}
	}
}

// Len returns the number of tracked IPs (for testing/metrics).
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// Use the most recent forwarded hop because the leftmost value is client-controlled.
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			if candidate := strings.TrimSpace(parts[i]); candidate != "" {
				return candidate
			}
		}
	}
	if xri := r.Header.Get("X-Real-Ip"); xri != "" {
		return xri
	}
	// Strip port from RemoteAddr.
	for i := len(r.RemoteAddr) - 1; i >= 0; i-- {
		if r.RemoteAddr[i] == ':' {
			return r.RemoteAddr[:i]
		}
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

func newTokenBucket(rate float64, burst int) *tokenBucket {
	return &tokenBucket{
		rate:     rate,
		burst:    burst,
		tokens:   float64(burst),
		lastTime: time.Now(),
	}
}

func (tb *tokenBucket) Allow() bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(tb.lastTime).Seconds()
	tb.lastTime = now
	tb.tokens += elapsed * tb.rate
	if tb.tokens > float64(tb.burst) {
		tb.tokens = float64(tb.burst)
	}

	if tb.tokens < 1 {
		return false
	}
	tb.tokens--
	return true
}
