package localization_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/AliAladraj/Central-Config-Stream/internal/localization"
)

// The handler is wired onto the same SQLite-backed service the service tests
// use rather than onto a stub: the status codes below are only worth asserting
// if the errors reaching writeErr are the ones a real request produces.
func newTestHandler(t *testing.T) (*localization.Handler, *capturePub) {
	t.Helper()
	svc, pub := newTestService(t)
	return localization.NewHandler(svc), pub
}

// call invokes one handler directly. The routes live in internal/app/config.go
// and nothing here is registered on a mux, so a path value a route would have
// bound is set by hand.
func call(h http.HandlerFunc, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h(rec, r)
	return rec
}

func decodeRow(t *testing.T, rec *httptest.ResponseRecorder) localization.Localization {
	t.Helper()
	var l localization.Localization
	if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil {
		t.Fatalf("decode response: %v (%s)", err, rec.Body.String())
	}
	return l
}

// The two write paths are not symmetric: a create posts to /localization, while
// an update puts to /localization/values and carries the id in the body rather
// than in the path. A client that infers the update path from the create one
// gets a 404 from the mux, so both shapes are pinned here.
func TestCreateAtTheCollectionAndUpdateAtValues(t *testing.T) {
	h, pub := newTestHandler(t)

	rec := call(h.CreateLocalization, httptest.NewRequest(http.MethodPost, "/localization",
		strings.NewReader(`{"microserviceId":2,"environmentId":1,"locale":"en-US","bundleJson":{"cart.title":"Cart"}}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d (%s)", rec.Code, rec.Body.String())
	}
	created := decodeRow(t, rec)
	if created.ID == 0 {
		t.Fatalf("create returned no id: %+v", created)
	}

	// The id travels in the body, and the whole natural key comes with it —
	// omitting microserviceId or environmentId is a 400, not a partial edit.
	body := `{"id":` + itoa(created.ID) + `,"microserviceId":2,"environmentId":1,"locale":"en-US","bundleJson":{"cart.title":"Basket"}}`
	rec = call(h.UpdateLocalization, httptest.NewRequest(http.MethodPut, "/localization/values", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("update: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if updated := decodeRow(t, rec); string(updated.BundleJSON) != `{"cart.title":"Basket"}` {
		t.Fatalf("update did not return the new bundle: %s", updated.BundleJSON)
	}

	if len(pub.published) != 2 {
		t.Fatalf("expected both writes to reach KV, got %v", pub.published)
	}
}

// A PUT with no id in the body cannot address a row at all, and one naming a row
// that does not exist is a 404 — not the 500 a bare "no rows affected" would
// produce.
func TestUpdateLocalizationNotFoundIs404(t *testing.T) {
	h, _ := newTestHandler(t)

	rec := call(h.UpdateLocalization, httptest.NewRequest(http.MethodPut, "/localization/values",
		strings.NewReader(`{"id":999,"microserviceId":1,"environmentId":1,"locale":"en-GB","bundleJson":{}}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d (%s)", rec.Code, rec.Body.String())
	}

	rec = call(h.UpdateLocalization, httptest.NewRequest(http.MethodPut, "/localization/values",
		strings.NewReader(`{"microserviceId":1,"environmentId":1,"locale":"en-GB","bundleJson":{}}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("update without an id: expected 400, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// A lost optimistic-concurrency race is the one failure an admin UI has to be
// able to act on — it means "reload and try again", not "the server broke". It
// only reads that way if it arrives as 409 rather than 500.
func TestUpdateLocalizationConflictIs409(t *testing.T) {
	h, _ := newTestHandler(t)

	// Seed row 301 is microservice 1, environment 1, locale pt-BR. The
	// timestamp is stale by two decades, so the guarded UPDATE matches nothing
	// while the row is still there.
	rec := call(h.UpdateLocalization, httptest.NewRequest(http.MethodPut, "/localization/values",
		strings.NewReader(`{"id":301,"microserviceId":1,"environmentId":1,"locale":"pt-BR",
			"bundleJson":{"a":"b"},"expectedUpdatedAt":"2000-01-01T00:00:00Z"}`)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d (%s)", rec.Code, rec.Body.String())
	}

	// The same body without the guard still applies: opting out of optimistic
	// concurrency has to keep last-write-wins.
	rec = call(h.UpdateLocalization, httptest.NewRequest(http.MethodPut, "/localization/values",
		strings.NewReader(`{"id":301,"microserviceId":1,"environmentId":1,"locale":"pt-BR","bundleJson":{"a":"b"}}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("unguarded update: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// Colliding with the natural unique key is a 409, not the 500 a constraint
// violation surfacing raw would give.
func TestCreateLocalizationDuplicateIs409(t *testing.T) {
	h, _ := newTestHandler(t)

	rec := call(h.CreateLocalization, httptest.NewRequest(http.MethodPost, "/localization",
		strings.NewReader(`{"microserviceId":1,"environmentId":1,"locale":"pt-BR","bundleJson":{}}`)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// A parent row that does not exist is a 404 rather than an opaque foreign-key
// failure: the caller sent a well-formed body naming something that is not there.
func TestCreateLocalizationMissingParentIs404(t *testing.T) {
	h, _ := newTestHandler(t)

	for _, body := range []string{
		`{"microserviceId":999,"environmentId":1,"locale":"fr-FR","bundleJson":{}}`,
		`{"microserviceId":1,"environmentId":999,"locale":"fr-FR","bundleJson":{}}`,
	} {
		rec := call(h.CreateLocalization, httptest.NewRequest(http.MethodPost, "/localization", strings.NewReader(body)))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: expected 404, got %d (%s)", body, rec.Code, rec.Body.String())
		}
	}
}

// The paths differ; the rule does not. A locale the collection POST refuses has
// to be refused by the PUT at /localization/values too — a locale that only one
// write path let through still becomes a KV key, and one a consumer cannot split
// on the separator is republished by every reconcile sweep from then on.
func TestBothWritePathsRejectTheSameLocale(t *testing.T) {
	h, pub := newTestHandler(t)

	for _, locale := range []string{"", "pt.BR", "pt BR", "pt/BR"} {
		bundle := `"bundleJson":{"a":"b"}`
		rec := call(h.CreateLocalization, httptest.NewRequest(http.MethodPost, "/localization",
			strings.NewReader(`{"microserviceId":2,"environmentId":1,"locale":"`+locale+`",`+bundle+`}`)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST with locale %q: expected 400, got %d (%s)", locale, rec.Code, rec.Body.String())
		}

		rec = call(h.UpdateLocalization, httptest.NewRequest(http.MethodPut, "/localization/values",
			strings.NewReader(`{"id":301,"microserviceId":1,"environmentId":1,"locale":"`+locale+`",`+bundle+`}`)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("PUT with locale %q: expected 400, got %d (%s)", locale, rec.Code, rec.Body.String())
		}
	}

	if len(pub.published) != 0 {
		t.Fatalf("a rejected locale still reached KV: %v", pub.published)
	}
}

// The lookup is deliberately looser: it only rejects an empty locale, because a
// read cannot corrupt a key. So a locale the write paths refuse is a 404 here —
// nothing holds it — and not the 400 the same string earns on a write.
func TestLookupRejectsOnlyAnEmptyLocale(t *testing.T) {
	h, _ := newTestHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/localization/lookup/1/1/pt.BR", nil)
	req.SetPathValue("msId", "1")
	req.SetPathValue("envId", "1")
	req.SetPathValue("locale", "pt.BR")
	if rec := call(h.GetLocalization, req); rec.Code != http.StatusNotFound {
		t.Fatalf("lookup of an unwritable locale: expected 404, got %d (%s)", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/localization/lookup/1/1/", nil)
	req.SetPathValue("msId", "1")
	req.SetPathValue("envId", "1")
	req.SetPathValue("locale", "")
	if rec := call(h.GetLocalization, req); rec.Code != http.StatusBadRequest {
		t.Fatalf("lookup with no locale: expected 400, got %d (%s)", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/localization/lookup/1/1/pt-BR", nil)
	req.SetPathValue("msId", "1")
	req.SetPathValue("envId", "1")
	req.SetPathValue("locale", "pt-BR")
	rec := call(h.GetLocalization, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("lookup of the seeded bundle: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if got := decodeRow(t, rec); got.ID != 301 {
		t.Fatalf("lookup returned the wrong row: %+v", got)
	}
}

// A bundle that is not a JSON object breaks every consumer's binding at once, so
// it is a 400 rather than a row nobody can use. A body that is not JSON at all
// never reaches the service.
func TestWriteRejectsAnUnusableBody(t *testing.T) {
	h, _ := newTestHandler(t)

	for _, bundle := range []string{`["a","b"]`, `"a string"`, `7`, `null`} {
		rec := call(h.CreateLocalization, httptest.NewRequest(http.MethodPost, "/localization",
			strings.NewReader(`{"microserviceId":2,"environmentId":1,"locale":"fr-FR","bundleJson":`+bundle+`}`)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("bundle %s: expected 400, got %d (%s)", bundle, rec.Code, rec.Body.String())
		}
	}

	rec := call(h.CreateLocalization, httptest.NewRequest(http.MethodPost, "/localization", strings.NewReader(`{"broken":`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body: expected 400, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// An id that is not a number never becomes a lookup: it is the request that is
// wrong, not the row that is missing.
func TestRowPathsSeparateABadIDFromAMissingRow(t *testing.T) {
	h, _ := newTestHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/localization/abc", nil)
	req.SetPathValue("id", "abc")
	if rec := call(h.GetLocalizationByID, req); rec.Code != http.StatusBadRequest {
		t.Fatalf("non-numeric id: expected 400, got %d (%s)", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/localization/999", nil)
	req.SetPathValue("id", "999")
	if rec := call(h.GetLocalizationByID, req); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id: expected 404, got %d (%s)", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/localization/301", nil)
	req.SetPathValue("id", "301")
	if rec := call(h.GetLocalizationByID, req); rec.Code != http.StatusOK {
		t.Fatalf("seeded id: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestDeleteLocalizationIs204ThenPurgesAndIs404(t *testing.T) {
	h, pub := newTestHandler(t)

	req := httptest.NewRequest(http.MethodDelete, "/localization/301", nil)
	req.SetPathValue("id", "301")
	if rec := call(h.DeleteLocalization, req); rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d (%s)", rec.Code, rec.Body.String())
	}
	if len(pub.deleted) != 1 || pub.deleted[0] != "LOCALIZATION|1.1.pt-BR" {
		t.Fatalf("delete did not purge the KV key: %v", pub.deleted)
	}

	if rec := call(h.DeleteLocalization, req); rec.Code != http.StatusNotFound {
		t.Fatalf("second delete: expected 404, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// A page the caller cannot have is a 400 rather than a silently clamped answer
// only for the parameters that are meaningless; an oversized ?limit is clamped,
// because a caller asking for too much still wants rows back.
func TestListLocalizationsValidatesItsQuery(t *testing.T) {
	h, _ := newTestHandler(t)

	for _, query := range []string{"?limit=-1", "?offset=abc", "?microserviceId=x", "?environmentId=-2"} {
		rec := call(h.ListLocalizations, httptest.NewRequest(http.MethodGet, "/localization"+query, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d (%s)", query, rec.Code, rec.Body.String())
		}
	}

	rec := call(h.ListLocalizations, httptest.NewRequest(http.MethodGet, "/localization?limit=100000&locale=pt-BR", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var rows []localization.Localization
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode list: %v (%s)", err, rec.Body.String())
	}
	if len(rows) != 1 || rows[0].Locale != "pt-BR" {
		t.Fatalf("unexpected rows for pt-BR: %+v", rows)
	}
}

// itoa keeps the request bodies above readable; strconv in the middle of a
// string literal does not.
func itoa(id int64) string {
	return strconv.FormatInt(id, 10)
}
