package servicesettings

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/AliAladraj/Central-Config-Stream/internal/messaging"
	"github.com/AliAladraj/Central-Config-Stream/internal/obs"
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

func (s *Service) UpdateMicroserviceConfig(ctx context.Context, req MicroserviceAppSettings) (*MicroserviceAppSettings, error) {
	if req.ID <= 0 {
		return nil, ErrInvalidConfigID
	}

	if req.MicroserviceID <= 0 {
		return nil, ErrInvalidMicroserviceID
	}

	if req.EnvironmentID <= 0 {
		return nil, ErrInvalidEnvironmentID
	}

	if err := checkSettings(req.SettingsJSON); err != nil {
		return nil, err
	}

	// An update may move the row to another (microservice, environment), which
	// is a different KV key. The one it used to feed is read before the write,
	// because afterwards there is nothing left to derive it from — and leaving
	// it behind means consumers in the old environment keep serving settings no
	// row backs.
	previous, err := s.repo.GetMicroserviceConfigByID(ctx, req.ID)
	if err != nil {
		return nil, err
	}

	// 1. The database is the source of truth.
	updated, err := s.repo.UpdateMicroserviceConfig(ctx, req)
	if err != nil {
		return nil, err
	}

	// 2. Write-through to KV. Best-effort: reconciler heals drift on failure.
	if err := s.pub.PublishServiceSettings(ctx, updated.EnvironmentID, updated.MicroserviceID, updated.SettingsJSON); err != nil {
		logPublishFailure(ctx, updated.EnvironmentID, updated.MicroserviceID, err)
	}
	s.purgeMoved(ctx, previous, updated)

	return updated, nil
}

func (s *Service) GetMicroserviceConfigByID(ctx context.Context, id int64) (*MicroserviceAppSettings, error) {
	if id <= 0 {
		return nil, ErrInvalidConfigID
	}

	return s.repo.GetMicroserviceConfigByID(ctx, id)
}

func (s *Service) GetMicroserviceByID(ctx context.Context, id int64) (*Microservice, error) {
	if id <= 0 {
		return nil, ErrInvalidMicroserviceID
	}

	return s.repo.GetMicroserviceByID(ctx, id)
}

func (s *Service) ListMicroserviceConfigs(ctx context.Context, filter AppSettingsFilter) ([]MicroserviceAppSettings, error) {
	filter.Page = normalizePage(filter.Page)
	return s.repo.ListMicroserviceConfigs(ctx, filter)
}

func (s *Service) CreateMicroserviceConfig(ctx context.Context, req MicroserviceAppSettings) (*MicroserviceAppSettings, error) {
	if req.MicroserviceID <= 0 {
		return nil, ErrInvalidMicroserviceID
	}

	if req.EnvironmentID <= 0 {
		return nil, ErrInvalidEnvironmentID
	}

	if err := checkSettings(req.SettingsJSON); err != nil {
		return nil, err
	}

	// 1. The database is the source of truth.
	created, err := s.repo.CreateMicroserviceConfig(ctx, req)
	if err != nil {
		return nil, err
	}

	// 2. Write-through to KV. Best-effort: reconciler heals drift on failure.
	if err := s.pub.PublishServiceSettings(ctx, created.EnvironmentID, created.MicroserviceID, created.SettingsJSON); err != nil {
		logPublishFailure(ctx, created.EnvironmentID, created.MicroserviceID, err)
	}

	return created, nil
}

// DeleteMicroserviceConfig removes the row and the KV key it fed. Waiting for
// the next full reconcile to prune the key would leave consumers serving deleted
// settings for up to a whole interval.
func (s *Service) DeleteMicroserviceConfig(ctx context.Context, id int64) error {
	if id <= 0 {
		return ErrInvalidConfigID
	}

	removed, err := s.repo.DeleteMicroserviceConfig(ctx, id)
	if err != nil {
		return err
	}

	key := messaging.ServiceSettingsKey(removed.EnvironmentID, removed.MicroserviceID)
	if err := s.pub.DeleteKey(ctx, messaging.BucketServiceSettings, key); err != nil {
		obs.FromContext(ctx, "servicesettings").Error("purge settings failed (will reconcile)",
			slog.String("kv.key", key), obs.Err(err))
	}

	return nil
}

