package app

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/AliAladraj/Central-Config-Stream/internal/audit"
	"github.com/AliAladraj/Central-Config-Stream/internal/obs"
)

const (
	// maxAuditPath matches the CONFIG_AUDIT_LOG.PATH column.
	maxAuditPath = 400
	// maxAuditTargetID matches the CONFIG_AUDIT_LOG.TARGET_ID column. The id
	// comes from {id} in the request path and is unbounded caller input, so
	// without this a padded one made the INSERT fail on VARCHAR(64) and the
	// request left no row at all — an attacker probing with the wrong
	// credential could stay out of the trail entirely by lengthening the id it
	// aimed at. SQLite ignores declared widths, so only a Postgres deployment
	// ever saw it.
	maxAuditTargetID = 64
	// auditWriteTimeout bounds the record insert. It runs after the response
	// handler, so a wedged database must not hold the connection open.
	auditWriteTimeout = 5 * time.Second
)

// auditSink collects what the write guard learns about a request so the audit
// middleware — which wraps the guard from the outside — can record it. A
// context value set by an inner handler does not propagate outwards, so the
// middleware seeds a pointer on the way in and reads it on the way out.
type auditSink struct {
	actor  string
	domain string
	rowID  string
	envID  int64

	body     []byte
	bodyRead bool
}

type auditSinkKey struct{}

func sinkFrom(r *http.Request) *auditSink {
	sink, _ := r.Context().Value(auditSinkKey{}).(*auditSink)
	return sink
}

// auditRecorder is the audit store as this package uses it, narrow enough that
// a test can substitute one that always fails.
type auditRecorder interface {
	Insert(ctx context.Context, e audit.Entry) error
}

// auditWrites records every mutating request that is addressed at a route. It
// wraps the whole mux rather than each route so that a write which never
// reaches a handler — a rejected token, a scope the credential does not cover —
// is recorded too, which is the trail that shows someone probing production
// with the wrong credential.
//
// routed reports whether the mux has a handler for the request, and must not be
// nil. A write matching no route changes nothing, so recording it buys nothing
// and costs a megabyte of body read, a full JSON walk and a synchronous insert
// per request — an unauthenticated way to flood the audit table, exhaust the
// connection pool and bury the real trail in noise.
//
// The insert deliberately cannot fail the request: it happens after the change
// has already been committed to the database and published to KV, so returning an
// error would report a failure that did not happen. Same philosophy as the KV
// publish path, which logs and lets the reconciler heal.
func auditWrites(rec auditRecorder, routed func(*http.Request) bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isMutating(r.Method) || !routed(r) {
			next.ServeHTTP(w, r) // reads, and writes addressed at nothing
			return
		}

		// The access-log middleware wraps this one and seeds the sink so it can
		// name the actor; reuse it rather than shadowing it with a second one.
		sink := sinkFrom(r)
		if sink == nil {
			sink = &auditSink{}
			r = r.WithContext(context.WithValue(r.Context(), auditSinkKey{}, sink))
		}
		if !sink.bodyRead {
			sink.body, sink.bodyRead = bufferBody(r), true
		}

		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)

		entry := audit.Entry{
			OccurredAt:    time.Now().UTC(),
			Actor:         sink.actor,
			Method:        r.Method,
			Path:          truncate(r.URL.Path, maxAuditPath),
			Domain:        sink.domain,
			TargetID:      truncate(sink.rowID, maxAuditTargetID),
			EnvironmentID: sink.envID,
			StatusCode:    recorder.status,
			RemoteAddr:    remoteHost(r),
			RequestBody:   audit.Redact(sink.body),
		}

		// The request context is done once the handler returns and may already
		// be cancelled if the client hung up; the record still has to be made.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), auditWriteTimeout)
		defer cancel()

		if err := rec.Insert(ctx, entry); err != nil {
			obs.FromContext(ctx, "audit").Error("audit record failed",
				slog.String("http.request.method", entry.Method),
				slog.String("url.path", entry.Path),
				obs.Err(err))
		}
	})
}

func isMutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// statusRecorder remembers the status the handler wrote. A handler that only
// calls Write never sets one, hence the 200 the middleware seeds. It also
// remembers whether anything went out at all, so the panic handler knows if a
// 500 can still be written.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusRecorder) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.status, w.wrote = code, true
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return truncate(r.RemoteAddr, 64)
	}
	return truncate(host, 64)
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}
