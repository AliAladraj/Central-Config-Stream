package messaging

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/ErasedKyte/Central-Config-Stream/internal/obs"
)

// recLog is the reconciler's logger. The sweep can touch thousands of keys, so
// per-key detail is emitted at debug and the info lines carry counts instead —
// except for a key that failed to publish, which is the one thing an operator
// has to be able to name.
var recLog = obs.Logger("reconciler")

// ReconcileSource re-publishes a domain's current Oracle state to KV. Called on
// startup and on an interval so KV eventually matches Oracle even if a
// write-through publish was dropped (the dual-write healing mechanism).
//
// Implementations live in the app layer (they read Oracle); messaging stays
// decoupled from the domains to avoid an import cycle. Resync must be
// idempotent — the publisher reads before writing, so republishing a value KV
// already holds is skipped rather than pushed to every consumer.
type ReconcileSource interface {
	// Name identifies the source in logs.
	Name() string
	// Bucket is the KV bucket this source owns. On a complete full sweep the
	// keys Resync returned are taken as the contents of that bucket and
	// everything else in it is a candidate for deletion.
	Bucket() string
	// Resync publishes rows changed at or after `since` and reports the KV keys
	// it published. A zero `since` means "publish everything" (full sweep). An
	// error means the source could not be read at all; a row that could not be
	// published is recorded on the result instead, so one bad row does not
	// strand the ones behind it.
	Resync(ctx context.Context, pub ConfigPublisher, since time.Time) (ResyncResult, error)
}

// ResyncResult is what one source reports back from a sweep.
type ResyncResult struct {
	// Keys are the KV keys the sweep published successfully.
	Keys []string
	// Partial is set when at least one row could not be published. The rest of
	// the sweep still ran, but the result no longer describes the bucket, so it
	// may neither be pruned against nor advance the incremental window.
	Partial bool
}

// Publish records the outcome of publishing one row. A failure is logged
// against the key that produced it: the source name alone leaves an operator
// with a whole domain to search for the row that is wedging convergence.
func (r *ResyncResult) Publish(source, key string, err error) {
	if err != nil {
		r.Partial = true
		recLog.Error("republish key failed",
			slog.String("source", source), slog.String("kv.key", key), obs.Err(err))
		return
	}
	r.Keys = append(r.Keys, key)
}

// fullSweepEvery forces a complete republish every N cycles, so keys deleted
// from KV out-of-band (or lost with a memory-storage bucket) are restored even
// though their rows have not changed recently.
const fullSweepEvery = 12

// reconcileOverlap is subtracted from the last run time when building the
// incremental window, absorbing clock skew between this service and the DB.
const reconcileOverlap = time.Minute

// Pruning limits. A prune exists to trim the keys left behind by deleted rows,
// which is a trickle; a sweep proposing to delete a large share of a bucket is
// describing a broken sweep — a truncated table, a connection string pointed at
// an empty database — and carrying it through would empty every consumer's
// cache fleet-wide. defaultPruneMaxFraction is the share of a bucket one cycle
// may remove, overridable with RECONCILE_PRUNE_MAX_FRACTION; minPruneKeys is
// the absolute allowance underneath it, because a fraction of a handful of keys
// rounds down to "never delete anything".
const (
	defaultPruneMaxFraction = 0.2
	minPruneKeys            = 5
)

// Reconciler periodically resyncs every registered source.
type Reconciler struct {
	interval         time.Duration
	pub              ConfigPublisher
	sources          []ReconcileSource
	pruneMaxFraction float64

	runs    int       // cycles completed, drives the periodic full sweep
	lastRun time.Time // start of the previous cycle; bounds the incremental window
}

func NewReconciler(interval time.Duration, pub ConfigPublisher, sources ...ReconcileSource) *Reconciler {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	return &Reconciler{
		interval:         interval,
		pub:              pub,
		sources:          sources,
		pruneMaxFraction: pruneMaxFractionFromEnv(),
	}
}

