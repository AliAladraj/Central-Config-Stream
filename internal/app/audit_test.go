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

func TestAuditRecordsWritesAndNotReads(t *testing.T) {
	rec := &fakeRecorder{}
	sec := &security{tokens: mustTokens(t, "alice:*:secret", "")}
	guarded := sec.guard(classEnvBody, "flagvalues", okHandler)
	handler := auditWrites(rec, guarded)

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
	handler := auditWrites(rec, sec.guard(classEnvBody, "flagvalues", okHandler))

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

// The audit write happens after the change is already committed and published,
// so a failing audit store must not turn a successful change into an error.
func TestAuditFailureDoesNotFailRequest(t *testing.T) {
	rec := &fakeRecorder{err: errors.New("audit table unavailable")}
	sec := &security{tokens: mustTokens(t, "", "secret")}
	handler := auditWrites(rec, sec.guard(classNeutral, "flags", okHandler))

	req := httptest.NewRequest(http.MethodPost, "/flags", strings.NewReader(`{"flagKey":"x"}`))
	req.Header.Set("Authorization", "Bearer secret")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("audit failure changed the response: %d", res.Code)
	}
}
