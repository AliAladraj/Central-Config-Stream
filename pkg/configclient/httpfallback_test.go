package configclient_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AliAladraj/Central-Config-Stream/pkg/configclient"
)

// fallbackToken stands in for a real bearer credential. It is deliberately
// distinctive so a test can prove it never reaches an error message, a log line
// or Status.
const fallbackToken = "s3cr3t-fallback-token"

// fallbackFixtures answers the three routes the fallback reads for a client in
// environment 3 scoped to microservice 1.
func fallbackFixtures(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/flags/values/7":
		w.Write([]byte(`{"id":7,"environmentId":3,"value":"on","enabled":1,"updatedAt":"2026-01-01T00:00:00Z"}`))
	case "/configs/values/9":
		w.Write([]byte(`{"id":9,"microserviceId":1,"environmentId":3,"settingsJson":{"timeout":30}}`))
	case "/localization/lookup/1/3/pt-BR":
		w.Write([]byte(`{"id":1,"microserviceId":1,"environmentId":3,"locale":"pt-BR","bundleJson":{"a":"b"}}`))
	default:
		http.NotFound(w, r)
	}
}

// fallbackFor is the fallback configuration matching those fixtures.
func fallbackFor(baseURL string) *configclient.HTTPFallback {
	return &configclient.HTTPFallback{
		BaseURL:       baseURL,
		Token:         fallbackToken,
		Timeout:       2 * time.Second,
		ConfigValueID: 9,
		FlagValueIDs:  map[string]int64{"search_v2": 7},
		Locales:       []string{"pt-BR"},
	}
}

// coldStart runs New with nothing listening for JetStream, so the fallback is
// the only way the client can boot. The context is short because the KV warm-up
// is certain to fail and the fallback is given its own budget once it has.
func coldStart(t *testing.T, f *configclient.HTTPFallback) (*configclient.Client, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return configclient.New(ctx, configclient.Options{
		NATSURL:        "nats://127.0.0.1:1",
		EnvironmentID:  3,
		MicroserviceID: 1,
		HTTPFallback:   f,
	})
}

// authRecorder is a fake admin API that records the Authorization header of
// every request it serves.
type authRecorder struct {
	mu   sync.Mutex
	seen map[string]string
}

func (a *authRecorder) record(r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.seen == nil {
		a.seen = make(map[string]string)
	}
	a.seen[r.URL.Path] = r.Header.Get("Authorization")
}

func (a *authRecorder) headers() map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]string, len(a.seen))
	for k, v := range a.seen {
		out[k] = v
	}
	return out
}

// TestFallbackSendsBearerTokenOnEveryRequest is the regression this package
// could not see: the admin API authenticates every route the fallback reads, so
// a request without a credential is a 401 discovered during an incident.
func TestFallbackSendsBearerTokenOnEveryRequest(t *testing.T) {
	rec := &authRecorder{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		if r.Header.Get("Authorization") != "Bearer "+fallbackToken {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		fallbackFixtures(w, r)
	}))
	defer api.Close()

	cc, err := coldStart(t, fallbackFor(api.URL))
	if err != nil {
		t.Fatalf("cold start: %v", err)
	}
	defer cc.Close()

	seen := rec.headers()
	for _, path := range []string{
		"/flags/values/7",
		"/configs/values/9",
		"/localization/lookup/1/3/pt-BR",
	} {
		got, ok := seen[path]
		if !ok {
			t.Errorf("%s was never requested", path)
			continue
		}
		if got != "Bearer "+fallbackToken {
			t.Errorf("%s Authorization = %q, want a Bearer credential", path, got)
		}
	}
	if len(seen) != 3 {
		t.Errorf("requests = %d, want the three fallback endpoints: %v", len(seen), seen)
	}

	// The credential is only worth anything if the cache actually filled.
	if !cc.FlagEnabled("search_v2") {
		t.Error("flag should have been hydrated over HTTP")
	}
	if _, ok := cc.ServiceSettings(1); !ok {
		t.Error("appsettings should have been hydrated over HTTP")
	}
	if _, ok := cc.Translate(1, "pt-BR", "a"); !ok {
		t.Error("bundle should have been hydrated over HTTP")
	}
}

// TestFallbackRefusalsAreDiagnosable covers the three ways the API turns a
// fallback read away. None of them may look like "no config exists", and a
// scope mismatch is the awkward one: the API answers it 404, the same as a row
// that was deleted.
func TestFallbackRefusalsAreDiagnosable(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   []string
	}{
		{"unauthorized", http.StatusUnauthorized, []string{"401", "HTTPFallback.Token"}},
		{"forbidden", http.StatusForbidden, []string{"403", "not permitted to read environment 3"}},
		{"scoped out", http.StatusNotFound, []string{"404", "not scoped to environment 3"}},
	}

	messages := make(map[string]string, len(cases))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, `{"error":"refused"}`, tc.status)
			}))
			defer api.Close()

			cc, err := coldStart(t, fallbackFor(api.URL))
			if err == nil {
				cc.Close()
				t.Fatalf("cold start against a %d succeeded and returned an empty cache", tc.status)
			}
			if cc != nil {
				t.Error("a failed cold start must not hand back a client")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not mention %q: %v", want, err)
				}
			}
			if strings.Contains(err.Error(), fallbackToken) {
				t.Error("the error quoted the token")
			}
			messages[tc.name] = err.Error()
		})
	}

	// An operator has to be able to tell the three apart at a glance.
	for a := range messages {
		for b := range messages {
			if a != b && messages[a] == messages[b] {
				t.Errorf("%s and %s produce the same error: %s", a, b, messages[a])
			}
		}
	}
}

