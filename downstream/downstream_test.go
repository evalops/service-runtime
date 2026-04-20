package downstream_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/evalops/service-runtime/downstream"
	"github.com/evalops/service-runtime/resilience"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

var errSimulated = errors.New("simulated downstream failure")

// --- FailClosed tests ---

func TestFailClosedReturnsErrorOnFailure(t *testing.T) {
	c := downstream.New("governance", downstream.Config{
		FailureMode: downstream.FailClosed,
	})

	_, err := downstream.Call(context.Background(), c, func(_ context.Context) (string, error) {
		return "", errSimulated
	})

	if err == nil {
		t.Fatal("expected error from fail-closed client")
	}
	if !errors.Is(err, errSimulated) {
		t.Fatalf("expected simulated error, got: %v", err)
	}
}

func TestFailClosedReturnsResultOnSuccess(t *testing.T) {
	c := downstream.New("governance", downstream.Config{
		FailureMode: downstream.FailClosed,
	})

	result, err := downstream.Call(context.Background(), c, func(_ context.Context) (string, error) {
		return "allow", nil
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "allow" {
		t.Fatalf("expected allow, got %s", result)
	}
}

// --- FailOpen tests ---

func TestFailOpenSwallowsErrorAndReturnsZero(t *testing.T) {
	c := downstream.New("meter", downstream.Config{
		FailureMode: downstream.FailOpen,
	})

	result, err := downstream.Call(context.Background(), c, func(_ context.Context) (int, error) {
		return 0, errSimulated
	})

	if err != nil {
		t.Fatalf("fail-open should not return error, got: %v", err)
	}
	if result != 0 {
		t.Fatalf("expected zero value, got %d", result)
	}
}

func TestFailOpenReturnsResultOnSuccess(t *testing.T) {
	c := downstream.New("registry", downstream.Config{
		FailureMode: downstream.FailOpen,
	})

	result, err := downstream.Call(context.Background(), c, func(_ context.Context) (string, error) {
		return "active", nil
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "active" {
		t.Fatalf("expected active, got %s", result)
	}
}

// --- Circuit breaker integration ---

func TestFailClosedWithOpenBreakerReturnsCircuitError(t *testing.T) {
	breaker := resilience.NewBreaker(resilience.BreakerConfig{
		FailureThreshold: 1,
		ResetTimeout:     time.Hour, // stay open
	})
	c := downstream.New("governance", downstream.Config{
		FailureMode: downstream.FailClosed,
		Breaker:     breaker,
	})

	// Trip the breaker.
	_, _ = downstream.Call(context.Background(), c, func(_ context.Context) (string, error) {
		return "", errSimulated
	})

	// Next call should get circuit open error.
	_, err := downstream.Call(context.Background(), c, func(_ context.Context) (string, error) {
		t.Fatal("fn should not be called when breaker is open")
		return "", nil
	})

	if err == nil {
		t.Fatal("expected circuit open error from fail-closed client")
	}
	if !errors.Is(err, resilience.ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen, got: %v", err)
	}
}

func TestFailOpenWithOpenBreakerReturnsNil(t *testing.T) {
	breaker := resilience.NewBreaker(resilience.BreakerConfig{
		FailureThreshold: 1,
		ResetTimeout:     time.Hour,
	})
	c := downstream.New("meter", downstream.Config{
		FailureMode: downstream.FailOpen,
		Breaker:     breaker,
	})

	// Trip the breaker.
	_, _ = downstream.Call(context.Background(), c, func(_ context.Context) (string, error) {
		return "", errSimulated
	})

	// Next call should degrade gracefully.
	result, err := downstream.Call(context.Background(), c, func(_ context.Context) (string, error) {
		t.Fatal("fn should not be called when breaker is open")
		return "should-not-reach", nil
	})

	if err != nil {
		t.Fatalf("fail-open should not return error even with open breaker, got: %v", err)
	}
	if result != "" {
		t.Fatalf("expected zero value, got %s", result)
	}
}

func TestBreakerRecoverAfterSuccess(t *testing.T) {
	breaker := resilience.NewBreaker(resilience.BreakerConfig{
		FailureThreshold: 2,
		ResetTimeout:     10 * time.Millisecond,
		HalfOpenMax:      1,
	})
	c := downstream.New("registry", downstream.Config{
		FailureMode: downstream.FailOpen,
		Breaker:     breaker,
	})

	// Fail twice to trip.
	for i := 0; i < 2; i++ {
		_, _ = downstream.Call(context.Background(), c, func(_ context.Context) (string, error) {
			return "", errSimulated
		})
	}

	// Wait for half-open.
	time.Sleep(20 * time.Millisecond)

	// Succeed to recover.
	result, err := downstream.Call(context.Background(), c, func(_ context.Context) (string, error) {
		return "recovered", nil
	})

	if err != nil {
		t.Fatalf("unexpected error after recovery: %v", err)
	}
	if result != "recovered" {
		t.Fatalf("expected recovered, got %s", result)
	}
}

// --- Metrics ---

func TestMetricsRecordErrorsAndLatency(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := &downstream.Metrics{
		Errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "downstream_errors_total",
		}, []string{"downstream", "op"}),
		Latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "downstream_latency_seconds",
			Buckets: []float64{0.001, 0.01, 0.1},
		}, []string{"downstream", "op"}),
	}
	reg.MustRegister(metrics.Errors, metrics.Latency)

	c := downstream.New("governance", downstream.Config{
		FailureMode: downstream.FailClosed,
		Metrics:     metrics,
	})

	// Make one failed call.
	_, _ = downstream.CallOp(context.Background(), c, "evaluate", func(_ context.Context) (string, error) {
		return "", errSimulated
	})

	// Check error counter.
	counter, err := metrics.Errors.GetMetricWith(prometheus.Labels{"downstream": "governance", "op": "evaluate"})
	if err != nil {
		t.Fatalf("get metric: %v", err)
	}
	ch := make(chan prometheus.Metric, 1)
	counter.Collect(ch)
	m := <-ch
	var metric dto.Metric
	if err := m.Write(&metric); err != nil {
		t.Fatalf("write metric: %v", err)
	}
	if got := metric.GetCounter().GetValue(); got != 1 {
		t.Fatalf("errors_total = %v, want 1", got)
	}

	// Make one successful call.
	result, err := downstream.CallOp(context.Background(), c, "evaluate", func(_ context.Context) (string, error) {
		return "allow", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "allow" {
		t.Fatalf("expected allow, got %s", result)
	}

	// Error count should still be 1 (success does not increment).
	counter.Collect(ch)
	m = <-ch
	metric.Reset()
	if err := m.Write(&metric); err != nil {
		t.Fatalf("write metric: %v", err)
	}
	if got := metric.GetCounter().GetValue(); got != 1 {
		t.Fatalf("errors_total after success = %v, want still 1", got)
	}
}

// --- Struct type test (proves generics work with any type) ---

type governanceDecision struct {
	Decision  string
	RiskLevel int
}

func TestCallWithStructType(t *testing.T) {
	c := downstream.New("governance", downstream.Config{
		FailureMode: downstream.FailClosed,
	})

	result, err := downstream.Call(context.Background(), c, func(_ context.Context) (governanceDecision, error) {
		return governanceDecision{Decision: "allow", RiskLevel: 2}, nil
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Decision != "allow" || result.RiskLevel != 2 {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestFailOpenWithStructReturnsZeroValue(t *testing.T) {
	c := downstream.New("governance", downstream.Config{
		FailureMode: downstream.FailOpen,
	})

	result, err := downstream.Call(context.Background(), c, func(_ context.Context) (governanceDecision, error) {
		return governanceDecision{}, errSimulated
	})

	if err != nil {
		t.Fatalf("fail-open should not error: %v", err)
	}
	if result.Decision != "" || result.RiskLevel != 0 {
		t.Fatalf("expected zero value struct, got: %+v", result)
	}
}

// --- Name and Mode accessors ---

func TestClientAccessors(t *testing.T) {
	c := downstream.New("governance", downstream.Config{
		FailureMode: downstream.FailClosed,
	})

	if c.Name() != "governance" {
		t.Fatalf("expected governance, got %s", c.Name())
	}
	if c.Mode() != downstream.FailClosed {
		t.Fatalf("expected FailClosed, got %s", c.Mode())
	}
}

func TestFailureModeString(t *testing.T) {
	if downstream.FailClosed.String() != "fail-closed" {
		t.Fatalf("expected fail-closed, got %s", downstream.FailClosed.String())
	}
	if downstream.FailOpen.String() != "fail-open" {
		t.Fatalf("expected fail-open, got %s", downstream.FailOpen.String())
	}
}

func TestFailurePolicyAliasAndLegacyConstructor(t *testing.T) {
	policy := downstream.FailurePolicy(downstream.FailOpen)
	c := downstream.New("meter", policy, downstream.Config{})

	if c.Name() != "meter" {
		t.Fatalf("expected meter, got %s", c.Name())
	}
	if c.Mode() != downstream.FailOpen {
		t.Fatalf("expected FailOpen, got %s", c.Mode())
	}
}

func TestLegacyConstructorPreservesConfig(t *testing.T) {
	breaker := resilience.NewBreaker(resilience.BreakerConfig{
		FailureThreshold: 1,
		ResetTimeout:     time.Hour,
	})
	c := downstream.New("governance", downstream.FailClosed, downstream.Config{
		Breaker: breaker,
	})

	_, _ = downstream.Call(context.Background(), c, func(_ context.Context) (string, error) {
		return "", errSimulated
	})

	_, err := downstream.Call(context.Background(), c, func(_ context.Context) (string, error) {
		t.Fatal("fn should not be called when breaker is open")
		return "", nil
	})
	if !errors.Is(err, resilience.ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen, got: %v", err)
	}
}

// --- Nil logger safety ---

func TestNilLoggerDefaultsToSlog(t *testing.T) {
	c := downstream.New("test", downstream.Config{
		FailureMode: downstream.FailOpen,
		Logger:      nil,
	})

	// Should not panic.
	_, _ = downstream.Call(context.Background(), c, func(_ context.Context) (string, error) {
		return "", errSimulated
	})
}
