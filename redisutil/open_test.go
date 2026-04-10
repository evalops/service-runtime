package redisutil

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/evalops/service-runtime/startup"
	"github.com/redis/go-redis/v9"
)

func TestOpenConnectsToRedis(t *testing.T) {
	server, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	defer server.Close()

	client, err := Open(context.Background(), "redis://"+server.Addr()+"/0", Options{
		Retry: startup.Config{MaxAttempts: 1, Delay: 0},
	})
	if err != nil {
		t.Fatalf("open redis: %v", err)
	}
	defer client.Close()
}

func TestOpenRetriesUntilSuccess(t *testing.T) {
	server, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	defer server.Close()

	previousPing := pingClient
	attempts := 0
	pingClient = func(ctx context.Context, client *redis.Client) error {
		attempts++
		if attempts < 3 {
			return errors.New("redis unavailable")
		}
		return previousPing(ctx, client)
	}
	defer func() { pingClient = previousPing }()

	client, err := Open(context.Background(), "redis://"+server.Addr()+"/0", Options{
		Retry: startup.Config{MaxAttempts: 3, Delay: 0},
	})
	if err != nil {
		t.Fatalf("expected redis open to succeed, got %v", err)
	}
	defer client.Close()
	if attempts != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts)
	}
}

func TestOpenReturnsLastError(t *testing.T) {
	server, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	defer server.Close()

	previousPing := pingClient
	pingClient = func(context.Context, *redis.Client) error {
		return errors.New("still unavailable")
	}
	defer func() { pingClient = previousPing }()

	_, err = Open(context.Background(), "redis://"+server.Addr()+"/0", Options{
		Retry: startup.Config{MaxAttempts: 2, Delay: 0},
	})
	if err == nil {
		t.Fatal("expected redis open to fail")
	}
	if !strings.Contains(err.Error(), "startup failed after 2 attempts") {
		t.Fatalf("expected retry count in error, got %v", err)
	}
}
