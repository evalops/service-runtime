package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"time"

	runtimemigrate "github.com/evalops/service-runtime/migrate"
	"github.com/evalops/service-runtime/startup"
)

// InitFunc is called after a successful ping to perform schema setup or migrations.
type InitFunc func(context.Context, *sql.DB) error

// Options configures postgres connection behaviour.
type Options struct {
	DriverName  string
	PingTimeout time.Duration
	Retry       startup.Config
}

var sqlOpen = sql.Open
var runMigrations = runtimemigrate.Run
var runIOFSMigrations = runtimemigrate.RunIOFS

// Open connects to a Postgres database, waiting for it to become ready.
func Open(ctx context.Context, databaseURL string, opts Options) (*sql.DB, error) {
	return OpenAndInit(ctx, databaseURL, nil, opts)
}

// OpenAndInit connects to a Postgres database and, after a successful ping, calls init.
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

// OpenAndMigrate connects to Postgres, applies file-based migrations, and then calls init.
func OpenAndMigrate(
	ctx context.Context,
	databaseURL string,
	migrationsDir string,
	migrateOpts runtimemigrate.Options,
	init InitFunc,
	opts Options,
) (*sql.DB, runtimemigrate.Result, error) {
	var result runtimemigrate.Result
	db, err := OpenAndInit(ctx, databaseURL, func(ctx context.Context, db *sql.DB) error {
		applied, err := runMigrations(ctx, db, migrationsDir, migrateOpts)
		if err != nil {
			return err
		}
		result = applied
		if init != nil {
			return init(ctx, db)
		}
		return nil
	}, opts)
	if err != nil {
		return nil, runtimemigrate.Result{}, err
	}
	return db, result, nil
}

// OpenAndMigrateIOFS connects to Postgres, applies embedded io/fs migrations, and then calls init.
func OpenAndMigrateIOFS(
	ctx context.Context,
	databaseURL string,
	fsys fs.FS,
	path string,
	migrateOpts runtimemigrate.Options,
	init InitFunc,
	opts Options,
) (*sql.DB, runtimemigrate.Result, error) {
	var result runtimemigrate.Result
	db, err := OpenAndInit(ctx, databaseURL, func(ctx context.Context, db *sql.DB) error {
		applied, err := runIOFSMigrations(ctx, db, fsys, path, migrateOpts)
		if err != nil {
			return err
		}
		result = applied
		if init != nil {
			return init(ctx, db)
		}
		return nil
	}, opts)
	if err != nil {
		return nil, runtimemigrate.Result{}, err
	}
	return db, result, nil
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
