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
// There is no "list flag values for environment X" endpoint on the control
// plane, so a flag fallback that needs no caller-supplied ids is not
// implementable without a new server endpoint. Configure what you have; what
// is left unset is simply not fetched.
type HTTPFallback struct {
	// BaseURL of central-config's HTTP API, e.g. http://central-config:8080.
	BaseURL string

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
// in the cache (from KV) win and are not overwritten. It fails only when every
// configured fetch failed, so a partial cold start still boots.
func (f *HTTPFallback) hydrate(ctx context.Context, c *Client) error {
	if strings.TrimSpace(f.BaseURL) == "" {
		return fmt.Errorf("configclient: HTTPFallback.BaseURL is required")
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
	ok := func(err error) {
		attempted++
		if err != nil {
			errs = append(errs, err)
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
	if len(errs) == attempted {
		return fmt.Errorf("configclient: HTTP fallback: %w", errors.Join(errs...))
	}
	c.httpUsed.Store(true)
	return nil
}

func (f *HTTPFallback) fetchFlag(ctx context.Context, c *Client, flagKey string, id int64) error {
	var r flagValueResponse
	if err := f.get(ctx, fmt.Sprintf("/flags/values/%d", id), &r); err != nil {
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
	if err := f.get(ctx, fmt.Sprintf("/configs/values/%d", f.ConfigValueID), &r); err != nil {
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
	if err := f.get(ctx, path, &r); err != nil {
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

// get performs one GET and decodes the JSON body into out.
func (f *HTTPFallback) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(f.BaseURL, "/")+path, nil)
	if err != nil {
		return fmt.Errorf("configclient: build request: %w", err)
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
		return fmt.Errorf("configclient: GET %s: status %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("configclient: decode %s: %w", path, err)
	}
	return nil
}
