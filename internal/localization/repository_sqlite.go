package localization

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ErasedKyte/Central-Config-Stream/internal/database"
)

// sqliteTimeLayout is how SQLite's CURRENT_TIMESTAMP renders a UTC timestamp.
const sqliteTimeLayout = "2006-01-02 15:04:05"

// SQLiteRepository backs the local test stack (see internal/database/sqlite.go).
// Same tables and columns as PostgreSQL; only the bind syntax differs (? vs $1).
type SQLiteRepository struct {
	db *sql.DB
}

func NewSQLiteRepository(db *sql.DB) *SQLiteRepository {
	return &SQLiteRepository{db: db}
}

func (r *SQLiteRepository) GetLocalizationByID(ctx context.Context, id int64) (*Localization, error) {
	query := `SELECT ` + selectColumns + ` FROM CONFIG_LOCALIZATION WHERE ID = ?`

	l, err := scanRow(r.db.QueryRowContext(ctx, query, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrLocalizationNotFound
		}
		return nil, fmt.Errorf("get localization by id: %w", err)
	}
	return l, nil
}

func (r *SQLiteRepository) GetLocalization(ctx context.Context, microserviceID, environmentID int64, locale string) (*Localization, error) {
	query := `SELECT ` + selectColumns + `
		FROM CONFIG_LOCALIZATION
		WHERE MICROSERVICE_ID = ? AND ENVIRONMENT_ID = ? AND LOCALE = ?`

	l, err := scanRow(r.db.QueryRowContext(ctx, query, microserviceID, environmentID, locale))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrLocalizationNotFound
		}
		return nil, fmt.Errorf("get localization: %w", err)
	}
	return l, nil
}

func (r *SQLiteRepository) UpdateLocalization(ctx context.Context, input Localization) (*Localization, error) {
	// An update rewrites the natural key, so it needs the same referential and
	// uniqueness checks a create gets: without them it can point at rows that
	// do not exist, or collide with another row and surface as a 500.
	if err := r.checkRefs(ctx, input); err != nil {
		return nil, err
	}

	const query = `
		UPDATE CONFIG_LOCALIZATION
		SET MICROSERVICE_ID = ?,
		    ENVIRONMENT_ID = ?,
		    LOCALE = ?,
		    BUNDLE_JSON = ?,
		    UPDATED_AT = CURRENT_TIMESTAMP
		WHERE ID = ?
	`

	// Optimistic concurrency is opt-in: only when the caller told us which
	// version it read does the UPDATE become conditional. CURRENT_TIMESTAMP
	// writes 'YYYY-MM-DD HH:MM:SS' in UTC, so the bound value has to be
	// formatted the same way for the string comparison.
	stmt := query
	args := []any{input.MicroserviceID, input.EnvironmentID, input.Locale, string(input.BundleJSON), input.ID}
	if input.ExpectedUpdatedAt != nil {
		stmt += ` AND UPDATED_AT = ?`
		args = append(args, input.ExpectedUpdatedAt.UTC().Format(sqliteTimeLayout))
	}

	result, err := r.db.ExecContext(ctx, stmt, args...)
	if err != nil {
		if database.IsUniqueViolation(err) {
			return nil, ErrLocalizationExists
		}
		return nil, fmt.Errorf("update localization: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("read affected rows: %w", err)
	}
	if rows == 0 {
		return nil, r.noRowsErr(ctx, input)
	}

	return r.GetLocalizationByID(ctx, input.ID)
}

// noRowsErr tells a lost optimistic-concurrency race apart from a missing row:
// a guarded UPDATE that matches nothing cannot distinguish the two by itself.
func (r *SQLiteRepository) noRowsErr(ctx context.Context, input Localization) error {
	if input.ExpectedUpdatedAt == nil {
		return ErrLocalizationNotFound
	}
	if _, err := r.GetLocalizationByID(ctx, input.ID); err != nil {
		return err // ErrLocalizationNotFound, or a real read failure
	}
	return ErrConflict
}

func (r *SQLiteRepository) ListLocalizations(ctx context.Context, filter Filter) ([]Localization, error) {
	query := `SELECT ` + selectColumns + ` FROM CONFIG_LOCALIZATION`

	var args []any
	var where []string
	if filter.MicroserviceID > 0 {
		where = append(where, "MICROSERVICE_ID = ?")
		args = append(args, filter.MicroserviceID)
	}
	if filter.EnvironmentID > 0 {
		where = append(where, "ENVIRONMENT_ID = ?")
		args = append(args, filter.EnvironmentID)
	}
	if filter.Locale != "" {
		where = append(where, "LOCALE = ?")
		args = append(args, filter.Locale)
	}
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, " AND ")
	}
	query += ` ORDER BY ID LIMIT ? OFFSET ?`
	args = append(args, filter.Limit, filter.Offset)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list localization: %w", err)
	}
	defer rows.Close()

	return scanRows(rows)
}

