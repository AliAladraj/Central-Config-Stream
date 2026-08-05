package flagsconfig

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Repository interface {
	GetFlagsByID(ctx context.Context, id int64) (*Flag, error)
	GetFlagValueByID(ctx context.Context, id int64) (*FlagValue, error)
	// UpdateFlagValue returns the stored row plus its flag's human-stable key,
	// so the caller can publish without a second lookup.
	UpdateFlagValue(ctx context.Context, input FlagValue) (*FlagValue, string, error)

	ListFlags(ctx context.Context, filter FlagFilter) ([]Flag, error)
	CreateFlag(ctx context.Context, input Flag) (*Flag, error)
	// DeleteFlag removes the flag and every value row hanging off it, returning
	// what went away so the caller can purge the matching KV keys.
	DeleteFlag(ctx context.Context, id int64) ([]DeletedFlagValue, error)

	ListFlagValues(ctx context.Context, filter FlagValueFilter) ([]FlagValueRow, error)
	// CreateFlagValue returns the stored row plus its flag key, on the same
	// terms as UpdateFlagValue.
	CreateFlagValue(ctx context.Context, input FlagValue) (*FlagValue, string, error)
	DeleteFlagValue(ctx context.Context, id int64) (*DeletedFlagValue, error)
}

type OracleRepository struct {
	db *sql.DB
}

func NewOracleRepository(db *sql.DB) *OracleRepository {
	return &OracleRepository{db: db}
}

