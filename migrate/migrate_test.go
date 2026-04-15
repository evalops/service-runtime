package migrate

import (
	"context"
	"database/sql"
	"flag"
	"net/url"
	"testing"
	"testing/fstest"

	gomigrate "github.com/golang-migrate/migrate/v4"
)

type stubRunner struct {
	upErr      error
	stepsErr   error
	version    uint
	dirty      bool
	versionErr error
	steps      []int
}

func (s *stubRunner) Up() error {
	return s.upErr
}

func (s *stubRunner) Steps(steps int) error {
	s.steps = append(s.steps, steps)
	return s.stepsErr
}

func (s *stubRunner) Version() (uint, bool, error) {
	return s.version, s.dirty, s.versionErr
}

func (s *stubRunner) Close() (error, error) {
	return nil, nil
}

func TestParseCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		want    Command
		wantErr bool
	}{
		{name: "default empty", raw: "", want: CommandUp},
		{name: "up", raw: "up", want: CommandUp},
		{name: "down", raw: "down", want: CommandDown},
		{name: "version", raw: "version", want: CommandVersion},
		{name: "invalid", raw: "drop", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseCommand(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parse command: %v", err)
			}
			if got != tt.want {
				t.Fatalf("expected %q, got %q", tt.want, got)
			}
		})
	}
}

func TestOptionsApplyEnv(t *testing.T) {
	t.Setenv("SERVICE_RUNTIME_MIGRATE_COMMAND", "down")
	t.Setenv("SERVICE_RUNTIME_MIGRATE_DOWN_STEPS", "3")
	t.Setenv("SERVICE_RUNTIME_MIGRATE_TABLE", "runtime_schema_migrations")

	var opts Options
	if err := opts.ApplyEnv("service-runtime"); err != nil {
		t.Fatalf("apply env: %v", err)
	}

	if opts.Command != CommandDown {
		t.Fatalf("expected down command, got %q", opts.Command)
	}
	if opts.DownSteps != 3 {
		t.Fatalf("expected down steps 3, got %d", opts.DownSteps)
	}
	if opts.MigrationsTable != "runtime_schema_migrations" {
		t.Fatalf("expected migrations table override, got %q", opts.MigrationsTable)
	}
}

func TestOptionsBindFlags(t *testing.T) {
	t.Parallel()

	var opts Options
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	opts.BindFlags(flags)

	if err := flags.Parse([]string{
		"-migrate-command=version",
		"-migrate-down-steps=2",
		"-migrate-table=custom_schema_migrations",
	}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}

	if opts.Command != CommandVersion {
		t.Fatalf("expected version command, got %q", opts.Command)
	}
	if opts.DownSteps != 2 {
		t.Fatalf("expected down steps 2, got %d", opts.DownSteps)
	}
	if opts.MigrationsTable != "custom_schema_migrations" {
		t.Fatalf("expected custom table, got %q", opts.MigrationsTable)
	}
}

func TestRunUsesFileSourceAndReportsNoChange(t *testing.T) {
	previousBuildRunner := buildRunner
	t.Cleanup(func() {
		buildRunner = previousBuildRunner
	})

	stub := &stubRunner{
		upErr:   gomigrate.ErrNoChange,
		version: 7,
	}

	buildRunner = func(_ *sql.DB, source sourceConfig, opts Options) (runner, error) {
		if opts.Command != CommandUp {
			t.Fatalf("expected up command, got %q", opts.Command)
		}
		parsed, err := url.Parse(source.url)
		if err != nil {
			t.Fatalf("parse source url: %v", err)
		}
		if parsed.Scheme != "file" {
			t.Fatalf("expected file source url, got %q", source.url)
		}
		return stub, nil
	}

	result, err := Run(context.Background(), &sql.DB{}, "migrations", Options{})
	if err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	if result.Applied {
		t.Fatal("expected no-change run to report Applied=false")
	}
	if result.Version == nil || *result.Version != 7 {
		t.Fatalf("expected version 7, got %#v", result.Version)
	}
}

func TestRunIOFSUsesStepsForDown(t *testing.T) {
	previousBuildRunner := buildRunner
	t.Cleanup(func() {
		buildRunner = previousBuildRunner
	})

	stub := &stubRunner{versionErr: gomigrate.ErrNilVersion}
	buildRunner = func(_ *sql.DB, source sourceConfig, opts Options) (runner, error) {
		if source.fsys == nil {
			t.Fatal("expected iofs source")
		}
		if source.path != "migrations" {
			t.Fatalf("expected migrations path, got %q", source.path)
		}
		if opts.Command != CommandDown {
			t.Fatalf("expected down command, got %q", opts.Command)
		}
		return stub, nil
	}

	result, err := RunIOFS(context.Background(), &sql.DB{}, fstest.MapFS{
		"migrations/001_init.up.sql":   &fstest.MapFile{Data: []byte("select 1;")},
		"migrations/001_init.down.sql": &fstest.MapFile{Data: []byte("select 1;")},
	}, "migrations", Options{
		Command:   CommandDown,
		DownSteps: 2,
	})
	if err != nil {
		t.Fatalf("run iofs migrations: %v", err)
	}
	if len(stub.steps) != 1 || stub.steps[0] != -2 {
		t.Fatalf("expected one step call with -2, got %#v", stub.steps)
	}
	if result.Applied != true {
		t.Fatal("expected down run to report Applied=true")
	}
}

func TestRunReturnsDirtyError(t *testing.T) {
	previousBuildRunner := buildRunner
	t.Cleanup(func() {
		buildRunner = previousBuildRunner
	})

	buildRunner = func(_ *sql.DB, _ sourceConfig, _ Options) (runner, error) {
		return &stubRunner{
			upErr: gomigrate.ErrDirty{Version: 9},
		}, nil
	}

	result, err := Run(context.Background(), &sql.DB{}, "migrations", Options{})
	if err == nil {
		t.Fatal("expected dirty migration error")
	}
	if !result.Dirty {
		t.Fatal("expected result to report dirty database")
	}
}
