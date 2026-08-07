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
	"slices"
	"strconv"
	"strings"

	"github.com/AliAladraj/Central-Config-Stream/internal/audit"
	"github.com/AliAladraj/Central-Config-Stream/internal/obs"
	"github.com/AliAladraj/Central-Config-Stream/internal/web"
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
			return nil, fmt.Errorf("ADMIN_TOKENS entry %d: want name:scope:secret "+
				"(a secret must not contain a comma — entries are comma separated)", i+1)
		}
		name, scope, secret := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), parts[2]
		if name == "" || secret == "" {
			return nil, fmt.Errorf("ADMIN_TOKENS entry %d: name and secret are required", i+1)
		}
		if !validTokenName(name) {
			return nil, fmt.Errorf("ADMIN_TOKENS entry %d: %q is not a usable token name "+
				"(letters, digits, dot, dash and underscore, up to %d characters) — "+
				"a secret containing a comma splits into an entry of its own", i+1, name, maxTokenName)
		}
		envs, err := parseTokenScope(scope)
		if err != nil {
			return nil, fmt.Errorf("ADMIN_TOKENS entry %d (%s): %w", i+1, name, err)
		}
		set.tokens = append(set.tokens, adminToken{name: name, secret: []byte(secret), envs: envs})
	}

	// A variable that holds separators but yields no token at all is a rendering
	// accident, not a configuration: an empty secret file, or a Helm value that
	// collapsed to " , , ". Returning the empty set for it is indistinguishable
	// from the genuinely-unset case, which disables authentication — so the
	// process came up announcing that neither variable was set, handed every
	// request a zero adminToken (nil envs, full scope) and served unauthenticated
	// deletes against production. docs/SECURITY.md says a malformed ADMIN_TOKENS
	// is a fatal startup error; this is what makes that true.
	//
	// The line is drawn after trimming, and it has to be: a value that is
	// entirely blank cannot be told apart from an unset one — ADMIN_TOKENS= in
	// an env file renders to exactly that — and the unset case is the documented
	// development path, which announces itself loudly rather than quietly. A
	// value with a separator in it is not blank: something was rendered into it,
	// and that says credentials were meant to be configured.
	if strings.TrimSpace(named) != "" && len(set.tokens) == 0 {
		return nil, errors.New("ADMIN_TOKENS is set but contains no entry — " +
			"want name:scope:secret entries separated by commas, or leave the variable unset")
	}

	if shared != "" {
		// The single-credential form is stricter about the blank value than the
		// list above, because the outcome is worse rather than merely equal to
		// unset: a whitespace ADMIN_TOKEN is a live full-scope credential whose
		// secret is a space, with authentication enabled and no warning printed,
		// and anyone who sends "Bearer  " holds it.
		//
		// The value is deliberately not trimmed before it becomes the secret. A
		// secret is bytes, and quietly accepting " hunter2 " as "hunter2" would
		// authenticate something the operator did not configure.
		if strings.TrimSpace(shared) == "" {
			return nil, errors.New("ADMIN_TOKEN is set to whitespace — " +
				"that is a credential nobody chose; set a secret or leave the variable unset")
		}
		set.tokens = append(set.tokens, adminToken{name: sharedTokenName, secret: []byte(shared)})
	}
	return set, nil
}

// maxTokenName matches the CONFIG_AUDIT_LOG.ACTOR column the name is recorded
// in.
const maxTokenName = 100

