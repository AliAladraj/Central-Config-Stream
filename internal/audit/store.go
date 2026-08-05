package audit

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// sqliteTimeLayout is how SQLite's CURRENT_TIMESTAMP renders a UTC timestamp;
// bound values have to match it for the range comparison to work. Same constant
// the domain repositories carry.
const sqliteTimeLayout = "2006-01-02 15:04:05"

// Store reads and writes CONFIG_AUDIT_LOG. Unlike the domain repositories there
// is a single implementation: the two statements differ only in bind syntax and
// pagination clause, so a dialect flag is cheaper than a second file that would
// have to be kept in sync by hand.
type Store struct {
	db     *sql.DB
	sqlite bool
}

// NewStore picks the dialect from the same DB_DRIVER value the repositories use.
func NewStore(db *sql.DB, driver string) *Store {
	return &Store{db: db, sqlite: strings.EqualFold(driver, "sqlite")}
}

// bind renders the n-th placeholder: Oracle wants :1, SQLite wants ?.
func (s *Store) bind(n int) string {
	if s.sqlite {
		return "?"
	}
	return ":" + strconv.Itoa(n)
}

// timeArg formats a timestamp the way the column stores it.
func (s *Store) timeArg(t time.Time) any {
	if s.sqlite {
		return t.UTC().Format(sqliteTimeLayout)
	}
	return t.UTC()
}

func (s *Store) Insert(ctx context.Context, e Entry) error {
	query := fmt.Sprintf(`
		INSERT INTO CONFIG_AUDIT_LOG
			(OCCURRED_AT, ACTOR, METHOD, PATH, TARGET_DOMAIN, TARGET_ID,
			 ENVIRONMENT_ID, STATUS_CODE, REMOTE_ADDR, REQUEST_BODY)
		VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, %s)
	`,
		s.bind(1), s.bind(2), s.bind(3), s.bind(4), s.bind(5),
		s.bind(6), s.bind(7), s.bind(8), s.bind(9), s.bind(10))

	// Oracle stores the empty string as NULL, so the optional columns are bound
	// as nullable rather than as "".
	_, err := s.db.ExecContext(ctx, query,
		s.timeArg(e.OccurredAt),
		nullString(e.Actor),
		e.Method,
		e.Path,
		nullString(e.Domain),
		nullString(e.TargetID),
		nullInt64(e.EnvironmentID),
		e.StatusCode,
		nullString(e.RemoteAddr),
		nullString(e.RequestBody),
	)
	if err != nil {
		return fmt.Errorf("insert audit entry: %w", err)
	}
	return nil
}

// List returns the most recent entries first — an audit reader is almost always
// asking "what just happened".
func (s *Store) List(ctx context.Context, filter Filter) ([]Entry, error) {
	query := `
		SELECT ID, OCCURRED_AT, ACTOR, METHOD, PATH, TARGET_DOMAIN, TARGET_ID,
		       ENVIRONMENT_ID, STATUS_CODE, REMOTE_ADDR, REQUEST_BODY
		FROM CONFIG_AUDIT_LOG
	`

	var args []any
	var where []string
	if filter.Actor != "" {
		args = append(args, filter.Actor)
		where = append(where, "ACTOR = "+s.bind(len(args)))
	}
	if !filter.From.IsZero() {
		args = append(args, s.timeArg(filter.From))
		where = append(where, "OCCURRED_AT >= "+s.bind(len(args)))
	}
	if !filter.To.IsZero() {
		args = append(args, s.timeArg(filter.To))
		where = append(where, "OCCURRED_AT <= "+s.bind(len(args)))
	}
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, " AND ")
	}

	if s.sqlite {
		query += ` ORDER BY ID DESC LIMIT ? OFFSET ?`
		args = append(args, filter.Limit, filter.Offset)
	} else {
		query += fmt.Sprintf(` ORDER BY ID DESC OFFSET :%d ROWS FETCH NEXT :%d ROWS ONLY`,
			len(args)+1, len(args)+2)
		args = append(args, filter.Offset, filter.Limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list audit entries: %w", err)
	}
	defer rows.Close()

	out := make([]Entry, 0)
	for rows.Next() {
		var (
			e      Entry
			actor  sql.NullString
			domain sql.NullString
			target sql.NullString
			envID  sql.NullInt64
			remote sql.NullString
			body   sql.NullString
		)
		if err := rows.Scan(&e.ID, &e.OccurredAt, &actor, &e.Method, &e.Path,
			&domain, &target, &envID, &e.StatusCode, &remote, &body); err != nil {
			return nil, fmt.Errorf("scan audit entry: %w", err)
		}
		e.Actor = actor.String
		e.Domain = domain.String
		e.TargetID = target.String
		e.EnvironmentID = envID.Int64
		e.RemoteAddr = remote.String
		e.RequestBody = body.String
		out = append(out, e)
	}
	return out, rows.Err()
}

func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
