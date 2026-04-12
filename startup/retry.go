package startup

import (
	"context"
	"fmt"
	"time"
)

const (
	DefaultMaxAttempts = 30
	DefaultDelay       = 2 * time.Second
)

type Config struct {
	MaxAttempts int
	Delay       time.Duration
}

func (c Config) withDefaults() Config {
	if c.MaxAttempts < 1 {
		c.MaxAttempts = DefaultMaxAttempts
	}
	if c.Delay < 0 {
		c.Delay = 0
	}
	return c
}

func Do(ctx context.Context, cfg Config, operation func(context.Context) error) error {
	cfg = cfg.withDefaults()

	var lastErr error
	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		err := operation(ctx)
		if err == nil {
			return nil
		}
		lastErr = err

		if attempt == cfg.MaxAttempts {
			break
		}

		timer := time.NewTimer(cfg.Delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("startup interrupted: %w", ctx.Err())
		case <-timer.C:
		}
	}

	return fmt.Errorf("startup failed after %d attempts: %w", cfg.MaxAttempts, lastErr)
}

func Value[T any](ctx context.Context, cfg Config, operation func(context.Context) (T, error)) (T, error) {
	var zero T
	var value T
	err := Do(ctx, cfg, func(ctx context.Context) error {
		result, err := operation(ctx)
		if err != nil {
			return err
		}
		value = result
		return nil
	})
	if err != nil {
		return zero, err
	}
	return value, nil
}
