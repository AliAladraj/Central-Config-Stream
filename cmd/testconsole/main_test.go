package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// consoleHost is what the browser would have in its address bar, so it is both
// the Host the proxy sees and the origin the console's own page sends.
const consoleHost = "localhost:8090"

// upstream stands in for central-config and records what actually arrived, so
// a test can assert on the request the proxy built rather than only on the
// status it returned.
type upstream struct {
	*httptest.Server
	gotPath  string
	gotQuery string
	gotAuth  string
	gotType  string
	gotBody  string
	calls    int
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.calls++
		u.gotPath = r.URL.Path
		u.gotQuery = r.URL.RawQuery
		u.gotAuth = r.Header.Get("Authorization")
		u.gotType = r.Header.Get("Content-Type")
		u.gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(u.Close)
	return u
}

func newProxy(t *testing.T, up *upstream, allowed ...string) *adminProxy {
	t.Helper()
	return &adminProxy{
		baseURL:        up.URL,
		token:          "local-dev-token",
		allowedOrigins: allowed,
		// The console under test is the one at consoleHost; a case that moves
		// the console elsewhere replaces this.
		hosts:  newHostPolicy(consoleHost),
		client: up.Client(),
	}
}

// call drives the proxy the way a browser would: same Host as the console,
// plus whatever headers the case is about.
func call(t *testing.T, p *adminProxy, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	req.Host = consoleHost
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	p.forward(rec, req)
	return rec
}

// The console's page sends Origin on its writes; a page on any other origin
// sends its own, and that is the whole CSRF signal.
func TestForwardOriginCheck(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		origin  string
		allowed []string
		want    int
	}{
		{"same origin", consoleHost, "http://" + consoleHost, nil, http.StatusOK},
		{"same origin, https scheme", consoleHost, "https://" + consoleHost, nil, http.StatusOK},
		{"same origin, different case", consoleHost, "http://LOCALHOST:8090", nil, http.StatusOK},
		{"absent on a plain GET", consoleHost, "", nil, http.StatusOK},
		{"cross origin", consoleHost, "http://evil.example", nil, http.StatusForbidden},
		{"cross origin on a loopback-looking name", consoleHost, "http://localhost.evil.example", nil, http.StatusForbidden},
		{"opaque origin", consoleHost, "null", nil, http.StatusForbidden},
		{"malformed origin", consoleHost, "not a url", nil, http.StatusForbidden},

		// `npm run dev` serves the UI on :5173 and proxies /api here with Host
		// rewritten, so the origin never matches literally.
		{"vite dev server", "127.0.0.1:8090", "http://localhost:5173", nil, http.StatusOK},
		{"vite dev server on another port", "127.0.0.1:8090", "http://127.0.0.1:5174", nil, http.StatusOK},

		// Bound off loopback, that relaxation does not apply.
		{"loopback origin against an exposed console", "192.168.1.20:8090", "http://localhost:5173", nil, http.StatusForbidden},
		{"exposed console, same origin", "192.168.1.20:8090", "http://192.168.1.20:8090", nil, http.StatusOK},

		{"explicitly allowed", "192.168.1.20:8090", "http://console.internal", []string{"http://console.internal"}, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newUpstream(t)
			p := newProxy(t, up, tc.allowed...)
			// Each case is about Origin, so the console is configured to be
			// reachable as the Host the case sends. Host itself is judged by
			// TestForwardRefusesUnexpectedHost below.
			p.hosts = newHostPolicy(tc.host)

			req := httptest.NewRequest(http.MethodGet, "/api/admin/flags", nil)
			req.Host = tc.host
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			p.forward(rec, req)

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want != http.StatusOK && up.calls != 0 {
				t.Fatalf("refused request still reached upstream")
			}
		})
	}
}

