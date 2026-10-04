package httpapi

import (
	"errors"
	"net/http"

	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/license"
	"github.com/a840817a/sluice/internal/presentation"
	"github.com/go-chi/chi/v5"
)

// Server holds shared dependencies for HTTP handlers.
type Server struct {
	cfg           config.Config
	manager       *ch.Manager
	presentations *presentation.Generator
}

// NewServer creates a Server.
//
// It deliberately has no side effects: VOD persistence belongs to the channel
// manager, not to having built an HTTP server.
func NewServer(cfg config.Config, manager *ch.Manager) *Server {
	return &Server{cfg: cfg, manager: manager, presentations: presentation.NewGenerator(cfg)}
}

// handleManifest serves the rewritten MPD for a channel.
//
// GET /v1/channels/{channelID}/manifest.mpd
func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	channelID := chi.URLParam(r, "channelID")

	rt := s.manager.Get(channelID)
	if rt == nil {
		http.Error(w, "channel not found or not running", http.StatusNotFound)
		return
	}

	// One read of the channel for the whole response. Mode, index and the
	// timing anchors have to describe the same instant: read one at a time, a
	// channel finishing into VOD mid-handler yields a manifest that declares
	// itself live while its segments come from the full-history index.
	sv := rt.Serving()

	out, err := s.presentations.DASHNow(sv.Channel)
	if errors.Is(err, presentation.ErrNotReady) && !sv.IsHLSSource && sv.IsVOD() {
		// A VOD record saved before its origin MPD could be restored has no
		// parsed MPD to render from, only the manifest the transition saved.
		if path := s.manager.SavedVODManifestPath(channelID); path != "" {
			serveFile(w, r, path, "application/dash+xml")
			return
		}
	}
	if err != nil {
		writeRenderError(w, err, "manifest generation failed")
		return
	}

	w.Header().Set("Content-Type", "application/dash+xml")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(out)
}

// writeRenderError answers a failed render: 404 for an output the channel
// cannot produce, 503 for one it has nothing to put in yet, and 500 with
// failure as the text for anything else.
func writeRenderError(w http.ResponseWriter, err error, failure string) {
	switch {
	case errors.Is(err, presentation.ErrNotServed):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, presentation.ErrNotReady):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	default:
		http.Error(w, failure, http.StatusInternalServerError)
	}
}

// handleLicense returns a handler that proxies DRM license requests to the
// license server URL configured for this channel. drmType is "playready" or
// "widevine".
func (s *Server) handleLicense(drmType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		channelID := chi.URLParam(r, "channelID")
		rt := s.manager.Get(channelID)
		if rt == nil {
			http.Error(w, "channel not found", http.StatusNotFound)
			return
		}

		// One read for the whole request: the admin API can swap the config
		// mid-handler, and reading it per field could pair one DRM system's
		// enable flag with the other's URL. The config is all a license
		// request needs, so it is read on its own rather than through Serving,
		// which also describes the channel's tracks.
		cfg := rt.Config()

		var licenseServerURL string
		switch drmType {
		case "playready":
			if !cfg.PlayReadyEnabled() {
				http.Error(w, "playready disabled", http.StatusForbidden)
				return
			}
			licenseServerURL = cfg.PlayReadyLicenseURL
		case "widevine":
			if !cfg.WidevineEnabled() {
				http.Error(w, "widevine disabled", http.StatusForbidden)
				return
			}
			licenseServerURL = cfg.WidevineLicenseURL
		}
		if licenseServerURL == "" {
			http.Error(w, drmType+" license URL not configured", http.StatusServiceUnavailable)
			return
		}

		proxy := license.NewProxy(licenseServerURL, ch.HeaderMap(cfg.LicenseHeaders), cfg.ForwardClientIP)
		proxy.Handler()(w, r)
	}
}
