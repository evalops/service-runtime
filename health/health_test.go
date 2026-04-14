package health_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/evalops/service-runtime/health"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
)

func TestEmptyChecker(t *testing.T) {
	c := health.New()
	report := c.Check(context.Background(), time.Second)
	if !report.Healthy {
		t.Error("empty checker should be healthy")
	}
	if len(report.Checks) != 0 {
		t.Errorf("expected 0 checks, got %d", len(report.Checks))
	}
}

func TestHealthyCheck(t *testing.T) {
	c := health.New()
	c.Add("db", func(_ context.Context) error { return nil })
	report := c.Check(context.Background(), time.Second)
	if !report.Healthy {
		t.Error("should be healthy")
	}
	if len(report.Checks) != 1 {
		t.Fatalf("expected 1 check, got %d", len(report.Checks))
	}
	if report.Checks[0].Name != "db" {
		t.Errorf("name = %q, want %q", report.Checks[0].Name, "db")
	}
	if !report.Checks[0].Healthy {
		t.Error("db check should be healthy")
	}
}

func TestUnhealthyCheck(t *testing.T) {
	c := health.New()
	c.Add("db", func(_ context.Context) error { return errors.New("connection refused") })
	report := c.Check(context.Background(), time.Second)
	if report.Healthy {
		t.Error("should be unhealthy")
	}
	if report.Checks[0].Error != "connection refused" {
		t.Errorf("error = %q, want %q", report.Checks[0].Error, "connection refused")
	}
}

func TestMixedChecks(t *testing.T) {
	c := health.New()
	c.Add("db", func(_ context.Context) error { return nil })
	c.Add("redis", func(_ context.Context) error { return errors.New("timeout") })
	c.Add("nats", func(_ context.Context) error { return nil })
	report := c.Check(context.Background(), time.Second)
	if report.Healthy {
		t.Error("should be unhealthy when any check fails")
	}
	healthy := 0
	for _, status := range report.Checks {
		if status.Healthy {
			healthy++
		}
	}
	if healthy != 2 {
		t.Errorf("expected 2 healthy checks, got %d", healthy)
	}
}

func TestTimeout(t *testing.T) {
	c := health.New()
	c.Add("slow", func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
			return nil
		}
	})
	report := c.Check(context.Background(), 50*time.Millisecond)
	if report.Healthy {
		t.Error("timed-out check should be unhealthy")
	}
}

func TestHandler200(t *testing.T) {
	c := health.New()
	c.Add("db", func(_ context.Context) error { return nil })
	recorder := httptest.NewRecorder()
	c.Handler(time.Second).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", recorder.Code)
	}
	var report health.Report
	_ = json.NewDecoder(recorder.Body).Decode(&report)
	if !report.Healthy {
		t.Error("report should be healthy")
	}
}

func TestHandler503(t *testing.T) {
	c := health.New()
	c.Add("db", func(_ context.Context) error { return errors.New("down") })
	recorder := httptest.NewRecorder()
	c.Handler(time.Second).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", recorder.Code)
	}
}

func TestCachedCheckReusesRecentReport(t *testing.T) {
	c := health.New()
	var calls atomic.Int32
	c.Add("db", func(_ context.Context) error {
		calls.Add(1)
		return nil
	})

	first := c.CachedCheck(context.Background(), time.Second, time.Minute)
	second := c.CachedCheck(context.Background(), time.Second, time.Minute)
	if !first.Healthy || !second.Healthy {
		t.Fatal("expected cached reports to stay healthy")
	}
	if calls.Load() != 1 {
		t.Fatalf("expected 1 check execution, got %d", calls.Load())
	}
}

func TestCachedCheckRefreshesExpiredReport(t *testing.T) {
	c := health.New()
	var calls atomic.Int32
	c.Add("db", func(_ context.Context) error {
		calls.Add(1)
		return nil
	})

	c.CachedCheck(context.Background(), time.Second, 10*time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	c.CachedCheck(context.Background(), time.Second, 10*time.Millisecond)

	if calls.Load() != 2 {
		t.Fatalf("expected cached report to refresh after expiry, got %d executions", calls.Load())
	}
}

func TestReadyzHandlerUsesCachedDefaults(t *testing.T) {
	c := health.New()
	var calls atomic.Int32
	c.Add("db", func(_ context.Context) error {
		calls.Add(1)
		return nil
	})

	handler := c.ReadyzHandler()
	first := httptest.NewRecorder()
	second := httptest.NewRecorder()

	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("expected readiness handler to return 200, got %d and %d", first.Code, second.Code)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected readyz cache to reuse the first report, got %d executions", calls.Load())
	}
}

type mockPinger struct{ err error }

func (m *mockPinger) Ping(_ context.Context) error { return m.err }

func TestPingCheck(t *testing.T) {
	check := health.PingCheck(&mockPinger{err: nil})
	if err := check(context.Background()); err != nil {
		t.Errorf("healthy ping returned error: %v", err)
	}

	check = health.PingCheck(&mockPinger{err: errors.New("refused")})
	if err := check(context.Background()); err == nil {
		t.Error("unhealthy ping should return error")
	}
}

type mockContextPinger struct{ err error }

func (m *mockContextPinger) PingContext(_ context.Context) error { return m.err }

func TestPostgresCheck(t *testing.T) {
	check := health.PostgresCheck(&mockContextPinger{})
	if err := check(context.Background()); err != nil {
		t.Fatalf("healthy postgres ping returned error: %v", err)
	}

	check = health.PostgresCheck(&mockContextPinger{err: errors.New("db down")})
	if err := check(context.Background()); err == nil {
		t.Fatal("expected postgres check to report unhealthy dependency")
	}
}

func TestRedisCheck(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()

	check := health.RedisCheck(client)
	if err := check(context.Background()); err != nil {
		t.Fatalf("healthy redis ping returned error: %v", err)
	}

	server.Close()
	if err := check(context.Background()); err == nil {
		t.Fatal("expected redis check to report unhealthy dependency")
	}
}

type mockNATSConn struct{ status nats.Status }

func (m mockNATSConn) Status() nats.Status { return m.status }

func TestNATSCheck(t *testing.T) {
	check := health.NATSCheck(mockNATSConn{status: nats.CONNECTED})
	if err := check(context.Background()); err != nil {
		t.Fatalf("connected nats check returned error: %v", err)
	}

	check = health.NATSCheck(mockNATSConn{status: nats.CLOSED})
	if err := check(context.Background()); err == nil {
		t.Fatal("expected nats check to report closed connection")
	}
}

func TestHTTPCheck(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	check := health.HTTPCheck(server.Client(), server.URL)
	if err := check(context.Background()); err != nil {
		t.Fatalf("healthy http check returned error: %v", err)
	}

	unhealthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer unhealthy.Close()

	check = health.HTTPCheck(unhealthy.Client(), unhealthy.URL)
	if err := check(context.Background()); err == nil {
		t.Fatal("expected http check to reject non-2xx response")
	}
}

func TestConcurrentAdd(_ *testing.T) {
	c := health.New()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			c.Add("check", func(_ context.Context) error { return nil })
		}
		close(done)
	}()
	for i := 0; i < 100; i++ {
		c.Check(context.Background(), time.Second)
	}
	<-done
}
