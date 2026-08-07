package messaging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/AliAladraj/Central-Config-Stream/internal/obs"

	"github.com/nats-io/nats.go/jetstream"
)

// observePublish records the outcome of one KV publish. Every write-through
// path is best-effort — the caller logs and lets the reconciler heal — so the
// counters are the only place a rising failure rate is visible. A skipped
// publish is its own outcome: in steady state a full sweep should skip
// everything, which is what makes "the fleet is not being pushed to" visible.
func observePublish(bucket string, skipped bool, err error) error {
	obs.KVPublishAttempts.Inc("bucket", bucket)
	switch {
	case err != nil:
		obs.KVPublishFailures.Inc("bucket", bucket)
		return err
	case skipped:
		obs.KVPublishSkipped.Inc("bucket", bucket)
	default:
		obs.KVPublishSuccess.Inc("bucket", bucket)
	}
	return nil
}

// publishAttempts bounds the revision-checked write loop. Each retry re-reads
// what KV ended up holding and rebuilds the value from it, so a couple of
// rounds settle any realistic race between two admins; past that the key is
// genuinely contended and the reconciler is the better place to converge it.
const publishAttempts = 3

// stored is what KV holds under a key: the bytes, plus the revision they were
// read at, which is what the write is then conditioned on. A zero revision
// means the key is absent — the ordinary "publish this" case, not an error.
type stored struct {
	value    []byte
	revision uint64
}

func current(ctx context.Context, kv jetstream.KeyValue, bucket, key string) (stored, error) {
	entry, err := kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrKeyDeleted) {
			return stored{}, nil
		}
		return stored{}, fmt.Errorf("messaging: kv get %s/%s: %w", bucket, key, err)
	}
	return stored{value: entry.Value(), revision: entry.Revision()}, nil
}

// buildValue derives, from the bytes KV currently holds, the bytes to write and
// the bytes to compare those already there against. The two differ only for
// flags, whose payload carries a timestamp that must not count as a change on
// its own.
type buildValue func(cur []byte) (write, compare []byte, err error)

// publish writes what build derives from the stored value, conditioned on the
// revision that value was read at. A plain Put would let two admins updating
// the same key leave KV holding the older of the two writes while the database
// holds the newer, with both requests answering 200; a revision-checked write
// turns that into a conflict, which is re-resolved against whatever KV actually
// ended up with.
//
// It reports whether the write was skipped. A KV write of a byte-identical
// value still cuts a revision and still pushes to every watcher, so without the
// skip a full sweep fans a no-op update out to the whole fleet — every
// consumer's OnChange handler rebuilding clients and resetting log levels for
// nothing — and burns the bucket's history window on identical revisions. Only
// exact equality skips: anything else, including a value KV holds that the
// database disagrees with, is still published, so the reconciler keeps healing
// drift.
func publish(ctx context.Context, kv jetstream.KeyValue, bucket, key string, build buildValue) (bool, error) {
	var conflict error
	for attempt := 0; attempt < publishAttempts; attempt++ {
		cur, err := current(ctx, kv, bucket, key)
		if err != nil {
			return false, err
		}

		write, compare, err := build(cur.value)
		if err != nil {
			return false, err
		}
		if cur.value != nil && bytes.Equal(cur.value, compare) {
			return true, nil
		}

		if cur.revision == 0 {
			_, err = kv.Create(ctx, key, write)
		} else {
			_, err = kv.Update(ctx, key, write, cur.revision)
		}
		switch {
		case err == nil:
			return false, nil
		case errors.Is(err, jetstream.ErrKeyExists):
			// The revision moved between the read and the write, so somebody
			// else published first. Their value is the newer one: re-read and
			// rebuild rather than overwriting it with what was already stale.
			conflict = err
		default:
			return false, fmt.Errorf("messaging: kv put %s/%s: %w", bucket, key, err)
		}
	}
	return false, fmt.Errorf("messaging: kv put %s/%s: %w", bucket, key, conflict)
}

