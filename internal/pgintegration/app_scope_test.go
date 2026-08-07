package pgintegration

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ErasedKyte/Central-Config-Stream/internal/app"
)

// The rest of this suite drives the repositories. This file drives the whole
// service — router, middleware chain, audit store and all — over a real
// PostgreSQL, because the security middleware has a Postgres branch of its own
// that nothing else executes.
//
// The gap it closes is specific. internal/app's end-to-end tests all set
// DBDriver "sqlite", and the scope lookup in middleware.go picks its bind
// placeholder from that same flag, so the branch a deployment runs was reachable
// from no test at all. What lived there was ":1" — Oracle syntax — which
// PostgreSQL answers with SQLSTATE 42601. The lookup reports failure, the guard
// treats an undeterminable environment as global, and every scoped token was
// refused every row-addressed write in every environment, including the ones its
// scope names. A green SQLite suite said nothing about it, and neither did the
// caller: the answer is a 403 that reads like a scope that was configured wrong.
//
// So these tests assert the scope decision through HTTP, not the SQL. Restore
// the ":1" and they fail; that is the whole point of them being here.

// apiTokens is one scoped credential and one full-scope one, the same shape
// docs/SECURITY.md documents. ci-dev covers environments 1 and 2, so the seeded
// rows in environment 1 are in scope for it and the environment-3 rows are not.
const apiTokens = "ci-dev:1|2:devsecret,release:*:prodsecret"

// prodRows are the environment-3 appsettings and bundle the shared seed does not
// have — a scoped token needs something it must not be able to reach in each of
// the three row-addressed domains, and the seed only supplies one for flags. The
// ids sit above the identity sequences the harness restarts, so a create in the
// same schema still gets a free id.
const prodRows = `
INSERT INTO CONFIG_MICROSERVICE_APPSETTINGS (ID, MICROSERVICE_ID, ENVIRONMENT_ID, SETTINGS_JSON) VALUES
    (903, 1, 3, '{"connectionString":"postgres://svc:Hunter2@prod-db:5432/app"}');
INSERT INTO CONFIG_LOCALIZATION (ID, MICROSERVICE_ID, ENVIRONMENT_ID, LOCALE, BUNDLE_JSON) VALUES
    (902, 1, 3, 'en-US', '{"catalog.title":"Production"}');
`

// newAPI builds the service over one test's private schema and serves it. It
// goes through app.NewApp rather than assembling the middleware by hand, so what
// answers these requests is what a deployment answers with — including the audit
// insert, which is the other half of what the bind placeholder broke.
func newAPI(t *testing.T, s *stack) *httptest.Server {
	t.Helper()

	if _, err := s.DB.Exec(prodRows); err != nil {
		t.Fatalf("seed the production rows: %v", err)
	}

	// The same schema the harness created, reached the same way: search_path is
	// a startup parameter, so every connection this second pool opens resolves
	// unqualified table names into it.
	dsn := dsnWith(t, dsnOrSkip(t), map[string]string{"search_path": s.Schema})

	instance, err := app.NewApp(&app.Config{
		DBDriver:       "postgres",
		DBConnString:   dsn,
		ServerPort:     ":0",
		AdminTokens:    apiTokens,
		WriteRateLimit: 120,
		PublishEnabled: false, // no NATS: every write here is a database write
	})
	if err != nil {
		t.Fatalf("build the service: %v", err)
	}
	// Before the harness drops the schema, so the pool is not left holding
	// connections whose search_path names something that no longer exists.
	t.Cleanup(func() {
		if err := instance.Close(); err != nil {
			t.Errorf("close the service: %v", err)
		}
	})

	api := httptest.NewServer(instance.Handler())
	t.Cleanup(api.Close)
	return api
}

// apiDo performs one request and returns the status and body. Every call here
// carries a token: the interesting answers are 200 and 403, and an accidental
// 401 would look like either.
func apiDo(t *testing.T, api *httptest.Server, method, path, token, body string) (int, string) {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, api.URL+path, reader)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := api.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s %s: %v", method, path, err)
	}
	return resp.StatusCode, string(got)
}