// DNS rebinding is the case the Origin check cannot see: the attacker's page
// controls Host and Origin both, and makes them agree. evil.example resolving
// to 127.0.0.1 is a page on the internet talking to this console, and the only
// header that gives it away is Host — a browser fills it in from the address
// bar, so a rebound page cannot claim to be reaching localhost.
func TestForwardRefusesUnexpectedHost(t *testing.T) {
	cases := []struct {
		name   string
		bound  string // what the console was configured to answer to
		host   string
		origin string
		want   int
	}{
		{"rebound host agreeing with its own origin", consoleHost, "evil.example:8090", "http://evil.example:8090", http.StatusForbidden},
		{"rebound host with no origin at all", consoleHost, "evil.example:8090", "", http.StatusForbidden},
		{"rebound host claiming the console's origin", consoleHost, "evil.example:8090", "http://" + consoleHost, http.StatusForbidden},
		{"rebound host that merely reads as loopback", consoleHost, "localhost.evil.example:8090", "http://localhost.evil.example:8090", http.StatusForbidden},
		{"no host at all", consoleHost, "", "", http.StatusForbidden},

		// Loopback is always answered to, under every name it has, because a
		// console on loopback is reachable as all of them.
		{"loopback by name", consoleHost, "localhost:8090", "http://localhost:8090", http.StatusOK},
		{"loopback by address", consoleHost, "127.0.0.1:8090", "http://127.0.0.1:8090", http.StatusOK},
		{"loopback over IPv6", consoleHost, "[::1]:8090", "http://[::1]:8090", http.StatusOK},

		// Bound to a LAN address, that address is a name it answers to; a
		// rebound one still is not.
		{"the address it is bound to", "192.168.1.20:8090", "192.168.1.20:8090", "http://192.168.1.20:8090", http.StatusOK},
		{"rebound against an exposed console", "192.168.1.20:8090", "evil.example:8090", "http://evil.example:8090", http.StatusForbidden},

		// A wildcard bind names no host, so it grants none: ALLOWED_HOSTS is
		// where the name is said out loud.
		{"wildcard bind grants no name", ":8090", "192.168.1.20:8090", "http://192.168.1.20:8090", http.StatusForbidden},
		{"wildcard bind still answers loopback", ":8090", "localhost:8090", "http://localhost:8090", http.StatusOK},
		{"named explicitly", "console.internal", "console.internal:8090", "http://console.internal:8090", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newUpstream(t)
			p := newProxy(t, up)
			p.hosts = newHostPolicy(tc.bound)

			req := httptest.NewRequest(http.MethodGet, "/api/admin/environments", nil)
			req.Host = tc.host
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			p.forward(rec, req)

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want != http.StatusOK && up.calls != 0 {
				t.Fatalf("refused request still reached upstream wearing the admin token")
			}
		})
	}
}