func (r *OracleRepository) GetFlagsByID(ctx context.Context, id int64) (*Flag, error) {
	const query = `
		SELECT ID, FLAG_KEY, IS_ACTIVE, UPDATED_AT
		FROM CONFIG_FLAG
		WHERE ID = :1
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

func (r *OracleRepository) GetFlagValueByID(ctx context.Context, id int64) (*FlagValue, error) {
	const query = `
		SELECT ID, ENVIRONMENT_ID, FLAG_ID, ENABLED, VALUE, UPDATED_AT
		FROM CONFIG_FLAG_VALUE
		WHERE ID = :1
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

func (r *OracleRepository) UpdateFlagValue(ctx context.Context, input FlagValue) (*FlagValue, string, error) {
	const query = `
		UPDATE CONFIG_FLAG_VALUE
		SET VALUE = :1,
		    ENABLED = :2,
		    UPDATED_AT = CURRENT_TIMESTAMP
		WHERE ID = :3
	`

	// Optimistic concurrency is opt-in: only when the caller told us which
	// version it read does the UPDATE become conditional.
	stmt, args := query, []any{input.Value, input.Enabled, input.ID}
	if input.ExpectedUpdatedAt != nil {
		stmt += ` AND UPDATED_AT = :4`
		args = append(args, *input.ExpectedUpdatedAt)
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
func (r *OracleRepository) noRowsErr(ctx context.Context, input FlagValue) error {
	if input.ExpectedUpdatedAt == nil {
		return ErrFlagNotFound
	}
	if _, err := r.GetFlagValueByID(ctx, input.ID); err != nil {
		return err // ErrFlagNotFound, or a real read failure
	}
	return ErrConflict
}

// getFlagValueWithKey reads back the stored row and its flag key in one round
// trip, so the publish path does not need a separate CONFIG_FLAG lookup.
func (r *OracleRepository) getFlagValueWithKey(ctx context.Context, id int64) (*FlagValue, string, error) {
	const query = `
		SELECT fv.ID, fv.ENVIRONMENT_ID, fv.FLAG_ID, fv.ENABLED, fv.VALUE, fv.UPDATED_AT, f.FLAG_KEY
		FROM CONFIG_FLAG_VALUE fv
		JOIN CONFIG_FLAG f ON f.ID = fv.FLAG_ID
		WHERE fv.ID = :1
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

func (r *OracleRepository) ListFlags(ctx context.Context, filter FlagFilter) ([]Flag, error) {
	query := `SELECT ID, FLAG_KEY, IS_ACTIVE, UPDATED_AT FROM CONFIG_FLAG`

	var args []any
	if filter.FlagKey != "" {
		query += ` WHERE FLAG_KEY = :1`
		args = append(args, filter.FlagKey)
	}
	query += fmt.Sprintf(` ORDER BY ID OFFSET :%d ROWS FETCH NEXT :%d ROWS ONLY`, len(args)+1, len(args)+2)
	args = append(args, filter.Offset, filter.Limit)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list flags: %w", err)
	}
	defer rows.Close()

	return scanFlags(rows)
}

func (r *OracleRepository) CreateFlag(ctx context.Context, input Flag) (*Flag, error) {
	// The unique key is checked here rather than left to the constraint: a
	// driver-specific violation code cannot be mapped to a 409 portably.
	taken, err := r.count(ctx, `SELECT COUNT(*) FROM CONFIG_FLAG WHERE FLAG_KEY = :1`, input.FlagKey)
	if err != nil {
		return nil, err
	}
	if taken > 0 {
		return nil, ErrFlagExists
	}

	const insert = `
		INSERT INTO CONFIG_FLAG (FLAG_KEY, IS_ACTIVE, UPDATED_AT)
		VALUES (:1, :2, CURRENT_TIMESTAMP)
	`
	if _, err := r.db.ExecContext(ctx, insert, input.FlagKey, input.IsActive); err != nil {
		return nil, fmt.Errorf("create flag: %w", err)
	}

	// Read back by the natural key: the generated id never crosses the driver
	// boundary, so no RETURNING clause is needed.
	return r.getFlagByKey(ctx, input.FlagKey)
}

func (r *OracleRepository) getFlagByKey(ctx context.Context, flagKey string) (*Flag, error) {
	const query = `
		SELECT ID, FLAG_KEY, IS_ACTIVE, UPDATED_AT
		FROM CONFIG_FLAG
		WHERE FLAG_KEY = :1
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

// DeleteFlag drops the flag and its values in one transaction. Doing it in two
// statements outside a transaction would leave orphaned value rows behind if the
// second one failed.
func (r *OracleRepository) DeleteFlag(ctx context.Context, id int64) ([]DeletedFlagValue, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin delete flag: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var flagKey string
	err = tx.QueryRowContext(ctx, `SELECT FLAG_KEY FROM CONFIG_FLAG WHERE ID = :1`, id).Scan(&flagKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrFlagNotFound
		}
		return nil, fmt.Errorf("read flag for delete: %w", err)
	}

	removed, err := scanDeletedValues(ctx, tx, `SELECT ENVIRONMENT_ID FROM CONFIG_FLAG_VALUE WHERE FLAG_ID = :1`, id, flagKey)
	if err != nil {
		return nil, err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM CONFIG_FLAG_VALUE WHERE FLAG_ID = :1`, id); err != nil {
		return nil, fmt.Errorf("delete flag values: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM CONFIG_FLAG WHERE ID = :1`, id); err != nil {
		return nil, fmt.Errorf("delete flag: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit delete flag: %w", err)
	}

	return removed, nil
}

func (r *OracleRepository) ListFlagValues(ctx context.Context, filter FlagValueFilter) ([]FlagValueRow, error) {
	query := `
		SELECT fv.ID, fv.ENVIRONMENT_ID, fv.FLAG_ID, f.FLAG_KEY, fv.VALUE, fv.ENABLED, fv.UPDATED_AT
		FROM CONFIG_FLAG_VALUE fv
		JOIN CONFIG_FLAG f ON f.ID = fv.FLAG_ID
	`

	var args []any
	var where []string
	if filter.EnvironmentID > 0 {
		args = append(args, filter.EnvironmentID)
		where = append(where, fmt.Sprintf("fv.ENVIRONMENT_ID = :%d", len(args)))
	}
	if filter.FlagKey != "" {
		args = append(args, filter.FlagKey)
		where = append(where, fmt.Sprintf("f.FLAG_KEY = :%d", len(args)))
	}
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, " AND ")
	}
	query += fmt.Sprintf(` ORDER BY fv.ID OFFSET :%d ROWS FETCH NEXT :%d ROWS ONLY`, len(args)+1, len(args)+2)
	args = append(args, filter.Offset, filter.Limit)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list flag values: %w", err)
	}
	defer rows.Close()

	return scanFlagValueRows(rows)
}

func (r *OracleRepository) CreateFlagValue(ctx context.Context, input FlagValue) (*FlagValue, string, error) {
	if err := r.checkFlagValueRefs(ctx, input); err != nil {
		return nil, "", err
	}

	const insert = `
		INSERT INTO CONFIG_FLAG_VALUE (ENVIRONMENT_ID, FLAG_ID, ENABLED, VALUE, UPDATED_AT)
		VALUES (:1, :2, :3, :4, CURRENT_TIMESTAMP)
	`
	if _, err := r.db.ExecContext(ctx, insert, input.EnvironmentID, input.FlagId, input.Enabled, input.Value); err != nil {
		return nil, "", fmt.Errorf("create flag value: %w", err)
	}

	return r.getFlagValueByEnvAndFlag(ctx, input.EnvironmentID, input.FlagId)
}

