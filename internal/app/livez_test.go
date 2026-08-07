package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/AliAladraj/Central-Config-Stream/internal/database"
	"github.com/AliAladraj/Central-Config-Stream/internal/messaging"

	natsserver "github.com/nats-io/nats-server/v2/test"
)

// Liveness and readiness answer different questions. /health reports the
// dependencies, so a NATS outage rightly takes the pod out of the load
// balancer; pointing the liveness probe at it as well turns that same outage
// into a kill-and-restart loop across every replica, which takes the admin
// API's read paths down too — exactly when an operator needs them to turn a
// flag off. /livez has to keep answering 200 through all of it.
func TestLivezStaysUpWhileHealthReportsNATSDown(t *testing.T) {
	opts := natsserver.DefaultTestOptions
	opts.Port = -1
	opts.JetStream = true
	opts.StoreDir = t.TempDir()
	srv := natsserver.RunServer(&opts)

	db, err := database.NewSQLiteDB("file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "livez.db")))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	client, err := messaging.Connect(messaging.Config{URL: srv.ClientURL(), Name: "livez-test"})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Drain() })

	a := &App{db: db, nats: client}

	// Both probes agree while everything is up.
	if code, body := decodeHealth(t, a); code != http.StatusOK || body["nats"] != "connected" {
		t.Fatalf("health with NATS up = %d %v", code, body)
	}
	if code := decodeLivez(t); code != http.StatusOK {
		t.Fatalf("livez with NATS up = %d", code)
	}

	// The distribution plane goes away.
	srv.Shutdown()
	waitFor(t, "the NATS connection to notice the outage", func() bool {
		return !client.Conn.IsConnected()
	})

	code, body := decodeHealth(t, a)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("health with NATS down = %d, want 503 so the pod leaves the load balancer", code)
	}
	if body["nats"] != "disconnected" || body["database"] != "ok" {
		t.Fatalf("unexpected degraded body: %v", body)
	}

	if code := decodeLivez(t); code != http.StatusOK {
		t.Fatalf("livez with NATS down = %d, want 200 — the process is fine", code)
	}
}

func decodeLivez(t *testing.T) int {
	t.Helper()
	rec := httptest.NewRecorder()
	livezHandler(rec, httptest.NewRequest(http.MethodGet, "/livez", nil))

	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode livez body: %v", err)
	}
	if body["status"] != "alive" {
		t.Errorf("livez body = %v", body)
	}
	return rec.Code
}

// A NATS outage at startup used to be fatal: EnsureBuckets failed and the
// process exited, so every pod crash-looped until NATS came back. The service
// has to boot degraded instead, serve its read paths, and pick the buckets up
// on its own once NATS answers.
func TestAppBootsWithNATSDown(t *testing.T) {
	// A NATS server with JetStream switched off: the connection comes up and
	// provisioning is refused, which is the same boot failure as an outage
	// without the wait for a dial timeout.
	opts := natsserver.DefaultTestOptions
	opts.Port = -1
	srv := natsserver.RunServer(&opts)
	t.Cleanup(srv.Shutdown)

	cfg := &Config{
		DBDriver:          "sqlite",
		DBConnString:      "file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "degraded.db")),
		ServerPort:        ":0",
		NATSURL:           srv.ClientURL(),
		PublishEnabled:    true,
		NATSReplicas:      1,
		ReconcileInterval: time.Hour,
		AdminToken:        "test-token",
	}

	app, err := NewApp(cfg)
	if err != nil {
		t.Fatalf("new app refused to start without NATS: %v", err)
	}
	t.Cleanup(func() {
		if app.stopReconcile != nil {
			app.stopReconcile()
		}
		_ = app.nats.Drain()
		_ = app.db.Close()
	})

	api := httptest.NewServer(app.server.Handler)
	t.Cleanup(api.Close)

	// Liveness is up.
	resp, err := http.Get(api.URL + "/livez")
	if err != nil {
		t.Fatalf("get /livez: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/livez = %d, want 200", resp.StatusCode)
	}

	// And the reads that need nothing from KV keep working.
	flagsReq, err := http.NewRequest(http.MethodGet, api.URL+"/flags/values", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	flagsReq.Header.Set("Authorization", "Bearer test-token")
	flags, err := http.DefaultClient.Do(flagsReq)
	if err != nil {
		t.Fatalf("get /flags/values: %v", err)
	}
	defer flags.Body.Close()
	if flags.StatusCode != http.StatusOK {
		t.Errorf("/flags/values = %d, want 200 — reads do not need NATS", flags.StatusCode)
	}
}