// TestFallbackRefusalSurvivesPartialSuccess guards the failure shape a wrong
// token really has: some endpoints answer, one is refused, and the missing key
// is indistinguishable from config nobody ever published.
func TestFallbackRefusalSurvivesPartialSuccess(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/flags/values/7" {
			http.Error(w, `{"error":"token is not scoped to this environment"}`, http.StatusForbidden)
			return
		}
		fallbackFixtures(w, r)
	}))
	defer api.Close()

	cc, err := coldStart(t, fallbackFor(api.URL))
	if err == nil {
		cc.Close()
		t.Fatal("a refused credential was written off as a partial cold start")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error should name the refusal: %v", err)
	}
}

// TestFallbackTokenIsNotDisclosed keeps the credential out of everything a
// consumer routinely prints: New's error and the Status a /healthz handler
// serves.
func TestFallbackTokenIsNotDisclosed(t *testing.T) {
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	}))
	defer refusing.Close()

	_, err := coldStart(t, fallbackFor(refusing.URL))
	if err == nil {
		t.Fatal("a 401 must fail the cold start")
	}
	if strings.Contains(err.Error(), fallbackToken) {
		t.Errorf("New's error disclosed the token: %v", err)
	}

	api := httptest.NewServer(http.HandlerFunc(fallbackFixtures))
	defer api.Close()

	cc, err := coldStart(t, fallbackFor(api.URL))
	if err != nil {
		t.Fatalf("cold start: %v", err)
	}
	defer cc.Close()

	status, err := json.Marshal(cc.Status())
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	if strings.Contains(string(status), fallbackToken) {
		t.Errorf("Status disclosed the token: %s", status)
	}
	if snap, err := json.Marshal(cc.Snapshot()); err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	} else if strings.Contains(string(snap), fallbackToken) {
		t.Errorf("Snapshot disclosed the token: %s", snap)
	}
}

// TestFallbackWithoutTokenFailsAtBoot proves the loud, early failure: a
// credential nobody set is rejected by New while JetStream is healthy and the
// fallback is nowhere near being used, rather than months later at 3am.
func TestFallbackWithoutTokenFailsAtBoot(t *testing.T) {
	var hits atomic.Int64
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fallbackFixtures(w, r)
	}))
	defer api.Close()

	url := startJetStream(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cc, err := configclient.New(ctx, configclient.Options{
		NATSURL:        url,
		EnvironmentID:  3,
		MicroserviceID: 1,
		HTTPFallback:   &configclient.HTTPFallback{BaseURL: api.URL, ConfigValueID: 9},
	})
	if err == nil {
		cc.Close()
		t.Fatal("a fallback with no credential must not boot")
	}
	for _, want := range []string{"HTTPFallback.Token is required", "AllowUnauthenticated"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("a fallback that cannot authenticate still made %d requests", n)
	}
}

// TestFallbackAllowUnauthenticated keeps the escape hatch working for a
// deployment running with auth switched off.
func TestFallbackAllowUnauthenticated(t *testing.T) {
	rec := &authRecorder{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		fallbackFixtures(w, r)
	}))
	defer api.Close()

	f := fallbackFor(api.URL)
	f.Token = ""
	f.AllowUnauthenticated = true

	cc, err := coldStart(t, f)
	if err != nil {
		t.Fatalf("cold start with auth disabled: %v", err)
	}
	defer cc.Close()

	if !cc.FlagEnabled("search_v2") {
		t.Error("flag should have been hydrated over HTTP")
	}
	for path, header := range rec.headers() {
		if header != "" {
			t.Errorf("%s carried an Authorization header: %q", path, header)
		}
	}
}

// TestFallbackRejectsUnusableToken catches the credential read from a file with
// a trailing newline before the transport does, and without echoing it.
func TestFallbackRejectsUnusableToken(t *testing.T) {
	const bad = "token-with-a\nnewline"
	f := fallbackFor("http://127.0.0.1:1")
	f.Token = bad

	cc, err := coldStart(t, f)
	if err == nil {
		cc.Close()
		t.Fatal("a token that cannot be sent as a header must fail at boot")
	}
	if !strings.Contains(err.Error(), "Authorization header value") {
		t.Errorf("error should say why the token is unusable: %v", err)
	}
	if strings.Contains(err.Error(), bad) {
		t.Errorf("the error quoted the token: %v", err)
	}
}