// The reproduction that mattered: a JSON POST from a rebound page created a row
// with the admin token attached. It has to be refused before the proxy builds
// anything, not merely fail somewhere upstream.
func TestForwardRefusesRebindingWrite(t *testing.T) {
	up := newUpstream(t)
	p := newProxy(t, up)

	// Built by hand rather than through call(), which sends the console's own
	// Host; the whole point here is the Host the attacker's page sends.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/environments", strings.NewReader(`{"name":"pwned"}`))
	req.Host = "evil.example:8090"
	req.Header.Set("Origin", "http://evil.example:8090")
	req.Header.Set("Content-Type", "application/json")
	p.forward(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	if up.calls != 0 {
		t.Fatalf("rebound write reached upstream with the admin token")
	}
	if !strings.Contains(rec.Body.String(), "Host") {
		t.Fatalf("403 body does not say it was the Host: %s", rec.Body.String())
	}
}

// /api/state and /api/events hand over the consumer's whole cache and have no
// Origin check of their own, so the Host guard is all that stands in front of
// them.
func TestGuardHostProtectsConsumerCache(t *testing.T) {
	hosts := newHostPolicy(consoleHost)

	t.Run("state refuses a rebound host", func(t *testing.T) {
		c := &consumer{}
		req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
		req.Host = "evil.example:8090"
		rec := httptest.NewRecorder()
		guardHost(hosts, c.stateHandler)(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("events refuses a rebound host", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
		req.Host = "evil.example:8090"
		rec := httptest.NewRecorder()
		// The stream never returns on its own; refusal happens before it starts.
		guardHost(hosts, sseHandler(newHub()))(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); ct == "text/event-stream" {
			t.Fatal("refused request still opened the event stream")
		}
	})

	t.Run("state answers the console's own host", func(t *testing.T) {
		c := &consumer{}
		req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
		req.Host = consoleHost
		rec := httptest.NewRecorder()
		guardHost(hosts, c.stateHandler)(rec, req)

		// 503, because no consumer is connected in this test — but it is the
		// handler's answer, not the guard's.
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want the handler's 503 (body %s)", rec.Code, rec.Body.String())
		}
	})
}

func TestHostPolicy(t *testing.T) {
	cases := []struct {
		name  string
		names []string
		host  string
		want  bool
	}{
		{"loopback needs no configuration", nil, "127.0.0.1:8090", true},
		{"localhost needs no configuration", nil, "localhost:8090", true},
		{"IPv6 loopback needs no configuration", nil, "[::1]:8090", true},
		{"anything else does", nil, "evil.example:8090", false},
		{"empty host", nil, "", false},

		{"a bound address is a name", []string{"192.168.1.20:8090"}, "192.168.1.20:8090", true},
		{"the port is not part of it", []string{"192.168.1.20:8090"}, "192.168.1.20:9999", true},
		{"a neighbouring address is not", []string{"192.168.1.20:8090"}, "192.168.1.21:8090", false},
		{"an origin contributes its host", []string{"http://console.internal"}, "console.internal", true},
		{"case is not significant", []string{"console.internal"}, "CONSOLE.INTERNAL:8090", true},
		{"a bare name", []string{"console.internal"}, "console.internal:8090", true},

		{"a wildcard bind names nothing", []string{":8090"}, "192.168.1.20:8090", false},
		{"0.0.0.0 names nothing", []string{"0.0.0.0:8090"}, "192.168.1.20:8090", false},
		{"[::] names nothing", []string{"[::]:8090"}, "192.168.1.20:8090", false},
		{"a wildcard bind still answers loopback", []string{"0.0.0.0:8090"}, "127.0.0.1:8090", true},

		{"a suffix of a configured name is not it", []string{"console.internal"}, "evil.console.internal", false},
		{"a name that merely starts with localhost is not loopback", nil, "localhost.evil.example", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := newHostPolicy(tc.names...).permits(tc.host); got != tc.want {
				t.Fatalf("newHostPolicy(%q).permits(%q) = %v, want %v", tc.names, tc.host, got, tc.want)
			}
		})
	}
}

func TestHostname(t *testing.T) {
	cases := []struct{ in, want string }{
		{"localhost:8090", "localhost"},
		{"127.0.0.1", "127.0.0.1"},
		{"[::1]:8090", "::1"},
		{"::1", "::1"},
		{"http://console.internal:8090", "console.internal"},
		{"https://Console.Internal", "console.internal"},
		{" 192.168.1.20:8090 ", "192.168.1.20"},
		{":8090", ""},
		{"", ""},
		{"http://%zz", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := hostname(tc.in); got != tc.want {
				t.Fatalf("hostname(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A cross-origin form post is the case that mattered: it never preflights, so
// the only thing standing between it and an authenticated write is the proxy.
func TestForwardRejectsCrossOriginFormPost(t *testing.T) {
	up := newUpstream(t)
	p := newProxy(t, up)

	rec := call(t, p, http.MethodPost, "/api/admin/flags", `{"name":"pwned"}`, map[string]string{
		"Origin":       "http://evil.example",
		"Content-Type": "text/plain;charset=UTF-8",
	})

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if up.calls != 0 {
		t.Fatalf("cross-origin form post reached upstream")
	}
}

// Content type is forwarded, never invented, so anything a form can send is
// refused even when it arrives with a believable Origin.
func TestForwardContentType(t *testing.T) {
	cases := []struct {
		name        string
		method      string
		contentType string
		body        string
		want        int
	}{
		{"json write", http.MethodPost, "application/json", `{"a":1}`, http.StatusOK},
		{"json with charset", http.MethodPut, "application/json; charset=utf-8", `{"a":1}`, http.StatusOK},
		{"text/plain form post", http.MethodPost, "text/plain;charset=UTF-8", `{"a":1}`, http.StatusUnsupportedMediaType},
		{"urlencoded form post", http.MethodPost, "application/x-www-form-urlencoded", "a=1", http.StatusUnsupportedMediaType},
		{"multipart form post", http.MethodPost, "multipart/form-data; boundary=x", "--x--", http.StatusUnsupportedMediaType},
		{"body without a content type", http.MethodPost, "", `{"a":1}`, http.StatusUnsupportedMediaType},
		{"bodyless delete", http.MethodDelete, "", "", http.StatusOK},
		{"bodyless read", http.MethodGet, "", "", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newUpstream(t)
			p := newProxy(t, up)

			headers := map[string]string{"Origin": "http://" + consoleHost}
			if tc.contentType != "" {
				headers["Content-Type"] = tc.contentType
			}
			rec := call(t, p, tc.method, "/api/admin/flags/values", tc.body, headers)

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want != http.StatusOK {
				if up.calls != 0 {
					t.Fatalf("refused request still reached upstream")
				}
				return
			}
			if tc.contentType != "" && up.gotType != tc.contentType {
				t.Fatalf("upstream content type = %q, want the one that arrived (%q)", up.gotType, tc.contentType)
			}
		})
	}
}

// The allowlist names central-config's surface; anything else is refused here
// rather than reaching the API with the admin token attached.
func TestUpstreamPath(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"/api/admin/flags", "/flags", true},
		{"/api/admin/flags/values", "/flags/values", true},
		{"/api/admin/flags/values/100", "/flags/values/100", true},
		{"/api/admin/configs/values/200", "/configs/values/200", true},
		{"/api/admin/localization/lookup/1/1/en-US", "/localization/lookup/1/1/en-US", true},
		{"/api/admin/environments", "/environments", true},
		{"/api/admin/microservices", "/microservices", true},
		{"/api/admin/health", "/health", true},
		{"/api/admin/livez", "/livez", true},
		{"/api/admin/metrics", "/metrics", true},
		{"/api/admin/inventory", "/inventory", true},
		{"/api/admin/audit", "/audit", true},

		{"/api/admin/", "", false},
		{"/api/admin/debug/pprof", "", false},
		{"/api/admin/flagsomething", "", false},
		{"/api/admin/../flags", "", false},
		{"/api/admin/flags/../../secret", "", false},
		{"/api/state", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := upstreamPath(tc.in)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("upstreamPath(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestForwardRejectsPathOutsideAllowlist(t *testing.T) {
	up := newUpstream(t)
	p := newProxy(t, up)

	rec := call(t, p, http.MethodGet, "/api/admin/debug/pprof/heap", "", map[string]string{
		"Origin": "http://" + consoleHost,
	})

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if up.calls != 0 {
		t.Fatalf("unlisted path reached upstream")
	}
}

// An oversized body is rejected rather than truncated: forwarding the first
// megabyte of a document produced invalid JSON and a confusing upstream 400.
func TestForwardBodyTooLarge(t *testing.T) {
	up := newUpstream(t)
	p := newProxy(t, up)

	oversized := `{"v":"` + strings.Repeat("x", maxBodyBytes) + `"}`
	rec := call(t, p, http.MethodPost, "/api/admin/configs/values", oversized, map[string]string{
		"Origin":       "http://" + consoleHost,
		"Content-Type": "application/json",
	})

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (body %s)", rec.Code, rec.Body.String())
	}
	if up.calls != 0 {
		t.Fatalf("oversized body was forwarded upstream")
	}
	if !strings.Contains(rec.Body.String(), "exceeds") {
		t.Fatalf("413 body does not say why: %s", rec.Body.String())
	}
}

func TestForwardBodyAtLimitIsForwarded(t *testing.T) {
	up := newUpstream(t)
	p := newProxy(t, up)

	body := `{"v":"` + strings.Repeat("x", maxBodyBytes-10) + `"}`
	rec := call(t, p, http.MethodPost, "/api/admin/configs/values", body, map[string]string{
		"Origin":       "http://" + consoleHost,
		"Content-Type": "application/json",
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if up.gotBody != body {
		t.Fatalf("upstream body was not forwarded intact (%d bytes, want %d)", len(up.gotBody), len(body))
	}
}

// Reads are proxied through the same handler as writes, so they carry the token
// too. The API requiring auth on reads must not take the console with it.
func TestForwardAttachesTokenToReadsAndWrites(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			up := newUpstream(t)
			p := newProxy(t, up)

			headers := map[string]string{"Origin": "http://" + consoleHost}
			body := ""
			if method == http.MethodPost || method == http.MethodPut {
				headers["Content-Type"] = "application/json"
				body = `{"a":1}`
			}
			rec := call(t, p, method, "/api/admin/flags/values", body, headers)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
			}
			if up.gotAuth != "Bearer local-dev-token" {
				t.Fatalf("upstream Authorization = %q, want the admin bearer token", up.gotAuth)
			}
		})
	}
}

// The query string carries the list filters, so it has to survive the hop.
func TestForwardPreservesQueryString(t *testing.T) {
	up := newUpstream(t)
	p := newProxy(t, up)

	rec := call(t, p, http.MethodGet, "/api/admin/flags/values?environmentId=1&limit=50", "", map[string]string{
		"Origin": "http://" + consoleHost,
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if up.gotPath != "/flags/values" {
		t.Fatalf("upstream path = %q, want /flags/values", up.gotPath)
	}
	if up.gotQuery != "environmentId=1&limit=50" {
		t.Fatalf("upstream query = %q, want it preserved", up.gotQuery)
	}
}

// A fresh clone has no web/, and the browser is where that has to be explained.
func TestWebHandlerMissingDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "never-built")

	rec := httptest.NewRecorder()
	webHandler(missing).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content type = %q, want HTML", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"cd webui", "npm ci", "npm run build", "WEB_DIR"} {
		if !strings.Contains(body, want) {
			t.Fatalf("page does not mention %q:\n%s", want, body)
		}
	}
}

// Building the UI while the console is already running used to just work,
// because the file server resolved the path per request. It still does.
func TestWebHandlerPicksUpALaterBuild(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "web")
	h := webHandler(dir)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status before the build = %d, want 503", rec.Code)
	}

	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>console</h1>"), 0o600); err != nil {
		t.Fatalf("write index.html: %v", err)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status after the build = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<h1>console</h1>") {
		t.Fatalf("built UI not served after the build: %s", rec.Body.String())
	}
}

