package pgxpoolutil

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/evalops/service-runtime/startup"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestOpenRetriesUntilSuccess(t *testing.T) {
	previousParseConfig := parseConfig
	previousNewPool := newPool
	previousPingPool := pingPool
	previousClosePool := closePool
	defer func() {
		parseConfig = previousParseConfig
		newPool = previousNewPool
		pingPool = previousPingPool
		closePool = previousClosePool
	}()

	attempts := 0
	parseConfig = func(string) (*pgxpool.Config, error) {
		return &pgxpool.Config{}, nil
	}
	newPool = func(context.Context, *pgxpool.Config) (*pgxpool.Pool, error) {
		return &pgxpool.Pool{}, nil
	}
	pingPool = func(context.Context, *pgxpool.Pool) error {
		attempts++
		if attempts < 3 {
			return errors.New("not ready")
		}
		return nil
	}
	closePool = func(*pgxpool.Pool) {}

	pool, err := Open(context.Background(), "postgres://gate", Options{
		Retry: startup.Config{MaxAttempts: 3, Delay: 0},
	})
	if err != nil {
		t.Fatalf("expected open to succeed, got %v", err)
	}
	if pool == nil {
		t.Fatal("expected pool")
	}
	if attempts != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts)
	}
}

func TestOpenReturnsLastError(t *testing.T) {
	previousParseConfig := parseConfig
	previousNewPool := newPool
	previousPingPool := pingPool
	previousClosePool := closePool
	defer func() {
		parseConfig = previousParseConfig
		newPool = previousNewPool
		pingPool = previousPingPool
		closePool = previousClosePool
	}()

	parseConfig = func(string) (*pgxpool.Config, error) {
		return &pgxpool.Config{}, nil
	}
	newPool = func(context.Context, *pgxpool.Config) (*pgxpool.Pool, error) {
		return &pgxpool.Pool{}, nil
	}
	pingPool = func(context.Context, *pgxpool.Pool) error {
		return errors.New("still unavailable")
	}
	closePool = func(*pgxpool.Pool) {}

	_, err := Open(context.Background(), "postgres://gate", Options{
		Retry: startup.Config{MaxAttempts: 2, Delay: 0},
	})
	if err == nil {
		t.Fatal("expected open to fail")
	}
	if !strings.Contains(err.Error(), "startup failed after 2 attempts") {
		t.Fatalf("expected retry count in error, got %v", err)
	}
	if !strings.Contains(err.Error(), "still unavailable") {
		t.Fatalf("expected wrapped ping error, got %v", err)
	}
}
