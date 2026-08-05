// Command testconsole is the local test harness for central-config.
//
// It plays the part of a consuming microservice: it holds a live
// configclient cache warmed from JetStream KV, and serves a browser UI that
// shows that cache updating in real time. Writes typed in the UI are proxied
// to central-config's admin endpoints, so a single screen shows the whole
// path: HTTP PUT -> DB -> KV publish -> watch -> consumer memory.
//
// It is a development tool rather than part of the server binary; see
// deploy/compose for how it is wired up alongside central-config.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/ErasedKyte/Central-Config-Stream/pkg/configclient"

	natsserver "github.com/nats-io/nats-server/v2/server"
)

type event struct {
	Bucket   string          `json:"bucket"`
	Key      string          `json:"key"`
	Value    json.RawMessage `json:"value"` // null on delete
	Received string          `json:"receivedAt"`
}

// hub fans KV updates out to every connected browser.
type hub struct {
	mu      sync.Mutex
	clients map[chan event]struct{}
}

func newHub() *hub {
	return &hub{clients: make(map[chan event]struct{})}
}

func (h *hub) subscribe() chan event {
	ch := make(chan event, 32)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *hub) unsubscribe(ch chan event) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
	close(ch)
}

// broadcast never blocks: a browser that cannot keep up drops updates rather
// than stalling the KV watcher goroutine.
func (h *hub) broadcast(e event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- e:
		default:
		}
	}
}

func main() {
	var (
		natsURL   = env("NATS_URL", "nats://localhost:4222")
		envID     = envInt("ENVIRONMENT_ID", 1)
		apiURL    = env("CENTRAL_CONFIG_URL", "http://localhost:8080")
		adminTok  = env("ADMIN_TOKEN", "")
		listen    = env("PORT", ":8090")
		connectTO = 30 * time.Second
	)

	// NATS_EMBEDDED=true runs a JetStream server in this process, so the whole
	// stack can be exercised without Docker. The compose stack leaves it off
	// and uses the real nats container instead.
	if envBool("NATS_EMBEDDED", false) {
		srv, err := startEmbeddedNATS(natsURL)
		if err != nil {
			log.Fatalf("testconsole: embedded nats: %v", err)
		}
		defer srv.Shutdown()
		log.Printf("testconsole: embedded JetStream server listening on %s", srv.ClientURL())
	}

	h := newHub()

	opts := configclient.Options{
		NATSURL:       natsURL,
		EnvironmentID: envID,
		OnChange: func(bucket, key string, value []byte) {
			e := event{Bucket: bucket, Key: key, Received: time.Now().UTC().Format(time.RFC3339Nano)}
			if value != nil {
				e.Value = json.RawMessage(append([]byte(nil), value...))
			}
			h.broadcast(e)
		},
	}

	// The buckets are created by central-config on ITS startup, and when the
	// console hosts the embedded server central-config cannot even connect
	// until this process is up. So retry indefinitely rather than racing:
	// whichever side starts first waits for the other.
	var client *configclient.Client
	for attempt := 1; ; attempt++ {
		attemptCtx, attemptCancel := context.WithTimeout(context.Background(), connectTO)
		var err error
		client, err = configclient.New(attemptCtx, opts)
		attemptCancel()
		if err == nil {
			break
		}
		log.Printf("testconsole: attempt %d — waiting for central-config to create KV buckets (%v)", attempt, err)
		time.Sleep(2 * time.Second)
	}
	defer client.Close()

	proxy := &adminProxy{baseURL: apiURL, token: adminTok}

	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServer(http.Dir(env("WEB_DIR", "web"))))
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, client.Snapshot())
	})
	mux.HandleFunc("GET /api/events", sseHandler(h))
	// Admin calls are proxied so the browser never holds the admin token and
	// no CORS setup is needed.
	// Every method the admin API exposes has to be registered explicitly:
	// ServeMux matches on method, so an unregistered one is a 404 before it
	// ever reaches the proxy.
	mux.HandleFunc("GET /api/admin/", proxy.forward)
	mux.HandleFunc("POST /api/admin/", proxy.forward)
	mux.HandleFunc("PUT /api/admin/", proxy.forward)
	mux.HandleFunc("DELETE /api/admin/", proxy.forward)

	srv := &http.Server{Addr: listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("testconsole: env=%d nats=%s api=%s listening on %s", envID, natsURL, apiURL, listen)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("testconsole: %v", err)
	}
}

func sseHandler(h *hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		ch := h.subscribe()
		defer h.unsubscribe(ch)

		ping := time.NewTicker(20 * time.Second)
		defer ping.Stop()

		for {
			select {
			case <-r.Context().Done():
				return
			case e := <-ch:
				b, err := json.Marshal(e)
				if err != nil {
					continue
				}
				fmt.Fprintf(w, "data: %s\n\n", b)
				flusher.Flush()
			case <-ping.C:
				fmt.Fprint(w, ": ping\n\n")
				flusher.Flush()
			}
		}
	}
}

// adminProxy forwards /api/admin/{path} to central-config's /{path} with the
// admin bearer token attached, preserving the method.
type adminProxy struct {
	baseURL string
	token   string
}

func (p *adminProxy) forward(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	// The query string carries the list filters and paging, so it has to
	// survive the hop — without it every browse view would be unfiltered.
	target := p.baseURL + r.URL.Path[len("/api/admin"):]
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "build request", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	defer resp.Body.Close()

	// Pass the upstream content type through rather than forcing JSON: /metrics
	// is Prometheus text. Retry-After and X-Request-Id are what the UI shows a
	// user when a write is rate limited or has to be reported to an operator.
	for _, name := range []string{"Content-Type", "Retry-After", "X-Request-Id"} {
		if v := resp.Header.Get(name); v != "" {
			w.Header().Set(name, v)
		}
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// startEmbeddedNATS runs an in-process JetStream server on the port from
// natsURL, storing its data under the OS temp dir.
func startEmbeddedNATS(natsURL string) (*natsserver.Server, error) {
	u, err := url.Parse(natsURL)
	if err != nil {
		return nil, fmt.Errorf("parse NATS_URL: %w", err)
	}
	port := 4222
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			port = n
		}
	}

	storeDir, err := os.MkdirTemp("", "testconsole-js")
	if err != nil {
		return nil, fmt.Errorf("store dir: %w", err)
	}

	srv, err := natsserver.NewServer(&natsserver.Options{
		Host:      "0.0.0.0",
		Port:      port,
		JetStream: true,
		StoreDir:  storeDir,
	})
	if err != nil {
		return nil, err
	}
	go srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		return nil, fmt.Errorf("embedded nats not ready")
	}
	return srv, nil
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}

func envInt(key string, fallback int64) int64 {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return fallback
}
