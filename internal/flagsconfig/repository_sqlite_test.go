package flagsconfig_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/ErasedKyte/Central-Config-Stream/internal/database"
	"github.com/ErasedKyte/Central-Config-Stream/internal/flagsconfig"
)

func newTestRepo(t *testing.T) (*flagsconfig.SQLiteRepository, *sql.DB) {
	t.Helper()
	db, err := database.NewSQLiteDB("file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "t.db")))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return flagsconfig.NewSQLiteRepository(db), db
}

// ageSeededRows backdates every seeded row so a later update is the only one
// inside a recent reconcile window. Without this the seed rows carry the
// current timestamp and every window matches everything.
func ageSeededRows(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`UPDATE CONFIG_FLAG_VALUE SET UPDATED_AT = '2000-01-01 00:00:00'`); err != nil {
		t.Fatalf("backdate rows: %v", err)
	}
}

func TestListAllForReconcileFullSweep(t *testing.T) {
	repo, _ := newTestRepo(t)

	rows, err := repo.ListAllForReconcile(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 4 { // seeded flag values
		t.Fatalf("expected 4 seeded rows, got %d", len(rows))
	}
}

// The incremental window is what keeps the periodic reconcile off the full
// table, so both halves matter: a future cutoff returns nothing, and a just
// updated row is inside a recent window.
func TestListAllForReconcileIncrementalWindow(t *testing.T) {
	repo, db := newTestRepo(t)
	ctx := context.Background()

	future, err := repo.ListAllForReconcile(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("list future: %v", err)
	}
	if len(future) != 0 {
		t.Fatalf("expected no rows after a future cutoff, got %d", len(future))
	}

	ageSeededRows(t, db)

	if _, _, err := repo.UpdateFlagValue(ctx, flagsconfig.FlagValue{ID: 100, Value: "canary", Enabled: 1}); err != nil {
		t.Fatalf("update: %v", err)
	}

	recent, err := repo.ListAllForReconcile(ctx, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("list recent: %v", err)
	}
	if len(recent) != 1 {
		t.Fatalf("expected only the changed row, got %d", len(recent))
	}
	if recent[0].FlagKey != "search_v2" || recent[0].Value != "canary" {
		t.Fatalf("unexpected row: %+v", recent[0])
	}
}

func TestUpdateFlagValueReturnsFlagKey(t *testing.T) {
	repo, _ := newTestRepo(t)

	val, key, err := repo.UpdateFlagValue(context.Background(),
		flagsconfig.FlagValue{ID: 101, Value: "on", Enabled: 1})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if key != "dark_mode" {
		t.Fatalf("expected flag key dark_mode, got %q", key)
	}
	if val.Value != "on" || val.Enabled != 1 {
		t.Fatalf("unexpected value row: %+v", val)
	}
}

func TestCreateAndDeleteFlag(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()

	created, err := repo.CreateFlag(ctx, flagsconfig.Flag{FlagKey: "new_flag", IsActive: 1})
	if err != nil {
		t.Fatalf("create flag: %v", err)
	}
	if created.ID == 0 || created.FlagKey != "new_flag" {
		t.Fatalf("unexpected flag: %+v", created)
	}

	// The natural key is unique, so a second create is a conflict, not a
	// duplicate row.
	if _, err := repo.CreateFlag(ctx, flagsconfig.Flag{FlagKey: "new_flag", IsActive: 1}); !errors.Is(err, flagsconfig.ErrFlagExists) {
		t.Fatalf("expected ErrFlagExists, got %v", err)
	}

	removed, err := repo.DeleteFlag(ctx, created.ID)
	if err != nil {
		t.Fatalf("delete flag: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("a flag with no values removed %d value rows", len(removed))
	}
	if _, err := repo.GetFlagsByID(ctx, created.ID); !errors.Is(err, flagsconfig.ErrFlagNotFound) {
		t.Fatalf("expected ErrFlagNotFound after delete, got %v", err)
	}
}

// Deleting a flag has to take its value rows with it: leaving them behind would
// orphan rows whose join no longer resolves, and the KV keys they fed would
// never be purged.
func TestDeleteFlagRemovesItsValues(t *testing.T) {
	repo, db := newTestRepo(t)
	ctx := context.Background()

	// Seed flag 7 (search_v2) has values in environments 1 and 3.
	removed, err := repo.DeleteFlag(ctx, 7)
	if err != nil {
		t.Fatalf("delete flag: %v", err)
	}
	if len(removed) != 2 {
		t.Fatalf("expected 2 removed value rows, got %d (%+v)", len(removed), removed)
	}
	for _, row := range removed {
		if row.FlagKey != "search_v2" {
			t.Errorf("removed row carries the wrong key: %+v", row)
		}
	}

	var left int
	if err := db.QueryRow(`SELECT COUNT(*) FROM CONFIG_FLAG_VALUE WHERE FLAG_ID = 7`).Scan(&left); err != nil {
		t.Fatalf("count leftovers: %v", err)
	}
	if left != 0 {
		t.Fatalf("delete left %d orphaned value rows", left)
	}
}

func TestCreateFlagValueChecksReferences(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()

	// Environment 2 (staging) has no value for flag 8 yet.
	created, _, err := repo.CreateFlagValue(ctx, flagsconfig.FlagValue{EnvironmentID: 2, FlagId: 8, Enabled: 1, Value: "on"})
	if err != nil {
		t.Fatalf("create flag value: %v", err)
	}
	if created.ID == 0 || created.Value != "on" {
		t.Fatalf("unexpected row: %+v", created)
	}

	if _, _, err := repo.CreateFlagValue(ctx, flagsconfig.FlagValue{EnvironmentID: 2, FlagId: 8, Value: "again"}); !errors.Is(err, flagsconfig.ErrFlagValueExists) {
		t.Fatalf("expected ErrFlagValueExists, got %v", err)
	}
	if _, _, err := repo.CreateFlagValue(ctx, flagsconfig.FlagValue{EnvironmentID: 999, FlagId: 8, Value: "x"}); !errors.Is(err, flagsconfig.ErrEnvironmentNotFound) {
		t.Fatalf("expected ErrEnvironmentNotFound, got %v", err)
	}
	if _, _, err := repo.CreateFlagValue(ctx, flagsconfig.FlagValue{EnvironmentID: 2, FlagId: 999, Value: "x"}); !errors.Is(err, flagsconfig.ErrFlagNotFound) {
		t.Fatalf("expected ErrFlagNotFound, got %v", err)
	}
}

// The delete has to report the KV coordinates of the row it removed, because
// after the DELETE there is nothing left to derive them from.
func TestDeleteFlagValueReportsItsKey(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()

	removed, err := repo.DeleteFlagValue(ctx, 103) // env 3, search_v2
	if err != nil {
		t.Fatalf("delete flag value: %v", err)
	}
	if removed.EnvironmentID != 3 || removed.FlagKey != "search_v2" {
		t.Fatalf("unexpected removed row: %+v", removed)
	}
	if _, err := repo.GetFlagValueByID(ctx, 103); !errors.Is(err, flagsconfig.ErrFlagNotFound) {
		t.Fatalf("expected ErrFlagNotFound after delete, got %v", err)
	}
	if _, err := repo.DeleteFlagValue(ctx, 103); !errors.Is(err, flagsconfig.ErrFlagNotFound) {
		t.Fatalf("second delete: expected ErrFlagNotFound, got %v", err)
	}
}

func TestListFlagValuesFiltersAndPages(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()

	all, err := repo.ListFlagValues(ctx, flagsconfig.FlagValueFilter{Page: flagsconfig.Page{Limit: 100}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 4 { // seeded flag values
		t.Fatalf("expected 4 seeded rows, got %d", len(all))
	}

	env1, err := repo.ListFlagValues(ctx, flagsconfig.FlagValueFilter{
		Page: flagsconfig.Page{Limit: 100}, EnvironmentID: 1,
	})
	if err != nil {
		t.Fatalf("list env 1: %v", err)
	}
	if len(env1) != 3 {
		t.Fatalf("expected 3 rows in environment 1, got %d", len(env1))
	}

	byKey, err := repo.ListFlagValues(ctx, flagsconfig.FlagValueFilter{
		Page: flagsconfig.Page{Limit: 100}, FlagKey: "search_v2",
	})
	if err != nil {
		t.Fatalf("list by key: %v", err)
	}
	if len(byKey) != 2 || byKey[0].FlagKey != "search_v2" {
		t.Fatalf("unexpected rows for search_v2: %+v", byKey)
	}

	page, err := repo.ListFlagValues(ctx, flagsconfig.FlagValueFilter{
		Page: flagsconfig.Page{Limit: 2, Offset: 2},
	})
	if err != nil {
		t.Fatalf("list page: %v", err)
	}
	if len(page) != 2 || page[0].ID != all[2].ID {
		t.Fatalf("offset page did not line up: %+v", page)
	}
}

// The optimistic-concurrency guard is opt-in: an update carrying the timestamp
// it read must fail once somebody else has changed the row, and an update
// without one keeps the original last-write-wins behaviour.
func TestUpdateFlagValueOptimisticConcurrency(t *testing.T) {
	repo, _ := newTestRepo(t)
	ctx := context.Background()

	current, err := repo.GetFlagValueByID(ctx, 100)
	if err != nil {
		t.Fatalf("read current: %v", err)
	}

	// Matching timestamp: the guarded update applies.
	expected := current.UpdatedAt
	updated, _, err := repo.UpdateFlagValue(ctx, flagsconfig.FlagValue{
		ID: 100, Value: "canary", Enabled: 1, ExpectedUpdatedAt: &expected,
	})
	if err != nil {
		t.Fatalf("guarded update with the current timestamp: %v", err)
	}
	if updated.Value != "canary" {
		t.Fatalf("unexpected value row: %+v", updated)
	}

	// Stale timestamp: somebody else got there first.
	stale := expected.Add(-time.Hour)
	_, _, err = repo.UpdateFlagValue(ctx, flagsconfig.FlagValue{
		ID: 100, Value: "loser", ExpectedUpdatedAt: &stale,
	})
	if !errors.Is(err, flagsconfig.ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}

	// A guard on a row that does not exist is still a not-found, not a conflict.
	_, _, err = repo.UpdateFlagValue(ctx, flagsconfig.FlagValue{
		ID: 999, Value: "x", ExpectedUpdatedAt: &stale,
	})
	if !errors.Is(err, flagsconfig.ErrFlagNotFound) {
		t.Fatalf("expected ErrFlagNotFound, got %v", err)
	}

	// No guard: last write still wins.
	if _, _, err := repo.UpdateFlagValue(ctx, flagsconfig.FlagValue{ID: 100, Value: "forced"}); err != nil {
		t.Fatalf("unguarded update: %v", err)
	}
	after, err := repo.GetFlagValueByID(ctx, 100)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if after.Value != "forced" {
		t.Fatalf("unguarded update did not apply: %+v", after)
	}
}

// The pre-check that keeps a duplicate create out is a SELECT followed by an
// INSERT with nothing holding the two together, so two creates racing on the
// same key both pass the check and one of them hits the constraint. The trigger
// stands in for the winning request: it lands between the loser's check and its
// insert, which is the interleaving a serial test cannot otherwise reach. The
// loser has to see the same "already exists" a serial create would have
// produced, not a bare driver error the handler turns into a 500.
func TestConcurrentCreateCollidesAsAlreadyExists(t *testing.T) {
	repo, db := newTestRepo(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `
		CREATE TRIGGER winner_flag BEFORE INSERT ON CONFIG_FLAG
		WHEN NEW.FLAG_KEY = 'race_me'
		BEGIN
			INSERT INTO CONFIG_FLAG (FLAG_KEY, IS_ACTIVE) VALUES ('race_me', 1);
		END;`); err != nil {
		t.Fatalf("install the racing writer: %v", err)
	}
	if _, err := repo.CreateFlag(ctx, flagsconfig.Flag{FlagKey: "race_me", IsActive: 1}); !errors.Is(err, flagsconfig.ErrFlagExists) {
		t.Fatalf("lost create of a flag: expected ErrFlagExists, got %v", err)
	}

	// The same for a flag value's (ENVIRONMENT_ID, FLAG_ID) key. Flag 9 has no
	// value in environment 2, so the pre-check passes.
	if _, err := db.ExecContext(ctx, `
		CREATE TRIGGER winner_flag_value BEFORE INSERT ON CONFIG_FLAG_VALUE
		WHEN NEW.ENVIRONMENT_ID = 2 AND NEW.FLAG_ID = 9
		BEGIN
			INSERT INTO CONFIG_FLAG_VALUE (ENVIRONMENT_ID, FLAG_ID, ENABLED, VALUE)
			VALUES (2, 9, 1, 'theirs');
		END;`); err != nil {
		t.Fatalf("install the racing writer: %v", err)
	}
	if _, _, err := repo.CreateFlagValue(ctx, flagsconfig.FlagValue{
		EnvironmentID: 2, FlagId: 9, Enabled: 1, Value: "mine",
	}); !errors.Is(err, flagsconfig.ErrFlagValueExists) {
		t.Fatalf("lost create of a flag value: expected ErrFlagValueExists, got %v", err)
	}
}

// The rows a stalled sweep reaches have to be the same ones every cycle;
// without an ORDER BY, which rows get stranded is whatever the database felt
// like returning.
func TestListAllForReconcileIsOrdered(t *testing.T) {
	repo, _ := newTestRepo(t)

	rows, err := repo.ListAllForReconcile(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].ID <= rows[i-1].ID {
			t.Fatalf("rows are not ordered by id: %d after %d", rows[i].ID, rows[i-1].ID)
		}
	}
}