func TestWebHandlerServesBuiltUI(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>console</h1>"), 0o600); err != nil {
		t.Fatalf("write index.html: %v", err)
	}

	rec := httptest.NewRecorder()
	webHandler(dir).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<h1>console</h1>") {
		t.Fatalf("built index.html was not served: %s", rec.Body.String())
	}
}

// PORT=8090 is the form most tooling uses and used to fail at ListenAndServe.
// PORT=:8090 says nothing about interfaces either, so BIND_ADDR decides and an
// unset BIND_ADDR keeps it on loopback — it used to mean every interface, which
// is how every documented recipe exposed an admin-token-proxying process.
// Widening it now takes a host said out loud, in PORT or in BIND_ADDR.
func TestListenAddr(t *testing.T) {
	cases := []struct {
		port string
		bind string
		want string
	}{
		{"8090", "127.0.0.1", "127.0.0.1:8090"},
		{" 8090 ", "127.0.0.1", "127.0.0.1:8090"},
		{"8090", "0.0.0.0", "0.0.0.0:8090"},
		{"8090", "", "127.0.0.1:8090"},
		{":8090", "127.0.0.1", "127.0.0.1:8090"},
		{":8090", "", "127.0.0.1:8090"},
		{":8090", "0.0.0.0", "0.0.0.0:8090"},
		{":8090", "192.168.1.20", "192.168.1.20:8090"},
		{"0.0.0.0:8090", "", "0.0.0.0:8090"},
		{"0.0.0.0:8090", "127.0.0.1", "0.0.0.0:8090"},
		{"127.0.0.1:9000", "0.0.0.0", "127.0.0.1:9000"},
		{"8090", "::1", "[::1]:8090"},
		{"[::]:8090", "", "[::]:8090"},
	}
	for _, tc := range cases {
		t.Run(tc.port+"|"+tc.bind, func(t *testing.T) {
			if got := listenAddr(tc.port, tc.bind); got != tc.want {
				t.Fatalf("listenAddr(%q, %q) = %q, want %q", tc.port, tc.bind, got, tc.want)
			}
		})
	}
}

