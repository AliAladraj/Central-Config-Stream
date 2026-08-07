package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/AliAladraj/Central-Config-Stream/internal/obs"
	"github.com/AliAladraj/Central-Config-Stream/internal/web"
)

// Inventory lists the editable rows with their primary keys. Admin tools need
// this because the IDs an update targets are data, not something a caller can
// know up front. Read-only, and it reuses the reconciler's list queries rather
// than adding new ones.
//
// Each of the three collections is paged independently by the same ?limit and
// ?offset, so a caller walks all three together; the reconcile queries return
// every row and the page is taken here, which is why the limit matters — the
// response is the whole configuration estate otherwise.
//
// Taking the page here also means ?limit bounds only the response, never the
// read: ListAllForReconcile has no LIMIT and no WHERE, so every request scans
// all three tables and materialises every document whatever page it was asked
// for. The read guard meters this route per credential for that reason
// (middleware.go), which bounds how often that happens but not what one call
// costs. Closing it properly needs the listers to take a limit, an offset and
// the caller's scope so the database does the paging — that is a change to the
// three repository packages, not to this handler.
type Inventory struct {
	Flags        []InventoryFlag  `json:"flags"`
	MicroConfigs []InventoryMicro `json:"microConfigs"`
	Localization []InventoryLoc   `json:"localization"`
}

type InventoryFlag struct {
	ID            int64  `json:"id"` // CONFIG_FLAG_VALUE.ID — the update target
	EnvironmentID int64  `json:"environmentId"`
	FlagKey       string `json:"flagKey"`
	Enabled       int64  `json:"enabled"`
	Value         string `json:"value"`
}

type InventoryMicro struct {
	ID             int64           `json:"id"`
	MicroserviceID int64           `json:"microserviceId"`
	EnvironmentID  int64           `json:"environmentId"`
	SettingsJSON   json.RawMessage `json:"settingsJson"`
}

type InventoryLoc struct {
	ID             int64           `json:"id"`
	MicroserviceID int64           `json:"microserviceId"`
	EnvironmentID  int64           `json:"environmentId"`
	Locale         string          `json:"locale"`
	BundleJSON     json.RawMessage `json:"bundleJson"`
}

type inventoryHandler struct {
	flags flagsLister
	micro microLister
	loc   locLister
}

func (h *inventoryHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := web.ParsePage(r)
	if err != nil {
		web.Error(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	page := pager{limit: limit, offset: offset}
	var inv Inventory

	// The driver errors name tables, connection strings and file paths; they go
	// to the log and the caller gets the same opaque message every handler
	// returns for an internal failure.
	flagRows, err := h.flags.ListAllForReconcile(ctx, time.Time{})
	if err != nil {
		obs.FromContext(ctx, "inventory").Error("list rows failed", slog.String("domain", "flags"), obs.Err(err))
		web.Error(w, http.StatusInternalServerError, "internal server error")
		return
	}
	flags := page.reset()
	for _, row := range flagRows {
		if !inScope(r, row.EnvironmentID) || !flags.take() {
			continue
		}
		inv.Flags = append(inv.Flags, InventoryFlag{
			ID:            row.ID,
			EnvironmentID: row.EnvironmentID,
			FlagKey:       row.FlagKey,
			Enabled:       row.Enabled,
			Value:         row.Value,
		})
	}

	microRows, err := h.micro.ListAllForReconcile(ctx, time.Time{})
	if err != nil {
		obs.FromContext(ctx, "inventory").Error("list rows failed", slog.String("domain", "microconfig"), obs.Err(err))
		web.Error(w, http.StatusInternalServerError, "internal server error")
		return
	}
	micro := page.reset()
	for _, row := range microRows {
		if !inScope(r, row.EnvironmentID) || !micro.take() {
			continue
		}
		inv.MicroConfigs = append(inv.MicroConfigs, InventoryMicro{
			ID:             row.ID,
			MicroserviceID: row.MicroserviceID,
			EnvironmentID:  row.EnvironmentID,
			SettingsJSON:   row.SettingsJSON,
		})
	}

	locRows, err := h.loc.ListAllForReconcile(ctx, time.Time{})
	if err != nil {
		obs.FromContext(ctx, "inventory").Error("list rows failed", slog.String("domain", "localization"), obs.Err(err))
		web.Error(w, http.StatusInternalServerError, "internal server error")
		return
	}
	loc := page.reset()
	for _, row := range locRows {
		if !inScope(r, row.EnvironmentID) || !loc.take() {
			continue
		}
		inv.Localization = append(inv.Localization, InventoryLoc{
			ID:             row.ID,
			MicroserviceID: row.MicroserviceID,
			EnvironmentID:  row.EnvironmentID,
			Locale:         row.Locale,
			BundleJSON:     row.BundleJSON,
		})
	}

	web.JSON(w, http.StatusOK, inv)
}

// pager applies ?limit and ?offset to rows the query returned in full. It
// counts only the rows the caller is allowed to see, so a scoped token pages
// through its own environments rather than through gaps left by another's.
type pager struct {
	limit  int
	offset int

	seen  int
	taken int
}

// reset returns a fresh pager over the same page, one per collection.
func (p pager) reset() *pager { return &pager{limit: p.limit, offset: p.offset} }

// take reports whether the next in-scope row belongs on this page.
func (p *pager) take() bool {
	if p.seen < p.offset {
		p.seen++
		return false
	}
	if p.taken >= p.limit {
		return false
	}
	p.seen++
	p.taken++
	return true
}
