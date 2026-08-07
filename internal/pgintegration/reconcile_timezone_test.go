package pgintegration

import (
	"context"
	"testing"
	"time"

	"github.com/AliAladraj/Central-Config-Stream/internal/flagsconfig"
	"github.com/AliAladraj/Central-Config-Stream/internal/localization"
	"github.com/AliAladraj/Central-Config-Stream/internal/servicesettings"
)

// nonUTCZones are the session time zones the reconcile window is exercised
// under. Both are fixed-offset all year, so the test cannot drift with a DST
// boundary, and they sit either side of UTC by a lot:
//
//   - Pacific/Kiritimati is UTC+14. If UPDATED_AT were a naive TIMESTAMP,
//     CURRENT_TIMESTAMP would write a wall clock fourteen hours ahead of the
//     instant the reconciler binds, so a window set an hour in the *future*
//     would still match every row — the sweep republishes the whole table on
//     every cycle and nobody notices, because the symptom is load rather than
//     an error.
//   - Pacific/Pago_Pago is UTC-11, which fails the other way: rows written
//     eleven hours behind the bound instant fall out of every recent window, so
//     the incremental sweep silently stops seeing changes and drift never heals.
//
// One zone would catch one sign of the error. Both catch either.
var nonUTCZones = []string{"UTC", "Pacific/Kiritimati", "Pacific/Pago_Pago"}

// backdatePastRow is the instant every seeded row is pushed back to, so the only
// row inside a recent window is the one a test just changed. Written with an
// explicit offset because a bare literal would be read in the session's zone,
// which is the very thing under test.
const backdatePastRow = `'2000-01-01T00:00:00+00'::timestamptz`

func backdateSeededRows(t *testing.T, s *stack) {
	t.Helper()
	for _, table := range []string{"CONFIG_FLAG_VALUE", "CONFIG_MICROSERVICE_APPSETTINGS", "CONFIG_LOCALIZATION"} {
		if _, err := s.DB.Exec(`UPDATE ` + table + ` SET UPDATED_AT = ` + backdatePastRow); err != nil {
			t.Fatalf("backdate %s: %v", table, err)
		}
	}
}

// The incremental reconcile window is the reason UPDATED_AT is TIMESTAMPTZ, and
// it is the query whose failure is silent: a broken window means configuration
// that quietly stops converging, not a failed request. TIMESTAMPTZ stores an
// instant, so the window is a plain comparison against a bound time.Time. A
// timezone-naive column would need that bind converted through the session's
// zone to mean anything, and without the conversion would silently match the
// wrong rows whenever the session is not UTC — which is what this test pins.
//
// It runs the whole thing on a session whose TimeZone is not UTC, because that
// is the only configuration in which that failure is visible at all. A CI
// runner is UTC, a developer's Docker container is UTC, and a production
// database is whatever the DBA set — so a suite that only ever ran in UTC would
// go green against a naive column too.
func TestReconcileWindowIgnoresTheSessionTimeZone(t *testing.T) {
	dsnOrSkip(t) // so the parent skips too, rather than passing on skipped subtests
	for _, zone := range nonUTCZones {
		t.Run(zone, func(t *testing.T) {
			s := newStack(t, withTimeZone(zone))
			ctx := context.Background()

			// Confirm the session really is in the zone the test thinks it is:
			// a typo in the DSN parameter would otherwise leave every case
			// running in UTC and passing for the wrong reason.
			var actual string
			if err := s.DB.QueryRow(`SHOW TimeZone`).Scan(&actual); err != nil {
				t.Fatalf("read the session TimeZone: %v", err)
			}
			if actual != zone {
				t.Fatalf("session TimeZone is %q, want %q — the DSN parameter did not take", actual, zone)
			}

			flags := flagsconfig.NewPostgresRepository(s.DB)
			micro := servicesettings.NewPostgresRepository(s.DB)
			local := localization.NewPostgresRepository(s.DB)

			// A zero `since` is the full sweep: every row, no window at all.
			assertReconcileCounts(t, ctx, flags, micro, local, time.Time{}, 4, 3, 2)

			backdateSeededRows(t, s)

			// A cutoff in the future matches nothing once every row is old.
			assertReconcileCounts(t, ctx, flags, micro, local, time.Now().Add(time.Hour), 0, 0, 0)

			// One row per domain is touched through the repository, so each
			// UPDATED_AT is written by CURRENT_TIMESTAMP exactly as a live admin
			// write would write it.
			if _, _, err := flags.UpdateFlagValue(ctx, flagsconfig.FlagValue{ID: 100, Value: "canary", Enabled: 1}); err != nil {
				t.Fatalf("update flag value: %v", err)
			}
			if _, err := micro.UpdateMicroserviceConfig(ctx, servicesettings.MicroserviceAppSettings{
				ID: 200, MicroserviceID: 1, EnvironmentID: 1, SettingsJSON: []byte(`{"log":{"level":"warn"}}`),
			}); err != nil {
				t.Fatalf("update appsettings: %v", err)
			}
			if _, err := local.UpdateLocalization(ctx, localization.Localization{
				ID: 300, MicroserviceID: 1, EnvironmentID: 1, Locale: "en-US", BundleJSON: []byte(`{"catalog.title":"Catalogue"}`),
			}); err != nil {
				t.Fatalf("update localization: %v", err)
			}

			// A window just behind now matches exactly what was touched. This is
			// the half a negative-offset session breaks: a row written eleven
			// hours behind the bound instant falls out of every recent window,
			// so the incremental sweep stops seeing changes at all.
			assertReconcileCounts(t, ctx, flags, micro, local, time.Now().Add(-time.Minute), 1, 1, 1)

			// A cutoff an hour ahead still matches nothing, now that rows have
			// been written by CURRENT_TIMESTAMP rather than backdated by hand.
			// This is the half a positive-offset session breaks: a row written
			// in local wall clock looks fourteen hours newer than it is, so it
			// stays inside every window forever and the sweep republishes it on
			// every cycle.
			assertReconcileCounts(t, ctx, flags, micro, local, time.Now().Add(time.Hour), 0, 0, 0)

			// And the row it matched is the one that changed, not merely a row.
			changed, err := flags.ListAllForReconcile(ctx, time.Now().Add(-time.Minute))
			if err != nil {
				t.Fatalf("list flags for reconcile: %v", err)
			}
			if len(changed) != 1 {
				t.Fatalf("the window matched %d rows, want only the one that changed: %+v", len(changed), changed)
			}
			if changed[0].ID != 100 || changed[0].FlagKey != "search_v2" || changed[0].Value != "canary" {
				t.Fatalf("the window matched the wrong row: %+v", changed[0])
			}
		})
	}
}

