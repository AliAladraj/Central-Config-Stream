package configclient_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ErasedKyte/Central-Config-Stream/internal/messaging"
	"github.com/ErasedKyte/Central-Config-Stream/pkg/configclient"

	natsserver "github.com/nats-io/nats-server/v2/test"
)

// TestEndToEnd proves the full distribution path: central-config publishes to KV,
// a consuming service's configclient warms its cache and serves reads, and a
// later update is pushed through to the cache.
func TestEndToEnd(t *testing.T) {
	// embedded JetStream server
	opts := natsserver.DefaultTestOptions
	opts.Port = -1
	opts.JetStream = true
	opts.StoreDir = t.TempDir()
	srv := natsserver.RunServer(&opts)
	defer srv.Shutdown()
	url := srv.ClientURL()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// central-config side: connect, ensure buckets, publish.
	client, err := messaging.Connect(messaging.Config{URL: url, Name: "test-cc"})
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
	if err := pub.PublishMicroConfig(ctx, env, 2, json.RawMessage(`{"timeout":30}`)); err != nil {
		t.Fatalf("publish micro: %v", err)
	}
	if err := pub.PublishLocalization(ctx, env, 2, "pt-BR", json.RawMessage(`{"catalog.title":"Catálogo"}`)); err != nil {
		t.Fatalf("publish loc: %v", err)
	}

	// consumer side: configclient warms cache and serves reads.
	cc, err := configclient.New(ctx, configclient.Options{NATSURL: url, EnvironmentID: env})
	if err != nil {
		t.Fatalf("configclient new: %v", err)
	}
	defer cc.Close()

	if !cc.FlagEnabled("search_v2") {
		t.Error("flag search_v2 should be enabled")
	}
	if v, ok := cc.FlagValue("search_v2"); !ok || v != "on" {
		t.Errorf("flag value = %q, %v", v, ok)
	}
	if s, ok := cc.MicroSettings(2); !ok || string(s) != `{"timeout":30}` {
		t.Errorf("micro settings = %s, %v", s, ok)
	}
	if tr, ok := cc.Translate(2, "pt-BR", "catalog.title"); !ok || tr != "Catálogo" {
		t.Errorf("translate = %q, %v", tr, ok)
	}

	// live update pushes through to the cache.
	if err := pub.PublishFlag(ctx, env, "search_v2", messaging.FlagPayload{Enabled: false, Value: "off"}); err != nil {
		t.Fatalf("republish: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !cc.FlagEnabled("search_v2") {
			return // update received
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("live flag update was not pushed to the consumer cache")
}
