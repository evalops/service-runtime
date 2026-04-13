package resilience

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"time"
)

// RetryConfig tunes exponential-backoff retry behaviour.
type RetryConfig struct {
	MaxAttempts  int                // total attempts including the first (default: 3)
	InitialDelay time.Duration     // base delay before first retry (default: 100ms)
	MaxDelay     time.Duration     // upper bound on computed delay (default: 10s)
	Multiplier   float64           // backoff multiplier (default: 2.0)
	IsRetryable  func(error) bool  // if non-nil, called to decide retryability
}

func (c RetryConfig) withDefaults() RetryConfig {
	if c.MaxAttempts < 1 {
		c.MaxAttempts = 3
	}
	if c.InitialDelay <= 0 {
		c.InitialDelay = 100 * time.Millisecond
	}
	if c.MaxDelay <= 0 {
		c.MaxDelay = 10 * time.Second
	}
	if c.Multiplier <= 0 {
		c.Multiplier = 2.0
	}
	return c
}

// PermanentError wraps an error to signal that it should not be retried.
type PermanentError struct {
	Err error
}

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent wraps err so that Retry will not attempt further retries.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// Retry executes fn with exponential backoff and full jitter. It stops early
// when fn returns nil, a PermanentError, or when IsRetryable returns false.
func Retry(ctx context.Context, cfg RetryConfig, fn func(context.Context) error) error {
	cfg = cfg.withDefaults()

	var lastErr error
	for attempt := 0; attempt < cfg.MaxAttempts; attempt++ {
		lastErr = fn(ctx)
		if lastErr == nil {
			return nil
		}

		// Stop on permanent error.
		var pe *PermanentError
		if errors.As(lastErr, &pe) {
			return pe.Err
		}

		// Stop if error classified as non-retryable.
		if cfg.IsRetryable != nil && !cfg.IsRetryable(lastErr) {
			return lastErr
		}

		// Don't sleep after the last attempt.
		if attempt == cfg.MaxAttempts-1 {
			break
		}

		delay := backoffDelay(cfg, attempt)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("retry interrupted: %w", ctx.Err())
		case <-timer.C:
		}
	}

	return fmt.Errorf("retry failed after %d attempts: %w", cfg.MaxAttempts, lastErr)
}

// backoffDelay calculates the jittered delay for the given attempt (0-indexed).
// Formula: min(initialDelay * multiplier^attempt, maxDelay), then full jitter.
func backoffDelay(cfg RetryConfig, attempt int) time.Duration {
	delayF := float64(cfg.InitialDelay) * math.Pow(cfg.Multiplier, float64(attempt))
	if delayF > float64(cfg.MaxDelay) {
		delayF = float64(cfg.MaxDelay)
	}
	// Full jitter: uniform random in [0, delay).
	if delayF <= 0 {
		return 0
	}
	jittered := rand.Int64N(int64(delayF))
	return time.Duration(jittered)
}

// RetryValue is like Retry but returns a value on success.
func RetryValue[T any](ctx context.Context, cfg RetryConfig, fn func(context.Context) (T, error)) (T, error) {
	var zero T
	var result T
	err := Retry(ctx, cfg, func(ctx context.Context) error {
		v, e := fn(ctx)
		if e != nil {
			return e
		}
		result = v
		return nil
	})
	if err != nil {
		return zero, err
	}
	return result, nil
}