// validTokenName constrains a token name to a plain label. It is what an
// operator chose to call a credential and it ends up in the audit trail and in
// every access-log line, so it is checked rather than trusted — and because
// ADMIN_TOKENS is split on commas before it is split on colons, the check is
// also what catches a secret that contains a comma and has quietly become an
// entry, and a full-scope token, of its own.
func validTokenName(name string) bool {
	if name == "" || len(name) > maxTokenName {
		return false
	}
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
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

// guard wraps a mutating handler with the named-token check, the per-credential
// write budget and the environment scope check, and publishes what the request
// targets to the audit record.
func (s *security) guard(class targetClass, domain string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sink := sinkFrom(r)

		// Authenticate before anything else: an unauthenticated caller must not
		// be able to make the API run database lookups, and a write budget is
		// only worth keeping per credential once the credential is known to be
		// one. The unauthenticated flood is bounded by address at the edge.
		token, ok := s.authenticate(w, r)
		if !ok {
			return
		}
		if s.tokens.enabled() {
			if sink != nil {
				sink.actor = token.name
			}
			if !s.spend(w, tokenKey(token.name)) {
				return
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

		if !token.allows(target) {
			web.Error(w, http.StatusForbidden, "token is not scoped to this environment")
			return
		}

		next(w, r)
	}
}

// authenticate matches the bearer token, answering 401 itself when it does not.
// The zero adminToken it reports when auth is disabled (dev only; warned at
// startup) is full scope, which is what an unprotected deployment already is.
func (s *security) authenticate(w http.ResponseWriter, r *http.Request) (adminToken, bool) {
	if !s.tokens.enabled() {
		return adminToken{}, true
	}
	got, prefixed := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	found, ok := s.tokens.lookup(got)
	if !prefixed || !ok {
		web.Error(w, http.StatusUnauthorized, "unauthorized")
		return adminToken{}, false
	}
	return found, true
}

// spend charges one write against key and answers 429 with a Retry-After when
// the bucket is empty.
func (s *security) spend(w http.ResponseWriter, key string) bool {
	retry, ok := s.limiter.allow(key)
	return charged(w, retry, ok)
}

// spendAnonymous charges a write from a caller that has presented no usable
// credential. It draws on a separate, deliberately smaller share of the same
// limiter — see allowAnonymous.
func (s *security) spendAnonymous(w http.ResponseWriter, key string) bool {
	retry, ok := s.limiter.allowAnonymous(key)
	return charged(w, retry, ok)
}

// charged turns a limiter verdict into the response. Retry-After is whole
// seconds, which is what the header takes.
func charged(w http.ResponseWriter, retry int, ok bool) bool {
	if ok {
		return true
	}
	w.Header().Set("Retry-After", strconv.Itoa(retry))
	web.Error(w, http.StatusTooManyRequests, "too many write requests")
	return false
}

// limitWrites is the outermost gate on a mutating request, and it keys on the
// client address because no proxy header can be trusted to name the caller. It
// runs ahead of the audit middleware: without it an unauthenticated request to
// any path — including one matching no route — buys a megabyte of body read, a
// full JSON walk and a synchronous insert into the audit table, which is a
// write primitive with no credential in front of it.
//
// A caller that has presented no usable credential draws on a bucket of its
// own. Sharing one address-keyed bucket meant a flood of credential-less writes
// spent the budget that the next authenticated write from the same address
// needed — and behind an ingress RemoteAddr is the proxy, so one anonymous
// attacker could 429 every operator arriving through it. The secret is matched
// here to decide which bucket to charge, which is a constant-time compare
// against a handful of configured tokens and none of the work this gate exists
// to keep away from unauthenticated callers; the request is still authenticated
// properly further in, and an attacker who does hold a token is bounded by the
// per-credential budget the guard charges.
func (s *security) limitWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isMutating(r.Method) {
			if s.presentsCredential(r) {
				if !s.spend(w, addressKey(r)) {
					return
				}
			} else if !s.spendAnonymous(w, anonymousKey(r)) {
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// presentsCredential reports whether the request arrives with a bearer secret
// this deployment actually configured. It is not authentication — it answers no
// for the cases authenticate would 401 — and it says yes to everything when
// auth is disabled, because there is then no credential to present and the
// caller is the local developer the mode exists for.
func (s *security) presentsCredential(r *http.Request) bool {
	if !s.tokens.enabled() {
		return true
	}
	secret, prefixed := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !prefixed {
		return false
	}
	_, ok := s.tokens.lookup(secret)
	return ok
}

// readClass says how a read route's response relates to the caller's
// environment scope. Like a write's targetClass it is declared where the route
// is registered, because what a response holds is a property of the route and
// not something the response body can be trusted to announce.
type readClass int

const (
	// classReadNeutral returns rows that belong to no environment — the flag
	// and microservice definitions, and the reference tables' own names.
	// Authentication applies to it, scope does not.
	classReadNeutral readClass = iota
	// classReadEnvRows returns rows that name their environment in
	// environmentId: the flag values, the appsettings and the bundles.
	classReadEnvRows
	// classReadEnvs returns the environment rows themselves, whose own id is
	// the environment.
	classReadEnvs
	// classReadAudit returns the trail, which the store narrows in SQL from the
	// scope this guard hands it rather than after the fact.
	classReadAudit
	// classReadInventory returns every editable row across all three domains;
	// that handler pages and narrows itself from the scope in the context.
	classReadInventory
)

// scopeField is the JSON field a row of this class names its environment in,
// or "" when the response is not narrowed on the way out.
func (c readClass) scopeField() string {
	switch c {
	case classReadEnvRows:
		return "environmentId"
	case classReadEnvs:
		return "id"
	default:
		return ""
	}
}

// read wraps a read route with authentication and narrows what comes back to
// the environments the caller's token names. Reads used to be open, which made
// a single anonymous GET enough to walk off with the whole configuration
// estate — every appsettings tree, every bundle and every flag value, for
// production as readily as for dev.
func (s *security) read(class readClass, next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := s.authenticate(w, r)
		if !ok {
			return
		}

		// Reads are otherwise unmetered because they answer from a page bounded
		// in SQL. /inventory does not: it reads all three domains whole and
		// pages in Go, so any valid token — including the narrowest — can loop
		// it and force three full scans plus every document in the estate into
		// memory, per request. Until the listers can take a limit and an offset
		// (see inventory.go), a budget of its own is the bound: it caps the rate
		// rather than the cost of one call, and it is charged per credential so
		// one tool looping it cannot spend an operator's write budget or
		// another caller's reads.
		if class == classReadInventory && !s.spend(w, inventoryKey(token.name, r)) {
			return
		}

		if token.envs == nil {
			next.ServeHTTP(w, r) // full scope
			return
		}

		// The handlers this package owns narrow their own rows, from the scope
		// on the context. The domain handlers have no notion of a token, so
		// their responses are narrowed on the way out instead.
		r = withScope(r, token.envs)
		if class == classReadAudit {
			r = audit.WithEnvironments(r, scopeList(token.envs))
		}

		field := class.scopeField()
		if field == "" {
			next.ServeHTTP(w, r)
			return
		}
		sw := &scopeWriter{ResponseWriter: w, envs: token.envs, field: field}
		next.ServeHTTP(sw, r)
		sw.flush()
	}
}

type scopeKey struct{}

// withScope publishes the caller's environment scope for the handlers that
// narrow their own rows.
func withScope(r *http.Request, envs map[int64]bool) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), scopeKey{}, envs))
}

