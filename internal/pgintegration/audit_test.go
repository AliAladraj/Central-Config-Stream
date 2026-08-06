package pgintegration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ErasedKyte/Central-Config-Stream/internal/audit"
)

// auditBase is the instant the fixtures hang off. Microsecond precision because
// that is what a TIMESTAMPTZ column stores — comparing against a time.Now()
// with nanoseconds would fail on the rounding rather than on anything real.
var auditBase = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func auditFixtures() []audit.Entry {
	return []audit.Entry{
		{
			OccurredAt: auditBase, Actor: "alice", Method: "PUT", Path: "/flagvalues/100",
			Domain: "flagvalues", TargetID: "100", EnvironmentID: 1, StatusCode: 200,
			RemoteAddr: "10.0.0.1", RequestBody: `{"value":"on"}`,
		},
		{
			OccurredAt: auditBase.Add(time.Minute), Actor: "bob", Method: "POST", Path: "/configs",
			Domain: "configs", TargetID: "204", EnvironmentID: 2, StatusCode: 201,
			RemoteAddr: "10.0.0.2", RequestBody: `{"settingsJson":{}}`,
		},
		{
			OccurredAt: auditBase.Add(2 * time.Minute), Actor: "alice", Method: "DELETE", Path: "/localization/301",
			Domain: "localization", TargetID: "301", EnvironmentID: 3, StatusCode: 204,
			RemoteAddr: "10.0.0.1",
		},
		// A rejected write: nothing is known about its target, so ACTOR,
		// TARGET_DOMAIN, TARGET_ID and ENVIRONMENT_ID all go in as NULL. This row
		// is why the columns are nullable and why the scoped read below has to
		// exclude it — NULL never satisfies IN.
		{
			OccurredAt: auditBase.Add(3 * time.Minute), Method: "POST", Path: "/flags",
			StatusCode: 401, RemoteAddr: "10.0.0.9",
		},
	}
}

func seedAudit(t *testing.T, store *audit.Store) {
	t.Helper()
	ctx := context.Background()
	for i, entry := range auditFixtures() {
		if err := store.Insert(ctx, entry); err != nil {
			t.Fatalf("insert audit fixture %d: %v", i, err)
		}
	}
}