// The normalised address has to be one net.Listen accepts — the bug was that
// a bare port reached it unchanged and failed with "missing port in address".
func TestListenAddrIsBindable(t *testing.T) {
	if _, err := net.Listen("tcp", "8090"); err == nil {
		t.Fatal("expected a bare port to be unusable as a listen address")
	}
	ln, err := net.Listen("tcp", listenAddr("0", "127.0.0.1"))
	if err != nil {
		t.Fatalf("normalised address is not bindable: %v", err)
	}
	defer ln.Close()

	if !strings.HasPrefix(ln.Addr().String(), "127.0.0.1:") {
		t.Fatalf("bound %s, want loopback", ln.Addr())
	}
}

// The documented recipes set PORT=:8090 and no BIND_ADDR. That combination is
// what put an admin-token proxy on every interface, so it is asserted on its
// own: unset means loopback, for the HTTP listener and the embedded NATS server
// alike.
func TestDefaultBindIsLoopback(t *testing.T) {
	if got := listenAddr(":8090", ""); isExposed(got) {
		t.Fatalf("PORT=:8090 with no BIND_ADDR binds %s, which is not loopback", got)
	}
	if got := bindHost(""); got != defaultBindAddr {
		t.Fatalf("bindHost(\"\") = %q, want %q", got, defaultBindAddr)
	}
	if got := bindHost("  "); got != defaultBindAddr {
		t.Fatalf("bindHost(blank) = %q, want %q", got, defaultBindAddr)
	}
	if got := bindHost("0.0.0.0"); got != "0.0.0.0" {
		t.Fatalf("bindHost(%q) = %q, want it honoured", "0.0.0.0", got)
	}
}

