// Package web holds the HTTP request and response plumbing shared by every
// domain handler: JSON encoding with a uniform error envelope, and decoding
// that is bounded before it is parsed.
//
// The bounds are the reason it is one package rather than a helper per domain.
// DecodeJSON caps the request body and ParsePage clamps ?limit, so a handler
// cannot forget either — an unbounded Decode or an unclamped page size is a
// memory-exhaustion path, and there is no second place to fix it.
package web

import (
	"encoding/json"
	"net/http"

	"github.com/AliAladraj/Central-Config-Stream/internal/obs"
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
