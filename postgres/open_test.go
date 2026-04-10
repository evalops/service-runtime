package postgres

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/evalops/service-runtime/startup"
)

func TestOpenAndInitRetriesUntilSuccess(t *testing.T) {
	t.Parallel()

	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	previousOpen := sqlOpen
	sqlOpen = func(string, string) (*sql.DB, error) {
		return db, nil
	}
	defer func() { sqlOpen = previousOpen }()

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
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sql expectations: %v", err)
	}
}

func TestOpenAndInitReturnsLastError(t *testing.T) {
	t.Parallel()

	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	previousOpen := sqlOpen
	sqlOpen = func(string, string) (*sql.DB, error) {
		return db, nil
	}
	defer func() { sqlOpen = previousOpen }()

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
