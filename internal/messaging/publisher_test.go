package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ErasedKyte/Central-Config-Stream/internal/obs"

	natsserver "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go/jetstream"
)

func newTestPublisher(t *testing.T) (*Publisher, context.Context) {
	t.Helper()

	opts := natsserver.DefaultTestOptions
	opts.Port = -1
	opts.JetStream = true
	opts.StoreDir = t.TempDir()
	srv := natsserver.RunServer(&opts)
	t.Cleanup(srv.Shutdown)

	client, err := Connect(Config{URL: srv.ClientURL(), Name: "publisher-test"})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Drain() })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	buckets, err := EnsureBuckets(ctx, client.JS, BucketOptions{Replicas: 1})
	if err != nil {
		t.Fatalf("ensure buckets: %v", err)
	}
	return NewPublisher(buckets), ctx
}

// The delete path is what stops a row removed from the database being served
// forever, so it has to work against a real bucket, not just the test fake.
func TestPublisherListAndDeleteKeys(t *testing.T) {
	pub, ctx := newTestPublisher(t)

	// An untouched bucket lists nothing and must not error.
	keys, err := pub.ListKeys(ctx, BucketMicroConfig)
	if err != nil {
		t.Fatalf("list keys on an empty bucket: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("expected no keys in an empty bucket, got %v", keys)
	}

	if err := pub.PublishFlag(ctx, 1, "search_v2", FlagPayload{Enabled: true, Value: "on"}); err != nil {
		t.Fatalf("publish flag: %v", err)
	}
	if err := pub.PublishFlag(ctx, 1, "dark_mode", FlagPayload{Value: "off"}); err != nil {
		t.Fatalf("publish flag: %v", err)
	}

	keys, err = pub.ListKeys(ctx, BucketFlags)
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %v", keys)
	}

	if err := pub.DeleteKey(ctx, BucketFlags, "1.dark_mode"); err != nil {
		t.Fatalf("delete key: %v", err)
	}

	keys, err = pub.ListKeys(ctx, BucketFlags)
	if err != nil {
		t.Fatalf("list keys after delete: %v", err)
	}
	if len(keys) != 1 || keys[0] != "1.search_v2" {
		t.Fatalf("expected only 1.search_v2 to survive, got %v", keys)
	}

	// Deleting a key that is already gone is not an error — the reconciler may
	// race with an out-of-band purge.
	if err := pub.DeleteKey(ctx, BucketFlags, "1.dark_mode"); err != nil {
		t.Fatalf("delete of an absent key: %v", err)
	}

	if _, err := pub.ListKeys(ctx, "NOPE"); err == nil {
		t.Fatal("expected an error for an unknown bucket name")
	}
}

// EnsureBuckets is called every reconcile cycle, so it has to be idempotent and
// leave the publisher working afterwards.
func TestPublisherEnsureBucketsIsIdempotent(t *testing.T) {
	pub, ctx := newTestPublisher(t)

	for i := 0; i < 3; i++ {
		if err := pub.EnsureBuckets(ctx); err != nil {
			t.Fatalf("ensure buckets (call %d): %v", i, err)
		}
	}

	if err := pub.PublishLocalization(ctx, 1, 1, "en-US", json.RawMessage(`{"a":"b"}`)); err != nil {
		t.Fatalf("publish after re-ensure: %v", err)
	}
	keys, err := pub.ListKeys(ctx, BucketLocalization)
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	if len(keys) != 1 || keys[0] != "1.1.en-US" {
		t.Fatalf("unexpected keys after re-ensure: %v", keys)
	}
}

// revisionOf reports the current revision of a key, or 0 when it is absent.
func revisionOf(t *testing.T, pub *Publisher, ctx context.Context, bucket, key string) uint64 {
	t.Helper()
	kv, err := pub.buckets.byName(bucket)
	if err != nil {
		t.Fatalf("bucket %s: %v", bucket, err)
	}
	entry, err := kv.Get(ctx, key)
	if err != nil {
		return 0
	}
	return entry.Revision()
}

