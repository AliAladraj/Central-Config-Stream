package audit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ErasedKyte/Central-Config-Stream/internal/database"
)

func TestRedact(t *testing.T) {
	body := []byte(`{"storage":{"accessKeyId":"key-id-1234","secretAccessKey":"s3cret"},"list":[{"apiKey":"abc"}],"http":{"timeoutMs":2000}}`)

	out := Redact(body)
	for _, leaked := range []string{"s3cret", "abc"} {
		if strings.Contains(out, leaked) {
			t.Errorf("secret %q survived redaction: %s", leaked, out)
		}
	}
	if !strings.Contains(out, "key-id-1234") || !strings.Contains(out, "2000") {
		t.Errorf("non-secret values were dropped: %s", out)
	}

	if got := Redact(nil); got != "" {
		t.Errorf("empty body produced %q", got)
	}
	if got := Redact([]byte("not json")); got != bodyNotJSON {
		t.Errorf("non-JSON body produced %q", got)
	}
}

// The fragment list is what stands between a settings tree and an audit table
// read by more people than the write API is used by. Every name here is one a
// real appsettings tree spells a credential with.
func TestRedactCoversCredentialFieldNames(t *testing.T) {
	cases := []struct {
		field string
		value string
	}{
		{"authorization", "Bearer prod-admin-token"},
		{"Authorization", "Bearer prod-admin-token"},
		{"connectionString", "mongodb://svc:Hunter2@prod-db:27017/app"},
		{"connection_string", "mongodb://svc:Hunter2@prod-db:27017/app"},
		{"connString", "Server=prod;Password=Hunter2"},
		{"conn_string", "Server=prod;Password=Hunter2"},
		{"dsn", "postgres://svc:Hunter2@prod-db:5432/app"},
		{"signingKey", "MIIEvQIBADANBg"},
		{"signing_key", "MIIEvQIBADANBg"},
		{"cert", "-----BEGIN CERTIFICATE-----AAAA"},
		{"clientCertificate", "-----BEGIN CERTIFICATE-----AAAA"},
		{"pem", "-----BEGIN RSA PRIVATE KEY-----AAAA"},
		{"keyPem", "-----BEGIN RSA PRIVATE KEY-----AAAA"},
		{"sessionId", "s%3AabcDEF123"},
		{"session_id", "s%3AabcDEF123"},
	}

	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{
				"environmentId": 3,
				"settingsJson":  map[string]any{"outer": map[string]any{tc.field: tc.value}},
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			out := Redact(body)
			if strings.Contains(out, tc.value) {
				t.Errorf("%s survived redaction: %s", tc.field, out)
			}
			if !strings.Contains(out, `"environmentId":3`) {
				t.Errorf("the envelope was lost with it: %s", out)
			}
		})
	}

	// The names an audit reader needs are not swept up with them.
	out := Redact([]byte(`{"locale":"pt-BR","flagKey":"search_v2","timeoutMs":2000}`))
	for _, kept := range []string{"pt-BR", "search_v2", "2000"} {
		if !strings.Contains(out, kept) {
			t.Errorf("a harmless value was redacted, %q is missing: %s", kept, out)
		}
	}
}

