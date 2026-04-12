// Package health provides dependency-aware readiness checks for EvalOps services.
//
// Services register named checks at startup. The Check method runs all checks
// concurrently and returns a report with per-check status. Wire this into your
// /readyz handler to give Kubernetes accurate readiness signals.
package health

import (
	"context"
	"encoding/json"
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

// Checker holds a set of named health checks and runs them on demand.
type Checker struct {
	mu     sync.RWMutex
	checks []namedCheck
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
			s := Status{Name: chk.name, Healthy: err == nil, Latency: ms(time.Since(t))}
			if err != nil {
				s.Error = err.Error()
			}
			results[i] = s
		}(i, chk)
	}
	wg.Wait()

	healthy := true
	for _, s := range results {
		if !s.Healthy {
			healthy = false
			break
		}
	}
	return Report{Healthy: healthy, Checks: results, Duration: ms(time.Since(start))}
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

// Pinger is implemented by *pgxpool.Pool, *redis.Client, etc.
type Pinger interface {
	Ping(ctx context.Context) error
}

// PingCheck returns a CheckFunc that pings the given dependency.
func PingCheck(p Pinger) CheckFunc {
	return func(ctx context.Context) error {
		return p.Ping(ctx)
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
