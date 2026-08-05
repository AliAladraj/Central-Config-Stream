package configclient_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/ErasedKyte/Central-Config-Stream/internal/messaging"
	"github.com/ErasedKyte/Central-Config-Stream/pkg/configclient"

	natsserver "github.com/nats-io/nats-server/v2/test"
)

// startJetStream boots an embedded JetStream server and returns its URL.
func startJetStream(t *testing.T) string {
	t.Helper()
	opts := natsserver.DefaultTestOptions
	opts.Port = -1
	opts.JetStream = true
	opts.StoreDir = t.TempDir()
	srv := natsserver.RunServer(&opts)
	t.Cleanup(srv.Shutdown)
	return srv.ClientURL()
}

// exampleControlPlane backs ExampleNew: an embedded JetStream server with one
// flag, one appsettings blob and one bundle already published, standing in for
// a central-config deployment. It lives here rather than beside the example so
// that pkg.go.dev shows the example on its own.
func exampleControlPlane() (url string, stop func()) {
	opts := natsserver.DefaultTestOptions
	opts.Port = -1
	opts.JetStream = true
	opts.StoreDir, _ = os.MkdirTemp("", "configclient-example")
	srv := natsserver.RunServer(&opts)

	must := func(err error) {
		if err != nil {
			panic(err)
		}
	}
	ctx := context.Background()
	client, err := messaging.Connect(messaging.Config{URL: srv.ClientURL(), Name: "example-cc"})
	must(err)
	buckets, err := messaging.EnsureBuckets(ctx, client.JS, messaging.BucketOptions{Replicas: 1})
	must(err)

	pub := messaging.NewPublisher(buckets)
	must(pub.PublishFlag(ctx, 3, "search_v2", messaging.FlagPayload{Enabled: true, Value: "on"}))
	must(pub.PublishMicroConfig(ctx, 3, 1, json.RawMessage(`{"timeout":30}`)))
	must(pub.PublishLocalization(ctx, 3, 1, "pt-BR", json.RawMessage(`{"catalog.title":"Catálogo"}`)))

	return srv.ClientURL(), func() {
		client.Drain()
		srv.Shutdown()
		os.RemoveAll(opts.StoreDir)
	}
}

