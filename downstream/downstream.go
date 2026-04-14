// Package downstream provides a resilient client wrapper for downstream
// service calls. It encodes fail-closed vs fail-open policy at construction
// time so call sites do not need to reason about failure modes.
package downstream

import (
	"context"
	"log/slog"
	"time"

	"github.com/evalops/service-runtime/resilience"
	"github.com/prometheus/client_golang/prometheus"
)

// FailureMode controls what happens when a downstream call fails.
type FailureMode int

const (
	// FailClosed returns the error to the caller. Use for safety-critical
	// services where a degraded response is worse than no response.
	// Examples: governance, approvals, identity.
	FailClosed FailureMode = iota

	// FailOpen returns the zero value with no error. Use for best-effort
	// services where the caller can continue without the result.
	// Examples: registry, meter, memory.
	FailOpen
)

func (m FailureMode) String() string {
	switch m {
	case FailClosed:
		return "fail-closed"
	case FailOpen:
		return "fail-open"
	default:
		return "unknown"
	}
}

// Config configures a downstream client.
type Config struct {
	FailureMode FailureMode
	Breaker     *resilience.Breaker // optional; nil disables circuit breaking
	Logger      *slog.Logger        // optional; nil disables logging
	Metrics     *Metrics            // optional; nil disables metrics
}

// Client wraps a downstream service with resilience policy.
type Client struct {
	name    string
	mode    FailureMode
	breaker *resilience.Breaker
	logger  *slog.Logger
	metrics *Metrics
}

// Metrics holds optional Prometheus instrumentation for a downstream client.
type Metrics struct {
	Errors  *prometheus.CounterVec   // labels: downstream, op
	Latency *prometheus.HistogramVec // labels: downstream, op
}

// New creates a downstream client with the given name and configuration.
func New(name string, cfg Config) *Client {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		name:    name,
		mode:    cfg.FailureMode,
		breaker: cfg.Breaker,
		logger:  logger,
		metrics: cfg.Metrics,
	}
}

// Name returns the downstream service name.
func (c *Client) Name() string { return c.name }

// Mode returns the configured failure mode.
func (c *Client) Mode() FailureMode { return c.mode }

// Call executes fn through the downstream client resilience policy.
func Call[T any](ctx context.Context, c *Client, fn func(context.Context) (T, error)) (T, error) {
	return CallOp[T](ctx, c, "", fn)
}

// CallOp is like Call but tags metrics and logs with an operation name.
func CallOp[T any](ctx context.Context, c *Client, op string, fn func(context.Context) (T, error)) (T, error) {
	var zero T
	start := time.Now()

	if c.breaker != nil {
		result, err := resilience.DoValue(ctx, c.breaker, fn)
		c.recordLatency(op, time.Since(start))
		if err != nil {
			c.recordError(op)
			return zero, c.failureResult(op, err)
		}
		return result, nil
	}

	// No breaker: call directly.
	result, err := fn(ctx)
	c.recordLatency(op, time.Since(start))
	if err != nil {
		c.recordError(op)
		return zero, c.failureResult(op, err)
	}
	return result, nil
}

// failureResult applies the failure mode policy.
func (c *Client) failureResult(op string, err error) error {
	switch c.mode {
	case FailOpen:
		c.logger.Warn("downstream call failed (fail-open)",
			"downstream", c.name, "op", op, "error", err)
		return nil
	default:
		c.logger.Error("downstream call failed (fail-closed)",
			"downstream", c.name, "op", op, "error", err)
		return err
	}
}

func (c *Client) recordLatency(op string, d time.Duration) {
	if c.metrics != nil && c.metrics.Latency != nil {
		c.metrics.Latency.With(prometheus.Labels{"downstream": c.name, "op": opLabel(op)}).Observe(d.Seconds())
	}
}

func (c *Client) recordError(op string) {
	if c.metrics != nil && c.metrics.Errors != nil {
		c.metrics.Errors.With(prometheus.Labels{"downstream": c.name, "op": opLabel(op)}).Inc()
	}
}

func opLabel(op string) string {
	if op == "" {
		return "call"
	}
	return op
}
