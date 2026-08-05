package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/ErasedKyte/Central-Config-Stream/internal/obs"
)

// capturePublisher records publish calls and stands in for the KV buckets: it
// keeps a per-bucket key set so the prune path can be exercised without NATS.
type capturePublisher struct {
	mu    sync.Mutex
	flags int
	micro int
	loc   int

	keys    map[string]map[string]struct{} // bucket -> keys currently in KV
	ensures int
}

func newCapturePublisher() *capturePublisher {
	return &capturePublisher{keys: map[string]map[string]struct{}{}}
}

func (c *capturePublisher) PublishFlag(context.Context, int64, string, FlagPayload) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.flags++
	return nil
}
func (c *capturePublisher) PublishMicroConfig(context.Context, int64, int64, json.RawMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.micro++
	return nil
}
func (c *capturePublisher) PublishLocalization(context.Context, int64, int64, string, json.RawMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loc++
	return nil
}

func (c *capturePublisher) ListKeys(_ context.Context, bucket string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for k := range c.keys[bucket] {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

func (c *capturePublisher) DeleteKey(_ context.Context, bucket, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.keys[bucket], key)
	return nil
}

func (c *capturePublisher) EnsureBuckets(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensures++
	return nil
}

// seed places keys in a bucket, as if they had been published earlier.
func (c *capturePublisher) seed(bucket string, keys ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.keys[bucket] == nil {
		c.keys[bucket] = map[string]struct{}{}
	}
	for _, k := range keys {
		c.keys[bucket][k] = struct{}{}
	}
}

func (c *capturePublisher) has(bucket, key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.keys[bucket][key]
	return ok
}

type fakeSource struct {
	name   string
	bucket string
	count  int
	err    error

	// keys is what Resync reports as published; on a full sweep the reconciler
	// treats it as the complete contents of the bucket.
	mu   sync.Mutex
	keys []string
}

func (f *fakeSource) Name() string   { return f.name }
func (f *fakeSource) Bucket() string { return f.bucket }
func (f *fakeSource) Resync(ctx context.Context, pub ConfigPublisher, since time.Time) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	for i := 0; i < f.count; i++ {
		_ = pub.PublishFlag(ctx, 1, "k", FlagPayload{})
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.keys...), nil
}

func (f *fakeSource) setKeys(keys ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = keys
}

func TestReconcilerRunsInitialSweep(t *testing.T) {
	pub := newCapturePublisher()
	rec := NewReconciler(time.Hour, pub,
		&fakeSource{name: "a", count: 2},
		&fakeSource{name: "b", count: 3},
	)
	stop := rec.Start(context.Background())
	defer stop()

	// initial sweep is async; wait briefly for it
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pub.mu.Lock()
		n := pub.flags
		pub.mu.Unlock()
		if n == 5 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected 5 publishes from initial sweep, got %d", pub.flags)
}

