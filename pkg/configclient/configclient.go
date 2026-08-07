// Package configclient is the consumer-side library for central-config.
//
// A microservice imports this to read configuration without ever touching the
// database. It warms an in-memory cache from JetStream KV, keeps it live with a
// Watch, and serves reads from memory (no network per read, survives NATS
// blips). The central-config database remains the source of truth.
//
// Scope: set Options.MicroserviceID. The client then watches only the keys the
// service actually needs — every flag in its environment (flags are env-wide by
// design), its own SERVICESETTINGS key, and its own locale bundles. Leaving it unset
// selects the fleet-wide watch; see Options.MicroserviceID.
//
// Cold start: if NATS is unreachable at boot, Options.HTTPFallback can hydrate
// the cache once from central-config's HTTP GET endpoints. It is off by default
// and never sits on the read path — see httpfallback.go for what it can and
// cannot reach, and for the bearer token those endpoints require.
package configclient

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	bucketFlags           = "FLAGS"
	bucketServiceSettings = "SERVICESETTINGS"
	bucketLocalization    = "LOCALIZATION"
)

// FlagPayload mirrors the value central-config stores in the FLAGS bucket.
type FlagPayload struct {
	Enabled   bool   `json:"enabled"`
	Value     string `json:"value"`
	UpdatedAt string `json:"updatedAt"`
}

// Options configures the client.
type Options struct {
	NATSURL       string // required, e.g. nats://nats:4222
	NATSCreds     string // optional path to a .creds file
	EnvironmentID int64  // required, the environment this service runs in

	// MicroserviceID is the service this client runs inside. When set, the
	// client watches only "{env}.{id}" on SERVICESETTINGS and "{env}.{id}.>" on
	// LOCALIZATION, so its memory grows with its own config rather than with
	// the size of the fleet. FLAGS is still watched env-wide because flags are
	// shared across services.
	//
	// Leaving it at 0 selects a fleet-wide watch: every service's appsettings
	// and every locale bundle in the environment. That is a deliberate escape
	// hatch for diagnostic consumers (the test console, admin views) that
	// genuinely want to observe the whole environment — it is the wrong setting
	// for a normal service, and Status().FleetWide reports it so the choice is
	// visible rather than silent. It is the zero value because the field is
	// optional, not because it is the setting to reach for.
	//
	// When it is set, ServiceSettings and Translate for any *other* microservice
	// return false (that data is not watched) and increment
	// Status().OutOfScopeReads, so an out-of-scope read shows up as a reported
	// mistake instead of an empty result that looks like "no config yet".
	MicroserviceID int64

	// HTTPFallback, if set, hydrates the cache from central-config's HTTP API
	// when JetStream is unreachable at boot or a scoped key is missing from KV.
	// Optional and boot-only; see HTTPFallback for its limits. It carries its
	// own credential and New rejects one that cannot work, whether or not the
	// fallback ends up being needed.
	HTTPFallback *HTTPFallback

	// Logger, if set, receives the client's diagnostics: watch failures, the
	// fallback being used, a malformed payload. It is optional on purpose —
	// this is a library, and a library that logs by default is a library that
	// prints into somebody else's log stream. Unset means silent, with the same
	// information still available through Status().
	Logger *slog.Logger

	// OnChange, if set, is called after each applied KV update: during the
	// initial snapshot and for every later push. bucket is FLAGS, SERVICESETTINGS
	// or LOCALIZATION; key is the full KV key; value is nil for a delete.
	// It runs on the watcher goroutine, so it must not block.
	//
	// Delivery is at-least-once, and a delivery does not mean the value
	// changed. The same value arrives again whenever the watcher reconnects
	// (the ordered consumer replays the current value of every watched key) and
	// whenever the control plane genuinely republishes it. Handlers must
	// therefore be idempotent: doing real work per call — rebuilding an HTTP
	// client, resetting a log level, invalidating a cache — must be safe to
	// repeat with an unchanged value. Compare against what you already hold if
	// the work is expensive.
	OnChange func(bucket, key string, value []byte)
}

// Client holds live, watched caches for one environment.
type Client struct {
	env  int64
	msID int64 // 0 = fleet-wide scope

	// nc and watch are swapped by Close/the fallback path while Status may be
	// reading them, so they live under mu with the caches.
	nc    *nats.Conn
	watch bool // true once the KV watchers are running

	mu    sync.RWMutex
	flags map[string]FlagPayload               // flagKey -> payload
	micro map[int64]json.RawMessage            // microserviceID -> settings
	loc   map[int64]map[string]json.RawMessage // microserviceID -> locale -> bundle

	lastUpdate atomic.Int64 // unix nanos of the last applied KV entry
	outOfScope atomic.Int64 // reads for a microservice this client does not watch
	httpUsed   atomic.Bool  // the HTTP fallback hydrated part of the cache
	lastErr    atomic.Pointer[string]

	onChange func(bucket, key string, value []byte)
	log      *slog.Logger
	cancel   context.CancelFunc
}

