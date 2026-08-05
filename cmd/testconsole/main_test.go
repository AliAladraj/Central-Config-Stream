package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		client:         up.Client(),
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

// PORT=8090 is the form most tooling uses and used to fail at ListenAndServe;
// PORT=:8090 is a Go listen address and still means every interface, which is
// what the compose stack relies on.
func TestListenAddr(t *testing.T) {
	cases := []struct {
		port string
		bind string
		want string
	}{
		{"8090", "127.0.0.1", "127.0.0.1:8090"},
		{" 8090 ", "127.0.0.1", "127.0.0.1:8090"},
		{"8090", "0.0.0.0", "0.0.0.0:8090"},
		{":8090", "127.0.0.1", ":8090"},
		{"0.0.0.0:8090", "127.0.0.1", "0.0.0.0:8090"},
		{"127.0.0.1:9000", "0.0.0.0", "127.0.0.1:9000"},
		{"8090", "::1", "[::1]:8090"},
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
