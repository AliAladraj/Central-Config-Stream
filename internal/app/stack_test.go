package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AliAladraj/Central-Config-Stream/pkg/configclient"

	natsserver "github.com/nats-io/nats-server/v2/test"
)

// TestSQLiteStackEndToEnd runs the compose stack in-process: the SQLite test
// backend, a real JetStream server, the HTTP API, and a consuming client. It
// proves an admin PUT reaches consumer memory through KV without the consumer
// ever querying the database.
func TestSQLiteStackEndToEnd(t *testing.T) {
	opts := natsserver.DefaultTestOptions
	opts.Port = -1
	opts.JetStream = true
	opts.StoreDir = t.TempDir()
	srv := natsserver.RunServer(&opts)
	defer srv.Shutdown()

	cfg := &Config{
		DBDriver:          "sqlite",
		DBConnString:      "file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "test.db")),
		ServerPort:        ":0",
		NATSURL:           srv.ClientURL(),
		PublishEnabled:    true,
		NATSReplicas:      1,
		ReconcileInterval: time.Hour, // startup sweep only; no periodic noise
		AdminToken:        "test-token",
	}

	app, err := NewApp(cfg)
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	t.Cleanup(func() {
		if app.stopReconcile != nil {
			app.stopReconcile()
		}
		_ = app.nats.Drain()
		_ = app.db.Close()
	})

	api := httptest.NewServer(app.server.Handler)
	defer api.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Consumer joins after the startup reconcile, so its cache warms from KV.
	updates := make(chan string, 16)
	cc, err := configclient.New(ctx, configclient.Options{
		NATSURL:       srv.ClientURL(),
		EnvironmentID: 1,
		OnChange: func(bucket, key string, value []byte) {
			select {
			case updates <- bucket + "|" + key:
			default:
			}
		},
	})
	if err != nil {
		t.Fatalf("configclient: %v", err)
	}
	defer cc.Close()

	// The reconciler's startup sweep must have populated the seeded flags.
	waitFor(t, "seeded flag in consumer cache", func() bool {
		v, ok := cc.FlagValue("search_v2")
		return ok && v == "on"
	})

	// Admin write -> DB -> KV publish -> consumer memory.
	body := `{"id":100,"value":"canary","enabled":0}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		api.URL+"/flags/values", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")

	resp, err := api.Client().Do(req)
	if err != nil {
		t.Fatalf("put flag: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put flag: status %d", resp.StatusCode)
	}

	waitFor(t, "updated flag pushed to consumer", func() bool {
		v, ok := cc.FlagValue("search_v2")
		return ok && v == "canary" && !cc.FlagEnabled("search_v2")
	})

	// The watch delivered a FLAGS update for this key.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case got := <-updates:
			if got == "FLAGS|1.search_v2" {
				goto microconfig
			}
		case <-deadline:
			t.Fatal("no FLAGS update delivered to the consumer")
		}
	}

microconfig:
	// Microconfig travels the same path and lands as raw JSON.
	settings := `{"id":200,"microserviceId":1,"environmentId":1,"settingsJson":{"timeoutMs":9000}}`
	mreq, err := http.NewRequestWithContext(ctx, http.MethodPut,
		api.URL+"/configs/values", strings.NewReader(settings))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	mreq.Header.Set("Authorization", "Bearer test-token")
	mreq.Header.Set("Content-Type", "application/json")

	mresp, err := api.Client().Do(mreq)
	if err != nil {
		t.Fatalf("put microconfig: %v", err)
	}
	defer mresp.Body.Close()
	if mresp.StatusCode != http.StatusOK {
		t.Fatalf("put microconfig: status %d", mresp.StatusCode)
	}

	waitFor(t, "microconfig pushed to consumer", func() bool {
		raw, ok := cc.MicroSettings(1)
		if !ok {
			return false
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return false
		}
		return m["timeoutMs"] == float64(9000)
	})
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}
