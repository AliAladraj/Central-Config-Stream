package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ErasedKyte/Central-Config-Stream/internal/audit"
)

type fakeRecorder struct {
	mu      sync.Mutex
	entries []audit.Entry
	err     error
}

func (f *fakeRecorder) Insert(_ context.Context, e audit.Entry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, e)
	return f.err
}

func (f *fakeRecorder) all() []audit.Entry {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]audit.Entry(nil), f.entries...)
}

// routedEverywhere stands in for the mux's own answer in the tests that drive a
// single handler rather than the router.
func routedEverywhere(*http.Request) bool { return true }

func TestAuditRecordsWritesAndNotReads(t *testing.T) {
	rec := &fakeRecorder{}
	sec := &security{tokens: mustTokens(t, "alice:*:secret", "")}
	guarded := sec.guard(classEnvBody, "flagvalues", okHandler)
	handler := auditWrites(rec, routedEverywhere, guarded)

	req := httptest.NewRequest(http.MethodPost, "/flags/values",
		strings.NewReader(`{"environmentId":3,"flagId":7,"wsPassword":"hunter2"}`))
	req.Header.Set("Authorization", "Bearer secret")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	entries := rec.all()
	if len(entries) != 1 {
		t.Fatalf("got %d audit entries, want 1", len(entries))
	}
	e := entries[0]
	if e.Actor != "alice" {
		t.Errorf("actor %q, want alice", e.Actor)
	}
	if e.Method != http.MethodPost || e.Path != "/flags/values" {
		t.Errorf("unexpected envelope: %s %s", e.Method, e.Path)
	}
	if e.Domain != "flagvalues" || e.EnvironmentID != 3 || e.StatusCode != http.StatusOK {
		t.Errorf("unexpected target: domain=%q env=%d status=%d", e.Domain, e.EnvironmentID, e.StatusCode)
	}
	if strings.Contains(e.RequestBody, "hunter2") {
		t.Errorf("secret survived redaction: %s", e.RequestBody)
	}

	// A read is not audited.
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/flags/values", nil))
	if len(rec.all()) != 1 {
		t.Errorf("a read was audited: %d entries", len(rec.all()))
	}
}

// A rejected write is still worth a record — that is the trail that shows
// someone probing production with the wrong credential.
func TestAuditRecordsRejectedWrites(t *testing.T) {
	rec := &fakeRecorder{}
	sec := &security{tokens: mustTokens(t, "ci-dev:1|2:devsecret", "")}
	handler := auditWrites(rec, routedEverywhere, sec.guard(classEnvBody, "flagvalues", okHandler))

	req := httptest.NewRequest(http.MethodPost, "/flags/values",
		strings.NewReader(`{"environmentId":3,"flagId":7}`))
	req.Header.Set("Authorization", "Bearer devsecret")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", res.Code)
	}
	entries := rec.all()
	if len(entries) != 1 || entries[0].StatusCode != http.StatusForbidden || entries[0].Actor != "ci-dev" {
		t.Fatalf("rejected write not recorded correctly: %+v", entries)
	}
}

// A write addressed at no route changes nothing, so recording it buys nothing
// and costs a megabyte of body read, a full JSON walk and a synchronous insert
// — an unauthenticated way to flood the audit table and drown the real trail.
func TestAuditSkipsRequestsThatMatchNoRoute(t *testing.T) {
	rec := &fakeRecorder{}
	sec := &security{tokens: mustTokens(t, "", "secret")}

	routes := map[string]bool{"/flags": true}
	routed := func(r *http.Request) bool { return routes[r.URL.Path] }
	handler := auditWrites(rec, routed, sec.guard(classNeutral, "flags", okHandler))

	// A body big enough that buffering it would be the point of the flood.
	body := `{"padding":"` + strings.Repeat("x", 4096) + `"}`

	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/no-such-route", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer secret")
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}
	if got := len(rec.all()); got != 0 {
		t.Fatalf("%d unrouted writes were recorded", got)
	}

	// A real route is still always recorded, whatever it answers.
	req := httptest.NewRequest(http.MethodPost, "/flags", strings.NewReader(`{"flagKey":"x"}`))
	req.Header.Set("Authorization", "Bearer secret")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	rejected := httptest.NewRequest(http.MethodPost, "/flags", strings.NewReader(`{"flagKey":"x"}`))
	rejected.Header.Set("Authorization", "Bearer wrong")
	handler.ServeHTTP(httptest.NewRecorder(), rejected)

	entries := rec.all()
	if len(entries) != 2 {
		t.Fatalf("got %d records for two routed writes, want 2", len(entries))
	}
	if entries[0].StatusCode != http.StatusOK || entries[1].StatusCode != http.StatusUnauthorized {
		t.Errorf("routed writes recorded as %d and %d", entries[0].StatusCode, entries[1].StatusCode)
	}
}

// The same property through the real router and the real store: a POST to a
// path nothing serves leaves no row behind.
func TestUnroutedWritesLeaveNoAuditRow(t *testing.T) {
	api, db := newTestAPI(t, "", "test-token")

	post := func(path, token string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, api.URL+path, strings.NewReader(`{"flagKey":"probe"}`))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := api.Client().Do(req)
		if err != nil {
			t.Fatalf("post %s: %v", path, err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	count := func() int {
		t.Helper()
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM CONFIG_AUDIT_LOG").Scan(&n); err != nil {
			t.Fatalf("count audit rows: %v", err)
		}
		return n
	}

	for _, path := range []string{"/no-such-route", "/flags/values/../../etc", "/"} {
		post(path, "test-token")
	}
	if got := count(); got != 0 {
		t.Fatalf("%d audit rows written by requests matching no route", got)
	}

	if status := post("/flags", "test-token"); status != http.StatusCreated {
		t.Fatalf("POST /flags = %d, want 201", status)
	}
	if got := count(); got != 1 {
		t.Errorf("a routed write produced %d audit rows, want 1", got)
	}
}

// The audit write happens after the change is already committed and published,
// so a failing audit store must not turn a successful change into an error.
func TestAuditFailureDoesNotFailRequest(t *testing.T) {
	rec := &fakeRecorder{err: errors.New("audit table unavailable")}
	sec := &security{tokens: mustTokens(t, "", "secret")}
	handler := auditWrites(rec, routedEverywhere, sec.guard(classNeutral, "flags", okHandler))

	req := httptest.NewRequest(http.MethodPost, "/flags", strings.NewReader(`{"flagKey":"x"}`))
	req.Header.Set("Authorization", "Bearer secret")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("audit failure changed the response: %d", res.Code)
	}
}
