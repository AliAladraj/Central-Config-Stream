package microconfig

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/ErasedKyte/Central-Config-Stream/internal/web"
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
	case errors.Is(err, ErrInvalidConfigID),
		errors.Is(err, ErrInvalidMicroserviceID),
		errors.Is(err, ErrInvalidEnvironmentID),
		errors.Is(err, ErrInvalidSettingsJSON),
		errors.Is(err, ErrSettingsTooLarge),
		errors.Is(err, ErrInvalidMicroserviceName),
		errors.Is(err, ErrInvalidEnvironmentName):
		web.Error(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrConfigNotFound),
		errors.Is(err, ErrMicroserviceNotFound),
		errors.Is(err, ErrEnvironmentNotFound):
		web.Error(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrConflict),
		errors.Is(err, ErrConfigExists),
		errors.Is(err, ErrMicroserviceExists),
		errors.Is(err, ErrEnvironmentExists),
		errors.Is(err, ErrMicroserviceInUse),
		errors.Is(err, ErrEnvironmentInUse):
		web.Error(w, http.StatusConflict, err.Error())
	default:
		web.Error(w, http.StatusInternalServerError, "internal server error")
	}
}

// GET /configs/values/{id}
func (h *Handler) GetMicroserviceConfig(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		web.Error(w, http.StatusBadRequest, "invalid id")
		return
	}

	cfg, err := h.service.GetMicroserviceConfigByID(r.Context(), id)
	if err != nil {
		h.writeErr(w, err)
		return
	}

	web.JSON(w, http.StatusOK, cfg)
}

// GET /configs/{id}
func (h *Handler) GetMicroservice(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		web.Error(w, http.StatusBadRequest, "invalid id")
		return
	}

	ms, err := h.service.GetMicroserviceByID(r.Context(), id)
	if err != nil {
		h.writeErr(w, err)
		return
	}

	web.JSON(w, http.StatusOK, ms)
}

// PUT /configs/values
func (h *Handler) UpdateMicroserviceConfig(w http.ResponseWriter, r *http.Request) {
	var req MicroserviceAppSettings
	if err := web.DecodeJSON(w, r, &req); err != nil {
		web.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}

	cfg, err := h.service.UpdateMicroserviceConfig(r.Context(), req)
	if err != nil {
		h.writeErr(w, err)
		return
	}

	web.JSON(w, http.StatusOK, cfg)
}

// GET /configs/values?microserviceId=&environmentId=&limit=&offset=
func (h *Handler) ListMicroserviceConfigs(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := web.ParsePage(r)
	if err != nil {
		web.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	microserviceID, err := web.Int64Param(r, "microserviceId")
	if err != nil {
		web.Error(w, http.StatusBadRequest, "invalid microserviceId")
		return
	}
	environmentID, err := web.Int64Param(r, "environmentId")
	if err != nil {
		web.Error(w, http.StatusBadRequest, "invalid environmentId")
		return
	}

	configs, err := h.service.ListMicroserviceConfigs(r.Context(), AppSettingsFilter{
		Page:           Page{Limit: limit, Offset: offset},
		MicroserviceID: microserviceID,
		EnvironmentID:  environmentID,
	})
	if err != nil {
		h.writeErr(w, err)
		return
	}

	web.JSON(w, http.StatusOK, configs)
}

// POST /configs/values
func (h *Handler) CreateMicroserviceConfig(w http.ResponseWriter, r *http.Request) {
	var req MicroserviceAppSettings
	if err := web.DecodeJSON(w, r, &req); err != nil {
		web.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}

	created, err := h.service.CreateMicroserviceConfig(r.Context(), req)
	if err != nil {
		h.writeErr(w, err)
		return
	}

	web.JSON(w, http.StatusCreated, created)
}

// DELETE /configs/values/{id}
func (h *Handler) DeleteMicroserviceConfig(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		web.Error(w, http.StatusBadRequest, "invalid id")
		return
	}

	if err := h.service.DeleteMicroserviceConfig(r.Context(), id); err != nil {
		h.writeErr(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// GET /microservices?limit=&offset=
func (h *Handler) ListMicroservices(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := web.ParsePage(r)
	if err != nil {
		web.Error(w, http.StatusBadRequest, err.Error())
		return
	}

	services, err := h.service.ListMicroservices(r.Context(), Page{Limit: limit, Offset: offset})
	if err != nil {
		h.writeErr(w, err)
		return
	}

	web.JSON(w, http.StatusOK, services)
}

// POST /microservices
func (h *Handler) CreateMicroservice(w http.ResponseWriter, r *http.Request) {
	var req Microservice
	if err := web.DecodeJSON(w, r, &req); err != nil {
		web.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}

	created, err := h.service.CreateMicroservice(r.Context(), req)
	if err != nil {
		h.writeErr(w, err)
		return
	}

	web.JSON(w, http.StatusCreated, created)
}

// DELETE /microservices/{id}
func (h *Handler) DeleteMicroservice(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		web.Error(w, http.StatusBadRequest, "invalid id")
		return
	}

	if err := h.service.DeleteMicroservice(r.Context(), id); err != nil {
		h.writeErr(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// GET /environments?limit=&offset=
func (h *Handler) ListEnvironments(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := web.ParsePage(r)
	if err != nil {
		web.Error(w, http.StatusBadRequest, err.Error())
		return
	}

	environments, err := h.service.ListEnvironments(r.Context(), Page{Limit: limit, Offset: offset})
	if err != nil {
		h.writeErr(w, err)
		return
	}

	web.JSON(w, http.StatusOK, environments)
}

// POST /environments
func (h *Handler) CreateEnvironment(w http.ResponseWriter, r *http.Request) {
	var req Environment
	if err := web.DecodeJSON(w, r, &req); err != nil {
		web.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}

	created, err := h.service.CreateEnvironment(r.Context(), req)
	if err != nil {
		h.writeErr(w, err)
		return
	}

	web.JSON(w, http.StatusCreated, created)
}

// DELETE /environments/{id}
func (h *Handler) DeleteEnvironment(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		web.Error(w, http.StatusBadRequest, "invalid id")
		return
	}

	if err := h.service.DeleteEnvironment(r.Context(), id); err != nil {
		h.writeErr(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
