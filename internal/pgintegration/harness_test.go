// Package pgintegration runs the production storage path against a real
// PostgreSQL server.
//
// Every other test in this repository runs against SQLite. That backend is a
// hand-written mirror of migrations/ (internal/database/sqlite.go), and it
// differs from the real thing in the three places that have historically
// carried the bugs: bind syntax, the pagination clause, and how a timestamp is
// stored and compared. Until this package existed, the storage path a
// deployment actually uses was the one path no test and no CI gate touched —
// docs/PRODUCTION_READINESS.md §3.1 and §3.6 said so for the project's whole
// life. It was an expensive gap to close under Oracle. It is cheap under
// Postgres, which is why it is closed here.
//
// # Why one package rather than a file per domain
//
// The obvious layout would be a repository_postgres_test.go beside each
// existing repository_sqlite_test.go. It is not possible without adding a
// non-test support package, because the harness below — schema creation,
// migration application, seeding, teardown — is ~200 lines that all four
// packages would otherwise carry a copy of, and Go cannot import a helper that
// lives in a _test.go file. Four hand-maintained copies of a harness is exactly
// the divergence risk §3.5 already describes for the schema. So the suite lives
// in one place and imports the domain packages instead.
//
// This directory holds only _test.go files: `go build ./...` skips it, `go vet
// ./...` and `golangci-lint` still analyse it, and `go test ./...` runs it.
//
// # How the tests find a database
//
// TEST_POSTGRES_DSN. Unset means skip, loudly and per-test, so `go test ./...`
// on a machine with no Postgres stays green and `go test -v` still lists every
// test that was not run. There is deliberately no build tag: a tag would hide
// this file from the default build, so it would stop compiling the day a
// repository signature changed and nobody would find out until the next time
// somebody remembered to pass the tag. A skip keeps the code on the compiler's
// and the linter's path every single run, and costs one env var to switch on.
//
// # How the tests isolate themselves
//
// One fresh Postgres schema per test, created by the test, populated by
// applying migrations/*.sql in order, seeded with the same fixed ids the local
// SQLite stack seeds, then dropped. Not a shared schema with unique ids
// (identity sequences, unique constraints and the reference-table delete guards
// are all global state that leaks between tests), and not transaction rollback
// (the repositories open their own transactions — DeleteFlag does — and a test
// that wraps them in an outer transaction stops testing the commit path).
//
// Applying the migrations per test is the other half of the point: the DDL that
// production runs is executed several dozen times on every CI run, so a
// migration that no longer applies is now a red build rather than a discovery
// made during a deploy window.
package pgintegration

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/ErasedKyte/Central-Config-Stream/internal/database"
)

// dsnEnv names the connection string. It is a separate variable from
// CONN_STRING on purpose: CONN_STRING is what the service reads, and pointing
// a test suite that creates and drops schemas at whatever a developer happens
// to have exported for `make run` is not a mistake worth making possible.
const dsnEnv = "TEST_POSTGRES_DSN"

// seededTables are the tables the seed below writes explicit ids into, in the
// order their foreign keys allow. resetIdentities walks it; see the comment
// there for why that is not optional.
var seededTables = []string{
	"CONFIG_ENVIRONMENTS",
	"CONFIG_MICROSERVICES",
	"CONFIG_FLAG",
	"CONFIG_FLAG_VALUE",
	"CONFIG_MICROSERVICE_APPSETTINGS",
	"CONFIG_LOCALIZATION",
}

// stack is one test's private database: a *sql.DB whose every connection
// resolves unqualified table names into a schema nothing else is using.
type stack struct {
	DB     *sql.DB
	Schema string
}

// stackOption tunes the session the pool opens. The options are startup
// parameters rather than a `SET` statement because a *sql.DB is a pool: a SET
// applies to whichever connection happened to run it, and the next query in the
// same test may well land on a different one. Only a connection-string
// parameter is guaranteed to hold for every connection the pool ever opens.
type stackOption func(map[string]string)

// withTimeZone sets the session TimeZone. Used by the reconcile-window tests:
// see reconcile_timezone_test.go for the bug class it exists to pin.
func withTimeZone(zone string) stackOption {
	return func(params map[string]string) { params["timezone"] = zone }
}

// dsnOrSkip returns the configured connection string, or skips the calling
// test. Table-driven tests call it directly as well as through newStack: a
// parent whose subtests all skip is reported as PASS, which is precisely the
// wrong thing for a suite whose whole point is that it either ran against a
// database or visibly did not.
func dsnOrSkip(t *testing.T) string {
	t.Helper()

	dsn := strings.TrimSpace(os.Getenv(dsnEnv))
	if dsn == "" {
		t.Skipf("%s is not set — skipping the PostgreSQL integration suite. "+
			"Run `make test-postgres` for a throwaway server, or export %s yourself.", dsnEnv, dsnEnv)
	}
	return dsn
}

