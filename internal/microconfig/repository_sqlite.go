package microconfig

import (
	"context"
	"database/sql"
	"encoding/json"
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

func (r *SQLiteRepository) GetMicroserviceConfigByID(ctx context.Context, id int64) (*MicroserviceAppSettings, error) {
	const query = `
		SELECT ID, MICROSERVICE_ID, ENVIRONMENT_ID, SETTINGS_JSON, UPDATED_AT
		FROM CONFIG_MICROSERVICE_APPSETTINGS
		WHERE ID = ?
	`

	var cfg MicroserviceAppSettings
	var settingsText string

	err := r.db.QueryRowContext(ctx, query, id).Scan(
		&cfg.ID,
		&cfg.MicroserviceID,
		&cfg.EnvironmentID,
		&settingsText,
		&cfg.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrConfigNotFound
		}
		return nil, fmt.Errorf("get config by id: %w", err)
	}

	cfg.SettingsJSON = json.RawMessage(settingsText)
	return &cfg, nil
}

func (r *SQLiteRepository) UpdateMicroserviceConfig(ctx context.Context, input MicroserviceAppSettings) (*MicroserviceAppSettings, error) {
	// An update rewrites the natural key, so it needs the same referential and
	// uniqueness checks a create gets: without them it can point at rows that
	// do not exist, or collide with another row and surface as a 500.
	if err := r.checkAppSettingsRefs(ctx, input); err != nil {
		return nil, err
	}

	const query = `
		UPDATE CONFIG_MICROSERVICE_APPSETTINGS
		SET MICROSERVICE_ID = ?,
		    ENVIRONMENT_ID = ?,
		    SETTINGS_JSON = ?,
		    UPDATED_AT = CURRENT_TIMESTAMP
		WHERE ID = ?
	`

	// Optimistic concurrency is opt-in: only when the caller told us which
	// version it read does the UPDATE become conditional. CURRENT_TIMESTAMP
	// writes 'YYYY-MM-DD HH:MM:SS' in UTC, so the bound value has to be
	// formatted the same way for the string comparison.
	stmt := query
	args := []any{input.MicroserviceID, input.EnvironmentID, string(input.SettingsJSON), input.ID}
	if input.ExpectedUpdatedAt != nil {
		stmt += ` AND UPDATED_AT = ?`
		args = append(args, input.ExpectedUpdatedAt.UTC().Format(sqliteTimeLayout))
	}

	result, err := r.db.ExecContext(ctx, stmt, args...)
	if err != nil {
		if database.IsUniqueViolation(err) {
			return nil, ErrConfigExists
		}
		return nil, fmt.Errorf("update config: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("read affected rows: %w", err)
	}
	if rowsAffected == 0 {
		return nil, r.noRowsErr(ctx, input)
	}

	return r.GetMicroserviceConfigByID(ctx, input.ID)
}

// noRowsErr tells a lost optimistic-concurrency race apart from a missing row:
// a guarded UPDATE that matches nothing cannot distinguish the two by itself.
func (r *SQLiteRepository) noRowsErr(ctx context.Context, input MicroserviceAppSettings) error {
	if input.ExpectedUpdatedAt == nil {
		return ErrConfigNotFound
	}
	if _, err := r.GetMicroserviceConfigByID(ctx, input.ID); err != nil {
		return err // ErrConfigNotFound, or a real read failure
	}
	return ErrConflict
}

func (r *SQLiteRepository) GetMicroserviceByID(ctx context.Context, id int64) (*Microservice, error) {
	const query = `
		SELECT ID, MICROSERVICE, UPDATED_AT
		FROM CONFIG_MICROSERVICES
		WHERE ID = ?
	`

	var m Microservice
	err := r.db.QueryRowContext(ctx, query, id).Scan(&m.ID, &m.Microservice, &m.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrMicroserviceNotFound
		}
		return nil, fmt.Errorf("get microservice by id: %w", err)
	}
	return &m, nil
}

func (r *SQLiteRepository) ListMicroserviceConfigs(ctx context.Context, filter AppSettingsFilter) ([]MicroserviceAppSettings, error) {
	query := `
		SELECT ID, MICROSERVICE_ID, ENVIRONMENT_ID, SETTINGS_JSON, UPDATED_AT
		FROM CONFIG_MICROSERVICE_APPSETTINGS
	`

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
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, " AND ")
	}
	query += ` ORDER BY ID LIMIT ? OFFSET ?`
	args = append(args, filter.Limit, filter.Offset)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list microconfig: %w", err)
	}
	defer rows.Close()

	return scanAppSettings(rows)
}

