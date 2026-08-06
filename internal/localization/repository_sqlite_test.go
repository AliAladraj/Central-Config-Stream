package localization_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/ErasedKyte/Central-Config-Stream/internal/database"
	"github.com/ErasedKyte/Central-Config-Stream/internal/localization"
)

func newTestRepo(t *testing.T) (*localization.SQLiteRepository, *sql.DB) {
	t.Helper()
	db, err := database.NewSQLiteDB("file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "t.db")))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return localization.NewSQLiteRepository(db), db
}

// ageSeededRows backdates every seeded row so a later update is the only one
// inside a recent reconcile window. Without this the seed rows carry the
// current timestamp and every window matches everything.
func ageSeededRows(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`UPDATE CONFIG_LOCALIZATION SET UPDATED_AT = '2000-01-01 00:00:00'`); err != nil {
		t.Fatalf("backdate rows: %v", err)
	}
}

func TestCreateAndDeleteLocalization(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()

	created, err := repo.CreateLocalization(ctx, localization.Localization{
		MicroserviceID: 2, EnvironmentID: 1, Locale: "fr-FR",
		BundleJSON: json.RawMessage(`{"cart.title":"Panier"}`),
	})
	if err != nil {
		t.Fatalf("create localization: %v", err)
	}
	if created.ID == 0 || string(created.BundleJSON) != `{"cart.title":"Panier"}` {
		t.Fatalf("unexpected row: %+v", created)
	}

	// The insert reads the row back by its natural key, so both lookups have to
	// land on the same row.
	byID, err := repo.GetLocalizationByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if byID.Locale != "fr-FR" || byID.MicroserviceID != 2 {
		t.Fatalf("get by id returned another row: %+v", byID)
	}

	// The natural key is unique, so a second create is a conflict, not a
	// duplicate row.
	if _, err := repo.CreateLocalization(ctx, localization.Localization{
		MicroserviceID: 2, EnvironmentID: 1, Locale: "fr-FR", BundleJSON: json.RawMessage(`{}`),
	}); !errors.Is(err, localization.ErrLocalizationExists) {
		t.Fatalf("expected ErrLocalizationExists, got %v", err)
	}

	// A missing parent is named rather than left to the foreign key, because the
	// caller has to tell it apart from a collision to pick a status code.
	if _, err := repo.CreateLocalization(ctx, localization.Localization{
		MicroserviceID: 999, EnvironmentID: 1, Locale: "de-DE", BundleJSON: json.RawMessage(`{}`),
	}); !errors.Is(err, localization.ErrMicroserviceNotFound) {
		t.Fatalf("expected ErrMicroserviceNotFound, got %v", err)
	}
	if _, err := repo.CreateLocalization(ctx, localization.Localization{
		MicroserviceID: 2, EnvironmentID: 999, Locale: "de-DE", BundleJSON: json.RawMessage(`{}`),
	}); !errors.Is(err, localization.ErrEnvironmentNotFound) {
		t.Fatalf("expected ErrEnvironmentNotFound, got %v", err)
	}

	// The delete hands back the row it removed: afterwards there is nothing left
	// to derive the KV key from.
	removed, err := repo.DeleteLocalization(ctx, created.ID)
	if err != nil {
		t.Fatalf("delete localization: %v", err)
	}
	if removed.EnvironmentID != 1 || removed.MicroserviceID != 2 || removed.Locale != "fr-FR" {
		t.Fatalf("unexpected removed row: %+v", removed)
	}
	if _, err := repo.GetLocalizationByID(ctx, created.ID); !errors.Is(err, localization.ErrLocalizationNotFound) {
		t.Fatalf("expected ErrLocalizationNotFound after delete, got %v", err)
	}
	if _, err := repo.DeleteLocalization(ctx, created.ID); !errors.Is(err, localization.ErrLocalizationNotFound) {
		t.Fatalf("second delete: expected ErrLocalizationNotFound, got %v", err)
	}
}

