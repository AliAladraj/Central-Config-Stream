package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ErasedKyte/Central-Config-Stream/internal/audit"
	"github.com/ErasedKyte/Central-Config-Stream/internal/database"
	"github.com/ErasedKyte/Central-Config-Stream/internal/flagsconfig"
	"github.com/ErasedKyte/Central-Config-Stream/internal/localization"
	"github.com/ErasedKyte/Central-Config-Stream/internal/messaging"
	"github.com/ErasedKyte/Central-Config-Stream/internal/microconfig"
	"github.com/ErasedKyte/Central-Config-Stream/internal/obs"
)

// appLog is the startup/lifecycle logger; request-scoped work uses the logger
// the access-log middleware puts in the context instead.
var appLog = obs.Logger("app")

// Server timeouts. ReadHeaderTimeout on its own bounds only the headers, so a
// client that sends one byte of body per minute — or never reads the response —
// can hold a connection forever; these close that off.
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 60 * time.Second
	idleTimeout       = 120 * time.Second
)

type App struct {
	db            *sql.DB
	server        *http.Server
	nats          *messaging.Client // nil when publishing is disabled
	stopReconcile func()

	// TLS material; empty means the admin API serves plain HTTP.
	tlsCertFile string
	tlsKeyFile  string
}

func NewApp(cfg *Config) (*App, error) {
	// Credentials are parsed before anything is opened: a malformed ADMIN_TOKENS
	// must stop the process, not silently leave writes less protected.
	tokens, err := parseAdminTokens(cfg.AdminTokens, cfg.AdminToken)
	if err != nil {
		return nil, fmt.Errorf("parse admin tokens: %w", err)
	}

	db, err := openDB(cfg)
	if err != nil {
		return nil, err
	}

	app := &App{db: db}

	// Publisher: real KV publisher when enabled, otherwise a no-op (dev, or any
	// deployment that serves the API without distributing). NATS + buckets first.
	var pub messaging.ConfigPublisher = messaging.NoopPublisher{}
	if cfg.PublishEnabled {
		client, buckets, err := setupMessaging(cfg)
		if err != nil {
			_ = db.Close()
			return nil, err
		}
		app.nats = client
		pub = messaging.NewPublisher(buckets)
	} else {
		appLog.Warn("running without KV distribution (dev mode)",
			slog.Bool("publish.enabled", false))
	}

	// Repositories (kept concrete so the reconciler can list all rows).
	flagsRepo, microRepo, locRepo := newRepositories(cfg, db)

	// Services + handlers.
	flagsHandler := flagsconfig.NewHandler(flagsconfig.NewService(flagsRepo, pub))
	microHandler := microconfig.NewHandler(microconfig.NewService(microRepo, pub))
	locHandler := localization.NewHandler(localization.NewService(locRepo, pub))

	warnIfAuthDisabled(tokens)
	warnIfTLSDisabled(cfg)

	sec := &security{
		tokens:  tokens,
		limiter: newRateLimiter(cfg.WriteRateLimit),
		db:      db,
		sqlite:  strings.EqualFold(cfg.DBDriver, "sqlite"),
	}

	auditStore := audit.NewStore(db, cfg.DBDriver)
	inventory := &inventoryHandler{flags: flagsRepo, micro: microRepo, loc: locRepo}

	router := NewRouter(microHandler, flagsHandler, locHandler, sec,
		app.healthHandler(), inventory, audit.NewHandler(auditStore))

	// The metric label for a request is the route it matched, which only the
	// mux knows; asking it keeps the label bounded by the routing table rather
	// than by whatever paths get probed.
	routeOf := func(r *http.Request) string {
		if _, pattern := router.Handler(r); pattern != "" {
			return pattern
		}
		return "other"
	}

	app.tlsCertFile, app.tlsKeyFile = cfg.TLSCertFile, cfg.TLSKeyFile
	app.server = &http.Server{
		Addr: cfg.ServerPort,
		// Auditing wraps the whole mux so that writes which never reach a
		// handler — unknown path, rejected token — are recorded too, and the
		// access log wraps that so every request is correlated and timed.
		Handler:           observe(routeOf, auditWrites(auditStore, router)),
		ErrorLog:          obs.StdLogger("http", slog.LevelWarn),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		TLSConfig:         tlsConfig(),
	}

	// Reconciler: republish Oracle -> KV on startup + interval to heal drift.
	if cfg.PublishEnabled {
		rec := messaging.NewReconciler(cfg.ReconcileInterval, pub,
			&flagsReconcileSource{repo: flagsRepo},
			&microReconcileSource{repo: microRepo},
			&locReconcileSource{repo: locRepo},
		)
		app.stopReconcile = rec.Start(context.Background())
	}

	return app, nil
}

