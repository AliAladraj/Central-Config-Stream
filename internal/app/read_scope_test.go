package app

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// prodSeed adds the environment-3 rows the shipped fixture is missing, so a
// scoped token has something it must not be able to reach. The appsettings row
// carries a connection string because that is exactly what a leaked production
// appsettings tree hands over.
const prodSeed = `
INSERT OR IGNORE INTO CONFIG_MICROSERVICE_APPSETTINGS (ID, MICROSERVICE_ID, ENVIRONMENT_ID, SETTINGS_JSON) VALUES
    (203, 1, 3, '{"connectionString":"mongodb://svc:Hunter2@prod-db:27017/app"}');
INSERT OR IGNORE INTO CONFIG_LOCALIZATION (ID, MICROSERVICE_ID, ENVIRONMENT_ID, LOCALE, BUNDLE_JSON) VALUES
    (302, 1, 3, 'en-US', '{"catalog.title":"Production"}');
`

// newTestAPI brings up the real server handler over the SQLite fixture: the
// same middleware chain, the same router, no NATS. Nothing here stubs the
// security layer, so what these tests exercise is what a deployment serves.
func newTestAPI(t *testing.T, named, shared string) (*httptest.Server, *sql.DB) {
	t.Helper()

	cfg := &Config{
		DBDriver:       "sqlite",
		DBConnString:   "file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "api.db")),
		ServerPort:     ":0",
		AdminTokens:    named,
		AdminToken:     shared,
		WriteRateLimit: defaultWriteRateLimit,
	}

	app, err := NewApp(cfg)
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	t.Cleanup(func() { _ = app.db.Close() })

	if _, err := app.db.Exec(prodSeed); err != nil {
		t.Fatalf("seed production rows: %v", err)
	}

	api := httptest.NewServer(app.server.Handler)
	t.Cleanup(api.Close)
	return api, app.db
}

// apiGet performs one read, with the bearer token when one is given.
func apiGet(t *testing.T, api *httptest.Server, path, token string) (int, string) {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, api.URL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := api.Client().Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return resp.StatusCode, string(body)
}

// readRoutes is every route that answers with configuration data.
var readRoutes = []string{
	"/inventory",
	"/audit",
	"/environments",
	"/microservices",
	"/configs/1",
	"/configs/values",
	"/configs/values/200",
	"/flags",
	"/flags/7",
	"/flags/values",
	"/flags/values/100",
	"/localization",
	"/localization/300",
	"/localization/lookup/1/1/en-US",
}

// Reads used to be open, so `curl http://host/inventory` returned the whole
// configuration estate — every appsettings tree and every bundle, production
// included — with no token and no audit entry behind it.
func TestReadsRequireAToken(t *testing.T) {
	api, _ := newTestAPI(t, "", "test-token")

	for _, path := range readRoutes {
		status, body := apiGet(t, api, path, "")
		if status != http.StatusUnauthorized {
			t.Errorf("anonymous GET %s = %d, want 401", path, status)
		}
		for _, leaked := range []string{"catalog.title", "Hunter2", "search_v2", "catalog-api"} {
			if strings.Contains(body, leaked) {
				t.Errorf("anonymous GET %s disclosed %q: %s", path, leaked, body)
			}
		}
	}

	// A wrong token is no better than none.
	if status, _ := apiGet(t, api, "/inventory", "not-the-token"); status != http.StatusUnauthorized {
		t.Errorf("GET /inventory with a bad token = %d, want 401", status)
	}

	// The token that the compose stack and the bundled console send still works.
	for _, path := range readRoutes {
		if status, _ := apiGet(t, api, path, "test-token"); status != http.StatusOK {
			t.Errorf("GET %s with the admin token = %d, want 200", path, status)
		}
	}

	// The probe and scrape routes stay open: an orchestrator and a Prometheus
	// scrape hold no credential, and none of the three carries configuration.
	for _, path := range []string{"/health", "/livez", "/metrics"} {
		if status, _ := apiGet(t, api, path, ""); status != http.StatusOK {
			t.Errorf("anonymous GET %s = %d, want 200", path, status)
		}
	}
}

