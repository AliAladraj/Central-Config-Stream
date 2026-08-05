package app

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/ErasedKyte/Central-Config-Stream/internal/obs"
	"github.com/ErasedKyte/Central-Config-Stream/internal/web"
)

// sharedTokenName is what the single ADMIN_TOKEN is recorded as in the audit
// trail. ADMIN_TOKEN carries no scope of its own, so it is full scope; a
// narrower credential is spelled out in ADMIN_TOKENS instead.
const sharedTokenName = "shared"

// adminToken is one named admin credential. A nil envs map is full scope; any
// other value restricts the token to the listed CONFIG_ENVIRONMENTS ids, which
// is how a dev/staging operator is kept out of production.
type adminToken struct {
	name   string
	secret []byte
	envs   map[int64]bool
}

// allows answers whether this token may perform a write against target.
func (t adminToken) allows(target writeTarget) bool {
	if t.envs == nil {
		return true
	}
	// A write that reaches every environment — or one whose environment could
	// not be determined — is only safe in full-scope hands.
	if target.global {
		return false
	}
	for _, env := range target.envs {
		if !t.envs[env] {
			return false
		}
	}
	return true
}

// tokenSet is the parsed admin credential configuration. An empty set means
// auth is DISABLED (dev only; warned at startup).
type tokenSet struct {
	tokens []adminToken
}

func (s *tokenSet) enabled() bool { return s != nil && len(s.tokens) > 0 }

// lookup returns the token whose secret matches. Every entry is compared, in
// constant time and without an early exit, so neither the secret itself nor a
// token's position in the list leaks through response timing.
func (s *tokenSet) lookup(secret string) (adminToken, bool) {
	got := []byte(secret)
	var found adminToken
	ok := false
	for _, t := range s.tokens {
		if subtle.ConstantTimeCompare(got, t.secret) == 1 {
			found, ok = t, true
		}
	}
	return found, ok
}

// parseAdminTokens reads the named-token configuration:
//
//	ADMIN_TOKENS=alice:*:s3cr3t,ci-dev:1|2:another-secret
//
// Entries are comma separated; each is name:scope:secret with the secret being
// everything after the second colon, so a secret may itself contain colons. A
// scope of "*" is every environment, otherwise it is a |-separated list of
// environment ids.
//
// shared is the value of the single-credential ADMIN_TOKEN variable, which
// carries no scope syntax of its own and is therefore admitted as a full-scope
// token. The two variables may be set together.
func parseAdminTokens(named, shared string) (*tokenSet, error) {
	set := &tokenSet{}

	for i, entry := range strings.Split(named, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		// The error messages carry the entry number rather than the entry:
		// startup logs are not a place to print secrets.
		parts := strings.SplitN(entry, ":", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("ADMIN_TOKENS entry %d: want name:scope:secret", i+1)
		}
		name, scope, secret := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), parts[2]
		if name == "" || secret == "" {
			return nil, fmt.Errorf("ADMIN_TOKENS entry %d: name and secret are required", i+1)
		}
		envs, err := parseTokenScope(scope)
		if err != nil {
			return nil, fmt.Errorf("ADMIN_TOKENS entry %d (%s): %w", i+1, name, err)
		}
		set.tokens = append(set.tokens, adminToken{name: name, secret: []byte(secret), envs: envs})
	}

	if shared != "" {
		set.tokens = append(set.tokens, adminToken{name: sharedTokenName, secret: []byte(shared)})
	}
	return set, nil
}

func parseTokenScope(scope string) (map[int64]bool, error) {
	if scope == "*" {
		return nil, nil // full scope
	}
	envs := make(map[int64]bool)
	for _, raw := range strings.Split(scope, "|") {
		id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("invalid environment id %q in scope", raw)
		}
		envs[id] = true
	}
	return envs, nil
}

// targetClass says how a route's target environment is found. It is declared
// where the route is registered because the request on its own cannot be
// trusted to say what it touches: the PUT handlers key off the row id, so
// an environmentId in the body is a claim rather than a fact.
type targetClass int

