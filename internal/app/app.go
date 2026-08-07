// Package app is the assembly point: it reads configuration from the
// environment, picks the Postgres or SQLite repositories, builds the domain
// services and HTTP handlers, registers the routes behind the auth, token
// scope, rate limit and audit middleware, and owns the server's lifecycle.
//
// Most of what lives here is wiring rather than logic, and the wiring is the
// hazard — several of the connections it makes fail at runtime rather than at
// compile time. A domain that is not registered with the reconciler still
// serves writes; a route class not handled in middleware.go still answers.
// CONTRIBUTING.md enumerates those sites under "Adding a config domain".
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

	"github.com/AliAladraj/Central-Config-Stream/internal/audit"
	"github.com/AliAladraj/Central-Config-Stream/internal/database"
	"github.com/AliAladraj/Central-Config-Stream/internal/flagsconfig"
	"github.com/AliAladraj/Central-Config-Stream/internal/localization"
	"github.com/AliAladraj/Central-Config-Stream/internal/messaging"
	"github.com/AliAladraj/Central-Config-Stream/internal/obs"
	"github.com/AliAladraj/Central-Config-Stream/internal/servicesettings"
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
	microHandler := servicesettings.NewHandler(servicesettings.NewService(microRepo, pub))
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
	// than by whatever paths get probed. The same answer tells the audit
	// middleware whether a write is addressed at anything at all.
	routeOf := func(r *http.Request) string {
		if _, pattern := router.Handler(r); pattern != "" {
			return pattern
		}
		return "other"
	}
	routed := func(r *http.Request) bool {
		_, pattern := router.Handler(r)
		return pattern != ""
	}

	app.tlsCertFile, app.tlsKeyFile = cfg.TLSCertFile, cfg.TLSKeyFile
	app.server = &http.Server{
		Addr: cfg.ServerPort,
		// Outside in: the headers that belong on every response, then the
		// access log and panic recovery so nothing below can drop a request
		// unobserved, then the address-keyed write limiter — ahead of the
		// audit insert, which is itself a database write an unauthenticated
		// caller must not be able to aim — then auditing, which records every
		// write that reaches a real route including one the guard rejects.
		Handler: securityHeaders(observe(routeOf,
			sec.limitWrites(auditWrites(auditStore, routed, router)))),
		ErrorLog:          obs.StdLogger("http", slog.LevelWarn),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		TLSConfig:         tlsConfig(),
	}

	// Reconciler: republish the database -> KV on startup + interval to heal drift.
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

// Handler is the middleware chain and router exactly as the server serves
// them. It exists for internal/pgintegration: this package's own end-to-end
// tests all run on SQLite, and the behaviour that differs between the two — the
// scope lookup's bind syntax, the audit columns' declared widths — is therefore
// reachable only from a test outside this package, which cannot build the
// unexported security type NewRouter takes. Nothing in the binary calls it.
func (a *App) Handler() http.Handler { return a.server.Handler }

// Close releases what NewApp opened, for a caller that never reached Run — a
// test, or a startup that failed after the app was built. Run does its own
// ordered shutdown instead, because there the listener has to stop and the
// reconciler has to be told before the database can go.
func (a *App) Close() error {
	if a.stopReconcile != nil {
		a.stopReconcile()
	}
	if a.nats != nil {
		if err := a.nats.Drain(); err != nil {
			appLog.Error("nats drain failed", obs.Err(err))
		}
	}
	return a.db.Close()
}

// openDB dials the configured backend. PostgreSQL is the production source of
// truth; sqlite exists only for the local test stack (deploy/compose).
func openDB(cfg *Config) (*sql.DB, error) {
	switch strings.ToLower(cfg.DBDriver) {
	case "", "postgres":
		db, err := database.NewPostgresDBWithPool(cfg.DBConnString, database.PoolOptions{
			MaxOpenConns:    cfg.DBMaxOpenConns,
			MaxIdleConns:    cfg.DBMaxIdleConns,
			ConnMaxLifetime: cfg.DBConnMaxLifetime,
			ConnMaxIdleTime: cfg.DBConnMaxIdleTime,
		})
		if err != nil {
			return nil, fmt.Errorf("create postgres db: %w", err)
		}
		return db, nil
	case "sqlite":
		appLog.Warn("local test stack, PostgreSQL is bypassed", slog.String("db.driver", "sqlite"))
		db, err := database.NewSQLiteDB(cfg.DBConnString)
		if err != nil {
			return nil, fmt.Errorf("create sqlite db: %w", err)
		}
		return db, nil
	default:
		return nil, fmt.Errorf("unknown DB_DRIVER %q (want postgres or sqlite)", cfg.DBDriver)
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
		servicesettings.Repository
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
			servicesettings.NewSQLiteRepository(db),
			localization.NewSQLiteRepository(db)
	}
	return flagsconfig.NewPostgresRepository(db),
		servicesettings.NewPostgresRepository(db),
		localization.NewPostgresRepository(db)
}

