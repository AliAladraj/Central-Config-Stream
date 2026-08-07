package configclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HTTPFallback hydrates the cache from central-config's HTTP API when
// JetStream cannot supply it. It runs only from New — never from a read — so
// once the cache is warm this type costs nothing.
//
// What it can reach, and why not more: KV is keyed the way a consumer thinks
// (flag key, microservice, locale) but two of the three HTTP endpoints are
// keyed by database row id:
//
//	GET /localization/lookup/{msId}/{envId}/{locale}  — reachable from what the
//	    client already knows, given the locales to fetch (Locales).
//	GET /configs/values/{id}   — {id} is the config-value row id, not the
//	    microservice id. Only reachable if the caller supplies ConfigValueID.
//	GET /flags/values/{id}     — {id} is the flag-value row id, not the flag
//	    key. Only reachable if the caller supplies FlagValueIDs.
//
// The control plane does list flag values for an environment
// (GET /flags/values?environmentId=), but this fallback fetches by row id
// instead, so the ids have to be named here. Configure what you have; what is
// left unset is simply not fetched.
//
// Credentials: the admin API authenticates every route except /health, /livez
// and /metrics, and a token's environment scope narrows what it may read as
// well as what it may write. Set Token to a credential scoped to
// Options.EnvironmentID.
type HTTPFallback struct {
	// BaseURL of central-config's HTTP API, e.g. http://central-config:8080.
	BaseURL string

	// Token is the bearer credential sent on every fallback request. It must be
	// scoped to the client's environment: the API answers a read outside a
	// token's scope with 404, the same as a row that does not exist.
	//
	// It is a secret and is treated as one — it is never logged, never put in
	// an error message and never reported by Status.
	Token string

	// AllowUnauthenticated sends the fallback requests with no Authorization
	// header. It is for a deployment running with auth switched off, which is a
	// dev-only mode the control plane warns about at startup. Setting it
	// anywhere else converts a boot-time configuration error into a 401
	// discovered on the day JetStream is already down.
	AllowUnauthenticated bool

	// Timeout for the whole hydration pass. Defaults to 5s.
	Timeout time.Duration

	// HTTPClient overrides the default client (tests, custom transports).
	HTTPClient *http.Client

	// ConfigValueID is the /configs/values/{id} row id holding this service's
	// appsettings. Required to fall back for MICROCONFIG.
	ConfigValueID int64

	// FlagValueIDs maps flag key -> /flags/values/{id} row id, for the flags
	// this service actually reads. Required to fall back for FLAGS.
	FlagValueIDs map[string]int64

	// Locales this service serves, e.g. []string{"en-US", "pt-BR"}. Required
	// to fall back for LOCALIZATION, and only usable on a client scoped with
	// Options.MicroserviceID (the endpoint is keyed by microservice).
	Locales []string
}

// flagValueResponse mirrors GET /flags/values/{id}.
type flagValueResponse struct {
	EnvironmentID int64  `json:"environmentId"`
	Value         string `json:"value"`
	Enabled       int64  `json:"enabled"`
	UpdatedAt     string `json:"updatedAt"`
}

// configValueResponse mirrors GET /configs/values/{id}.
type configValueResponse struct {
	MicroserviceID int64           `json:"microserviceId"`
	EnvironmentID  int64           `json:"environmentId"`
	SettingsJSON   json.RawMessage `json:"settingsJson"`
}

// localizationResponse mirrors GET /localization/lookup/{msId}/{envId}/{locale}.
type localizationResponse struct {
	MicroserviceID int64           `json:"microserviceId"`
	EnvironmentID  int64           `json:"environmentId"`
	Locale         string          `json:"locale"`
	BundleJSON     json.RawMessage `json:"bundleJson"`
}

