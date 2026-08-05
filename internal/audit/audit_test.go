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
