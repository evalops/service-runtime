package ratelimit_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/evalops/service-runtime/ratelimit"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/redis/go-redis/v9"
)

type manualClock struct {
	now time.Time
}

func (c *manualClock) Now() time.Time {
	return c.now
}

func (c *manualClock) Advance(duration time.Duration) {
	c.now = c.now.Add(duration)
}

func TestAllowWithinLimit(t *testing.T) {
	cfg := ratelimit.DefaultConfig()
	cfg.RequestsPerSecond = 10
	cfg.Burst = 10
	limiter := ratelimit.New(cfg)
	t.Cleanup(limiter.Close)

	for i := 0; i < 10; i++ {
		if !limiter.Allow("192.168.1.1") {
			t.Fatalf("request %d should be allowed", i)
		}
	}
}

func TestAllowContextUsesRedisAcrossLimiters(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() {
		_ = client.Close()
	})

	clock := &manualClock{now: time.Unix(1, 0)}
	cfg := ratelimit.DefaultConfig()
	cfg.RequestsPerSecond = 1
	cfg.Burst = 1
	cfg.RedisClient = client
	cfg.Now = clock.Now

	first := ratelimit.New(cfg)
	second := ratelimit.New(cfg)
	t.Cleanup(first.Close)
	t.Cleanup(second.Close)

	allowed, err := first.AllowContext(context.Background(), "10.0.0.1")
	if err != nil {
		t.Fatalf("first allow returned error: %v", err)
	}
	if !allowed {
		t.Fatal("first request should be allowed")
	}

	allowed, err = second.AllowContext(context.Background(), "10.0.0.1")
	if err != nil {
		t.Fatalf("second allow returned error: %v", err)
	}
	if allowed {
		t.Fatal("shared redis limiter should reject the second request")
	}

	clock.Advance(time.Second)
	allowed, err = second.AllowContext(context.Background(), "10.0.0.1")
	if err != nil {
		t.Fatalf("third allow returned error: %v", err)
	}
	if !allowed {
		t.Fatal("request should be allowed after refill")
	}
}

func TestMiddlewareFallsBackToLocalWhenRedisUnavailable(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{
		Addr:         server.Addr(),
		MaxRetries:   0,
		DialTimeout:  5 * time.Millisecond,
		ReadTimeout:  5 * time.Millisecond,
		WriteTimeout: 5 * time.Millisecond,
	})
	t.Cleanup(func() {
		_ = client.Close()
	})
	server.Close()

	var errCount atomic.Int32
	clock := &manualClock{now: time.Unix(1, 0)}
	cfg := ratelimit.DefaultConfig()
	cfg.RequestsPerSecond = 1
	cfg.Burst = 1
	cfg.RedisClient = client
	cfg.Now = clock.Now
	cfg.OnError = func(_ *http.Request, err error) {
		if err != nil {
			errCount.Add(1)
		}
	}

	limiter := ratelimit.New(cfg)
	t.Cleanup(limiter.Close)

	handler := limiter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	request := httptest.NewRequest(http.MethodGet, "/v1/test", nil)
	request.RemoteAddr = "10.0.0.1:12345"

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, request)
	if first.Code != http.StatusOK {
		t.Fatalf("fallback request should succeed, got %d", first.Code)
	}

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, request)
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("fallback limiter should still enforce the limit, got %d", second.Code)
	}
	if errCount.Load() == 0 {
		t.Fatal("expected redis fallback error hook to run")
	}
}

func TestMiddlewareScopesBucketsPerRoute(t *testing.T) {
	cfg := ratelimit.DefaultConfig()
	cfg.RequestsPerSecond = 1
	cfg.Burst = 1
	cfg.PolicyFunc = func(r *http.Request) ratelimit.Policy {
		switch r.URL.Path {
		case "/oauth/token":
			return ratelimit.Policy{Scope: "oauth_token", RequestsPerSecond: 1, Burst: 1}
		case "/v1/prompts":
			return ratelimit.Policy{Scope: "prompts", RequestsPerSecond: 1, Burst: 1}
		default:
			return ratelimit.Policy{}
		}
	}

	limiter := ratelimit.New(cfg)
	t.Cleanup(limiter.Close)

	handler := limiter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	oauth := httptest.NewRequest(http.MethodPost, "/oauth/token", nil)
	oauth.RemoteAddr = "10.0.0.1:12345"
	prompts := httptest.NewRequest(http.MethodGet, "/v1/prompts", nil)
	prompts.RemoteAddr = "10.0.0.1:12345"

	firstOAuth := httptest.NewRecorder()
	handler.ServeHTTP(firstOAuth, oauth)
	if firstOAuth.Code != http.StatusOK {
		t.Fatalf("oauth request should be allowed, got %d", firstOAuth.Code)
	}

	firstPrompts := httptest.NewRecorder()
	handler.ServeHTTP(firstPrompts, prompts)
	if firstPrompts.Code != http.StatusOK {
		t.Fatalf("prompts request should use a different bucket, got %d", firstPrompts.Code)
	}

	secondOAuth := httptest.NewRecorder()
	handler.ServeHTTP(secondOAuth, oauth)
	if secondOAuth.Code != http.StatusTooManyRequests {
		t.Fatalf("second oauth request should be limited, got %d", secondOAuth.Code)
	}
}

