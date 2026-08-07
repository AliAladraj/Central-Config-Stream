package servicesettings_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/AliAladraj/Central-Config-Stream/internal/database"
	"github.com/AliAladraj/Central-Config-Stream/internal/servicesettings"
)

func newTestRepo(t *testing.T) (*servicesettings.SQLiteRepository, *sql.DB) {
	t.Helper()
	db, err := database.NewSQLiteDB("file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "t.db")))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return servicesettings.NewSQLiteRepository(db), db
}

// ageSeededRows backdates every seeded row so a later update is the only one
// inside a recent reconcile window. Without this the seed rows carry the
// current timestamp and every window matches everything.
func ageSeededRows(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`UPDATE CONFIG_MICROSERVICE_APPSETTINGS SET UPDATED_AT = '2000-01-01 00:00:00'`); err != nil {
		t.Fatalf("backdate rows: %v", err)
	}
}

func TestCreateAndDeleteMicroserviceConfig(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()

	// Microservice 2 has no appsettings in environment 3 yet.
	created, err := repo.CreateMicroserviceConfig(ctx, servicesettings.MicroserviceAppSettings{
		MicroserviceID: 2, EnvironmentID: 3, SettingsJSON: json.RawMessage(`{"http":{"timeoutMs":100}}`),
	})
	if err != nil {
		t.Fatalf("create config: %v", err)
	}
	if created.ID == 0 || string(created.SettingsJSON) != `{"http":{"timeoutMs":100}}` {
		t.Fatalf("unexpected row: %+v", created)
	}

	// The insert reads the row back by its natural key, so both lookups have to
	// land on the same row.
	byID, err := repo.GetMicroserviceConfigByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if byID.MicroserviceID != 2 || byID.EnvironmentID != 3 {
		t.Fatalf("get by id returned another row: %+v", byID)
	}

	// The natural key is unique, so a second create is a conflict, not a
	// duplicate row.
	if _, err := repo.CreateMicroserviceConfig(ctx, servicesettings.MicroserviceAppSettings{
		MicroserviceID: 2, EnvironmentID: 3, SettingsJSON: json.RawMessage(`{}`),
	}); !errors.Is(err, servicesettings.ErrConfigExists) {
		t.Fatalf("expected ErrConfigExists, got %v", err)
	}

	// A missing parent is named rather than left to the foreign key, because the
	// caller has to tell it apart from a collision to pick a status code.
	if _, err := repo.CreateMicroserviceConfig(ctx, servicesettings.MicroserviceAppSettings{
		MicroserviceID: 999, EnvironmentID: 3, SettingsJSON: json.RawMessage(`{}`),
	}); !errors.Is(err, servicesettings.ErrMicroserviceNotFound) {
		t.Fatalf("expected ErrMicroserviceNotFound, got %v", err)
	}
	if _, err := repo.CreateMicroserviceConfig(ctx, servicesettings.MicroserviceAppSettings{
		MicroserviceID: 2, EnvironmentID: 999, SettingsJSON: json.RawMessage(`{}`),
	}); !errors.Is(err, servicesettings.ErrEnvironmentNotFound) {
		t.Fatalf("expected ErrEnvironmentNotFound, got %v", err)
	}

	// The delete hands back the row it removed: afterwards there is nothing left
	// to derive the KV key from.
	removed, err := repo.DeleteMicroserviceConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("delete config: %v", err)
	}
	if removed.EnvironmentID != 3 || removed.MicroserviceID != 2 {
		t.Fatalf("unexpected removed row: %+v", removed)
	}
	if _, err := repo.GetMicroserviceConfigByID(ctx, created.ID); !errors.Is(err, servicesettings.ErrConfigNotFound) {
		t.Fatalf("expected ErrConfigNotFound after delete, got %v", err)
	}
	if _, err := repo.DeleteMicroserviceConfig(ctx, created.ID); !errors.Is(err, servicesettings.ErrConfigNotFound) {
		t.Fatalf("second delete: expected ErrConfigNotFound, got %v", err)
	}
}