func TestReconcilerContinuesAfterSourceError(t *testing.T) {
	pub := newCapturePublisher()
	rec := NewReconciler(time.Hour, pub,
		&fakeSource{name: "bad", err: errors.New("boom")},
		&fakeSource{name: "good", count: 1},
	)
	stop := rec.Start(context.Background())
	defer stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pub.mu.Lock()
		n := pub.flags
		pub.mu.Unlock()
		if n == 1 { // good source still ran despite bad one failing
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("good source did not run after bad source errored")
}

// A row deleted from the database leaves its KV key behind; the full sweep is
// the only place that can notice, because only it sees every row.
func TestReconcilerFullSweepDeletesOrphanedKeys(t *testing.T) {
	pub := newCapturePublisher()
	pub.seed(BucketFlags, "1.live", "1.deleted")

	src := &fakeSource{name: "flags", bucket: BucketFlags}
	src.setKeys("1.live") // "1.deleted" no longer has a row

	rec := NewReconciler(time.Hour, pub, src)
	rec.runOnce(context.Background()) // first cycle is always a full sweep

	if pub.has(BucketFlags, "1.deleted") {
		t.Error("full sweep left the orphaned key in KV")
	}
	if !pub.has(BucketFlags, "1.live") {
		t.Error("full sweep deleted a key that still has a row")
	}
}

// An incremental sweep only reads recently changed rows, so the keys it reports
// are not the whole bucket. Pruning on it would wipe everything unchanged.
func TestReconcilerIncrementalSweepDeletesNothing(t *testing.T) {
	pub := newCapturePublisher()
	pub.seed(BucketFlags, "1.live", "1.untouched")

	src := &fakeSource{name: "flags", bucket: BucketFlags}
	src.setKeys("1.live", "1.untouched")

	rec := NewReconciler(time.Hour, pub, src)
	ctx := context.Background()
	rec.runOnce(ctx) // full sweep; sets lastRun so the next cycle is incremental

	// Only "1.live" changed inside the next window.
	src.setKeys("1.live")
	rec.runOnce(ctx)

	if !pub.has(BucketFlags, "1.untouched") {
		t.Error("incremental sweep deleted a key it never looked at")
	}
	if !pub.has(BucketFlags, "1.live") {
		t.Error("incremental sweep deleted a republished key")
	}
}

// Buckets are re-ensured every cycle so a NATS restart with an empty store does
// not leave every publish failing until this process restarts.
func TestReconcilerReEnsuresBucketsEachCycle(t *testing.T) {
	pub := newCapturePublisher()
	rec := NewReconciler(time.Hour, pub, &fakeSource{name: "flags", bucket: BucketFlags})

	ctx := context.Background()
	rec.runOnce(ctx)
	rec.runOnce(ctx)

	pub.mu.Lock()
	defer pub.mu.Unlock()
	if pub.ensures != 2 {
		t.Fatalf("expected buckets ensured once per cycle, got %d", pub.ensures)
	}
}

// The reconcile counters are what "is drift being repaired?" is answered with,
// so a cycle has to move them: a failing source must not be recorded as a clean
// cycle, and a pruned key must be countable.
func TestReconcileCycleIsCounted(t *testing.T) {
	pub := newCapturePublisher()
	pub.seed(BucketFlags, "1.live", "1.gone")
	src := &fakeSource{name: "metrics-src", bucket: BucketFlags}
	src.setKeys("1.live", "1.also-live") // two keys republished, "1.gone" pruned
	bad := &fakeSource{name: "metrics-bad", err: errors.New("boom")}

	republished := obs.ReconcileKeysRepublished.Value("source", "metrics-src")
	pruned := obs.ReconcileKeysPruned.Value("bucket", BucketFlags)
	failures := obs.ReconcileSourceFailures.Value("source", "metrics-bad")
	cycles := obs.ReconcileCycles.Value("sweep", "full", "result", "failed")
	lastSuccess := obs.ReconcileLastSuccess.Value()

	NewReconciler(time.Hour, pub, src, bad).runOnce(context.Background())

	if got := obs.ReconcileKeysRepublished.Value("source", "metrics-src"); got != republished+2 {
		t.Errorf("republished = %d, want %d", got, republished+2)
	}
	if got := obs.ReconcileKeysPruned.Value("bucket", BucketFlags); got != pruned+1 {
		t.Errorf("pruned = %d, want %d", got, pruned+1)
	}
	if got := obs.ReconcileSourceFailures.Value("source", "metrics-bad"); got != failures+1 {
		t.Errorf("source failures = %d, want %d", got, failures+1)
	}
	if got := obs.ReconcileCycles.Value("sweep", "full", "result", "failed"); got != cycles+1 {
		t.Errorf("failed cycles = %d, want %d", got, cycles+1)
	}
	// A cycle with a failed source must not advance the freshness gauge.
	if obs.ReconcileLastSuccess.Value() != lastSuccess {
		t.Errorf("last-success gauge moved on a failed cycle")
	}
}
