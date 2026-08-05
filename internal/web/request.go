package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
)

// MaxRequestBody bounds a write request body. Appsettings trees and translation
// bundles are the largest payloads the API accepts and they are kilobytes, so a
// megabyte is generous — the point is that an unbounded Decode cannot be used to
// exhaust memory.
const MaxRequestBody = 1 << 20 // 1 MiB

// DefaultListLimit / MaxListLimit bound the list endpoints, so a caller that
// omits ?limit does not walk the whole table and one that asks for everything
// still cannot.
const (
	DefaultListLimit = 100
	MaxListLimit     = 500
)

// ErrInvalidQueryParam is returned when a numeric query parameter is not a
// non-negative integer. Callers map it to 400.
var ErrInvalidQueryParam = errors.New("invalid query parameter")

// DecodeJSON reads a size-limited JSON body into dst. Everything that decodes a
// request body goes through here so the limit cannot be forgotten.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBody)
	return json.NewDecoder(r.Body).Decode(dst)
}

// ParsePage reads ?limit and ?offset. Absent values fall back to the default
// page; an out-of-range limit is clamped rather than rejected, because a caller
// asking for too much still wants rows back.
func ParsePage(r *http.Request) (limit, offset int, err error) {
	limit, err = intParam(r, "limit", DefaultListLimit)
	if err != nil {
		return 0, 0, err
	}
	offset, err = intParam(r, "offset", 0)
	if err != nil {
		return 0, 0, err
	}
	if limit <= 0 || limit > MaxListLimit {
		limit = MaxListLimit
	}
	return limit, offset, nil
}

// Int64Param reads an optional numeric filter (an id). Absent yields 0, which
// every filter treats as "no filter".
func Int64Param(r *http.Request, name string) (int64, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return 0, ErrInvalidQueryParam
	}
	return v, nil
}

func intParam(r *http.Request, name string, fallback int) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		return 0, ErrInvalidQueryParam
	}
	return v, nil
}
