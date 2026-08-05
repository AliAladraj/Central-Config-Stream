package database

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/sijms/go-ora/v2/network"
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

	// ORA-00001 is the Oracle side of the same thing.
	if !IsUniqueViolation(fmt.Errorf("create flag: %w", network.NewOracleError(1))) {
		t.Error("ORA-00001 not recognised")
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
	if IsUniqueViolation(network.NewOracleError(1017)) {
		t.Error("ORA-01017 was reported as a unique violation")
	}
	if IsUniqueViolation(nil) {
		t.Error("nil was reported as a unique violation")
	}
}

// An unbounded pool lets a slow database plus two replicas plus the reconciler
// open Oracle sessions until the shared instance refuses new ones, and without
// a lifetime a session survives a failover as a half-dead connection.
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