// New connects, warms the caches, and starts watching. It blocks until the
// initial values for the client's scope have been loaded.
//
// If JetStream is unreachable and Options.HTTPFallback is set, New hydrates
// what it can over HTTP and returns a client whose Status().Watching is false —
// i.e. a running-on-a-cold-snapshot client that will never see updates. Without
// a fallback configured, that case is still an error.
//
// A fallback that could not work — no BaseURL, or no credential for an API that
// requires one — is an error here even when JetStream is healthy and the
// fallback is never reached.
func New(ctx context.Context, opts Options) (*Client, error) {
	if opts.NATSURL == "" {
		return nil, fmt.Errorf("configclient: NATSURL is required")
	}
	if opts.EnvironmentID <= 0 {
		return nil, fmt.Errorf("configclient: EnvironmentID is required")
	}
	if opts.MicroserviceID < 0 {
		return nil, fmt.Errorf("configclient: MicroserviceID must not be negative")
	}
	if opts.HTTPFallback != nil {
		if err := opts.HTTPFallback.validate(); err != nil {
			return nil, err
		}
	}

	c := &Client{
		env:      opts.EnvironmentID,
		msID:     opts.MicroserviceID,
		flags:    make(map[string]FlagPayload),
		micro:    make(map[int64]json.RawMessage),
		loc:      make(map[int64]map[string]json.RawMessage),
		onChange: opts.OnChange,
		log:      opts.Logger,
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}

	if err := c.connectAndWatch(ctx, opts); err != nil {
		c.setLastErr(err)
		if opts.HTTPFallback == nil {
			c.Close()
			return nil, err
		}
		// Cold start with NATS down: drop the half-open connection (nothing is
		// watching it) and serve what the HTTP API can give us rather than
		// booting with no config at all. The Drain error is discarded on
		// purpose — the connection is being abandoned either way, and failing
		// the whole warm-up over it would defeat the point of the fallback.
		_ = c.closeConn()
		if ferr := opts.HTTPFallback.hydrate(ctx, c); ferr != nil {
			return nil, fmt.Errorf("configclient: KV warm-up failed (%v) and HTTP fallback failed: %w", err, ferr)
		}
		return c, nil
	}

	// Warm but incomplete: this service's own keys may not be in KV yet.
	if opts.HTTPFallback != nil && c.missingOwnKeys(opts.HTTPFallback) {
		if ferr := opts.HTTPFallback.hydrate(ctx, c); ferr != nil {
			c.setLastErr(ferr)
		}
	}
	return c, nil
}

// connectAndWatch dials NATS and opens the three watchers, leaving the client
// warm on success. On failure the client keeps whatever it managed to load.
func (c *Client) connectAndWatch(ctx context.Context, opts Options) error {
	natsOpts := []nats.Option{
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2 * time.Second),
		nats.RetryOnFailedConnect(true),
	}
	if opts.NATSCreds != "" {
		natsOpts = append(natsOpts, nats.UserCredentials(opts.NATSCreds))
	}
	nc, err := nats.Connect(opts.NATSURL, natsOpts...)
	if err != nil {
		return fmt.Errorf("configclient: connect NATS: %w", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return fmt.Errorf("configclient: init JetStream: %w", err)
	}

	watchCtx, cancel := context.WithCancel(context.Background())
	c.mu.Lock()
	c.nc = nc
	c.cancel = cancel
	c.mu.Unlock()

	// FLAGS stays env-wide: flags are shared by every service in the
	// environment. The per-service buckets are narrowed to this service's own
	// keys when a MicroserviceID is set.
	envPrefix := fmt.Sprintf("%d.>", c.env)
	microPrefix, locPrefix := envPrefix, envPrefix
	if c.msID > 0 {
		microPrefix = fmt.Sprintf("%d.%d", c.env, c.msID)
		locPrefix = fmt.Sprintf("%d.%d.>", c.env, c.msID)
	}

	if err := c.startWatch(ctx, watchCtx, js, bucketFlags, envPrefix, c.applyFlag); err != nil {
		return err
	}
	if err := c.startWatch(ctx, watchCtx, js, bucketServiceSettings, microPrefix, c.applyServiceSettings); err != nil {
		return err
	}
	if err := c.startWatch(ctx, watchCtx, js, bucketLocalization, locPrefix, c.applyLoc); err != nil {
		return err
	}

	c.mu.Lock()
	c.watch = true
	c.mu.Unlock()
	return nil
}

