package httpapi

import (
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/a840817a/sluice/internal/gwurl"
	"github.com/a840817a/sluice/internal/hlsingest"
	"github.com/a840817a/sluice/internal/hlsout"
	"github.com/a840817a/sluice/internal/index"
	"github.com/a840817a/sluice/internal/manifest"
	imdp "github.com/a840817a/sluice/internal/mpd"
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
	view, ok := s.hlsViewFor(rt)
	if !ok {
		http.Error(w, "channel cannot be served as HLS (encrypted or unsupported)", http.StatusNotFound)
		return
	}
	if !view.ready {
		http.Error(w, "playlist not yet available", http.StatusServiceUnavailable)
		return
	}

	out := hlsout.Master(view.variants, hlsout.MasterOptions{
		GatewayBaseURL:      s.cfg.Server.BaseURL,
		ChannelID:           channelID,
		IndependentSegments: view.independentSegments,
		Renditions:          view.renditions,
		Subtitles:           view.subtitles,
	})

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
	view, ok := s.hlsViewFor(rt)
	if !ok {
		http.Error(w, "channel cannot be served as HLS", http.StatusNotFound)
		return
	}
	if !view.knownRep(repID) {
		http.NotFound(w, r)
		return
	}

	ci := rt.ActiveIndex()
	segs := publishedSegsForRep(ci, repID)

	// The advertised target duration must never shrink between reloads, so it
	// is tracked per channel+representation rather than recomputed per request.
	targetDur := s.stickyTargetDuration.Get(channelID+"/"+repID, segs)

	isVOD := rt.Mode() == ch.ModeVOD
	opts := hlsout.MediaOptions{
		GatewayBaseURL: s.cfg.Server.BaseURL,
		ChannelID:      channelID,
		RepID:          repID,
		VOD:            isVOD,
		TargetDuration: targetDur,
		WindowSegments: windowSegments(s.cfg.Window.Depth.Seconds(), targetDur, rt.Config().KeepAllSegments || isVOD),
	}
	// fMP4 representations need their initialization section advertised via
	// EXT-X-MAP; a non-empty init path is exactly the fMP4 signal, and is empty
	// for self-contained TS/WebVTT segments regardless of source.
	if initPath := publishedInitPath(ci, repID); initPath != "" {
		opts.InitURI = gwurl.InitURL(s.cfg.Server.BaseURL, channelID, repID)
	}
	// Segments kept encrypted on disk are advertised with a key URI pointing at
	// this gateway's proxy. Decrypt-on-ingest channels store cleartext and carry
	// no key material, so nothing is emitted for them.
	opts.KeyProxyBase = keyProxyURL(s.cfg.Server.BaseURL, channelID)

	out := hlsout.Media(segs, opts)

	w.Header().Set("Content-Type", playlistContentType)
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(out)
}

// synthesizeHLSManifest builds the stand-in DASH MPD for an fMP4 HLS channel,
// or nil when nothing is published yet. HLS carries no MPD, so the structure
// comes from the discovered variants and the timeline from the segment index.
func (s *Server) synthesizeHLSManifest(rt *ch.Runtime, snap index.Snapshot, isVOD bool, windowDepth time.Duration) *imdp.ParsedMPD {
	variants := rt.HLS.Variants()
	out := make([]manifest.HLSVariant, 0, len(variants))
	for _, v := range variants {
		out = append(out, manifest.HLSVariant{
			RepID:     v.RepID,
			Bandwidth: v.Bandwidth,
			Codecs:    v.Codecs,
			Width:     uint64(v.Width),
			Height:    uint64(v.Height),
		})
	}
	// Demuxed audio renditions become a separate audio AdaptationSet, so the
	// DASH output carries audio too (not just the video track).
	var audio []manifest.HLSAudio
	for _, r := range rt.HLS.Renditions() {
		audio = append(audio, manifest.HLSAudio{RepID: r.RepID})
	}

	return manifest.SynthesizeHLSMPD(out, snap, manifest.HLSSynthOptions{
		ASID:      hlsingest.VideoASID,
		MediaType: imdp.MediaVideo,
		MimeType:  "video/mp4",
		Audio:     audio,
		AudioASID: hlsingest.AudioASID,
		VOD:       isVOD,
		// A stable anchor for the live edge; unused for VOD.
		AvailabilityStartTime: rt.StaticIngestStartTime(),
		MinimumUpdatePeriod:   s.cfg.Upstream.PollInterval,
		TimeShiftBufferDepth:  windowDepth,
	})
}

// windowSegments converts the configured sliding-window depth into a segment
// count. Returns 0 (no cap) when the channel keeps its full history.
func windowSegments(depthSec float64, targetDur int, keepAll bool) int {
	if keepAll || depthSec <= 0 || targetDur <= 0 {
		return 0
	}
	n := int(math.Ceil(depthSec / float64(targetDur)))
	// RFC 8216 §6.2.2 requires a live playlist to hold at least three segments.
	if n < 3 {
		n = 3
	}
	return n
}

// publishedSegsForRep collects every Published segment for repID across all
// periods, which is what the media playlist renders from.
func publishedSegsForRep(ci *index.ChannelIndex, repID string) []index.SegmentState {
	var out []index.SegmentState
	ci.ForEachRep(func(ref index.RepRef, rep *index.RepresentationState) {
		if ref.RepID == repID {
			out = append(out, rep.Published()...)
		}
	})
	return out
}

// keyProxyURL returns the base URL of a channel's AES-128 key proxy.
//
// A thin wrapper over gwurl so that urls_test.go's byte-exact cases — including
// the trailing-slash ones — keep guarding the shape from this side.
func keyProxyURL(baseURL, channelID string) string {
	return gwurl.KeyProxyBaseURL(baseURL, channelID)
}

// handleKeyProxy serves an AES-128 content key to players on behalf of the
// upstream key server.
//
// GET /v1/channels/{channelID}/key/{keyID}
//
// keyID is the gateway's own identifier for a key URI, and is resolved through
// the registry of URIs actually seen in this channel's playlists. The upstream
// URL is deliberately never accepted from the request: taking it as a parameter
// would turn the gateway into an open proxy that any caller could aim at
// internal addresses.
func (s *Server) handleKeyProxy(w http.ResponseWriter, r *http.Request) {
	channelID := chi.URLParam(r, "channelID")
	keyID := chi.URLParam(r, "keyID")

	rt := s.manager.Get(channelID)
	if rt == nil || !rt.CanServeKeys() {
		http.NotFound(w, r)
		return
	}
	// Decrypt-on-ingest channels republish in the clear; there is no key to hand
	// out, and serving one would leak a credential for content we already
	// stripped protection from.
	if rt.Config().DecryptOnIngest() {
		http.NotFound(w, r)
		return
	}

	uri, ok := rt.HLS.KeyURIForID(keyID)
	if !ok {
		http.NotFound(w, r)
		return
	}

	key, err := rt.Keys.Get(r.Context(), uri)
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
