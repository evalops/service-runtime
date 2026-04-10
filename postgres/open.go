package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/evalops/service-runtime/startup"
)

type InitFunc func(context.Context, *sql.DB) error

type Options struct {
	DriverName  string
	PingTimeout time.Duration
	Retry       startup.Config
}

var sqlOpen = sql.Open

func Open(ctx context.Context, databaseURL string, opts Options) (*sql.DB, error) {
	return OpenAndInit(ctx, databaseURL, nil, opts)
}

func OpenAndInit(ctx context.Context, databaseURL string, init InitFunc, opts Options) (*sql.DB, error) {
	opts = withDefaults(opts)

	db, err := sqlOpen(opts.DriverName, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open_postgres: %w", err)
	}

	err = startup.Do(ctx, opts.Retry, func(ctx context.Context) error {
		attemptCtx, cancel := context.WithTimeout(ctx, opts.PingTimeout)
		defer cancel()

		if err := db.PingContext(attemptCtx); err != nil {
			return fmt.Errorf("ping_postgres: %w", err)
		}
		if init != nil {
			if err := init(attemptCtx, db); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	return db, nil
}

func withDefaults(opts Options) Options {
	if opts.DriverName == "" {
		opts.DriverName = "pgx"
	}
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
