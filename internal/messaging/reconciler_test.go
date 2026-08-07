package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/AliAladraj/Central-Config-Stream/internal/obs"
)

// capturePublisher records publish calls and stands in for the KV buckets: it
// keeps a per-bucket key set, with the revision each key is at, so both the
// prune path and its "was this written during the sweep?" guard can be
// exercised without NATS.
type capturePublisher struct {
	mu    sync.Mutex
	flags int
	micro int
	loc   int

	keys    map[string]map[string]uint64 // bucket -> key -> current revision
	ensures int
}

func newCapturePublisher() *capturePublisher {
	return &capturePublisher{keys: map[string]map[string]uint64{}}
}

func (c *capturePublisher) PublishFlag(context.Context, int64, string, FlagPayload) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.flags++
	return nil
}
func (c *capturePublisher) PublishServiceSettings(context.Context, int64, int64, json.RawMessage) error {
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

func (c *capturePublisher) ListRevisions(_ context.Context, bucket string) ([]KeyRevision, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []KeyRevision
	for k, rev := range c.keys[bucket] {
		out = append(out, KeyRevision{Key: k, Revision: rev})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
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
		c.keys[bucket] = map[string]uint64{}
	}
	for _, k := range keys {
		c.keys[bucket][k] = 1
	}
}

// write puts a key at a new revision, standing in for somebody publishing to
// the bucket while a sweep is running.
func (c *capturePublisher) write(bucket, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.keys[bucket] == nil {
		c.keys[bucket] = map[string]uint64{}
	}
	c.keys[bucket][key]++
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

	// partial stands in for a source that published some of its rows and could
	// not publish the rest.
	partial bool

	// keys is what Resync reports as published; on a full sweep the reconciler
	// treats it as the complete contents of the bucket.
	mu   sync.Mutex
	keys []string
}

func (f *fakeSource) Name() string   { return f.name }
func (f *fakeSource) Bucket() string { return f.bucket }
func (f *fakeSource) Resync(ctx context.Context, pub ConfigPublisher, since time.Time) (ResyncResult, error) {
	if f.err != nil {
		return ResyncResult{}, f.err
	}
	for i := 0; i < f.count; i++ {
		_ = pub.PublishFlag(ctx, 1, "k", FlagPayload{})
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return ResyncResult{
		Keys:    append([]string(nil), f.keys...),
		Partial: f.partial,
	}, nil
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

// keySource publishes one key per entry and lets the test decide which of them
// fail, which is what an unpublishable row looks like to the reconciler.
type keySource struct {
	name   string
	bucket string
	keys   []string
	fail   map[string]error

	// during runs after the rows have been published, standing in for an admin
	// write that lands while the sweep is still going.
	during func()
}

func (s *keySource) Name() string   { return s.name }
func (s *keySource) Bucket() string { return s.bucket }
func (s *keySource) Resync(_ context.Context, _ ConfigPublisher, _ time.Time) (ResyncResult, error) {
	res := ResyncResult{Keys: make([]string, 0, len(s.keys))}
	for _, k := range s.keys {
		res.Publish(s.name, k, s.fail[k])
	}
	if s.during != nil {
		s.during()
	}
	return res, nil
}

// One row that will not publish used to abort the whole source: every row
// behind it stayed unpublished, the cycle never advanced its window, and the
// next cycle degenerated into a full sweep that died on the same row. The rows
// behind it have to get through, and a second source in the same cycle has to
// finish its own work.
func TestPartialSweepDoesNotStrandTheRestOrOtherSources(t *testing.T) {
	pub := newCapturePublisher()
	pub.seed(BucketFlags, "1.a", "1.bad", "1.c")
	pub.seed(BucketServiceSettings, "1.1", "1.gone")

	bad := &keySource{
		name:   "flags",
		bucket: BucketFlags,
		keys:   []string{"1.a", "1.bad", "1.c"},
		fail:   map[string]error{"1.bad": errors.New("value too large")},
	}
	good := &keySource{name: "servicesettings", bucket: BucketServiceSettings, keys: []string{"1.1"}}

	rec := NewReconciler(time.Hour, pub, bad, good)
	rec.runOnce(context.Background())

	// The row after the failing one was still published, and the sweep knows
	// it did not describe the whole bucket.
	res, err := bad.Resync(context.Background(), pub, time.Time{})
	if err != nil {
		t.Fatalf("resync: %v", err)
	}
	if !res.Partial {
		t.Error("a failed key did not mark the sweep partial")
	}
	if len(res.Keys) != 2 || res.Keys[0] != "1.a" || res.Keys[1] != "1.c" {
		t.Errorf("published keys = %v, want the two that succeeded", res.Keys)
	}

	// The partial source may not prune: its keep set is missing whatever it
	// failed to publish, so pruning against it would delete a live key.
	if !pub.has(BucketFlags, "1.bad") {
		t.Error("a partial sweep pruned the key it had just failed to publish")
	}

	// The source that completed still pruned, rather than being held back by
	// the other one's failure.
	if pub.has(BucketServiceSettings, "1.gone") {
		t.Error("a healthy source did not prune because another source was partial")
	}
}

// A partial cycle must not advance the incremental window: rows it failed to
// publish have to stay inside the next query's range.
func TestPartialSweepDoesNotAdvanceTheWindow(t *testing.T) {
	pub := newCapturePublisher()
	src := &keySource{
		name:   "flags",
		bucket: BucketFlags,
		keys:   []string{"1.a"},
		fail:   map[string]error{"1.a": errors.New("boom")},
	}

	rec := NewReconciler(time.Hour, pub, src)
	rec.runOnce(context.Background())

	if !rec.lastRun.IsZero() {
		t.Error("a partial cycle advanced lastRun, so the failed rows fall outside the next window")
	}
	if got := obs.ReconcileCycles.Value("sweep", "full", "result", "failed"); got == 0 {
		t.Error("a partial cycle was not counted as failed")
	}
}

// The keep set comes from a database read taken at the start of the sweep, but
// the bucket is listed at the end of it. A key written in between has no row in
// that read through no fault of its own — and with two replicas each running
// their own reconciler, that key is routinely the other replica's write.
func TestPruneKeepsAKeyWrittenDuringTheSweep(t *testing.T) {
	pub := newCapturePublisher()
	pub.seed(BucketFlags, "1.live", "1.gone")

	src := &keySource{
		name:   "flags",
		bucket: BucketFlags,
		keys:   []string{"1.live"},
		during: func() { pub.write(BucketFlags, "1.brand_new") },
	}

	NewReconciler(time.Hour, pub, src).runOnce(context.Background())

	if !pub.has(BucketFlags, "1.brand_new") {
		t.Error("prune deleted a key written after the sweep read the database")
	}
	if pub.has(BucketFlags, "1.gone") {
		t.Error("prune left behind a key that was stale for the whole sweep")
	}
}

// A key that existed before the sweep but was rewritten during it is equally
// out of the sweep's reach: the value it now holds is newer than the read.
func TestPruneKeepsAKeyRewrittenDuringTheSweep(t *testing.T) {
	pub := newCapturePublisher()
	pub.seed(BucketFlags, "1.live", "1.flipped")

	src := &keySource{
		name:   "flags",
		bucket: BucketFlags,
		keys:   []string{"1.live"},
		during: func() { pub.write(BucketFlags, "1.flipped") },
	}

	NewReconciler(time.Hour, pub, src).runOnce(context.Background())

	if !pub.has(BucketFlags, "1.flipped") {
		t.Error("prune deleted a key that was republished mid-sweep")
	}
}

// An empty-but-successful read of the database — a truncated table, a
// connection string pointed at the wrong instance — must not be taken at its
// word: honouring it empties the bucket and every consumer's cache with it.
func TestPruneRefusesToEmptyABucket(t *testing.T) {
	pub := newCapturePublisher()
	live := []string{"1.a", "1.b", "1.c", "1.d", "1.e", "1.f"}
	pub.seed(BucketFlags, live...)

	src := &keySource{name: "flags", bucket: BucketFlags} // read no rows at all
	refused := obs.ReconcilePruneRefused.Value("bucket", BucketFlags)

	NewReconciler(time.Hour, pub, src).runOnce(context.Background())

	for _, k := range live {
		if !pub.has(BucketFlags, k) {
			t.Fatalf("prune deleted %s after a sweep that read nothing", k)
		}
	}
	if got := obs.ReconcilePruneRefused.Value("bucket", BucketFlags); got != refused+1 {
		t.Errorf("prune-refused counter = %d, want %d", got, refused+1)
	}
}

// The same floor holds when the sweep did read rows but is proposing to delete
// far more of the bucket than a trickle of deleted rows could account for.
func TestPruneRefusesAboveTheCeiling(t *testing.T) {
	pub := newCapturePublisher()
	var all []string
	for i := 0; i < 40; i++ {
		all = append(all, "1.k"+strconv.Itoa(i))
	}
	pub.seed(BucketFlags, all...)

	// 30 of 40 keys have no row behind them: 75% of the bucket, far past the
	// 20% ceiling.
	src := &keySource{name: "flags", bucket: BucketFlags, keys: all[:10]}
	refused := obs.ReconcilePruneRefused.Value("bucket", BucketFlags)

	NewReconciler(time.Hour, pub, src).runOnce(context.Background())

	for _, k := range all {
		if !pub.has(BucketFlags, k) {
			t.Fatalf("prune deleted %s despite exceeding the ceiling", k)
		}
	}
	if got := obs.ReconcilePruneRefused.Value("bucket", BucketFlags); got != refused+1 {
		t.Errorf("prune-refused counter = %d, want %d", got, refused+1)
	}

	// A trickle under the ceiling still gets through, so the floor has not
	// simply disabled pruning.
	src.keys = all[:38]
	rec := NewReconciler(time.Hour, pub, src)
	rec.runOnce(context.Background())
	if pub.has(BucketFlags, all[39]) {
		t.Error("prune refused a deletion well inside the ceiling")
	}
}

// Shutdown cancels the reconciler and then drains NATS and closes the database.
// Without waiting for the cycle to unwind, those run while a sweep is still in
// a query, which is an error log on every rolling restart.
func TestStopWaitsForTheRunningCycle(t *testing.T) {
	pub := newCapturePublisher()
	released := make(chan struct{})
	finished := make(chan struct{})

	src := &keySource{
		name:   "flags",
		bucket: BucketFlags,
		during: func() {
			<-released
			close(finished)
		},
	}

	stop := NewReconciler(time.Hour, pub, src).Start(context.Background())

	go func() {
		time.Sleep(50 * time.Millisecond)
		close(released)
	}()
	stop()

	select {
	case <-finished:
	default:
		t.Fatal("stop returned while a sweep was still running")
	}
}
