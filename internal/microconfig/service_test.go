package microconfig_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ErasedKyte/Central-Config-Stream/internal/database"
	"github.com/ErasedKyte/Central-Config-Stream/internal/messaging"
	"github.com/ErasedKyte/Central-Config-Stream/internal/microconfig"
)

// The services are exercised against the SQLite repository rather than a stub:
// referential integrity is enforced by the repository, so a stub would test the
// checks by restating them.
func newTestService(t *testing.T) (*microconfig.Service, *capturePub, *sql.DB) {
	t.Helper()
	db, err := database.NewSQLiteDB("file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "t.db")))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	pub := &capturePub{}
	return microconfig.NewService(microconfig.NewSQLiteRepository(db), pub), pub, db
}

type capturePub struct {
	messaging.NoopPublisher
	published []string
	deleted   []string
}

func (c *capturePub) PublishMicroConfig(ctx context.Context, env, ms int64, settings json.RawMessage) error {
	c.published = append(c.published, messaging.MicroKey(env, ms))
	return nil
}

func (c *capturePub) DeleteKey(ctx context.Context, bucket, key string) error {
	c.deleted = append(c.deleted, bucket+"|"+key)
	return nil
}

// A settings payload that is not a JSON object breaks every consumer's binding
// at once, and nothing downstream would catch it — KV publishes whatever bytes
// it is handed. So it is refused at write time.
func TestCreateRejectsNonObjectSettings(t *testing.T) {
	svc, pub, _ := newTestService(t)
	ctx := context.Background()

	for _, settings := range []string{`[1,2,3]`, `"a string"`, `42`, `null`, `{"broken":`} {
		_, err := svc.CreateMicroserviceConfig(ctx, microconfig.MicroserviceAppSettings{
			MicroserviceID: 2, EnvironmentID: 3, SettingsJSON: json.RawMessage(settings),
		})
		if !errors.Is(err, microconfig.ErrInvalidSettingsJSON) {
			t.Errorf("settings %s: expected ErrInvalidSettingsJSON, got %v", settings, err)
		}
	}
	if len(pub.published) != 0 {
		t.Fatalf("a rejected payload still reached KV: %v", pub.published)
	}

	// The same rule applies to an update of an existing row.
	_, err := svc.UpdateMicroserviceConfig(ctx, microconfig.MicroserviceAppSettings{
		ID: 200, MicroserviceID: 1, EnvironmentID: 1, SettingsJSON: json.RawMessage(`[1,2,3]`),
	})
	if !errors.Is(err, microconfig.ErrInvalidSettingsJSON) {
		t.Fatalf("update: expected ErrInvalidSettingsJSON, got %v", err)
	}
}

func TestCreateMicroserviceConfigPublishes(t *testing.T) {
	svc, pub, _ := newTestService(t)
	ctx := context.Background()

	// Microservice 2 has no appsettings in environment 3 yet.
	created, err := svc.CreateMicroserviceConfig(ctx, microconfig.MicroserviceAppSettings{
		MicroserviceID: 2, EnvironmentID: 3, SettingsJSON: json.RawMessage(`{"timeoutMs":100}`),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == 0 {
		t.Fatalf("create returned no id: %+v", created)
	}
	if len(pub.published) != 1 || pub.published[0] != "3.2" {
		t.Fatalf("create did not write through to KV: %v", pub.published)
	}

	if _, err := svc.CreateMicroserviceConfig(ctx, microconfig.MicroserviceAppSettings{
		MicroserviceID: 2, EnvironmentID: 3, SettingsJSON: json.RawMessage(`{}`),
	}); !errors.Is(err, microconfig.ErrConfigExists) {
		t.Fatalf("expected ErrConfigExists, got %v", err)
	}
	if _, err := svc.CreateMicroserviceConfig(ctx, microconfig.MicroserviceAppSettings{
		MicroserviceID: 999, EnvironmentID: 3, SettingsJSON: json.RawMessage(`{}`),
	}); !errors.Is(err, microconfig.ErrMicroserviceNotFound) {
		t.Fatalf("expected ErrMicroserviceNotFound, got %v", err)
	}
	if _, err := svc.CreateMicroserviceConfig(ctx, microconfig.MicroserviceAppSettings{
		MicroserviceID: 2, EnvironmentID: 999, SettingsJSON: json.RawMessage(`{}`),
	}); !errors.Is(err, microconfig.ErrEnvironmentNotFound) {
		t.Fatalf("expected ErrEnvironmentNotFound, got %v", err)
	}
}

func TestDeleteMicroserviceConfigPurgesKey(t *testing.T) {
	svc, pub, _ := newTestService(t)
	ctx := context.Background()

	// Seed row 200 is microservice 1 in environment 1.
	if err := svc.DeleteMicroserviceConfig(ctx, 200); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(pub.deleted) != 1 || pub.deleted[0] != "MICROCONFIG|1.1" {
		t.Fatalf("delete did not purge the KV key: %v", pub.deleted)
	}
	if _, err := svc.GetMicroserviceConfigByID(ctx, 200); !errors.Is(err, microconfig.ErrConfigNotFound) {
		t.Fatalf("expected ErrConfigNotFound after delete, got %v", err)
	}
}

// The reference tables refuse to be deleted out from under the rows that point
// at them: cascading would silently take those settings rows with them.
func TestDeleteReferenceRowsRefuseWhenInUse(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()

	if err := svc.DeleteMicroservice(ctx, 1); !errors.Is(err, microconfig.ErrMicroserviceInUse) {
		t.Fatalf("expected ErrMicroserviceInUse, got %v", err)
	}
	if err := svc.DeleteEnvironment(ctx, 1); !errors.Is(err, microconfig.ErrEnvironmentInUse) {
		t.Fatalf("expected ErrEnvironmentInUse, got %v", err)
	}

	// Environment 2 (staging) is seeded but unused, so it can go.
	if err := svc.DeleteEnvironment(ctx, 2); err != nil {
		t.Fatalf("delete unused environment: %v", err)
	}
	if err := svc.DeleteEnvironment(ctx, 2); !errors.Is(err, microconfig.ErrEnvironmentNotFound) {
		t.Fatalf("second delete: expected ErrEnvironmentNotFound, got %v", err)
	}
}

func TestCreateReferenceRows(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()

	env, err := svc.CreateEnvironment(ctx, microconfig.Environment{Environment: "qa"})
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}
	if env.ID == 0 || env.Environment != "qa" {
		t.Fatalf("unexpected environment: %+v", env)
	}
	if _, err := svc.CreateEnvironment(ctx, microconfig.Environment{Environment: "qa"}); !errors.Is(err, microconfig.ErrEnvironmentExists) {
		t.Fatalf("expected ErrEnvironmentExists, got %v", err)
	}
	if _, err := svc.CreateEnvironment(ctx, microconfig.Environment{Environment: "  "}); !errors.Is(err, microconfig.ErrInvalidEnvironmentName) {
		t.Fatalf("expected ErrInvalidEnvironmentName, got %v", err)
	}

	ms, err := svc.CreateMicroservice(ctx, microconfig.Microservice{Microservice: "search-api"})
	if err != nil {
		t.Fatalf("create microservice: %v", err)
	}
	if ms.ID == 0 || ms.Microservice != "search-api" {
		t.Fatalf("unexpected microservice: %+v", ms)
	}
	if _, err := svc.CreateMicroservice(ctx, microconfig.Microservice{Microservice: "search-api"}); !errors.Is(err, microconfig.ErrMicroserviceExists) {
		t.Fatalf("expected ErrMicroserviceExists, got %v", err)
	}

	// A freshly created microservice is unused, so it can be deleted again.
	if err := svc.DeleteMicroservice(ctx, ms.ID); err != nil {
		t.Fatalf("delete unused microservice: %v", err)
	}
}

func TestListFiltersAndPages(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()

	envs, err := svc.ListEnvironments(ctx, microconfig.Page{Limit: 2})
	if err != nil {
		t.Fatalf("list environments: %v", err)
	}
	if len(envs) != 2 {
		t.Fatalf("expected a page of 2 environments, got %d", len(envs))
	}

	configs, err := svc.ListMicroserviceConfigs(ctx, microconfig.AppSettingsFilter{
		Page: microconfig.Page{Limit: 100}, MicroserviceID: 1,
	})
	if err != nil {
		t.Fatalf("list configs: %v", err)
	}
	if len(configs) != 1 || configs[0].MicroserviceID != 1 {
		t.Fatalf("unexpected configs for microservice 1: %+v", configs)
	}
}

// The update path rewrites MICROSERVICE_ID and ENVIRONMENT_ID, so it needs the
// referential and uniqueness checks the create path runs — otherwise it points
// a row at a parent that does not exist, or collides with another row and
// surfaces as a 500 instead of a 409.
func TestUpdateMicroserviceConfigChecksRefsAndUniqueness(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()

	// Seed row 200 is microservice 1 in environment 1.
	if _, err := svc.UpdateMicroserviceConfig(ctx, microconfig.MicroserviceAppSettings{
		ID: 200, MicroserviceID: 999, EnvironmentID: 1, SettingsJSON: json.RawMessage(`{}`),
	}); !errors.Is(err, microconfig.ErrMicroserviceNotFound) {
		t.Errorf("expected ErrMicroserviceNotFound, got %v", err)
	}
	if _, err := svc.UpdateMicroserviceConfig(ctx, microconfig.MicroserviceAppSettings{
		ID: 200, MicroserviceID: 1, EnvironmentID: 999, SettingsJSON: json.RawMessage(`{}`),
	}); !errors.Is(err, microconfig.ErrEnvironmentNotFound) {
		t.Errorf("expected ErrEnvironmentNotFound, got %v", err)
	}

	// Row 201 already holds (microservice 2, environment 1).
	if _, err := svc.UpdateMicroserviceConfig(ctx, microconfig.MicroserviceAppSettings{
		ID: 200, MicroserviceID: 2, EnvironmentID: 1, SettingsJSON: json.RawMessage(`{}`),
	}); !errors.Is(err, microconfig.ErrConfigExists) {
		t.Errorf("expected ErrConfigExists, got %v", err)
	}

	// A row keeping the key it already has is not a collision with itself.
	if _, err := svc.UpdateMicroserviceConfig(ctx, microconfig.MicroserviceAppSettings{
		ID: 200, MicroserviceID: 1, EnvironmentID: 1, SettingsJSON: json.RawMessage(`{"a":1}`),
	}); err != nil {
		t.Errorf("update in place: %v", err)
	}
}

// Moving a row to another environment publishes a new KV key. The old one has
// no row behind it any more, so leaving it there means consumers in the old
// environment keep serving settings that no longer exist.
func TestUpdateMicroserviceConfigPurgesTheKeyItMovedAwayFrom(t *testing.T) {
	svc, pub, _ := newTestService(t)
	ctx := context.Background()

	// Seed row 200 is microservice 1 in environment 1; environment 3 is free.
	if _, err := svc.UpdateMicroserviceConfig(ctx, microconfig.MicroserviceAppSettings{
		ID: 200, MicroserviceID: 1, EnvironmentID: 3, SettingsJSON: json.RawMessage(`{"a":1}`),
	}); err != nil {
		t.Fatalf("move: %v", err)
	}

	if len(pub.published) != 1 || pub.published[0] != "3.1" {
		t.Fatalf("the new key was not published: %v", pub.published)
	}
	if len(pub.deleted) != 1 || pub.deleted[0] != "MICROCONFIG|1.1" {
		t.Fatalf("the key the row moved away from was not purged: %v", pub.deleted)
	}
}

// An update that keeps the same identity has no old key to purge.
func TestUpdateMicroserviceConfigInPlacePurgesNothing(t *testing.T) {
	svc, pub, _ := newTestService(t)

	if _, err := svc.UpdateMicroserviceConfig(context.Background(), microconfig.MicroserviceAppSettings{
		ID: 200, MicroserviceID: 1, EnvironmentID: 1, SettingsJSON: json.RawMessage(`{"a":1}`),
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(pub.deleted) != 0 {
		t.Fatalf("an in-place update purged a key: %v", pub.deleted)
	}
}

// A settings tree bigger than a KV value may be used to be stored and answered
// with a 201, then fail at publish on every sweep from then on — the drift that
// wedged a whole domain's convergence. It is a validation error now, on both
// write paths, and the row is never created.
func TestOversizedSettingsAreRejected(t *testing.T) {
	svc, pub, _ := newTestService(t)
	ctx := context.Background()

	oversized := json.RawMessage(`{"k":"` + strings.Repeat("x", messaging.MaxValueSize) + `"}`)

	_, err := svc.CreateMicroserviceConfig(ctx, microconfig.MicroserviceAppSettings{
		MicroserviceID: 2, EnvironmentID: 3, SettingsJSON: oversized,
	})
	if !errors.Is(err, microconfig.ErrSettingsTooLarge) {
		t.Fatalf("create: expected ErrSettingsTooLarge, got %v", err)
	}

	rows, err := svc.ListMicroserviceConfigs(ctx, microconfig.AppSettingsFilter{
		Page: microconfig.Page{Limit: 100}, MicroserviceID: 2, EnvironmentID: 3,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("an oversized create still wrote a row: %+v", rows)
	}

	_, err = svc.UpdateMicroserviceConfig(ctx, microconfig.MicroserviceAppSettings{
		ID: 200, MicroserviceID: 1, EnvironmentID: 1, SettingsJSON: oversized,
	})
	if !errors.Is(err, microconfig.ErrSettingsTooLarge) {
		t.Fatalf("update: expected ErrSettingsTooLarge, got %v", err)
	}
	if len(pub.published) != 0 {
		t.Fatalf("an oversized tree reached KV: %v", pub.published)
	}

	// A tree right up against the ceiling is still accepted.
	fitting := json.RawMessage(`{"k":"` + strings.Repeat("x", messaging.MaxValueSize-16) + `"}`)
	if _, err := svc.CreateMicroserviceConfig(ctx, microconfig.MicroserviceAppSettings{
		MicroserviceID: 2, EnvironmentID: 3, SettingsJSON: fitting,
	}); err != nil {
		t.Fatalf("a tree inside the ceiling was rejected: %v", err)
	}
}
