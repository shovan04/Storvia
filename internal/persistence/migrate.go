package persistence

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	storviaMigrations "github.com/shovan04/storvia-project/migrations"
)

// migrationFS carries the SQL for both dialects so a binary can migrate without
// shipping loose files beside it. The files are embedded from the repository
// root migrations/ directory.
var migrationFS = storviaMigrations.FS()

// Migration is one versioned schema file.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// LoadMigrations reads the migration set for a dialect.
//
// Each dialect has its own directory because the schemas genuinely differ: the
// job-lease query and the identifier types are not portable, and pretending they
// are would let a migration pass on SQLite and fail on Postgres.
func LoadMigrations(dialect Dialect) ([]Migration, error) {
	dir := string(dialect)
	entries, err := fs.ReadDir(migrationFS, "sql/"+dir)
	if err != nil {
		return nil, fmt.Errorf("persistence: read migrations for %s: %w", dialect, err)
	}

	out := make([]Migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		raw, err := migrationFS.ReadFile("sql/" + dir + "/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("persistence: read migration %s: %w", entry.Name(), err)
		}
		version, name, err := parseMigrationName(entry.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, Migration{Version: version, Name: name, SQL: string(raw)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// parseMigrationName splits NNNN_name.sql into its version and name.
func parseMigrationName(file string) (version int, name string, err error) {
	base := strings.TrimSuffix(file, ".sql")
	prefix, rest, found := strings.Cut(base, "_")
	if !found {
		return 0, "", fmt.Errorf("persistence: migration %q is not named NNNN_name.sql", file)
	}
	if _, err := fmt.Sscanf(prefix, "%d", &version); err != nil {
		return 0, "", fmt.Errorf("persistence: migration %q has a non-numeric version", file)
	}
	return version, rest, nil
}

// Migrate applies every pending migration inside a transaction.
//
// It records each applied version, so a partially migrated database is detected
// rather than silently re-run.
func (s *Store) Migrate(ctx context.Context) (applied int, err error) {
	if err := s.ensureMigrationTable(ctx); err != nil {
		return 0, err
	}
	migrations, err := LoadMigrations(s.dialect)
	if err != nil {
		return 0, err
	}

	for _, migration := range migrations {
		already, err := s.migrationApplied(ctx, migration.Version)
		if err != nil {
			return applied, err
		}
		if already {
			continue
		}
		if err := s.applyMigration(ctx, migration); err != nil {
			return applied, err
		}
		applied++
	}
	return applied, nil
}

func (s *Store) ensureMigrationTable(ctx context.Context) error {
	var stmt string
	if s.dialect == DialectPostgres {
		stmt = `CREATE TABLE IF NOT EXISTS schema_migrations (
			version     INTEGER PRIMARY KEY,
			name        TEXT NOT NULL,
			applied_at  TIMESTAMPTZ NOT NULL
		)`
	} else {
		stmt = `CREATE TABLE IF NOT EXISTS schema_migrations (
			version     INTEGER PRIMARY KEY,
			name        TEXT NOT NULL,
			applied_at  TIMESTAMP NOT NULL
		)`
	}
	if _, err := s.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("persistence: create schema_migrations: %w", err)
	}
	return nil
}

func (s *Store) migrationApplied(ctx context.Context, version int) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations WHERE version = ?", version).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("persistence: check migration %d: %w", version, err)
	}
	return count > 0, nil
}

// applyMigration runs one migration in a transaction.
//
// SQLite cannot run every statement in a transaction when a DDL statement
// implicitly commits, so the transaction is best-effort there; Postgres runs the
// whole file atomically. The applied-version record is written in the same
// transaction wherever the dialect allows it.
func (s *Store) applyMigration(ctx context.Context, migration Migration) error {
	statements := splitStatements(migration.SQL)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("persistence: begin migration %d: %w", migration.Version, err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("persistence: migration %04d_%s failed: %w", migration.Version, migration.Name, err)
		}
	}

	insert := s.Rebind("INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)")
	if _, err := tx.ExecContext(ctx, insert, migration.Version, migration.Name, time.Now().UTC()); err != nil {
		return fmt.Errorf("persistence: record migration %d: %w", migration.Version, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("persistence: commit migration %d: %w", migration.Version, err)
	}
	return nil
}

// splitStatements breaks a migration file into executable statements, dropping
// comment-only lines and full-line comments.
//
// The splitting is intentionally simple: the migration files are authored by this
// project and contain no stored procedure bodies, which is the only case where a
// naive splitter breaks down.
func splitStatements(sqlText string) []string {
	withoutComments := make([]string, 0, 32)
	for _, line := range strings.Split(sqlText, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		withoutComments = append(withoutComments, line)
	}

	joined := strings.Join(withoutComments, "\n")
	parts := strings.Split(joined, ";")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		statement := strings.TrimSpace(part)
		if statement == "" {
			continue
		}
		out = append(out, statement)
	}
	return out
}

// CurrentVersion reports the highest applied migration version.
func (s *Store) CurrentVersion(ctx context.Context) (int, error) {
	if err := s.ensureMigrationTable(ctx); err != nil {
		return 0, err
	}
	var version sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("persistence: read schema version: %w", err)
	}
	if !version.Valid {
		return 0, nil
	}
	return int(version.Int64), nil
}

// Ping verifies connectivity for the readiness probe.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("persistence: ping: %w", err)
	}
	return nil
}
