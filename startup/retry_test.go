package startup

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDoRetriesUntilSuccess(t *testing.T) {
	t.Parallel()

	attempts := 0
	err := Do(context.Background(), Config{MaxAttempts: 5, Delay: 0}, func(context.Context) error {
		attempts++
		if attempts < 3 {
			return errors.New("not ready")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected retry to succeed, got %v", err)
	}
	if attempts != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts)
	}
}

func TestDoReturnsLastError(t *testing.T) {
	t.Parallel()

	attempts := 0
	err := Do(context.Background(), Config{MaxAttempts: 2, Delay: 0}, func(context.Context) error {
		attempts++
		return errors.New("still unavailable")
	})
	if err == nil {
		t.Fatal("expected retry to fail")
	}
	if !strings.Contains(err.Error(), "startup failed after 2 attempts") {
		t.Fatalf("expected retry count in error, got %v", err)
	}
	if !strings.Contains(err.Error(), "still unavailable") {
		t.Fatalf("expected wrapped startup error, got %v", err)
	}
	if attempts != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts)
	}
}

func TestDoHonorsContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	err := Do(ctx, Config{MaxAttempts: 3, Delay: 10 * time.Millisecond}, func(context.Context) error {
		attempts++
		cancel()
		return errors.New("startup failure")
	})
	if err == nil {
		t.Fatal("expected retry to fail")
	}
	if !strings.Contains(err.Error(), "startup interrupted") {
		t.Fatalf("expected interruption error, got %v", err)
	}
	if attempts != 1 {
		t.Fatalf("expected cancellation after first attempt, got %d", attempts)
	}
}

func TestValueReturnsResult(t *testing.T) {
	t.Parallel()

	attempts := 0
	value, err := Value(context.Background(), Config{MaxAttempts: 5, Delay: 0}, func(context.Context) (int, error) {
		attempts++
		if attempts < 2 {
			return 0, errors.New("not ready")
		}
		return 42, nil
	})
	if err != nil {
		t.Fatalf("expected retry to succeed, got %v", err)
	}
	if value != 42 {
		t.Fatalf("expected value 42, got %d", value)
	}
}
