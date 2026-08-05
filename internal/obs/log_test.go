package obs

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestSetupEmitsECSFields asserts on the parsed document rather than on a
// substring: the point of the handler is the field *names* Elasticsearch will
// index, so a rename has to fail the test.
func TestSetupEmitsECSFields(t *testing.T) {
	var buf bytes.Buffer
	Setup(LogOptions{Format: "json", Level: "debug", Service: "central-config", Version: "1.2.3", Output: &buf})
	t.Cleanup(resetLogging)

	Logger("reconciler").Error("source resync failed", Err(errBoom{}))

	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("log line is not JSON: %v (%q)", err, buf.String())
	}

	want := map[string]any{
		"log.level":       "error",
		"message":         "source resync failed",
		"service.name":    "central-config",
		"service.version": "1.2.3",
		"event.dataset":   "central-config.reconciler",
		"error.message":   "boom",
	}
	for k, v := range want {
		if doc[k] != v {
			t.Errorf("%s = %v, want %v", k, doc[k], v)
		}
	}

	ts, ok := doc["@timestamp"].(string)
	if !ok {
		t.Fatalf("@timestamp missing or not a string: %v", doc["@timestamp"])
	}
	if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
		t.Errorf("@timestamp %q is not RFC3339: %v", ts, err)
	}

	// slog's own key names must not survive alongside the ECS ones.
	for _, k := range []string{"time", "level", "msg"} {
		if _, found := doc[k]; found {
			t.Errorf("slog key %q leaked into the ECS document", k)
		}
	}
}

func TestLevelIsConfigurable(t *testing.T) {
	var buf bytes.Buffer
	Setup(LogOptions{Format: "json", Level: "warn", Output: &buf})
	t.Cleanup(resetLogging)

	Logger("app").Info("not emitted")
	Logger("app").Warn("emitted")

	if strings.Contains(buf.String(), "not emitted") {
		t.Error("info line emitted at LOG_LEVEL=warn")
	}
	if !strings.Contains(buf.String(), "emitted") {
		t.Error("warn line missing at LOG_LEVEL=warn")
	}
}

// A logger created before Setup — as every package-level logger is — must still
// end up writing through the handler Setup installs.
func TestLoggerCreatedBeforeSetupFollowsIt(t *testing.T) {
	logger := Logger("early-" + t.Name())

	var buf bytes.Buffer
	Setup(LogOptions{Format: "json", Output: &buf})
	t.Cleanup(resetLogging)

	logger.Info("late")
	if !strings.Contains(buf.String(), `"message":"late"`) {
		t.Errorf("pre-Setup logger did not follow Setup: %q", buf.String())
	}
}

func TestTextModeIsHumanReadable(t *testing.T) {
	var buf bytes.Buffer
	Setup(LogOptions{Format: "text", Output: &buf})
	t.Cleanup(resetLogging)

	Logger("app").Info("hello")
	line := buf.String()
	if !strings.Contains(line, "msg=hello") || strings.Contains(line, "{") {
		t.Errorf("text mode produced %q", line)
	}
}

type errBoom struct{}

func (errBoom) Error() string { return "boom" }

// resetLogging restores the pre-Setup default so tests do not leak a closed
// buffer into each other.
func resetLogging() {
	Setup(LogOptions{Format: "text", Output: discard{}})
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