// inScope reports whether the caller may see rows living in env. An absent
// scope is full scope: the read guard publishes one only for a token that has a
// narrower one, and a deployment with auth disabled has no scopes at all.
func inScope(r *http.Request, env int64) bool {
	envs, ok := r.Context().Value(scopeKey{}).(map[int64]bool)
	if !ok {
		return true
	}
	return envs[env]
}

// scopeList renders a scope as the sorted id list a store binds.
func scopeList(envs map[int64]bool) []int64 {
	out := make([]int64, 0, len(envs))
	for env := range envs {
		out = append(out, env)
	}
	slices.Sort(out)
	return out
}

// scopeWriter narrows a read response to the caller's scope on the way out. The
// domain handlers page in the database and this runs after, so a scoped caller
// can see fewer than ?limit rows on a page — the cost of not teaching handlers
// that answer for one row at a time about credentials.
type scopeWriter struct {
	http.ResponseWriter
	envs  map[int64]bool
	field string

	body   bytes.Buffer
	status int
	direct bool
}

func (w *scopeWriter) WriteHeader(code int) {
	if w.status != 0 {
		return
	}
	w.status = code
	// Only a successful response carries rows; an error body is the handler's
	// own message and goes out as written.
	if code != http.StatusOK {
		w.direct = true
		w.ResponseWriter.WriteHeader(code)
	}
}

func (w *scopeWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.direct {
		return w.ResponseWriter.Write(b)
	}
	return w.body.Write(b)
}

// flush sends what survived the scope. A response that was passed straight
// through, or a handler that wrote nothing at all, has nothing left to do.
func (w *scopeWriter) flush() {
	if w.direct || w.status == 0 {
		return
	}
	switch status, body := scopeFilter(w.body.Bytes(), w.envs, w.field); status {
	case http.StatusOK:
		w.ResponseWriter.WriteHeader(w.status)
		_, _ = w.ResponseWriter.Write(body)
	case http.StatusNotFound:
		web.Error(w.ResponseWriter, http.StatusNotFound, "not found")
	default:
		web.Error(w.ResponseWriter, http.StatusInternalServerError, "internal server error")
	}
}