func (r *SQLiteRepository) CreateMicroserviceConfig(ctx context.Context, input MicroserviceAppSettings) (*MicroserviceAppSettings, error) {
	if err := r.checkAppSettingsRefs(ctx, input); err != nil {
		return nil, err
	}

	const insert = `
		INSERT INTO CONFIG_MICROSERVICE_APPSETTINGS (MICROSERVICE_ID, ENVIRONMENT_ID, SETTINGS_JSON, UPDATED_AT)
		VALUES (?, ?, ?, CURRENT_TIMESTAMP)
	`
	if _, err := r.db.ExecContext(ctx, insert, input.MicroserviceID, input.EnvironmentID, string(input.SettingsJSON)); err != nil {
		// The check above is a fast path, not a lock: two concurrent creates
		// both pass it and the constraint decides between them.
		if database.IsUniqueViolation(err) {
			return nil, ErrConfigExists
		}
		return nil, fmt.Errorf("create config: %w", err)
	}

	return r.getConfigByServiceAndEnv(ctx, input.MicroserviceID, input.EnvironmentID)
}

// checkAppSettingsRefs enforces referential integrity in the application rather
// than relying on the foreign keys: SQLite does not even enforce them unless
// PRAGMA foreign_keys is on, and the caller needs to tell "no such microservice"
// apart from "already exists" to pick a status code.
func (r *SQLiteRepository) checkAppSettingsRefs(ctx context.Context, input MicroserviceAppSettings) error {
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
		`SELECT COUNT(*) FROM CONFIG_MICROSERVICE_APPSETTINGS
		 WHERE MICROSERVICE_ID = ? AND ENVIRONMENT_ID = ? AND ID <> ?`,
		input.MicroserviceID, input.EnvironmentID, input.ID)
	if err != nil {
		return err
	}
	if taken > 0 {
		return ErrConfigExists
	}
	return nil
}

func (r *SQLiteRepository) getConfigByServiceAndEnv(ctx context.Context, microserviceID, environmentID int64) (*MicroserviceAppSettings, error) {
	const query = `
		SELECT ID, MICROSERVICE_ID, ENVIRONMENT_ID, SETTINGS_JSON, UPDATED_AT
		FROM CONFIG_MICROSERVICE_APPSETTINGS
		WHERE MICROSERVICE_ID = ? AND ENVIRONMENT_ID = ?
	`

	var cfg MicroserviceAppSettings
	var settingsText string
	err := r.db.QueryRowContext(ctx, query, microserviceID, environmentID).Scan(
		&cfg.ID,
		&cfg.MicroserviceID,
		&cfg.EnvironmentID,
		&settingsText,
		&cfg.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrConfigNotFound
		}
		return nil, fmt.Errorf("get config by service and env: %w", err)
	}

	cfg.SettingsJSON = json.RawMessage(settingsText)
	return &cfg, nil
}

