package messaging

import (
	"context"
	"log/slog"
	"time"

	"github.com/ErasedKyte/Central-Config-Stream/internal/obs"
)

// recLog is the reconciler's logger. The sweep can touch thousands of keys, so
// per-key detail is emitted at debug and the info lines carry counts instead.
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
	// Bucket is the KV bucket this source owns. On a full sweep the keys Resync
	// returned are taken as the complete contents of that bucket and everything
	// else in it is deleted.
	Bucket() string
	// Resync publishes rows changed at or after `since` and returns the KV keys
	// it published. A zero `since` means "publish everything" (full sweep).
	Resync(ctx context.Context, pub ConfigPublisher, since time.Time) ([]string, error)
}

// fullSweepEvery forces a complete republish every N cycles, so keys deleted
// from KV out-of-band (or lost with a memory-storage bucket) are restored even
// though their rows have not changed recently.
const fullSweepEvery = 12

// reconcileOverlap is subtracted from the last run time when building the
// incremental window, absorbing clock skew between this service and the DB.
const reconcileOverlap = time.Minute

// Reconciler periodically resyncs every registered source.
type Reconciler struct {
	interval time.Duration
	pub      ConfigPublisher
	sources  []ReconcileSource

	runs    int       // cycles completed, drives the periodic full sweep
	lastRun time.Time // start of the previous cycle; bounds the incremental window
}

func NewReconciler(interval time.Duration, pub ConfigPublisher, sources ...ReconcileSource) *Reconciler {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	return &Reconciler{interval: interval, pub: pub, sources: sources}
}

// Start runs an initial resync, then repeats on the interval until the returned
// stop function is called (or ctx is cancelled). Non-blocking.
func (r *Reconciler) Start(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
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
	return cancel
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
	// exists. Re-provisioning is idempotent, so this is a cheap no-op normally.
	if err := r.pub.EnsureBuckets(ctx); err != nil {
		recLog.Error("ensure buckets failed", obs.Err(err))
	}

	full := since.IsZero()
	sweep := "incremental"
	if full {
		sweep = "full"
	}

	failed := false
	for _, s := range r.sources {
		keys, err := s.Resync(ctx, r.pub, since)
		if err != nil {
			failed = true
			obs.ReconcileSourceFailures.Inc("source", s.Name())
			recLog.Error("source resync failed", slog.String("source", s.Name()), obs.Err(err))
			continue
		}
		if len(keys) > 0 {
			obs.ReconcileKeysRepublished.Add(int64(len(keys)), "source", s.Name())
			recLog.Info("source republished",
				slog.String("source", s.Name()),
				slog.Int("count", len(keys)),
				slog.String("sweep", sweep))
		}
		// Only a full sweep read every row, so only a full sweep may conclude
		// that a key has no row behind it any more. Pruning on an incremental
		// sweep would wipe every key that simply had not changed recently.
		if full {
			r.prune(ctx, s, keys)
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

// prune deletes the keys in a source's bucket that its full resync did not
// publish — the KV side of a row deleted from the database, which otherwise
// survives forever and keeps being served from consumer caches. Failures are
// logged and skipped: a lingering key is less harmful than aborting the cycle.
func (r *Reconciler) prune(ctx context.Context, s ReconcileSource, published []string) {
	bucket := s.Bucket()
	if bucket == "" {
		return
	}

	actual, err := r.pub.ListKeys(ctx, bucket)
	if err != nil {
		recLog.Error("list keys failed", slog.String("bucket", bucket), obs.Err(err))
		return
	}

	keep := make(map[string]struct{}, len(published))
	for _, k := range published {
		keep[k] = struct{}{}
	}

	pruned := 0
	for _, k := range actual {
		if _, ok := keep[k]; ok {
			continue
		}
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