// validate reports a fallback that cannot work, before anything is dialled. New
// calls it whether or not the fallback is reached, because this code path only
// runs on the deployment's worst day: a credential nobody remembered to set has
// to fail at boot, in dev and in CI, rather than months later with KV already
// down and a 401 as the only clue.
func (f *HTTPFallback) validate() error {
	if strings.TrimSpace(f.BaseURL) == "" {
		return fmt.Errorf("configclient: HTTPFallback.BaseURL is required")
	}
	token := f.bearer()
	if token == "" {
		if f.AllowUnauthenticated {
			return nil
		}
		return fmt.Errorf("configclient: HTTPFallback.Token is required: the admin API " +
			"authenticates every route the fallback reads, so a fallback without a token " +
			"can only be answered 401 (set AllowUnauthenticated for a deployment running " +
			"with auth switched off)")
	}
	if !usableHeaderValue(token) {
		return fmt.Errorf("configclient: HTTPFallback.Token cannot be sent as an " +
			"Authorization header value: it must be printable ASCII with no spaces")
	}
	return nil
}

// bearer is the credential as it goes on the wire. Whitespace around a token
// read from a file or an environment variable is not part of it.
func (f *HTTPFallback) bearer() string {
	return strings.TrimSpace(f.Token)
}

// usableHeaderValue reports whether the token can go out as written. A
// credential carrying a stray newline is caught here, where the message can say
// so without quoting the secret, rather than by the transport.
func usableHeaderValue(token string) bool {
	for _, r := range token {
		if r < '!' || r > '~' {
			return false
		}
	}
	return true
}

// missingOwnKeys reports whether the keys this client is scoped to are absent
// from the warm cache, i.e. whether a fallback pass is worth making at all.
func (c *Client) missingOwnKeys(f *HTTPFallback) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if f.ConfigValueID > 0 && c.msID > 0 {
		if _, ok := c.micro[c.msID]; !ok {
			return true
		}
	}
	for _, locale := range f.Locales {
		if _, ok := c.loc[c.msID][locale]; !ok {
			return true
		}
	}
	for flagKey := range f.FlagValueIDs {
		if _, ok := c.flags[flagKey]; !ok {
			return true
		}
	}
	return false
}

// hydrate fills whatever the configured endpoints can reach. Entries already
// in the cache (from KV) win and are not overwritten. It fails when every
// configured fetch failed — so a partial cold start still boots — and when any
// of them was refused on the credential, which is not the kind of failure that
// gets better while the process runs.
func (f *HTTPFallback) hydrate(ctx context.Context, c *Client) error {
	if err := f.validate(); err != nil {
		return err
	}
	timeout := f.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	// A KV warm-up that failed by timing out leaves the caller's context
	// already dead; the fallback still deserves its own budget.
	if ctx.Err() != nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var attempted int
	var errs []error
	var refused bool
	ok := func(err error) {
		attempted++
		if err == nil {
			return
		}
		errs = append(errs, err)
		var se *statusError
		if errors.As(err, &se) && se.refused() {
			refused = true
		}
	}

	for flagKey, id := range f.FlagValueIDs {
		if _, cached := c.FlagValue(flagKey); cached {
			continue
		}
		ok(f.fetchFlag(ctx, c, flagKey, id))
	}
	if f.ConfigValueID > 0 {
		if _, cached := c.MicroSettings(c.msID); !cached || c.msID == 0 {
			ok(f.fetchConfig(ctx, c))
		}
	}
	if len(f.Locales) > 0 && c.msID > 0 {
		for _, locale := range f.Locales {
			if _, cached := c.bundle(c.msID, locale); cached {
				continue
			}
			ok(f.fetchLocalization(ctx, c, locale))
		}
	}

	if attempted == 0 {
		return fmt.Errorf("configclient: HTTP fallback has no reachable targets " +
			"(set ConfigValueID, FlagValueIDs and/or Locales)")
	}
	if len(errs) < attempted {
		c.httpUsed.Store(true)
	}
	// A refused credential is reported even when other endpoints answered:
	// hydrating half the cache and calling that a cold start is how a wrong
	// token comes to look like config that was never published.
	if refused || len(errs) == attempted {
		return fmt.Errorf("configclient: HTTP fallback: %w", errors.Join(errs...))
	}
	if len(errs) > 0 {
		c.setLastErr(fmt.Errorf("configclient: HTTP fallback hydrated partially: %w", errors.Join(errs...)))
	}
	return nil
}

