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

// sinceWindowUTC converts the bound reconcile window into the session time zone
// before comparing it. UPDATED_AT is a plain TIMESTAMP written by
// CURRENT_TIMESTAMP, so it carries the session's local wall clock, while the
// bound value comes from a Go UTC clock — on a non-UTC session comparing them
// directly matches the wrong rows, or none at all, and reports success either
// way. Converting the bound value rather than the column leaves UPDATED_AT bare
// on the left-hand side, so its index still serves the window.
const sinceWindowUTC = `CAST(FROM_TZ(CAST(:1 AS TIMESTAMP), 'UTC') AT TIME ZONE SESSIONTIMEZONE AS TIMESTAMP)`

type Repository interface {
	GetMicroserviceConfigByID(ctx context.Context, id int64) (*MicroserviceAppSettings, error)
	UpdateMicroserviceConfig(ctx context.Context, input MicroserviceAppSettings) (*MicroserviceAppSettings, error)
	GetMicroserviceByID(ctx context.Context, id int64) (*Microservice, error)

	ListMicroserviceConfigs(ctx context.Context, filter AppSettingsFilter) ([]MicroserviceAppSettings, error)
	CreateMicroserviceConfig(ctx context.Context, input MicroserviceAppSettings) (*MicroserviceAppSettings, error)
	// DeleteMicroserviceConfig returns the row it removed, so the caller can
	// derive the KV key it has to purge.
	DeleteMicroserviceConfig(ctx context.Context, id int64) (*MicroserviceAppSettings, error)

	ListMicroservices(ctx context.Context, page Page) ([]Microservice, error)
	CreateMicroservice(ctx context.Context, name string) (*Microservice, error)
	DeleteMicroservice(ctx context.Context, id int64) error

	ListEnvironments(ctx context.Context, page Page) ([]Environment, error)
	CreateEnvironment(ctx context.Context, name string) (*Environment, error)
	DeleteEnvironment(ctx context.Context, id int64) error
}

type OracleRepository struct {
	db *sql.DB
}

func NewOracleRepository(db *sql.DB) *OracleRepository {
	return &OracleRepository{db: db}
}

