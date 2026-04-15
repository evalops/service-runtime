package migrate

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	gomigrate "github.com/golang-migrate/migrate/v4"
	migratepostgres "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	migrateiofs "github.com/golang-migrate/migrate/v4/source/iofs"
)

const (
	// DefaultEnvPrefix is the default env prefix used for migration options.
	DefaultEnvPrefix = "EVALOPS"
	// DefaultDownSteps is the default number of migrations to roll back for down commands.
	DefaultDownSteps = 1
)

// Command selects which migration operation to run.
type Command string

const (
	// CommandUp applies every pending up migration.
	CommandUp Command = "up"
	// CommandDown rolls back one or more migrations.
	CommandDown Command = "down"
	// CommandVersion reports the current migration version without applying changes.
	CommandVersion Command = "version"
)

// Result reports the post-run migration state.
type Result struct {
	Command Command
	Applied bool
	Version *uint
	Dirty   bool
}

// Options configures shared migration execution.
type Options struct {
	Command         Command
	DownSteps       int
	MigrationsTable string
}

type sourceConfig struct {
	url  string
	fsys fs.FS
	path string
}

type runner interface {
	Up() error
	Steps(int) error
	Version() (uint, bool, error)
	Close() (error, error)
}

var buildRunner = func(db *sql.DB, source sourceConfig, opts Options) (runner, error) {
	driver, err := migratepostgres.WithInstance(db, &migratepostgres.Config{
		MigrationsTable: strings.TrimSpace(opts.MigrationsTable),
	})
	if err != nil {
		return nil, fmt.Errorf("open postgres migration driver: %w", err)
	}

	if source.fsys != nil {
		sourceDriver, err := migrateiofs.New(source.fsys, source.path)
		if err != nil {
			_ = driver.Close()
			return nil, fmt.Errorf("open iofs migration source: %w", err)
		}

		migrator, err := gomigrate.NewWithInstance("iofs", sourceDriver, "postgres", driver)
		if err != nil {
			_ = sourceDriver.Close()
			_ = driver.Close()
			return nil, fmt.Errorf("build migrator: %w", err)
		}
		return migrator, nil
	}

	migrator, err := gomigrate.NewWithDatabaseInstance(source.url, "postgres", driver)
	if err != nil {
		_ = driver.Close()
		return nil, fmt.Errorf("build migrator: %w", err)
	}
	return migrator, nil
}

// ParseCommand validates and normalizes a migration command value.
func ParseCommand(raw string) (Command, error) {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case "", string(CommandUp):
		return CommandUp, nil
	case string(CommandDown):
		return CommandDown, nil
	case string(CommandVersion):
		return CommandVersion, nil
	default:
		return "", fmt.Errorf("unsupported migrate command %q", raw)
	}
}

// String implements flag.Value.
func (c Command) String() string {
	if c == "" {
		return string(CommandUp)
	}
	return string(c)
}

// Set implements flag.Value.
func (c *Command) Set(raw string) error {
	command, err := ParseCommand(raw)
	if err != nil {
		return err
	}
	*c = command
	return nil
}

// BindFlags registers shared migration flags onto fs.
func (o *Options) BindFlags(fs *flag.FlagSet) {
	if fs == nil {
		return
	}

	current := o.withDefaults()
	o.Command = current.Command
	o.DownSteps = current.DownSteps

	fs.Var(&o.Command, "migrate-command", `database migration command: "up", "down", or "version"`)
	fs.IntVar(&o.DownSteps, "migrate-down-steps", current.DownSteps, "number of migrations to roll back when migrate-command=down")
	fs.StringVar(&o.MigrationsTable, "migrate-table", strings.TrimSpace(o.MigrationsTable), "schema migrations table name")
}

// ApplyEnv loads migration options from env vars like EVALOPS_MIGRATE_COMMAND.
func (o *Options) ApplyEnv(prefix string) error {
	current := o.withDefaults()

	envPrefix := normalizeEnvPrefix(prefix)
	if raw := strings.TrimSpace(os.Getenv(envPrefix + "_MIGRATE_COMMAND")); raw != "" {
		if err := current.Command.Set(raw); err != nil {
			return err
		}
	}
	if raw := strings.TrimSpace(os.Getenv(envPrefix + "_MIGRATE_DOWN_STEPS")); raw != "" {
		steps, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("%s_MIGRATE_DOWN_STEPS: %w", envPrefix, err)
		}
		current.DownSteps = steps
	}
	if raw := strings.TrimSpace(os.Getenv(envPrefix + "_MIGRATE_TABLE")); raw != "" {
		current.MigrationsTable = raw
	}

	if err := current.validate(); err != nil {
		return err
	}
	*o = current
	return nil
}

