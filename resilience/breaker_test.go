package resilience

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBreakerAllowsCallsWhenClosed(t *testing.T) {
	t.Parallel()

	b := NewBreaker(BreakerConfig{})
	called := false
	err := b.Do(context.Background(), func(context.Context) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if !called {
		t.Fatal("expected function to be called")
	}
	if b.State() != StateClosed {
		t.Fatalf("expected state Closed, got %v", b.State())
	}
}

func TestBreakerOpensAfterFailureThreshold(t *testing.T) {
	t.Parallel()

	b := NewBreaker(BreakerConfig{FailureThreshold: 3, ResetTimeout: time.Hour})
	opErr := errors.New("operation failed")

	for i := 0; i < 3; i++ {
		err := b.Do(context.Background(), func(context.Context) error {
			return opErr
		})
		if !errors.Is(err, opErr) {
			t.Fatalf("attempt %d: expected operation error, got %v", i+1, err)
		}
	}

	if b.State() != StateOpen {
		t.Fatalf("expected state Open after %d failures, got %v", 3, b.State())
	}
}

func TestBreakerRejectsCallsWhenOpen(t *testing.T) {
	t.Parallel()

	b := NewBreaker(BreakerConfig{FailureThreshold: 1, ResetTimeout: time.Hour})

	// Trip the breaker.
	_ = b.Do(context.Background(), func(context.Context) error {
		return errors.New("fail")
	})

	called := false
	err := b.Do(context.Background(), func(context.Context) error {
		called = true
		return nil
	})
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen, got %v", err)
	}
	if called {
		t.Fatal("function should not be called when circuit is open")
	}
}

func TestBreakerTransitionsToHalfOpenAfterTimeout(t *testing.T) {
	t.Parallel()

	now := time.Now()
	b := NewBreaker(BreakerConfig{FailureThreshold: 1, ResetTimeout: 50 * time.Millisecond})
	b.timeNow = func() time.Time { return now }

	// Trip the breaker.
	_ = b.Do(context.Background(), func(context.Context) error {
		return errors.New("fail")
	})
	if b.State() != StateOpen {
		t.Fatalf("expected Open, got %v", b.State())
	}

	// Advance time past the reset timeout.
	now = now.Add(100 * time.Millisecond)

	if b.State() != StateHalfOpen {
		t.Fatalf("expected HalfOpen after reset timeout, got %v", b.State())
	}
}

func TestBreakerClosesOnHalfOpenSuccess(t *testing.T) {
	t.Parallel()

	now := time.Now()
	b := NewBreaker(BreakerConfig{FailureThreshold: 1, ResetTimeout: 50 * time.Millisecond})
	b.timeNow = func() time.Time { return now }

	// Trip the breaker.
	_ = b.Do(context.Background(), func(context.Context) error {
		return errors.New("fail")
	})

	// Advance time past the reset timeout.
	now = now.Add(100 * time.Millisecond)

	// Successful probe should close the circuit.
	err := b.Do(context.Background(), func(context.Context) error {
		return nil
	})
	if err != nil {
		t.Fatalf("expected nil error for half-open probe, got %v", err)
	}
	if b.State() != StateClosed {
		t.Fatalf("expected Closed after successful half-open probe, got %v", b.State())
	}
}

func TestBreakerReopensOnHalfOpenFailure(t *testing.T) {
	t.Parallel()

	now := time.Now()
	b := NewBreaker(BreakerConfig{FailureThreshold: 1, ResetTimeout: 50 * time.Millisecond})
	b.timeNow = func() time.Time { return now }

	// Trip the breaker.
	_ = b.Do(context.Background(), func(context.Context) error {
		return errors.New("fail")
	})

	// Advance time past the reset timeout.
	now = now.Add(100 * time.Millisecond)
	if b.State() != StateHalfOpen {
		t.Fatalf("expected HalfOpen, got %v", b.State())
	}

	// Failed probe should re-open the circuit.
	_ = b.Do(context.Background(), func(context.Context) error {
		return errors.New("still broken")
	})
	if b.State() != StateOpen {
		t.Fatalf("expected Open after failed half-open probe, got %v", b.State())
	}
}

