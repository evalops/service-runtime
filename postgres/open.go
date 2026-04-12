// Package postgres provides helpers for opening and initializing database/sql connections with retry.
package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/evalops/service-runtime/startup"
)

// InitFunc is a callback invoked after a successful ping to initialize the database.
type InitFunc func(context.Context, *sql.DB) error

// Options configures the Postgres connection driver, ping timeout, and retry behavior.
type Options struct {
	DriverName  string
	PingTimeout time.Duration
	Retry       startup.Config
}

var sqlOpen = sql.Open

// Open opens a Postgres connection and verifies it with a ping, retrying on failure.
func Open(ctx context.Context, databaseURL string, opts Options) (*sql.DB, error) {
	return OpenAndInit(ctx, databaseURL, nil, opts)
}

// OpenAndInit opens a Postgres connection, pings it, and runs the init function, retrying on failure.
func OpenAndInit(ctx context.Context, databaseURL string, init InitFunc, opts Options) (*sql.DB, error) {
	opts = withDefaults(opts)

	db, err := sqlOpen(opts.DriverName, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open_postgres: %w", err)
	}

	err = startup.Do(ctx, opts.Retry, func(ctx context.Context) error {
		attemptCtx, cancel := context.WithTimeout(ctx, opts.PingTimeout)
		defer cancel()

		if pingErr := db.PingContext(attemptCtx); pingErr != nil {
			return fmt.Errorf("ping_postgres: %w", pingErr)
		}
		if init != nil {
			if initErr := init(attemptCtx, db); initErr != nil {
				return initErr
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