func TestPolicyChangesDoNotResetLocalBucket(t *testing.T) {
	clock := &manualClock{now: time.Unix(1, 0)}
	cfg := ratelimit.DefaultConfig()
	cfg.Now = clock.Now
	cfg.PolicyFunc = func(r *http.Request) ratelimit.Policy {
		if r.Header.Get("X-Plan") == "priority" {
			return ratelimit.Policy{Scope: "shared", RequestsPerSecond: 10, Burst: 10}
		}
		return ratelimit.Policy{Scope: "shared", RequestsPerSecond: 1, Burst: 1}
	}

	limiter := ratelimit.New(cfg)
	t.Cleanup(limiter.Close)

	handler := limiter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	first := httptest.NewRequest(http.MethodGet, "/v1/test", nil)
	first.RemoteAddr = "10.0.0.1:12345"

	second := httptest.NewRequest(http.MethodGet, "/v1/test", nil)
	second.RemoteAddr = "10.0.0.1:12345"
	second.Header.Set("X-Plan", "priority")

	firstResponse := httptest.NewRecorder()
	handler.ServeHTTP(firstResponse, first)
	if firstResponse.Code != http.StatusOK {
		t.Fatalf("first request should be allowed, got %d", firstResponse.Code)
	}

	secondResponse := httptest.NewRecorder()
	handler.ServeHTTP(secondResponse, second)
	if secondResponse.Code != http.StatusTooManyRequests {
		t.Fatalf("policy change should not refill the bucket, got %d", secondResponse.Code)
	}
}

func TestExemptPaths(t *testing.T) {
	cfg := ratelimit.DefaultConfig()
	cfg.RequestsPerSecond = 1
	cfg.Burst = 1
	limiter := ratelimit.New(cfg)
	t.Cleanup(limiter.Close)

	handler := limiter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	request.RemoteAddr = "10.0.0.1:12345"

	limiter.Allow("10.0.0.1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("healthz should bypass rate limiting, got %d", response.Code)
	}
}

func TestOnLimitedCallback(t *testing.T) {
	var count atomic.Int32
	cfg := ratelimit.DefaultConfig()
	cfg.RequestsPerSecond = 1
	cfg.Burst = 1
	cfg.OnLimited = func(_ *http.Request) {
		count.Add(1)
	}

	limiter := ratelimit.New(cfg)
	t.Cleanup(limiter.Close)

	handler := limiter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	request := httptest.NewRequest(http.MethodGet, "/v1/test", nil)
	request.RemoteAddr = "10.0.0.1:12345"

	handler.ServeHTTP(httptest.NewRecorder(), request)
	handler.ServeHTTP(httptest.NewRecorder(), request)

	if count.Load() != 1 {
		t.Fatalf("OnLimited called %d times, want 1", count.Load())
	}
}

func TestCleanup(t *testing.T) {
	cfg := ratelimit.DefaultConfig()
	cfg.RequestsPerSecond = 10
	cfg.Burst = 10
	cfg.CleanupInterval = 10 * time.Millisecond
	cfg.MaxAge = 10 * time.Millisecond

	limiter := ratelimit.New(cfg)
	t.Cleanup(limiter.Close)

	limiter.Allow("10.0.0.1")
	if limiter.Len() != 1 {
		t.Fatalf("expected 1 entry, got %d", limiter.Len())
	}

	time.Sleep(50 * time.Millisecond)
	if limiter.Len() != 0 {
		t.Fatalf("expected cleanup to evict the idle entry, got %d", limiter.Len())
	}
}

