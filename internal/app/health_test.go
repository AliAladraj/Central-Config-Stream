package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ErasedKyte/Central-Config-Stream/internal/database"
)

func decodeHealth(t *testing.T, a *App) (int, map[string]string) {
	t.Helper()
	rec := httptest.NewRecorder()
	a.healthHandler()(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode health body: %v", err)
	}
	return rec.Code, body
}

// The health check has to report what it actually observed. A nil NATS client
// means PUBLISH_ENABLED=false, which is a configured mode rather than an
// outage; a database that no longer answers is a genuine 503.
func TestHealthHandlerReportsRealState(t *testing.T) {
	db, err := database.NewSQLiteDB("file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "health.db")))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	a := &App{db: db}

	code, body := decodeHealth(t, a)
	if code != http.StatusOK {
		t.Fatalf("expected 200 with a live database, got %d", code)
	}
	if body["database"] != "ok" || body["nats"] != "disabled" || body["status"] != "ok" {
		t.Fatalf("unexpected healthy body: %v", body)
	}

	// A closed pool stands in for a database that has gone away.
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	code, body = decodeHealth(t, a)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 once the database is down, got %d", code)
	}
	if body["database"] != "unavailable" || body["status"] != "degraded" {
		t.Fatalf("unexpected degraded body: %v", body)
	}
}
