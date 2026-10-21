// Package persistence owns the relational metadata store.
//
// The store holds tenant-scoped metadata and workflow state only. It never holds
// file bytes, and there is deliberately no replica table: one logical object has
// exactly one provider pointer, and a second location is not representable in
// this schema.
package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shovan04/storvia-project/internal/config"

	_ "github.com/jackc/pgx/v5/stdlib" // postgres driver
	_ "modernc.org/sqlite"             // pure-Go sqlite driver, no cgo
)

// Dialect is the SQL flavour a Store speaks.
type Dialect string

const (
	// DialectSQLite is for development and single-node evaluation.
	DialectSQLite Dialect = "sqlite"
	// DialectPostgres is the production target.
	DialectPostgres Dialect = "postgres"
)

// Store is the database handle plus its dialect.
//
// The dialect is carried explicitly because the schema genuinely differs: SQLite
// has no SKIP LOCKED and no native UUID type, so job claiming and identifier
// generation use different implementations per backend. Hiding that behind a
// pretend-portable layer is how a migration passes on SQLite and fails on
// Postgres.
type Store struct {
	db      *sql.DB
	dialect Dialect
}

// DB exposes the handle for the repositories in this package.
func (s *Store) DB() *sql.DB { return s.db }

// Dialect returns the active SQL flavour.
func (s *Store) Dialect() Dialect { return s.dialect }

// Close releases the pool.
func (s *Store) Close() error { return s.db.Close() }

// Open dials the configured database and applies pool settings.
//
// SQLite is forced to a single writer connection: the shared-cache, multi-writer
// configuration produces SQLITE_BUSY under concurrent uploads, and a busy error
// after a provider write is exactly the kind of uncertainty this design refuses
// to create.
//
// password carries the resolved Postgres password. It is passed in rather than read
// here so that no layer below the application entry point touches the environment.
func Open(ctx context.Context, cfg config.DatabaseConfig, password string) (*Store, error) {
	var driverName, dsn string
	var dialect Dialect

	switch cfg.Driver {
	case config.DriverSQLite:
		dialect = DialectSQLite
		driverName = "sqlite"
		dsn = normalizeSQLiteDSN(cfg.BuildDSN(""))
	case config.DriverPostgres:
		dialect = DialectPostgres
		driverName = "pgx"
		dsn = cfg.BuildDSN(password)
	default:
		return nil, fmt.Errorf("persistence: unsupported driver %q", cfg.Driver)
	}

	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("persistence: open %s: %w", cfg.Driver, err)
	}

	if dialect == DialectSQLite {
		// A single connection also makes the in-memory DSN useful for tests.
		db.SetMaxOpenConns(1)
	} else {
		db.SetMaxOpenConns(cfg.MaxOpenConns)
		db.SetMaxIdleConns(cfg.MaxIdleConns)
	}
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime.Duration())
	db.SetConnMaxIdleTime(cfg.ConnMaxIdleTime.Duration())

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("persistence: ping %s: %w", cfg.Driver, err)
	}

	return &Store{db: db, dialect: dialect}, nil
}

// normalizeSQLiteDSN appends the pragmas Storvia depends on to a file path.
//
// WAL keeps readers from blocking the single writer during a long upload, and
// foreign_keys is off by default in SQLite even though every Storvia table
// depends on it.
func normalizeSQLiteDSN(dsn string) string {
	pragmas := "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	if dsn == "" {
		return "file::memory:?" + pragmas
	}
	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	return dsn + separator + pragmas
}

// Placeholder renders the nth bind placeholder for the dialect. Callers build
// their WHERE clauses with it so one repository body serves both backends.
func (s *Store) Placeholder(n int) string {
	if s.dialect == DialectPostgres {
		return fmt.Sprintf("$%d", n)
	}
	return "?"
}

// Rebind converts a query written with ? placeholders to the dialect's form.
func (s *Store) Rebind(query string) string {
	if s.dialect == DialectPostgres {
		return rebindToPostgres(query)
	}
	return query
}

func rebindToPostgres(query string) string {
	var out strings.Builder
	out.Grow(len(query) + 8)
	n := 0
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			n++
			out.WriteByte('$')
			out.WriteString(fmt.Sprintf("%d", n))
			continue
		}
		out.WriteByte(query[i])
	}
	return out.String()
}

// BoolValue converts a dialect boolean into a Go bool.
func (s *Store) BoolValue(v bool) any {
	if s.dialect == DialectPostgres {
		return v
	}
	if v {
		return 1
	}
	return 0
}

// UpsertConflict builds the dialect-specific conflict target suffix.
//
// The idempotency constraint is what stops a retried browser request from
// creating a second provider write, so the clause must be exactly as strict as
// the migration's unique index.
func (s *Store) UpsertConflict(target string) string {
	if s.dialect == DialectPostgres {
		return fmt.Sprintf("ON CONFLICT (%s) DO UPDATE SET", target)
	}
	return fmt.Sprintf("ON CONFLICT (%s) DO UPDATE SET", target)
}

// ErrNotFound is returned by repositories when a tenant-scoped lookup misses.
//
// It is one sentinel across both dialects so a service can branch on it without
// inspecting driver-specific error values.
var ErrNotFound = errors.New("persistence: not found")

// ErrConflict is returned when a write loses an optimistic row-version race.
var ErrConflict = errors.New("persistence: concurrent modification")

// IsNotFound reports whether err is ErrNotFound.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// IsConflict reports whether err is ErrConflict.
func IsConflict(err error) bool { return errors.Is(err, ErrConflict) }