func TestBreakerResetClearsState(t *testing.T) {
	t.Parallel()

	b := NewBreaker(BreakerConfig{FailureThreshold: 1, ResetTimeout: time.Hour})

	// Trip the breaker.
	_ = b.Do(context.Background(), func(context.Context) error {
		return errors.New("fail")
	})
	if b.State() != StateOpen {
		t.Fatalf("expected Open, got %v", b.State())
	}

	b.Reset()
	if b.State() != StateClosed {
		t.Fatalf("expected Closed after reset, got %v", b.State())
	}

	// Should allow calls again.
	err := b.Do(context.Background(), func(context.Context) error {
		return nil
	})
	if err != nil {
		t.Fatalf("expected nil error after reset, got %v", err)
	}
}

func TestBreakerHonorsContextCancellation(t *testing.T) {
	t.Parallel()

	b := NewBreaker(BreakerConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := b.Do(ctx, func(ctx context.Context) error {
		// The function itself checks the context.
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestBreakerConcurrentAccess(t *testing.T) {
	t.Parallel()

	b := NewBreaker(BreakerConfig{FailureThreshold: 50, ResetTimeout: time.Hour})
	var wg sync.WaitGroup
	var successCount atomic.Int64
	var failureCount atomic.Int64

	const goroutines = 100
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(n int) {
			defer wg.Done()
			err := b.Do(context.Background(), func(context.Context) error {
				if n%2 == 0 {
					return errors.New("fail")
				}
				return nil
			})
			if err != nil {
				failureCount.Add(1)
			} else {
				successCount.Add(1)
			}
		}(i)
	}
	wg.Wait()

	total := successCount.Load() + failureCount.Load()
	if total != goroutines {
		t.Fatalf("expected %d total calls, got %d", goroutines, total)
	}

	// State should be valid (no panics, no data races — the -race flag catches those).
	state := b.State()
	if state != StateClosed && state != StateOpen {
		t.Fatalf("unexpected state %v after concurrent access", state)
	}
}

func TestBreakerSuccessResetsFailureCount(t *testing.T) {
	t.Parallel()

	b := NewBreaker(BreakerConfig{FailureThreshold: 3, ResetTimeout: time.Hour})

	// Two failures, then a success.
	for i := 0; i < 2; i++ {
		_ = b.Do(context.Background(), func(context.Context) error {
			return errors.New("fail")
		})
	}
	_ = b.Do(context.Background(), func(context.Context) error {
		return nil
	})

	// Two more failures should not open because the counter was reset.
	for i := 0; i < 2; i++ {
		_ = b.Do(context.Background(), func(context.Context) error {
			return errors.New("fail")
		})
	}
	if b.State() != StateClosed {
		t.Fatalf("expected Closed (counter should have reset on success), got %v", b.State())
	}
}

func TestDoValueBreaker(t *testing.T) {
	t.Parallel()

	b := NewBreaker(BreakerConfig{})
	val, err := DoValue(context.Background(), b, func(context.Context) (int, error) {
		return 42, nil
	})
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if val != 42 {
		t.Fatalf("expected 42, got %d", val)
	}
}

func TestDoValueBreakerRejectsWhenOpen(t *testing.T) {
	t.Parallel()

	b := NewBreaker(BreakerConfig{FailureThreshold: 1, ResetTimeout: time.Hour})
	_ = b.Do(context.Background(), func(context.Context) error {
		return errors.New("trip")
	})

	val, err := DoValue(context.Background(), b, func(context.Context) (string, error) {
		return "should not reach", nil
	})
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen, got %v", err)
	}
	if val != "" {
		t.Fatalf("expected zero value, got %q", val)
	}
}
