// Package health provides dependency-aware readiness checks for EvalOps services.
//
// Services register named checks at startup. The Check method runs all checks
// concurrently and returns a report with per-check status. Wire this into your
// /readyz handler to give Kubernetes accurate readiness signals.
package health

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// Status is the result of a single dependency check.
type Status struct {
	Name    string `json:"name"`
	Healthy bool   `json:"healthy"`
	Latency int64  `json:"latency_ms"`
	Error   string `json:"error,omitempty"`
}

// Report is the aggregate result of all registered checks.
type Report struct {
	Healthy  bool     `json:"healthy"`
	Checks   []Status `json:"checks"`
	Duration int64    `json:"duration_ms"`
}

// CheckFunc verifies a dependency is reachable. Return nil for healthy.
type CheckFunc func(ctx context.Context) error

type namedCheck struct {
	name string
	fn   CheckFunc
}

const (
	// DefaultTimeout keeps readiness probes from hanging on a slow dependency.
	DefaultTimeout = 2 * time.Second
	// DefaultCacheTTL avoids hammering dependencies on every /readyz probe.
	DefaultCacheTTL = 5 * time.Second
)

// Checker holds a set of named health checks and runs them on demand.
type Checker struct {
	mu           sync.RWMutex
	cacheMu      sync.Mutex
	checks       []namedCheck
	cachedReport Report
	cacheUntil   time.Time
}

// New creates an empty Checker.
func New() *Checker {
	return &Checker{}
}

// Add registers a named health check.
func (c *Checker) Add(name string, fn CheckFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checks = append(c.checks, namedCheck{name: name, fn: fn})
	c.cachedReport = Report{}
	c.cacheUntil = time.Time{}
}

// Check runs all registered checks concurrently with the given timeout.
func (c *Checker) Check(ctx context.Context, timeout time.Duration) Report {
	start := time.Now()

	c.mu.RLock()
	snapshot := make([]namedCheck, len(c.checks))
	copy(snapshot, c.checks)
	c.mu.RUnlock()

	if len(snapshot) == 0 {
		return Report{Healthy: true, Duration: ms(time.Since(start))}
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	results := make([]Status, len(snapshot))
	var wg sync.WaitGroup
	wg.Add(len(snapshot))

	for i, chk := range snapshot {
		go func(i int, chk namedCheck) {
			defer wg.Done()
			t := time.Now()
			err := chk.fn(ctx)
			status := Status{Name: chk.name, Healthy: err == nil, Latency: ms(time.Since(t))}
			if err != nil {
				status.Error = err.Error()
			}
			results[i] = status
		}(i, chk)
	}
	wg.Wait()

	healthy := true
	for _, status := range results {
		if !status.Healthy {
			healthy = false
			break
		}
	}
	return Report{Healthy: healthy, Checks: results, Duration: ms(time.Since(start))}
}

// CachedCheck reuses a recent readiness report for the given TTL.
func (c *Checker) CachedCheck(ctx context.Context, timeout time.Duration, ttl time.Duration) Report {
	if ttl <= 0 {
		return c.Check(ctx, timeout)
	}
	if report, ok := c.cached(time.Now()); ok {
		return report
	}

	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()

	if report, ok := c.cached(time.Now()); ok {
		return report
	}

	report := c.Check(detach(ctx), timeout)

	c.mu.Lock()
	c.cachedReport = report
	c.cacheUntil = time.Now().Add(ttl)
	c.mu.Unlock()

	return report
}

// Handler returns an http.HandlerFunc that runs checks and writes a JSON report.
// Returns 200 if all checks pass, 503 otherwise.
func (c *Checker) Handler(timeout time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		report := c.Check(r.Context(), timeout)
		w.Header().Set("Content-Type", "application/json")
		if !report.Healthy {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(report)
	}
}

// CachedHandler returns a readiness handler that reuses recent reports for ttl.
func (c *Checker) CachedHandler(timeout time.Duration, ttl time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		report := c.CachedCheck(r.Context(), timeout, ttl)
		w.Header().Set("Content-Type", "application/json")
		if !report.Healthy {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(report)
	}
}

// ReadyzHandler returns the shared cached readiness handler.
func (c *Checker) ReadyzHandler() http.HandlerFunc {
	return c.CachedHandler(DefaultTimeout, DefaultCacheTTL)
}

// Pinger is implemented by dependencies that expose Ping(ctx) error.
type Pinger interface {
	Ping(ctx context.Context) error
}

// ContextPinger is implemented by *sql.DB and *pgxpool.Pool.
type ContextPinger interface {
	PingContext(ctx context.Context) error
}

// PingCheck returns a CheckFunc that pings the given dependency.
func PingCheck(p Pinger) CheckFunc {
	return func(ctx context.Context) error {
		if p == nil {
			return errors.New("dependency_not_configured")
		}
		return p.Ping(ctx)
	}
}

// PostgresCheck returns a CheckFunc that pings a Postgres-compatible dependency.
func PostgresCheck(p ContextPinger) CheckFunc {
	return func(ctx context.Context) error {
		if p == nil {
			return errors.New("postgres_not_configured")
		}
		return p.PingContext(ctx)
	}
}

// HTTPCheck returns a CheckFunc that verifies an upstream responds with 2xx.
func HTTPCheck(client *http.Client, url string) CheckFunc {
	if client == nil {
		client = http.DefaultClient
	}

	return func(ctx context.Context) error {
		if url == "" {
			return errors.New("http_url_required")
		}

		request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}

		response, err := client.Do(request)
		if err != nil {
			return err
		}
		defer func() {
			_ = response.Body.Close()
		}()
		_, _ = io.Copy(io.Discard, response.Body)

		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			return fmt.Errorf("unexpected_status: %d", response.StatusCode)
		}
		return nil
	}
}

// TCPCheck returns a CheckFunc that dials the given address.
func TCPCheck(addr string) CheckFunc {
	return func(ctx context.Context) error {
		d := net.Dialer{Timeout: 2 * time.Second}
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return err
		}
		return conn.Close()
	}
}

func ms(d time.Duration) int64 {
	return d.Milliseconds()
}

func detach(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return context.WithoutCancel(ctx)
}

func (c *Checker) cached(now time.Time) (Report, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.cacheUntil.IsZero() || now.After(c.cacheUntil) {
		return Report{}, false
	}
	return c.cachedReport, true
}
