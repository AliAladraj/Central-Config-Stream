// Package obs is the instrumentation tier: ECS-shaped structured logging and a
// standard-library metrics registry rendered in Prometheus text format.
//
// Nothing here changes what the service does. Logging goes to stdout as JSON so
// a log shipper (Filebeat / Elastic Agent) can pick it straight off the
// container, with a text mode for local runs — see docs/OBSERVABILITY.md.
package obs

import (
	"context"
	"io"
	"log"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

// LogOptions is the logging configuration, read from the environment by the
// app layer. Zero values fall back to the ECS/JSON production shape.
type LogOptions struct {
	Level   string    // debug | info | warn | error (default info)
	Format  string    // json (ECS) | text (local development)
	Service string    // service.name
	Version string    // service.version
	Output  io.Writer // defaults to os.Stdout
}

// current is the handler every logger this package hands out delegates to.
// Loggers are created in package-level vars, i.e. before Setup runs, so the
// indirection is what lets a logger created at init time still pick up the
// configuration chosen at startup.
var current atomic.Pointer[slog.Handler]

func init() {
	// Until Setup runs — tests, and anything before config is loaded — behave
	// like the standard logger did: human-readable, on stderr.
	h := slog.Handler(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	current.Store(&h)
}

// Setup installs the configured handler and makes it the slog default. It is
// called once, from the app layer, before anything else is wired.
func Setup(o LogOptions) {
	out := o.Output
	if out == nil {
		out = os.Stdout
	}

	var h slog.Handler
	if strings.EqualFold(o.Format, "text") {
		// Local development: slog's own key names, no ECS service fields — the
		// point of this mode is a line a human can read.
		h = slog.NewTextHandler(out, &slog.HandlerOptions{Level: parseLevel(o.Level)})
	} else {
		h = slog.NewJSONHandler(out, &slog.HandlerOptions{
			Level:       parseLevel(o.Level),
			ReplaceAttr: ecsKeys,
		})
		h = h.WithAttrs([]slog.Attr{
			slog.String("service.name", o.Service),
			slog.String("service.version", o.Version),
		})
	}

	current.Store(&h)
	slog.SetDefault(slog.New(routing{}))
}

// ecsKeys renames slog's three built-in keys to their ECS equivalents. Every
// other attribute is already written with its ECS name at the call site, which
// is why only the top level (len(groups) == 0) is touched.
func ecsKeys(groups []string, a slog.Attr) slog.Attr {
	if len(groups) > 0 {
		return a
	}
	switch a.Key {
	case slog.TimeKey:
		a.Key = "@timestamp"
	case slog.LevelKey:
		a.Key = "log.level"
		a.Value = slog.StringValue(strings.ToLower(a.Value.String()))
	case slog.MessageKey:
		a.Key = "message"
	}
	return a
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// routing is a handler that resolves to whatever Setup last installed. WithAttrs
// and WithGroup cannot be applied eagerly (the real handler may not exist yet),
// so they are recorded and replayed on the way to each record.
type routing struct {
	mods []func(slog.Handler) slog.Handler
}

func (r routing) resolve() slog.Handler {
	h := *current.Load()
	for _, mod := range r.mods {
		h = mod(h)
	}
	return h
}

func (r routing) Enabled(ctx context.Context, l slog.Level) bool {
	return (*current.Load()).Enabled(ctx, l)
}

func (r routing) Handle(ctx context.Context, rec slog.Record) error {
	return r.resolve().Handle(ctx, rec)
}

func (r routing) with(mod func(slog.Handler) slog.Handler) slog.Handler {
	return routing{mods: append(slices.Clone(r.mods), mod)}
}

func (r routing) WithAttrs(attrs []slog.Attr) slog.Handler {
	return r.with(func(h slog.Handler) slog.Handler { return h.WithAttrs(attrs) })
}

func (r routing) WithGroup(name string) slog.Handler {
	return r.with(func(h slog.Handler) slog.Handler { return h.WithGroup(name) })
}

// datasetPrefix namespaces event.dataset, which is how Kibana separates this
// service's streams from everything else in the index.
const datasetPrefix = "central-config."

var loggers sync.Map // component -> *slog.Logger

// Logger returns the logger for a component, tagged with its event.dataset. It
// is safe to call from a package-level var: the returned logger follows Setup.
func Logger(component string) *slog.Logger {
	if l, ok := loggers.Load(component); ok {
		return l.(*slog.Logger)
	}
	l := slog.New(routing{}).With(slog.String("event.dataset", datasetPrefix+component))
	actual, _ := loggers.LoadOrStore(component, l)
	return actual.(*slog.Logger)
}

// StdLogger adapts the active handler to *log.Logger, for the standard-library
// APIs that still insist on one (http.Server.ErrorLog).
func StdLogger(component string, level slog.Level) *log.Logger {
	return slog.NewLogLogger(Logger(component).Handler(), level)
}

// Err renders an error as ECS fields. Attach it rather than formatting the error
// into the message, so failures can be aggregated by type in Kibana.
func Err(err error) slog.Attr {
	if err == nil {
		return slog.Attr{}
	}
	return slog.String("error.message", err.Error())
}

// Stack carries a captured goroutine stack. Only the panic handler has one.
func Stack(buf []byte) slog.Attr {
	return slog.String("error.stack_trace", string(buf))
}

// ---- request-scoped loggers ----

type loggerKey struct{}

// WithLogger returns a context carrying the request's logger. The access-log
// middleware puts one in; handlers and services read it back with FromContext
// so their lines carry the same http.request.id.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, l)
}

// RequestLogger returns a logger carrying the request correlation id and
// nothing else: the component's event.dataset is added by FromContext at the
// log site, so it appears exactly once.
func RequestLogger(requestID string) *slog.Logger {
	return slog.New(routing{}).With(slog.String("http.request.id", requestID))
}

// FromContext returns the request's logger, or the component logger when the
// call did not come from a request (the reconciler, startup).
func FromContext(ctx context.Context, component string) *slog.Logger {
	if l, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok {
		return l.With(slog.String("event.dataset", datasetPrefix+component))
	}
	return Logger(component)
}