// inScope reports whether microserviceID is one this client watches.
func (c *Client) inScope(microserviceID int64) bool {
	return c.msID == 0 || c.msID == microserviceID
}

func (c *Client) setLastErr(err error) {
	if err == nil {
		return
	}
	s := err.Error()
	c.lastErr.Store(&s)
	c.log.Warn("configclient error", slog.String("error.message", s))
}

// startWatch opens a KV watcher on prefix, drains the initial snapshot
// synchronously (so the cache is warm on return), then keeps applying updates
// in the background.
func (c *Client) startWatch(
	initCtx, runCtx context.Context,
	js jetstream.JetStream,
	bucket, prefix string,
	apply func(key string, value []byte),
) error {
	kv, err := js.KeyValue(initCtx, bucket)
	if err != nil {
		return fmt.Errorf("configclient: open bucket %s: %w", bucket, err)
	}
	w, err := kv.Watch(runCtx, prefix)
	if err != nil {
		return fmt.Errorf("configclient: watch %s: %w", bucket, err)
	}

	// applyEntry updates the cache and then notifies the optional observer.
	applyEntry := func(entry jetstream.KeyValueEntry) {
		var value []byte
		if entry.Operation() == jetstream.KeyValuePut {
			value = entry.Value()
		}
		apply(entry.Key(), value)
		c.lastUpdate.Store(time.Now().UnixNano())
		c.log.Debug("kv update applied",
			slog.String("bucket", bucket),
			slog.String("kv.key", entry.Key()),
			slog.Bool("deleted", value == nil))
		if c.onChange != nil {
			c.onChange(bucket, entry.Key(), value)
		}
	}

	// Drain the initial replay until the "all current values delivered" marker
	// (a nil entry) arrives.
	for {
		select {
		case <-initCtx.Done():
			return initCtx.Err()
		case entry := <-w.Updates():
			if entry == nil {
				goto background // initial snapshot complete
			}
			applyEntry(entry)
		}
	}

background:
	go func() {
		for entry := range w.Updates() {
			if entry == nil {
				continue
			}
			applyEntry(entry)
		}
	}()
	return nil
}

func (c *Client) trimEnv(key string) string {
	return strings.TrimPrefix(key, fmt.Sprintf("%d.", c.env))
}

func (c *Client) applyFlag(key string, value []byte) {
	flagKey := c.trimEnv(key)
	c.mu.Lock()
	defer c.mu.Unlock()
	if value == nil {
		delete(c.flags, flagKey)
		return
	}
	var p FlagPayload
	if err := json.Unmarshal(value, &p); err != nil {
		c.log.Warn("malformed flag payload",
			slog.String("kv.key", key), slog.String("error.message", err.Error()))
		return
	}
	c.flags[flagKey] = p
}

func (c *Client) applyServiceSettings(key string, value []byte) {
	msID, err := strconv.ParseInt(c.trimEnv(key), 10, 64)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if value == nil {
		delete(c.micro, msID)
		return
	}
	c.micro[msID] = append(json.RawMessage(nil), value...)
}

func (c *Client) applyLoc(key string, value []byte) {
	rest := c.trimEnv(key) // "{msID}.{locale}"
	parts := strings.SplitN(rest, ".", 2)
	if len(parts) != 2 {
		return
	}
	msID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return
	}
	locale := parts[1]
	c.mu.Lock()
	defer c.mu.Unlock()
	if value == nil {
		if m := c.loc[msID]; m != nil {
			delete(m, locale)
		}
		return
	}
	if c.loc[msID] == nil {
		c.loc[msID] = make(map[string]json.RawMessage)
	}
	c.loc[msID][locale] = append(json.RawMessage(nil), value...)
}

// ---- Reads (served from the in-memory cache) ----

// FlagEnabled reports whether a flag is on. Unknown flags are false.
func (c *Client) FlagEnabled(flagKey string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.flags[flagKey].Enabled
}

// FlagValue returns a flag's string value and whether it was found.
func (c *Client) FlagValue(flagKey string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	p, ok := c.flags[flagKey]
	return p.Value, ok
}

