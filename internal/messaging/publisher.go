package messaging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ErasedKyte/Central-Config-Stream/internal/obs"

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

// current returns the bytes stored under key, or nil when the key is absent.
// A missing key is not an error — it is the ordinary "publish this" case.
func current(ctx context.Context, kv jetstream.KeyValue, bucket, key string) ([]byte, error) {
	entry, err := kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrKeyDeleted) {
			return nil, nil
		}
		return nil, fmt.Errorf("messaging: kv get %s/%s: %w", bucket, key, err)
	}
	return entry.Value(), nil
}

// putUnlessEqual writes value only when it differs from cur, the bytes already
// stored under key. It reports whether the write was skipped.
//
// KV Put of a byte-identical value still cuts a revision and still pushes to
// every watcher, so without this a full sweep fans a no-op update out to the
// whole fleet — every consumer's OnChange handler rebuilding clients and
// resetting log levels for nothing — and burns the bucket's history window on
// identical revisions. Only exact equality skips: anything else, including a
// value KV holds that Oracle disagrees with, is still published, so the
// reconciler keeps healing drift.
func putUnlessEqual(ctx context.Context, kv jetstream.KeyValue, bucket, key string, cur, value []byte) (bool, error) {
	if cur != nil && bytes.Equal(cur, value) {
		return true, nil
	}
	return false, put(ctx, kv, bucket, key, value)
}

func put(ctx context.Context, kv jetstream.KeyValue, bucket, key string, value []byte) error {
	if _, err := kv.Put(ctx, key, value); err != nil {
		return fmt.Errorf("messaging: kv put %s/%s: %w", bucket, key, err)
	}
	return nil
}

type FlagPayload struct {
	Enabled   bool   `json:"enabled"`
	Value     string `json:"value"`
	UpdatedAt string `json:"updatedAt"` // RFC3339
}

type ConfigPublisher interface {
	PublishFlag(ctx context.Context, environmentID int64, flagKey string, payload FlagPayload) error
	PublishMicroConfig(ctx context.Context, environmentID, microserviceID int64, settings json.RawMessage) error
	PublishLocalization(ctx context.Context, environmentID, microserviceID int64, locale string, bundle json.RawMessage) error

	// ListKeys returns every key currently held in the named bucket. A bucket
	// that is empty or does not exist yields no keys and no error.
	ListKeys(ctx context.Context, bucket string) ([]string, error)
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
	cur, err := current(ctx, kv, BucketFlags, key)
	if err != nil {
		return false, err
	}

	// No caller sets UpdatedAt — it is stamped here — so a fresh timestamp on
	// every call would make every payload differ and defeat the comparison
	// entirely. "Unchanged" therefore means the same flag state, not the same
	// bytes for the same instant: the stored timestamp is carried over before
	// comparing, and a new one is stamped only when the state really changed.
	// A caller that does supply UpdatedAt is compared on it as written.
	compare := payload
	if compare.UpdatedAt == "" {
		var prev FlagPayload
		if err := json.Unmarshal(cur, &prev); err == nil {
			compare.UpdatedAt = prev.UpdatedAt
		}
	}
	b, err := json.Marshal(compare)
	if err != nil {
		return false, fmt.Errorf("messaging: marshal flag payload: %w", err)
	}
	if cur != nil && bytes.Equal(cur, b) {
		return true, nil
	}

	if payload.UpdatedAt == "" {
		payload.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		if b, err = json.Marshal(payload); err != nil {
			return false, fmt.Errorf("messaging: marshal flag payload: %w", err)
		}
	}
	return false, put(ctx, kv, BucketFlags, key, b)
}

func (p *Publisher) PublishMicroConfig(ctx context.Context, environmentID, microserviceID int64, settings json.RawMessage) error {
	skipped, err := p.publishMicroConfig(ctx, environmentID, microserviceID, settings)
	return observePublish(BucketMicroConfig, skipped, err)
}

func (p *Publisher) publishMicroConfig(ctx context.Context, environmentID, microserviceID int64, settings json.RawMessage) (bool, error) {
	if len(settings) == 0 {
		return false, fmt.Errorf("messaging: empty microconfig settings")
	}
	kv, err := p.buckets.byName(BucketMicroConfig)
	if err != nil {
		return false, err
	}
	key := MicroKey(environmentID, microserviceID)
	cur, err := current(ctx, kv, BucketMicroConfig, key)
	if err != nil {
		return false, err
	}
	return putUnlessEqual(ctx, kv, BucketMicroConfig, key, cur, settings)
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
	key := LocalizationKey(environmentID, microserviceID, locale)
	cur, err := current(ctx, kv, BucketLocalization, key)
	if err != nil {
		return false, err
	}
	return putUnlessEqual(ctx, kv, BucketLocalization, key, cur, bundle)
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