func (f *HTTPFallback) fetchFlag(ctx context.Context, c *Client, flagKey string, id int64) error {
	var r flagValueResponse
	if err := f.get(ctx, c.env, fmt.Sprintf("/flags/values/%d", id), &r); err != nil {
		return fmt.Errorf("configclient: fallback flag %q: %w", flagKey, err)
	}
	if r.EnvironmentID != c.env {
		return fmt.Errorf("configclient: fallback flag %q: environment %d, want %d", flagKey, r.EnvironmentID, c.env)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.flags[flagKey] = FlagPayload{Enabled: r.Enabled != 0, Value: r.Value, UpdatedAt: r.UpdatedAt}
	return nil
}

func (f *HTTPFallback) fetchConfig(ctx context.Context, c *Client) error {
	var r configValueResponse
	if err := f.get(ctx, c.env, fmt.Sprintf("/configs/values/%d", f.ConfigValueID), &r); err != nil {
		return fmt.Errorf("configclient: fallback config %d: %w", f.ConfigValueID, err)
	}
	if r.EnvironmentID != c.env {
		return fmt.Errorf("configclient: fallback config %d: environment %d, want %d", f.ConfigValueID, r.EnvironmentID, c.env)
	}
	if !c.inScope(r.MicroserviceID) {
		return fmt.Errorf("configclient: fallback config %d: microservice %d is out of scope", f.ConfigValueID, r.MicroserviceID)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.micro[r.MicroserviceID] = append(json.RawMessage(nil), r.SettingsJSON...)
	return nil
}

func (f *HTTPFallback) fetchLocalization(ctx context.Context, c *Client, locale string) error {
	path := fmt.Sprintf("/localization/lookup/%d/%d/%s", c.msID, c.env, url.PathEscape(locale))
	var r localizationResponse
	if err := f.get(ctx, c.env, path, &r); err != nil {
		return fmt.Errorf("configclient: fallback localization %s: %w", locale, err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loc[c.msID] == nil {
		c.loc[c.msID] = make(map[string]json.RawMessage)
	}
	c.loc[c.msID][locale] = append(json.RawMessage(nil), r.BundleJSON...)
	return nil
}

// get performs one GET and decodes the JSON body into out. env is the
// environment the caller expects to read, and appears only in the message a
// refusal produces.
func (f *HTTPFallback) get(ctx context.Context, env int64, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(f.BaseURL, "/")+path, nil)
	if err != nil {
		return fmt.Errorf("configclient: build request: %w", err)
	}
	if token := f.bearer(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	hc := f.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("configclient: GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return refusal(resp.StatusCode, path, env)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("configclient: decode %s: %w", path, err)
	}
	return nil
}

// statusError is a non-200 answer from the admin API. It carries the status so
// hydrate can tell a credential the control plane turned away from an endpoint
// that was merely unwell.
type statusError struct {
	status int
	msg    string
}

func (e *statusError) Error() string { return e.msg }

// refused reports whether the API rejected the caller rather than the row.
func (e *statusError) refused() bool {
	return e.status == http.StatusUnauthorized || e.status == http.StatusForbidden
}

// refusal turns a non-200 into the sentence an operator can act on. The three
// auth statuses each mean something different and none of them is "no config
// exists": 401 is a token the deployment does not accept, 403 is one it accepts
// but will not use here, and a read outside a token's environment scope is
// answered 404 by design — identical to a deleted row, so the message has to
// name the other possibility. Nothing here quotes the token.
func refusal(status int, path string, env int64) error {
	e := &statusError{status: status}
	switch status {
	case http.StatusUnauthorized:
		e.msg = fmt.Sprintf("configclient: GET %s: status 401: the HTTP fallback needs a bearer "+
			"token the control plane accepts — set HTTPFallback.Token (every admin API route "+
			"except /health, /livez and /metrics requires one)", path)
	case http.StatusForbidden:
		e.msg = fmt.Sprintf("configclient: GET %s: status 403: the fallback token is not permitted "+
			"to read environment %d — supply one whose scope lists it", path, env)
	case http.StatusNotFound:
		e.msg = fmt.Sprintf("configclient: GET %s: status 404: no such row, or the fallback token "+
			"is not scoped to environment %d — a read outside a token's scope is answered 404 "+
			"rather than 403, so check the scope before concluding the row is gone", path, env)
	default:
		e.msg = fmt.Sprintf("configclient: GET %s: status %d", path, status)
	}
	return e
}
