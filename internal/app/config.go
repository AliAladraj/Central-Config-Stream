package app

import (
	"context"
	"errors"
	flagsconfig "github.com/ErasedKyte/Central-Config-Stream/internal/flagsconfig"
	"github.com/ErasedKyte/Central-Config-Stream/internal/localization"
	"github.com/ErasedKyte/Central-Config-Stream/internal/microconfig"
	"github.com/ErasedKyte/Central-Config-Stream/internal/obs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

type Config struct {
	// DBDriver selects the storage backend: "oracle" (production, the source of
	// truth) or "sqlite" (local test stack only — see internal/database/sqlite.go).
	DBDriver     string
	DBConnString string
	ServerPort   string

	// Connection pool bounds. A zero field takes the database package's own
	// default; they are read here so every tunable an operator sets is visible
	// in one place rather than in whichever package happens to consume it.
	DBMaxOpenConns    int
	DBMaxIdleConns    int
	DBConnMaxLifetime time.Duration
	DBConnMaxIdleTime time.Duration

	// Messaging / JetStream
	NATSURL        string
	NATSCreds      string // optional path to a .creds file
	PublishEnabled bool
	NATSReplicas   int // KV bucket replicas: 1 dev, 3 prod cluster

	// Reconciler
	ReconcileInterval time.Duration

	// Security
	//
	// AdminTokens is the named-token configuration (see parseAdminTokens):
	//   ADMIN_TOKENS=alice:*:s3cr3t,ci-dev:1|2:another-secret
	// AdminToken is the single shared token form, for a deployment that does
	// not need per-environment scopes; it is recorded as the "shared"
	// full-scope actor.
	AdminTokens string
	AdminToken  string

	// WriteRateLimit bounds writes per caller per minute; <= 0 disables it.
	WriteRateLimit int

	// TLS: when both are set the admin API serves HTTPS, otherwise plain HTTP
	// with a startup warning.
	TLSCertFile string
	TLSKeyFile  string

	// Observability. LogFormat "json" emits ECS documents for Elasticsearch;
	// "text" is the readable local-development shape.
	LogLevel       string
	LogFormat      string
	ServiceName    string
	ServiceVersion string
}

func LoadConfig() *Config {
	return &Config{
		DBDriver:          getEnv("DB_DRIVER", "oracle"),
		DBConnString:      getEnv("CONN_STRING", ""),
		ServerPort:        getEnv("PORT", ":8080"),
		DBMaxOpenConns:    getEnvInt("DB_MAX_OPEN_CONNS", 0),
		DBMaxIdleConns:    getEnvInt("DB_MAX_IDLE_CONNS", 0),
		DBConnMaxLifetime: getEnvDuration("DB_CONN_MAX_LIFETIME", 0),
		DBConnMaxIdleTime: getEnvDuration("DB_CONN_MAX_IDLE_TIME", 0),
		NATSURL:           getEnv("NATS_URL", ""),
		NATSCreds:         getEnv("NATS_CREDS", ""),
		PublishEnabled:    getEnvBool("PUBLISH_ENABLED", false),
		NATSReplicas:      getEnvInt("NATS_REPLICAS", 1),
		ReconcileInterval: getEnvDuration("RECONCILE_INTERVAL", 5*time.Minute),
		AdminTokens:       getEnv("ADMIN_TOKENS", ""),
		AdminToken:        getEnv("ADMIN_TOKEN", ""),
		WriteRateLimit:    getEnvInt("WRITE_RATE_LIMIT_PER_MINUTE", defaultWriteRateLimit),
		TLSCertFile:       getEnv("TLS_CERT_FILE", ""),
		TLSKeyFile:        getEnv("TLS_KEY_FILE", ""),
		LogLevel:          getEnv("LOG_LEVEL", "info"),
		LogFormat:         getEnv("LOG_FORMAT", "json"),
		ServiceName:       getEnv("SERVICE_NAME", "central-config"),
		ServiceVersion:    getEnv("SERVICE_VERSION", "dev"),
	}
}

// LogOptions is the logging configuration in the form the obs package wants.
func (c *Config) LogOptions() obs.LogOptions {
	return obs.LogOptions{
		Level:   c.LogLevel,
		Format:  c.LogFormat,
		Service: c.ServiceName,
		Version: c.ServiceVersion,
	}
}

// defaultWriteRateLimit is per caller per minute. Admin writes are typed by
// humans or applied by a deploy script in batches; a couple per second is far
// above real use and far below what it takes to hurt Oracle or KV.
const defaultWriteRateLimit = 120

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func getEnvBool(key string, fallback bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func getEnvDuration(key string, fallback time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

// NewRouter registers the API. Every write goes through sec.guard, which
// declares how that route's target environment is found so a token can only
// reach the environments its scope names; every read goes through sec.read,
// which declares how that route's response is narrowed to the same scope. The
// probe and scrape routes are the only open ones.
func NewRouter(
	h *microconfig.Handler,
	fh *flagsconfig.Handler,
	lh *localization.Handler,
	sec *security,
	health http.HandlerFunc,
	inventory http.Handler,
	auditLog http.Handler,
) *http.ServeMux {
	mux := http.NewServeMux()

	// Readiness answers for the dependencies; liveness answers only for the
	// process, so that an outage in either takes the pod out of the load
	// balancer rather than getting every replica killed and restarted.
	mux.HandleFunc("GET /health", health)
	mux.HandleFunc("GET /livez", livezHandler)

	// Prometheus scrape target. Open like the probes: it carries counters, not
	// configuration values.
	mux.Handle("GET /metrics", obs.Handler())

	// every editable row + its id, so admin tools do not have to guess ids
	mux.HandleFunc("GET /inventory", sec.read(classReadInventory, inventory))

	mux.HandleFunc("GET /audit", sec.read(classReadAudit, auditLog))

	// reference tables every other domain keys off
	mux.HandleFunc("GET /environments", sec.read(classReadEnvs, http.HandlerFunc(h.ListEnvironments)))
	// Adding an environment changes the set every other scope is expressed in.
	mux.HandleFunc("POST /environments", sec.guard(classGlobal, "environments", h.CreateEnvironment))
	// Here {id} is itself the environment id.
	mux.HandleFunc("DELETE /environments/{id}", sec.guard(classEnvPath, "environments", h.DeleteEnvironment))

	mux.HandleFunc("GET /microservices", sec.read(classReadNeutral, http.HandlerFunc(h.ListMicroservices)))
	mux.HandleFunc("POST /microservices", sec.guard(classNeutral, "microservices", h.CreateMicroservice))
	mux.HandleFunc("DELETE /microservices/{id}", sec.guard(classGlobal, "microservices", h.DeleteMicroservice))

	// microservice appsettings. GET /configs/{id} is the microservice
	// definition, which belongs to no environment; the values below do.
	mux.HandleFunc("GET /configs/{id}", sec.read(classReadNeutral, http.HandlerFunc(h.GetMicroservice)))
	mux.HandleFunc("GET /configs/values", sec.read(classReadEnvRows, http.HandlerFunc(h.ListMicroserviceConfigs)))
	mux.HandleFunc("GET /configs/values/{id}", sec.read(classReadEnvRows, http.HandlerFunc(h.GetMicroserviceConfig)))
	mux.HandleFunc("POST /configs/values", sec.guard(classEnvBody, "microconfig", h.CreateMicroserviceConfig))
	mux.HandleFunc("DELETE /configs/values/{id}", sec.guard(classRowMicroConfig, "microconfig", h.DeleteMicroserviceConfig))
	mux.HandleFunc("PUT /configs/values", sec.guard(classRowMicroConfig, "microconfig", h.UpdateMicroserviceConfig))

	// feature flags. A flag definition carries no environment, but deleting one
	// removes its values in every environment.
	mux.HandleFunc("GET /flags", sec.read(classReadNeutral, http.HandlerFunc(fh.ListFlags)))
	mux.HandleFunc("GET /flags/{id}", sec.read(classReadNeutral, http.HandlerFunc(fh.GetFlagsByID)))
	mux.HandleFunc("POST /flags", sec.guard(classNeutral, "flags", fh.CreateFlag))
	mux.HandleFunc("DELETE /flags/{id}", sec.guard(classGlobal, "flags", fh.DeleteFlag))

	mux.HandleFunc("GET /flags/values", sec.read(classReadEnvRows, http.HandlerFunc(fh.ListFlagValues)))
	mux.HandleFunc("GET /flags/values/{id}", sec.read(classReadEnvRows, http.HandlerFunc(fh.GetFlagsValueByID)))
	mux.HandleFunc("POST /flags/values", sec.guard(classEnvBody, "flagvalues", fh.CreateFlagValue))
	mux.HandleFunc("DELETE /flags/values/{id}", sec.guard(classRowFlagValue, "flagvalues", fh.DeleteFlagValue))
	mux.HandleFunc("PUT /flags/values", sec.guard(classRowFlagValue, "flagvalues", fh.UpdateFlagValue))

	// localization
	mux.HandleFunc("GET /localization", sec.read(classReadEnvRows, http.HandlerFunc(lh.ListLocalizations)))
	mux.HandleFunc("GET /localization/{id}", sec.read(classReadEnvRows, http.HandlerFunc(lh.GetLocalizationByID)))
	mux.HandleFunc("GET /localization/lookup/{msId}/{envId}/{locale}", sec.read(classReadEnvRows, http.HandlerFunc(lh.GetLocalization)))
	mux.HandleFunc("POST /localization", sec.guard(classEnvBody, "localization", lh.CreateLocalization))
	mux.HandleFunc("DELETE /localization/{id}", sec.guard(classRowLocalization, "localization", lh.DeleteLocalization))
	mux.HandleFunc("PUT /localization/values", sec.guard(classRowLocalization, "localization", lh.UpdateLocalization))

	return mux
}

func (a *App) Run() error {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		// TLS is opt-in so the local stack can talk to the API over plain HTTP;
		// the plain-HTTP case is warned about at startup.
		var err error
		if a.tlsCertFile != "" && a.tlsKeyFile != "" {
			appLog.Info("server starting", slog.String("server.address", a.server.Addr), slog.Bool("tls", true))
			err = a.server.ListenAndServeTLS(a.tlsCertFile, a.tlsKeyFile)
		} else {
			appLog.Info("server starting", slog.String("server.address", a.server.Addr), slog.Bool("tls", false))
			err = a.server.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			appLog.Error("server failed", obs.Err(err))
			os.Exit(1)
		}
	}()

	<-stop
	appLog.Info("shutting down server gracefully")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// stop background reconciler
	if a.stopReconcile != nil {
		a.stopReconcile()
	}

	if err := a.server.Shutdown(ctx); err != nil {
		return err
	}

	// drain NATS before closing DB
	if a.nats != nil {
		if err := a.nats.Drain(); err != nil {
			appLog.Error("nats drain failed", obs.Err(err))
		}
	}

	if err := a.db.Close(); err != nil {
		return err
	}

	appLog.Info("server stopped")
	return nil
}