// scopeFilter narrows a response body, reporting the status to answer with. A
// listing keeps the rows the scope covers; a single row outside it answers 404,
// the same as one that does not exist — any other answer confirms it does. A
// body that does not decode is not something a handler here produces, and it is
// not forwarded on the chance that it holds rows this caller may not see.
func scopeFilter(body []byte, envs map[int64]bool, field string) (int, []byte) {
	trimmed := bytes.TrimSpace(body)
	switch {
	case len(trimmed) == 0:
		return http.StatusOK, body

	// An empty listing arrives as null, not as []: the repositories build their
	// result into a nil slice and Go marshals that. A filter that matched
	// nothing and an ?offset past the end of the table both produce it, so this
	// is an ordinary empty page and not a malformed body — without this arm it
	// fell to the default and a scoped caller got 500 for the very response
	// docs/SECURITY.md tells it to page until it sees. It answers [] rather than
	// null because that is already what the arm below produces when the scope
	// keeps none of a page's rows: a narrowed caller then has one shape for "no
	// rows", whichever end of the pipe the emptiness came from. A single row
	// fetched by id never reaches here as null — those handlers answer their own
	// 404 through the error path.
	case bytes.Equal(trimmed, []byte("null")):
		return http.StatusOK, []byte("[]\n")

	case trimmed[0] == '[':
		var rows []json.RawMessage
		if err := json.Unmarshal(trimmed, &rows); err != nil {
			return http.StatusInternalServerError, nil
		}
		kept := make([]json.RawMessage, 0, len(rows))
		for _, row := range rows {
			if env, ok := rowEnvironment(row, field); ok && envs[env] {
				kept = append(kept, row)
			}
		}
		out, err := json.Marshal(kept)
		if err != nil {
			return http.StatusInternalServerError, nil
		}
		return http.StatusOK, append(out, '\n')

	case trimmed[0] == '{':
		if env, ok := rowEnvironment(trimmed, field); !ok || !envs[env] {
			return http.StatusNotFound, nil
		}
		return http.StatusOK, body

	default:
		return http.StatusInternalServerError, nil
	}
}

// rowEnvironment reads the environment a response row belongs to. A row that
// does not name one is outside every narrowed scope: the guard cannot tell
// whether it is harmless or the one row that matters.
func rowEnvironment(row []byte, field string) (int64, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(row, &fields); err != nil {
		return 0, false
	}
	raw, ok := fields[field]
	if !ok {
		return 0, false
	}
	var env int64
	if err := json.Unmarshal(raw, &env); err != nil {
		return 0, false
	}
	return env, true
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
	// table is one of the constants above, never caller input. The placeholder
	// is not: PostgreSQL is the production driver and wants $1, and the Oracle
	// :1 that used to stand here made every one of these lookups fail with
	// SQLSTATE 42601. A failed lookup reports false, which the caller turns into
	// target.global — so the effect was not an error a caller could see but a
	// blanket 403 for every scoped token on every row-addressed write, an
	// unloggable-looking permissions bug. Only the SQLite branch was ever
	// exercised, because every test in this package runs on that driver.
	bind := "$1"
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

// tlsConfig is the server profile used when a certificate is configured: a TLS
// 1.3 floor, so a downgrade cannot get an admin token onto a legacy cipher.
// Everything that talks to this API is either a browser or a Go client — the
// console proxy, the configclient fallback, Prometheus — and all of them have
// spoken 1.3 for years; nothing in the stack needs 1.2 kept open.
func tlsConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13}
}

// hstsMaxAge is a year, the value below which preload lists ignore the header.
const hstsMaxAge = "max-age=31536000; includeSubDomains"

// securityHeaders sets what belongs on every response regardless of route.
// internal/web writes the bodies but the headers are an edge concern, and this
// is the only middleware every response passes through. HSTS goes out only on a
// connection that is really TLS: over plain HTTP it means nothing, and a
// deployment terminating TLS at an ingress sets it there, where the certificate
// and the hostname actually live.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.TLS != nil {
			w.Header().Set("Strict-Transport-Security", hstsMaxAge)
		}
		next.ServeHTTP(w, r)
	})
}
