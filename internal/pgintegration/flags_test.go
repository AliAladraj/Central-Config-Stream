package pgintegration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AliAladraj/Central-Config-Stream/internal/flagsconfig"
)

func flagsRepo(t *testing.T, opts ...stackOption) (*flagsconfig.PostgresRepository, *stack) {
	t.Helper()
	s := newStack(t, opts...)
	return flagsconfig.NewPostgresRepository(s.DB), s
}

// The create/read/list round trip over the real driver. The filters are all
// exercised because each one is a separately built WHERE fragment with its own
// hand-numbered $n placeholder — the failure mode is not "the filter is wrong",
// it is "the third filter binds the second one's argument", and only running it
// finds that.
func TestFlagsCreateReadAndList(t *testing.T) {
	repo, _ := flagsRepo(t)
	ctx := context.Background()

	created, err := repo.CreateFlag(ctx, flagsconfig.Flag{FlagKey: "checkout_v3", IsActive: 1})
	if err != nil {
		t.Fatalf("create flag: %v", err)
	}
	if created.ID == 0 || created.FlagKey != "checkout_v3" || created.IsActive != 1 {
		t.Fatalf("unexpected created flag: %+v", created)
	}
	if created.UpdatedAt.IsZero() {
		t.Fatalf("created flag carries no UPDATED_AT: %+v", created)
	}

	read, err := repo.GetFlagsByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if read.FlagKey != created.FlagKey || !read.UpdatedAt.Equal(created.UpdatedAt) {
		t.Fatalf("read back %+v, created %+v", read, created)
	}

	if _, err := repo.GetFlagsByID(ctx, 999999); !errors.Is(err, flagsconfig.ErrFlagNotFound) {
		t.Fatalf("read of a missing flag: expected ErrFlagNotFound, got %v", err)
	}

	all, err := repo.ListFlags(ctx, flagsconfig.FlagFilter{Page: flagsconfig.Page{Limit: 100}})
	if err != nil {
		t.Fatalf("list flags: %v", err)
	}
	if len(all) != 4 { // three seeded plus the one created above
		t.Fatalf("expected 4 flags, got %d (%+v)", len(all), all)
	}

	byKey, err := repo.ListFlags(ctx, flagsconfig.FlagFilter{Page: flagsconfig.Page{Limit: 100}, FlagKey: "dark_mode"})
	if err != nil {
		t.Fatalf("list flags by key: %v", err)
	}
	if len(byKey) != 1 || byKey[0].FlagKey != "dark_mode" {
		t.Fatalf("unexpected rows for dark_mode: %+v", byKey)
	}

	// The filtered list numbers its LIMIT/OFFSET binds after the filter's, so
	// paging is checked with a filter applied and not only without one.
	firstPage, err := repo.ListFlags(ctx, flagsconfig.FlagFilter{Page: flagsconfig.Page{Limit: 2}})
	if err != nil {
		t.Fatalf("list flags page 1: %v", err)
	}
	secondPage, err := repo.ListFlags(ctx, flagsconfig.FlagFilter{Page: flagsconfig.Page{Limit: 2, Offset: 2}})
	if err != nil {
		t.Fatalf("list flags page 2: %v", err)
	}
	if len(firstPage) != 2 || len(secondPage) != 2 {
		t.Fatalf("pages were %d and %d rows, want 2 and 2", len(firstPage), len(secondPage))
	}
	if firstPage[0].ID != all[0].ID || secondPage[0].ID != all[2].ID {
		t.Fatalf("pages did not line up with the unpaged list: %+v %+v", firstPage, secondPage)
	}
}

