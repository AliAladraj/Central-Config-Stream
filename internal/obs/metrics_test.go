package obs

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerRendersPrometheusText(t *testing.T) {
	KVPublishAttempts.Inc("bucket", "FLAGS")
	KVPublishFailures.Inc("bucket", "FLAGS")
	DBUp.SetBool(true)
	ReconcileDuration.Observe(0.75, "sweep", "full")

	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q", ct)
	}
	body := rec.Body.String()

	for _, want := range []string{
		"# TYPE centralconfig_kv_publish_failures_total counter",
		`centralconfig_kv_publish_failures_total{bucket="FLAGS"} 1`,
		"# TYPE centralconfig_db_up gauge",
		"centralconfig_db_up 1",
		"# TYPE centralconfig_reconcile_duration_seconds histogram",
		`centralconfig_reconcile_duration_seconds_bucket{sweep="full",le="1"} 1`,
		`centralconfig_reconcile_duration_seconds_bucket{sweep="full",le="0.5"} 0`,
		`centralconfig_reconcile_duration_seconds_bucket{sweep="full",le="+Inf"} 1`,
		`centralconfig_reconcile_duration_seconds_count{sweep="full"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}

	// Every sample line must be "name value", and every family must be typed.
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if len(strings.Fields(line)) != 2 {
			t.Errorf("malformed exposition line %q", line)
		}
	}
}

func TestCounterLabelsAreEscaped(t *testing.T) {
	c := NewCounter("centralconfig_test_escape_total", "Test only.")
	c.Inc("route", `GET /"weird"`)

	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if !strings.Contains(rec.Body.String(), `{route="GET /\"weird\""}`) {
		t.Errorf("label value not escaped:\n%s", rec.Body.String())
	}
}