const (
	// classGlobal reaches every environment, or changes the environment set
	// itself. Full-scope tokens only.
	classGlobal targetClass = iota
	// classNeutral creates a definition row that belongs to no environment and
	// publishes nothing to KV — any valid token may issue it.
	classNeutral
	// classEnvBody is a create whose body carries the environment the new row
	// will live in. Here the body IS authoritative: it is what gets inserted.
	classEnvBody
	// classEnvPath is a route whose {id} path value is itself an environment id.
	classEnvPath
	// The classRow* routes address an existing row by id; their environment is
	// read back from the database rather than taken from the body.
	classRowFlagValue
	classRowMicroConfig
	classRowLocalization
)

// table names the row the classRow* classes read their environment from.
func (c targetClass) table() string {
	switch c {
	case classRowFlagValue:
		return "CONFIG_FLAG_VALUE"
	case classRowMicroConfig:
		return "CONFIG_MICROSERVICE_APPSETTINGS"
	case classRowLocalization:
		return "CONFIG_LOCALIZATION"
	default:
		return ""
	}
}

// movesEnvironment reports whether the class's update statement rewrites
// ENVIRONMENT_ID from the request body — microconfig and localization do, so
// such a write touches both the environment it leaves and the one it enters.
// The flag-value update sets only VALUE and ENABLED.
func (c targetClass) movesEnvironment() bool {
	return c == classRowMicroConfig || c == classRowLocalization
}

// writeTarget is what a mutating request actually touches.
type writeTarget struct {
	domain string
	rowID  string
	envs   []int64
	global bool // every environment, or not determinable — treated the same
}

// security holds everything the write guard needs: the credentials, the write
// rate limiter, and the database the row-addressed routes resolve against.
type security struct {
	tokens  *tokenSet
	limiter *rateLimiter
	db      *sql.DB
	sqlite  bool
}

// guard wraps a mutating handler with the write rate limiter, the named-token
// check and the environment scope check, and publishes what the request targets
// to the audit record.
func (s *security) guard(class targetClass, domain string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if retry, ok := s.limiter.allow(rateLimitKey(r)); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			web.Error(w, http.StatusTooManyRequests, "too many write requests")
			return
		}

		sink := sinkFrom(r)

		// Authenticate before resolving the target: an unauthenticated caller
		// must not be able to make the API run database lookups.
		var token adminToken
		if s.tokens.enabled() {
			got, prefixed := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			found, ok := s.tokens.lookup(got)
			if !prefixed || !ok {
				web.Error(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			token = found
			if sink != nil {
				sink.actor = token.name
			}
		}

		target := s.resolveTarget(r, class, domain)
		if sink != nil {
			sink.domain = target.domain
			sink.rowID = target.rowID
			if len(target.envs) == 1 {
				sink.envID = target.envs[0]
			}
		}

		if s.tokens.enabled() && !token.allows(target) {
			web.Error(w, http.StatusForbidden, "token is not scoped to this environment")
			return
		}

		next(w, r)
	}
}

