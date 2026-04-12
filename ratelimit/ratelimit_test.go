package ratelimit_test

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/evalops/service-runtime/ratelimit"
)

func TestAllowWithinLimit(t *testing.T) {
	cfg := ratelimit.DefaultConfig()
	cfg.RequestsPerSecond = 10
	cfg.Burst = 10
	l := ratelimit.New(cfg)
	defer l.Close()

	for i := 0; i < 10; i++ {
		if !l.Allow("192.168.1.1") {
			t.Fatalf("request %d should be allowed", i)
		}
	}
}

func TestDenyOverLimit(t *testing.T) {
	cfg := ratelimit.DefaultConfig()
	cfg.RequestsPerSecond = 1
	cfg.Burst = 2
	l := ratelimit.New(cfg)
	defer l.Close()

	l.Allow("192.168.1.1") // consume 1
	l.Allow("192.168.1.1") // consume 2 (burst)
	if l.Allow("192.168.1.1") {
		t.Error("third request should be denied (burst=2)")
	}
}

func TestPerIPIsolation(t *testing.T) {
	cfg := ratelimit.DefaultConfig()
	cfg.RequestsPerSecond = 1
	cfg.Burst = 1
	l := ratelimit.New(cfg)
	defer l.Close()

	l.Allow("10.0.0.1") // exhaust IP1
	if !l.Allow("10.0.0.2") {
		t.Error("different IP should have its own bucket")
	}
}

func TestMiddleware200(t *testing.T) {
	cfg := ratelimit.DefaultConfig()
	cfg.RequestsPerSecond = 100
	cfg.Burst = 100
	l := ratelimit.New(cfg)
	defer l.Close()

	handler := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/v1/prompts", nil))
	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestMiddleware429(t *testing.T) {
	cfg := ratelimit.DefaultConfig()
	cfg.RequestsPerSecond = 1
	cfg.Burst = 1
	l := ratelimit.New(cfg)
	defer l.Close()

	handler := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))

	req := httptest.NewRequest("GET", "/v1/prompts", nil)
	req.RemoteAddr = "10.0.0.1:12345"

	// First request succeeds.
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("first request: status = %d, want 200", w.Code)
	}

	// Second request is rate limited.
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 429 {
		t.Errorf("second request: status = %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") != "1" {
		t.Error("missing Retry-After header")
	}
}

func TestExemptPaths(t *testing.T) {
	cfg := ratelimit.DefaultConfig()
	cfg.RequestsPerSecond = 1
	cfg.Burst = 1
	l := ratelimit.New(cfg)
	defer l.Close()

	handler := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))

	req := httptest.NewRequest("GET", "/healthz", nil)
	req.RemoteAddr = "10.0.0.1:12345"

	// Exempt paths should always pass, even after exhausting the bucket.
	l.Allow("10.0.0.1") // exhaust bucket
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Errorf("healthz should be exempt, got %d", w.Code)
	}
}

func TestOnLimitedCallback(t *testing.T) {
	var count atomic.Int32
	cfg := ratelimit.DefaultConfig()
	cfg.RequestsPerSecond = 1
	cfg.Burst = 1
	cfg.OnLimited = func(_ *http.Request) { count.Add(1) }
	l := ratelimit.New(cfg)
	defer l.Close()

	handler := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))

	req := httptest.NewRequest("GET", "/v1/test", nil)
	req.RemoteAddr = "10.0.0.1:12345"
	handler.ServeHTTP(httptest.NewRecorder(), req) // allowed
	handler.ServeHTTP(httptest.NewRecorder(), req) // limited

	if count.Load() != 1 {
		t.Errorf("OnLimited called %d times, want 1", count.Load())
	}
}

func TestCleanup(t *testing.T) {
	cfg := ratelimit.DefaultConfig()
	cfg.RequestsPerSecond = 10
	cfg.Burst = 10
	cfg.CleanupInterval = 10 * time.Millisecond
	cfg.MaxAge = 10 * time.Millisecond
	l := ratelimit.New(cfg)
	defer l.Close()

	l.Allow("10.0.0.1")
	if l.Len() != 1 {
		t.Fatalf("expected 1 entry, got %d", l.Len())
	}

	time.Sleep(50 * time.Millisecond)
	if l.Len() != 0 {
		t.Errorf("expected 0 entries after cleanup, got %d", l.Len())
	}
}

func TestXForwardedFor(t *testing.T) {
	cfg := ratelimit.DefaultConfig()
	cfg.RequestsPerSecond = 1
	cfg.Burst = 1
	l := ratelimit.New(cfg)
	defer l.Close()

	handler := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))

	// Two requests with different X-Forwarded-For should use different buckets.
	req1 := httptest.NewRequest("GET", "/v1/test", nil)
	req1.Header.Set("X-Forwarded-For", "203.0.113.1, 10.0.0.1")
	req2 := httptest.NewRequest("GET", "/v1/test", nil)
	req2.Header.Set("X-Forwarded-For", "203.0.113.2, 10.0.0.1")

	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, req1)
	l.Allow("203.0.113.1") // exhaust IP1's bucket

	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)
	if w2.Code != 200 {
		t.Errorf("different XFF IP should have separate bucket, got %d", w2.Code)
	}
}
