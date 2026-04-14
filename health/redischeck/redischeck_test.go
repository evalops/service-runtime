package redischeck_test

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/evalops/service-runtime/health/redischeck"
	"github.com/redis/go-redis/v9"
)

func TestCheck(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() {
		_ = client.Close()
	})

	check := redischeck.Check(client)
	if err := check(context.Background()); err != nil {
		t.Fatalf("healthy redis ping returned error: %v", err)
	}

	server.Close()
	if err := check(context.Background()); err == nil {
		t.Fatal("expected redis check to report unhealthy dependency")
	}
}
