package resilience

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetrySucceedsOnFirstAttempt(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	err := Retry(context.Background(), RetryConfig{MaxAttempts: 3}, func(context.Context) error {
		attempts.Add(1)
		return nil
	})
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if attempts.Load() != 1 {
		t.Fatalf("expected 1 attempt, got %d", attempts.Load())
	}
}

func TestRetrySucceedsAfterTransientFailures(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	err := Retry(context.Background(), RetryConfig{
		MaxAttempts:  5,
		InitialDelay: time.Millisecond,
		MaxDelay:     10 * time.Millisecond,
	}, func(context.Context) error {
		n := attempts.Add(1)
		if n < 3 {
			return errors.New("transient failure")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected nil error after transient failures, got %v", err)
	}
	if attempts.Load() != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts.Load())
	}
}

func TestRetryExhaustsMaxAttempts(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	persistentErr := errors.New("persistent failure")
	err := Retry(context.Background(), RetryConfig{
		MaxAttempts:  4,
		InitialDelay: time.Millisecond,
		MaxDelay:     5 * time.Millisecond,
	}, func(context.Context) error {
		attempts.Add(1)
		return persistentErr
	})
	if err == nil {
		t.Fatal("expected error after exhausting attempts")
	}
	if !errors.Is(err, persistentErr) {
		t.Fatalf("expected persistent error to be wrapped, got %v", err)
	}
	if attempts.Load() != 4 {
		t.Fatalf("expected 4 attempts, got %d", attempts.Load())
	}
}

func TestRetryStopsOnPermanentError(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	inner := errors.New("bad request")
	err := Retry(context.Background(), RetryConfig{
		MaxAttempts:  10,
		InitialDelay: time.Millisecond,
	}, func(context.Context) error {
		attempts.Add(1)
		return Permanent(inner)
	})
	if err == nil {
		t.Fatal("expected error from permanent failure")
	}
	if !errors.Is(err, inner) {
		t.Fatalf("expected inner error to be unwrapped, got %v", err)
	}
	if !strings.Contains(err.Error(), "retry permanent after 1 attempts") {
		t.Fatalf("expected wrapped permanent error message, got %v", err)
	}
	if attempts.Load() != 1 {
		t.Fatalf("expected 1 attempt (permanent stops immediately), got %d", attempts.Load())
	}
}

