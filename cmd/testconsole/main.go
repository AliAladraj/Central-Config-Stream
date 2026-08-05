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
//
// The proxy carries a full-scope admin token, so this process is as powerful as
// the token it is given. It therefore binds to loopback, refuses cross-origin
// requests, and forwards only JSON to an explicit list of API paths — a page in
// another tab must not be able to reach through it.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ErasedKyte/Central-Config-Stream/pkg/configclient"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// adminPrefix is stripped before a request is forwarded upstream.
	adminPrefix = "/api/admin"

	// maxBodyBytes caps a proxied request body. The admin API takes small JSON
	// documents; anything larger is a mistake worth naming.
	maxBodyBytes = 1 << 20

	// connectWindow bounds the wait for the rest of the stack to appear. It is
	// generous because the documented workflow starts the two processes in two
	// terminals, and the gap between them is however long it takes to paste a
	// command — but it is finite, so a console that is never going to connect
	// eventually says so instead of retrying into the void.
	connectWindow  = 5 * time.Minute
	connectTimeout = 30 * time.Second
	// Retries back off so a five minute wait is not five minutes of log.
	connectWait    = time.Second
	connectMaxWait = 15 * time.Second
)

// proxiedPaths is the set of central-config path prefixes the console will
// forward. The admin token is attached to everything that gets through here, so
// the surface is named rather than inherited: a path the API does not serve
// never reaches it wearing the token.
var proxiedPaths = map[string]bool{
	"/health":        true,
	"/livez":         true,
	"/metrics":       true,
	"/inventory":     true,
	"/audit":         true,
	"/environments":  true,
	"/microservices": true,
	"/configs":       true,
	"/flags":         true,
	"/localization":  true,
}

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
	if err := run(); err != nil {
		log.Fatalf("testconsole: %v", err)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var (
		natsURL  = env("NATS_URL", "nats://localhost:4222")
		envID    = envInt("ENVIRONMENT_ID", 1)
		apiURL   = env("CENTRAL_CONFIG_URL", "http://localhost:8080")
		adminTok = env("ADMIN_TOKEN", "")
		webDir   = env("WEB_DIR", "web")
		listen   = listenAddr(env("PORT", "8090"), env("BIND_ADDR", "127.0.0.1"))
	)

	// NATS_EMBEDDED=true runs a JetStream server in this process, so the whole
	// stack can be exercised without Docker. The compose stack leaves it off
	// and uses the real nats container instead.
	if envBool("NATS_EMBEDDED", false) {
		srv, storeDir, err := startEmbeddedNATS(natsURL)
		if err != nil {
			return fmt.Errorf("embedded nats: %w", err)
		}
		defer func() {
			srv.Shutdown()
			srv.WaitForShutdown()
			// The store is scratch data for one run of a dev tool; leaving it
			// behind would accumulate a JetStream tree per restart.
			if err := os.RemoveAll(storeDir); err != nil {
				log.Printf("testconsole: could not remove embedded NATS store %s: %v", storeDir, err)
			}
		}()
		log.Printf("testconsole: embedded JetStream server listening on %s", srv.ClientURL())
	}

	h := newHub()
	cons := &consumer{}

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

	proxy := &adminProxy{
		baseURL:        strings.TrimSuffix(apiURL, "/"),
		token:          adminTok,
		allowedOrigins: envList("ALLOWED_ORIGINS"),
		// One client, so proxied calls share a connection pool instead of
		// opening a fresh transport per request.
		client: &http.Client{Timeout: 15 * time.Second},
	}

	mux := http.NewServeMux()
	mux.Handle("GET /", webHandler(webDir))
	mux.HandleFunc("GET /api/state", cons.stateHandler)
	mux.HandleFunc("GET /api/events", sseHandler(h))
	// Admin calls are proxied so the browser never holds the admin token and
	// no CORS setup is needed. Reads go through here too — the token is
	// attached to every method, not just the writing ones.
	// Every method the admin API exposes has to be registered explicitly:
	// ServeMux matches on method, so an unregistered one is a 404 before it
	// ever reaches the proxy.
	mux.HandleFunc("GET /api/admin/", proxy.forward)
	mux.HandleFunc("POST /api/admin/", proxy.forward)
	mux.HandleFunc("PUT /api/admin/", proxy.forward)
	mux.HandleFunc("DELETE /api/admin/", proxy.forward)

	// Bind before anything slow, so a port clash is reported as one rather
	// than surfacing minutes later behind a failed NATS connection.
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", listen, err)
	}
	if isExposed(listen) {
		log.Printf("testconsole: ****************************************************************")
		log.Printf("testconsole: WARNING: listening on %s, which is not loopback.", listen)
		log.Printf("testconsole: This process attaches an admin token to everything it proxies")
		log.Printf("testconsole: to %s. Anyone who can reach this port can change", apiURL)
		log.Printf("testconsole: configuration for every service watching it.")
		log.Printf("testconsole: Set BIND_ADDR=127.0.0.1 (or PORT=8090) to keep it local.")
		log.Printf("testconsole: ****************************************************************")
	}

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// Request contexts derive from this one, so a signal ends the SSE
		// streams too. They are open by design and never finish on their own;
		// without this, shutdown would wait out its whole timeout on them.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	serveErr := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	// The consumer connects behind the server rather than in front of it: the
	// console is then reachable while NATS is still coming up, and can say the
	// consumer is not connected instead of being invisible.
	go cons.connect(ctx, opts)

	log.Printf("testconsole: env=%d nats=%s api=%s listening on %s", envID, natsURL, apiURL, ln.Addr())

	select {
	case <-ctx.Done():
		log.Printf("testconsole: shutting down")
	case err := <-serveErr:
		if err != nil {
			return err
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("testconsole: shutdown: %v", err)
	}
	cons.close()
	return nil
}