// openDB dials the configured backend. Oracle is the production source of
// truth; sqlite exists only for the local test stack (deploy/compose).
func openDB(cfg *Config) (*sql.DB, error) {
	switch strings.ToLower(cfg.DBDriver) {
	case "", "oracle":
		db, err := database.NewOracleDB(cfg.DBConnString)
		if err != nil {
			return nil, fmt.Errorf("create oracle db: %w", err)
		}
		return db, nil
	case "sqlite":
		appLog.Warn("local test stack, Oracle is bypassed", slog.String("db.driver", "sqlite"))
		db, err := database.NewSQLiteDB(cfg.DBConnString)
		if err != nil {
			return nil, fmt.Errorf("create sqlite db: %w", err)
		}
		return db, nil
	default:
		return nil, fmt.Errorf("unknown DB_DRIVER %q (want oracle or sqlite)", cfg.DBDriver)
	}
}

// Repository shapes the app layer needs: the domain interface plus the
// list-for-reconcile method the reconciler drives.
type (
	flagsRepository interface {
		flagsconfig.Repository
		flagsLister
	}
	microRepository interface {
		microconfig.Repository
		microLister
	}
	locRepository interface {
		localization.Repository
		locLister
	}
)

func newRepositories(cfg *Config, db *sql.DB) (flagsRepository, microRepository, locRepository) {
	if strings.EqualFold(cfg.DBDriver, "sqlite") {
		return flagsconfig.NewSQLiteRepository(db),
			microconfig.NewSQLiteRepository(db),
			localization.NewSQLiteRepository(db)
	}
	return flagsconfig.NewOracleRepository(db),
		microconfig.NewOracleRepository(db),
		localization.NewOracleRepository(db)
}

func setupMessaging(cfg *Config) (*messaging.Client, *messaging.Buckets, error) {
	client, err := messaging.Connect(messaging.Config{
		URL:       cfg.NATSURL,
		CredsFile: cfg.NATSCreds,
		Name:      "central-config",
	})
	if err != nil {
		return nil, nil, fmt.Errorf("connect messaging: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	buckets, err := messaging.EnsureBuckets(ctx, client.JS, messaging.BucketOptions{
		Replicas: cfg.NATSReplicas,
	})
	if err != nil {
		_ = client.Drain()
		return nil, nil, fmt.Errorf("ensure buckets: %w", err)
	}
	return client, buckets, nil
}

// healthCheckTimeout bounds the database ping so a wedged connection pool
// cannot hang the health endpoint (and with it, the orchestrator's probe).
const healthCheckTimeout = 2 * time.Second

