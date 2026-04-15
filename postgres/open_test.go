package postgres

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/DATA-DOG/go-sqlmock"
	runtimemigrate "github.com/evalops/service-runtime/migrate"
	"github.com/evalops/service-runtime/startup"
)

var sqlOpenMu sync.Mutex

func TestOpenAndInitRetriesUntilSuccess(t *testing.T) {
	t.Parallel()

	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	closePostgresTestDB(t, db, mock)

	restoreSQLOpen(t, func(string, string) (*sql.DB, error) {
		return db, nil
	})

	mock.ExpectPing().WillReturnError(errors.New("not ready"))
	mock.ExpectPing()
	mock.ExpectExec(regexp.QuoteMeta("SELECT 1")).WillReturnResult(sqlmock.NewResult(0, 0))

	opened, err := OpenAndInit(context.Background(), "postgres://memory", func(ctx context.Context, db *sql.DB) error {
		_, err := db.ExecContext(ctx, "SELECT 1")
		return err
	}, Options{
		Retry: startup.Config{MaxAttempts: 2, Delay: 0},
	})
	if err != nil {
		t.Fatalf("expected open to succeed, got %v", err)
	}
	if opened != db {
		t.Fatal("expected returned db handle")
	}
}

func TestOpenAndInitReturnsLastError(t *testing.T) {
	t.Parallel()

	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}

	restoreSQLOpen(t, func(string, string) (*sql.DB, error) {
		return db, nil
	})

	mock.ExpectPing().WillReturnError(errors.New("still unavailable"))
	mock.ExpectPing().WillReturnError(errors.New("still unavailable"))
	mock.ExpectClose()

	_, err = OpenAndInit(context.Background(), "postgres://memory", nil, Options{
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
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}

func TestOpenAndMigrateRunsMigrationsBeforeInit(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	closePostgresTestDB(t, db, mock)

	restoreSQLOpen(t, func(string, string) (*sql.DB, error) {
		return db, nil
	})

	restoreMigrationRunner(t, func(ctx context.Context, gotDB *sql.DB, migrationsDir string, opts runtimemigrate.Options) (runtimemigrate.Result, error) {
		if gotDB != db {
			t.Fatal("expected migration helper to receive opened db")
		}
		if migrationsDir != "db/migrations" {
			t.Fatalf("expected migrations dir, got %q", migrationsDir)
		}
		if opts.Command != runtimemigrate.CommandVersion {
			t.Fatalf("expected version command, got %q", opts.Command)
		}
		return runtimemigrate.Result{Command: runtimemigrate.CommandVersion}, nil
	}, nil)

	mock.ExpectPing()

	initCalled := false
	opened, result, err := OpenAndMigrate(
		context.Background(),
		"postgres://memory",
		"db/migrations",
		runtimemigrate.Options{Command: runtimemigrate.CommandVersion},
		func(context.Context, *sql.DB) error {
			initCalled = true
			return nil
		},
		Options{},
	)
	if err != nil {
		t.Fatalf("open and migrate: %v", err)
	}
	if opened != db {
		t.Fatal("expected returned db handle")
	}
	if result.Command != runtimemigrate.CommandVersion {
		t.Fatalf("expected version result, got %#v", result)
	}
	if !initCalled {
		t.Fatal("expected init callback to run")
	}
}

func TestOpenAndMigrateReturnsMigrationError(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}

	restoreSQLOpen(t, func(string, string) (*sql.DB, error) {
		return db, nil
	})

	restoreMigrationRunner(t, func(context.Context, *sql.DB, string, runtimemigrate.Options) (runtimemigrate.Result, error) {
		return runtimemigrate.Result{}, errors.New("migration failed")
	}, nil)

	mock.ExpectPing()
	mock.ExpectClose()

	_, _, err = OpenAndMigrate(
		context.Background(),
		"postgres://memory",
		"db/migrations",
		runtimemigrate.Options{},
		nil,
		Options{
			Retry: startup.Config{MaxAttempts: 1, Delay: 0},
		},
	)
	if err == nil {
		t.Fatal("expected migration error")
	}
	if !strings.Contains(err.Error(), "migration failed") {
		t.Fatalf("expected wrapped migration error, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}

func TestOpenAndMigrateIOFSUsesEmbeddedRunner(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	closePostgresTestDB(t, db, mock)

	restoreSQLOpen(t, func(string, string) (*sql.DB, error) {
		return db, nil
	})

	called := false
	restoreMigrationRunner(t, nil, func(ctx context.Context, gotDB *sql.DB, fsys fs.FS, path string, opts runtimemigrate.Options) (runtimemigrate.Result, error) {
		if gotDB != db {
			t.Fatal("expected migration helper to receive opened db")
		}
		if fsys == nil {
			t.Fatal("expected io/fs source")
		}
		if path != "migrations" {
			t.Fatalf("expected migrations path, got %q", path)
		}
		called = true
		return runtimemigrate.Result{Command: runtimemigrate.CommandUp, Applied: true}, nil
	})

	mock.ExpectPing()

	_, result, err := OpenAndMigrateIOFS(context.Background(), "postgres://memory", fstest.MapFS{
		"migrations/001_init.up.sql":   &fstest.MapFile{Data: []byte("select 1;")},
		"migrations/001_init.down.sql": &fstest.MapFile{Data: []byte("select 1;")},
	}, "migrations", runtimemigrate.Options{}, nil, Options{})
	if err != nil {
		t.Fatalf("open and migrate iofs: %v", err)
	}
	if !called {
		t.Fatal("expected embedded migration helper to be called")
	}
	if !result.Applied {
		t.Fatalf("expected applied result, got %#v", result)
	}
}

func restoreSQLOpen(t *testing.T, opener func(string, string) (*sql.DB, error)) {
	t.Helper()

	sqlOpenMu.Lock()
	previousOpen := sqlOpen
	sqlOpen = opener
	t.Cleanup(func() {
		sqlOpen = previousOpen
		sqlOpenMu.Unlock()
	})
}

func restoreMigrationRunner(
	t *testing.T,
	run func(context.Context, *sql.DB, string, runtimemigrate.Options) (runtimemigrate.Result, error),
	runIOFS func(context.Context, *sql.DB, fs.FS, string, runtimemigrate.Options) (runtimemigrate.Result, error),
) {
	t.Helper()

	previousRun := runMigrations
	previousRunIOFS := runIOFSMigrations
	if run != nil {
		runMigrations = run
	}
	if runIOFS != nil {
		runIOFSMigrations = runIOFS
	}

	t.Cleanup(func() {
		runMigrations = previousRun
		runIOFSMigrations = previousRunIOFS
	})
}

func closePostgresTestDB(t *testing.T, db *sql.DB, mock sqlmock.Sqlmock) {
	t.Helper()
	t.Cleanup(func() {
		mock.ExpectClose()
		if err := db.Close(); err != nil {
			t.Errorf("close db: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("sql expectations: %v", err)
		}
	})
}