// newStack skips the test when no database is configured, and otherwise hands
// back a freshly migrated and seeded schema of its own.
func newStack(t *testing.T, opts ...stackOption) *stack {
	t.Helper()

	dsn := dsnOrSkip(t)
	schema := uniqueSchemaName(t)
	params := map[string]string{"search_path": schema}
	for _, opt := range opts {
		opt(params)
	}

	// The pool is opened through the production helper rather than sql.Open, so
	// the driver registration, the pool bounds and the startup ping are the same
	// code a deployment runs. A DSN this suite cannot reach fails here, once,
	// with the same message an operator would see.
	db, err := database.NewPostgresDB(dsnWith(t, dsn, params))
	if err != nil {
		t.Fatalf("connect to %s: %v", dsnEnv, err)
	}

	// The schema named in search_path does not have to exist for a connection to
	// open — an unresolvable entry is simply skipped during name lookup — so it
	// can be created over the very pool that will then use it.
	if _, err := db.Exec(`CREATE SCHEMA ` + schema); err != nil {
		_ = db.Close()
		t.Fatalf("create schema %s: %v", schema, err)
	}

	t.Cleanup(func() {
		// A failed test's schema is left behind with its name in the log: it
		// holds the exact rows that produced the failure, and the alternative is
		// reproducing them by hand. Nothing accumulates in practice, because the
		// only databases this suite is ever pointed at are throwaway ones.
		if t.Failed() {
			t.Logf("leaving schema %s in place for inspection (%s)", schema, dsnEnv)
		} else if _, err := db.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); err != nil {
			t.Errorf("drop schema %s: %v", schema, err)
		}
		_ = db.Close()
	})

	s := &stack{DB: db, Schema: schema}
	s.migrate(t)
	s.seed(t)
	return s
}

// uniqueSchemaName keeps two tests, two concurrent runs and two developers
// sharing one database from colliding. Lower case because unquoted identifiers
// fold to lower case, which is the convention migrations/001 states and the
// repositories' SQL depends on.
func uniqueSchemaName(t *testing.T) string {
	t.Helper()
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("generate a schema name: %v", err)
	}
	return "cct_" + hex.EncodeToString(raw[:])
}

// dsnWith adds startup parameters to the operator's connection string in
// whichever of the two shapes libpq accepts, because a developer's exported DSN
// may legitimately be either and guessing wrong is a confusing failure. pgx
// forwards any key it does not recognise as a server runtime parameter, which
// is what makes search_path and TimeZone settable this way at all.
func dsnWith(t *testing.T, dsn string, params map[string]string) string {
	t.Helper()

	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("parse %s as a URL: %v", dsnEnv, err)
		}
		q := u.Query()
		for _, k := range keys {
			q.Set(k, params[k])
		}
		u.RawQuery = q.Encode()
		return u.String()
	}

	// Keyword/value form ("host=… dbname=…"). Nothing here needs quoting: the
	// values are a generated identifier and an IANA zone name.
	var b strings.Builder
	b.WriteString(dsn)
	for _, k := range keys {
		fmt.Fprintf(&b, " %s=%s", k, params[k])
	}
	return b.String()
}

// migrate applies migrations/*.sql in lexical order, which migrations/001
// states is now also the dependency order. Each file goes over the wire whole:
// splitting on semicolons would be a parser, and this way a file that only
// works when applied as one unit keeps working.
func (s *stack) migrate(t *testing.T) {
	t.Helper()

	dir := filepath.Join(repoRoot(t), "migrations")
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	if len(files) == 0 {
		t.Fatalf("no migrations found in %s — the suite would then assert against an empty schema", dir)
	}
	sort.Strings(files)

	for _, file := range files {
		ddl, err := os.ReadFile(file) // #nosec G304 — a path this test built from the repo root
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if _, err := s.DB.Exec(string(ddl)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(file), err)
		}
	}
}