// consumer holds the configclient once it has managed to connect, and the
// reason it has not when it hasn't.
type consumer struct {
	mu      sync.Mutex
	client  *configclient.Client
	lastErr string
}

func (c *consumer) set(client *configclient.Client) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.client = client
	c.lastErr = ""
}

func (c *consumer) fail(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastErr = reason
}

func (c *consumer) get() (*configclient.Client, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.client, c.lastErr
}

func (c *consumer) close() {
	c.mu.Lock()
	client := c.client
	c.client = nil
	c.mu.Unlock()
	if client != nil {
		_ = client.Close()
	}
}

// connect warms the cache, retrying while the other half of the stack starts.
//
// The buckets are created by central-config on ITS startup, and when the
// console hosts the embedded server central-config cannot even connect until
// this process is up. So whichever side starts first waits for the other —
// but only for a bounded time, because a console that is never going to
// connect is more useful saying so than retrying in silence.
func (c *consumer) connect(ctx context.Context, opts configclient.Options) {
	deadline := time.Now().Add(connectWindow)
	wait := connectWait

	for attempt := 1; ; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, connectTimeout)
		client, err := configclient.New(attemptCtx, opts)
		cancel()
		if err == nil {
			c.set(client)
			log.Printf("testconsole: consumer cache warm, watching environment %d", opts.EnvironmentID)
			return
		}
		if ctx.Err() != nil {
			return
		}

		reason := diagnose(err)
		if time.Now().After(deadline) {
			c.fail("gave up connecting after " + connectWindow.String() + ": " + reason)
			log.Printf("testconsole: giving up after %s and %d attempts (%s). The console stays up and reports "+
				"the consumer as disconnected; start NATS and central-config — or set NATS_EMBEDDED=true — and "+
				"restart this process.", connectWindow, attempt, reason)
			return
		}
		c.fail(reason)
		log.Printf("testconsole: consumer not connected (attempt %d): %s — %v", attempt, reason, err)

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if wait *= 2; wait > connectMaxWait {
			wait = connectMaxWait
		}
	}
}

// diagnose says which of the two very different first-run failures happened.
// "No NATS at all" and "NATS is up but central-config has not created the
// buckets yet" look identical from here and need opposite fixes.
func diagnose(err error) string {
	switch {
	case errors.Is(err, jetstream.ErrBucketNotFound), errors.Is(err, jetstream.ErrStreamNotFound):
		return "NATS is reachable but the KV buckets do not exist yet; waiting for central-config to create them"
	case errors.Is(err, nats.ErrNoServers), errors.Is(err, nats.ErrConnectionClosed),
		errors.Is(err, context.DeadlineExceeded), errors.Is(err, nats.ErrTimeout):
		return "cannot reach NATS at all; is it running, or should NATS_EMBEDDED=true be set?"
	default:
		return "cannot warm the consumer cache"
	}
}