func (r *SQLiteRepository) CreateLocalization(ctx context.Context, input Localization) (*Localization, error) {
	if err := r.checkRefs(ctx, input); err != nil {
		return nil, err
	}

	const insert = `
		INSERT INTO CONFIG_LOCALIZATION (MICROSERVICE_ID, ENVIRONMENT_ID, LOCALE, BUNDLE_JSON, UPDATED_AT)
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
	`
	if _, err := r.db.ExecContext(ctx, insert,
		input.MicroserviceID, input.EnvironmentID, input.Locale, string(input.BundleJSON)); err != nil {
		// The check above is a fast path, not a lock: two concurrent creates
		// both pass it and the constraint decides between them.
		if database.IsUniqueViolation(err) {
			return nil, ErrLocalizationExists
		}
		return nil, fmt.Errorf("create localization: %w", err)
	}

	return r.GetLocalization(ctx, input.MicroserviceID, input.EnvironmentID, input.Locale)
}

// checkRefs enforces referential integrity in the application rather than
// relying on the foreign keys: SQLite does not even enforce them unless PRAGMA
// foreign_keys is on, and the caller needs to tell "no such microservice" apart
// from "already exists" to pick a status code.
func (r *SQLiteRepository) checkRefs(ctx context.Context, input Localization) error {
	services, err := r.count(ctx, `SELECT COUNT(*) FROM CONFIG_MICROSERVICES WHERE ID = ?`, input.MicroserviceID)
	if err != nil {
		return err
	}
	if services == 0 {
		return ErrMicroserviceNotFound
	}

	envs, err := r.count(ctx, `SELECT COUNT(*) FROM CONFIG_ENVIRONMENTS WHERE ID = ?`, input.EnvironmentID)
	if err != nil {
		return err
	}
	if envs == 0 {
		return ErrEnvironmentNotFound
	}

	// An update may legitimately keep the natural key it already holds, so the
	// row being written is excluded; on a create input.ID is zero and excludes
	// nothing.
	taken, err := r.count(ctx,
		`SELECT COUNT(*) FROM CONFIG_LOCALIZATION
		 WHERE MICROSERVICE_ID = ? AND ENVIRONMENT_ID = ? AND LOCALE = ? AND ID <> ?`,
		input.MicroserviceID, input.EnvironmentID, input.Locale, input.ID)
	if err != nil {
		return err
	}
	if taken > 0 {
		return ErrLocalizationExists
	}
	return nil
}

func (r *SQLiteRepository) DeleteLocalization(ctx context.Context, id int64) (*Localization, error) {
	// The KV key is read before the row goes away — afterwards there is nothing
	// left to derive it from.
	removed, err := r.GetLocalizationByID(ctx, id)
	if err != nil {
		return nil, err
	}

	result, err := r.db.ExecContext(ctx, `DELETE FROM CONFIG_LOCALIZATION WHERE ID = ?`, id)
	if err != nil {
		return nil, fmt.Errorf("delete localization: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("read affected rows: %w", err)
	}
	if rows == 0 {
		return nil, ErrLocalizationNotFound
	}

	return removed, nil
}

func (r *SQLiteRepository) count(ctx context.Context, query string, args ...any) (int64, error) {
	var n int64
	if err := r.db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count rows: %w", err)
	}
	return n, nil
}

func (r *SQLiteRepository) ListAllForReconcile(ctx context.Context, since time.Time) ([]Localization, error) {
	query := `SELECT ` + selectColumns + ` FROM CONFIG_LOCALIZATION`
	var args []any
	if !since.IsZero() {
		query += ` WHERE UPDATED_AT >= ?`
		args = append(args, since.UTC().Format(sqliteTimeLayout))
	}
	// A deterministic order matters when the sweep cannot publish every row:
	// which rows a stalled sweep got to has to be the same every cycle.
	query += ` ORDER BY ID`

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list localization for reconcile: %w", err)
	}
	defer rows.Close()

	var out []Localization
	for rows.Next() {
		l, err := scanRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scan localization reconcile row: %w", err)
		}
		out = append(out, *l)
	}
	return out, rows.Err()
}

var _ Repository = (*SQLiteRepository)(nil)
