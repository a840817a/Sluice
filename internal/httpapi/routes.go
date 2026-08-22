package httpapi

import (
	"context"
	"net/http"

	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/a840817a/sluice/internal/config"
	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// UI carries the embedded static-asset handlers the gateway serves its admin
// page and standalone player from.
//
// It is a parameter rather than package state because internal/web used to
// reach up into this package and assign three globals, which both inverted the
// dependency (a leaf asset package importing the router) and created an
// unenforceable ordering contract: web.Register() had to run before NewRouter,
// and nothing failed if it did not. Passing them in makes the compiler the
// enforcement.
//
// A nil member falls back to a "not loaded" placeholder, which is what the
// tests in this package exercise.
type UI struct {
	Admin        http.Handler
	Player       http.Handler
	PlayerAssets http.Handler
}

// notLoaded is the placeholder used for a UI handler that was not supplied.
func notLoaded(msg string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, msg, http.StatusNotFound)
	}
}

func orPlaceholder(h http.Handler, msg string) http.HandlerFunc {
	if h == nil {
		return notLoaded(msg)
	}
	return h.ServeHTTP
}

// NewRouter builds and returns the chi router with all routes attached.
// License proxies are created per-request from each channel's stored config.
func NewRouter(
	cfg config.Config,
	srv *Server,
	admin *AdminServer,
	ui UI,
) http.Handler {
	serveAdminUI := orPlaceholder(ui.Admin, "admin UI not loaded")
	servePlayerUI := orPlaceholder(ui.Player, "player UI not loaded")
	servePlayerAssets := orPlaceholder(ui.PlayerAssets, "player assets not loaded")

	r := chi.NewRouter()
	r.Use(recoveryMiddleware)
	r.Use(loggingMiddleware)

	// Root landing page
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(rootHTML))
	})

	// Health check
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// Prometheus metrics (unauthenticated; scope to internal network via firewall)
	r.Get("/metrics", promhttp.Handler().ServeHTTP)

	// --- Public DASH endpoints + standalone player (CORS enabled) ---
	r.Group(func(r chi.Router) {
		r.Use(corsMiddleware)
		// Standalone player — no auth, safe to share
		r.Get("/player/{channelID}", servePlayerUI)
		r.Get("/player-assets/*", servePlayerAssets)
		r.Get("/v1/channels/{channelID}/manifest.mpd", srv.handleManifest)
		// HLS output. A channel serves whichever of these matches its source:
		// DASH sources serve manifest.mpd, HLS sources serve master.m3u8, and
		// an fMP4 source can serve both from the same segments on disk.
		r.Get("/v1/channels/{channelID}/master.m3u8", srv.handleMasterPlaylist)
		r.Get("/v1/channels/{channelID}/media/{repID}.m3u8", srv.handleMediaPlaylist)
		// AES-128 key proxy for HLS channels kept in passthrough mode.
		r.Get("/v1/channels/{channelID}/key/{keyID}", srv.handleKeyProxy)
		r.Get("/v1/channels/{channelID}/health", srv.handleChannelHealth)
		r.Get("/v1/channels/{channelID}/segments/{repID}/{segFile}", srv.handleSegment)
		r.Get("/v1/channels/{channelID}/init/{repID}.mp4", srv.handleInit)
		// Per-channel license proxies (OPTIONS preflight also handled by corsMiddleware)
		r.Post("/v1/channels/{channelID}/license/playready", srv.handleLicense("playready"))
		r.Post("/v1/channels/{channelID}/license/widevine", srv.handleLicense("widevine"))
		r.Options("/v1/channels/{channelID}/license/playready", func(w http.ResponseWriter, r *http.Request) {})
		r.Options("/v1/channels/{channelID}/license/widevine", func(w http.ResponseWriter, r *http.Request) {})
	})

	// --- Admin UI + API (Basic Auth protected) ---
	authMw := basicAuth(cfg.Admin.Username, cfg.Admin.Password)
	r.Group(func(r chi.Router) {
		r.Use(authMw)

		// Admin SPA
		r.Get("/admin", http.RedirectHandler("/admin/", http.StatusMovedPermanently).ServeHTTP)
		r.Get("/admin/*", serveAdminUI)

		// Admin REST API
		r.Get("/admin/api/channels", admin.handleListChannels)
		r.Post("/admin/api/channels", admin.handleCreateChannel)
		r.Put("/admin/api/channels/{channelID}", admin.handleUpdateChannel)
		r.Delete("/admin/api/channels/{channelID}", admin.handleDeleteChannel)
		r.Get("/admin/api/channels/{channelID}/status", admin.handleChannelStatus)
		r.Post("/admin/api/channels/{channelID}/restart", admin.handleRestartChannel)
		r.Post("/admin/api/channels/{channelID}/transition-to-vod", admin.handleTransitionToVOD)
	})

	return r
}

// NewAdminServerFromContext is a convenience constructor used in main.
func NewAdminServerFromContext(
	ctx context.Context,
	manager *ch.Manager,
	store *ch.Store,
) *AdminServer {
	return NewAdminServer(ctx, manager, store)
}
