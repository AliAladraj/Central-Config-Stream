package messaging

import (
	"context"
	"encoding/json"
)

// NoopPublisher satisfies ConfigPublisher but does nothing. Used when
// PUBLISH_ENABLED=false — the API still serves reads and writes against the
// database, it just does not distribute anything — and in tests.
type NoopPublisher struct{}

func (NoopPublisher) PublishFlag(context.Context, int64, string, FlagPayload) error {
	return nil
}

func (NoopPublisher) PublishMicroConfig(context.Context, int64, int64, json.RawMessage) error {
	return nil
}

func (NoopPublisher) PublishLocalization(context.Context, int64, int64, string, json.RawMessage) error {
	return nil
}

func (NoopPublisher) ListKeys(context.Context, string) ([]string, error) {
	return nil, nil
}

func (NoopPublisher) ListRevisions(context.Context, string) ([]KeyRevision, error) {
	return nil, nil
}

func (NoopPublisher) DeleteKey(context.Context, string, string) error {
	return nil
}

func (NoopPublisher) EnsureBuckets(context.Context) error {
	return nil
}

var _ ConfigPublisher = NoopPublisher{}
