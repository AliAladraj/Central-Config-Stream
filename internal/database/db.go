// Package database opens and bounds the connection pool, and hides the two
// drivers behind one surface: PostgreSQL (github.com/jackc/pgx/v5, driven
// through database/sql by its stdlib shim — the production source of truth)
// and SQLite (modernc.org/sqlite, the local test stack).
//
// It carries the one difference that would otherwise leak into every
// repository: unique-violation detection, so a racing insert becomes a 409
// rather than a 500. Both drivers are pure Go, so CGO_ENABLED=0 builds work —
// which is what lets .goreleaser.yaml cross-compile four platforms from one
// runner and the Dockerfile produce a distroless image.
//
// There is deliberately no large-document binding helper here. Oracle needed
// one, because go-ora sent a Go string as VARCHAR2 and an ordinary appsettings
// tree overflowed it; Postgres takes a plain Go string straight into a text
// column, so the repositories bind documents as strings and nothing has to be
// wrapped.
//
// sqlite.go additionally defines the local schema and seed data by hand.
// Nothing keeps that in step with migrations/, and because every test runs
// against SQLite, a divergence shows up as a green build.
package database

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	// Registers the "pgx" driver with database/sql. Imported for its side
	// effect only: the repositories speak database/sql, not pgx's native API,
	// so nothing here calls into it directly.
	_ "github.com/jackc/pgx/v5/stdlib"
	sqlitedriver "modernc.org/sqlite"
)

// Pool defaults. database/sql leaves the pool unbounded, which against Postgres
// means a slow query plus two replicas plus the reconciler can open connections
// until the server hits max_connections — and because every Postgres connection
// is a backend process, the cost of approaching that limit lands on every other
// consumer of the instance before the refusals even start. A lifetime is just as
// important: without one a connection survives a failover or a pgbouncer restart
// as a half-dead socket that only fails when it is next used.
const (
	defaultMaxOpenConns    = 20
	defaultMaxIdleConns    = 5
	defaultConnMaxLifetime = 30 * time.Minute
	defaultConnMaxIdleTime = 5 * time.Minute
)

// PoolOptions bounds the connection pool. A zero field takes the default above.
type PoolOptions struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
}

// PoolOptionsFromEnv reads the pool limits from the environment. They are read
// here rather than carried on the application Config because the pool is this
// package's concern and nothing above it has an opinion on the numbers.
func PoolOptionsFromEnv() PoolOptions {
	return PoolOptions{
		MaxOpenConns:    envInt("DB_MAX_OPEN_CONNS", defaultMaxOpenConns),
		MaxIdleConns:    envInt("DB_MAX_IDLE_CONNS", defaultMaxIdleConns),
		ConnMaxLifetime: envDuration("DB_CONN_MAX_LIFETIME", defaultConnMaxLifetime),
		ConnMaxIdleTime: envDuration("DB_CONN_MAX_IDLE_TIME", defaultConnMaxIdleTime),
	}
}

func (o PoolOptions) withDefaults() PoolOptions {
	if o.MaxOpenConns == 0 {
		o.MaxOpenConns = defaultMaxOpenConns
	}
	if o.MaxIdleConns == 0 {
		o.MaxIdleConns = defaultMaxIdleConns
	}
	if o.ConnMaxLifetime == 0 {
		o.ConnMaxLifetime = defaultConnMaxLifetime
	}
	if o.ConnMaxIdleTime == 0 {
		o.ConnMaxIdleTime = defaultConnMaxIdleTime
	}
	return o
}

func (o PoolOptions) apply(db *sql.DB) {
	o = o.withDefaults()
	db.SetMaxOpenConns(o.MaxOpenConns)
	db.SetMaxIdleConns(o.MaxIdleConns)
	db.SetConnMaxLifetime(o.ConnMaxLifetime)
	db.SetConnMaxIdleTime(o.ConnMaxIdleTime)
}

func NewPostgresDB(dsn string) (*sql.DB, error) {
	return NewPostgresDBWithPool(dsn, PoolOptionsFromEnv())
}

func NewPostgresDBWithPool(dsn string, pool PoolOptions) (*sql.DB, error) {
	if dsn == "" {
		dsn = os.Getenv("CONN_STRING")
	}
	if dsn == "" {
		return nil, fmt.Errorf("DB connection string is required")
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres db: %w", err)
	}

	// Applied before the ping so even the first connection is pool-managed.
	pool.apply(db)

	// sql.Open is lazy — it validates nothing and dials nothing. Without this
	// ping an unreachable database or a rejected password is discovered by the
	// first request rather than at startup, which turns a config mistake into a
	// pod that passes its own liveness probe while answering 500s.
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping postgres db: %w", err)
	}

	return db, nil
}

// Unique-constraint result codes. Postgres reports an error class rather than a
// per-constraint code, so 23505 covers a primary key, a unique constraint and a
// unique index alike.
const (
	pgUniqueViolation          = "23505" // SQLSTATE unique_violation
	sqliteConstraintUnique     = 2067    // SQLITE_CONSTRAINT_UNIQUE
	sqliteConstraintPrimaryKey = 1555    // SQLITE_CONSTRAINT_PRIMARYKEY
)

// IsUniqueViolation reports whether err is the driver's unique-constraint
// violation, for either backend. The repositories check the natural key before
// inserting, but two concurrent creates both pass that check and one of them
// then hits the constraint; without this the loser gets a 500 where the
// pre-check would have produced a 409.
//
// The Postgres side matches on SQLSTATE and not on the message. Postgres
// renders "duplicate key value violates unique constraint …" in whatever
// lc_messages the server is configured with and names the constraint inside it,
// so a message match would break on a non-English server and again on the first
// constraint rename — silently, and in the direction that costs a 500.
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == pgUniqueViolation
	}
	var lite *sqlitedriver.Error
	if errors.As(err, &lite) {
		return lite.Code() == sqliteConstraintUnique || lite.Code() == sqliteConstraintPrimaryKey
	}
	return false
}

func envInt(key string, fallback int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return fallback
}