// waitForAudit polls for an audit row. The record is inserted after the response
// has been written — deliberately, so a failed insert cannot fail a write that
// already happened — which means the client can be back before the row is.
func waitForAudit(t *testing.T, s *stack, what, where string, args ...any) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if n := s.count(t, `SELECT COUNT(*) FROM CONFIG_AUDIT_LOG WHERE `+where, args...); n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no audit row for %s (WHERE %s, %v)", what, where, args)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A scoped token writing a row inside its own scope. This is the case the bind
// placeholder broke outright: the row is in environment 1 and the token names
// environment 1, and the answer was 403.
func TestScopedTokenWritesARowInsideItsScope(t *testing.T) {
	s := newStack(t)
	api := newAPI(t, s)

	// Seeded flag value 100 lives in environment 1.
	status, body := apiDo(t, api, http.MethodPut, "/flags/values", "devsecret",
		`{"id":100,"value":"canary","enabled":1}`)
	if status != http.StatusOK {
		t.Fatalf("PUT /flags/values on an in-scope row = %d (%s), want 200", status, strings.TrimSpace(body))
	}

	// The 403 was returned before the handler ran, so the row not changing is
	// the fact the status code stands for. Assert it directly.
	if n := s.count(t, `SELECT COUNT(*) FROM CONFIG_FLAG_VALUE WHERE ID = 100 AND VALUE = 'canary'`); n != 1 {
		t.Errorf("the write answered 200 but row 100 still holds its old value")
	}

	// The audit trail is the second casualty. lookupRowEnvironment failing left
	// sink.envID at zero, which the store writes as NULL — and NULL never
	// satisfies the IN the scoped /audit reader is narrowed with, so a scoped
	// operator could not see its own writes.
	waitForAudit(t, s, "the in-scope flag value update",
		`METHOD = 'PUT' AND PATH = '/flags/values' AND ACTOR = 'ci-dev' AND TARGET_ID = '100' AND ENVIRONMENT_ID = 1 AND STATUS_CODE = 200`)

	// A full-scope token's write of the same row records the environment too.
	// It never went through lookupRowEnvironment's failure path as a 403 — the
	// token allows everything — so this is the case that only ever showed up as
	// an ERROR line in the log.
	status, body = apiDo(t, api, http.MethodPut, "/flags/values", "prodsecret",
		`{"id":103,"value":"off","enabled":0}`)
	if status != http.StatusOK {
		t.Fatalf("PUT /flags/values as full scope = %d (%s), want 200", status, strings.TrimSpace(body))
	}
	waitForAudit(t, s, "the full-scope flag value update",
		`METHOD = 'PUT' AND ACTOR = 'release' AND TARGET_ID = '103' AND ENVIRONMENT_ID = 3`)
}

// The other direction, and the one that has to keep working: the environment is
// read from the row, so a token that does not name it is refused whatever the
// request says. Each of the three classRow* classes names a different table in
// that lookup, so all three are driven.
func TestScopedTokenIsRefusedRowsOutsideItsScope(t *testing.T) {
	s := newStack(t)
	api := newAPI(t, s)

	cases := []struct {
		name   string
		method string
		path   string
		body   string
		want   int
	}{
		// CONFIG_FLAG_VALUE: 100 is environment 1, 103 is environment 3.
		{"update an in-scope flag value", http.MethodPut, "/flags/values", `{"id":100,"value":"on","enabled":1}`, http.StatusOK},
		{"update a production flag value", http.MethodPut, "/flags/values", `{"id":103,"value":"on","enabled":1}`, http.StatusForbidden},
		// A body that names an environment the token does hold must not buy
		// access to a row that lives in one it does not.
		{"update a production flag value claiming environment 1", http.MethodPut, "/flags/values", `{"id":103,"environmentId":1,"value":"on","enabled":1}`, http.StatusForbidden},
		{"delete a production flag value", http.MethodDelete, "/flags/values/103", "", http.StatusForbidden},
		{"delete an in-scope flag value", http.MethodDelete, "/flags/values/102", "", http.StatusNoContent},

		// CONFIG_MICROSERVICE_APPSETTINGS: 200 is environment 1, 903 is 3.
		{"delete a production appsettings row", http.MethodDelete, "/configs/values/903", "", http.StatusForbidden},
		{"delete an in-scope appsettings row", http.MethodDelete, "/configs/values/202", "", http.StatusNoContent},

		// CONFIG_LOCALIZATION: 300 is environment 1, 902 is 3.
		{"delete a production bundle", http.MethodDelete, "/localization/902", "", http.StatusForbidden},
		{"delete an in-scope bundle", http.MethodDelete, "/localization/301", "", http.StatusNoContent},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := apiDo(t, api, tc.method, tc.path, "devsecret", tc.body)
			if status != tc.want {
				t.Errorf("%s %s = %d (%s), want %d", tc.method, tc.path, status, strings.TrimSpace(body), tc.want)
			}
		})
	}

	// Refused means refused: the production rows are all still there.
	for _, q := range []string{
		`SELECT COUNT(*) FROM CONFIG_FLAG_VALUE WHERE ID = 103`,
		`SELECT COUNT(*) FROM CONFIG_MICROSERVICE_APPSETTINGS WHERE ID = 903`,
		`SELECT COUNT(*) FROM CONFIG_LOCALIZATION WHERE ID = 902`,
	} {
		if n := s.count(t, q); n != 1 {
			t.Errorf("a refused write removed a production row (%s)", q)
		}
	}

	// And the refusals are in the trail, which is what shows a credential being
	// pointed at an environment it does not hold.
	waitForAudit(t, s, "the refused production delete",
		`METHOD = 'DELETE' AND TARGET_ID = '103' AND STATUS_CODE = 403`)
}