// requireAuth protects a read route that is not itself a write: the audit log
// carries request bodies, so it is not open the way the config reads are. No
// environment scope applies — every named token may read the trail.
func (s *security) requireAuth(next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.tokens.enabled() {
			next.ServeHTTP(w, r) // auth disabled (dev only; warned at startup)
			return
		}
		got, prefixed := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if _, ok := s.tokens.lookup(got); !prefixed || !ok {
			web.Error(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	}
}

// bodyFields are the only body values the guard reads. Decoding into a narrow
// struct keeps the guard out of the domain models.
type bodyFields struct {
	ID            int64 `json:"id"`
	EnvironmentID int64 `json:"environmentId"`
}

func (s *security) resolveTarget(r *http.Request, class targetClass, domain string) writeTarget {
	target := writeTarget{domain: domain, rowID: r.PathValue("id")}

	switch class {
	case classNeutral:
		return target

	case classGlobal:
		target.global = true
		return target

	case classEnvPath:
		id, err := strconv.ParseInt(target.rowID, 10, 64)
		if err != nil || id <= 0 {
			target.global = true // unparseable: fail closed, the handler will 400
			return target
		}
		target.envs = []int64{id}
		return target

	case classEnvBody:
		fields := decodeBodyFields(r)
		if fields.EnvironmentID <= 0 {
			target.global = true
			return target
		}
		target.envs = []int64{fields.EnvironmentID}
		return target
	}

	// classRow*: an existing row, addressed by {id} on a delete and by the body
	// id on the PUT routes.
	fields := decodeBodyFields(r)
	if target.rowID == "" && fields.ID > 0 {
		target.rowID = strconv.FormatInt(fields.ID, 10)
	}
	id, err := strconv.ParseInt(target.rowID, 10, 64)
	if err != nil || id <= 0 {
		target.global = true
		return target
	}

	envID, ok := s.lookupRowEnvironment(r.Context(), class.table(), id)
	if !ok {
		target.global = true
		return target
	}
	target.envs = append(target.envs, envID)

	if class.movesEnvironment() && fields.EnvironmentID > 0 && fields.EnvironmentID != envID {
		target.envs = append(target.envs, fields.EnvironmentID)
	}
	return target
}

// lookupRowEnvironment reads the environment a row lives in. A missing row or a
// failed read reports false, which the caller turns into a full-scope
// requirement — the handler is about to 404 anyway.
func (s *security) lookupRowEnvironment(ctx context.Context, table string, id int64) (int64, bool) {
	if s.db == nil || table == "" {
		return 0, false
	}
	// table is one of the constants above, never caller input.
	bind := ":1"
	if s.sqlite {
		bind = "?"
	}
	query := fmt.Sprintf("SELECT ENVIRONMENT_ID FROM %s WHERE ID = %s", table, bind)

	var envID int64
	if err := s.db.QueryRowContext(ctx, query, id).Scan(&envID); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			obs.FromContext(ctx, "security").Error("read row environment failed",
				slog.String("db.table", table), slog.Int64("row.id", id), obs.Err(err))
		}
		return 0, false
	}
	return envID, true
}

// decodeBodyFields peeks at the request body without consuming it. A body that
// does not parse yields zero values, which every caller treats as "unknown".
func decodeBodyFields(r *http.Request) bodyFields {
	var fields bodyFields
	body := bodyFor(r)
	if len(body) == 0 {
		return fields
	}
	if err := json.Unmarshal(body, &fields); err != nil {
		return bodyFields{}
	}
	return fields
}

// bodyFor returns the request body, reading and restoring it if the audit
// middleware has not already buffered it.
func bodyFor(r *http.Request) []byte {
	if sink := sinkFrom(r); sink != nil && sink.bodyRead {
		return sink.body
	}
	return bufferBody(r)
}

// bufferBody reads the body and puts it back so the handler can still decode
// it. The read is bounded by the same limit web.DecodeJSON enforces, so an
// oversized body is still rejected downstream rather than buffered whole.
func bufferBody(r *http.Request) []byte {
	if r.Body == nil {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, web.MaxRequestBody+1))
	if err != nil {
		obs.FromContext(r.Context(), "http").Warn("read request body failed", obs.Err(err))
		return nil
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body
}

// warnIfAuthDisabled logs a prominent warning when write auth is off.
func warnIfAuthDisabled(tokens *tokenSet) {
	if !tokens.enabled() {
		appLog.Warn("neither ADMIN_TOKENS nor ADMIN_TOKEN is set — write endpoints are UNAUTHENTICATED. Do not run like this in production.")
	}
}

// warnIfTLSDisabled logs when the admin API serves plain HTTP: the bearer token
// that can flip a production flag then crosses the wire in clear text.
func warnIfTLSDisabled(cfg *Config) {
	if cfg.TLSCertFile == "" || cfg.TLSKeyFile == "" {
		appLog.Warn("TLS_CERT_FILE/TLS_KEY_FILE are not set — the admin API serves plain HTTP and admin tokens cross the wire in clear text.")
	}
}

// tlsConfig is the server profile used when a certificate is configured: TLS
// 1.2 floor, so a downgrade cannot get an admin token onto a legacy cipher.
func tlsConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12}
}