func (c *consumer) stateHandler(w http.ResponseWriter, r *http.Request) {
	client, lastErr := c.get()
	if client == nil {
		if lastErr == "" {
			lastErr = "the consumer is still connecting to NATS"
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": lastErr})
		return
	}
	writeJSON(w, http.StatusOK, client.Snapshot())
}

// webHandler serves the built UI, and when it has not been built serves a page
// that says how to build it. web/ is gitignored, so a fresh clone has none, and
// the failure used to be a bare 404 at the URL the README tells you to open.
func webHandler(dir string) http.Handler {
	files := http.FileServer(http.Dir(dir))
	if isDir(dir) {
		return files
	}

	log.Printf("testconsole: WARNING: web directory %q does not exist, so there is no UI to serve. "+
		"Build it with: cd webui && npm ci && npm run build", dir)
	// Re-checked per request rather than decided once at startup, so building
	// the UI in the other terminal is picked up by a reload.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isDir(dir) {
			files.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, notBuiltPage(dir))
	})
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func notBuiltPage(dir string) string {
	return `<!doctype html>
<html lang="en">
<meta charset="utf-8">
<title>testconsole — UI not built</title>
<style>
  body { font: 15px/1.6 ui-monospace, SFMono-Regular, Menlo, monospace; margin: 4rem auto; max-width: 44rem; padding: 0 1.5rem; }
  h1 { font-size: 1.3rem; }
  pre { background: #f4f4f5; border-left: 3px solid #999; padding: 1rem; overflow-x: auto; }
  code { background: #f4f4f5; padding: 0 .25em; }
</style>
<h1>The console UI has not been built yet</h1>
<p>The Go console is running, but there is no <code>` + html.EscapeString(dir) + `</code> directory to
serve the compiled React app from. It is gitignored, so a fresh clone has none.</p>
<pre>cd webui &amp;&amp; npm ci &amp;&amp; npm run build</pre>
<p>That writes <code>web/</code>. Reload this page when it finishes — there is no need to
restart the console. Set <code>WEB_DIR</code> to serve from somewhere else.</p>
<p>The API endpoints are already live: <code>/api/state</code> is the consumer's cache and
<code>/api/events</code> is the KV push stream.</p>
</html>
`
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
	baseURL        string
	token          string
	allowedOrigins []string
	client         *http.Client
}

func (p *adminProxy) forward(w http.ResponseWriter, r *http.Request) {
	if !p.sameOrigin(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "cross-origin request refused; the console proxies an admin token and only answers its own page",
		})
		return
	}

	upstream, ok := upstreamPath(r.URL.Path)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "not a proxied central-config path",
		})
		return
	}

	if !isJSONRequest(r) {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{
			"error": "the proxy forwards application/json only",
		})
		return
	}

	// MaxBytesReader rather than LimitReader: a truncated body would be
	// forwarded as invalid JSON and come back as a puzzling upstream 400.
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
				"error": fmt.Sprintf("request body exceeds %d bytes", maxBodyBytes),
			})
			return
		}
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}

	// The query string carries the list filters and paging, so it has to
	// survive the hop — without it every browse view would be unfiltered.
	target := p.baseURL + upstream
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "build request", http.StatusInternalServerError)
		return
	}
	// Pass the content type through rather than declaring one: rewriting it is
	// what turned a cross-origin form post — which no browser preflights — into
	// an authenticated admin write.
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	// Reads are authenticated the same as writes. The API may require a token
	// on either, and the browser has none, so the proxy is the only place it
	// can be attached.
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}

	resp, err := p.client.Do(req)
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