// A token scoped to dev and staging must not be able to read production
// through any route, whether it lists rows or fetches one by id.
func TestScopedTokenCannotReadAnotherEnvironment(t *testing.T) {
	api, _ := newTestAPI(t, "ci-dev:1|2:devsecret,release:*:prodsecret", "")

	// Listings: the production rows are simply not in them.
	for _, tc := range []struct {
		path    string
		wanted  string
		refused string
	}{
		{"/flags/values", `"id":100`, `"id":103`},
		{"/configs/values", `"id":200`, `"id":203`},
		{"/localization", `"id":300`, `"id":302`},
		{"/environments", `"name":"dev"`, `"name":"prod"`},
	} {
		status, body := apiGet(t, api, tc.path, "devsecret")
		if status != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", tc.path, status)
		}
		if !strings.Contains(body, tc.wanted) {
			t.Errorf("GET %s dropped a row the token may read: %s", tc.path, body)
		}
		if strings.Contains(body, tc.refused) {
			t.Errorf("GET %s leaked a production row: %s", tc.path, body)
		}
		if strings.Contains(body, "Hunter2") {
			t.Errorf("GET %s leaked a production connection string: %s", tc.path, body)
		}
	}

	// By id: a production row answers the same as one that does not exist.
	for _, path := range []string{
		"/flags/values/103",
		"/configs/values/203",
		"/localization/302",
		"/localization/lookup/1/3/en-US",
	} {
		status, body := apiGet(t, api, path, "devsecret")
		if status != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, status)
		}
		if strings.Contains(body, "Hunter2") || strings.Contains(body, "Production") {
			t.Errorf("GET %s leaked the row it refused: %s", path, body)
		}
	}

	// The rows the token does cover still come back whole.
	for _, path := range []string{"/flags/values/100", "/configs/values/200", "/localization/300"} {
		if status, _ := apiGet(t, api, path, "devsecret"); status != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 — the token covers environment 1", path, status)
		}
	}

	// /inventory hands back every editable row at once, so it is the one that
	// matters most.
	status, body := apiGet(t, api, "/inventory", "devsecret")
	if status != http.StatusOK {
		t.Fatalf("GET /inventory = %d, want 200", status)
	}
	if strings.Contains(body, `"environmentId":3`) || strings.Contains(body, "Hunter2") {
		t.Errorf("/inventory disclosed production to a dev token: %s", body)
	}
	if !strings.Contains(body, `"environmentId":1`) {
		t.Errorf("/inventory dropped the rows the token may read: %s", body)
	}

	// A full-scope token still sees everything, including what it holds.
	status, body = apiGet(t, api, "/inventory", "prodsecret")
	if status != http.StatusOK {
		t.Fatalf("GET /inventory as full scope = %d, want 200", status)
	}
	if !strings.Contains(body, `"environmentId":3`) || !strings.Contains(body, "Hunter2") {
		t.Errorf("full scope lost the production rows: %s", body)
	}
}

// /inventory returned every row in the estate in one unbounded response. It
// pages on the shared web.ParsePage convention now, like every other listing.
func TestInventoryPages(t *testing.T) {
	api, _ := newTestAPI(t, "", "test-token")

	decode := func(query string) Inventory {
		t.Helper()
		status, body := apiGet(t, api, "/inventory"+query, "test-token")
		if status != http.StatusOK {
			t.Fatalf("GET /inventory%s = %d, want 200", query, status)
		}
		var inv Inventory
		if err := json.Unmarshal([]byte(body), &inv); err != nil {
			t.Fatalf("decode inventory: %v", err)
		}
		return inv
	}

	all := decode("")
	if len(all.Flags) < 2 || len(all.MicroConfigs) < 2 || len(all.Localization) < 2 {
		t.Fatalf("fixture is too small to page: %+v", all)
	}

	first := decode("?limit=1")
	if len(first.Flags) != 1 || len(first.MicroConfigs) != 1 || len(first.Localization) != 1 {
		t.Fatalf("?limit=1 returned %d/%d/%d rows, want one of each",
			len(first.Flags), len(first.MicroConfigs), len(first.Localization))
	}

	second := decode("?limit=1&offset=1")
	if len(second.Flags) != 1 || second.Flags[0].ID == first.Flags[0].ID {
		t.Errorf("?offset=1 did not move the page: %+v then %+v", first.Flags, second.Flags)
	}

	if status, _ := apiGet(t, api, "/inventory?limit=nope", "test-token"); status != http.StatusBadRequest {
		t.Errorf("unparseable limit = %d, want 400", status)
	}
}

