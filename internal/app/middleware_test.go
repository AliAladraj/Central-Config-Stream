package app

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ErasedKyte/Central-Config-Stream/internal/database"
)

func okHandler(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }

func mustTokens(t *testing.T, named, shared string) *tokenSet {
	t.Helper()
	set, err := parseAdminTokens(named, shared)
	if err != nil {
		t.Fatalf("parse admin tokens: %v", err)
	}
	return set
}

// call drives a guarded handler and reports the status it produced.
func call(t *testing.T, h http.HandlerFunc, method, target, auth, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// ADMIN_TOKEN is the single-credential form: unset disables the check, and a
// set value is accepted only when it arrives as a bearer token.
func TestGuardSharedToken(t *testing.T) {
	cases := []struct {
		name   string
		shared string
		header string
		want   int
	}{
		{"disabled when unset", "", "", http.StatusOK},
		{"valid token", "secret", "Bearer secret", http.StatusOK},
		{"missing header", "secret", "", http.StatusUnauthorized},
		{"wrong token", "secret", "Bearer nope", http.StatusUnauthorized},
		{"no bearer prefix", "secret", "secret", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sec := &security{tokens: mustTokens(t, "", tc.shared)}
			h := sec.guard(classNeutral, "flags", okHandler)
			if got := call(t, h, http.MethodPost, "/flags", tc.header, "").Code; got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// The ADMIN_TOKEN credential is full scope, so it may write any environment and
// issue the writes that reach every environment.
func TestSharedTokenIsFullScope(t *testing.T) {
	sec := &security{tokens: mustTokens(t, "", "secret")}

	prod := sec.guard(classEnvBody, "flagvalues", okHandler)
	if got := call(t, prod, http.MethodPost, "/flags/values", "Bearer secret", `{"environmentId":3,"flagId":7}`).Code; got != http.StatusOK {
		t.Errorf("shared token rejected on prod write: %d", got)
	}

	global := sec.guard(classGlobal, "flags", okHandler)
	if got := call(t, global, http.MethodDelete, "/flags/7", "Bearer secret", "").Code; got != http.StatusOK {
		t.Errorf("shared token rejected on global write: %d", got)
	}
}

// The point of named tokens: a dev/staging credential cannot touch prod.
func TestScopedTokenCannotWriteProduction(t *testing.T) {
	sec := &security{tokens: mustTokens(t, "ci-dev:1|2:devsecret,release:*:prodsecret", "")}
	h := sec.guard(classEnvBody, "flagvalues", okHandler)

	cases := []struct {
		name  string
		token string
		body  string
		want  int
	}{
		{"dev token on dev", "devsecret", `{"environmentId":1,"flagId":7}`, http.StatusOK},
		{"dev token on staging", "devsecret", `{"environmentId":2,"flagId":7}`, http.StatusOK},
		{"dev token on prod", "devsecret", `{"environmentId":3,"flagId":7}`, http.StatusForbidden},
		{"full scope on prod", "prodsecret", `{"environmentId":3,"flagId":7}`, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := call(t, h, http.MethodPost, "/flags/values", "Bearer "+tc.token, tc.body).Code; got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}

	// A write whose environment cannot be pinned down is only for full scope.
	global := sec.guard(classGlobal, "environments", okHandler)
	if got := call(t, global, http.MethodPost, "/environments", "Bearer devsecret", `{"name":"qa"}`).Code; got != http.StatusForbidden {
		t.Errorf("scoped token allowed a global write: %d", got)
	}

	// DELETE /environments/{id} names its environment in the path.
	byPath := sec.guard(classEnvPath, "environments", okHandler)
	if got := deleteByID(t, byPath, "/environments/3", "3", "devsecret"); got != http.StatusForbidden {
		t.Errorf("scoped token allowed deleting prod: %d", got)
	}
	if got := deleteByID(t, byPath, "/environments/1", "1", "devsecret"); got != http.StatusOK {
		t.Errorf("scoped token blocked deleting dev: %d", got)
	}
}

// deleteByID drives a {id} route the way the mux would, with the path value set.
func deleteByID(t *testing.T, h http.HandlerFunc, target, id, token string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, target, nil)
	req.SetPathValue("id", id)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec.Code
}

// The PUT and delete-by-id routes key off the row id, so the environment
// has to come from the row rather than from whatever the body claims.
func TestRowScopeReadsEnvironmentFromDatabase(t *testing.T) {
	db, err := database.NewSQLiteDB("file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "scope.db")))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	sec := &security{tokens: mustTokens(t, "ci-dev:1|2:devsecret", ""), db: db, sqlite: true}
	h := sec.guard(classRowFlagValue, "flagvalues", okHandler)

	// Seeded row 100 lives in environment 1, row 103 in environment 3.
	if got := call(t, h, http.MethodPut, "/flags/values", "Bearer devsecret", `{"id":100,"value":"x"}`).Code; got != http.StatusOK {
		t.Errorf("dev row rejected: %d", got)
	}
	if got := call(t, h, http.MethodPut, "/flags/values", "Bearer devsecret", `{"id":103,"value":"x"}`).Code; got != http.StatusForbidden {
		t.Errorf("prod row allowed: %d", got)
	}
	// A body that lies about the environment must not buy access to the row.
	if got := call(t, h, http.MethodPut, "/flags/values", "Bearer devsecret", `{"id":103,"environmentId":1}`).Code; got != http.StatusForbidden {
		t.Errorf("body environmentId overrode the stored row: %d", got)
	}

	if got := deleteByID(t, h, "/flags/values/103", "103", "devsecret"); got != http.StatusForbidden {
		t.Errorf("delete of a prod row allowed: %d", got)
	}
	if got := deleteByID(t, h, "/flags/values/100", "100", "devsecret"); got != http.StatusOK {
		t.Errorf("delete of a dev row blocked: %d", got)
	}
}

// Moving a row between environments touches both, so a token needs both.
func TestRowScopeCoversEnvironmentMove(t *testing.T) {
	db, err := database.NewSQLiteDB("file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "move.db")))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	sec := &security{tokens: mustTokens(t, "ci-dev:1|2:devsecret", ""), db: db, sqlite: true}
	h := sec.guard(classRowMicroConfig, "microconfig", okHandler)

	// Row 200 is in environment 1; the update rewrites ENVIRONMENT_ID.
	body := `{"id":200,"microserviceId":1,"environmentId":3,"settingsJson":{}}`
	if got := call(t, h, http.MethodPut, "/configs/values", "Bearer devsecret", body).Code; got != http.StatusForbidden {
		t.Errorf("scoped token moved a row into prod: %d", got)
	}
}

// The guard must leave the body readable for the handler behind it.
func TestGuardPreservesRequestBody(t *testing.T) {
	sec := &security{tokens: mustTokens(t, "", "secret")}
	var seen string
	h := sec.guard(classEnvBody, "flagvalues", func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 64)
		n, _ := r.Body.Read(body)
		seen = string(body[:n])
		w.WriteHeader(http.StatusOK)
	})

	const body = `{"environmentId":1,"flagId":7}`
	call(t, h, http.MethodPost, "/flags/values", "Bearer secret", body)
	if seen != body {
		t.Errorf("handler saw %q, want %q", seen, body)
	}
}

func TestParseAdminTokensRejectsMalformedEntries(t *testing.T) {
	cases := []string{
		"missing-scope-and-secret",
		"name:1|2",
		":*:secret",
		"name:*:",
		"name:notanumber:secret",
		"name::secret",
	}
	for _, entry := range cases {
		if _, err := parseAdminTokens(entry, ""); err == nil {
			t.Errorf("parseAdminTokens(%q) accepted a malformed entry", entry)
		}
	}
}

// The write limiter answers 429 with a Retry-After rather than letting a script
// hammer Oracle and the KV buckets.
func TestRateLimiterReturns429(t *testing.T) {
	sec := &security{
		tokens:  mustTokens(t, "", "secret"),
		limiter: newRateLimiter(1), // one write per minute
	}
	h := sec.guard(classNeutral, "flags", okHandler)

	if got := call(t, h, http.MethodPost, "/flags", "Bearer secret", "").Code; got != http.StatusOK {
		t.Fatalf("first write rejected: %d", got)
	}

	rec := call(t, h, http.MethodPost, "/flags", "Bearer secret", "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second write got %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 without a Retry-After header")
	}

	// A different credential has its own budget.
	sec2 := &security{tokens: mustTokens(t, "other:*:another", "secret"), limiter: sec.limiter}
	h2 := sec2.guard(classNeutral, "flags", okHandler)
	if got := call(t, h2, http.MethodPost, "/flags", "Bearer another", "").Code; got != http.StatusOK {
		t.Errorf("a second caller was charged the first caller's budget: %d", got)
	}
}
