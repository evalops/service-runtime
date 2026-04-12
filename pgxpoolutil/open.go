// Package pgxpoolutil provides helpers for opening and pinging pgxpool connections with retry.
package pgxpoolutil

import (
	"context"
	"fmt"
	"time"

	"github.com/evalops/service-runtime/startup"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Options configures pgxpool connection behavior including ping timeout and retry settings.
type Options struct {
	PingTimeout time.Duration
	Retry       startup.Config
	Configure   func(*pgxpool.Config) error
}

var (
	parseConfig = pgxpool.ParseConfig
	newPool     = pgxpool.NewWithConfig
	pingPool    = func(ctx context.Context, pool *pgxpool.Pool) error {
		return pool.Ping(ctx)
	}
	closePool = func(pool *pgxpool.Pool) {
		pool.Close()
	}
)

// Open creates a pgxpool.Pool, retrying the connection until it succeeds or attempts are exhausted.
func Open(ctx context.Context, dsn string, opts Options) (*pgxpool.Pool, error) {
	opts = withDefaults(opts)

	return startup.Value(ctx, opts.Retry, func(ctx context.Context) (*pgxpool.Pool, error) {
		cfg, err := parseConfig(dsn)
		if err != nil {
			return nil, fmt.Errorf("parse_pgxpool_config: %w", err)
		}
		if opts.Configure != nil {
			if configureErr := opts.Configure(cfg); configureErr != nil {
				return nil, fmt.Errorf("configure_pgxpool: %w", configureErr)
			}
		}

		pool, err := newPool(ctx, cfg)
		if err != nil {
			return nil, fmt.Errorf("open_pgxpool: %w", err)
		}

		attemptCtx, cancel := context.WithTimeout(ctx, opts.PingTimeout)
		defer cancel()
		if err := pingPool(attemptCtx, pool); err != nil {
			closePool(pool)
			return nil, fmt.Errorf("ping_pgxpool: %w", err)
		}

		return pool, nil
	})
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
