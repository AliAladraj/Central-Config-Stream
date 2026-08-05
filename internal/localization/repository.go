package localization

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Repository interface {
	GetLocalizationByID(ctx context.Context, id int64) (*Localization, error)
	GetLocalization(ctx context.Context, microserviceID, environmentID int64, locale string) (*Localization, error)
	UpdateLocalization(ctx context.Context, input Localization) (*Localization, error)

	ListLocalizations(ctx context.Context, filter Filter) ([]Localization, error)
	CreateLocalization(ctx context.Context, input Localization) (*Localization, error)
	// DeleteLocalization returns the row it removed, so the caller can derive
	// the KV key it has to purge.
	DeleteLocalization(ctx context.Context, id int64) (*Localization, error)
}

type OracleRepository struct {
	db *sql.DB
}

func NewOracleRepository(db *sql.DB) *OracleRepository {
	return &OracleRepository{db: db}
}

const selectColumns = `ID, MICROSERVICE_ID, ENVIRONMENT_ID, LOCALE, BUNDLE_JSON, UPDATED_AT`

func scanRow(row interface{ Scan(...any) error }) (*Localization, error) {
	var l Localization
	var bundle string
	if err := row.Scan(&l.ID, &l.MicroserviceID, &l.EnvironmentID, &l.Locale, &bundle, &l.UpdatedAt); err != nil {
		return nil, err
	}
	l.BundleJSON = json.RawMessage(bundle)
	return &l, nil
}

func (r *OracleRepository) GetLocalizationByID(ctx context.Context, id int64) (*Localization, error) {
	query := `SELECT ` + selectColumns + ` FROM CONFIG_LOCALIZATION WHERE ID = :1`

	l, err := scanRow(r.db.QueryRowContext(ctx, query, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrLocalizationNotFound
		}
		return nil, fmt.Errorf("get localization by id: %w", err)
	}
	return l, nil
}

func (r *OracleRepository) GetLocalization(ctx context.Context, microserviceID, environmentID int64, locale string) (*Localization, error) {
	query := `SELECT ` + selectColumns + `
		FROM CONFIG_LOCALIZATION
		WHERE MICROSERVICE_ID = :1 AND ENVIRONMENT_ID = :2 AND LOCALE = :3`

	l, err := scanRow(r.db.QueryRowContext(ctx, query, microserviceID, environmentID, locale))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrLocalizationNotFound
		}
		return nil, fmt.Errorf("get localization: %w", err)
	}
	return l, nil
}

func (r *OracleRepository) UpdateLocalization(ctx context.Context, input Localization) (*Localization, error) {
	const query = `
		UPDATE CONFIG_LOCALIZATION
		SET MICROSERVICE_ID = :1,
		    ENVIRONMENT_ID = :2,
		    LOCALE = :3,
		    BUNDLE_JSON = :4,
		    UPDATED_AT = CURRENT_TIMESTAMP
		WHERE ID = :5
	`

	// Optimistic concurrency is opt-in: only when the caller told us which
	// version it read does the UPDATE become conditional.
	stmt := query
	args := []any{input.MicroserviceID, input.EnvironmentID, input.Locale, string(input.BundleJSON), input.ID}
	if input.ExpectedUpdatedAt != nil {
		stmt += ` AND UPDATED_AT = :6`
		args = append(args, *input.ExpectedUpdatedAt)
	}

	result, err := r.db.ExecContext(ctx, stmt, args...)
	if err != nil {
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
func (r *OracleRepository) noRowsErr(ctx context.Context, input Localization) error {
	if input.ExpectedUpdatedAt == nil {
		return ErrLocalizationNotFound
	}
	if _, err := r.GetLocalizationByID(ctx, input.ID); err != nil {
		return err // ErrLocalizationNotFound, or a real read failure
	}
	return ErrConflict
}

func (r *OracleRepository) ListLocalizations(ctx context.Context, filter Filter) ([]Localization, error) {
	query := `SELECT ` + selectColumns + ` FROM CONFIG_LOCALIZATION`

	var args []any
	var where []string
	if filter.MicroserviceID > 0 {
		args = append(args, filter.MicroserviceID)
		where = append(where, fmt.Sprintf("MICROSERVICE_ID = :%d", len(args)))
	}
	if filter.EnvironmentID > 0 {
		args = append(args, filter.EnvironmentID)
		where = append(where, fmt.Sprintf("ENVIRONMENT_ID = :%d", len(args)))
	}
	if filter.Locale != "" {
		args = append(args, filter.Locale)
		where = append(where, fmt.Sprintf("LOCALE = :%d", len(args)))
	}
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, " AND ")
	}
	query += fmt.Sprintf(` ORDER BY ID OFFSET :%d ROWS FETCH NEXT :%d ROWS ONLY`, len(args)+1, len(args)+2)
	args = append(args, filter.Offset, filter.Limit)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list localization: %w", err)
	}
	defer rows.Close()

	return scanRows(rows)
}