// The update rewrites the whole natural key, so it carries the create's checks —
// and the row it is rewriting is excluded from the uniqueness one, or every
// in-place edit would collide with itself.
func TestUpdateLocalizationRewritesTheNaturalKey(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()

	// Seed row 301 is (microservice 1, environment 1, pt-BR); row 300 holds
	// en-US in the same pair.
	if _, err := repo.UpdateLocalization(ctx, localization.Localization{
		ID: 301, MicroserviceID: 1, EnvironmentID: 1, Locale: "en-US", BundleJSON: json.RawMessage(`{}`),
	}); !errors.Is(err, localization.ErrLocalizationExists) {
		t.Fatalf("expected ErrLocalizationExists, got %v", err)
	}

	moved, err := repo.UpdateLocalization(ctx, localization.Localization{
		ID: 301, MicroserviceID: 2, EnvironmentID: 3, Locale: "fr-FR",
		BundleJSON: json.RawMessage(`{"a":"b"}`),
	})
	if err != nil {
		t.Fatalf("move the row: %v", err)
	}
	if moved.MicroserviceID != 2 || moved.EnvironmentID != 3 || moved.Locale != "fr-FR" {
		t.Fatalf("the update did not rewrite the key: %+v", moved)
	}

	// The old key is free again, and nothing answers a lookup on it.
	if _, err := repo.GetLocalization(ctx, 1, 1, "pt-BR"); !errors.Is(err, localization.ErrLocalizationNotFound) {
		t.Fatalf("expected ErrLocalizationNotFound on the old key, got %v", err)
	}

	if _, err := repo.UpdateLocalization(ctx, localization.Localization{
		ID: 999, MicroserviceID: 1, EnvironmentID: 1, Locale: "pt-BR", BundleJSON: json.RawMessage(`{}`),
	}); !errors.Is(err, localization.ErrLocalizationNotFound) {
		t.Fatalf("update of a missing row: expected ErrLocalizationNotFound, got %v", err)
	}
}