// A KV Put of an identical value still cuts a revision and still pushes to
// every watcher, so a full sweep would wake the whole fleet for nothing. The
// revision is the observable proof that no write happened; the watcher below
// is the proof that nothing was pushed.
func TestPublishSkipsIdenticalValue(t *testing.T) {
	pub, ctx := newTestPublisher(t)
	key := FlagKey(1, "search_v2")

	if err := pub.PublishFlag(ctx, 1, "search_v2", FlagPayload{Enabled: true, Value: "on"}); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	rev := revisionOf(t, pub, ctx, BucketFlags, key)

	kv, err := pub.buckets.byName(BucketFlags)
	if err != nil {
		t.Fatalf("bucket: %v", err)
	}
	w, err := kv.Watch(ctx, key, jetstream.UpdatesOnly())
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	defer func() { _ = w.Stop() }()

	skipped := obs.KVPublishSkipped.Value("bucket", BucketFlags)
	success := obs.KVPublishSuccess.Value("bucket", BucketFlags)

	// Same flag state, no caller-supplied timestamp — exactly what a reconcile
	// sweep republishes. It must not become a new revision just because the
	// payload would carry a fresher UpdatedAt.
	if err := pub.PublishFlag(ctx, 1, "search_v2", FlagPayload{Enabled: true, Value: "on"}); err != nil {
		t.Fatalf("republish: %v", err)
	}
	if got := revisionOf(t, pub, ctx, BucketFlags, key); got != rev {
		t.Errorf("revision moved on an unchanged publish: %d -> %d", rev, got)
	}
	if got := obs.KVPublishSkipped.Value("bucket", BucketFlags); got != skipped+1 {
		t.Errorf("skipped = %d, want %d", got, skipped+1)
	}
	if got := obs.KVPublishSuccess.Value("bucket", BucketFlags); got != success {
		t.Errorf("success = %d, want it unchanged at %d", got, success)
	}

	select {
	case entry := <-w.Updates():
		if entry != nil {
			t.Fatalf("watcher was pushed an update for an unchanged value (rev %d)", entry.Revision())
		}
	case <-time.After(300 * time.Millisecond):
	}

	// A real change still has to reach the bucket and the watcher.
	if err := pub.PublishFlag(ctx, 1, "search_v2", FlagPayload{Enabled: false, Value: "off"}); err != nil {
		t.Fatalf("changed publish: %v", err)
	}
	if got := revisionOf(t, pub, ctx, BucketFlags, key); got <= rev {
		t.Errorf("revision did not move on a changed publish: %d -> %d", rev, got)
	}
	if got := obs.KVPublishSuccess.Value("bucket", BucketFlags); got != success+1 {
		t.Errorf("success = %d, want %d", got, success+1)
	}

	select {
	case entry := <-w.Updates():
		if entry == nil {
			t.Fatal("watcher got the end-of-snapshot marker instead of the changed value")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watcher never received the changed value")
	}
}

// The same skip has to hold for the raw-JSON buckets, which have no timestamp
// to normalise away.
func TestPublishSkipsIdenticalRawValues(t *testing.T) {
	pub, ctx := newTestPublisher(t)

	settings := json.RawMessage(`{"timeoutMs":30}`)
	bundle := json.RawMessage(`{"catalog.title":"Catalog"}`)
	for i := 0; i < 2; i++ {
		if err := pub.PublishMicroConfig(ctx, 1, 3, settings); err != nil {
			t.Fatalf("publish microconfig: %v", err)
		}
		if err := pub.PublishLocalization(ctx, 1, 3, "en-US", bundle); err != nil {
			t.Fatalf("publish localization: %v", err)
		}
	}

	if got := revisionOf(t, pub, ctx, BucketMicroConfig, MicroKey(1, 3)); got != 1 {
		t.Errorf("microconfig revision = %d, want 1 (second publish was identical)", got)
	}
	if got := revisionOf(t, pub, ctx, BucketLocalization, LocalizationKey(1, 3, "en-US")); got != 1 {
		t.Errorf("localization revision = %d, want 1 (second publish was identical)", got)
	}

	if err := pub.PublishMicroConfig(ctx, 1, 3, json.RawMessage(`{"timeoutMs":60}`)); err != nil {
		t.Fatalf("publish changed microconfig: %v", err)
	}
	if got := revisionOf(t, pub, ctx, BucketMicroConfig, MicroKey(1, 3)); got != 2 {
		t.Errorf("microconfig revision = %d, want 2 after a real change", got)
	}
}

// Skipping must never cost the healing property: if KV disagrees with Oracle —
// hand-edited, restored from an old snapshot — the next publish has to fix it.
func TestPublishCorrectsDrift(t *testing.T) {
	pub, ctx := newTestPublisher(t)
	key := FlagKey(1, "search_v2")

	if err := pub.PublishFlag(ctx, 1, "search_v2", FlagPayload{Enabled: true, Value: "on"}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	kv, err := pub.buckets.byName(BucketFlags)
	if err != nil {
		t.Fatalf("bucket: %v", err)
	}
	if _, err := kv.Put(ctx, key, []byte(`{"enabled":false,"value":"WRONG","updatedAt":""}`)); err != nil {
		t.Fatalf("seed drift: %v", err)
	}

	if err := pub.PublishFlag(ctx, 1, "search_v2", FlagPayload{Enabled: true, Value: "on"}); err != nil {
		t.Fatalf("republish over drift: %v", err)
	}

	entry, err := kv.Get(ctx, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var got FlagPayload
	if err := json.Unmarshal(entry.Value(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Enabled || got.Value != "on" {
		t.Errorf("drift not corrected: %+v", got)
	}
}

// flagsSource republishes a fixed set of flags, standing in for the app-layer
// reconcile source (which reads them from Oracle) without importing it.
type flagsSource struct{ values map[string]string }

func (s *flagsSource) Name() string   { return "flags-sweep" }
func (s *flagsSource) Bucket() string { return BucketFlags }
func (s *flagsSource) Resync(ctx context.Context, pub ConfigPublisher, _ time.Time) (ResyncResult, error) {
	res := ResyncResult{Keys: make([]string, 0, len(s.values))}
	for flagKey, value := range s.values {
		res.Publish(s.Name(), FlagKey(1, flagKey),
			pub.PublishFlag(ctx, 1, flagKey, FlagPayload{Enabled: true, Value: value}))
	}
	return res, nil
}

// The whole point of the read-before-put: a full sweep over data that has not
// changed writes nothing at all, so no consumer is pushed to and the bucket's
// history window is not spent on identical revisions.
func TestFullSweepOverUnchangedDataPublishesNothing(t *testing.T) {
	pub, ctx := newTestPublisher(t)
	src := &flagsSource{values: map[string]string{"a": "1", "b": "2", "c": "3"}}
	rec := NewReconciler(time.Hour, pub, src)

	rec.runOnce(ctx) // first cycle populates the bucket

	revs := map[string]uint64{}
	for flagKey := range src.values {
		key := FlagKey(1, flagKey)
		revs[key] = revisionOf(t, pub, ctx, BucketFlags, key)
	}

	skipped := obs.KVPublishSkipped.Value("bucket", BucketFlags)
	success := obs.KVPublishSuccess.Value("bucket", BucketFlags)

	rec.runs = 0 // force another full sweep
	rec.runOnce(ctx)

	for key, want := range revs {
		if got := revisionOf(t, pub, ctx, BucketFlags, key); got != want {
			t.Errorf("%s revision moved on an unchanged sweep: %d -> %d", key, want, got)
		}
	}
	if got := obs.KVPublishSkipped.Value("bucket", BucketFlags); got != skipped+int64(len(src.values)) {
		t.Errorf("skipped = %d, want %d", got, skipped+int64(len(src.values)))
	}
	if got := obs.KVPublishSuccess.Value("bucket", BucketFlags); got != success {
		t.Errorf("success = %d, want it unchanged at %d — the sweep wrote to KV", got, success)
	}

	// One genuinely changed row still gets through, and only that one.
	src.values["b"] = "changed"
	rec.runs = 0
	rec.runOnce(ctx)

	if got := obs.KVPublishSuccess.Value("bucket", BucketFlags); got != success+1 {
		t.Errorf("success = %d, want %d (only the changed row)", got, success+1)
	}
	if got := revisionOf(t, pub, ctx, BucketFlags, FlagKey(1, "b")); got == revs[FlagKey(1, "b")] {
		t.Error("the changed row was not republished")
	}
}

// A failed publish is invisible from the caller's side — the request still
// succeeded and the reconciler is left to heal the drift — so the counter is
// the only thing an operator can alert on. It has to move, and it has to come
// out of /metrics as valid Prometheus text.
func TestPublishFailureIsCounted(t *testing.T) {
	pub, ctx := newTestPublisher(t)

	attempts := obs.KVPublishAttempts.Value("bucket", BucketMicroConfig)
	failures := obs.KVPublishFailures.Value("bucket", BucketMicroConfig)
	success := obs.KVPublishSuccess.Value("bucket", BucketMicroConfig)

	if err := pub.PublishMicroConfig(ctx, 1, 1, nil); err == nil {
		t.Fatal("expected a publish of empty settings to fail")
	}

	if got := obs.KVPublishFailures.Value("bucket", BucketMicroConfig); got != failures+1 {
		t.Errorf("failures = %d, want %d", got, failures+1)
	}
	if got := obs.KVPublishAttempts.Value("bucket", BucketMicroConfig); got != attempts+1 {
		t.Errorf("attempts = %d, want %d", got, attempts+1)
	}
	if got := obs.KVPublishSuccess.Value("bucket", BucketMicroConfig); got != success {
		t.Errorf("success = %d, want it unchanged at %d", got, success)
	}

	// A publish that works moves the success counter instead.
	if err := pub.PublishMicroConfig(ctx, 1, 1, json.RawMessage(`{"timeoutMs":10}`)); err != nil {
		t.Fatalf("publish microconfig: %v", err)
	}
	if got := obs.KVPublishSuccess.Value("bucket", BucketMicroConfig); got != success+1 {
		t.Errorf("success = %d, want %d", got, success+1)
	}

	rec := httptest.NewRecorder()
	obs.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	want := fmt.Sprintf("centralconfig_kv_publish_failures_total{bucket=%q} %d", BucketMicroConfig, failures+1)
	if !strings.Contains(rec.Body.String(), want) {
		t.Errorf("missing %q in:\n%s", want, rec.Body.String())
	}
}

// Two admins updating the same key used to be a plain read-then-write each: the
// slower writer's Put landed last and KV kept the older of the two values while
// the database kept the newer, both requests answering 200. The revision check
// turns that into a conflict the publisher re-resolves, so the value KV ends up
// with is one that was written against what KV actually held.
func TestPublishResolvesAConcurrentWrite(t *testing.T) {
	pub, ctx := newTestPublisher(t)
	key := MicroKey(1, 3)

	if err := pub.PublishMicroConfig(ctx, 1, 3, json.RawMessage(`{"timeoutMs":10}`)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	kv, err := pub.buckets.byName(BucketMicroConfig)
	if err != nil {
		t.Fatalf("bucket: %v", err)
	}

	// Stand in for the other admin: the value moves between this publisher's
	// read and its write, which is exactly the race a plain Put cannot see.
	entry, err := kv.Get(ctx, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	stale := entry.Revision()
	if _, err := kv.Put(ctx, key, []byte(`{"timeoutMs":20}`)); err != nil {
		t.Fatalf("concurrent write: %v", err)
	}

	// A write conditioned on the revision that was read is refused outright.
	if _, err := kv.Update(ctx, key, []byte(`{"timeoutMs":30}`), stale); err == nil {
		t.Fatal("a write against a stale revision was accepted")
	} else if !errors.Is(err, jetstream.ErrKeyExists) {
		t.Fatalf("stale-revision write failed with an unexpected error: %v", err)
	}

	// The publisher itself retries against what KV now holds and succeeds.
	if err := pub.PublishMicroConfig(ctx, 1, 3, json.RawMessage(`{"timeoutMs":40}`)); err != nil {
		t.Fatalf("publish after a concurrent write: %v", err)
	}
	entry, err = kv.Get(ctx, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(entry.Value()) != `{"timeoutMs":40}` {
		t.Errorf("stored value = %s, want the last write", entry.Value())
	}
	if entry.Revision() <= stale+1 {
		t.Errorf("revision = %d, want it past the concurrent write at %d", entry.Revision(), stale+1)
	}
}

// A bucket that inherits the server's max_payload accepts values the admin API
// has no ceiling for, so an oversized row is stored, answered with a 201, and
// then fails at publish on every sweep from then on. The bucket carries an
// explicit ceiling, and it is the same number the services validate against.
func TestBucketsCarryTheValueCeiling(t *testing.T) {
	pub, ctx := newTestPublisher(t)

	kv, err := pub.buckets.byName(BucketMicroConfig)
	if err != nil {
		t.Fatalf("bucket: %v", err)
	}

	// A value past the ceiling is refused by JetStream, which is what makes
	// the service-layer check load-bearing rather than cosmetic.
	oversized := make([]byte, MaxValueSize+1)
	for i := range oversized {
		oversized[i] = 'x'
	}
	if _, err := kv.Put(ctx, "1.1", oversized); err == nil {
		t.Error("the bucket accepted a value past its configured ceiling")
	}

	// Everything under it still publishes.
	if err := pub.PublishMicroConfig(ctx, 1, 1, json.RawMessage(`{"a":1}`)); err != nil {
		t.Fatalf("publish inside the ceiling: %v", err)
	}
}

// NATS being down at startup must not be fatal, so the publisher has to survive
// being handed buckets that were never provisioned: publishes fail cleanly and
// Ensure fills the handles in once NATS answers.
func TestPublishAgainstUnprovisionedBucketsFailsCleanly(t *testing.T) {
	opts := natsserver.DefaultTestOptions
	opts.Port = -1
	opts.JetStream = true
	opts.StoreDir = t.TempDir()
	srv := natsserver.RunServer(&opts)
	t.Cleanup(srv.Shutdown)

	client, err := Connect(Config{URL: srv.ClientURL(), Name: "unprovisioned-test"})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Drain() })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	pub := NewPublisher(NewBuckets(client.JS, BucketOptions{Replicas: 1}))

	err = pub.PublishFlag(ctx, 1, "search_v2", FlagPayload{Enabled: true})
	if !errors.Is(err, ErrNotProvisioned) {
		t.Fatalf("publish before provisioning = %v, want ErrNotProvisioned", err)
	}
	if _, err := pub.ListKeys(ctx, BucketFlags); !errors.Is(err, ErrNotProvisioned) {
		t.Fatalf("list before provisioning = %v, want ErrNotProvisioned", err)
	}

	// The recovery machinery the reconciler drives every cycle.
	if err := pub.EnsureBuckets(ctx); err != nil {
		t.Fatalf("ensure buckets: %v", err)
	}
	if err := pub.PublishFlag(ctx, 1, "search_v2", FlagPayload{Enabled: true}); err != nil {
		t.Fatalf("publish after provisioning: %v", err)
	}
}

// The reconciler snapshots a bucket before every full sweep, so an empty one
// has to come back immediately rather than blocking until the cycle's context
// gives up.
func TestListRevisionsOnAnEmptyBucketReturnsAtOnce(t *testing.T) {
	pub, _ := newTestPublisher(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	entries, err := pub.ListRevisions(ctx, BucketMicroConfig)
	if err != nil {
		t.Fatalf("list revisions on an empty bucket: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no entries, got %v", entries)
	}

	// And it reports the revision each key is actually at, which is what tells
	// a key untouched since the sweep began from one written during it.
	if err := pub.PublishMicroConfig(ctx, 1, 3, json.RawMessage(`{"a":1}`)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := pub.PublishMicroConfig(ctx, 1, 3, json.RawMessage(`{"a":2}`)); err != nil {
		t.Fatalf("republish: %v", err)
	}

	entries, err = pub.ListRevisions(ctx, BucketMicroConfig)
	if err != nil {
		t.Fatalf("list revisions: %v", err)
	}
	if len(entries) != 1 || entries[0].Key != MicroKey(1, 3) {
		t.Fatalf("unexpected entries: %+v", entries)
	}
	if got := revisionOf(t, pub, ctx, BucketMicroConfig, MicroKey(1, 3)); entries[0].Revision != got {
		t.Errorf("revision = %d, want %d", entries[0].Revision, got)
	}
}
