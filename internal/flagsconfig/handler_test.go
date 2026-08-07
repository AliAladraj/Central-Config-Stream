package flagsconfig

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ErasedKyte/Central-Config-Stream/internal/database"
)

// The handler is wired onto a SQLite-backed service rather than onto a stub:
// the status codes below are only worth asserting if the errors reaching
// writeErr are the ones a real request produces. A stub returning a canned
// sentinel would pass whether or not the service still produces it.
func newTestHandler(t *testing.T) (*Handler, *capturePub) {
	t.Helper()
	db, err := database.NewSQLiteDB("file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "t.db")))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	pub := &capturePub{}
	return NewHandler(NewService(NewSQLiteRepository(db), pub)), pub
}

// call invokes one handler directly. The routes live in internal/app/config.go
// and nothing here is registered on a mux, so a path value a route would have
// bound is set by hand.
func call(h http.HandlerFunc, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h(rec, r)
	return rec
}

func post(path, body string) *http.Request {
	return httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
}

func put(body string) *http.Request {
	return httptest.NewRequest(http.MethodPut, "/flags/values", strings.NewReader(body))
}

func byID(method, path, id string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.SetPathValue("id", id)
	return r
}

func decodeValue(t *testing.T, rec *httptest.ResponseRecorder) FlagValue {
	t.Helper()
	var v FlagValue
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode response: %v (%s)", err, rec.Body.String())
	}
	return v
}