// pruneMaxFractionFromEnv reads the prune ceiling. Anything outside (0,1] is
// ignored rather than honoured: a misread value here deletes production data.
func pruneMaxFractionFromEnv() float64 {
	v, ok := os.LookupEnv("RECONCILE_PRUNE_MAX_FRACTION")
	if !ok {
		return defaultPruneMaxFraction
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 || f > 1 {
		return defaultPruneMaxFraction
	}
	return f
}

// Start runs an initial resync, then repeats on the interval until the returned
// stop function is called (or ctx is cancelled). Non-blocking.
//
// stop waits for the running cycle to unwind. Without that wait the shutdown
// path drains NATS and closes the database out from under a sweep still in a
// query, which surfaces as an error log on every rolling restart.
func (r *Reconciler) Start(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)

	var done sync.WaitGroup
	done.Add(1)
	go func() {
		defer done.Done()
		r.runOnce(ctx) // startup sweep
		ticker := time.NewTicker(r.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.runOnce(ctx)
			}
		}
	}()

	return func() {
		cancel()
		done.Wait()
	}
}

// runOnce sweeps every source. The first cycle and every fullSweepEvery-th
// cycle republish everything; the cycles in between only read rows changed
// since the previous run, which is what keeps the steady-state DB cost small.
// runOnce is only ever called from the single goroutine started by Start, so
// runs/lastRun need no locking.
func (r *Reconciler) runOnce(ctx context.Context) {
	started := time.Now()

	var since time.Time // zero = full sweep
	if !r.lastRun.IsZero() && r.runs%fullSweepEvery != 0 {
		since = r.lastRun.Add(-reconcileOverlap)
	}

	// NATS may have come back with an empty store since the last cycle, in
	// which case every publish below would fail against a bucket that no longer
	// exists. Re-provisioning is idempotent, so this is a cheap no-op normally,
	// and it is also what brings the buckets up after a start with NATS down.
	if err := r.pub.EnsureBuckets(ctx); err != nil {
		if ctx.Err() != nil {
			return
		}
		recLog.Error("ensure buckets failed", obs.Err(err))
	}

	full := since.IsZero()
	sweep := "incremental"
	if full {
		sweep = "full"
	}

	failed := false
	for _, s := range r.sources {
		// The snapshot has to be taken before the source reads the database: a
		// key written in between has no row in the sweep's result through no
		// fault of its own, and pruning it would un-publish a live value.
		var atStart []KeyRevision
		snapshotted := true
		if full {
			atStart, snapshotted = r.snapshot(ctx, s)
		}

		res, err := s.Resync(ctx, r.pub, since)
		if ctx.Err() != nil {
			return // shutting down; the next process start sweeps from scratch
		}
		if err != nil {
			failed = true
			obs.ReconcileSourceFailures.Inc("source", s.Name())
			recLog.Error("source resync failed", slog.String("source", s.Name()), obs.Err(err))
			continue
		}
		if len(res.Keys) > 0 {
			obs.ReconcileKeysRepublished.Add(int64(len(res.Keys)), "source", s.Name())
			recLog.Info("source republished",
				slog.String("source", s.Name()),
				slog.Int("count", len(res.Keys)),
				slog.String("sweep", sweep))
		}
		// A row that would not publish is drift this cycle failed to repair,
		// but the rows behind it were still republished, so the cycle is a
		// partial success rather than an abort.
		if res.Partial {
			failed = true
			obs.ReconcileSourceFailures.Inc("source", s.Name())
			recLog.Error("source resync completed with unpublished keys",
				slog.String("source", s.Name()), slog.Int("published", len(res.Keys)))
		}
		// Only a complete full sweep read every row, so only it may conclude
		// that a key has no row behind it any more. Pruning on an incremental
		// sweep would wipe every key that simply had not changed recently, and
		// pruning on a partial one would wipe whatever it failed to publish.
		if full && !res.Partial && snapshotted {
			r.prune(ctx, s, res.Keys, atStart)
		}
	}

	r.runs++
	// Only advance the window on a clean cycle: a failed source must not have
	// its unpublished changes fall outside the next incremental query.
	result := "ok"
	if failed {
		result = "failed"
	} else {
		r.lastRun = started
		obs.ReconcileLastSuccess.Set(float64(time.Now().Unix()))
	}
	obs.ReconcileCycles.Inc("sweep", sweep, "result", result)
	obs.ReconcileDuration.Observe(time.Since(started).Seconds(), "sweep", sweep)
}