// The repositories build an empty listing into a nil slice, which marshals to
// null rather than []. The scope filter had no arm for that shape, so it fell to
// the default — "a body that does not decode" — and answered 500. Every listing
// ends in an empty page, and docs/SECURITY.md tells a scoped caller to page
// until it sees one, so this was a 500 on the ordinary end of every walk.
func TestScopedTokenGetsAnEmptyPageNotAnError(t *testing.T) {
	api, _ := newTestAPI(t, "ci-dev:1|2:devsecret,release:*:prodsecret", "")

	for _, path := range []string{
		"/flags/values?offset=999",
		"/configs/values?offset=999",
		"/localization?offset=999",
		"/environments?offset=999",
		// The same empty answer arriving from a filter rather than from paging
		// past the end: nothing is seeded in environment 2.
		"/flags/values?environmentId=2",
	} {
		status, body := apiGet(t, api, path, "devsecret")
		if status != http.StatusOK {
			t.Errorf("GET %s as a scoped token = %d (%s), want 200", path, status, strings.TrimSpace(body))
			continue
		}
		if strings.TrimSpace(body) != "[]" {
			t.Errorf("GET %s answered %q, want []", path, strings.TrimSpace(body))
		}
	}

	// A full-scope token is not narrowed at all, so its empty page is still the
	// null the repository produced — the split docs/openapi.yaml describes.
	status, body := apiGet(t, api, "/flags/values?offset=999", "prodsecret")
	if status != http.StatusOK || strings.TrimSpace(body) != "null" {
		t.Errorf("GET an empty page as full scope = %d %q, want 200 null", status, strings.TrimSpace(body))
	}
}

// The audit trail carries the request bodies of every environment, so a
// dev-scoped credential reading it used to be a way around the write scope it
// is subject to.
func TestAuditIsFilteredByScope(t *testing.T) {
	api, _ := newTestAPI(t, "ci-dev:1|2:devsecret,release:*:prodsecret", "")

	// Two writes by the full-scope token, one per environment.
	for _, w := range []struct{ env, flag int }{{1, 8}, {3, 8}} {
		body := fmt.Sprintf(`{"environmentId":%d,"flagId":%d,"value":"canary","enabled":1}`, w.env, w.flag)
		req, err := http.NewRequest(http.MethodPost, api.URL+"/flags/values", strings.NewReader(body))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer prodsecret")
		req.Header.Set("Content-Type", "application/json")
		resp, err := api.Client().Do(req)
		if err != nil {
			t.Fatalf("post flag value: %v", err)
		}
		_ = resp.Body.Close()
	}

	status, body := apiGet(t, api, "/audit", "devsecret")
	if status != http.StatusOK {
		t.Fatalf("GET /audit = %d, want 200", status)
	}
	var entries []map[string]any
	if err := json.Unmarshal([]byte(body), &entries); err != nil {
		t.Fatalf("decode audit: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the dev token saw none of its own environment's trail")
	}
	for _, e := range entries {
		if env, _ := e["environmentId"].(float64); env != 1 && env != 2 {
			t.Errorf("dev token read an audit row for environment %v: %v", e["environmentId"], e)
		}
	}

	// Full scope reads the whole trail, production rows included.
	status, body = apiGet(t, api, "/audit", "prodsecret")
	if status != http.StatusOK {
		t.Fatalf("GET /audit as full scope = %d, want 200", status)
	}
	if !strings.Contains(body, `"environmentId":3`) {
		t.Errorf("full scope lost the production trail: %s", body)
	}
}

// nosniff belongs on every response, and HSTS only on a connection that really
// is TLS — over plain HTTP it promises nothing.
func TestSecurityResponseHeaders(t *testing.T) {
	api, _ := newTestAPI(t, "", "test-token")

	resp, err := api.Client().Get(api.URL + "/livez")
	if err != nil {
		t.Fatalf("get /livez: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := resp.Header.Get("Strict-Transport-Security"); got != "" {
		t.Errorf("HSTS sent over plain HTTP: %q", got)
	}

	// A 401 is a response too, and the one an unauthenticated scan sees most.
	status, _ := apiGet(t, api, "/inventory", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("GET /inventory = %d, want 401", status)
	}
}
