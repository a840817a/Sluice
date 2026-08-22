package httpapi

import (
	"net/http"
	"time"

	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/gwurl"
	"github.com/a840817a/sluice/internal/hlsout"
	"github.com/a840817a/sluice/internal/license"
	"github.com/a840817a/sluice/internal/manifest"
	imdp "github.com/a840817a/sluice/internal/mpd"
	"github.com/go-chi/chi/v5"
)

// Server holds shared dependencies for HTTP handlers.
type Server struct {
	cfg     config.Config
	manager *ch.Manager
	// stickyTargetDuration keeps each HLS representation's advertised
	// EXT-X-TARGETDURATION monotonically non-decreasing across reloads. Keys are
	// "{channelID}/{repID}".
	stickyTargetDuration hlsout.StickyTargetDuration
}

// NewServer creates a Server.
//
// It deliberately has no side effects. Registering the manager's OnVODReady
// callback used to happen here, which buried a domain wiring decision inside a
// constructor and made VOD persistence a property of having built an HTTP
// server. main.go now wires it explicitly.
func NewServer(cfg config.Config, manager *ch.Manager) *Server {
	return &Server{cfg: cfg, manager: manager}
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

	// An HLS source is only serveable as DASH when its segments are fMP4;
	// MPEG-TS would require remuxing, which this gateway deliberately does not
	// do. Such channels are served through master.m3u8 instead.
	if !rt.ServesDASH() {
		http.Error(w, "channel has an MPEG-TS HLS source; use master.m3u8", http.StatusNotFound)
		return
	}

	mode := rt.Mode()
	isVOD := mode == ch.ModeVOD
	isStaticIngesting := mode == ch.ModeStaticIngesting
	// One snapshot for the whole response. Synthesis decides which
	// representations exist and at what timescale, and generation builds their
	// timelines; reading the live index twice let those two answers come from
	// two different instants while ingest kept committing in between.
	snap := rt.ActiveIndex().Snapshot()

	baseURL := s.cfg.Server.BaseURL
	chCfg := rt.Config()
	windowDepth := s.cfg.Window.Depth
	if chCfg.KeepAllSegments {
		windowDepth = 24 * time.Hour
	}

	// An fMP4 HLS channel has no upstream MPD; synthesize one from the
	// discovered variants and the segment index so the DASH output is generated
	// from the very same files the HLS output serves. DASH channels use the
	// real upstream MPD.
	var p *imdp.ParsedMPD
	if rt.IsHLSSource() {
		p = s.synthesizeHLSManifest(rt, snap, isVOD, windowDepth)
	} else {
		p = rt.ParsedMPD()
		if p == nil && isVOD {
			if path := s.savedVODManifestPath(channelID); path != "" {
				serveFile(w, r, path, "application/dash+xml")
				return
			}
		}
	}
	if p == nil {
		http.Error(w, "manifest not yet available", http.StatusServiceUnavailable)
		return
	}
	cfg := manifest.GeneratorConfig{
		GatewayBaseURL:              baseURL,
		ChannelID:                   channelID,
		LicenseURL:                  licenseProxyURL(baseURL, channelID, "playready", chCfg.PlayReadyEnabled()),
		WidevineLicenseURL:          licenseProxyURL(baseURL, channelID, "widevine", chCfg.WidevineEnabled()),
		DisablePlayReady:            !chCfg.PlayReadyEnabled(),
		DisableWidevine:             !chCfg.WidevineEnabled(),
		WindowDepth:                 windowDepth,
		SafeEdgeBuffer:              s.cfg.Window.SafeEdgeBuffer,
		FallbackMinimumUpdatePeriod: s.cfg.Upstream.PollInterval,
		VODMode:                     isVOD,
		FinalDuration:               rt.FinalDuration(),
		StaticIngesting:             isStaticIngesting,
		StaticIngestStart:           rt.StaticIngestStartTime(),
	}

	out, err := manifest.Generate(p, snap, cfg)
	if err != nil {
		http.Error(w, "manifest generation failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/dash+xml")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(out)
}

// licenseProxyURL returns the URL of a channel's DRM license proxy, or "" when
// that DRM system is disabled for the channel.
func licenseProxyURL(baseURL, channelID, drmType string, enabled bool) string {
	if !enabled {
		return ""
	}
	return gwurl.LicenseURL(baseURL, channelID, drmType)
}

// handleLicense returns a handler that proxies DRM license requests to the
// upstream URL configured for this channel. drmType is "playready" or "widevine".
func (s *Server) handleLicense(drmType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		channelID := chi.URLParam(r, "channelID")
		rt := s.manager.Get(channelID)
		if rt == nil {
			http.Error(w, "channel not found", http.StatusNotFound)
			return
		}

		// One snapshot for the whole request: the admin API can swap the config
		// mid-handler, and reading it per field could pair one DRM system's
		// enable flag with the other's URL.
		cfg := rt.Config()

		var upstreamURL string
		switch drmType {
		case "playready":
			if !cfg.PlayReadyEnabled() {
				http.Error(w, "playready disabled", http.StatusForbidden)
				return
			}
			upstreamURL = cfg.PlayReadyLicenseURL
		case "widevine":
			if !cfg.WidevineEnabled() {
				http.Error(w, "widevine disabled", http.StatusForbidden)
				return
			}
			upstreamURL = cfg.WidevineLicenseURL
		}
		if upstreamURL == "" {
			http.Error(w, drmType+" license URL not configured", http.StatusServiceUnavailable)
			return
		}

		proxy := license.NewProxy(upstreamURL, ch.HeaderMap(cfg.LicenseHeaders), cfg.ForwardClientIP)
		proxy.Handler()(w, r)
	}
}