// raw builds a value that is stored exactly as given — the appsettings tree and
// the translation bundle are published verbatim, so what is written and what is
// compared are the same bytes.
func raw(value []byte) buildValue {
	return func([]byte) ([]byte, []byte, error) { return value, value, nil }
}

// KeyRevision pairs a KV key with the revision of the value it holds. The
// revision is a per-bucket sequence, so comparing two readings of it is how a
// caller decides whether a key changed between them without trusting a clock.
type KeyRevision struct {
	Key      string
	Revision uint64
}

type FlagPayload struct {
	Enabled   bool   `json:"enabled"`
	Value     string `json:"value"`
	UpdatedAt string `json:"updatedAt"` // RFC3339
}

type ConfigPublisher interface {
	PublishFlag(ctx context.Context, environmentID int64, flagKey string, payload FlagPayload) error
	PublishServiceSettings(ctx context.Context, environmentID, microserviceID int64, settings json.RawMessage) error
	PublishLocalization(ctx context.Context, environmentID, microserviceID int64, locale string, bundle json.RawMessage) error

	// ListKeys returns every key currently held in the named bucket. A bucket
	// that is empty or does not exist yields no keys and no error.
	ListKeys(ctx context.Context, bucket string) ([]string, error)
	// ListRevisions returns the same keys with the revision each one is
	// currently at. The reconciler needs the revisions to tell a key that has
	// sat untouched since its sweep began from one written during it.
	ListRevisions(ctx context.Context, bucket string) ([]KeyRevision, error)
	// DeleteKey removes a key from the named bucket. This is how a row deleted
	// from the database stops being served: without it the KV entry survives
	// forever and consumers keep the stale value in memory.
	DeleteKey(ctx context.Context, bucket, key string) error
	// EnsureBuckets re-provisions the buckets, so a NATS restart with an empty
	// store does not leave every publish failing until this process restarts.
	EnsureBuckets(ctx context.Context) error
}

type Publisher struct {
	buckets *Buckets
}

func NewPublisher(b *Buckets) *Publisher {
	return &Publisher{buckets: b}
}

func (p *Publisher) PublishFlag(ctx context.Context, environmentID int64, flagKey string, payload FlagPayload) error {
	skipped, err := p.publishFlag(ctx, environmentID, flagKey, payload)
	return observePublish(BucketFlags, skipped, err)
}

func (p *Publisher) publishFlag(ctx context.Context, environmentID int64, flagKey string, payload FlagPayload) (bool, error) {
	kv, err := p.buckets.byName(BucketFlags)
	if err != nil {
		return false, err
	}
	key := FlagKey(environmentID, flagKey)

	return publish(ctx, kv, BucketFlags, key, func(cur []byte) ([]byte, []byte, error) {
		// No caller sets UpdatedAt — it is stamped here — so a fresh timestamp
		// on every call would make every payload differ and defeat the
		// comparison entirely. "Unchanged" therefore means the same flag state,
		// not the same bytes for the same instant: the stored timestamp is
		// carried into the value compared against, and a fresh one only into
		// the value actually written. A caller that does supply UpdatedAt is
		// compared on it as written.
		written, compared := payload, payload
		if payload.UpdatedAt == "" {
			var prev FlagPayload
			if err := json.Unmarshal(cur, &prev); err == nil {
				compared.UpdatedAt = prev.UpdatedAt
			}
			written.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		}

		w, err := json.Marshal(written)
		if err != nil {
			return nil, nil, fmt.Errorf("messaging: marshal flag payload: %w", err)
		}
		c, err := json.Marshal(compared)
		if err != nil {
			return nil, nil, fmt.Errorf("messaging: marshal flag payload: %w", err)
		}
		return w, c, nil
	})
}

func (p *Publisher) PublishServiceSettings(ctx context.Context, environmentID, microserviceID int64, settings json.RawMessage) error {
	skipped, err := p.publishServiceSettings(ctx, environmentID, microserviceID, settings)
	return observePublish(BucketServiceSettings, skipped, err)
}

