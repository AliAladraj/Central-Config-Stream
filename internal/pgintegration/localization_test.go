package pgintegration

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ErasedKyte/Central-Config-Stream/internal/localization"
)

func localizationRepo(t *testing.T, opts ...stackOption) (*localization.PostgresRepository, *stack) {
	t.Helper()
	s := newStack(t, opts...)
	return localization.NewPostgresRepository(s.DB), s
}

func TestLocalizationCreateReadAndList(t *testing.T) {
	repo, _ := localizationRepo(t)
	ctx := context.Background()

	// The bundle carries a non-ASCII value on purpose: the seed does too, and a
	// client_encoding mismatch between the driver and the server would corrupt it
	// somewhere no unit test can see.
	bundle := json.RawMessage(`{"cart.title":"Panier","cart.empty":"Votre panier est vide"}`)
	created, err := repo.CreateLocalization(ctx, localization.Localization{
		MicroserviceID: 2, EnvironmentID: 1, Locale: "fr-FR", BundleJSON: bundle,
	})
	if err != nil {
		t.Fatalf("create localization: %v", err)
	}
	if created.ID == 0 || string(created.BundleJSON) != string(bundle) {
		t.Fatalf("unexpected created row: %+v", created)
	}

	read, err := repo.GetLocalizationByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("read back by id: %v", err)
	}
	if string(read.BundleJSON) != string(bundle) {
		t.Fatalf("read back %q, wrote %q", read.BundleJSON, bundle)
	}

	// The lookup endpoint reads by the natural key rather than by id, so it is a
	// separate statement with its own three binds.
	byKey, err := repo.GetLocalization(ctx, 1, 1, "pt-BR")
	if err != nil {
		t.Fatalf("lookup by natural key: %v", err)
	}
	if byKey.ID != 301 {
		t.Fatalf("lookup returned row %d, want the seeded 301", byKey.ID)
	}
	if _, err := repo.GetLocalization(ctx, 1, 1, "de-DE"); !errors.Is(err, localization.ErrLocalizationNotFound) {
		t.Fatalf("lookup of a missing locale: expected ErrLocalizationNotFound, got %v", err)
	}
	if _, err := repo.GetLocalizationByID(ctx, 999999); !errors.Is(err, localization.ErrLocalizationNotFound) {
		t.Fatalf("read of a missing row: expected ErrLocalizationNotFound, got %v", err)
	}

	if _, err := repo.CreateLocalization(ctx, localization.Localization{
		MicroserviceID: 2, EnvironmentID: 1, Locale: "fr-FR", BundleJSON: bundle,
	}); !errors.Is(err, localization.ErrLocalizationExists) {
		t.Fatalf("duplicate natural key: expected ErrLocalizationExists, got %v", err)
	}
	if _, err := repo.CreateLocalization(ctx, localization.Localization{
		MicroserviceID: 999, EnvironmentID: 1, Locale: "fr-FR", BundleJSON: bundle,
	}); !errors.Is(err, localization.ErrMicroserviceNotFound) {
		t.Fatalf("missing microservice: expected ErrMicroserviceNotFound, got %v", err)
	}
	if _, err := repo.CreateLocalization(ctx, localization.Localization{
		MicroserviceID: 2, EnvironmentID: 999, Locale: "fr-FR", BundleJSON: bundle,
	}); !errors.Is(err, localization.ErrEnvironmentNotFound) {
		t.Fatalf("missing environment: expected ErrEnvironmentNotFound, got %v", err)
	}

	all, err := repo.ListLocalizations(ctx, localization.Filter{Page: localization.Page{Limit: 100}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 3 { // two seeded plus the one created above
		t.Fatalf("expected 3 rows, got %d", len(all))
	}

	// All three filters at once, so LOCALE binds $3 and the LIMIT/OFFSET $4/$5 —
	// this domain has the longest hand-numbered bind list in the codebase and is
	// the likeliest place for an off-by-one to hide.
	narrowed, err := repo.ListLocalizations(ctx, localization.Filter{
		Page: localization.Page{Limit: 100}, MicroserviceID: 1, EnvironmentID: 1, Locale: "pt-BR",
	})
	if err != nil {
		t.Fatalf("list narrowed: %v", err)
	}
	if len(narrowed) != 1 || narrowed[0].ID != 301 {
		t.Fatalf("unexpected narrowed rows: %+v", narrowed)
	}

	byService, err := repo.ListLocalizations(ctx, localization.Filter{
		Page: localization.Page{Limit: 100}, MicroserviceID: 1,
	})
	if err != nil {
		t.Fatalf("list by microservice: %v", err)
	}
	if len(byService) != 2 {
		t.Fatalf("expected 2 rows for microservice 1, got %d", len(byService))
	}

	page, err := repo.ListLocalizations(ctx, localization.Filter{Page: localization.Page{Limit: 1, Offset: 1}})
	if err != nil {
		t.Fatalf("list page: %v", err)
	}
	if len(page) != 1 || page[0].ID != all[1].ID {
		t.Fatalf("offset page did not line up: %+v", page)
	}

	removed, err := repo.DeleteLocalization(ctx, created.ID)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if removed.Locale != "fr-FR" || removed.MicroserviceID != 2 || removed.EnvironmentID != 1 {
		t.Fatalf("delete reported the wrong KV coordinates: %+v", removed)
	}
	if _, err := repo.DeleteLocalization(ctx, created.ID); !errors.Is(err, localization.ErrLocalizationNotFound) {
		t.Fatalf("second delete: expected ErrLocalizationNotFound, got %v", err)
	}
}

func TestLocalizationGuardedUpdate(t *testing.T) {
	repo, _ := localizationRepo(t)
	ctx := context.Background()

	current, err := repo.GetLocalizationByID(ctx, 300)
	if err != nil {
		t.Fatalf("read current: %v", err)
	}

	expected := current.UpdatedAt
	updated, err := repo.UpdateLocalization(ctx, localization.Localization{
		ID: 300, MicroserviceID: 1, EnvironmentID: 1, Locale: "en-US",
		BundleJSON:        json.RawMessage(`{"catalog.title":"Catalogue"}`),
		ExpectedUpdatedAt: &expected,
	})
	if err != nil {
		t.Fatalf("guarded update with the current timestamp: %v", err)
	}
	if string(updated.BundleJSON) != `{"catalog.title":"Catalogue"}` {
		t.Fatalf("unexpected row after update: %+v", updated)
	}

	stale := expected
	if _, err := repo.UpdateLocalization(ctx, localization.Localization{
		ID: 300, MicroserviceID: 1, EnvironmentID: 1, Locale: "en-US",
		BundleJSON:        json.RawMessage(`{"catalog.title":"Loser"}`),
		ExpectedUpdatedAt: &stale,
	}); !errors.Is(err, localization.ErrConflict) {
		t.Fatalf("stale guard: expected ErrConflict, got %v", err)
	}

	if _, err := repo.UpdateLocalization(ctx, localization.Localization{
		ID: 999999, MicroserviceID: 1, EnvironmentID: 1, Locale: "es-ES",
		BundleJSON:        json.RawMessage(`{}`),
		ExpectedUpdatedAt: &stale,
	}); !errors.Is(err, localization.ErrLocalizationNotFound) {
		t.Fatalf("guarded update of a missing row: expected ErrLocalizationNotFound, got %v", err)
	}

	// Moving row 300 onto row 301's (microservice, environment, locale) is a
	// conflict, not a constraint violation surfacing as a 500.
	if _, err := repo.UpdateLocalization(ctx, localization.Localization{
		ID: 300, MicroserviceID: 1, EnvironmentID: 1, Locale: "pt-BR",
		BundleJSON: json.RawMessage(`{}`),
	}); !errors.Is(err, localization.ErrLocalizationExists) {
		t.Fatalf("update onto another row's key: expected ErrLocalizationExists, got %v", err)
	}
}

func TestLocalizationCreateLosingARaceIsAConflict(t *testing.T) {
	repo, s := localizationRepo(t)
	ctx := context.Background()

	// (microservice 1, environment 1, es-ES) is free, so the pre-check passes.
	s.installRacingWriter(t, "winner_localization", "CONFIG_LOCALIZATION",
		"NEW.MICROSERVICE_ID = 1 AND NEW.ENVIRONMENT_ID = 1 AND NEW.LOCALE = 'es-ES'",
		`INSERT INTO CONFIG_LOCALIZATION (MICROSERVICE_ID, ENVIRONMENT_ID, LOCALE, BUNDLE_JSON) VALUES (1, 1, 'es-ES', '{"theirs":"si"}')`)

	if _, err := repo.CreateLocalization(ctx, localization.Localization{
		MicroserviceID: 1, EnvironmentID: 1, Locale: "es-ES",
		BundleJSON: json.RawMessage(`{"mine":"si"}`),
	}); !errors.Is(err, localization.ErrLocalizationExists) {
		t.Fatalf("lost create of a bundle: expected ErrLocalizationExists, got %v", err)
	}
}
