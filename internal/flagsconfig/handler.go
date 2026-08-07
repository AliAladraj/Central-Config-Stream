package flagsconfig

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/AliAladraj/Central-Config-Stream/internal/web"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// writeErr maps the domain errors onto status codes. Every handler funnels
// through it so a new error only has to be classified once.
func (h *Handler) writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidFlagID),
		errors.Is(err, ErrInvalidEnvironmentID),
		errors.Is(err, ErrInvalidFlagKey),
		errors.Is(err, ErrInvalidFlagValue),
		errors.Is(err, ErrFlagValueTooLarge),
		errors.Is(err, ErrInvalidEnabled),
		errors.Is(err, ErrInvalidIsActive):
		web.Error(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrFlagNotFound),
		errors.Is(err, ErrEnvironmentNotFound):
		web.Error(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrConflict),
		errors.Is(err, ErrFlagExists),
		errors.Is(err, ErrFlagValueExists):
		web.Error(w, http.StatusConflict, err.Error())
	default:
		web.Error(w, http.StatusInternalServerError, "internal server error")
	}
}

// GET /flags/{id}
func (h *Handler) GetFlagsByID(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		web.Error(w, http.StatusBadRequest, "invalid id")
		return
	}

	cfg, err := h.service.GetFlagByID(r.Context(), id)

	if err != nil {
		h.writeErr(w, err)
		return
	}

	web.JSON(w, http.StatusOK, cfg)
}

// GET /flags/values/{id}
func (h *Handler) GetFlagsValueByID(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		web.Error(w, http.StatusBadRequest, "invalid id")
		return
	}

	cfg, err := h.service.GetFlagValueByID(r.Context(), id)

	if err != nil {
		h.writeErr(w, err)
		return
	}

	web.JSON(w, http.StatusOK, cfg)
}

// GET /flags?flagKey=&limit=&offset=
func (h *Handler) ListFlags(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := web.ParsePage(r)
	if err != nil {
		web.Error(w, http.StatusBadRequest, err.Error())
		return
	}

	flags, err := h.service.ListFlags(r.Context(), FlagFilter{
		Page:    Page{Limit: limit, Offset: offset},
		FlagKey: r.URL.Query().Get("flagKey"),
	})
	if err != nil {
		h.writeErr(w, err)
		return
	}

	web.JSON(w, http.StatusOK, flags)
}

// GET /flags/values?environmentId=&flagKey=&limit=&offset=
func (h *Handler) ListFlagValues(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := web.ParsePage(r)
	if err != nil {
		web.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	environmentID, err := web.Int64Param(r, "environmentId")
	if err != nil {
		web.Error(w, http.StatusBadRequest, "invalid environmentId")
		return
	}

	values, err := h.service.ListFlagValues(r.Context(), FlagValueFilter{
		Page:          Page{Limit: limit, Offset: offset},
		EnvironmentID: environmentID,
		FlagKey:       r.URL.Query().Get("flagKey"),
	})
	if err != nil {
		h.writeErr(w, err)
		return
	}

	web.JSON(w, http.StatusOK, values)
}

// POST /flags
func (h *Handler) CreateFlag(w http.ResponseWriter, r *http.Request) {
	var req Flag
	if err := web.DecodeJSON(w, r, &req); err != nil {
		web.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}

	created, err := h.service.CreateFlag(r.Context(), req)
	if err != nil {
		h.writeErr(w, err)
		return
	}

	web.JSON(w, http.StatusCreated, created)
}

// DELETE /flags/{id}
func (h *Handler) DeleteFlag(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		web.Error(w, http.StatusBadRequest, "invalid id")
		return
	}

	if err := h.service.DeleteFlag(r.Context(), id); err != nil {
		h.writeErr(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// POST /flags/values
func (h *Handler) CreateFlagValue(w http.ResponseWriter, r *http.Request) {
	var req FlagValue
	if err := web.DecodeJSON(w, r, &req); err != nil {
		web.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}

	created, err := h.service.CreateFlagValue(r.Context(), req)
	if err != nil {
		h.writeErr(w, err)
		return
	}

	web.JSON(w, http.StatusCreated, created)
}

// DELETE /flags/values/{id}
func (h *Handler) DeleteFlagValue(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		web.Error(w, http.StatusBadRequest, "invalid id")
		return
	}

	if err := h.service.DeleteFlagValue(r.Context(), id); err != nil {
		h.writeErr(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// PUT /flags/values
func (h *Handler) UpdateFlagValue(w http.ResponseWriter, r *http.Request) {
	var req FlagValue
	if err := web.DecodeJSON(w, r, &req); err != nil {
		web.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}

	cfg, err := h.service.UpdateFlagValue(r.Context(), req)
	if err != nil {
		h.writeErr(w, err)
		return
	}

	web.JSON(w, http.StatusOK, cfg)
}
