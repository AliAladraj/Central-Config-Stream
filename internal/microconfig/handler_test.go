package microconfig_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/AliAladraj/Central-Config-Stream/internal/messaging"
	"github.com/AliAladraj/Central-Config-Stream/internal/microconfig"
)

// The handler is wired onto the same SQLite-backed service the service tests
// use rather than onto a stub: the status codes below are only worth asserting
// if the errors reaching writeErr are the ones a real request produces.
func newTestHandler(t *testing.T) (*microconfig.Handler, *capturePub) {
	t.Helper()
	svc, pub, _ := newTestService(t)
	return microconfig.NewHandler(svc), pub
}

// call invokes one handler directly. The routes live in internal/app/config.go
// and nothing here is registered on a mux, so a path value a route would have
// bound is set by hand.
func call(h http.HandlerFunc, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h(rec, r)
	return rec
}

func decodeConfig(t *testing.T, rec *httptest.ResponseRecorder) microconfig.MicroserviceAppSettings {
	t.Helper()
	var cfg microconfig.MicroserviceAppSettings
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("decode response: %v (%s)", err, rec.Body.String())
	}
	return cfg
}

// The update has no /{id} route of its own: a PUT goes to the same collection
// path the create posts to and names its row in the body. Sending the natural
// key with it is not optional — the update rewrites it, so a body that omits
// microserviceId is a 400 rather than a partial edit.
func TestCreateAndUpdateBothAddressTheCollection(t *testing.T) {
	h, pub := newTestHandler(t)

	// Microservice 2 has no appsettings in environment 3 yet.
	rec := call(h.CreateMicroserviceConfig, httptest.NewRequest(http.MethodPost, "/configs/values",
		strings.NewReader(`{"microserviceId":2,"environmentId":3,"settingsJson":{"http":{"timeoutMs":100}}}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d (%s)", rec.Code, rec.Body.String())
	}
	created := decodeConfig(t, rec)
	if created.ID == 0 {
		t.Fatalf("create returned no id: %+v", created)
	}

	body := `{"id":` + strconv.FormatInt(created.ID, 10) + `,"microserviceId":2,"environmentId":3,"settingsJson":{"http":{"timeoutMs":250}}}`
	rec = call(h.UpdateMicroserviceConfig, httptest.NewRequest(http.MethodPut, "/configs/values", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("update: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if updated := decodeConfig(t, rec); string(updated.SettingsJSON) != `{"http":{"timeoutMs":250}}` {
		t.Fatalf("update did not return the new tree: %s", updated.SettingsJSON)
	}

	rec = call(h.UpdateMicroserviceConfig, httptest.NewRequest(http.MethodPut, "/configs/values",
		strings.NewReader(`{"id":`+strconv.FormatInt(created.ID, 10)+`,"settingsJson":{}}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("update without the natural key: expected 400, got %d (%s)", rec.Code, rec.Body.String())
	}

	if len(pub.published) != 2 {
		t.Fatalf("expected both accepted writes to reach KV, got %v", pub.published)
	}
}

// /configs/{id} is the microservice and /configs/values/{id} is its settings
// row. The two live one path segment apart and answer with different shapes, so
// a caller that confuses them has to get a 404 from a real lookup rather than
// the wrong row.
func TestConfigsAndConfigValuesAddressDifferentRows(t *testing.T) {
	h, _ := newTestHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/configs/1", nil)
	req.SetPathValue("id", "1")
	rec := call(h.GetMicroservice, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get microservice: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var ms microconfig.Microservice
	if err := json.Unmarshal(rec.Body.Bytes(), &ms); err != nil {
		t.Fatalf("decode microservice: %v (%s)", err, rec.Body.String())
	}
	if ms.Microservice != "catalog-api" {
		t.Fatalf("unexpected microservice: %+v", ms)
	}

	// Seed row 200 is microservice 1 in environment 1 — a different id space.
	req = httptest.NewRequest(http.MethodGet, "/configs/values/200", nil)
	req.SetPathValue("id", "200")
	rec = call(h.GetMicroserviceConfig, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get config: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if cfg := decodeConfig(t, rec); cfg.MicroserviceID != 1 || cfg.EnvironmentID != 1 {
		t.Fatalf("unexpected config row: %+v", cfg)
	}

	// A settings id read as a microservice id finds nothing, and says so.
	req = httptest.NewRequest(http.MethodGet, "/configs/200", nil)
	req.SetPathValue("id", "200")
	if rec := call(h.GetMicroservice, req); rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestGetMicroserviceConfigSeparatesABadIDFromAMissingRow(t *testing.T) {
	h, _ := newTestHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/configs/values/abc", nil)
	req.SetPathValue("id", "abc")
	if rec := call(h.GetMicroserviceConfig, req); rec.Code != http.StatusBadRequest {
		t.Fatalf("non-numeric id: expected 400, got %d (%s)", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/configs/values/999", nil)
	req.SetPathValue("id", "999")
	if rec := call(h.GetMicroserviceConfig, req); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id: expected 404, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestUpdateMicroserviceConfigNotFoundIs404(t *testing.T) {
	h, _ := newTestHandler(t)

	rec := call(h.UpdateMicroserviceConfig, httptest.NewRequest(http.MethodPut, "/configs/values",
		strings.NewReader(`{"id":999,"microserviceId":2,"environmentId":3,"settingsJson":{}}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// A lost optimistic-concurrency race is the one failure an admin UI has to be
// able to act on — it means "reload and try again", not "the server broke". It
// only reads that way if it arrives as 409 rather than 500.
func TestUpdateMicroserviceConfigConflictIs409(t *testing.T) {
	h, _ := newTestHandler(t)

	// Seed row 200 is microservice 1 in environment 1. The timestamp is stale by
	// two decades, so the guarded UPDATE matches nothing while the row is still
	// there.
	rec := call(h.UpdateMicroserviceConfig, httptest.NewRequest(http.MethodPut, "/configs/values",
		strings.NewReader(`{"id":200,"microserviceId":1,"environmentId":1,
			"settingsJson":{"a":1},"expectedUpdatedAt":"2000-01-01T00:00:00Z"}`)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d (%s)", rec.Code, rec.Body.String())
	}

	// The same body without the guard still applies: opting out of optimistic
	// concurrency has to keep last-write-wins.
	rec = call(h.UpdateMicroserviceConfig, httptest.NewRequest(http.MethodPut, "/configs/values",
		strings.NewReader(`{"id":200,"microserviceId":1,"environmentId":1,"settingsJson":{"a":1}}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("unguarded update: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// Colliding with (MICROSERVICE_ID, ENVIRONMENT_ID) is a 409, and naming a parent
// that does not exist is a 404 — neither is the 500 a raw constraint or foreign
// key failure would produce.
func TestCreateMicroserviceConfigMapsCollisionsAndMissingParents(t *testing.T) {
	h, _ := newTestHandler(t)

	rec := call(h.CreateMicroserviceConfig, httptest.NewRequest(http.MethodPost, "/configs/values",
		strings.NewReader(`{"microserviceId":1,"environmentId":1,"settingsJson":{}}`)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate: expected 409, got %d (%s)", rec.Code, rec.Body.String())
	}

	for _, body := range []string{
		`{"microserviceId":999,"environmentId":1,"settingsJson":{}}`,
		`{"microserviceId":1,"environmentId":999,"settingsJson":{}}`,
	} {
		rec := call(h.CreateMicroserviceConfig, httptest.NewRequest(http.MethodPost, "/configs/values", strings.NewReader(body)))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: expected 404, got %d (%s)", body, rec.Code, rec.Body.String())
		}
	}
}

// settingsJson is an object or it is nothing. An array or a scalar is
// well-formed JSON that breaks every consumer's binding at once, and KV would
// have published it happily — so both write paths refuse it before the row
// exists.
func TestBothWritePathsRequireAnObject(t *testing.T) {
	h, pub := newTestHandler(t)

	for _, settings := range []string{`[1,2,3]`, `"a string"`, `42`, `null`} {
		rec := call(h.CreateMicroserviceConfig, httptest.NewRequest(http.MethodPost, "/configs/values",
			strings.NewReader(`{"microserviceId":2,"environmentId":3,"settingsJson":`+settings+`}`)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST with settings %s: expected 400, got %d (%s)", settings, rec.Code, rec.Body.String())
		}

		rec = call(h.UpdateMicroserviceConfig, httptest.NewRequest(http.MethodPut, "/configs/values",
			strings.NewReader(`{"id":200,"microserviceId":1,"environmentId":1,"settingsJson":`+settings+`}`)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("PUT with settings %s: expected 400, got %d (%s)", settings, rec.Code, rec.Body.String())
		}
	}

	rec := call(h.CreateMicroserviceConfig, httptest.NewRequest(http.MethodPost, "/configs/values", strings.NewReader(`{"broken":`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body: expected 400, got %d (%s)", rec.Code, rec.Body.String())
	}

	if len(pub.published) != 0 {
		t.Fatalf("a rejected payload still reached KV: %v", pub.published)
	}
}

// A tree over the KV value ceiling is a 400 from the domain, not a 413 from the
// body limit and not a 201 for a row that can never be published: the ceiling
// sits well below the request-body cap, so the request arrives intact and is
// refused on its merits.
func TestOversizedSettingsIs400(t *testing.T) {
	h, pub := newTestHandler(t)

	oversized := `{"k":"` + strings.Repeat("x", messaging.MaxValueSize) + `"}`
	rec := call(h.CreateMicroserviceConfig, httptest.NewRequest(http.MethodPost, "/configs/values",
		strings.NewReader(`{"microserviceId":2,"environmentId":3,"settingsJson":`+oversized+`}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create: expected 400, got %d (%s)", rec.Code, rec.Body.String())
	}

	rec = call(h.UpdateMicroserviceConfig, httptest.NewRequest(http.MethodPut, "/configs/values",
		strings.NewReader(`{"id":200,"microserviceId":1,"environmentId":1,"settingsJson":`+oversized+`}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("update: expected 400, got %d (%s)", rec.Code, rec.Body.String())
	}
	if len(pub.published) != 0 {
		t.Fatalf("an oversized tree reached KV: %v", pub.published)
	}

	// A tree right up against the ceiling is still accepted.
	fitting := `{"k":"` + strings.Repeat("x", messaging.MaxValueSize-16) + `"}`
	rec = call(h.CreateMicroserviceConfig, httptest.NewRequest(http.MethodPost, "/configs/values",
		strings.NewReader(`{"microserviceId":2,"environmentId":3,"settingsJson":`+fitting+`}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("a tree inside the ceiling: expected 201, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestDeleteMicroserviceConfigIs204ThenPurgesAndIs404(t *testing.T) {
	h, pub := newTestHandler(t)

	req := httptest.NewRequest(http.MethodDelete, "/configs/values/200", nil)
	req.SetPathValue("id", "200")
	if rec := call(h.DeleteMicroserviceConfig, req); rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d (%s)", rec.Code, rec.Body.String())
	}
	if len(pub.deleted) != 1 || pub.deleted[0] != "MICROCONFIG|1.1" {
		t.Fatalf("delete did not purge the KV key: %v", pub.deleted)
	}
	if rec := call(h.DeleteMicroserviceConfig, req); rec.Code != http.StatusNotFound {
		t.Fatalf("second delete: expected 404, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// The reference tables are served from here too, and their delete refuses
// rather than cascades: a 409 is the difference between "you cannot do that"
// and quietly taking an environment's whole configuration with it.
func TestReferenceRowHandlersMapTheirErrors(t *testing.T) {
	h, _ := newTestHandler(t)

	rec := call(h.CreateEnvironment, httptest.NewRequest(http.MethodPost, "/environments", strings.NewReader(`{"name":"qa"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create environment: expected 201, got %d (%s)", rec.Code, rec.Body.String())
	}
	rec = call(h.CreateEnvironment, httptest.NewRequest(http.MethodPost, "/environments", strings.NewReader(`{"name":"qa"}`)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate environment: expected 409, got %d (%s)", rec.Code, rec.Body.String())
	}
	rec = call(h.CreateMicroservice, httptest.NewRequest(http.MethodPost, "/microservices", strings.NewReader(`{"name":"   "}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("blank microservice name: expected 400, got %d (%s)", rec.Code, rec.Body.String())
	}

	// Environment 1 carries the seeded flag values, appsettings and bundles.
	req := httptest.NewRequest(http.MethodDelete, "/environments/1", nil)
	req.SetPathValue("id", "1")
	if rec := call(h.DeleteEnvironment, req); rec.Code != http.StatusConflict {
		t.Fatalf("delete an environment in use: expected 409, got %d (%s)", rec.Code, rec.Body.String())
	}

	// Environment 2 (staging) is seeded but unused, so it can go.
	req = httptest.NewRequest(http.MethodDelete, "/environments/2", nil)
	req.SetPathValue("id", "2")
	if rec := call(h.DeleteEnvironment, req); rec.Code != http.StatusNoContent {
		t.Fatalf("delete an unused environment: expected 204, got %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := call(h.DeleteEnvironment, req); rec.Code != http.StatusNotFound {
		t.Fatalf("second delete: expected 404, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// A page the caller cannot have is a 400; an oversized ?limit is clamped
// instead, because a caller asking for too much still wants rows back.
func TestListMicroserviceConfigsValidatesItsQuery(t *testing.T) {
	h, _ := newTestHandler(t)

	for _, query := range []string{"?limit=-1", "?offset=abc", "?microserviceId=x", "?environmentId=-2"} {
		rec := call(h.ListMicroserviceConfigs, httptest.NewRequest(http.MethodGet, "/configs/values"+query, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d (%s)", query, rec.Code, rec.Body.String())
		}
	}

	rec := call(h.ListMicroserviceConfigs, httptest.NewRequest(http.MethodGet, "/configs/values?limit=100000&microserviceId=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var rows []microconfig.MicroserviceAppSettings
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode list: %v (%s)", err, rec.Body.String())
	}
	if len(rows) != 1 || rows[0].MicroserviceID != 1 {
		t.Fatalf("unexpected configs for microservice 1: %+v", rows)
	}
}
