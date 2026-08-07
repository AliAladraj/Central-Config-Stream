package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/AliAladraj/Central-Config-Stream/internal/database"
	"github.com/AliAladraj/Central-Config-Stream/internal/flagsconfig"
	"github.com/AliAladraj/Central-Config-Stream/internal/messaging"
)

// rejectingPublisher refuses one key and accepts the rest, standing in for the
// row that JetStream will not take — an oversized value, a key the bucket
// rejects — which is what wedged a whole domain's convergence.
type rejectingPublisher struct {
	messaging.NoopPublisher
	reject    string
	published []string
}

func (p *rejectingPublisher) PublishFlag(_ context.Context, environmentID int64, flagKey string, _ messaging.FlagPayload) error {
	key := messaging.FlagKey(environmentID, flagKey)
	if key == p.reject {
		return errors.New("maximum value size exceeded")
	}
	p.published = append(p.published, key)
	return nil
}

func (p *rejectingPublisher) PublishMicroConfig(_ context.Context, environmentID, microserviceID int64, _ json.RawMessage) error {
	key := messaging.MicroKey(environmentID, microserviceID)
	if key == p.reject {
		return errors.New("maximum value size exceeded")
	}
	p.published = append(p.published, key)
	return nil
}

// The reconcile source used to return on the first publish error, so every row
// behind the bad one stayed unpublished — and because the sweep was then marked
// failed, it never advanced its window either, so the next cycle was another
// full sweep that died on the same row. The rows behind it have to get through,
// and the result has to say the sweep was incomplete.
func TestReconcileSourceCarriesOnPastAnUnpublishableRow(t *testing.T) {
	db, err := database.NewSQLiteDB("file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "partial.db")))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	source := &flagsReconcileSource{repo: flagsconfig.NewSQLiteRepository(db)}

	// The seed holds four flag values; the first of them by id is rejected.
	pub := &rejectingPublisher{reject: "1.search_v2"}
	res, err := source.Resync(ctx, pub, time.Time{})
	if err != nil {
		t.Fatalf("resync: %v", err)
	}

	if !res.Partial {
		t.Error("a rejected row did not mark the sweep partial")
	}
	if len(res.Keys) != 3 {
		t.Fatalf("published keys = %v, want the three that were not rejected", res.Keys)
	}
	for _, key := range []string{"1.dark_mode", "1.new_pricing", "3.search_v2"} {
		if !contains(res.Keys, key) {
			t.Errorf("%s was stranded behind the rejected row", key)
		}
	}
	if contains(res.Keys, "1.search_v2") {
		t.Error("the rejected row was reported as published")
	}
}

// The rows a stalled sweep reaches have to be the same ones every cycle;
// without an ORDER BY, which rows get stranded is whatever the database felt
// like returning.
func TestReconcileListIsOrderedByID(t *testing.T) {
	db, err := database.NewSQLiteDB("file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "order.db")))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	rows, err := flagsconfig.NewSQLiteRepository(db).ListAllForReconcile(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("list for reconcile: %v", err)
	}
	if len(rows) < 2 {
		t.Fatalf("expected the seeded flag values, got %d rows", len(rows))
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].ID <= rows[i-1].ID {
			t.Fatalf("rows are not ordered by id: %d after %d", rows[i].ID, rows[i-1].ID)
		}
	}
}

func contains(keys []string, want string) bool {
	for _, k := range keys {
		if k == want {
			return true
		}
	}
	return false
}
