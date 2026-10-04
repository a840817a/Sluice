package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/go-chi/chi/v5"
)

const playlistContentType = "application/vnd.apple.mpegurl"

// handleMasterPlaylist serves the HLS multivariant playlist for a channel.
//
// GET /v1/channels/{channelID}/master.m3u8
func (s *Server) handleMasterPlaylist(w http.ResponseWriter, r *http.Request) {
	channelID := chi.URLParam(r, "channelID")

	rt := s.manager.Get(channelID)
	if rt == nil {
		http.Error(w, "channel not found or not running", http.StatusNotFound)
		return
	}
	out, err := s.presentations.Master(rt.Serving().Channel)
	if err != nil {
		writeRenderError(w, err, "playlist generation failed")
		return
	}

	w.Header().Set("Content-Type", playlistContentType)
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(out)
}

// handleMediaPlaylist serves the HLS media playlist for one representation.
//
// GET /v1/channels/{channelID}/media/{repID}.m3u8
func (s *Server) handleMediaPlaylist(w http.ResponseWriter, r *http.Request) {
	channelID := chi.URLParam(r, "channelID")
	repID := chi.URLParam(r, "repID")

	rt := s.manager.Get(channelID)
	if rt == nil {
		http.Error(w, "channel not found or not running", http.StatusNotFound)
		return
	}
	// One read of the channel for the whole response: the index the segments
	// come from and the VOD flag that describes them have to agree.
	out, err := s.presentations.Media(rt.Serving().Channel, repID)
	if err != nil {
		writeRenderError(w, err, "playlist generation failed")
		return
	}

	w.Header().Set("Content-Type", playlistContentType)
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(out)
}

// handleKeyProxy serves an AES-128 content key to players on behalf of the
// key server.
//
// GET /v1/channels/{channelID}/key/{keyID}
//
// keyID is the gateway's own identifier for a key URI, and is resolved through
// the registry of URIs actually seen in this channel's playlists. The key
// server's URL is deliberately never accepted from the request: taking it as
// a parameter would turn the gateway into an open proxy that any caller could
// aim at internal addresses.
func (s *Server) handleKeyProxy(w http.ResponseWriter, r *http.Request) {
	channelID := chi.URLParam(r, "channelID")
	keyID := chi.URLParam(r, "keyID")

	rt := s.manager.Get(channelID)
	if rt == nil {
		http.NotFound(w, r)
		return
	}
	// ResolveKey decides whether this channel brokers keys at all and whether
	// keyID is one it issued; both refusals look like an unknown key.
	key, err := rt.ResolveKey(r.Context(), keyID)
	if errors.Is(err, ch.ErrNoKeyProxy) || errors.Is(err, ch.ErrUnknownKey) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		slog.Warn("key proxy fetch failed", "channel", channelID, "err", err)
		http.Error(w, "key unavailable", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(key)))
	// Keys are per-key-period secrets brokered for a specific viewer; do not let
	// shared caches keep copies.
	w.Header().Set("Cache-Control", "no-store")
	w.Write(key)
}
