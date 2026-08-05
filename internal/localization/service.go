package localization

import (
	"bytes"
	"context"
	"encoding/json"
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

func (s *Service) GetLocalizationByID(ctx context.Context, id int64) (*Localization, error) {
	if id <= 0 {
		return nil, ErrInvalidLocalizationID
	}
	return s.repo.GetLocalizationByID(ctx, id)
}

func (s *Service) GetLocalization(ctx context.Context, microserviceID, environmentID int64, locale string) (*Localization, error) {
	if microserviceID <= 0 {
		return nil, ErrInvalidMicroserviceID
	}
	if environmentID <= 0 {
		return nil, ErrInvalidEnvironmentID
	}
	if locale == "" {
		return nil, ErrInvalidLocale
	}
	return s.repo.GetLocalization(ctx, microserviceID, environmentID, locale)
}

func (s *Service) UpdateLocalization(ctx context.Context, req Localization) (*Localization, error) {
	if req.ID <= 0 {
		return nil, ErrInvalidLocalizationID
	}
	if req.MicroserviceID <= 0 {
		return nil, ErrInvalidMicroserviceID
	}
	if req.EnvironmentID <= 0 {
		return nil, ErrInvalidEnvironmentID
	}
	// The same check a create runs. A locale that only an update let through
	// still becomes a KV key, and one a consumer cannot parse is republished by
	// every reconcile sweep from then on.
	if !validLocale(req.Locale) {
		return nil, ErrInvalidLocale
	}
	if err := checkBundle(req.BundleJSON); err != nil {
		return nil, err
	}

	// An update may move the row to another (microservice, environment,
	// locale), which is a different KV key. The one it used to feed is read
	// before the write, because afterwards there is nothing left to derive it
	// from — and leaving it behind means consumers of the old identity keep
	// serving a bundle no row backs.
	previous, err := s.repo.GetLocalizationByID(ctx, req.ID)
	if err != nil {
		return nil, err
	}

	// 1. Oracle is the source of truth.
	updated, err := s.repo.UpdateLocalization(ctx, req)
	if err != nil {
		return nil, err
	}

	// 2. Write-through to KV. Best-effort: reconciler heals drift on failure.
	s.publish(ctx, updated)
	s.purgeMoved(ctx, previous, updated)

	return updated, nil
}

func (s *Service) ListLocalizations(ctx context.Context, filter Filter) ([]Localization, error) {
	filter.Page = normalizePage(filter.Page)
	return s.repo.ListLocalizations(ctx, filter)
}

func (s *Service) CreateLocalization(ctx context.Context, req Localization) (*Localization, error) {
	if req.MicroserviceID <= 0 {
		return nil, ErrInvalidMicroserviceID
	}
	if req.EnvironmentID <= 0 {
		return nil, ErrInvalidEnvironmentID
	}
	if !validLocale(req.Locale) {
		return nil, ErrInvalidLocale
	}
	if err := checkBundle(req.BundleJSON); err != nil {
		return nil, err
	}

	// 1. Oracle is the source of truth.
	created, err := s.repo.CreateLocalization(ctx, req)
	if err != nil {
		return nil, err
	}

	// 2. Write-through to KV. Best-effort: reconciler heals drift on failure.
	s.publish(ctx, created)

	return created, nil
}

// DeleteLocalization removes the row and the KV key it fed. Waiting for the next
// full reconcile to prune the key would leave consumers serving a deleted locale
// for up to a whole interval.
func (s *Service) DeleteLocalization(ctx context.Context, id int64) error {
	if id <= 0 {
		return ErrInvalidLocalizationID
	}

	removed, err := s.repo.DeleteLocalization(ctx, id)
	if err != nil {
		return err
	}

	key := messaging.LocalizationKey(removed.EnvironmentID, removed.MicroserviceID, removed.Locale)
	if err := s.pub.DeleteKey(ctx, messaging.BucketLocalization, key); err != nil {
		obs.FromContext(ctx, "localization").Error("purge bundle failed (will reconcile)",
			slog.String("kv.key", key), obs.Err(err))
	}

	return nil
}

// publish pushes the latest bundle to KV. Best-effort: errors are logged, not
// returned.
func (s *Service) publish(ctx context.Context, l *Localization) {
	if err := s.pub.PublishLocalization(ctx, l.EnvironmentID, l.MicroserviceID, l.Locale, l.BundleJSON); err != nil {
		obs.FromContext(ctx, "localization").Error("publish bundle failed (will reconcile)",
			slog.Int64("environment.id", l.EnvironmentID),
			slog.Int64("microservice.id", l.MicroserviceID),
			slog.String("locale", l.Locale),
			obs.Err(err))
	}
}

// purgeMoved drops the KV key an update left behind when it moved the row to a
// different (environment, microservice, locale). Nothing else removes it until
// the next full reconcile sweep, and until then consumers watching the old key
// keep the bundle they last saw.
func (s *Service) purgeMoved(ctx context.Context, previous, updated *Localization) {
	old := messaging.LocalizationKey(previous.EnvironmentID, previous.MicroserviceID, previous.Locale)
	if old == messaging.LocalizationKey(updated.EnvironmentID, updated.MicroserviceID, updated.Locale) {
		return
	}
	if err := s.pub.DeleteKey(ctx, messaging.BucketLocalization, old); err != nil {
		obs.FromContext(ctx, "localization").Error("purge moved bundle failed (will reconcile)",
			slog.String("kv.key", old), obs.Err(err))
	}
}

// checkBundle rejects anything a consumer cannot bind to its translation map. A
// top-level array or scalar is well-formed JSON but breaks every consumer's
// deserialization at once, and KV would have published it happily.
//
// Size is checked here too, against the same ceiling the bucket is configured
// with: the bundle is published verbatim as the KV value, so anything JetStream
// would refuse has to be refused before the row exists, not accepted with a 201
// and left failing at publish forever.
func checkBundle(bundle json.RawMessage) error {
	if len(bundle) > messaging.MaxValueSize {
		return ErrBundleTooLarge
	}
	if len(bundle) == 0 || !json.Valid(bundle) {
		return ErrInvalidBundleJSON
	}
	if trimmed := bytes.TrimLeft(bundle, " \t\r\n"); len(trimmed) == 0 || trimmed[0] != '{' {
		return ErrInvalidBundleJSON
	}
	return nil
}

// validLocale accepts only what a KV key may contain, minus the dot: the
// LOCALIZATION key is "{env}.{microserviceID}.{locale}", so a dot inside the
// locale would make it ambiguous for a consumer splitting on the separator.
func validLocale(locale string) bool {
	if locale == "" || len(locale) > 35 {
		return false
	}
	for _, c := range locale {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_':
		default:
			return false
		}
	}
	return true
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