func TestListLocalizationsFiltersAndPages(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()

	// The two seeded bundles are both (microservice 1, environment 1); two more
	// under microservice 2 give the filters and the offset something to cut.
	for _, locale := range []string{"en-US", "fr-FR"} {
		if _, err := repo.CreateLocalization(ctx, localization.Localization{
			MicroserviceID: 2, EnvironmentID: 1, Locale: locale, BundleJSON: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatalf("seed %s: %v", locale, err)
		}
	}

	all, err := repo.ListLocalizations(ctx, localization.Filter{Page: localization.Page{Limit: 100}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("expected 4 rows, got %d", len(all))
	}

	byService, err := repo.ListLocalizations(ctx, localization.Filter{
		Page: localization.Page{Limit: 100}, MicroserviceID: 2,
	})
	if err != nil {
		t.Fatalf("list by microservice: %v", err)
	}
	if len(byService) != 2 {
		t.Fatalf("expected 2 rows for microservice 2, got %d", len(byService))
	}

	byLocale, err := repo.ListLocalizations(ctx, localization.Filter{
		Page: localization.Page{Limit: 100}, EnvironmentID: 1, Locale: "en-US",
	})
	if err != nil {
		t.Fatalf("list by environment and locale: %v", err)
	}
	if len(byLocale) != 2 {
		t.Fatalf("expected 2 en-US rows in environment 1, got %d", len(byLocale))
	}

	// The page is bound exactly as given, and the order is by id, so an offset
	// lands on the row the caller can predict from the unpaged list.
	page, err := repo.ListLocalizations(ctx, localization.Filter{
		Page: localization.Page{Limit: 2, Offset: 2},
	})
	if err != nil {
		t.Fatalf("list page: %v", err)
	}
	if len(page) != 2 || page[0].ID != all[2].ID || page[1].ID != all[3].ID {
		t.Fatalf("offset page did not line up: %+v", page)
	}

	// Nothing is normalised down here — a zero limit fetches nothing, which is
	// why the service normalises the page before a repository ever sees it.
	empty, err := repo.ListLocalizations(ctx, localization.Filter{})
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
func TestUpdateLocalizationOptimisticConcurrency(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()

	current, err := repo.GetLocalizationByID(ctx, 301)
	if err != nil {
		t.Fatalf("read current: %v", err)
	}

	// Matching timestamp: the guarded update applies.
	expected := current.UpdatedAt
	updated, err := repo.UpdateLocalization(ctx, localization.Localization{
		ID: 301, MicroserviceID: 1, EnvironmentID: 1, Locale: "pt-BR",
		BundleJSON: json.RawMessage(`{"a":"b"}`), ExpectedUpdatedAt: &expected,
	})
	if err != nil {
		t.Fatalf("guarded update with the current timestamp: %v", err)
	}
	if string(updated.BundleJSON) != `{"a":"b"}` {
		t.Fatalf("unexpected row: %+v", updated)
	}

	// Stale timestamp: somebody else got there first.
	stale := expected.Add(-time.Hour)
	_, err = repo.UpdateLocalization(ctx, localization.Localization{
		ID: 301, MicroserviceID: 1, EnvironmentID: 1, Locale: "pt-BR",
		BundleJSON: json.RawMessage(`{"loser":true}`), ExpectedUpdatedAt: &stale,
	})
	if !errors.Is(err, localization.ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}

	// A guard on a row that does not exist is still a not-found, not a conflict.
	_, err = repo.UpdateLocalization(ctx, localization.Localization{
		ID: 999, MicroserviceID: 2, EnvironmentID: 1, Locale: "fr-FR",
		BundleJSON: json.RawMessage(`{}`), ExpectedUpdatedAt: &stale,
	})
	if !errors.Is(err, localization.ErrLocalizationNotFound) {
		t.Fatalf("expected ErrLocalizationNotFound, got %v", err)
	}

	// No guard: last write still wins.
	if _, err := repo.UpdateLocalization(ctx, localization.Localization{
		ID: 301, MicroserviceID: 1, EnvironmentID: 1, Locale: "pt-BR",
		BundleJSON: json.RawMessage(`{"forced":true}`),
	}); err != nil {
		t.Fatalf("unguarded update: %v", err)
	}
	after, err := repo.GetLocalizationByID(ctx, 301)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(after.BundleJSON) != `{"forced":true}` {
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
	if len(rows) != 2 { // seeded bundles
		t.Fatalf("expected 2 seeded rows, got %d", len(rows))
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

	if _, err := repo.UpdateLocalization(ctx, localization.Localization{
		ID: 301, MicroserviceID: 1, EnvironmentID: 1, Locale: "pt-BR",
		BundleJSON: json.RawMessage(`{"a":"b"}`),
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
	if recent[0].ID != 301 || string(recent[0].BundleJSON) != `{"a":"b"}` {
		t.Fatalf("unexpected row: %+v", recent[0])
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
		CREATE TRIGGER winner_localization BEFORE INSERT ON CONFIG_LOCALIZATION
		WHEN NEW.LOCALE = 'race-me'
		BEGIN
			INSERT INTO CONFIG_LOCALIZATION (MICROSERVICE_ID, ENVIRONMENT_ID, LOCALE, BUNDLE_JSON)
			VALUES (2, 1, 'race-me', '{"theirs":true}');
		END;`); err != nil {
		t.Fatalf("install the racing writer: %v", err)
	}

	if _, err := repo.CreateLocalization(ctx, localization.Localization{
		MicroserviceID: 2, EnvironmentID: 1, Locale: "race-me", BundleJSON: json.RawMessage(`{"mine":true}`),
	}); !errors.Is(err, localization.ErrLocalizationExists) {
		t.Fatalf("lost create: expected ErrLocalizationExists, got %v", err)
	}
}
