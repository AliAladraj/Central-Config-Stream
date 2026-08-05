package flagsconfig

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A lost optimistic-concurrency race is the one failure an admin UI has to be
// able to act on — it means "reload and try again", not "the server broke". It
// only reads that way if it arrives as 409 rather than 500.
func TestUpdateFlagValueConflictIs409(t *testing.T) {
	repo := &fakeRepo{
		flag:      &Flag{ID: 7, FlagKey: "search_v2"},
		updateErr: ErrConflict,
	}
	h := NewHandler(NewService(repo, nil))

	req := httptest.NewRequest(http.MethodPut, "/flags/values",
		strings.NewReader(`{"id":100,"value":"canary","enabled":0}`))
	rec := httptest.NewRecorder()

	h.UpdateFlagValue(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestUpdateFlagValueNotFoundIs404(t *testing.T) {
	repo := &fakeRepo{
		flag:      &Flag{ID: 7, FlagKey: "search_v2"},
		updateErr: ErrFlagNotFound,
	}
	h := NewHandler(NewService(repo, nil))

	req := httptest.NewRequest(http.MethodPut, "/flags/values",
		strings.NewReader(`{"id":100,"value":"canary","enabled":0}`))
	rec := httptest.NewRecorder()

	h.UpdateFlagValue(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d (%s)", rec.Code, rec.Body.String())
	}
}