// ServiceSettings returns the appsettings JSON blob for a microservice. If the
// client is scoped to one microservice (Options.MicroserviceID) and a different
// one is asked for, this returns false and counts an out-of-scope read: that
// data is not watched, so any cached copy would be a lie.
func (c *Client) ServiceSettings(microserviceID int64) (json.RawMessage, bool) {
	if !c.inScope(microserviceID) {
		c.outOfScope.Add(1)
		return nil, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.micro[microserviceID]
	return v, ok
}

// Translate resolves a single localization key for a service+locale. Returns
// the translated string and whether it was found. Out-of-scope microservices
// are reported the same way as in ServiceSettings.
func (c *Client) Translate(microserviceID int64, locale, key string) (string, bool) {
	if !c.inScope(microserviceID) {
		c.outOfScope.Add(1)
		return "", false
	}
	c.mu.RLock()
	bundle, ok := c.loc[microserviceID][locale]
	c.mu.RUnlock()
	if !ok {
		return "", false
	}
	var m map[string]string
	if err := json.Unmarshal(bundle, &m); err != nil {
		return "", false
	}
	v, ok := m[key]
	return v, ok
}

// bundle returns the raw locale bundle for a service, if cached.
func (c *Client) bundle(microserviceID int64, locale string) (json.RawMessage, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.loc[microserviceID][locale]
	return v, ok
}

// Snapshot is a point-in-time copy of everything the client has cached for its
// environment. Intended for diagnostics and admin views, not the hot path.
type Snapshot struct {
	EnvironmentID   int64                                `json:"environmentId"`
	Flags           map[string]FlagPayload               `json:"flags"`
	ServiceSettings map[int64]json.RawMessage            `json:"serviceSettings"`
	Localization    map[int64]map[string]json.RawMessage `json:"localization"`
}

// Snapshot returns a copy of the current cache.
func (c *Client) Snapshot() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()

	s := Snapshot{
		EnvironmentID:   c.env,
		Flags:           make(map[string]FlagPayload, len(c.flags)),
		ServiceSettings: make(map[int64]json.RawMessage, len(c.micro)),
		Localization:    make(map[int64]map[string]json.RawMessage, len(c.loc)),
	}
	for k, v := range c.flags {
		s.Flags[k] = v
	}
	for k, v := range c.micro {
		s.ServiceSettings[k] = v
	}
	for msID, locales := range c.loc {
		copied := make(map[string]json.RawMessage, len(locales))
		for locale, bundle := range locales {
			copied[locale] = bundle
		}
		s.Localization[msID] = copied
	}
	return s
}

// Status describes how healthy the cache is. It exists so a consumer can answer the
// two questions that matter after an incident: am I still being pushed updates,
// and where did this config come from?
type Status struct {
	EnvironmentID    int64  `json:"environmentId"`
	MicroserviceID   int64  `json:"microserviceId"` // 0 when fleet-wide
	FleetWide        bool   `json:"fleetWide"`      // watching every service's config
	Connected        bool   `json:"connected"`      // NATS connection is up right now
	Watching         bool   `json:"watching"`       // KV watchers are running
	UsedHTTPFallback bool   `json:"usedHttpFallback"`
	LastUpdate       string `json:"lastUpdate,omitempty"` // RFC3339 of the last applied KV entry
	StaleFor         string `json:"staleFor,omitempty"`   // time since that update
	LastError        string `json:"lastError,omitempty"`
	OutOfScopeReads  int64  `json:"outOfScopeReads"`
	Counts           struct {
		Flags           int `json:"flags"`
		ServiceSettings int `json:"serviceSettings"`
		Localization    int `json:"localization"`
	} `json:"counts"`
}

// Status reports the client's current health. Cheap enough to expose from a
// /healthz handler.
func (c *Client) Status() Status {
	s := Status{
		EnvironmentID:    c.env,
		MicroserviceID:   c.msID,
		FleetWide:        c.msID == 0,
		UsedHTTPFallback: c.httpUsed.Load(),
		OutOfScopeReads:  c.outOfScope.Load(),
	}
	if ns := c.lastUpdate.Load(); ns > 0 {
		t := time.Unix(0, ns).UTC()
		s.LastUpdate = t.Format(time.RFC3339Nano)
		s.StaleFor = time.Since(t).Truncate(time.Second).String()
	}
	if p := c.lastErr.Load(); p != nil {
		s.LastError = *p
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	s.Connected = c.nc != nil && c.nc.IsConnected()
	s.Watching = c.watch
	s.Counts.Flags = len(c.flags)
	s.Counts.ServiceSettings = len(c.micro)
	s.Counts.Localization = len(c.loc)
	return s
}

// Close stops all watchers and closes the connection.
func (c *Client) Close() error {
	return c.closeConn()
}

// closeConn tears down the watchers and the NATS connection, leaving the cache
// intact and Status().Connected false.
func (c *Client) closeConn() error {
	c.mu.Lock()
	cancel, nc := c.cancel, c.nc
	c.cancel, c.nc, c.watch = nil, nil, false
	c.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if nc != nil {
		return nc.Drain()
	}
	return nil
}