// The audit table is the only place that remembers who flipped a production
// flag, and its store is the one persistence type with a single implementation
// switched by a dialect flag rather than two files — so the Postgres branch of
// every bind() and timeArg() call is reachable only with a Postgres session
// behind it.
//
// The whole test runs under each session time zone: OCCURRED_AT is TIMESTAMPTZ
// and the range filter binds a time.Time straight through, which is exactly the
// comparison a naive column would get wrong by the session's offset. An audit
// range that silently returns the wrong day is worse than one that errors.
func TestAuditStoreInsertAndList(t *testing.T) {
	dsnOrSkip(t) // so the parent skips too, rather than passing on skipped subtests
	for _, zone := range nonUTCZones {
		t.Run(zone, func(t *testing.T) {
			s := newStack(t, withTimeZone(zone))
			store := audit.NewStore(s.DB, "postgres")
			ctx := context.Background()

			seedAudit(t, store)

			all, err := store.List(ctx, audit.Filter{Limit: 100})
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(all) != 4 {
				t.Fatalf("expected 4 entries, got %d", len(all))
			}

			// Most recent first: an audit reader is almost always asking what
			// just happened.
			for i := 1; i < len(all); i++ {
				if all[i].ID >= all[i-1].ID {
					t.Fatalf("entries are not newest-first: %d after %d", all[i].ID, all[i-1].ID)
				}
			}

			// The envelope row's NULL columns have to arrive as zero values and
			// not as a scan failure.
			envelope := all[0]
			if envelope.StatusCode != 401 || envelope.Actor != "" || envelope.Domain != "" ||
				envelope.TargetID != "" || envelope.EnvironmentID != 0 {
				t.Fatalf("the rejected-write envelope did not round-trip: %+v", envelope)
			}
			if !envelope.OccurredAt.Equal(auditBase.Add(3 * time.Minute)) {
				t.Fatalf("OCCURRED_AT round-tripped as %s, want %s", envelope.OccurredAt, auditBase.Add(3*time.Minute))
			}

			byActor, err := store.List(ctx, audit.Filter{Actor: "alice", Limit: 100})
			if err != nil {
				t.Fatalf("list by actor: %v", err)
			}
			if len(byActor) != 2 {
				t.Fatalf("expected 2 entries for alice, got %d", len(byActor))
			}

			// A closed range over the two middle rows. Both bounds are
			// inclusive, so the boundaries are chosen to land exactly on the
			// stored instants — an off-by-an-offset comparison cannot pass this
			// by accident.
			inRange, err := store.List(ctx, audit.Filter{
				From:  auditBase.Add(time.Minute),
				To:    auditBase.Add(2 * time.Minute),
				Limit: 100,
			})
			if err != nil {
				t.Fatalf("list by range: %v", err)
			}
			if len(inRange) != 2 {
				t.Fatalf("expected 2 entries in the range, got %d (%+v)", len(inRange), inRange)
			}
			for _, entry := range inRange {
				if entry.Actor != "alice" && entry.Actor != "bob" {
					t.Fatalf("range returned an entry outside it: %+v", entry)
				}
			}

			// An open-ended range: everything from the second row on, including
			// the envelope, which carries no environment but does carry a time.
			openEnded, err := store.List(ctx, audit.Filter{From: auditBase.Add(time.Minute), Limit: 100})
			if err != nil {
				t.Fatalf("list open-ended: %v", err)
			}
			if len(openEnded) != 3 {
				t.Fatalf("expected 3 entries from the second on, got %d", len(openEnded))
			}

			// A reader's scope narrows the listing. The envelope has no
			// environment, so it is outside every narrowed scope — its body is
			// the one thing about it that is not already known.
			scoped, err := store.List(ctx, audit.Filter{Environments: []int64{1, 3}, Limit: 100})
			if err != nil {
				t.Fatalf("list scoped: %v", err)
			}
			if len(scoped) != 2 {
				t.Fatalf("expected 2 entries in environments 1 and 3, got %d (%+v)", len(scoped), scoped)
			}
			for _, entry := range scoped {
				if entry.EnvironmentID != 1 && entry.EnvironmentID != 3 {
					t.Fatalf("scoped listing leaked environment %d: %+v", entry.EnvironmentID, entry)
				}
			}

			// Every filter at once, so ACTOR binds $1, the range $2 and $3, and
			// the scope $4 onwards ahead of the LIMIT/OFFSET.
			combined, err := store.List(ctx, audit.Filter{
				Actor:        "alice",
				From:         auditBase,
				To:           auditBase.Add(2 * time.Minute),
				Environments: []int64{3},
				Limit:        100,
			})
			if err != nil {
				t.Fatalf("list combined: %v", err)
			}
			if len(combined) != 1 || combined[0].TargetID != "301" {
				t.Fatalf("unexpected combined result: %+v", combined)
			}

			// An empty scope is a token scoped to no environment at all, and it
			// must see nothing rather than everything.
			empty, err := store.List(ctx, audit.Filter{Environments: []int64{}, Limit: 100})
			if err != nil {
				t.Fatalf("list empty scope: %v", err)
			}
			if len(empty) != 0 {
				t.Fatalf("an empty scope returned %d entries", len(empty))
			}

			page, err := store.List(ctx, audit.Filter{Limit: 2, Offset: 2})
			if err != nil {
				t.Fatalf("list page: %v", err)
			}
			if len(page) != 2 || page[0].ID != all[2].ID || page[1].ID != all[3].ID {
				t.Fatalf("offset page did not line up: %+v", page)
			}
		})
	}
}

// The audit insert is on the request path: a body the column rejects is a failed
// INSERT, not a shortened row. audit.MaxStoredBody is a byte budget, while
// REQUEST_BODY's VARCHAR(4000) counts characters, so the budget is deliberately
// conservative — and that is worth pinning at both ends of the encoding, since
// the two units sit furthest apart for a multi-byte body and coincide for an
// ASCII one. The column has to take the row either way.
func TestAuditStoreAcceptsABodyAtTheStoredCap(t *testing.T) {
	s := newStack(t)
	store := audit.NewStore(s.DB, "postgres")
	ctx := context.Background()

	cases := []struct {
		name string
		body string
	}{
		{"ascii at the byte cap", strings.Repeat("a", audit.MaxStoredBody)},
		{"multi-byte at the byte cap", strings.Repeat("é", audit.MaxStoredBody/2)},
	}

	for _, tc := range cases {
		entry := audit.Entry{
			OccurredAt: auditBase, Actor: "alice", Method: "PUT", Path: "/configs/200",
			Domain: "configs", TargetID: "200", EnvironmentID: 1, StatusCode: 200,
			RequestBody: tc.body,
		}
		if err := store.Insert(ctx, entry); err != nil {
			t.Fatalf("%s (%d bytes, %d runes): %v", tc.name, len(tc.body), len([]rune(tc.body)), err)
		}
	}

	// Newest first, so the listing is the reverse of the order they went in.
	stored, err := store.List(ctx, audit.Filter{Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(stored) != len(cases) {
		t.Fatalf("expected %d entries, got %d", len(cases), len(stored))
	}
	for i, tc := range cases {
		got := stored[len(stored)-1-i].RequestBody
		if got != tc.body {
			t.Errorf("%s: stored body came back as %d bytes, wrote %d", tc.name, len(got), len(tc.body))
		}
	}
}