func TestXForwardedForUsesLastHop(t *testing.T) {
	cfg := ratelimit.DefaultConfig()
	cfg.RequestsPerSecond = 1
	cfg.Burst = 1
	limiter := ratelimit.New(cfg)
	t.Cleanup(limiter.Close)

	handler := limiter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	first := httptest.NewRequest(http.MethodGet, "/v1/test", nil)
	first.Header.Set("X-Forwarded-For", "203.0.113.1, 10.0.0.1")

	second := httptest.NewRequest(http.MethodGet, "/v1/test", nil)
	second.Header.Set("X-Forwarded-For", "203.0.113.2, 10.0.0.1")

	handler.ServeHTTP(httptest.NewRecorder(), first)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, second)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("spoofed XFF should not bypass the shared bucket, got %d", response.Code)
	}
}

func TestMiddlewareRecordsMetrics(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	cfg := ratelimit.DefaultConfig()
	cfg.RequestsPerSecond = 1
	cfg.Burst = 1
	cfg.ServiceName = "gate"
	cfg.Registerer = registry

	limiter := ratelimit.New(cfg)
	t.Cleanup(limiter.Close)

	handler := limiter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	request := httptest.NewRequest(http.MethodGet, "/v1/test", nil)
	request.RemoteAddr = "10.0.0.1:12345"

	handler.ServeHTTP(httptest.NewRecorder(), request)
	handler.ServeHTTP(httptest.NewRecorder(), request)

	metricFamilies, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}

	if value := metricValueWithLabels(metricFamilies, "gate_rate_limit_hits_total", map[string]string{
		"route":  "/v1/test",
		"action": "allowed",
	}); value != 1 {
		t.Fatalf("allowed metric = %v, want 1", value)
	}

	if value := metricValueWithLabels(metricFamilies, "gate_rate_limit_hits_total", map[string]string{
		"route":  "/v1/test",
		"action": "limited",
	}); value != 1 {
		t.Fatalf("limited metric = %v, want 1", value)
	}
}

func TestMiddlewareRecordsMetricsForLeadingDigitServiceName(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	cfg := ratelimit.DefaultConfig()
	cfg.RequestsPerSecond = 1
	cfg.Burst = 1
	cfg.ServiceName = "3gate"
	cfg.Registerer = registry

	limiter := ratelimit.New(cfg)
	t.Cleanup(limiter.Close)

	handler := limiter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	request := httptest.NewRequest(http.MethodGet, "/v1/test", nil)
	request.RemoteAddr = "10.0.0.1:12345"
	handler.ServeHTTP(httptest.NewRecorder(), request)

	metricFamilies, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}

	if value := metricValueWithLabels(metricFamilies, "service_3gate_rate_limit_hits_total", map[string]string{
		"route":  "/v1/test",
		"action": "allowed",
	}); value != 1 {
		t.Fatalf("allowed metric = %v, want 1", value)
	}
}

func TestAllowContextPropagatesRedisError(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{
		Addr:         server.Addr(),
		MaxRetries:   0,
		DialTimeout:  5 * time.Millisecond,
		ReadTimeout:  5 * time.Millisecond,
		WriteTimeout: 5 * time.Millisecond,
	})
	t.Cleanup(func() {
		_ = client.Close()
	})
	server.Close()

	cfg := ratelimit.DefaultConfig()
	cfg.RedisClient = client

	limiter := ratelimit.New(cfg)
	t.Cleanup(limiter.Close)

	allowed, err := limiter.AllowContext(context.Background(), "10.0.0.1")
	if err == nil {
		t.Fatal("expected redis error to be returned")
	}
	if !allowed {
		t.Fatal("allow context should fall back locally when redis is unavailable")
	}
	if err.Error() == "" {
		t.Fatalf("unexpected empty error: %v", err)
	}
}

func metricValueWithLabels(metricFamilies []*dto.MetricFamily, name string, labels map[string]string) float64 {
	for _, family := range metricFamilies {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			if !metricHasLabels(metric, labels) {
				continue
			}
			if counter := metric.GetCounter(); counter != nil {
				return counter.GetValue()
			}
		}
	}
	return 0
}

func metricHasLabels(metric *dto.Metric, labels map[string]string) bool {
	for key, value := range labels {
		found := false
		for _, label := range metric.GetLabel() {
			if label.GetName() == key && label.GetValue() == value {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