func (r *SQLiteRepository) DeleteMicroserviceConfig(ctx context.Context, id int64) (*MicroserviceAppSettings, error) {
	// The KV key is read before the row goes away — afterwards there is nothing
	// left to derive it from.
	removed, err := r.GetMicroserviceConfigByID(ctx, id)
	if err != nil {
		return nil, err
	}

	result, err := r.db.ExecContext(ctx, `DELETE FROM CONFIG_MICROSERVICE_APPSETTINGS WHERE ID = ?`, id)
	if err != nil {
		return nil, fmt.Errorf("delete config: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("read affected rows: %w", err)
	}
	if rowsAffected == 0 {
		return nil, ErrConfigNotFound
	}

	return removed, nil
}

func (r *SQLiteRepository) ListMicroservices(ctx context.Context, page Page) ([]Microservice, error) {
	const query = `
		SELECT ID, MICROSERVICE, UPDATED_AT
		FROM CONFIG_MICROSERVICES
		ORDER BY ID
		LIMIT ? OFFSET ?
	`

	rows, err := r.db.QueryContext(ctx, query, page.Limit, page.Offset)
	if err != nil {
		return nil, fmt.Errorf("list microservices: %w", err)
	}
	defer rows.Close()

	return scanMicroservices(rows)
}

func (r *SQLiteRepository) CreateMicroservice(ctx context.Context, name string) (*Microservice, error) {
	taken, err := r.count(ctx, `SELECT COUNT(*) FROM CONFIG_MICROSERVICES WHERE MICROSERVICE = ?`, name)
	if err != nil {
		return nil, err
	}
	if taken > 0 {
		return nil, ErrMicroserviceExists
	}

	const insert = `INSERT INTO CONFIG_MICROSERVICES (MICROSERVICE, UPDATED_AT) VALUES (?, CURRENT_TIMESTAMP)`
	if _, err := r.db.ExecContext(ctx, insert, name); err != nil {
		if database.IsUniqueViolation(err) {
			return nil, ErrMicroserviceExists
		}
		return nil, fmt.Errorf("create microservice: %w", err)
	}

	const query = `SELECT ID, MICROSERVICE, UPDATED_AT FROM CONFIG_MICROSERVICES WHERE MICROSERVICE = ?`

	var m Microservice
	if err := r.db.QueryRowContext(ctx, query, name).Scan(&m.ID, &m.Microservice, &m.UpdatedAt); err != nil {
		return nil, fmt.Errorf("get microservice by name: %w", err)
	}
	return &m, nil
}

func (r *SQLiteRepository) DeleteMicroservice(ctx context.Context, id int64) error {
	const refs = `
		SELECT (SELECT COUNT(*) FROM CONFIG_MICROSERVICE_APPSETTINGS WHERE MICROSERVICE_ID = ?)
		     + (SELECT COUNT(*) FROM CONFIG_LOCALIZATION WHERE MICROSERVICE_ID = ?)
	`

	inUse, err := r.count(ctx, refs, id, id)
	if err != nil {
		return err
	}
	if inUse > 0 {
		return ErrMicroserviceInUse
	}

	result, err := r.db.ExecContext(ctx, `DELETE FROM CONFIG_MICROSERVICES WHERE ID = ?`, id)
	if err != nil {
		return fmt.Errorf("delete microservice: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read affected rows: %w", err)
	}
	if rowsAffected == 0 {
		return ErrMicroserviceNotFound
	}
	return nil
}

func (r *SQLiteRepository) ListEnvironments(ctx context.Context, page Page) ([]Environment, error) {
	const query = `
		SELECT ID, ENVIRONMENT, UPDATED_AT
		FROM CONFIG_ENVIRONMENTS
		ORDER BY ID
		LIMIT ? OFFSET ?
	`

	rows, err := r.db.QueryContext(ctx, query, page.Limit, page.Offset)
	if err != nil {
		return nil, fmt.Errorf("list environments: %w", err)
	}
	defer rows.Close()

	return scanEnvironments(rows)
}

func (r *SQLiteRepository) CreateEnvironment(ctx context.Context, name string) (*Environment, error) {
	taken, err := r.count(ctx, `SELECT COUNT(*) FROM CONFIG_ENVIRONMENTS WHERE ENVIRONMENT = ?`, name)
	if err != nil {
		return nil, err
	}
	if taken > 0 {
		return nil, ErrEnvironmentExists
	}

	const insert = `INSERT INTO CONFIG_ENVIRONMENTS (ENVIRONMENT, UPDATED_AT) VALUES (?, CURRENT_TIMESTAMP)`
	if _, err := r.db.ExecContext(ctx, insert, name); err != nil {
		if database.IsUniqueViolation(err) {
			return nil, ErrEnvironmentExists
		}
		return nil, fmt.Errorf("create environment: %w", err)
	}

	const query = `SELECT ID, ENVIRONMENT, UPDATED_AT FROM CONFIG_ENVIRONMENTS WHERE ENVIRONMENT = ?`

	var e Environment
	if err := r.db.QueryRowContext(ctx, query, name).Scan(&e.ID, &e.Environment, &e.UpdatedAt); err != nil {
		return nil, fmt.Errorf("get environment by name: %w", err)
	}
	return &e, nil
}

func (r *SQLiteRepository) DeleteEnvironment(ctx context.Context, id int64) error {
	const refs = `
		SELECT (SELECT COUNT(*) FROM CONFIG_FLAG_VALUE WHERE ENVIRONMENT_ID = ?)
		     + (SELECT COUNT(*) FROM CONFIG_MICROSERVICE_APPSETTINGS WHERE ENVIRONMENT_ID = ?)
		     + (SELECT COUNT(*) FROM CONFIG_LOCALIZATION WHERE ENVIRONMENT_ID = ?)
	`

	inUse, err := r.count(ctx, refs, id, id, id)
	if err != nil {
		return err
	}
	if inUse > 0 {
		return ErrEnvironmentInUse
	}

	result, err := r.db.ExecContext(ctx, `DELETE FROM CONFIG_ENVIRONMENTS WHERE ID = ?`, id)
	if err != nil {
		return fmt.Errorf("delete environment: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read affected rows: %w", err)
	}
	if rowsAffected == 0 {
		return ErrEnvironmentNotFound
	}
	return nil
}

func (r *SQLiteRepository) count(ctx context.Context, query string, args ...any) (int64, error) {
	var n int64
	if err := r.db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count rows: %w", err)
	}
	return n, nil
}

func (r *SQLiteRepository) ListAllForReconcile(ctx context.Context, since time.Time) ([]MicroserviceAppSettings, error) {
	const base = `
		SELECT ID, MICROSERVICE_ID, ENVIRONMENT_ID, SETTINGS_JSON, UPDATED_AT
		FROM CONFIG_MICROSERVICE_APPSETTINGS
	`

	query := base
	var args []any
	if !since.IsZero() {
		query = base + ` WHERE UPDATED_AT >= ?`
		args = append(args, since.UTC().Format(sqliteTimeLayout))
	}
	// A deterministic order matters when the sweep cannot publish every row:
	// which rows a stalled sweep got to has to be the same every cycle.
	query += ` ORDER BY ID`

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list microconfig for reconcile: %w", err)
	}
	defer rows.Close()

	var out []MicroserviceAppSettings
	for rows.Next() {
		var cfg MicroserviceAppSettings
		var settingsText string
		if err := rows.Scan(&cfg.ID, &cfg.MicroserviceID, &cfg.EnvironmentID, &settingsText, &cfg.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan microconfig reconcile row: %w", err)
		}
		cfg.SettingsJSON = json.RawMessage(settingsText)
		out = append(out, cfg)
	}
	return out, rows.Err()
}

var _ Repository = (*SQLiteRepository)(nil)
