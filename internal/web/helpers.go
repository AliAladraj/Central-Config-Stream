package web

import (
	"encoding/json"
	"net/http"

	"github.com/ErasedKyte/Central-Config-Stream/internal/obs"
)

// webLog reports failures that happen after the status line is already on the
// wire, where there is nothing left to return to the caller.
var webLog = obs.Logger("http")

func JSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		webLog.Warn("write json response failed", obs.Err(err))
	}
}

func Error(w http.ResponseWriter, status int, message string) {
	JSON(w, status, ErrorResponse{Error: message})
}
