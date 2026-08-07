package messaging

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/nats-io/nats.go/jetstream"
)

// MaxValueSize bounds a single KV value. Left unset a bucket inherits the
// server's max_payload (1 MB by default), so a payload the admin API accepted
// with a 201 could be refused by JetStream forever afterwards — drift the
// reconciler can never heal because every sweep fails on the same row. The
// domain services refuse anything larger before the database write, which is
// why this number and the one they check against have to be the same one.
const MaxValueSize = 512 << 10 // 512 KiB

// ErrNotProvisioned is returned by a publish attempted before the buckets
// exist: NATS was unreachable at startup and Ensure has not yet succeeded.
// Distinct so a caller can tell it apart from a rejected value.
var ErrNotProvisioned = errors.New("messaging: KV buckets are not provisioned")

// Buckets holds the three KV stores this service publishes to. Handles are
// swapped in place by Ensure, so every read goes through the mutex.
type Buckets struct {
	mu              sync.RWMutex
	flags           jetstream.KeyValue
	serviceSettings jetstream.KeyValue
	localization    jetstream.KeyValue

	// Kept so Ensure can re-provision the buckets later. Without them a NATS
	// restart with an empty store would leave every publish failing against a
	// bucket that no longer exists until the process is restarted.
	js   jetstream.JetStream
	opts BucketOptions
}

// BucketOptions controls how buckets are provisioned. Defaults are safe for a
// single-node dev NATS; set Replicas=3 and Storage=file for a prod cluster.
type BucketOptions struct {
	History  uint8 // number of historical values kept per key (rollback depth)
	Replicas int   // 1 for dev, 3 for prod cluster
}

func (o BucketOptions) withDefaults() BucketOptions {
	if o.History == 0 {
		o.History = 5
	}
	if o.Replicas == 0 {
		o.Replicas = 1
	}
	return o
}

// EnsureBuckets creates (or updates) the FLAGS, SERVICESETTINGS and LOCALIZATION
// KV buckets idempotently. Safe to call on every startup.
func EnsureBuckets(ctx context.Context, js jetstream.JetStream, opts BucketOptions) (*Buckets, error) {
	opts = opts.withDefaults()

	flags, err := ensureBucket(ctx, js, BucketFlags, opts)
	if err != nil {
		return nil, err
	}
	micro, err := ensureBucket(ctx, js, BucketServiceSettings, opts)
	if err != nil {
		return nil, err
	}
	loc, err := ensureBucket(ctx, js, BucketLocalization, opts)
	if err != nil {
		return nil, err
	}

	return &Buckets{
		flags:           flags,
		serviceSettings: micro,
		localization:    loc,
		js:              js,
		opts:            opts,
	}, nil
}

// NewBuckets returns a Buckets whose handles are not provisioned yet. It is
// what makes a NATS outage at startup non-fatal: every publish fails cleanly
// against it while the database-backed read paths carry on serving, and the
// reconciler's per-cycle Ensure fills the handles in as soon as NATS is back.
func NewBuckets(js jetstream.JetStream, opts BucketOptions) *Buckets {
	return &Buckets{js: js, opts: opts.withDefaults()}
}

// Ensure re-provisions the buckets and swaps in the fresh handles. It exists so
// the service heals itself when NATS comes back with an empty store: startup
// provisioning alone would leave every publish failing forever. Idempotent and
// cheap — CreateOrUpdateKeyValue is a no-op when the bucket already matches —
// so it is safe to call at the start of every reconcile cycle.
func (b *Buckets) Ensure(ctx context.Context) error {
	fresh, err := EnsureBuckets(ctx, b.js, b.opts)
	if err != nil {
		return err
	}

	b.mu.Lock()
	b.flags, b.serviceSettings, b.localization = fresh.flags, fresh.serviceSettings, fresh.localization
	b.mu.Unlock()
	return nil
}

// byName resolves a bucket name to its live handle.
func (b *Buckets) byName(name string) (jetstream.KeyValue, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	var kv jetstream.KeyValue
	switch name {
	case BucketFlags:
		kv = b.flags
	case BucketServiceSettings:
		kv = b.serviceSettings
	case BucketLocalization:
		kv = b.localization
	default:
		return nil, fmt.Errorf("messaging: unknown bucket %q", name)
	}
	if kv == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotProvisioned, name)
	}
	return kv, nil
}

func ensureBucket(ctx context.Context, js jetstream.JetStream, name string, opts BucketOptions) (jetstream.KeyValue, error) {
	kv, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:       name,
		History:      opts.History,
		Storage:      jetstream.FileStorage,
		Replicas:     opts.Replicas,
		MaxValueSize: MaxValueSize,
	})
	if err != nil {
		return nil, fmt.Errorf("messaging: ensure bucket %s: %w", name, err)
	}
	return kv, nil
}
