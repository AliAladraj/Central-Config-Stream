package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/AliAladraj/Central-Config-Stream/internal/database"
	"github.com/AliAladraj/Central-Config-Stream/internal/flagsconfig"
	"github.com/AliAladraj/Central-Config-Stream/internal/messaging"

	natsserver "github.com/nats-io/nats-server/v2/test"
)

// A row deleted from the database has to stop being served. Nothing else in the
// system removes a KV key, so the reconciler's full sweep is the only thing
// standing between a deleted flag and consumers caching it forever.
func TestFullSweepPurgesDeletedRowFromKV(t *testing.T) {
	opts := natsserver.DefaultTestOptions
	opts.Port = -1
	opts.JetStream = true
	opts.StoreDir = t.TempDir()
	srv := natsserver.RunServer(&opts)
	defer srv.Shutdown()

	db, err := database.NewSQLiteDB("file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "prune.db")))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()

	client, err := messaging.Connect(messaging.Config{URL: srv.ClientURL(), Name: "prune-test"})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Drain()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	buckets, err := messaging.EnsureBuckets(ctx, client.JS, messaging.BucketOptions{Replicas: 1})
	if err != nil {
		t.Fatalf("ensure buckets: %v", err)
	}
	pub := messaging.NewPublisher(buckets)

	source := &flagsReconcileSource{repo: flagsconfig.NewSQLiteRepository(db)}

	// Each Start does one immediate full sweep; the interval is long enough
	// that no second cycle fires before the reconciler is stopped again.
	sweep := func(what string, done func() bool) {
		stop := messaging.NewReconciler(time.Hour, pub, source).Start(ctx)
		defer stop()
		waitFor(t, what, done)
	}

	sweep("seeded flag published to KV", func() bool {
		return hasKey(t, ctx, pub, "1.dark_mode")
	})

	// The row goes away out from under KV.
	if _, err := db.ExecContext(ctx, `DELETE FROM CONFIG_FLAG_VALUE WHERE ID = 101`); err != nil {
		t.Fatalf("delete row: %v", err)
	}

	sweep("deleted row purged from KV", func() bool {
		return !hasKey(t, ctx, pub, "1.dark_mode")
	})

	// The rows that survived are untouched.
	if !hasKey(t, ctx, pub, "1.search_v2") {
		t.Fatal("full sweep purged a key that still has a row")
	}
}

func hasKey(t *testing.T, ctx context.Context, pub *messaging.Publisher, key string) bool {
	t.Helper()
	keys, err := pub.ListKeys(ctx, messaging.BucketFlags)
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