// snapshot records where a source's bucket stood before its sweep read the
// database. It reports whether the reading is usable: without one there is no
// way to tell a stale key from a freshly written one, and prune is skipped for
// the cycle rather than run blind.
func (r *Reconciler) snapshot(ctx context.Context, s ReconcileSource) ([]KeyRevision, bool) {
	bucket := s.Bucket()
	if bucket == "" {
		return nil, true
	}
	entries, err := r.pub.ListRevisions(ctx, bucket)
	if err != nil {
		if ctx.Err() == nil {
			recLog.Error("snapshot bucket failed, skipping prune",
				slog.String("bucket", bucket), obs.Err(err))
		}
		return nil, false
	}
	return entries, true
}

// prune deletes the keys in a source's bucket that its full resync did not
// publish — the KV side of a row deleted from the database, which otherwise
// survives forever and keeps being served from consumer caches. Failures are
// logged and skipped: a lingering key is less harmful than aborting the cycle.
//
// atStart is where the bucket stood before the sweep read the database. Only a
// key that is still at the revision it had then is a candidate: anything
// created or rewritten while the sweep ran was never in the set the sweep read,
// and two replicas each running their own reconciler would otherwise prune each
// other's writes.
func (r *Reconciler) prune(ctx context.Context, s ReconcileSource, published []string, atStart []KeyRevision) {
	bucket := s.Bucket()
	if bucket == "" {
		return
	}

	actual, err := r.pub.ListRevisions(ctx, bucket)
	if err != nil {
		if ctx.Err() == nil {
			recLog.Error("list keys failed", slog.String("bucket", bucket), obs.Err(err))
		}
		return
	}

	keep := make(map[string]struct{}, len(published))
	for _, k := range published {
		keep[k] = struct{}{}
	}
	unchanged := make(map[string]uint64, len(atStart))
	for _, e := range atStart {
		unchanged[e.Key] = e.Revision
	}

	var stale []string
	for _, e := range actual {
		if _, ok := keep[e.Key]; ok {
			continue
		}
		if rev, ok := unchanged[e.Key]; !ok || rev != e.Revision {
			recLog.Debug("kept a key written during the sweep",
				slog.String("bucket", bucket), slog.String("kv.key", e.Key))
			continue
		}
		stale = append(stale, e.Key)
	}
	if len(stale) == 0 {
		return
	}

	// A sweep that read no rows at all describes a database that answered, not
	// one that is empty on purpose; taking it at its word empties the bucket.
	limit := max(minPruneKeys, int(float64(len(actual))*r.pruneMaxFraction))
	if len(published) == 0 || len(stale) > limit {
		obs.ReconcilePruneRefused.Inc("bucket", bucket)
		recLog.Error("refusing to prune: too much of the bucket has no row behind it",
			slog.String("bucket", bucket),
			slog.Int("stale", len(stale)),
			slog.Int("keys", len(actual)),
			slog.Int("published", len(published)),
			slog.Int("limit", limit))
		return
	}

	pruned := 0
	for _, k := range stale {
		if err := r.pub.DeleteKey(ctx, bucket, k); err != nil {
			recLog.Error("delete stale key failed",
				slog.String("bucket", bucket), slog.String("kv.key", k), obs.Err(err))
			continue
		}
		pruned++
		recLog.Debug("deleted stale key (no matching row in the database)",
			slog.String("bucket", bucket), slog.String("kv.key", k))
	}
	if pruned > 0 {
		obs.ReconcileKeysPruned.Add(int64(pruned), "bucket", bucket)
		recLog.Info("pruned stale keys", slog.String("bucket", bucket), slog.Int("count", pruned))
	}
}
