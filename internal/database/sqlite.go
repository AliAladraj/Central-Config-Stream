package database

import (
	"database/sql"
	"fmt"

	// Registers the pure-Go "sqlite" driver with database/sql. Imported for
	// its side effect only; nothing here calls into it directly, and without
	// it sql.Open("sqlite", …) fails at runtime rather than at compile time.
	_ "modernc.org/sqlite"
)

// NewSQLiteDB opens the local test-stack database. It is NOT a production
// path: it exists so the whole JetStream distribution flow (write-through,
// watch, reconcile) can be exercised without a Postgres instance. Postgres is
// the source of truth in a deployed setup; see DB_DRIVER in app config.
//
// dsn is a file path, or ":memory:" for an ephemeral database.
func NewSQLiteDB(dsn string) (*sql.DB, error) {
	if dsn == "" {
		dsn = "file:central-config.db?_pragma=busy_timeout(5000)"
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite db: %w", err)
	}

	// SQLite writers are serialized; a single connection avoids "database is
	// locked" under the reconciler + HTTP writes running concurrently.
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite db: %w", err)
	}

	if _, err := db.Exec(sqliteSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply sqlite schema: %w", err)
	}
	if _, err := db.Exec(sqliteSeed); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("seed sqlite db: %w", err)
	}

	return db, nil
}

// sqliteSchema mirrors the Postgres tables the repositories query. migrations/
// defines that schema; this is the SQLite translation of it. Column names match
// exactly so the SQL differs only in bind-parameter syntax, and the indexes are
// mirrored too so a query that is cheap here is cheap there.
//
// One difference the mirror cannot express: migrations/ declares UPDATED_AT as
// TIMESTAMPTZ, which stores an instant, while SQLite keeps CURRENT_TIMESTAMP as
// a naive UTC string that the repositories parse with a fixed layout. The
// column names and comparisons are the same on both sides; what differs is that
// the SQLite path can never catch a timezone mistake in the Postgres one.
const sqliteSchema = `
CREATE TABLE IF NOT EXISTS CONFIG_ENVIRONMENTS (
    ID          INTEGER PRIMARY KEY,
    ENVIRONMENT TEXT      NOT NULL UNIQUE,
    UPDATED_AT  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS CONFIG_MICROSERVICES (
    ID           INTEGER PRIMARY KEY,
    MICROSERVICE TEXT      NOT NULL UNIQUE,
    UPDATED_AT   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS CONFIG_FLAG (
    ID         INTEGER PRIMARY KEY,
    FLAG_KEY   TEXT      NOT NULL UNIQUE,
    IS_ACTIVE  INTEGER   NOT NULL DEFAULT 1,
    UPDATED_AT TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS CONFIG_FLAG_VALUE (
    ID             INTEGER PRIMARY KEY,
    ENVIRONMENT_ID INTEGER   NOT NULL REFERENCES CONFIG_ENVIRONMENTS (ID),
    FLAG_ID        INTEGER   NOT NULL REFERENCES CONFIG_FLAG (ID),
    ENABLED        INTEGER   NOT NULL DEFAULT 0,
    VALUE          TEXT      NOT NULL DEFAULT '',
    UPDATED_AT     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (ENVIRONMENT_ID, FLAG_ID)
);

CREATE TABLE IF NOT EXISTS CONFIG_MICROSERVICE_APPSETTINGS (
    ID              INTEGER PRIMARY KEY,
    MICROSERVICE_ID INTEGER   NOT NULL REFERENCES CONFIG_MICROSERVICES (ID),
    ENVIRONMENT_ID  INTEGER   NOT NULL REFERENCES CONFIG_ENVIRONMENTS (ID),
    SETTINGS_JSON   TEXT      NOT NULL,
    UPDATED_AT      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (MICROSERVICE_ID, ENVIRONMENT_ID)
);

CREATE TABLE IF NOT EXISTS CONFIG_LOCALIZATION (
    ID              INTEGER PRIMARY KEY,
    MICROSERVICE_ID INTEGER   NOT NULL REFERENCES CONFIG_MICROSERVICES (ID),
    ENVIRONMENT_ID  INTEGER   NOT NULL REFERENCES CONFIG_ENVIRONMENTS (ID),
    LOCALE          TEXT      NOT NULL,
    BUNDLE_JSON     TEXT      NOT NULL,
    UPDATED_AT      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (MICROSERVICE_ID, ENVIRONMENT_ID, LOCALE)
);

-- Mirrors migrations/008: the admin write audit trail. No foreign key on
-- ENVIRONMENT_ID — an audit row outlives the environment it refers to.
CREATE TABLE IF NOT EXISTS CONFIG_AUDIT_LOG (
    ID             INTEGER PRIMARY KEY,
    OCCURRED_AT    TIMESTAMP NOT NULL,
    ACTOR          TEXT,
    METHOD         TEXT      NOT NULL,
    PATH           TEXT      NOT NULL,
    TARGET_DOMAIN  TEXT,
    TARGET_ID      TEXT,
    ENVIRONMENT_ID INTEGER,
    STATUS_CODE    INTEGER   NOT NULL,
    REMOTE_ADDR    TEXT,
    REQUEST_BODY   TEXT
);

CREATE INDEX IF NOT EXISTS IX_FLAG_VALUE_FLAG        ON CONFIG_FLAG_VALUE (FLAG_ID);
CREATE INDEX IF NOT EXISTS IX_FLAG_VALUE_UPDATED_AT  ON CONFIG_FLAG_VALUE (UPDATED_AT);
CREATE INDEX IF NOT EXISTS IX_APPSETTINGS_ENVIRONMENT ON CONFIG_MICROSERVICE_APPSETTINGS (ENVIRONMENT_ID);
CREATE INDEX IF NOT EXISTS IX_APPSETTINGS_UPDATED_AT  ON CONFIG_MICROSERVICE_APPSETTINGS (UPDATED_AT);
CREATE INDEX IF NOT EXISTS IX_LOC_SERVICE_ENV_LOCALE ON CONFIG_LOCALIZATION (MICROSERVICE_ID, ENVIRONMENT_ID, LOCALE);
CREATE INDEX IF NOT EXISTS IX_LOC_ENVIRONMENT        ON CONFIG_LOCALIZATION (ENVIRONMENT_ID);
CREATE INDEX IF NOT EXISTS IX_LOC_UPDATED_AT         ON CONFIG_LOCALIZATION (UPDATED_AT);
CREATE INDEX IF NOT EXISTS IX_AUDIT_OCCURRED_AT      ON CONFIG_AUDIT_LOG (OCCURRED_AT);
CREATE INDEX IF NOT EXISTS IX_AUDIT_ACTOR_OCCURRED   ON CONFIG_AUDIT_LOG (ACTOR, OCCURRED_AT);
`

