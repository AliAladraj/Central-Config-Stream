package pgintegration

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ErasedKyte/Central-Config-Stream/internal/localization"
	"github.com/ErasedKyte/Central-Config-Stream/internal/microconfig"
)

// documentSize is the size of the settings tree and translation bundle the
// round-trip tests write. Real appsettings trees reach this; the seeded one is
// a few hundred bytes and proves nothing about a column's behaviour under a
// document. It is comfortably under messaging.MaxValueSize (512 KiB), which is
// what the services check before the write — the point here is the storage
// layer, not the size limit.
const documentSize = 64 << 10

// awkwardDocument builds a JSON document of at least documentSize bytes that is
// deliberately not in any canonical form: the keys are in an order no sort would
// produce, the separators carry runs of extra whitespace, and a non-ASCII value
// is in there too.
//
// All three exist to fail loudly if SETTINGS_JSON or BUNDLE_JSON is ever
// "improved" from TEXT to JSONB. JSONB is a parsed representation — it re-emits
// with its own key order and no insignificant whitespace — so the bytes read
// back would differ from the bytes the admin sent. That matters here and not
// merely aesthetically: the publisher skips a KV write whose bytes are
// identical to what the bucket already holds, and that skip is the only thing
// stopping a full reconcile sweep fanning a no-op out to every consumer in the
// fleet. Under JSONB the skip would never fire again.
func awkwardDocument(t *testing.T) string {
	t.Helper()

	var b strings.Builder
	b.WriteString("{\n")
	b.WriteString(`  "zzz.written.first"   :   "and a canonicalising column would sort it last",` + "\n")
	b.WriteString(`  "unicode"  :  "Catálogo — ¥ — 日本語",` + "\n")

	for i := 0; b.Len() < documentSize; i++ {
		fmt.Fprintf(&b, "  %q    :    %q,\n", fmt.Sprintf("section.%05d.key", i), strings.Repeat("v", 48))
	}

	b.WriteString(`  "aaa.written.last": true` + "\n")
	b.WriteString("}")

	doc := b.String()
	if !json.Valid([]byte(doc)) {
		t.Fatalf("the test built an invalid document (%d bytes)", len(doc))
	}
	if len(doc) < documentSize {
		t.Fatalf("the test built a %d byte document, want at least %d", len(doc), documentSize)
	}
	return doc
}

// A settings tree has to come back out of the database exactly as it went in.
// Under Oracle this was the CLOB-binding problem — go-ora sent a Go string as
// VARCHAR2 and a document this size overflowed it outright — and the fix,
// database.CLOB, never once ran against a real driver
// (docs/PRODUCTION_READINESS.md §3.1). Postgres takes a plain string into a
// TEXT column and needs no wrapper at all, so what is left to prove is that the
// bytes survive: the write, the read-back by id, the list, and the reconcile
// sweep that feeds the publisher.
func TestLargeSettingsDocumentRoundTripsByteIdentically(t *testing.T) {
	s := newStack(t)
	repo := microconfig.NewPostgresRepository(s.DB)
	ctx := context.Background()

	doc := awkwardDocument(t)

	created, err := repo.CreateMicroserviceConfig(ctx, microconfig.MicroserviceAppSettings{
		MicroserviceID: 2, EnvironmentID: 2, SettingsJSON: json.RawMessage(doc),
	})
	if err != nil {
		t.Fatalf("create a %d byte settings tree: %v", len(doc), err)
	}
	assertSameBytes(t, "create", doc, string(created.SettingsJSON))

	read, err := repo.GetMicroserviceConfigByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	assertSameBytes(t, "read by id", doc, string(read.SettingsJSON))

	listed, err := repo.ListMicroserviceConfigs(ctx, microconfig.AppSettingsFilter{
		Page: microconfig.Page{Limit: 10}, MicroserviceID: 2, EnvironmentID: 2,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("expected the row back from the list, got %d rows", len(listed))
	}
	assertSameBytes(t, "list", doc, string(listed[0].SettingsJSON))

	// The reconcile path is the one that matters most for the skip: it is what
	// every sweep compares against the bucket.
	swept, err := repo.ListAllForReconcile(ctx, time.Time{})
	if err != nil {
		t.Fatalf("reconcile sweep: %v", err)
	}
	var found bool
	for _, row := range swept {
		if row.ID == created.ID {
			assertSameBytes(t, "reconcile sweep", doc, string(row.SettingsJSON))
			found = true
		}
	}
	if !found {
		t.Fatalf("the reconcile sweep did not return row %d", created.ID)
	}

	// An update has to preserve the bytes too — it binds the document through a
	// different statement, with the document in a different placeholder
	// position.
	edited := strings.Replace(doc, `"aaa.written.last": true`, `"aaa.written.last": false`, 1)
	updated, err := repo.UpdateMicroserviceConfig(ctx, microconfig.MicroserviceAppSettings{
		ID: created.ID, MicroserviceID: 2, EnvironmentID: 2, SettingsJSON: json.RawMessage(edited),
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	assertSameBytes(t, "update", edited, string(updated.SettingsJSON))
}

// The same for a translation bundle: same column type, same verbatim republish,
// same reason the bytes have to survive.
func TestLargeLocalizationBundleRoundTripsByteIdentically(t *testing.T) {
	s := newStack(t)
	repo := localization.NewPostgresRepository(s.DB)
	ctx := context.Background()

	doc := awkwardDocument(t)

	created, err := repo.CreateLocalization(ctx, localization.Localization{
		MicroserviceID: 2, EnvironmentID: 2, Locale: "ja-JP", BundleJSON: json.RawMessage(doc),
	})
	if err != nil {
		t.Fatalf("create a %d byte bundle: %v", len(doc), err)
	}
	assertSameBytes(t, "create", doc, string(created.BundleJSON))

	read, err := repo.GetLocalization(ctx, 2, 2, "ja-JP")
	if err != nil {
		t.Fatalf("read back by natural key: %v", err)
	}
	assertSameBytes(t, "lookup", doc, string(read.BundleJSON))

	swept, err := repo.ListAllForReconcile(ctx, time.Time{})
	if err != nil {
		t.Fatalf("reconcile sweep: %v", err)
	}
	var found bool
	for _, row := range swept {
		if row.ID == created.ID {
			assertSameBytes(t, "reconcile sweep", doc, string(row.BundleJSON))
			found = true
		}
	}
	if !found {
		t.Fatalf("the reconcile sweep did not return row %d", created.ID)
	}
}

// assertSameBytes reports where the two documents first diverge rather than
// printing 64 KiB of JSON twice.
func assertSameBytes(t *testing.T, stage, want, got string) {
	t.Helper()
	if want == got {
		return
	}
	if len(want) != len(got) {
		t.Errorf("%s: document is %d bytes, wrote %d", stage, len(got), len(want))
	}
	for i := 0; i < len(want) && i < len(got); i++ {
		if want[i] != got[i] {
			from := max(0, i-40)
			t.Fatalf("%s: documents diverge at byte %d\n wrote: %q\n  read: %q",
				stage, i, want[from:min(len(want), i+40)], got[from:min(len(got), i+40)])
		}
	}
	t.Fatalf("%s: documents differ but share a common prefix of %d bytes", stage, min(len(want), len(got)))
}
