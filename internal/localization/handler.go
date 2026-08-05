package localization

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

func (h *Handler) writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidLocalizationID),
		errors.Is(err, ErrInvalidMicroserviceID),
		errors.Is(err, ErrInvalidEnvironmentID),
		errors.Is(err, ErrInvalidLocale),
		errors.Is(err, ErrInvalidBundleJSON),
		errors.Is(err, ErrBundleTooLarge):
		web.Error(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrLocalizationNotFound),
		errors.Is(err, ErrMicroserviceNotFound),
		errors.Is(err, ErrEnvironmentNotFound):
		web.Error(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrConflict),
		errors.Is(err, ErrLocalizationExists):
		web.Error(w, http.StatusConflict, err.Error())
	default:
		web.Error(w, http.StatusInternalServerError, "internal server error")
	}
}

// GET /localization/{id}
func (h *Handler) GetLocalizationByID(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		web.Error(w, http.StatusBadRequest, "invalid id")
		return
	}
	l, err := h.service.GetLocalizationByID(r.Context(), id)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	web.JSON(w, http.StatusOK, l)
}

// GET /localization/lookup/{msId}/{envId}/{locale}
func (h *Handler) GetLocalization(w http.ResponseWriter, r *http.Request) {
	msID, err := strconv.ParseInt(r.PathValue("msId"), 10, 64)
	if err != nil {
		web.Error(w, http.StatusBadRequest, "invalid msId")
		return
	}
	envID, err := strconv.ParseInt(r.PathValue("envId"), 10, 64)
	if err != nil {
		web.Error(w, http.StatusBadRequest, "invalid envId")
		return
	}
	locale := r.PathValue("locale")

	l, err := h.service.GetLocalization(r.Context(), msID, envID, locale)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	web.JSON(w, http.StatusOK, l)
}

// PUT /localization/values
func (h *Handler) UpdateLocalization(w http.ResponseWriter, r *http.Request) {
	var req Localization
	if err := web.DecodeJSON(w, r, &req); err != nil {
		web.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	l, err := h.service.UpdateLocalization(r.Context(), req)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	web.JSON(w, http.StatusOK, l)
}

// GET /localization?microserviceId=&environmentId=&locale=&limit=&offset=
func (h *Handler) ListLocalizations(w http.ResponseWriter, r *http.Request) {
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

	rows, err := h.service.ListLocalizations(r.Context(), Filter{
		Page:           Page{Limit: limit, Offset: offset},
		MicroserviceID: microserviceID,
		EnvironmentID:  environmentID,
		Locale:         r.URL.Query().Get("locale"),
	})
	if err != nil {
		h.writeErr(w, err)
		return
	}
	web.JSON(w, http.StatusOK, rows)
}

// POST /localization
func (h *Handler) CreateLocalization(w http.ResponseWriter, r *http.Request) {
	var req Localization
	if err := web.DecodeJSON(w, r, &req); err != nil {
		web.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	l, err := h.service.CreateLocalization(r.Context(), req)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	web.JSON(w, http.StatusCreated, l)
}

// DELETE /localization/{id}
func (h *Handler) DeleteLocalization(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		web.Error(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := h.service.DeleteLocalization(r.Context(), id); err != nil {
		h.writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
