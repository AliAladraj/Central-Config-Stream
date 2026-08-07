package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/AliAladraj/Central-Config-Stream/internal/messaging"

	natsserver "github.com/nats-io/nats-server/v2/test"
)

// The reconciler's full sweep prunes deleted rows, but it only runs on its
// interval — up to five minutes at the default setting. A delete has to take
// the KV key with it there and then, or every consumer keeps serving the
// deleted row until the next sweep.
func TestDeleteEndpointsPurgeKVImmediately(t *testing.T) {
	opts := natsserver.DefaultTestOptions
	opts.Port = -1
	opts.JetStream = true
	opts.StoreDir = t.TempDir()
	srv := natsserver.RunServer(&opts)
	defer srv.Shutdown()

	cfg := &Config{
		DBDriver:          "sqlite",
		DBConnString:      "file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "purge.db")),
		ServerPort:        ":0",
		NATSURL:           srv.ClientURL(),
		PublishEnabled:    true,
		NATSReplicas:      1,
		ReconcileInterval: time.Hour, // startup sweep only: nothing else may prune
		AdminToken:        "test-token",
	}

	app, err := NewApp(cfg)
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	t.Cleanup(func() {
		if app.stopReconcile != nil {
			app.stopReconcile()
		}
		_ = app.nats.Drain()
		_ = app.db.Close()
	})

	api := httptest.NewServer(app.server.Handler)
	defer api.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// A second connection just to read the buckets back, so the assertions do
	// not go through the same publisher the app writes with.
	client, err := messaging.Connect(messaging.Config{URL: srv.ClientURL(), Name: "purge-test"})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Drain()

	buckets, err := messaging.EnsureBuckets(ctx, client.JS, messaging.BucketOptions{Replicas: 1})
	if err != nil {
		t.Fatalf("ensure buckets: %v", err)
	}
	pub := messaging.NewPublisher(buckets)

	// The startup sweep has to have published the seeded rows first, otherwise
	// "the key is gone" proves nothing.
	waitFor(t, "seeded flag value published", func() bool {
		return hasKey(t, ctx, pub, "1.dark_mode")
	})
	waitFor(t, "seeded localization published", func() bool {
		return hasBucketKey(t, ctx, pub, messaging.BucketLocalization, "1.1.pt-BR")
	})

	// Flag value 101 is dark_mode in environment 1.
	adminDelete(t, ctx, api, "/flags/values/101")
	if hasKey(t, ctx, pub, "1.dark_mode") {
		t.Fatal("deleting a flag value left its KV key behind")
	}

	// Localization row 301 is microservice 1, environment 1, locale pt-BR.
	adminDelete(t, ctx, api, "/localization/301")
	if hasBucketKey(t, ctx, pub, messaging.BucketLocalization, "1.1.pt-BR") {
		t.Fatal("deleting a localization row left its KV key behind")
	}

	// Deleting the flag itself takes every environment's value key with it.
	waitFor(t, "both search_v2 keys published", func() bool {
		return hasKey(t, ctx, pub, "1.search_v2") && hasKey(t, ctx, pub, "3.search_v2")
	})
	adminDelete(t, ctx, api, "/flags/7")
	if hasKey(t, ctx, pub, "1.search_v2") || hasKey(t, ctx, pub, "3.search_v2") {
		t.Fatal("deleting a flag left one of its value keys behind")
	}
}

// adminDelete drives the real router, so the route, the auth middleware and the
// handler are all part of what is being tested.
func adminDelete(t *testing.T, ctx context.Context, api *httptest.Server, path string) {
	t.Helper()

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, api.URL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-token")

	resp, err := api.Client().Do(req)
	if err != nil {
		t.Fatalf("delete %s: %v", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete %s: status %d", path, resp.StatusCode)
	}
}

func hasBucketKey(t *testing.T, ctx context.Context, pub *messaging.Publisher, bucket, key string) bool {
	t.Helper()
	keys, err := pub.ListKeys(ctx, bucket)
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	for _, k := range keys {
		if k == key {
			return true
		}
	}
	return false
}