func (p *Publisher) publishServiceSettings(ctx context.Context, environmentID, microserviceID int64, settings json.RawMessage) (bool, error) {
	if len(settings) == 0 {
		return false, fmt.Errorf("messaging: empty servicesettings settings")
	}
	kv, err := p.buckets.byName(BucketServiceSettings)
	if err != nil {
		return false, err
	}
	return publish(ctx, kv, BucketServiceSettings, ServiceSettingsKey(environmentID, microserviceID), raw(settings))
}

func (p *Publisher) PublishLocalization(ctx context.Context, environmentID, microserviceID int64, locale string, bundle json.RawMessage) error {
	skipped, err := p.publishLocalization(ctx, environmentID, microserviceID, locale, bundle)
	return observePublish(BucketLocalization, skipped, err)
}

func (p *Publisher) publishLocalization(ctx context.Context, environmentID, microserviceID int64, locale string, bundle json.RawMessage) (bool, error) {
	if locale == "" {
		return false, fmt.Errorf("messaging: empty locale")
	}
	if len(bundle) == 0 {
		return false, fmt.Errorf("messaging: empty localization bundle")
	}
	kv, err := p.buckets.byName(BucketLocalization)
	if err != nil {
		return false, err
	}
	return publish(ctx, kv, BucketLocalization, LocalizationKey(environmentID, microserviceID, locale), raw(bundle))
}

func (p *Publisher) ListKeys(ctx context.Context, bucket string) ([]string, error) {
	kv, err := p.buckets.byName(bucket)
	if err != nil {
		return nil, err
	}

	lister, err := kv.ListKeys(ctx)
	if err != nil {
		// Nothing to enumerate is not a failure — the caller simply has no
		// keys to compare against this cycle.
		if errors.Is(err, jetstream.ErrNoKeysFound) || errors.Is(err, jetstream.ErrBucketNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("messaging: kv list keys %s: %w", bucket, err)
	}
	defer func() { _ = lister.Stop() }()

	var keys []string
	for k := range lister.Keys() {
		keys = append(keys, k)
	}
	return keys, nil
}

// ListRevisions walks the bucket's current values metadata-only, so a sweep
// over thousands of keys costs one pass and none of the payloads.
func (p *Publisher) ListRevisions(ctx context.Context, bucket string) ([]KeyRevision, error) {
	kv, err := p.buckets.byName(bucket)
	if err != nil {
		return nil, err
	}

	watcher, err := kv.WatchAll(ctx, jetstream.IgnoreDeletes(), jetstream.MetaOnly())
	if err != nil {
		// Nothing to enumerate is not a failure — the caller simply has no
		// keys to compare against this cycle.
		if errors.Is(err, jetstream.ErrNoKeysFound) || errors.Is(err, jetstream.ErrBucketNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("messaging: kv list revisions %s: %w", bucket, err)
	}
	defer func() { _ = watcher.Stop() }()

	var out []KeyRevision
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case entry := <-watcher.Updates():
			// A nil entry closes the initial snapshot; everything after it
			// would be a live update, which is not what is being asked for.
			if entry == nil {
				return out, nil
			}
			out = append(out, KeyRevision{Key: entry.Key(), Revision: entry.Revision()})
		}
	}
}

func (p *Publisher) DeleteKey(ctx context.Context, bucket, key string) error {
	err := p.deleteKey(ctx, bucket, key)
	if err != nil {
		obs.KVDeleteFailures.Inc("bucket", bucket)
	}
	return err
}

func (p *Publisher) deleteKey(ctx context.Context, bucket, key string) error {
	kv, err := p.buckets.byName(bucket)
	if err != nil {
		return err
	}
	// Purge rather than Delete: it drops the key's history too, so a bucket
	// does not accumulate the full contents of every row ever removed.
	if err := kv.Purge(ctx, key); err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil
		}
		return fmt.Errorf("messaging: kv purge %s/%s: %w", bucket, key, err)
	}
	return nil
}

func (p *Publisher) EnsureBuckets(ctx context.Context) error {
	return p.buckets.Ensure(ctx)
}

var _ ConfigPublisher = (*Publisher)(nil)