// Run applies migrations from a filesystem directory path or file:// URL.
func Run(ctx context.Context, db *sql.DB, migrationsDir string, opts Options) (Result, error) {
	sourceURL, err := normalizeSourceURL(migrationsDir)
	if err != nil {
		return Result{}, err
	}
	return run(ctx, db, sourceConfig{url: sourceURL}, opts)
}

// RunIOFS applies migrations from an embedded io/fs source.
func RunIOFS(ctx context.Context, db *sql.DB, fsys fs.FS, path string, opts Options) (Result, error) {
	if fsys == nil {
		return Result{}, errors.New("migration fs is required")
	}
	if strings.TrimSpace(path) == "" {
		return Result{}, errors.New("migration fs path is required")
	}
	return run(ctx, db, sourceConfig{fsys: fsys, path: path}, opts)
}

func run(ctx context.Context, db *sql.DB, source sourceConfig, opts Options) (result Result, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if db == nil {
		return Result{}, errors.New("migration db is required")
	}

	opts = opts.withDefaults()
	if err := opts.validate(); err != nil {
		return Result{}, err
	}

	migrator, err := buildRunner(db, source, opts)
	if err != nil {
		return Result{}, err
	}
	defer func() {
		sourceErr, databaseErr := migrator.Close()
		closeErr := errors.Join(sourceErr, databaseErr)
		if closeErr == nil {
			return
		}
		if err != nil {
			err = errors.Join(err, fmt.Errorf("close migrator: %w", closeErr))
			return
		}
		err = fmt.Errorf("close migrator: %w", closeErr)
	}()

	result.Command = opts.Command
	applied, runErr := execute(migrator, opts)
	if runErr != nil {
		var dirtyErr gomigrate.ErrDirty
		if errors.As(runErr, &dirtyErr) {
			result.Dirty = true
		}
		return result, fmt.Errorf("run %s migrations: %w", opts.Command, runErr)
	}
	result.Applied = applied

	version, dirty, err := migrator.Version()
	switch {
	case err == nil:
		result.Version = &version
		result.Dirty = dirty
	case errors.Is(err, gomigrate.ErrNilVersion):
		result.Dirty = dirty
	default:
		return result, fmt.Errorf("read migration version: %w", err)
	}

	return result, nil
}

func execute(migrator runner, opts Options) (bool, error) {
	switch opts.Command {
	case CommandUp:
		if err := migrator.Up(); err != nil {
			if errors.Is(err, gomigrate.ErrNoChange) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	case CommandDown:
		if err := migrator.Steps(-opts.DownSteps); err != nil {
			if errors.Is(err, gomigrate.ErrNoChange) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	case CommandVersion:
		return false, nil
	default:
		return false, fmt.Errorf("unsupported migrate command %q", opts.Command)
	}
}

func (o Options) withDefaults() Options {
	if o.Command == "" {
		o.Command = CommandUp
	}
	if o.DownSteps == 0 {
		o.DownSteps = DefaultDownSteps
	}
	o.MigrationsTable = strings.TrimSpace(o.MigrationsTable)
	return o
}

func (o Options) validate() error {
	if _, err := ParseCommand(o.Command.String()); err != nil {
		return err
	}
	if o.DownSteps < 1 {
		return fmt.Errorf("migrate down steps must be >= 1")
	}
	return nil
}

func normalizeSourceURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("migration directory is required")
	}
	if strings.Contains(raw, "://") {
		return raw, nil
	}

	absolute, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("resolve migration directory: %w", err)
	}
	return (&url.URL{
		Scheme: "file",
		Path:   filepath.ToSlash(absolute),
	}).String(), nil
}

func normalizeEnvPrefix(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		prefix = DefaultEnvPrefix
	}
	prefix = strings.ToUpper(prefix)

	var builder strings.Builder
	for _, r := range prefix {
		switch {
		case r >= 'A' && r <= 'Z':
			builder.WriteRune(r)
		case r >= '0' && r <= '9':
			builder.WriteRune(r)
		default:
			builder.WriteByte('_')
		}
	}
	return strings.Trim(builder.String(), "_")
}
