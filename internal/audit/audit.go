// Package audit records every mutating admin request against the control
// plane. Flipping a value here changes the behaviour of every consuming
// microservice within milliseconds, so "who changed what, when" has to survive
// the request that made the change — the KV bucket only ever holds the current
// value.
//
// An audit write is best effort by design: it is recorded after the handler has
// already committed, and a failure is logged rather than propagated. Losing the
// record of a change is bad; refusing a legitimate production change because
// the audit table is unavailable is worse.
package audit

import (
	"encoding/json"
	"strings"
	"time"
)

// Entry is one recorded mutation. Domain/TargetID/EnvironmentID are best-effort
// — the middleware fills them from whatever the route makes knowable, and a
// request that never reached a route (404, 401) carries only the envelope.
type Entry struct {
	ID            int64     `json:"id"`
	OccurredAt    time.Time `json:"occurredAt"`
	Actor         string    `json:"actor"`
	Method        string    `json:"method"`
	Path          string    `json:"path"`
	Domain        string    `json:"domain,omitempty"`
	TargetID      string    `json:"targetId,omitempty"`
	EnvironmentID int64     `json:"environmentId,omitempty"`
	StatusCode    int       `json:"statusCode"`
	RemoteAddr    string    `json:"remoteAddr,omitempty"`
	RequestBody   string    `json:"requestBody,omitempty"`
}

// Filter narrows the audit listing. A zero From/To is an open end of the range.
//
// Environments is the reader's own scope rather than a query parameter: a nil
// slice is full scope, and any other value restricts the listing to rows
// recorded against those environments. A row whose environment is unknown —
// the 404 and 401 envelopes, and the writes that belong to no environment — is
// outside every narrowed scope, because its body is the one thing about it that
// is not already known.
type Filter struct {
	Actor        string
	From         time.Time
	To           time.Time
	Environments []int64
	Limit        int
	Offset       int
}

// MaxStoredBody bounds the recorded body. It is a byte budget rather than a
// character one because the Oracle column is VARCHAR2(4000 BYTE); the headroom
// covers the truncation marker.
const MaxStoredBody = 3800

const (
	bodyTruncated = "...[truncated]"
	bodyNotJSON   = "[non-JSON body omitted]"
)

// sensitiveKeys are the field-name fragments whose values never reach the audit
// table. A settings tree is free-form JSON, so nothing stops a field from being
// named "password" or "token", and the audit log is read by more people than
// the write API is used by.
//
// The match is a case-insensitive substring, so both the camelCase and the
// snake_case spelling of a compound name has to be listed: "connectionString"
// contains "connectionstring" but "connection_string" does not.
//
// This stays a deny list rather than an allow list of storable fields. The
// bodies recorded here are appsettings trees and translation bundles whose keys
// are defined by the consuming services, not by this schema; an allow list
// would have nothing to enumerate and would reduce every recorded body to its
// envelope, which is the part the other audit columns already carry. The trail
// exists to show what an operator actually sent.
var sensitiveKeys = []string{
	"password", "passwd", "pwd", "secret", "token",
	"apikey", "api_key", "credential", "privatekey", "private_key",
	"authorization", "connectionstring", "connection_string",
	"connstring", "conn_string", "dsn",
	"signingkey", "signing_key", "cert", "pem",
	"sessionid", "session_id",
}

// Redact prepares a request body for storage: secret-looking values are masked
// and the result is bounded. A body that is not JSON is dropped entirely rather
// than stored blind — the API only accepts JSON, so anything else is either a
// probe or a mistake and neither is worth persisting verbatim.
func Redact(body []byte) string {
	if len(body) == 0 {
		return ""
	}

	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return bodyNotJSON
	}

	cleaned, err := json.Marshal(redactValue(parsed))
	if err != nil {
		return bodyNotJSON
	}

	out := string(cleaned)
	if len(out) > MaxStoredBody {
		// Cutting on a byte boundary can split a rune; drop the fragment.
		out = strings.ToValidUTF8(out[:MaxStoredBody], "") + bodyTruncated
	}
	return out
}

// redactValue walks the decoded body masking sensitive leaves. Appsettings and
// translation bundles nest arbitrarily deep, so the walk cannot stop at the top
// level.
func redactValue(v any) any {
	switch value := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		for k, nested := range value {
			if isSensitiveKey(k) {
				out[k] = "[redacted]"
				continue
			}
			out[k] = redactValue(nested)
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, nested := range value {
			out[i] = redactValue(nested)
		}
		return out
	default:
		return value
	}
}

func isSensitiveKey(name string) bool {
	lower := strings.ToLower(name)
	for _, needle := range sensitiveKeys {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}
