package migrate

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/evalops/service-runtime/testutil"
)

func TestRunAppliesFileMigrationsWithBackedPostgres(t *testing.T) {
	db := testutil.NewTestDB(t, "")

	dir := t.TempDir()
	writeMigrationFile(t, dir, "001_widgets.up.sql", `
CREATE TABLE widgets (
	id TEXT PRIMARY KEY
);
`)
	writeMigrationFile(t, dir, "001_widgets.down.sql", `DROP TABLE widgets;`)

	result, err := Run(context.Background(), db, dir, Options{})
	if err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	if !result.Applied {
		t.Fatalf("expected migrations to apply, got %#v", result)
	}
	assertTableExists(t, db, "widgets")
}

func TestRunIOFSAppliesEmbeddedMigrationsWithBackedPostgres(t *testing.T) {
	db := testutil.NewTestDB(t, "")

	result, err := RunIOFS(context.Background(), db, fstest.MapFS{
		"migrations/001_widgets.up.sql":   &fstest.MapFile{Data: []byte("CREATE TABLE widgets_embedded (id TEXT PRIMARY KEY);")},
		"migrations/001_widgets.down.sql": &fstest.MapFile{Data: []byte("DROP TABLE widgets_embedded;")},
	}, "migrations", Options{})
	if err != nil {
		t.Fatalf("run embedded migrations: %v", err)
	}
	if !result.Applied {
		t.Fatalf("expected embedded migrations to apply, got %#v", result)
	}
	assertTableExists(t, db, "widgets_embedded")
}

func writeMigrationFile(t *testing.T, dir, name, contents string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o644); err != nil {
		t.Fatalf("write migration file %s: %v", name, err)
	}
}

func assertTableExists(t *testing.T, db *sql.DB, table string) {
	t.Helper()

	var exists bool
	err := db.QueryRowContext(
		context.Background(),
		`SELECT EXISTS (
			SELECT 1
			FROM information_schema.tables
			WHERE table_schema = CURRENT_SCHEMA()
			  AND table_name = $1
		)`,
		table,
	).Scan(&exists)
	if err != nil {
		t.Fatalf("check table %s: %v", table, err)
	}
	if !exists {
		t.Fatalf("expected table %s to exist", table)
	}
}