// TARGET_ID is VARCHAR(64) and the id it holds comes straight out of the request
// path, so an over-long one made the whole INSERT fail and the request left no
// trace — a way to probe credentials without appearing in the trail. It is
// truncated like PATH and REMOTE_ADDR now. SQLite ignores the declared width, so
// this is only ever visible here.
func TestAPaddedRowIDStillLeavesAnAuditRow(t *testing.T) {
	s := newStack(t)
	api := newAPI(t, s)

	padded := strings.Repeat("9", 65)
	status, _ := apiDo(t, api, http.MethodDelete, "/flags/values/"+padded, "devsecret", "")
	// The id does not parse as a row, so the target cannot be determined and the
	// guard fails closed against a scoped token. What matters is the row below.
	if status != http.StatusForbidden {
		t.Fatalf("DELETE with a padded id = %d, want 403", status)
	}

	waitForAudit(t, s, "the padded-id delete",
		`TARGET_ID = $1 AND STATUS_CODE = 403`, padded[:64])

	if n := s.count(t, `SELECT COUNT(*) FROM CONFIG_AUDIT_LOG WHERE LENGTH(TARGET_ID) > 64`); n != 0 {
		t.Errorf("%d audit rows carry a TARGET_ID over the column width", n)
	}
}

// An empty page is the ordinary end of a listing — docs/SECURITY.md tells a
// scoped caller to page until it sees one — and the repositories express it as a
// nil slice, which marshals to null. The scope filter had no arm for that, so the
// narrowing turned the last page of every listing into a 500 for exactly the
// callers it applies to. Postgres is where the nil slice really comes from, so
// this belongs here rather than against the mirror schema.
func TestScopedEmptyPageIsNotAnError(t *testing.T) {
	s := newStack(t)
	api := newAPI(t, s)

	for _, path := range []string{
		"/flags/values?offset=999",
		"/configs/values?offset=999",
		"/localization?offset=999",
		"/environments?offset=999",
		// Environment 2 is seeded with no rows at all, which is the same empty
		// answer arriving from a filter rather than from paging past the end.
		"/flags/values?environmentId=2",
	} {
		status, body := apiDo(t, api, http.MethodGet, path, "devsecret", "")
		if status != http.StatusOK {
			t.Errorf("GET %s as a scoped token = %d (%s), want 200", path, status, strings.TrimSpace(body))
			continue
		}
		if strings.TrimSpace(body) != "[]" {
			t.Errorf("GET %s answered %q, want []", path, strings.TrimSpace(body))
		}
	}

	// A full-scope token is not narrowed, so its empty page is still the null the
	// repositories produced — the split docs/openapi.yaml describes.
	status, body := apiDo(t, api, http.MethodGet, "/flags/values?offset=999", "prodsecret", "")
	if status != http.StatusOK || strings.TrimSpace(body) != "null" {
		t.Errorf("GET an empty page as full scope = %d %q, want 200 null", status, strings.TrimSpace(body))
	}
}