// The update rewrites MICROSERVICE_ID and ENVIRONMENT_ID, so it carries the
// create's checks — and the row it is rewriting is excluded from the uniqueness
// one, or every in-place edit would collide with itself.
func TestUpdateMicroserviceConfigRewritesTheNaturalKey(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()

	// Seed row 200 is microservice 1 in environment 1; row 201 already holds
	// microservice 2 in the same environment.
	if _, err := repo.UpdateMicroserviceConfig(ctx, servicesettings.MicroserviceAppSettings{
		ID: 200, MicroserviceID: 2, EnvironmentID: 1, SettingsJSON: json.RawMessage(`{}`),
	}); !errors.Is(err, servicesettings.ErrConfigExists) {
		t.Fatalf("expected ErrConfigExists, got %v", err)
	}

	moved, err := repo.UpdateMicroserviceConfig(ctx, servicesettings.MicroserviceAppSettings{
		ID: 200, MicroserviceID: 1, EnvironmentID: 3, SettingsJSON: json.RawMessage(`{"a":1}`),
	})
	if err != nil {
		t.Fatalf("move the row: %v", err)
	}
	if moved.EnvironmentID != 3 || string(moved.SettingsJSON) != `{"a":1}` {
		t.Fatalf("the update did not rewrite the key: %+v", moved)
	}

	if _, err := repo.UpdateMicroserviceConfig(ctx, servicesettings.MicroserviceAppSettings{
		ID: 999, MicroserviceID: 2, EnvironmentID: 3, SettingsJSON: json.RawMessage(`{}`),
	}); !errors.Is(err, servicesettings.ErrConfigNotFound) {
		t.Fatalf("update of a missing row: expected ErrConfigNotFound, got %v", err)
	}
}