func (s *Service) ListMicroservices(ctx context.Context, page Page) ([]Microservice, error) {
	return s.repo.ListMicroservices(ctx, normalizePage(page))
}

func (s *Service) CreateMicroservice(ctx context.Context, req Microservice) (*Microservice, error) {
	name := strings.TrimSpace(req.Microservice)
	if !validName(name) {
		return nil, ErrInvalidMicroserviceName
	}

	// Reference rows have no KV representation of their own, so there is nothing
	// to publish: keys appear when an appsettings or localization row does.
	return s.repo.CreateMicroservice(ctx, name)
}

func (s *Service) DeleteMicroservice(ctx context.Context, id int64) error {
	if id <= 0 {
		return ErrInvalidMicroserviceID
	}

	return s.repo.DeleteMicroservice(ctx, id)
}

func (s *Service) ListEnvironments(ctx context.Context, page Page) ([]Environment, error) {
	return s.repo.ListEnvironments(ctx, normalizePage(page))
}

func (s *Service) CreateEnvironment(ctx context.Context, req Environment) (*Environment, error) {
	name := strings.TrimSpace(req.Environment)
	if !validName(name) {
		return nil, ErrInvalidEnvironmentName
	}

	return s.repo.CreateEnvironment(ctx, name)
}

func (s *Service) DeleteEnvironment(ctx context.Context, id int64) error {
	if id <= 0 {
		return ErrInvalidEnvironmentID
	}

	return s.repo.DeleteEnvironment(ctx, id)
}

// logPublishFailure records a dropped write-through. The request still
// succeeded — the database has the row — so this is the only trace of the drift the
// reconciler will have to repair.
func logPublishFailure(ctx context.Context, environmentID, microserviceID int64, err error) {
	obs.FromContext(ctx, "servicesettings").Error("publish settings failed (will reconcile)",
		slog.Int64("environment.id", environmentID),
		slog.Int64("microservice.id", microserviceID),
		obs.Err(err))
}

// purgeMoved drops the KV key an update left behind when it moved the row to a
// different (environment, microservice). Nothing else removes it until the next
// full reconcile sweep, and until then consumers in the old environment keep
// the settings they last saw.
func (s *Service) purgeMoved(ctx context.Context, previous, updated *MicroserviceAppSettings) {
	old := messaging.ServiceSettingsKey(previous.EnvironmentID, previous.MicroserviceID)
	if old == messaging.ServiceSettingsKey(updated.EnvironmentID, updated.MicroserviceID) {
		return
	}
	if err := s.pub.DeleteKey(ctx, messaging.BucketServiceSettings, old); err != nil {
		obs.FromContext(ctx, "servicesettings").Error("purge moved settings failed (will reconcile)",
			slog.String("kv.key", old), obs.Err(err))
	}
}

// checkSettings rejects anything a consumer cannot bind to its configuration
// object. A top-level array or scalar is well-formed JSON but breaks every
// consumer's deserialization at once, and KV would have published it happily.
//
// Size is checked here too, against the same ceiling the bucket is configured
// with: the tree is published verbatim as the KV value, so anything JetStream
// would refuse has to be refused before the row exists, not accepted with a 201
// and left failing at publish forever.
func checkSettings(settings json.RawMessage) error {
	if len(settings) > messaging.MaxValueSize {
		return ErrSettingsTooLarge
	}
	if len(settings) == 0 || !json.Valid(settings) {
		return ErrInvalidSettingsJSON
	}
	if trimmed := bytes.TrimLeft(settings, " \t\r\n"); len(trimmed) == 0 || trimmed[0] != '{' {
		return ErrInvalidSettingsJSON
	}
	return nil
}

// validName bounds a reference-table name. Empty names make an unusable admin
// UI and the column is a VARCHAR(100).
func validName(name string) bool {
	return name != "" && len(name) <= 100
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