// The embedded JetStream server used to be pinned to 0.0.0.0 whatever BIND_ADDR
// said: unauthenticated, writable KV on the LAN, which is the forged-entry
// scenario docs/SECURITY.md warns about. It now goes where it is told.
func TestStartEmbeddedNATSBindsWhereItIsTold(t *testing.T) {
	port := freePort(t)
	srv, storeDir, err := startEmbeddedNATS("nats://127.0.0.1:"+strconv.Itoa(port), defaultBindAddr)
	if err != nil {
		t.Fatalf("start embedded nats: %v", err)
	}
	t.Cleanup(func() {
		srv.Shutdown()
		srv.WaitForShutdown()
		_ = os.RemoveAll(storeDir)
	})

	addr := srv.Addr().String()
	if !isLoopbackHost(addr) {
		t.Fatalf("embedded NATS bound %s, want loopback", addr)
	}
	// Reachable where it was asked to be...
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 5*time.Second)
	if err != nil {
		t.Fatalf("embedded NATS not reachable on loopback: %v", err)
	}
	_ = c.Close()

	// ...and nowhere else. Skipped on a machine with no other address to try.
	lan := lanAddr(t)
	if lan == "" {
		t.Skip("no non-loopback address on this machine to prove it is not listening there")
	}
	if c, err := net.DialTimeout("tcp", net.JoinHostPort(lan, strconv.Itoa(port)), 2*time.Second); err == nil {
		_ = c.Close()
		t.Fatalf("embedded NATS answered on %s, which is not loopback", lan)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// lanAddr returns one of this machine's non-loopback IPv4 addresses, or "" if
// it has none.
func lanAddr(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.To4() == nil {
			continue
		}
		return ipnet.IP.String()
	}
	return ""
}

func TestIsExposed(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8090", false},
		{"localhost:8090", false},
		{"[::1]:8090", false},
		{":8090", true},
		{"0.0.0.0:8090", true},
		{"192.168.1.20:8090", true},
		{"[::]:8090", true},
		{"nonsense", true},
	}
	for _, tc := range cases {
		t.Run(tc.addr, func(t *testing.T) {
			if got := isExposed(tc.addr); got != tc.want {
				t.Fatalf("isExposed(%q) = %v, want %v", tc.addr, got, tc.want)
			}
		})
	}
}

func TestIsLoopbackHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"localhost", true},
		{"localhost:5173", true},
		{"LocalHost:8090", true},
		{"127.0.0.1:8090", true},
		{"127.0.0.53", true},
		{"[::1]:8090", true},
		{"::1", true},
		{"localhost.evil.example", false},
		{"evil.example:8090", false},
		{"192.168.1.20:8090", false},
		{"0.0.0.0:8090", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.host, func(t *testing.T) {
			if got := isLoopbackHost(tc.host); got != tc.want {
				t.Fatalf("isLoopbackHost(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}
}

// Until the consumer connects, /api/state says so rather than the console being
// unreachable altogether.
func TestStateHandlerBeforeConsumerConnects(t *testing.T) {
	c := &consumer{}

	rec := httptest.NewRecorder()
	c.stateHandler(rec, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "connecting") {
		t.Fatalf("body does not say the consumer is still connecting: %s", rec.Body.String())
	}

	c.fail("cannot reach NATS at all")
	rec = httptest.NewRecorder()
	c.stateHandler(rec, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	if !strings.Contains(rec.Body.String(), "cannot reach NATS") {
		t.Fatalf("body does not carry the last failure: %s", rec.Body.String())
	}
}

func TestEnvList(t *testing.T) {
	t.Setenv("ALLOWED_ORIGINS", " http://localhost:5173 , ,http://127.0.0.1:5173")
	got := envList("ALLOWED_ORIGINS")
	want := []string{"http://localhost:5173", "http://127.0.0.1:5173"}
	if len(got) != len(want) {
		t.Fatalf("envList = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("envList = %q, want %q", got, want)
		}
	}
}