// TestScopedClientIgnoresOtherServices proves the narrowed watch: a client
// scoped to service 1 caches its own appsettings and locales plus every flag
// in the environment, and nothing belonging to service 2.
func TestScopedClientIgnoresOtherServices(t *testing.T) {
	url := startJetStream(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := messaging.Connect(messaging.Config{URL: url, Name: "test-scope"})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Drain()

	buckets, err := messaging.EnsureBuckets(ctx, client.JS, messaging.BucketOptions{Replicas: 1})
	if err != nil {
		t.Fatalf("ensure buckets: %v", err)
	}
	pub := messaging.NewPublisher(buckets)

	const env = int64(3)
	if err := pub.PublishFlag(ctx, env, "search_v2", messaging.FlagPayload{Enabled: true, Value: "on"}); err != nil {
		t.Fatalf("publish flag: %v", err)
	}
	if err := pub.PublishFlag(ctx, env, "search_v3", messaging.FlagPayload{Enabled: true, Value: "on"}); err != nil {
		t.Fatalf("publish flag: %v", err)
	}
	for _, ms := range []int64{1, 2} {
		if err := pub.PublishMicroConfig(ctx, env, ms, json.RawMessage(`{"timeout":30}`)); err != nil {
			t.Fatalf("publish micro %d: %v", ms, err)
		}
		if err := pub.PublishLocalization(ctx, env, ms, "pt-BR", json.RawMessage(`{"a":"b"}`)); err != nil {
			t.Fatalf("publish loc %d: %v", ms, err)
		}
	}

	cc, err := configclient.New(ctx, configclient.Options{
		NATSURL:        url,
		EnvironmentID:  env,
		MicroserviceID: 1,
	})
	if err != nil {
		t.Fatalf("configclient new: %v", err)
	}
	defer cc.Close()

	// Flags stay env-wide.
	if !cc.FlagEnabled("search_v2") || !cc.FlagEnabled("search_v3") {
		t.Error("scoped client should still see every flag in the environment")
	}

	// Own config and locale bundle are cached.
	if _, ok := cc.MicroSettings(1); !ok {
		t.Error("own MICROCONFIG should be cached")
	}
	if _, ok := cc.Translate(1, "pt-BR", "a"); !ok {
		t.Error("own localization should be cached")
	}

	// The other service's config is neither cached nor readable.
	if _, ok := cc.MicroSettings(2); ok {
		t.Error("service 2's MICROCONFIG must not be cached by a client scoped to 1")
	}
	if _, ok := cc.Translate(2, "pt-BR", "a"); ok {
		t.Error("service 2's localization must not be cached by a client scoped to 1")
	}

	snap := cc.Snapshot()
	if _, ok := snap.Micro[2]; ok {
		t.Errorf("snapshot leaked service 2 micro: %v", snap.Micro)
	}
	if _, ok := snap.Localization[2]; ok {
		t.Errorf("snapshot leaked service 2 localization: %v", snap.Localization)
	}
	if len(snap.Flags) != 2 {
		t.Errorf("snapshot flags = %d, want 2", len(snap.Flags))
	}

	st := cc.Status()
	if st.FleetWide || st.MicroserviceID != 1 {
		t.Errorf("status scope = %+v", st)
	}
	if !st.Watching || !st.Connected {
		t.Errorf("status should report a live watch: %+v", st)
	}
	if st.OutOfScopeReads != 2 {
		t.Errorf("out-of-scope reads = %d, want 2", st.OutOfScopeReads)
	}
}

// TestFleetWideClientStillSeesEverything guards the unscoped mode that the test
// console and admin views rely on.
func TestFleetWideClientStillSeesEverything(t *testing.T) {
	url := startJetStream(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := messaging.Connect(messaging.Config{URL: url, Name: "test-fleet"})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Drain()
	buckets, err := messaging.EnsureBuckets(ctx, client.JS, messaging.BucketOptions{Replicas: 1})
	if err != nil {
		t.Fatalf("ensure buckets: %v", err)
	}
	pub := messaging.NewPublisher(buckets)

	const env = int64(3)
	for _, ms := range []int64{1, 2} {
		if err := pub.PublishMicroConfig(ctx, env, ms, json.RawMessage(`{"timeout":30}`)); err != nil {
			t.Fatalf("publish micro %d: %v", ms, err)
		}
	}

	cc, err := configclient.New(ctx, configclient.Options{NATSURL: url, EnvironmentID: env})
	if err != nil {
		t.Fatalf("configclient new: %v", err)
	}
	defer cc.Close()

	for _, ms := range []int64{1, 2} {
		if _, ok := cc.MicroSettings(ms); !ok {
			t.Errorf("fleet-wide client should cache service %d", ms)
		}
	}
	if st := cc.Status(); !st.FleetWide {
		t.Errorf("status should advertise the fleet-wide scope: %+v", st)
	}
}

// TestHTTPFallbackColdStart proves a consumer boots with config even when
// JetStream is unreachable, and that Status reports how it got there.
func TestHTTPFallbackColdStart(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The real API authenticates these routes, so the fake does too —
		// a fixture that answers anonymously cannot notice a missing header.
		if r.Header.Get("Authorization") != "Bearer "+fallbackToken {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/flags/values/7":
			w.Write([]byte(`{"id":7,"environmentId":3,"flagId":1,"value":"on","enabled":1,"updatedAt":"2026-01-01T00:00:00Z"}`))
		case "/configs/values/9":
			w.Write([]byte(`{"id":9,"microserviceId":1,"environmentId":3,"settingsJson":{"timeout":30}}`))
		case "/localization/lookup/1/3/pt-BR":
			w.Write([]byte(`{"id":1,"microserviceId":1,"environmentId":3,"locale":"pt-BR","bundleJson":{"a":"b"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Port 1 has no JetStream behind it, so the KV warm-up must fail.
	cc, err := configclient.New(ctx, configclient.Options{
		NATSURL:        "nats://127.0.0.1:1",
		EnvironmentID:  3,
		MicroserviceID: 1,
		HTTPFallback: &configclient.HTTPFallback{
			BaseURL:       api.URL,
			Token:         fallbackToken,
			Timeout:       2 * time.Second,
			ConfigValueID: 9,
			FlagValueIDs:  map[string]int64{"search_v2": 7},
			Locales:       []string{"pt-BR"},
		},
	})
	if err != nil {
		t.Fatalf("configclient new with fallback: %v", err)
	}
	defer cc.Close()

	if !cc.FlagEnabled("search_v2") {
		t.Error("flag should have been hydrated over HTTP")
	}
	if s, ok := cc.MicroSettings(1); !ok || string(s) != `{"timeout":30}` {
		t.Errorf("micro settings = %s, %v", s, ok)
	}
	if tr, ok := cc.Translate(1, "pt-BR", "a"); !ok || tr != "b" {
		t.Errorf("translate = %q, %v", tr, ok)
	}

	st := cc.Status()
	if !st.UsedHTTPFallback {
		t.Error("status should report the HTTP fallback was used")
	}
	if st.Watching || st.Connected {
		t.Errorf("a fallback-only client is not watching anything: %+v", st)
	}
	if st.LastError == "" {
		t.Error("status should carry the KV warm-up failure")
	}
}

// TestHTTPFallbackNotUsedWhenKVIsWarm keeps the fallback off the happy path.
func TestHTTPFallbackNotUsedWhenKVIsWarm(t *testing.T) {
	var hits int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.NotFound(w, r)
	}))
	defer api.Close()

	url := startJetStream(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := messaging.Connect(messaging.Config{URL: url, Name: "test-warm"})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Drain()
	buckets, err := messaging.EnsureBuckets(ctx, client.JS, messaging.BucketOptions{Replicas: 1})
	if err != nil {
		t.Fatalf("ensure buckets: %v", err)
	}
	pub := messaging.NewPublisher(buckets)
	if err := pub.PublishMicroConfig(ctx, 3, 1, json.RawMessage(`{"timeout":30}`)); err != nil {
		t.Fatalf("publish micro: %v", err)
	}

	cc, err := configclient.New(ctx, configclient.Options{
		NATSURL:        url,
		EnvironmentID:  3,
		MicroserviceID: 1,
		HTTPFallback: &configclient.HTTPFallback{
			BaseURL:       api.URL,
			Token:         fallbackToken,
			ConfigValueID: 9,
		},
	})
	if err != nil {
		t.Fatalf("configclient new: %v", err)
	}
	defer cc.Close()

	if hits != 0 {
		t.Errorf("HTTP fallback was called %d times despite a warm KV cache", hits)
	}
	if st := cc.Status(); st.UsedHTTPFallback {
		t.Error("status should not report a fallback that never ran")
	}
}