func TestListMicroserviceConfigsFiltersAndPages(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()

	all, err := repo.ListMicroserviceConfigs(ctx, servicesettings.AppSettingsFilter{Page: servicesettings.Page{Limit: 100}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 3 { // seeded appsettings
		t.Fatalf("expected 3 seeded rows, got %d", len(all))
	}

	byService, err := repo.ListMicroserviceConfigs(ctx, servicesettings.AppSettingsFilter{
		Page: servicesettings.Page{Limit: 100}, MicroserviceID: 1,
	})
	if err != nil {
		t.Fatalf("list by microservice: %v", err)
	}
	if len(byService) != 1 || byService[0].MicroserviceID != 1 {
		t.Fatalf("unexpected rows for microservice 1: %+v", byService)
	}

	byEnv, err := repo.ListMicroserviceConfigs(ctx, servicesettings.AppSettingsFilter{
		Page: servicesettings.Page{Limit: 100}, EnvironmentID: 3,
	})
	if err != nil {
		t.Fatalf("list by environment: %v", err)
	}
	if len(byEnv) != 0 {
		t.Fatalf("expected no rows in environment 3, got %d", len(byEnv))
	}

	// The page is bound exactly as given, and the order is by id, so an offset
	// lands on the row the caller can predict from the unpaged list.
	page, err := repo.ListMicroserviceConfigs(ctx, servicesettings.AppSettingsFilter{
		Page: servicesettings.Page{Limit: 2, Offset: 1},
	})
	if err != nil {
		t.Fatalf("list page: %v", err)
	}
	if len(page) != 2 || page[0].ID != all[1].ID || page[1].ID != all[2].ID {
		t.Fatalf("offset page did not line up: %+v", page)
	}

	// Nothing is normalised down here — a zero limit fetches nothing, which is
	// why the service normalises the page before a repository ever sees it.
	empty, err := repo.ListMicroserviceConfigs(ctx, servicesettings.AppSettingsFilter{})
	if err != nil {
		t.Fatalf("list with a zero page: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("a zero limit returned %d rows", len(empty))
	}
}

// The optimistic-concurrency guard is opt-in: an update carrying the timestamp
// it read must fail once somebody else has changed the row, and an update
// without one keeps the original last-write-wins behaviour.
func TestUpdateMicroserviceConfigOptimisticConcurrency(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()

	current, err := repo.GetMicroserviceConfigByID(ctx, 200)
	if err != nil {
		t.Fatalf("read current: %v", err)
	}

	// Matching timestamp: the guarded update applies.
	expected := current.UpdatedAt
	updated, err := repo.UpdateMicroserviceConfig(ctx, servicesettings.MicroserviceAppSettings{
		ID: 200, MicroserviceID: 1, EnvironmentID: 1,
		SettingsJSON: json.RawMessage(`{"a":1}`), ExpectedUpdatedAt: &expected,
	})
	if err != nil {
		t.Fatalf("guarded update with the current timestamp: %v", err)
	}
	if string(updated.SettingsJSON) != `{"a":1}` {
		t.Fatalf("unexpected row: %+v", updated)
	}

	// Stale timestamp: somebody else got there first.
	stale := expected.Add(-time.Hour)
	_, err = repo.UpdateMicroserviceConfig(ctx, servicesettings.MicroserviceAppSettings{
		ID: 200, MicroserviceID: 1, EnvironmentID: 1,
		SettingsJSON: json.RawMessage(`{"loser":true}`), ExpectedUpdatedAt: &stale,
	})
	if !errors.Is(err, servicesettings.ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}

	// A guard on a row that does not exist is still a not-found, not a conflict.
	_, err = repo.UpdateMicroserviceConfig(ctx, servicesettings.MicroserviceAppSettings{
		ID: 999, MicroserviceID: 2, EnvironmentID: 3,
		SettingsJSON: json.RawMessage(`{}`), ExpectedUpdatedAt: &stale,
	})
	if !errors.Is(err, servicesettings.ErrConfigNotFound) {
		t.Fatalf("expected ErrConfigNotFound, got %v", err)
	}

	// No guard: last write still wins.
	if _, err := repo.UpdateMicroserviceConfig(ctx, servicesettings.MicroserviceAppSettings{
		ID: 200, MicroserviceID: 1, EnvironmentID: 1, SettingsJSON: json.RawMessage(`{"forced":true}`),
	}); err != nil {
		t.Fatalf("unguarded update: %v", err)
	}
	after, err := repo.GetMicroserviceConfigByID(ctx, 200)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(after.SettingsJSON) != `{"forced":true}` {
		t.Fatalf("unguarded update did not apply: %+v", after)
	}
}

// The incremental window is what keeps the periodic reconcile off the full
// table, so both halves matter: a future cutoff returns nothing, and a just
// updated row is inside a recent window. The rows a stalled sweep reaches also
// have to be the same ones every cycle, which is what the ordering buys.
func TestListAllForReconcileSweepsAndWindows(t *testing.T) {
	repo, db := newTestRepo(t)
	ctx := context.Background()

	rows, err := repo.ListAllForReconcile(ctx, time.Time{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 3 { // seeded appsettings
		t.Fatalf("expected 3 seeded rows, got %d", len(rows))
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].ID <= rows[i-1].ID {
			t.Fatalf("rows are not ordered by id: %d after %d", rows[i].ID, rows[i-1].ID)
		}
	}

	future, err := repo.ListAllForReconcile(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("list future: %v", err)
	}
	if len(future) != 0 {
		t.Fatalf("expected no rows after a future cutoff, got %d", len(future))
	}

	ageSeededRows(t, db)

	if _, err := repo.UpdateMicroserviceConfig(ctx, servicesettings.MicroserviceAppSettings{
		ID: 200, MicroserviceID: 1, EnvironmentID: 1, SettingsJSON: json.RawMessage(`{"a":1}`),
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	recent, err := repo.ListAllForReconcile(ctx, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("list recent: %v", err)
	}
	if len(recent) != 1 {
		t.Fatalf("expected only the changed row, got %d", len(recent))
	}
	if recent[0].ID != 200 || string(recent[0].SettingsJSON) != `{"a":1}` {
		t.Fatalf("unexpected row: %+v", recent[0])
	}
}

func TestReferenceTableListsPage(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()

	services, err := repo.ListMicroservices(ctx, servicesettings.Page{Limit: 2})
	if err != nil {
		t.Fatalf("list microservices: %v", err)
	}
	if len(services) != 2 || services[0].Microservice != "catalog-api" {
		t.Fatalf("unexpected first page of microservices: %+v", services)
	}

	rest, err := repo.ListMicroservices(ctx, servicesettings.Page{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("list microservices page 2: %v", err)
	}
	if len(rest) != 1 || rest[0].ID <= services[1].ID {
		t.Fatalf("the second page did not follow the first: %+v", rest)
	}

	envs, err := repo.ListEnvironments(ctx, servicesettings.Page{Limit: 2, Offset: 1})
	if err != nil {
		t.Fatalf("list environments: %v", err)
	}
	if len(envs) != 2 || envs[0].Environment != "staging" {
		t.Fatalf("unexpected page of environments: %+v", envs)
	}
}

// The in-use check spans every table that points at the reference row, not just
// the one this package owns. Deleting a microservice that only localization
// still references would orphan those bundles — and their KV keys would outlive
// the row with nothing left to prune them.
func TestDeleteReferenceRowCountsEveryPointingTable(t *testing.T) {
	repo, db := newTestRepo(t)
	ctx := context.Background()

	// Microservice 1 has appsettings row 200 and both seeded bundles. Remove the
	// appsettings and the localization rows still hold it.
	if _, err := repo.DeleteMicroserviceConfig(ctx, 200); err != nil {
		t.Fatalf("delete config: %v", err)
	}
	if err := repo.DeleteMicroservice(ctx, 1); !errors.Is(err, servicesettings.ErrMicroserviceInUse) {
		t.Fatalf("localization rows did not hold the microservice: got %v", err)
	}

	if _, err := db.ExecContext(ctx, `DELETE FROM CONFIG_LOCALIZATION WHERE MICROSERVICE_ID = 1`); err != nil {
		t.Fatalf("clear bundles: %v", err)
	}
	if err := repo.DeleteMicroservice(ctx, 1); err != nil {
		t.Fatalf("delete an unreferenced microservice: %v", err)
	}
	if err := repo.DeleteMicroservice(ctx, 1); !errors.Is(err, servicesettings.ErrMicroserviceNotFound) {
		t.Fatalf("second delete: expected ErrMicroserviceNotFound, got %v", err)
	}

	// Environment 1 is the same story across three tables: appsettings and
	// bundles are gone now, and the seeded flag values alone still hold it.
	if _, err := db.ExecContext(ctx, `DELETE FROM CONFIG_MICROSERVICE_APPSETTINGS WHERE ENVIRONMENT_ID = 1`); err != nil {
		t.Fatalf("clear appsettings: %v", err)
	}
	if err := repo.DeleteEnvironment(ctx, 1); !errors.Is(err, servicesettings.ErrEnvironmentInUse) {
		t.Fatalf("flag values did not hold the environment: got %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM CONFIG_FLAG_VALUE WHERE ENVIRONMENT_ID = 1`); err != nil {
		t.Fatalf("clear flag values: %v", err)
	}
	if err := repo.DeleteEnvironment(ctx, 1); err != nil {
		t.Fatalf("delete an unreferenced environment: %v", err)
	}
}

func TestCreateReferenceRowsCollideOnTheirNames(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()

	ms, err := repo.CreateMicroservice(ctx, "search-api")
	if err != nil {
		t.Fatalf("create microservice: %v", err)
	}
	if ms.ID == 0 || ms.Microservice != "search-api" {
		t.Fatalf("unexpected microservice: %+v", ms)
	}
	if _, err := repo.CreateMicroservice(ctx, "search-api"); !errors.Is(err, servicesettings.ErrMicroserviceExists) {
		t.Fatalf("expected ErrMicroserviceExists, got %v", err)
	}

	env, err := repo.CreateEnvironment(ctx, "qa")
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}
	if env.ID == 0 || env.Environment != "qa" {
		t.Fatalf("unexpected environment: %+v", env)
	}
	if _, err := repo.CreateEnvironment(ctx, "qa"); !errors.Is(err, servicesettings.ErrEnvironmentExists) {
		t.Fatalf("expected ErrEnvironmentExists, got %v", err)
	}
}

// The pre-check that keeps a duplicate create out is a SELECT followed by an
// INSERT with nothing holding the two together, so two creates racing on the
// same natural key both pass the check and one of them hits the constraint. The
// trigger stands in for the winning request: it lands between the loser's check
// and its insert, which is the interleaving a serial test cannot otherwise
// reach. The loser has to see the same "already exists" a serial create would
// have produced, not a bare driver error the handler turns into a 500.
func TestConcurrentCreateCollidesAsAlreadyExists(t *testing.T) {
	repo, db := newTestRepo(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `
		CREATE TRIGGER winner_appsettings BEFORE INSERT ON CONFIG_MICROSERVICE_APPSETTINGS
		WHEN NEW.MICROSERVICE_ID = 2 AND NEW.ENVIRONMENT_ID = 3
		BEGIN
			INSERT INTO CONFIG_MICROSERVICE_APPSETTINGS (MICROSERVICE_ID, ENVIRONMENT_ID, SETTINGS_JSON)
			VALUES (2, 3, '{"theirs":true}');
		END;`); err != nil {
		t.Fatalf("install the racing writer: %v", err)
	}

	if _, err := repo.CreateMicroserviceConfig(ctx, servicesettings.MicroserviceAppSettings{
		MicroserviceID: 2, EnvironmentID: 3, SettingsJSON: json.RawMessage(`{"mine":true}`),
	}); !errors.Is(err, servicesettings.ErrConfigExists) {
		t.Fatalf("lost create: expected ErrConfigExists, got %v", err)
	}

	// The reference tables run the same shape of pre-check before their insert.
	if _, err := db.ExecContext(ctx, `
		CREATE TRIGGER winner_microservice BEFORE INSERT ON CONFIG_MICROSERVICES
		WHEN NEW.MICROSERVICE = 'race-me'
		BEGIN
			INSERT INTO CONFIG_MICROSERVICES (MICROSERVICE) VALUES ('race-me');
		END;`); err != nil {
		t.Fatalf("install the racing writer: %v", err)
	}
	if _, err := repo.CreateMicroservice(ctx, "race-me"); !errors.Is(err, servicesettings.ErrMicroserviceExists) {
		t.Fatalf("lost create of a microservice: expected ErrMicroserviceExists, got %v", err)
	}
}
