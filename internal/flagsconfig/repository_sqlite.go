package flagsconfig

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// sqliteTimeLayout is how SQLite's CURRENT_TIMESTAMP renders a UTC timestamp.
const sqliteTimeLayout = "2006-01-02 15:04:05"

// SQLiteRepository backs the local test stack (see internal/database/sqlite.go).
// Same tables and columns as Oracle; only the bind syntax differs (? vs :1).
type SQLiteRepository struct {
	db *sql.DB
}

func NewSQLiteRepository(db *sql.DB) *SQLiteRepository {
	return &SQLiteRepository{db: db}
}

func (r *SQLiteRepository) GetFlagsByID(ctx context.Context, id int64) (*Flag, error) {
	const query = `
		SELECT ID, FLAG_KEY, IS_ACTIVE, UPDATED_AT
		FROM CONFIG_FLAG
		WHERE ID = ?
	`

	var cfg Flag
	err := r.db.QueryRowContext(ctx, query, id).Scan(
		&cfg.ID,
		&cfg.FlagKey,
		&cfg.IsActive,
		&cfg.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrFlagNotFound
		}
		return nil, fmt.Errorf("get flag by id: %w", err)
	}
	return &cfg, nil
}

func (r *SQLiteRepository) GetFlagValueByID(ctx context.Context, id int64) (*FlagValue, error) {
	const query = `
		SELECT ID, ENVIRONMENT_ID, FLAG_ID, ENABLED, VALUE, UPDATED_AT
		FROM CONFIG_FLAG_VALUE
		WHERE ID = ?
	`

	var val FlagValue
	err := r.db.QueryRowContext(ctx, query, id).Scan(
		&val.ID,
		&val.EnvironmentID,
		&val.FlagId,
		&val.Enabled,
		&val.Value,
		&val.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrFlagNotFound
		}
		return nil, fmt.Errorf("get flag value by id: %w", err)
	}
	return &val, nil
}

func (r *SQLiteRepository) UpdateFlagValue(ctx context.Context, input FlagValue) (*FlagValue, string, error) {
	const query = `
		UPDATE CONFIG_FLAG_VALUE
		SET VALUE = ?,
		    ENABLED = ?,
		    UPDATED_AT = CURRENT_TIMESTAMP
		WHERE ID = ?
	`

	// Optimistic concurrency is opt-in: only when the caller told us which
	// version it read does the UPDATE become conditional. CURRENT_TIMESTAMP
	// writes 'YYYY-MM-DD HH:MM:SS' in UTC, so the bound value has to be
	// formatted the same way for the string comparison.
	stmt, args := query, []any{input.Value, input.Enabled, input.ID}
	if input.ExpectedUpdatedAt != nil {
		stmt += ` AND UPDATED_AT = ?`
		args = append(args, input.ExpectedUpdatedAt.UTC().Format(sqliteTimeLayout))
	}

	result, err := r.db.ExecContext(ctx, stmt, args...)
	if err != nil {
		return nil, "", fmt.Errorf("update flag value: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return nil, "", fmt.Errorf("read affected rows: %w", err)
	}
	if rowsAffected == 0 {
		return nil, "", r.noRowsErr(ctx, input)
	}

	return r.getFlagValueWithKey(ctx, input.ID)
}

// noRowsErr tells a lost optimistic-concurrency race apart from a missing row:
// a guarded UPDATE that matches nothing cannot distinguish the two by itself.
func (r *SQLiteRepository) noRowsErr(ctx context.Context, input FlagValue) error {
	if input.ExpectedUpdatedAt == nil {
		return ErrFlagNotFound
	}
	if _, err := r.GetFlagValueByID(ctx, input.ID); err != nil {
		return err // ErrFlagNotFound, or a real read failure
	}
	return ErrConflict
}

func (r *SQLiteRepository) getFlagValueWithKey(ctx context.Context, id int64) (*FlagValue, string, error) {
	const query = `
		SELECT fv.ID, fv.ENVIRONMENT_ID, fv.FLAG_ID, fv.ENABLED, fv.VALUE, fv.UPDATED_AT, f.FLAG_KEY
		FROM CONFIG_FLAG_VALUE fv
		JOIN CONFIG_FLAG f ON f.ID = fv.FLAG_ID
		WHERE fv.ID = ?
	`

	var val FlagValue
	var flagKey string
	err := r.db.QueryRowContext(ctx, query, id).Scan(
		&val.ID,
		&val.EnvironmentID,
		&val.FlagId,
		&val.Enabled,
		&val.Value,
		&val.UpdatedAt,
		&flagKey,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", ErrFlagNotFound
		}
		return nil, "", fmt.Errorf("get flag value with key: %w", err)
	}
	return &val, flagKey, nil
}

