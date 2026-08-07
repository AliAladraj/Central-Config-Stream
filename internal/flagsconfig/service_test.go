package flagsconfig

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

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
	// published records every key in order, not just the last, so a test can
	// assert that a rejected write reached KV zero times.
	published []string
	deleted   []string
}

func (c *capturePub) PublishFlag(ctx context.Context, env int64, flagKey string, p messaging.FlagPayload) error {
	c.calls++
	c.env, c.flagKey, c.payload = env, flagKey, p
	c.published = append(c.published, messaging.FlagKey(env, flagKey))
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

	// The request carries a usable value and state because this test is about
	// what gets published, not about validation: an invalid one never reaches
	// the repository at all.
	_, err := svc.UpdateFlagValue(context.Background(), FlagValue{ID: 100, Value: "on", Enabled: 1})
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

	if _, err := svc.CreateFlagValue(context.Background(), FlagValue{
		EnvironmentID: 3, FlagId: 7, Value: "on", Enabled: 1,
	}); err != nil {
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

// VALUE is VARCHAR(4000) NOT NULL, and migrations/005 says in as many words
// that keeping an empty string out is this validation's job: PostgreSQL keeps
// one distinct from NULL, so NOT NULL closes only half the hole. Both halves
// matter because the
// value is republished to every consumer in the environment and read as a
// rollout percentage, a variant name or a threshold — "" arrives at the far end
// indistinguishable from a parse bug, and an oversized one is a driver overflow
// the handler can only call a 500.
func TestWritesRejectAnUnusableValue(t *testing.T) {
	repo := &fakeRepo{
		flag:    &Flag{ID: 7, FlagKey: "search_v2"},
		updated: &FlagValue{ID: 100, EnvironmentID: 3, FlagId: 7, Enabled: 1, Value: "on"},
	}
	pub := &capturePub{}
	svc := NewService(repo, pub)
	ctx := context.Background()

	cases := []struct {
		value string
		want  error
	}{
		{"", ErrInvalidFlagValue},
		{strings.Repeat("x", maxFlagValueLen+1), ErrFlagValueTooLarge},
		// The column counts characters, not bytes, so the ceiling has to as
		// well: these are 8002 bytes and one character over the limit.
		{strings.Repeat("é", maxFlagValueLen+1), ErrFlagValueTooLarge},
	}
	for _, c := range cases {
		if _, err := svc.CreateFlagValue(ctx, FlagValue{
			EnvironmentID: 3, FlagId: 7, Value: c.value, Enabled: 1,
		}); !errors.Is(err, c.want) {
			t.Errorf("create with a %d-char value: expected %v, got %v", utf8.RuneCountInString(c.value), c.want, err)
		}
		if _, err := svc.UpdateFlagValue(ctx, FlagValue{
			ID: 100, Value: c.value, Enabled: 1,
		}); !errors.Is(err, c.want) {
			t.Errorf("update with a %d-char value: expected %v, got %v", utf8.RuneCountInString(c.value), c.want, err)
		}
	}

	if pub.calls != 0 {
		t.Fatalf("a rejected value still reached KV: %v", pub.published)
	}

	// A value the column would have taken is still written — including one whose
	// bytes outnumber its characters, which a byte-counted bound would refuse.
	for _, value := range []string{strings.Repeat("x", maxFlagValueLen), strings.Repeat("é", maxFlagValueLen)} {
		if _, err := svc.UpdateFlagValue(ctx, FlagValue{ID: 100, Value: value, Enabled: 1}); err != nil {
			t.Errorf("update at exactly the limit (%d bytes): %v", len(value), err)
		}
	}
}

// ENABLED and IS_ACTIVE are SMALLINT columns carrying a boolean the API spells
// as 1/0. Anything else is meaningless rather than merely out of range: the KV
// payload collapses every non-zero to true, so a stored 7 is a row the API
// reads back as 7 while consumers see the same true a 1 would have given them.
// Left to the column, 70000 overflows the SMALLINT into a 500.
func TestWritesRejectAnOutOfRangeState(t *testing.T) {
	repo := &fakeRepo{
		flag:    &Flag{ID: 7, FlagKey: "search_v2"},
		updated: &FlagValue{ID: 100, EnvironmentID: 3, FlagId: 7, Enabled: 1, Value: "on"},
	}
	pub := &capturePub{}
	svc := NewService(repo, pub)
	ctx := context.Background()

	for _, state := range []int64{-1, 2, 70000, 99999} {
		if _, err := svc.CreateFlagValue(ctx, FlagValue{
			EnvironmentID: 3, FlagId: 7, Value: "on", Enabled: state,
		}); !errors.Is(err, ErrInvalidEnabled) {
			t.Errorf("create with enabled=%d: expected ErrInvalidEnabled, got %v", state, err)
		}
		if _, err := svc.UpdateFlagValue(ctx, FlagValue{
			ID: 100, Value: "on", Enabled: state,
		}); !errors.Is(err, ErrInvalidEnabled) {
			t.Errorf("update with enabled=%d: expected ErrInvalidEnabled, got %v", state, err)
		}
		if _, err := svc.CreateFlag(ctx, Flag{FlagKey: "probe", IsActive: state}); !errors.Is(err, ErrInvalidIsActive) {
			t.Errorf("create flag with isActive=%d: expected ErrInvalidIsActive, got %v", state, err)
		}
	}

	if pub.calls != 0 {
		t.Fatalf("a rejected state still reached KV: %v", pub.published)
	}

	// Both legitimate values still write: the bound is the meaning, not the
	// caller's ability to turn a flag off.
	for _, state := range []int64{0, 1} {
		if _, err := svc.UpdateFlagValue(ctx, FlagValue{ID: 100, Value: "on", Enabled: state}); err != nil {
			t.Errorf("update with enabled=%d: %v", state, err)
		}
		if _, err := svc.CreateFlag(ctx, Flag{FlagKey: "probe", IsActive: state}); err != nil {
			t.Errorf("create flag with isActive=%d: %v", state, err)
		}
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
