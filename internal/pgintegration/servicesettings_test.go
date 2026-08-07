package pgintegration

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/AliAladraj/Central-Config-Stream/internal/servicesettings"
)

func microRepo(t *testing.T, opts ...stackOption) (*servicesettings.PostgresRepository, *stack) {
	t.Helper()
	s := newStack(t, opts...)
	return servicesettings.NewPostgresRepository(s.DB), s
}

func TestAppSettingsCreateReadAndList(t *testing.T) {
	repo, _ := microRepo(t)
	ctx := context.Background()

	doc := json.RawMessage(`{"service":{"displayName":"Catalog (staging)"},"http":{"timeoutMs":2500}}`)
	created, err := repo.CreateMicroserviceConfig(ctx, servicesettings.MicroserviceAppSettings{
		MicroserviceID: 1, EnvironmentID: 2, SettingsJSON: doc,
	})
	if err != nil {
		t.Fatalf("create config: %v", err)
	}
	if created.ID == 0 || string(created.SettingsJSON) != string(doc) {
		t.Fatalf("unexpected created row: %+v", created)
	}

	read, err := repo.GetMicroserviceConfigByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(read.SettingsJSON) != string(doc) {
		t.Fatalf("read back %q, wrote %q", read.SettingsJSON, doc)
	}
	if _, err := repo.GetMicroserviceConfigByID(ctx, 999999); !errors.Is(err, servicesettings.ErrConfigNotFound) {
		t.Fatalf("read of a missing row: expected ErrConfigNotFound, got %v", err)
	}

	if _, err := repo.CreateMicroserviceConfig(ctx, servicesettings.MicroserviceAppSettings{
		MicroserviceID: 1, EnvironmentID: 2, SettingsJSON: doc,
	}); !errors.Is(err, servicesettings.ErrConfigExists) {
		t.Fatalf("duplicate natural key: expected ErrConfigExists, got %v", err)
	}
	if _, err := repo.CreateMicroserviceConfig(ctx, servicesettings.MicroserviceAppSettings{
		MicroserviceID: 999, EnvironmentID: 2, SettingsJSON: doc,
	}); !errors.Is(err, servicesettings.ErrMicroserviceNotFound) {
		t.Fatalf("missing microservice: expected ErrMicroserviceNotFound, got %v", err)
	}
	if _, err := repo.CreateMicroserviceConfig(ctx, servicesettings.MicroserviceAppSettings{
		MicroserviceID: 1, EnvironmentID: 999, SettingsJSON: doc,
	}); !errors.Is(err, servicesettings.ErrEnvironmentNotFound) {
		t.Fatalf("missing environment: expected ErrEnvironmentNotFound, got %v", err)
	}

	all, err := repo.ListMicroserviceConfigs(ctx, servicesettings.AppSettingsFilter{Page: servicesettings.Page{Limit: 100}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 4 { // three seeded plus the one created above
		t.Fatalf("expected 4 rows, got %d", len(all))
	}

	byService, err := repo.ListMicroserviceConfigs(ctx, servicesettings.AppSettingsFilter{
		Page: servicesettings.Page{Limit: 100}, MicroserviceID: 1,
	})
	if err != nil {
		t.Fatalf("list by microservice: %v", err)
	}
	if len(byService) != 2 {
		t.Fatalf("expected 2 rows for microservice 1, got %d", len(byService))
	}

	byEnv, err := repo.ListMicroserviceConfigs(ctx, servicesettings.AppSettingsFilter{
		Page: servicesettings.Page{Limit: 100}, EnvironmentID: 1,
	})
	if err != nil {
		t.Fatalf("list by environment: %v", err)
	}
	if len(byEnv) != 3 {
		t.Fatalf("expected 3 rows in environment 1, got %d", len(byEnv))
	}

	// Both filters plus paging: MICROSERVICE_ID binds $1, ENVIRONMENT_ID $2, and
	// the LIMIT/OFFSET $3 and $4.
	both, err := repo.ListMicroserviceConfigs(ctx, servicesettings.AppSettingsFilter{
		Page: servicesettings.Page{Limit: 100}, MicroserviceID: 1, EnvironmentID: 2,
	})
	if err != nil {
		t.Fatalf("list by microservice and environment: %v", err)
	}
	if len(both) != 1 || both[0].ID != created.ID {
		t.Fatalf("unexpected rows for (service 1, env 2): %+v", both)
	}

	page, err := repo.ListMicroserviceConfigs(ctx, servicesettings.AppSettingsFilter{Page: servicesettings.Page{Limit: 2, Offset: 1}})
	if err != nil {
		t.Fatalf("list page: %v", err)
	}
	if len(page) != 2 || page[0].ID != all[1].ID || page[1].ID != all[2].ID {
		t.Fatalf("offset page did not line up: %+v", page)
	}

	removed, err := repo.DeleteMicroserviceConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if removed.MicroserviceID != 1 || removed.EnvironmentID != 2 {
		t.Fatalf("delete reported the wrong KV coordinates: %+v", removed)
	}
	if _, err := repo.DeleteMicroserviceConfig(ctx, created.ID); !errors.Is(err, servicesettings.ErrConfigNotFound) {
		t.Fatalf("second delete: expected ErrConfigNotFound, got %v", err)
	}
}

func TestAppSettingsGuardedUpdate(t *testing.T) {
	repo, _ := microRepo(t)
	ctx := context.Background()

	current, err := repo.GetMicroserviceConfigByID(ctx, 200)
	if err != nil {
		t.Fatalf("read current: %v", err)
	}

	expected := current.UpdatedAt
	updated, err := repo.UpdateMicroserviceConfig(ctx, servicesettings.MicroserviceAppSettings{
		ID: 200, MicroserviceID: 1, EnvironmentID: 1,
		SettingsJSON:      json.RawMessage(`{"log":{"level":"warn"}}`),
		ExpectedUpdatedAt: &expected,
	})
	if err != nil {
		t.Fatalf("guarded update with the current timestamp: %v", err)
	}
	if string(updated.SettingsJSON) != `{"log":{"level":"warn"}}` {
		t.Fatalf("unexpected row after update: %+v", updated)
	}

	stale := expected
	if _, err := repo.UpdateMicroserviceConfig(ctx, servicesettings.MicroserviceAppSettings{
		ID: 200, MicroserviceID: 1, EnvironmentID: 1,
		SettingsJSON:      json.RawMessage(`{"log":{"level":"error"}}`),
		ExpectedUpdatedAt: &stale,
	}); !errors.Is(err, servicesettings.ErrConflict) {
		t.Fatalf("stale guard: expected ErrConflict, got %v", err)
	}

	// A guard on a row that is not there stays a not-found. The distinction is
	// the whole reason the repository re-reads the row after a zero-row UPDATE.
	if _, err := repo.UpdateMicroserviceConfig(ctx, servicesettings.MicroserviceAppSettings{
		ID: 999999, MicroserviceID: 2, EnvironmentID: 2,
		SettingsJSON:      json.RawMessage(`{}`),
		ExpectedUpdatedAt: &stale,
	}); !errors.Is(err, servicesettings.ErrConfigNotFound) {
		t.Fatalf("guarded update of a missing row: expected ErrConfigNotFound, got %v", err)
	}

	// An update rewrites the natural key, so it carries the create's checks:
	// moving row 200 onto row 201's (microservice, environment) is a conflict
	// and not a constraint violation surfacing as a 500.
	if _, err := repo.UpdateMicroserviceConfig(ctx, servicesettings.MicroserviceAppSettings{
		ID: 200, MicroserviceID: 2, EnvironmentID: 1,
		SettingsJSON: json.RawMessage(`{}`),
	}); !errors.Is(err, servicesettings.ErrConfigExists) {
		t.Fatalf("update onto another row's key: expected ErrConfigExists, got %v", err)
	}
}

func TestAppSettingsCreateLosingARaceIsAConflict(t *testing.T) {
	repo, s := microRepo(t)
	ctx := context.Background()

	// Microservice 1 has no appsettings in environment 3, so the pre-check passes.
	s.installRacingWriter(t, "winner_appsettings", "CONFIG_MICROSERVICE_APPSETTINGS",
		"NEW.MICROSERVICE_ID = 1 AND NEW.ENVIRONMENT_ID = 3",
		`INSERT INTO CONFIG_MICROSERVICE_APPSETTINGS (MICROSERVICE_ID, ENVIRONMENT_ID, SETTINGS_JSON) VALUES (1, 3, '{"theirs":true}')`)

	if _, err := repo.CreateMicroserviceConfig(ctx, servicesettings.MicroserviceAppSettings{
		MicroserviceID: 1, EnvironmentID: 3, SettingsJSON: json.RawMessage(`{"mine":true}`),
	}); !errors.Is(err, servicesettings.ErrConfigExists) {
		t.Fatalf("lost create of an appsettings row: expected ErrConfigExists, got %v", err)
	}
}

func TestReferenceTablesCreateAndList(t *testing.T) {
	repo, _ := microRepo(t)
	ctx := context.Background()

	svc, err := repo.CreateMicroservice(ctx, "billing-api")
	if err != nil {
		t.Fatalf("create microservice: %v", err)
	}
	if svc.ID <= 3 || svc.Microservice != "billing-api" {
		t.Fatalf("unexpected microservice: %+v", svc)
	}
	if _, err := repo.CreateMicroservice(ctx, "billing-api"); !errors.Is(err, servicesettings.ErrMicroserviceExists) {
		t.Fatalf("duplicate microservice: expected ErrMicroserviceExists, got %v", err)
	}
	if read, err := repo.GetMicroserviceByID(ctx, svc.ID); err != nil || read.Microservice != "billing-api" {
		t.Fatalf("read back microservice: %+v %v", read, err)
	}
	if _, err := repo.GetMicroserviceByID(ctx, 999999); !errors.Is(err, servicesettings.ErrMicroserviceNotFound) {
		t.Fatalf("missing microservice: expected ErrMicroserviceNotFound, got %v", err)
	}

	env, err := repo.CreateEnvironment(ctx, "canary")
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}
	if _, err := repo.CreateEnvironment(ctx, "canary"); !errors.Is(err, servicesettings.ErrEnvironmentExists) {
		t.Fatalf("duplicate environment: expected ErrEnvironmentExists, got %v", err)
	}

	services, err := repo.ListMicroservices(ctx, servicesettings.Page{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("list microservices: %v", err)
	}
	if len(services) != 2 || services[0].ID != 3 || services[1].ID != svc.ID {
		t.Fatalf("unexpected microservice page: %+v", services)
	}

	envs, err := repo.ListEnvironments(ctx, servicesettings.Page{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("list environments: %v", err)
	}
	if len(envs) != 2 || envs[0].ID != 3 || envs[1].ID != env.ID {
		t.Fatalf("unexpected environment page: %+v", envs)
	}

	// Nothing points at either new row yet, so both delete cleanly. The refusal
	// path is the next two tests; this is the half that proves the guard is a
	// guard and not a blanket refusal.
	if err := repo.DeleteMicroservice(ctx, svc.ID); err != nil {
		t.Fatalf("delete unreferenced microservice: %v", err)
	}
	if err := repo.DeleteEnvironment(ctx, env.ID); err != nil {
		t.Fatalf("delete unreferenced environment: %v", err)
	}
	if err := repo.DeleteMicroservice(ctx, svc.ID); !errors.Is(err, servicesettings.ErrMicroserviceNotFound) {
		t.Fatalf("second delete: expected ErrMicroserviceNotFound, got %v", err)
	}
	if err := repo.DeleteEnvironment(ctx, env.ID); !errors.Is(err, servicesettings.ErrEnvironmentNotFound) {
		t.Fatalf("second delete: expected ErrEnvironmentNotFound, got %v", err)
	}
}

// An environment is the widest blast radius in the schema: cascading its delete
// would wipe every flag value, appsettings row and bundle for a whole stage. The
// guard is three sub-selects in one statement, and this walks them one at a time
// — removing only the flag values and expecting a refusal is what catches a
// guard that only ever looked at the first table.
func TestDeleteEnvironmentRefusesWhileReferenced(t *testing.T) {
	repo, s := microRepo(t)
	ctx := context.Background()

	if err := repo.DeleteEnvironment(ctx, 1); !errors.Is(err, servicesettings.ErrEnvironmentInUse) {
		t.Fatalf("environment 1 is referenced by all three domains: expected ErrEnvironmentInUse, got %v", err)
	}

	if _, err := s.DB.ExecContext(ctx, `DELETE FROM CONFIG_FLAG_VALUE WHERE ENVIRONMENT_ID = 1`); err != nil {
		t.Fatalf("clear flag values: %v", err)
	}
	if err := repo.DeleteEnvironment(ctx, 1); !errors.Is(err, servicesettings.ErrEnvironmentInUse) {
		t.Fatalf("appsettings still reference environment 1: expected ErrEnvironmentInUse, got %v", err)
	}

	if _, err := s.DB.ExecContext(ctx, `DELETE FROM CONFIG_MICROSERVICE_APPSETTINGS WHERE ENVIRONMENT_ID = 1`); err != nil {
		t.Fatalf("clear appsettings: %v", err)
	}
	if err := repo.DeleteEnvironment(ctx, 1); !errors.Is(err, servicesettings.ErrEnvironmentInUse) {
		t.Fatalf("localization still references environment 1: expected ErrEnvironmentInUse, got %v", err)
	}

	if _, err := s.DB.ExecContext(ctx, `DELETE FROM CONFIG_LOCALIZATION WHERE ENVIRONMENT_ID = 1`); err != nil {
		t.Fatalf("clear localization: %v", err)
	}
	if err := repo.DeleteEnvironment(ctx, 1); err != nil {
		t.Fatalf("delete of an unreferenced environment: %v", err)
	}
}

func TestDeleteMicroserviceRefusesWhileReferenced(t *testing.T) {
	repo, s := microRepo(t)
	ctx := context.Background()

	// Microservice 1 (catalog-api) is referenced by appsettings 200 and by both
	// localization bundles.
	if err := repo.DeleteMicroservice(ctx, 1); !errors.Is(err, servicesettings.ErrMicroserviceInUse) {
		t.Fatalf("expected ErrMicroserviceInUse, got %v", err)
	}

	if _, err := s.DB.ExecContext(ctx, `DELETE FROM CONFIG_MICROSERVICE_APPSETTINGS WHERE MICROSERVICE_ID = 1`); err != nil {
		t.Fatalf("clear appsettings: %v", err)
	}
	if err := repo.DeleteMicroservice(ctx, 1); !errors.Is(err, servicesettings.ErrMicroserviceInUse) {
		t.Fatalf("localization still references microservice 1: expected ErrMicroserviceInUse, got %v", err)
	}

	if _, err := s.DB.ExecContext(ctx, `DELETE FROM CONFIG_LOCALIZATION WHERE MICROSERVICE_ID = 1`); err != nil {
		t.Fatalf("clear localization: %v", err)
	}
	if err := repo.DeleteMicroservice(ctx, 1); err != nil {
		t.Fatalf("delete of an unreferenced microservice: %v", err)
	}
}
