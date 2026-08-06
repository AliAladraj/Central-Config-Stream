package database

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// The repositories check the natural key before inserting, but that check is a
// fast path, not a lock: two concurrent creates both pass it and one of them
// then hits the constraint. Recognising that error is what turns the loser's
// 500 back into the 409 the pre-check would have produced.
func TestIsUniqueViolation(t *testing.T) {
	db, err := NewSQLiteDB("file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "unique.db")))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// search_v2 is seeded, so this collides with FLAG_KEY's unique index.
	_, err = db.Exec(`INSERT INTO CONFIG_FLAG (FLAG_KEY, IS_ACTIVE) VALUES ('search_v2', 1)`)
	if err == nil {
		t.Fatal("expected the unique constraint to reject the duplicate")
	}
	if !IsUniqueViolation(err) {
		t.Errorf("sqlite unique violation not recognised: %v", err)
	}
	// Wrapped the way a repository returns it.
	if !IsUniqueViolation(fmt.Errorf("create flag: %w", err)) {
		t.Error("a wrapped unique violation was not recognised")
	}

	// SQLSTATE 23505 is the Postgres side of the same thing. The production
	// driver cannot be exercised without a server, so the error it would return
	// is constructed directly — which is exactly why the check is on the code
	// and not on the message: Message is localised by the server's lc_messages
	// and names the constraint, so neither is a stable thing to match.
	pgDup := &pgconn.PgError{
		Code:                "23505",
		Message:             `duplicate key value violates unique constraint "uq_flag_key"`,
		ConstraintName:      "uq_flag_key",
		SeverityUnlocalized: "ERROR",
	}
	if !IsUniqueViolation(fmt.Errorf("create flag: %w", pgDup)) {
		t.Error("SQLSTATE 23505 not recognised")
	}

	// Anything else has to stay a 500 rather than being reported as a conflict.
	if _, err := db.Exec(`INSERT INTO CONFIG_FLAG (FLAG_KEY) VALUES (NULL)`); err == nil {
		t.Fatal("expected the NOT NULL constraint to reject the row")
	} else if IsUniqueViolation(err) {
		t.Errorf("a NOT NULL violation was reported as a unique violation: %v", err)
	}
	if IsUniqueViolation(errors.New("connection reset")) {
		t.Error("an unrelated error was reported as a unique violation")
	}
	// 23503 is a foreign-key violation and 23502 a not-null one: both are in the
	// same integrity-constraint class as 23505 and neither is a conflict, so a
	// prefix match on "23" would turn a genuine 500 into a misleading 409.
	if IsUniqueViolation(&pgconn.PgError{Code: "23503", Message: "insert or update violates foreign key constraint"}) {
		t.Error("SQLSTATE 23503 was reported as a unique violation")
	}
	if IsUniqueViolation(&pgconn.PgError{Code: "23502", Message: "null value in column violates not-null constraint"}) {
		t.Error("SQLSTATE 23502 was reported as a unique violation")
	}
	if IsUniqueViolation(nil) {
		t.Error("nil was reported as a unique violation")
	}
}

// An unbounded pool lets a slow database plus two replicas plus the reconciler
// open Postgres connections until the server hits max_connections, and without
// a lifetime a connection survives a failover as a half-dead socket.
func TestPoolOptionsAreAppliedAndConfigurable(t *testing.T) {
	db, err := NewSQLiteDB("file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "pool.db")))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	PoolOptions{}.apply(db)
	if got := db.Stats().MaxOpenConnections; got != defaultMaxOpenConns {
		t.Errorf("MaxOpenConnections = %d, want the default %d", got, defaultMaxOpenConns)
	}

	PoolOptions{MaxOpenConns: 7}.apply(db)
	if got := db.Stats().MaxOpenConnections; got != 7 {
		t.Errorf("MaxOpenConnections = %d, want 7", got)
	}

	// Every limit is overridable from the environment.
	t.Setenv("DB_MAX_OPEN_CONNS", "42")
	t.Setenv("DB_MAX_IDLE_CONNS", "9")
	t.Setenv("DB_CONN_MAX_LIFETIME", "13m")
	t.Setenv("DB_CONN_MAX_IDLE_TIME", "45s")

	opts := PoolOptionsFromEnv()
	want := PoolOptions{
		MaxOpenConns:    42,
		MaxIdleConns:    9,
		ConnMaxLifetime: 13 * time.Minute,
		ConnMaxIdleTime: 45 * time.Second,
	}
	if opts != want {
		t.Errorf("PoolOptionsFromEnv() = %+v, want %+v", opts, want)
	}

	// A value that is not usable falls back rather than disabling the bound.
	t.Setenv("DB_MAX_OPEN_CONNS", "not-a-number")
	t.Setenv("DB_CONN_MAX_LIFETIME", "0")
	opts = PoolOptionsFromEnv()
	if opts.MaxOpenConns != defaultMaxOpenConns {
		t.Errorf("MaxOpenConns = %d, want the default after an unparseable value", opts.MaxOpenConns)
	}
	if opts.ConnMaxLifetime != defaultConnMaxLifetime {
		t.Errorf("ConnMaxLifetime = %v, want the default after a zero value", opts.ConnMaxLifetime)
	}
}
