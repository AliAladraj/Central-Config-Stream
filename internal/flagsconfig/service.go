package flagsconfig

import (
	"context"
	"log/slog"

	"github.com/ErasedKyte/Central-Config-Stream/internal/messaging"
	"github.com/ErasedKyte/Central-Config-Stream/internal/obs"
)

type Service struct {
	repo Repository
	pub  messaging.ConfigPublisher
}

func NewService(repo Repository, pub messaging.ConfigPublisher) *Service {
	if pub == nil {
		pub = messaging.NoopPublisher{}
	}
	return &Service{repo: repo, pub: pub}
}

func (s *Service) GetFlagByID(ctx context.Context, id int64) (*Flag, error) {
	if id <= 0 {
		return nil, ErrInvalidFlagID
	}

	return s.repo.GetFlagsByID(ctx, id)
}

func (s *Service) GetFlagValueByID(ctx context.Context, id int64) (*FlagValue, error) {
	if id <= 0 {
		return nil, ErrInvalidFlagID
	}

	return s.repo.GetFlagValueByID(ctx, id)
}

func (s *Service) UpdateFlagValue(ctx context.Context, req FlagValue) (*FlagValue, error) {
	if req.ID <= 0 {
		return nil, ErrInvalidFlagID
	}

	// 1. Oracle is the source of truth. The repository hands back the flag key
	//    alongside the row, so publishing needs no extra query.
	updated, flagKey, err := s.repo.UpdateFlagValue(ctx, req)
	if err != nil {
		return nil, err
	}

	// 2. Write-through to KV (the live surface consumers watch).
	//    A publish failure must NOT fail the request — the reconciler heals
	//    drift. We log and continue.
	s.publish(ctx, updated, flagKey)

	return updated, nil
}

func (s *Service) ListFlags(ctx context.Context, filter FlagFilter) ([]Flag, error) {
	filter.Page = normalizePage(filter.Page)
	return s.repo.ListFlags(ctx, filter)
}

func (s *Service) ListFlagValues(ctx context.Context, filter FlagValueFilter) ([]FlagValueRow, error) {
	filter.Page = normalizePage(filter.Page)
	return s.repo.ListFlagValues(ctx, filter)
}

func (s *Service) CreateFlag(ctx context.Context, req Flag) (*Flag, error) {
	if !validFlagKey(req.FlagKey) {
		return nil, ErrInvalidFlagKey
	}

	// A flag on its own has no per-environment value yet, so there is nothing to
	// publish: KV entries appear when the first value row is created.
	return s.repo.CreateFlag(ctx, req)
}

// DeleteFlag removes the flag, its values, and the KV keys those values fed.
// Waiting for the next full reconcile to prune them would leave consumers
// serving a deleted flag for up to a whole interval.
func (s *Service) DeleteFlag(ctx context.Context, id int64) error {
	if id <= 0 {
		return ErrInvalidFlagID
	}

	removed, err := s.repo.DeleteFlag(ctx, id)
	if err != nil {
		return err
	}

	for _, row := range removed {
		s.purge(ctx, row)
	}
	return nil
}

func (s *Service) CreateFlagValue(ctx context.Context, req FlagValue) (*FlagValue, error) {
	if req.FlagId <= 0 {
		return nil, ErrInvalidFlagID
	}
	if req.EnvironmentID <= 0 {
		return nil, ErrInvalidEnvironmentID
	}

	// 1. Oracle is the source of truth.
	created, flagKey, err := s.repo.CreateFlagValue(ctx, req)
	if err != nil {
		return nil, err
	}

	// 2. Write-through to KV, on the same best-effort terms as an update.
	s.publish(ctx, created, flagKey)

	return created, nil
}

func (s *Service) DeleteFlagValue(ctx context.Context, id int64) error {
	if id <= 0 {
		return ErrInvalidFlagID
	}

	removed, err := s.repo.DeleteFlagValue(ctx, id)
	if err != nil {
		return err
	}

	s.purge(ctx, *removed)
	return nil
}

// publish pushes the latest value to KV. Best-effort: errors are logged, not
// returned.
func (s *Service) publish(ctx context.Context, v *FlagValue, flagKey string) {
	err := s.pub.PublishFlag(ctx, v.EnvironmentID, flagKey, messaging.FlagPayload{
		Enabled: v.Enabled != 0,
		Value:   v.Value,
	})
	if err != nil {
		obs.FromContext(ctx, "flagsconfig").Error("publish flag failed (will reconcile)",
			slog.String("flag.key", flagKey),
			slog.Int64("environment.id", v.EnvironmentID),
			obs.Err(err))
	}
}

// purge drops the KV key of a deleted row. Best-effort like publish: the
// reconciler's full sweep removes anything left behind.
func (s *Service) purge(ctx context.Context, row DeletedFlagValue) {
	key := messaging.FlagKey(row.EnvironmentID, row.FlagKey)
	if err := s.pub.DeleteKey(ctx, messaging.BucketFlags, key); err != nil {
		obs.FromContext(ctx, "flagsconfig").Error("purge flag failed (will reconcile)",
			slog.String("kv.key", key), obs.Err(err))
	}
}

// normalizePage keeps a repository from ever seeing an unbounded or negative
// page, whatever the caller sent.
func normalizePage(p Page) Page {
	if p.Limit <= 0 || p.Limit > maxListLimit {
		p.Limit = maxListLimit
	}
	if p.Offset < 0 {
		p.Offset = 0
	}
	return p
}

// maxListLimit caps a single page of list results.
const maxListLimit = 500

// validFlagKey accepts only what a KV key may contain, minus the dot: the FLAGS
// key is "{environmentID}.{flagKey}", so a dot inside the key would make it
// ambiguous for a consumer splitting on the separator.
func validFlagKey(key string) bool {
	if key == "" || len(key) > 128 {
		return false
	}
	for _, c := range key {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}
