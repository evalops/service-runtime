// Package redisutil provides helpers for opening Redis connections with retry.
package redisutil

import (
	"context"
	"fmt"
	"time"

	"github.com/evalops/service-runtime/startup"
	"github.com/redis/go-redis/v9"
)

// Options configures Redis connection behavior including ping timeout and retry settings.
type Options struct {
	PingTimeout time.Duration
	Retry       startup.Config
}

var pingClient = func(ctx context.Context, client *redis.Client) error {
	return client.Ping(ctx).Err()
}

// Open creates a Redis client from the given URL, retrying the ping until it succeeds.
func Open(ctx context.Context, redisURL string, opts Options) (*redis.Client, error) {
	options, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("parse_redis_url: %w", err)
	}

	client := redis.NewClient(options)
	opts = withDefaults(opts)

	err = startup.Do(ctx, opts.Retry, func(ctx context.Context) error {
		attemptCtx, cancel := context.WithTimeout(ctx, opts.PingTimeout)
		defer cancel()
		return pingClient(attemptCtx, client)
	})
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping_redis: %w", err)
	}

	return client, nil
}

func withDefaults(opts Options) Options {
	if opts.PingTimeout <= 0 {
		opts.PingTimeout = 5 * time.Second
	}
	if opts.Retry == (startup.Config{}) {
		opts.Retry = startup.Config{
			MaxAttempts: startup.DefaultMaxAttempts,
			Delay:       startup.DefaultDelay,
		}
	}
	return opts
}
