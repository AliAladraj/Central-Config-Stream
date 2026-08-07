package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AliAladraj/Central-Config-Stream/internal/obs"
)

// serveObserved runs one request through the access-log middleware with logging
// captured, and returns the response plus the decoded log lines.
func serveObserved(t *testing.T, req *http.Request, next http.Handler) (*httptest.ResponseRecorder, []map[string]any) {
	t.Helper()

	var buf bytes.Buffer
	obs.Setup(obs.LogOptions{Format: "json", Level: "debug", Service: "central-config", Output: &buf})
	t.Cleanup(func() { obs.Setup(obs.LogOptions{Format: "text", Output: &bytes.Buffer{}}) })

	routeOf := func(r *http.Request) string { return r.Method + " " + r.URL.Path }
	rec := httptest.NewRecorder()
	observe(routeOf, next).ServeHTTP(rec, req)

	var lines []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(line), &doc); err != nil {
			t.Fatalf("log line is not JSON: %v (%q)", err, line)
		}
		lines = append(lines, doc)
	}
	return rec, lines
}

func find(lines []map[string]any, message string) map[string]any {
	for _, l := range lines {
		if l["message"] == message {
			return l
		}
	}
	return nil
}

// An inbound X-Request-Id is what lets a caller's lines be joined to ours, so
// it has to survive into the response header, into the access line, and into
// anything a handler logs through the context.
func TestRequestIDFlowsFromHeaderToLogAndResponse(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/flags", nil)
	req.Header.Set(requestIDHeader, "inbound-abc123")

	rec, lines := serveObserved(t, req, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		obs.FromContext(r.Context(), "flagsconfig").Info("handler ran")
		w.WriteHeader(http.StatusOK)
	}))

	if got := rec.Header().Get(requestIDHeader); got != "inbound-abc123" {
		t.Errorf("response %s = %q, want the inbound id", requestIDHeader, got)
	}

	access := find(lines, "http request")
	if access == nil {
		t.Fatalf("no access log line in %v", lines)
	}
	want := map[string]any{
		"http.request.id":           "inbound-abc123",
		"http.request.method":       "GET",
		"url.path":                  "/flags",
		"http.route":                "GET /flags",
		"http.response.status_code": float64(http.StatusOK),
		"event.dataset":             "central-config.http",
	}
	for k, v := range want {
		if access[k] != v {
			t.Errorf("access line %s = %v, want %v", k, access[k], v)
		}
	}
	if _, ok := access["event.duration"].(float64); !ok {
		t.Errorf("access line has no event.duration: %v", access["event.duration"])
	}

	// The handler's own line carries the same id under its own dataset, which
	// is what makes a request greppable as a group in Kibana.
	handler := find(lines, "handler ran")
	if handler == nil {
		t.Fatal("handler line missing")
	}
	if handler["http.request.id"] != "inbound-abc123" {
		t.Errorf("handler line id = %v", handler["http.request.id"])
	}
	if handler["event.dataset"] != "central-config.flagsconfig" {
		t.Errorf("handler line dataset = %v", handler["event.dataset"])
	}
}

// A header that could forge a log record must not be echoed back or logged.
func TestRequestIDRejectsUnsafeInboundValue(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set(requestIDHeader, "bad\nvalue \"quoted\"")

	rec, _ := serveObserved(t, req, http.HandlerFunc(okHandler))

	got := rec.Header().Get(requestIDHeader)
	if got == "" || strings.ContainsAny(got, "\n\" ") {
		t.Errorf("unsafe inbound id was not replaced: %q", got)
	}
}

// A generated id must still be present when the caller sends none.
func TestRequestIDGeneratedWhenAbsent(t *testing.T) {
	rec, lines := serveObserved(t, httptest.NewRequest(http.MethodGet, "/flags", nil), http.HandlerFunc(okHandler))

	id := rec.Header().Get(requestIDHeader)
	if len(id) != 32 {
		t.Fatalf("generated id = %q, want 32 hex characters", id)
	}
	if access := find(lines, "http request"); access == nil || access["http.request.id"] != id {
		t.Errorf("access line id does not match the response header (%q)", id)
	}
}

// A panicking handler must answer 500 and leave the stack behind in the log,
// rather than dropping the connection the way net/http's recovery does.
func TestPanicIsRecoveredAndLogged(t *testing.T) {
	rec, lines := serveObserved(t, httptest.NewRequest(http.MethodGet, "/flags", nil),
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("kaboom") }))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	line := find(lines, "handler panicked")
	if line == nil {
		t.Fatalf("no panic line in %v", lines)
	}
	if line["error.message"] != "kaboom" {
		t.Errorf("error.message = %v", line["error.message"])
	}
	if s, _ := line["error.stack_trace"].(string); !strings.Contains(s, "observe_test.go") {
		t.Errorf("error.stack_trace does not name the panicking frame: %v", line["error.stack_trace"])
	}
}

// r.Method is caller input and a metric label allocates a series that lives as
// long as the process. Without an allowlist, `curl -X <anything>` on an open
// route is unauthenticated, unrate-limited memory growth and a /metrics body
// that keeps getting slower to scrape.
func TestMethodLabelIsBounded(t *testing.T) {
	for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"} {
		if got := methodLabel(method); got != method {
			t.Errorf("methodLabel(%q) = %q, want it kept", method, got)
		}
	}
	for _, method := range []string{"BREW", "PROPFIND", "x", "GETX", "get", ""} {
		if got := methodLabel(method); got != "other" {
			t.Errorf("methodLabel(%q) = %q, want \"other\"", method, got)
		}
	}
}

// The bound label has to be the one that actually reaches the counter, not just
// something the helper could produce.
func TestUnknownMethodDoesNotAllocateASeries(t *testing.T) {
	// serveObserved labels the route with the request's own method and path.
	const route = "BREW /flags"
	before := obs.HTTPRequests.Value("method", "BREW", "route", route, "status", "200")
	other := obs.HTTPRequests.Value("method", "other", "route", route, "status", "200")

	serveObserved(t, httptest.NewRequest("BREW", "/flags", nil), http.HandlerFunc(okHandler))

	if got := obs.HTTPRequests.Value("method", "BREW", "route", route, "status", "200"); got != before {
		t.Errorf("a series was allocated for an arbitrary method: %d", got)
	}
	if got := obs.HTTPRequests.Value("method", "other", "route", route, "status", "200"); got != other+1 {
		t.Errorf("the \"other\" series = %d, want %d", got, other+1)
	}
}