func TestRetryStopsWhenIsRetryableReturnsFalse(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	notRetryable := errors.New("not retryable")
	err := Retry(context.Background(), RetryConfig{
		MaxAttempts:  10,
		InitialDelay: time.Millisecond,
		IsRetryable: func(err error) bool {
			return !errors.Is(err, notRetryable)
		},
	}, func(context.Context) error {
		attempts.Add(1)
		return notRetryable
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, notRetryable) {
		t.Fatalf("expected notRetryable error, got %v", err)
	}
	if !strings.Contains(err.Error(), "retry non-retryable after 1 attempts") {
		t.Fatalf("expected wrapped non-retryable error message, got %v", err)
	}
	if attempts.Load() != 1 {
		t.Fatalf("expected 1 attempt (IsRetryable returned false), got %d", attempts.Load())
	}
}

func TestRetryHonorsContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	var attempts atomic.Int32
	err := Retry(ctx, RetryConfig{
		MaxAttempts:  100,
		InitialDelay: 10 * time.Millisecond,
		MaxDelay:     50 * time.Millisecond,
	}, func(context.Context) error {
		n := attempts.Add(1)
		if n >= 2 {
			cancel()
		}
		return errors.New("keep failing")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestRetryAppliesExponentialBackoff(t *testing.T) {
	t.Parallel()

	// Track the delays by recording timestamps of each attempt.
	var timestamps []time.Time
	cfg := RetryConfig{
		MaxAttempts:  4,
		InitialDelay: 20 * time.Millisecond,
		MaxDelay:     500 * time.Millisecond,
		Multiplier:   2.0,
	}
	_ = Retry(context.Background(), cfg, func(context.Context) error {
		timestamps = append(timestamps, time.Now())
		return errors.New("fail")
	})

	if len(timestamps) != 4 {
		t.Fatalf("expected 4 timestamps, got %d", len(timestamps))
	}

	// With jitter, each delay is in [0, initialDelay * multiplier^attempt].
	// We can't assert exact delays, but we can verify the total elapsed time
	// is reasonable (not zero, and not exceeding the sum of max delays).
	totalElapsed := timestamps[len(timestamps)-1].Sub(timestamps[0])
	if totalElapsed < 5*time.Millisecond {
		t.Fatalf("total elapsed %v seems too short for exponential backoff", totalElapsed)
	}

	// Verify delays generally increase (with some tolerance for jitter).
	// The max possible delays are: 20ms, 40ms, 80ms.
	// Just verify total time is under the sum of maximums + generous buffer.
	maxTotal := 20*time.Millisecond + 40*time.Millisecond + 80*time.Millisecond + 100*time.Millisecond
	if totalElapsed > maxTotal {
		t.Fatalf("total elapsed %v exceeds expected maximum %v", totalElapsed, maxTotal)
	}
}

func TestRetryValueReturnsResult(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	val, err := RetryValue(context.Background(), RetryConfig{
		MaxAttempts:  5,
		InitialDelay: time.Millisecond,
	}, func(context.Context) (string, error) {
		n := attempts.Add(1)
		if n < 3 {
			return "", errors.New("not yet")
		}
		return "hello", nil
	})
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if val != "hello" {
		t.Fatalf("expected %q, got %q", "hello", val)
	}
	if attempts.Load() != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts.Load())
	}
}

func TestRetryDelayDoesNotExceedMaxDelay(t *testing.T) {
	t.Parallel()

	// With a very small max delay and many attempts, ensure we don't sleep too long.
	maxDelay := 5 * time.Millisecond
	cfg := RetryConfig{
		MaxAttempts:  6,
		InitialDelay: time.Millisecond,
		MaxDelay:     maxDelay,
		Multiplier:   10.0, // aggressive multiplier to hit the cap quickly
	}

	start := time.Now()
	_ = Retry(context.Background(), cfg, func(context.Context) error {
		return errors.New("fail")
	})
	elapsed := time.Since(start)

	// 5 delays of at most 5ms each = 25ms max. Allow generous buffer for scheduling.
	upperBound := 5*maxDelay + 50*time.Millisecond
	if elapsed > upperBound {
		t.Fatalf("elapsed %v exceeds upper bound %v (max delay should cap backoff)", elapsed, upperBound)
	}
}

func TestRetryDefaultConfig(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	_ = Retry(context.Background(), RetryConfig{}, func(context.Context) error {
		n := attempts.Add(1)
		if n < 3 {
			return errors.New("fail")
		}
		return nil
	})
	// Default MaxAttempts is 3, so this should succeed on the 3rd try.
	if attempts.Load() != 3 {
		t.Fatalf("expected 3 attempts with default config, got %d", attempts.Load())
	}
}

func TestPermanentNil(t *testing.T) {
	t.Parallel()

	if Permanent(nil) != nil {
		t.Fatal("expected Permanent(nil) to return nil")
	}
}

func TestPermanentErrorUnwrap(t *testing.T) {
	t.Parallel()

	inner := errors.New("bad input")
	pErr := Permanent(inner)

	var pe *PermanentError
	if !errors.As(pErr, &pe) {
		t.Fatal("expected errors.As to match PermanentError")
	}
	if !errors.Is(pErr, inner) {
		t.Fatal("expected errors.Is to match inner error")
	}
	if pe.Error() != inner.Error() {
		t.Fatalf("expected %q, got %q", inner.Error(), pe.Error())
	}
}