func (r *OracleRepository) CreateLocalization(ctx context.Context, input Localization) (*Localization, error) {
	if err := r.checkRefs(ctx, input); err != nil {
		return nil, err
	}

	const insert = `
		INSERT INTO CONFIG_LOCALIZATION (MICROSERVICE_ID, ENVIRONMENT_ID, LOCALE, BUNDLE_JSON, UPDATED_AT)
		VALUES (:1, :2, :3, :4, CURRENT_TIMESTAMP)
	`
	if _, err := r.db.ExecContext(ctx, insert,
		input.MicroserviceID, input.EnvironmentID, input.Locale, string(input.BundleJSON)); err != nil {
		return nil, fmt.Errorf("create localization: %w", err)
	}

	// Read back by the natural key: the generated id never crosses the driver
	// boundary, so no RETURNING clause is needed.
	return r.GetLocalization(ctx, input.MicroserviceID, input.EnvironmentID, input.Locale)
}

// checkRefs enforces referential integrity in the application rather than
// relying on the foreign keys: a constraint violation arrives as an opaque
// driver error, and the caller needs to tell "no such microservice" apart from
// "already exists" to pick a status code.
func (r *OracleRepository) checkRefs(ctx context.Context, input Localization) error {
	services, err := r.count(ctx, `SELECT COUNT(*) FROM CONFIG_MICROSERVICES WHERE ID = :1`, input.MicroserviceID)
	if err != nil {
		return err
	}
	if services == 0 {
		return ErrMicroserviceNotFound
	}

	envs, err := r.count(ctx, `SELECT COUNT(*) FROM CONFIG_ENVIRONMENTS WHERE ID = :1`, input.EnvironmentID)
	if err != nil {
		return err
	}
	if envs == 0 {
		return ErrEnvironmentNotFound
	}

	taken, err := r.count(ctx,
		`SELECT COUNT(*) FROM CONFIG_LOCALIZATION WHERE MICROSERVICE_ID = :1 AND ENVIRONMENT_ID = :2 AND LOCALE = :3`,
		input.MicroserviceID, input.EnvironmentID, input.Locale)
	if err != nil {
		return err
	}
	if taken > 0 {
		return ErrLocalizationExists
	}
	return nil
}

func (r *OracleRepository) DeleteLocalization(ctx context.Context, id int64) (*Localization, error) {
	// The KV key is read before the row goes away — afterwards there is nothing
	// left to derive it from.
	removed, err := r.GetLocalizationByID(ctx, id)
	if err != nil {
		return nil, err
	}

	result, err := r.db.ExecContext(ctx, `DELETE FROM CONFIG_LOCALIZATION WHERE ID = :1`, id)
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

func (r *OracleRepository) count(ctx context.Context, query string, args ...any) (int64, error) {
	var n int64
	if err := r.db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count rows: %w", err)
	}
	return n, nil
}

// scanRows drains a multi-row query. Shared by both drivers: the SQL differs,
// the rows do not.
func scanRows(rows *sql.Rows) ([]Localization, error) {
	var out []Localization
	for rows.Next() {
		l, err := scanRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scan localization row: %w", err)
		}
		out = append(out, *l)
	}
	return out, rows.Err()
}

// ListAllForReconcile returns localization bundles to republish to KV. A zero
// `since` sweeps every row; otherwise only rows changed at or after `since`.
func (r *OracleRepository) ListAllForReconcile(ctx context.Context, since time.Time) ([]Localization, error) {
	query := `SELECT ` + selectColumns + ` FROM CONFIG_LOCALIZATION`
	var args []any
	if !since.IsZero() {
		query += ` WHERE UPDATED_AT >= :1`
		args = append(args, since)
	}

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