// sqliteSeed inserts a small fixed dataset. Idempotent (INSERT OR IGNORE with
// explicit IDs) so restarts keep whatever the operator edited through the API.
const sqliteSeed = `
INSERT OR IGNORE INTO CONFIG_ENVIRONMENTS (ID, ENVIRONMENT) VALUES
    (1, 'dev'), (2, 'staging'), (3, 'prod');

INSERT OR IGNORE INTO CONFIG_MICROSERVICES (ID, MICROSERVICE) VALUES
    (1, 'catalog-api'), (2, 'cart-api'), (3, 'storefront-api');

INSERT OR IGNORE INTO CONFIG_FLAG (ID, FLAG_KEY, IS_ACTIVE) VALUES
    (7, 'search_v2', 1),
    (8, 'dark_mode', 1),
    (9, 'new_pricing', 1);

INSERT OR IGNORE INTO CONFIG_FLAG_VALUE (ID, ENVIRONMENT_ID, FLAG_ID, ENABLED, VALUE) VALUES
    (100, 1, 7, 1, 'on'),
    (101, 1, 8, 0, 'off'),
    (102, 1, 9, 1, '0.25'),
    (103, 3, 7, 0, 'off');

INSERT OR IGNORE INTO CONFIG_MICROSERVICE_APPSETTINGS (ID, MICROSERVICE_ID, ENVIRONMENT_ID, SETTINGS_JSON) VALUES
    -- Row 200 is deliberately the widest shape the API has to survive: nested
    -- objects, an array, integers, a float and booleans. Secret-shaped fields
    -- carry "env:VAR_NAME" markers instead of values, because KV is readable by
    -- anyone holding NATS credentials — the secret itself stays in the
    -- deployment secret store and is resolved by the consumer at read time.
    (200, 1, 1, '{
      "service": { "displayName": "Catalog", "region": "eu-west", "shutdownGraceSeconds": 20 },
      "http": { "baseUrl": "https://api.example.com/v1", "timeoutMs": 2000, "maxRetries": 3, "verifyTls": true },
      "cache": { "enabled": true, "ttlSeconds": 300, "maxEntries": 10000 },
      "search": { "indexName": "catalog", "pageSize": 25, "facets": ["brand", "price", "rating"] },
      "storage": {
        "endpoint": "https://objects.example.com",
        "bucket": "catalog-media",
        "accessKeyId": "env:STORAGE_ACCESS_KEY_ID",
        "secretAccessKey": "env:STORAGE_SECRET_ACCESS_KEY"
      },
      "log": { "level": "debug", "sampleRate": 0.1 }
    }'),
    (201, 2, 1, '{"service":{"displayName":"Cart"},"http":{"timeoutMs":1500,"maxRetries":5},"cache":{"enabled":false}}'),
    (202, 3, 1, '{"service":{"displayName":"Storefront"},"http":{"timeoutMs":3000,"maxRetries":2},"log":{"level":"info"}}');

INSERT OR IGNORE INTO CONFIG_LOCALIZATION (ID, MICROSERVICE_ID, ENVIRONMENT_ID, LOCALE, BUNDLE_JSON) VALUES
    (300, 1, 1, 'en-US', '{"catalog.title":"Catalog","catalog.search":"Search"}'),
    (301, 1, 1, 'pt-BR', '{"catalog.title":"Catálogo","catalog.search":"Buscar"}');
`