// repoRoot walks up from the test's working directory to the module root, so
// the suite finds migrations/ wherever `go test` is invoked from.
func repoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// seedSQL is the fixed dataset internal/database/sqlite.go seeds, restated for
// Postgres. The ids matter and are not incidental: they are the public contract
// with consumers (migrations/001), the existing SQLite repository tests assert
// against these exact numbers, and deploy/compose/README.md walks an operator
// through them. Restating them here is duplication, and it is unavoidable —
// sqliteSeed is an unexported const, and its INSERT OR IGNORE is not Postgres
// syntax. If the two ever disagree, this file is the one that is wrong.
const seedSQL = `
INSERT INTO CONFIG_ENVIRONMENTS (ID, ENVIRONMENT) VALUES
    (1, 'dev'), (2, 'staging'), (3, 'prod');

INSERT INTO CONFIG_MICROSERVICES (ID, MICROSERVICE) VALUES
    (1, 'catalog-api'), (2, 'cart-api'), (3, 'storefront-api');

INSERT INTO CONFIG_FLAG (ID, FLAG_KEY, IS_ACTIVE) VALUES
    (7, 'search_v2', 1),
    (8, 'dark_mode', 1),
    (9, 'new_pricing', 1);

INSERT INTO CONFIG_FLAG_VALUE (ID, ENVIRONMENT_ID, FLAG_ID, ENABLED, VALUE) VALUES
    (100, 1, 7, 1, 'on'),
    (101, 1, 8, 0, 'off'),
    (102, 1, 9, 1, '0.25'),
    (103, 3, 7, 0, 'off');

INSERT INTO CONFIG_MICROSERVICE_APPSETTINGS (ID, MICROSERVICE_ID, ENVIRONMENT_ID, SETTINGS_JSON) VALUES
    (200, 1, 1, '{"service":{"displayName":"Catalog","region":"eu-west"},"http":{"timeoutMs":2000,"maxRetries":3},"cache":{"enabled":true,"ttlSeconds":300}}'),
    (201, 2, 1, '{"service":{"displayName":"Cart"},"http":{"timeoutMs":1500,"maxRetries":5},"cache":{"enabled":false}}'),
    (202, 3, 1, '{"service":{"displayName":"Storefront"},"http":{"timeoutMs":3000,"maxRetries":2},"log":{"level":"info"}}');

INSERT INTO CONFIG_LOCALIZATION (ID, MICROSERVICE_ID, ENVIRONMENT_ID, LOCALE, BUNDLE_JSON) VALUES
    (300, 1, 1, 'en-US', '{"catalog.title":"Catalog","catalog.search":"Search"}'),
    (301, 1, 1, 'pt-BR', '{"catalog.title":"Catálogo","catalog.search":"Buscar"}');
`

func (s *stack) seed(t *testing.T) {
	t.Helper()
	if _, err := s.DB.Exec(seedSQL); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s.resetIdentities(t)
}

// resetIdentities is the step migrations/001 note 2 warns about, performed
// rather than described. The id columns are GENERATED BY DEFAULT AS IDENTITY
// so that a seed can supply the fixed ids consumers depend on — and an explicit
// insert does not advance the identity sequence, so the first row the
// repositories generate would otherwise be id 1, colliding with seeded
// environment 1. That collision arrives at a caller as a duplicate-key error on
// a create that had nothing wrong with it: a 409 nobody asked for, or a 500 if
// the column was not one the repository maps.
//
// Getting this wrong is not hypothetical for this suite — every create test
// below inserts without an id — so it is asserted directly in
// TestSeedDoesNotStrandTheIdentitySequences rather than only relied upon.
func (s *stack) resetIdentities(t *testing.T) {
	t.Helper()
	for _, table := range seededTables {
		var next int64
		if err := s.DB.QueryRow(`SELECT COALESCE(MAX(ID), 0) + 1 FROM ` + table).Scan(&next); err != nil {
			t.Fatalf("read the high-water id of %s: %v", table, err)
		}
		if _, err := s.DB.Exec(fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN ID RESTART WITH %d`, table, next)); err != nil {
			t.Fatalf("restart the identity sequence of %s: %v", table, err)
		}
	}
}

// installRacingWriter arranges for a write to land between the repositories'
// "does this key already exist" SELECT and their INSERT — the interleaving two
// concurrent creates produce and a serial test otherwise cannot reach. The
// trigger stands in for the request that wins the race, so the caller's own
// INSERT hits the unique index and the repository has to map SQLSTATE 23505
// back to its own "already exists" sentinel.
//
// pg_trigger_depth() is the recursion guard: it reads 1 inside the trigger
// fired by the caller's INSERT and 2 inside the one fired by the stand-in
// INSERT this function issues, so the duplicate is written exactly once instead
// of forever.
func (s *stack) installRacingWriter(t *testing.T, name, table, when, insert string) {
	t.Helper()

	stmt := fmt.Sprintf(`
		CREATE FUNCTION %[1]s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF (%[3]s) AND pg_trigger_depth() = 1 THEN
				%[4]s;
			END IF;
			RETURN NEW;
		END;
		$$;
		CREATE TRIGGER %[1]s BEFORE INSERT ON %[2]s
		FOR EACH ROW EXECUTE FUNCTION %[1]s();`, name, table, when, insert)

	if _, err := s.DB.Exec(stmt); err != nil {
		t.Fatalf("install the racing writer on %s: %v", table, err)
	}
}

// count is the escape hatch for assertions the repository API cannot make —
// leftover rows after a delete, and the like.
func (s *stack) count(t *testing.T, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := s.DB.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count (%s): %v", query, err)
	}
	return n
}