func TestFlagValuesCreateReadAndList(t *testing.T) {
	repo, _ := flagsRepo(t)
	ctx := context.Background()

	// Environment 2 (staging) has no value for flag 8 yet.
	created, flagKey, err := repo.CreateFlagValue(ctx, flagsconfig.FlagValue{
		EnvironmentID: 2, FlagId: 8, Enabled: 1, Value: "on",
	})
	if err != nil {
		t.Fatalf("create flag value: %v", err)
	}
	if flagKey != "dark_mode" {
		t.Fatalf("create returned flag key %q, want dark_mode", flagKey)
	}
	if created.ID == 0 || created.Value != "on" || created.Enabled != 1 {
		t.Fatalf("unexpected created row: %+v", created)
	}

	read, err := repo.GetFlagValueByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if read.Value != "on" || read.EnvironmentID != 2 || read.FlagId != 8 {
		t.Fatalf("read back %+v", read)
	}

	// Referential checks run in the application so the caller can tell "no such
	// environment" apart from "already exists" and pick a status code.
	if _, _, err := repo.CreateFlagValue(ctx, flagsconfig.FlagValue{EnvironmentID: 2, FlagId: 8, Value: "again"}); !errors.Is(err, flagsconfig.ErrFlagValueExists) {
		t.Fatalf("duplicate natural key: expected ErrFlagValueExists, got %v", err)
	}
	if _, _, err := repo.CreateFlagValue(ctx, flagsconfig.FlagValue{EnvironmentID: 999, FlagId: 8, Value: "x"}); !errors.Is(err, flagsconfig.ErrEnvironmentNotFound) {
		t.Fatalf("missing environment: expected ErrEnvironmentNotFound, got %v", err)
	}
	if _, _, err := repo.CreateFlagValue(ctx, flagsconfig.FlagValue{EnvironmentID: 2, FlagId: 999, Value: "x"}); !errors.Is(err, flagsconfig.ErrFlagNotFound) {
		t.Fatalf("missing flag: expected ErrFlagNotFound, got %v", err)
	}

	all, err := repo.ListFlagValues(ctx, flagsconfig.FlagValueFilter{Page: flagsconfig.Page{Limit: 100}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 5 { // four seeded plus the one created above
		t.Fatalf("expected 5 flag values, got %d", len(all))
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
	if len(byKey) != 2 {
		t.Fatalf("expected 2 rows for search_v2, got %d", len(byKey))
	}

	// Both filters at once: the second one has to bind $2, and the LIMIT/OFFSET
	// after it $3 and $4.
	both, err := repo.ListFlagValues(ctx, flagsconfig.FlagValueFilter{
		Page: flagsconfig.Page{Limit: 100}, EnvironmentID: 3, FlagKey: "search_v2",
	})
	if err != nil {
		t.Fatalf("list by env and key: %v", err)
	}
	if len(both) != 1 || both[0].ID != 103 || both[0].FlagKey != "search_v2" || both[0].EnvironmentID != 3 {
		t.Fatalf("unexpected rows for (env 3, search_v2): %+v", both)
	}

	page, err := repo.ListFlagValues(ctx, flagsconfig.FlagValueFilter{Page: flagsconfig.Page{Limit: 2, Offset: 2}})
	if err != nil {
		t.Fatalf("list page: %v", err)
	}
	if len(page) != 2 || page[0].ID != all[2].ID || page[1].ID != all[3].ID {
		t.Fatalf("offset page did not line up: %+v", page)
	}
}

// The optimistic-concurrency guard is opt-in, and the three outcomes have to
// stay distinguishable: a guarded UPDATE that changes no rows cannot tell a lost
// race from a row that was never there, so the repository reads the row back to
// decide. Getting that wrong turns a missing row into a 409 or a lost race into
// a 404, and both mislead the client into retrying the wrong thing.
func TestFlagValueGuardedUpdate(t *testing.T) {
	repo, _ := flagsRepo(t)
	ctx := context.Background()

	current, err := repo.GetFlagValueByID(ctx, 100)
	if err != nil {
		t.Fatalf("read current: %v", err)
	}

	expected := current.UpdatedAt
	updated, key, err := repo.UpdateFlagValue(ctx, flagsconfig.FlagValue{
		ID: 100, Value: "canary", Enabled: 1, ExpectedUpdatedAt: &expected,
	})
	if err != nil {
		t.Fatalf("guarded update with the current timestamp: %v", err)
	}
	if updated.Value != "canary" || key != "search_v2" {
		t.Fatalf("unexpected update result: %+v key=%q", updated, key)
	}
	// The guard is a timestamp equality test against a TIMESTAMPTZ column, so
	// the value the caller read back has to be the exact instant the column
	// holds — a driver that rounded it would make every second update a 409.
	if !updated.UpdatedAt.After(expected) {
		t.Fatalf("UPDATED_AT did not advance: was %s, now %s", expected, updated.UpdatedAt)
	}

	stale := expected
	if _, _, err := repo.UpdateFlagValue(ctx, flagsconfig.FlagValue{
		ID: 100, Value: "loser", ExpectedUpdatedAt: &stale,
	}); !errors.Is(err, flagsconfig.ErrConflict) {
		t.Fatalf("stale guard: expected ErrConflict, got %v", err)
	}

	if _, _, err := repo.UpdateFlagValue(ctx, flagsconfig.FlagValue{
		ID: 999999, Value: "x", ExpectedUpdatedAt: &stale,
	}); !errors.Is(err, flagsconfig.ErrFlagNotFound) {
		t.Fatalf("guarded update of a missing row: expected ErrFlagNotFound, got %v", err)
	}

	// Unguarded, last write still wins — the guard has to stay opt-in.
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

	if _, _, err := repo.UpdateFlagValue(ctx, flagsconfig.FlagValue{ID: 999999, Value: "x"}); !errors.Is(err, flagsconfig.ErrFlagNotFound) {
		t.Fatalf("unguarded update of a missing row: expected ErrFlagNotFound, got %v", err)
	}
}

// The repositories check the natural key with a SELECT and then INSERT, with
// nothing holding the two together. Two creates racing on the same key both
// pass the check and one of them hits the unique index — so the loser's 23505
// has to come back as the same "already exists" the pre-check would have
// produced. Without database.IsUniqueViolation mapping it, the loser gets a
// 500 for a request that was merely second.
//
// This is the assertion that could not be made before: SQLite's constraint
// codes and Postgres's SQLSTATE are different branches of IsUniqueViolation,
// and only the SQLite one had ever executed.
func TestFlagCreateLosingARaceIsAConflict(t *testing.T) {
	repo, s := flagsRepo(t)
	ctx := context.Background()

	s.installRacingWriter(t, "winner_flag", "CONFIG_FLAG",
		"NEW.FLAG_KEY = 'race_me'",
		"INSERT INTO CONFIG_FLAG (FLAG_KEY, IS_ACTIVE) VALUES ('race_me', 1)")

	if _, err := repo.CreateFlag(ctx, flagsconfig.Flag{FlagKey: "race_me", IsActive: 1}); !errors.Is(err, flagsconfig.ErrFlagExists) {
		t.Fatalf("lost create of a flag: expected ErrFlagExists, got %v", err)
	}

	// The same for a flag value's (ENVIRONMENT_ID, FLAG_ID) key. Flag 9 has no
	// value in environment 2, so the pre-check passes.
	s.installRacingWriter(t, "winner_flag_value", "CONFIG_FLAG_VALUE",
		"NEW.ENVIRONMENT_ID = 2 AND NEW.FLAG_ID = 9",
		"INSERT INTO CONFIG_FLAG_VALUE (ENVIRONMENT_ID, FLAG_ID, ENABLED, VALUE) VALUES (2, 9, 1, 'theirs')")

	if _, _, err := repo.CreateFlagValue(ctx, flagsconfig.FlagValue{
		EnvironmentID: 2, FlagId: 9, Enabled: 1, Value: "mine",
	}); !errors.Is(err, flagsconfig.ErrFlagValueExists) {
		t.Fatalf("lost create of a flag value: expected ErrFlagValueExists, got %v", err)
	}
}

// Deleting a flag has to take its value rows with it, in one transaction, and
// report the KV coordinates of everything it removed — after the DELETE there
// is nothing left to derive those keys from, and a key that is never purged
// keeps serving a deleted flag to the whole fleet.
func TestDeleteFlagRemovesItsValues(t *testing.T) {
	repo, s := flagsRepo(t)
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
	if left := s.count(t, `SELECT COUNT(*) FROM CONFIG_FLAG_VALUE WHERE FLAG_ID = 7`); left != 0 {
		t.Fatalf("delete left %d orphaned value rows", left)
	}
	if _, err := repo.GetFlagsByID(ctx, 7); !errors.Is(err, flagsconfig.ErrFlagNotFound) {
		t.Fatalf("expected ErrFlagNotFound after delete, got %v", err)
	}
	if _, err := repo.DeleteFlag(ctx, 7); !errors.Is(err, flagsconfig.ErrFlagNotFound) {
		t.Fatalf("second delete: expected ErrFlagNotFound, got %v", err)
	}
}

func TestDeleteFlagValueReportsItsKey(t *testing.T) {
	repo, _ := flagsRepo(t)
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

// A stalled sweep has to strand the same rows every cycle, which it only does
// if the reconcile query orders its rows. Postgres is free to return an
// unordered query in any order at all, and will happily change its mind once a
// row is updated in place — so this is a real risk here in a way it never was
// against the single-threaded SQLite file.
func TestListAllForReconcileIsOrderedAndComplete(t *testing.T) {
	repo, _ := flagsRepo(t)
	ctx := context.Background()

	// Rewrite a row in the middle so its physical position moves.
	if _, _, err := repo.UpdateFlagValue(ctx, flagsconfig.FlagValue{ID: 101, Value: "moved", Enabled: 1}); err != nil {
		t.Fatalf("update: %v", err)
	}

	rows, err := repo.ListAllForReconcile(ctx, time.Time{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 4 { // seeded flag values
		t.Fatalf("expected 4 seeded rows, got %d", len(rows))
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].ID <= rows[i-1].ID {
			t.Fatalf("rows are not ordered by id: %d after %d", rows[i].ID, rows[i-1].ID)
		}
	}
}