func (r *SQLiteRepository) ListFlags(ctx context.Context, filter FlagFilter) ([]Flag, error) {
	query := `SELECT ID, FLAG_KEY, IS_ACTIVE, UPDATED_AT FROM CONFIG_FLAG`

	var args []any
	if filter.FlagKey != "" {
		query += ` WHERE FLAG_KEY = ?`
		args = append(args, filter.FlagKey)
	}
	query += ` ORDER BY ID LIMIT ? OFFSET ?`
	args = append(args, filter.Limit, filter.Offset)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list flags: %w", err)
	}
	defer rows.Close()

	return scanFlags(rows)
}

func (r *SQLiteRepository) CreateFlag(ctx context.Context, input Flag) (*Flag, error) {
	// The unique key is checked here rather than left to the constraint: a
	// driver-specific violation code cannot be mapped to a 409 portably.
	taken, err := r.count(ctx, `SELECT COUNT(*) FROM CONFIG_FLAG WHERE FLAG_KEY = ?`, input.FlagKey)
	if err != nil {
		return nil, err
	}
	if taken > 0 {
		return nil, ErrFlagExists
	}

	const insert = `
		INSERT INTO CONFIG_FLAG (FLAG_KEY, IS_ACTIVE, UPDATED_AT)
		VALUES (?, ?, CURRENT_TIMESTAMP)
	`
	if _, err := r.db.ExecContext(ctx, insert, input.FlagKey, input.IsActive); err != nil {
		return nil, fmt.Errorf("create flag: %w", err)
	}

	return r.getFlagByKey(ctx, input.FlagKey)
}

func (r *SQLiteRepository) getFlagByKey(ctx context.Context, flagKey string) (*Flag, error) {
	const query = `
		SELECT ID, FLAG_KEY, IS_ACTIVE, UPDATED_AT
		FROM CONFIG_FLAG
		WHERE FLAG_KEY = ?
	`

	var cfg Flag
	err := r.db.QueryRowContext(ctx, query, flagKey).Scan(&cfg.ID, &cfg.FlagKey, &cfg.IsActive, &cfg.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrFlagNotFound
		}
		return nil, fmt.Errorf("get flag by key: %w", err)
	}
	return &cfg, nil
}

func (r *SQLiteRepository) DeleteFlag(ctx context.Context, id int64) ([]DeletedFlagValue, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin delete flag: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var flagKey string
	err = tx.QueryRowContext(ctx, `SELECT FLAG_KEY FROM CONFIG_FLAG WHERE ID = ?`, id).Scan(&flagKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrFlagNotFound
		}
		return nil, fmt.Errorf("read flag for delete: %w", err)
	}

	removed, err := scanDeletedValues(ctx, tx, `SELECT ENVIRONMENT_ID FROM CONFIG_FLAG_VALUE WHERE FLAG_ID = ?`, id, flagKey)
	if err != nil {
		return nil, err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM CONFIG_FLAG_VALUE WHERE FLAG_ID = ?`, id); err != nil {
		return nil, fmt.Errorf("delete flag values: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM CONFIG_FLAG WHERE ID = ?`, id); err != nil {
		return nil, fmt.Errorf("delete flag: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit delete flag: %w", err)
	}

	return removed, nil
}

func (r *SQLiteRepository) ListFlagValues(ctx context.Context, filter FlagValueFilter) ([]FlagValueRow, error) {
	query := `
		SELECT fv.ID, fv.ENVIRONMENT_ID, fv.FLAG_ID, f.FLAG_KEY, fv.VALUE, fv.ENABLED, fv.UPDATED_AT
		FROM CONFIG_FLAG_VALUE fv
		JOIN CONFIG_FLAG f ON f.ID = fv.FLAG_ID
	`

	var args []any
	var where []string
	if filter.EnvironmentID > 0 {
		where = append(where, "fv.ENVIRONMENT_ID = ?")
		args = append(args, filter.EnvironmentID)
	}
	if filter.FlagKey != "" {
		where = append(where, "f.FLAG_KEY = ?")
		args = append(args, filter.FlagKey)
	}
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, " AND ")
	}
	query += ` ORDER BY fv.ID LIMIT ? OFFSET ?`
	args = append(args, filter.Limit, filter.Offset)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list flag values: %w", err)
	}
	defer rows.Close()

	return scanFlagValueRows(rows)
}

func (r *SQLiteRepository) CreateFlagValue(ctx context.Context, input FlagValue) (*FlagValue, string, error) {
	if err := r.checkFlagValueRefs(ctx, input); err != nil {
		return nil, "", err
	}

	const insert = `
		INSERT INTO CONFIG_FLAG_VALUE (ENVIRONMENT_ID, FLAG_ID, ENABLED, VALUE, UPDATED_AT)
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
	`
	if _, err := r.db.ExecContext(ctx, insert, input.EnvironmentID, input.FlagId, input.Enabled, input.Value); err != nil {
		return nil, "", fmt.Errorf("create flag value: %w", err)
	}

	return r.getFlagValueByEnvAndFlag(ctx, input.EnvironmentID, input.FlagId)
}

