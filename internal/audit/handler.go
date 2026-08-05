package audit

import (
	"net/http"
	"time"

	"github.com/ErasedKyte/Central-Config-Stream/internal/obs"
	"github.com/ErasedKyte/Central-Config-Stream/internal/web"
)

// dateOnlyLayout lets a caller write ?from=2026-01-01 instead of a full
// timestamp; the range is the common case and typing an RFC 3339 instant by
// hand is not.
const dateOnlyLayout = "2006-01-02"

// Handler serves the audit log.
//
//	GET /audit?actor=&from=&to=&limit=&offset=
//
// It follows the list-endpoint conventions of the domain handlers: ParsePage
// for the page, a flat JSON array, and no internal detail in error bodies.
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

	entries, err := h.store.List(r.Context(), Filter{
		Actor:  r.URL.Query().Get("actor"),
		From:   from,
		To:     to,
		Limit:  limit,
		Offset: offset,
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
