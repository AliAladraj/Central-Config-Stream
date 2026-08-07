package audit

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/AliAladraj/Central-Config-Stream/internal/obs"
	"github.com/AliAladraj/Central-Config-Stream/internal/web"
)

// dateOnlyLayout lets a caller write ?from=2026-01-01 instead of a full
// timestamp; the range is the common case and typing an RFC 3339 instant by
// hand is not.
const dateOnlyLayout = "2006-01-02"

type scopeKey struct{}

// WithEnvironments narrows what the request may read to the given environment
// ids. The admin API resolves the reader's credential and applies its scope
// here, so a request that arrives unnarrowed reads the whole trail — which is
// what a full-scope token, and a deployment with auth disabled, is entitled to.
func WithEnvironments(r *http.Request, envs []int64) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), scopeKey{}, envs))
}

func environmentsFrom(r *http.Request) []int64 {
	envs, _ := r.Context().Value(scopeKey{}).([]int64)
	return envs
}

// environmentFilter resolves ?environmentId= against the reader's scope. The
// parameter narrows and can never widen: a caller asking for an environment
// its token does not reach gets an empty listing rather than the rows, and
// rather than a 403 that would confirm the environment exists — the same
// answer the domain handlers give a single row fetched from outside scope.
//
// The three returns are "filter by these", "answer empty" and "bad request".
func environmentFilter(r *http.Request) (envs []int64, empty bool, err error) {
	scope := environmentsFrom(r)
	raw := r.URL.Query().Get("environmentId")
	if raw == "" {
		return scope, false, nil
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 {
		return nil, false, errBadEnvironment
	}
	// A nil scope is a full-scope reader, so there is nothing to intersect with.
	if len(scope) > 0 && !slices.Contains(scope, id) {
		return nil, true, nil
	}
	return []int64{id}, false, nil
}

var errBadEnvironment = errors.New("invalid environmentId")

// Handler serves the audit log.
//
//	GET /audit?actor=&from=&to=&limit=&offset=&environmentId=
//
// It follows the list-endpoint conventions of the domain handlers: ParsePage
// for the page, a flat JSON array, and no internal detail in error bodies. The
// rows carry request bodies from every environment, so the reader's scope
// (WithEnvironments) narrows the listing the same way it narrows a write.
type Handler struct {
	store *Store
}

func NewHandler(store *Store) *Handler {
	return &Handler{store: store}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := web.ParsePage(r)
	if err != nil {
		web.Error(w, http.StatusBadRequest, err.Error())
		return
	}

	from, err := timeParam(r, "from")
	if err != nil {
		web.Error(w, http.StatusBadRequest, "invalid from")
		return
	}
	to, err := timeParam(r, "to")
	if err != nil {
		web.Error(w, http.StatusBadRequest, "invalid to")
		return
	}

	envs, empty, err := environmentFilter(r)
	if err != nil {
		web.Error(w, http.StatusBadRequest, "invalid environmentId")
		return
	}
	if empty {
		web.JSON(w, http.StatusOK, []Entry{})
		return
	}

	entries, err := h.store.List(r.Context(), Filter{
		Actor:        r.URL.Query().Get("actor"),
		From:         from,
		To:           to,
		Environments: envs,
		Limit:        limit,
		Offset:       offset,
	})
	if err != nil {
		// The driver error names tables and connection strings; it belongs in
		// the log, not in the response.
		obs.FromContext(r.Context(), "audit").Error("list audit entries failed", obs.Err(err))
		web.Error(w, http.StatusInternalServerError, "internal server error")
		return
	}

	web.JSON(w, http.StatusOK, entries)
}

// timeParam accepts either a plain date or an RFC 3339 instant. An absent
// parameter yields the zero time, which the filter reads as "no bound".
func timeParam(r *http.Request, name string) (time.Time, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, nil
	}
	return time.Parse(dateOnlyLayout, raw)
}
