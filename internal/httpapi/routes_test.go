package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/a840817a/sluice/internal/config"
)

// Every other test in this package drives handlers directly through
// executeRequest, which bypasses the router and the middleware chain entirely.
// These tests are the only ones that exercise NewRouter and basicAuth, so they
// are what stands between a wiring mistake and a gateway that 404s every route
// (or serves the admin API without authentication) while the suite stays green.

const (
	testAdminUser = "au"
	testAdminPass = "ap"
)

// newTestRouter builds the real mux over an empty manager and store.
func newTestRouter(t *testing.T) http.Handler {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{
		Server: config.ServerConfig{BaseURL: "http://gw.test"},
		Store:  config.StoreConfig{DataDir: dir},
		Admin:  config.AdminConfig{Username: testAdminUser, Password: testAdminPass},
	}
	store, err := ch.NewStore(filepath.Join(dir, "channels.json"))
	if err != nil {
		t.Fatalf("store init: %v", err)
	}
	mgr := ch.NewManager(cfg)
	mgr.SetStore(store)
	// Zero UI: the static assets live in internal/web, which these tests do not
	// import, so every UI route falls back to its "not loaded" placeholder —
	// which is exactly what TestRouteTable asserts on below.
	return NewRouter(cfg, NewServer(cfg, mgr), NewAdminServer(context.Background(), mgr, store), UI{})
}

func doReq(t *testing.T, h http.Handler, method, path string, auth bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if auth {
		req.SetBasicAuth(testAdminUser, testAdminPass)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// TestAdminRoutesRequireAuth pins that every admin route sits behind basicAuth.
// Without this, deleting r.Use(authMw) from the admin group leaves the whole
// admin API — including channel creation and deletion — open, and no other test
// in the repo notices.
func TestAdminRoutesRequireAuth(t *testing.T) {
	r := newTestRouter(t)

	adminPaths := []struct {
		method, path string
	}{
		{http.MethodGet, "/admin/api/channels"},
		{http.MethodPost, "/admin/api/channels"},
		{http.MethodPut, "/admin/api/channels/ch1"},
		{http.MethodDelete, "/admin/api/channels/ch1"},
		{http.MethodGet, "/admin/api/channels/ch1/status"},
		{http.MethodPost, "/admin/api/channels/ch1/transition-to-vod"},
		{http.MethodGet, "/admin/"},
	}
	for _, p := range adminPaths {
		w := doReq(t, r, p.method, p.path, false)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without credentials: got %d, want 401", p.method, p.path, w.Code)
		}
		if got := w.Header().Get("WWW-Authenticate"); got != `Basic realm="admin"` {
			t.Errorf("%s %s: WWW-Authenticate = %q, want Basic realm=\"admin\"", p.method, p.path, got)
		}
	}
}

func TestAdminAuthRejectsWrongCredentials(t *testing.T) {
	r := newTestRouter(t)

	for _, tc := range []struct {
		name, user, pass string
	}{
		{"wrong password", testAdminUser, "nope"},
		{"wrong username", "nobody", testAdminPass},
		{"both wrong", "nobody", "nope"},
		{"empty", "", ""},
	} {
		req := httptest.NewRequest(http.MethodGet, "/admin/api/channels", nil)
		req.SetBasicAuth(tc.user, tc.pass)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: got %d, want 401", tc.name, w.Code)
		}
	}
}

func TestAdminAuthAcceptsCorrectCredentials(t *testing.T) {
	r := newTestRouter(t)

	w := doReq(t, r, http.MethodGet, "/admin/api/channels", true)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /admin/api/channels with credentials: got %d, want 200: %s", w.Code, w.Body.String())
	}
	// An empty store lists no channels, but the body must still be valid JSON —
	// that is what proves the handler ran rather than a middleware short-circuit.
	var out map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, w.Body.String())
	}
	if len(out) != 0 {
		t.Errorf("expected no channels, got %d", len(out))
	}
}

