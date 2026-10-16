package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Migrations are NNNN_description.sql files, applied once each, in order,
// every one in its own transaction. Never edit an applied file: add a new one.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrationLock is the advisory lock key that serialises Migrate calls of
// several trips instances starting at once ("trpsmigr" in ASCII).
const migrationLock int64 = 0x747270736d696772

type migration struct {
	version int
	name    string
	sql     string
}

func migrations() ([]migration, error) {
	paths, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, p := range paths {
		base := strings.TrimSuffix(path.Base(p), ".sql")
		num, _, ok := strings.Cut(base, "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil || v <= 0 {
			return nil, fmt.Errorf("migration %s: name must be NNNN_description.sql", p)
		}
		b, err := migrationFiles.ReadFile(p)
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: v, name: base, sql: string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i := 1; i < len(out); i++ {
		if out[i].version == out[i-1].version {
			return nil, fmt.Errorf("migrations %s and %s share a version", out[i-1].name, out[i].name)
		}
	}
	return out, nil
}

// Migrate brings the schema up to date and returns the names of the
// migrations it applied.
func (s *Store) Migrate(ctx context.Context) ([]string, error) {
	all, err := migrations()
	if err != nil {
		return nil, err
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLock); err != nil {
		return nil, fmt.Errorf("migrate: lock: %w", err)
	}
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLock) }()

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    integer PRIMARY KEY,
		name       text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	rows, _ := conn.Query(ctx, "SELECT version FROM schema_migrations")
	versions, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	applied := map[int]bool{}
	for _, v := range versions {
		applied[v] = true
	}

	var done []string
	for _, m := range all {
		if applied[m.version] {
			continue
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return done, err
		}
		// No arguments: pgx sends it with the simple protocol, so a file may
		// hold several statements.
		if _, err := tx.Exec(ctx, m.sql); err != nil {
			_ = tx.Rollback(ctx)
			return done, fmt.Errorf("migrate: %s: %w", m.name, err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version, name) VALUES ($1, $2)", m.version, m.name); err != nil {
			_ = tx.Rollback(ctx)
			return done, fmt.Errorf("migrate: %s: %w", m.name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return done, fmt.Errorf("migrate: %s: %w", m.name, err)
		}
		done = append(done, m.name)
	}
	return done, nil
}