// The two write paths are not symmetric: a flag is created at /flags, its value
// at /flags/values, and an update puts to /flags/values carrying the id in the
// body rather than in the path. A client that infers the update path from the
// create one gets a 404 from the mux, so all three shapes are pinned here.
func TestCreateFlagThenItsValueThenUpdateAtValues(t *testing.T) {
	h, pub := newTestHandler(t)

	rec := call(h.CreateFlag, post("/flags", `{"flagKey":"checkout_v3","isActive":1}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create flag: expected 201, got %d (%s)", rec.Code, rec.Body.String())
	}
	var flag Flag
	if err := json.Unmarshal(rec.Body.Bytes(), &flag); err != nil {
		t.Fatalf("decode flag: %v (%s)", err, rec.Body.String())
	}
	if flag.ID == 0 {
		t.Fatalf("create returned no id: %+v", flag)
	}

	// A flag on its own has no per-environment value, so nothing is published
	// until the first value row exists.
	if len(pub.published) != 0 {
		t.Fatalf("creating a flag published a KV key: %v", pub.published)
	}

	rec = call(h.CreateFlagValue, post("/flags/values",
		`{"flagId":`+itoa(flag.ID)+`,"environmentId":2,"value":"canary","enabled":1}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create value: expected 201, got %d (%s)", rec.Code, rec.Body.String())
	}
	created := decodeValue(t, rec)
	if created.ID == 0 {
		t.Fatalf("create value returned no id: %+v", created)
	}

	rec = call(h.UpdateFlagValue, put(`{"id":`+itoa(created.ID)+`,"value":"0.5","enabled":0}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("update: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if updated := decodeValue(t, rec); updated.Value != "0.5" || updated.Enabled != 0 {
		t.Fatalf("update did not return the new row: %+v", updated)
	}

	if len(pub.published) != 2 {
		t.Fatalf("expected both value writes to reach KV, got %v", pub.published)
	}
}

// The paths differ; the rule does not. A value the collection POST refuses has
// to be refused by the PUT at /flags/values too. "" is the case that matters:
// VALUE is NOT NULL, but PostgreSQL keeps an empty string and NULL apart, so
// the column takes one happily and this is the only thing standing between a
// caller and {"enabled":true,"value":""} reaching every consumer in the
// environment — where it is indistinguishable from a parse bug.
func TestBothWritePathsRejectAnUnusableValue(t *testing.T) {
	h, pub := newTestHandler(t)

	for _, value := range []string{"", strings.Repeat("x", maxFlagValueLen+1)} {
		rec := call(h.CreateFlagValue, post("/flags/values",
			`{"flagId":8,"environmentId":2,"value":"`+value+`","enabled":1}`))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST with a %d-char value: expected 400, got %d (%s)", len(value), rec.Code, rec.Body.String())
		}

		// Seed row 100 is flag 7 in environment 1.
		rec = call(h.UpdateFlagValue, put(`{"id":100,"value":"`+value+`","enabled":1}`))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("PUT with a %d-char value: expected 400, got %d (%s)", len(value), rec.Code, rec.Body.String())
		}
	}

	if len(pub.published) != 0 {
		t.Fatalf("a rejected value still reached KV: %v", pub.published)
	}

	// The bound is the column's, not one short of it: a value the database
	// would have taken is still written.
	rec := call(h.UpdateFlagValue, put(`{"id":100,"value":"`+strings.Repeat("x", maxFlagValueLen)+`","enabled":1}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT at exactly the limit: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// enabled and isActive are booleans the API spells as 1/0 and the columns hold
// as SMALLINT. A caller sending 70000 is not asking for anything the contract
// can express, and left to the column it overflows into a 500 — a server fault
// for what is ordinary bad input.
func TestBothWritePathsRejectAnOutOfRangeState(t *testing.T) {
	h, pub := newTestHandler(t)

	for _, state := range []string{"70000", "-1", "2", "99999"} {
		rec := call(h.CreateFlagValue, post("/flags/values",
			`{"flagId":8,"environmentId":2,"value":"on","enabled":`+state+`}`))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST with enabled=%s: expected 400, got %d (%s)", state, rec.Code, rec.Body.String())
		}

		rec = call(h.UpdateFlagValue, put(`{"id":100,"value":"on","enabled":`+state+`}`))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("PUT with enabled=%s: expected 400, got %d (%s)", state, rec.Code, rec.Body.String())
		}

		rec = call(h.CreateFlag, post("/flags", `{"flagKey":"probe_`+strings.TrimPrefix(state, "-")+`","isActive":`+state+`}`))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST /flags with isActive=%s: expected 400, got %d (%s)", state, rec.Code, rec.Body.String())
		}
	}

	if len(pub.published) != 0 {
		t.Fatalf("a rejected state still reached KV: %v", pub.published)
	}

	// Both legitimate values still write; the check bounds the meaning, not the
	// caller's ability to turn a flag off.
	for _, state := range []string{"0", "1"} {
		rec := call(h.CreateFlag, post("/flags", `{"flagKey":"ok_`+state+`","isActive":`+state+`}`))
		if rec.Code != http.StatusCreated {
			t.Errorf("POST /flags with isActive=%s: expected 201, got %d (%s)", state, rec.Code, rec.Body.String())
		}
		rec = call(h.UpdateFlagValue, put(`{"id":100,"value":"on","enabled":`+state+`}`))
		if rec.Code != http.StatusOK {
			t.Errorf("PUT with enabled=%s: expected 200, got %d (%s)", state, rec.Code, rec.Body.String())
		}
	}
}

// A flag key ends up inside the KV key "{environmentID}.{flagKey}", so a key
// carrying a dot or a space is refused at the write rather than republished by
// every sweep from then on.
func TestCreateFlagRejectsAnUnusableKey(t *testing.T) {
	h, _ := newTestHandler(t)

	for _, key := range []string{"", "has space", "has.dot", "has/slash"} {
		rec := call(h.CreateFlag, post("/flags", `{"flagKey":"`+key+`","isActive":1}`))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("flag key %q: expected 400, got %d (%s)", key, rec.Code, rec.Body.String())
		}
	}

	// A body that is not JSON at all never reaches the service.
	if rec := call(h.CreateFlag, post("/flags", `{"broken":`)); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body: expected 400, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// A lost optimistic-concurrency race is the one failure an admin UI has to be
// able to act on — it means "reload and try again", not "the server broke". It
// only reads that way if it arrives as 409 rather than 500.
func TestUpdateFlagValueConflictIs409(t *testing.T) {
	h, _ := newTestHandler(t)

	// Seed row 100 is flag 7 in environment 1. The timestamp is stale by two
	// decades, so the guarded UPDATE matches nothing while the row is still there.
	rec := call(h.UpdateFlagValue, put(
		`{"id":100,"value":"canary","enabled":0,"expectedUpdatedAt":"2000-01-01T00:00:00Z"}`))
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d (%s)", rec.Code, rec.Body.String())
	}

	// The same body without the guard still applies: opting out of optimistic
	// concurrency has to keep last-write-wins.
	if rec := call(h.UpdateFlagValue, put(`{"id":100,"value":"canary","enabled":0}`)); rec.Code != http.StatusOK {
		t.Fatalf("unguarded update: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// A PUT with no id in the body cannot address a row at all, and one naming a
// row that does not exist is a 404 — not the 500 a bare "no rows affected"
// would produce.
func TestUpdateFlagValueNotFoundIs404(t *testing.T) {
	h, _ := newTestHandler(t)

	if rec := call(h.UpdateFlagValue, put(`{"id":999,"value":"canary","enabled":0}`)); rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d (%s)", rec.Code, rec.Body.String())
	}

	if rec := call(h.UpdateFlagValue, put(`{"value":"canary","enabled":0}`)); rec.Code != http.StatusBadRequest {
		t.Fatalf("update without an id: expected 400, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// Colliding with (ENVIRONMENT_ID, FLAG_ID) is a 409, and a parent row that does
// not exist is a 404 rather than an opaque foreign-key failure — the caller sent
// a well-formed body naming something that is not there.
func TestCreateFlagValueSeparatesACollisionFromAMissingParent(t *testing.T) {
	h, _ := newTestHandler(t)

	// Seed row 100 already holds flag 7 in environment 1.
	rec := call(h.CreateFlagValue, post("/flags/values", `{"flagId":7,"environmentId":1,"value":"on","enabled":1}`))
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate: expected 409, got %d (%s)", rec.Code, rec.Body.String())
	}

	for _, body := range []string{
		`{"flagId":999,"environmentId":1,"value":"on","enabled":1}`,
		`{"flagId":7,"environmentId":999,"value":"on","enabled":1}`,
	} {
		if rec := call(h.CreateFlagValue, post("/flags/values", body)); rec.Code != http.StatusNotFound {
			t.Errorf("%s: expected 404, got %d (%s)", body, rec.Code, rec.Body.String())
		}
	}

	// An id that cannot address a row is the caller's mistake, not a missing row.
	for _, body := range []string{
		`{"flagId":0,"environmentId":1,"value":"on","enabled":1}`,
		`{"flagId":7,"environmentId":0,"value":"on","enabled":1}`,
	} {
		if rec := call(h.CreateFlagValue, post("/flags/values", body)); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d (%s)", body, rec.Code, rec.Body.String())
		}
	}
}

// Deleting a flag has to take every environment's KV key with it, not just the
// one the caller happened to be looking at — otherwise consumers elsewhere keep
// serving a flag no row backs until the next full reconcile sweep.
func TestDeleteFlagIs204ThenPurgesEveryEnvironmentAndIs404(t *testing.T) {
	h, pub := newTestHandler(t)

	// Seed flag 7 (search_v2) has values in environments 1 and 3.
	if rec := call(h.DeleteFlag, byID(http.MethodDelete, "/flags/7", "7")); rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d (%s)", rec.Code, rec.Body.String())
	}
	want := []string{"FLAGS|1.search_v2", "FLAGS|3.search_v2"}
	if len(pub.deleted) != len(want) {
		t.Fatalf("expected %d purges, got %v", len(want), pub.deleted)
	}
	for i, key := range want {
		if pub.deleted[i] != key {
			t.Errorf("purge %d: want %q, got %q", i, key, pub.deleted[i])
		}
	}

	if rec := call(h.DeleteFlag, byID(http.MethodDelete, "/flags/7", "7")); rec.Code != http.StatusNotFound {
		t.Fatalf("second delete: expected 404, got %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := call(h.DeleteFlag, byID(http.MethodDelete, "/flags/abc", "abc")); rec.Code != http.StatusBadRequest {
		t.Fatalf("non-numeric id: expected 400, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestDeleteFlagValueIs204ThenPurgesAndIs404(t *testing.T) {
	h, pub := newTestHandler(t)

	// Seed row 103 is search_v2 in environment 3.
	if rec := call(h.DeleteFlagValue, byID(http.MethodDelete, "/flags/values/103", "103")); rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d (%s)", rec.Code, rec.Body.String())
	}
	if len(pub.deleted) != 1 || pub.deleted[0] != "FLAGS|3.search_v2" {
		t.Fatalf("delete did not purge the KV key: %v", pub.deleted)
	}

	if rec := call(h.DeleteFlagValue, byID(http.MethodDelete, "/flags/values/103", "103")); rec.Code != http.StatusNotFound {
		t.Fatalf("second delete: expected 404, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// An id that is not a number never becomes a lookup: it is the request that is
// wrong, not the row that is missing.
func TestRowPathsSeparateABadIDFromAMissingRow(t *testing.T) {
	h, _ := newTestHandler(t)

	cases := []struct {
		name    string
		handler http.HandlerFunc
		path    string
		found   string
	}{
		{"flag", h.GetFlagsByID, "/flags/", "7"},
		{"flag value", h.GetFlagsValueByID, "/flags/values/", "100"},
	}
	for _, c := range cases {
		if rec := call(c.handler, byID(http.MethodGet, c.path+"abc", "abc")); rec.Code != http.StatusBadRequest {
			t.Errorf("%s, non-numeric id: expected 400, got %d (%s)", c.name, rec.Code, rec.Body.String())
		}
		if rec := call(c.handler, byID(http.MethodGet, c.path+"999", "999")); rec.Code != http.StatusNotFound {
			t.Errorf("%s, unknown id: expected 404, got %d (%s)", c.name, rec.Code, rec.Body.String())
		}
		if rec := call(c.handler, byID(http.MethodGet, c.path+"0", "0")); rec.Code != http.StatusBadRequest {
			t.Errorf("%s, id 0: expected 400, got %d (%s)", c.name, rec.Code, rec.Body.String())
		}
		if rec := call(c.handler, byID(http.MethodGet, c.path+c.found, c.found)); rec.Code != http.StatusOK {
			t.Errorf("%s, seeded id: expected 200, got %d (%s)", c.name, rec.Code, rec.Body.String())
		}
	}
}

// A page the caller cannot have is a 400 rather than a silently clamped answer
// only for the parameters that are meaningless; an oversized ?limit is clamped,
// because a caller asking for too much still wants rows back.
func TestListFlagsAndValuesValidateTheirQuery(t *testing.T) {
	h, _ := newTestHandler(t)

	for _, query := range []string{"?limit=-1", "?offset=abc", "?limit=x"} {
		if rec := call(h.ListFlags, httptest.NewRequest(http.MethodGet, "/flags"+query, nil)); rec.Code != http.StatusBadRequest {
			t.Errorf("GET /flags%s: expected 400, got %d (%s)", query, rec.Code, rec.Body.String())
		}
		if rec := call(h.ListFlagValues, httptest.NewRequest(http.MethodGet, "/flags/values"+query, nil)); rec.Code != http.StatusBadRequest {
			t.Errorf("GET /flags/values%s: expected 400, got %d (%s)", query, rec.Code, rec.Body.String())
		}
	}
	if rec := call(h.ListFlagValues, httptest.NewRequest(http.MethodGet, "/flags/values?environmentId=x", nil)); rec.Code != http.StatusBadRequest {
		t.Errorf("a non-numeric environmentId: expected 400, got %d (%s)", rec.Code, rec.Body.String())
	}

	rec := call(h.ListFlags, httptest.NewRequest(http.MethodGet, "/flags?limit=100000&flagKey=search_v2", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var flags []Flag
	if err := json.Unmarshal(rec.Body.Bytes(), &flags); err != nil {
		t.Fatalf("decode list: %v (%s)", err, rec.Body.String())
	}
	if len(flags) != 1 || flags[0].FlagKey != "search_v2" {
		t.Fatalf("unexpected flags for search_v2: %+v", flags)
	}

	rec = call(h.ListFlagValues, httptest.NewRequest(http.MethodGet, "/flags/values?environmentId=1&limit=100000", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var rows []FlagValueRow
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode list: %v (%s)", err, rec.Body.String())
	}
	if len(rows) != 3 { // the seeded rows in environment 1
		t.Fatalf("expected 3 rows in environment 1, got %d (%+v)", len(rows), rows)
	}
}

// itoa keeps the request bodies above readable; strconv in the middle of a
// string literal does not.
func itoa(id int64) string {
	return strconv.FormatInt(id, 10)
}