func (r *OracleRepository) GetMicroserviceConfigByID(ctx context.Context, id int64) (*MicroserviceAppSettings, error) {
	const query = `
		SELECT ID, MICROSERVICE_ID, ENVIRONMENT_ID, SETTINGS_JSON, UPDATED_AT
		FROM CONFIG_MICROSERVICE_APPSETTINGS
		WHERE ID = :1
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

func (r *OracleRepository) UpdateMicroserviceConfig(ctx context.Context, input MicroserviceAppSettings) (*MicroserviceAppSettings, error) {
	// An update rewrites the natural key, so it needs the same referential and
	// uniqueness checks a create gets: without them it can point at rows that
	// do not exist, or collide with another row and surface as a 500.
	if err := r.checkAppSettingsRefs(ctx, input); err != nil {
		return nil, err
	}

	const query = `
		UPDATE CONFIG_MICROSERVICE_APPSETTINGS
		SET MICROSERVICE_ID = :1,
		    ENVIRONMENT_ID = :2,
		    SETTINGS_JSON = :3,
		    UPDATED_AT = CURRENT_TIMESTAMP
		WHERE ID = :4
	`

	// Optimistic concurrency is opt-in: only when the caller told us which
	// version it read does the UPDATE become conditional.
	stmt := query
	args := []any{input.MicroserviceID, input.EnvironmentID, database.CLOB(string(input.SettingsJSON)), input.ID}
	if input.ExpectedUpdatedAt != nil {
		stmt += ` AND UPDATED_AT = :5`
		args = append(args, *input.ExpectedUpdatedAt)
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
func (r *OracleRepository) noRowsErr(ctx context.Context, input MicroserviceAppSettings) error {
	if input.ExpectedUpdatedAt == nil {
		return ErrConfigNotFound
	}
	if _, err := r.GetMicroserviceConfigByID(ctx, input.ID); err != nil {
		return err // ErrConfigNotFound, or a real read failure
	}
	return ErrConflict
}

// ListAllForReconcile returns appsettings rows to republish to KV. A zero
// `since` sweeps every row; otherwise only rows changed at or after `since`.
func (r *OracleRepository) ListAllForReconcile(ctx context.Context, since time.Time) ([]MicroserviceAppSettings, error) {
	const base = `
		SELECT ID, MICROSERVICE_ID, ENVIRONMENT_ID, SETTINGS_JSON, UPDATED_AT
		FROM CONFIG_MICROSERVICE_APPSETTINGS
	`

	query := base
	var args []any
	if !since.IsZero() {
		query = base + ` WHERE UPDATED_AT >= ` + sinceWindowUTC
		args = append(args, since.UTC())
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

func (r *OracleRepository) GetMicroserviceByID(ctx context.Context, id int64) (*Microservice, error) {
	const query = `
		SELECT ID, MICROSERVICE, UPDATED_AT
		FROM CONFIG_MICROSERVICES
		WHERE ID = :1
	`

	var m Microservice
	err := r.db.QueryRowContext(ctx, query, id).Scan(
		&m.ID,
		&m.Microservice,
		&m.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrMicroserviceNotFound
		}
		return nil, fmt.Errorf("get microservice by id: %w", err)
	}

	return &m, nil
}

func (r *OracleRepository) ListMicroserviceConfigs(ctx context.Context, filter AppSettingsFilter) ([]MicroserviceAppSettings, error) {
	query := `
		SELECT ID, MICROSERVICE_ID, ENVIRONMENT_ID, SETTINGS_JSON, UPDATED_AT
		FROM CONFIG_MICROSERVICE_APPSETTINGS
	`

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
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, " AND ")
	}
	query += fmt.Sprintf(` ORDER BY ID OFFSET :%d ROWS FETCH NEXT :%d ROWS ONLY`, len(args)+1, len(args)+2)
	args = append(args, filter.Offset, filter.Limit)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list microconfig: %w", err)
	}
	defer rows.Close()

	return scanAppSettings(rows)
}

func (r *OracleRepository) CreateMicroserviceConfig(ctx context.Context, input MicroserviceAppSettings) (*MicroserviceAppSettings, error) {
	if err := r.checkAppSettingsRefs(ctx, input); err != nil {
		return nil, err
	}

	const insert = `
		INSERT INTO CONFIG_MICROSERVICE_APPSETTINGS (MICROSERVICE_ID, ENVIRONMENT_ID, SETTINGS_JSON, UPDATED_AT)
		VALUES (:1, :2, :3, CURRENT_TIMESTAMP)
	`
	if _, err := r.db.ExecContext(ctx, insert,
		input.MicroserviceID, input.EnvironmentID, database.CLOB(string(input.SettingsJSON))); err != nil {
		// The check above is a fast path, not a lock: two concurrent creates
		// both pass it and the constraint decides between them.
		if database.IsUniqueViolation(err) {
			return nil, ErrConfigExists
		}
		return nil, fmt.Errorf("create config: %w", err)
	}

	// Read back by the natural key: the generated id never crosses the driver
	// boundary, so no RETURNING clause is needed.
	return r.getConfigByServiceAndEnv(ctx, input.MicroserviceID, input.EnvironmentID)
}

// checkAppSettingsRefs enforces referential integrity in the application rather
// than relying on the foreign keys: a constraint violation arrives as an opaque
// driver error, and the caller needs to tell "no such microservice" apart from
// "already exists" to pick a status code.
func (r *OracleRepository) checkAppSettingsRefs(ctx context.Context, input MicroserviceAppSettings) error {
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

	// An update may legitimately keep the natural key it already holds, so the
	// row being written is excluded; on a create input.ID is zero and excludes
	// nothing.
	taken, err := r.count(ctx,
		`SELECT COUNT(*) FROM CONFIG_MICROSERVICE_APPSETTINGS
		 WHERE MICROSERVICE_ID = :1 AND ENVIRONMENT_ID = :2 AND ID <> :3`,
		input.MicroserviceID, input.EnvironmentID, input.ID)
	if err != nil {
		return err
	}
	if taken > 0 {
		return ErrConfigExists
	}
	return nil
}

func (r *OracleRepository) getConfigByServiceAndEnv(ctx context.Context, microserviceID, environmentID int64) (*MicroserviceAppSettings, error) {
	const query = `
		SELECT ID, MICROSERVICE_ID, ENVIRONMENT_ID, SETTINGS_JSON, UPDATED_AT
		FROM CONFIG_MICROSERVICE_APPSETTINGS
		WHERE MICROSERVICE_ID = :1 AND ENVIRONMENT_ID = :2
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

func (r *OracleRepository) DeleteMicroserviceConfig(ctx context.Context, id int64) (*MicroserviceAppSettings, error) {
	// The KV key is read before the row goes away — afterwards there is nothing
	// left to derive it from.
	removed, err := r.GetMicroserviceConfigByID(ctx, id)
	if err != nil {
		return nil, err
	}

	result, err := r.db.ExecContext(ctx, `DELETE FROM CONFIG_MICROSERVICE_APPSETTINGS WHERE ID = :1`, id)
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

func (r *OracleRepository) ListMicroservices(ctx context.Context, page Page) ([]Microservice, error) {
	const query = `
		SELECT ID, MICROSERVICE, UPDATED_AT
		FROM CONFIG_MICROSERVICES
		ORDER BY ID
		OFFSET :1 ROWS FETCH NEXT :2 ROWS ONLY
	`

	rows, err := r.db.QueryContext(ctx, query, page.Offset, page.Limit)
	if err != nil {
		return nil, fmt.Errorf("list microservices: %w", err)
	}
	defer rows.Close()

	return scanMicroservices(rows)
}

func (r *OracleRepository) CreateMicroservice(ctx context.Context, name string) (*Microservice, error) {
	taken, err := r.count(ctx, `SELECT COUNT(*) FROM CONFIG_MICROSERVICES WHERE MICROSERVICE = :1`, name)
	if err != nil {
		return nil, err
	}
	if taken > 0 {
		return nil, ErrMicroserviceExists
	}

	const insert = `INSERT INTO CONFIG_MICROSERVICES (MICROSERVICE, UPDATED_AT) VALUES (:1, CURRENT_TIMESTAMP)`
	if _, err := r.db.ExecContext(ctx, insert, name); err != nil {
		if database.IsUniqueViolation(err) {
			return nil, ErrMicroserviceExists
		}
		return nil, fmt.Errorf("create microservice: %w", err)
	}

	const query = `SELECT ID, MICROSERVICE, UPDATED_AT FROM CONFIG_MICROSERVICES WHERE MICROSERVICE = :1`

	var m Microservice
	if err := r.db.QueryRowContext(ctx, query, name).Scan(&m.ID, &m.Microservice, &m.UpdatedAt); err != nil {
		return nil, fmt.Errorf("get microservice by name: %w", err)
	}
	return &m, nil
}

// DeleteMicroservice refuses to orphan the rows that point at it. Cascading
// would silently delete that service's appsettings and translations in every
// environment.
func (r *OracleRepository) DeleteMicroservice(ctx context.Context, id int64) error {
	const refs = `
		SELECT (SELECT COUNT(*) FROM CONFIG_MICROSERVICE_APPSETTINGS WHERE MICROSERVICE_ID = :1)
		     + (SELECT COUNT(*) FROM CONFIG_LOCALIZATION WHERE MICROSERVICE_ID = :2)
		FROM DUAL
	`

	inUse, err := r.count(ctx, refs, id, id)
	if err != nil {
		return err
	}
	if inUse > 0 {
		return ErrMicroserviceInUse
	}

	result, err := r.db.ExecContext(ctx, `DELETE FROM CONFIG_MICROSERVICES WHERE ID = :1`, id)
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

func (r *OracleRepository) ListEnvironments(ctx context.Context, page Page) ([]Environment, error) {
	const query = `
		SELECT ID, ENVIRONMENT, UPDATED_AT
		FROM CONFIG_ENVIRONMENTS
		ORDER BY ID
		OFFSET :1 ROWS FETCH NEXT :2 ROWS ONLY
	`

	rows, err := r.db.QueryContext(ctx, query, page.Offset, page.Limit)
	if err != nil {
		return nil, fmt.Errorf("list environments: %w", err)
	}
	defer rows.Close()

	return scanEnvironments(rows)
}

func (r *OracleRepository) CreateEnvironment(ctx context.Context, name string) (*Environment, error) {
	taken, err := r.count(ctx, `SELECT COUNT(*) FROM CONFIG_ENVIRONMENTS WHERE ENVIRONMENT = :1`, name)
	if err != nil {
		return nil, err
	}
	if taken > 0 {
		return nil, ErrEnvironmentExists
	}

	const insert = `INSERT INTO CONFIG_ENVIRONMENTS (ENVIRONMENT, UPDATED_AT) VALUES (:1, CURRENT_TIMESTAMP)`
	if _, err := r.db.ExecContext(ctx, insert, name); err != nil {
		if database.IsUniqueViolation(err) {
			return nil, ErrEnvironmentExists
		}
		return nil, fmt.Errorf("create environment: %w", err)
	}

	const query = `SELECT ID, ENVIRONMENT, UPDATED_AT FROM CONFIG_ENVIRONMENTS WHERE ENVIRONMENT = :1`

	var e Environment
	if err := r.db.QueryRowContext(ctx, query, name).Scan(&e.ID, &e.Environment, &e.UpdatedAt); err != nil {
		return nil, fmt.Errorf("get environment by name: %w", err)
	}
	return &e, nil
}

// DeleteEnvironment refuses to orphan the rows that point at it. An environment
// is the widest blast radius in the schema — cascading it would wipe every
// flag value, appsettings row and bundle for a whole stage.
func (r *OracleRepository) DeleteEnvironment(ctx context.Context, id int64) error {
	const refs = `
		SELECT (SELECT COUNT(*) FROM CONFIG_FLAG_VALUE WHERE ENVIRONMENT_ID = :1)
		     + (SELECT COUNT(*) FROM CONFIG_MICROSERVICE_APPSETTINGS WHERE ENVIRONMENT_ID = :2)
		     + (SELECT COUNT(*) FROM CONFIG_LOCALIZATION WHERE ENVIRONMENT_ID = :3)
		FROM DUAL
	`

	inUse, err := r.count(ctx, refs, id, id, id)
	if err != nil {
		return err
	}
	if inUse > 0 {
		return ErrEnvironmentInUse
	}

	result, err := r.db.ExecContext(ctx, `DELETE FROM CONFIG_ENVIRONMENTS WHERE ID = :1`, id)
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

func (r *OracleRepository) count(ctx context.Context, query string, args ...any) (int64, error) {
	var n int64
	if err := r.db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count rows: %w", err)
	}
	return n, nil
}

// ---- Scan helpers shared by both drivers (the SQL differs, the rows do not) ----

func scanAppSettings(rows *sql.Rows) ([]MicroserviceAppSettings, error) {
	var out []MicroserviceAppSettings
	for rows.Next() {
		var cfg MicroserviceAppSettings
		var settingsText string
		if err := rows.Scan(&cfg.ID, &cfg.MicroserviceID, &cfg.EnvironmentID, &settingsText, &cfg.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan microconfig row: %w", err)
		}
		cfg.SettingsJSON = json.RawMessage(settingsText)
		out = append(out, cfg)
	}
	return out, rows.Err()
}

func scanMicroservices(rows *sql.Rows) ([]Microservice, error) {
	var out []Microservice
	for rows.Next() {
		var m Microservice
		if err := rows.Scan(&m.ID, &m.Microservice, &m.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan microservice row: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func scanEnvironments(rows *sql.Rows) ([]Environment, error) {
	var out []Environment
	for rows.Next() {
		var e Environment
		if err := rows.Scan(&e.ID, &e.Environment, &e.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan environment row: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
