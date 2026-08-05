package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/ErasedKyte/Central-Config-Stream/internal/obs"
	"github.com/ErasedKyte/Central-Config-Stream/internal/web"
)

// Inventory lists every editable row with its primary key. Admin tools need
// this because the IDs an update targets are data, not something a caller can
// know up front. Read-only, and it reuses the reconciler's list queries rather
// than adding new ones.
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
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

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
	for _, row := range flagRows {
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
	for _, row := range microRows {
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
	for _, row := range locRows {
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
