package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"strconv"
	"time"

	"github.com/ErasedKyte/Central-Config-Stream/internal/obs"
	"github.com/ErasedKyte/Central-Config-Stream/internal/web"
)

// requestIDHeader carries the correlation id in both directions: an inbound
// value is honoured so a request can be followed across services, and it is
// always echoed back so a caller can quote it in a bug report.
const requestIDHeader = "X-Request-Id"

// maxRequestID bounds an inbound id. The value ends up in every log line for
// the request, so it is length-checked and character-checked rather than
// trusted — a header is caller input.
const maxRequestID = 64

// observe is the outermost middleware: it assigns the request id, puts a logger
// carrying it into the context, records the access log line and the HTTP
// metrics, and turns a panic into a 500 instead of a dropped connection.
//
// routeOf maps a request to the route pattern it matched, so the metric label
// stays bounded by the routing table rather than by whatever paths get probed.
// It wraps the audit middleware from the outside, and seeds the audit sink so
// the access line can name the authenticated actor the write guard resolved.
func observe(routeOf func(*http.Request) string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		id := requestID(r)
		w.Header().Set(requestIDHeader, id)

		sink := sinkFrom(r)
		if sink == nil {
			sink = &auditSink{}
		}
		ctx := context.WithValue(r.Context(), auditSinkKey{}, sink)
		r = r.WithContext(obs.WithLogger(ctx, obs.RequestLogger(id)))

		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		route := routeOf(r)

		defer func() {
			if rec := recover(); rec != nil {
				logPanic(r, route, rec)
				if !recorder.wrote {
					web.Error(recorder, http.StatusInternalServerError, "internal server error")
				}
				recorder.status = http.StatusInternalServerError
			}
			record(r, route, recorder.status, time.Since(started))
		}()

		next.ServeHTTP(recorder, r)
	})
}

// record emits the one access line per request plus its metrics. Health and
// metrics scrapes are logged at debug: an orchestrator probing every few
// seconds would otherwise be most of the log volume.
func record(r *http.Request, route string, status int, elapsed time.Duration) {
	method := methodLabel(r.Method)
	obs.HTTPRequests.Inc("method", method, "route", route, "status", strconv.Itoa(status))
	obs.HTTPDuration.Observe(elapsed.Seconds(), "method", method, "route", route)

	level := slog.LevelInfo
	switch {
	case route == "GET /health" || route == "GET /livez" || route == "GET /metrics":
		level = slog.LevelDebug
	case status >= http.StatusInternalServerError:
		level = slog.LevelError
	case status >= http.StatusBadRequest:
		level = slog.LevelWarn
	}

	log := obs.FromContext(r.Context(), "http")
	if !log.Enabled(r.Context(), level) {
		return
	}

	attrs := []any{
		slog.String("http.request.method", r.Method),
		slog.String("url.path", r.URL.Path),
		slog.String("http.route", route),
		slog.Int("http.response.status_code", status),
		slog.Int64("event.duration", elapsed.Nanoseconds()),
		slog.String("client.ip", remoteHost(r)),
	}
	if sink := sinkFrom(r); sink != nil && sink.actor != "" {
		attrs = append(attrs, slog.String("user.name", sink.actor))
	}
	log.Log(r.Context(), level, "http request", attrs...)
}

// methodLabel bounds the method label to the verbs the API actually serves.
// r.Method is caller input and a metric label allocates a series that lives as
// long as the process, so `curl -X <anything>` on an open route is otherwise
// unauthenticated, unrate-limited memory growth and a /metrics body that keeps
// getting slower to scrape.
func methodLabel(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions,
		http.MethodConnect, http.MethodTrace:
		return method
	default:
		return "other"
	}
}

// logPanic records the failure with the stack that produced it. net/http's own
// recovery just drops the connection, which tells an operator nothing beyond a
// client-side EOF.
func logPanic(r *http.Request, route string, rec any) {
	obs.HTTPPanics.Inc("route", route)

	buf := make([]byte, 8<<10)
	buf = buf[:runtime.Stack(buf, false)]

	obs.FromContext(r.Context(), "http").Error("handler panicked",
		slog.String("http.request.method", r.Method),
		slog.String("url.path", r.URL.Path),
		slog.String("error.message", fmt.Sprint(rec)),
		obs.Stack(buf),
	)
}

// requestID honours a usable inbound id and otherwise mints one.
func requestID(r *http.Request) string {
	if id := r.Header.Get(requestIDHeader); validRequestID(id) {
		return id
	}
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand does not fail in practice; a time-based id still
		// correlates the lines of one request, which is all this is for.
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(buf[:])
}

// validRequestID keeps caller input out of the log as anything but a plain
// token: no newlines, no quotes, nothing that could forge a second log record.
func validRequestID(id string) bool {
	if id == "" || len(id) > maxRequestID {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}
