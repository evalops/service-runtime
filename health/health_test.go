package health_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/evalops/service-runtime/health"
)

func TestEmptyChecker(t *testing.T) {
	c := health.New()
	r := c.Check(context.Background(), time.Second)
	if !r.Healthy {
		t.Error("empty checker should be healthy")
	}
	if len(r.Checks) != 0 {
		t.Errorf("expected 0 checks, got %d", len(r.Checks))
	}
}

func TestHealthyCheck(t *testing.T) {
	c := health.New()
	c.Add("db", func(ctx context.Context) error { return nil })
	r := c.Check(context.Background(), time.Second)
	if !r.Healthy {
		t.Error("should be healthy")
	}
	if len(r.Checks) != 1 {
		t.Fatalf("expected 1 check, got %d", len(r.Checks))
	}
	if r.Checks[0].Name != "db" {
		t.Errorf("name = %q, want %q", r.Checks[0].Name, "db")
	}
	if !r.Checks[0].Healthy {
		t.Error("db check should be healthy")
	}
}

func TestUnhealthyCheck(t *testing.T) {
	c := health.New()
	c.Add("db", func(ctx context.Context) error { return errors.New("connection refused") })
	r := c.Check(context.Background(), time.Second)
	if r.Healthy {
		t.Error("should be unhealthy")
	}
	if r.Checks[0].Error != "connection refused" {
		t.Errorf("error = %q, want %q", r.Checks[0].Error, "connection refused")
	}
}

func TestMixedChecks(t *testing.T) {
	c := health.New()
	c.Add("db", func(ctx context.Context) error { return nil })
	c.Add("redis", func(ctx context.Context) error { return errors.New("timeout") })
	c.Add("nats", func(ctx context.Context) error { return nil })
	r := c.Check(context.Background(), time.Second)
	if r.Healthy {
		t.Error("should be unhealthy when any check fails")
	}
	healthy := 0
	for _, s := range r.Checks {
		if s.Healthy {
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
	r := c.Check(context.Background(), 50*time.Millisecond)
	if r.Healthy {
		t.Error("timed-out check should be unhealthy")
	}
}

func TestHandler200(t *testing.T) {
	c := health.New()
	c.Add("db", func(ctx context.Context) error { return nil })
	w := httptest.NewRecorder()
	c.Handler(time.Second).ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	var r health.Report
	_ = json.NewDecoder(w.Body).Decode(&r)
	if !r.Healthy {
		t.Error("report should be healthy")
	}
}

func TestHandler503(t *testing.T) {
	c := health.New()
	c.Add("db", func(ctx context.Context) error { return errors.New("down") })
	w := httptest.NewRecorder()
	c.Handler(time.Second).ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

type mockPinger struct{ err error }

func (m *mockPinger) Ping(ctx context.Context) error { return m.err }

func TestPingCheck(t *testing.T) {
	fn := health.PingCheck(&mockPinger{err: nil})
	if err := fn(context.Background()); err != nil {
		t.Errorf("healthy ping returned error: %v", err)
	}

	fn = health.PingCheck(&mockPinger{err: errors.New("refused")})
	if err := fn(context.Background()); err == nil {
		t.Error("unhealthy ping should return error")
	}
}

func TestConcurrentAdd(t *testing.T) {
	c := health.New()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			c.Add("check", func(ctx context.Context) error { return nil })
		}
		close(done)
	}()
	for i := 0; i < 100; i++ {
		c.Check(context.Background(), time.Second)
	}
	<-done
}
