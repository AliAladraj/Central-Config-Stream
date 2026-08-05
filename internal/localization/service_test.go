package localization_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
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