// TestRouteTable pins that every public route reaches its intended handler.
//
// The assertion is on the response BODY, not just the status: a deleted route
// and a request for a channel that does not exist both produce 404, so status
// alone cannot tell "the route is wired" from "the route is gone". Each handler
// emits a distinctive message, and that message is the proof of wiring.
func TestRouteTable(t *testing.T) {
	r := newTestRouter(t)

	cases := []struct {
		name       string
		method     string
		path       string
		auth       bool
		wantStatus int
		wantBody   string // substring; "" means do not assert on the body
	}{
		{"root", http.MethodGet, "/", false, http.StatusOK, "FireRing"},
		{"healthz", http.MethodGet, "/healthz", false, http.StatusOK, "ok"},
		{"metrics", http.MethodGet, "/metrics", false, http.StatusOK, ""},

		{"manifest", http.MethodGet, "/v1/channels/nope/manifest.mpd", false,
			http.StatusNotFound, "channel not found or not running"},
		{"master playlist", http.MethodGet, "/v1/channels/nope/master.m3u8", false,
			http.StatusNotFound, "channel not found or not running"},
		{"media playlist", http.MethodGet, "/v1/channels/nope/media/v0.m3u8", false,
			http.StatusNotFound, "channel not found or not running"},
		{"segment", http.MethodGet, "/v1/channels/nope/segments/v0/000000001.m4s", false,
			http.StatusNotFound, ""},
		{"init", http.MethodGet, "/v1/channels/nope/init/v0.mp4", false,
			http.StatusNotFound, ""},
		{"license playready", http.MethodPost, "/v1/channels/nope/license/playready", false,
			http.StatusNotFound, "channel not found"},
		{"license widevine", http.MethodPost, "/v1/channels/nope/license/widevine", false,
			http.StatusNotFound, "channel not found"},
		// handleKeyProxy answers with http.NotFound, which is byte-identical to
		// chi's own "no such route" body, so only the status is assertable here.
		{"key proxy", http.MethodGet, "/v1/channels/nope/key/abc", false,
			http.StatusNotFound, ""},
		// health is the one public endpoint that answers 200 for an unknown
		// channel, which makes it a strong wiring signal on its own.
		{"channel health", http.MethodGet, "/v1/channels/nope/health", false,
			http.StatusOK, `"channel_id":"nope"`},

		// The player and admin UI handlers are supplied through NewRouter's UI
		// parameter, which these tests leave zero. Asserting the placeholder
		// bodies pins that seam: the routes must exist and be wired even when no
		// static assets were passed in, and a test that quietly starts depending
		// on the real embedded assets flips these and says so.
		{"player UI", http.MethodGet, "/player/nope", false,
			http.StatusNotFound, "player UI not loaded"},
		{"player assets", http.MethodGet, "/player-assets/js/app.js", false,
			http.StatusNotFound, "player assets not loaded"},
		{"admin UI", http.MethodGet, "/admin/", true,
			http.StatusNotFound, "admin UI not loaded"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := doReq(t, r, tc.method, tc.path, tc.auth)
			if w.Code != tc.wantStatus {
				t.Fatalf("%s %s: got %d, want %d (body %q)", tc.method, tc.path, w.Code, tc.wantStatus, w.Body.String())
			}
			if tc.wantBody != "" && !strings.Contains(w.Body.String(), tc.wantBody) {
				t.Fatalf("%s %s: body %q does not contain %q", tc.method, tc.path, w.Body.String(), tc.wantBody)
			}
		})
	}
}

// TestAdminRedirect pins the /admin -> /admin/ redirect, which the SPA depends on.
func TestAdminRedirect(t *testing.T) {
	r := newTestRouter(t)

	w := doReq(t, r, http.MethodGet, "/admin", true)
	if w.Code != http.StatusMovedPermanently {
		t.Fatalf("GET /admin: got %d, want 301", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/admin/" {
		t.Errorf("Location = %q, want /admin/", loc)
	}
}

// TestCORSOnPublicRoutes pins that the public group carries CORS headers and
// that an OPTIONS preflight is answered without reaching a handler. Browser
// playback (Shaka, hls.js) breaks entirely without this.
func TestCORSOnPublicRoutes(t *testing.T) {
	r := newTestRouter(t)

	w := doReq(t, r, http.MethodOptions, "/v1/channels/nope/license/playready", false)
	if w.Code != http.StatusNoContent {
		t.Fatalf("OPTIONS preflight: got %d, want 204", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, http.MethodGet) {
		t.Errorf("Access-Control-Allow-Methods = %q, want it to include GET", got)
	}

	// A plain GET on a public route must carry the header too, not just preflight.
	w = doReq(t, r, http.MethodGet, "/v1/channels/nope/manifest.mpd", false)
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("GET manifest: Access-Control-Allow-Origin = %q, want *", got)
	}
}

// TestNoCORSOnAdminRoutes pins that the admin group is NOT in the CORS group:
// admin lives behind Basic Auth, and answering cross-origin requests there
// would let any page a logged-in operator visits drive the admin API.
func TestNoCORSOnAdminRoutes(t *testing.T) {
	r := newTestRouter(t)

	w := doReq(t, r, http.MethodGet, "/admin/api/channels", true)
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("admin route carries Access-Control-Allow-Origin = %q, want none", got)
	}
}
