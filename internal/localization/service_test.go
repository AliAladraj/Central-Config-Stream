package localization_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ErasedKyte/Central-Config-Stream/internal/database"
	"github.com/ErasedKyte/Central-Config-Stream/internal/localization"
	"github.com/ErasedKyte/Central-Config-Stream/internal/messaging"
)

// The service is exercised against the SQLite repository rather than a stub:
// referential integrity is enforced by the repository, so a stub would test the
// checks by restating them.
func newTestService(t *testing.T) (*localization.Service, *capturePub) {
	t.Helper()
	db, err := database.NewSQLiteDB("file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "t.db")))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	pub := &capturePub{}
	return localization.NewService(localization.NewSQLiteRepository(db), pub), pub
}

type capturePub struct {
	messaging.NoopPublisher
	published []string
	deleted   []string
}

func (c *capturePub) PublishLocalization(ctx context.Context, env, ms int64, locale string, bundle json.RawMessage) error {
	c.published = append(c.published, messaging.LocalizationKey(env, ms, locale))
	return nil
}

func (c *capturePub) DeleteKey(ctx context.Context, bucket, key string) error {
	c.deleted = append(c.deleted, bucket+"|"+key)
	return nil
}

func TestCreateLocalizationPublishes(t *testing.T) {
	svc, pub := newTestService(t)
	ctx := context.Background()

	created, err := svc.CreateLocalization(ctx, localization.Localization{
		MicroserviceID: 2, EnvironmentID: 1, Locale: "en-US",
		BundleJSON: json.RawMessage(`{"cart.title":"Cart"}`),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == 0 {
		t.Fatalf("create returned no id: %+v", created)
	}
	if len(pub.published) != 1 || pub.published[0] != "1.2.en-US" {
		t.Fatalf("create did not write through to KV: %v", pub.published)
	}

	if _, err := svc.CreateLocalization(ctx, localization.Localization{
		MicroserviceID: 2, EnvironmentID: 1, Locale: "en-US", BundleJSON: json.RawMessage(`{}`),
	}); !errors.Is(err, localization.ErrLocalizationExists) {
		t.Fatalf("expected ErrLocalizationExists, got %v", err)
	}
	if _, err := svc.CreateLocalization(ctx, localization.Localization{
		MicroserviceID: 999, EnvironmentID: 1, Locale: "fr-FR", BundleJSON: json.RawMessage(`{}`),
	}); !errors.Is(err, localization.ErrMicroserviceNotFound) {
		t.Fatalf("expected ErrMicroserviceNotFound, got %v", err)
	}
	if _, err := svc.CreateLocalization(ctx, localization.Localization{
		MicroserviceID: 2, EnvironmentID: 999, Locale: "fr-FR", BundleJSON: json.RawMessage(`{}`),
	}); !errors.Is(err, localization.ErrEnvironmentNotFound) {
		t.Fatalf("expected ErrEnvironmentNotFound, got %v", err)
	}
}

// A bundle that is not a JSON object breaks every consumer's binding at once,
// and a locale that is not KV-key-safe would make "{env}.{ms}.{locale}"
// ambiguous. Both are refused at write time.
func TestCreateLocalizationRejectsBadPayload(t *testing.T) {
	svc, pub := newTestService(t)
	ctx := context.Background()

	for _, bundle := range []string{`["a","b"]`, `"a string"`, `7`, `{"broken":`} {
		_, err := svc.CreateLocalization(ctx, localization.Localization{
			MicroserviceID: 2, EnvironmentID: 1, Locale: "fr-FR", BundleJSON: json.RawMessage(bundle),
		})
		if !errors.Is(err, localization.ErrInvalidBundleJSON) {
			t.Errorf("bundle %s: expected ErrInvalidBundleJSON, got %v", bundle, err)
		}
	}

	for _, locale := range []string{"", "pt.BR", "pt BR"} {
		_, err := svc.CreateLocalization(ctx, localization.Localization{
			MicroserviceID: 2, EnvironmentID: 1, Locale: locale, BundleJSON: json.RawMessage(`{}`),
		})
		if !errors.Is(err, localization.ErrInvalidLocale) {
			t.Errorf("locale %q: expected ErrInvalidLocale, got %v", locale, err)
		}
	}

	if len(pub.published) != 0 {
		t.Fatalf("a rejected payload still reached KV: %v", pub.published)
	}
}

func TestDeleteLocalizationPurgesKey(t *testing.T) {
	svc, pub := newTestService(t)
	ctx := context.Background()

	// Seed row 301 is microservice 1, environment 1, locale pt-BR.
	if err := svc.DeleteLocalization(ctx, 301); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(pub.deleted) != 1 || pub.deleted[0] != "LOCALIZATION|1.1.pt-BR" {
		t.Fatalf("delete did not purge the KV key: %v", pub.deleted)
	}
	if _, err := svc.GetLocalizationByID(ctx, 301); !errors.Is(err, localization.ErrLocalizationNotFound) {
		t.Fatalf("expected ErrLocalizationNotFound after delete, got %v", err)
	}
	if err := svc.DeleteLocalization(ctx, 301); !errors.Is(err, localization.ErrLocalizationNotFound) {
		t.Fatalf("second delete: expected ErrLocalizationNotFound, got %v", err)
	}
}

func TestListLocalizationsFilters(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	all, err := svc.ListLocalizations(ctx, localization.Filter{Page: localization.Page{Limit: 100}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 { // seeded bundles
		t.Fatalf("expected 2 seeded rows, got %d", len(all))
	}

	byLocale, err := svc.ListLocalizations(ctx, localization.Filter{
		Page: localization.Page{Limit: 100}, Locale: "pt-BR",
	})
	if err != nil {
		t.Fatalf("list by locale: %v", err)
	}
	if len(byLocale) != 1 || byLocale[0].Locale != "pt-BR" {
		t.Fatalf("unexpected rows for pt-BR: %+v", byLocale)
	}

	none, err := svc.ListLocalizations(ctx, localization.Filter{
		Page: localization.Page{Limit: 100}, EnvironmentID: 3,
	})
	if err != nil {
		t.Fatalf("list by environment: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("expected no rows in environment 3, got %d", len(none))
	}
}

// The create path refused a locale that is not KV-key-safe while the update
// path checked only that it was non-empty, so the same body was a 400 as a POST
// and a 200 as a PUT — writing a key consumers cannot split on the separator,
// which every reconcile sweep then republishes.
func TestUpdateLocalizationValidatesTheLocale(t *testing.T) {
	svc, pub := newTestService(t)
	ctx := context.Background()

	// Seed row 301 is microservice 1, environment 1, locale pt-BR.
	for _, locale := range []string{"", "pt.BR", "pt BR", "pt/BR"} {
		_, err := svc.UpdateLocalization(ctx, localization.Localization{
			ID: 301, MicroserviceID: 1, EnvironmentID: 1, Locale: locale,
			BundleJSON: json.RawMessage(`{"a":"b"}`),
		})
		if !errors.Is(err, localization.ErrInvalidLocale) {
			t.Errorf("update with locale %q: expected ErrInvalidLocale, got %v", locale, err)
		}
	}
	if len(pub.published) != 0 {
		t.Fatalf("a rejected locale still reached KV: %v", pub.published)
	}

	// The same locale the create path accepts still updates.
	if _, err := svc.UpdateLocalization(ctx, localization.Localization{
		ID: 301, MicroserviceID: 1, EnvironmentID: 1, Locale: "pt-BR",
		BundleJSON: json.RawMessage(`{"a":"b"}`),
	}); err != nil {
		t.Fatalf("update with a valid locale: %v", err)
	}
}

// The update path rewrites the whole natural key, so it needs the referential
// and uniqueness checks the create path runs — otherwise it points rows at
// parents that do not exist, or collides and surfaces as a 500.
func TestUpdateLocalizationChecksRefsAndUniqueness(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	if _, err := svc.UpdateLocalization(ctx, localization.Localization{
		ID: 301, MicroserviceID: 999, EnvironmentID: 1, Locale: "pt-BR",
		BundleJSON: json.RawMessage(`{}`),
	}); !errors.Is(err, localization.ErrMicroserviceNotFound) {
		t.Errorf("expected ErrMicroserviceNotFound, got %v", err)
	}
	if _, err := svc.UpdateLocalization(ctx, localization.Localization{
		ID: 301, MicroserviceID: 1, EnvironmentID: 999, Locale: "pt-BR",
		BundleJSON: json.RawMessage(`{}`),
	}); !errors.Is(err, localization.ErrEnvironmentNotFound) {
		t.Errorf("expected ErrEnvironmentNotFound, got %v", err)
	}

	// Row 300 already holds (1, 1, en-US); moving 301 onto it is a 409, not a
	// constraint violation surfacing as a 500.
	if _, err := svc.UpdateLocalization(ctx, localization.Localization{
		ID: 301, MicroserviceID: 1, EnvironmentID: 1, Locale: "en-US",
		BundleJSON: json.RawMessage(`{}`),
	}); !errors.Is(err, localization.ErrLocalizationExists) {
		t.Errorf("expected ErrLocalizationExists, got %v", err)
	}

	// A row keeping the key it already has is not a collision with itself.
	if _, err := svc.UpdateLocalization(ctx, localization.Localization{
		ID: 301, MicroserviceID: 1, EnvironmentID: 1, Locale: "pt-BR",
		BundleJSON: json.RawMessage(`{"a":"b"}`),
	}); err != nil {
		t.Errorf("update in place: %v", err)
	}
}

// Moving a row to another identity publishes a new KV key. The old one has no
// row behind it any more, so leaving it there means consumers of the old
// identity keep serving a bundle that no longer exists.
func TestUpdateLocalizationPurgesTheKeyItMovedAwayFrom(t *testing.T) {
	svc, pub := newTestService(t)
	ctx := context.Background()

	// Seed row 301 is (microservice 1, environment 1, pt-BR).
	if _, err := svc.UpdateLocalization(ctx, localization.Localization{
		ID: 301, MicroserviceID: 2, EnvironmentID: 3, Locale: "fr-FR",
		BundleJSON: json.RawMessage(`{"a":"b"}`),
	}); err != nil {
		t.Fatalf("move: %v", err)
	}

	if len(pub.published) != 1 || pub.published[0] != "3.2.fr-FR" {
		t.Fatalf("the new key was not published: %v", pub.published)
	}
	if len(pub.deleted) != 1 || pub.deleted[0] != "LOCALIZATION|1.1.pt-BR" {
		t.Fatalf("the key the row moved away from was not purged: %v", pub.deleted)
	}
}

// An update that keeps the same identity has no old key to purge.
func TestUpdateLocalizationInPlacePurgesNothing(t *testing.T) {
	svc, pub := newTestService(t)

	if _, err := svc.UpdateLocalization(context.Background(), localization.Localization{
		ID: 301, MicroserviceID: 1, EnvironmentID: 1, Locale: "pt-BR",
		BundleJSON: json.RawMessage(`{"a":"b"}`),
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(pub.deleted) != 0 {
		t.Fatalf("an in-place update purged a key: %v", pub.deleted)
	}
}

// A bundle bigger than a KV value may be used to be stored and answered with a
// 201, then fail at publish on every sweep from then on — the drift that
// wedged a whole domain's convergence. It is a validation error now, on both
// write paths, and the row is never created.
func TestOversizedBundleIsRejected(t *testing.T) {
	svc, pub := newTestService(t)
	ctx := context.Background()

	oversized := json.RawMessage(`{"k":"` + strings.Repeat("x", messaging.MaxValueSize) + `"}`)

	_, err := svc.CreateLocalization(ctx, localization.Localization{
		MicroserviceID: 2, EnvironmentID: 1, Locale: "en-US", BundleJSON: oversized,
	})
	if !errors.Is(err, localization.ErrBundleTooLarge) {
		t.Fatalf("create: expected ErrBundleTooLarge, got %v", err)
	}
	if _, err := svc.GetLocalization(ctx, 2, 1, "en-US"); !errors.Is(err, localization.ErrLocalizationNotFound) {
		t.Fatalf("an oversized create still wrote a row: %v", err)
	}

	_, err = svc.UpdateLocalization(ctx, localization.Localization{
		ID: 301, MicroserviceID: 1, EnvironmentID: 1, Locale: "pt-BR", BundleJSON: oversized,
	})
	if !errors.Is(err, localization.ErrBundleTooLarge) {
		t.Fatalf("update: expected ErrBundleTooLarge, got %v", err)
	}
	if len(pub.published) != 0 {
		t.Fatalf("an oversized bundle reached KV: %v", pub.published)
	}

	// A bundle right up against the ceiling is still accepted.
	fitting := json.RawMessage(`{"k":"` + strings.Repeat("x", messaging.MaxValueSize-16) + `"}`)
	if _, err := svc.CreateLocalization(ctx, localization.Localization{
		MicroserviceID: 2, EnvironmentID: 1, Locale: "en-US", BundleJSON: fitting,
	}); err != nil {
		t.Fatalf("a bundle inside the ceiling was rejected: %v", err)
	}
}
