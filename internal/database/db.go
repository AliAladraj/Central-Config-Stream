// Package database opens and bounds the connection pool, and hides the two
// drivers behind one surface: Oracle (github.com/sijms/go-ora, the production
// source of truth) and SQLite (modernc.org/sqlite, the local test stack).
//
// It also carries the differences that would otherwise leak into every
// repository — CLOB binding, which go-ora needs for any document larger than a
// VARCHAR2, and unique-violation detection, so a racing insert becomes a 409
// rather than a 500. Both drivers are pure Go, so CGO_ENABLED=0 builds work.
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

	go_ora "github.com/sijms/go-ora/v2"
	"github.com/sijms/go-ora/v2/network"
	sqlitedriver "modernc.org/sqlite"
)

// Pool defaults. database/sql leaves the pool unbounded, which on Oracle means
// a slow query plus two replicas plus the reconciler can open sessions until
// the shared instance refuses new ones — this service taking down every other
// consumer of that database. A lifetime is just as important: without one a
// session survives a failover as a half-dead connection that only fails when it
// is next used.
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

func NewOracleDB(dsn string) (*sql.DB, error) {
	return NewOracleDBWithPool(dsn, PoolOptionsFromEnv())
}

func NewOracleDBWithPool(dsn string, pool PoolOptions) (*sql.DB, error) {
	if dsn == "" {
		dsn = os.Getenv("CONN_STRING")
	}
	if dsn == "" {
		return nil, fmt.Errorf("DB connection string is required")
	}

	db, err := sql.Open("oracle", dsn)
	if err != nil {
		return nil, fmt.Errorf("open oracle db: %w", err)
	}

	// Applied before the ping so even the first connection is pool-managed.
	pool.apply(db)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping oracle db: %w", err)
	}

	return db, nil
}

// CLOB returns a JSON document in the form the Oracle driver has to bind it in.
// SETTINGS_JSON and BUNDLE_JSON are CLOBs; go-ora sends a plain Go string as
// VARCHAR2, which an ordinary appsettings tree overflows (ORA-01461/ORA-01704)
// on any database without MAX_STRING_SIZE=EXTENDED.
func CLOB(s string) go_ora.Clob {
	return go_ora.Clob{String: s, Valid: true}
}

// Oracle and SQLite unique-constraint result codes.
const (
	oraUniqueViolation         = 1    // ORA-00001
	sqliteConstraintUnique     = 2067 // SQLITE_CONSTRAINT_UNIQUE
	sqliteConstraintPrimaryKey = 1555 // SQLITE_CONSTRAINT_PRIMARYKEY
)

// IsUniqueViolation reports whether err is the driver's unique-constraint
// violation, for either backend. The repositories check the natural key before
// inserting, but two concurrent creates both pass that check and one of them
// then hits the constraint; without this the loser gets a 500 where the
// pre-check would have produced a 409.
func IsUniqueViolation(err error) bool {
	var ora *network.OracleError
	if errors.As(err, &ora) {
		return ora.ErrCode == oraUniqueViolation
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
