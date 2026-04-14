package testutil

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

const testDatabaseURLEnv = "TEST_DATABASE_URL"

// NewTestDB opens a schema-scoped database/sql handle for integration tests and cleans it up automatically.
//
// When schemaSQL is not empty, it is executed against the newly created schema before the handle is returned.
func NewTestDB(t *testing.T, schemaSQL string) *sql.DB {
	t.Helper()

	databaseURL := testDatabaseURL(t)
	admin := openAdminDB(t, databaseURL)
	schemaName := createTestSchema(t, admin)

	config := scopedConnConfig(t, databaseURL, schemaName)
	db := stdlib.OpenDB(*config)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close scoped postgres db: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping scoped postgres db: %v", err)
	}
	applySchemaSQL(t, ctx, db.ExecContext, schemaSQL)
	return db
}

// NewTestPGXPool opens a schema-scoped pgxpool.Pool for integration tests and cleans it up automatically.
//
// When schemaSQL is not empty, it is executed against the newly created schema before the pool is returned.
func NewTestPGXPool(t *testing.T, schemaSQL string) *pgxpool.Pool {
	t.Helper()

	databaseURL := testDatabaseURL(t)
	admin := openAdminDB(t, databaseURL)
	schemaName := createTestSchema(t, admin)

	config := scopedPoolConfig(t, databaseURL, schemaName)
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open scoped pgx pool: %v", err)
	}
	t.Cleanup(pool.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping scoped pgx pool: %v", err)
	}
	applySchemaSQL(t, ctx, pool.Exec, schemaSQL)
	return pool
}

func testDatabaseURL(t *testing.T) string {
	t.Helper()

	databaseURL := strings.TrimSpace(os.Getenv(testDatabaseURLEnv))
	if databaseURL == "" {
		t.Skipf("%s is not set", testDatabaseURLEnv)
	}
	return databaseURL
}

func openAdminDB(t *testing.T, databaseURL string) *sql.DB {
	t.Helper()

	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatalf("open admin postgres db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close admin postgres db: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping admin postgres db: %v", err)
	}
	return db
}

func createTestSchema(t *testing.T, admin *sql.DB) string {
	t.Helper()

	schemaName := "test_" + randomSuffix(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := admin.ExecContext(ctx, "create schema "+quoteIdentifier(schemaName)); err != nil {
		t.Fatalf("create test schema: %v", err)
	}

	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if _, err := admin.ExecContext(cleanupCtx, "drop schema if exists "+quoteIdentifier(schemaName)+" cascade"); err != nil {
			t.Errorf("drop test schema %q: %v", schemaName, err)
		}
	})
	return schemaName
}

func scopedConnConfig(t *testing.T, databaseURL, schemaName string) *pgx.ConnConfig {
	t.Helper()

	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse postgres config: %v", err)
	}
	if config.RuntimeParams == nil {
		config.RuntimeParams = map[string]string{}
	}
	config.RuntimeParams["search_path"] = schemaName
	return config
}

func scopedPoolConfig(t *testing.T, databaseURL, schemaName string) *pgxpool.Config {
	t.Helper()

	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse pgx pool config: %v", err)
	}
	if config.ConnConfig.RuntimeParams == nil {
		config.ConnConfig.RuntimeParams = map[string]string{}
	}
	config.ConnConfig.RuntimeParams["search_path"] = schemaName
	return config
}

func applySchemaSQL[T any](
	t *testing.T,
	ctx context.Context,
	exec func(context.Context, string, ...any) (T, error),
	schemaSQL string,
) {
	t.Helper()

	if strings.TrimSpace(schemaSQL) == "" {
		return
	}
	if _, err := exec(ctx, schemaSQL); err != nil {
		t.Fatalf("apply test schema sql: %v", err)
	}
}

func quoteIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

func randomSuffix(t *testing.T) string {
	t.Helper()

	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("read random suffix: %v", err)
	}
	return hex.EncodeToString(raw[:])
}