// setupMessaging connects to NATS and provisions the KV buckets. A URL that
// cannot be parsed is a configuration error and stops the process; NATS being
// unreachable is not. The read paths of the admin API need nothing from KV, and
// turning a flag off is exactly what an operator reaches for while the
// distribution plane is down — crash-looping every pod removes that. The
// buckets come up on their own: the reconciler re-ensures them every cycle.
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

	buckets := messaging.NewBuckets(client.JS, messaging.BucketOptions{
		Replicas: cfg.NATSReplicas,
	})
	if err := buckets.Ensure(ctx); err != nil {
		obs.NATSUp.SetBool(false)
		appLog.Error("KV buckets unavailable, starting without distribution",
			slog.String("nats.url", cfg.NATSURL), obs.Err(err))
		return client, buckets, nil
	}

	obs.NATSUp.SetBool(true)
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
		if a.nats != nil {
			obs.NATSUp.SetBool(natsState == "connected")
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

// livezHandler answers only "this process is running". Liveness must not depend
// on the database or NATS: an outage in either is a reason to stop taking
// traffic, which is what /health is for, not a reason for the orchestrator to
// kill every pod — that takes the admin API's read paths down as well, exactly
// when an operator needs them to see and change configuration.
func livezHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"alive"}` + "\n"))
}

// ---- Reconcile source adapters (bridge the repos to messaging.ReconcileSource) ----

// The reconcile sources need the list-all method that the domain Repository
// interfaces do not expose, so each declares the narrow interface it needs.
// Any driver-specific repository (Postgres in production, SQLite in the local
// test stack) satisfies these.

type flagsLister interface {
	ListAllForReconcile(ctx context.Context, since time.Time) ([]flagsconfig.ReconcileRow, error)
}

type microLister interface {
	ListAllForReconcile(ctx context.Context, since time.Time) ([]servicesettings.MicroserviceAppSettings, error)
}

type locLister interface {
	ListAllForReconcile(ctx context.Context, since time.Time) ([]localization.Localization, error)
}

// Each source also reports the bucket it owns and the keys it published, so the
// reconciler's full sweep can delete KV keys whose row is gone. A row that will
// not publish is recorded on the result and the sweep carries on: returning
// early would leave every row behind it unpublished, and — because the sweep
// then neither prunes nor advances its window — would do so again on every
// cycle from then on.

type flagsReconcileSource struct{ repo flagsLister }

func (s *flagsReconcileSource) Name() string   { return "flags" }
func (s *flagsReconcileSource) Bucket() string { return messaging.BucketFlags }
func (s *flagsReconcileSource) Resync(ctx context.Context, pub messaging.ConfigPublisher, since time.Time) (messaging.ResyncResult, error) {
	rows, err := s.repo.ListAllForReconcile(ctx, since)
	if err != nil {
		return messaging.ResyncResult{}, err
	}
	res := messaging.ResyncResult{Keys: make([]string, 0, len(rows))}
	for _, row := range rows {
		res.Publish(s.Name(), messaging.FlagKey(row.EnvironmentID, row.FlagKey),
			pub.PublishFlag(ctx, row.EnvironmentID, row.FlagKey, messaging.FlagPayload{
				Enabled: row.Enabled != 0,
				Value:   row.Value,
			}))
	}
	return res, nil
}

type microReconcileSource struct{ repo microLister }

func (s *microReconcileSource) Name() string   { return "servicesettings" }
func (s *microReconcileSource) Bucket() string { return messaging.BucketServiceSettings }
func (s *microReconcileSource) Resync(ctx context.Context, pub messaging.ConfigPublisher, since time.Time) (messaging.ResyncResult, error) {
	rows, err := s.repo.ListAllForReconcile(ctx, since)
	if err != nil {
		return messaging.ResyncResult{}, err
	}
	res := messaging.ResyncResult{Keys: make([]string, 0, len(rows))}
	for _, row := range rows {
		res.Publish(s.Name(), messaging.ServiceSettingsKey(row.EnvironmentID, row.MicroserviceID),
			pub.PublishServiceSettings(ctx, row.EnvironmentID, row.MicroserviceID, row.SettingsJSON))
	}
	return res, nil
}

type locReconcileSource struct {
	repo locLister
}

func (s *locReconcileSource) Name() string   { return "localization" }
func (s *locReconcileSource) Bucket() string { return messaging.BucketLocalization }
func (s *locReconcileSource) Resync(ctx context.Context, pub messaging.ConfigPublisher, since time.Time) (messaging.ResyncResult, error) {
	rows, err := s.repo.ListAllForReconcile(ctx, since)
	if err != nil {
		return messaging.ResyncResult{}, err
	}
	res := messaging.ResyncResult{Keys: make([]string, 0, len(rows))}
	for _, row := range rows {
		res.Publish(s.Name(), messaging.LocalizationKey(row.EnvironmentID, row.MicroserviceID, row.Locale),
			pub.PublishLocalization(ctx, row.EnvironmentID, row.MicroserviceID, row.Locale, row.BundleJSON))
	}
	return res, nil
}
