package redischeck

import (
	"context"
	"errors"

	"github.com/evalops/service-runtime/health"
	"github.com/redis/go-redis/v9"
)

type pinger interface {
	Ping(ctx context.Context) *redis.StatusCmd
}

// Check returns a health.CheckFunc that pings a Redis-compatible dependency.
func Check(client pinger) health.CheckFunc {
	return func(ctx context.Context) error {
		if client == nil {
			return errors.New("redis_not_configured")
		}
		return client.Ping(ctx).Err()
	}
}