// checkFlagValueRefs enforces referential integrity in the application rather
// than relying on the foreign keys: SQLite does not even enforce them unless
// PRAGMA foreign_keys is on, and the caller needs to tell "no such environment"
// apart from "already exists" to pick a status code.
func (r *SQLiteRepository) checkFlagValueRefs(ctx context.Context, input FlagValue) error {
	envs, err := r.count(ctx, `SELECT COUNT(*) FROM CONFIG_ENVIRONMENTS WHERE ID = ?`, input.EnvironmentID)
	if err != nil {
		return err
	}
	if envs == 0 {
		return ErrEnvironmentNotFound
	}

	flags, err := r.count(ctx, `SELECT COUNT(*) FROM CONFIG_FLAG WHERE ID = ?`, input.FlagId)
	if err != nil {
		return err
	}
	if flags == 0 {
		return ErrFlagNotFound
	}

	taken, err := r.count(ctx,
		`SELECT COUNT(*) FROM CONFIG_FLAG_VALUE WHERE ENVIRONMENT_ID = ? AND FLAG_ID = ?`,
		input.EnvironmentID, input.FlagId)
	if err != nil {
		return err
	}
	if taken > 0 {
		return ErrFlagValueExists
	}
	return nil
}

func (r *SQLiteRepository) getFlagValueByEnvAndFlag(ctx context.Context, environmentID, flagID int64) (*FlagValue, string, error) {
	const query = `
		SELECT fv.ID, fv.ENVIRONMENT_ID, fv.FLAG_ID, fv.ENABLED, fv.VALUE, fv.UPDATED_AT, f.FLAG_KEY
		FROM CONFIG_FLAG_VALUE fv
		JOIN CONFIG_FLAG f ON f.ID = fv.FLAG_ID
		WHERE fv.ENVIRONMENT_ID = ? AND fv.FLAG_ID = ?
	`

	var val FlagValue
	var flagKey string
	err := r.db.QueryRowContext(ctx, query, environmentID, flagID).Scan(
		&val.ID,
		&val.EnvironmentID,
		&val.FlagId,
		&val.Enabled,
		&val.Value,
		&val.UpdatedAt,
		&flagKey,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", ErrFlagNotFound
		}
		return nil, "", fmt.Errorf("get flag value by env and flag: %w", err)
	}
	return &val, flagKey, nil
}

func (r *SQLiteRepository) DeleteFlagValue(ctx context.Context, id int64) (*DeletedFlagValue, error) {
	const read = `
		SELECT fv.ENVIRONMENT_ID, f.FLAG_KEY
		FROM CONFIG_FLAG_VALUE fv
		JOIN CONFIG_FLAG f ON f.ID = fv.FLAG_ID
		WHERE fv.ID = ?
	`

	// The KV key is read before the row goes away — afterwards there is nothing
	// left to derive it from.
	var removed DeletedFlagValue
	err := r.db.QueryRowContext(ctx, read, id).Scan(&removed.EnvironmentID, &removed.FlagKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrFlagNotFound
		}
		return nil, fmt.Errorf("read flag value for delete: %w", err)
	}

	result, err := r.db.ExecContext(ctx, `DELETE FROM CONFIG_FLAG_VALUE WHERE ID = ?`, id)
	if err != nil {
		return nil, fmt.Errorf("delete flag value: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("read affected rows: %w", err)
	}
	if rowsAffected == 0 {
		return nil, ErrFlagNotFound
	}

	return &removed, nil
}

func (r *SQLiteRepository) count(ctx context.Context, query string, args ...any) (int64, error) {
	var n int64
	if err := r.db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count rows: %w", err)
	}
	return n, nil
}

func (r *SQLiteRepository) ListAllForReconcile(ctx context.Context, since time.Time) ([]ReconcileRow, error) {
	const base = `
		SELECT fv.ID, fv.ENVIRONMENT_ID, f.FLAG_KEY, fv.ENABLED, fv.VALUE
		FROM CONFIG_FLAG_VALUE fv
		JOIN CONFIG_FLAG f ON f.ID = fv.FLAG_ID
	`

	query := base
	var args []any
	if !since.IsZero() {
		// CURRENT_TIMESTAMP writes 'YYYY-MM-DD HH:MM:SS' in UTC, so the bound
		// value has to be formatted the same way for the string comparison.
		query = base + ` WHERE fv.UPDATED_AT >= ?`
		args = append(args, since.UTC().Format(sqliteTimeLayout))
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list flags for reconcile: %w", err)
	}
	defer rows.Close()

	var out []ReconcileRow
	for rows.Next() {
		var row ReconcileRow
		if err := rows.Scan(&row.ID, &row.EnvironmentID, &row.FlagKey, &row.Enabled, &row.Value); err != nil {
			return nil, fmt.Errorf("scan flag reconcile row: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

var _ Repository = (*SQLiteRepository)(nil)