// healthHandler reports the real state of both dependencies. The database is
// actually pinged, and NATS is only counted against health when publishing is
// configured — a nil client means PUBLISH_ENABLED=false, which is a deliberate
// mode, not an outage.
func (a *App) healthHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), healthCheckTimeout)
		defer cancel()

		dbState := "ok"
		if err := a.db.PingContext(ctx); err != nil {
			dbState = "unavailable"
			obs.DBPingFailures.Inc()
			obs.FromContext(ctx, "health").Error("database ping failed", obs.Err(err))
		}
		obs.DBUp.SetBool(dbState == "ok")

		natsState := "disabled"
		switch {
		case a.nats == nil: // publishing disabled by configuration
		case a.nats.Conn.IsConnected():
			natsState = "connected"
		default:
			natsState = "disconnected"
		}

		healthy := dbState == "ok" && natsState != "disconnected"
		status := http.StatusOK
		if !healthy {
			status = http.StatusServiceUnavailable
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":   map[bool]string{true: "ok", false: "degraded"}[healthy],
			"database": dbState,
			"nats":     natsState,
		})
	}
}

// ---- Reconcile source adapters (bridge Oracle repos to messaging.ReconcileSource) ----

// The reconcile sources need the list-all method that the domain Repository
// interfaces do not expose, so each declares the narrow interface it needs.
// Any driver-specific repository (Oracle in production, SQLite in the local
// test stack) satisfies these.

type flagsLister interface {
	ListAllForReconcile(ctx context.Context, since time.Time) ([]flagsconfig.ReconcileRow, error)
}

type microLister interface {
	ListAllForReconcile(ctx context.Context, since time.Time) ([]microconfig.MicroserviceAppSettings, error)
}

type locLister interface {
	ListAllForReconcile(ctx context.Context, since time.Time) ([]localization.Localization, error)
}

// Each source also reports the bucket it owns and the keys it published, so the
// reconciler's full sweep can delete KV keys whose row is gone.

type flagsReconcileSource struct{ repo flagsLister }

func (s *flagsReconcileSource) Name() string   { return "flags" }
func (s *flagsReconcileSource) Bucket() string { return messaging.BucketFlags }
func (s *flagsReconcileSource) Resync(ctx context.Context, pub messaging.ConfigPublisher, since time.Time) ([]string, error) {
	rows, err := s.repo.ListAllForReconcile(ctx, since)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(rows))
	for _, row := range rows {
		if err := pub.PublishFlag(ctx, row.EnvironmentID, row.FlagKey, messaging.FlagPayload{
			Enabled: row.Enabled != 0,
			Value:   row.Value,
		}); err != nil {
			return keys, err
		}
		keys = append(keys, messaging.FlagKey(row.EnvironmentID, row.FlagKey))
	}
	return keys, nil
}

type microReconcileSource struct{ repo microLister }

func (s *microReconcileSource) Name() string   { return "microconfig" }
func (s *microReconcileSource) Bucket() string { return messaging.BucketMicroConfig }
func (s *microReconcileSource) Resync(ctx context.Context, pub messaging.ConfigPublisher, since time.Time) ([]string, error) {
	rows, err := s.repo.ListAllForReconcile(ctx, since)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(rows))
	for _, row := range rows {
		if err := pub.PublishMicroConfig(ctx, row.EnvironmentID, row.MicroserviceID, row.SettingsJSON); err != nil {
			return keys, err
		}
		keys = append(keys, messaging.MicroKey(row.EnvironmentID, row.MicroserviceID))
	}
	return keys, nil
}

type locReconcileSource struct {
	repo locLister
}

func (s *locReconcileSource) Name() string   { return "localization" }
func (s *locReconcileSource) Bucket() string { return messaging.BucketLocalization }
func (s *locReconcileSource) Resync(ctx context.Context, pub messaging.ConfigPublisher, since time.Time) ([]string, error) {
	rows, err := s.repo.ListAllForReconcile(ctx, since)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(rows))
	for _, row := range rows {
		if err := pub.PublishLocalization(ctx, row.EnvironmentID, row.MicroserviceID, row.Locale, row.BundleJSON); err != nil {
			return keys, err
		}
		keys = append(keys, messaging.LocalizationKey(row.EnvironmentID, row.MicroserviceID, row.Locale))
	}
	return keys, nil
}