// checkFlagValueRefs enforces referential integrity in the application rather
// than relying on the foreign keys: a constraint violation arrives as an opaque
// driver error, and the caller needs to tell "no such environment" apart from
// "already exists" to pick a status code.
func (r *OracleRepository) checkFlagValueRefs(ctx context.Context, input FlagValue) error {
	envs, err := r.count(ctx, `SELECT COUNT(*) FROM CONFIG_ENVIRONMENTS WHERE ID = :1`, input.EnvironmentID)
	if err != nil {
		return err
	}
	if envs == 0 {
		return ErrEnvironmentNotFound
	}

	flags, err := r.count(ctx, `SELECT COUNT(*) FROM CONFIG_FLAG WHERE ID = :1`, input.FlagId)
	if err != nil {
		return err
	}
	if flags == 0 {
		return ErrFlagNotFound
	}

	taken, err := r.count(ctx,
		`SELECT COUNT(*) FROM CONFIG_FLAG_VALUE WHERE ENVIRONMENT_ID = :1 AND FLAG_ID = :2`,
		input.EnvironmentID, input.FlagId)
	if err != nil {
		return err
	}
	if taken > 0 {
		return ErrFlagValueExists
	}
	return nil
}

func (r *OracleRepository) getFlagValueByEnvAndFlag(ctx context.Context, environmentID, flagID int64) (*FlagValue, string, error) {
	const query = `
		SELECT fv.ID, fv.ENVIRONMENT_ID, fv.FLAG_ID, fv.ENABLED, fv.VALUE, fv.UPDATED_AT, f.FLAG_KEY
		FROM CONFIG_FLAG_VALUE fv
		JOIN CONFIG_FLAG f ON f.ID = fv.FLAG_ID
		WHERE fv.ENVIRONMENT_ID = :1 AND fv.FLAG_ID = :2
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

func (r *OracleRepository) DeleteFlagValue(ctx context.Context, id int64) (*DeletedFlagValue, error) {
	const read = `
		SELECT fv.ENVIRONMENT_ID, f.FLAG_KEY
		FROM CONFIG_FLAG_VALUE fv
		JOIN CONFIG_FLAG f ON f.ID = fv.FLAG_ID
		WHERE fv.ID = :1
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

	result, err := r.db.ExecContext(ctx, `DELETE FROM CONFIG_FLAG_VALUE WHERE ID = :1`, id)
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

func (r *OracleRepository) count(ctx context.Context, query string, args ...any) (int64, error) {
	var n int64
	if err := r.db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count rows: %w", err)
	}
	return n, nil
}

// ---- Scan helpers shared by both drivers (the SQL differs, the rows do not) ----

func scanFlags(rows *sql.Rows) ([]Flag, error) {
	var out []Flag
	for rows.Next() {
		var cfg Flag
		if err := rows.Scan(&cfg.ID, &cfg.FlagKey, &cfg.IsActive, &cfg.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan flag row: %w", err)
		}
		out = append(out, cfg)
	}
	return out, rows.Err()
}

func scanFlagValueRows(rows *sql.Rows) ([]FlagValueRow, error) {
	var out []FlagValueRow
	for rows.Next() {
		var row FlagValueRow
		if err := rows.Scan(&row.ID, &row.EnvironmentID, &row.FlagId, &row.FlagKey, &row.Value, &row.Enabled, &row.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan flag value row: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// scanDeletedValues collects the KV coordinates of every value row under a flag
// before they are deleted. The rows are drained and closed before the caller
// issues its DELETE, because a transaction cannot execute while a query on it
// is still open.
func scanDeletedValues(ctx context.Context, tx *sql.Tx, query string, flagID int64, flagKey string) ([]DeletedFlagValue, error) {
	rows, err := tx.QueryContext(ctx, query, flagID)
	if err != nil {
		return nil, fmt.Errorf("list flag values for delete: %w", err)
	}
	defer rows.Close()

	var removed []DeletedFlagValue
	for rows.Next() {
		var environmentID int64
		if err := rows.Scan(&environmentID); err != nil {
			return nil, fmt.Errorf("scan flag value for delete: %w", err)
		}
		removed = append(removed, DeletedFlagValue{EnvironmentID: environmentID, FlagKey: flagKey})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list flag values for delete: %w", err)
	}
	return removed, rows.Close()
}

// ReconcileRow is a flat flag-value + its human-stable key, used to republish
// every flag to KV during reconciliation. ID is the CONFIG_FLAG_VALUE id — the
// value an admin update targets.
type ReconcileRow struct {
	ID            int64
	EnvironmentID int64
	FlagKey       string
	Enabled       int64
	Value         string
}

// ListAllForReconcile returns flag values joined with their flag key. A zero
// `since` sweeps every row; otherwise only rows changed at or after `since`,
// which keeps the periodic reconcile from re-reading the whole table.
func (r *OracleRepository) ListAllForReconcile(ctx context.Context, since time.Time) ([]ReconcileRow, error) {
	const base = `
		SELECT fv.ID, fv.ENVIRONMENT_ID, f.FLAG_KEY, fv.ENABLED, fv.VALUE
		FROM CONFIG_FLAG_VALUE fv
		JOIN CONFIG_FLAG f ON f.ID = fv.FLAG_ID
	`

	query := base
	var args []any
	if !since.IsZero() {
		query = base + ` WHERE fv.UPDATED_AT >= :1`
		args = append(args, since)
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
