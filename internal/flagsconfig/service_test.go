package flagsconfig

import (
	"context"
	"testing"

	"github.com/ErasedKyte/Central-Config-Stream/internal/messaging"
)

type fakeRepo struct {
	flag       *Flag
	updated    *FlagValue
	deleted    []DeletedFlagValue
	updateErr  error
	getFlagErr error
}

func (f *fakeRepo) GetFlagsByID(ctx context.Context, id int64) (*Flag, error) {
	if f.getFlagErr != nil {
		return nil, f.getFlagErr
	}
	return f.flag, nil
}
func (f *fakeRepo) GetFlagValueByID(ctx context.Context, id int64) (*FlagValue, error) {
	return f.updated, nil
}
func (f *fakeRepo) UpdateFlagValue(ctx context.Context, in FlagValue) (*FlagValue, string, error) {
	if f.updateErr != nil {
		return nil, "", f.updateErr
	}
	return f.updated, f.flag.FlagKey, nil
}
func (f *fakeRepo) ListFlags(ctx context.Context, filter FlagFilter) ([]Flag, error) {
	return []Flag{*f.flag}, nil
}
func (f *fakeRepo) CreateFlag(ctx context.Context, in Flag) (*Flag, error) {
	return f.flag, nil
}
func (f *fakeRepo) DeleteFlag(ctx context.Context, id int64) ([]DeletedFlagValue, error) {
	return f.deleted, nil
}
func (f *fakeRepo) ListFlagValues(ctx context.Context, filter FlagValueFilter) ([]FlagValueRow, error) {
	return nil, nil
}
func (f *fakeRepo) CreateFlagValue(ctx context.Context, in FlagValue) (*FlagValue, string, error) {
	return f.updated, f.flag.FlagKey, nil
}
func (f *fakeRepo) DeleteFlagValue(ctx context.Context, id int64) (*DeletedFlagValue, error) {
	return &f.deleted[0], nil
}

type capturePub struct {
	messaging.NoopPublisher
	env     int64
	flagKey string
	payload messaging.FlagPayload
	calls   int
	deleted []string
}

func (c *capturePub) PublishFlag(ctx context.Context, env int64, flagKey string, p messaging.FlagPayload) error {
	c.calls++
	c.env, c.flagKey, c.payload = env, flagKey, p
	return nil
}

func (c *capturePub) DeleteKey(ctx context.Context, bucket, key string) error {
	c.deleted = append(c.deleted, bucket+"|"+key)
	return nil
}

func TestUpdateFlagValuePublishesWithResolvedKey(t *testing.T) {
	repo := &fakeRepo{
		flag:    &Flag{ID: 7, FlagKey: "search_v2"},
		updated: &FlagValue{ID: 100, EnvironmentID: 3, FlagId: 7, Enabled: 1, Value: "on"},
	}
	pub := &capturePub{}
	svc := NewService(repo, pub)

	_, err := svc.UpdateFlagValue(context.Background(), FlagValue{ID: 100})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if pub.calls != 1 {
		t.Fatalf("expected 1 publish, got %d", pub.calls)
	}
	if pub.env != 3 || pub.flagKey != "search_v2" {
		t.Errorf("published wrong key: env=%d key=%q", pub.env, pub.flagKey)
	}
	if !pub.payload.Enabled || pub.payload.Value != "on" {
		t.Errorf("wrong payload: %+v", pub.payload)
	}
}

func TestUpdateFlagValueRejectsBadID(t *testing.T) {
	svc := NewService(&fakeRepo{}, nil)
	if _, err := svc.UpdateFlagValue(context.Background(), FlagValue{ID: 0}); err != ErrInvalidFlagID {
		t.Fatalf("expected ErrInvalidFlagID, got %v", err)
	}
}

func TestCreateFlagValuePublishes(t *testing.T) {
	repo := &fakeRepo{
		flag:    &Flag{ID: 7, FlagKey: "search_v2"},
		updated: &FlagValue{ID: 100, EnvironmentID: 3, FlagId: 7, Enabled: 1, Value: "on"},
	}
	pub := &capturePub{}
	svc := NewService(repo, pub)

	if _, err := svc.CreateFlagValue(context.Background(), FlagValue{EnvironmentID: 3, FlagId: 7}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if pub.calls != 1 || pub.flagKey != "search_v2" {
		t.Fatalf("create did not write through to KV: calls=%d key=%q", pub.calls, pub.flagKey)
	}
}

// Deleting a flag has to take every environment's KV key with it, not just the
// one the caller happened to be looking at.
func TestDeleteFlagPurgesEveryEnvironmentKey(t *testing.T) {
	repo := &fakeRepo{
		flag: &Flag{ID: 7, FlagKey: "search_v2"},
		deleted: []DeletedFlagValue{
			{EnvironmentID: 1, FlagKey: "search_v2"},
			{EnvironmentID: 3, FlagKey: "search_v2"},
		},
	}
	pub := &capturePub{}
	svc := NewService(repo, pub)

	if err := svc.DeleteFlag(context.Background(), 7); err != nil {
		t.Fatalf("delete flag: %v", err)
	}

	want := []string{"FLAGS|1.search_v2", "FLAGS|3.search_v2"}
	if len(pub.deleted) != len(want) {
		t.Fatalf("expected %d purges, got %v", len(want), pub.deleted)
	}
	for i, key := range want {
		if pub.deleted[i] != key {
			t.Errorf("purge %d: want %q, got %q", i, key, pub.deleted[i])
		}
	}
}

func TestDeleteFlagValuePurgesItsKey(t *testing.T) {
	repo := &fakeRepo{
		flag:    &Flag{ID: 7, FlagKey: "search_v2"},
		deleted: []DeletedFlagValue{{EnvironmentID: 3, FlagKey: "search_v2"}},
	}
	pub := &capturePub{}
	svc := NewService(repo, pub)

	if err := svc.DeleteFlagValue(context.Background(), 100); err != nil {
		t.Fatalf("delete flag value: %v", err)
	}
	if len(pub.deleted) != 1 || pub.deleted[0] != "FLAGS|3.search_v2" {
		t.Fatalf("unexpected purges: %v", pub.deleted)
	}
}

// A flag key ends up inside a KV key, so anything JetStream would refuse — or
// that would make "{env}.{flagKey}" ambiguous — is rejected at write time.
func TestCreateFlagRejectsUnusableKey(t *testing.T) {
	svc := NewService(&fakeRepo{flag: &Flag{ID: 7, FlagKey: "ok"}}, nil)

	for _, key := range []string{"", "has space", "has.dot", "has/slash"} {
		if _, err := svc.CreateFlag(context.Background(), Flag{FlagKey: key}); err != ErrInvalidFlagKey {
			t.Errorf("flag key %q: expected ErrInvalidFlagKey, got %v", key, err)
		}
	}
}