// sameOrigin decides whether r may be treated as coming from the console's own
// page.
//
// A browser sends Origin on every cross-origin request and on same-origin
// writes, so a mismatch is the CSRF signal. It omits the header on a plain
// same-origin GET — and so does curl — which is why absence is allowed rather
// than rejected: the header is only judged when it is there.
//
// Host is compared rather than the whole origin, because the console is plain
// HTTP and has no scheme to be sure of.
//
// One origin is accepted that is not literally the console's own: a page served
// from loopback, when the console is itself answering on loopback. `npm run
// dev` puts the UI on :5173 and proxies /api here with Host rewritten to the
// console, so the two never match on their own. Loopback is the one place that
// relaxation is safe — the pages this check exists to stop are served from the
// internet, and none of them can claim to be on this machine. Bind the console
// anywhere else and the comparison goes back to being exact.
func (p *adminProxy) sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	if isLoopbackHost(u.Host) && isLoopbackHost(r.Host) {
		return true
	}
	for _, allowed := range p.allowedOrigins {
		if strings.EqualFold(allowed, origin) {
			return true
		}
	}
	return false
}

// upstreamPath maps a console path to the central-config path it forwards to,
// refusing anything outside proxiedPaths. The whole path is cleaned before the
// prefix is matched, so a traversal attempt is judged as the path it resolves
// to — one that walks out of /api/admin is no longer an admin path.
func upstreamPath(consolePath string) (string, bool) {
	cleaned := path.Clean("/" + strings.TrimPrefix(consolePath, "/"))
	if !strings.HasPrefix(cleaned, adminPrefix+"/") {
		return "", false
	}
	rest := cleaned[len(adminPrefix):]
	first := rest
	if i := strings.Index(rest[1:], "/"); i >= 0 {
		first = rest[:i+1]
	}
	if !proxiedPaths[first] {
		return "", false
	}
	return rest, true
}

// isJSONRequest reports whether the request is one the proxy will forward. A
// cross-origin form post can only declare urlencoded, multipart or text/plain,
// none of which get past this — which is the second half of the CSRF defence,
// independent of the Origin header being present.
func isJSONRequest(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		// The UI's reads and deletes send no body and no content type.
		return r.ContentLength == 0
	}
	mediaType, _, err := mime.ParseMediaType(ct)
	return err == nil && mediaType == "application/json"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// startEmbeddedNATS runs an in-process JetStream server on the port from
// natsURL, storing its data under the OS temp dir. The store directory is
// returned so the caller can remove it on the way out.
func startEmbeddedNATS(natsURL string) (*natsserver.Server, string, error) {
	u, err := url.Parse(natsURL)
	if err != nil {
		return nil, "", fmt.Errorf("parse NATS_URL: %w", err)
	}
	port := 4222
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			port = n
		}
	}

	storeDir, err := os.MkdirTemp("", "testconsole-js")
	if err != nil {
		return nil, "", fmt.Errorf("store dir: %w", err)
	}

	srv, err := natsserver.NewServer(&natsserver.Options{
		Host:      "0.0.0.0",
		Port:      port,
		JetStream: true,
		StoreDir:  storeDir,
		// Without this the embedded server installs its own SIGINT handler,
		// which calls os.Exit and takes the process down before this one has
		// closed the consumer or removed the store directory below.
		NoSigs: true,
	})
	if err != nil {
		_ = os.RemoveAll(storeDir)
		return nil, "", err
	}
	go srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		srv.Shutdown()
		_ = os.RemoveAll(storeDir)
		return nil, "", fmt.Errorf("embedded nats not ready")
	}
	return srv, storeDir, nil
}

// listenAddr resolves the address to bind from PORT and BIND_ADDR.
//
// A bare "8090" is the form most tooling uses and says nothing about which
// interfaces to expose, so BIND_ADDR — loopback by default — supplies the host.
// A value containing a colon is a Go listen address and is honoured as written,
// including ":8090" for every interface, which is what the compose stack sets
// so its published port is reachable from the host.
func listenAddr(port, bind string) string {
	port = strings.TrimSpace(port)
	if strings.Contains(port, ":") {
		return port
	}
	return net.JoinHostPort(bind, port)
}

// isExposed reports whether addr accepts connections from beyond this machine.
func isExposed(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return true
	}
	return !isLoopbackHost(host)
}

// isLoopbackHost reports whether host — either "host" or "host:port" — names
// this machine's loopback interface.
func isLoopbackHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// envList reads a comma-separated variable, dropping blanks.
func envList(key string) []string {
	var out []string
	for _, part := range strings.Split(os.Getenv(key), ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
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