func assertReconcileCounts(
	t *testing.T,
	ctx context.Context,
	flags *flagsconfig.PostgresRepository,
	micro *servicesettings.PostgresRepository,
	local *localization.PostgresRepository,
	since time.Time,
	wantFlags, wantAppSettings, wantLocalization int,
) {
	t.Helper()

	flagRows, err := flags.ListAllForReconcile(ctx, since)
	if err != nil {
		t.Fatalf("list flags for reconcile: %v", err)
	}
	if len(flagRows) != wantFlags {
		t.Errorf("flag values in the window since %s: got %d, want %d", since, len(flagRows), wantFlags)
	}

	appSettingsRows, err := micro.ListAllForReconcile(ctx, since)
	if err != nil {
		t.Fatalf("list appsettings for reconcile: %v", err)
	}
	if len(appSettingsRows) != wantAppSettings {
		t.Errorf("appsettings in the window since %s: got %d, want %d", since, len(appSettingsRows), wantAppSettings)
	}

	localizationRows, err := local.ListAllForReconcile(ctx, since)
	if err != nil {
		t.Fatalf("list localization for reconcile: %v", err)
	}
	if len(localizationRows) != wantLocalization {
		t.Errorf("localization bundles in the window since %s: got %d, want %d", since, len(localizationRows), wantLocalization)
	}
}

// The optimistic-concurrency guard compares a timestamp the client read back
// against the one the column holds, so it depends on the same instant surviving
// a round trip through the driver. On a non-UTC session a timestamp that lost
// its offset anywhere in that trip would make every guarded update a 409 that
// no retry can clear — the client re-reads, sends the same value, and is
// refused again.
func TestGuardedUpdateSurvivesANonUTCSession(t *testing.T) {
	dsnOrSkip(t) // so the parent skips too, rather than passing on skipped subtests
	for _, zone := range nonUTCZones {
		t.Run(zone, func(t *testing.T) {
			s := newStack(t, withTimeZone(zone))
			ctx := context.Background()
			repo := flagsconfig.NewPostgresRepository(s.DB)

			current, err := repo.GetFlagValueByID(ctx, 100)
			if err != nil {
				t.Fatalf("read current: %v", err)
			}

			expected := current.UpdatedAt
			if _, _, err := repo.UpdateFlagValue(ctx, flagsconfig.FlagValue{
				ID: 100, Value: "canary", Enabled: 1, ExpectedUpdatedAt: &expected,
			}); err != nil {
				t.Fatalf("guarded update on a %s session: %v", zone, err)
			}
		})
	}
}
