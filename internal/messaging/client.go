// Package messaging is the distribution plane: the NATS connection, the
// JetStream KV buckets (FLAGS, SERVICESETTINGS, LOCALIZATION), the key layout
// consumers watch, the write-through publisher, and the reconciler.
//
// Keys are environment-prefixed — "{envID}.…" — so a consumer can watch one
// prefix and receive every change relevant to it. keys.go is the authority on
// that shape; consumers in other languages build the same keys by hand from
// docs/CONSUMER_CONTRACT.md, so changing it is a contract change.
//
// The dual write is not transactional. A publish can fail after the database
// has committed, which is what the reconciler exists for: it periodically
// resweeps each domain, republishes what KV is missing or stale, and prunes
// keys whose rows are gone. A sweep that only partially succeeds neither
// prunes nor advances its window, so it retries rather than losing ground.
package messaging

import (
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Config holds the NATS/JetStream connection settings. All values come from
// the application Config (never hardcoded).
type Config struct {
	URL           string // e.g. nats://nats:4222 or comma-separated cluster URLs
	CredsFile     string // optional path to a NATS .creds file (NKEY/JWT)
	Name          string // connection name, shown in NATS monitoring
	MaxReconnect  int    // -1 = unlimited
	ReconnectWait time.Duration
}

// Client bundles a live NATS connection with its JetStream context.
type Client struct {
	Conn *nats.Conn
	JS   jetstream.JetStream
}

// Connect dials NATS and returns a JetStream handle. The connection is
// configured to reconnect indefinitely so a NATS blip does not take the
// service down; consumers reading from KV keep serving cached values anyway.
func Connect(cfg Config) (*Client, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("messaging: NATS URL is required")
	}

	maxReconnect := cfg.MaxReconnect
	if maxReconnect == 0 {
		maxReconnect = -1 // unlimited
	}
	reconnectWait := cfg.ReconnectWait
	if reconnectWait == 0 {
		reconnectWait = 2 * time.Second
	}

	opts := []nats.Option{
		nats.Name(cfg.Name),
		nats.MaxReconnects(maxReconnect),
		nats.ReconnectWait(reconnectWait),
		nats.RetryOnFailedConnect(true),
	}
	if cfg.CredsFile != "" {
		opts = append(opts, nats.UserCredentials(cfg.CredsFile))
	}

	nc, err := nats.Connect(cfg.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("messaging: connect NATS: %w", err)
	}

	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("messaging: init JetStream: %w", err)
	}

	return &Client{Conn: nc, JS: js}, nil
}

// Drain gracefully flushes and closes the connection. Safe on a nil client.
func (c *Client) Drain() error {
	if c == nil || c.Conn == nil {
		return nil
	}
	return c.Conn.Drain()
}
