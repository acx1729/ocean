package db

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// SchemaVersion is the newest migration this binary knows about, read from the
// embedded migration file names (NNNNN_name.sql).
func SchemaVersion() (int64, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return 0, err
	}
	var max int64
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		idx := strings.IndexByte(name, '_')
		if idx <= 0 {
			continue
		}
		v, err := strconv.ParseInt(name[:idx], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("migration %s: bad version prefix", name)
		}
		if v > max {
			max = v
		}
	}
	return max, nil
}

// Migrate applies every pending embedded migration under a Postgres session
// lock, so any number of replicas may start together and exactly one migrates.
// It refuses to run against a schema newer than this binary knows.
func (d *DB) Migrate(ctx context.Context) (int64, error) {
	sqlDB := stdlib.OpenDBFromPool(d.primary)
	defer sqlDB.Close()
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return 0, err
	}
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return 0, err
	}
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, sub, goose.WithSessionLocker(locker))
	if err != nil {
		return 0, fmt.Errorf("goose provider: %w", err)
	}
	srcs := p.ListSources()
	var max int64
	if len(srcs) > 0 {
		max = srcs[len(srcs)-1].Version
	}
	current, err := p.GetDBVersion(ctx)
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	if current > max {
		return current, fmt.Errorf("database schema version %d is newer than this binary (%d); refusing to start", current, max)
	}
	results, err := p.Up(ctx)
	for _, r := range results {
		if r.Error == nil {
			d.log.Info("migration applied", "version", r.Source.Version, "path", r.Source.Path, "duration", r.Duration)
		}
	}
	if err != nil {
		return current, fmt.Errorf("migrate: %w", err)
	}
	return p.GetDBVersion(ctx)
}