func TestRedactTruncatesOversizedBodies(t *testing.T) {
	big, err := json.Marshal(map[string]string{"value": strings.Repeat("x", MaxStoredBody*2)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	out := Redact(big)
	if len(out) > MaxStoredBody+len(bodyTruncated) {
		t.Errorf("stored body is %d bytes, over the column budget", len(out))
	}
	if !strings.HasSuffix(out, bodyTruncated) {
		t.Error("truncated body is not marked as such")
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := database.NewSQLiteDB("file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "audit.db")))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewStore(db, "sqlite")
}

func TestStoreRoundTripAndFilters(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	entries := []Entry{
		{OccurredAt: now.Add(-48 * time.Hour), Actor: "alice", Method: "PUT", Path: "/flags/values", Domain: "flagvalues", TargetID: "100", EnvironmentID: 1, StatusCode: 200, RequestBody: `{"id":100}`},
		{OccurredAt: now.Add(-time.Hour), Actor: "bob", Method: "POST", Path: "/flags", Domain: "flags", StatusCode: 201},
		{OccurredAt: now, Actor: "alice", Method: "DELETE", Path: "/flags/7", Domain: "flags", TargetID: "7", StatusCode: 204},
	}
	for _, e := range entries {
		if err := store.Insert(ctx, e); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	all, err := store.List(ctx, Filter{Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d entries, want 3", len(all))
	}
	// Newest first.
	if all[0].Method != "DELETE" {
		t.Errorf("unexpected order, first is %s %s", all[0].Method, all[0].Path)
	}
	// The optional columns survive the round trip, including the empty ones.
	if all[2].EnvironmentID != 1 || all[2].TargetID != "100" || all[2].RequestBody != `{"id":100}` {
		t.Errorf("round trip lost detail: %+v", all[2])
	}
	if all[1].EnvironmentID != 0 || all[1].TargetID != "" {
		t.Errorf("null columns did not scan as zero values: %+v", all[1])
	}

	byActor, err := store.List(ctx, Filter{Actor: "alice", Limit: 10})
	if err != nil {
		t.Fatalf("list by actor: %v", err)
	}
	if len(byActor) != 2 {
		t.Fatalf("actor filter returned %d entries, want 2", len(byActor))
	}

	byRange, err := store.List(ctx, Filter{From: now.Add(-2 * time.Hour), Limit: 10})
	if err != nil {
		t.Fatalf("list by range: %v", err)
	}
	if len(byRange) != 2 {
		t.Fatalf("date range returned %d entries, want 2", len(byRange))
	}

	page, err := store.List(ctx, Filter{Limit: 1, Offset: 1})
	if err != nil {
		t.Fatalf("list page: %v", err)
	}
	if len(page) != 1 || page[0].Method != "POST" {
		t.Fatalf("pagination returned %+v", page)
	}
}

// A reader's environment scope narrows the trail the same way it narrows a
// write: the rows carry request bodies, so reading them all is reading every
// environment's writes.
func TestListNarrowsToTheReaderScope(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	for _, e := range []Entry{
		{OccurredAt: now, Actor: "release", Method: "PUT", Path: "/flags/values", EnvironmentID: 1, StatusCode: 200, RequestBody: `{"env":1}`},
		{OccurredAt: now, Actor: "release", Method: "PUT", Path: "/flags/values", EnvironmentID: 2, StatusCode: 200, RequestBody: `{"env":2}`},
		{OccurredAt: now, Actor: "release", Method: "PUT", Path: "/flags/values", EnvironmentID: 3, StatusCode: 200, RequestBody: `{"env":3}`},
		{OccurredAt: now, Actor: "release", Method: "POST", Path: "/flags", StatusCode: 201, RequestBody: `{"flagKey":"x"}`},
	} {
		if err := store.Insert(ctx, e); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	scoped, err := store.List(ctx, Filter{Environments: []int64{1, 2}, Limit: 10})
	if err != nil {
		t.Fatalf("list scoped: %v", err)
	}
	if len(scoped) != 2 {
		t.Fatalf("a 1|2 reader saw %d rows, want 2: %+v", len(scoped), scoped)
	}
	for _, e := range scoped {
		if e.EnvironmentID != 1 && e.EnvironmentID != 2 {
			t.Errorf("a 1|2 reader saw environment %d", e.EnvironmentID)
		}
	}

	// A nil scope is full scope, and reads the rows no environment owns too.
	all, err := store.List(ctx, Filter{Limit: 10})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 4 {
		t.Errorf("full scope saw %d rows, want 4", len(all))
	}

	// An empty scope is not the same as no scope.
	none, err := store.List(ctx, Filter{Environments: []int64{}, Limit: 10})
	if err != nil {
		t.Fatalf("list empty scope: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("an empty scope returned %d rows", len(none))
	}
}

// The handler takes its scope from the request the admin API narrowed, not
// from a query parameter a caller could simply widen.
func TestHandlerAppliesTheRequestScope(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	for _, e := range []Entry{
		{OccurredAt: now, Actor: "release", Method: "PUT", Path: "/flags/values", EnvironmentID: 1, StatusCode: 200},
		{OccurredAt: now, Actor: "release", Method: "PUT", Path: "/flags/values", EnvironmentID: 3, StatusCode: 200},
	} {
		if err := store.Insert(ctx, e); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	h := NewHandler(store)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, WithEnvironments(httptest.NewRequest(http.MethodGet, "/audit", nil), []int64{1}))

	var got []Entry
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].EnvironmentID != 1 {
		t.Fatalf("the narrowed request returned %+v", got)
	}

	// Widening it from the query string is not on offer.
	wide := httptest.NewRecorder()
	h.ServeHTTP(wide, WithEnvironments(
		httptest.NewRequest(http.MethodGet, "/audit?environmentId=3", nil), []int64{1}))
	if strings.Contains(wide.Body.String(), `"environmentId":3`) {
		t.Errorf("a query parameter widened the scope: %s", wide.Body.String())
	}
}

func TestHandlerServesFilteredEntries(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	for _, e := range []Entry{
		{OccurredAt: now, Actor: "alice", Method: "POST", Path: "/flags", StatusCode: 201},
		{OccurredAt: now, Actor: "bob", Method: "POST", Path: "/flags", StatusCode: 201},
	} {
		if err := store.Insert(ctx, e); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	h := NewHandler(store)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/audit?actor=bob", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	var got []Entry
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].Actor != "bob" {
		t.Fatalf("actor filter returned %+v", got)
	}

	bad := httptest.NewRecorder()
	h.ServeHTTP(bad, httptest.NewRequest(http.MethodGet, "/audit?from=yesterday", nil))
	if bad.Code != http.StatusBadRequest {
		t.Errorf("unparseable from produced %d, want 400", bad.Code)
	}
}
